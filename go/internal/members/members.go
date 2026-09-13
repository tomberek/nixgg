// Package members defines the on-disk sidecar an archive shim writes for a
// THIN archive (`ar --thin`/`T`): since a thin archive's `.a` file stores
// only its members' paths, not their bytes, each member must also be
// declared as an input of any derivation that consumes the archive, or Nix
// won't mount it into that consumer's sandbox.
//
// This is safe because every member path nixgg ever hands to `ar` is
// already a permanent /nix/store/... path (or a CA placeholder Nix
// substitutes before the build script runs — see
// expr/derivation.go's KindArchive case), so those references stay valid
// from any sandbox for as long as the referenced store path exists. Only a
// RELATIVE member path would be fragile (GNU ar resolves it against the
// archive's own location, not cwd) — nixgg never produces one.
//
// This is a separate sidecar rather than an extension of internal/drvref
// because drvref's wire format is bounded to ~4096 bytes and a thin archive
// can have hundreds of members (QEMU's libqemuutil.a has ~280).
//
// Written to .nixgg/members/<key>.json, where <key> is the same id the
// archive's own thunk/drv path already produces, so a consumer can compute
// the same lookup key with no extra state to thread through.
package members

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tbereknyei/nixgg/internal/paths"
)

// Record is one archive member, in the same {Kind, Ref, Name} shape as
// expr.Input/expr.JSONDrvInput so a caller can convert directly with no
// translation layer.
type Record struct {
	Kind string
	Ref  string
	Name string
}

func path(l paths.Layout, key string) string {
	return filepath.Join(l.Members, key+".json")
}

// Write persists recs as the member list for the thin archive keyed by
// key, using temp-file-then-rename so a half-written file is never
// observed by a concurrent reader.
func Write(l paths.Layout, key string, recs []Record) (string, error) {
	if err := os.MkdirAll(l.Members, 0o755); err != nil {
		return "", err
	}
	body, err := json.Marshal(recs)
	if err != nil {
		return "", fmt.Errorf("members: encode records: %w", err)
	}
	dst := path(l, key)
	tmp, err := os.CreateTemp(l.Members, key+".tmp.*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// Read looks up the member list for key. ok is false (with a nil error)
// iff no sidecar exists for key — the normal case for any archive that
// isn't thin.
func Read(l paths.Layout, key string) (recs []Record, ok bool, err error) {
	body, err := os.ReadFile(path(l, key))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("members: read %s: %w", key, err)
	}
	if err := json.Unmarshal(body, &recs); err != nil {
		return nil, false, fmt.Errorf("members: decode %s: %w", key, err)
	}
	return recs, true, nil
}
