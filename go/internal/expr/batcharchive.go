// Package expr's batch-archive emitters combine N compiles + 1 archive
// into ONE derivation for a same-group batch (see internal/batch).
// Native mode leaves each member's srcTree as an unquoted Nix path
// literal for nix/batchArchiver.nix to interpolate at eval time (Go
// never sees the resolved store path); everything else in the compile
// line is pre-quoted shell text.
//
// Named "batch-<outName>" rather than "ar-<outName>" so
// tests/drv-equivalence.sh's regex filter (which expects tu-/ar-/bin-
// prefixed names) doesn't mistake it for a plain archive with no
// native counterpart; tests/batch-drv-equivalence.sh covers this
// shape's own equivalence separately.
package expr

import (
	"fmt"
	"strings"
)

// BatchCompileMember is one compile folded into a combined batch
// archive, in the archive's own `ar` argv order (also the compile
// script order, and the resulting archive's member order).
type BatchCompileMember struct {
	Tool     string
	SrcTree  string // native mode only
	SrcStore string // sandbox mode only
	Source   string
	OutName  string
	Flags    []string
}

// BatchArchiveParams is the native-mode input for one combined
// batch-archive expression.
type BatchArchiveParams struct {
	Helpers    string
	OutName    string
	ARFlags    string
	Members    []BatchCompileMember
	StoreDeps  []string
	WrapperEnv map[string]string
}

// BatchArchive renders a native-mode `import
// <helpers>/batchArchiver.nix { ... }` expression. Constructs the call
// directly rather than through Derivation.ToNix since this shape's
// members list doesn't fit ToNix's per-Kind switch.
func BatchArchive(p BatchArchiveParams) string {
	var b strings.Builder
	fmt.Fprintf(&b, "import %s/batchArchiver.nix {\n", p.Helpers)
	fmt.Fprintf(&b, "  outName        = %q;\n", p.OutName)
	fmt.Fprintf(&b, "  arFlags        = %q;\n", p.ARFlags)
	b.WriteString("  members        = ")
	b.WriteString(batchMembersList(p.Members))
	b.WriteString(";\n")
	fmt.Fprintf(&b, "  storeDepsJSON  = ''%s'';\n", jsonArrayIndented(p.StoreDeps))
	fmt.Fprintf(&b, "  wrapperEnvJSON = ''%s'';\n", jsonObjectSorted(p.WrapperEnv))
	b.WriteString("}\n")
	return b.String()
}

// compileLine uses a Nix indented string, not %q, because
// memberCompileLine already contains double quotes that %q would
// double-escape.
func batchMembersList(members []BatchCompileMember) string {
	if len(members) == 0 {
		return "[ ]"
	}
	var b strings.Builder
	b.WriteString("[\n")
	for _, m := range members {
		b.WriteString("    { ")
		fmt.Fprintf(&b, "srcTree = %s; ", m.SrcTree)
		fmt.Fprintf(&b, "outName = %q; ", m.OutName)
		fmt.Fprintf(&b, "compileLine = %s; ", nixIndentedStringLiteral(memberCompileLine(m)))
		b.WriteString("}\n")
	}
	b.WriteString("  ]")
	return b.String()
}

// memberCompileLine renders one member's compile invocation as plain,
// already shell-quoted text, matching buildScript's KindCompile case
// minus the `cd "$src"` (left to nix/batchArchiver.nix's own srcTree
// interpolation).
func memberCompileLine(m BatchCompileMember) string {
	return fmt.Sprintf(`"%s" %s -c "%s" -o "$objroot/%s"`,
		m.Tool, shellQuoteFlags(m.Flags), m.Source, m.OutName)
}

// BatchArchiveJSONParams is the sandbox-mode input for one combined
// batch-archive JSON drv.
type BatchArchiveJSONParams struct {
	Name      string
	OutName   string
	System    string
	Bash      string
	Coreutils string
	AR        string // also put on PATH for compiling, alongside every member's Tool
	ARFlags   string
	Members   []BatchCompileMember // SrcStore populated, not SrcTree
	StoreDeps []string
	ExtraSrcs []string
	Env       map[string]string
}

