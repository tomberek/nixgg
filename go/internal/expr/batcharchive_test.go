package expr

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testMembers() []BatchCompileMember {
	return []BatchCompileMember{
		{
			Tool: "cc", SrcTree: "../srcs/deps-hiredis-sds-o", SrcStore: "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-deps-hiredis-sds-o",
			Source: "sds.c", OutName: "sds.o", Flags: []string{"-O2", "-Wall"},
		},
		{
			Tool: "cc", SrcTree: "../srcs/deps-hiredis-net-o", SrcStore: "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-deps-hiredis-net-o",
			Source: "net.c", OutName: "net.o", Flags: []string{"-O2"},
		},
	}
}

// TestBatchArchiveMemberOrderPreserved pins that both the compile
// lines and the ar argv preserve the caller's own member order —
// some archives are order-sensitive for symbol resolution.
func TestBatchArchiveMemberOrderPreserved(t *testing.T) {
	script := batchArchiveScript("/COREUTILS", "/AR", "rcs", "libhiredis.a", testMembers())

	sdsAt := strings.Index(script, `-c "sds.c"`)
	netAt := strings.Index(script, `-c "net.c"`)
	if sdsAt < 0 || netAt < 0 {
		t.Fatalf("missing a compile line entirely:\n%s", script)
	}
	if sdsAt > netAt {
		t.Errorf("sds.c compiled after net.c — member order not preserved:\n%s", script)
	}

	arAt := strings.Index(script, "ar D")
	arLine := script[arAt:]
	sdsObjAt := strings.Index(arLine, "sds.o")
	netObjAt := strings.Index(arLine, "net.o")
	if sdsObjAt < 0 || netObjAt < 0 || sdsObjAt > netObjAt {
		t.Errorf("ar argv order doesn't match member order:\n%s", arLine)
	}
}

// TestBatchArchiveScriptFlagsQuoted pins that a flag containing a
// shell-meaningful character is escaped the same way as other Kinds.
func TestBatchArchiveScriptFlagsQuoted(t *testing.T) {
	members := []BatchCompileMember{
		{Tool: "cc", SrcStore: "/nix/store/x-src", Source: "a.c", OutName: "a.o",
			Flags: []string{"-DFOO='bar baz'"}},
	}
	script := batchArchiveScript("/COREUTILS", "/AR", "rcs", "lib.a", members)
	if !strings.Contains(script, `'-DFOO='\''bar baz'\'''`) {
		t.Errorf("flag containing a single quote not escaped the way shellQuoteFlags escapes it:\n%s", script)
	}
}

// TestBatchArchiveScriptToolAndPaths pins that each member's compile
// runs from inside its own SrcStore (sandbox mode), using its own
// Tool, and writes into a $objroot shared by every member.
func TestBatchArchiveScriptToolAndPaths(t *testing.T) {
	script := batchArchiveScript("/COREUTILS", "/AR", "rcs", "libhiredis.a", testMembers())
	if !strings.Contains(script, `cd "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-deps-hiredis-sds-o"`) {
		t.Errorf("missing cd into first member's SrcStore:\n%s", script)
	}
	if !strings.Contains(script, `"cc"`) {
		t.Errorf("tool name not embedded:\n%s", script)
	}
	if !strings.Contains(script, `$objroot/sds.o`) || !strings.Contains(script, `$objroot/net.o`) {
		t.Errorf("compile outputs don't land under a shared $objroot:\n%s", script)
	}
}

// TestBatchArchiveScriptThinUsesPermanentObjroot pins that a THIN
// archive writes member objects into $out/lib/.nixgg-objs/ rather than
// a build-tmp scratch dir: a thin archive stores each member's file
// PATH, not its bytes, so the path must survive after the sandbox is
// torn down — confirmed experimentally ("error opening thin archive
// member" from a tmp-relative objroot vs. working from $out/lib/).
func TestBatchArchiveScriptThinUsesPermanentObjroot(t *testing.T) {
	script := batchArchiveScript("/COREUTILS", "/AR", "csrDT", "libfoo.a", testMembers())
	if !strings.Contains(script, `mkdir -p "$out/lib/.nixgg-objs"`) {
		t.Errorf("thin archive script missing permanent objroot mkdir:\n%s", script)
	}
	if !strings.Contains(script, `objroot="$out/lib/.nixgg-objs"`) {
		t.Errorf("thin archive script missing permanent objroot assignment:\n%s", script)
	}
	if strings.Contains(script, `objroot="$PWD/.nixgg-objs"`) {
		t.Errorf("thin archive script still uses the build-tmp scratch objroot:\n%s", script)
	}
}

