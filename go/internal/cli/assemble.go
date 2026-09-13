// cmdAssemble implements `nixgg assemble <root> <name>`: splitStdenv's
// build-stage postBuild step. <root> is a tree of real files
// interleaved with drvref stubs left by the shims (internal/drvref);
// this walks it, builds one drv that restores the tree and overlays
// each stub with its resolved artifact, and submits it as the outer
// derivation's "out" output.
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
	if len(args) != 2 {
		return fmt.Errorf("usage: nixgg assemble <root> <name>")
	}
	root, name := args[0], args[1]

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

	if err := sandbox.SubmitOutput(cfg, drvPath, "out"); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[nixgg assemble] submitted: %s\n", drvPath)
	return nil
}
