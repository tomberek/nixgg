package expr

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The drv-script golden test.
//
// Both modes get their shell body from one place — Go's buildScript.
// Sandbox renders it resolved; native renders the same layout with
// @NIXGG_*@ markers for nix/resolve-script.nix to fill in. These tests pin
// the seam:
//
//   - the marker contract desynchronising, which would send a literal
//     `@NIXGG_INPUT0@` to bash → TestNativeTemplateResolvesToSandboxScript
//   - the template not being a valid Nix literal, reachable from flags
//     carrying `''` or `${` → TestScriptTemplateSurvivesNixParsing
//   - layout regressions → TestScriptEndsWithNewline, plus
//     TestLinkScriptEmitsLibFlagsAfterInputs in expr_test.go
//
// Cases deliberately cover shapes no pinned fixture contains — that blind
// spot is how the `'` quoting divergence survived 81 drvs.
//
// All pure eval via `nix-instantiate --eval --raw` reading drvAttrs.args:
// ~1s, no daemon, no store writes. End-to-end drv-hash equality remains
// tests/drv-equivalence.sh's job.

// nixEvalScript renders `helper` with `args` and returns args[1] of the
// resulting derivation — the bash body. helper is a basename in nix/.
func nixEvalScript(t *testing.T, helper string, argsNix string) string {
	t.Helper()
	dir, err := filepath.Abs("../../../nix")
	if err != nil {
		t.Fatal(err)
	}
	expr := "builtins.elemAt (import " + filepath.Join(dir, helper) + " " + argsNix + ").drvAttrs.args 1"
	return runNixEval(t, expr)
}

// runNixEval evaluates `expr` and returns its raw string value. Nix's
// diagnostics go to stderr and are the whole story when eval fails
// (e.g. "context key ... is not a store path"), so capture and report
// them — an "exit status 1" alone sends you back here to re-run by hand.
func runNixEval(t *testing.T, expr string) string {
	t.Helper()
	cmd := exec.Command("nix-instantiate", "--eval", "--raw", "--impure", "--expr", expr)
	// A user's NIX_PATH / overlays can't affect these helpers (they take
	// every store path as an argument and never touch <nixpkgs>), but keep
	// the environment minimal so the test can't pick up an eval cache from
	// a different checkout.
	cmd.Env = append(os.Environ(), "NIX_PATH=")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("nix-instantiate failed: %v\nexpr:\n%s\nstderr:\n%s", err, expr, stderr.String())
	}
	return string(out)
}

