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

// LD is the shim entrypoint for `ld`.
//
// Only `-r` (partial link: several objects in, one object out) is
// modelled. A build typically reaches it two ways:
//
//	cmd_ld_multi_m = $(LD) $(ld_flags) -r -o $@ @$<
//	cmd_ld_single  = ; $(LD) $(ld_flags) -r -o $(tmp-target) $@; mv …
//
// Both were failing the same way once the compile shim was live —
// `ld.bfd: algos.o: file format not recognized; treating as linker
// script`, because ld met a drvref stub.
//
// The single-object form needs no special handling: it writes to a temp
// name and make then `mv`s it over the original. A drvref stub is an
// ordinary regular file, so the mv carries it across intact and the
// object ends up pointing at this derivation.
//
// Everything that is not a `-r` link passes through. Full links
// (executables, .so) go through the compiler driver in every build nixgg
// supports, so modelling them here would duplicate link.go against a
// second, lower-level command line for no gain.
func LD(args []string, cfg *toolchain.Config, l paths.Layout) error {
	real := realLDFor(cfg)
	if bypassed() || !sandbox.Enabled() {
		return Passthrough(real, args)
	}

	flags, output, inputs, ok := parseLDArgs(args)
	if !ok {
		// Not a `-r` partial link, so it is an ordinary full link —
		// hand it to Link rather than passing through, which keeps a
		// raw `ld` final link (Kbuild's cmd_ld) inside nixgg's graph.
		//
		// Except in a carved-out subtree. Kbuild runs `ld` with cwd
		// INSIDE the object directory, so `-o realmode.elf` arrives
		// bare — and a bare name matches neither passthroughPaths nor
		// mode.ForLink's own Kbuild carveouts, both of which key on the
		// full path. The link then gets modelled, arch/x86/tools/relocs
		// reads the drvref stub back synchronously, and the build dies
		// with "No ELF magic" — a message that names nothing useful.
		// Resolve against cwd first so the subtree is visible.
		if out := ldOutputArg(args); out != "" {
			if abs, err := filepath.Abs(out); err == nil && carvedOut(abs) {
				logf("ld passthrough: %s is in a carved-out subtree", out)
				return Passthrough(real, args)
			}
		}
		// Quiet otherwise: this is the overwhelmingly common case and
		// logging each one would bury the lines that matter.
		return Link(dispatch.ToolLD, args, cfg, l)
	}

	// Same carveout for the `-r` path, where parseLDArgs already gave
	// us the output. Checked absolute for the same cwd reason.
	if carvedOut(output) {
		logf("ld passthrough: %s is in a carved-out subtree", output)
		return Passthrough(real, args)
	}
	if abs, err := filepath.Abs(output); err == nil && carvedOut(abs) {
		logf("ld passthrough: %s is in a carved-out subtree", output)
		return Passthrough(real, args)
	}
	logf("ld -r %s <- %s", output, joinBase(inputs))

	ci, err, ok := classifyInputs(cfg, inputs, altStorePrefix(cfg.Store), l, "ld",
		func() error { return Passthrough(real, args) })
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
	drv := expr.PartialLinkJSON(expr.PartialLinkJSONParams{
		Name:        "ld-" + outName,
		OutName:     outName,
		System:      cfg.System,
		Bash:        cfg.BashRoot,
		Coreutils:   cfg.CoreutilsRoot,
		ToolBin:     real,
		Flags:       flags,
		Inputs:      ci.JSON,
		StoreDeps:   storedeps.From(flags, wrapperEnvJSON, cfg.KnownStorePaths),
		Placeholder: "/" + expr.OutPlaceholderNix32,
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
	return nil
}

// parseLDArgs recognises the `ld … -r -o <out> <obj>…` shape and bails
// on everything else.
//
// `-r` must be present: without it this is a full link, which belongs
// to link.go. `-o` must be present too — ld's default of `a.out` is
// never what a build system means here, and guessing would put the
// artifact somewhere nothing looks for it.
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
			// Flags whose value is a separate token. Without this the
			// value reads as a positional: linker flag sets
			// starts `-m elf_x86_64`, and "elf_x86_64" is neither an
			// object nor an archive, so the whole invocation bailed.
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
			// A positional that is neither an object nor an archive:
			// a linker script, a --script value, something we do not
			// model. Bail rather than silently drop it.
			return nil, "", nil, false
		}
	}
	if !relocatable || output == "" || len(inputs) == 0 {
		return nil, "", nil, false
	}
	return flags, output, inputs, true
}

// realLDFor resolves the `ld` this shim stands in for.
//
// NIXGG_REAL_LD wins when set: a project may need the UNWRAPPED ld,
// passes the UNWRAPPED binutils ld there (see common-flags.nix — "The
// which is not the
// `ld` sitting next to the compiler.
//
// Otherwise fall back to the binutils dir beside the pinned cc, the
// same place archive.go finds `ar`.
func realLDFor(cfg *toolchain.Config) string {
	if v := os.Getenv("NIXGG_REAL_LD"); v != "" {
		return v
	}
	return filepath.Join(filepath.Dir(cfg.RealCC), "ld")
}

// ldRootOf turns /nix/store/…-binutils/bin/ld into the store root that
// has to be mounted for it.
func ldRootOf(ld string) string {
	return filepath.Dir(filepath.Dir(ld))
}

// ldTwoArg lists the `ld` options whose value is a separate argv token.
//
// Deliberately a fixed list rather than a heuristic: the alternative is
// guessing from whether the next token looks like a filename, and a
// wrong guess either swallows an object or leaks a flag value into the
// input list — both produce a silently wrong object rather than an
// error. An option missing from here makes parseLDArgs bail into
// passthrough, which is the safe direction.
var ldTwoArg = map[string]bool{
	"-m": true, "-z": true, "-T": true, "-e": true, "-u": true,
	"-y": true, "-Y": true, "-b": true, "-A": true, "-R": true,
	"-F": true, "-h": true, "-I": true,
	"--architecture": true, "--defsym": true, "--dynamic-linker": true,
	"--entry": true, "--script": true, "--soname": true, "--wrap": true,
	"-rpath": true, "--rpath": true, "--undefined": true,
}

// ldOutputArg pulls the `-o` value out of a raw ld command line.
//
// Separate from parseLDArgs, which bails on anything that is not a `-r`
// link and so cannot report the output for the full-link case — the one
// place LD needs it to test a carveout before handing off to Link.
func ldOutputArg(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-o" && i+1 < len(args):
			return args[i+1]
		case strings.HasPrefix(args[i], "-o") && len(args[i]) > 2:
			return args[i][2:]
		}
	}
	return ""
}
