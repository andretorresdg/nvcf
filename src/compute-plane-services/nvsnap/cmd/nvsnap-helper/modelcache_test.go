// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dsx-ai-factory/nvcf/src/compute-plane-services/nvsnap/internal/modelcache"
)

const cacheKey = "0123456789abcdef0123456789abcdef"

// cachedModel publishes a one-file generation in a cache root and returns
// the key directory, as the download init sees it mounted.
func cachedModel(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "weights.bin"), []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &modelcache.Store{Root: t.TempDir()}
	if _, err := s.Harvest(cacheKey, src, modelcache.Limits{}); err != nil {
		t.Fatal(err)
	}
	return s.KeyDir(cacheKey)
}

// download is a fallback the test can see ran: it writes a marker file.
func download(dir string) []string {
	return []string{"/bin/sh", "-c", "echo downloaded > " + filepath.Join(dir, "weights.bin")}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMain(m *testing.M) {
	terminationLog = filepath.Join(os.TempDir(), "nvsnap-helper-test-termination-log")
	os.Exit(m.Run())
}

func TestModelCacheSeedsFromACompleteGeneration(t *testing.T) {
	cache, to := cachedModel(t), t.TempDir()
	code, err := modelCacheCmd(append([]string{"--cache", cache, "--to", to, "--"}, download(to)...))
	if code != 0 || err != nil {
		t.Fatalf("code %d err %v", code, err)
	}
	if got := read(t, filepath.Join(to, "weights.bin")); got != "cached" {
		t.Fatalf("weights.bin = %q, want the cached copy (the download ran)", got)
	}
	if got := read(t, terminationLog); got != modelcache.InitSeeded {
		t.Fatalf("termination message = %q", got)
	}
}

func TestModelCacheFallsBackOnACorruptGeneration(t *testing.T) {
	cache, to := cachedModel(t), t.TempDir()
	gen, _, _ := modelcache.LatestIn(cache)
	if err := os.WriteFile(filepath.Join(gen, modelcache.TreeDir, "weights.bin"), []byte("CACHED"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, err := modelCacheCmd(append([]string{"--cache", cache, "--to", to, "--"}, download(to)...))
	if code != 0 || err != nil {
		t.Fatalf("code %d err %v", code, err)
	}
	if got := read(t, filepath.Join(to, "weights.bin")); got != "downloaded\n" {
		t.Fatalf("weights.bin = %q, want the download's", got)
	}
}

func TestModelCacheDownloadsWithNoCache(t *testing.T) {
	to := t.TempDir()
	for _, cache := range []string{t.TempDir(), filepath.Join(t.TempDir(), "absent")} {
		_ = os.RemoveAll(filepath.Join(to, "weights.bin"))
		code, err := modelCacheCmd(append([]string{"--cache", cache, "--to", to, "--"}, download(to)...))
		if code != 0 || err != nil {
			t.Fatalf("code %d err %v", code, err)
		}
		if got := read(t, filepath.Join(to, "weights.bin")); got != "downloaded\n" {
			t.Fatalf("weights.bin = %q", got)
		}
	}
}

func TestModelCacheLeavesANonEmptyVolumeToTheDownload(t *testing.T) {
	cache, to := cachedModel(t), t.TempDir()
	if err := os.WriteFile(filepath.Join(to, "partial"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, err := modelCacheCmd(append([]string{"--cache", cache, "--to", to, "--"}, download(to)...)); code != 0 || err != nil {
		t.Fatalf("code %d err %v", code, err)
	}
	if got := read(t, filepath.Join(to, "weights.bin")); got != "downloaded\n" {
		t.Fatalf("weights.bin = %q, want the download's", got)
	}
	if _, err := os.Stat(filepath.Join(to, "partial")); err != nil {
		t.Fatal("the helper removed what an earlier run of the download left")
	}
}

func TestModelCachePassesTheDownloadsExitCode(t *testing.T) {
	code, _ := modelCacheCmd([]string{"--cache", t.TempDir(), "--to", t.TempDir(), "--", "/bin/sh", "-c", "exit 7"})
	if code != 7 {
		t.Fatalf("code = %d, want the download's 7", code)
	}
}

func TestModelCacheDoesNotWaitWithoutAFreshHeartbeat(t *testing.T) {
	cache := t.TempDir()
	hb := filepath.Join(cache, modelcache.IncomingFile)
	if err := os.WriteFile(hb, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(hb, old, old); err != nil {
		t.Fatal(err)
	}
	to := t.TempDir()
	start := time.Now()
	if code, err := modelCacheCmd(append([]string{"--cache", cache, "--to", to, "--wait", "60", "--stale", "20", "--"}, download(to)...)); code != 0 || err != nil {
		t.Fatalf("code %d err %v", code, err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("waited %v on a stale heartbeat", d)
	}
}

func TestModelCacheWaitsForALiveFetchThenSeeds(t *testing.T) {
	cache := cachedModel(t)
	// Hide the generation behind a live heartbeat, then publish it.
	gen, _, _ := modelcache.LatestIn(cache)
	// Under a name LatestIn does not look at, so only the wait finds it.
	hidden := filepath.Join(cache, "hidden")
	if err := os.Rename(gen, hidden); err != nil {
		t.Fatal(err)
	}
	hb := filepath.Join(cache, modelcache.IncomingFile)
	if err := os.WriteFile(hb, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(1500 * time.Millisecond)
		_ = os.Rename(hidden, gen)
	}()
	to := t.TempDir()
	code, err := modelCacheCmd(append([]string{"--cache", cache, "--to", to, "--wait", "30", "--stale", "20", "--poll", "200ms", "--"}, download(to)...))
	if code != 0 || err != nil {
		t.Fatalf("code %d err %v", code, err)
	}
	if got := read(t, filepath.Join(to, "weights.bin")); got != "cached" {
		t.Fatalf("weights.bin = %q, want the fetched copy", got)
	}
	if _, err := os.Stat(hidden); !os.IsNotExist(err) {
		t.Fatal("the seed did not wait for the generation to be published")
	}
}

func TestModelCacheReportsTheDownloadedTree(t *testing.T) {
	to := t.TempDir()
	if code, err := modelCacheCmd(append([]string{"--cache", t.TempDir(), "--to", to, "--"}, download(to)...)); code != 0 || err != nil {
		t.Fatalf("code %d err %v", code, err)
	}
	ents, err := modelcache.Scan(to)
	if err != nil {
		t.Fatal(err)
	}
	if got := modelcache.DownloadedTree(read(t, terminationLog)); got != modelcache.Fingerprint(ents) {
		t.Fatalf("reported tree %q, want the volume's %q", got, modelcache.Fingerprint(ents))
	}
}

func TestModelCacheWaitIsCapped(t *testing.T) {
	cache := t.TempDir()
	hb := filepath.Join(cache, modelcache.IncomingFile)
	if err := os.WriteFile(hb, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() { // a heartbeat that never stops
		for {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
				now := time.Now()
				_ = os.Chtimes(hb, now, now)
			}
		}
	}()
	to := t.TempDir()
	start := time.Now()
	if code, err := modelCacheCmd(append([]string{"--cache", cache, "--to", to, "--wait", "2", "--poll", "100ms", "--"}, download(to)...)); code != 0 || err != nil {
		t.Fatalf("code %d err %v", code, err)
	}
	if d := time.Since(start); d > 6*time.Second {
		t.Fatalf("waited %v past a 2 s cap", d)
	}
	if got := read(t, filepath.Join(to, "weights.bin")); got != "downloaded\n" {
		t.Fatalf("weights.bin = %q", got)
	}
}

func TestModelCacheNeedsACommand(t *testing.T) {
	if code, err := modelCacheCmd([]string{"--cache", t.TempDir(), "--to", t.TempDir()}); code == 0 || err == nil {
		t.Fatalf("no download command: code %d err %v, want an error", code, err)
	}
}
