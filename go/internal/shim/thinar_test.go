package shim

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Built with the real `ar`, not a hand-rolled fixture: the whole point
// is to parse what GNU ar actually emits, including its long-name table.
func buildThinArchive(t *testing.T, dir string, members ...string) string {
	t.Helper()
	arBin := arPath(t)
	var rels []string
	for _, m := range members {
		p := filepath.Join(dir, m)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("\x7fELF fake "+m), 0o644); err != nil {
			t.Fatal(err)
		}
		rels = append(rels, m)
	}
	archive := filepath.Join(dir, "built-in.a")
	// The modifier set build systems use for aggregate archives.
	cmd := exec.Command(arBin, append([]string{"cDPrST", "built-in.a"}, rels...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ar failed: %v\n%s", err, out)
	}
	return archive
}

// A thin archive's members must be recoverable, because
// storing the archive file alone loses them and the parent `ar` then
// drops them without error. Long member paths exercise the "//"
// long-name table, which is where a naive parser breaks.
func TestThinArchiveMembers(t *testing.T) {
	dir := t.TempDir()
	archive := buildThinArchive(t, dir,
		"init.o",
		"rmpiggy.o",
		"some/deeply/nested/path/that/needs/the/long/name/table/object.o",
	)

	members, isThin, ok := thinArchiveMembers(archive)
	if !isThin {
		t.Fatal("real `ar cDPrST` output was not recognised as thin")
	}
	if !ok {
		t.Fatal("real `ar cDPrST` output was not recognised as a thin archive")
	}
	if len(members) != 3 {
		t.Fatalf("got %d members, want 3: %q", len(members), members)
	}
	// Paths must be resolved against the archive's directory, or the
	// caller cannot store-add them.
	for _, m := range members {
		if !filepath.IsAbs(m) {
			t.Errorf("member %q is not absolute; caller cannot open it", m)
		}
		if _, err := os.Stat(m); err != nil {
			t.Errorf("member %q does not resolve: %v", m, err)
		}
	}
}

// A normal (fat) archive carries its members' bytes, so it needs no
// expansion — and must not be mistaken for a thin one.
func TestThinArchiveMembersRejectsFatArchive(t *testing.T) {
	arBin := arPath(t)
	dir := t.TempDir()
	obj := filepath.Join(dir, "a.o")
	if err := os.WriteFile(obj, []byte("\x7fELF fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "fat.a")
	cmd := exec.Command(arBin, "crs", "fat.a", "a.o")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ar failed: %v\n%s", err, out)
	}
	if _, isThin, _ := thinArchiveMembers(archive); isThin {
		t.Error("a fat archive was reported as thin; expanding it would be wrong")
	}
}

func TestThinArchiveMembersRejectsNonArchives(t *testing.T) {
	dir := t.TempDir()
	obj := filepath.Join(dir, "plain.o")
	if err := os.WriteFile(obj, []byte("\x7fELF not an archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{obj, filepath.Join(dir, "absent.a"), dir} {
		if _, isThin, _ := thinArchiveMembers(p); isThin {
			t.Errorf("%s was reported as a thin archive", p)
		}
	}
}

// Build systems emit empty aggregate archives, and GNU ar writes those
// as a plain "!<arch>" — it only
// switches to "!<thin>" once there is a member to reference. Verified
// directly: `ar cDPrST empty.a` produces exactly 8 bytes of "!<arch>".
//
// So an empty archive is correctly NOT reported as thin, and falls
// through to being store-added whole. That is harmless: there are no
// member paths to lose, which is the only thing expansion protects
// against.
func TestThinArchiveMembersEmptyArchiveIsNotThin(t *testing.T) {
	dir := t.TempDir()
	archive := buildThinArchive(t, dir)
	if _, isThin, _ := thinArchiveMembers(archive); isThin {
		t.Error("empty archive reported as thin; ar writes !<arch> for these")
	}
}

// arPath finds a real `ar`. PATH first, then the binutils in the store —
// without this the thin-archive tests silently SKIP in environments
// where ar is not on PATH, which is exactly where a parser regression
// would go unnoticed.
func arPath(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("ar"); err == nil {
		return p
	}
	for _, pat := range []string{
		"/nix/store/*binutils-*/bin/ar",
		"/nix/store/*gcc-wrapper-*/bin/ar",
	} {
		if m, _ := filepath.Glob(pat); len(m) > 0 {
			return m[0]
		}
	}
	t.Skip("no ar available anywhere")
	return ""
}

// A truncated "//" long-name table must be rejected, not panic.
//
// ar pads every member to an even boundary, so the parser advances by
// size+size%2. If an odd-sized member runs exactly to the end of the
// file its pad byte is missing, and a guard that checks only `size`
// lets the advance slice past the end — panicking inside the shim
// mid-build instead of falling back to the real ar.
//
// Hand-rolled rather than built with ar, because ar will not emit a
// truncated archive.
func TestThinArchiveMembersTruncatedLongNameTable(t *testing.T) {
	hdr := make([]byte, arHeaderSize)
	for i := range hdr {
		hdr[i] = ' '
	}
	copy(hdr[0:], "//")
	copy(hdr[48:], "3")   // size: odd
	copy(hdr[58:], "`\n") // fmag

	// Exactly 3 bytes of table and no pad byte: size == len(body), odd.
	data := append([]byte(thinMagic), hdr...)
	data = append(data, 'a', 'b', 'c')

	path := filepath.Join(t.TempDir(), "truncated.a")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	members, isThin, ok := thinArchiveMembers(path)
	if !isThin {
		t.Fatal("truncated thin archive reported as NOT thin; it would be stored whole, silently losing every member")
	}
	if ok {
		t.Errorf("truncated archive parsed as valid, members=%v", members)
	}
}
