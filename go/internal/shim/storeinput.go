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
	"github.com/tbereknyei/nixgg/internal/mode"
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
// Shared by link.go and archive.go so the two cannot drift, and so this is
// reachable from a test — the original bug lived in code only exercised
// through a full build.
func storeInput(c classify.Result, callerPath string) (expr.Input, expr.JSONDrvInput) {
	rel := c.Sub
	if rel == "" {
		// No Sub means classification could not observe the artifact's
		// position inside its store path. Two ways that happens, and they
		// need opposite treatment:
		//
		//   - A foreign dependency reached through a symlink: Sub IS set
		//     (classify resolved the link), so we never get here.
		//   - One of OUR OWN outputs that `force` promoted to a real file:
		//     the promoted registry records only the store ROOT, so Sub is
		//     empty and the artifact's FHS subdir has to be re-derived.
		//
		// Missing the second case broke native-mode lua: liblua.a lives at
		// <root>/lib/liblua.a but was referenced as <root>/liblua.a, and
		// luac failed with `ld: cannot find …-ar-liblua.a/liblua.a`.
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
// Before classifying, each input is checked against
// batchpending.Path: if it's a still-deferred batch member (see
// deferCompileToBatch), it's resolved into an ordinary per-TU
// thunk/drv HERE, via ResolvePendingMember, before classify.Target
// ever sees it. This is what makes batching safe for every consumer
// that ISN'T a same-group archive (archive.go's own tryBatchArchive
// checks for all-same-group-pending BEFORE calling classifyInputs at
// all, and only reaches this prologue when that fast path didn't
// apply): a mixed-group archive, a direct link with no archive, or
// any other caller of this function transparently falls back to
// today's one-derivation-per-TU behavior for that one input, with
// classify.Target none the wiser that the input was ever deferred.
//
// A Thunk/Drv-classified input that is itself a THIN archive (see
// members.go's own docstring) additionally needs every ONE OF ITS OWN
// members declared as a dependency of this derivation too — Nix only
// mounts what a derivation's own inputs.drvs/inputs.srcs declare, and
// a thin archive's on-disk bytes are just paths, not embedded
// content, so nothing else would make those paths resolve inside a
// LATER, separate consumer's sandbox. expandMembers does this lookup
// (keyed identically to how archive.go wrote the sidecar for this
// exact archive) and recurses into any member that is itself a thin
// archive with its own sidecar — structurally necessary for a thin
// archive nested inside another archive, though not exercised by any
// current fixture (archive.go's own parseARArgs only ever accepts
// `.o` members, so an archive's OWN recorded members are always
// object files today, never another archive — the recursion is
// forward-looking, not dead weight, since loosening that constraint
// later shouldn't require touching this function again).
//
// Crucially, expandMembers' own appends go into the EXTRA slices, not
// the primary linkInputs/jsonInputs the caller's own argv produced.
// A thin archive's members are already referenced from inside its own
// stored bytes (that's the entire point of `ar T` — see members.go's
// docstring on why that stays safe under nixgg's per-derivation-
// sandbox model); the LINK/AR step consuming that archive only needs
// those members MOUNTED into its sandbox, never listed a second time
// as literal argv tokens. Merging them into the rendered set produced
// exactly that bug: `cc main.o libthin.a` where libthin.a already
// contains path references to foo.o/bar.o, plus foo.o/bar.o appended
// AGAIN as separate link-line arguments, made ld see each symbol
// twice ("multiple definition of `foo'"). ExtraLink/ExtraJSON are
// rendered into the derivation's own dependency declarations
// (extraInputs in native mode, inputs.drvs/srcs in sandbox mode) but
// never into the build script text — see Derivation.ExtraInputs'
// docstring.
//
// The PRIMARY lists (Link/JSON) preserve every occurrence from the
// caller's own argv, including repeats — deduplicating them was a
// real regression found against a real LLVM build: CMake's own
// generated link line for llvm-min-tblgen lists `libLLVMSupport.a
// libLLVMTableGen.a libLLVMSupport.a` (Support repeated AFTER
// TableGen), which is CMake's OWN answer to plain `ld`'s left-to-right,
// no-`--start-group` archive resolution — TableGen's objects need
// symbols FROM Support, so Support must appear again after it. Before
// this fix, the second occurrence was silently dropped, producing
// "undefined reference to llvm::FoldingSetBase::..." at link time —
// wrong output, not a passthrough or an error, so it went unnoticed
// until a real end-to-end LLVM build caught it. The PRIMARY lists are
// keyed by (still-tracked, just never checked to skip) seenLink/
// seenJSON maps for a different reason: expandMembers below must know
// which entries are ALREADY explicit primary inputs, so it doesn't
// redundantly re-declare one of them a second time as a dependency-
// only extra — see expandMembers' own docstring on why THAT case is a
// real bug (duplicate dependency declarations render differently
// across native's list-based vs sandbox's map-based wire format).
//
// The EXTRA lists (ExtraLink/ExtraJSON, populated only by
// expandMembers) DO dedup — that's the one place a duplicate is
// actually wrong: a thin archive's own member reachable through two
// different sibling thin archives on one link line must be declared
// as a dependency exactly once, not once per archive that references
// it (see expandMembers' own docstring for the concrete regression
// this prevents).
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

	// A worklist rather than a range, because expanding a FOREIGN thin
	// archive — one nixgg did not produce, so it has no members sidecar
	// and arrives as classify.Regular (see thinar.go) — appends its
	// members back onto the list, and those can themselves be thin
	// archives. Archives nixgg DID produce take the sidecar path
	// instead, via expandMembers below; the two are disjoint.
	//
	// `expanded` guards ONLY the members synthesised here, so a nested
	// archive reached twice, or a cycle, cannot loop. It deliberately
	// does not cover the caller's own inputs: link order is significant
	// and repeats are meaningful there.
	pending := append([]string(nil), inputs...)
	expanded := make(map[string]bool)
	for len(pending) > 0 {
		in := pending[0]
		pending = pending[1:]

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
		case classify.Regular:
			// A real file nixgg did not produce — a project may compile
			// some objects with a tool the shims do not cover.
			//
			// Bailing is not a local decision: an unmodellable input makes
			// THIS archive passthrough, hence a plain file, which makes its
			// parent unmodellable in turn, all the way up. So store the
			// file and depend on its content instead.
			//
			// Sandbox mode only. Native mode has no cascade to break, and a
			// store round-trip there would change drv content for builds
			// that work today.
			if !sandbox.Enabled() {
				logf("%s passthrough: can't model input %s (%s)", logPrefix, in, c.Reason())
				return classifiedInputs{}, passthrough(), false
			}
			// A thin archive is a list of PATHS, not bytes, so storing
			// the file alone loses everything it points at — see
			// thinar.go. Expand it and depend on the members instead.
			if members, isThin, parsed := thinArchiveMembers(in); isThin {
				if !parsed {
					// Thin, but the member table would not parse. Storing
					// it is the one thing we must not do: it holds paths,
					// not bytes, so its members would vanish silently and
					// surface as undefined references at the final link.
					logf("%s passthrough: unparseable thin archive %s", logPrefix, in)
					return classifiedInputs{}, passthrough(), false
				}
				logf("  %s: expanding thin archive %s (%d members)", logPrefix, in, len(members))
				// Only expanded members are deduped, and only against
				// other expanded members: a nested archive can be
				// reached twice (a diamond) or cycle. The caller's own
				// input list must keep every occurrence — `-lfoo … -lfoo`
				// is the standard circular-archive idiom and dropping the
				// repeat loses symbol resolution.
				for _, m := range members {
					if expanded[m] {
						continue
					}
					expanded[m] = true
					pending = append(pending, m)
				}
				continue
			}
			sp, err := storeAddLooseFile(cfg, in)
			if err != nil {
				logf("%s passthrough: store-add %s failed: %v", logPrefix, in, err)
				return classifiedInputs{}, passthrough(), false
			}
			appendJSONDedup(&ci.JSON, seenJSON, expr.JSONDrvInput{
				Kind: "src", Ref: filepath.Base(sp), Name: filepath.Base(in),
			})
		default:
			logf("%s passthrough: can't model input %s (%s)", logPrefix, in, c.Reason())
			return classifiedInputs{}, passthrough(), false
		}
	}
	return ci, nil, true
}

