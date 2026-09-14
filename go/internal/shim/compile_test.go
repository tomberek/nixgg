package shim

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tbereknyei/nixgg/internal/expr"
	"github.com/tbereknyei/nixgg/internal/mode"
	"github.com/tbereknyei/nixgg/internal/scan"
)

// TestRewriteFlagsKeepsForceIncludes guards a bug that already shipped
// and was fixed once, in 267722b: `-include <file>` names a file to
// textually include, not an include directory. Treating it as a
// directory meant the file was staged wrong and the flag dropped, so
// the TU compiled without it — a silent miscompile, not a build
// failure. No fixture uses -include, which is why the integration test
// never caught it; this is the unit test that would have.
//
// Every case asserts the full output slice rather than Contains(),
// because the failure mode is positional: forceInc must land after the
// staged -I flags, or a same-named header in an earlier directory wins.
func TestRewriteFlagsKeepsForceIncludes(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		caller, staged, store, forceInc []string
		want                            []string
	}{
		{
			name:     "caller -include is replaced by the staged one",
			caller:   []string{"-O2", "-include", "config.h", "-Wall"},
			staged:   []string{"-I", "."},
			forceInc: []string{"-include", "config.h"},
			want:     []string{"-O2", "-Wall", "-I", ".", "-include", "config.h"},
		},
		{
			name:     "forceInc lands after staged include dirs",
			caller:   []string{"-include", "pch.h"},
			staged:   []string{"-I", "inc", "-I", "gen"},
			forceInc: []string{"-include", "pch.h"},
			want:     []string{"-I", "inc", "-I", "gen", "-include", "pch.h"},
		},
		{
			name:     "several -include flags all survive",
			caller:   []string{"-include", "a.h", "-O1", "-include", "b.h"},
			staged:   []string{"-I", "."},
			forceInc: []string{"-include", "a.h", "-include", "b.h"},
			want:     []string{"-O1", "-I", ".", "-include", "a.h", "-include", "b.h"},
		},
		{
			name:     "include dirs are still dropped, both spellings",
			caller:   []string{"-I", "/abs", "-I/abs2", "-isystem", "/sys", "-O2"},
			staged:   []string{"-I", "."},
			forceInc: nil,
			want:     []string{"-O2", "-I", "."},
		},
		{
			name:     "no -include at all is unchanged apart from appends",
			caller:   []string{"-O2", "-Wall"},
			staged:   []string{"-I", "."},
			store:    []string{"-I", "/nix/store/x/include"},
			forceInc: nil,
			want:     []string{"-O2", "-Wall", "-I", ".", "-I", "/nix/store/x/include"},
		},
		{
			name:     "-include as the final token doesn't run off the end",
			caller:   []string{"-O2", "-include"},
			staged:   nil,
			forceInc: nil,
			want:     []string{"-O2"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteFlags(tc.caller, tc.staged, tc.store, tc.forceInc)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("rewriteFlags mismatch\ncaller  : %q\nstaged  : %q\nforceInc: %q\ngot     : %q\nwant    : %q",
					tc.caller, tc.staged, tc.forceInc, got, tc.want)
			}
		})
	}
}

// TestRewriteFlagsDropsCallerIncludePaths pins that a caller's
// `-include` path never survives verbatim: it's relative to the
// caller's own cwd, which doesn't exist inside the sandbox; only the
// staged replacement in forceInc is valid there.
func TestRewriteFlagsDropsCallerIncludePaths(t *testing.T) {
	got := rewriteFlags(
		[]string{"-include", "../../outside/config.h", "-O2"},
		[]string{"-I", "."},
		nil,
		[]string{"-include", "config.h"},
	)
	for _, g := range got {
		if strings.Contains(g, "outside") {
			t.Errorf("caller's -include path leaked into the sandbox flags: %q", got)
		}
	}
}

