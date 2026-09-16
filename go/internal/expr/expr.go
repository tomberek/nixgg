// Package expr builds the Nix expression string a shim writes to
// .nixgg/thunks/<id>.nix; the expression body must be byte-deterministic
// since the thunk id is derived from it.
package expr

import (
	"crypto/sha256"
	"encoding/json"
	"strings"
)

func Compile(p CompileParams) string {
	return compileDerivation(p).ToNix(p.Helpers)
}

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
	Helpers    string
	Tool       string
	SrcTree    string
	Source     string
	OutName    string
	Flags      []string
	StoreDeps  []string
	WrapperEnv map[string]string
}

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
	// Name overrides linker.nix's default "bin-<OutName>"; needed by
	// mkNixggBuild's non-primary multi-target outputs to satisfy Nix's
	// outputPathName check.
	Name               string
	Inputs             []Input
	ExtraInputs        []Input
	Flags              []string
	GroupInputs        bool
	WholeArchiveInputs []string
	InlineFilesStore   string
	AbsFilePath        string
	AbsFileContent     string
	StoreDeps          []string
	WrapperEnv         map[string]string
}

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
	Helpers     string
	OutName     string
	Name        string
	Inputs      []Input
	ExtraInputs []Input
	ARFlags     string
	StoreDeps   []string
	WrapperEnv  map[string]string
}

func Transform(p TransformParams) string {
	return transformDerivation(p).ToNix(p.Helpers)
}

func transformDerivation(p TransformParams) *Derivation {
	return &Derivation{
		Kind:        KindTransform,
		Name:        p.Name,
		OutName:     p.OutName,
		ToolBin:     p.ToolBin,
		ToolInPlace: p.InPlace,
		Flags:       p.Flags,
		Inputs:      inputsToDeriv([]Input{p.Input}),
		StoreDeps:   p.StoreDeps,
		WrapperEnv:  p.WrapperEnv,
	}
}

// TransformParams is the input for one native-mode transform
// expression (objtool rewriting an object in place, or objcopy
// reading one object and writing another). Sandbox mode's analog is
// TransformJSONParams; the two must agree field-for-field so both
// modes hash identically for the same rewrite.
type TransformParams struct {
	Helpers    string
	Name       string
	OutName    string
	ToolBin    string
	InPlace    bool
	Flags      []string
	Input      Input
	StoreDeps  []string
	WrapperEnv map[string]string
}

func PartialLink(p PartialLinkParams) string {
	return partialLinkDerivation(p).ToNix(p.Helpers)
}

func partialLinkDerivation(p PartialLinkParams) *Derivation {
	return &Derivation{
		Kind:       KindPartialLink,
		Name:       p.Name,
		OutName:    p.OutName,
		ToolBin:    p.ToolBin,
		Flags:      p.Flags,
		Inputs:     inputsToDeriv(p.Inputs),
		StoreDeps:  p.StoreDeps,
		WrapperEnv: p.WrapperEnv,
	}
}

// PartialLinkParams is the input for one native-mode `ld -r` expression:
// several objects in, one object out. Sandbox mode's analog is
// PartialLinkJSONParams; the two must agree field-for-field so both
// modes hash identically for the same partial link.
type PartialLinkParams struct {
	Helpers    string
	Name       string
	OutName    string
	ToolBin    string
	Flags      []string
	Inputs     []Input
	StoreDeps  []string
	WrapperEnv map[string]string
}

// Input describes one linker/archiver input. Kind is "store" for
// realised inputs or "nix" for unrealised sibling thunks.
type Input struct {
	Kind string
	// Ref must be an absolute path for Kind=="nix": a Makefile step
	// that `cp`s a thunk symlink to a peer path dereferences it into a
	// regular file, and only an absolute import still resolves from
	// there.
	Ref  string
	Name string
}

// jsonArrayIndented renders a []string as a pretty, indented JSON
// array — the exact indentation is load-bearing for thunk-id stability.
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

