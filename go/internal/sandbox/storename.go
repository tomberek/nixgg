package sandbox

import "strings"

// StoreName makes a store-object name out of a filename.
//
// Nix restricts store path names to [A-Za-z0-9+._?=-] and rejects
// anything else outright. Naming a store object after the file it holds
// puts arbitrary source filenames through that check, and real projects
// have filenames Nix will not accept.
//
// Renaming is safe: the name is cosmetic here, since a staged tree
// references the object by its own path and identity comes from the
// content hash. Two filenames can map to the same name, which only
// matters if their content is identical too — in which case sharing one
// object is correct.
func StoreName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '+', r == '-', r == '.', r == '_', r == '?', r == '=':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	// Nix caps names at 211 chars. Trim from the FRONT so the extension
	// survives — it is the part that carries meaning when reading a log.
	const max = 200
	if len(s) > max {
		s = s[len(s)-max:]
	}
	// A leading dot is rejected, and "." / ".." are not names at all.
	// Trim AFTER the length clamp: truncating can expose a dot that was
	// interior in the full string, which would put a name Nix rejects
	// back on the wire.
	s = strings.TrimLeft(s, ".")
	if s == "" {
		s = "unnamed"
	}
	return s
}
