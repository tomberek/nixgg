package shim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// NIXGG_RUSTC_ENV declares environment variables a crate reads at
// COMPILE time via `env!()` or `std::env::var` in a proc macro. Rust
// has no `-D` for this — a crate that wants a build-tree path reaches
// for the environment instead, and the value is baked into the
// compiled code:
//
//	include!(concat!(env!("OBJTREE"), "/rust/uapi/uapi_generated.rs"))
//
// That makes the variable part of the compile's input, so it has to
// reach the derivation — an unset one is a hard rustc error, and a
// wrong one silently compiles against the wrong file. It can't be
// inferred: nothing on the command line mentions it, and carrying the
// whole environment would put the caller's PATH/TMPDIR/build-tree
// paths into every drv hash, so no two machines would share a cache
// entry. Same shape as NIXGG_PASSTHROUGH_PATHS, and for the same
// reason: the rule is general, the list is not.
const rustcEnvVar = "NIXGG_RUSTC_ENV"

// rustcEnv returns the declared variables' values, with any path
// inside the project rewritten to its position in the staged tree —
// e.g. /build/linux-6.18 names a directory the sandbox doesn't have,
// but the same tree is mounted at srcStore, so env!("OBJTREE") there
// has to name that instead. Values outside the project root are
// carried verbatim.
func rustcEnv(projectRoot, srcStore string) map[string]string {
	names := parseRustcEnvNames(os.Getenv(rustcEnvVar))
	if len(names) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, n := range names {
		v, ok := os.LookupEnv(n)
		if !ok {
			continue
		}
		out[n] = restagePath(v, projectRoot, srcStore)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseRustcEnvNames(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	kept := out[:0]
	for _, n := range out {
		if strings.TrimSpace(n) != "" {
			kept = append(kept, n)
		}
	}
	return kept
}

// restagePath maps an absolute path under projectRoot onto srcStore,
// leaving everything else alone.
func restagePath(v, projectRoot, srcStore string) string {
	if !filepath.IsAbs(v) || projectRoot == "" || srcStore == "" {
		return v
	}
	clean := filepath.Clean(v)
	if clean == projectRoot {
		return srcStore
	}
	rel, err := filepath.Rel(projectRoot, clean)
	if err != nil || strings.HasPrefix(rel, "..") {
		return v
	}
	return filepath.Join(srcStore, rel)
}
