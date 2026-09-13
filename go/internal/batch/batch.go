// Package batch classifies a compile's source path against an opt-in
// list of glob patterns naming directories the caller has decided are
// stable enough to bundle into one multi-output derivation instead of
// nixgg's default one-derivation-per-TU.
//
// This package only answers "does this source belong to a batch, and
// which one" — matching the shape of
// nix/configureSrcFilterPresets.nix's includePatterns (`find
// -path`-style globs, curated per-project, not inferred). The actual
// multi-output batch derivation lives elsewhere: internal/shim's
// deferCompileToBatch records a pending member
// (internal/batchmember/internal/batchpending) instead of submitting a
// per-TU derivation, and internal/shim's
// tryBatchArchive/submitCombinedArchive combine every pending member
// belonging to one archive's group into ONE derivation when that
// archive's `ar` invocation sees them. See ARCHITECTURE.md's "What we
// don't (yet) do" for why batching stays opt-in rather than inferred
// (batching an actively-edited directory trades saved Nix
// per-derivation overhead for wasted compiler time on unchanged
// siblings).
package batch

import (
	"path/filepath"
	"strings"
)

// Group is one opt-in batch: a name (used to derive the eventual
// multi-output derivation's own name) and the glob patterns matched,
// UNANCHORED, against the TU's absolute source path — see Classify's
// own docstring for why unanchored. Each pattern is filepath.Match
// syntax per path segment (segments split on "/"), plus a "**"
// segment meaning "zero or more path segments", so "deps/**/*.c"
// reaches deps/hiredis/foo.c and deps/hiredis/sub/foo.c. Patterns use
// forward slashes regardless of host OS.
type Group struct {
	Name     string
	Patterns []string
}

// Config is the parsed opt-in batch manifest — the set of Groups a
// project author declared, in declaration order. Order matters for
// Classify: the first matching Group wins, so an author can list a
// narrow exception before a broad catch-all pattern, same convention
// as switch/case fallthrough.
type Config struct {
	Groups []Group
}

// Classify reports which Group (if any) a TU belongs to, given its
// absolute source path. ok=false means the TU isn't batched — the
// caller should fall back to nixgg's existing one-derivation-per-TU
// path.
//
// Matching is UNANCHORED: a pattern like "deps/**/*.c" matches if
// "deps/..." appears anywhere in the path's segment sequence. This is
// deliberate: the natural anchor — the source's path relative to "the
// project root" — has no single stable value across a build.
// internal/scan computes a ProjectRoot per compile call (the common
// ancestor of that call's own cwd + -I dirs), so the same logical file
// can resolve to a different relative path depending on which
// directory `make` happened to invoke the shim from — confirmed
// against a real redis build, where compiling from inside
// deps/hiredis/ collapsed ProjectRoot down to deps/hiredis itself,
// making the "relative path" just "sds.c". An unanchored, absolute-
// path search has no such root to destabilize.
func (c Config) Classify(absPath string) (group string, ok bool) {
	segs := strings.Split(filepath.ToSlash(absPath), "/")
	for _, g := range c.Groups {
		for _, pat := range g.Patterns {
			patSegs := strings.Split(pat, "/")
			for start := range segs {
				if matchSegs(patSegs, segs[start:]) {
					return g.Name, true
				}
			}
		}
	}
	return "", false
}

// matchSegs matches pat (glob segments, "**" meaning zero-or-more)
// against name (path segments) anchored at name's own start; the
// "unanchored" search happens at the call site by trying every start
// offset into the full path.
func matchSegs(pat, name []string) bool {
	if len(pat) == 0 {
		return len(name) == 0
	}
	if pat[0] == "**" {
		if matchSegs(pat[1:], name) {
			return true
		}
		if len(name) == 0 {
			return false
		}
		return matchSegs(pat, name[1:])
	}
	if len(name) == 0 {
		return false
	}
	ok, err := filepath.Match(pat[0], name[0])
	if err != nil || !ok {
		return false
	}
	return matchSegs(pat[1:], name[1:])
}
