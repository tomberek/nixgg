package shim

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The kernel's own two rustc command shapes, since they are the only
// ones this has been exercised against and their differences are the
// interesting ones: `cmd_rustc_o_rs` emits an object alone, while
// `cmd_rustc_library` also emits the metadata every dependent crate
// resolves `--extern` against.
func TestParseRustcArgsKernelShapes(t *testing.T) {
	t.Run("cmd_rustc_o_rs", func(t *testing.T) {
		rc, _, ok := parseRustcArgs([]string{
			"--edition=2021", "-Zallow-features=asm_const", "-Zcrate-attr=no_std",
			"--extern", "pin_init", "--extern", "kernel",
			"--crate-type", "rlib", "-L", "rust/",
			"--crate-name", "drm_panic_qr", "--sysroot=/dev/null",
			"--out-dir", "drivers/gpu/drm", "--emit=dep-info=.qr.d",
			"--emit=obj=drivers/gpu/drm/drm_panic_qr.o",
			"drivers/gpu/drm/drm_panic_qr.rs",
		})
		if !ok {
			t.Fatal("did not parse")
		}
		if rc.Source != "drivers/gpu/drm/drm_panic_qr.rs" {
			t.Errorf("Source = %q", rc.Source)
		}
		want := []rustcEmit{{Kind: "obj", Path: "drivers/gpu/drm/drm_panic_qr.o"}}
		if !reflect.DeepEqual(rc.Emits, want) {
			t.Errorf("Emits = %+v, want %+v (dep-info is the build's own bookkeeping)", rc.Emits, want)
		}
		if len(rc.Externs) != 2 || rc.Externs[0].Crate != "pin_init" || rc.Externs[0].Path != "" {
			t.Errorf("Externs = %+v; bare form must survive parsing for -L resolution", rc.Externs)
		}
		if !reflect.DeepEqual(rc.LibDirs, []string{"rust/"}) {
			t.Errorf("LibDirs = %v", rc.LibDirs)
		}
		// -L and --out-dir describe the build tree, which the sandbox does
		// not have. They must not reach the derivation.
		for _, f := range rc.Flags {
			if f == "-Lrust/" || f == "rust/" || f == "--out-dir" {
				t.Errorf("build-tree flag %q leaked into the derivation flags: %v", f, rc.Flags)
			}
		}
	})

	t.Run("cmd_rustc_library emits metadata too", func(t *testing.T) {
		rc, _, ok := parseRustcArgs([]string{
			"--emit=dep-info=.kernel.d", "--emit=obj=rust/kernel.o",
			"--emit=metadata=rust/libkernel.rmeta",
			"--crate-type", "rlib", "-Lrust",
			"--crate-name", "kernel", "rust/kernel/lib.rs",
			"--sysroot=/dev/null", "-Zunstable-options",
		})
		if !ok {
			t.Fatal("did not parse")
		}
		want := []rustcEmit{
			{Kind: "obj", Path: "rust/kernel.o"},
			{Kind: "metadata", Path: "rust/libkernel.rmeta"},
		}
		if !reflect.DeepEqual(rc.Emits, want) {
			t.Errorf("Emits = %+v, want %+v", rc.Emits, want)
		}
	})
}

// Everything that must NOT become a derivation. Each of these runs
// during a normal kernel build, several times, before any crate is
// compiled — modelling one would either hang the build waiting for an
// answer it prints to stdout, or write a stub where a real file has to
// be.
func TestParseRustcArgsRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"--print asks a question", []string{
			"--print", "file-names", "--crate-name", "macros",
			"--crate-type", "proc-macro", "-"}},
		{"--version asks a question", []string{"--version", "--verbose"}},
		{"unpretty writes to stdout", []string{
			"-Zunpretty=expanded", "--emit=obj=x.o", "x.rs"}},
		{"stdin source", []string{"--emit=obj=x.o", "-"}},
		{"unnamed emit", []string{"--emit=obj", "--out-dir", "rust", "x.rs"}},
		{"no emit at all", []string{"--crate-type", "rlib", "x.rs"}},
		{"two sources", []string{"--emit=obj=x.o", "a.rs", "b.rs"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := parseRustcArgs(tc.args); ok {
				t.Errorf("parsed %v; expected a bail into passthrough", tc.args)
			}
		})
	}
}

// A proc-macro crate has to stay a real file: rustc dlopen's it to
// expand a dependent crate's macros, including during that crate's
// dependency scan. Deferring it makes every downstream scan silently
// incomplete rather than failing.
func TestParseRustcArgsFlagsProcMacro(t *testing.T) {
	rc, _, ok := parseRustcArgs([]string{
		"--crate-type", "proc-macro", "--crate-name", "macros",
		"--emit=link=rust/libmacros.so", "rust/macros/lib.rs",
	})
	if !ok {
		t.Fatal("did not parse")
	}
	if !rc.ProcMacro {
		t.Error("proc-macro crate not flagged; it would be deferred and every dependent scan would go blind")
	}
}

