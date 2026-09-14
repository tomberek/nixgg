package scan

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

// `-include` shares the pathFlags map with -I/-isystem/-iquote/-idirafter,
// but its value is a FILE, not a directory. `-include config.h` is the
// standard autoconf/CMake way to inject HAVE_XXX defines, so this hit
// real projects: the header still gets staged (scan's own -MM -MG lists
// it as a dependency), so the build succeeds with different preprocessor
// state than the caller asked for, exit code 0, no error.
func TestExtractIncludeDirsExcludesForceInclude(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		want  []string
	}{
		{
			name:  "force-include is not a dir",
			flags: []string{"-include", "config.h"},
			want:  nil,
		},
		{
			name:  "dirs collected, force-include skipped",
			flags: []string{"-I", "inc", "-include", "config.h", "-Isrc"},
			want:  []string{"inc", "src"},
		},
		{
			name:  "attached and separated -I both work",
			flags: []string{"-Ia", "-I", "b", "-isystem", "c", "-iquote", "d", "-idirafter", "e"},
			want:  []string{"a", "b", "c", "d", "e"},
		},
		{
			name:  "value that looks like a flag is still consumed",
			flags: []string{"-include", "-weird.h", "-Ilast"},
			want:  []string{"last"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractIncludeDirs(tc.flags); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("extractIncludeDirs(%q)\n got: %q\nwant: %q", tc.flags, got, tc.want)
			}
		})
	}
}

func TestExtractForceIncludes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		want  []string
	}{
		{"none", []string{"-O2", "-Iinc"}, nil},
		{"one", []string{"-include", "config.h"}, []string{"config.h"}},
		{
			"several, order preserved",
			[]string{"-include", "a.h", "-O2", "-include", "b.h"},
			[]string{"a.h", "b.h"},
		},
		{
			"not confused by -I dirs",
			[]string{"-I", "inc", "-include", "config.h", "-isystem", "sys"},
			[]string{"config.h"},
		},
		{
			"attached spelling is not a thing",
			[]string{"-includeconfig.h"},
			nil,
		},
		{"dangling at end of argv", []string{"-O2", "-include"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractForceIncludes(tc.flags); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("extractForceIncludes(%q)\n got: %q\nwant: %q", tc.flags, got, tc.want)
			}
		})
	}
}

