package classify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tbereknyei/nixgg/internal/drvref"
	"github.com/tbereknyei/nixgg/internal/paths"
)

// A regression to Regular would make the link/archive shim silently fall
// back to Passthrough with no error.
func TestTargetDrvRefStub(t *testing.T) {
	dir := t.TempDir()
	want := "/nix/store/00000000000000000000000000000000-ar-libfoo.a.drv"
	stub := filepath.Join(dir, "libfoo.a")
	if err := os.WriteFile(stub, []byte(drvref.Body(want)), 0o644); err != nil {
		t.Fatal(err)
	}
	got := Target(stub, "", paths.Layout{})
	if got.Kind != Drv {
		t.Errorf("Kind = %v, want Drv", got.Kind)
	}
	if got.Ref != want {
		t.Errorf("Ref = %q, want %q", got.Ref, want)
	}
}

func TestTargetPlainFileIsRegular(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "libvendored.a")
	if err := os.WriteFile(f, []byte("!<arch>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Target(f, "", paths.Layout{}); got.Kind != Regular {
		t.Errorf("Kind = %v, want Regular", got.Kind)
	}
}

// A dependency reached via a literal, already-resolved /nix/store/...
// path with no symlink hop (e.g. meson emitting zlib's absolute path
// directly on a link line) used to fall through to Regular, degrading
// the whole link to Passthrough — confirmed against a real QEMU build.
func TestTargetLiteralStorePathIsStore(t *testing.T) {
	// Can't create a file under /nix/store in a unit test, so exercise
	// the altStorePrefix branch, which shares the same stripping logic.
	dir := t.TempDir()
	storeDir := filepath.Join(dir, "nix", "store", "abc123-zlib-1.3.2-static", "lib")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(storeDir, "libz.a")
	if err := os.WriteFile(f, []byte("!<arch>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := Target(f, dir, paths.Layout{})
	if got.Kind != Store {
		t.Fatalf("Kind = %v, want Store", got.Kind)
	}
	wantRef := "/nix/store/abc123-zlib-1.3.2-static"
	if got.Ref != wantRef {
		t.Errorf("Ref = %q, want %q", got.Ref, wantRef)
	}
	if got.Sub != "lib/libz.a" {
		t.Errorf("Sub = %q, want %q", got.Sub, "lib/libz.a")
	}
}

func TestTargetAbsent(t *testing.T) {
	got := Target(filepath.Join(t.TempDir(), "nope.o"), "", paths.Layout{})
	if got.Kind != Absent {
		t.Errorf("Kind = %v, want Absent", got.Kind)
	}
}

// A dangling symlink to a .drv still classifies as Drv, because
// readlinkFollow falls back to os.Readlink when EvalSymlinks fails. The
// historical bug (mosh's `mosh-client: ../crypto/libmoshcrypto.a`) was a
// Makefile's shell-level `test -e` on such a symlink, not this layer —
// which is why sandbox mode switched to writing regular-file stubs (see
// internal/drvref).
func TestTargetDanglingDrvSymlink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "libfoo.a")
	target := "/nix/store/00000000000000000000000000000000-ar-libfoo.a.drv"
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot symlink here: %v", err)
	}
	if _, err := os.Stat(link); err == nil {
		t.Skip("target unexpectedly exists; cannot test the dangling case")
	}
	got := Target(link, "", paths.Layout{})
	if got.Kind != Drv {
		t.Errorf("Kind = %v, want Drv — classify resolves a dangling .drv "+
			"symlink via the Readlink fallback; the historical bug was at the "+
			"shell `test -e` layer, not here", got.Kind)
	}
}