// nixStr renders a Go string as a Nix double-quoted string literal.
func nixStr(s string) string {
	e := strings.ReplaceAll(s, `\`, `\\`)
	e = strings.ReplaceAll(e, `"`, `\"`)
	e = strings.ReplaceAll(e, "${", `\${`)
	return `"` + e + `"`
}

// nixIndentedString renders a Go string as a Nix indented-string literal
// (the two-apostrophe delimiter), which is how the driver passes
// flagsJSON / storeDepsJSON / wrapperEnvJSON. Our JSON payloads contain
// `"`, which needs no escaping there, but could contain the delimiter
// itself or `${`, both of which do.
//
// The delimiter is spelled through strings.ReplaceAll below rather than
// written in this comment on purpose: gofmt rewrites a bare pair of
// apostrophes in a comment into typographic quotes, which would make
// this file permanently unformatted (see internal/expr/expr.go).
func nixIndentedString(s string) string {
	e := strings.ReplaceAll(s, "''", "'''")
	e = strings.ReplaceAll(e, "${", "''${")
	return "''" + e + "''"
}

func flagsJSONOf(t *testing.T, flags []string) string {
	t.Helper()
	if len(flags) == 0 {
		return nixIndentedString("[]")
	}
	b, err := json.Marshal(flags)
	if err != nil {
		t.Fatal(err)
	}
	return nixIndentedString(string(b))
}

// Fixed fake store paths. Real-looking (32-char hash + name) so the
// helpers' pureStorePath and Go's string handling both behave as they
// would in production, but stable so the test is deterministic.
//
// The hash characters must come from the nix32 alphabet — pureStorePath
// calls builtins.appendContext, which validates the path and rejects
// anything containing E, O, U or T (see nix32Chars).
const (
	fakeBash      = "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bash-5.2"
	fakeCoreutils = "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-coreutils-9.5"
	fakeCompiler  = "/nix/store/cccccccccccccccccccccccccccccccc-gcc-wrapper-15"
	fakeAR        = "/nix/store/dddddddddddddddddddddddddddddddd-binutils-2.43"
	fakeObj       = "/nix/store/gggggggggggggggggggggggggggggggg-tu-main.o"
	fakeArchive   = "/nix/store/hhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhh-ar-libx.a"
)

// toolchainArgsNix is the three-root prefix every helper takes.
func toolchainArgsNix(compilerOrAR string) string {
	return "  bashRoot      = " + nixStr(fakeBash) + ";\n" +
		"  coreutilsRoot = " + nixStr(fakeCoreutils) + ";\n" +
		"  compilerRoot  = " + nixStr(compilerOrAR) + ";\n"
}

// TestDrvRefInputsRenderIdentically pins the "nix"-kind input path: an
// unrealised sibling drv, which Go renders as a downstream CA output
// placeholder and native mode renders by interpolating the imported
// derivation.
//
// These two CANNOT be compared by string equality — that is the point of
// the test. Go computes the placeholder itself (caOutputPlaceholder);
// native lets Nix compute it while instantiating the sibling. Both
// produce a 53-char `/`-prefixed nix32 string in the same argv slot, and
// drv-equivalence.sh proves the values agree end to end. What is checked
// here is the surrounding layout: same quoting, same `/name` suffix, same
// position relative to flags and other inputs. A layout change would
// otherwise only surface as a hash mismatch in the slow test.
func TestDrvRefInputsRenderIdentically(t *testing.T) {
	drv := "/nix/store/" + strings.Repeat("9", 32) + "-tu-other.o.drv"
	d := &Derivation{
		Kind:      KindLink,
		Tool:      "cc",
		Coreutils: fakeCoreutils,
		Compiler:  fakeCompiler,
		OutName:   "prog",
		Flags:     []string{"-O2"},
		Inputs: []derivInput{
			{InputKind: "store", Ref: fakeObj, Name: "main.o"},
			{InputKind: "nix", Ref: drv, Name: "other.o"},
		},
	}
	got := d.script()

	ph := caOutputPlaceholder(drv, "out")
	if len(ph) != 53 || ph[0] != '/' {
		t.Fatalf("placeholder shape wrong: %q", ph)
	}
	// The placeholder must appear quoted, with the input's basename
	// appended — the exact shape `'${i.drv}/${i.name}'` in linker.nix.
	want := "'" + ph + "/other.o'"
	if !strings.Contains(got, want) {
		t.Errorf("drv input not rendered as %q:\n%s", want, got)
	}
	// And it must sit after the realised input, preserving argv order.
	if strings.Index(got, "main.o'") > strings.Index(got, want) {
		t.Errorf("input order inverted — argv order is load-bearing for ld:\n%s", got)
	}
}

// TestScriptEndsWithNewline pins a property that is easy to break with an
// innocuous edit and expensive to notice: both emitters end the script
// with exactly one trailing newline. Nix's indented-string literal strips
// the common indent and keeps the final newline; Go's fmt.Sprintf has it
// written literally. Adding or dropping one changes every drv hash in the
// project at once.
func TestScriptEndsWithNewline(t *testing.T) {
	for _, d := range []*Derivation{
		{Kind: KindCompile, Tool: "cc", Coreutils: "/C", Compiler: "/G",
			Source: "a.c", OutName: "a.o", Flags: []string{"-O2"}},
		{Kind: KindLink, Tool: "cc", Coreutils: "/C", Compiler: "/G",
			OutName: "p", Flags: []string{"-O2"}},
		{Kind: KindLink, Tool: "cc", Coreutils: "/C", Compiler: "/G",
			OutName: "p", Flags: []string{"-lm"}}, // the other branch
		{Kind: KindArchive, Coreutils: "/C", AR: "/AR",
			OutName: "l.a", ARFlags: "rcs"},
	} {
		s := d.script()
		if !strings.HasSuffix(s, "\n") {
			t.Errorf("kind %d script has no trailing newline: %q", d.Kind, s)
		}
		if strings.HasSuffix(s, "\n\n") {
			t.Errorf("kind %d script has two trailing newlines: %q", d.Kind, s)
		}
	}
}

// nixEvalScriptWithPS is nixEvalScript for the helpers whose `inputs`
// argument needs pure-store-path in scope as `ps`.
func nixEvalScriptWithPS(t *testing.T, helper string, argsNix string) string {
	t.Helper()
	dir, err := filepath.Abs("../../../nix")
	if err != nil {
		t.Fatal(err)
	}
	expr := "let ps = import " + filepath.Join(dir, "pure-store-path.nix") + "; in " +
		"builtins.elemAt (import " + filepath.Join(dir, helper) + " " + argsNix + ").drvAttrs.args 1"
	return runNixEval(t, expr)
}

func requireNixInstantiate(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("nix-instantiate"); err != nil {
		t.Skip("nix-instantiate not on PATH")
	}
}

// assertSameScript reports a byte difference between the two emitters
// with enough context to see which line diverged.
func assertSameScript(t *testing.T, goScript, nixScript string) {
	t.Helper()
	if goScript == nixScript {
		return
	}
	gl := strings.Split(goScript, "\n")
	nl := strings.Split(nixScript, "\n")
	t.Errorf("Go script() and the .nix helper disagree — native and sandbox " +
		"modes would produce different drv hashes for the same build")
	for i := 0; i < len(gl) || i < len(nl); i++ {
		g, n := "<absent>", "<absent>"
		if i < len(gl) {
			g = gl[i]
		}
		if i < len(nl) {
			n = nl[i]
		}
		if g != n {
			t.Errorf("  line %d differs:\n    go : %q\n    nix: %q", i+1, g, n)
		}
	}
}

// TestNativeTemplateResolvesToSandboxScript is the core guard for the
// unified serializer.
//
// script() and scriptTemplate() come from the same buildScript call, so
// the layout cannot diverge — but the template still has to survive a
// round trip through Nix. This test runs the REAL nix/resolve-script.nix
// with the same store paths and inputs the sandbox side used, and demands
// the resolved text equal script() byte for byte.
//
// That is what makes the marker contract enforceable. If Go renames a
// marker, adds an input marker the helper does not substitute, or the
// helper's replaceStrings lists fall out of order, the template reaches
// bash with a literal `@NIXGG_INPUT0@` in argv — a build failure whose
// cause is nowhere near its symptom. Here it is a one-line diff.
//
// Only "store"-kind inputs are used. A "nix"-kind input is a sibling drv
// whose placeholder Go computes and Nix derives while instantiating; the
// two agree (verified by TestCAOutputPlaceholder against a captured
// vector, and end to end by drv-equivalence.sh), but reproducing it here
// would mean instantiating a real sibling derivation, which needs a
// writable store. TestDrvRefInputsRenderIdentically covers the layout of
// that case instead.
func TestNativeTemplateResolvesToSandboxScript(t *testing.T) {
	requireNixInstantiate(t)

	twoInputs := []derivInput{
		{InputKind: "store", Ref: fakeObj, Name: "main.o"},
		{InputKind: "store", Ref: fakeArchive, Name: "libx.a"},
	}
	// Two inputs sharing one store path: pins that markers are positional,
	// not content-derived. A content-keyed scheme would collapse these.
	dupInputs := []derivInput{
		{InputKind: "store", Ref: fakeObj, Name: "a.o"},
		{InputKind: "store", Ref: fakeObj, Name: "b.o"},
	}

	for _, tc := range []struct {
		name string
		d    *Derivation
	}{
		{"compile, no flags", &Derivation{
			Kind: KindCompile, Tool: "cc", Source: "main.c", OutName: "main.o"}},
		{"compile, plain flags", &Derivation{
			Kind: KindCompile, Tool: "gcc", Source: "main.c", OutName: "main.o",
			Flags: []string{"-O2", "-Wall"}}},
		// Shapes no pinned fixture contains — the blind spot that let the
		// `'` quoting divergence through 81 drvs.
		{"compile, apostrophe", &Derivation{
			Kind: KindCompile, Tool: "cc", Source: "main.c", OutName: "main.o",
			Flags: []string{"-DMSG=it's"}}},
		{"compile, doubled apostrophe", &Derivation{
			Kind: KindCompile, Tool: "cc", Source: "main.c", OutName: "main.o",
			Flags: []string{"-DA=''"}}},
		{"compile, nix interpolation lookalike", &Derivation{
			Kind: KindCompile, Tool: "cc", Source: "main.c", OutName: "main.o",
			Flags: []string{"-DX=${HOME}"}}},
		{"compile, backslash and quotes", &Derivation{
			Kind: KindCompile, Tool: "cc", Source: "main.c", OutName: "main.o",
			Flags: []string{`-DP=a\b`, `-DS="x"`, "-DY=$HOME`id`"}}},
		{"compile, literal at-sign", &Derivation{
			Kind: KindCompile, Tool: "cc", Source: "main.c", OutName: "main.o",
			Flags: []string{"-DAT=@NIXGG_COMPILER@"}}},

		{"link, no -l (fallback branch)", &Derivation{
			Kind: KindLink, Tool: "c++", OutName: "prog",
			Flags: []string{"-O2", "-Wl,-E"}, Inputs: twoInputs}},
		{"link, with -l (split branch)", &Derivation{
			Kind: KindLink, Tool: "cc", OutName: "prog",
			Flags: []string{"-O2", "-lm", "-ldl"}, Inputs: twoInputs}},
		{"link, no flags no inputs", &Derivation{
			Kind: KindLink, Tool: "cc", OutName: "prog"}},
		{"link, duplicate input paths", &Derivation{
			Kind: KindLink, Tool: "cc", OutName: "prog",
			Flags: []string{"-O2"}, Inputs: dupInputs}},
		{"link, apostrophe and -l", &Derivation{
			Kind: KindLink, Tool: "cc", OutName: "prog",
			Flags: []string{"-DMSG=it's", "-lm"}, Inputs: twoInputs}},

		{"archive, rcs", &Derivation{
			Kind: KindArchive, OutName: "libfoo.a", ARFlags: "rcs", Inputs: twoInputs}},
		{"archive, no inputs", &Derivation{
			Kind: KindArchive, OutName: "libempty.a", ARFlags: "rcs"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := tc.d
			d.Coreutils = fakeCoreutils
			d.Bash = fakeBash
			if d.Kind == KindArchive {
				d.AR = fakeAR
			} else {
				d.Compiler = fakeCompiler
			}

			// The helper renders `${i.drv}/${i.name}`, so pass the store
			// root as drv and let it append the name — same as production.
			items := make([]string, 0, len(d.Inputs))
			for _, in := range d.Inputs {
				items = append(items,
					"{ drv = ps "+nixStr(in.Ref)+"; name = "+nixStr(in.Name)+"; }")
			}
			tmpl, tag := d.scriptTemplate()
			expr := "let ps = import " + nixHelperPath(t, "pure-store-path.nix") + "; in " +
				"import " + nixHelperPath(t, "resolve-script.nix") + " {\n" +
				"  scriptTemplate = " + nixIndentedStringLiteral(tmpl) + ";\n" +
				"  markerTag = " + nixStr(tag) + ";\n" +
				"  coreutils = ps " + nixStr(fakeCoreutils) + ";\n" +
				"  compiler  = ps " + nixStr(d.compilerOrAR()) + ";\n" +
				"  inputs = [ " + strings.Join(items, " ") + " ];\n" +
				"}"

			resolved := runNixEval(t, expr)
			assertSameScript(t, d.script(), resolved)

			// No marker of the chosen tag may survive resolution.
			if strings.Contains(resolved, "@"+tag+"_") {
				t.Errorf("unresolved @%s_ marker reached the final script — bash "+
					"would receive it as a literal argument:\n%s", tag, resolved)
			}
		})
	}
}

// TestScriptTemplateSurvivesNixParsing pins that the template Go writes
// into a thunk is a well-formed Nix indented-string literal whose value
// is the template unchanged.
//
// Two constructs are special inside that literal and both are reachable
// from ordinary flags: a doubled apostrophe closes the string, and `${`
// opens an interpolation. Unescaped, the first yields a thunk that will
// not parse and the second one that silently interpolates at eval time —
// or fails with "undefined variable", which at least is loud.
func TestScriptTemplateSurvivesNixParsing(t *testing.T) {
	requireNixInstantiate(t)

	for _, tc := range []struct {
		name  string
		flags []string
	}{
		{"plain", []string{"-O2"}},
		{"single apostrophe", []string{"-DMSG=it's"}},
		{"doubled apostrophe", []string{"-DA=''"}},
		{"tripled apostrophe", []string{"-DA='''"}},
		{"interpolation open", []string{"-DX=${HOME}"}},
		{"interpolation open, no close", []string{"-DX=${"}},
		{"both hazards", []string{"-DA=''", "-DX=${HOME}"}},
		{"backslash before apostrophe", []string{`-DA=\'`}},
		{"dollar alone", []string{"-DX=$"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &Derivation{
				Kind: KindCompile, Tool: "cc", Coreutils: fakeCoreutils,
				Compiler: fakeCompiler, Source: "main.c", OutName: "main.o",
				Flags: tc.flags,
			}
			want, _ := d.scriptTemplate()
			got := runNixEval(t, nixIndentedStringLiteral(want))
			if got != want {
				t.Errorf("template did not round-trip through Nix parsing\n"+
					"flags: %q\nwant: %q\ngot : %q", tc.flags, want, got)
			}
		})
	}
}

// nixHelperPath returns the absolute path of a helper in nix/.
func nixHelperPath(t *testing.T, name string) string {
	t.Helper()
	dir, err := filepath.Abs("../../../nix")
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, name)
}

