// Package storedeps finds which of the build's known store-path inputs
// are referenced by compile flags or wrapper env values. Those roots
// must appear in the derivation's inputs so Nix will actually make
// them available inside the sandbox — a bare -I/nix/store/... flag
// isn't enough by itself.
package storedeps

import (
	"os"
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

// FromFile is From's counterpart for a real binary on disk rather
// than flag/env text: it substring-scans the file's own bytes for
// each knownPaths entry. Needed by any native-mode Kind whose tool is
// a dynamically-linked binary the wrapped project just built itself
// (e.g. objtool linked against elfutils' libelf.so.1 via an absolute
// RPATH) — sandbox mode gets this for free from `nix store add
// --scan`'s NAR reference scan, but that RPC only exists inside a
// builder-rpc-v0/recursive-nix session, so native mode has no
// daemon-side equivalent and must scan client-side instead. Without
// this, the tool's RPATH targets never become derivation inputs and
// Nix's build sandbox denies it access to them at runtime ("error
// while loading shared libraries").
//
// Best-effort: a read failure returns no matches rather than an error,
// since the caller's real bug (if any) will surface immediately as a
// missing-library failure from the tool itself, with a much clearer
// message than a plumbing error here would give.
func FromFile(path string, knownPaths []string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	content := string(b)
	set := map[string]bool{}
	for _, p := range knownPaths {
		if p != "" && strings.Contains(content, p) {
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
