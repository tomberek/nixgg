package shim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/tbereknyei/nixgg/internal/batchpending"
	"github.com/tbereknyei/nixgg/internal/classify"
	"github.com/tbereknyei/nixgg/internal/expr"
	"github.com/tbereknyei/nixgg/internal/members"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/sandbox"
	"github.com/tbereknyei/nixgg/internal/toolchain"
)

// Name must be the store-root-relative path, not the caller's basename:
// both serializers render argv tokens as Ref+"/"+Name, and some deps
// (e.g. cmake's zlib) sit in a subdir the basename alone can't reach,
// producing "ld.bfd: cannot find". Ref stays the root since that's what
// builtins.storePath/inputs.srcs accept.
func storeInput(c classify.Result, callerPath string) (expr.Input, expr.JSONDrvInput) {
	rel := c.Sub
	if rel == "" {
		// Empty Sub means one of our own outputs that `force` promoted
		// to a real file (its registry records only the store root);
		// re-derive the FHS subdir or native-mode lua's liblua.a breaks
		// (lives at <root>/lib/liblua.a, referenced as <root>/liblua.a).
		base := filepath.Base(callerPath)
		if sub := expr.ArtifactSubdir(base); sub != "" {
			rel = sub + "/" + base
		} else {
			rel = base
		}
	}
	return expr.Input{
			Kind: "store", Ref: c.Ref, Name: rel,
		}, expr.JSONDrvInput{
			Kind: "src", Ref: filepath.Base(c.Ref), Name: rel,
		}
}

// classifyInputs classifies every input a link/ar step received into
// an expr.Input/JSONDrvInput pair, shared so link.go and archive.go
// can't drift. passthrough runs when an input can't be modeled.
//
// A still-deferred batch member is resolved via ResolvePendingMember
// before classify.Target sees it.
//
// A thin archive's members aren't embedded bytes, just paths, so Nix
// needs them declared too; expandMembers does that lookup recursively
// and appends to the EXTRA slices only — merging them into PRIMARY
// double-declares members already referenced inside the archive's own
// bytes, which made ld see each symbol twice ("multiple definition").
//
// PRIMARY lists preserve every occurrence from the caller's argv,
// including repeats: CMake's llvm-min-tblgen link line repeats
// libLLVMSupport.a after libLLVMTableGen.a for ld's left-to-right
// archive resolution, and deduplicating that produced "undefined
// reference to llvm::FoldingSetBase::...". EXTRA lists (from
// expandMembers) DO dedup, since the same member can be reachable
// through multiple sibling thin archives on one link line.
type classifiedInputs struct {
	Link      []expr.Input
	ExtraLink []expr.Input
	JSON      []expr.JSONDrvInput
	ExtraJSON []expr.JSONDrvInput
}

