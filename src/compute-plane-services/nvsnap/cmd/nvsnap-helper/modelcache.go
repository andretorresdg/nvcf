// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dsx-ai-factory/nvcf/src/compute-plane-services/nvsnap/internal/modelcache"
)

// modelCacheCmd wraps a pod's model download init
// (docs/design/node-model-cache.md). The node's cache of this download is
// mounted read-only at --cache. When it holds a complete generation, the
// helper copies it into the download volume at --to, checking every file
// against the manifest, and the download does not run. Otherwise, or when
// anything about the copy is wrong, it runs the download exactly as the
// pod would have. The volume is the pod's own, unchanged, so the download
// can always run; the cache never fails the init.
//
// It waits only while the agent is visibly fetching the key from a peer
// (a fresh heartbeat), and never past --wait seconds in all.
func modelCacheCmd(args []string) (int, error) {
	fs := flag.NewFlagSet("model-cache", flag.ContinueOnError)
	cache := fs.String("cache", "", "the node's cache directory for this download, read-only")
	to := fs.String("to", "", "the download volume to fill")
	wait := fs.Int("wait", 600, "most seconds to wait for a peer fetch in progress")
	stale := fs.Int("stale", 20, "seconds after which the fetch heartbeat counts as dead")
	poll := fs.Duration("poll", 2*time.Second, "how often to look again while waiting")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	argv := fs.Args()
	if *cache == "" || *to == "" || len(argv) == 0 {
		return 2, errors.New("model-cache: --cache, --to and the download command are required")
	}
	if seedFromCache(*cache, *to, time.Duration(*wait)*time.Second, time.Duration(*stale)*time.Second, *poll) {
		report(modelcache.InitSeeded)
		return 0, nil
	}
	fmt.Println("nvsnap: model cache: downloading")
	code, err := run(argv)
	if code == 0 {
		// What the download left, for the agent: it keeps a copy only if
		// the volume still has exactly this tree when it reads it.
		msg := modelcache.InitDownloaded
		if ents, serr := modelcache.Scan(*to); serr == nil {
			msg += " " + modelcache.TreeField + modelcache.Fingerprint(ents)
		}
		report(msg)
	}
	return code, err
}

// terminationLog is where Kubernetes reads a container's termination
// message; a test points it elsewhere.
var terminationLog = "/dev/termination-log"

func report(msg string) { _ = os.WriteFile(terminationLog, []byte(msg), 0o644) }

// seedFromCache fills to from the cache and reports whether it did. It
// leaves to as it found it otherwise.
func seedFromCache(cache, to string, wait, stale, poll time.Duration) bool {
	if ents, err := os.ReadDir(to); err != nil || len(ents) > 0 {
		// A restarted init finds what its last run downloaded; the
		// download knows how to go on from it.
		fmt.Println("nvsnap: model cache: the volume already has content; the download goes on from it")
		return false
	}
	deadline := time.Now().Add(wait)
	for {
		if gen, m, ok := modelcache.LatestIn(cache); ok {
			start := time.Now()
			err := modelcache.Seed(gen, m, to)
			if err == nil {
				fmt.Printf("nvsnap: model cache: seeded %d bytes from %s in %s\n", m.Bytes, filepath.Base(gen), time.Since(start).Round(time.Millisecond))
				return true
			}
			fmt.Println("nvsnap: model cache: the cached copy is unusable:", err)
			emptyDir(to)
			return false
		}
		if !heartbeatFresh(filepath.Join(cache, modelcache.IncomingFile), stale) {
			fmt.Println("nvsnap: model cache: no copy on this node")
			return false
		}
		if time.Now().After(deadline) {
			fmt.Printf("nvsnap: model cache: no copy after %s\n", wait)
			return false
		}
		time.Sleep(poll)
	}
}

func heartbeatFresh(p string, stale time.Duration) bool {
	fi, err := os.Stat(p)
	return err == nil && time.Since(fi.ModTime()) < stale
}

// emptyDir removes everything under dir, leaving dir: it is the volume's
// mount point.
func emptyDir(dir string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		// A directory a failed copy made read-only would keep its children.
		_ = filepath.Walk(p, func(q string, fi os.FileInfo, err error) error {
			if err == nil && fi.IsDir() {
				_ = os.Chmod(q, 0o700)
			}
			return nil
		})
		_ = os.RemoveAll(p)
	}
}
