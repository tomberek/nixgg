// Package mode decides whether a given shim invocation defers via a
// placeholder thunk (the default — realised later by `nixgg force` or
// NIXGG_AUTOFORCE=1) or realises synchronously.
//
// Realise is a narrow carveout for cases where a downstream tool needs
// to run the just-produced artifact before make continues (autoconf
// conftests, cmake try_compile probes, and a handful of Linux Kbuild
// artifacts read synchronously by an unshimmed tool in the same
// recursive make). Decided per-path by filename pattern.
package mode

import (
	"path/filepath"
	"strings"
)

// Mode is the result of the placeholder-vs-realise decision.
type Mode int

const (
	Placeholder Mode = iota
	Realise
)

// For returns the mode for a compile source/output path.
//
// Placeholder unless the path matches a known conftest/probe pattern:
//   - autoconf conftests (basename starts with "conftest")
//   - cmake compiler-detection files (test?Compiler…, CheckXXX…)
//   - cmake TryCompile scratch (path contains CMakeFiles/CMake{Scratch,Tmp})
//
// Deliberately NOT matched here: Linux Kbuild's scripts/mod/empty.o and
// arch/x86/realmode/rm/*.o, which have the same "reader needs real ELF
// bytes now" shape but are handled by a plain Passthrough in compile.go
// (isKbuildElfProbe/isKbuildRealmodeObj) instead of mode.Realise —
// mode.Realise's `nix build --file` doesn't work in sandbox mode (the
// builder-rpc-v0 protocol has no "build now" op, only "register for
// later"), and these probes have no headers worth CA-hashing, so
// Passthrough loses nothing by skipping nixgg's graph for them.
//
// Not consulted by the link or archive shims (see ForLink for the one
// exception): configure-time try_run/AC_RUN_IFELSE links happen under
// NIXGG_BYPASS=1 and never reach this package, and build-time codegen
// tools (llvm-tblgen, protoc, ...) have no filename pattern that
// reliably distinguishes them from ordinary tools — those need the
// phase-chain pattern instead (see examples/two-phase, examples/llvm).
func For(path string) Mode {
	base := filepath.Base(path)
	switch {
	case strings.HasPrefix(base, "conftest"):
		return Realise
	case matchCMakeProbe(base):
		return Realise
	case strings.Contains(path, "/CMakeFiles/CMakeScratch/") ||
		strings.Contains(path, "/CMakeFiles/CMakeTmp/"):
		return Realise
	}
	return Placeholder
}

// ForLink is For's link-shim counterpart: a link output is realised
// synchronously when an unshimmed tool reads it back in the same
// recursive make (Linux Kbuild's relocs/checkundef.sh/nm/sorttable
// steps, all below). This carveout only applies to link outputs — a
// link path and a compile path are never the same, so there's no
// ambiguity with For's own patterns.
//
// realmode.elf/vdso32.so.dbg genuinely need real nixgg-produced link
// content (their inputs are other TUs this same build compiled), so —
// unlike the compile-side empty.o/realmode-.o probes, which get a
// plain Passthrough instead — they can't skip nixgg's graph; this
// fixture (examples/linux-kernel) is native-mode only because
// mode.Realise's `nix build --file` doesn't work in sandbox mode.
func ForLink(path string) Mode {
	if isKbuildRealmodeELF(path) || isKbuildVDSODbg(path) || isKbuildModpost(path) ||
		isKbuildVmlinuxO(path) || isKbuildVmlinux(path) {
		return Realise
	}
	return Placeholder
}

// isKbuildRealmodeELF matches arch/x86/realmode/rm/realmode.elf:
// arch/x86/tools/relocs reads it as raw ELF bytes immediately after
// linking, in the same recursive make — a placeholder thunk there
// would fail relocs with "No ELF magic". Unconditional on x86_64.
func isKbuildRealmodeELF(path string) bool {
	return strings.HasSuffix(path, "arch/x86/realmode/rm/realmode.elf")
}

// isKbuildVDSODbg matches arch/x86/entry/vdso/{vdso32,vdsox32}.so.dbg:
// the same link recipe that produces them immediately runs
// checkundef.sh against the just-linked file, and a later objcopy/
// readelf step reads it again — both need real ELF bytes.
func isKbuildVDSODbg(path string) bool {
	return strings.HasSuffix(path, "arch/x86/entry/vdso/vdso32.so.dbg") ||
		strings.HasSuffix(path, "arch/x86/entry/vdso/vdsox32.so.dbg")
}

// isKbuildModpost matches scripts/mod/modpost: built via the ordinary
// link recipe, then EXEC'D DIRECTLY (not just read as bytes) by later
// rules in the same recursive make. A placeholder thunk there is a
// `.nix` text file, so exec fails outright rather than with "No ELF
// magic". Unlike fixdep/Kconfig's `conf` (built under the `make
// prepare` NIXGG_BYPASS=1 pre-pass), modpost is built on-demand inside
// the main shimmed build — there's no bypass window to build it first.
func isKbuildModpost(path string) bool {
	return strings.HasSuffix(path, "scripts/mod/modpost")
}

// isKbuildVmlinuxO matches Kbuild's vmlinux.o: `objcopy -j .modinfo`
// reads it back synchronously right after linking (the
// modules.builtin.modinfo rule) to extract its .modinfo section.
// Matched on bare basename since vmlinux.o is Kbuild's own canonical
// build-root artifact name, produced by exactly one rule per build.
func isKbuildVmlinuxO(path string) bool {
	return filepath.Base(path) == "vmlinux.o"
}

// isKbuildVmlinux matches Kbuild's final `vmlinux` binary:
// scripts/link-vmlinux.sh runs nm (for System.map) and sorttable on it
// immediately after the real `ld` link, in the same script.
func isKbuildVmlinux(path string) bool {
	return filepath.Base(path) == "vmlinux"
}

func matchCMakeProbe(base string) bool {
	// e.g. testCCompiler.c, testCXXCompilerABI_C.cpp,
	//      CMakeCCompilerId.c, CMakeCXXCompilerABI_CXX.cpp
	if strings.HasPrefix(base, "test") && strings.Contains(base, "Compiler") {
		return true
	}
	if strings.HasPrefix(base, "CMake") && strings.Contains(base, "Compiler") {
		return true
	}
	// CheckFunctionExists, CheckIncludeFile, CheckCSourceCompiles, ...
	if strings.HasPrefix(base, "Check") &&
		(strings.Contains(base, "Exists") ||
			strings.Contains(base, "Include") ||
			strings.Contains(base, "SourceCompiles") ||
			strings.Contains(base, "SourceRuns") ||
			strings.Contains(base, "SymbolExists") ||
			strings.Contains(base, "TypeSize")) {
		return true
	}
	return false
}
