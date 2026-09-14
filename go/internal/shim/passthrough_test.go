package shim

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tbereknyei/nixgg/internal/classify"
	"github.com/tbereknyei/nixgg/internal/paths"
)

// TestRealiseThunkArgsAndPassthroughClassificationBoundary pins the
// classification RealiseThunkArgsAndPassthrough relies on to decide
// which argv tokens get a synchronous realise attempt before the real
// tool runs: only classify.Thunk. A foreign archive and a nonexistent
// path must both classify as something other than Thunk — calling
// realise.Realise on either would fail trying to read a nonexistent
// .nix file.
//
// This can't exercise RealiseThunkArgsAndPassthrough itself in a unit
// test, since its whole point is to exec the real tool via
// Passthrough, replacing the test process; that path is covered by
// the real end-to-end kernel-fixture build instead. This test only
// pins the classification boundary the fix depends on.
func TestRealiseThunkArgsAndPassthroughClassificationBoundary(t *testing.T) {
	dir := t.TempDir()
	l := paths.Layout{Thunks: filepath.Join(dir, "thunks")}

	t.Run("absent path is not Thunk", func(t *testing.T) {
		absent := filepath.Join(dir, "does-not-exist.a")
		if c := classify.Target(absent, "", l); c.Kind == classify.Thunk {
			t.Errorf("absent path %q classified as Thunk, want anything else", absent)
		}
	})

	t.Run("foreign regular file is not Thunk", func(t *testing.T) {
		foreign := filepath.Join(dir, "libforeign.a")
		if err := os.WriteFile(foreign, []byte("!<arch>\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if c := classify.Target(foreign, "", l); c.Kind == classify.Thunk {
			t.Errorf("foreign file %q classified as Thunk, want anything else", foreign)
		}
	})

	t.Run("a real thunk symlink IS Thunk", func(t *testing.T) {
		if err := os.MkdirAll(l.Thunks, 0o755); err != nil {
			t.Fatal(err)
		}
		thunkFile := filepath.Join(l.Thunks, "abc123.nix")
		if err := os.WriteFile(thunkFile, []byte("# fake thunk\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		sym := filepath.Join(dir, "still-deferred.a")
		if err := os.Symlink(thunkFile, sym); err != nil {
			t.Fatal(err)
		}
		c := classify.Target(sym, "", l)
		if c.Kind != classify.Thunk {
			t.Fatalf("thunk symlink %q classified as %v, want Thunk", sym, c.Kind)
		}
		if c.Ref != thunkFile {
			t.Errorf("Thunk Ref = %q, want %q (the .nix path RealiseThunkArgsAndPassthrough "+
				"passes straight to realise.Realise)", c.Ref, thunkFile)
		}
	})
}
