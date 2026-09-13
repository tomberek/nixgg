// Package realise turns a placeholder thunk into a materialised store
// path plus a byte-copy in the working tree. It's the shared engine
// behind both `nixgg force` (invoked from the CLI at build end) and the
// link-shim's inline auto-force path (invoked when NIXGG_AUTOFORCE=1
// is set, so a plain `make` produces real binaries).
package realise

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tbereknyei/nixgg/internal/expr"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/thunk"
	"github.com/tbereknyei/nixgg/internal/toolchain"
)

// Realise builds `thunkPath`'s whole DAG in a single Nix invocation and
// re-points every caller-visible symlink at the resulting store paths.
// `target` is the working-tree path to materialise first (a
// compile/link/archive output); its manifest siblings get promoted too.
//
// We call Nix exactly once, not once per thunk: walk the import graph
// locally to enumerate child thunks, write a helper .nix exposing each
// as a named attribute, then `nix build --file <helper> <attr>...` so
// Nix realises everything under one eval/daemon session.
func Realise(l paths.Layout, cfg *toolchain.Config, thunkPath, target string) error {
	children, err := CollectThunks(thunkPath)
	if err != nil {
		return err
	}

	helper, err := writeHelper(thunkPath, children)
	if err != nil {
		return err
	}
	defer os.Remove(helper)

	attrs := make([]string, 0, 1+len(children))
	attrs = append(attrs, "root")
	for i := range children {
		attrs = append(attrs, fmt.Sprintf("t%d", i))
	}
	storePaths, err := nixBuildAttrs(cfg, helper, attrs)
	if err != nil {
		return err
	}
	if len(storePaths) != len(attrs) {
		return fmt.Errorf("realise: expected %d store paths, got %d", len(attrs), len(storePaths))
	}

	rootStore := storePaths[0]
	rootID := thunk.ID(strings.TrimSuffix(filepath.Base(thunkPath), ".nix"))
	if err := PromoteToStore(l, cfg, rootID, rootStore, target); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[nixgg force] %s -> %s\n", target, rootStore)

	if err := promoteManifest(l, cfg, rootID, rootStore); err != nil {
		fmt.Fprintf(os.Stderr, "[nixgg force] %s: %v\n", rootID, err)
	}
	for i, child := range children {
		childStore := storePaths[i+1]
		id := thunk.ID(strings.TrimSuffix(filepath.Base(child), ".nix"))
		if err := promoteManifest(l, cfg, id, childStore); err != nil {
			fmt.Fprintf(os.Stderr, "[nixgg force] %s: %v\n", id, err)
		}
	}
	return nil
}

