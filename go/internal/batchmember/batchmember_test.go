package batchmember

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tbereknyei/nixgg/internal/paths"
)

func testLayout(t *testing.T) paths.Layout {
	t.Helper()
	dir := t.TempDir()
	return paths.Layout{Batches: filepath.Join(dir, "batches")}
}

func TestWriteReadRoundTrip(t *testing.T) {
	l := testLayout(t)

	native := MemberRecord{
		Group: "vendor", Tool: "cc",
		Source: "sds.c", OutName: "sds.o",
		Flags: []string{"-O2", "-Wall"}, StoreDeps: []string{"/nix/store/aaa-foo"},
		WrapperEnv:     map[string]string{"NIX_CFLAGS_COMPILE": "-isystem /nix/store/aaa-foo/include"},
		SrcTreeLiteral: "../srcs/deps-hiredis-sds-o",
	}
	sandbox := MemberRecord{
		Group: "vendor", Tool: "cc",
		Source: "lapi.c", OutName: "lapi.o",
		Flags:    []string{"-DLUA_ANSI"},
		SrcStore: "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-deps-lua-src-lapi-o",
	}

	for _, tc := range []struct {
		name string
		m    MemberRecord
		out  string
	}{
		{"native", native, "/build/source/deps/hiredis/sds.o"},
		{"sandbox", sandbox, "/build/source/deps/lua/src/lapi.o"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recPath, err := Write(l, tc.m.Group, tc.out, tc.m)
			if err != nil {
				t.Fatalf("Write: %v", err)
			}
			got, err := Read(recPath)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if !reflect.DeepEqual(got, tc.m) {
				t.Errorf("round-trip mismatch:\n got:  %+v\n want: %+v", got, tc.m)
			}
		})
	}
}

// Key is a pure function of the absolute path, letting a reader (ar's argv) recompute a prior writer's filename without any shared index.
func TestKeyDeterministic(t *testing.T) {
	a := Key("/build/source/deps/hiredis/sds.o")
	b := Key("/build/source/deps/hiredis/sds.o")
	if a != b {
		t.Errorf("Key not deterministic: %q != %q", a, b)
	}
	c := Key("/build/source/deps/hiredis/other.o")
	if a == c {
		t.Errorf("Key collided for different paths: both %q", a)
	}
}

func TestWriteCreatesGroupDir(t *testing.T) {
	l := testLayout(t)
	m := MemberRecord{Group: "vendor", OutName: "x.o", SrcTreeLiteral: "../srcs/x"}
	recPath, err := Write(l, "vendor", "/build/source/x.c", m)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	wantDir := filepath.Join(l.Batches, "vendor")
	if filepath.Dir(recPath) != wantDir {
		t.Errorf("record written to %q, want under %q", recPath, wantDir)
	}
}

func TestReadMissingFile(t *testing.T) {
	if _, err := Read(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Error("Read(missing) = nil error, want an error")
	}
}
