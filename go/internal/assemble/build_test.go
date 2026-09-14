package assemble

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tbereknyei/nixgg/internal/expr"
)

func TestBuildReferencesEachStubOnce(t *testing.T) {
	drv := Build(BuildParams{
		Name:      "gg-tree-hello",
		System:    "x86_64-linux",
		Bash:      "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bash",
		Coreutils: "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-coreutils",
		TreeSrc:   "cccccccccccccccccccccccccccccccc-gg-tree-hello-tree",
		Stubs: []Stub{
			{RelPath: "src/hello.o", DrvPath: "/nix/store/dddddddddddddddddddddddddddddddd-tu-hello.o.drv"},
			{RelPath: "bin/hello", DrvPath: "/nix/store/eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee-bin-hello.drv"},
			{RelPath: "lib/libfoo.a", DrvPath: "/nix/store/ffffffffffffffffffffffffffffffff-ar-libfoo.a.drv"},
		},
	})

	if len(drv.Inputs.Drvs) != 3 {
		t.Fatalf("expected 3 drv inputs, got %d: %+v", len(drv.Inputs.Drvs), drv.Inputs.Drvs)
	}
	for _, key := range []string{
		"dddddddddddddddddddddddddddddddd-tu-hello.o.drv",
		"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee-bin-hello.drv",
		"ffffffffffffffffffffffffffffffff-ar-libfoo.a.drv",
	} {
		if _, ok := drv.Inputs.Drvs[key]; !ok {
			t.Errorf("missing drv input key %q; got keys %v", key, keysOf(drv.Inputs.Drvs))
		}
	}

	// Args stays a short fixed string (see Build's docstring re ARG_MAX); the real content is passAsFile'd here.
	if len(drv.Args) != 2 || drv.Args[0] != "-c" {
		t.Fatalf("Args = %v, want [\"-c\", ...]", drv.Args)
	}
	if drv.Env["passAsFile"] != "buildScript" {
		t.Errorf("Env[passAsFile] = %q, want %q", drv.Env["passAsFile"], "buildScript")
	}

	script := drv.Env["buildScript"]
	if !strings.Contains(script, `"$out/src/hello.o"`) {
		t.Errorf("script missing copy destination for src/hello.o:\n%s", script)
	}
	if !strings.Contains(script, `"$out/bin/hello"`) {
		t.Errorf("script missing copy destination for bin/hello:\n%s", script)
	}
	if !strings.Contains(script, `"$out/lib/libfoo.a"`) {
		t.Errorf("script missing copy destination for lib/libfoo.a:\n%s", script)
	}
	// Confirms the source side reads the PRODUCING drv's ArtifactSubdir layout, not the stub's own RelPath.
	if !strings.Contains(script, "/lib/libfoo.a") {
		t.Errorf("script's archive source should read from the producing drv's lib/ subdir:\n%s", script)
	}
}

func TestBuildRestoresTreeBeforeOverlayingStubs(t *testing.T) {
	drv := Build(BuildParams{
		Name:      "gg-tree-hello",
		System:    "x86_64-linux",
		Bash:      "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bash",
		Coreutils: "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-coreutils",
		TreeSrc:   "cccccccccccccccccccccccccccccccc-gg-tree-hello-tree",
	})
	script := drv.Env["buildScript"]
	restoreIdx := strings.Index(script, "cp -a \"/nix/store/cccccccccccccccccccccccccccccccc-gg-tree-hello-tree/.\"")
	if restoreIdx < 0 {
		t.Fatalf("script doesn't restore the captured tree:\n%s", script)
	}
}

func TestBuildDedupesRepeatedStubDrv(t *testing.T) {
	drv := Build(BuildParams{
		Name:      "gg-tree-hello",
		System:    "x86_64-linux",
		Bash:      "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bash",
		Coreutils: "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-coreutils",
		TreeSrc:   "cccccccccccccccccccccccccccccccc-gg-tree-hello-tree",
		Stubs: []Stub{
			{RelPath: "bin/hello", DrvPath: "/nix/store/eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee-bin-hello.drv"},
			{RelPath: "bin/hello2", DrvPath: "/nix/store/eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee-bin-hello.drv"},
		},
	})
	if len(drv.Inputs.Drvs) != 1 {
		t.Fatalf("expected exactly 1 deduped drv input, got %d: %+v", len(drv.Inputs.Drvs), drv.Inputs.Drvs)
	}
}

// Pins the openssl "Argument list too long" fix: Args must stay short and fixed regardless of stub count.
func TestBuildArgsStaySmallAtScale(t *testing.T) {
	stubs := make([]Stub, 2230) // openssl's real count when this broke
	for i := range stubs {
		stubs[i] = Stub{
			RelPath: fmt.Sprintf("crypto/libcrypto-lib-obj%d.o", i),
			DrvPath: fmt.Sprintf("/nix/store/%032x-tu-obj%d.o.drv", i, i),
		}
	}
	drv := Build(BuildParams{
		Name:      "gg-tree-openssl",
		System:    "x86_64-linux",
		Bash:      "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bash",
		Coreutils: "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-coreutils",
		TreeSrc:   "cccccccccccccccccccccccccccccccc-gg-tree-openssl-tree",
		Stubs:     stubs,
	})

	const argMax = 2 * 1024 * 1024 // getconf ARG_MAX on a typical Linux box
	argvSize := 0
	for _, a := range drv.Args {
		argvSize += len(a)
	}
	if argvSize > 4096 {
		t.Errorf("Args total %d bytes across %d stubs — want a small, fixed size regardless of stub count", argvSize, len(stubs))
	}
	if argvSize >= argMax {
		t.Fatalf("Args total %d bytes exceeds ARG_MAX (%d) — this is exactly the bug", argvSize, argMax)
	}
	if len(drv.Env["buildScript"]) == 0 {
		t.Fatal("Env[buildScript] is empty — the script content has to live somewhere")
	}
}

func keysOf(m map[string]expr.JSONDrvRef) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
