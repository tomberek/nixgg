package scan

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

// TestExtractIncludeDirsExcludesForceInclude pins that `-include` is NOT
// treated as an include DIRECTORY.
//
// Regression origin: `-include` shared the pathFlags map with
// -I/-isystem/-iquote/-idirafter, so its value — a FILE — was collected
// as if it were a directory. Two consequences, both silent:
//
//  1. scan emitted a bogus `-I<path-to-a-file>`, which gcc reports only
//     as "warning: config.h: not a directory".
//  2. rewriteFlags, sharing the same map, DROPPED the `-include` from
//     the flags reaching the compiler.
//
// The header still got staged (scan's own -MM -MG lists it as a
// dependency), so the build SUCCEEDED — compiling with different
// preprocessor state than the caller asked for, exit code 0, no error.
// `-include config.h` is the standard autoconf/CMake way to inject
// HAVE_XXX defines, so this hit real projects.
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

// TestExtractForceIncludes pins the companion extractor: `-include`
// values must be recovered so the flag can be re-emitted pointing at the
// staged copy of the header.
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
			// gcc has no attached `-include<file>` spelling, so a token
			// merely starting with -include is not a force-include.
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

// TestResultCacheRoundTrip pins that every Result field survives the
// scan cache. The cache is consulted whenever all recorded deps' mtimes
// match, so a field that encodes but doesn't decode produces a bug
// visible ONLY on warm rebuilds — intermittent by construction, and the
// exact trap StagedIncludeFlags would have fallen into.
func TestResultCacheRoundTrip(t *testing.T) {
	want := &Result{
		ProjectRoot:  "/tmp/proj",
		StagedIFlags: []string{"-I.", "-Iinc"},
		StoreIFlags:  []string{"-I/nix/store/aaa-dep/include"},
		// Flat slice of alternating flag/value, as rewriteFlags appends it.
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
	// corresponding encode/decode line: count them and fail loudly.
	const knownFields = 5
	if n := reflect.TypeOf(Result{}).NumField(); n != knownFields {
		t.Errorf("Result has %d fields, round-trip test knows about %d — "+
			"add the new field to encodeResult/decodeResult and to `want` above, "+
			"then bump knownFields", n, knownFields)
	}
}

// TestRunScannerDoesNotLeakSrcDirAsIFlag pins the ffmpeg regression at
// its real call site: runScanner must pass only the CALLER's include
// dirs to stagedIFlags, never projectRootHints.
//
// Regression origin: those were one list. Compiling
// libavutil/parseutils.c from the ffmpeg root meant srcDir (libavutil/)
// was in it, so it leaked out as a spurious `-Ilibavutil`. Inside the
// drv (cwd = staged root) that resolved to $src/libavutil, whose own
// time.h shadowed glibc's <time.h> — -I dirs are searched before
// -isystem ones — leaving `struct tm` incomplete. Every TU that reached
// it failed, and no ffmpeg fixture exists in drv-equivalence.sh, so
// only a real ffmpeg build caught it.
//
// This drives runScanner with a fake `cc` that prints a canned -MM
// fragment. Testing stagedIFlags alone does NOT catch this: the bug was
// which list the caller hands it, not what it does with the list.
func TestRunScannerDoesNotLeakSrcDirAsIFlag(t *testing.T) {
	// Lay out the ffmpeg shape: root/ with a libavutil/ subdir.
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

	// Fake cc: emit a -MM fragment naming the one header, ignore argv.
	fakeCC := filepath.Join(t.TempDir(), "fake-cc")
	script := "#!/bin/sh\necho 'parseutils.o: libavutil/parseutils.h'\n"
	if err := os.WriteFile(fakeCC, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// Run from the ffmpeg root, as ffmpeg's make does.
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(prev)
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}

	// Caller passes `-I.` only — exactly what ffmpeg's CPPFLAGS does.
	// srcDir is libavutil/, which must NOT become an -I flag.
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

// TestStagedIFlags pins the rewriting itself, given a caller-dir list.
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
			// Only reachable if projectRoot computation already went
			// wrong; a visibly-broken flag beats a silently wrong one.
			// The synthetic "-I." lands LAST here because the caller
			// never named the root.
			"dir outside root becomes ..-relative",
			"/p/sub", []string{"/p/other"}, []string{"-I../other", "-I."},
		},
		{
			// Include ORDER is semantics. kbuild passes the root LAST
			// (-I../include ... -I../. -I.) so that include/net/nfc/nfc.h
			// wins over net/nfc/nfc.h. Hoisting "-I." to the front
			// inverted that and broke net/nfc/af_nfc.c 19,000 compiles
			// into a kernel build.
			"caller order is preserved, root stays last",
			"/p", []string{"/p/include", "/p/net/nfc", "/p"},
			[]string{"-Iinclude", "-Inet/nfc", "-I."},
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

// TestCommonAncestor pins projectRoot selection. projectRoot decides
// where every staged file lands (headers at <root>-relative paths), so a
// wrong answer here either escapes the staging dir with `..` or widens
// far enough to stage unrelated trees.
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
			// The ffmpeg case: cwd=root, srcDir=subdir. Root must stay
			// the ffmpeg root, not narrow to libavutil.
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

// TestIsAssemblySource pins the extension check that gates the
// .incbin scan pass — must match compile.go's own isSource assembly
// recognition (.s/.S), not a broader or narrower set.
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

// TestRunScannerFindsIncbinTargets is a regression test for a real
// gap found against a real Linux kernel build: Linux Kbuild's own
// arch/x86/realmode/rmpiggy.S uses GNU as's `.incbin "path"` directive
// to embed a previously-built binary blob into a later object file.
// `.incbin` is processed by the ASSEMBLER after preprocessing, so
// gcc's own `-M`/`-MM` (which tracks only what the PREPROCESSOR
// consumed — #include, never .incbin) silently omits it — confirmed
// directly against real gcc: no error, no warning, the incbin'd file
// just never appears in the dependency list. Without this, the
// compile shim never stages the incbin target into the TU's sandbox,
// and the real, deferred compile fails with "file not found" for a
// file that exists right there in the caller's own build tree —
// confirmed directly against a real kernel build before this fix.
//
// Uses the REAL system compiler (skips if none on PATH), not a fake
// script, because what's under test is real `as --MD` behavior
// (binutils' own dependency-output flag, unrelated to gcc's -M
// family) — a fake compiler script can't stand in for that without
// just reimplementing the thing being tested.
// realCCForTest finds a REAL compiler binary, bypassing nixgg's own
// shims even when this test runs inside `nixgg develop` (where a bare
// "cc"/"gcc" on PATH IS the shim, not the real tool — using it would
// make this test exercise the shim's own compile path instead of
// as's real --MD behavior, and the shim's own placeholder-thunk
// output would confuse a plain compile probe like this one).
// NIXGG_COMPILER_ROOT (set by `nixgg env` / the dev shell) points at
// the real gcc-wrapper root when present; otherwise fall back to a
// plain PATH lookup, skipping if neither yields a compiler.
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
	found := false
	for _, h := range r.Headers {
		if h.Rel == "blob.bin" {
			found = true
		}
	}
	if !found {
		t.Errorf(".incbin target blob.bin missing from scan result headers: %+v", r.Headers)
	}
}

// The kbuild case. `-Wp,-MMD,<file>` redirects dependency output to a
// file, so a scanner running `gcc -M -MG -MF -` alongside it gets an
// EMPTY stdout and concludes the TU has no headers. The symptom is a
// staging tree containing only the .c, and a missing-header error from
// inside the inner derivation — a long way from the cause.
func TestStripWpDep(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
		keep bool
	}{
		{"kbuild depfile", "-Wp,-MMD,./.helper.o.d", "", false},
		{"MD form", "-Wp,-MD,dep.d", "", false},
		{"MF form", "-Wp,-MF,dep.d", "", false},
		{"valueless forms", "-Wp,-MM,-MG", "", false},
		{"non-dep -Wp survives", "-Wp,-DFOO=1", "-Wp,-DFOO=1", true},
		{"mixed keeps the remainder", "-Wp,-MMD,dep.d,-DBAR", "-Wp,-DBAR", true},
		{"non--Wp flag untouched", "-O2", "-O2", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := StripWpDep(tc.in)
			if ok != tc.keep || (ok && got != tc.want) {
				t.Errorf("StripWpDep(%q) = (%q, %v), want (%q, %v)",
					tc.in, got, ok, tc.want, tc.keep)
			}
		})
	}
}
