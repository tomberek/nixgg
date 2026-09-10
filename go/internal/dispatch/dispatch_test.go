package dispatch

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestFromArgv0 pins the argv[0] → role mapping.
//
// An unclassified name returns ToolUnknown and the shim has no role to
// play, so acceleration silently never engages for that tool: no error,
// no drv, nothing in the log — the build just quietly runs
// unaccelerated. Real build systems invoke compilers under
// target-triple and version decorations, so the matcher has to be
// generous about spelling.
func TestFromArgv0(t *testing.T) {
	for _, tc := range []struct {
		argv0 string
		want  Tool
	}{
		// The seven canonical names.
		{"cc", ToolCC},
		{"gcc", ToolGCC},
		{"c++", ToolCXX},
		{"g++", ToolGXX},
		{"ar", ToolAR},
		{"ranlib", ToolRanlib},
		{"ld", ToolLD},

		// Full paths — only the basename matters.
		{"/usr/bin/gcc", ToolGCC},
		{"/nix/store/xxx-gcc-wrapper/bin/c++", ToolCXX},
		{"./cc", ToolCC},

		// Version-decorated, as Debian/Fedora ship parallel compilers.
		{"gcc-15", ToolGCC},
		{"g++-14", ToolGXX},
		{"g++-14.2", ToolGXX},
		{"clang-18", ToolGCC},
		{"clang++-18", ToolGXX},

		// Target-triple prefixed, from cross and explicit-triple setups.
		{"x86_64-unknown-linux-gnu-gcc", ToolGCC},
		{"x86_64-linux-gnu-g++", ToolGXX},
		{"aarch64-linux-gnu-ar", ToolAR},
		{"arm-none-eabi-ranlib", ToolRanlib},

		// Both decorations at once.
		{"x86_64-linux-gnu-g++-14", ToolGXX},
		{"x86_64-unknown-linux-gnu-gcc-15", ToolGCC},

		// clang maps onto the gcc/g++ roles: nixgg pins its own
		// compiler, so the role only selects C vs C++ mode.
		{"clang", ToolGCC},
		{"clang++", ToolGXX},

		// binutils equivalents.
		{"llvm-ar", ToolAR},
		{"llvm-ranlib", ToolRanlib},

		// Linux Kbuild's raw-link tool names — bfd/gold/lld linker
		// personalities, and the ld.lld spelling LLVM builds use.
		{"ld.bfd", ToolLD},
		{"ld.gold", ToolLD},
		{"ld.lld", ToolLD},
		{"x86_64-linux-gnu-ld", ToolLD},
		// Not a compiler, but modelled: `ld -r` partial links fuse
		// multi-object components. Anything that is not `-r`
		// passes straight through — see shim.LD.
		{"ld", ToolLD},

		// A whole-crate compiler rather than a per-TU one — see
		// shim.Rustc. Reached only when a caller points the build's
		// RUSTC at our shim; nothing resolves it via PATH.
		{"rustc", ToolRustc},
		{"/nix/store/xxx-rustc-1.95.0/bin/rustc", ToolRustc},

		// Not compilers.
		{"make", ToolUnknown},
		{"python3", ToolUnknown},
		{"nixgg", ToolUnknown},
		{"", ToolUnknown},
		// A trailing dash leaves nothing to match.
		{"gcc-", ToolUnknown},
	} {
		t.Run(tc.argv0, func(t *testing.T) {
			if got := FromArgv0(tc.argv0); got != tc.want {
				t.Errorf("FromArgv0(%q) = %v, want %v", tc.argv0, got, tc.want)
			}
		})
	}
}

// TestBasenameIsClosedOverSevenNames pins the property that makes widening
// FromArgv0 safe for drv hashes: however a tool was spelled on the
// command line, Basename() — which is what lands in the derivation as
// toolBasename — returns one of seven canonical strings.
//
// So `gcc-15` dispatches as ToolGCC and the drv still says "gcc". That
// is deliberate, not a lossy shortcut: the drv must name a tool that
// exists inside the sandbox, which contains nixgg's pinned compiler and
// not the caller's versioned one.
func TestBasenameIsClosedOverSevenNames(t *testing.T) {
	canonical := map[string]bool{
		"cc": true, "gcc": true, "c++": true,
		"g++": true, "ar": true, "ranlib": true, "ld": true,
	}
	spellings := []string{
		"cc", "gcc", "c++", "g++", "ar", "ranlib",
		"gcc-15", "clang", "clang++", "llvm-ar",
		"x86_64-unknown-linux-gnu-gcc", "x86_64-linux-gnu-g++-14",
		"/usr/bin/gcc", "arm-none-eabi-ranlib",
		"ld", "ld.bfd", "ld.gold", "ld.lld", "x86_64-linux-gnu-ld",
	}
	for _, s := range spellings {
		tool := FromArgv0(s)
		if tool == ToolUnknown {
			t.Errorf("FromArgv0(%q) = ToolUnknown; expected a real role", s)
			continue
		}
		b := tool.Basename()
		if !canonical[b] {
			t.Errorf("FromArgv0(%q).Basename() = %q, which is not one of the seven "+
				"canonical names — this WOULD change toolBasename in the drv and "+
				"break drv-equivalence", s, b)
		}
	}
	// ToolUnknown has no basename; nothing should map to "".
	if got := ToolUnknown.Basename(); got != "" {
		t.Errorf("ToolUnknown.Basename() = %q, want empty", got)
	}
}