// ---------------------------------------------------------------------
// JSON-drv emission (sandbox / dyn-drv mode, NIXGG_SANDBOX=1): the
// `nix derivation add` JSON shape, which differs from `nix derivation
// show`'s output shape. inputs.srcs entries must be BASENAMES — a full
// /nix/store/... path triggers "illegal base-32 character '/'".

// JSONDrv is the JSON shape `nix derivation add` accepts.
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

// JSONDrvInputs holds the two input slots Nix distinguishes. Both
// Drvs keys and Srcs entries are store-path BASENAMES, not full paths.
type JSONDrvInputs struct {
	Drvs map[string]JSONDrvRef `json:"drvs"`
	Srcs []string              `json:"srcs"`
}

// JSONDrvRef is the value side of an inputs.drvs entry. DynamicOutputs
// must be present (empty map is fine) or Nix's parser rejects the JSON
// with "Expected JSON object to contain key 'dynamicOutputs'".
type JSONDrvRef struct {
	Outputs        []string       `json:"outputs"`
	DynamicOutputs map[string]any `json:"dynamicOutputs"`
}

// JSONOut describes an output of the derivation.
type JSONOut struct {
	Method   string `json:"method"`
	HashAlgo string `json:"hashAlgo"`
}

// CompileJSONParams is the sandbox-mode analog of CompileParams.
// SrcTree is a store-path basename already `nix store add`ed by the
// caller and present in Srcs.
type CompileJSONParams struct {
	Name        string
	OutName     string
	System      string
	Bash        string
	Coreutils   string
	Compiler    string
	Tool        string
	SrcStore    string
	Source      string
	Flags       []string
	StoreDeps   []string
	Placeholder string
	Srcs        []string
	Env         map[string]string
}

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
	return d.toJSON(p.Srcs)
}

// LinkJSONParams is the sandbox-mode analog of LinkParams. Inputs
// from prior shim calls have Kind="drv" and Ref=<drv-path>; inputs
// from already-realised store paths have Kind="src" and Ref=<basename>.
type LinkJSONParams struct {
	Name               string
	OutName            string
	System             string
	Bash               string
	Coreutils          string
	Compiler           string
	Tool               string
	Inputs             []JSONDrvInput
	ExtraInputs        []JSONDrvInput
	Flags              []string
	GroupInputs        bool
	WholeArchiveInputs []string
	InlineFilesStore   string
	AbsFilePath        string
	AbsFileContent     string
	StoreDeps          []string
	Placeholder        string
	ExtraSrcs          []string
	Env                map[string]string
}

// JSONDrvInput is one entry in a linker/archiver's input list.
// Kind=="drv" means Ref is a full drv store path and Name is the file
// inside that drv's out dir. Kind=="src" means Ref is a store-path
// basename (goes in srcs) and Name is the file inside that store path.
type JSONDrvInput struct {
	Kind string
	Ref  string
	Name string
	// Crate: rustc externs only. The name the consuming crate binds
	// this dependency to.
	Crate string
}

// ArchiveJSONParams is the sandbox-mode analog of ArchiveParams.
type ArchiveJSONParams struct {
	Name        string
	OutName     string
	System      string
	Bash        string
	Coreutils   string
	AR          string
	ARFlags     string
	Inputs      []JSONDrvInput
	ExtraInputs []JSONDrvInput
	StoreDeps   []string
	Placeholder string
	ExtraSrcs   []string
	Env         map[string]string
}

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
	return d.toJSON(p.ExtraSrcs)
}

// PartialLinkJSONParams describes an `ld -r` step: several objects in,
// one object out. Sandbox mode only.
type PartialLinkJSONParams struct {
	Name      string
	OutName   string
	System    string
	Bash      string
	Coreutils string
	ToolBin   string // absolute /nix/store/…/bin/ld
	Flags     []string
	Inputs    []JSONDrvInput
	StoreDeps []string
	ExtraSrcs []string
	Env       map[string]string
}

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
	return d.toJSON(p.ExtraSrcs)
}

