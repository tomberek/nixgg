package shim

import (
	"path/filepath"
	"strings"

	"github.com/tbereknyei/nixgg/internal/activitylog"
	"github.com/tbereknyei/nixgg/internal/expr"
	"github.com/tbereknyei/nixgg/internal/members"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/sandbox"
	"github.com/tbereknyei/nixgg/internal/storedeps"
	"github.com/tbereknyei/nixgg/internal/thunk"
	"github.com/tbereknyei/nixgg/internal/toolchain"
	"github.com/tbereknyei/nixgg/internal/wrapperenv"
)

// Archive is the shim entrypoint for `ar <mods> archive.a obj1 obj2 …`.
//
// We don't model `ar r archive.a …` operations that mutate an existing
// archive — every archive we build is fresh. Modifier flags like
// `s`/`u`/`q` still make it into the thunk expression's ARFlags so
// `ar` inside the sandbox is called with the caller's exact intent.
func Archive(args []string, cfg *toolchain.Config, l paths.Layout) error {
	if bypassed() {
		return Passthrough(realARFor(cfg), args)
	}
	modifiers, archive, inputs, ok := parseARArgs(args)
	if !ok {
		// RealiseThunkArgsAndPassthrough (not a bare Passthrough)
		// realizes any argv token that's still one of our own
		// not-yet-realized thunks before the real `ar` reads it —
		// e.g. `ar t vmlinux.a` reading a thin archive this same shim
		// just created moments earlier in the same recipe.
		logf("ar passthrough: not an archive-creating invocation (%s)", joinBase(args))
		activitylog.Emit("ar", "passthrough", activitylog.Fields{"reason": "not_archive_creating", "argv": args})
		return RealiseThunkArgsAndPassthrough(cfg, l, realARFor(cfg), args, sandbox.Enabled())
	}

	logf("archive %s <- %s", archive, joinBase(inputs))

	if handled, err := tryBatchArchive(cfg, l, archive, modifiers, inputs); handled {
		return err
	}

	altPrefix := altStorePrefix(cfg.Store)
	ci, err, ok := classifyInputs(cfg, inputs, altPrefix, l, "ar", func() error {
		// Other already-classified siblings may still be real nixgg
		// thunks even though this one wasn't — realize them first, or
		// thin mode's "store the path" semantics would write a
		// thunk's own .nixgg/thunks/*.nix path into the archive as if
		// it were a real member.
		return RealiseThunkArgsAndPassthrough(cfg, l, realARFor(cfg), args, sandbox.Enabled())
	})
	if !ok {
		return err
	}

	wrapperEnvJSON, err := wrapperenv.JSON()
	if err != nil {
		return err
	}
	storeDeps := storedeps.From(nil, wrapperEnvJSON, cfg.KnownStorePaths)

	if sandbox.Enabled() || sandbox.EagerDrv() {
		drvPath, err := archiveSandbox(cfg, archive, modifiers, ci.JSON, ci.ExtraJSON, storeDeps, wrapperEnvJSON)
		if err != nil {
			return err
		}
		if strings.ContainsRune(modifiers, 'T') {
			key := expr.StoreBasename(drvPath)
			if _, err := members.Write(l, key, jsonDrvInputsToRecords(ci.JSON)); err != nil {
				return err
			}
		}
		return nil
	}

	wrapperEnv, err := decodeStringMap(wrapperEnvJSON)
	if err != nil {
		return err
	}
	e := expr.Archive(expr.ArchiveParams{
		Helpers:     cfg.Helpers,
		Name:        multiTargetName(archive),
		OutName:     filepath.Base(archive),
		Inputs:      ci.Link,
		ExtraInputs: ci.ExtraLink,
		ARFlags:     modifiers,
		StoreDeps:   storeDeps,
		WrapperEnv:  wrapperEnv,
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
	if strings.ContainsRune(modifiers, 'T') {
		if _, err := members.Write(l, string(id), inputsToRecords(ci.Link)); err != nil {
			return err
		}
	}
	logf("  thunk:      %s", thunkPath)
	activitylog.Emit("ar", "thunk", activitylog.Fields{"archive": archive, "thunk": thunkPath, "inputs": ci.Link})
	return nil
}

// inputsToRecords/jsonDrvInputsToRecords convert classifyInputs'
// native/sandbox input slices into members.Record, field-for-field.
func inputsToRecords(in []expr.Input) []members.Record {
	out := make([]members.Record, len(in))
	for i, v := range in {
		out[i] = members.Record{Kind: v.Kind, Ref: v.Ref, Name: v.Name}
	}
	return out
}

func jsonDrvInputsToRecords(in []expr.JSONDrvInput) []members.Record {
	out := make([]members.Record, len(in))
	for i, v := range in {
		out[i] = members.Record{Kind: v.Kind, Ref: v.Ref, Name: v.Name}
	}
	return out
}

// parseARArgs pulls the modifier string, archive path, and input list.
// The classic `ar` CLI has three forms we care about:
//
//	ar rcs   archive.a  obj1 obj2   (rcs = create+replace+index)
//	ar -rcs  archive.a  obj1 obj2   (leading dash tolerated by GNU ar)
//	ar Drcs  archive.a  obj1 obj2   (D = deterministic)
//
// Everything else — tar-like operations, positional -N, `ranlib`-style
// invocations — we pass through unmodeled.
func parseARArgs(args []string) (modifiers, archive string, inputs []string, ok bool) {
	// meson's GCC-toolchain static_library() always prepends
	// `--plugin <path-to-liblto_plugin.so>` regardless of whether this
	// particular library is LTO-compiled; strip every occurrence.
	for len(args) >= 2 && args[0] == "--plugin" {
		args = args[2:]
	}
	if len(args) < 2 {
		return
	}
	modifiers = args[0]
	modifiers = strings.TrimPrefix(modifiers, "-")
	if !isARModifiers(modifiers) {
		return
	}
	archive = args[1]
	for _, in := range args[2:] {
		if !strings.HasSuffix(in, ".o") && !strings.HasSuffix(in, ".a") {
			return "", "", nil, false
		}
		inputs = append(inputs, in)
	}
	if archive == "" {
		return "", "", nil, false
	}
	// Zero members is only modeled for `r`/`q` (construct/replace),
	// e.g. Kbuild's `ar cDPrST archive.a` for a disabled-subsystem
	// directory. t/p/x/m legitimately take zero trailing members too,
	// but there it means "operate on every existing member", so those
	// must still bail to Passthrough.
	if len(inputs) == 0 && !strings.ContainsAny(modifiers, "rq") {
		return "", "", nil, false
	}
	return modifiers, archive, inputs, true
}

func isARModifiers(s string) bool {
	if s == "" {
		return false
	}
	// "T" (thin archive) is included deliberately — see members.go for
	// why it's safe under nixgg's model.
	allowed := "cruvsDxtpqRUbNaimoPST"
	for _, r := range s {
		if !strings.ContainsRune(allowed, r) {
			return false
		}
	}
	return true
}

// realARFor returns the sibling `ar` binary next to the pinned gcc,
// not whatever's earliest on PATH.
func realARFor(cfg *toolchain.Config) string {
	return filepath.Join(filepath.Dir(cfg.RealCC), "ar")
}

// archiveSandbox handles NIXGG_SANDBOX=1: emit a JSON drv describing
// this archive step, hand it to `nix derivation add`, symlink the
// output at the returned drv path. Only submits when
// NIXGG_SANDBOX_TARGET names this archive explicitly.
//
// Returns the registered drv path so a caller building a THIN archive
// can key its own members.Write sidecar off it.
func archiveSandbox(
	cfg *toolchain.Config,
	archive, modifiers string,
	inputs []expr.JSONDrvInput,
	extraInputs []expr.JSONDrvInput,
	storeDeps []string,
	wrapperEnvJSON string,
) (string, error) {
	outName := filepath.Base(archive)
	wrapperEnv, err := decodeStringMap(wrapperEnvJSON)
	if err != nil {
		return "", err
	}
	arRoot := filepath.Dir(filepath.Dir(cfg.RealCC)) // strip /bin

	name := "ar-" + outName
	if override := multiTargetName(archive); override != "" {
		name = override
	}
	drv := expr.ArchiveJSON(expr.ArchiveJSONParams{
		Name:        name,
		OutName:     outName,
		System:      cfg.System,
		Bash:        cfg.BashRoot,
		Coreutils:   cfg.CoreutilsRoot,
		AR:          arRoot,
		ARFlags:     modifiers,
		Inputs:      inputs,
		ExtraInputs: extraInputs,
		StoreDeps:   storeDeps,
		Placeholder: "/" + expr.OutPlaceholderNix32,
		ExtraSrcs: []string{
			baseNameOf(cfg.BashRoot),
			baseNameOf(cfg.CoreutilsRoot),
			baseNameOf(arRoot),
		},
		Env: wrapperEnv,
	})
	drvPath, err := sandbox.DerivationAdd(cfg, drv)
	if err != nil {
		return "", err
	}
	if err := sandbox.PointOutputAtDrv(archive, drvPath); err != nil {
		return "", err
	}
	logf("  drv:        %s", drvPath)
	activitylog.Emit("ar", "drv", activitylog.Fields{"archive": archive, "drv": drvPath})

	maybeSubmit(cfg, drvPath, archive, false)
	return drvPath, nil
}
