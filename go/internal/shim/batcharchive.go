package shim

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tbereknyei/nixgg/internal/activitylog"
	"github.com/tbereknyei/nixgg/internal/batchmember"
	"github.com/tbereknyei/nixgg/internal/batchpending"
	"github.com/tbereknyei/nixgg/internal/expr"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/sandbox"
	"github.com/tbereknyei/nixgg/internal/storedeps"
	"github.com/tbereknyei/nixgg/internal/thunk"
	"github.com/tbereknyei/nixgg/internal/toolchain"
	"github.com/tbereknyei/nixgg/internal/wrapperenv"
)

// tryBatchArchive is Archive's fast path for a same-group batch: if
// every one of ar's own inputs is a still-pending member of the SAME
// batch group (see deferCompileToBatch), it combines all of them plus
// this archive step into ONE derivation instead of N+1. Runs BEFORE
// classifyInputs, so on any failure to qualify, nothing has been
// touched yet and Archive's normal path runs as if this didn't exist.
//
// handled=false (nil error) means "fall through to Archive's normal
// path". handled=true carries either a real error or nil (submitted).
func tryBatchArchive(cfg *toolchain.Config, l paths.Layout, archive, modifiers string, inputs []string) (handled bool, err error) {
	// Never batch the archive that IS this build's own submission
	// target: submit-output requires the drv's name to match
	// outputPathName($name, outputKey) exactly, and a "batch-"-named
	// drv would violate that. Known gap: such an archive never
	// benefits from batching regardless of group membership.
	if sandbox.Enabled() && targetOutputKey(archive) != "" {
		return false, nil
	}

	members, ok := collectSameGroupMembers(inputs)
	if !ok {
		return false, nil
	}

	return true, submitCombinedArchive(cfg, l, archive, modifiers, members)
}

// collectSameGroupMembers reads every input's batch-pending record, in
// argv order, and reports ok=false the moment any input isn't a
// pending member or belongs to a different group than the first.
func collectSameGroupMembers(inputs []string) (members []batchmember.MemberRecord, ok bool) {
	if len(inputs) == 0 {
		return nil, false
	}
	members = make([]batchmember.MemberRecord, 0, len(inputs))
	var group string
	for i, in := range inputs {
		recPath := batchpending.Path(in)
		if recPath == "" {
			return nil, false
		}
		m, err := batchmember.Read(recPath)
		if err != nil {
			return nil, false
		}
		if i == 0 {
			group = m.Group
		} else if m.Group != group {
			return nil, false
		}
		members = append(members, m)
	}
	return members, true
}

// submitCombinedArchive builds and submits the combined derivation
// covering every member's compile plus this archive step, then reuses
// the same post-build calls Archive's non-batched path makes for its
// output, so downstream classification treats it as an ordinary
// Thunk/Drv regardless of batching.
func submitCombinedArchive(cfg *toolchain.Config, l paths.Layout, archive, modifiers string, members []batchmember.MemberRecord) error {
	outName := filepath.Base(archive)
	members = disambiguateOutNames(members)

	// Computed fresh here rather than reconciled from each member's
	// own snapshot: all members share one ambient build-wide env, so
	// there's nothing to merge.
	wrapperEnvJSON, err := wrapperenv.JSON()
	if err != nil {
		return err
	}
	wrapperEnv, err := decodeStringMap(wrapperEnvJSON)
	if err != nil {
		return err
	}

	storeDeps := unionStoreDeps(members, storedeps.From(nil, wrapperEnvJSON, cfg.KnownStorePaths))

	if sandbox.Enabled() || sandbox.EagerDrv() {
		return submitCombinedArchiveSandbox(cfg, archive, outName, modifiers, members, storeDeps, wrapperEnv)
	}
	return submitCombinedArchiveNative(cfg, l, archive, outName, modifiers, members, storeDeps, wrapperEnv)
}

// disambiguateOutNames renames any member whose OutName collides with
// an earlier member's own — same basename, different subdirectory
// (e.g. libavutil/cpu.c and libavutil/x86/cpu.c both compiling to
// "cpu.o"), which is common in C projects with per-arch/per-backend
// variant files.
//
// batchArchiveScript writes every member's compiled object into ONE
// shared "$objroot" directory keyed only by OutName, so a name
// collision silently overwrites an earlier member's object before
// `ar` ever runs — no build error, just a quietly incomplete archive.
// Confirmed against a real ffmpeg build: batching libavutil this way
// dropped libavutil/x86/cpu.c's object, breaking the final link with
// "undefined reference to `av_get_cpu_flags'".
//
// Renaming is order-stable: the first member to use a basename keeps
// it, later collisions get "-2", "-3", ... before the extension. This
// changes the archive's own member names relative to the unbatched
// path, which is harmless since nothing downstream looks up an object
// by name inside the archive.
func disambiguateOutNames(members []batchmember.MemberRecord) []batchmember.MemberRecord {
	seen := make(map[string]int, len(members))
	out := make([]batchmember.MemberRecord, len(members))
	for i, m := range members {
		n := seen[m.OutName]
		seen[m.OutName] = n + 1
		if n == 0 {
			out[i] = m
			continue
		}
		ext := filepath.Ext(m.OutName)
		base := strings.TrimSuffix(m.OutName, ext)
		m.OutName = fmt.Sprintf("%s-%d%s", base, n+1, ext)
		out[i] = m
	}
	return out
}

