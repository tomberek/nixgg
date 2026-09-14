package shim

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tbereknyei/nixgg/internal/drvref"
	"github.com/tbereknyei/nixgg/internal/expr"
	"github.com/tbereknyei/nixgg/internal/members"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/toolchain"
)

func thinTestLayout(t *testing.T) paths.Layout {
	t.Helper()
	base := t.TempDir()
	return paths.Layout{
		Thunks:  filepath.Join(base, "thunks"),
		Members: filepath.Join(base, "members"),
	}
}

// TestClassifyInputsExpandsSandboxThinArchive pins the sandbox-mode
// case this mechanism exists for: a link argv naming one thin archive
// must pull in every one of that archive's own recorded members as
// additional direct inputs, not just the archive's own single drv
// reference.
func TestClassifyInputsExpandsSandboxThinArchive(t *testing.T) {
	l := thinTestLayout(t)

	archiveDrv := "/nix/store/" + strings.Repeat("a", 32) + "-ar-libqemuutil.a.drv"
	memberDrv1 := "/nix/store/" + strings.Repeat("b", 32) + "-tu-foo.c.o.drv"
	memberDrv2 := "/nix/store/" + strings.Repeat("c", 32) + "-tu-bar.c.o.drv"

	key := expr.StoreBasename(archiveDrv)
	if _, err := members.Write(l, key, []members.Record{
		{Kind: "drv", Ref: memberDrv1, Name: "foo.c.o"},
		{Kind: "drv", Ref: memberDrv2, Name: "bar.c.o"},
	}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	archivePath := filepath.Join(dir, "libqemuutil.a")
	if err := os.WriteFile(archivePath, []byte(drvref.Body(archiveDrv)), 0o644); err != nil {
		t.Fatal(err)
	}

	ci, err, ok := classifyInputs(&toolchain.Config{}, []string{archivePath}, "", l, "link", func() error {
		t.Fatal("should not passthrough — the archive resolves to one of our own drvref stubs")
		return nil
	})
	if err != nil || !ok {
		t.Fatalf("classifyInputs failed: ok=%v err=%v", ok, err)
	}
	if len(ci.JSON) != 1 {
		t.Fatalf("got %d primary jsonInputs, want 1 (just the archive itself — members are dependency-only): %+v", len(ci.JSON), ci.JSON)
	}
	if ci.JSON[0].Ref != archiveDrv {
		t.Errorf("primary input Ref = %q, want %q", ci.JSON[0].Ref, archiveDrv)
	}
	if len(ci.ExtraJSON) != 2 {
		t.Fatalf("got %d extraJSON, want 2 (the archive's own members, dependency-only): %+v", len(ci.ExtraJSON), ci.ExtraJSON)
	}
	var gotRefs []string
	for _, ji := range ci.ExtraJSON {
		gotRefs = append(gotRefs, ji.Ref)
	}
	for _, want := range []string{memberDrv1, memberDrv2} {
		found := false
		for _, got := range gotRefs {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("extraJSON missing %q; got refs %v", want, gotRefs)
		}
	}
}

// TestClassifyInputsExpandsNativeThinArchive is the native-mode analog:
// a Thunk-classified archive input must pull in its own recorded
// members too.
func TestClassifyInputsExpandsNativeThinArchive(t *testing.T) {
	l := thinTestLayout(t)

	thunkDir := t.TempDir()
	thunkPath := filepath.Join(thunkDir, "abcd1234.nix")
	if err := os.WriteFile(thunkPath, []byte("# fake thunk\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	key := "abcd1234"
	if _, err := members.Write(l, key, []members.Record{
		{Kind: "store", Ref: "/nix/store/" + strings.Repeat("d", 32) + "-tu-baz.c.o", Name: "baz.c.o"},
	}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	archivePath := filepath.Join(dir, "libfoo.a")
	if err := os.Symlink(thunkPath, archivePath); err != nil {
		t.Fatal(err)
	}

	ci, err, ok := classifyInputs(&toolchain.Config{}, []string{archivePath}, "", l, "link", func() error {
		t.Fatal("should not passthrough")
		return nil
	})
	if err != nil || !ok {
		t.Fatalf("classifyInputs failed: ok=%v err=%v", ok, err)
	}
	if len(ci.Link) != 1 {
		t.Fatalf("got %d primary linkInputs, want 1 (just the archive thunk — members are dependency-only): %+v", len(ci.Link), ci.Link)
	}
	if len(ci.ExtraLink) != 1 {
		t.Fatalf("got %d extraLink, want 1 (the propagated member): %+v", len(ci.ExtraLink), ci.ExtraLink)
	}
	found := false
	for _, li := range ci.ExtraLink {
		if li.Kind == "store" && strings.Contains(li.Ref, "tu-baz.c.o") {
			found = true
		}
	}
	if !found {
		t.Errorf("extraLink missing the propagated member: %+v", ci.ExtraLink)
	}
}

// TestClassifyInputsThinArchiveDedup pins that the same member reachable
// through two different thin archives on one link line must appear
// exactly once, in both the native and sandbox slice — native mode's
// own serializer has no dedup of its own.
func TestClassifyInputsThinArchiveDedup(t *testing.T) {
	l := thinTestLayout(t)

	sharedMember := "/nix/store/" + strings.Repeat("e", 32) + "-tu-shared.c.o.drv"
	archiveDrv1 := "/nix/store/" + strings.Repeat("f", 32) + "-ar-liba.a.drv"
	archiveDrv2 := "/nix/store/" + strings.Repeat("1", 32) + "-ar-libb.a.drv"

	if _, err := members.Write(l, expr.StoreBasename(archiveDrv1), []members.Record{
		{Kind: "drv", Ref: sharedMember, Name: "shared.c.o"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := members.Write(l, expr.StoreBasename(archiveDrv2), []members.Record{
		{Kind: "drv", Ref: sharedMember, Name: "shared.c.o"},
	}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	pathA := filepath.Join(dir, "liba.a")
	pathB := filepath.Join(dir, "libb.a")
	if err := os.WriteFile(pathA, []byte(drvref.Body(archiveDrv1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathB, []byte(drvref.Body(archiveDrv2)), 0o644); err != nil {
		t.Fatal(err)
	}

	ci, err, ok := classifyInputs(&toolchain.Config{}, []string{pathA, pathB}, "", l, "link", func() error {
		t.Fatal("should not passthrough")
		return nil
	})
	if err != nil || !ok {
		t.Fatalf("classifyInputs failed: ok=%v err=%v", ok, err)
	}
	if len(ci.JSON) != 2 {
		t.Fatalf("got %d primary jsonInputs, want 2 (the 2 archives): %+v", len(ci.JSON), ci.JSON)
	}
	if len(ci.ExtraJSON) != 1 {
		t.Fatalf("got %d extraJSON, want 1 (the shared member, deduped): %+v", len(ci.ExtraJSON), ci.ExtraJSON)
	}
	if ci.ExtraJSON[0].Ref != sharedMember {
		t.Errorf("extraJSON[0].Ref = %q, want %q", ci.ExtraJSON[0].Ref, sharedMember)
	}
}

// TestClassifyInputsThinArchiveRecursion pins that a thin archive
// consumed by another thin archive expands transitively, verified here
// with a synthetic 3-level chain since no real fixture exercises depth
// > 1.
func TestClassifyInputsThinArchiveRecursion(t *testing.T) {
	l := thinTestLayout(t)

	innerMember := "/nix/store/" + strings.Repeat("2", 32) + "-tu-inner.c.o.drv"
	innerArchiveDrv := "/nix/store/" + strings.Repeat("3", 32) + "-ar-libinner.a.drv"
	outerArchiveDrv := "/nix/store/" + strings.Repeat("4", 32) + "-ar-libouter.a.drv"

	if _, err := members.Write(l, expr.StoreBasename(innerArchiveDrv), []members.Record{
		{Kind: "drv", Ref: innerMember, Name: "inner.c.o"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := members.Write(l, expr.StoreBasename(outerArchiveDrv), []members.Record{
		{Kind: "drv", Ref: innerArchiveDrv, Name: "libinner.a"},
	}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	outerPath := filepath.Join(dir, "libouter.a")
	if err := os.WriteFile(outerPath, []byte(drvref.Body(outerArchiveDrv)), 0o644); err != nil {
		t.Fatal(err)
	}

	ci, err, ok := classifyInputs(&toolchain.Config{}, []string{outerPath}, "", l, "link", func() error {
		t.Fatal("should not passthrough")
		return nil
	})
	if err != nil || !ok {
		t.Fatalf("classifyInputs failed: ok=%v err=%v", ok, err)
	}
	if len(ci.JSON) != 1 || ci.JSON[0].Ref != outerArchiveDrv {
		t.Fatalf("primary jsonInputs = %+v, want exactly [outerArchiveDrv]", ci.JSON)
	}
	var gotRefs []string
	for _, ji := range ci.ExtraJSON {
		gotRefs = append(gotRefs, ji.Ref)
	}
	for _, want := range []string{innerArchiveDrv, innerMember} {
		found := false
		for _, got := range gotRefs {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("extraJSON missing %q via recursion; got refs %v", want, gotRefs)
		}
	}
}

// TestClassifyInputsNonThinArchiveUnaffected pins that an ordinary
// (non-thin) archive is completely untouched by this mechanism: no
// sidecar was ever written for it, so the members.Read lookup is a
// guaranteed miss.
func TestClassifyInputsNonThinArchiveUnaffected(t *testing.T) {
	l := thinTestLayout(t)

	archiveDrv := "/nix/store/" + strings.Repeat("5", 32) + "-ar-libfoo.a.drv"
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "libfoo.a")
	if err := os.WriteFile(archivePath, []byte(drvref.Body(archiveDrv)), 0o644); err != nil {
		t.Fatal(err)
	}

	ci, err, ok := classifyInputs(&toolchain.Config{}, []string{archivePath}, "", l, "link", func() error {
		t.Fatal("should not passthrough")
		return nil
	})
	if err != nil || !ok {
		t.Fatalf("classifyInputs failed: ok=%v err=%v", ok, err)
	}
	if len(ci.JSON) != 1 {
		t.Fatalf("got %d jsonInputs, want exactly 1 (no member propagation for a non-thin archive): %+v", len(ci.JSON), ci.JSON)
	}
	if ci.JSON[0].Ref != archiveDrv {
		t.Errorf("Ref = %q, want %q", ci.JSON[0].Ref, archiveDrv)
	}
	if len(ci.ExtraJSON) != 0 {
		t.Errorf("got %d extraJSON, want 0 for a non-thin archive: %+v", len(ci.ExtraJSON), ci.ExtraJSON)
	}
}

// TestClassifyInputsPreservesRepeatedPrimaryInput is a regression test
// for a real bug found against a real LLVM build: CMake's own generated
// link line for llvm-min-tblgen lists `libLLVMSupport.a
// libLLVMTableGen.a libLLVMSupport.a` — Support repeated after
// TableGen, CMake's own fix for plain `ld`'s left-to-right,
// no-`--start-group` archive resolution. classifyInputs used to
// deduplicate the primary input list the same way it (correctly)
// dedupes the extra (thin-archive-member) list, silently dropping the
// second occurrence — the link then failed with "undefined reference
// to llvm::FoldingSetBase::..." because ld never got a second pass at
// Support's own symbols. Confirmed directly against the real
// llvm-min-tblgen fixture.
func TestClassifyInputsPreservesRepeatedPrimaryInput(t *testing.T) {
	l := thinTestLayout(t)

	supportDrv := "/nix/store/" + strings.Repeat("6", 32) + "-ar-libLLVMSupport.a.drv"
	tablegenDrv := "/nix/store/" + strings.Repeat("7", 32) + "-ar-libLLVMTableGen.a.drv"

	dir := t.TempDir()
	supportPath := filepath.Join(dir, "libLLVMSupport.a")
	tablegenPath := filepath.Join(dir, "libLLVMTableGen.a")
	if err := os.WriteFile(supportPath, []byte(drvref.Body(supportDrv)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tablegenPath, []byte(drvref.Body(tablegenDrv)), 0o644); err != nil {
		t.Fatal(err)
	}

	ci, err, ok := classifyInputs(&toolchain.Config{}, []string{supportPath, tablegenPath, supportPath}, "", l, "link", func() error {
		t.Fatal("should not passthrough")
		return nil
	})
	if err != nil || !ok {
		t.Fatalf("classifyInputs failed: ok=%v err=%v", ok, err)
	}
	if len(ci.JSON) != 3 {
		t.Fatalf("got %d primary jsonInputs, want 3 (Support, TableGen, Support again — "+
			"the caller's own repeat must survive): %+v", len(ci.JSON), ci.JSON)
	}
	if ci.JSON[0].Ref != supportDrv || ci.JSON[1].Ref != tablegenDrv || ci.JSON[2].Ref != supportDrv {
		t.Errorf("primary jsonInputs order/content = %+v, want [Support, TableGen, Support]", ci.JSON)
	}
}

// TestClassifyInputsNestedArchiveMember pins the shape Kbuild's own
// recursive built-in.a construction needs: `ar rcs parent/built-in.a
// a.o b.o child/built-in.a` — a parent archive whose member list
// includes a child directory's own built-in.a, not just object files.
// classifyInputs dispatches purely on classify.Target's Kind, never on
// file extension, so a .a member classified as Drv is handled
// identically to a .o member classified as Drv.
func TestClassifyInputsNestedArchiveMember(t *testing.T) {
	l := thinTestLayout(t)

	childArchiveDrv := "/nix/store/" + strings.Repeat("6", 32) + "-ar-built-in.a.drv"
	dir := t.TempDir()
	objPath := filepath.Join(dir, "a.o")
	if err := os.WriteFile(objPath, []byte("not a real object, just a placeholder"), 0o644); err != nil {
		t.Fatal(err)
	}
	childArchivePath := filepath.Join(dir, "child-built-in.a")
	if err := os.WriteFile(childArchivePath, []byte(drvref.Body(childArchiveDrv)), 0o644); err != nil {
		t.Fatal(err)
	}

	// Only the archive member is passed; a.o would resolve as Regular
	// (a plain on-disk file) and passthrough, which this test isn't
	// exercising.
	ci, err, ok := classifyInputs(&toolchain.Config{}, []string{childArchivePath}, "", l, "ar", func() error {
		t.Fatal("should not passthrough — the nested archive resolves to one of our own drvref stubs")
		return nil
	})
	if err != nil || !ok {
		t.Fatalf("classifyInputs failed: ok=%v err=%v", ok, err)
	}
	if len(ci.JSON) != 1 {
		t.Fatalf("got %d primary jsonInputs, want 1 (the nested archive, as an ordinary drv input): %+v", len(ci.JSON), ci.JSON)
	}
	if ci.JSON[0].Kind != "drv" || ci.JSON[0].Ref != childArchiveDrv {
		t.Errorf("primary input = %+v, want Kind=drv Ref=%q", ci.JSON[0], childArchiveDrv)
	}
}