func classifyInputs(
	cfg *toolchain.Config, inputs []string, altPrefix string, l paths.Layout, logPrefix string, passthrough func() error,
) (ci classifiedInputs, err error, ok bool) {
	ci.Link = make([]expr.Input, 0, len(inputs))
	ci.JSON = make([]expr.JSONDrvInput, 0, len(inputs))
	seenLink := map[string]bool{}
	seenJSON := map[string]bool{}
	archKeys := map[string]bool{}
	for _, in := range inputs {
		if batchpending.Is(in) {
			if err := ResolvePendingMember(cfg, l, in); err != nil {
				logf("%s passthrough: resolving deferred batch member %s: %v", logPrefix, in, err)
				return classifiedInputs{}, passthrough(), false
			}
		}
		c := classify.Target(in, altPrefix, l)
		switch c.Kind {
		case classify.Store:
			ni, ji := storeInput(c, in)
			appendLinkNoDedup(&ci.Link, seenLink, ni)
			appendJSONNoDedup(&ci.JSON, seenJSON, ji)
			// c.ThunkID is set only for a promoted (force-realised)
			// native-mode output; key the sidecar lookup by the same
			// thunk ID archive.go wrote before promotion.
			if c.ThunkID != "" {
				expandMembers(l, c.ThunkID, &ci.ExtraLink, &ci.ExtraJSON, seenLink, seenJSON, archKeys)
			}
		case classify.Thunk:
			appendLinkNoDedup(&ci.Link, seenLink, expr.Input{
				Kind: "nix", Ref: c.Ref, Name: filepath.Base(in),
			})
			expandMembers(l, thunkKeyFromRef(c.Ref), &ci.ExtraLink, &ci.ExtraJSON, seenLink, seenJSON, archKeys)
		case classify.Drv:
			// c.Sub is set when classify.Target followed a SONAME
			// alias symlink (libcrypto.so -> libcrypto.so.3) to reach
			// the drv; the drv's real output name is the alias
			// target's basename, not the caller's own argv basename.
			name := filepath.Base(in)
			if c.Sub != "" {
				name = c.Sub
			}
			appendJSONNoDedup(&ci.JSON, seenJSON, expr.JSONDrvInput{
				Kind: "drv", Ref: c.Ref, Name: name,
			})
			expandMembers(l, expr.StoreBasename(c.Ref), &ci.ExtraLink, &ci.ExtraJSON, seenLink, seenJSON, archKeys)
		default:
			logf("%s passthrough: can't model input %s (%s)", logPrefix, in, c.Reason())
			return classifiedInputs{}, passthrough(), false
		}
	}
	return ci, nil, true
}

// expandMembers appends a thin archive's own members.Write sidecar
// entries into extraLink/extraJSON, recursing into any member that is
// itself a thin archive. archKeys guards against re-expanding the
// same archive twice (a cycle isn't currently constructible — ar
// refuses to nest an archive inside its own members).
func expandMembers(
	l paths.Layout, key string,
	extraLink *[]expr.Input, extraJSON *[]expr.JSONDrvInput,
	seenLink, seenJSON, archKeys map[string]bool,
) {
	if key == "" || archKeys[key] {
		return
	}
	archKeys[key] = true
	recs, ok, err := members.Read(l, key)
	if err != nil || !ok {
		return
	}
	for _, r := range recs {
		switch r.Kind {
		case "store", "nix":
			appendLinkDedup(extraLink, seenLink, expr.Input{Kind: r.Kind, Ref: r.Ref, Name: r.Name})
			if r.Kind == "nix" {
				expandMembers(l, thunkKeyFromRef(r.Ref), extraLink, extraJSON, seenLink, seenJSON, archKeys)
			}
		case "src", "drv":
			appendJSONDedup(extraJSON, seenJSON, expr.JSONDrvInput{Kind: r.Kind, Ref: r.Ref, Name: r.Name})
			if r.Kind == "drv" {
				expandMembers(l, expr.StoreBasename(r.Ref), extraLink, extraJSON, seenLink, seenJSON, archKeys)
			}
		}
	}
}

// thunkKeyFromRef recovers the thunk.ID a Thunk-classified ref's own
// filename encodes, matching how archive.go keys its members sidecar.
func thunkKeyFromRef(ref string) string {
	return strings.TrimSuffix(filepath.Base(ref), ".nix")
}

// appendLinkNoDedup/appendJSONNoDedup append a PRIMARY input
// unconditionally — argv repeats are real (see classifiedInputs) and
// must survive verbatim. The seen map still records the key so
// expandMembers won't re-declare it as a redundant extra.
func appendLinkNoDedup(dst *[]expr.Input, seen map[string]bool, in expr.Input) {
	seen[in.Kind+"|"+in.Ref+"|"+in.Name] = true
	*dst = append(*dst, in)
}

func appendJSONNoDedup(dst *[]expr.JSONDrvInput, seen map[string]bool, in expr.JSONDrvInput) {
	seen[in.Kind+"|"+in.Ref+"|"+in.Name] = true
	*dst = append(*dst, in)
}

