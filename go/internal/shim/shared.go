package shim

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tbereknyei/nixgg/internal/sandbox"
	"github.com/tbereknyei/nixgg/internal/toolchain"
)

// sharedStagingEnabled reports whether staged trees should be symlink
// farms into per-file store objects rather than hardlinked copies.
//
// Gated while the approach is being proven. The decisive unknown is
// whether `nix store add --scan` records a symlink TARGET as a
// reference: if it only scans file contents, the header objects never
// become inputs of the staged tree, so they are not mounted in the
// inner build sandbox and every compile fails on a missing header.
func sharedStagingEnabled() bool {
	return os.Getenv("NIXGG_SHARED_STAGE") == "1"
}

// storeShared puts one file in the store and returns its path,
// memoised so a header included by thousands of translation units is
// added once per build.
//
// The memo is a directory of one file per key rather than a single
// index: shims run concurrently under `make -j`, and one-file-per-key
// with an atomic rename needs no locking.
//
// Keyed on path+mtime+size rather than content. Hashing every header of
// every TU would be millions of hashes; within a single build the
// inputs do not change underneath us, and `nix store add` hashes the
// content anyway to decide the store path — so identical content still
// collapses to one object even when two different paths hold it.
func storeShared(cfg *toolchain.Config, path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	key := fmt.Sprintf("%s|%d|%d", abs, st.ModTime().UnixNano(), st.Size())
	sum := sha256.Sum256([]byte(key))
	name := hex.EncodeToString(sum[:16])

	// Under .nixgg/ so the assembly never captures it — see scratch.go.
	dir, dirErr := scratchDir("shared")
	cache := ""
	if dirErr == nil {
		cache = filepath.Join(dir, name)
		if b, err := os.ReadFile(cache); err == nil {
			if sp := strings.TrimSpace(string(b)); sp != "" {
				return sp, nil
			}
		}
	}

	sp, err := sandbox.StoreAddScan(cfg, filepath.Base(abs), abs)
	if err != nil {
		return "", err
	}
	if cache != "" {
		// os.CreateTemp, not a fixed cache+".tmp": every shim racing for
		// the same header computes the SAME cache path, and os.WriteFile
		// opens O_TRUNC. Two concurrent writers would share one temp file
		// — B truncates while A is mid-write, then A renames the short
		// result into place. A later reader passes the non-empty guard
		// above and stages a symlink to a truncated store path. Same
		// hazard, same fix as storeAddTool in objtool.go.
		if tmp, err := os.CreateTemp(filepath.Dir(cache), ".shared-*"); err == nil {
			_, werr := tmp.WriteString(sp)
			cerr := tmp.Close()
			if werr == nil && cerr == nil {
				_ = os.Rename(tmp.Name(), cache) // atomic; losing a race is harmless
			} else {
				_ = os.Remove(tmp.Name())
			}
		}
	}
	return sp, nil
}
