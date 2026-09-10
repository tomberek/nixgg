// Package mode decides whether a given shim invocation defers via a
// placeholder thunk or realises synchronously.
//
// Placeholder is the default: every compile writes a .nix expression
// file and symlinks the output at it. The link shim's inline realise
// hook (NIXGG_AUTOFORCE=1) or an explicit `nixgg force` materialises
// the whole DAG in one Nix invocation at the end.
//
// Realise mode is a narrow carveout for cases where a downstream tool
// needs to run the just-produced artifact before make continues:
// autoconf conftests (`if ./conftest; then ... fi`) and cmake's
// try_compile probes are the canonical examples. In those cases the
// probe would see a .nix thunk file where it expected a runnable ELF.
// The decision is made per-TU by filename pattern — the outer build
// doesn't need to know or care.
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
	// Passthrough: run the real compiler in the build tree and model
	// nothing.
	//
	// A different reason from Realise. Realise is for builds that need a
	// runnable artifact immediately; Passthrough is for compiles the
	// build EXPECTS TO FAIL, where the failure is the result and the
	// artifact is the compiler's stderr.
	//
	// Accelerating those is not merely wasteful, it is wrong: a
	// derivation that fails, fails the build, whereas the caller was
	// going to inspect the error and carry on. Realise is no help
	// either — `nix build` on a deliberately-failing compile fails just
	// as hard.
	Passthrough
)

