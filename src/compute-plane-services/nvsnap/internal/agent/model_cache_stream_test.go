// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dsx-ai-factory/nvcf/src/compute-plane-services/nvsnap/internal/modelcache"
)

// Streaming (docs/design/model-cache-single-flight.md, 3.3 and 3.4), on
// plain directories: a source over a "download" the test writes, and a
// streamer copying it into another directory through a local client.

func fastStream() streamTiming {
	return streamTiming{settle: 200 * time.Millisecond, poll: 50 * time.Millisecond, stall: 10 * time.Second}
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

func sha(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// countingClient counts file opens, to tell a rename from a second copy.
type countingClient struct {
	liveClient
	opens atomic.Int64
}

func (c *countingClient) Open(ctx context.Context, rel string) (io.ReadCloser, error) {
	c.opens.Add(1)
	return c.liveClient.Open(ctx, rel)
}

// finish signals the source's download as over, as the helper does, and
// waits for the final manifest.
func finish(t *testing.T, src *liveSource) {
	t.Helper()
	ents, err := modelcache.ScanLive(src.root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.finalize(modelcache.Fingerprint(modelcache.Entries(ents))); err != nil {
		t.Fatalf("finalize: %v", err)
	}
}

func runStream(t *testing.T, client liveClient, dst string) (chan error, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &streamer{client: client, dst: dst, timing: fastStream()}
	done := make(chan error, 1)
	go func() { done <- s.run(ctx) }()
	return done, cancel
}

func waitDone(t *testing.T, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("the stream did not end")
		return nil
	}
}

func assertTree(t *testing.T, dst string, want map[string]string) {
	t.Helper()
	got := map[string]string{}
	_ = filepath.Walk(dst, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			rel, _ := filepath.Rel(dst, p)
			b, _ := os.ReadFile(p)
			got[filepath.ToSlash(rel)] = string(b)
		}
		return nil
	})
	if len(got) != len(want) {
		t.Fatalf("tree %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q, want %q (tree %v)", k, got[k], v, got)
		}
	}
}

func TestStreamCopiesAnAppendingDownloadAndEndsWithItsTree(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	ls := newLiveSource(src, "uid-holder", fastStream())
	done, _ := runStream(t, localClient{ls}, dst)
	write(t, src, "a/shard-1.bin", "one")
	time.Sleep(600 * time.Millisecond) // settles and is copied before the end
	f, err := os.OpenFile(filepath.Join(src, "a", "shard-2.bin"), os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"tw", "o-", "two"} {
		_, _ = f.WriteString(part)
		time.Sleep(100 * time.Millisecond)
	}
	f.Close()
	if err := os.Symlink("shard-1.bin", filepath.Join(src, "a", "latest")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	finish(t, ls)
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
	assertTree(t, dst, map[string]string{"a/shard-1.bin": "one", "a/shard-2.bin": "two-two"})
	if tgt, err := os.Readlink(filepath.Join(dst, "a", "latest")); err != nil || tgt != "shard-1.bin" {
		t.Fatalf("symlink = %q %v", tgt, err)
	}
	fi, _ := os.Stat(filepath.Join(dst, "a", "shard-1.bin"))
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode %o, want the download's 0640", fi.Mode().Perm())
	}
}