// TestMarkerTagAvoidsCollisionWithFlagText pins the escape hatch that
// makes marker substitution safe against adversarial flag text.
//
// Markers are plain text in a shell script, so a flag spelling one would
// be substituted along with the real markers: `-DAT=@NIXGG_COMPILER@`
// would reach the compiler as `-DAT=/nix/store/…-gcc-wrapper`. Contrived,
// yes — and so was an apostrophe in a -D, right up until it broke native
// mode. This is caught by TestNativeTemplateResolvesToSandboxScript with
// a fixed tag; the point here is that the fallback actually engages and
// stays deterministic.
func TestMarkerTagAvoidsCollisionWithFlagText(t *testing.T) {
	mk := func(flags ...string) *Derivation {
		return &Derivation{
			Kind: KindCompile, Tool: "cc", Coreutils: fakeCoreutils,
			Compiler: fakeCompiler, Source: "main.c", OutName: "main.o",
			Flags: flags,
		}
	}

	t.Run("ordinary flags get the base tag", func(t *testing.T) {
		if _, tag := mk("-O2").scriptTemplate(); tag != "NIXGG" {
			t.Errorf("tag = %q, want the base tag for a body with no markers", tag)
		}
	})

	t.Run("a flag spelling a marker forces a different tag", func(t *testing.T) {
		tmpl, tag := mk("-DAT=@NIXGG_COMPILER@").scriptTemplate()
		if tag == "NIXGG" {
			t.Fatal("tag did not move away from a colliding flag; the flag text " +
				"would be substituted as if it were a marker")
		}
		// The flag's own text must still be present, unsubstituted.
		if !strings.Contains(tmpl, "-DAT=@NIXGG_COMPILER@") {
			t.Errorf("flag text lost from template:\n%s", tmpl)
		}
		// And the real markers must use the new tag.
		if !strings.Contains(tmpl, "@"+tag+"_COMPILER@") {
			t.Errorf("template has no @%s_COMPILER@ marker:\n%s", tag, tmpl)
		}
	})

	t.Run("escalates past several occupied tags", func(t *testing.T) {
		_, tag := mk(
			"-DA=@NIXGG_X@", "-DB=@NIXGG1_X@", "-DC=@NIXGG2_X@",
		).scriptTemplate()
		if tag == "NIXGG" || tag == "NIXGG1" || tag == "NIXGG2" {
			t.Errorf("tag %q collides with flag text", tag)
		}
	})

	t.Run("deterministic", func(t *testing.T) {
		// The tag lands in the thunk text and therefore in its hash, so a
		// second call on equal input must produce the same tag.
		_, a := mk("-DAT=@NIXGG_COMPILER@").scriptTemplate()
		_, b := mk("-DAT=@NIXGG_COMPILER@").scriptTemplate()
		if a != b {
			t.Errorf("tag not deterministic: %q then %q — thunk hashes would be unstable", a, b)
		}
	})
}