// Attached and separated spellings of the same option must produce the
// same derivation flags, or the same compile gets two drv hashes
// depending on how the caller happened to write it.
func TestParseRustcArgsNormalisesShortOptions(t *testing.T) {
	sep, _, ok1 := parseRustcArgs([]string{"-C", "opt-level=2", "--emit=obj=x.o", "x.rs"})
	att, _, ok2 := parseRustcArgs([]string{"-Copt-level=2", "--emit=obj=x.o", "x.rs"})
	if !ok1 || !ok2 {
		t.Fatal("did not parse")
	}
	if !reflect.DeepEqual(sep.Flags, att.Flags) {
		t.Errorf("separated %v != attached %v", sep.Flags, att.Flags)
	}
}

// This shim is the only one normally reached through PATH rather than an
// explicit tool= assignment: kbuild's default is a bare `rustc` and
// nixpkgs does not override it. So the PATH fallback must not find the
// shim again — that forks the build until it dies, with no error naming
// the cause.
func TestRealRustcSkipsItself(t *testing.T) {
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Skip("no executable path available")
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		t.Skip("cannot resolve own path")
	}
	// A shims/ dir on PATH, exactly as mkNixggBuild lays it out.
	shim := filepath.Join(dir, "rustc")
	if err := os.Symlink(self, shim); err != nil {
		t.Skip("cannot symlink")
	}
	real := filepath.Join(t.TempDir(), "rustc")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NIXGG_REAL_RUSTC", "")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+filepath.Dir(real))

	got, err := realRustc()
	if err != nil {
		t.Fatalf("realRustc() error: %v", err)
	}
	if got != real {
		t.Errorf("realRustc() = %q, want %q — resolving to the shim itself forks forever", got, real)
	}
}

// The dep file is the build's, not the derivation's. kbuild's
// `if_changed_dep` runs fixdep over it and hard-fails when it is
// missing, and the derivation cannot write it — the emit flags were
// replaced with the derivation's own.
func TestParseRustcArgsRemembersDepFile(t *testing.T) {
	rc, _, ok := parseRustcArgs([]string{
		"--emit=dep-info=rust/.kernel.o.d", "--emit=obj=rust/kernel.o",
		"rust/kernel/lib.rs",
	})
	if !ok {
		t.Fatal("did not parse")
	}
	if rc.DepFile != "rust/.kernel.o.d" {
		t.Errorf("DepFile = %q; fixdep would fail on the missing file", rc.DepFile)
	}
	if len(rc.Emits) != 1 {
		t.Errorf("dep-info became an artifact: %+v", rc.Emits)
	}
}

// The real command line kbuild records for a Rust driver, verbatim from
// drivers/gpu/drm/.drm_panic_qr.o.cmd. It is here because reading the
// argv grammar was not enough: `--remap-path-prefix FROM=TO` is a
// SEPARATED two-argument flag, its value was taken for a second
// positional, and the parser bailed. That bail is safe — the build runs
// unaccelerated rather than wrong — but it is silent, so it cost a full
// kernel run to notice.
func TestParseRustcArgsRealKernelDriver(t *testing.T) {
	args := []string{
		"--edition=2021", "-Zbinary_dep_depinfo=y", "-Astable_features",
		"-Dnon_ascii_idents", "-Wmissing_docs", "-Wclippy::all",
		"-Aclippy::collapsible_if", "-Wrustdoc::missing_crate_level_docs",
		"-Cpanic=abort", "-Cembed-bitcode=n", "-Ccodegen-units=1",
		"-Csymbol-mangling-version=v0", "-Zfunction-sections=n",
		"-Wclippy::float_arithmetic", "--target=./scripts/target.json",
		"-Ctarget-feature=-sse,-sse2", "-Zcf-protection=branch",
		"-Ctarget-cpu=x86-64", "-Ztune-cpu=generic", "-Ccode-model=kernel",
		"-Zpatchable-function-entry=16,16", "-Copt-level=2", "-Cdebuginfo=2",
		"--remap-path-prefix", "/nix/store/aaa-rust-lib-src=/",
		"-Zallow-features=asm_const,asm_goto", "-Zcrate-attr=no_std",
		"-Zcrate-attr=feature(asm_const,asm_goto)", "-Zunstable-options",
		"--extern", "pin_init", "--extern", "kernel",
		"--crate-type", "rlib", "-L", "./rust/",
		"--crate-name", "drm_panic_qr", "--sysroot=/dev/null",
		"--out-dir", "drivers/gpu/drm/",
		"--emit=dep-info=drivers/gpu/drm/.drm_panic_qr.o.d",
		"--emit=obj=drivers/gpu/drm/drm_panic_qr.o",
		"../drivers/gpu/drm/drm_panic_qr.rs",
	}
	rc, _, ok := parseRustcArgs(args)
	if !ok {
		t.Fatal("did not parse the kernel's own rustc command line")
	}
	if rc.Source != "../drivers/gpu/drm/drm_panic_qr.rs" {
		t.Errorf("Source = %q", rc.Source)
	}
	if len(rc.Emits) != 1 || rc.Emits[0].Path != "drivers/gpu/drm/drm_panic_qr.o" {
		t.Errorf("Emits = %+v", rc.Emits)
	}
	if len(rc.Externs) != 2 {
		t.Errorf("Externs = %+v", rc.Externs)
	}
	// A two-argument flag whose value was mistaken for the source is the
	// exact shape of the bug this pins, so check the value did not leak
	// back out as a flag of its own.
	for _, f := range rc.Flags {
		if f == "/nix/store/aaa-rust-lib-src=/" {
			t.Errorf("--remap-path-prefix's value became a bare flag: %v", rc.Flags)
		}
	}
}