// TestSplitRspLine pins response-file tokenisation.
//
// The bug this replaces: `strings.Fields` split the line first and a
// per-token unquote ran second, so any quoted argument containing a space
// — the only reason to quote at all — was already shattered by the time
// unquoting happened:
//
//	"-DGREETING=hello world"  ->  ["-DGREETING=hello, world"]
//
// each fragment keeping a stray quote character. cmake and ninja emit
// exactly that shape for defines with spaces, and ExpandRspfiles runs on
// every shim invocation, so the corrupted flag went straight to the
// compiler. Confirmed against real gcc: it treats the quoted run as one
// argument whose macro body is `hello world`.
func TestSplitRspLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want []string
	}{
		{"plain", `-O2 -Wall`, []string{"-O2", "-Wall"}},
		{"empty", ``, nil},
		{"only spaces", `   `, nil},
		{"tabs separate too", "-O2\t-Wall", []string{"-O2", "-Wall"}},

		// The regression.
		{"double-quoted value with a space",
			`-DGREETING="hello world" -DA=1`,
			[]string{"-DGREETING=hello world", "-DA=1"}},
		{"whole arg double-quoted",
			`"-DGREETING=hello world" -DA=2`,
			[]string{"-DGREETING=hello world", "-DA=2"}},
		{"single-quoted value with a space",
			`-DGREETING='hello world'`,
			[]string{"-DGREETING=hello world"}},
		{"several quoted args on one line",
			`"-DA=x y" "-DB=p q"`,
			[]string{"-DA=x y", "-DB=p q"}},

		// Quoting minutiae the drivers permit.
		{"quote opens mid-token", `-DX="a b"c`, []string{"-DX=a bc"}},
		{"empty quoted value still yields a token", `-DX=""`, []string{"-DX="}},
		{"bare empty quotes yield an empty token", `""`, []string{""}},
		{"single quotes are literal inside double quotes",
			`-DX="it's"`, []string{"-DX=it's"}},
		{"double quotes are literal inside single quotes",
			`-DX='say "hi"'`, []string{`-DX=say "hi"`}},
		{"backslash escapes inside double quotes",
			`-DX="a\"b"`, []string{`-DX=a"b`}},
		{"backslash is literal inside single quotes",
			`-DX='a\b'`, []string{`-DX=a\b`}},
		{"windows-style path keeps its backslashes unquoted",
			`-IC:\proj\inc`, []string{`-IC:\proj\inc`}},

		// Malformed input must degrade, not crash: the compiler will
		// reject a bad flag on its own terms, and erroring here would
		// turn that into a nixgg failure.
		{"unterminated double quote", `-DX="a b`, []string{"-DX=a b"}},
		{"unterminated single quote", `-DX='a b`, []string{"-DX=a b"}},
		{"lone quote", `"`, []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := splitRspLine(tc.line)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("splitRspLine(%q)\n got: %q\nwant: %q", tc.line, got, tc.want)
			}
		})
	}
}

// TestExpandRspfilesPreservesQuotedSpaces drives the whole path through a
// real file on disk, since that is what a shim actually sees. The unit
// test above pins the tokeniser; this pins that ExpandRspfiles uses it.
func TestExpandRspfilesPreservesQuotedSpaces(t *testing.T) {
	dir := t.TempDir()
	rsp := filepath.Join(dir, "link.rsp")
	body := "-DGREETING=\"hello world\"\n-O2\n\"-DPATH=/a b/c\"\n"
	if err := os.WriteFile(rsp, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got := ExpandRspfiles([]string{"cc", "@" + rsp, "main.c"})
	want := []string{"cc", "-DGREETING=hello world", "-O2", "-DPATH=/a b/c", "main.c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExpandRspfiles\n got: %q\nwant: %q", got, want)
	}
}

// TestExpandRspfilesLeavesNonFilesAlone pins the existing guard: plenty
// of legitimate flags start with @, so an @arg that isn't a readable file
// must pass through untouched.
func TestExpandRspfilesLeavesNonFilesAlone(t *testing.T) {
	in := []string{"cc", "@notafile", "-O2"}
	got := ExpandRspfiles(in)
	if !reflect.DeepEqual(got, in) {
		t.Errorf("ExpandRspfiles mangled a non-file @arg\n got: %q\nwant: %q", got, in)
	}
}

// rustc's @-file format is not the compiler drivers': one line is one
// argument, verbatim. The kernel's generated cfg file is the case that
// matters, and it fails loudly rather than subtly — rustc requires the
// quotes that shell tokenisation removes:
//
//	error: invalid `--cfg` argument: `CONFIG_RTC_DRV_CROS_EC=m`
//
// which is where this came from.
func TestExpandRustArgfilesKeepsLinesVerbatim(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rustc_cfg")
	body := "--cfg=CONFIG_RTC_DRV_CROS_EC\n" +
		"--cfg=CONFIG_RTC_DRV_CROS_EC=\"m\"\n" +
		"--cfg=CONFIG_MSG=\"hello world\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got := ExpandRustArgfiles([]string{"--edition=2021", "@" + path, "lib.rs"})
	want := []string{
		"--edition=2021",
		"--cfg=CONFIG_RTC_DRV_CROS_EC",
		`--cfg=CONFIG_RTC_DRV_CROS_EC="m"`,
		`--cfg=CONFIG_MSG="hello world"`,
		"lib.rs",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExpandRustArgfiles =\n  %q\nwant\n  %q", got, want)
	}

	// The compiler-driver expander must still do its own thing — this is
	// a second convention, not a replacement.
	drv := ExpandRspfiles([]string{"@" + path})
	for _, a := range drv {
		if strings.Contains(a, `="m"`) {
			t.Errorf("ExpandRspfiles now behaves like the rustc one: %q", drv)
		}
	}
}
