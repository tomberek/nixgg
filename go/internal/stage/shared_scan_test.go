package stage

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tbereknyei/nixgg/internal/paths"
)

// Shared staging replaces every staged file with a symlink into a
// content-addressed store object, so a farm's hash depends on its headers
// only INDIRECTLY — through the target path strings in the NAR. Copying got
// content-tracking for free because the bytes were in the NAR; symlinking
// gets it only because the target path is itself derived from content. If
// that ever broke, a header edit would leave the farm's hash unchanged and
// every dependent TU would reuse a stale compile — silently, and with no
// build failure to point at it.
//
// TestSourcesShared covers the tree SHAPE with a fake store. This covers
// the hashing, which needs a real one.
//
// The other half of the scheme — that `nix store add --scan` records
// symlink targets as REFERENCES — cannot be tested here: --scan only works
// inside a recursive-nix derivation builder. It is pinned end-to-end by
// tests/shared-closure.sh, which also documents why that half is less
// fragile than it looks (Nix mounts only an input's closure, so a missing
// reference fails the compile outright rather than silently).
func TestSourcesSharedHashTracksContent(t *testing.T) {
	nixBin := requireNix(t)
	isolatedStore(t)

	farmFor := func(content string) string {
		root := t.TempDir()
		src := filepath.Join(root, "src")
		if err := os.MkdirAll(src, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, "hdr.h"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		store := func(abs string) (string, error) {
			return runNix(t, nixBin, "store", "add", "-n", filepath.Base(abs), abs)
		}
		l := paths.Layout{Srcs: filepath.Join(root, "srcs")}
		res, err := SourcesShared(l, "tu0", []Entry{
			{Abs: filepath.Join(src, "hdr.h"), Rel: "hdr.h"},
		}, store)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Readlink(filepath.Join(res, "hdr.h")); err != nil {
			t.Fatalf("staged hdr.h is not a symlink: %v", err)
		}
		// Plain `store add` (no --scan): references are metadata and do not
		// participate in this path computation, so the hashing property is
		// observable outside a builder even though --scan is not.
		farm, err := runNix(t, nixBin, "store", "add", "-n", "farm", res)
		if err != nil {
			t.Fatalf("store-add farm: %v", err)
		}
		return farm
	}

	a1 := farmFor("#define V 1\n")
	a2 := farmFor("#define V 1\n")
	b := farmFor("#define V 2\n")

	if a1 != a2 {
		t.Errorf("identical content produced different farms:\n  %s\n  %s\n"+
			"Staged trees must be reproducible or nothing caches across machines.", a1, a2)
	}
	if a1 == b {
		t.Errorf("changed header content did NOT change the farm hash (%s).\n"+
			"Dependents would reuse a stale compile.", a1)
	}
}

func runNix(t *testing.T, nixBin string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(nixBin, append([]string{"--offline"}, args...)...)
	cmd.Env = os.Environ()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", errWith(err, stderr.String())
	}
	return strings.TrimSpace(string(out)), nil
}

func errWith(err error, stderr string) error {
	if stderr == "" {
		return err
	}
	return &nixErr{err: err, stderr: stderr}
}

type nixErr struct {
	err    error
	stderr string
}

func (e *nixErr) Error() string { return e.err.Error() + "\n" + e.stderr }

// requireNix locates a nix binary, preferring the repo's patched build,
// and skips when none is available so `go test ./...` still passes on a
// machine without it.
func requireNix(t *testing.T) string {
	t.Helper()
	if p, err := filepath.Abs("../../../.patched-nix/bin/nix"); err == nil {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	p, err := exec.LookPath("nix")
	if err != nil {
		t.Skip("no nix binary available; skipping store-backed test")
	}
	return p
}

// isolatedStore points nix at a throwaway store root so the test never
// writes to the developer's real store or needs a daemon.
func isolatedStore(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	// Store paths are read-only, so t.TempDir's own cleanup cannot remove
	// them. Cleanups run LIFO, so this one restores write permission just
	// before that removal.
	t.Cleanup(func() {
		filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err == nil {
				os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	t.Setenv("NIX_REMOTE", "local?root="+root)
	t.Setenv("NIX_CONFIG", "experimental-features = nix-command")
}
