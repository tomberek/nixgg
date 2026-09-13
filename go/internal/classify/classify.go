// Package classify inspects a compile input symlink and reports what
// kind of nixgg artifact (if any) it references.
//
// A caller-visible target is one of:
//   - Store    → symlink resolves to /nix/store/…-tu-foo.o/…
//     (already realised; use the store path as a reference)
//   - Thunk    → symlink resolves to .../thunks/<id>.nix
//     (not yet realised; the link/ar thunk `import`s it)
//   - Drv      → symlink resolves to /nix/store/…-tu-foo.o.drv
//     (sandbox-mode: not yet realised; the link/ar drv
//     references it via inputs.drvs)
//   - Regular  → real file or symlink to something nixgg doesn't own
//     (passthrough — link/ar can't include it in a thunk)
//   - Absent   → the path doesn't exist at all
package classify

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/tbereknyei/nixgg/internal/drvref"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/thunk"
)

// Kind labels a classification outcome.
type Kind int

const (
	Absent Kind = iota
	Regular
	Store
	Thunk
	Drv
)

func (k Kind) String() string {
	switch k {
	case Absent:
		return "absent"
	case Regular:
		return "regular"
	case Store:
		return "store"
	case Thunk:
		return "thunk"
	case Drv:
		return "drv"
	}
	return "?"
}

// Result carries the classification plus a reference that means
// different things per kind:
//   - Store: canonical /nix/store/<hash>-<name> root (alt-store prefix
//     stripped so the reference is portable).
//   - Thunk: absolute path to the .nix thunk file.
//   - Regular / Absent: empty.
type Result struct {
	Kind Kind
	Ref  string
	// Sub is the target's path relative to Ref, set when Kind == Store
	// and the target lives below the store root rather than directly
	// inside it (Ref=/nix/store/…-zlib, Sub="lib/libz.so"). Empty when
	// the target is Ref/<basename>. Ref alone is what builtins.storePath
	// and inputs.srcs accept (a root, not an arbitrary subpath); use
	// ArgvPath to reconstruct the full file path.
	Sub string
	// Err is set when Kind fell back to Regular because Lstat/readlink
	// failed (EACCES, ELOOP), not because the file is genuinely an
	// ordinary file nixgg doesn't own. Both cases passthrough the same
	// way, but the diagnostic needs to say which happened.
	Err error
	// ThunkID is set when Kind == Store AND we know which thunk file
	// produced this output (only populated for promoted regular files
	// today — a real symlink → /nix/store/... doesn't carry that
	// association). Empty otherwise.
	ThunkID string
}

// ArgvPath returns the path that belongs on a compiler/linker command
// line for a Store result. Falls back to Ref/<name> when Sub is empty.
func (r Result) ArgvPath(name string) string {
	if r.Sub != "" {
		return r.Ref + "/" + r.Sub
	}
	return r.Ref + "/" + name
}

// Reason describes why this classification means "nixgg can't model
// this input", for use in a passthrough diagnostic. It distinguishes a
// file nixgg simply doesn't own from one it could not inspect.
func (r Result) Reason() string {
	if r.Err != nil {
		return "stat failed: " + r.Err.Error()
	}
	return r.Kind.String()
}

// Target classifies a single path.
//
// altStorePrefix is the on-disk root of the alt store (e.g.
// "/tmp/nixgg-store"), or "" for a system store; stripped from store
// paths so the reference is always canonical /nix/store/....
//
// l is consulted for the "promoted" registry (regular-file targets
// force copied from a store output, rather than symlinked, so make's
// mtime check works). Pass a zero-value Layout to skip that check.
func Target(path, altStorePrefix string, l paths.Layout) Result {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Result{Kind: Absent}
		}
		return Result{Kind: Regular, Err: err}
	}
	if info.Mode()&os.ModeSymlink == 0 {
		// Regular file, not a symlink. Could be a sandbox-mode drvref
		// stub, a promoted store output, a foreign dependency living
		// directly under /nix/store/ (meson/CMake sometimes emit an
		// absolute store path as a literal link argument instead of a
		// symlink — QEMU's libz.a did this), or a genuinely regular file.
		if ref := drvref.Path(path); ref != "" {
			return Result{Kind: Drv, Ref: ref}
		}
		if l.Promoted != "" {
			if info := thunk.LookupPromoted(l, path); info != nil {
				return Result{Kind: Store, Ref: info.StorePath, ThunkID: string(info.ThunkID)}
			}
		}
		canonical := path
		if altStorePrefix != "" && strings.HasPrefix(path, altStorePrefix+"/nix/store/") {
			canonical = strings.TrimPrefix(path, altStorePrefix)
		}
		if strings.HasPrefix(canonical, "/nix/store/") {
			root, sub := splitStorePath(canonical)
			return Result{Kind: Store, Ref: root, Sub: sub}
		}
		return Result{Kind: Regular}
	}

	dest, err := readlinkFollow(path)
	if err != nil {
		return Result{Kind: Regular, Err: err}
	}

	canonical := dest
	if altStorePrefix != "" && strings.HasPrefix(dest, altStorePrefix+"/nix/store/") {
		canonical = strings.TrimPrefix(dest, altStorePrefix)
	}
	// Check .drv before the generic /nix/store/ branch below, since .drv
	// paths are themselves under /nix/store/.
	if strings.HasPrefix(canonical, "/nix/store/") && strings.HasSuffix(canonical, ".drv") {
		return Result{Kind: Drv, Ref: canonical}
	}
	if strings.HasPrefix(canonical, "/nix/store/") {
		root, sub := splitStorePath(canonical)
		return Result{Kind: Store, Ref: root, Sub: sub}
	}
	if strings.HasSuffix(dest, ".nix") {
		return Result{Kind: Thunk, Ref: dest}
	}
	// The symlink resolved to one of our own drvref stubs rather than
	// into the store — a SONAME alias chain (libfoo.so -> libfoo.so.1.2.3)
	// pointing at the stub nixgg wrote for the real link output.
	if ref := drvref.Path(dest); ref != "" {
		// Sub must be the drv's real output basename (dest's own
		// basename, e.g. "libcrypto.so.3"), not the alias name
		// ("libcrypto.so") — otherwise the emitted link line reaches
		// for a file the drv never produced. Confirmed against
		// openssl's engines/*.so linking through its own
		// `ln -s libcrypto.so.3 libcrypto.so` alias.
		return Result{Kind: Drv, Ref: ref, Sub: filepath.Base(dest)}
	}
	return Result{Kind: Regular}
}

// splitStorePath splits a store path into its /nix/store/<hash>-<name>
// root (what builtins.storePath/inputs.srcs accept) and the remainder
// below it: ("…-zlib", "lib/libz.so") for "…-zlib/lib/libz.so". sub is
// "" when p is itself the root.
func splitStorePath(p string) (root, sub string) {
	rest := strings.TrimPrefix(p, "/nix/store/")
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		return "/nix/store/" + rest[:slash], rest[slash+1:]
	}
	return "/nix/store/" + rest, ""
}

// readlinkFollow returns the final resolved target. We use EvalSymlinks
// because a symlink can chain (e.g. output → thunk → nothing yet). If
// the chain leads to a nonexistent path we fall back to the raw target.
func readlinkFollow(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	return os.Readlink(path)
}
