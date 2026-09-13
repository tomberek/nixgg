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

// storeInput builds the pair of per-input records a Store-classified
// link/archive input needs — one for each wire format.
//
// The subtlety is `Name`. Both serializers render an input's argv token as
// Ref+"/"+Name, so Name must be the path relative to the store ROOT, not
// the caller-visible basename. Those coincide for everything nixgg
// produces (a drv output dir holding one artifact) but not for a
// dependency it merely consumes: LLVM's cmake puts an absolute
// `…-zlib-1.3.2/lib/libz.so` on the link line, and using the basename
// there yields `…-zlib-1.3.2/libz.so` — a file that does not exist, so the
// link fails with `ld.bfd: cannot find`.
//
// Ref stays the root because that is what `builtins.storePath` (native)
// and inputs.srcs (sandbox) accept; neither takes a subpath.
//
// Shared by link.go and archive.go so the two cannot drift.
func storeInput(c classify.Result, callerPath string) (expr.Input, expr.JSONDrvInput) {
	rel := c.Sub
	if rel == "" {
		// No Sub means classification couldn't observe the artifact's
		// position inside its store path — either a foreign dependency
		// reached through a symlink (Sub IS set there, so we never get
		// here) or one of our own outputs that `force` promoted to a
		// real file (the promoted registry records only the store
		// root, so the FHS subdir has to be re-derived here).
		//
		// Missing the second case broke native-mode lua: liblua.a lives
		// at <root>/lib/liblua.a but was referenced as <root>/liblua.a.
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

// classifyInputs classifies every input path a link or archive step
// received, so link.go and archive.go can't drift on how a
// Store/Thunk/Drv classification becomes an expr.Input/JSONDrvInput
// pair. logPrefix names the caller in log lines ("link"/"ar");
// passthrough runs the moment an input can't be modeled, and its
// error is returned with ok=false.
//
// Each input is checked against batchpending.Is first: a still-
// deferred batch member is resolved into an ordinary per-TU thunk/drv
// here (ResolvePendingMember) before classify.Target ever sees it.
//
// A Thunk/Drv-classified input that is itself a thin archive needs
// every one of its own members declared as a dependency too — Nix
// only mounts what inputs.drvs/inputs.srcs declare, and a thin
// archive's on-disk bytes are just paths, not embedded content.
// expandMembers does this lookup and recurses into any member that is
// itself a thin archive.
//
// expandMembers' appends go into the EXTRA slices, never the PRIMARY
// Link/JSON slices the caller's own argv produced: a thin archive's
// members are already referenced from inside its own stored bytes, so
// the consuming link/ar step only needs them MOUNTED, not listed a
// second time as argv tokens. Merging them into the primary set
// produced exactly that bug: `cc main.o libthin.a` where libthin.a
// already references foo.o/bar.o, plus foo.o/bar.o appended again as
// link-line arguments, made ld see each symbol twice ("multiple
// definition of `foo'").
//
// The PRIMARY lists preserve every occurrence from the caller's argv,
// including repeats — deduplicating them was a real regression: CMake's
// link line for llvm-min-tblgen lists `libLLVMSupport.a
// libLLVMTableGen.a libLLVMSupport.a` (ld's left-to-right archive
// resolution needs Support again after TableGen), and dropping the
// second occurrence produced "undefined reference to
// llvm::FoldingSetBase::..." at link time. The seenLink/seenJSON maps
// still track primary entries (just never use them to skip) so
// expandMembers knows not to re-declare one of them as a redundant
// dependency-only extra.
//
// The EXTRA lists (populated only by expandMembers) DO dedup: a thin
// archive's member reachable through two different sibling thin
// archives on one link line must be declared as a dependency exactly
// once, not once per archive that references it.
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
			// c.ThunkID is only set for a promoted (force-realised)
			// native-mode output — the one way a Store classification
			// can still be one of OUR OWN archives rather than a
			// foreign dependency reached through an ordinary store
			// symlink. Key the sidecar lookup by that same thunk ID,
			// matching how archive.go wrote it before promotion.
			if c.ThunkID != "" {
				expandMembers(l, c.ThunkID, &ci.ExtraLink, &ci.ExtraJSON, seenLink, seenJSON, archKeys)
			}
		case classify.Thunk:
			appendLinkNoDedup(&ci.Link, seenLink, expr.Input{
				Kind: "nix", Ref: c.Ref, Name: filepath.Base(in),
			})
			expandMembers(l, thunkKeyFromRef(c.Ref), &ci.ExtraLink, &ci.ExtraJSON, seenLink, seenJSON, archKeys)
		case classify.Drv:
			// Sandbox-mode input: previous shim produced a .drv here.
			// Only meaningful when we're also in sandbox mode.
			//
			// Name is normally the caller's own argv basename — correct
			// when the caller referenced the drv's real output
			// directly. But c.Sub is set when classify.Target followed
			// a SONAME alias symlink (libcrypto.so -> libcrypto.so.3)
			// to reach the drv; there, the caller's basename
			// ("libcrypto.so") is NOT the name the drv's own output
			// will exist under once resolved ("libcrypto.so.3"), so
			// c.Sub — the alias's real target basename — must win.
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

// expandMembers appends every member recorded in a thin archive's own
// members.Write sidecar (a guaranteed miss for any non-thin archive)
// into extraLink/extraJSON — dependency-only, never the primary argv
// set (see classifyInputs' docstring for why). Recurses into any
// member that is itself a thin archive. archKeys guards against
// re-expanding the same archive twice and against a cycle (not
// currently constructible — ar refuses to nest an archive inside
// another archive's own members).
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

// thunkKeyFromRef recovers the thunk.ID string a Thunk-classified
// ref's own filename encodes — the same ID archive.go's native path
// keys its members sidecar by (see archive.go: `members.Write(l,
// string(id), ...)`).
func thunkKeyFromRef(ref string) string {
	return strings.TrimSuffix(filepath.Base(ref), ".nix")
}

// appendLinkNoDedup/appendJSONNoDedup append a PRIMARY input
// unconditionally — the caller's own argv repeats are real
// (CMake's own archive-reordering idiom; see classifiedInputs' own
// docstring) and must survive verbatim. They still mark the
// Kind+Ref+Name key as seen, so expandMembers (which DOES dedup, via
// appendLinkDedup/appendJSONDedup below) knows this entry is already
// an explicit primary input and won't re-declare it as a redundant
// dependency-only extra.
func appendLinkNoDedup(dst *[]expr.Input, seen map[string]bool, in expr.Input) {
	seen[in.Kind+"|"+in.Ref+"|"+in.Name] = true
	*dst = append(*dst, in)
}

func appendJSONNoDedup(dst *[]expr.JSONDrvInput, seen map[string]bool, in expr.JSONDrvInput) {
	seen[in.Kind+"|"+in.Ref+"|"+in.Name] = true
	*dst = append(*dst, in)
}

// appendLinkDedup/appendJSONDedup append iff this exact (Kind, Ref,
// Name) triple hasn't been added to this call's result yet. Used ONLY
// by expandMembers for the EXTRA (dependency-only) lists — a thin
// archive's member reachable through two different sibling thin
// archives on one link line must be declared as a dependency exactly
// once. NOT used for primary inputs — see classifiedInputs' own
// docstring for why deduplicating those was a real regression.
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

// maybeSubmit submits drvPath as one of the outer derivation's outputs
// iff path matches NIXGG_SANDBOX_TARGET, or TARGET is unset and
// defaultSubmit is true.
//
// No-op unless sandbox.Enabled() is the real kind (NIXGG_SANDBOX=1) —
// `nix store submit-output` only makes sense with a live outer
// builder-rpc-v0 derivation. Callers reachable under sandbox.EagerDrv()
// too call this unconditionally; this guard is what makes that safe
// without each call site checking the distinction itself.
//
// linkSandbox passes defaultSubmit=true (a link is usually the final
// artifact); archiveSandbox passes false (an archive is usually
// intermediate, consumed by a later link — it only submits when
// TARGET names it explicitly, e.g. a static-lib-only build).
//
// NIXGG_SANDBOX_TARGET has two shapes:
//
//   - A plain string (today's format, still the default): a single
//     path/basename/absolute-path pattern, matched via matchesTarget.
//     A match submits under output "out" — mkNixggBuild's single-
//     target shape, and splitStdenv's own "/nonexistent/..."
//     never-match sentinel, both keep working unchanged.
//   - A JSON object {"<pattern>": "<outputKey>", ...} — a multi-
//     target mkNixggBuild build. Each key is matched the same way a
//     plain string is; the FIRST match's value names the real output
//     key to submit under (e.g. "mosh-server.drv"), not "out". See
//     nix/mkNixggBuild.nix's own targets docstring for how this map
//     is constructed and why every value ends in ".drv" — outputKey
//     also becomes targetName (below) with LinkJSON/ArchiveJSON's
//     Name override, so submit-output's own outputPathName(outerName,
//     outputKey) check has a real, matching name on the submitted
//     side too.
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
// valid JSON for that shape, or nil if it's the plain-string format
// (single-target). A plain string like "/nonexistent/foo" or
// "mosh-server" is never valid JSON, so this is an unambiguous
// dispatch — no separate env var needed.
func parseTargetMap(target string) map[string]string {
	var m map[string]string
	if err := json.Unmarshal([]byte(target), &m); err != nil {
		return nil
	}
	return m
}

// targetOutputKey returns the output key path would submit under
// per NIXGG_SANDBOX_TARGET, or "" if it wouldn't submit at all — used
// by linkSandbox/archiveSandbox to compute the matching Name override
// (see LinkParams.Name's own docstring) BEFORE the drv is emitted,
// since submit-output's outputPathName check needs the two to already
// agree at emission time, not after the fact.
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

// multiTargetName computes the Name override link.go/archive.go must
// give their own emitted drv when path matches a NON-"out" entry in
// NIXGG_SANDBOX_TARGET's map — i.e. one of 2+ targets sharing one
// outer wrapper. Returns "" when path isn't a (non-"out") multi-
// target match, meaning "use the caller's own default naming"
// (LinkParams.Name/ArchiveParams.Name treat "" as "no override").
//
// Nix's own submit-output enforces outputPathName($name, key) ==
// <the submitted drv's own real name> — confirmed directly: that
// function is drvName + "-" + key (key stripped of ".drv", since
// nix derivation add appends its OWN separate ".drv" to whatever
// name it's given), with NO "bin-"/"ar-" marker anywhere in the
// formula. $name is Nix's own env var for the outer derivation's
// name (confirmed present for every derivation, always, not
// something nixgg has to compute) — mkNixggBuild.nix exports it
// into both preBuild and shellHook precisely so this lookup works
// identically in native mode too.
func multiTargetName(path string) string {
	key := targetOutputKey(path)
	if key == "" || key == "out" {
		return ""
	}
	return os.Getenv("name") + "-" + strings.TrimSuffix(key, ".drv")
}