// For returns the mode for a given source or output path.
//
// Placeholder unless the path matches:
//   - a caller-declared passthrough subtree (passthrough.go)
//   - autoconf conftests (basename starts with "conftest")
//   - cmake compiler-detection files (test?Compiler…, CheckXXX…)
//   - cmake TryCompile scratch (path contains CMakeFiles/CMake{Scratch,Tmp})
//
// Every pattern here was added because a real project tripped it.
//
// NOT Linux Kbuild's scripts/mod/empty.o or arch/x86/realmode/rm/
// {header,trampoline_32,trampoline_64,stack,reboot}.o, despite both
// being the exact same "downstream tool reads a just-compiled TU's
// raw bytes synchronously" shape autoconf/cmake probes have: both are
// handled by a plain Passthrough in compile.go instead (see
// isKbuildElfProbe and isKbuildRealmodeObj there), because
// mode.Realise's own `nix build --file` mechanism turned out to be
// fundamentally incompatible with sandbox mode — confirmed directly,
// TWICE (once for each pattern), with "no substituter" from a nested,
// network-isolated build that could never succeed inside a real
// sandboxed build (builder-rpc-v0's own protocol has no "build this
// now" operation, only "register for later"). Neither probe has
// headers worth CA-hashing anyway, so Passthrough loses nothing by
// skipping nixgg's graph entirely for these — unlike a REAL autoconf/
// cmake probe, which genuinely needs the sandboxed build's exact
// flags to answer correctly and so still needs mode.Realise.
//
// Deliberately NOT consulted by the link or archive shims. It looks like
// an omission — a `try_run` probe execs a link output, so surely the link
// shim needs the same carveout? — but both reachable cases are already
// handled elsewhere:
//
//   - autoconf AC_RUN_IFELSE and cmake try_run run at CONFIGURE time,
//     and every example runs its configure phase under NIXGG_BYPASS=1.
//     bypassed() returns before any mode check, so those links never
//     reach this package at all.
//
//   - Build-time codegen tools (llvm-tblgen, protoc, a project's own
//     generator) are NOT bypassed, and they are the real case. But there
//     is no filename pattern that identifies them: llvm-tblgen looks
//     exactly like llvm-config. Any guess would either miss tools or
//     eagerly realise things that should stay lazy, forfeiting the
//     parallelism that is the point of deferring. Those builds use the
//     phase-chain pattern instead — see examples/two-phase for the
//     minimal shape and examples/llvm for a three-phase real one, where
//     phase N+1 consumes phase N's realised output via buildInputs.
//
// UPDATE: this is no longer true without exception. Linux Kbuild's own
// `arch/x86/realmode/rm/realmode.elf` IS immediately consumed by an
// unshimmed tool (`arch/x86/tools/relocs`) in the same recursive make,
// the same shape a `try_run` probe would need — see ForLink, which the
// link shim now DOES consult, narrowly, for exactly this one pattern.
// ForLink is a separate function rather than widening this one because
// a link output and a compile output are never the same path, so there
// is no ambiguity risk in keeping the two pattern sets apart — but it's
// also a deliberate signal that this is a distinct, newer exception to
// the "link/archive never consult mode" rule above, not a reversion of
// it. (ForLink shares mode.Realise's OWN sandbox-mode gap — confirmed,
// not just theorized, once isKbuildRealmodeObj's removal from this
// function got a real sandboxed build past scripts/mod/empty.o and
// arch/x86/realmode/rm/header.o far enough to reach ForLink's own
// realmode.elf/vdso32.so.dbg carveouts; see ForLink's own docstring
// for the still-open question of whether THOSE need the same
// Passthrough treatment or a genuine phase split.)
//
// So: if you are here because a mid-build exec got a thunk or a drvref
// stub, the fix is a phase split — UNLESS the pattern is narrow and
// project-confirmed enough to add here (or to ForLink) the way the
// Kbuild-specific cases were, AND the consuming tool genuinely needs
// nixgg's own sandboxed build (headers, flags, CA-hash) rather than
// just the ambient host toolchain — if it doesn't, a plain compile.go/
// link.go-level Passthrough (mode-agnostic, sandbox-safe) is the
// better fix, per isKbuildElfProbe's own precedent.
func For(path string) Mode {
	base := filepath.Base(path)
	switch {
	// Project-specific subtrees the caller declared — see passthrough.go.
	case matchesPassthrough(path):
		return Passthrough
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

// ForLink is For's link-shim counterpart — see the UPDATE paragraph on
// For's own docstring for why link outputs get a (narrow) carveout
// after all, where before none existed.
//
// realmode.elf and vdso32.so.dbg/vdsox32.so.dbg share mode.Realise's
// sandbox-mode incompatibility with the compile-side patterns For
// used to carve out here (see isKbuildElfProbe/isKbuildRealmodeObj/
// isKbuildVDSO32Obj in compile.go, and the UPDATE paragraph above) —
// confirmed reachable in a real sandboxed build once the compile-side
// gaps were fixed. NOT fixed for these two: unlike empty.o/the
// realmode/vdso32 .o probes, realmode.elf and vdso32.so.dbg
// genuinely need real nixgg-produced link content (their own inputs
// are other TUs this same build compiled), so a plain Passthrough
// would need those inputs pre-realized first — the same shape
// RealiseThunkArgsAndPassthrough already handles for archive/link
// Passthrough fallbacks, but that mechanism is confirmed
// architecturally impossible in sandbox mode (see its own docstring —
// the daemon's own op allowlist has no build operation). This is WHY
// the examples/linux-kernel fixture is native-mode only.
//
// isKbuildModpost is different in kind, not just degree: modpost is a
// build-time HOST TOOL (like fixdep/Kconfig's conf), not a target
// artifact consumed by an unshimmed reader — but unlike those, it has
// no separate NIXGG_BYPASS pre-pass window, so it reaches the ordinary
// shimmed link path. mode.Realise's `nix build --file` mechanism is
// unaffected by the sandbox-mode question for a NATIVE-mode-only
// fixture (there is no sandbox to be incompatible with), so this one
// IS fixed here, same shape as the other two, no phase split needed.
//
// isKbuildVmlinuxO is the SAME shape as isKbuildModpost, on a target
// artifact rather than a host tool: `objcopy` reads vmlinux.o right
// after it links (scripts/Makefile.vmlinux_o's own modules.builtin.
// modinfo rule) to extract its .modinfo section — same recursive
// make, same "unshimmed tool reads a just-produced artifact
// synchronously" shape as realmode.elf/vdso32.so.dbg, fixed the same
// way those two would be in a native-mode-only fixture.
func ForLink(path string) Mode {
	if isKbuildRealmodeELF(path) || isKbuildVDSODbg(path) || isKbuildModpost(path) ||
		isKbuildVmlinuxO(path) || isKbuildVmlinux(path) {
		return Realise
	}
	return Placeholder
}

// isKbuildRealmodeELF matches Linux Kbuild's
// arch/x86/realmode/rm/realmode.elf: `arch/x86/tools/relocs` reads it
// as raw ELF bytes immediately after linking, in the same recursive
// make (`scripts/Makefile.build`'s `cmd_relocs`), expecting a real
// binary — nixgg's link shim defers by default (writes a placeholder
// thunk), so without this carveout relocs sees a thunk symlink where
// it expects an ELF file ("No ELF magic"). Unconditional on x86_64
// (arch/x86/Kbuild's own `obj-y += realmode/`), so this is reachable
// on every build, not a config-dependent edge case.
func isKbuildRealmodeELF(path string) bool {
	return strings.HasSuffix(path, "arch/x86/realmode/rm/realmode.elf")
}

// isKbuildVDSODbg matches Linux Kbuild's arch/x86/entry/vdso/
// {vdso32,vdsox32}.so.dbg: the SAME link recipe that produces them
// (`cmd_vdso_and_check` in arch/x86/entry/vdso/Makefile) immediately
// runs `checkundef.sh '$(NM)' '$@'` against the just-linked file, and
// a later `objcopy`/`readelf` step (producing the final .so from the
// .dbg) reads it again — both need real ELF bytes, not a placeholder
// thunk. Reachable whenever CONFIG_X86_32 (vdso32) or CONFIG_X86_X32
// (vdsox32) support is enabled, which most real x86_64 defconfigs
// carry for 32-bit compat syscalls.
func isKbuildVDSODbg(path string) bool {
	return strings.HasSuffix(path, "arch/x86/entry/vdso/vdso32.so.dbg") ||
		strings.HasSuffix(path, "arch/x86/entry/vdso/vdsox32.so.dbg")
}

// isKbuildModpost matches Linux Kbuild's scripts/mod/modpost: a
// `hostprogs-always-y` multi-object host tool (scripts/mod/Makefile's
// own `modpost-objs`), built via the ordinary HOSTCC link recipe
// (scripts/Makefile.host's host-cmulti) and then EXEC'D DIRECTLY by
// later Makefile rules in the same recursive make (scripts/
// Makefile.modpost's own `cmd_modpost`) — not just read as bytes, the
// way realmode.elf/vdso32.so.dbg are. Same root cause as those two:
// nixgg's link shim defers by default (writes a placeholder thunk), so
// without this carveout the shell tries to exec a `.nix` text file
// and fails with a permission/exec-format error, not "No ELF magic"
// (the OS's own exec() rejects a non-executable file outright, before
// modpost's own argv parsing ever runs).
//
// Unlike fixdep/Kconfig's `conf` (built during `make prepare`'s own
// NIXGG_BYPASS=1 pre-pass and never touched again), modpost is built
// on-demand the FIRST time scripts/mod/ is descended into as part of
// the main (shimmed) build — there is no separate bypass window that
// would let it build for real before the shimmed path ever sees it.
func isKbuildModpost(path string) bool {
	return strings.HasSuffix(path, "scripts/mod/modpost")
}

// isKbuildVmlinuxO matches Linux Kbuild's own vmlinux.o: immediately
// after linking (scripts/Makefile.vmlinux_o's own `LD vmlinux.o`
// rule), `objcopy -j .modinfo -O binary` reads it back synchronously
// (the modules.builtin.modinfo rule, same recursive make) to extract
// the .modinfo section — same "unshimmed tool reads a just-linked
// artifact" shape as realmode.elf/vdso32.so.dbg. Matched on the bare
// basename (not a directory-scoped suffix like the other Kbuild
// carveouts): vmlinux.o is Kbuild's own canonical build-root artifact
// name, produced by exactly one rule per build, unlike e.g. "empty.o"
// which is generic enough to collide with an unrelated project's own
// TU.
func isKbuildVmlinuxO(path string) bool {
	return filepath.Base(path) == "vmlinux.o"
}

// isKbuildVmlinux matches Linux Kbuild's own final `vmlinux` binary.
// scripts/link-vmlinux.sh — the script cmd_link_vmlinux invokes right
// after the real `ld` link — immediately runs `nm` on the just-
// linked file (to compute System.map) and scripts/sorttable on it
// (info's own "SORTTAB vmlinux" line), both in the SAME shell script
// the link itself runs inside, same recursive make. Without this
// carveout the link shim's own placeholder thunk (a `.nix` text file)
// reaches nm/sorttable instead of a real ELF — "file format not
// recognized" / "unrecognized ELF data encoding" — the outermost
// instance of the same "unshimmed tool reads a just-linked artifact
// synchronously" shape vmlinux.o/modpost/realmode.elf already needed
// this carveout for.
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
