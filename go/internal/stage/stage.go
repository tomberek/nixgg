// Package stage materialises the source tree for a single compile into
// .nixgg/srcs/<tu-id>/, using hardlinks to share inodes with the working
// tree. The staging dir is referenced from the thunk as a relative Nix
// path (`srcTree = ../srcs/<tu-id>;`) so Nix imports it into the store
// at eval time — no `nix store add` in the shim.
//
// Reuse policy: the dir already exists AND every recorded entry's inode
// matches the current original AND no stale files remain → keep as-is.
// Anything else → nuke and repopulate. Inodes are the correctness
// signal: editors that write-then-rename break the hardlink, so the
// original's inode changes and we detect it.
package stage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/tbereknyei/nixgg/internal/paths"
)

// Entry pairs an absolute path in the working tree with the relative
// path where it should appear under the staging root.
type Entry struct {
	Abs, Rel string
}

// Sources materialises `entries` into l.Srcs/tuID/. Idempotent, safe to
// call concurrently on different tuIDs, MUST NOT be called concurrently
// on the same tuID (we don't lock). Returns the absolute path to
// .nixgg/srcs/<tu-id>/.
func Sources(l paths.Layout, tuID string, entries []Entry) (string, error) {
	dir := filepath.Join(l.Srcs, tuID)
	if err := os.MkdirAll(l.Srcs, 0o755); err != nil {
		return "", err
	}

	if reuseOK(dir, entries) {
		return dir, nil
	}

	// Rebuild from scratch. Cheaper than trying to reconcile — hardlinks
	// are essentially free.
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for _, e := range entries {
		if err := hardlinkOrCopy(e.Abs, filepath.Join(dir, e.Rel)); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// FileEntry pairs a relative staging path with the exact bytes that
// should exist there. Content is already in memory (the caller read
// it off disk itself, before anything downstream could turn the
// source into a stub) — this just writes it out, unconditionally,
// under a caller-chosen ID.
type FileEntry struct {
	Rel     string
	Content []byte
}

// ContentFiles materialises `entries` into l.Srcs/id/ by writing bytes
// directly — no hardlinking, since there's no original file on disk
// this content is guaranteed to still match (the caller already read
// it once; re-reading later could race with whatever generated it).
// Always rewrites; content this small doesn't benefit from the
// inode-reuse dance Sources does for real source trees.
func ContentFiles(l paths.Layout, id string, entries []FileEntry) (string, error) {
	dir := filepath.Join(l.Srcs, id)
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for _, e := range entries {
		dst := filepath.Join(dir, e.Rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(dst, e.Content, 0o644); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// reuseOK returns true iff dir has exactly the requested entries, each
// hardlinked to its current original.
func reuseOK(dir string, entries []Entry) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}

	// Inode-match every requested entry.
	want := make(map[string]bool, len(entries))
	for _, e := range entries {
		want[e.Rel] = true
		orig, err := statInode(e.Abs)
		if err != nil {
			return false
		}
		staged, err := statInode(filepath.Join(dir, e.Rel))
		if err != nil {
			return false
		}
		if orig != staged {
			return false
		}
	}

	// Guard against stale files (a header was #include-removed).
	have, err := listAllRelFiles(dir)
	if err != nil {
		return false
	}
	if len(have) != len(want) {
		return false
	}
	for _, rel := range have {
		if !want[rel] {
			return false
		}
	}
	return true
}

func statInode(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("stat: %s: no syscall.Stat_t", path)
	}
	return sys.Ino, nil
}

func listAllRelFiles(root string) ([]string, error) {
	var out []string
	prefix := root + string(filepath.Separator)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		out = append(out, strings.TrimPrefix(path, prefix))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// hardlinkOrCopy tries os.Link first, falls back to a byte copy if the
// filesystems differ (EXDEV) or hardlinks are otherwise forbidden.
// We intentionally do NOT chmod the result — it's hardlinked to the
// original, so mode changes would affect the working tree.
//
// src is resolved through any symlink chain first (filepath.
// EvalSymlinks) — the destination always ends up a real regular
// file, never a symlink. A staged symlink's relative target text is
// only correct from its ORIGINAL absolute location; once staged
// under a project root at a different depth than the original (a
// narrower root is common — the scanner's own projectRoot is the
// common ancestor of cwd + -I dirs, not necessarily the repo root),
// that same text resolves to a path the staged tree never contains.
// Confirmed directly building PostgreSQL's src/backend: `./configure`
// creates `src/include/pg_config_os.h -> ../../src/include/port/
// linux.h`; staged under a narrower root, the compile failed with a
// plain "No such file or directory" on the header, because the
// symlink's TARGET was never staged as its own entry (the scanner
// only ever recorded the symlink's own path as a dependency). Since
// the compiler only needs real byte content at the header's expected
// path — never the symlink itself as a filesystem object — resolving
// to the real file up front sidesteps needing the scanner to also
// discover and stage the target separately, and needs no change
// there at all.
func hardlinkOrCopy(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(src); err == nil {
		src = resolved
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	} else if !isCrossDeviceOrEPERM(err) {
		return err
	}
	// Cross-fs fallback.
	sf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sf.Close()
	df, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer df.Close()
	if _, err := io.Copy(df, sf); err != nil {
		return err
	}
	return nil
}

func isCrossDeviceOrEPERM(err error) bool {
	var lerr *os.LinkError
	if errors.As(err, &lerr) {
		if errno, ok := lerr.Err.(syscall.Errno); ok {
			return errno == syscall.EXDEV || errno == syscall.EPERM
		}
	}
	return false
}

// TUID returns a filesystem-safe cache-dir name for a compile whose
// output resolves to absOutput. Two different compiles must never
// collide, and calls from different cwds that happen to have the same
// -o basename (e.g. redis's `deps/hiredis/sds.o` vs `redis/src/sds.o`)
// must produce different IDs.
//
// We hash the absolute output path and prefix with a short human-
// readable slug so cache dirs are still greppable.
func TUID(absOutput string) string {
	// Human-readable stem — the output basename, minus any .o/.xo/.lo suffix.
	base := filepath.Base(absOutput)
	stem := strings.TrimSuffix(base, ".o")
	stem = strings.TrimSuffix(stem, ".xo")
	stem = strings.TrimSuffix(stem, ".lo")
	// Scrub the slug to filesystem-safe chars.
	var b strings.Builder
	b.Grow(len(stem))
	for _, r := range stem {
		switch {
		case r == '.' || r == '_' || r == '-' ||
			(r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	slug := b.String()
	if slug == "" {
		slug = "tu"
	}
	// Uniqueness comes from the abs-path hash (first 12 hex chars is
	// plenty for a filesystem-local dir name).
	h := sha256.Sum256([]byte(absOutput))
	return slug + "-" + hex.EncodeToString(h[:6])
}

// SourcesShared stages entries as SYMLINKS into per-file store objects
// rather than hardlinked copies.
//
// The staged tree keeps its exact shape — same relative paths, so
// `#include "../foo.h"` and the caller's -I flags resolve unchanged —
// but its bytes collapse from the full header closure to one symlink
// entry each. A TU staging ~190 headers becomes ~190
// symlinks / a few KB, and the header content is stored once no matter
// how many TUs include it.
//
// That matters because the closure is overwhelmingly shared: across 60
// sampled translation units the same headers recurred roughly 17x, and
// a per-TU copy pays that duplication in full every time.
//
// store maps an absolute path to the store object holding its content;
// the caller supplies it so this package stays free of nix plumbing.
// Returns the absolute path to .nixgg/srcs/<tu-id>/, like Sources.
func SourcesShared(l paths.Layout, tuID string, entries []Entry, store func(abs string) (string, error)) (string, error) {
	dir := filepath.Join(l.Srcs, tuID)
	if err := os.MkdirAll(l.Srcs, 0o755); err != nil {
		return "", err
	}
	// No reuse check: the symlink targets are content-addressed, so a
	// changed header yields a different target and the cheap thing is to
	// rebuild the (tiny) farm.
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for _, e := range entries {
		sp, err := store(e.Abs)
		if err != nil {
			return "", fmt.Errorf("share %s: %w", e.Abs, err)
		}
		dst := filepath.Join(dir, e.Rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		if err := os.Symlink(sp, dst); err != nil {
			return "", err
		}
	}
	return dir, nil
}