// TestParseCompileArgsExplicitLanguage pins that `-x <lang>` overrides
// extension-based source detection. isSource matches on suffix, so a
// precompiled-header compile (`g++ -x c++-header -c pch.h -o
// pch.h.gch`, as CMake's target_precompile_headers and Qt's build
// emit) had no source by nixgg's reckoning and fell to Passthrough
// uncached.
func TestParseCompileArgsExplicitLanguage(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		wantSource string
		wantOutput string
		wantOK     bool
	}{
		{
			name:       "c++ precompiled header",
			args:       []string{"-x", "c++-header", "-c", "pch.h", "-o", "pch.h.gch"},
			wantSource: "pch.h", wantOutput: "pch.h.gch", wantOK: true,
		},
		{
			name:       "c precompiled header",
			args:       []string{"-x", "c-header", "-c", "pch.h", "-o", "pch.h.gch"},
			wantSource: "pch.h", wantOutput: "pch.h.gch", wantOK: true,
		},
		{
			name:       "explicit language with an odd extension",
			args:       []string{"-x", "c++", "-c", "gen.inc", "-o", "gen.o"},
			wantSource: "gen.inc", wantOutput: "gen.o", wantOK: true,
		},
		{
			name:   "header with no -x is still not a source",
			args:   []string{"-c", "pch.h", "-o", "pch.h.gch"},
			wantOK: false,
		},
		{
			name:       "the language token is not the source",
			args:       []string{"-x", "c++-header", "-c", "real.h"},
			wantSource: "real.h", wantOutput: "real.h.gch", wantOK: true,
		},
		{
			name:   "two sources under -x still bails",
			args:   []string{"-x", "c++", "-c", "a.cc", "b.cc"},
			wantOK: false,
		},
		{
			name:   "-x with no value bails rather than indexing past the end",
			args:   []string{"-c", "a.cc", "-x"},
			wantOK: false,
		},
		{
			// AC_LINK_IFELSE / AC_RUN_IFELSE compile a conftest straight to
			// a binary — no -c — so this must bail into Passthrough.
			// TestRealiseCarveoutOutputsAreAlwaysFlat's unreachability
			// claim rests on this holding.
			name:   "a no -c conftest link line is not a compile",
			args:   []string{"conftest.c", "-o", "conftest"},
			wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, out, _, flags, ok := parseCompileArgs(tc.args)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (args %q)", ok, tc.wantOK, tc.args)
			}
			if !ok {
				return
			}
			if src != tc.wantSource {
				t.Errorf("source = %q, want %q", src, tc.wantSource)
			}
			if out == "" {
				out = defaultOutputName(src, flags)
			}
			if out != tc.wantOutput {
				t.Errorf("output = %q, want %q", out, tc.wantOutput)
			}
			var sawX bool
			for i := 0; i+1 < len(flags); i++ {
				if flags[i] == "-x" {
					sawX = true
				}
			}
			if !sawX {
				t.Errorf("-x dropped from flags %q — the sandbox compile would "+
					"guess the language from the extension instead", flags)
			}
		})
	}
}

// TestPCHDefaultOutputName pins gcc's naming rule for a precompiled
// header, which differs from the object rule: .gch is appended to the
// source's full name, so pch.h becomes pch.h.gch, not pch.gch.
// Verified against gcc by compiling a header with no -o.
func TestPCHDefaultOutputName(t *testing.T) {
	if !isHeaderLang("c++-header") {
		t.Fatal("c++-header must be recognised as a header language")
	}
	for _, lang := range []string{"c-header", "c++-header",
		"objective-c-header", "objective-c++-header"} {
		if !isHeaderLang(lang) {
			t.Errorf("isHeaderLang(%q) = false, want true", lang)
		}
	}
	for _, lang := range []string{"c", "c++", "assembler", ""} {
		if isHeaderLang(lang) {
			t.Errorf("isHeaderLang(%q) = true, want false — a normal compile "+
				"must still get the .o naming rule", lang)
		}
	}
	if got := langOf([]string{"-O2", "-x", "c++-header", "-Wall"}); got != "c++-header" {
		t.Errorf("langOf = %q, want \"c++-header\"", got)
	}
	if got := langOf([]string{"-O2"}); got != "" {
		t.Errorf("langOf with no -x = %q, want \"\"", got)
	}
}

