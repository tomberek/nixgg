// Package drvref defines the on-disk marker that sandbox mode writes at
// a caller-visible output path in place of the artifact itself.
//
// This is a wire format shared by a writer and two readers, so it gets
// its own leaf package rather than living next to any one of them:
//
//	internal/sandbox   PointOutputAtDrv  — writes the stub
//	internal/classify  Target            — reads it to classify Kind.Drv
//	internal/shim      resolveLibFlag    — reads it to claim a -l archive
//
// # Why a file and not a symlink
//
// builder-rpc-v0 registers .drv files with the daemon but does not
// materialise them into the sandbox filesystem. A symlink to
// /nix/store/….drv would therefore dangle, and dangling is fatal for
// downstream Makefile prerequisite checks — mosh's
// `mosh-client: ../crypto/libmoshcrypto.a` runs a shell-level `test -e`
// that a dangling symlink fails. A small regular file passes `test -e`
// while still telling our own shims which drv produced it.
//
// # Format
//
//	#!nixgg-drvref\n
//	/nix/store/<hash>-<name>.drv\n
//
// The `#!` opening is deliberate: if something mistakes the stub for an
// executable, the error names this file rather than being a mystery.
package drvref

import (
	"os"
	"strings"
)

// Header is the magic first line of every drvref stub, newline included.
const Header = "#!nixgg-drvref\n"

// Body renders a complete stub for the given drv store path.
func Body(drvPath string) string {
	return Header + drvPath + "\n"
}

// maxSize bounds how much of a file we will read while looking for the
// header. Stubs are two short lines; anything larger is a real artifact
// and we should not slurp it.
const maxSize = 4096

// Path returns the drv store path recorded in the stub at `path`, or ""
// if `path` is not a drvref stub. Safe to call on any file: a real
// object or archive simply returns "".
func Path(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, maxSize)
	n, _ := f.Read(buf)
	body := string(buf[:n])
	if !strings.HasPrefix(body, Header) {
		return ""
	}
	rest := body[len(Header):]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[:nl]
	}
	return rest
}

// Is reports whether `path` is a drvref stub. Equivalent to
// Path(path) != "" but clearer at call sites that only need the boolean.
func Is(path string) bool { return Path(path) != "" }
