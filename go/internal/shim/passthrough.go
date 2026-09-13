package shim

import (
	"os"
	"syscall"

	"github.com/tbereknyei/nixgg/internal/classify"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/realise"
	"github.com/tbereknyei/nixgg/internal/toolchain"
)

// Passthrough replaces the current process image with the real tool.
// This is the right thing for shims that decide not to model a call:
// we don't pay for a fork, stdin/stdout/stderr are already correct,
// and the caller sees the tool's true exit code.
func Passthrough(realTool string, args []string) error {
	argv := append([]string{realTool}, args...)
	return syscall.Exec(realTool, argv, os.Environ())
}

// RealiseThunkArgsAndPassthrough realizes every argv token that
// classifies as one of our own not-yet-realized native-mode outputs
// (a symlink to a .nix thunk) before exec-ing the real tool, then
// calls Passthrough.
//
// When Archive/Link give up modeling an invocation and fall to
// Passthrough, the real tool may still read OTHER argv tokens that
// are nixgg's own deferred thunk symlinks, not real bytes. Most tools
// fail loudly on that, but `ar` with a thin (`T`) modifier stores
// each member's file PATH rather than reading its content, so `ar
// cDPrST archive.a <mix of real .o and still-thunk .o paths>`
// SUCCEEDS silently, producing an archive whose member list includes
// literal `.nixgg/thunks/<id>.nix` paths — the failure only surfaces
// much later, far from the cause, when a linker/`nm`/`ar mPiT` tries
// to read one of those paths as real object bytes. Confirmed against
// a real Linux kernel build: Kbuild's recursive `built-in.a`
// construction routinely has one unmodelable sibling that sends the
// whole `ar` call to Passthrough alongside many real nixgg thunks for
// the other members, each of which needs realizing here first.
//
// Sandbox mode isn't handled here: realizing a single registered drv
// on demand has no existing mechanism to reuse — builder-rpc-v0's
// daemon (src/libstore/daemon.cc's performOp) allowlists only
// AddToStore{,Multiple,Nar,Scanning}, SubmitOutput, AddTempRoot, and
// IsValidPath for a RecursiveSubmitted connection; BuildDerivation/
// BuildPaths are deliberately not on that list. In a sandboxed kernel
// build this means vmlinux.a's final `ar`/`ld` call can hit the same
// unmodelable-sibling case with no synchronous way to materialize the
// other, still-un-realized sandbox-mode drv stubs first, failing as
// `ld.bfd: member ... is not an object`. A no-op in sandbox mode, so
// callers can use this unconditionally.
func RealiseThunkArgsAndPassthrough(cfg *toolchain.Config, l paths.Layout, realTool string, args []string, sandboxEnabled bool) error {
	if !sandboxEnabled {
		altPrefix := altStorePrefix(cfg.Store)
		for _, a := range args {
			if c := classify.Target(a, altPrefix, l); c.Kind == classify.Thunk {
				if err := realise.Realise(l, cfg, c.Ref, a); err != nil {
					logf("  realise-before-passthrough: %s: %v", a, err)
				}
			}
		}
	}
	return Passthrough(realTool, args)
}

// bypassed reports whether the shim should skip nixgg's derivation
// path and just exec the real tool. Controlled by NIXGG_BYPASS.
//
// Intended for phases that can't route through nixgg without
// breaking — most commonly autoconf `./configure` or cmake's
// probe phase, which compile+exec tiny binaries synchronously.
// Users flip it around those phases:
//
//	NIXGG_BYPASS=1 ./configure
//	make      # NIXGG_BYPASS unset → shims fire as normal
//
// Any non-empty, non-"0" value is truthy.
func bypassed() bool {
	v := os.Getenv("NIXGG_BYPASS")
	return v != "" && v != "0"
}
