package shim

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/tbereknyei/nixgg/internal/dispatch"
	"github.com/tbereknyei/nixgg/internal/expr"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/sandbox"
	"github.com/tbereknyei/nixgg/internal/storedeps"
	"github.com/tbereknyei/nixgg/internal/toolchain"
	"github.com/tbereknyei/nixgg/internal/wrapperenv"
)

// LD is the shim entrypoint for raw `ld`. Only `-r` (partial link:
// several objects in, one object out) is modelled; that shape doesn't
// fit KindLink (no executable/.so results) or KindArchive (result is
// one object, not an archive). Sandbox/eager-drv only — no native-mode
// helper exists, since a build that needs `ld -r` accelerated (e.g.
// Kbuild fusing a multi-object kernel module) already requires one of
// those modes for its other steps.
//
// Everything else — including a raw full link, which some builds also
// route through `ld` directly rather than a compiler driver — falls
// through to Link, keeping it inside nixgg's graph rather than passed
// through unmodelled.
func LD(args []string, cfg *toolchain.Config, l paths.Layout) error {
	real := realLDFor(cfg)
	if bypassed() {
		return Passthrough(real, args)
	}

	flags, output, inputs, ok := parseLDArgs(args)
	if !ok {
		return Link(dispatch.ToolLD, args, cfg, l)
	}
	if !sandbox.Enabled() && !sandbox.EagerDrv() {
		return Passthrough(real, args)
	}

	logf("ld -r %s <- %s", output, joinBase(inputs))

	altPrefix := altStorePrefix(cfg.Store)
	ci, err, ok := classifyInputs(cfg, inputs, altPrefix, l, "ld",
		func() error { return RealiseThunkArgsAndPassthrough(cfg, l, real, args, sandbox.Enabled()) })
	if !ok {
		return err
	}

	wrapperEnvJSON, err := wrapperenv.JSON()
	if err != nil {
		return err
	}
	wrapperEnv, err := decodeStringMap(wrapperEnvJSON)
	if err != nil {
		return err
	}

	outName := filepath.Base(output)
	name := "ld-" + outName
	if override := multiTargetName(output); override != "" {
		name = override
	}
	drv := expr.PartialLinkJSON(expr.PartialLinkJSONParams{
		Name:      name,
		OutName:   outName,
		System:    cfg.System,
		Bash:      cfg.BashRoot,
		Coreutils: cfg.CoreutilsRoot,
		ToolBin:   real,
		Flags:     flags,
		Inputs:    ci.JSON,
		StoreDeps: storedeps.From(flags, wrapperEnvJSON, cfg.KnownStorePaths),
		ExtraSrcs: []string{
			baseNameOf(cfg.BashRoot),
			baseNameOf(cfg.CoreutilsRoot),
			baseNameOf(ldRootOf(real)),
		},
		Env: wrapperEnv,
	})
	drvPath, err := sandbox.DerivationAdd(cfg, drv)
	if err != nil {
		return err
	}
	if err := sandbox.PointOutputAtDrv(output, drvPath); err != nil {
		return err
	}
	logf("  drv:        %s", drvPath)
	maybeSubmit(cfg, drvPath, output, true)
	return nil
}

// parseLDArgs recognises `ld … -r -o <out> <obj>…` and bails on
// everything else. `-r` must be present (otherwise this is a full
// link, Link's job) and so must `-o` — ld's default `a.out` name is
// never what a build system means here.
func parseLDArgs(args []string) (flags []string, output string, inputs []string, ok bool) {
	relocatable := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-r" || a == "--relocatable":
			relocatable = true
			flags = append(flags, a)
		case a == "-o":
			if i+1 >= len(args) {
				return nil, "", nil, false
			}
			output = args[i+1]
			i++
		case strings.HasPrefix(a, "-o") && len(a) > 2:
			output = a[2:]
		case ldTwoArg[a]:
			// Flags whose value is a separate token — without this the
			// value reads as a positional (e.g. "-m elf_x86_64" bails
			// on "elf_x86_64" being neither an object nor an archive).
			if i+1 >= len(args) {
				return nil, "", nil, false
			}
			flags = append(flags, a, args[i+1])
			i++
		case strings.HasSuffix(a, ".o") || strings.HasSuffix(a, ".a"):
			inputs = append(inputs, a)
		case strings.HasPrefix(a, "-"):
			flags = append(flags, a)
		default:
			// A positional that's neither object nor archive: a linker
			// script or something else unmodelled — bail rather than
			// silently drop it.
			return nil, "", nil, false
		}
	}
	if !relocatable || output == "" || len(inputs) == 0 {
		return nil, "", nil, false
	}
	return flags, output, inputs, true
}

// realLDFor resolves the `ld` this shim stands in for: NIXGG_REAL_LD
// if the caller needs the unwrapped binutils ld specifically,
// otherwise the binutils dir beside the pinned cc (same place
// archive.go finds `ar`).
func realLDFor(cfg *toolchain.Config) string {
	if v := os.Getenv("NIXGG_REAL_LD"); v != "" {
		return v
	}
	return filepath.Join(filepath.Dir(cfg.RealCC), "ld")
}

// ldRootOf turns /nix/store/…-binutils/bin/ld into the store root that
// must be mounted for it.
func ldRootOf(ld string) string {
	return filepath.Dir(filepath.Dir(ld))
}

// ldTwoArg lists the `ld` options whose value is a separate argv
// token. A fixed list rather than a heuristic ("does the next token
// look like a filename") — a wrong guess would swallow an object or
// leak a flag value into the input list, producing a silently wrong
// object instead of an error. An option missing here just makes
// parseLDArgs bail, which is the safe direction.
var ldTwoArg = map[string]bool{
	"-m": true, "-z": true, "-T": true, "-e": true, "-u": true,
	"-y": true, "-Y": true, "-b": true, "-A": true, "-R": true,
	"-F": true, "-h": true, "-I": true,
	"--architecture": true, "--defsym": true, "--dynamic-linker": true,
	"--entry": true, "--script": true, "--soname": true, "--wrap": true,
	"-rpath": true, "--rpath": true, "--undefined": true,
}
