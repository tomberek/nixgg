package shim

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tbereknyei/nixgg/internal/classify"
	"github.com/tbereknyei/nixgg/internal/paths"
)

// parseObjtoolArgs has to hold up against a real invocation: one
// object preceded by a long, config-determined flag list
// (scripts/Makefile.lib's objtool-args). None of objtool's options
// take a separate value token — bare or `--opt=value` only — so the
// sole non-flag operand is the object.
func TestParseObjtoolArgs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantFlags []string
		wantObj   string
		wantOK    bool
	}{
		{
			name: "full real-world invocation",
			args: []string{
				"--link", "--module", "--ibt", "--orc", "--stackval",
				"--retpoline", "--rethunk", "--sls", "--static-call",
				"--uaccess", "--hacks=jump_label,noinstr", "lib/crc/crc4.o",
			},
			wantFlags: []string{
				"--link", "--module", "--ibt", "--orc", "--stackval",
				"--retpoline", "--rethunk", "--sls", "--static-call",
				"--uaccess", "--hacks=jump_label,noinstr",
			},
			wantObj: "lib/crc/crc4.o",
			wantOK:  true,
		},
		{
			name: "no flags at all", args: []string{"foo.o"},
			wantFlags: nil, wantObj: "foo.o", wantOK: true,
		},
		{
			name: "no operand bails", args: []string{"--orc"},
			wantOK: false,
		},
		{
			// Two operands is a shape we do not model; rewriting only
			// one of them would silently corrupt the other.
			name: "two operands bail", args: []string{"--orc", "a.o", "b.o"},
			wantOK: false,
		},
		{
			name: "empty argv bails", args: nil, wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flags, obj, ok := parseObjtoolArgs(tc.args)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if obj != tc.wantObj {
				t.Errorf("object = %q, want %q", obj, tc.wantObj)
			}
			if !reflect.DeepEqual(flags, tc.wantFlags) {
				t.Errorf("flags = %q, want %q", flags, tc.wantFlags)
			}
		})
	}
}

// TestSubmitTransformThunkWritesPlaceholderAndSymlink pins the native-
// mode write path Objtool/Objcopy now share with Compile/Archive/Link:
// a thunk file lands under l.Thunks, and the output path becomes a
// symlink to it (thunk.LinkPlaceholder's own contract) rather than
// staying a real file — the same "deferred until nixgg force" shape
// every other Kind already relies on.
func TestSubmitTransformThunkWritesPlaceholderAndSymlink(t *testing.T) {
	dir := t.TempDir()
	l := paths.Layout{
		Thunks:   filepath.Join(dir, "thunks"),
		Symlinks: filepath.Join(dir, "symlinks"),
	}
	output := filepath.Join(dir, "foo.o")

	thunkPath, err := submitTransformThunk(l, "import /fake/transform.nix { }\n", output)
	if err != nil {
		t.Fatalf("submitTransformThunk: %v", err)
	}
	if _, err := os.Stat(thunkPath); err != nil {
		t.Errorf("thunk file %q not written: %v", thunkPath, err)
	}

	c := classify.Target(output, "", l)
	if c.Kind != classify.Thunk {
		t.Fatalf("output classified as %v after submitTransformThunk, want Thunk", c.Kind)
	}
	if c.Ref != thunkPath {
		t.Errorf("output symlink resolves to %q, want %q", c.Ref, thunkPath)
	}
}

// TestSubmitTransformThunkIsIdempotent: a second call with the SAME
// expression body (the shape a rebuild with no source changes
// produces) must not error and must resolve to the same thunk —
// thunk.Write's own dedup contract, exercised through this Kind for
// the first time.
func TestSubmitTransformThunkIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	l := paths.Layout{
		Thunks:   filepath.Join(dir, "thunks"),
		Symlinks: filepath.Join(dir, "symlinks"),
	}
	output := filepath.Join(dir, "foo.o")
	body := "import /fake/transform.nix { }\n"

	first, err := submitTransformThunk(l, body, output)
	if err != nil {
		t.Fatalf("first submitTransformThunk: %v", err)
	}
	second, err := submitTransformThunk(l, body, output)
	if err != nil {
		t.Fatalf("second submitTransformThunk: %v", err)
	}
	if first != second {
		t.Errorf("thunk path changed across identical resubmission: %q vs %q", first, second)
	}
}