// TestDefaultOutputName covers the -o-omitted path directly, calling
// the production function rather than restating its rule inline (an
// earlier version of the PCH test above did that, which let a mutation
// pass clean).
func TestDefaultOutputName(t *testing.T) {
	for _, tc := range []struct {
		source string
		flags  []string
		want   string
	}{
		{"a.cc", nil, "a.o"},
		{"src/b.c", nil, "b.o"},
		{"a.b.cc", nil, "a.b.o"},
		{"noext", nil, "noext.o"},
		{"pch.h", []string{"-x", "c++-header"}, "pch.h.gch"},
		{"inc/pch.hpp", []string{"-x", "c++-header"}, "pch.hpp.gch"},
		{"pch.h", []string{"-x", "c-header"}, "pch.h.gch"},
		{"gen.inc", []string{"-x", "c++"}, "gen.o"},
	} {
		if got := defaultOutputName(tc.source, tc.flags); got != tc.want {
			t.Errorf("defaultOutputName(%q, %q) = %q, want %q",
				tc.source, tc.flags, got, tc.want)
		}
	}
}

// TestOnlyDashXLegitimisesAnOddSource pins that the "any token can be
// the source" relaxation is gated on -x specifically, not on any
// two-argument flag: -Xlinker and -Xassembler go through the same
// parser branch, and their values say nothing about the source
// language.
func TestOnlyDashXLegitimisesAnOddSource(t *testing.T) {
	if _, _, _, _, ok := parseCompileArgs([]string{
		"-c", "-Xassembler", "--noexecstack", "mystery.dat", "-o", "out.o",
	}); ok {
		t.Error("an -Xassembler value legitimised a non-source token as the " +
			"compile source; only -x names a language")
	}
	if _, _, _, _, ok := parseCompileArgs([]string{
		"-c", "-Xlinker", "-z", "mystery.dat", "-o", "out.o",
	}); ok {
		t.Error("-Xlinker legitimised a non-source token as the compile source")
	}
	if src, _, _, _, ok := parseCompileArgs([]string{
		"-x", "c++-header", "-c", "pch.h", "-o", "pch.h.gch",
	}); !ok || src != "pch.h" {
		t.Errorf("-x path broken: src=%q ok=%v", src, ok)
	}
}

// TestRealiseCarveoutOutputsAreAlwaysFlat pins that every compile-side
// realise-mode probe's default output stays flat, per
// expr.ArtifactSubdir, which is what lets compile.go's own
// realiseAndLink call site pass "" as the subdir unconditionally.
// realiseAndLink no longer derives the subdir from the output's own
// name (it used to, via expr.ArtifactSubdir, but that guessed wrong
// for Kbuild's vmlinux.o, a link output named like a compile one).
func TestRealiseCarveoutOutputsAreAlwaysFlat(t *testing.T) {
	for _, source := range []string{
		"conftest.c", "conftest.cpp",
		"testCCompiler.c", "CMakeCXXCompilerId.c",
		"CheckFunctionExists.c", "CheckIncludeFile.c",
	} {
		if mode.For(source) != mode.Realise {
			t.Fatalf("%q no longer matches mode.Realise — update this test's "+
				"fixture list, don't just delete the case", source)
		}
		out := defaultOutputName(source, nil)
		if sub := expr.ArtifactSubdir(out); sub != "" {
			t.Errorf("source %q compiles to %q by default, whose ArtifactSubdir "+
				"is %q, want flat (\"\") for a compile-side realise probe",
				source, out, sub)
		}
	}

	// A link-style probe (AC_LINK_IFELSE / AC_RUN_IFELSE, output
	// "conftest" with no extension) never reaches Compile at all: no
	// -c means parseCompileArgs returns ok=false before mode.For is
	// consulted, so it's not a hole in this guard.
}

// TestIsKbuildElfProbe pins the Passthrough carveout for Kbuild's
// scripts/mod/empty.o: mode.Realise's `nix build --file` is
// incompatible with sandbox mode, and this probe has no headers and
// gains nothing from CA-hashing anyway.
func TestIsKbuildElfProbe(t *testing.T) {
	for _, tc := range []struct {
		source string
		want   bool
	}{
		{"scripts/mod/empty.c", true},
		{"scripts/mod/empty.o", true},
		{"/build/linux-6.12/scripts/mod/empty.c", true},
		{"empty.c", false},
		{"scripts/mod/modpost.c", false},
		{"conftest.c", false},
	} {
		if got := isKbuildElfProbe(tc.source); got != tc.want {
			t.Errorf("isKbuildElfProbe(%q) = %v, want %v", tc.source, got, tc.want)
		}
	}
}