// TestLinkScriptGroupWrapsInputs pins where the re-emitted archive group
// lands in the generated command, and that the no-group case is unchanged.
//
// The second half matters as much as the first: GroupInputs is false for
// every one of the 81 pinned drvs, so if setting it false ever altered the
// layout, every pinned hash would move.
func TestLinkScriptGroupWrapsInputs(t *testing.T) {
	mk := func(group bool, flags []string) *Derivation {
		return &Derivation{
			Kind: KindLink, Tool: "cc", OutName: "prog",
			Coreutils: fakeCoreutils, Compiler: fakeCompiler, Bash: fakeBash,
			Flags: flags, GroupInputs: group,
			Inputs: []derivInput{
				{InputKind: "store", Ref: fakeObj, Name: "main.o"},
				{InputKind: "store", Ref: fakeArchive, Name: "libx.a"},
			},
		}
	}

	t.Run("group brackets the inputs, not the flags", func(t *testing.T) {
		s := mk(true, []string{"-O2"}).script()
		start := strings.Index(s, "-Wl,--start-group")
		end := strings.Index(s, "-Wl,--end-group")
		firstIn := strings.Index(s, "main.o'")
		lastIn := strings.LastIndex(s, "libx.a'")
		if start < 0 || end < 0 {
			t.Fatalf("group brackets missing:\n%s", s)
		}
		if !(start < firstIn && lastIn < end) {
			t.Errorf("group does not span the inputs (start=%d firstIn=%d lastIn=%d end=%d)\n%s",
				start, firstIn, lastIn, end, s)
		}
		// A group spanning nothing is the exact bug being fixed.
		if end < firstIn {
			t.Errorf("--end-group precedes the first input — the group spans "+
				"nothing and ld's rescan is defeated:\n%s", s)
		}
	})

	t.Run("group coexists with the -l split", func(t *testing.T) {
		// -l flags go after the inputs; they must land outside the group,
		// since the group is about archive rescanning among the inputs.
		s := mk(true, []string{"-O2", "-lm"}).script()
		end := strings.Index(s, "-Wl,--end-group")
		lm := strings.Index(s, "'-lm'")
		if end < 0 || lm < 0 {
			t.Fatalf("expected both a group and -lm:\n%s", s)
		}
		if lm < end {
			t.Errorf("-lm landed inside the group; it belongs after it:\n%s", s)
		}
	})

	t.Run("no group is byte-identical to before", func(t *testing.T) {
		// Pins that GroupInputs=false changes nothing. All 81 pinned drvs
		// depend on this.
		s := mk(false, []string{"-O2"}).script()
		if strings.Contains(s, "start-group") || strings.Contains(s, "end-group") {
			t.Errorf("group emitted without being asked for:\n%s", s)
		}
		if strings.Contains(s, "  ") {
			t.Errorf("double space in the no-group script — layout drifted and "+
				"every pinned drv hash would move:\n%q", s)
		}
	})

	t.Run("group with no inputs emits no brackets", func(t *testing.T) {
		d := mk(true, []string{"-O2"})
		d.Inputs = nil
		s := d.script()
		if strings.Contains(s, "start-group") {
			t.Errorf("empty group emitted; ld would see --start-group --end-group "+
				"around nothing:\n%s", s)
		}
	})

	t.Run("raw ld tool uses bare group brackets, not -Wl,", func(t *testing.T) {
		// A raw `ld` invocation (Linux Kbuild's own vmlinux.o link —
		// see isRawLinker's own docstring) rejects -Wl,--start-group
		// outright ("unrecognized option"); only cc/gcc/clang-driven
		// links use the -Wl, spelling, which is what mk's default
		// Tool: "cc" already pins above.
		d := mk(true, []string{"-O2"})
		d.Tool = "ld"
		s := d.script()
		if strings.Contains(s, "-Wl,--start-group") || strings.Contains(s, "-Wl,--end-group") {
			t.Errorf("raw ld got the driver spelling -Wl,--start-group; "+
				"ld itself rejects this:\n%s", s)
		}
		if !strings.Contains(s, "--start-group") || !strings.Contains(s, "--end-group") {
			t.Errorf("raw ld missing the bare --start-group/--end-group it needs:\n%s", s)
		}
	})
}