// unionStoreDeps returns the deduplicated union of every member's own
// StoreDeps plus extra (the archive's own).
func unionStoreDeps(members []batchmember.MemberRecord, extra []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, m := range members {
		for _, s := range m.StoreDeps {
			add(s)
		}
	}
	for _, s := range extra {
		add(s)
	}
	return out
}

// submitCombinedArchiveNative builds the native-mode combined
// expression and submits it through the identical thunk-write/
// symlink/record calls Archive's own non-batched path uses.
func submitCombinedArchiveNative(cfg *toolchain.Config, l paths.Layout, archive, outName, modifiers string, members []batchmember.MemberRecord, storeDeps []string, wrapperEnv map[string]string) error {
	batchMembers := make([]expr.BatchCompileMember, len(members))
	for i, m := range members {
		batchMembers[i] = expr.BatchCompileMember{
			Tool:    m.Tool,
			SrcTree: m.SrcTreeLiteral,
			Source:  m.Source,
			OutName: m.OutName,
			Flags:   m.Flags,
		}
	}
	e := expr.BatchArchive(expr.BatchArchiveParams{
		Helpers:    cfg.Helpers,
		OutName:    outName,
		ARFlags:    modifiers,
		Members:    batchMembers,
		StoreDeps:  storeDeps,
		WrapperEnv: wrapperEnv,
	})

	id := thunk.Compute(e)
	thunkPath, err := thunk.Write(l, id, e)
	if err != nil {
		return err
	}
	if err := thunk.LinkPlaceholder(l, archive, thunkPath); err != nil {
		return err
	}
	if err := thunk.RecordSymlink(l, id, archive); err != nil {
		return err
	}
	logf("  thunk:      %s (combined batch archive, %d members)", thunkPath, len(members))
	activitylog.Emit("batch", "thunk", activitylog.Fields{
		"archive": archive, "thunk": thunkPath, "members": len(members),
	})
	return nil
}

// submitCombinedArchiveSandbox builds the sandbox-mode combined JSON
// drv and submits it through the identical DerivationAdd/
// PointOutputAtDrv/maybeSubmit calls Archive's own non-batched
// (archiveSandbox) path uses.
func submitCombinedArchiveSandbox(cfg *toolchain.Config, archive, outName, modifiers string, members []batchmember.MemberRecord, storeDeps []string, wrapperEnv map[string]string) error {
	batchMembers := make([]expr.BatchCompileMember, len(members))
	for i, m := range members {
		batchMembers[i] = expr.BatchCompileMember{
			Tool:     m.Tool,
			SrcStore: m.SrcStore,
			Source:   m.Source,
			OutName:  m.OutName,
			Flags:    m.Flags,
		}
	}
	// `ar` (and every member's own compiler) lives in the same dir as
	// the caller's real cc — the gcc-wrapper's binutils dependency.
	// Same convention archiveSandbox already uses.
	arRoot := filepath.Dir(filepath.Dir(cfg.RealCC)) // strip /bin

	extraSrcs := []string{baseNameOf(cfg.BashRoot), baseNameOf(cfg.CoreutilsRoot), baseNameOf(arRoot)}

	drv := expr.BatchArchiveJSON(expr.BatchArchiveJSONParams{
		Name:      "batch-" + outName,
		OutName:   outName,
		System:    cfg.System,
		Bash:      cfg.BashRoot,
		Coreutils: cfg.CoreutilsRoot,
		AR:        arRoot,
		ARFlags:   modifiers,
		Members:   batchMembers,
		StoreDeps: storeDeps,
		ExtraSrcs: extraSrcs,
		Env:       wrapperEnv,
	})

	drvPath, err := sandbox.DerivationAdd(cfg, drv)
	if err != nil {
		return err
	}
	if err := sandbox.PointOutputAtDrv(archive, drvPath); err != nil {
		return err
	}
	logf("  drv:        %s (combined batch archive, %d members)", drvPath, len(members))
	activitylog.Emit("batch", "drv", activitylog.Fields{
		"archive": archive, "drv": drvPath, "members": len(members),
	})

	// See maybeSubmit's own comment: an archive only submits when
	// NIXGG_SANDBOX_TARGET names it explicitly. tryBatchArchive already
	// refused to batch the actual target archive, so defaultSubmit=false
	// mirrors archiveSandbox's call for defensiveness only.
	maybeSubmit(cfg, drvPath, archive, false)
	return nil
}