// TestIsKbuildRealmodeObj pins the Passthrough carveout for Kbuild's
// arch/x86/realmode/rm/{header,trampoline_32,trampoline_64,stack,
// reboot}.o: confirmed directly against a real sandboxed build,
// routing through mode.Realise hit the same "no substituter" failure
// empty.o did.
func TestIsKbuildRealmodeObj(t *testing.T) {
	for _, tc := range []struct {
		source string
		want   bool
	}{
		{"arch/x86/realmode/rm/header.S", true},
		{"arch/x86/realmode/rm/header.o", true},
		{"arch/x86/realmode/rm/trampoline_32.S", true},
		{"arch/x86/realmode/rm/trampoline_64.o", true},
		{"arch/x86/realmode/rm/stack.o", true},
		{"arch/x86/realmode/rm/reboot.o", true},
		{"/build/linux-6.12/arch/x86/realmode/rm/reboot.o", true},
		{"arch/x86/realmode/rm/realmode.lds.S", false},
		{"arch/x86/kernel/head_32.S", false},
	} {
		if got := isKbuildRealmodeObj(tc.source); got != tc.want {
			t.Errorf("isKbuildRealmodeObj(%q) = %v, want %v", tc.source, got, tc.want)
		}
	}
}

// TestIsKbuildVDSO32Obj pins the Passthrough carveout for Kbuild's
// arch/x86/entry/vdso/vdso32/{note,system_call,sigreturn,
// vclock_gettime,vgetcpu}.o. Confirmed directly against a real
// sandboxed build: with these Passthrough'd, vdso32.so.dbg's own link
// falls to RealiseThunkArgsAndPassthrough's Passthrough instead of
// mode.ForLink's sandbox-incompatible realiseAndLink.
func TestIsKbuildVDSO32Obj(t *testing.T) {
	for _, tc := range []struct {
		source string
		want   bool
	}{
		{"arch/x86/entry/vdso/vdso32/note.S", true},
		{"arch/x86/entry/vdso/vdso32/system_call.S", true},
		{"arch/x86/entry/vdso/vdso32/sigreturn.o", true},
		{"arch/x86/entry/vdso/vdso32/vclock_gettime.o", true},
		{"arch/x86/entry/vdso/vdso32/vgetcpu.c", true},
		{"/build/linux-6.12/arch/x86/entry/vdso/vdso32/vgetcpu.o", true},
		{"arch/x86/entry/vdso/vdso32/vdso32.lds.S", false},
		{"arch/x86/entry/vdso/vclock_gettime.c", false},
	} {
		if got := isKbuildVDSO32Obj(tc.source); got != tc.want {
			t.Errorf("isKbuildVDSO32Obj(%q) = %v, want %v", tc.source, got, tc.want)
		}
	}
}

// TestParseCompileArgsCapturesDepfile pins depfile-path recovery for
// the three shapes a compile invocation can request dependency output
// in: Kbuild's comma-joined `-Wp,-MMD,<path>` (what
// scripts/Makefile.lib actually uses, not bare -MD/-MF), the
// autotools-style explicit `-MF <path>`, and bare `-MD`/`-MMD` with no
// `-MF` (gcc's documented default: the object's own path with .o
// replaced by .d). This is what lets writeSynthesizedDepfile put a
// substitute .d file exactly where Kbuild's `cmd_and_fixdep` macro
// will look for it.
func TestParseCompileArgsCapturesDepfile(t *testing.T) {
	for _, tc := range []struct {
		name        string
		args        []string
		wantDepfile string
	}{
		{
			name:        "Kbuild's real form: -Wp,-MMD,<path>",
			args:        []string{"-Wp,-MMD,kernel/.foo.o.d", "-c", "foo.c", "-o", "foo.o"},
			wantDepfile: "kernel/.foo.o.d",
		},
		{
			name:        "-Wp with extra trailing option after the path",
			args:        []string{"-Wp,-MMD,kernel/.foo.o.d,-MP", "-c", "foo.c", "-o", "foo.o"},
			wantDepfile: "kernel/.foo.o.d",
		},
		{
			name:        "explicit -MF",
			args:        []string{"-MD", "-MF", "foo.d", "-c", "foo.c", "-o", "foo.o"},
			wantDepfile: "foo.d",
		},
		{
			name:        "bare -MD with no -MF falls back to gcc's own default: obj with .d",
			args:        []string{"-MD", "-c", "foo.c", "-o", "foo.o"},
			wantDepfile: "foo.d",
		},
		{
			name:        "no dep flags at all: no depfile captured",
			args:        []string{"-c", "foo.c", "-o", "foo.o"},
			wantDepfile: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, depfile, _, ok := parseCompileArgs(tc.args)
			if !ok {
				t.Fatalf("parseCompileArgs bailed on %q", tc.args)
			}
			if depfile != tc.wantDepfile {
				t.Errorf("depfile = %q, want %q (args %q)", depfile, tc.wantDepfile, tc.args)
			}
		})
	}
}