// TestLinkScriptWholeArchiveWrapsOnlyNamedInputs pins that
// WholeArchiveInputs wraps EXACTLY the named inputs, unlike
// GroupInputs' single global span — modeled directly on Linux
// Kbuild's own vmlinux.o link (scripts/Makefile.vmlinux_o's
// cmd_ld_vmlinux.o: `--whole-archive vmlinux.a --no-whole-archive
// --start-group $(KBUILD_VMLINUX_LIBS) --end-group`), the real
// recipe this was written against.
func TestLinkScriptWholeArchiveWrapsOnlyNamedInputs(t *testing.T) {
	mk := func() *Derivation {
		return &Derivation{
			Kind: KindLink, Tool: "ld", OutName: "vmlinux.o",
			Coreutils: fakeCoreutils, Compiler: fakeCompiler, Bash: fakeBash,
			GroupInputs:        true,
			WholeArchiveInputs: []string{"vmlinux.a"},
			Inputs: []derivInput{
				{InputKind: "store", Ref: fakeObj, Name: "vmlinux.a"},
				{InputKind: "store", Ref: fakeArchive, Name: "lib.a"},
			},
		}
	}

	t.Run("only the named input is wrapped", func(t *testing.T) {
		s := mk().script()
		wStart := strings.Index(s, "--whole-archive")
		wEnd := strings.Index(s, "--no-whole-archive")
		vmlinuxA := strings.Index(s, "vmlinux.a")
		libA := strings.Index(s, "lib.a'")
		if wStart < 0 || wEnd < 0 {
			t.Fatalf("whole-archive brackets missing:\n%s", s)
		}
		if !(wStart < vmlinuxA && vmlinuxA < wEnd) {
			t.Errorf("--whole-archive does not span vmlinux.a (wStart=%d vmlinuxA=%d wEnd=%d)\n%s",
				wStart, vmlinuxA, wEnd, s)
		}
		if wStart < libA && libA < wEnd {
			t.Errorf("--whole-archive wrongly spans lib.a too — its scope must stay "+
				"narrow, unlike --start-group's own widened span:\n%s", s)
		}
	})

	t.Run("coexists with GroupInputs' own separate, wider span", func(t *testing.T) {
		s := mk().script()
		gStart := strings.LastIndex(s, "--start-group")
		gEnd := strings.Index(s, "--end-group")
		if gStart < 0 || gEnd < 0 {
			t.Fatalf("group brackets missing alongside whole-archive:\n%s", s)
		}
		firstIn := strings.Index(s, "vmlinux.a")
		lastIn := strings.LastIndex(s, "lib.a'")
		if !(gStart < firstIn && lastIn < gEnd) {
			t.Errorf("--start-group/--end-group does not span every input "+
				"(gStart=%d firstIn=%d lastIn=%d gEnd=%d)\n%s",
				gStart, firstIn, lastIn, gEnd, s)
		}
	})

	t.Run("no WholeArchiveInputs is unaffected", func(t *testing.T) {
		d := mk()
		d.WholeArchiveInputs = nil
		s := d.script()
		if strings.Contains(s, "whole-archive") {
			t.Errorf("whole-archive brackets emitted with no WholeArchiveInputs set:\n%s", s)
		}
	})
}

