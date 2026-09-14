// nixgg is a busybox-style multi-call binary: the same ELF is symlinked
// into shims/{cc,gcc,c++,g++,ar,ranlib,ld}, and dispatches by argv[0].
// When invoked as its own name it acts as the CLI (run/eval/force/build/
// emit/stats/env).
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/tbereknyei/nixgg/internal/cli"
	"github.com/tbereknyei/nixgg/internal/dispatch"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/shim"
	"github.com/tbereknyei/nixgg/internal/toolchain"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "nixgg: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	argv0 := filepath.Base(os.Args[0])
	tool := dispatch.FromArgv0(argv0)

	// CLI mode: argv[0] is "nixgg" (or anything we don't recognise).
	if tool == dispatch.ToolUnknown {
		return cli.Main(os.Args[1:])
	}

	// Shim mode. Load config + resolve on-disk layout once.
	cfg, err := toolchain.FromEnv()
	if err != nil {
		return err
	}
	l, err := paths.Resolve()
	if err != nil {
		return err
	}

	// Expand @rspfiles once at the top so every downstream parser sees
	// the flattened form.
	args := dispatch.ExpandRspfiles(os.Args[1:])

	switch tool {
	case dispatch.ToolAR:
		return shim.Archive(args, cfg, l)
	case dispatch.ToolLD:
		return shim.LD(args, cfg, l)
	case dispatch.ToolObjtool:
		return shim.Objtool(args, cfg, l)
	case dispatch.ToolObjcopy:
		return shim.Objcopy(args, cfg, l)
	case dispatch.ToolRanlib:
		// Our archives are already indexed (ar handles `s` in the
		// sandbox), so ranlib on them is a no-op.
		return nil
	case dispatch.ToolCC, dispatch.ToolGCC, dispatch.ToolCXX, dispatch.ToolGXX:
		if dispatch.IsCompile(args) {
			return shim.Compile(tool, args, cfg, l)
		}
		return shim.Link(tool, args, cfg, l)
	}
	return fmt.Errorf("shim: unhandled tool %q", argv0)
}