// TestParseCompileArgsWpFlagsDoNotLeakIntoSandboxFlags pins that
// -Wp,-MMD,... never reaches the sandbox compile's own flag list: the
// path it carries is relative to make's cwd, not the staged tree.
func TestParseCompileArgsWpFlagsDoNotLeakIntoSandboxFlags(t *testing.T) {
	_, _, _, flags, ok := parseCompileArgs([]string{
		"-Wp,-MMD,kernel/.foo.o.d", "-O2", "-c", "foo.c", "-o", "foo.o",
	})
	if !ok {
		t.Fatal("parseCompileArgs bailed unexpectedly")
	}
	for _, f := range flags {
		if strings.HasPrefix(f, "-Wp,") {
			t.Errorf("-Wp,... flag leaked into sandbox flags: %q", flags)
		}
	}
	if !reflect.DeepEqual(flags, []string{"-O2"}) {
		t.Errorf("flags = %q, want just [-O2]", flags)
	}
}

// TestWriteSynthesizedDepfileSatisfiesRealFixdep runs the real Linux
// kernel `fixdep` binary (built from upstream scripts/basic/fixdep.c,
// vendored under testdata/fixdep for this test — see
// testdata/fixdep/README) against a depfile produced by
// writeSynthesizedDepfile, using a real on-disk header. fixdep doesn't
// care whether a .d file's dependency list came from genuine `-MD`
// compiler output or was synthesized from scan's own header list — it
// only requires that every listed path be real and readable.
func TestWriteSynthesizedDepfileSatisfiesRealFixdep(t *testing.T) {
	fixdepBin := buildFixdep(t)

	dir := t.TempDir()
	header := filepath.Join(dir, "header.h")
	if err := os.WriteFile(header, []byte("#ifdef CONFIG_FOO\nint x;\n#endif\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "foo.c")
	if err := os.WriteFile(source, []byte(`#include "header.h"`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	depfile := filepath.Join(dir, "foo.o.d")
	output := filepath.Join(dir, "foo.o")

	if err := writeSynthesizedDepfile(depfile, output, source, []scan.Header{
		{Abs: header, Rel: "header.h"},
	}); err != nil {
		t.Fatalf("writeSynthesizedDepfile: %v", err)
	}

	cmd := exec.Command(fixdepBin, depfile, "foo.o", "cc -c foo.c -o foo.o")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("real fixdep rejected the synthesized depfile: %v\n%s", err, stderr)
	}
	cmdOut := string(out)
	if !strings.Contains(cmdOut, "savedcmd_foo.o := cc -c foo.c -o foo.o") {
		t.Errorf("fixdep output missing expected savedcmd_ line:\n%s", cmdOut)
	}
	if !strings.Contains(cmdOut, "include/config/FOO") {
		t.Errorf("fixdep did not extract CONFIG_FOO from the synthesized header "+
			"dependency — it never read the header, meaning the synthesized "+
			"depfile's path wasn't recognized as real:\n%s", cmdOut)
	}
}

// buildFixdep compiles the vendored, real upstream fixdep.c with the
// host's cc, skipping the test if no C compiler is on PATH — CI's
// go-vet/go-test job runs with CGO_ENABLED=0 but still has a real `cc`
// on PATH for this.
func buildFixdep(t *testing.T) string {
	t.Helper()
	cc, err := exec.LookPath("cc")
	if err != nil {
		cc, err = exec.LookPath("gcc")
	}
	if err != nil {
		t.Skip("no C compiler on PATH to build the real fixdep binary")
	}
	bin := filepath.Join(t.TempDir(), "fixdep")
	cmd := exec.Command(cc, "-I", "testdata/fixdep", "-o", bin, "testdata/fixdep/fixdep.c")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building real fixdep: %v\n%s", err, out)
	}
	return bin
}