// The scan needs the caller's ORIGINAL command line, not the reduced
// flag list the derivation gets. Stripped of `-L` and `--extern`, a
// kernel crate compiled under `--sysroot=/dev/null` cannot resolve
// `core`, and rustc then fails at the driver level — before it writes
// any dep-info at all, so the scan has nothing to read and the crate
// silently falls back to passthrough.
func TestParseRustcArgsKeepsScanFlags(t *testing.T) {
	rc, _, ok := parseRustcArgs([]string{
		"--edition=2021", "--sysroot=/dev/null",
		"-L", "./rust/", "--extern", "kernel",
		"--emit=obj=x.o", "--out-dir", "d", "x.rs",
	})
	if !ok {
		t.Fatal("did not parse")
	}
	want := []string{
		"--edition=2021", "--sysroot=/dev/null",
		"-L", "./rust/", "--extern", "kernel",
		"--emit=obj=x.o", "--out-dir", "d",
	}
	if !reflect.DeepEqual(rc.ScanFlags, want) {
		t.Errorf("ScanFlags = %q\nwant %q", rc.ScanFlags, want)
	}
	// The derivation's flags stay reduced — -L names the build tree and
	// externs are resolved to explicit paths.
	for _, f := range rc.Flags {
		if f == "-L" || f == "./rust/" || f == "--extern" {
			t.Errorf("build-tree flag %q leaked into the derivation flags: %v", f, rc.Flags)
		}
	}
}

// A kernel's generated cfg file is 19,746 lines and 601 KB — one
// `--cfg=CONFIG_…` per config symbol. rustc takes it as an @-file
// precisely so it never has to be a command line; expanding it inline
// to model the compile puts all 601 KB into the single `bash -c`
// argument the builder runs, and execve caps that at MAX_ARG_STRLEN
// (32 pages, 131072 bytes, a compile-time kernel constant with no
// runtime knob). The builder then dies with "Argument list too long"
// and nothing that names the flags as the cause.
func TestFlagsFitInline(t *testing.T) {
	if _, fits := flagsFitInline([]string{"--edition=2021", "-Cpanic=abort"}); !fits {
		t.Error("an ordinary flag list must stay inline; spilling it would move every drv hash")
	}

	var kernelCfg []string
	for i := 0; i < 19746; i++ {
		kernelCfg = append(kernelCfg, `--cfg=CONFIG_SOME_LONGISH_SYMBOL_NAME="m"`)
	}
	n, fits := flagsFitInline(kernelCfg)
	if n < 600_000 {
		t.Fatalf("fixture is %d bytes; the real kernel cfg file is ~601 KB", n)
	}
	if fits {
		t.Errorf("a %d-byte flag list was judged inlineable, %d over MAX_ARG_STRLEN", n, n-131072)
	}
}

// A builtin target triple is not a file and must be left alone.
// Rewriting it would turn a valid target into a missing path, and
// store-adding it is meaningless.
func TestStoreFlagFilesLeavesTripleAlone(t *testing.T) {
	in := []string{"--target=x86_64-unknown-none", "--edition=2021"}
	flags, deps, err := storeFlagFiles(nil, in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(flags, in) || len(deps) != 0 {
		t.Errorf("flags = %q deps = %v; want unchanged", flags, deps)
	}
}

// A spec already in the store keeps its path: it is content-addressed
// and mounted already, and re-adding it would change the filename rustc
// derives the target NAME from.
func TestStoreFlagFilesSkipsStorePaths(t *testing.T) {
	in := []string{"--target=/nix/store/aaa-spec/target.json"}
	flags, deps, err := storeFlagFiles(nil, in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(flags, in) || len(deps) != 0 {
		t.Errorf("flags = %q deps = %v; want unchanged", flags, deps)
	}
}