// Regression guard for a link failure that reached a real build: LLVM's
// cmake puts an absolute positional shared library on the link line, and
// nixgg linked against a path that did not exist:
//
//	ld.bfd: cannot find /nix/store/…-zlib-1.3.2/libz.so
//
// The file is at <root>/lib/libz.so. Classification reduced it to the
// store root, and the link shim rebuilt the argv as
// Ref+"/"+filepath.Base(input), silently dropping the "lib/" in between.
func TestStoreSubpathSurvivesClassification(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "nix", "store", strings.Repeat("a", 32)+"-zlib-1.3.2")
	if err := os.MkdirAll(filepath.Join(root, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "lib", "libz.so")
	if err := os.WriteFile(real, []byte("\x7fELF"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "libz.so")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	got := Target(link, dir, paths.Layout{})
	if got.Kind != Store {
		t.Fatalf("Kind = %v, want Store", got.Kind)
	}

	wantRef := "/nix/store/" + strings.Repeat("a", 32) + "-zlib-1.3.2"
	if got.Ref != wantRef {
		t.Errorf("Ref = %q, want the store ROOT %q — builtins.storePath and\n"+
			"inputs.srcs reject a subpath", got.Ref, wantRef)
	}
	if got.Sub != "lib/libz.so" {
		t.Errorf("Sub = %q, want \"lib/libz.so\" — without it the argv path\n"+
			"cannot be reconstructed and the link references a nonexistent file", got.Sub)
	}

	if want := wantRef + "/lib/libz.so"; got.ArgvPath("libz.so") != want {
		t.Errorf("ArgvPath = %q, want %q", got.ArgvPath("libz.so"), want)
	}
}

// Every one of the 81 pinned drvs depends on this shape (artifact sits
// directly in its drv output dir, Sub stays empty), so a change here
// would move hashes across the board.
func TestStoreDirectChildHasNoSub(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "nix", "store", strings.Repeat("b", 32)+"-tu-main.o")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "main.o")
	if err := os.WriteFile(real, []byte("obj"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "main.o")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	got := Target(link, dir, paths.Layout{})
	if got.Kind != Store {
		t.Fatalf("Kind = %v, want Store", got.Kind)
	}
	if got.Sub != "main.o" {
		t.Errorf("Sub = %q, want \"main.o\"", got.Sub)
	}
	if want := "/nix/store/" + strings.Repeat("b", 32) + "-tu-main.o/main.o"; got.ArgvPath("main.o") != want {
		t.Errorf("ArgvPath = %q, want %q", got.ArgvPath("main.o"), want)
	}
}