// TestBatchArchiveScriptNonThinKeepsScratchObjroot pins the
// complement: non-thin archives keep the build-tmp scratch objroot,
// since `ar` copies member bytes into the archive itself.
func TestBatchArchiveScriptNonThinKeepsScratchObjroot(t *testing.T) {
	script := batchArchiveScript("/COREUTILS", "/AR", "rcs", "libfoo.a", testMembers())
	if !strings.Contains(script, `mkdir -p "$out/lib" .nixgg-objs`) {
		t.Errorf("non-thin archive script missing the original scratch-dir mkdir:\n%s", script)
	}
	if !strings.Contains(script, `objroot="$PWD/.nixgg-objs"`) {
		t.Errorf("non-thin archive script missing the original scratch objroot:\n%s", script)
	}
	if strings.Contains(script, `$out/lib/.nixgg-objs`) {
		t.Errorf("non-thin archive script must not reference the permanent objroot path:\n%s", script)
	}
}

// TestBatchArchiveScriptRunsConcurrently EXECUTES the rendered script
// under real bash and confirms members run with real overlapping
// concurrency, not strictly one at a time — pinning the fix for a
// "one gcc/cc1 process at a time regardless of available cores"
// regression found batching ffmpeg's libavcodec.
func TestBatchArchiveScriptRunsConcurrently(t *testing.T) {
	dir := t.TempDir()
	fakeCC := filepath.Join(dir, "fakecc")
	script := "#!/bin/sh\n" +
		`echo "$(date +%s%N) start $1" >> "` + dir + `/log"` + "\n" +
		"sleep 0.2\n" +
		`echo "$(date +%s%N) end $1" >> "` + dir + `/log"` + "\n"
	if err := os.WriteFile(fakeCC, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	const n = 6
	members := make([]BatchCompileMember, n)
	for i := range members {
		srcDir := filepath.Join(dir, fmt.Sprintf("src%d", i))
		if err := os.MkdirAll(srcDir, 0o755); err != nil {
			t.Fatal(err)
		}
		members[i] = BatchCompileMember{
			Tool: fakeCC, SrcStore: srcDir,
			Source: fmt.Sprintf("m%d", i), OutName: fmt.Sprintf("m%d.o", i),
		}
	}

	full := "#!/bin/sh\nset -e\n" + batchArchiveScript("/usr", "/usr", "rcs", "lib.a", members)
	full = full[:strings.Index(full, "ar D")] + "mkdir -p \"$out/lib\"; : \"$out/lib\"\n"
	scriptPath := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(scriptPath, []byte(full), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(), "NIX_BUILD_CORES=3", "out="+dir+"/out")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s\nscript:\n%s", err, out, full)
	}

	logBytes, err := os.ReadFile(filepath.Join(dir, "log"))
	if err != nil {
		t.Fatalf("no log produced — did any member actually run? %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(logBytes)), "\n")
	if len(lines) != 2*n {
		t.Fatalf("expected %d start/end lines, got %d:\n%s", 2*n, len(lines), logBytes)
	}

	type event struct {
		ts   int64
		kind string
	}
	events := make([]event, 0, len(lines))
	for _, l := range lines {
		var ts int64
		var kind, name string
		if _, err := fmt.Sscanf(l, "%d %s %s", &ts, &kind, &name); err != nil {
			t.Fatalf("unparseable log line %q: %v", l, err)
		}
		events = append(events, event{ts, kind})
	}
	// At least 2 members must be simultaneously "started but not yet
	// ended" — impossible if the runner were fully serial.
	running := 0
	maxRunning := 0
	for _, e := range events {
		if e.kind == "start" {
			running++
		} else {
			running--
		}
		if running > maxRunning {
			maxRunning = running
		}
	}
	if maxRunning < 2 {
		t.Errorf("max concurrent members observed = %d, want >= 2 — batch ran serially despite NIX_BUILD_CORES=3", maxRunning)
	}
	if maxRunning > 3 {
		t.Errorf("max concurrent members observed = %d, want <= 3 (NIX_BUILD_CORES) — concurrency cap not respected", maxRunning)
	}
}

// TestBatchArchiveScriptPropagatesFailure EXECUTES the rendered
// script and confirms a single failing member anywhere in the batch
// (not just the last-launched one) fails the WHOLE script — pinning
// against the "plain `wait` only returns the LAST job's exit status"
// footgun found while designing the concurrency fix.
func TestBatchArchiveScriptPropagatesFailure(t *testing.T) {
	dir := t.TempDir()
	fakeCC := filepath.Join(dir, "fakecc")
	// Invocation shape is `"$tool" ... -c "$source" -o "..."`, so the
	// source name is the arg after "-c", not $1.
	script := "#!/bin/sh\n" +
		`while [ "$#" -gt 0 ]; do` + "\n" +
		`  if [ "$1" = "-c" ]; then src="$2"; fi` + "\n" +
		`  shift` + "\n" +
		"done\n" +
		`if [ "$src" = "bad" ]; then exit 1; fi` + "\n" +
		"sleep 0.05\n"
	if err := os.WriteFile(fakeCC, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	names := []string{"a", "b", "bad", "c", "d"}
	members := make([]BatchCompileMember, len(names))
	for i, name := range names {
		srcDir := filepath.Join(dir, "src"+name)
		if err := os.MkdirAll(srcDir, 0o755); err != nil {
			t.Fatal(err)
		}
		members[i] = BatchCompileMember{
			Tool: fakeCC, SrcStore: srcDir,
			Source: name, OutName: name + ".o",
		}
	}

	full := "#!/bin/sh\n" + batchArchiveScript("/usr", "/usr", "rcs", "lib.a", members)
	full = full[:strings.Index(full, "ar D")] + "mkdir -p \"$out/lib\"; : \"$out/lib\"\n"

	scriptPath := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(scriptPath, []byte(full), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(), "NIX_BUILD_CORES=3", "out="+dir+"/out")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("script succeeded despite a failing member — the failure was silently swallowed\noutput:\n%s\nscript:\n%s", out, full)
	}
}

// TestBatchArchiveScriptThinArchiveSurvivesObjectDeletion EXECUTES a
// rendered thin-archive script under real bash + real ar, deletes the
// build-tmp-relative SCRATCH directory the OLD scheme would have used,
// and confirms the resulting archive still resolves its members from
// $out/lib/.nixgg-objs/ — the real, end-to-end property
// TestBatchArchiveScriptThinUsesPermanentObjroot only checks at the
// script-text level.
func TestBatchArchiveScriptThinArchiveSurvivesObjectDeletion(t *testing.T) {
	if _, err := exec.LookPath("ar"); err != nil {
		t.Skip("ar not on PATH")
	}

	workDir := t.TempDir()
	outDir := t.TempDir()

	fakeCC := filepath.Join(workDir, "fakecc")
	ccScript := "#!/bin/sh\n" +
		`while [ "$#" -gt 0 ]; do` + "\n" +
		`  if [ "$1" = "-o" ]; then out="$2"; fi` + "\n" +
		`  shift` + "\n" +
		"done\n" +
		`printf 'OBJECT-BYTES' > "$out"` + "\n"
	if err := os.WriteFile(fakeCC, []byte(ccScript), 0o755); err != nil {
		t.Fatal(err)
	}

	srcDir := filepath.Join(workDir, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	members := []BatchCompileMember{
		{Tool: fakeCC, SrcStore: srcDir, Source: "a.c", OutName: "a.o"},
	}

	arPath, err := exec.LookPath("ar")
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nset -e\n" + batchArchiveScript("/usr", filepath.Dir(filepath.Dir(arPath)), "csrDT", "libfoo.a", members)

	scriptPath := filepath.Join(workDir, "run.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "out="+outDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v\noutput:\n%s\nscript:\n%s", err, out, script)
	}

	archivePath := filepath.Join(outDir, "lib", "libfoo.a")
	if _, err := os.Stat(archivePath); err != nil {
		t.Fatalf("archive not produced: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "lib", ".nixgg-objs", "a.o")); err != nil {
		t.Fatalf("member object not written under $out/lib/.nixgg-objs/: %v", err)
	}

	// Simulate the build sandbox being torn down.
	if err := os.RemoveAll(workDir); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(arPath, "p", archivePath, "a.o").Output()
	if err != nil {
		t.Fatalf("ar p failed after workDir deletion — thin archive member did not survive: %v", err)
	}
	if string(out) != "OBJECT-BYTES" {
		t.Errorf("member content = %q, want %q", out, "OBJECT-BYTES")
	}
}

// TestBatchArchiveJSONSrcsDedup pins that BatchArchiveJSON's
// inputs.srcs is the union of every member's own SrcStore basename
// plus the archive's own StoreDeps and ExtraSrcs, deduplicated.
func TestBatchArchiveJSONSrcsDedup(t *testing.T) {
	members := testMembers()
	dupStoreDep := "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-deps-hiredis-sds-o"

	drv := BatchArchiveJSON(BatchArchiveJSONParams{
		Name: "batch-libhiredis.a", OutName: "libhiredis.a",
		System: "x86_64-linux", Bash: "/nix/store/bash", Coreutils: "/nix/store/coreutils",
		AR: "/nix/store/binutils", ARFlags: "rcs",
		Members:   members,
		StoreDeps: []string{dupStoreDep},
		ExtraSrcs: []string{"bash", "coreutils"},
	})

	seen := map[string]int{}
	for _, s := range drv.Inputs.Srcs {
		seen[s]++
	}
	for s, n := range seen {
		if n > 1 {
			t.Errorf("srcs entry %q appears %d times, want at most once", s, n)
		}
	}
	if seen["deps-hiredis-sds-o"] == 0 && seen[StoreBasename(dupStoreDep)] == 0 {
		t.Errorf("expected the deduplicated store path to still appear once; got srcs=%v", drv.Inputs.Srcs)
	}
	for _, want := range []string{"bash", "coreutils", StoreBasename(members[0].SrcStore), StoreBasename(members[1].SrcStore)} {
		if seen[want] == 0 {
			t.Errorf("missing expected srcs entry %q; got %v", want, drv.Inputs.Srcs)
		}
	}
}

// TestBatchArchiveJSONSingleOutput pins that this Kind produces
// exactly one output named "out", same shape as an ordinary
// KindArchive derivation, so the result flows through
// thunk.LinkPlaceholder / sandbox.PointOutputAtDrv unmodified.
func TestBatchArchiveJSONSingleOutput(t *testing.T) {
	drv := BatchArchiveJSON(BatchArchiveJSONParams{
		Name: "batch-lib.a", OutName: "lib.a", System: "x86_64-linux",
		Bash: "/nix/store/bash", Coreutils: "/nix/store/coreutils", AR: "/nix/store/binutils",
		Members: testMembers(),
	})
	if len(drv.Outputs) != 1 {
		t.Fatalf("got %d outputs, want exactly 1: %+v", len(drv.Outputs), drv.Outputs)
	}
	if _, ok := drv.Outputs["out"]; !ok {
		t.Errorf(`outputs map has no "out" key: %+v`, drv.Outputs)
	}
}

// TestBatchArchiveJSONNeverReferencesSiblingDrv pins that a
// batch-archive's inputs.drvs is always empty — every input is a
// plain staged source tree, never a not-yet-realized sibling
// drv/thunk.
func TestBatchArchiveJSONNeverReferencesSiblingDrv(t *testing.T) {
	drv := BatchArchiveJSON(BatchArchiveJSONParams{
		Name: "batch-lib.a", OutName: "lib.a", System: "x86_64-linux",
		Bash: "/nix/store/bash", Coreutils: "/nix/store/coreutils", AR: "/nix/store/binutils",
		Members: testMembers(),
	})
	if len(drv.Inputs.Drvs) != 0 {
		t.Errorf("Inputs.Drvs = %+v, want empty", drv.Inputs.Drvs)
	}
}

// TestBatchArchiveJSONScriptPassedAsFile pins that the combined script
// goes through Env["batchScript"] + passAsFile, same mechanism as
// assemble.Build's own Env["buildScript"] fix, so Args stays a short,
// fixed `source "$batchScriptPath"` regardless of member count.
func TestBatchArchiveJSONScriptPassedAsFile(t *testing.T) {
	drv := BatchArchiveJSON(BatchArchiveJSONParams{
		Name: "batch-lib.a", OutName: "lib.a", System: "x86_64-linux",
		Bash: "/nix/store/bash", Coreutils: "/nix/store/coreutils", AR: "/nix/store/binutils",
		Members: testMembers(),
	})
	if len(drv.Args) != 2 || drv.Args[0] != "-c" || drv.Args[1] != `source "$batchScriptPath"` {
		t.Fatalf(`Args = %v, want ["-c", "source \"$batchScriptPath\""]`, drv.Args)
	}
	if drv.Env["passAsFile"] != "batchScript" {
		t.Errorf(`Env["passAsFile"] = %q, want "batchScript"`, drv.Env["passAsFile"])
	}
	if !strings.Contains(drv.Env["batchScript"], `-c "sds.c"`) {
		t.Errorf("Env[\"batchScript\"] missing the actual compile script:\n%s", drv.Env["batchScript"])
	}
}

// TestBatchArchiveJSONArgsStaySmallAtScale pins the fix for ffmpeg's
// "Argument list too long" failure batching libavcodec (350+ members,
// 1MB+ combined script): Args must stay short no matter how large the
// real script grows — checked directly against MAX_ARG_STRLEN
// (131072).
func TestBatchArchiveJSONArgsStaySmallAtScale(t *testing.T) {
	members := make([]BatchCompileMember, 900)
	for i := range members {
		members[i] = BatchCompileMember{
			Tool:     "cc",
			SrcStore: fmt.Sprintf("/nix/store/%032x-src%d", i, i),
			Source:   fmt.Sprintf("file%d.c", i),
			OutName:  fmt.Sprintf("file%d.o", i),
			Flags:    []string{"-O2", "-DHAVE_AV_CONFIG_H", "-std=c17"},
		}
	}
	drv := BatchArchiveJSON(BatchArchiveJSONParams{
		Name: "batch-libavcodec.a", OutName: "libavcodec.a", System: "x86_64-linux",
		Bash: "/nix/store/bash", Coreutils: "/nix/store/coreutils", AR: "/nix/store/binutils",
		ARFlags: "rcs", Members: members,
	})

	const maxArgStrlen = 131072 // Linux MAX_ARG_STRLEN
	argvSize := 0
	for _, a := range drv.Args {
		argvSize += len(a)
	}
	if argvSize > 4096 {
		t.Errorf("Args total %d bytes across %d members — want a small, fixed size regardless of member count", argvSize, len(members))
	}
	if argvSize >= maxArgStrlen {
		t.Fatalf("Args total %d bytes exceeds MAX_ARG_STRLEN (%d) — this is exactly the bug", argvSize, maxArgStrlen)
	}
	if len(drv.Env["batchScript"]) < maxArgStrlen {
		t.Fatalf("test setup didn't actually exercise the bug: Env[\"batchScript\"] is only %d bytes, want > %d", len(drv.Env["batchScript"]), maxArgStrlen)
	}
}

// TestBatchArchiveNativeExprShape pins the native-mode expression's
// gross structure: imports batchArchiver.nix, carries a members list
// with an unquoted srcTree path literal per member, and each member's
// own compileLine present as an indented string.
func TestBatchArchiveNativeExprShape(t *testing.T) {
	e := BatchArchive(BatchArchiveParams{
		Helpers: "/nix/store/helpers", OutName: "libhiredis.a", ARFlags: "rcs",
		Members: testMembers(),
	})
	if !strings.HasPrefix(e, "import /nix/store/helpers/batchArchiver.nix {\n") {
		t.Fatalf("expression doesn't import batchArchiver.nix:\n%s", e)
	}
	if !strings.Contains(e, "srcTree = ../srcs/deps-hiredis-sds-o;") {
		t.Errorf("first member's srcTree not present as an unquoted path literal:\n%s", e)
	}
	if !strings.Contains(e, `-c "sds.c"`) {
		t.Errorf("first member's compileLine missing:\n%s", e)
	}
	if strings.Contains(e, "SrcStore") || strings.Contains(e, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") {
		t.Errorf("native-mode expression leaked a sandbox-only SrcStore value:\n%s", e)
	}
}