// TransformJSONParams describes an in-place rewrite (objtool) or
// read/write rewrite (objcopy) of one object. Sandbox mode only.
type TransformJSONParams struct {
	Name      string
	OutName   string
	System    string
	Bash      string
	Coreutils string
	ToolBin   string // absolute /nix/store/… path of the rewriting binary
	InPlace   bool   // true: tool rewrites its single operand (objtool)
	Flags     []string
	Input     JSONDrvInput
	StoreDeps []string
	ExtraSrcs []string
	Env       map[string]string
}

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
	return d.toJSON(p.ExtraSrcs)
}

// RustcJSONParams describes one rustc crate compile. Sandbox mode only.
type RustcJSONParams struct {
	Name      string
	System    string
	Bash      string
	Coreutils string
	RustcBin  string // absolute /nix/store/…/bin/rustc
	SrcStore  string // staged crate tree
	// Source is relative to SrcStore, or an absolute /nix/store path
	// when the crate root is itself already store content — Rust's
	// own `core` is compiled straight out of the rustc source tree.
	Source    string
	Flags     []string
	Externs   []JSONDrvInput // each carries the crate name it binds to
	Emits     []RustEmit
	StoreDeps []string
	ExtraSrcs []string
	Env       map[string]string
}

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
	return d.toJSON(p.ExtraSrcs)
}

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
	return d.toJSON(p.ExtraSrcs)
}

// inputsFromJSON translates JSONDrvInput's "drv"/"src" kinds into
// derivInput's "nix"/"store" kinds.
func inputsFromJSON(xs []JSONDrvInput) []derivInput {
	out := make([]derivInput, len(xs))
	for i, in := range xs {
		kind := in.Kind
		if kind == "drv" {
			kind = "nix"
		} else if kind == "src" {
			kind = "store"
		}
		ref := in.Ref
		if kind == "store" && !strings.HasPrefix(ref, "/nix/store/") {
			ref = "/nix/store/" + ref
		}
		out[i] = derivInput{InputKind: kind, Ref: ref, Name: in.Name, Crate: in.Crate}
	}
	return out
}

// caOutputPlaceholder mirrors NixOS/nix's
// src/libstore/downstream-placeholder.cc:unknownCaOutput:
// sha256("nix-upstream-output:" + drvHashPart + ":" + pathName),
// nix32-encoded with a leading '/'.
func caOutputPlaceholder(drvPath, output string) string {
	base := StoreBasename(drvPath)
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

// storeHashLen is Nix's HashLen constant: nix32-encoded hash prefix
// length in a store-path basename.
const storeHashLen = 32

// CAOutputPlaceholder is exported for internal/assemble, which needs
// the same downstream-placeholder substitution for drvref stubs found
// by walking a directory rather than by argv parsing.
func CAOutputPlaceholder(drvPath, output string) string { return caOutputPlaceholder(drvPath, output) }

// OutPlaceholderNix32 is the Nix32-encoded sha256 of "nix-output:out",
// i.e. `nix eval --raw --expr 'builtins.placeholder "out"'` minus its
// leading '/'. Every single-"out"-output derivation shares this value.
const OutPlaceholderNix32 = "1rz4g4znpzjwh1xymhjpm42vipw92pr73vdgl6xs1hycac8kf2n9"

// nix32Chars is Nix's base32 alphabet (src/libutil/include/nix/util/base-nix-32.hh): digits 0-9 and lowercase a-z minus E,O,U,T.
const nix32Chars = "0123456789abcdfghijklmnpqrsvwxyz"

// nix32Encode mirrors BaseNix32::encode in src/libutil/base-nix-32.cc.
func nix32Encode(bs []byte) string {
	if len(bs) == 0 {
		return ""
	}
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

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
