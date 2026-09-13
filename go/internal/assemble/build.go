package assemble

import (
	"path"
	"strings"

	"github.com/tbereknyei/nixgg/internal/expr"
)

// BuildParams describes the tree-assembly drv: restore a captured tree
// verbatim, then overwrite every discovered stub with its real,
// resolved content.
type BuildParams struct {
	Name      string // drv name, e.g. "gg-tree-hello-2.12.3"
	System    string
	Bash      string // full /nix/store/... path
	Coreutils string // full /nix/store/... path
	// TreeSrc is the store-path basename of the whole captured tree
	// (already `nix store add --scan`ed by the caller — this package
	// only builds the drv, it doesn't touch the store itself).
	TreeSrc string
	Stubs   []Stub
}

// Build assembles the JSONDrv. The builder script copies TreeSrc (the
// captured tree, stubs and all) into $out, then overwrites each stub's
// RelPath with the real artifact from its producing drv's own "out"
// output — found via the same convention link.go/archive.go use
// (expr.ArtifactSubdir, keyed on the stub's basename): flat for a .o,
// lib/ for a .a, bin/ for anything else.
//
// The script goes into Env["buildScript"] + passAsFile rather than
// Args: a large build (openssl: 2230 stubs) makes a multi-hundred-KB
// script, and passing that via `args = ["-c", script]` overflows
// ARG_MAX ("Argument list too long"). Args itself stays a fixed-size
// `source "$buildScriptPath"` regardless of stub count.
func Build(p BuildParams) expr.JSONDrv {
	drvs := map[string]expr.JSONDrvRef{}
	srcs := []string{expr.StoreBasename(p.Bash), expr.StoreBasename(p.Coreutils), p.TreeSrc}

	var script strings.Builder
	script.WriteString("set -euo pipefail\n")
	script.WriteString(`export PATH="` + p.Coreutils + `/bin"` + "\n")
	script.WriteString(`mkdir -p "$out"` + "\n")
	script.WriteString(`cp -a "/nix/store/` + p.TreeSrc + `/." "$out/"` + "\n")
	script.WriteString(`chmod -R u+w "$out"` + "\n")

	for _, s := range p.Stubs {
		key := expr.StoreBasename(s.DrvPath)
		ref := drvs[key]
		ref.Outputs = []string{"out"}
		ref.DynamicOutputs = map[string]any{}
		drvs[key] = ref

		base := path.Base(s.RelPath)
		subdir := expr.ArtifactSubdir(base)
		src := expr.CAOutputPlaceholder(s.DrvPath, "out")
		if subdir != "" {
			src += "/" + subdir
		}
		src += "/" + base
		script.WriteString(`cp -a ` + shellQuote(src) + ` "$out/` + s.RelPath + `"` + "\n")
	}

	return expr.JSONDrv{
		Name:    p.Name,
		System:  p.System,
		Builder: p.Bash + "/bin/bash",
		Args:    []string{"-c", `source "$buildScriptPath"`},
		Env: map[string]string{
			"out":            "/" + expr.OutPlaceholderNix32,
			"name":           p.Name,
			"system":         p.System,
			"builder":        p.Bash + "/bin/bash",
			"outputHashAlgo": "sha256",
			"outputHashMode": "nar",
			"passAsFile":     "buildScript",
			"buildScript":    script.String(),
		},
		Inputs: expr.JSONDrvInputs{
			Drvs: drvs,
			Srcs: srcs,
		},
		Outputs: map[string]expr.JSONOut{
			"out": {Method: "nar", HashAlgo: "sha256"},
		},
		Version: 4,
	}
}

// shellQuote wraps s in single quotes. RelPath's basename comes from
// the project tree, not nixgg, so it isn't necessarily shell-safe.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
