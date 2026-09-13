package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tbereknyei/nixgg/internal/classify"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/realise"
	"github.com/tbereknyei/nixgg/internal/shim"
	"github.com/tbereknyei/nixgg/internal/toolchain"
)

// cmdForce: `nixgg force [--roots] [target…]`
//
// For each target: classify it; if it's a thunk symlink, walk the
// transitive `import …/<id>.nix` graph, realise the root via one
// `nix build --file <helper>`, then re-point every recorded
// caller-visible symlink at the resulting store paths.
//
// --roots: skip explicit targets; scan .nixgg/thunks/ for thunks not
// imported by any other thunk (leaf outputs) and force them.
func cmdForce(args []string) error {
	var (
		thunksDir string
		roots     bool
		targets   []string
	)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--thunks-dir":
			if i+1 >= len(args) {
				return fmt.Errorf("--thunks-dir requires an argument")
			}
			thunksDir = args[i+1]
			i++
		case "--roots":
			roots = true
		default:
			targets = append(targets, a)
		}
	}

	if thunksDir != "" {
		if err := os.Setenv("NIXGG_THUNKS_DIR", thunksDir); err != nil {
			return err
		}
	}
	l, err := paths.Resolve()
	if err != nil {
		return err
	}
	cfg, err := toolchain.FromEnv()
	if err != nil {
		return err
	}

	if roots {
		extra, err := findRootTargets(l)
		if err != nil {
			return err
		}
		targets = append(targets, extra...)
	}

	if len(targets) == 0 {
		return fmt.Errorf("force: no targets (pass targets or --roots)")
	}

	altPrefix := altStorePrefix(cfg.Store)

	for _, target := range targets {
		// A still-deferred batch member (deferCompileToBatch, in
		// internal/shim) skips classifyInputs' own fallback prologue
		// when forced manually, so without this it would classify as
		// Regular below and silently fail to accelerate.
		if err := shim.ResolvePendingMember(cfg, l, target); err != nil {
			return err
		}
		c := classify.Target(target, altPrefix, l)
		switch c.Kind {
		case classify.Absent:
			fmt.Fprintf(os.Stderr, "[nixgg force] no target %s\n", target)
			continue
		case classify.Regular:
			fmt.Fprintf(os.Stderr, "[nixgg force] %s is not a nixgg symlink\n", target)
			continue
		case classify.Store:
			// The target is a regular file previously promoted from a
			// thunk. We know which thunk (from the promoted registry)
			// so we can re-evaluate the DAG rooted there. Nix's eval
			// cache decides whether anything actually needs rebuilding.
			if c.ThunkID != "" {
				thunkPath := filepath.Join(l.Thunks, c.ThunkID+".nix")
				if _, err := os.Stat(thunkPath); err == nil {
					if err := realise.Realise(l, cfg, thunkPath, target); err != nil {
						return err
					}
					continue
				}
			}
			fmt.Fprintf(os.Stderr, "[nixgg force] %s already realised (%s)\n", target, c.Ref)
			continue
		case classify.Thunk:
			if err := realise.Realise(l, cfg, c.Ref, target); err != nil {
				return err
			}
		}
	}
	return nil
}

// findRootTargets scans thunks/ for .nix files not imported by any
// other .nix, then returns one symlink per root (from the manifest).
func findRootTargets(l paths.Layout) ([]string, error) {
	entries, err := os.ReadDir(l.Thunks)
	if err != nil {
		return nil, err
	}
	// Set of all thunk IDs.
	allIDs := map[string]bool{}
	// Union of all IDs referenced by any thunk's imports.
	referenced := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".nix") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".nix")
		allIDs[id] = true
		body, err := os.ReadFile(filepath.Join(l.Thunks, e.Name()))
		if err != nil {
			continue
		}
		for _, sib := range realise.FindThunkImports(body) {
			referenced[strings.TrimSuffix(sib, ".nix")] = true
		}
	}
	var targets []string
	for id := range allIDs {
		if referenced[id] {
			continue
		}
		// Root thunk. Pick any symlink from its manifest.
		manifest := filepath.Join(l.Symlinks, id)
		f, err := os.Open(manifest)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if s := sc.Text(); s != "" {
				targets = append(targets, s)
				break
			}
		}
		f.Close()
	}
	return targets, nil
}

// altStorePrefix returns the on-disk root for a `local?root=<path>`
// store URL (empty string for the canonical /nix/store).
func altStorePrefix(storeURL string) string {
	const prefix = "local?root="
	if strings.HasPrefix(storeURL, prefix) {
		return strings.TrimPrefix(storeURL, prefix)
	}
	return ""
}
