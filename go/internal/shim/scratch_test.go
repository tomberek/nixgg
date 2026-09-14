package shim

import (
	"path/filepath"
	"strings"
	"testing"
)

// Shim scratch must live under a directory assemble already excludes
// (assemble.skipNames) — a memo file's content IS a store path, so a
// cache at the build root would put one reference per memo into the
// captured tree's closure.
func TestScratchDirIsExcludedFromAssembly(t *testing.T) {
	t.Setenv("NIX_BUILD_TOP", t.TempDir())

	dir, err := scratchDir("tools")
	if err != nil {
		t.Fatalf("scratchDir(%q): %v", "tools", err)
	}
	// assemble.skipNames matches on a path COMPONENT at any depth, so
	// the guarantee is that one component is ".nixgg".
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
			"tools", dir, scratchDirName)
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
