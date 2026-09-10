// Package expr builds the Nix expression string that a shim writes to
// .nixgg/thunks/<id>.nix. Every expression is one `import <helper>
// { ... }` call — the helper (builder.nix / linker.nix / archiver.nix)
// lives in the realised nix/ package alongside toolchain.nix which
// supplies the pinned compiler/bash/coreutils store paths.
//
// The expression body is byte-deterministic: same inputs → same string
// → same thunk id. That's what makes idempotent Write and Nix's own
// eval cache work.
package expr

import (
	"crypto/sha256"
	"encoding/json"
	"sort"
	"strings"
)

// Compile builds a per-TU compile expression. Routes through the
// shared Derivation struct: same struct also drives CompileJSON in
// sandbox mode, so any field added here shows up in both wire
// formats. See internal/expr/derivation.go on the equivalence
// property.
func Compile(p CompileParams) string {
	d := compileDerivation(p)
	return d.ToNix(p.Helpers)
}

// compileDerivation is the shared "params → Derivation" builder for
// the compile Kind. Used by Compile (→ ToNix) and CompileJSON
// (→ ToJSON).
func compileDerivation(p CompileParams) *Derivation {
	return &Derivation{
		Kind:       KindCompile,
		Tool:       p.Tool,
		SrcStore:   p.SrcTree,
		Source:     p.Source,
		OutName:    p.OutName,
		Flags:      p.Flags,
		StoreDeps:  p.StoreDeps,
		WrapperEnv: p.WrapperEnv,
	}
}

// CompileParams is the input for one compile expression.
type CompileParams struct {
	Helpers    string            // /nix/store/…-nixgg-nix
	Tool       string            // "cc", "gcc", "c++", "g++"
	SrcTree    string            // Nix path literal, e.g. "../srcs/foo"
	Source     string            // relative path inside srcTree, e.g. "src/foo.c"
	OutName    string            // "foo.o"
	Flags      []string          // sandbox-relative flags
	StoreDeps  []string          // /nix/store/... roots referenced by Flags/WrapperEnv
	WrapperEnv map[string]string // NIX_CFLAGS_COMPILE etc.
}

// Link builds a link expression. Same Derivation-based flow as
// Compile — see compileDerivation.
func Link(p LinkParams) string {
	return linkDerivation(p).ToNix(p.Helpers)
}

func linkDerivation(p LinkParams) *Derivation {
	return &Derivation{
		Kind:               KindLink,
		Name:               p.Name,
		Tool:               p.Tool,
		OutName:            p.OutName,
		Inputs:             inputsToDeriv(p.Inputs),
		ExtraInputs:        inputsToDeriv(p.ExtraInputs),
		Flags:              p.Flags,
		GroupInputs:        p.GroupInputs,
		WholeArchiveInputs: p.WholeArchiveInputs,
		InlineFilesStore:   p.InlineFilesStore,
		AbsFilePath:        p.AbsFilePath,
		AbsFileContent:     p.AbsFileContent,
		StoreDeps:          p.StoreDeps,
		WrapperEnv:         p.WrapperEnv,
	}
}

// LinkParams is the input for one link expression.
type LinkParams struct {
	Helpers string
	Tool    string
	OutName string
	// Name overrides linker.nix's default "bin-<OutName>" derivation
	// name — empty means use that default. Set by a multi-target
	// mkNixggBuild build, whose non-primary targets need a name of
	// the form "<outerBuildName>-<targetKey>" to satisfy Nix's own
	// outputPathName check — see go/internal/shim/link.go's
	// linkSandbox docstring.
	Name   string
	Inputs []Input
	// See Derivation.ExtraInputs.
	ExtraInputs []Input
	Flags       []string
	// GroupInputs wraps the input list in --start-group/--end-group.
	// See Derivation.GroupInputs.
	GroupInputs bool
	// See Derivation.WholeArchiveInputs.
	WholeArchiveInputs []string
	// See Derivation.InlineFilesStore.
	InlineFilesStore string
	// See Derivation.AbsFilePath/AbsFileContent.
	AbsFilePath    string
	AbsFileContent string
	StoreDeps      []string
	WrapperEnv     map[string]string
}

// Archive builds an `ar` expression. Same Derivation-based flow.
func Archive(p ArchiveParams) string {
	return archiveDerivation(p).ToNix(p.Helpers)
}

