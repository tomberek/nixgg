// Package storedeps finds which of the build's known store-path inputs
// are referenced by compile flags or wrapper env values. Those roots
// must appear in the derivation's inputs so Nix will actually make
// them available inside the sandbox — a bare -I/nix/store/... flag
// isn't enough by itself.
package storedeps

import (
	"sort"
	"strings"
)

// From returns the sorted subset of knownPaths that appear as a
// substring of any flag string or of wrapperEnvJSON.
//
// wrapperEnvJSON is a compact JSON object (from wrapperenv.JSON). We
// don't parse it as JSON; a substring search is fine because the
// wrapper env values are opaque strings — we just need every known
// store path they mention to be an input to the drv.
//
// This matches mkNixggBuild.nix's exported/known-paths list verbatim
// rather than pattern-matching arbitrary "/nix/store/..." shaped text:
// reconstructing Nix's store-path grammar (32-char nix32 hash + name)
// with a regex is easy to get subtly wrong at the edges (matching
// hash-lookalike text that isn't valid nix32, or swallowing trailing
// punctuation Nix's name grammar wouldn't accept), producing strings
// `nix derivation add` then rejects outright. Substring-matching a
// pre-known set of hashes — like Nix's own RefScanSink — avoids that
// whole class of bug.
func From(flags []string, wrapperEnvJSON string, knownPaths []string) []string {
	set := map[string]bool{}
	for _, p := range knownPaths {
		if p == "" {
			continue
		}
		for _, f := range flags {
			if strings.Contains(f, p) {
				set[p] = true
				break
			}
		}
		if !set[p] && strings.Contains(wrapperEnvJSON, p) {
			set[p] = true
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