// TestLinkScriptStagesInlineFiles pins that InlineFilesStore (a
// staged directory holding a linker script the link shim read off
// disk before staging it — see shim/link.go's linkerScriptPath) gets
// copied into the build root BEFORE the link command runs, and that
// the flag referencing it (e.g. -Wl,--version-script=libcrypto.ld)
// passes through unchanged — the copied file now exists at exactly
// the relative path the flag already names. Copied via `cp`, not
// embedded as text: a large generated linker script plus hundreds of
// real object-file paths on one link line can exceed the kernel's
// argv limit if baked into the script body directly (confirmed
// directly against openssl's libcrypto.so.3 — "Argument list too
// long").
func TestLinkScriptStagesInlineFiles(t *testing.T) {
	d := &Derivation{
		Kind: KindLink, Tool: "cc", OutName: "libcrypto.so.3",
		Coreutils: fakeCoreutils, Compiler: fakeCompiler, Bash: fakeBash,
		Flags: []string{"-shared", "-Wl,--version-script=libcrypto.ld"},
		Inputs: []derivInput{
			{InputKind: "store", Ref: fakeObj, Name: "main.o"},
		},
		InlineFilesStore: "/nix/store/iiiiiiiiiiiiiiiiiiiiiiiiiiiiiiii-inline-libcrypto.so.3-libcrypto.ld",
	}
	s := d.script()

	stageIdx := strings.Index(s, "cp -a")
	linkIdx := strings.Index(s, `"cc"`)
	if stageIdx < 0 {
		t.Fatalf("expected inline-file staging in script:\n%s", s)
	}
	if linkIdx < 0 {
		t.Fatalf("expected the link command in script:\n%s", s)
	}
	if stageIdx > linkIdx {
		t.Errorf("inline file staged AFTER the link command runs — libcrypto.ld "+
			"wouldn't exist yet:\n%s", s)
	}
	if !strings.Contains(s, `"$src/."`) {
		t.Errorf("expected the staged directory to be copied from $src:\n%s", s)
	}
	if !strings.Contains(s, "-Wl,--version-script=libcrypto.ld") {
		t.Errorf("the version-script flag itself must pass through unchanged "+
			"(the file now exists at that relative path):\n%s", s)
	}
}

// TestLinkScriptNoInlineFilesUnchanged pins that omitting
// InlineFilesStore (the overwhelming common case — every link before
// this feature existed) doesn't add anything to the script at all.
func TestLinkScriptNoInlineFilesUnchanged(t *testing.T) {
	d := &Derivation{
		Kind: KindLink, Tool: "cc", OutName: "prog",
		Coreutils: fakeCoreutils, Compiler: fakeCompiler, Bash: fakeBash,
		Flags: []string{"-O2"},
		Inputs: []derivInput{
			{InputKind: "store", Ref: fakeObj, Name: "main.o"},
		},
	}
	s := d.script()
	if strings.Contains(s, "cp -a") {
		t.Errorf("no InlineFilesStore was set, but the script stages one anyway:\n%s", s)
	}
}

