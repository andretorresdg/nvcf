// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package modelcache

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// modelTree writes a small model tree: nested files, an empty directory,
// an executable, and a relative symlink inside the tree.
func modelTree(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(src, "m/weights"), 0o755))
	must(t, os.MkdirAll(filepath.Join(src, "m/empty"), 0o750))
	must(t, os.WriteFile(filepath.Join(src, "m/config.json"), []byte(`{"a":1}`), 0o644))
	must(t, os.WriteFile(filepath.Join(src, "m/weights/w-0001.safetensors"), bytes.Repeat([]byte{7}, 3<<20), 0o644))
	must(t, os.WriteFile(filepath.Join(src, "m/run.sh"), []byte("#!/bin/sh\n"), 0o755))
	must(t, os.Symlink("weights/w-0001.safetensors", filepath.Join(src, "m/latest")))
	return src
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func harvest(t *testing.T, s *Store, key, src string) string {
	t.Helper()
	gen, err := s.Harvest(key, src, Limits{})
	if err != nil {
		t.Fatalf("harvest: %v", err)
	}
	return gen
}

func TestHarvestThenSeedReproducesTheTree(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	src := modelTree(t)
	harvest(t, s, "k1", src)

	gen, m, ok := s.Latest("k1")
	if !ok {
		t.Fatal("no complete generation after harvest")
	}
	dst := t.TempDir()
	if err := Seed(gen, m, dst); err != nil {
		t.Fatalf("seed: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dst, "m/config.json"))
	if err != nil || string(b) != `{"a":1}` {
		t.Fatalf("config.json = %q, %v", b, err)
	}
	if fi, err := os.Stat(filepath.Join(dst, "m/run.sh")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("run.sh mode = %v, %v; want 0755", fi.Mode().Perm(), err)
	}
	if fi, err := os.Stat(filepath.Join(dst, "m/empty")); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o750 {
		t.Fatalf("empty dir = %v, %v; want a 0750 directory", fi, err)
	}
	if tgt, err := os.Readlink(filepath.Join(dst, "m/latest")); err != nil || tgt != "weights/w-0001.safetensors" {
		t.Fatalf("symlink = %q, %v", tgt, err)
	}
	if fi, err := os.Stat(filepath.Join(dst, "m/weights/w-0001.safetensors")); err != nil || fi.Size() != 3<<20 {
		t.Fatalf("weights = %v, %v", fi, err)
	}
}

func TestSeedRefusesACorruptedFile(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	harvest(t, s, "k1", modelTree(t))
	gen, m, _ := s.Latest("k1")

	// Same size, different bytes: only the checksum can tell.
	p := filepath.Join(gen, TreeDir, "m/weights/w-0001.safetensors")
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	must(t, err)
	_, err = f.WriteAt([]byte{8}, 1<<20)
	must(t, err)
	must(t, f.Close())

	err = Seed(gen, m, t.TempDir())
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("seed of a corrupted generation: err = %v, want ErrCorrupt", err)
	}
}

func TestSeedRefusesAMissingFile(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	harvest(t, s, "k1", modelTree(t))
	gen, m, _ := s.Latest("k1")
	must(t, os.Remove(filepath.Join(gen, TreeDir, "m/config.json")))
	if err := Seed(gen, m, t.TempDir()); err == nil {
		t.Fatal("seed with a missing file succeeded")
	}
}

func TestSeedRefusesANonEmptyTarget(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	harvest(t, s, "k1", modelTree(t))
	gen, m, _ := s.Latest("k1")
	dst := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dst, "partial"), []byte("x"), 0o644))
	if err := Seed(gen, m, dst); !errors.Is(err, ErrTargetNotEmpty) {
		t.Fatalf("seed into a non-empty target: err = %v, want ErrTargetNotEmpty", err)
	}
}

func TestHarvestRejectsAnEscapingSymlink(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	for _, tgt := range []string{"/etc/passwd", "../../outside", "m/../../x"} {
		src := modelTree(t)
		must(t, os.Symlink(tgt, filepath.Join(src, "m/bad")))
		if _, err := s.Harvest("k-"+strings.ReplaceAll(tgt, "/", "_"), src, Limits{}); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("harvest with symlink -> %q: err = %v, want ErrUnsupported", tgt, err)
		}
	}
	if gens, _ := filepath.Glob(filepath.Join(s.Root, "*", GenPrefix+"*")); len(gens) != 0 {
		t.Fatalf("a rejected harvest published %v", gens)
	}
}

