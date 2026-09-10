package assemble

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tbereknyei/nixgg/internal/drvref"
)

func writeStub(t *testing.T, path, drvPath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(drvref.Body(drvPath)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWalkFindsStubsAtCorrectRelPaths(t *testing.T) {
	root := t.TempDir()
	writeStub(t, filepath.Join(root, "src", "hello.o"), "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-tu-hello.o.drv")
	writeStub(t, filepath.Join(root, "bin", "hello"), "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-bin-hello.drv")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("not a stub"), 0o644); err != nil {
		t.Fatal(err)
	}

	stubs, err := Walk(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(stubs) != 2 {
		t.Fatalf("expected 2 stubs, got %d: %+v", len(stubs), stubs)
	}
	got := map[string]string{}
	for _, s := range stubs {
		got[s.RelPath] = s.DrvPath
	}
	if got["src/hello.o"] != "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-tu-hello.o.drv" {
		t.Errorf("src/hello.o -> %q", got["src/hello.o"])
	}
	if got["bin/hello"] != "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-bin-hello.drv" {
		t.Errorf("bin/hello -> %q", got["bin/hello"])
	}
}

func TestWalkSkipsNixSocket(t *testing.T) {
	root := t.TempDir()
	writeStub(t, filepath.Join(root, "bin", "hello"), "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-bin-hello.drv")
	if err := os.WriteFile(filepath.Join(root, ".nix-socket"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	stubs, err := Walk(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(stubs) != 1 || stubs[0].RelPath != "bin/hello" {
		t.Fatalf("expected only bin/hello, got %+v", stubs)
	}
}

func TestWalkDeterministicOrder(t *testing.T) {
	root := t.TempDir()
	writeStub(t, filepath.Join(root, "z.o"), "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-z.drv")
	writeStub(t, filepath.Join(root, "a.o"), "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-a.drv")

	stubs, err := Walk(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(stubs) != 2 || stubs[0].RelPath != "a.o" || stubs[1].RelPath != "z.o" {
		t.Fatalf("expected lexical order [a.o z.o], got %+v", stubs)
	}
}

func TestStageForScanExcludesNixSocket(t *testing.T) {
	root := t.TempDir()
	writeStub(t, filepath.Join(root, "bin", "hello"), "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-bin-hello.drv")
	if err := os.WriteFile(filepath.Join(root, ".nix-socket"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("docs"), 0o644); err != nil {
		t.Fatal(err)
	}

	staged, err := StageForScan(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(staged, ".nix-socket")); !os.IsNotExist(err) {
		t.Errorf(".nix-socket should not be staged, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(staged, "bin", "hello")); err != nil {
		t.Errorf("bin/hello should be staged: %v", err)
	}
	if _, err := os.Stat(filepath.Join(staged, "README.md")); err != nil {
		t.Errorf("README.md should be staged: %v", err)
	}
}

func TestStageForScanPreservesSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "libfoo.so.1.2.3"), []byte("elf"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("libfoo.so.1.2.3", filepath.Join(root, "libfoo.so")); err != nil {
		t.Fatal(err)
	}

	staged, err := StageForScan(root)
	if err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(staged, "libfoo.so"))
	if err != nil {
		t.Fatalf("libfoo.so should remain a symlink: %v", err)
	}
	if target != "libfoo.so.1.2.3" {
		t.Errorf("libfoo.so -> %q, want libfoo.so.1.2.3", target)
	}
}

func TestStageForScanCopiesReadOnlySourceDirs(t *testing.T) {
	// The regression this pins: an untouched subtree of the build
	// tree (e.g. LLVM's cmake/ dir, never written to during the
	// build) still carries its Nix-store read-only permission bits
	// (dr-xr-xr-x) at scan time. copyRecursive used to create dst
	// with that same read-only mode via MkdirAll(dst,
	// info.Mode().Perm()), so its own subsequent recursive calls
	// populating dst's children failed with "permission denied" —
	// confirmed directly building llvm-dyndrv (4013/4013 TUs
	// compiled, then failed at this exact step). dst is disposable
	// scratch space consumed only by `nix store add --scan`, so the
	// exact mode doesn't matter as long as this function can write
	// into it.
	root := t.TempDir()
	roDir := filepath.Join(root, "cmake")
	if err := os.MkdirAll(roDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roDir, "config.cmake"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(roDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(roDir, 0o755) })

	staged, err := StageForScan(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(staged, "cmake", "config.cmake")); err != nil {
		t.Errorf("cmake/config.cmake should be staged: %v", err)
	}
}

func TestStageForScanIsSelfExcluding(t *testing.T) {
	// The regression this pins: StageForScan used to accept a
	// caller-supplied dest, which — inside a real builder-rpc-v0
	// sandbox where $TMPDIR resolves under root — could itself land
	// under root. Copying root's entries into a destination that is
	// ALSO one of root's own entries recursed into itself at every
	// level until the kernel refused with "file name too long"
	// (confirmed directly building hello-dyndrv). StageForScan now
	// always stages at a FIXED, excluded name directly under root,
	// which by construction cannot be copied into itself: this test
	// pins that the staged tree contains no infinite ".gg-stage"
	// nesting no matter how many times StageForScan runs against the
	// same root.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	staged, err := StageForScan(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(staged, ".gg-stage")); !os.IsNotExist(err) {
		t.Errorf("staged tree should not contain a nested .gg-stage, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(staged, "a.txt")); err != nil {
		t.Errorf("a.txt should be staged: %v", err)
	}
}

// .nixgg is nixgg's own scratch dir. Capturing it into the assembled
// tree is expensive at scale: under shared staging it holds one symlink
// farm per translation unit, and `nix store add --scan` records a
// reference to every file object they point at.
//
// Neither Walk nor StageForScan may look inside it.
func TestNixggScratchDirIsExcluded(t *testing.T) {
	root := t.TempDir()
	// A staged farm at the depth it actually occurs: nixgg puts its
	// scratch dir at the project root, which is usually several levels
	// down — NOT the top level. A version of this test using the top
	// level passes against code that only filters the outer ReadDir.
	farm := filepath.Join(root, "src-1.0", "build", ".nixgg", "srcs", "tu0")
	if err := os.MkdirAll(farm, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nix/store/aaa-hdr.h", filepath.Join(farm, "hdr.h")); err != nil {
		t.Fatal(err)
	}
	// A thunk, and a stub that must NOT be mistaken for build output.
	if err := os.WriteFile(filepath.Join(root, "src-1.0", "build", ".nixgg", "scan-cache"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Real output alongside it.
	if err := os.WriteFile(filepath.Join(root, "src-1.0", "build", "real.txt"), []byte("out"), 0o644); err != nil {
		t.Fatal(err)
	}

	staged, err := StageForScan(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(staged, "src-1.0", "build", ".nixgg")); !os.IsNotExist(err) {
		t.Error(".nixgg was copied into the staged tree; its closure would pull in " +
			"every staged source object")
	}
	if _, err := os.Lstat(filepath.Join(staged, "src-1.0", "build", "real.txt")); err != nil {
		t.Errorf("real build output was not staged: %v", err)
	}
}

// A drvref stub's content is a /nix/store/….drv path, and
// `nix store add --scan` records every store path it finds. Copying
// stubs verbatim therefore makes the captured tree reference every
// producing derivation — and a derivation's closure contains its own
// inputs, so for a TU that is its staged source farm and every header in
// it.
//
// At scale that is tens of thousands of stubs pulling in the whole
// staging closure, which Nix bind-mounts into every derivation that
// consumes the tree — enough to exceed the kernel mount limit and fail
// as "No space left on device" on a disk that is nowhere near full.
//
// Walk runs before StageForScan and already holds every drv path, and
// the assembly overlays the real artifact over each stub, so the staged
// bytes are dead. Blank them.
func TestStageForScanBlanksDrvrefStubs(t *testing.T) {
	root := t.TempDir()
	drvPath := "/nix/store/00000000000000000000000000000000-tu-x.o.drv"
	stub := filepath.Join(root, "x.o")
	if err := os.WriteFile(stub, []byte(drvref.Body(drvPath)), 0o644); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "real.txt")
	if err := os.WriteFile(real, []byte("genuine output"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Walk must still see the stub: it runs first and is what the
	// assembly is built from.
	stubs, err := Walk(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(stubs) != 1 || stubs[0].DrvPath != drvPath {
		t.Fatalf("Walk lost the stub: %+v", stubs)
	}

	staged, err := StageForScan(root)
	if err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(staged, "x.o"))
	if err != nil {
		t.Fatalf("staged stub missing entirely; the overlay needs the path to exist: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("staged stub still carries %d bytes (%q) — --scan will record "+
			"a reference to the drv and drag in its whole input closure",
			len(got), string(got))
	}
	// Real output must be untouched.
	if b, err := os.ReadFile(filepath.Join(staged, "real.txt")); err != nil || string(b) != "genuine output" {
		t.Errorf("real build output was altered: %q %v", string(b), err)
	}
}
