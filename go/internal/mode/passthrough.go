package mode

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
)

// NIXGG_PASSTHROUGH_PATHS lets a caller declare project-specific
// subtrees that build by reading object bytes inline (`nm
// $(real-prereqs) | sed > $@`, an objdump piped to grep deciding
// pass/fail) — a derivation can't model that, since the answer is
// needed before make continues, same shape as the Realise carveouts
// above but keyed on a project's own layout rather than a build-
// system convention. Compiling a per-project list into this package
// would make every such change a Go change and every other project
// carry dead comparisons; this way, the paths are eval-time
// configuration, same contract as NIXGG_KNOWN_STORE_PATHS (both modes
// must export it identically or native/sandbox drv hashes diverge).
//
// Matching is substring-based: a path names a subtree, and both a
// source and the object built from it must match regardless of make's
// cwd.
const passthroughEnv = "NIXGG_PASSTHROUGH_PATHS"

var (
	ptOnce  sync.Once
	ptPaths []string
)

// passthroughPaths returns the configured subtrees, parsed once.
// Unset or unparseable yields none — a project with no such subtrees
// is the normal case.
func passthroughPaths() []string {
	ptOnce.Do(func() { ptPaths = parsePassthrough(os.Getenv(passthroughEnv)) })
	return ptPaths
}

func parsePassthrough(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	// Drop empties: a stray "" would substring-match every path and
	// silently pass the entire build through.
	kept := out[:0]
	for _, p := range out {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return kept
}

// SetPassthroughPaths overrides the configured paths. For tests only
// — production reads the environment exactly once.
func SetPassthroughPaths(paths []string) {
	ptOnce.Do(func() {}) // consume the once so the env can't overwrite
	ptPaths = paths
}

func matchesPassthrough(path string) bool {
	for _, p := range passthroughPaths() {
		if strings.Contains(path, p) {
			return true
		}
	}
	return false
}
