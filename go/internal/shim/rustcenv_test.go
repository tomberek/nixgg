package shim

import (
	"testing"
)

// The rewrite is the point, not the carry-through. A crate that does
//
//	include!(concat!(env!("OBJTREE"), "/rust/uapi/uapi_generated.rs"))
//
// gets a value naming the caller's build tree, which the sandbox does
// not have. The same tree is mounted at the staged store path, so the
// value has to name that instead — passed verbatim, rustc fails on a
// missing file and points at the include, not at the variable.
func TestRustcEnvRestagesProjectPaths(t *testing.T) {
	const root = "/build/linux-6.18.41"
	const staged = "/nix/store/aaa-rs-uapi.o"

	for _, tc := range []struct{ name, in, want string }{
		{"the project root itself", root, staged},
		{"a path inside it", root + "/rust/uapi", staged + "/rust/uapi"},
		{"a trailing slash", root + "/", staged},
		{"a path outside it", "/nix/store/bbb-rustc", "/nix/store/bbb-rustc"},
		{"a sibling with a shared prefix", "/build/linux-6.18.41-other", "/build/linux-6.18.41-other"},
		{"not a path at all", "6.18.41", "6.18.41"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := restagePath(tc.in, root, staged); got != tc.want {
				t.Errorf("restagePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Only declared variables travel. Carrying the whole environment would
// put the caller's PATH and TMPDIR into every drv hash, so no two
// machines would ever share a cache entry.
func TestRustcEnvCarriesOnlyDeclaredVars(t *testing.T) {
	t.Setenv("OBJTREE", "/build/tree")
	t.Setenv("RUST_MODFILE", "drivers/foo.rs")
	t.Setenv("TMPDIR", "/tmp/should-not-travel")

	t.Setenv(rustcEnvVar, `["OBJTREE","MISSING_ON_PURPOSE"]`)
	got := rustcEnv("/build/tree", "/nix/store/aaa-crate")

	if got["OBJTREE"] != "/nix/store/aaa-crate" {
		t.Errorf("OBJTREE = %q", got["OBJTREE"])
	}
	if _, ok := got["TMPDIR"]; ok {
		t.Error("TMPDIR travelled; every drv hash would then depend on the caller's temp dir")
	}
	if _, ok := got["MISSING_ON_PURPOSE"]; ok {
		t.Error("an unset variable was carried as empty rather than omitted")
	}
	if _, ok := got["RUST_MODFILE"]; ok {
		t.Error("an undeclared variable travelled")
	}

	// Nothing declared is the normal case and must add nothing to the drv.
	t.Setenv(rustcEnvVar, "")
	if got := rustcEnv("/build/tree", "/nix/store/aaa-crate"); got != nil {
		t.Errorf("undeclared env produced %v, want nil", got)
	}
}