func TestHarvestRejectsATreeOverItsLimit(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	if _, err := s.Harvest("k1", modelTree(t), Limits{MaxBytes: 1 << 20}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if _, _, ok := s.Latest("k1"); ok {
		t.Fatal("an over-limit harvest published a generation")
	}
}

func TestHarvestLeavesNoAttemptBehindOnFailure(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	_, _ = s.Harvest("k1", modelTree(t), Limits{MaxBytes: 1})
	left, _ := filepath.Glob(filepath.Join(s.Root, "k1", AttemptPrefix+"*"))
	if len(left) != 0 {
		t.Fatalf("failed harvest left %v", left)
	}
}

func TestIncompleteGenerationIsInvisible(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	harvest(t, s, "k1", modelTree(t))
	gen, _, _ := s.Latest("k1")
	must(t, os.Remove(filepath.Join(gen, CompleteFile)))
	if _, _, ok := s.Latest("k1"); ok {
		t.Fatal("a generation without its completion marker was returned")
	}
}

func TestLatestPrefersTheNewestGeneration(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	first := harvest(t, s, "k1", modelTree(t))
	time.Sleep(2 * time.Millisecond)
	second := harvest(t, s, "k1", modelTree(t))
	gen, _, ok := s.Latest("k1")
	if !ok || gen != second || gen == first {
		t.Fatalf("Latest = %s, want %s", gen, second)
	}
}

func TestKeyDependsOnEveryInput(t *testing.T) {
	base := Key("tenant:a", "ngc://m:1", "img@sha256:1", "recipe")
	for _, k := range []string{
		Key("tenant:b", "ngc://m:1", "img@sha256:1", "recipe"),
		Key("tenant:a", "ngc://m:2", "img@sha256:1", "recipe"),
		Key("tenant:a", "ngc://m:1", "img@sha256:2", "recipe"),
		Key("tenant:a", "ngc://m:1", "img@sha256:1", "recipe2"),
	} {
		if k == base {
			t.Fatalf("key %s does not change with an input", k)
		}
	}
	if !ValidKey(base) || ValidKey("../x") || ValidKey("") {
		t.Fatal("ValidKey accepts a bad key or rejects a good one")
	}
}

func TestManifestPathsAreConfined(t *testing.T) {
	for _, p := range []string{"", "/abs", "../x", "a/../../x", "a/./b", "a//b"} {
		if validRel(p) {
			t.Fatalf("validRel(%q) = true", p)
		}
	}
	for _, p := range []string{"a", "a/b.json", ".hidden/x"} {
		if !validRel(p) {
			t.Fatalf("validRel(%q) = false", p)
		}
	}
}

func TestEvictRemovesAGenerationAndUsageCountsIt(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	harvest(t, s, "k1", modelTree(t))
	used, err := s.Usage()
	must(t, err)
	if used < 3<<20 {
		t.Fatalf("usage = %d, want at least the 3 MiB file", used)
	}
	must(t, s.EvictKey("k1"))
	if _, _, ok := s.Latest("k1"); ok {
		t.Fatal("evicted key still has a generation")
	}
	if used, _ := s.Usage(); used != 0 {
		t.Fatalf("usage after evict = %d", used)
	}
}

func TestCleanAttemptsRemovesOnlyAttempts(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	harvest(t, s, "k1", modelTree(t))
	stale := filepath.Join(s.Root, "k1", AttemptPrefix+"dead")
	must(t, os.MkdirAll(filepath.Join(stale, TreeDir), 0o755))
	must(t, s.CleanAttempts())
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale attempt survived")
	}
	if _, _, ok := s.Latest("k1"); !ok {
		t.Fatal("CleanAttempts removed a complete generation")
	}
}

func TestFetchVerifiesEachFileAgainstTheManifest(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	harvest(t, s, "k1", modelTree(t))
	gen, m, _ := s.Latest("k1")

	dst := &Store{Root: t.TempDir()}
	open := func(rel string) (ReadCloser, error) { return os.Open(filepath.Join(gen, TreeDir, rel)) }
	if _, err := dst.Fetch("k1", m, open, Limits{}); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if _, _, ok := dst.Latest("k1"); !ok {
		t.Fatal("fetch published nothing")
	}

	// A source serving other bytes than its manifest is refused.
	bad := &Store{Root: t.TempDir()}
	lie := func(rel string) (ReadCloser, error) {
		if strings.HasSuffix(rel, ".json") {
			return nopCloser{strings.NewReader(`{"a":2}`)}, nil
		}
		return open(rel)
	}
	if _, err := bad.Fetch("k1", m, lie, Limits{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("fetch from a lying source: err = %v, want ErrCorrupt", err)
	}
	if _, _, ok := bad.Latest("k1"); ok {
		t.Fatal("a failed fetch published a generation")
	}
}

func TestFetchRejectsAManifestWithAnEscapingPath(t *testing.T) {
	m := Manifest{Version: ManifestVersion, Entries: []Entry{{Path: "../evil", Type: TypeFile, Size: 1, SHA256: strings.Repeat("0", 64)}}}
	s := &Store{Root: t.TempDir()}
	open := func(string) (ReadCloser, error) { return nopCloser{strings.NewReader("x")}, nil }
	if _, err := s.Fetch("k1", m, open, Limits{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

type nopCloser struct{ *strings.Reader }

func (nopCloser) Close() error { return nil }

func init() { testKey = func(k string) bool { return strings.HasPrefix(k, "k") } }

func TestHarvestRefusesATreeWrittenAfterTheDownload(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	src := modelTree(t)
	finished := time.Now().Add(-time.Hour)
	old := finished.Add(-time.Minute)
	ts := []unix.Timeval{unix.NsecToTimeval(old.UnixNano()), unix.NsecToTimeval(old.UnixNano())}
	must(t, filepath.Walk(src, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return unix.Lutimes(p, ts) // the symlink itself, not its target
	}))
	// Untouched since the download: cached.
	if _, err := s.Harvest("k1", src, Limits{NotAfter: finished}); err != nil {
		t.Fatalf("a tree untouched since the download: %v", err)
	}
	// The engine wrote a file into the volume after it started.
	must(t, os.WriteFile(filepath.Join(src, "m/engine.lock"), nil, 0o644))
	if _, err := s.Harvest("k2", src, Limits{NotAfter: finished}); !errors.Is(err, ErrWrittenAfter) {
		t.Fatalf("err = %v, want ErrWrittenAfter", err)
	}
	if _, _, ok := s.Latest("k2"); ok {
		t.Fatal("a tree written after the download was cached")
	}
}
