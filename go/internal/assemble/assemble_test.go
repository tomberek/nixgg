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
	// Nix-store read-only dirs (dr-xr-xr-x) used to get copied with that same mode, so populating their children failed with "permission denied" (confirmed on llvm-dyndrv).
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
	// StageForScan used to accept a caller-supplied dest, which under a sandboxed $TMPDIR could land under root, recursing into itself until "file name too long" (confirmed on hello-dyndrv). It now always stages at a fixed, excluded name.
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

// .nixgg is nixgg's own scratch dir (staged source trees, thunks,
// memo caches) — never real build output, but capturing it drags
// every store path a memo file's own content names into the captured
// tree's closure. Neither Walk nor StageForScan may look inside it,
// at any depth: it sits at the project root, usually several levels
// down, not at the tree's own top level.
func TestNixggScratchDirIsExcluded(t *testing.T) {
	root := t.TempDir()
	farm := filepath.Join(root, "src-1.0", "build", ".nixgg", "srcs", "tu0")
	if err := os.MkdirAll(farm, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nix/store/aaa-hdr.h", filepath.Join(farm, "hdr.h")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src-1.0", "build", "real.txt"), []byte("out"), 0o644); err != nil {
		t.Fatal(err)
	}

	staged, err := StageForScan(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(staged, "src-1.0", "build", ".nixgg")); !os.IsNotExist(err) {
		t.Error(".nixgg was copied into the staged tree; its closure would pull in every staged source object")
	}
	if _, err := os.Lstat(filepath.Join(staged, "src-1.0", "build", "real.txt")); err != nil {
		t.Errorf("real build output was not staged: %v", err)
	}
}
