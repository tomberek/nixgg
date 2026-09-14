package shim

import (
	"os"
	"path/filepath"
)

// scratchDirName is nixgg's own scratch dir at the build root — staged
// source trees, thunks, memo caches. assemble.skipNames excludes it by
// name at any depth, so anything rooted here is excluded from the
// captured tree by construction rather than needing its own denylist
// entry in another package.
const scratchDirName = ".nixgg"

// scratchDir returns a directory for shim scratch that assemble will
// not capture, creating it if needed. Rooted at $NIX_BUILD_TOP so it
// lives exactly as long as the build does; falls back to the system
// temp dir outside a sandbox.
//
// sub names the kind of scratch ("tools", ...), keeping unrelated
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