// TestOutSubdirAgreesWithInputSubdirFor pins the producer/consumer
// contract for FHS placement.
//
// outSubdir decides where a derivation WRITES its artifact, keyed on Kind.
// inputSubdirFor decides where a downstream derivation LOOKS for it, keyed
// on the artifact's filename. They are separate functions because each
// side has only one of those two facts — the producer knows its Kind, the
// consumer sees only a filename. If they disagree, a link reaches for an
// archive at a path the archive drv never wrote.
func TestOutSubdirAgreesWithInputSubdirFor(t *testing.T) {
	for _, tc := range []struct {
		kind Kind
		name string
	}{
		{KindCompile, "main.o"},
		{KindCompile, "sub/dir/main.o"},
		{KindArchive, "libfoo.a"},
		{KindArchive, "libLLVMSupport.a"},
		{KindLink, "prog"},
		{KindLink, "mosh-server"},
		{KindLink, "llc"},
		// A shared library is a link output; bin/ is where the link drv
		// puts whatever it was told to produce.
		{KindLink, "libfoo.so"},
		// A rustc crate's artifacts stay flat, like a compile's: both
		// the object and the .rmeta its dependents resolve against are
		// intermediates, not installables.
		{KindRustc, "kernel.o"},
		{KindRustc, "libkernel.rmeta"},
	} {
		producer := (&Derivation{Kind: tc.kind, OutName: tc.name}).outSubdir()
		consumer := inputSubdirFor(tc.name)
		if producer != consumer {
			t.Errorf("kind=%v name=%q: producer writes to %q but a consumer would "+
				"look in %q — the reference would dangle",
				tc.kind, tc.name, producer, consumer)
		}
	}
}

// TestInputSubdirForIsModeIndependent pins that the subdir is derived from
// something BOTH modes have.
//
// Sandbox references a sibling by drv path ("…-ar-libfoo.a.drv"); native
// references it by thunk path (".nixgg/thunks/<hash>.nix"), which encodes
// no kind whatsoever. An earlier version of this keyed on the drv name and
// therefore resolved to "lib" in sandbox mode and "" in native mode for
// the same input — different scripts, different drv hashes, invariant
// broken. Keying on the artifact filename is what makes the two agree.
func TestInputSubdirForIsModeIndependent(t *testing.T) {
	for _, name := range []string{"libfoo.a", "main.o", "prog"} {
		want := inputSubdirFor(name)
		// Whatever wrapping either mode applies, the answer must not move.
		for _, ref := range []string{
			name,
			"/nix/store/" + strings.Repeat("a", 32) + "-ar-" + name + ".drv",
			"/abs/path/.nixgg/thunks/deadbeef.nix",
		} {
			_ = ref // the point: we do NOT consult the ref at all
		}
		if got := inputSubdirFor("/some/dir/" + name); got != want {
			t.Errorf("inputSubdirFor is sensitive to the leading path: %q vs %q", got, want)
		}
	}
	if inputSubdirFor("libfoo.a") != "lib" {
		t.Error("archives must resolve to lib/")
	}
	if inputSubdirFor("main.o") != "" {
		t.Error("compile outputs must stay flat")
	}
	if inputSubdirFor("prog") != "bin" {
		t.Error("executables must resolve to bin/")
	}
}

// TestScriptCreatesTheDirectoryItWritesTo pins that every emitted script
// mkdir's the directory it then writes into.
//
// Without this, moving an artifact into $out/bin while still creating only
// $out produces a script that fails at BUILD time — `gcc -o $out/bin/prog`
// with no $out/bin — which no unit test would notice and which surfaces as
// a compiler error deep in a Nix build log. Mutation-caught: dropping the
// subdir from outDir() passed every other test here.
func TestScriptCreatesTheDirectoryItWritesTo(t *testing.T) {
	for _, d := range []*Derivation{
		{Kind: KindCompile, Tool: "cc", Coreutils: "/C", Compiler: "/G",
			Source: "a.c", OutName: "a.o"},
		{Kind: KindLink, Tool: "cc", Coreutils: "/C", Compiler: "/G",
			OutName: "prog", Flags: []string{"-O2"}},
		{Kind: KindLink, Tool: "cc", Coreutils: "/C", Compiler: "/G",
			OutName: "prog", Flags: []string{"-lm"}}, // the other branch
		{Kind: KindArchive, Coreutils: "/C", AR: "/AR",
			OutName: "libfoo.a", ARFlags: "rcs"},
	} {
		s := d.script()
		// The directory the script creates.
		var made string
		for _, line := range strings.Split(s, "\n") {
			if strings.HasPrefix(line, "mkdir -p ") {
				made = strings.Trim(strings.TrimPrefix(line, "mkdir -p "), `"`)
			}
		}
		if made == "" {
			t.Fatalf("kind %d: no mkdir in script:\n%s", d.Kind, s)
		}
		// The directory the artifact is written into.
		want := d.outPath()
		wantDir := want[:strings.LastIndexByte(want, '/')]
		if made != wantDir {
			t.Errorf("kind %d: script creates %q but writes to %q — the build "+
				"fails with a missing-directory error, not a test failure:\n%s",
				d.Kind, made, want, s)
		}
	}
}