// CollectThunks walks import sibling refs transitively from root, and
// returns every child thunk path in a deterministic order. The root
// itself is NOT in the returned slice.
func CollectThunks(root string) ([]string, error) {
	visited := map[string]bool{root: true}
	var out []string
	var walk func(t string) error
	walk = func(t string) error {
		body, err := os.ReadFile(t)
		if err != nil {
			return nil // stale reference; ignore
		}
		dir := filepath.Dir(t)
		for _, sib := range findThunkImports(body) {
			child := filepath.Join(dir, sib)
			if visited[child] {
				continue
			}
			visited[child] = true
			if _, err := os.Stat(child); err != nil {
				continue
			}
			out = append(out, child)
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	return out, nil
}

// FindThunkImports is CollectThunks' one-hop version, exposed for callers
// that want to inspect thunk graphs (e.g. cli's --roots discovery).
func FindThunkImports(body []byte) []string { return findThunkImports(body) }

// writeHelper emits a temp .nix file that exposes the root thunk plus
// every child under numbered attribute names (root, t0, t1, …). The
// file lives next to the root thunk so absolute imports resolve.
func writeHelper(root string, children []string) (string, error) {
	var b strings.Builder
	b.WriteString("{\n")
	fmt.Fprintf(&b, "  root = import %q;\n", root)
	for i, c := range children {
		fmt.Fprintf(&b, "  t%d = import %q;\n", i, c)
	}
	b.WriteString("}\n")
	tmp, err := os.CreateTemp(filepath.Dir(root), "force-*.nix")
	if err != nil {
		return "", err
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// nixBuildAttrs runs `nix build --file <helper> <attr>...` and returns
// the printed store paths in the same order as attrs.
//
// --refresh: Nix's eval cache would otherwise return the cached
// derivation for identical thunk file bytes even when the referenced
// srcTree directory contents have changed. Our thunks reference
// staging dirs by path literal, and edits to user source files change
// the srcTree content but not the thunk file itself. --refresh forces
// Nix to re-hash the referenced paths.
func nixBuildAttrs(cfg *toolchain.Config, helper string, attrs []string) ([]string, error) {
	args := []string{"build", "-L", "--refresh", "--no-link", "--print-out-paths", "--file", helper}
	args = append(args, attrs...)
	cmd := exec.Command(cfg.Nix, args...)
	cmd.Env = append(os.Environ(),
		"NIX_REMOTE=",
		"NIX_CONFIG=experimental-features = nix-command flakes ca-derivations\nstore = "+cfg.Store+"\n",
	)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("nix build --file %s: %w", helper, err)
	}
	var paths []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line != "" {
			paths = append(paths, line)
		}
	}
	return paths, nil
}

// thunkImportRE matches `import <path>/<32-hex>.nix` — sibling thunk
// references emitted by the shim, whether the path is relative (`./`)
// or absolute (`/…/.nixgg/thunks/`). Deliberately narrow so we don't
// misfire on `import <nixpkgs>` or non-thunk imports.
var thunkImportRE = regexp.MustCompile(`import (?:\./|/[^\s;]*/)([a-f0-9]{32}\.nix)`)

func findThunkImports(body []byte) []string {
	matches := thunkImportRE.FindAllSubmatch(body, -1)
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		s := string(m[1])
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// promoteManifest reads the .nixgg/symlinks/<id> manifest and promotes
// every currently-existing caller-visible entry to point at storePath.
func promoteManifest(l paths.Layout, cfg *toolchain.Config, id thunk.ID, storePath string) error {
	manifest := filepath.Join(l.Symlinks, string(id))
	f, err := os.Open(manifest)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		sym := sc.Text()
		if sym == "" {
			continue
		}
		if _, err := os.Lstat(sym); err != nil {
			continue // user deleted the target
		}
		if err := PromoteToStore(l, cfg, id, storePath, sym); err != nil {
			fmt.Fprintf(os.Stderr, "[nixgg force] %s: %v\n", sym, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "[nixgg force] %s -> %s\n", sym, storePath)
	}
	return sc.Err()
}

// PromoteToStore re-points `target` at the on-disk copy of `storePath`.
// The realised derivation always contains a single file whose basename
// equals the caller-visible symlink's basename (the shim set outName
// that way), so we can construct the source path directly.
//
// We copy bytes into the working tree rather than symlinking, because:
//
//  1. Nix pins store-path mtimes to 1969 for reproducibility. A symlink
//     target's mtime (via stat(2), which make uses) would always look
//     older than the .c source, forcing a full rebuild every `make`.
//  2. Hardlinks avoid the mtime issue but inherit the store's 0444
//     mode, which breaks tools that expect to overwrite (some ar/ld
//     variants; the user's `chmod +x` scripts).
//
// Subdir is guessed from target's own filename via expr.ArtifactSubdir.
// That's correct for Realise's own automatic promotion (always driven
// by a Compile/Link/Archive output matching its own Kind's naming
// convention), but wrong for a link output named like a compile one
// (Linux Kbuild's vmlinux.o) — see PromoteToStoreSubdir for that case.
func PromoteToStore(l paths.Layout, cfg *toolchain.Config, thunkID thunk.ID, storePath, target string) error {
	return PromoteToStoreSubdir(l, cfg, thunkID, storePath, target, expr.ArtifactSubdir(filepath.Base(target)))
}

// PromoteToStoreSubdir is PromoteToStore with an explicit subdir,
// for a caller that already knows its artifact's real Kind instead of
// re-deriving it from the output's basename (shim.realiseAndLink).
func PromoteToStoreSubdir(l paths.Layout, cfg *toolchain.Config, thunkID thunk.ID, storePath, target, subdir string) error {
	base := filepath.Base(target)
	src := altStoreOnDisk(cfg.Store, storePath) + "/"
	if subdir != "" {
		src += subdir + "/"
	}
	src += base
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("expected %s to exist: %w", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if err := copyThroughRename(src, target); err != nil {
		return err
	}
	now := time.Now()
	if err := os.Chtimes(target, now, now); err != nil {
		return err
	}
	return thunk.RecordPromoted(l, target, thunkID, storePath)
}

// copyThroughRename copies src → target atomically via a tempfile in
// the same directory (so rename is on-fs). Removes any pre-existing
// target (which may be a symlink to a thunk, or a store copy from a
// prior build).
func copyThroughRename(src, target string) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(target)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	sf, err := os.Open(src)
	if err != nil {
		tmp.Close()
		return err
	}
	if _, err := io.Copy(tmp, sf); err != nil {
		sf.Close()
		tmp.Close()
		return err
	}
	srcInfo, err := sf.Stat()
	sf.Close()
	if err != nil {
		tmp.Close()
		return err
	}
	mode := (srcInfo.Mode() & 0o111) | 0o644
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	_ = os.Remove(target)
	return os.Rename(name, target)
}

// altStoreOnDisk resolves a canonical /nix/store/... path to its actual
// on-disk location under a `local?root=<path>` store URL.
func altStoreOnDisk(storeURL, canonical string) string {
	const prefix = "local?root="
	if strings.HasPrefix(storeURL, prefix) {
		return strings.TrimPrefix(storeURL, prefix) + canonical
	}
	return canonical
}
