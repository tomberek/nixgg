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
// It parses the ar modifier string, resolves inputs the same way link
// does, and writes an archive thunk.
//
// We don't model `ar r archive.a …` operations that mutate an existing
// archive — every archive we build is fresh. Modifier flags like
// `s` (index), `u` (update), `q` (quick-append) still make it into the
// thunk expression's ARFlags so `ar` inside the sandbox is called
// with the caller's exact intent.
func Archive(args []string, cfg *toolchain.Config, l paths.Layout) error {
	if bypassed() {
		// See compile.go: no logf here.
		return Passthrough(realARFor(cfg), args)
	}
	modifiers, archive, inputs, ok := parseARArgs(args)
	if !ok {
		// Read-mode invocations (ar t/p/x) land here, as does anything
		// whose modifier string we don't model. RealiseThunkArgsAndPassthrough
		// (not a bare Passthrough) realizes any argv token that's
		// still one of our own not-yet-realized thunks before the real
		// `ar` reads it — Linux Kbuild's cmd_ar_vmlinux.a hits this
		// directly (`ar t vmlinux.a` reads a thin archive this same
		// shim just created moments earlier, in the same recipe).
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
		// One sibling input couldn't be classified, so the whole call
		// falls back, but the other, already-classified siblings may
		// still be real nixgg thunks — realize them first. Confirmed
		// against a real kernel build: Kbuild's `ar cDPrST built-in.a
		// <14 real nixgg thunks> <1 already-real, empty sibling
		// archive>` — thin mode's "store the path, don't read the
		// content" semantics mean a bare Passthrough would otherwise
		// silently write the 14 thunks' own .nixgg/thunks/*.nix paths
		// into the archive as if they were real members.
		return RealiseThunkArgsAndPassthrough(cfg, l, realARFor(cfg), args, sandbox.Enabled())
	})
	if !ok {
		return err
	}

	wrapperEnvJSON, err := wrapperenv.JSON()
	if err != nil {
		return err
	}
	// Archives have no flag list of their own; the CA hash comes from
	// inputs + modifiers. Wrapper env still matters if any input was
	// compiled with -fPIC / whatever, so we plumb it.
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

// inputsToRecords/jsonDrvInputsToRecords convert classifyInputs' own
// native/sandbox input slices into members.Record — deliberately
// identical field-for-field, since members.Record's own docstring
// states it's meant as a drop-in mirror of expr.Input/JSONDrvInput.
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
	// Skip leading `--plugin <path>` pairs — meson's own GCC-toolchain
	// static_library() invocation always prepends one (loading gcc's
	// LTO plugin so `ar` can read IR-bitcode member objects), whether
	// or not this particular library is itself LTO-compiled. Confirmed
	// directly against a real meson+ninja build of Nix's own libutil
	// (examples/nix-util): `ar --plugin <path-to-liblto_plugin.so>
	// -csrD libnixutil.a nixutil-prelink.o`. `ar` allows repeating the
	// flag, so strip every occurrence, not just one.
	for len(args) >= 2 && args[0] == "--plugin" {
		args = args[2:]
	}
	if len(args) < 2 {
		return
	}
	modifiers = args[0]
	// Tolerate GNU's leading dash.
	modifiers = strings.TrimPrefix(modifiers, "-")
	// A modifier string is 1+ chars from a fixed alphabet. Anything
	// else, we bail — it's a positional-count invocation like
	// `ar rN 3 archive.a obj` that we don't model.
	if !isARModifiers(modifiers) {
		return
	}
	archive = args[1]
	for _, in := range args[2:] {
		if !strings.HasSuffix(in, ".o") && !strings.HasSuffix(in, ".a") {
			// Anything else (a .lo, a response file, a positional
			// count) means we can't model the member list; bail
			// entirely rather than silently dropping it.
			return "", "", nil, false
		}
		inputs = append(inputs, in)
	}
	// A zero-member archive (`ar cDPrST archive.a` with nothing after
	// the archive name) IS modeled, not bailed on, but ONLY for the
	// two modifiers that actually construct/replace content (`r`,
	// `q`) — Linux Kbuild issues exactly `ar cDPrST archive.a` (r
	// present) for every disabled-subsystem directory (`obj-y` empty
	// because the subsystem's own CONFIG_* is off). `t`/`p`/`x`/`m`
	// (list/print/extract/move) legitimately take ZERO trailing
	// members too, meaning "operate on every existing member" — those
	// must still bail to Passthrough; a zero-length inputs slice there
	// is not "an empty archive," it is "no member filter given."
	// expr.Archive/archiveSandbox already render a valid empty-inputs
	// derivation (pinned by TestScriptGolden's "archive, no inputs").
	if archive == "" {
		return "", "", nil, false
	}
	if len(inputs) == 0 && !strings.ContainsAny(modifiers, "rq") {
		// t/p/x/m with no trailing members means "operate on every
		// existing member" — not an archive-creating invocation at
		// all, so this must fall to the caller's own !ok branch
		// (RealiseThunkArgsAndPassthrough), same as before.
		return "", "", nil, false
	}
	return modifiers, archive, inputs, true
}

func isARModifiers(s string) bool {
	if s == "" {
		return false
	}
	// Union of the modifier characters ar accepts. Anything outside
	// means we're looking at a positional arg, not modifiers.
	//
	// "T" (thin archive: store each member's own file path instead of
	// embedding its bytes) is included deliberately — see members.go
	// for why a thin archive is safe under nixgg's model (every member
	// is already a permanent, immutable store path before `ar` runs).
	allowed := "cruvsDxtpqRUbNaimoPST"
	for _, r := range s {
		if !strings.ContainsRune(allowed, r) {
			return false
		}
	}
	return true
}

// realARFor returns the sibling `ar` binary next to the pinned gcc.
// GCC wrappers ship an `ar` in the same bin/ dir; that's what we
// want the passthrough to hit, not whatever's earliest on PATH.
func realARFor(cfg *toolchain.Config) string {
	return filepath.Join(filepath.Dir(cfg.RealCC), "ar")
}

// archiveSandbox handles NIXGG_SANDBOX=1: emit a JSON drv describing
// this archive step, hand it to `nix derivation add`, symlink the
// output at the returned drv path. Usually doesn't submit — archives
// are usually intermediate, consumed by a later link — but DOES
// submit when NIXGG_SANDBOX_TARGET names this archive explicitly
// (e.g. a static-lib-only build, or one of a multi-target build's
// targets is itself an archive). See maybeSubmit's own docstring for
// the naming override a multi-target match needs, mirrored here the
// same way linkSandbox does it.
//
// Returns the registered drv path so a caller building a THIN archive
// can key its own members.Write sidecar off it — see members.go's own
// docstring for why that key must match what classify.Target/
// classifyInputs will later derive for this same archive.
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
	// `ar` lives in the same dir as the caller's real cc — that's the
	// gcc-wrapper's binutils dependency.
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

	// See maybeSubmit's comment.
	maybeSubmit(cfg, drvPath, archive, false)
	return drvPath, nil
}
