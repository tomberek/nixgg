// Package batchmember defines the on-disk record a compile shim writes
// when a TU is deferred into an opt-in batch group (see internal/batch)
// instead of submitted as its own derivation.
//
// A record snapshots everything internal/expr.CompileParams (native) or
// internal/expr.CompileJSONParams (sandbox) needs to build that TU's
// compile step later — either standalone (internal/shim.resolvePendingMember)
// or as one member of a combined batch-archive derivation
// (internal/shim.tryBatchArchive).
//
// Written via temp-file-then-rename to
// .nixgg/batches/<group>/<sha1(absOutputPath)>.json, keyed by the
// compile's ABSOLUTE caller-visible output path rather than a
// project-relative one: internal/scan recomputes ProjectRoot per compile
// call, so the same logical file can resolve to different relative paths
// depending on which directory `make` happened to be in. Hashing the
// absolute path lets a reader (ar's own argv, resolved via filepath.Abs)
// reproduce the same key independently, with no shared index.
package batchmember

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tbereknyei/nixgg/internal/paths"
)

// MemberRecord snapshots one deferred compile.
type MemberRecord struct {
	Group   string
	Tool    string // "cc", "gcc", "c++", "g++"
	Source  string // relative path inside the src tree, e.g. "sds.c"
	OutName string // "sds.o"

	Flags      []string
	StoreDeps  []string
	WrapperEnv map[string]string

	// Exactly one of these is populated, matching whichever mode this
	// build is running under — a build never mixes native and sandbox
	// mode, so a record never needs both.
	SrcTreeLiteral string `json:",omitempty"` // native: Nix path literal, e.g. "../srcs/foo"
	SrcStore       string `json:",omitempty"` // sandbox: full /nix/store/... path, already
	// uploaded via sandbox.StoreAddDirectory at compile time (deferring
	// only derivation submission, not staging/upload).
}

// Key returns the filename (not a full path) a record for the given
// absolute output path is stored under: sha1(absOutput), hex-encoded.
// Exported so callers that only have the output path (e.g. ar's own argv,
// resolved via filepath.Abs) can compute the same key independently.
func Key(absOutput string) string {
	h := sha1.Sum([]byte(absOutput))
	return hex.EncodeToString(h[:])
}

// path returns the record's on-disk location for a given group and
// absolute output path.
func path(l paths.Layout, group, absOutput string) string {
	return filepath.Join(l.Batches, group, Key(absOutput)+".json")
}

// Write persists m at its canonical location (derived from group and
// absOutput) and returns that path. Uses temp-file-then-rename so a
// half-written file is never observed by a concurrent reader. There is no
// dedup check: each compile's absolute output path is already unique
// within a build, so an unconditional write is always correct here.
func Write(l paths.Layout, group, absOutput string, m MemberRecord) (string, error) {
	dir := filepath.Join(l.Batches, group)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	body, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("batchmember: encode record: %w", err)
	}
	dst := path(l, group, absOutput)
	tmp, err := os.CreateTemp(dir, Key(absOutput)+".tmp.*")
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

// Read parses a record previously written by Write, given its full
// path (as recorded in a batchpending stub).
func Read(recordPath string) (MemberRecord, error) {
	body, err := os.ReadFile(recordPath)
	if err != nil {
		return MemberRecord{}, fmt.Errorf("batchmember: read %s: %w", recordPath, err)
	}
	var m MemberRecord
	if err := json.Unmarshal(body, &m); err != nil {
		return MemberRecord{}, fmt.Errorf("batchmember: decode %s: %w", recordPath, err)
	}
	return m, nil
}
