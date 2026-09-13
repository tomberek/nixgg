// Package thunk writes Nix expression files to .nixgg/thunks/<id>.nix
// and records the caller-visible symlinks that reference each one.
//
// A "thunk" is any Nix expression produced by a shim: compile, link,
// or archive. Its ID is sha256(expression body) truncated to 32 chars,
// so identical inputs collapse to the same file.
package thunk

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tbereknyei/nixgg/internal/paths"
)

// ID is a 32-char lowercase hex hash of the expression body.
type ID string

// Compute derives an ID from an expression body. Same bytes → same ID.
func Compute(expr string) ID {
	sum := sha256.Sum256([]byte(expr))
	return ID(hex.EncodeToString(sum[:16]))
}

// Path returns the absolute file path for this thunk under the layout.
func (id ID) Path(l paths.Layout) string {
	return filepath.Join(l.Thunks, string(id)+".nix")
}

// Write persists the expression at .nixgg/thunks/<id>.nix if it doesn't
// already exist. Idempotent: same content is never rewritten.
//
// Uses tmp+rename so concurrent shim invocations that produce the same
// thunk don't race producing a half-written file.
func Write(l paths.Layout, id ID, expr string) (string, error) {
	if err := os.MkdirAll(l.Thunks, 0o755); err != nil {
		return "", err
	}
	dst := id.Path(l)
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	tmp, err := os.CreateTemp(l.Thunks, string(id)+".tmp.*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	// On any error path, best-effort remove the tmp file.
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(expr); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	// Rename is atomic within a filesystem; on races the second write
	// loses to the first, but content is identical so it doesn't matter.
	if err := os.Rename(tmpName, dst); err != nil {
		// If dst appeared under us (another writer won the race), that's
		// fine — bytes match by construction.
		if _, statErr := os.Stat(dst); statErr == nil {
			return dst, nil
		}
		return "", err
	}
	return dst, nil
}

// LinkPlaceholder replaces `output` with a symlink pointing at the
// thunk file, creating the parent dir if missing.
//
// Also bumps the thunk file's mtime to now, even when Write reused a
// byte-identical file. make follows symlinks with stat(2), so if the
// thunk's mtime never changes, downstream targets look up-to-date
// relative to it and make skips the step that should have re-run.
//
// Also drops any stale "promoted" registry entry for output: the shim
// is authoritative about what a file is now, and a re-shim invalidates
// the last "promoted from store" record for this path.
func LinkPlaceholder(l paths.Layout, output, thunkPath string) error {
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	// os.Symlink refuses to overwrite; a stale target from a previous
	// build is expected here.
	_ = os.Remove(output)
	if err := os.Symlink(thunkPath, output); err != nil {
		return fmt.Errorf("symlink %s -> %s: %w", output, thunkPath, err)
	}
	now := time.Now()
	_ = os.Chtimes(thunkPath, now, now)
	if abs, err := filepath.Abs(output); err == nil {
		_ = os.Remove(filepath.Join(l.Promoted, promotedKey(abs)))
	}
	return nil
}

// PromotedInfo records what a caller-visible regular file was
// produced from: which thunk (so force can re-evaluate) and which
// store path (so link/ar shims can reference it as builtins.storePath).
type PromotedInfo struct {
	ThunkID   ID
	StorePath string
}

// RecordPromoted records "target file was produced by realising thunkID
// which built to storePath". Written by force after promoting a thunk
// symlink into a real byte-copy.
//
// Layout: .nixgg/promoted/<sha1(abs-target)>. Two lines:
//
//	<thunk-id>
//	<store-path>
//
// Small enough that we don't bother with a shared index — one file per
// promoted target, keyed by the target's absolute path hash.
func RecordPromoted(l paths.Layout, target string, thunkID ID, storePath string) error {
	if err := os.MkdirAll(l.Promoted, 0o755); err != nil {
		return err
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	key := promotedKey(abs)
	dst := filepath.Join(l.Promoted, key)
	body := fmt.Sprintf("%s\n%s\n", thunkID, storePath)
	return os.WriteFile(dst, []byte(body), 0o644)
}

// LookupPromoted returns the recorded info for a target, or nil.
func LookupPromoted(l paths.Layout, target string) *PromotedInfo {
	abs, err := filepath.Abs(target)
	if err != nil {
		return nil
	}
	body, err := os.ReadFile(filepath.Join(l.Promoted, promotedKey(abs)))
	if err != nil {
		return nil
	}
	lines := strings.SplitN(strings.TrimSpace(string(body)), "\n", 2)
	if len(lines) != 2 {
		return nil
	}
	return &PromotedInfo{ThunkID: ID(lines[0]), StorePath: lines[1]}
}

// promotedKey is sha1 of the abs path — short, filesystem-safe,
// collision-resistant enough for a local cache.
func promotedKey(abs string) string {
	h := sha1.Sum([]byte(abs))
	return hex.EncodeToString(h[:])
}

// RecordSymlink appends the caller-visible symlink path to the manifest
// under .nixgg/symlinks/<id>. Idempotent (skips exact-match duplicates).
func RecordSymlink(l paths.Layout, id ID, output string) error {
	if err := os.MkdirAll(l.Symlinks, 0o755); err != nil {
		return err
	}
	abs, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	manifest := filepath.Join(l.Symlinks, string(id))
	// Cheap dedup: read + scan. Manifests are small (a few dozen lines
	// max in practice) so this doesn't cost.
	if existing, err := os.ReadFile(manifest); err == nil {
		off := 0
		for off < len(existing) {
			end := off
			for end < len(existing) && existing[end] != '\n' {
				end++
			}
			if string(existing[off:end]) == abs {
				return nil // already recorded
			}
			off = end + 1
		}
	}
	f, err := os.OpenFile(manifest, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, abs)
	return err
}
