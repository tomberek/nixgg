package mode

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
)

// Project-specific passthrough paths, supplied by the caller rather than
// compiled in.
//
// The rule mode.For encodes is general: some subtrees build by reading
// object BYTES inline — `nm $(real-prereqs) | sed > $@`, an objdump
// piped to grep that decides whether to fail — and a derivation cannot
// model that, because the answer is needed before make continues.
//
// WHICH subtrees do it is not general at all: they are specific to a
// project, often to its target architecture and version. Compiling a
// list into a general-purpose tool makes every such change a Go change,
// and every other project carries dead string comparisons.
//
// So the paths come from $NIXGG_PASSTHROUGH_PATHS, a JSON array the
// caller sets — the same shape and the same eval-time-computed contract
// as $NIXGG_KNOWN_STORE_PATHS, which matters because both modes must
// export it identically or native and sandbox drv hashes diverge.
//
// Matching stays substring-based, as the compiled-in version was: these
// name a subtree, and both a source and the object built from it have
// to match, from any working
// directory make happens to be in.
//
// What deliberately does NOT move here: rules keyed on build-system
// CONVENTION rather than project layout — autoconf's conftest, cmake's
// probe files and scratch dirs. Those are properties of the tool that
// generated the build, identical across every project using it, so they
// stay compiled in.

const passthroughEnv = "NIXGG_PASSTHROUGH_PATHS"

var (
	ptOnce  sync.Once
	ptPaths []string
)

// passthroughPaths returns the configured subtrees, parsed once.
//
// Unset or unparseable yields none rather than an error: a project with
// no such subtrees is the normal case, and every example build has none.
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
	// silently pass the entire build through, which looks like nixgg
	// accelerating nothing rather than like a config error.
	kept := out[:0]
	for _, p := range out {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return kept
}

// SetPassthroughPaths overrides the configured paths. For tests only —
// production reads the environment exactly once.
func SetPassthroughPaths(paths []string) {
	ptOnce.Do(func() {}) // consume the once so the env cannot overwrite
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
