// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package modelcache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"

	"golang.org/x/sys/unix"
)

// node is one entry the walker reached: its path in the tree, its own
// status (not a symlink's target's), and the open directory it is in.
type node struct {
	rel   string
	st    unix.Stat_t
	dirfd int
	name  string
}

// walkNoFollow calls fn for every entry under root, parents before
// children, names in order. It never follows a symlink, anywhere in a
// path: each entry is reached through its parent's open descriptor, and a
// directory is entered only if the one opened is the one examined. A tree
// that a running workload changes under the walk therefore cannot lead it
// outside the tree (a directory swapped for a symlink to /), only to an
// error.
func walkNoFollow(root string, fn func(n node) error) error {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return walkDirFD(fd, "", fn)
}

func walkDirFD(dirfd int, prefix string, fn func(n node) error) error {
	names, err := readNames(dirfd)
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		var st unix.Stat_t
		if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		rel := name
		if prefix != "" {
			rel = prefix + "/" + name
		}
		if err := fn(node{rel: rel, st: st, dirfd: dirfd, name: name}); err != nil {
			return err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			continue
		}
		sub, err := openSame(dirfd, name, &st, unix.O_DIRECTORY)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		err = walkDirFD(sub, rel, fn)
		unix.Close(sub)
		if err != nil {
			return err
		}
	}
	return nil
}

// openSame opens name in dirfd without following a symlink, and checks it
// is the entry st described.
func openSame(dirfd int, name string, st *unix.Stat_t, flags int) (int, error) {
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC|flags, 0)
	if err != nil {
		return -1, err
	}
	var now unix.Stat_t
	if err := unix.Fstat(fd, &now); err != nil {
		unix.Close(fd)
		return -1, err
	}
	if now.Ino != st.Ino || now.Dev != st.Dev || now.Mode&unix.S_IFMT != st.Mode&unix.S_IFMT {
		unix.Close(fd)
		return -1, ErrChanged
	}
	return fd, nil
}

func readNames(dirfd int) ([]string, error) {
	dup, err := unix.Dup(dirfd)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(dup), "")
	defer f.Close()
	return f.Readdirnames(-1)
}

// entryOf describes n, without its checksum.
func entryOf(n node, target string) (Entry, error) {
	e := Entry{Path: n.rel, Mode: n.st.Mode & 0o7777, UID: int(n.st.Uid), GID: int(n.st.Gid), Mtime: n.st.Mtim.Nano()}
	switch n.st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		e.Type = TypeDir
	case unix.S_IFREG:
		e.Type, e.Size = TypeFile, n.st.Size
	case unix.S_IFLNK:
		e.Type, e.Target, e.Mode = TypeSymlink, target, 0
	default:
		return e, fmt.Errorf("%w: %s has file type %o", ErrUnsupported, n.rel, n.st.Mode&unix.S_IFMT)
	}
	return e, nil
}

func readLink(n node) (string, error) {
	buf := make([]byte, 4096)
	k, err := unix.Readlinkat(n.dirfd, n.name, buf)
	if err != nil {
		return "", err
	}
	if k >= len(buf) {
		return "", fmt.Errorf("%w: symlink %s target too long", ErrUnsupported, n.rel)
	}
	return string(buf[:k]), nil
}

// Scan describes the tree at root without reading file contents: every
// entry's path, type, size, mode, owner, mtime and link target.
func Scan(root string) ([]Entry, error) {
	var out []Entry
	err := walkNoFollow(root, func(n node) error {
		var target string
		if n.st.Mode&unix.S_IFMT == unix.S_IFLNK {
			t, err := readLink(n)
			if err != nil {
				return err
			}
			target = t
		}
		e, err := entryOf(n, target)
		if err != nil {
			return err
		}
		out = append(out, e)
		return nil
	})
	return out, err
}

// Fingerprint identifies a tree's shape and metadata: every entry's path,
// type, size, mode, owner, mtime and link target, in walk order. The
// download init records it when its download finishes; a harvest
// publishes only a copy with the same fingerprint, so nothing the engine
// changed afterwards (a file removed, added, rewritten) is cached.
func Fingerprint(entries []Entry) string {
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s\t%d:%s\t%d\t%o\t%d\t%d\t%d\t%d:%s\n",
			e.Type, len(e.Path), e.Path, e.Size, e.Mode, e.UID, e.GID, e.Mtime, len(e.Target), e.Target)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// AllocEstimate is the disk a tree takes, rounding each file to 4 KiB and
// counting 4 KiB per entry for its inode and directory slot, so a tree of
// many small files is not reserved as if it were their byte count.
func AllocEstimate(entries []Entry) int64 {
	const block = 4096
	total := int64(block) // the manifest
	for _, e := range entries {
		total += block + (e.Size+block-1)/block*block
	}
	return total
}
