// Package assemble walks a build tree left behind by a splitStdenv
// build-stage buildPhase and finds every drvref stub the nixgg shims
// wrote in place of a real artifact. splitStdenv's build stage (unlike
// mkNixggBuild) has no single target — stubs are discovered by
// walking, not by argument parsing.
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
// StageForScan's own working directory. ".nixgg" is nixgg's own
// scratch (staged source trees, thunks, memo caches) — capturing it
// drags every store path referenced by a memo file's own content into
// the captured tree's closure, which at kernel scale exceeds the
// sandbox's mount limit ("bind mount ... failed: No space left on
// device", naming neither the cache nor the assembly).
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
// Must be inside root, not an os.MkdirTemp("", ...) dest: $TMPDIR
// inside a builder-rpc-v0 sandbox resolves under root, so an
// externally-supplied dest could itself be a descendant of root,
// making the copy recurse into itself. A fixed, excluded name directly
// under root can't be an ancestor of root, so that can't happen.
//
// `nix store add --scan` can't ingest root directly either: it leaves
// a live .nix-socket (NIX_REMOTE points at it) that --scan rejects,
// and the socket can't be deleted first since the caller's own
// subsequent store calls go through it.
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
		// Force owner-writable regardless of src's mode: src may still
		// carry Nix-store read-only bits, and MkdirAll would otherwise
		// create dst read-only too, blocking the recursive copies into
		// it below. dst is disposable scratch for `nix store add
		// --scan`, which assigns its own final permissions.
		if err := os.MkdirAll(dst, info.Mode().Perm()|0o200); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			// skipNames applies at every depth, not just root:
			// StageForScan's own top-level loop already filters, but a
			// scratch dir named .nixgg can appear several levels down
			// wherever paths.Resolve rooted it.
			if skipNames[e.Name()] {
				continue
			}
			if err := copyRecursive(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
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
