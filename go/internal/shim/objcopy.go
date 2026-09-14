package shim

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/tbereknyei/nixgg/internal/classify"
	"github.com/tbereknyei/nixgg/internal/expr"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/sandbox"
	"github.com/tbereknyei/nixgg/internal/storedeps"
	"github.com/tbereknyei/nixgg/internal/toolchain"
)

// Objcopy is the shim entrypoint for `objcopy <flags> <in> <out>`: a
// build step that reads one object and writes another (prefixing
// symbols, stripping sections). Under nixgg the input is a drvref
// stub, so the real tool would report "file format not recognized".
//
// Same KindTransform as Objtool, different operand shape: this reads
// one file and writes another, so nothing has to be copied out of the
// store first. The binary is normally already a store path, so it
// needs no store-add step either.
func Objcopy(args []string, cfg *toolchain.Config, l paths.Layout) error {
	real := realBinutil(cfg, "NIXGG_REAL_OBJCOPY", "objcopy")
	if bypassed() || (!sandbox.Enabled() && !sandbox.EagerDrv()) {
		return Passthrough(real, args)
	}

	flags, input, output, ok := parseObjcopyArgs(args)
	if !ok {
		// A one-operand (in-place) or read-only form: neither is
		// modelled. Say so — silence here looks identical to
		// acceleration.
		logf("objcopy passthrough: not an <in> <out> rewrite (%s)", joinBase(args))
		return Passthrough(real, args)
	}

	t := classify.Target(input, altStorePrefix(cfg.Store), l)
	var in expr.JSONDrvInput
	switch t.Kind {
	case classify.Drv:
		in = expr.JSONDrvInput{Kind: "drv", Ref: t.Ref, Name: filepath.Base(input)}
	case classify.Store:
		in = expr.JSONDrvInput{Kind: "src", Ref: expr.StoreBasename(t.Ref), Name: filepath.Base(input)}
	default:
		logf("objcopy passthrough: can't model input %s (%s)", input, t.Reason())
		return Passthrough(real, args)
	}

	if carvedOut(output) {
		logf("objcopy passthrough: %s is in a carved-out subtree", output)
		return Passthrough(real, args)
	}

	logf("objcopy %s <- %s", output, filepath.Base(input))

	outName := filepath.Base(output)
	drv := expr.TransformJSON(expr.TransformJSONParams{
		Name:      "oc-" + outName,
		OutName:   outName,
		System:    cfg.System,
		Bash:      cfg.BashRoot,
		Coreutils: cfg.CoreutilsRoot,
		ToolBin:   real,
		InPlace:   false,
		Flags:     flags,
		Input:     in,
		StoreDeps: storedeps.From(nil, "", cfg.KnownStorePaths),
		ExtraSrcs: []string{
			baseNameOf(cfg.BashRoot),
			baseNameOf(cfg.CoreutilsRoot),
			baseNameOf(toolRootOf(real)),
		},
	})
	drvPath, err := sandbox.DerivationAdd(cfg, drv)
	if err != nil {
		return err
	}
	if err := sandbox.PointOutputAtDrv(output, drvPath); err != nil {
		return err
	}
	logf("  drv:        %s", drvPath)
	return nil
}

// parseObjcopyArgs recognises `objcopy <flags> <infile> <outfile>`.
// objcopy also accepts a single-operand in-place form and read-only
// uses like --dump-section; neither is modelled — bail so the real
// tool runs. Only the two-operand rewrite (scripts/Makefile.lib's
// cmd_objcopy shape) becomes a derivation.
func parseObjcopyArgs(args []string) (flags []string, input, output string, ok bool) {
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case objcopyTwoArg[a]:
			if i+1 >= len(args) {
				return nil, "", "", false
			}
			flags = append(flags, a, args[i+1])
			i++
		case strings.HasPrefix(a, "-"):
			flags = append(flags, a)
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) != 2 {
		return nil, "", "", false
	}
	return flags, positional[0], positional[1], true
}

// objcopyTwoArg lists objcopy options taking a separate value token.
// Same reasoning as ldTwoArg: a wrong guess turns a flag value into an
// operand and silently rewrites the wrong file, so a fixed list bails
// into passthrough on anything missing instead.
var objcopyTwoArg = map[string]bool{
	"-B": true, "-F": true, "-I": true, "-O": true, "-R": true,
	"-b": true, "-i": true, "-j": true, "-K": true, "-N": true,
	"-L": true, "-G": true, "-W": true, "--add-section": true,
	"--rename-section": true, "--set-section-flags": true,
	"--remove-section": true, "--only-section": true,
	"--prefix-symbols": true, "--prefix-alloc-sections": true,
	"--redefine-sym": true,
	"--strip-symbol": true,
	"--keep-symbol":  true,
}

// realBinutil resolves a binutils tool: the named env override wins,
// otherwise the sibling of the pinned cc — the same bin/ dir
// archive.go takes `ar` from.
func realBinutil(cfg *toolchain.Config, envVar, name string) string {
	if v := os.Getenv(envVar); v != "" {
		return v
	}
	return filepath.Join(filepath.Dir(cfg.RealCC), name)
}

// toolRootOf turns /nix/store/…-pkg/bin/tool into the store root that
// must be mounted for it.
func toolRootOf(tool string) string {
	return filepath.Dir(filepath.Dir(tool))
}
