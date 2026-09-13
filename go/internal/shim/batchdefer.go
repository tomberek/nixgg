package shim

import (
	"os"
	"path/filepath"

	"github.com/tbereknyei/nixgg/internal/batchmember"
	"github.com/tbereknyei/nixgg/internal/batchpending"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/sandbox"
	"github.com/tbereknyei/nixgg/internal/toolchain"
)

// deferCompileToBatch is Compile's deferral path for a TU that
// matched an opt-in batch group (cfg.BatchGroups.Classify). Instead
// of submitting its own derivation now, it snapshots everything
// needed to build it later — either standalone (ResolvePendingMember)
// or as one member of a combined batch-archive derivation
// (tryBatchArchive, in batcharchive.go) — and writes a batchpending
// stub at output instead of today's thunk symlink / drvref stub.
//
// Staging (stage.Sources, called by Compile before this) and, in
// sandbox mode, the src tree's upload via sandbox.StoreAddDirectory
// stay unconditional; only the final build+submit step is deferred.
// StoreAddDirectory (not the scanning StoreAddScan) matches
// compileSandbox's own upload so this deferred member's store path
// agrees with native mode's plain path-literal import.
func deferCompileToBatch(
	cfg *toolchain.Config, l paths.Layout, group, tuID string,
	toolName, output, srcRel, srcTreeLiteral string,
	flags, storeDeps []string, wrapperEnv map[string]string,
) error {
	m := batchmember.MemberRecord{
		Group:      group,
		Tool:       toolName,
		Source:     srcRel,
		OutName:    filepath.Base(output),
		Flags:      flags,
		StoreDeps:  storeDeps,
		WrapperEnv: wrapperEnv,
	}
	if sandbox.Enabled() || sandbox.EagerDrv() {
		srcStore, err := sandbox.StoreAddDirectory(cfg, tuID, filepath.Join(l.Srcs, tuID))
		if err != nil {
			return err
		}
		m.SrcStore = srcStore
	} else {
		m.SrcTreeLiteral = srcTreeLiteral
	}

	absOutput, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	recordPath, err := batchmember.Write(l, group, absOutput, m)
	if err != nil {
		return err
	}

	if err := writeBatchPendingStub(output, recordPath); err != nil {
		return err
	}
	logf("  batch-pending: %s -> %s", output, recordPath)
	return nil
}

// writeBatchPendingStub replaces output with a batch-pending stub —
// the deferred-compile analogue of thunk.LinkPlaceholder /
// sandbox.PointOutputAtDrv. A regular file, not a symlink, since
// nothing exists yet at any target a symlink could point to.
func writeBatchPendingStub(output, recordPath string) error {
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	_ = os.Remove(output)
	return os.WriteFile(output, []byte(batchpending.Body(recordPath)), 0o644)
}