// A path we could not inspect (ELOOP, EACCES) must be distinguished from
// a genuinely ordinary file nixgg doesn't own: both classify as Regular,
// but they need different diagnostics.
func TestTargetDistinguishesStatFailure(t *testing.T) {
	dir := t.TempDir()

	t.Run("symlink loop stays Regular, and that is not the Err path", func(t *testing.T) {
		// a -> b -> a. EvalSymlinks fails with "too many links", but
		// readlinkFollow falls back to os.Readlink, which succeeds on a
		// loop (reads one hop without following) — load-bearing, since
		// it's also how an unrealised thunk symlink classifies as Thunk.
		a := filepath.Join(dir, "loop-a")
		b := filepath.Join(dir, "loop-b")
		if err := os.Symlink(b, a); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(a, b); err != nil {
			t.Fatal(err)
		}
		got := Target(a, "", paths.Layout{})
		if got.Kind != Regular {
			t.Errorf("Kind = %v, want Regular — a loop points at nothing nixgg owns", got.Kind)
		}
		if got.Err != nil {
			t.Errorf("Err = %v, want nil: os.Readlink succeeds on a loop, so no "+
				"error surfaces here. If this ever becomes non-nil, the "+
				"readlinkFollow fallback changed and thunk symlinks may have "+
				"stopped resolving.", got.Err)
		}
	})

	t.Run("plain file has no Err and reads as regular", func(t *testing.T) {
		f := filepath.Join(dir, "ordinary.o")
		if err := os.WriteFile(f, []byte("obj"), 0o644); err != nil {
			t.Fatal(err)
		}
		got := Target(f, "", paths.Layout{})
		if got.Err != nil {
			t.Errorf("Err = %v, want nil for a readable ordinary file", got.Err)
		}
		if got.Reason() != "regular" {
			t.Errorf("Reason() = %q, want \"regular\"", got.Reason())
		}
	})

	t.Run("unreadable directory yields Err, not a bare regular", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses permission checks")
		}
		locked := filepath.Join(dir, "locked")
		if err := os.MkdirAll(locked, 0o755); err != nil {
			t.Fatal(err)
		}
		victim := filepath.Join(locked, "hidden.o")
		if err := os.WriteFile(victim, []byte("obj"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Remove search permission so Lstat on the child fails EACCES.
		if err := os.Chmod(locked, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

		got := Target(victim, "", paths.Layout{})
		if got.Kind != Regular {
			t.Errorf("Kind = %v, want Regular (passthrough is still correct)", got.Kind)
		}
		if got.Err == nil {
			t.Fatal("Err is nil — an EACCES path is indistinguishable from an " +
				"ordinary unowned file, so the diagnostic misattributes the cause")
		}
		if r := got.Reason(); !strings.Contains(r, "stat failed") {
			t.Errorf("Reason() = %q, want it to mention the stat failure", r)
		}
	})
}

// A SONAME alias chain (`libfoo.so -> libfoo.so.1.2.3`) resolving to our
// own drvref stub must classify as Drv. The direct-regular-file branch
// always checked drvref; the symlink branch did not, so such an input
// classified as Regular and the entire link silently degraded to an
// unaccelerated Passthrough.
func TestTargetSymlinkToDrvRefStub(t *testing.T) {
	drv := "/nix/store/" + strings.Repeat("a", 32) + "-bin-libfoo.so.drv"

	t.Run("one hop", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(dir, "libfoo.so.1.2.3")
		if err := os.WriteFile(real, []byte(drvref.Body(drv)), 0o644); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(dir, "libfoo.so")
		if err := os.Symlink(real, alias); err != nil {
			t.Fatal(err)
		}

		got := Target(alias, "", paths.Layout{})
		if got.Kind != Drv {
			t.Fatalf("Kind = %v, want Drv — a SONAME alias to our own stub "+
				"reads as Regular, so the link falls back to Passthrough", got.Kind)
		}
		if got.Ref != drv {
			t.Errorf("Ref = %q, want %q", got.Ref, drv)
		}
		// Sub is the real target's basename, not the alias's own name —
		// broke openssl's engines/*.so, which link against the plain
		// `ln -s libcrypto.so.3 libcrypto.so` alias openssl's own
		// Makefile creates.
		if got.Sub != "libfoo.so.1.2.3" {
			t.Errorf("Sub = %q, want %q (the real target's basename, not the alias)", got.Sub, "libfoo.so.1.2.3")
		}
	})

	t.Run("multi hop", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(dir, "libfoo.so.1.2.3")
		if err := os.WriteFile(real, []byte(drvref.Body(drv)), 0o644); err != nil {
			t.Fatal(err)
		}
		mid := filepath.Join(dir, "libfoo.so.1")
		if err := os.Symlink(real, mid); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(dir, "libfoo.so")
		if err := os.Symlink(mid, alias); err != nil {
			t.Fatal(err)
		}

		got := Target(alias, "", paths.Layout{})
		if got.Kind != Drv || got.Ref != drv {
			t.Errorf("multi-hop chain: Kind=%v Ref=%q, want Drv %q", got.Kind, got.Ref, drv)
		}
		if got.Sub != "libfoo.so.1.2.3" {
			t.Errorf("multi-hop chain: Sub = %q, want %q", got.Sub, "libfoo.so.1.2.3")
		}
	})

	t.Run("symlink to a foreign file is still Regular", func(t *testing.T) {
		// Must NOT be claimed: nixgg never staged it.
		dir := t.TempDir()
		real := filepath.Join(dir, "libsystem.so.6")
		if err := os.WriteFile(real, []byte("\x7fELF not ours"), 0o644); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(dir, "libsystem.so")
		if err := os.Symlink(real, alias); err != nil {
			t.Fatal(err)
		}
		if got := Target(alias, "", paths.Layout{}); got.Kind != Regular {
			t.Errorf("Kind = %v, want Regular for a foreign library", got.Kind)
		}
	})
}