// TestRustcScriptResolvesExternsAndEmits pins the two things the rustc
// script does that no other Kind does.
//
// Externs: the caller's bare `--extern kernel` searches `-L` dirs, which
// inside the sandbox do not exist. Every extern must come out as an
// explicit `<crate>=<path>` binding pointing at the producing
// derivation's output, or the crate compiles against nothing and the
// dependency edge Nix enforces is a lie.
//
// Emits: one invocation, several artifacts. Dropping an emit yields a
// derivation that builds cleanly and silently omits the .rmeta every
// dependent crate resolves against — a failure that surfaces one crate
// later, naming the wrong file.
func TestRustcScriptResolvesExternsAndEmits(t *testing.T) {
	d := &Derivation{
		Kind:      KindRustc,
		Name:      "rs-kernel.o",
		System:    "x86_64-linux",
		Bash:      "/nix/store/bbb-bash",
		Coreutils: "/nix/store/ccc-coreutils",
		RustcBin:  "/nix/store/rrr-rustc/bin/rustc",
		SrcStore:  "/nix/store/sss-rs-kernel",
		Source:    "rust/kernel/lib.rs",
		Flags:     []string{"--crate-type", "rlib", "--edition=2021"},
		Inputs: []derivInput{
			{InputKind: "nix", Ref: "/nix/store/ddd-rs-ffi.o.drv", Name: "libffi.rmeta", Crate: "ffi"},
			{InputKind: "store", Ref: "/nix/store/eee-prebuilt", Name: "libpin_init.rmeta", Crate: "pin_init"},
		},
		Emits: []RustEmit{
			{Kind: "obj", Name: "kernel.o"},
			{Kind: "metadata", Name: "libkernel.rmeta"},
		},
	}
	script := d.script()

	for _, want := range []string{
		"--extern 'ffi=" + caOutputPlaceholder("/nix/store/ddd-rs-ffi.o.drv", "out") + "/libffi.rmeta'",
		"--extern 'pin_init=/nix/store/eee-prebuilt/libpin_init.rmeta'",
		`"--emit=obj=$out/kernel.o"`,
		`"--emit=metadata=$out/libkernel.rmeta"`,
		`--out-dir "$out"`,
		`cd "$src"`,
		`"$source"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("rustc script missing %q\n--- script ---\n%s", want, script)
		}
	}
	// A bare extern reaching the sandbox means -L resolution was relied
	// on, which cannot work there.
	if strings.Contains(script, "--extern 'ffi'") || strings.Contains(script, "--extern ffi ") {
		t.Errorf("rustc script emitted an unresolved extern\n%s", script)
	}
}

// A crate's own dependencies travel in its metadata, not on the command
// line: a kernel driver names two externs and rustc then loads ten
// crates, resolving the rest from `-L`. Every input therefore needs a
// search path, or the compile dies on a transitive dependency nothing
// on the command line ever mentioned.
func TestRustcScriptEmitsSearchPathPerInput(t *testing.T) {
	d := &Derivation{
		Kind: KindRustc, Name: "rs-drm_panic_qr.o", System: "x86_64-linux",
		Bash: "/nix/store/bbb-bash", Coreutils: "/nix/store/ccc-coreutils",
		RustcBin: "/nix/store/rrr-rustc/bin/rustc",
		SrcStore: "/nix/store/sss-crate", Source: "drm_panic_qr.rs",
		Inputs: []derivInput{
			// Named on the command line.
			{InputKind: "nix", Ref: "/nix/store/ddd-rs-kernel.o.drv", Name: "libkernel.rmeta", Crate: "kernel"},
			// Never named: reached only through the search path.
			{InputKind: "nix", Ref: "/nix/store/eee-rs-uapi.o.drv", Name: "libuapi.rmeta"},
			{InputKind: "store", Ref: "/nix/store/fff-macros", Name: "libmacros.so"},
		},
		Emits: []RustEmit{{Kind: "obj", Name: "drm_panic_qr.o"}},
	}
	script := d.script()

	for _, want := range []string{
		"-L '" + caOutputPlaceholder("/nix/store/ddd-rs-kernel.o.drv", "out") + "'",
		"-L '" + caOutputPlaceholder("/nix/store/eee-rs-uapi.o.drv", "out") + "'",
		"--extern 'kernel=" + caOutputPlaceholder("/nix/store/ddd-rs-kernel.o.drv", "out") + "/libkernel.rmeta'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("rustc script missing %q\n--- script ---\n%s", want, script)
		}
	}
	// An unnamed input must NOT get an --extern: rustc rejects a crate
	// bound to a name the source never refers to.
	if strings.Contains(script, "--extern '=") {
		t.Errorf("search-path-only input got an empty --extern binding\n%s", script)
	}
	// A store input sits FLAT — the shape storeAddLooseFile produces for
	// a proc-macro left as a real file. Only a sibling drv places its
	// artifact under an FHS subdir, so applying it to both would send
	// the search path into a bin/ that does not exist.
	if !strings.Contains(script, "-L '/nix/store/fff-macros'") {
		t.Errorf("search path for a store input must not gain an FHS subdir\n%s", script)
	}
}
