package scan

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/tbereknyei/nixgg/internal/paths"
)

// The property RunRust depends on, and which nothing else in the suite
// would catch if a future rustc changed it: rustc writes the dep-info
// file even when the crate FAILS to compile. Under nixgg a crate's
// `--extern` dependencies are drvref stubs, so every scan is such a
// failing compile — if rustc ever started skipping dep-info on error,
// the shim would stage an incomplete source tree and the resulting
// derivation would fail deep inside the sandbox, naming a missing
// module rather than the scan.
//
// Also pins that `include!` is reported. That is the reason this scanner
// is worth having at all: it closes, for Rust, the hole `gcc -M` leaves
// for `.incbin` — a file the compiler reads at expansion time, named in
// a string no preprocessor examines.
func TestRunRustFindsIncludesDespiteBrokenExtern(t *testing.T) {
	rustc := requireRustc(t)

	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "sub", "inc.rs"), "pub const X: u32 = 1;\n")
	mustWrite(t, filepath.Join(root, "main.rs"),
		"mod sub { include!(\"sub/inc.rs\"); }\n"+
			"pub fn go() -> u32 { missingcrate::hi() + sub::X }\n")

	chdir(t, root)
	l := paths.Layout{Scans: filepath.Join(root, "scans")}
	// A bare --extern with nothing to resolve it: the same shape a
	// nixgg-shimmed build presents, minus the stub file.
	r, err := RunRust(l, rustc, "main.rs",
		[]string{"--crate-type=rlib", "--crate-name=app", "--edition=2021",
			"--extern", "missingcrate"})
	if err != nil {
		t.Fatalf("RunRust: %v", err)
	}
	if !hasRel(r.Headers, "sub/inc.rs") {
		t.Errorf("include!(\"sub/inc.rs\") missing from scan; got %v", rels(r.Headers))
	}
}

// The scan must not write the artifact the build is waiting for. The
// caller's --emit and --out-dir name real build outputs, and the scan
// compiles with stub externs — so leaving them in place would drop a
// broken object exactly where a correct one belongs.
func TestRunRustLeavesCallerOutputsAlone(t *testing.T) {
	rustc := requireRustc(t)

	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "lib.rs"), "pub fn hi() -> u32 { 7 }\n")
	chdir(t, root)

	l := paths.Layout{Scans: filepath.Join(root, "scans")}
	out := filepath.Join(root, "lib.o")
	if _, err := RunRust(l, rustc, "lib.rs",
		[]string{"--crate-type=rlib", "--crate-name=lib", "--edition=2021",
			"--emit=obj=" + out, "--out-dir", root}); err != nil {
		t.Fatalf("RunRust: %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Errorf("scan wrote %s; the build's own compile must be the only thing that does", out)
	}
}

func hasRel(hs []Header, rel string) bool {
	for _, h := range hs {
		if h.Rel == rel {
			return true
		}
	}
	return false
}

func rels(hs []Header) []string {
	var out []string
	for _, h := range hs {
		out = append(out, h.Rel)
	}
	return out
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
}

// requireRustc skips when no rustc is available, so `go test ./...`
// still passes on a machine without one.
func requireRustc(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("rustc")
	if err != nil {
		t.Skip("no rustc on PATH; skipping rust scanner test")
	}
	return p
}
