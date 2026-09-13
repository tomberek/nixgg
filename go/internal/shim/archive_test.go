package shim

import (
	"reflect"
	"testing"
)

// TestParseARArgs pins the `ar` command-line parser. It had no test at
// all, which matters because the shim's decision here is binary: model
// the archive as a derivation, or hand the whole invocation to the real
// `ar` untouched. Getting `archive` wrong doesn't fail loudly — it names
// the derivation's output after the wrong token.
func TestParseARArgs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		wantMods   string
		wantArch   string
		wantInputs []string
		wantOK     bool
	}{
		{
			name: "canonical rcs", args: []string{"rcs", "libfoo.a", "a.o", "b.o"},
			wantMods: "rcs", wantArch: "libfoo.a", wantInputs: []string{"a.o", "b.o"}, wantOK: true,
		},
		{
			// GNU accepts a leading dash; both spellings must produce the
			// same modifier string, since it lands verbatim in the drv.
			name: "leading dash is tolerated", args: []string{"-rcs", "libfoo.a", "a.o"},
			wantMods: "rcs", wantArch: "libfoo.a", wantInputs: []string{"a.o"}, wantOK: true,
		},
		{
			name: "cru", args: []string{"cru", "libbar.a", "x.o"},
			wantMods: "cru", wantArch: "libbar.a", wantInputs: []string{"x.o"}, wantOK: true,
		},
		{
			// D (deterministic) is already prepended by the emitter, but a
			// caller may pass it explicitly.
			name: "explicit D", args: []string{"Drcs", "libbaz.a", "y.o"},
			wantMods: "Drcs", wantArch: "libbaz.a", wantInputs: []string{"y.o"}, wantOK: true,
		},
		{
			name: "read-mode t is not modelled", args: []string{"t", "libfoo.a"},
			wantOK: false,
		},
		{
			name: "too few args", args: []string{"rcs"},
			wantOK: false,
		},
		{
			// A zero-member archive IS modeled, not bailed on — see
			// parseARArgs' own docstring: Linux Kbuild issues exactly
			// this for every disabled-subsystem directory, and letting
			// it fall to Passthrough (the old behavior) cascades into
			// every ancestor archive misclassifying it as foreign.
			// Kbuild's own real invocation is `cDPrST` (r present).
			name:     "no inputs is modeled as an empty archive",
			args:     []string{"cDPrST", "libfoo.a"},
			wantMods: "cDPrST", wantArch: "libfoo.a", wantInputs: nil, wantOK: true,
		},
		{
			// q (quick-append) is the OTHER create-ish modifier that
			// legitimately takes zero members — same gating as r.
			name:     "no inputs with q is also modeled",
			args:     []string{"q", "libfoo.a"},
			wantMods: "q", wantArch: "libfoo.a", wantInputs: nil, wantOK: true,
		},
		{
			// D alone (no r, no q) is a near-miss: a real modifier
			// string, but not one of the two that construct content,
			// so zero members here must still bail — there's no
			// Kbuild recipe (or any real caller) that issues this.
			name: "no inputs with D alone does not qualify", args: []string{"D", "libfoo.a"},
			wantOK: false,
		},
		{
			// A member extension outside {.o, .a} — a .lo (libtool
			// object) or a response file — means we can't model the
			// member list; bail entirely rather than silently
			// dropping it. .a IS modeled (nested built-in.a-style
			// archives, see TestParseARArgsNestedArchiveMember below),
			// so this case must use something genuinely unmodeled.
			name: "unmodeled extension bails", args: []string{"rcs", "libfoo.a", "a.o", "sub.lo"},
			wantOK: false,
		},
		{
			// A .a member is a nested archive (Kbuild's own
			// built-in.a construction lists child directories' own
			// built-in.a as members of the parent's) — modeled the
			// same as a .o member, resolved later by classifyInputs
			// via classify.Target the same way a link step's archive
			// input already is.
			name: "nested archive member", args: []string{"rcs", "libfoo.a", "a.o", "sub/built-in.a"},
			wantMods: "rcs", wantArch: "libfoo.a", wantInputs: []string{"a.o", "sub/built-in.a"}, wantOK: true,
		},
		{
			// T (thin archive) is a modelled modifier, same as any
			// other — meson emits exactly this for QEMU's internal
			// static libraries.
			name: "thin archive modifiers", args: []string{"csrDT", "libqemuutil.a", "a.o", "b.o"},
			wantMods: "csrDT", wantArch: "libqemuutil.a", wantInputs: []string{"a.o", "b.o"}, wantOK: true,
		},
		{
			name: "modifier outside the alphabet bails", args: []string{"rzz", "libfoo.a", "a.o"},
			wantOK: false,
		},
		{
			// meson's own GCC-toolchain static_library() invocation
			// always prepends `--plugin <path-to-liblto_plugin.so>`
			// (loading gcc's LTO plugin so `ar` can read IR-bitcode
			// member objects), regardless of whether THIS library is
			// itself LTO-compiled — confirmed directly against a real
			// meson+ninja build of Nix's own libutil
			// (examples/nix-util). Must be skipped before the
			// modifier string is located.
			name: "leading --plugin pair is skipped", args: []string{"--plugin", "/nix/store/x/liblto_plugin.so", "csrD", "libnixutil.a", "prelink.o"},
			wantMods: "csrD", wantArch: "libnixutil.a", wantInputs: []string{"prelink.o"}, wantOK: true,
		},
		{
			// ar allows repeating --plugin; both occurrences must be
			// stripped, not just the first.
			name: "repeated --plugin pairs are all skipped", args: []string{"--plugin", "/a.so", "--plugin", "/b.so", "rcs", "libfoo.a", "a.o"},
			wantMods: "rcs", wantArch: "libfoo.a", wantInputs: []string{"a.o"}, wantOK: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, a, in, ok := parseARArgs(tc.args)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (args %q)", ok, tc.wantOK, tc.args)
			}
			if !ok {
				return
			}
			if m != tc.wantMods {
				t.Errorf("modifiers = %q, want %q", m, tc.wantMods)
			}
			if a != tc.wantArch {
				t.Errorf("archive = %q, want %q", a, tc.wantArch)
			}
			if !reflect.DeepEqual(in, tc.wantInputs) {
				t.Errorf("inputs = %q, want %q", in, tc.wantInputs)
			}
		})
	}
}

