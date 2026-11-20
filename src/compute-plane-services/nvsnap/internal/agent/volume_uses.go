// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

// The mounts a dump's processes use, read from the dump itself, for a
// group capture's decision on which setup its placeholders may skip
// (webhook/unused_volumes.go). The dump records every file its processes
// hold: open descriptors, memory mappings, cwd and root, the executable,
// deleted files (ghosts), each with the id of the mount it is on, and the
// mount table those ids resolve in. A path-bound unix socket is on the
// mount whose mountpoint is the longest prefix of its path.
//
// Anything the reading cannot account for (a descriptor kind that names a
// file another way, a mount id the table does not have, more than one
// mount namespace) makes the use unknown, and the group skips nothing.

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// pathlessFileTypes are the descriptor kinds of a dump's files.img that
// name no file of a mount: sockets on the network, pipes, anonymous
// descriptors. A FIFO's path is a regular-file entry of its own.
var pathlessFileTypes = map[string]bool{
	"INETSK": true, "PACKETSK": true, "NETLINKSK": true,
	"PIPE": true, "FIFO": true, "EVENTFD": true, "EVENTPOLL": true,
	"SIGNALFD": true, "TIMERFD": true, "MEMFD": true, "IO_URING": true,
	"TUNF": true, "EXT": true, "NS": true, "BPFMAP": true, "PIDFD": true,
}

// critShow decodes a CRIU image to JSON; a variable for tests.
var critShow = func(img string) ([]byte, error) {
	return exec.Command("crit", "show", img).Output()
}

// dumpUsedMounts returns the mountpoints, in the dumped container's view,
// of the mounts the processes of the dump in dir use.
func dumpUsedMounts(dir string) ([]string, error) {
	mps, err := filepath.Glob(filepath.Join(dir, "mountpoints-*.img"))
	if err != nil {
		return nil, err
	}
	if len(mps) != 1 {
		return nil, fmt.Errorf("%d mount namespaces in the dump; one is understood", len(mps))
	}
	mounts, err := critShow(mps[0])
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", filepath.Base(mps[0]), err)
	}
	files, err := critShow(filepath.Join(dir, "files.img"))
	if err != nil {
		return nil, fmt.Errorf("decode files.img: %w", err)
	}
	return usedMounts(files, mounts)
}

// usedMounts is dumpUsedMounts on the decoded files.img and mount table.
func usedMounts(filesJSON, mountsJSON []byte) ([]string, error) {
	var mt struct {
		Entries []struct {
			MntID      uint32 `json:"mnt_id"`
			Mountpoint string `json:"mountpoint"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(mountsJSON, &mt); err != nil {
		return nil, fmt.Errorf("mount table: %w", err)
	}
	byID := map[uint32]string{}
	var points []string
	for _, e := range mt.Entries {
		mp := path.Clean(e.Mountpoint)
		byID[e.MntID] = mp
		points = append(points, mp)
	}
	var fs struct {
		Entries []struct {
			Type string `json:"type"`
			Reg  *struct {
				Name  string `json:"name"`
				MntID *int64 `json:"mnt_id"`
			} `json:"reg"`
			Usk *struct {
				Name string `json:"name"`
			} `json:"usk"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(filesJSON, &fs); err != nil {
		return nil, fmt.Errorf("files: %w", err)
	}
	used := map[string]bool{}
	for _, e := range fs.Entries {
		switch {
		case e.Type == "REG" && e.Reg != nil:
			if e.Reg.MntID == nil || *e.Reg.MntID < 0 {
				return nil, fmt.Errorf("file %q has no mount id", e.Reg.Name)
			}
			mp, ok := byID[uint32(*e.Reg.MntID)]
			if !ok {
				return nil, fmt.Errorf("file %q is on mount %d, not in the dump's mount table", e.Reg.Name, *e.Reg.MntID)
			}
			used[mp] = true
		case e.Type == "UNIXSK" && e.Usk != nil:
			name := e.Usk.Name
			if i := strings.Index(name, "=00"); i >= 0 {
				name = name[:i] // CRIU's encoding of the trailing NUL
			}
			if !strings.HasPrefix(name, "/") {
				continue // abstract or unbound
			}
			if mp := longestMountPrefix(points, path.Clean(name)); mp != "" {
				used[mp] = true
			}
		case pathlessFileTypes[e.Type]:
		default:
			return nil, fmt.Errorf("descriptor kind %s is not understood", e.Type)
		}
	}
	out := make([]string, 0, len(used))
	for mp := range used {
		out = append(out, mp)
	}
	sort.Strings(out)
	return out, nil
}

// longestMountPrefix returns the mountpoint among points that holds p.
func longestMountPrefix(points []string, p string) string {
	best := ""
	for _, mp := range points {
		if (p == mp || mp == "/" || strings.HasPrefix(p, mp+"/")) && len(mp) > len(best) {
			best = mp
		}
	}
	return best
}