func archiveDerivation(p ArchiveParams) *Derivation {
	return &Derivation{
		Kind:        KindArchive,
		Name:        p.Name,
		OutName:     p.OutName,
		Inputs:      inputsToDeriv(p.Inputs),
		ExtraInputs: inputsToDeriv(p.ExtraInputs),
		ARFlags:     p.ARFlags,
		StoreDeps:   p.StoreDeps,
		WrapperEnv:  p.WrapperEnv,
	}
}

// inputsToDeriv translates the shim-facing Input type (Kind="store"/"nix")
// to the internal derivInput used by Derivation.
func inputsToDeriv(xs []Input) []derivInput {
	out := make([]derivInput, len(xs))
	for i, in := range xs {
		out[i] = derivInput{
			InputKind: in.Kind,
			Ref:       in.Ref,
			Name:      in.Name,
		}
	}
	return out
}

// ArchiveParams is the input for one archive expression.
type ArchiveParams struct {
	Helpers string
	OutName string
	// Name overrides archiver.nix's default "ar-<OutName>" — see
	// LinkParams.Name's own docstring for when/why.
	Name   string
	Inputs []Input
	// See Derivation.ExtraInputs.
	ExtraInputs []Input
	ARFlags     string
	StoreDeps   []string
	WrapperEnv  map[string]string
}

// Input describes one linker/archiver input.
type Input struct {
	// Kind is "store" for realised inputs (rendered as builtins.storePath)
	// or "nix" for unrealised sibling thunks (rendered as an `import`).
	Kind string
	// Ref is either a /nix/store/… root (Kind=store) or an absolute
	// path to a sibling thunk .nix file (Kind=nix). Absolute paths let
	// the thunk file survive `cp`: Makefile steps that copy a thunk
	// symlink to a peer location (e.g. `cp obj/foo.o dest/foo.o`)
	// dereference the symlink and produce a regular file with the same
	// bytes. Absolute imports still resolve; relative ones wouldn't.
	Ref string
	// Name is the basename that will appear inside the derivation
	// output — same as the caller-visible symlink's basename.
	Name string
}

// jsonArrayIndented renders a []string as a pretty JSON array. This is
// what the bash driver used to embed inside a ”...” Nix string. The
// indentation matches so thunk IDs are stable across the rewrite.
func jsonArrayIndented(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	var b strings.Builder
	b.WriteString("[")
	for i, s := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("\n  ")
		enc, _ := json.Marshal(s)
		b.Write(enc)
	}
	b.WriteString("\n]")
	return b.String()
}

