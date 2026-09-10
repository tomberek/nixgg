// cmdAssemble implements `nixgg assemble <root> <name> [outputKey]`.
//
// splitStdenv's build-stage postBuild step: <root> is a tree of real
// files interleaved with drvref stubs (one per cc/c++/ar/link call the
// shims intercepted — see internal/drvref). Walk it, build one
// assembly drv whose builder restores the tree and overlays each stub
// with its resolved artifact, and submit it as one of the outer
// derivation's outputs.
//
// outputKey defaults to "out", which is dynDrvStdenv's shape: its
// phase-1 derivation declares exactly one output. mkNixggBuild is the
// caller that needs it explicit — since the multi-target rework its
// outer derivation declares "<target>.drv" per target and NO "out" at
// all, so submitting under "out" fails with "submitted unknown output".
// The name argument must agree: Nix enforces that the submitted drv is
// named outputPathName(<outer name>, outputKey), i.e. the outer name
// plus "-" plus outputKey minus its ".drv" suffix.
package cli

import (
	"fmt"
	"os"

	"github.com/tbereknyei/nixgg/internal/assemble"
	"github.com/tbereknyei/nixgg/internal/expr"
	"github.com/tbereknyei/nixgg/internal/sandbox"
	"github.com/tbereknyei/nixgg/internal/toolchain"
)

func cmdAssemble(args []string) error {
	if len(args) != 2 && len(args) != 3 {
		return fmt.Errorf("usage: nixgg assemble <root> <name> [output-key]")
	}
	root, name := args[0], args[1]
	outputKey := "out"
	if len(args) == 3 && args[2] != "" {
		outputKey = args[2]
	}

	cfg, err := toolchain.FromEnv()
	if err != nil {
		return err
	}

	stubs, err := assemble.Walk(root)
	if err != nil {
		return fmt.Errorf("walk %s: %w", root, err)
	}
	fmt.Fprintf(os.Stderr, "[nixgg assemble] %d stub(s) found under %s\n", len(stubs), root)

	// Staged copy excludes .nix-socket (nix store add --scan can't
	// ingest it) — see StageForScan. Left behind afterward: it's
	// self-excluding from future Walk/StageForScan calls, and this
	// sandbox gets torn down anyway.
	staged, err := assemble.StageForScan(root)
	if err != nil {
		return fmt.Errorf("stage %s for scan: %w", root, err)
	}

	treeStore, err := sandbox.StoreAddScan(cfg, name+"-tree", staged)
	if err != nil {
		return fmt.Errorf("stage tree to store: %w", err)
	}
	treeBase := expr.StoreBasename(treeStore)

	drv := assemble.Build(assemble.BuildParams{
		Name:      name,
		System:    cfg.System,
		Bash:      cfg.BashRoot,
		Coreutils: cfg.CoreutilsRoot,
		TreeSrc:   treeBase,
		Stubs:     stubs,
	})

	drvPath, err := sandbox.DerivationAdd(cfg, drv)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[nixgg assemble] drv: %s\n", drvPath)

	if err := sandbox.SubmitOutput(cfg, drvPath, outputKey); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[nixgg assemble] submitted: %s (output %s)\n", drvPath, outputKey)
	return nil
}