// The cache is consulted whenever all recorded deps' mtimes match, so a
// field that encodes but doesn't decode produces a bug visible ONLY on
// warm rebuilds.
func TestResultCacheRoundTrip(t *testing.T) {
	want := &Result{
		ProjectRoot:        "/tmp/proj",
		StagedIFlags:       []string{"-I.", "-Iinc"},
		StoreIFlags:        []string{"-I/nix/store/aaa-dep/include"},
		StagedIncludeFlags: []string{"-include", "config.h", "-include", "sub/other.h"},
		Headers: []Header{
			{Abs: "/tmp/proj/inc/a.h", Rel: "inc/a.h"},
			{Abs: "/tmp/proj/config.h", Rel: "config.h"},
		},
	}
	body, err := encodeResult(want)
	if err != nil {
		t.Fatal(err)
	}
	var got Result
	if err := decodeResult(body, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(&got, want) {
		t.Errorf("round-trip lost data\n got: %#v\nwant: %#v", &got, want)
	}

	// Guard against a future field being added to Result without a
	// corresponding encode/decode line.
	const knownFields = 5
	if n := reflect.TypeOf(Result{}).NumField(); n != knownFields {
		t.Errorf("Result has %d fields, round-trip test knows about %d — "+
			"add the new field to encodeResult/decodeResult and to `want` above, "+
			"then bump knownFields", n, knownFields)
	}
}

// Regression: runScanner must pass only the CALLER's include dirs to
// stagedIFlags, never projectRootHints. Compiling libavutil/parseutils.c
// from the ffmpeg root meant srcDir (libavutil/) leaked out as a spurious
// `-Ilibavutil`; inside the drv (cwd = staged root) that resolved to
// $src/libavutil, whose own time.h shadowed glibc's <time.h> (-I dirs
// are searched before -isystem ones), leaving `struct tm` incomplete.
// Only a real ffmpeg build caught it — no fixture exists for this.
//
// Drives runScanner with a fake `cc`, since testing stagedIFlags alone
// would not catch this: the bug was which list the caller hands it.
func TestRunScannerDoesNotLeakSrcDirAsIFlag(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "libavutil")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(sub, "parseutils.c")
	hdr := filepath.Join(sub, "parseutils.h")
	for _, f := range []string{src, hdr} {
		if err := os.WriteFile(f, []byte("/* x */\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	fakeCC := filepath.Join(t.TempDir(), "fake-cc")
	script := "#!/bin/sh\necho 'parseutils.o: libavutil/parseutils.h'\n"
	if err := os.WriteFile(fakeCC, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(prev)
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}

	r, _, err := runScanner(fakeCC, "libavutil/parseutils.c", []string{"-I."})
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range r.StagedIFlags {
		if f == "-Ilibavutil" {
			t.Errorf("srcDir leaked into StagedIFlags as %q — inside the drv this "+
				"resolves to $src/libavutil and shadows system headers with the "+
				"project's own (e.g. libavutil/time.h over glibc <time.h>).\n"+
				"got StagedIFlags: %q", f, r.StagedIFlags)
		}
	}
	if want := []string{"-I."}; !reflect.DeepEqual(r.StagedIFlags, want) {
		t.Errorf("StagedIFlags = %q, want %q", r.StagedIFlags, want)
	}
}

func TestStagedIFlags(t *testing.T) {
	for _, tc := range []struct {
		name        string
		projectRoot string
		callerDirs  []string
		want        []string
	}{
		{"root only dedups to -I.", "/p", []string{"/p"}, []string{"-I."}},
		{
			"caller dirs become relative",
			"/p", []string{"/p", "/p/inc", "/p/lib/sub"},
			[]string{"-I.", "-Iinc", "-Ilib/sub"},
		},
		{
			"repeats dedup",
			"/p", []string{"/p", "/p", "/p/inc", "/p/inc"},
			[]string{"-I.", "-Iinc"},
		},
		{"no caller dirs still yields -I.", "/p", nil, []string{"-I."}},
		{
			"dir outside root becomes ..-relative",
			"/p/sub", []string{"/p/other"}, []string{"-I.", "-I../other"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := stagedIFlags(tc.projectRoot, tc.callerDirs)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("stagedIFlags(%q, %q)\n got: %q\nwant: %q",
					tc.projectRoot, tc.callerDirs, got, tc.want)
			}
		})
	}
}

func TestCommonAncestor(t *testing.T) {
	for _, tc := range []struct {
		name string
		dirs []string
		want string
	}{
		{"single dir is its own root", []string{"/p/a"}, "/p/a"},
		{"siblings widen to parent", []string{"/p/a", "/p/b"}, "/p"},
		{"nested keeps the ancestor", []string{"/p", "/p/a/b"}, "/p"},
		{"order does not matter", []string{"/p/a/b", "/p"}, "/p"},
		{"disjoint trees widen to /", []string{"/x/a", "/y/b"}, "/"},
		{"empty input", nil, "/"},
		{"trailing slashes are cleaned", []string{"/p/a/", "/p/a"}, "/p/a"},
		{
			"cwd plus srcDir subdir",
			[]string{"/ff", "/ff/libavutil"},
			"/ff",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := commonAncestor(tc.dirs); got != tc.want {
				t.Errorf("commonAncestor(%q) = %q, want %q", tc.dirs, got, tc.want)
			}
		})
	}
}

func TestIsAssemblySource(t *testing.T) {
	for _, tc := range []struct {
		source string
		want   bool
	}{
		{"foo.s", true},
		{"foo.S", true},
		{"dir/foo.S", true},
		{"foo.c", false},
		{"foo.cpp", false},
		{"foo.h", false},
		{"foo", false},
	} {
		if got := isAssemblySource(tc.source); got != tc.want {
			t.Errorf("isAssemblySource(%q) = %v, want %v", tc.source, got, tc.want)
		}
	}
}

// Regression test: Linux Kbuild's arch/x86/realmode/rmpiggy.S uses GNU
// as's `.incbin "path"` directive to embed a previously-built binary
// blob. `.incbin` is processed by the ASSEMBLER after preprocessing, so
// gcc's `-M`/`-MM` (which tracks only what the preprocessor consumed)
// silently omits it — confirmed against real gcc, no error, no warning.
// Without this, the compile shim never stages the incbin target, and the
// deferred compile fails with "file not found" for a file that exists in
// the caller's own build tree.
//
// realCCForTest uses the REAL system compiler (skips if none on PATH),
// bypassing nixgg's own shims even inside `nixgg develop` (where a bare
// "cc"/"gcc" on PATH IS the shim) — testing real `as --MD` behavior
// needs the real tool, not a fake script standing in for it.
func realCCForTest(t *testing.T) string {
	t.Helper()
	if root := os.Getenv("NIXGG_COMPILER_ROOT"); root != "" {
		if p := filepath.Join(root, "bin", "gcc"); fileExists(p) {
			return p
		}
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		cc, err = exec.LookPath("gcc")
	}
	if err != nil {
		t.Skip("no C compiler available to exercise real as --MD behavior")
	}
	return cc
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func TestRunScannerFindsIncbinTargets(t *testing.T) {
	cc := realCCForTest(t)

	dir := t.TempDir()
	blob := filepath.Join(dir, "blob.bin")
	if err := os.WriteFile(blob, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "wrapper.S")
	asm := "\t.section \".data\"\n\t.incbin \"blob.bin\"\n"
	if err := os.WriteFile(src, []byte(asm), 0o644); err != nil {
		t.Fatal(err)
	}

	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(prev)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	r, _, err := runScanner(cc, "wrapper.S", nil)
	if err != nil {
		t.Fatalf("runScanner: %v", err)
	}
	// Abs, not Rel: a real system gcc's implicit predef header can land
	// outside projectRoot and widen it to "/", shifting Rel unpredictably
	// and making this assertion flaky on CI's system gcc.
	found := false
	for _, h := range r.Headers {
		if h.Abs == blob {
			found = true
		}
	}
	if !found {
		t.Errorf(".incbin target blob.bin missing from scan result headers: %+v", r.Headers)
	}
}
