package shim

import (
	"os"
	"path/filepath"
)

// The shims memoise expensive store-adds in files under the build root.
// Where those live is load-bearing: `nixgg assemble` captures that root
// and hands it to `nix store add --scan`, which records a reference for
// every store path it finds — and a memo file's entire content IS a
// store path. A memo directory at the build root therefore adds one
// reference per memo to the captured tree's closure, which Nix then
// bind-mounts into every derivation consuming that tree.
//
// assemble.skipNames already excludes ".nixgg" at any depth, so rooting
// scratch there makes new caches excluded by construction. Adding each
// new name to a denylist in another package is the alternative, and it
// fails silently: an over-large closure is not an error until it hits a
// kernel mount limit at build time.
//
// Not everything under the build root is scratch — dynDrvStdenv writes
// files there for phase 2 to read back — so this is a positive
// convention for scratch rather than a blanket exclusion.
const scratchDirName = ".nixgg"

// scratchDir returns a directory for shim scratch that assemble will not
// capture, creating it if needed. Rooted at $NIX_BUILD_TOP so it lives
// exactly as long as the build does; falls back to the system temp dir
// outside a sandbox.
//
// sub names the kind of scratch ("shared", "tools"), keeping unrelated
// caches from colliding in one flat directory.
func scratchDir(sub string) (string, error) {
	dir := filepath.Join(cacheRoot(), scratchDirName, sub)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// cacheRoot picks a directory that lives as long as the build does:
// $NIX_BUILD_TOP inside a sandbox, the system temp dir otherwise.
func cacheRoot() string {
	if v := os.Getenv("NIX_BUILD_TOP"); v != "" {
		return v
	}
	return os.TempDir()
}
