// Package assemble walks a build tree left behind by a splitStdenv
// build-stage buildPhase and finds every drvref stub the nixgg shims
// wrote in place of a real artifact.
//
// splitStdenv's build stage (unlike mkNixggBuild) has no single
// target — the tree can have dozens of shimmed outputs — so stubs are
// discovered by walking, not by argument parsing.
package assemble

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/tbereknyei/nixgg/internal/drvref"
)

// Stub is one discovered drvref stub: its path relative to the walked
// root, and the drv that will produce its real content.
type Stub struct {
	// RelPath is slash-separated and relative to root, e.g.
	// "src/hello.o" — never absolute.
	RelPath string
	// DrvPath is the full /nix/store/<hash>-<name>.drv path recorded
	// in the stub — see drvref.Path.
	DrvPath string
}

// skipNames are sandbox-infrastructure entries that are never real
// build output. ".nix-socket" is builder-rpc-v0's own unix socket —
// `nix store add --scan` can't ingest a socket. ".gg-stage" is
// StageForScan's own working directory.
//
// ".nixgg" is nixgg's own scratch dir — staged source trees, thunks and
// memo caches. It is scaffolding, never output, and capturing it is
// expensive: `nix store add --scan` records a reference for every store
// path it finds inside, which drags the whole staging closure into the
// captured tree.
//
// Excluding it is safe. The final stage re-runs the build under the
// shims and recreates what it needs; staged sources are already in the
// store as derivation inputs; and sandbox mode marks outputs with
// drvref stub FILES, so nothing in the tree points into .nixgg.
var skipNames = map[string]bool{
	".nix-socket": true,
	".gg-stage":   true,
	".nixgg":      true,
}

// Walk finds every drvref stub under root, in deterministic
// (lexical) order so the resulting JSON drv hash doesn't depend on
// directory-read ordering.
func Walk(root string) ([]Stub, error) {
	var stubs []Stub
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skipNames[d.Name()] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		ref := drvref.Path(path)
		if ref == "" {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		stubs = append(stubs, Stub{RelPath: filepath.ToSlash(rel), DrvPath: ref})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return stubs, nil
}

// StageForScan copies root into a fresh directory INSIDE root itself
// (".gg-stage"), excluding skipNames entries, and returns the staged
// path.
//
// Must be inside root, not under an os.MkdirTemp("", ...) dest:
// $TMPDIR inside a builder-rpc-v0 sandbox resolves under root, so an
// externally-supplied dest could itself be a descendant of root,
// making the copy recurse into itself. A fixed, excluded name directly
// under root can't be an ancestor of root, so this can't happen.
//
// `nix store add --scan` also can't ingest root directly: it leaves a
// live .nix-socket (NIX_REMOTE points at it) that --scan rejects, and
// that socket can't be deleted first — the caller's own subsequent
// nix store add/derivation add/submit-output calls go through it.
func StageForScan(root string) (string, error) {
	staged := filepath.Join(root, ".gg-stage")
	if err := os.MkdirAll(staged, 0o755); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if skipNames[e.Name()] {
			continue
		}
		if err := copyRecursive(filepath.Join(root, e.Name()), filepath.Join(staged, e.Name())); err != nil {
			return "", err
		}
	}
	return staged, nil
}

// copyRecursive copies a file, directory, or symlink from src to dst,
// preserving symlinks (SONAME alias chains like libfoo.so ->
// libfoo.so.1.2.3 must stay symlinks, not become copies).
func copyRecursive(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	case info.IsDir():
		// Owner-writable regardless of src's own mode: src may be an
		// untouched subtree still carrying its Nix-store read-only
		// bits (e.g. an unmodified cmake/ dir under a build's source
		// tree), and MkdirAll below would otherwise create dst with
		// that same read-only mode — then the recursive copyRecursive
		// calls a few lines down can't create any entries inside it.
		// dst is disposable scratch space consumed only by `nix store
		// add --scan`, which assigns its own final store permissions,
		// so the exact mode copied here doesn't matter beyond letting
		// this function populate it.
		if err := os.MkdirAll(dst, info.Mode().Perm()|0o200); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			// skipNames applies at EVERY depth, not just the root: the
			// scratch dir sits at the project root paths.Resolve chose,
			// which is usually several levels down. Walk already skips by
			// name at any depth, so without this the two halves of the
			// assembly disagree about what counts as build output.
			if skipNames[e.Name()] {
				continue
			}
			if err := copyRecursive(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	case drvref.Is(src):
		// A drvref stub's whole content is a /nix/store/….drv path, and
		// `nix store add --scan` records every store path it finds. So
		// copying stubs verbatim makes the captured tree REFERENCE each
		// producing derivation, and a derivation's closure includes its
		// own inputs — for a compile, its whole staged source tree.
		// Nix then bind-mounts that closure into every derivation that
		// consumes the tree.
		//
		// Blanking them is safe because of ordering, not luck:
		// cmdAssemble calls Walk on the ORIGINAL root and already holds
		// every stub's drv path before StageForScan runs. The staged
		// copy needs only the tree's SHAPE, and the assembly overlays
		// the real artifact over each stub anyway.
		//
		// Keep an empty file rather than skipping: recipes stat their
		// outputs, and the overlay's `cp -a` wants the path to exist.
		return os.WriteFile(dst, nil, info.Mode().Perm())
	default:
		return copyFile(src, dst, info.Mode().Perm())
	}
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
