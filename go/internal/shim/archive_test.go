package shim

import (
	"reflect"
	"testing"
)

// Getting `archive` wrong doesn't fail loudly — it names the
// derivation's output after the wrong token.
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
			name: "leading dash is tolerated", args: []string{"-rcs", "libfoo.a", "a.o"},
			wantMods: "rcs", wantArch: "libfoo.a", wantInputs: []string{"a.o"}, wantOK: true,
		},
		{
			name: "cru", args: []string{"cru", "libbar.a", "x.o"},
			wantMods: "cru", wantArch: "libbar.a", wantInputs: []string{"x.o"}, wantOK: true,
		},
		{
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
			// Kbuild issues exactly this for every disabled-subsystem
			// directory; a zero-member archive is modeled, not bailed on.
			name:     "no inputs is modeled as an empty archive",
			args:     []string{"cDPrST", "libfoo.a"},
			wantMods: "cDPrST", wantArch: "libfoo.a", wantInputs: nil, wantOK: true,
		},
		{
			name:     "no inputs with q is also modeled",
			args:     []string{"q", "libfoo.a"},
			wantMods: "q", wantArch: "libfoo.a", wantInputs: nil, wantOK: true,
		},
		{
			// D alone (no r, no q) doesn't construct content, so zero
			// members here must still bail.
			name: "no inputs with D alone does not qualify", args: []string{"D", "libfoo.a"},
			wantOK: false,
		},
		{
			// A member extension outside {.o, .a} means the member list
			// can't be modeled; bail entirely rather than silently
			// dropping it.
			name: "unmodeled extension bails", args: []string{"rcs", "libfoo.a", "a.o", "sub.lo"},
			wantOK: false,
		},
		{
			// A .a member is a nested archive (Kbuild's own built-in.a
			// construction), modeled the same as a .o member.
			name: "nested archive member", args: []string{"rcs", "libfoo.a", "a.o", "sub/built-in.a"},
			wantMods: "rcs", wantArch: "libfoo.a", wantInputs: []string{"a.o", "sub/built-in.a"}, wantOK: true,
		},
		{
			name: "thin archive modifiers", args: []string{"csrDT", "libqemuutil.a", "a.o", "b.o"},
			wantMods: "csrDT", wantArch: "libqemuutil.a", wantInputs: []string{"a.o", "b.o"}, wantOK: true,
		},
		{
			name: "modifier outside the alphabet bails", args: []string{"rzz", "libfoo.a", "a.o"},
			wantOK: false,
		},
		{
			// meson's GCC-toolchain static_library() invocation always
			// prepends `--plugin <path>`, regardless of whether this
			// library is itself LTO-compiled; must be skipped before the
			// modifier string is located.
			name: "leading --plugin pair is skipped", args: []string{"--plugin", "/nix/store/x/liblto_plugin.so", "csrD", "libnixutil.a", "prelink.o"},
			wantMods: "csrD", wantArch: "libnixutil.a", wantInputs: []string{"prelink.o"}, wantOK: true,
		},
		{
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
// rather than asserting correct behaviour: `a`, `b`, `i` and `N` all
// take a positional argument following the modifier string
// (`ar rN <count> <archive> <member>...`), but parseARArgs treats
// args[1] as the archive unconditionally, so the count is taken as
// the archive name. This is reachable only from ar's positional-count
// forms, which no fixture and no example build uses — hence pinned,
// not fixed here.
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
		{"rz", false},
		{"libfoo.a", false},
		{"2", false},
		{"rN", true},
		{"csrDT", true},
		{"T", true},
	} {
		if got := isARModifiers(tc.in); got != tc.want {
			t.Errorf("isARModifiers(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
