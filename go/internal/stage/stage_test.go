package stage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tbereknyei/nixgg/internal/paths"
)

// TestSourcesResolvesRelativeSymlinkTargets pins the regression this
// fix addresses: a relative symlink staged into a rebased project
// root (narrower than its original directory) must still resolve to
// real content. PostgreSQL's own ./configure creates exactly this
// shape (src/include/pg_config_os.h -> ../../src/include/port/
// linux.h) — confirmed directly to break real compiles before this
// fix, both because the relative target text no longer resolves from
// the staged location AND because the symlink's target was never
// staged as its own entry in the first place (the header scanner
// only records the symlink's own path as a dependency). Resolving
// through the symlink chain before hardlinking sidesteps both: the
// staged entry is the real file's content directly, no symlink
// involved.
func TestSourcesResolvesRelativeSymlinkTargets(t *testing.T) {
	root := t.TempDir()

	// Mirror postgres's own layout: symlink and target are siblings
	// under a common directory two levels up from where the symlink
	// itself lives, so ".." actually matters.
	targetDir := filepath.Join(root, "src", "include", "port")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	targetFile := filepath.Join(targetDir, "linux.h")
	if err := os.WriteFile(targetFile, []byte("/* linux port header */\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	linkDir := filepath.Join(root, "src", "include")
	linkPath := filepath.Join(linkDir, "pg_config_os.h")
	if err := os.Symlink("../../src/include/port/linux.h", linkPath); err != nil {
		t.Fatal(err)
	}

	l := testLayout(t)

	// Stage the symlink under a project root NARROWER than its
	// original location (root/src, not root) — the rebasing that
	// breaks a relative target's own text.
	entries := []Entry{
		{Abs: linkPath, Rel: "include/pg_config_os.h"},
	}
	dir, err := Sources(l, "pg-symlink-test", entries)
	if err != nil {
		t.Fatal(err)
	}

	staged := filepath.Join(dir, "include", "pg_config_os.h")
	info, err := os.Lstat(staged)
	if err != nil {
		t.Fatalf("staged entry missing: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("expected %s to be a real file, not a symlink", staged)
	}

	body, err := os.ReadFile(staged)
	if err != nil {
		t.Fatalf("staged entry does not resolve: %v", err)
	}
	if string(body) != "/* linux port header */\n" {
		t.Fatalf("unexpected content: %q", body)
	}
}

// TestSourcesResolvesAbsoluteSymlinkTargets pins the same resolution
// for an already-absolute symlink target.
func TestSourcesResolvesAbsoluteSymlinkTargets(t *testing.T) {
	root := t.TempDir()
	targetFile := filepath.Join(root, "target.txt")
	if err := os.WriteFile(targetFile, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(root, "link.txt")
	if err := os.Symlink(targetFile, linkPath); err != nil {
		t.Fatal(err)
	}

	l := testLayout(t)
	entries := []Entry{{Abs: linkPath, Rel: "link.txt"}}
	dir, err := Sources(l, "abs-symlink-test", entries)
	if err != nil {
		t.Fatal(err)
	}

	staged := filepath.Join(dir, "link.txt")
	info, err := os.Lstat(staged)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("expected %s to be a real file, not a symlink", staged)
	}
	body, err := os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello\n" {
		t.Fatalf("unexpected content: %q", body)
	}
}

func testLayout(t *testing.T) paths.Layout {
	t.Helper()
	base := t.TempDir()
	return paths.Layout{Srcs: filepath.Join(base, "srcs")}
}

// SourcesShared must reproduce the tree's SHAPE exactly — relative
// paths are what make `#include "../foo.h"` and the caller's -I flags
// resolve — while replacing contents with symlinks into shared store
// objects.
func TestSourcesShared(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"a.c", "sub/dup.h", "other.h"} {
		if err := os.WriteFile(filepath.Join(src, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l := paths.Layout{Srcs: filepath.Join(root, "srcs")}
	entries := []Entry{
		{Abs: filepath.Join(src, "a.c"), Rel: "a.c"},
		{Abs: filepath.Join(src, "sub/dup.h"), Rel: "sub/dup.h"},
		{Abs: filepath.Join(src, "other.h"), Rel: "other.h"},
	}

	// Stand-in for the store: identical content collapses to one object,
	// which is the property the real implementation gets from
	// content-addressing.
	calls := 0
	store := func(abs string) (string, error) {
		calls++
		return "/nix/store/deadbeef-" + filepath.Base(abs), nil
	}

	res, err := SourcesShared(l, "tu-1", entries, store)
	if err != nil {
		t.Fatalf("SourcesShared: %v", err)
	}
	if calls != 3 {
		t.Errorf("store called %d times, want 3", calls)
	}
	for _, e := range entries {
		p := filepath.Join(res, e.Rel)
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatalf("%s missing: %v", e.Rel, err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s is not a symlink — the whole point is to stop copying", e.Rel)
		}
		tgt, _ := os.Readlink(p)
		if !strings.HasPrefix(tgt, "/nix/store/") {
			t.Errorf("%s -> %q, want a store path", e.Rel, tgt)
		}
	}
}
