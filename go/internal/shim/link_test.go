package shim

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tbereknyei/nixgg/internal/batchpending"
	"github.com/tbereknyei/nixgg/internal/classify"
	"github.com/tbereknyei/nixgg/internal/drvref"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/toolchain"
)

func TestParseLinkArgs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		wantOut    string
		wantInputs []string
		wantFlags  []string
		wantOK     bool
	}{
		{
			name:       "separated -o",
			args:       []string{"a.o", "b.o", "-o", "prog"},
			wantOut:    "prog",
			wantInputs: []string{"a.o", "b.o"},
			wantOK:     true,
		},
		{
			name:       "attached -o",
			args:       []string{"a.o", "-oprog"},
			wantOut:    "prog",
			wantInputs: []string{"a.o"},
			wantOK:     true,
		},
		{
			name:       "archives are inputs too",
			args:       []string{"main.o", "libfoo.a", "-o", "prog"},
			wantOut:    "prog",
			wantInputs: []string{"main.o", "libfoo.a"},
			wantOK:     true,
		},
		{
			name:       "-l flags stay flags when unresolvable",
			args:       []string{"a.o", "-lm", "-ldl", "-o", "prog"},
			wantOut:    "prog",
			wantInputs: []string{"a.o"},
			wantFlags:  []string{"-lm", "-ldl"},
			wantOK:     true,
		},
		{
			// -M* families target paths outside the sandbox.
			name:       "dep-file flags dropped",
			args:       []string{"a.o", "-MD", "-MF", "dep.d", "-o", "prog"},
			wantOut:    "prog",
			wantInputs: []string{"a.o"},
			wantOK:     true,
		},
		{
			// CMake 4 emits this so ninja can track link deps; it makes ld
			// write to a build-tree-relative path that doesn't exist in
			// the link drv's sandbox.
			name:       "-Wl,--dependency-file= dropped (attached)",
			args:       []string{"a.o", "-Wl,--dependency-file=x/link.d", "-o", "prog"},
			wantOut:    "prog",
			wantInputs: []string{"a.o"},
			wantOK:     true,
		},
		{
			name:       "-Wl,--dependency-file dropped (separated)",
			args:       []string{"a.o", "-Wl,--dependency-file", "x/link.d", "-o", "prog"},
			wantOut:    "prog",
			wantInputs: []string{"a.o"},
			wantOK:     true,
		},
		{
			name:   "no inputs is not a link we model",
			args:   []string{"-o", "prog"},
			wantOK: false,
		},
		{
			name:   "no -o is not a link we model",
			args:   []string{"a.o"},
			wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, inputs, flags, _, ok := parseLinkArgs(tc.args)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if out != tc.wantOut {
				t.Errorf("output = %q, want %q", out, tc.wantOut)
			}
			if !reflect.DeepEqual(inputs, tc.wantInputs) {
				t.Errorf("inputs = %q, want %q", inputs, tc.wantInputs)
			}
			if tc.wantFlags != nil && !reflect.DeepEqual(flags, tc.wantFlags) {
				t.Errorf("flags = %q, want %q", flags, tc.wantFlags)
			}
			for _, f := range flags {
				if strings.Contains(f, "dependency-file") {
					t.Errorf("dependency-file survived into flags: %q", flags)
				}
				if strings.HasPrefix(f, "-M") {
					t.Errorf("dep-file flag survived into flags: %q", flags)
				}
			}
		})
	}
}