// A downloader that writes a temporary name and renames it: the stream
// moves its copy instead of copying the file again.
func TestStreamFollowsARenameWithoutASecondCopy(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	ls := newLiveSource(src, "uid-holder", fastStream())
	c := &countingClient{liveClient: localClient{ls}}
	done, _ := runStream(t, c, dst)
	write(t, src, ".incomplete/w.bin", "weights")
	time.Sleep(700 * time.Millisecond)
	if err := os.Rename(filepath.Join(src, ".incomplete", "w.bin"), filepath.Join(src, "w.bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(src, ".incomplete")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	finish(t, ls)
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
	assertTree(t, dst, map[string]string{"w.bin": "weights"})
	if _, err := os.Stat(filepath.Join(dst, ".incomplete")); !os.IsNotExist(err) {
		t.Fatal("the downloader's temporary directory is left")
	}
	if n := c.opens.Load(); n != 1 {
		t.Fatalf("the file was read %d times; want once (a rename is not a new file)", n)
	}
}

// A file rewritten after it settled and was copied, and a file deleted
// after it was copied: the end matches the final tree, nothing else.
func TestStreamEndsWithTheFinalTreeAfterRewritesAndDeletes(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	ls := newLiveSource(src, "uid-holder", fastStream())
	done, _ := runStream(t, localClient{ls}, dst)
	write(t, src, "keep.bin", "v1")
	write(t, src, "gone.bin", "temp")
	time.Sleep(700 * time.Millisecond)
	write(t, src, "keep.bin", "v2-longer")
	if err := os.Remove(filepath.Join(src, "gone.bin")); err != nil {
		t.Fatal(err)
	}
	finish(t, ls) // at once: keep.bin has not settled again
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
	assertTree(t, dst, map[string]string{"keep.bin": "v2-longer"})
}

// The source's final manifest is the only truth: a copy whose bytes do
// not match it is copied again.
func TestStreamRecopiesAFileWhoseCopyDoesNotMatchTheFinalManifest(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	ls := newLiveSource(src, "uid-holder", fastStream())
	done, _ := runStream(t, localClient{ls}, dst)
	write(t, src, "w.bin", "right")
	time.Sleep(700 * time.Millisecond)
	// Corrupt the stream's copy behind its back.
	if err := os.WriteFile(filepath.Join(dst, "w.bin"), []byte("wrong"), 0o640); err != nil {
		t.Fatal(err)
	}
	finish(t, ls)
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
	assertTree(t, dst, map[string]string{"w.bin": "right"})
}

func TestStreamFailsWhenTheSourceFails(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	ls := newLiveSource(src, "uid-holder", fastStream())
	done, _ := runStream(t, localClient{ls}, dst)
	write(t, src, "w.bin", "x")
	ls.fail()
	if err := waitDone(t, done); err == nil {
		t.Fatal("the stream ended without error on a failed source")
	}
}

func TestStreamFailsOnAStall(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	ls := newLiveSource(src, "uid-holder", fastStream())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tm := fastStream()
	tm.stall = 500 * time.Millisecond
	s := &streamer{client: localClient{ls}, dst: dst, timing: tm}
	done := make(chan error, 1)
	go func() { done <- s.run(ctx) }()
	if err := waitDone(t, done); err == nil {
		t.Fatal("no error after the source made no progress")
	}
}

// Finalizing refuses a tree that is not the one the download reported.
func TestLiveSourceFinalizeRefusesAnotherTree(t *testing.T) {
	src := t.TempDir()
	write(t, src, "w.bin", "x")
	ls := newLiveSource(src, "uid-holder", fastStream())
	if err := ls.finalize("not-this-tree"); err == nil {
		t.Fatal("finalized a tree the download did not report")
	}
	if v := ls.view(); v.State != liveFailed {
		t.Fatalf("state %q, want failed", v.State)
	}
}

// The final manifest carries every file's sha256, and once done the
// source serves only those files, and only while unchanged.
func TestLiveSourceServesOnlyUnchangedManifestFilesOnceDone(t *testing.T) {
	src := t.TempDir()
	write(t, src, "w.bin", "weights")
	ls := newLiveSource(src, "uid-holder", fastStream())
	finish(t, ls)
	v := ls.view()
	if v.State != liveDone || v.Final == nil || len(v.Final.Entries) != 1 || v.Final.Entries[0].SHA256 != sha("weights") {
		t.Fatalf("view %+v", v)
	}
	ctx := context.Background()
	if r, err := ls.open(ctx, "w.bin"); err != nil {
		t.Fatalf("open w.bin: %v", err)
	} else {
		r.Close()
	}
	write(t, src, "engine.log", "the engine wrote this")
	if r, err := ls.open(ctx, "engine.log"); err == nil {
		r.Close()
		t.Fatal("served a file the manifest does not list")
	}
	write(t, src, "w.bin", "changed")
	if r, err := ls.open(ctx, "w.bin"); err == nil {
		r.Close()
		t.Fatal("served a manifest file that changed")
	}
}

// Applying the manifest's owners and modes runs as root on the host: a
// path swapped for a symlink must not lead it outside the volume.
func TestApplyMetaStaysInTheVolume(t *testing.T) {
	dst, outside := t.TempDir(), t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("v"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dst, "f")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	m := modelcache.Manifest{Entries: []modelcache.Entry{{Path: "f", Type: modelcache.TypeFile, Mode: 0o777, UID: os.Getuid(), GID: os.Getgid()}}}
	_ = applyMetaRoot(root, m)
	if fi, _ := os.Stat(victim); fi.Mode().Perm() != 0o600 {
		t.Fatalf("a file outside the volume became %o", fi.Mode().Perm())
	}
}