// TestParseARArgsPositionalCountIsMisparsed documents a real defect
// rather than asserting correct behaviour, because fixing it properly
// means teaching parseARArgs ar's positional grammar and that is a
// separate change.
//
// `a`, `b`, `i` and `N` all take a positional argument that follows the
// modifier string: `ar rN <count> <archive> <member>...`. parseARArgs
// treats args[1] as the archive unconditionally, so the count is taken as
// the archive name. Nothing catches this by accident anymore — once .a
// members were accepted (for nested built-in.a support), the count-as-
// archive misparse goes through directly, with no filter incidentally
// saving it the way the old .o-only check did for a real archive.a name.
//
// The output derivation would be named after "2" and the real archive
// would be treated as a member. This is reachable only from a caller
// using ar's positional-count forms, which no fixture and no example
// build does — hence documented and pinned, not fixed here.
func TestParseARArgsPositionalCountIsMisparsed(t *testing.T) {
	m, a, in, ok := parseARArgs([]string{"rN", "2", "weird.o", "member.o"})
	if !ok {
		t.Skip("parseARArgs now rejects positional-count forms — " +
			"the grammar was fixed; delete this test and assert the fix instead")
	}
	if a != "2" {
		t.Errorf("archive = %q; this test exists to pin the known-wrong %q. "+
			"If it changed, the positional grammar was addressed.", a, "2")
	}
	t.Logf("known defect: mods=%q archive=%q inputs=%q", m, a, in)
}

// TestIsARModifiers pins the alphabet check. It is a set membership test
// with no positional grammar, which is exactly why the misparse above is
// possible — worth stating so a reader doesn't assume more rigour here
// than exists.
func TestIsARModifiers(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"rcs", true},
		{"cru", true},
		{"Drcs", true},
		{"t", true},
		{"", false},
		{"rz", false},       // z is not in the alphabet
		{"libfoo.a", false}, // a real filename must not read as modifiers
		{"2", false},        // a positional count must not read as modifiers
		{"rN", true},        // accepted, and that is what enables the misparse
		{"csrDT", true},     // meson's own thin-archive modifiers (T = thin)
		{"T", true},         // bare thin flag
	} {
		if got := isARModifiers(tc.in); got != tc.want {
			t.Errorf("isARModifiers(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