// TestResolveLibFlagOnlyClaimsOurArtifacts pins that `-l<name>` is
// promoted to an explicit input only when the matching lib<name>.a in
// a -L dir is something nixgg produced. resolveLibFlag recognizes two
// markers: a symlink (native mode's thunk pointer) and a regular file
// starting with the drvref magic header (sandbox mode, since
// builder-rpc-v0 doesn't materialise .drv files into the sandbox so a
// symlink would dangle).
func TestResolveLibFlagOnlyClaimsOurArtifacts(t *testing.T) {
	dir := t.TempDir()

	stub := filepath.Join(dir, "libours.a")
	body := drvref.Body("/nix/store/" + strings.Repeat("a", 32) + "-ar-libours.a.drv")
	if err := os.WriteFile(stub, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	foreign := filepath.Join(dir, "libforeign.a")
	if err := os.WriteFile(foreign, []byte("!<arch>\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := resolveLibFlag("ours", []string{dir}); got != stub {
		t.Errorf("drvref stub not claimed: got %q, want %q", got, stub)
	}
	if got := resolveLibFlag("foreign", []string{dir}); got != "" {
		t.Errorf("foreign archive wrongly claimed as ours: %q — it would be "+
			"referenced as a drv input that does not exist", got)
	}
	if got := resolveLibFlag("absent", []string{dir}); got != "" {
		t.Errorf("nonexistent lib claimed: %q", got)
	}
}

// TestResolveLibFlagClaimsBatchPendingArtifacts is a regression test:
// a -lfoo resolving to a still-deferred batch member's own stub (see
// batchpending.Is) must be claimed the same way a drvref stub is, or
// resolveLibFlag returns "" and the caller leaves a bare -lfoo flag
// that resolves to nothing inside the sandbox.
func TestResolveLibFlagClaimsBatchPendingArtifacts(t *testing.T) {
	dir := t.TempDir()

	pending := filepath.Join(dir, "libbatched.a")
	recordPath := filepath.Join(dir, "record.json")
	if err := os.WriteFile(pending, []byte(batchpending.Body(recordPath)), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := resolveLibFlag("batched", []string{dir}); got != pending {
		t.Errorf("batch-pending stub not claimed: got %q, want %q", got, pending)
	}
}

// TestIsLinkInput pins which link-line tokens are files the linker
// consumes. An unrecognized token is filed under `flags` by
// parseLinkArgs and baked into the drv as a bare relative path that
// does not exist in the sandbox.
func TestIsLinkInput(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"main.o", true},
		{"libfoo.a", true},
		{"mod.xo", true},
		{"thing.lo", true},
		{"MAIN.O", true},
		{"sub/dir/main.o", true},

		// filepath.Ext reports ".2" for the last one, which is why this
		// needs its own check.
		{"libfoo.so", true},
		{"libfoo.so.1", true},
		{"libfoo.so.1.2", true},
		{"libfoo.so.1.2.3", true},
		{"/abs/path/libfoo.so.1", true},

		// Without the leading-dash guard, `-l:libexact.a` has
		// filepath.Ext ".a" and is mistaken for an archive.
		{"-l:libexact.a", false},
		{"-lfoo", false},
		{"-o", false},
		{"-Wl,--as-needed", false},
		{"-L/usr/lib", false},

		{"libfoo.solid", false},
		{"libfoo.so.1.x", false},
		{"libfoo.so.", false},
		{"notes.txt", false},
		{"main.c", false},
		{"", false},
	} {
		t.Run(tc.in, func(t *testing.T) {
			if got := isLinkInput(tc.in); got != tc.want {
				t.Errorf("isLinkInput(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestLinkerScriptPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"version-script attached", []string{"-Wl,--version-script=libcrypto.ld"}, "libcrypto.ld"},
		{"version-script build-tree path", []string{"main.o", "-Wl,--version-script=engines/afalg.ld", "-o", "afalg.so"}, "engines/afalg.ld"},
		{"-Wl,-T comma form", []string{"-Wl,-T,script.ld"}, "script.ld"},
		{"-Wl,--dynamic-list= comma form", []string{"-Wl,--dynamic-list=plugins/qemu-plugin.symbols"}, "plugins/qemu-plugin.symbols"},
		{"-Xlinker --dynamic-list= two-token form", []string{"-Xlinker", "--dynamic-list=/build/source/build/plugins/qemu-plugin.symbols"}, "/build/source/build/plugins/qemu-plugin.symbols"},
		{"-Xlinker --version-script= two-token form", []string{"-Xlinker", "--version-script=libfoo.ld"}, "libfoo.ld"},
		{"-Xlinker with an unrelated value is not a script", []string{"-Xlinker", "-z,now"}, ""},
		{"trailing -Xlinker with no value", []string{"main.o", "-Xlinker"}, ""},
		{"separate -T", []string{"-T", "script.ld"}, "script.ld"},
		{"attached -Tscript.ld", []string{"-Tscript.ld"}, "script.ld"},
		{"-Ttext= is an address, not a script", []string{"-Ttext=0x1000"}, ""},
		{"-Tdata= is an address, not a script", []string{"-Tdata=0x2000"}, ""},
		{"-Tbss= is an address, not a script", []string{"-Tbss=0x3000"}, ""},
		{"no linker script flag at all", []string{"main.o", "-o", "prog"}, ""},
		{"trailing -T with no value", []string{"main.o", "-T"}, ""},
		// Linux Kbuild's scripts/link-vmlinux.sh emits bare --script=
		// (no -Wl,/-Xlinker wrapper) when it invokes $(LD) directly.
		{"bare --script=", []string{"--script=./arch/x86/kernel/vmlinux.lds", "-o", "vmlinux"}, "./arch/x86/kernel/vmlinux.lds"},
		{"-Wl,--script= comma form", []string{"-Wl,--script=script.ld"}, "script.ld"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := linkerScriptPath(tc.args); got != tc.want {
				t.Errorf("linkerScriptPath(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// TestParseLinkArgsSharedLibIsAnInput pins that a positional shared
// library reaches `inputs`, not `flags`; as a flag it would be baked
// into the drv as a bare path with no corresponding staged file.
func TestParseLinkArgsSharedLibIsAnInput(t *testing.T) {
	_, inputs, flags, _, ok := parseLinkArgs(
		[]string{"main.o", "libfoo.so", "libbar.so.1.2", "-o", "prog"})
	if !ok {
		t.Fatal("parseLinkArgs returned !ok")
	}
	want := []string{"main.o", "libfoo.so", "libbar.so.1.2"}
	if !reflect.DeepEqual(inputs, want) {
		t.Errorf("inputs = %q, want %q", inputs, want)
	}
	for _, f := range flags {
		if strings.Contains(f, ".so") {
			t.Errorf("shared lib landed in flags (%q) — it would be baked into "+
				"the drv as a path that does not exist in the sandbox", flags)
		}
	}
}

// TestParseLinkArgsSonameValueIsNotAnInput pins a real regression:
// Linux Kbuild's vdso32 build passes raw ld `-soname linux-gate.so.1`
// (ELF DT_SONAME metadata, not a link input). isSharedLib's own
// `.so.N` version-suffix match means the bare value following -soname
// looks exactly like a versioned shared-library input unless
// -soname's own two-token shape is recognized first. Confirmed
// directly against a real build: without this case the entire link
// fell to unshimmed Passthrough, which then failed because its other
// inputs were still-deferred placeholder thunk symlinks, not real
// objects.
func TestParseLinkArgsSonameValueIsNotAnInput(t *testing.T) {
	_, inputs, flags, _, ok := parseLinkArgs(
		[]string{"note.o", "-soname", "linux-gate.so.1", "-shared", "-o", "vdso32.so.dbg"})
	if !ok {
		t.Fatal("parseLinkArgs returned !ok")
	}
	want := []string{"note.o"}
	if !reflect.DeepEqual(inputs, want) {
		t.Errorf("inputs = %q, want %q — -soname's value must never be treated "+
			"as a link input", inputs, want)
	}
	wantFlags := []string{"-soname", "linux-gate.so.1", "-shared"}
	if !reflect.DeepEqual(flags, wantFlags) {
		t.Errorf("flags = %q, want %q", flags, wantFlags)
	}
}

func TestParseLinkArgsTrailingSonameWithNoValue(t *testing.T) {
	_, inputs, flags, _, ok := parseLinkArgs(
		[]string{"main.o", "-o", "prog", "-soname"})
	if !ok {
		t.Fatal("parseLinkArgs returned !ok")
	}
	if !reflect.DeepEqual(inputs, []string{"main.o"}) {
		t.Errorf("inputs = %q, want [main.o]", inputs)
	}
	if !reflect.DeepEqual(flags, []string{"-soname"}) {
		t.Errorf("flags = %q, want [-soname]", flags)
	}
}

// TestResolveLibFlagExactNameForm pins `-l:libfoo.a`, the spelling
// build systems use to pin a static archive when a shared one also
// exists (ld takes the name literally instead of expanding lib…/.a).
func TestResolveLibFlagExactNameForm(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "libexact.a")
	body := drvref.Body("/nix/store/" + strings.Repeat("a", 32) + "-ar-libexact.a.drv")
	if err := os.WriteFile(stub, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(dir, "libforeign.a")
	if err := os.WriteFile(foreign, []byte("!<arch>\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		arg  string
		want string
	}{
		{"exact name resolves to our stub", ":libexact.a", stub},
		{"plain name still works", "exact", stub},
		{"foreign archive not claimed", ":libforeign.a", ""},
		{"absent file", ":libnope.a", ""},
		{"bare -l: is not a filename", ":", ""},
		{"path separator is not a plain filename", ":sub/libexact.a", ""},
		{"empty name", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveLibFlag(tc.arg, []string{dir}); got != tc.want {
				t.Errorf("resolveLibFlag(%q) = %q, want %q", tc.arg, got, tc.want)
			}
		})
	}
}

// TestStoreInputPreservesSubpath guards the composition step that
// caused a real LLVM link failure ("ld.bfd: cannot find
// /nix/store/…-zlib-1.3.2/libz.so"): LLVM's cmake puts an absolute
// positional shared library on the link line, classification reduces
// it to the store root, and the shim composes the argv token as
// Ref+"/"+Name — using the caller-visible basename for Name drops the
// intervening "lib/", producing a path that doesn't exist.
func TestStoreInputPreservesSubpath(t *testing.T) {
	const root = "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-zlib-1.3.2"

	t.Run("subpath input keeps its directories", func(t *testing.T) {
		c := classify.Result{Kind: classify.Store, Ref: root, Sub: "lib/libz.so"}
		ni, ji := storeInput(c, "/build/llvm/libz.so")

		if ni.Ref != root {
			t.Errorf("native Ref = %q, want the store root %q", ni.Ref, root)
		}
		if ni.Name != "lib/libz.so" {
			t.Errorf("native Name = %q, want \"lib/libz.so\" — the serializers render\n"+
				"Ref+\"/\"+Name, so a bare basename links against a nonexistent file",
				ni.Name)
		}
		if ji.Name != "lib/libz.so" {
			t.Errorf("sandbox Name = %q, want \"lib/libz.so\"", ji.Name)
		}
		// inputs.srcs takes a basename, and must stay the root's basename:
		// the sandbox mounts the whole store object, not one file in it.
		if want := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-zlib-1.3.2"; ji.Ref != want {
			t.Errorf("sandbox Ref = %q, want %q", ji.Ref, want)
		}
	})

	t.Run("direct child falls back to the basename", func(t *testing.T) {
		c := classify.Result{Kind: classify.Store, Ref: root, Sub: ""}
		ni, ji := storeInput(c, "/build/obj/main.o")
		if ni.Name != "main.o" {
			t.Errorf("native Name = %q, want \"main.o\"", ni.Name)
		}
		if ji.Name != "main.o" {
			t.Errorf("sandbox Name = %q, want \"main.o\"", ji.Name)
		}
	})

	t.Run("Sub wins over the caller-visible name", func(t *testing.T) {
		c := classify.Result{Kind: classify.Store, Ref: root, Sub: "lib/libz.so.1.3.2"}
		ni, _ := storeInput(c, "/build/deps/libz.so")
		if ni.Name != "lib/libz.so.1.3.2" {
			t.Errorf("native Name = %q, want the resolved Sub, not the caller's name", ni.Name)
		}
	})
}

// TestClassifyInputsSonameAliasUsesRealOutputName pins the end-to-end
// path for openssl's engines/*.so links, which reference libcrypto via
// the plain `ln -s libcrypto.so.3 libcrypto.so` alias openssl's own
// Makefile creates, not through -lcrypto. classify.Target resolves
// the alias and reports the referenced drv's real output basename via
// Sub; classifyInputs must use that, or the emitted link line reaches
// for a file that never exists.
func TestClassifyInputsSonameAliasUsesRealOutputName(t *testing.T) {
	drv := "/nix/store/" + strings.Repeat("a", 32) + "-bin-libcrypto.so.3.drv"

	dir := t.TempDir()
	real := filepath.Join(dir, "libcrypto.so.3")
	if err := os.WriteFile(real, []byte(drvref.Body(drv)), 0o644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "libcrypto.so")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}

	ci, err, ok := classifyInputs(&toolchain.Config{}, []string{alias}, "", paths.Layout{}, "link", func() error {
		t.Fatal("should not passthrough — the alias resolves to one of our own drvref stubs")
		return nil
	})
	if err != nil || !ok {
		t.Fatalf("classifyInputs failed: ok=%v err=%v", ok, err)
	}
	jsonInputs := ci.JSON
	if len(jsonInputs) != 1 {
		t.Fatalf("got %d jsonInputs, want 1", len(jsonInputs))
	}
	if jsonInputs[0].Ref != drv {
		t.Errorf("Ref = %q, want %q", jsonInputs[0].Ref, drv)
	}
	if jsonInputs[0].Name != "libcrypto.so.3" {
		t.Errorf("Name = %q, want %q (the drv's real output name, not the alias %q)",
			jsonInputs[0].Name, "libcrypto.so.3", filepath.Base(alias))
	}
}

// TestResolveLibFlagFindsSharedLibs pins that `-lfoo` can resolve to a
// nixgg-produced libfoo.so, not only libfoo.a. Search order is
// load-bearing and verified against the real linker: ld tries
// lib<name>.so before lib<name>.a in each -L directory and takes the
// first hit.
func TestResolveLibFlagFindsSharedLibs(t *testing.T) {
	t.Run("plain -lfoo resolves a .so thunk", func(t *testing.T) {
		dir := t.TempDir()
		so := filepath.Join(dir, "libfoo.so")
		if err := os.Symlink(filepath.Join(dir, "whatever.nix"), so); err != nil {
			t.Fatal(err)
		}
		if got := resolveLibFlag("foo", []string{dir}); got != so {
			t.Errorf("resolveLibFlag(\"foo\") = %q, want %q — a self-built shared "+
				"library is invisible to -l resolution, so the link fails at "+
				"`ld: cannot find -lfoo`", got, so)
		}
	})

	t.Run("plain -lfoo resolves a .so drvref stub", func(t *testing.T) {
		dir := t.TempDir()
		so := filepath.Join(dir, "libfoo.so")
		if err := os.WriteFile(so, []byte(drvref.Body("/nix/store/"+
			strings.Repeat("a", 32)+"-bin-libfoo.so.drv")), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := resolveLibFlag("foo", []string{dir}); got != so {
			t.Errorf("sandbox-mode .so stub not resolved: got %q, want %q", got, so)
		}
	})

	t.Run("shared library wins over an archive in the same dir", func(t *testing.T) {
		dir := t.TempDir()
		for _, n := range []string{"libfoo.so", "libfoo.a"} {
			if err := os.WriteFile(filepath.Join(dir, n), []byte(drvref.Body(
				"/nix/store/"+strings.Repeat("b", 32)+"-x.drv")), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want := filepath.Join(dir, "libfoo.so")
		if got := resolveLibFlag("foo", []string{dir}); got != want {
			t.Errorf("resolveLibFlag = %q, want %q — ld searches .so first, so "+
				"claiming the archive links something the caller's toolchain "+
				"would not have chosen", got, want)
		}
	})

	t.Run("archive still resolves when there is no .so", func(t *testing.T) {
		dir := t.TempDir()
		a := filepath.Join(dir, "libfoo.a")
		if err := os.WriteFile(a, []byte(drvref.Body("/nix/store/"+
			strings.Repeat("c", 32)+"-ar-libfoo.a.drv")), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := resolveLibFlag("foo", []string{dir}); got != a {
			t.Errorf("archive resolution regressed: got %q, want %q", got, a)
		}
	})

	t.Run("a foreign .so is still not claimed", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "libfoo.so"),
			[]byte("\x7fELF not ours"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := resolveLibFlag("foo", []string{dir}); got != "" {
			t.Errorf("resolveLibFlag claimed a foreign .so: %q", got)
		}
	})
}

// TestParseLinkArgsPreservesArchiveGroup pins that a linker archive
// group survives the input/flag separation. parseLinkArgs sorts
// tokens into inputs and flags, and buildScript emits all flags before
// all inputs; group brackets are positional, so the pair ended up
// adjacent in flags, spanning nothing — silently defeating ld's
// multi-pass rescan. Reproduced against the real linker with two
// mutually-recursive archives: with the group it links, without it
// fails with `undefined reference to b_fn`.
//
// The fix records that a group was asked for and re-emits it around
// the whole input list, widening the span (harmless, also verified
// against ld) since the caller's exact span is no longer expressible
// once inputs and flags have been separated.
func TestParseLinkArgsPreservesArchiveGroup(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		wantGroup  bool
		wantInputs []string
		wantFlags  []string
	}{
		{
			name: "long spelling",
			args: []string{"m.o", "-Wl,--start-group", "libb.a", "liba.a",
				"-Wl,--end-group", "-o", "prog"},
			wantGroup:  true,
			wantInputs: []string{"m.o", "libb.a", "liba.a"},
			wantFlags:  nil,
		},
		{
			// ld accepts -Wl,-( / -Wl,-) as equivalent; verified.
			name: "paren spelling",
			args: []string{"m.o", "-Wl,-(", "libb.a", "liba.a", "-Wl,-)",
				"-o", "prog"},
			wantGroup:  true,
			wantInputs: []string{"m.o", "libb.a", "liba.a"},
			wantFlags:  nil,
		},
		{
			name:       "bare spelling, no -Wl prefix",
			args:       []string{"m.o", "--start-group", "liba.a", "--end-group", "-o", "prog"},
			wantGroup:  true,
			wantInputs: []string{"m.o", "liba.a"},
			wantFlags:  nil,
		},
		{
			name:       "no group leaves the flag list untouched",
			args:       []string{"m.o", "liba.a", "-O2", "-o", "prog"},
			wantGroup:  false,
			wantInputs: []string{"m.o", "liba.a"},
			wantFlags:  []string{"-O2"},
		},
		{
			name: "other flags still pass through alongside a group",
			args: []string{"-O2", "m.o", "-Wl,--start-group", "liba.a",
				"-Wl,--end-group", "-Wl,-E", "-o", "prog"},
			wantGroup:  true,
			wantInputs: []string{"m.o", "liba.a"},
			wantFlags:  []string{"-O2", "-Wl,-E"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, inputs, flags, group, ok := parseLinkArgs(tc.args)
			if !ok {
				t.Fatalf("parseLinkArgs failed on %q", tc.args)
			}
			if group != tc.wantGroup {
				t.Errorf("group = %v, want %v", group, tc.wantGroup)
			}
			if !reflect.DeepEqual(inputs, tc.wantInputs) {
				t.Errorf("inputs = %q, want %q", inputs, tc.wantInputs)
			}
			if !reflect.DeepEqual(flags, tc.wantFlags) {
				t.Errorf("flags = %q, want %q", flags, tc.wantFlags)
			}
			for _, f := range flags {
				if isGroupBracket(f) {
					t.Errorf("group bracket %q left in flags — it would be emitted "+
						"before the inputs and span nothing", f)
				}
			}
		})
	}
}

// TestParseLinkArgsWholeArchiveTracksNamedSubset pins that
// --whole-archive/--no-whole-archive tracks exactly which inputs fell
// inside the span, unlike isGroupBracket's own single global group
// (widened to cover every input — safe there, not safe here, since it
// changes archive member selection, not just resolution order).
// Modeled directly on Linux Kbuild's own vmlinux.o link recipe
// (scripts/Makefile.vmlinux_o's cmd_ld_vmlinux.o).
func TestParseLinkArgsWholeArchiveTracksNamedSubset(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		wantWhole  []string
		wantInputs []string
	}{
		{
			// Kbuild's own real shape: --whole-archive wraps only
			// vmlinux.a, --start-group/--end-group wraps the libs — two
			// independent, non-overlapping spans on the same line.
			name: "kbuild vmlinux.o shape",
			args: []string{"-r", "--whole-archive", "vmlinux.a", "--no-whole-archive",
				"--start-group", "lib.a", "lib2.a", "--end-group", "-o", "vmlinux.o"},
			wantWhole:  []string{"vmlinux.a"},
			wantInputs: []string{"vmlinux.a", "lib.a", "lib2.a"},
		},
		{
			name:       "no whole-archive span at all",
			args:       []string{"a.o", "liba.a", "-o", "prog"},
			wantWhole:  nil,
			wantInputs: []string{"a.o", "liba.a"},
		},
		{
			name: "-Wl, spelling (driver-invoked link)",
			args: []string{"a.o", "-Wl,--whole-archive", "liba.a",
				"-Wl,--no-whole-archive", "-o", "prog"},
			wantWhole:  []string{"liba.a"},
			wantInputs: []string{"a.o", "liba.a"},
		},
		{
			name: "multiple inputs inside one span",
			args: []string{"--whole-archive", "liba.a", "libb.a",
				"--no-whole-archive", "-o", "prog"},
			wantWhole:  []string{"liba.a", "libb.a"},
			wantInputs: []string{"liba.a", "libb.a"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var whole []string
			_, inputs, _, _, ok := parseLinkArgsWholeArchive(tc.args, &whole)
			if !ok {
				t.Fatalf("parseLinkArgsWholeArchive failed on %q", tc.args)
			}
			if !reflect.DeepEqual(inputs, tc.wantInputs) {
				t.Errorf("inputs = %q, want %q", inputs, tc.wantInputs)
			}
			if !reflect.DeepEqual(whole, tc.wantWhole) {
				t.Errorf("wholeArchive = %q, want %q", whole, tc.wantWhole)
			}
		})
	}
}

// TestStoreInputPromotedArtifactKeepsItsSubdir guards the second half
// of the FHS change: storeInput uses classify.Result.Sub when it has
// one, but doesn't have one for our own promoted outputs — `force`
// copies a realised artifact into the working tree and the promoted
// registry records only the store root, so Sub is empty and the
// artifact's FHS subdir has to be re-derived from its name. Missing
// that broke native-mode lua: liblua.a is at <root>/lib/liblua.a but
// was referenced as <root>/liblua.a.
//
// This is a different cause than the zlib subpath bug above (there,
// Sub was populated from a resolved symlink and the fix was to stop
// discarding it; here Sub is legitimately empty).
func TestStoreInputPromotedArtifactKeepsItsSubdir(t *testing.T) {
	const root = "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-ar-liblua.a"

	t.Run("promoted archive gets lib/", func(t *testing.T) {
		c := classify.Result{Kind: classify.Store, Ref: root}
		ni, ji := storeInput(c, "/build/lua/src/liblua.a")
		if ni.Name != "lib/liblua.a" {
			t.Errorf("native Name = %q, want \"lib/liblua.a\" — the archive drv "+
				"wrote it under lib/, so a flat reference cannot resolve", ni.Name)
		}
		if ji.Name != "lib/liblua.a" {
			t.Errorf("sandbox Name = %q, want \"lib/liblua.a\"", ji.Name)
		}
	})

	t.Run("promoted object stays flat", func(t *testing.T) {
		c := classify.Result{Kind: classify.Store,
			Ref: "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-tu-main.o"}
		ni, _ := storeInput(c, "/build/obj/main.o")
		if ni.Name != "main.o" {
			t.Errorf("Name = %q, want \"main.o\" — compile outputs are flat", ni.Name)
		}
	})

	t.Run("an explicit Sub still wins", func(t *testing.T) {
		c := classify.Result{Kind: classify.Store,
			Ref: "/nix/store/cccccccccccccccccccccccccccccccc-zlib-1.3",
			Sub: "lib/libz.so",
		}
		ni, _ := storeInput(c, "/build/deps/libz.so")
		if ni.Name != "lib/libz.so" {
			t.Errorf("Name = %q, want the resolved Sub", ni.Name)
		}
	})
}

// TestMatchesTarget pins the basename-fallback boundary: it fires only
// when target itself is a bare name (no directory component), never
// when target is itself a relative/absolute path. Modeled on Linux
// Kbuild's own recursive build, which produces many archives sharing
// a basename ("lib.a" at both lib/lib.a and arch/x86/lib/lib.a).
func TestMatchesTarget(t *testing.T) {
	for _, tc := range []struct {
		name           string
		target, output string
		want           bool
	}{
		{"bare name matches exactly", "mosh-server", "mosh-server", true},
		{"bare name matches via basename fallback", "mosh-server", "src/mosh-server", true},
		{"bare name matches an absolute output", "mosh-server", "/build/src/mosh-server", true},
		{
			name:   "path-shaped target does not basename-match a sibling",
			target: "lib/lib.a", output: "arch/x86/lib/lib.a", want: false,
		},
		{
			name:   "path-shaped target does not basename-match the other sibling either",
			target: "arch/x86/lib/lib.a", output: "lib/lib.a", want: false,
		},
		{"path-shaped target matches its own exact path", "lib/lib.a", "lib/lib.a", true},
		{
			name:   "path-shaped built-in.a target does not basename-match a different nested one",
			target: "arch/x86/built-in.a", output: "arch/x86/kernel/built-in.a", want: false,
		},
		{
			// Kbuild's own top-level archive is invoked with exactly this
			// literal bare name, so the basename fallback correctly fires
			// for it — but the same declaration would also match every
			// nested built-in.a's own call, which is why a fixture that
			// needs to target the top-level one uses vmlinux.a instead.
			name:   "bare built-in.a matches the literal top-level invocation",
			target: "built-in.a", output: "built-in.a", want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesTarget(tc.target, tc.output); got != tc.want {
				t.Errorf("matchesTarget(%q, %q) = %v, want %v", tc.target, tc.output, got, tc.want)
			}
		})
	}
}