// expandMembers appends every member recorded in a thin archive's own
// members.Write sidecar (if key has one at all — a guaranteed miss
// for any non-thin archive, which never writes one) into extraLink/
// extraJSON — dependency-only, never the rendered argv set; see this
// function's caller for why. Recurses into any member that is itself
// a thin archive with its own sidecar. archKeys guards against
// re-expanding the same archive twice (redundant work, not a
// correctness bug on its own) and against a cycle (a real bug, though
// not one anything in this codebase can currently construct — ar
// refuses to nest an archive inside another archive's own members at
// all; see this function's caller for why).
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

// storeAddLooseFile puts a single build-tree file into the store as a
// DIRECTORY containing it.
//
// `nix store add <file>` would give a store path that IS the file, but
// both serializers render an input's argv token as Ref+"/"+Name and
// expect Ref to be a directory — the shape every drv output already
// has. Staging into a one-file directory keeps that invariant instead
// of special-casing the emitters, which are the byte-identity-critical
// part of the codebase.
func storeAddLooseFile(cfg *toolchain.Config, path string) (string, error) {
	base := filepath.Base(path)
	tmp, err := os.MkdirTemp(os.Getenv("NIX_BUILD_TOP"), "gg-loose-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	src, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, base), src, 0o444); err != nil {
		return "", err
	}
	return sandbox.StoreAddScan(cfg, base, tmp)
}

// carvedOut reports whether a subtree is excluded from modelling.
//
// A declared subtree means "nixgg models nothing here", so every shim
// that produces an artifact has to honour it directly rather than
// inherit it from its inputs. Inheriting only worked while unmodellable
// inputs made the whole subtree bail together; once those are
// store-added instead, a shim can model an artifact inside a subtree
// whose siblings were passed through, and the two halves no longer
// agree.
func carvedOut(path string) bool {
	return mode.For(path) == mode.Passthrough
}