// BatchArchiveJSON produces a JSONDrv for a combined batch-archive
// step: N compiles then 1 archive, one derivation, one "out" output.
//
// The script goes into Env["batchScript"] + passAsFile rather than
// Args: `args = ["-c", script]` exceeds the kernel's ARG_MAX past
// ~350 members ("Argument list too long", confirmed against real
// projects); passAsFile writes it to a file instead, exposed via
// `${name}Path`.
func BatchArchiveJSON(p BatchArchiveJSONParams) JSONDrv {
	script := batchArchiveScript(p.Coreutils, p.AR, p.ARFlags, p.OutName, p.Members)
	srcs := append([]string{}, p.ExtraSrcs...)
	seen := map[string]bool{}
	for _, s := range srcs {
		seen[s] = true
	}
	for _, m := range p.Members {
		base := StoreBasename(m.SrcStore)
		if !seen[base] {
			srcs = append(srcs, base)
			seen[base] = true
		}
	}
	for _, sd := range p.StoreDeps {
		base := StoreBasename(sd)
		if !seen[base] {
			srcs = append(srcs, base)
			seen[base] = true
		}
	}
	env := map[string]string{
		"out":            "/" + OutPlaceholderNix32,
		"name":           p.Name,
		"system":         p.System,
		"builder":        p.Bash + "/bin/bash",
		"outputHashAlgo": "sha256",
		"outputHashMode": "nar",
		"_storeDeps":     strings.Join(p.StoreDeps, ":"),
		"passAsFile":     "batchScript",
		"batchScript":    script,
	}
	for k, v := range p.Env {
		env[k] = v
	}
	return JSONDrv{
		Name:    p.Name,
		System:  p.System,
		Builder: p.Bash + "/bin/bash",
		Args:    []string{"-c", `source "$batchScriptPath"`},
		Env:     env,
		Inputs: JSONDrvInputs{
			Drvs: map[string]JSONDrvRef{}, // never a sibling drv reference — see package docstring
			Srcs: srcs,
		},
		Outputs: map[string]JSONOut{
			"out": {Method: "nar", HashAlgo: "sha256"},
		},
		Version: 4,
	}
}

// batchArchiveScript renders N compiles into an objects dir, then one
// `ar` over all of them in member order. For a THIN archive (`T`),
// $objroot must be $out/lib/.nixgg-objs (survives past sandbox
// teardown, since a thin archive stores member paths not bytes);
// otherwise a build-tmp scratch dir is fine since `ar` copies bytes in.
// Compiles run at bounded concurrency (capped by $NIX_BUILD_CORES,
// default 1) since folding N TUs into one derivation loses Nix's own
// per-derivation scheduling. Waits are FIFO on explicit pids, not
// `wait -n`, because `wait -n` can return 127 instead of the real exit
// code for a job that already finished before it was called.
func batchArchiveScript(coreutils, ar, arFlags, archiveOutName string, members []BatchCompileMember) string {
	thin := strings.ContainsRune(arFlags, 'T')
	var b strings.Builder
	fmt.Fprintf(&b, "set -euo pipefail\n")
	fmt.Fprintf(&b, "export PATH=\"%s/bin:%s/bin\"\n", coreutils, ar)
	if thin {
		b.WriteString("mkdir -p \"$out/lib/.nixgg-objs\"\n")
		b.WriteString("objroot=\"$out/lib/.nixgg-objs\"\n")
	} else {
		b.WriteString("mkdir -p \"$out/lib\" .nixgg-objs\n")
		b.WriteString("objroot=\"$PWD/.nixgg-objs\"\n")
	}
	b.WriteString(batchConcurrencyPreamble)
	for _, m := range members {
		fmt.Fprintf(&b, "(cd \"%s\" && %s) &\n", m.SrcStore, memberCompileLine(m))
		b.WriteString("gg_after $!\n")
	}
	b.WriteString(batchConcurrencyDrain)
	objs := make([]string, 0, len(members))
	for _, m := range members {
		objs = append(objs, "\"$objroot/"+m.OutName+"\"")
	}
	fmt.Fprintf(&b, "ar D%s \"$out/lib/%s\" %s\n", arFlags, archiveOutName, strings.Join(objs, " "))
	return b.String()
}

// gg_after backgrounds bookkeeping for a bounded-concurrency job
// runner (plain POSIX-ish bash, no external tool available on PATH).
// Backgrounding happens at each compile's own call site since a
// subshell isn't a valid argv for gg_after to take as one command;
// only pid tracking is shared. gg_fail accumulates failures across the
// whole batch because plain `wait` with no args only reports the LAST
// job's exit status, which would swallow an earlier failure.
const batchConcurrencyPreamble = `gg_max="${NIX_BUILD_CORES:-1}"
case "$gg_max" in ''|*[!0-9]*) gg_max=1 ;; esac
[ "$gg_max" -ge 1 ] || gg_max=1
gg_pids=""
gg_fail=0
gg_after() {
  gg_pids="$gg_pids $1"
  set -- $gg_pids
  if [ "$#" -ge "$gg_max" ]; then
    wait "$1" || gg_fail=1
    shift
    gg_pids="$*"
  fi
}
`

// batchConcurrencyDrain waits out remaining members after the launch
// loop, not per-member, so a later member's failure is never masked by
// ar running anyway on a partial set.
const batchConcurrencyDrain = `for gg_pid in $gg_pids; do
  wait "$gg_pid" || gg_fail=1
done
[ "$gg_fail" -eq 0 ] || exit 1
`
