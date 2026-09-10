package shim

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// A GNU "thin" archive (`ar` with the T modifier) stores PATHS TO its
// members, not their bytes, and build systems nest them.
//
// That is fine while every member path stays resolvable: a modelled
// sub-archive records its members as absolute store paths, which resolve
// anywhere. It breaks for a sub-archive nixgg did NOT model, because
// classifyInputs stores that file ALONE — its siblings are not beside
// it, so when the parent `ar` flattens the nested archive every member
// resolves to nothing and is dropped. Silently: ar reports no error, the
// archive is just smaller, and the damage only surfaces at the final
// link as undefined references.
//
// So a thin archive cannot be treated as an opaque file. Expand it into
// its members and depend on those — see classifyInputs.
const thinMagic = "!<thin>\n"

// arHeaderSize is the fixed size of an ar member header.
const arHeaderSize = 60

// thinArchiveMembers parses a GNU thin archive and returns its member
// paths, resolved against the archive's own directory (thin archives
// record relative paths that way).
//
// Three outcomes, and callers must tell them apart:
//
//   - isThin=false           not a thin archive (a normal archive, or
//     not an archive at all). Safe to store whole: it carries bytes.
//   - isThin=true, ok=false  the magic says thin, but the member table
//     is malformed or truncated. Storing this file is the WORST
//     outcome — it is a path list, so its members silently vanish and
//     the damage surfaces at the final link. Callers must bail to
//     passthrough instead.
//   - isThin=true, ok=true   members parsed; depend on those.
//
// Parsed here rather than shelled out to `ar t`: ar wants to resolve the
// members it lists, so on exactly the broken archives this exists to fix
// it fails with a message naming the ARCHIVE rather than the member,
// which is worse than useless as a diagnostic.
func thinArchiveMembers(path string) (members []string, isThin bool, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, false
	}
	defer f.Close()

	// ReadFull, not Read: a short read is not an error, so a file
	// shorter than the magic would compare unequal and be reported as
	// "not thin" — the one answer that gets it stored whole.
	magic := make([]byte, len(thinMagic))
	if _, err := io.ReadFull(f, magic); err != nil || string(magic) != thinMagic {
		return nil, false, false
	}

	// Past here the file IS thin, so every failure below reports
	// isThin=true and lets the caller bail rather than store it.
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, true, false
	}
	body = body[len(thinMagic):]

	dir := filepath.Dir(path)
	var longNames []byte

	for len(body) >= arHeaderSize {
		hdr := body[:arHeaderSize]
		name := strings.TrimRight(string(hdr[0:16]), " ")
		size, err := strconv.Atoi(strings.TrimSpace(string(hdr[48:58])))
		if err != nil || size < 0 {
			break
		}
		body = body[arHeaderSize:]

		switch {
		case name == "//":
			// The long-name table: the only member of a thin archive
			// that actually carries data, alongside the symbol index.
			// Guard the PADDED length: ar pads members to an even
			// boundary, so an odd size that exactly fills body means the
			// pad byte is missing and the archive is truncated. Guarding
			// only `size` would slice past the end and panic.
			if size+size%2 > len(body) {
				return nil, true, false
			}
			longNames = body[:size]
			body = body[size+size%2:]
			continue
		case name == "/" || name == "/SYM64/":
			// Symbol index, also carries data. Same padded-length guard.
			if size+size%2 > len(body) {
				return nil, true, false
			}
			body = body[size+size%2:]
			continue
		}

		// Every other member of a thin archive is a reference: the
		// header records the size the file HAS, but no data follows it.
		var member string
		if strings.HasPrefix(name, "/") {
			off, err := strconv.Atoi(name[1:])
			if err != nil || off < 0 || off >= len(longNames) {
				continue
			}
			member = readLongName(longNames[off:])
		} else {
			member = strings.TrimSuffix(name, "/")
		}
		if member == "" {
			continue
		}
		if !filepath.IsAbs(member) {
			member = filepath.Join(dir, member)
		}
		members = append(members, member)
	}
	return members, true, true
}

// readLongName pulls one entry out of the "//" table, which terminates
// names with "/\n" (or just "\n" from some producers).
func readLongName(b []byte) string {
	if i := bytes.IndexAny(b, "\n"); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSuffix(string(b), "/")
}
