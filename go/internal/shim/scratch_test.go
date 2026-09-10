package shim

import (
	"path/filepath"
	"strings"
	"testing"
)

// Shim scratch must live under a directory assemble already excludes.
//
// A memo file's content IS a store path, so a cache at the build root
// puts one reference per memo into the captured tree's closure, which
// Nix then bind-mounts into every derivation consuming that tree. At
// scale that is tens of thousands of extra references, and the build
// dies with "bind mount ... failed: No space left on device" on a disk
// that is nowhere near full.
//
// Nothing about that failure points at a memo cache, and adding each new
// cache name to a denylist in another package is what allowed it: the
// cache was written long after the denylist and nothing tied them
// together. So assert the convention here, where the scratch is created.
func TestScratchDirIsExcludedFromAssembly(t *testing.T) {
	t.Setenv("NIX_BUILD_TOP", t.TempDir())

	for _, sub := range []string{"shared", "tools"} {
		dir, err := scratchDir(sub)
		if err != nil {
			t.Fatalf("scratchDir(%q): %v", sub, err)
		}
		// assemble.skipNames matches on a path COMPONENT at any depth,
		// so the guarantee is that one component is ".nixgg".
		var excluded bool
		for _, part := range strings.Split(filepath.ToSlash(dir), "/") {
			if part == scratchDirName {
				excluded = true
			}
		}
		if !excluded {
			t.Errorf("scratchDir(%q) = %q, which has no %q component — "+
				"assemble will capture it and every memo becomes a "+
				"reference in the assembled tree's closure",
				sub, dir, scratchDirName)
		}
	}
}

// The name has to stay in step with assemble.skipNames. They live in
// different packages, so nothing but this test connects them.
func TestScratchDirNameMatchesAssemblySkipList(t *testing.T) {
	if scratchDirName != ".nixgg" {
		t.Errorf("scratchDirName = %q; assemble.skipNames excludes \".nixgg\". "+
			"Changing one without the other silently reintroduces the "+
			"closure blowup.", scratchDirName)
	}
}