// SortedFlags is a defensive helper — some call sites want stable
// ordering even when the caller passed flags in an incidental order.
// Compile flags are typically order-sensitive so we don't use this
// there; it's a utility for tests.
func SortedFlags(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------
// JSON-drv emission (sandbox / dyn-drv mode)
//
// These are byte-for-byte-different serialisations of the same shim
// intent, targeting `nix derivation add` (which reads JSON on stdin)
// rather than a `.nix` text file. Used when NIXGG_SANDBOX=1 — the
// outer mkNixggBuild derivation runs the shims inside a builder-rpc-v0
// sandbox where the only permitted store ops are add / text-add /
// submit-output.
//
// The JSON we emit follows the format Nix's `derivation add` accepts,
// which is *not* the same shape as the JSON `nix derivation show`
// prints. Notably:
//
//   - `inputs.srcs` is an array of BASENAMES (hash+name), not full
//     /nix/store/... paths. Full paths trigger "illegal base-32
//     character '/'" at parse time.
//   - `env.out` must be the placeholder string returned by
//     `builtins.placeholder "out"` for this specific derivation.
//   - `inputs.drvs` maps a full drv store path to `{ outputs = […]; }`
//     — those are the drv-references we get back from previous
//     `nix derivation add` calls.
//
// See nixgg/dyn-drv/NOTES.md for the exploration that established
// this schema.

// JSONDrv is the JSON shape `nix derivation add` accepts. Fields
// mirror the Nix internal derivation type; we assemble it in-Go and
// let json.Marshal handle the wire encoding.
type JSONDrv struct {
	Name    string             `json:"name"`
	System  string             `json:"system"`
	Builder string             `json:"builder"`
	Args    []string           `json:"args"`
	Env     map[string]string  `json:"env"`
	Inputs  JSONDrvInputs      `json:"inputs"`
	Outputs map[string]JSONOut `json:"outputs"`
	Version int                `json:"version"`
}

// JSONDrvInputs holds the two input slots Nix distinguishes.
type JSONDrvInputs struct {
	// Drvs maps a drv's store-path BASENAME (not a full path — same
	// convention as Srcs below; confirmed against derivation.go's own
	// toJSON, which populates this via StoreBasename(in.Ref)) to the
	// set of outputs of that drv we depend on.
	Drvs map[string]JSONDrvRef `json:"drvs"`
	// Srcs is a list of already-realised store objects the sandbox
	// needs mounted. BASENAMES ONLY (see file docstring above).
	Srcs []string `json:"srcs"`
}

// JSONDrvRef is the value side of an inputs.drvs entry. Nix's
// derivation-JSON parser requires `dynamicOutputs` to be present
// (empty is fine); the `omitempty` tag would drop the key when the
// map is nil and trigger "Expected JSON object to contain key
// 'dynamicOutputs' but it doesn't".
type JSONDrvRef struct {
	Outputs        []string       `json:"outputs"`
	DynamicOutputs map[string]any `json:"dynamicOutputs"`
}

// JSONOut describes an output of the derivation.
type JSONOut struct {
	Method   string `json:"method"`   // "nar" for a directory, "flat" for a single file
	HashAlgo string `json:"hashAlgo"` // "sha256"
}

// CompileJSONParams is the sandbox-mode analog of CompileParams.
// SrcTree is a store-path basename (e.g. "abc123-src-foo") — the
// caller has already `nix store add`ed the staged src tree and put
// it in Inputs.Srcs.
type CompileJSONParams struct {
	Name      string   // derivation name, e.g. "tu-foo.o" — no .drv suffix
	OutName   string   // "foo.o"; the builder writes to $out/<outName>
	System    string   // "x86_64-linux"
	Bash      string   // full /nix/store/... path to bash (for builder)
	Coreutils string   // full /nix/store/... path to coreutils (added to PATH)
	Compiler  string   // full /nix/store/... path to gcc-wrapper (added to PATH)
	Tool      string   // "cc", "g++", etc.
	SrcStore  string   // full /nix/store/... path to the staged src tree
	Source    string   // relative path inside SrcStore, e.g. "src/foo.c"
	Flags     []string // compile flags
	StoreDeps []string // full /nix/store/... roots referenced by flags/env — same as
	// native's storeDepsJSON, flattened to colon-separated for the
	// _storeDeps env var (matches builder.nix).
	Placeholder string            // builtins.placeholder "out" for this drv
	Srcs        []string          // basenames for inputs.srcs — bash, coreutils, compiler, srcStore
	Env         map[string]string // extra env (NIX_CFLAGS_COMPILE etc.); merged over defaults
}

// CompileJSON produces a JSONDrv for one compile TU. Delegates to
// Derivation for the env/script shape so this stays in lockstep with
// the native `.nix` emitter — see derivation.go.
func CompileJSON(p CompileJSONParams) JSONDrv {
	d := &Derivation{
		Kind:       KindCompile,
		Name:       p.Name,
		System:     p.System,
		Bash:       p.Bash,
		Coreutils:  p.Coreutils,
		Compiler:   p.Compiler,
		Tool:       p.Tool,
		SrcStore:   p.SrcStore,
		Source:     p.Source,
		OutName:    p.OutName,
		Flags:      p.Flags,
		StoreDeps:  p.StoreDeps,
		WrapperEnv: p.Env,
	}
	return d.toJSON(p.Srcs, nil)
}

// LinkJSONParams is the sandbox-mode analog of LinkParams. Inputs
// coming from prior shim calls have Kind="drv" and Ref=<drv-path>.
// Inputs coming from already-realised store paths (e.g. system libs
// referenced via -I on a compile) have Kind="src" and Ref=<basename>.
type LinkJSONParams struct {
	Name      string
	OutName   string
	System    string
	Bash      string
	Coreutils string
	Compiler  string
	Tool      string
	Inputs    []JSONDrvInput // per-input drv or store-path reference
	// See Derivation.ExtraInputs.
	ExtraInputs []JSONDrvInput
	Flags       []string
	GroupInputs bool // wrap inputs in --start-group/--end-group
	// See Derivation.WholeArchiveInputs.
	WholeArchiveInputs []string
	// See Derivation.InlineFilesStore.
	InlineFilesStore string
	// See Derivation.AbsFilePath/AbsFileContent.
	AbsFilePath    string
	AbsFileContent string
	StoreDeps      []string // full /nix/store/... roots referenced; joined into _storeDeps env
	Placeholder    string
	ExtraSrcs      []string          // additional basenames for inputs.srcs (bash, coreutils, compiler)
	Env            map[string]string // wrapper env
}

// JSONDrvInput is one entry in a linker/archiver's input list. Either
// references a drv (whose "out" we'll dereference at build time via a
// placeholder) or a real store path we already have (basename in
// inputs.srcs, path fragment in args).
type JSONDrvInput struct {
	// Kind = "drv" or "src". "drv" means Ref is a full drv store path
	// (/nix/store/…-….drv) and Name is the file inside that drv's out
	// dir. "src" means Ref is a store-path basename (goes in srcs) and
	// Name is the file inside that store path.
	Kind string
	Ref  string
	Name string
	// Crate: rustc externs only. The name the consuming crate binds
	// this dependency to.
	Crate string
}

// ArchiveJSONParams is the sandbox-mode analog of ArchiveParams.
type ArchiveJSONParams struct {
	Name      string
	OutName   string
	System    string
	Bash      string
	Coreutils string
	AR        string // full /nix/store/... path to gnu binutils (for `ar`)
	ARFlags   string // e.g. "rcs"
	Inputs    []JSONDrvInput
	// See Derivation.ExtraInputs.
	ExtraInputs []JSONDrvInput
	StoreDeps   []string // full /nix/store/... roots; joined into _storeDeps env
	Placeholder string
	ExtraSrcs   []string
	Env         map[string]string
}

// ArchiveJSON produces a JSONDrv for an ar step. Delegates to
// Derivation for env/script shape.
func ArchiveJSON(p ArchiveJSONParams) JSONDrv {
	d := &Derivation{
		Kind:        KindArchive,
		Name:        p.Name,
		System:      p.System,
		Bash:        p.Bash,
		Coreutils:   p.Coreutils,
		AR:          p.AR,
		OutName:     p.OutName,
		ARFlags:     p.ARFlags,
		Inputs:      inputsFromJSON(p.Inputs),
		ExtraInputs: inputsFromJSON(p.ExtraInputs),
		StoreDeps:   p.StoreDeps,
		WrapperEnv:  p.Env,
	}
	return d.toJSON(p.ExtraSrcs, nil)
}

// TransformJSONParams describes an in-place rewrite of one object.
type TransformJSONParams struct {
	Name        string
	OutName     string
	System      string
	Bash        string
	Coreutils   string
	ToolBin     string // absolute /nix/store/… path of the rewriting binary
	InPlace     bool   // true: tool rewrites its single operand (objtool)
	Flags       []string
	Input       JSONDrvInput
	StoreDeps   []string
	Placeholder string
	ExtraSrcs   []string
	Env         map[string]string
}

// TransformJSON produces a JSONDrv for a KindTransform step: consume
// one object, emit the same object rewritten. Sandbox mode only.
func TransformJSON(p TransformJSONParams) JSONDrv {
	d := &Derivation{
		Kind:        KindTransform,
		ToolInPlace: p.InPlace,
		Name:        p.Name,
		System:      p.System,
		Bash:        p.Bash,
		Coreutils:   p.Coreutils,
		OutName:     p.OutName,
		ToolBin:     p.ToolBin,
		Flags:       p.Flags,
		Inputs:      inputsFromJSON([]JSONDrvInput{p.Input}),
		StoreDeps:   p.StoreDeps,
		WrapperEnv:  p.Env,
	}
	return d.toJSON(p.ExtraSrcs, nil)
}

// PartialLinkJSONParams describes an `ld -r` step.
type PartialLinkJSONParams struct {
	Name        string
	OutName     string
	System      string
	Bash        string
	Coreutils   string
	ToolBin     string // absolute /nix/store/… path of `ld`
	Flags       []string
	Inputs      []JSONDrvInput
	StoreDeps   []string
	Placeholder string
	ExtraSrcs   []string
	Env         map[string]string
}

// PartialLinkJSON produces a JSONDrv for `ld -r`: several objects in,
// one object out. Sandbox mode only.
func PartialLinkJSON(p PartialLinkJSONParams) JSONDrv {
	d := &Derivation{
		Kind:       KindPartialLink,
		Name:       p.Name,
		System:     p.System,
		Bash:       p.Bash,
		Coreutils:  p.Coreutils,
		OutName:    p.OutName,
		ToolBin:    p.ToolBin,
		Flags:      p.Flags,
		Inputs:     inputsFromJSON(p.Inputs),
		StoreDeps:  p.StoreDeps,
		WrapperEnv: p.Env,
	}
	return d.toJSON(p.ExtraSrcs, nil)
}

// RustcJSONParams describes one rustc crate compile.
type RustcJSONParams struct {
	Name      string
	System    string
	Bash      string
	Coreutils string
	RustcBin  string // absolute /nix/store/…/bin/rustc
	SrcStore  string // staged crate tree
	// Source is relative to SrcStore, or an absolute /nix/store path
	// when the crate root is itself already store content — Rust's own
	// `core` is compiled straight out of the rustc source tree.
	Source    string
	Flags     []string
	Externs   []JSONDrvInput // each carries the crate name it binds to
	Emits     []RustEmit
	StoreDeps []string
	ExtraSrcs []string
	Env       map[string]string
}

// RustcJSON produces a JSONDrv for a rustc crate compile. Sandbox mode
// only: nothing that builds Rust reaches nixgg without it.
func RustcJSON(p RustcJSONParams) JSONDrv {
	d := &Derivation{
		Kind:       KindRustc,
		Name:       p.Name,
		System:     p.System,
		Bash:       p.Bash,
		Coreutils:  p.Coreutils,
		RustcBin:   p.RustcBin,
		SrcStore:   p.SrcStore,
		Source:     p.Source,
		Flags:      p.Flags,
		Inputs:     inputsFromJSON(p.Externs),
		Emits:      p.Emits,
		StoreDeps:  p.StoreDeps,
		WrapperEnv: p.Env,
	}
	return d.toJSON(p.ExtraSrcs, nil)
}

// LinkJSON produces a JSONDrv for a link step. Delegates to
// Derivation for env/script shape.
func LinkJSON(p LinkJSONParams) JSONDrv {
	d := &Derivation{
		Kind:               KindLink,
		Name:               p.Name,
		System:             p.System,
		Bash:               p.Bash,
		Coreutils:          p.Coreutils,
		Compiler:           p.Compiler,
		Tool:               p.Tool,
		OutName:            p.OutName,
		Inputs:             inputsFromJSON(p.Inputs),
		ExtraInputs:        inputsFromJSON(p.ExtraInputs),
		Flags:              p.Flags,
		GroupInputs:        p.GroupInputs,
		WholeArchiveInputs: p.WholeArchiveInputs,
		InlineFilesStore:   p.InlineFilesStore,
		AbsFilePath:        p.AbsFilePath,
		AbsFileContent:     p.AbsFileContent,
		StoreDeps:          p.StoreDeps,
		WrapperEnv:         p.Env,
	}
	return d.toJSON(p.ExtraSrcs, nil)
}

// inputsFromJSON translates the shim-facing JSONDrvInput type
// ("drv"/"src") into Derivation's internal Input type ("nix"/"store").
// Same semantics; two names for historical reasons.
func inputsFromJSON(xs []JSONDrvInput) []derivInput {
	out := make([]derivInput, len(xs))
	for i, in := range xs {
		kind := in.Kind
		if kind == "drv" {
			kind = "nix"
		} else if kind == "src" {
			kind = "store"
		}
		// For store inputs, LinkJSON was previously formatting
		// '/nix/store/%s/%s' from a basename; keep that behavior by
		// making Ref look like a canonical path.
		ref := in.Ref
		if kind == "store" && !strings.HasPrefix(ref, "/nix/store/") {
			ref = "/nix/store/" + ref
		}
		out[i] = derivInput{InputKind: kind, Ref: ref, Name: in.Name, Crate: in.Crate}
	}
	return out
}

// caOutputPlaceholder returns the `/<nix32-hash>` downstream
// placeholder for a specific output of a CA derivation. Nix
// substitutes this string with the resolved store path at build
// time. The formula (mirrored from NixOS/nix
// src/libstore/downstream-placeholder.cc:unknownCaOutput):
//
//	drvName    = drvBasename with trailing ".drv" stripped
//	pathName   = drvName + (outputName == "out" ? "" : "-" + outputName)
//	clearText  = "nix-upstream-output:" + drvHashPart + ":" + pathName
//	digest     = sha256(clearText)
//	rendered   = "/" + nix32(digest)
//
// drvPath is expected to be a full store path — /nix/store/<hash>-<name>.drv.
func caOutputPlaceholder(drvPath, output string) string {
	base := StoreBasename(drvPath)
	// A store-path basename is "<32-char-hash>-<name>".
	// storeHashLen = 32 chars (Nix32 encoding of 160-bit compressed hash).
	if len(base) <= storeHashLen+1 {
		return "/INVALID_DRV_PATH_" + base
	}
	hashPart := base[:storeHashLen]
	drvName := base[storeHashLen+1:]
	drvName = strings.TrimSuffix(drvName, ".drv")

	pathName := drvName
	if output != "out" {
		pathName = drvName + "-" + output
	}
	clearText := "nix-upstream-output:" + hashPart + ":" + pathName
	digest := sha256.Sum256([]byte(clearText))
	return "/" + nix32Encode(digest[:])
}

// storeHashLen is the number of Nix32 characters in the hash prefix
// of a store-path basename. Nix's internal constant is `HashLen = 32`.
const storeHashLen = 32

// CAOutputPlaceholder is caOutputPlaceholder, exported for
// internal/assemble: assembling a tree of resolved drvref stubs needs
// the exact same downstream-placeholder substitution link/archive
// already use for their own "nix" inputs, just driven by a set of
// (relative path -> drv) pairs discovered by walking a directory
// instead of by argv parsing.
func CAOutputPlaceholder(drvPath, output string) string { return caOutputPlaceholder(drvPath, output) }

// OutPlaceholderNix32 is the Nix32-encoded sha256 of "nix-output:out"
// — the string Nix substitutes for a placeholder-`$out` reference at
// build time. Since every derivation with a single "out" output
// uses the same placeholder, we hardcode it. Verified:
//
//	nix eval --raw --expr 'builtins.placeholder "out"'
//	=> /1rz4g4znpzjwh1xymhjpm42vipw92pr73vdgl6xs1hycac8kf2n9
//
// (The leading '/' is the placeholder prefix; drop it if you need
// the bare digest.)
const OutPlaceholderNix32 = "1rz4g4znpzjwh1xymhjpm42vipw92pr73vdgl6xs1hycac8kf2n9"

// nix32Chars is the alphabet used by Nix's base32 encoding. Copied
// verbatim from NixOS/nix src/libutil/include/nix/util/base-nix-32.hh
// — "omitted: E O U T" (and case-folded).
const nix32Chars = "0123456789abcdfghijklmnpqrsvwxyz"

// nix32Encode encodes bytes into Nix's flavour of base32. Iterates
// characters back-to-front, taking 5 bits at a time from a virtual
// bit-shifted view of the input. Mirrors BaseNix32::encode in
// src/libutil/base-nix-32.cc.
func nix32Encode(bs []byte) string {
	if len(bs) == 0 {
		return ""
	}
	// encodedLength(n) = (n*8 - 1) / 5 + 1.
	n := (len(bs)*8-1)/5 + 1
	out := make([]byte, n)
	for k := n - 1; k >= 0; k-- {
		b := k * 5
		i := b / 8
		j := b % 8
		var c byte
		c = bs[i] >> j
		if i+1 < len(bs) {
			c |= bs[i+1] << (8 - j)
		}
		out[n-1-k] = nix32Chars[c&0x1f]
	}
	return string(out)
}

func appendUnique(xs []string, s string) []string {
	for _, x := range xs {
		if x == s {
			return xs
		}
	}
	return append(xs, s)
}

// shellQuoteFlags renders a list of flags for a bash `-c` script,
// single-quoted so no shell interpretation happens.
func shellQuoteFlags(flags []string) string {
	if len(flags) == 0 {
		return ""
	}
	parts := make([]string, 0, len(flags))
	for _, f := range flags {
		parts = append(parts, shellQuote(f))
	}
	return strings.Join(parts, " ")
}

// shellQuote single-quotes one string for a bash `-c` script — the
// same escaping shellQuoteFlags applies per-element, factored out for
// callers (e.g. Derivation.absFileScript) that need to quote exactly
// one path rather than a flag list.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
