package shim

import (
	"github.com/tbereknyei/nixgg/internal/batchmember"
	"github.com/tbereknyei/nixgg/internal/batchpending"
	"github.com/tbereknyei/nixgg/internal/expr"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/sandbox"
	"github.com/tbereknyei/nixgg/internal/toolchain"
)

// ResolvePendingMember is the safe fallback for a deferred batch
// member (see deferCompileToBatch) that ends up NOT part of a
// successful combined-archive submission: any other consumer (a
// mixed-group archive, a direct link with no archive, a foreign -l
// reference, a manual `nixgg force`) resolves the member here into
// the ordinary per-TU thunk symlink / drvref stub Compile's
// non-batched path would have written.
//
// Idempotent (thunk.Write and sandbox.PointOutputAtDrv already are),
// so it's safe to call from multiple places: the classifyInputs
// fallback prologue, resolveLibFlag, and cli/force.go's per-target
// loop all reach it.
//
// output is unchanged if this returns an error, or if output does
// not reference a pending member at all (returns nil, a no-op).
func ResolvePendingMember(cfg *toolchain.Config, l paths.Layout, output string) error {
	recordPath := batchpending.Path(output)
	if recordPath == "" {
		return nil // not pending; nothing to do
	}
	m, err := batchmember.Read(recordPath)
	if err != nil {
		return err
	}
	if sandbox.Enabled() || sandbox.EagerDrv() {
		return submitCompileSandboxDrv(cfg, m.Tool, m.OutName, output, m.Source, m.SrcStore,
			m.Flags, m.StoreDeps, m.WrapperEnv)
	}
	e := expr.Compile(expr.CompileParams{
		Helpers:    cfg.Helpers,
		Tool:       m.Tool,
		SrcTree:    m.SrcTreeLiteral,
		Source:     m.Source,
		OutName:    m.OutName,
		Flags:      m.Flags,
		StoreDeps:  m.StoreDeps,
		WrapperEnv: m.WrapperEnv,
	})
	thunkPath, err := submitCompileThunk(l, e, output)
	if err != nil {
		return err
	}
	logf("  thunk:      %s (resolved from deferred batch member)", thunkPath)
	return nil
}