// appendLinkDedup/appendJSONDedup append iff this (Kind, Ref, Name)
// triple hasn't been added yet. Used only by expandMembers for the
// EXTRA lists — see classifiedInputs for why primary inputs must not
// dedup the same way.
func appendLinkDedup(dst *[]expr.Input, seen map[string]bool, in expr.Input) {
	key := in.Kind + "|" + in.Ref + "|" + in.Name
	if seen[key] {
		return
	}
	seen[key] = true
	*dst = append(*dst, in)
}

func appendJSONDedup(dst *[]expr.JSONDrvInput, seen map[string]bool, in expr.JSONDrvInput) {
	key := in.Kind + "|" + in.Ref + "|" + in.Name
	if seen[key] {
		return
	}
	seen[key] = true
	*dst = append(*dst, in)
}

// maybeSubmit submits drvPath as an outer-derivation output iff path
// matches NIXGG_SANDBOX_TARGET, or TARGET is unset and defaultSubmit
// is true. No-op unless sandbox.Enabled() (NIXGG_SANDBOX=1) — the
// underlying `nix store submit-output` only makes sense with a live
// builder-rpc-v0 derivation, and sandbox.EagerDrv() callers rely on
// this guard to call it unconditionally without checking themselves.
//
// NIXGG_SANDBOX_TARGET is either a plain single pattern matched via
// matchesTarget (submits under output "out"), or a JSON object
// {"<pattern>": "<outputKey>", ...} for a multi-target
// mkNixggBuild build, whose first-matching value names the real
// output key to submit under (see nix/mkNixggBuild.nix's targets
// docstring). outputKey also becomes targetName via LinkJSON/
// ArchiveJSON's Name override, so submit-output's own
// outputPathName(outerName, outputKey) check agrees with the
// submitted drv's actual name.
func maybeSubmit(cfg *toolchain.Config, drvPath, path string, defaultSubmit bool) {
	if !sandbox.Enabled() {
		return
	}
	outputKey := "out"
	submit := defaultSubmit
	if os.Getenv("NIXGG_SANDBOX_TARGET") != "" {
		outputKey = targetOutputKey(path)
		submit = outputKey != ""
	}
	if !submit {
		return
	}
	if err := sandbox.SubmitOutput(cfg, drvPath, outputKey); err != nil {
		logf("  submit-output: %v", err)
	} else {
		logf("  submitted: %s (output %s)", drvPath, outputKey)
	}
}

// parseTargetMap returns the {pattern: outputKey} map if target is
// valid JSON, or nil for the plain-string single-target format —
// unambiguous since a plain pattern is never valid JSON.
func parseTargetMap(target string) map[string]string {
	var m map[string]string
	if err := json.Unmarshal([]byte(target), &m); err != nil {
		return nil
	}
	return m
}

// targetOutputKey returns the output key path would submit under per
// NIXGG_SANDBOX_TARGET, or "" if it wouldn't submit — computed before
// the drv is emitted so linkSandbox/archiveSandbox can set a matching
// Name override up front (submit-output's outputPathName check needs
// the two to already agree at emission time).
func targetOutputKey(path string) string {
	target := os.Getenv("NIXGG_SANDBOX_TARGET")
	if target == "" {
		return ""
	}
	if targets := parseTargetMap(target); targets != nil {
		for pattern, key := range targets {
			if matchesTarget(pattern, path) {
				return key
			}
		}
		return ""
	}
	if matchesTarget(target, path) {
		return "out"
	}
	return ""
}

// multiTargetName computes the Name override for a multi-target
// NIXGG_SANDBOX_TARGET match (one of 2+ targets sharing one outer
// wrapper), or "" to mean "use the caller's default naming". Formula
// matches Nix's own submit-output check: outputPathName($name, key)
// == drvName + "-" + key (key stripped of its ".drv" suffix). $name
// is Nix's env var for the outer derivation's name, which
// mkNixggBuild.nix also exports into preBuild/shellHook so native
// mode resolves it the same way.
func multiTargetName(path string) string {
	key := targetOutputKey(path)
	if key == "" || key == "out" {
		return ""
	}
	return os.Getenv("name") + "-" + strings.TrimSuffix(key, ".drv")
}
