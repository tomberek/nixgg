// Package shim implements the shim entry points invoked when make (or
// any other build tool with our shims on PATH) executes cc/c++/ar.
//
// Every shim's job is: parse argv, build a Nix expression describing
// what should be produced, write that as a thunk, and symlink the
// output to the thunk. It never calls `nix build` (except for the
// autoconf-conftest carveout). All realisation happens later in
// `nixgg force`.
package shim

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tbereknyei/nixgg/internal/activitylog"
	"github.com/tbereknyei/nixgg/internal/dispatch"
	"github.com/tbereknyei/nixgg/internal/expr"
	"github.com/tbereknyei/nixgg/internal/mode"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/realise"
	"github.com/tbereknyei/nixgg/internal/sandbox"
	"github.com/tbereknyei/nixgg/internal/scan"
	"github.com/tbereknyei/nixgg/internal/stage"
	"github.com/tbereknyei/nixgg/internal/storedeps"
	"github.com/tbereknyei/nixgg/internal/thunk"
	"github.com/tbereknyei/nixgg/internal/toolchain"
	"github.com/tbereknyei/nixgg/internal/wrapperenv"
)

// realToolFor picks the sibling binary matching the caller's argv[0]
// role from the same bin/ dir NIXGG_REAL_CC points at. Nix's
// gcc-wrapper has `cc → gcc` (C mode) and `c++ → g++` (C++ mode) —
// invoking g++ on a `.c` source triggers cc-wrapper's isCxx=1 self-
// check (see wrapper's `bin/gcc = *++` test) and breaks C-only build
// systems. `cc` → `gcc`, `c++` → `g++`, unknown → the pinned RealCC.
func realToolFor(cfg *toolchain.Config, tool dispatch.Tool) string {
	base := tool.Basename()
	if base == "" {
		return cfg.RealCC
	}
	return filepath.Join(filepath.Dir(cfg.RealCC), base)
}

// Compile is the shim entrypoint for `cc -c ...`. It parses argv,
// stages source + headers, writes a thunk, and symlinks the output.
//
// tool is the caller's argv[0] role (cc / gcc / c++ / g++) — that name
// is what gets baked into the derivation's compile command, so
// `cc -c foo.c` produces a "cc" invocation inside the sandbox, not g++.
func Compile(tool dispatch.Tool, args []string, cfg *toolchain.Config, l paths.Layout) error {
	realTool := realToolFor(cfg, tool)
	if bypassed() {
		// No logf here: configure/cmake probes (autoconf's
		// ac_fn_c_check_header_preproc) treat ANY stderr output as a
		// failed check regardless of exit code, so a "[nixgg] ..."
		// line here silently flips HAVE_*_H results to "no".
		return Passthrough(realTool, args)
	}
	source, output, depfile, flags, ok := parseCompileArgs(args)
	if !ok {
		// Not a single-TU compile; execv the real cc and hope. Say so:
		// otherwise a build where nothing is accelerated is
		// indistinguishable from one where everything is.
		logf("compile passthrough: not a single-TU compile (%s)", joinBase(args))
		activitylog.Emit("compile", "passthrough", activitylog.Fields{"argv": args})
		return Passthrough(realTool, args)
	}

	// Kbuild probes/objects that get read back synchronously in the
	// same recipe (mk_elfconfig on empty.o, cmd_pasyms's `nm` on the
	// realmode objects, vdso32.so.dbg's own link inputs). mode.Realise's
	// `nix build --file` can't serve these under sandbox mode — the
	// builder-rpc-v0 protocol only supports "register for later", not
	// "build now and give me output" — so a plain Passthrough (real
	// files on disk) is required here, confirmed against real sandboxed
	// kernel builds.
	if isKbuildElfProbe(source) || isKbuildRealmodeObj(source) || isKbuildVDSO32Obj(source) {
		return Passthrough(realTool, args)
	}

	// Fill in a default output name if -o was omitted.
	if output == "" {
		output = defaultOutputName(source, flags)
	}

	logf("compile %s -> %s", source, output)

	// Resolve the real cc for scan-headers to match the caller's tool
	// role — same reason as the passthrough case above.
	scannerCC := realTool

	// 1. Discover headers.
	scanResult, err := scan.Run(l, scannerCC, source, flags)
	if err != nil {
		return err
	}

	// Kbuild's own `cmd_and_fixdep` macro (scripts/Kbuild.include) runs
	// `fixdep <depfile> <target> <cmdline>` synchronously, in the same
	// recipe, right after this shim invocation returns to make — it
	// expects a real `.d` file (from `-Wp,-MMD,<depfile>`) to already
	// exist. nixgg's deferred-compile model means the real compiler
	// that would eventually honor that flag may not run until much
	// later (or never, if the `.o` is only ever consumed as a store
	// reference) — too late for fixdep's synchronous call. Write a
	// real substitute instead: fixdep doesn't care whether a `.d`
	// file's dependency list came from genuine `-MD` output, only that
	// every listed path is real and readable, which scan's own header
	// list already is.
	if depfile != "" {
		if err := writeSynthesizedDepfile(depfile, output, source, scanResult.Headers); err != nil {
			return err
		}
		logf("  fixdep:     %s", depfile)
	}

	// 2. Stage source + headers into .nixgg/srcs/<tu-id>/.
	srcAbs, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	// The source's staged relpath is its position under the project
	// root — the same layout every header uses. This matches the bash
	// driver: sources aren't special.
	srcRel, err := filepath.Rel(scanResult.ProjectRoot, srcAbs)
	if err != nil {
		return err
	}

	// Opt-in batch classification, deferred until mode.For(source) is
	// known below — a conftest/cmake-probe TU must never be deferred,
	// it needs a synchronous real build now.
	//
	// Classify on srcAbs, NOT srcRel: srcRel is relative to
	// scanResult.ProjectRoot, which can collapse to just the TU's own
	// directory when nothing widens it (e.g. compiling from inside
	// redis's deps/hiredis/ makes srcRel just "sds.c", never matching
	// "deps/**/*.c").
	batchGroup, batched := cfg.BatchGroups.Classify(srcAbs)

	entries := make([]stage.Entry, 0, 1+len(scanResult.Headers))
	entries = append(entries, stage.Entry{Abs: srcAbs, Rel: srcRel})
	for _, h := range scanResult.Headers {
		entries = append(entries, stage.Entry{Abs: h.Abs, Rel: h.Rel})
	}
	// tuID must be unique project-wide so same-basename outputs in
	// different dirs (redis src/sds.o vs deps/hiredis/sds.o) don't
	// collide on the staging dir. Keyed on the output path relative to
	// the workspace root, not absolute — the absolute prefix differs
	// between native (cwd) and sandbox (/build/work) mode for identical
	// source, which would otherwise diverge the drv hash.
	absOut, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	tuKey := absOut
	if rel, err := filepath.Rel(scanResult.ProjectRoot, absOut); err == nil && !strings.HasPrefix(rel, "..") {
		tuKey = rel
	}
	tuID := stage.TUID(tuKey)
	if _, err := stage.Sources(l, tuID, entries); err != nil {
		return err
	}

	// 3. Assemble the sandbox flag list. Strip the caller's -I family
	// (both attached and separated forms) then re-add our staged -I
	// flags (relative to project root) and any store-prefixed -I flags
	// verbatim.
	sandboxFlags := rewriteFlags(flags, scanResult.StagedIFlags, scanResult.StoreIFlags,
		scanResult.StagedIncludeFlags)

	// 4. Build the expression.
	wrapperEnvJSON, err := wrapperenv.JSON()
	if err != nil {
		return err
	}
	storeDeps := storedeps.From(sandboxFlags, wrapperEnvJSON, cfg.KnownStorePaths)
	wrapperEnv, err := decodeStringMap(wrapperEnvJSON)
	if err != nil {
		return err
	}

	// srcTree is a Nix path literal referring to the staging dir.
	// Absolute so the thunk file survives `cp` to a peer directory —
	// see expr.Input.Ref docstring.
	srcTreeLiteral := filepath.Join(l.Srcs, tuID)

	e := expr.Compile(expr.CompileParams{
		Helpers:    cfg.Helpers,
		Tool:       tool.Basename(),
		SrcTree:    srcTreeLiteral,
		Source:     srcRel,
		OutName:    filepath.Base(output),
		Flags:      sandboxFlags,
		StoreDeps:  storeDeps,
		WrapperEnv: wrapperEnv,
	})

	// 5. Dispatch on mode.
	if mode.For(source) == mode.Realise {
		return realiseAndLink(e, output, "", cfg, l)
	}

	if batched {
		return deferCompileToBatch(cfg, l, batchGroup, tuID, tool.Basename(), output, srcRel,
			srcTreeLiteral, sandboxFlags, storeDeps, wrapperEnv)
	}

	// Sandbox mode (or EagerDrv, its unrestricted-daemon variant —
	// see sandbox.EagerDrv): submit a JSON drv directly to the daemon
	// and symlink/stub the output at the returned drv path. No .nix
	// thunk on disk; downstream link/archive shims resolve this via
	// classify.Drv.
	if sandbox.Enabled() || sandbox.EagerDrv() {
		return compileSandbox(cfg, l, tool, tuID, filepath.Base(output), output, srcRel, sandboxFlags, storeDeps, wrapperEnvJSON)
	}

	thunkPath, err := submitCompileThunk(l, e, output)
	if err != nil {
		return err
	}
	logf("  thunk:      %s", thunkPath)
	activitylog.Emit("compile", "thunk", activitylog.Fields{
		"tool": tool.Basename(), "source": source, "output": output, "thunk": thunkPath,
	})
	return nil
}

// submitCompileThunk writes e's thunk, symlinks output at it, and
// records the symlink — native mode's per-TU submission, factored out
// so a later individually-resolved batch member (see
// ResolvePendingMember) can reach the identical code path Compile's
// own native branch uses, without duplicating it.
func submitCompileThunk(l paths.Layout, e, output string) (thunkPath string, err error) {
	id := thunk.Compute(e)
	thunkPath, err = thunk.Write(l, id, e)
	if err != nil {
		return "", err
	}
	if err := thunk.LinkPlaceholder(l, output, thunkPath); err != nil {
		return "", err
	}
	if err := thunk.RecordSymlink(l, id, output); err != nil {
		return "", err
	}
	return thunkPath, nil
}

// compileSandbox handles NIXGG_SANDBOX=1: emit a JSON drv describing
// this compile, hand it to `nix derivation add`, symlink the output
// at the returned drv path.
//
// The staged src tree is uploaded via a plain (non-scanning) `nix
// store add` so its store path matches native mode's plain Nix
// path-literal import byte-for-byte (sandbox.StoreAddDirectory) — a
// scanning add would pick up any /nix/store/... substring in staged
// content as a NAR reference, diverging the store path from native
// mode for identical bytes. See #14/ARCHITECTURE.md.
func compileSandbox(
	cfg *toolchain.Config, l paths.Layout,
	tool dispatch.Tool, tuID, outName, output, srcRel string,
	flags []string, storeDeps []string, wrapperEnvJSON string,
) error {
	// tuID as the store-path name matches what native mode's .nix
	// thunk produces for the same content — same store path, same drv
	// hash.
	srcStore, err := sandbox.StoreAddDirectory(cfg, tuID, filepath.Join(l.Srcs, tuID))
	if err != nil {
		return fmt.Errorf("stage src to store: %w", err)
	}
	wrapperEnv, err := decodeStringMap(wrapperEnvJSON)
	if err != nil {
		return err
	}
	return submitCompileSandboxDrv(cfg, tool.Basename(), outName, output, srcRel, srcStore, flags, storeDeps, wrapperEnv)
}

// submitCompileSandboxDrv is compileSandbox's logic from AFTER the
// src tree is already uploaded and wrapper env decoded — factored
// out so a resolved batch member (whose src tree was uploaded once,
// at defer time, by deferCompileToBatch, and whose WrapperEnv is
// already a decoded map in its MemberRecord) can reach the same
// drv-assembly/submission code without paying for a second,
// redundant upload of unchanged content or a pointless
// decode-then-reencode-then-redecode of the same map.
func submitCompileSandboxDrv(
	cfg *toolchain.Config, toolName, outName, output, srcRel, srcStore string,
	flags []string, storeDeps []string, wrapperEnv map[string]string,
) error {
	// Resolve toolchain roots for the drv. cfg carries the store
	// paths we bootstrapped from NIXGG_COMPILER_ROOT / _BASH_ROOT /
	// _COREUTILS_ROOT.
	bash := cfg.BashRoot
	coreutils := cfg.CoreutilsRoot
	compiler := cfg.CompilerRoot

	// Compute the drv's own $out placeholder. `builtins.placeholder
	// "out"` is sha256("nix-output:out") base32'd. Every derivation
	// gets the same value; the caOutputPlaceholder we compute for
	// referring downstream is different.
	outPlaceholder := "/" + expr.OutPlaceholderNix32

	// Assemble the JSON drv.
	drv := expr.CompileJSON(expr.CompileJSONParams{
		Name:        "tu-" + outName,
		OutName:     outName,
		System:      cfg.System,
		Bash:        bash,
		Coreutils:   coreutils,
		Compiler:    compiler,
		Tool:        toolName,
		SrcStore:    srcStore,
		Source:      srcRel,
		Flags:       flags,
		StoreDeps:   storeDeps,
		Placeholder: outPlaceholder,
		Srcs: []string{
			baseNameOf(bash),
			baseNameOf(coreutils),
			baseNameOf(compiler),
			baseNameOf(srcStore),
		},
		Env: wrapperEnv,
	})

	drvPath, err := sandbox.DerivationAdd(cfg, drv)
	if err != nil {
		return err
	}
	if err := sandbox.PointOutputAtDrv(output, drvPath); err != nil {
		return err
	}
	logf("  drv:        %s", drvPath)
	activitylog.Emit("compile", "drv", activitylog.Fields{
		"tool": toolName, "source": srcRel, "output": output, "drv": drvPath,
	})
	return nil
}

// decodeStringMap parses `{"K1": "V1", ...}` into a Go map.
func decodeStringMap(s string) (map[string]string, error) {
	if s == "" || s == "{}" {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, fmt.Errorf("decode wrapperEnv: %w", err)
	}
	return m, nil
}

// baseNameOf strips the /nix/store/ prefix so we get a hash+name
// basename suitable for a JSONDrv.Inputs.Srcs entry.
func baseNameOf(p string) string {
	return expr.StoreBasename(p)
}

// parseCompileArgs identifies the source + output + non-path flags.
// Returns ok=false if the invocation isn't a single-TU `-c` compile —
// in that case the caller passes through to the real cc.
//
// depfile is the path a genuine `-MD`/`-MMD` compile would have
// written its Makefile-dependency-rule output to, captured (not
// stripped-and-forgotten) so the caller can synthesize a substitute —
// see the fixdep comment in Compile. Recognizes both autotools-style
// argv (`-MF <path>`, or bare `-MD`/`-MMD` with the path implied by
// `-o`) and Kbuild's `-Wp,-MMD,<path>` comma-joined pass-to-cpp form.
func parseCompileArgs(args []string) (source, output, depfile string, flags []string, ok bool) {
	hasDashC := false
	hasDepFlag := false // bare -MD/-MMD with no explicit -MF seen yet
	// Non-empty once `-x <lang>` has been seen: from that point a
	// non-flag token is the source whatever its extension.
	explicitLang := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-c":
			hasDashC = true
		case a == "-o":
			if i+1 >= len(args) {
				return "", "", "", nil, false
			}
			output = args[i+1]
			i++
		case strings.HasPrefix(a, "-o") && len(a) > 2:
			output = a[2:]
		// Dep-file generation flags — drop them. They target paths
		// outside our sandbox (relative to make's cwd), so gcc inside
		// the derivation would try to open cachedObjs/… and fail.
		// scan-headers already gave us the header list we need. The
		// depfile path itself is captured above, not discarded, so a
		// caller relying on a post-compile fixdep-style step (Kbuild)
		// still finds a real file waiting for it.
		case a == "-M" || a == "-MM" || a == "-MG" || a == "-MP":
			// no-op (single-arg forms; no depfile path implied)
		case a == "-MD" || a == "-MMD":
			hasDepFlag = true
		case a == "-MF":
			if i+1 >= len(args) {
				return "", "", "", nil, false
			}
			depfile = args[i+1]
			i++
		case a == "-MT" || a == "-MQ":
			// two-arg forms; skip the value too
			if i+1 >= len(args) {
				return "", "", "", nil, false
			}
			i++
		case strings.HasPrefix(a, "-Wp,"):
			// GCC's comma-joined pass-to-cpp form, e.g.
			// `-Wp,-MMD,path/to/foo.d` or `-Wp,-MMD,path,-MP` — Kbuild's
			// own c_flags (scripts/Makefile.lib) always uses this
			// instead of separate -MD/-MF tokens.
			if p := depfileFromWp(a); p != "" {
				depfile = p
			}
			// Not appended to flags: these target the caller's own
			// cpp/depfile bookkeeping, meaningless inside the sandbox.
		case a == "-x" || a == "-Xlinker" || a == "-Xassembler":
			// Two-arg forms with values that aren't sources; keep both.
			if i+1 >= len(args) {
				return "", "", "", nil, false
			}
			flags = append(flags, a, args[i+1])
			// `-x <lang>` overrides extension-based language detection,
			// so the source that follows need not have a known suffix.
			// The canonical case is a precompiled header:
			//
			//	g++ -x c++-header -c pch.h -o pch.h.gch
			//
			// isSource rejects .h, so without this the whole TU fell to
			// Passthrough — correct output, never cached or distributed.
			if a == "-x" {
				explicitLang = args[i+1]
			}
			i++
		case isSource(a):
			if source != "" {
				// Multiple sources — we don't model that in a single TU.
				return "", "", "", nil, false
			}
			source = a
		case explicitLang != "" && source == "" && !strings.HasPrefix(a, "-"):
			// A bare token after `-x <lang>`: the source, by the driver's
			// own rules, even though its extension says nothing.
			source = a
		default:
			flags = append(flags, a)
		}
	}
	if !hasDashC || source == "" {
		return "", "", "", nil, false
	}
	// gcc's documented default when -MD/-MMD appears without -MF: the
	// depfile path is the object's own path with its extension
	// replaced by .d (computed after `output` is fully resolved, since
	// a caller's `-MDfoo.d` attached form doesn't exist — only -MF
	// carries an explicit path — so this is the only implicit case).
	if depfile == "" && hasDepFlag {
		out := output
		if out == "" {
			out = defaultOutputName(source, flags)
		}
		depfile = depfileFromObjName(out)
	}
	return source, output, depfile, flags, true
}

// depfileFromWp extracts a depfile path from a `-Wp,opt1,opt2,...`
// token: the value immediately following a `-MMD`/`-MD` option in the
// comma list is the depfile path (Kbuild's own convention — see
// scripts/Makefile.lib's `c_flags = -Wp,-MMD,$(depfile) ...`).
func depfileFromWp(a string) string {
	parts := strings.Split(strings.TrimPrefix(a, "-Wp,"), ",")
	for i, p := range parts {
		if (p == "-MMD" || p == "-MD") && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// depfileFromObjName mirrors gcc's own default depfile-naming rule
// for -MD/-MMD without an explicit -MF: replace the object's
// extension with .d.
func depfileFromObjName(obj string) string {
	if dot := strings.LastIndexByte(obj, '.'); dot > 0 {
		return obj[:dot] + ".d"
	}
	return obj + ".d"
}

// isSource returns true if a token looks like a source file that the
// driver would compile into a single .o. Matches the bash driver's
// extension set.
func isSource(a string) bool {
	switch strings.ToLower(filepath.Ext(a)) {
	case ".c", ".cc", ".cpp", ".cxx", ".s":
		return true
	}
	// Uppercase .C, .S are also sources (C++ / preprocessed asm) — keep
	// case-sensitive check for those.
	ext := filepath.Ext(a)
	if ext == ".C" || ext == ".S" {
		return true
	}
	return false
}

// rewriteFlags produces the sandbox-flag list. Strip -I/-isystem/etc
// pairs (both forms) since our staged -I flags cover the same
// directories in the sandbox's layout; then append staged + store.
//
// `-include <file>` is handled separately via forceInc rather than being
// stripped: its value is a header to prepend to the TU, not a directory
// to search, so dropping it changes the preprocessor state the caller
// asked for. scan.StagedIncludeFlags supplies the re-pointed form.
// They go last so the -I flags they may resolve against are already in
// effect.
func rewriteFlags(caller, staged, store, forceInc []string) []string {
	pathFlags := map[string]bool{
		"-I": true, "-isystem": true, "-iquote": true,
		"-idirafter": true,
	}
	var out []string
	for i := 0; i < len(caller); i++ {
		a := caller[i]
		switch {
		case pathFlags[a]:
			if i+1 < len(caller) {
				i++
			}
			continue
		case strings.HasPrefix(a, "-I") && len(a) > 2:
			continue
		// Drop the caller's `-include <file>`; forceInc carries the
		// staged-relative replacement appended below.
		case a == "-include":
			if i+1 < len(caller) {
				i++
			}
			continue
		}
		out = append(out, a)
	}
	out = append(out, staged...)
	out = append(out, store...)
	out = append(out, forceInc...)
	return out
}

// realiseAndLink is the (rare) realise-mode carveout: build the thunk
// synchronously via `nix build --file` and re-target output at a real,
// writable copy of the result. Used for autoconf conftests, cmake
// probes, and (via mode.ForLink, from link.go) Kbuild's own
// realmode.elf/vmlinux.o/vmlinux.
//
// subdir is the caller's own knowledge of where its Kind places this
// artifact — NOT re-derived from output's name here, because
// expr.ArtifactSubdir's name-based guess is wrong for vmlinux.o (a
// LINK output that happens to be named like a compile one, so it
// guesses flat instead of bin/).
//
// Copies the result rather than symlinking it: link-vmlinux.sh's
// `sorttable` rewrites vmlinux in place, which fails "Permission
// denied" through a symlink into the read-only store.
func realiseAndLink(exprBody, output, subdir string, cfg *toolchain.Config, l paths.Layout) error {
	// Write to a tempfile alongside the real thunks so relative-path
	// imports resolve. Reuse the id-based path to keep the file if the
	// same expression comes back later.
	id := thunk.Compute(exprBody)
	thunkPath, err := thunk.Write(l, id, exprBody)
	if err != nil {
		return err
	}
	built, err := nixBuildFile(cfg, thunkPath)
	if err != nil {
		return err
	}
	if err := realise.PromoteToStoreSubdir(l, cfg, id, built, output, subdir); err != nil {
		return err
	}
	logf("  built:      %s", built)
	return nil
}

func nixBuildFile(cfg *toolchain.Config, thunkPath string) (string, error) {
	cmd := exec.Command(cfg.Nix, "build", "-L", "--no-link", "--print-out-paths", "--file", thunkPath)
	cmd.Env = append(os.Environ(),
		"NIX_REMOTE=",
		"NIX_CONFIG=experimental-features = nix-command flakes ca-derivations\nstore = "+cfg.Store+"\n",
	)
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		return "", fmt.Errorf("nix build --file %s: %w\n%s", thunkPath, err, stderr)
	}
	// Last non-empty line of stdout.
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i] != "" {
			return lines[i], nil
		}
	}
	return "", fmt.Errorf("nix build returned no output")
}

// logf emits a one-line `[nixgg]` diagnostic on stderr. Kept minimal
// so we don't clutter build output. Callers pass a format string as
// they would to fmt.Fprintf.
func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[nixgg]   "+format+"\n", args...)
}

// defaultOutputName is what the compiler would write to when -o is
// omitted. Two rules, and they differ in a way that is easy to get wrong:
//
//	a.cc  -> a.o          extension replaced
//	pch.h -> pch.h.gch    extension kept, .gch appended
//
// Verified against gcc by compiling a header with no -o.
func defaultOutputName(source string, flags []string) string {
	base := filepath.Base(source)
	if isHeaderLang(langOf(flags)) {
		return base + ".gch"
	}
	if dot := strings.LastIndexByte(base, '.'); dot > 0 {
		base = base[:dot]
	}
	return base + ".o"
}

// langOf returns the value of the last `-x <lang>` in flags, or "" if
// there is none. parseCompileArgs keeps both tokens, so the language the
// caller asked for is recoverable without widening its signature.
func langOf(flags []string) string {
	lang := ""
	for i := 0; i+1 < len(flags); i++ {
		if flags[i] == "-x" {
			lang = flags[i+1]
		}
	}
	return lang
}

// isHeaderLang reports whether a `-x <lang>` value names a header
// language, i.e. this compile produces a precompiled header rather than
// an object file. The output-naming rule differs: gcc appends .gch to the
// source's full name instead of replacing its extension with .o.
func isHeaderLang(lang string) bool {
	switch lang {
	case "c-header", "c++-header", "objective-c-header", "objective-c++-header":
		return true
	}
	return false
}

// isKbuildElfProbe matches Linux Kbuild's scripts/mod/empty.o: an
// empty TU mk_elfconfig reads back to detect the target's ELF class.
// Matched on the Kbuild-specific path suffix, not the bare basename —
// "empty.c"/"empty.o" is generic enough a real project could name a
// TU that.
func isKbuildElfProbe(source string) bool {
	return strings.HasSuffix(source, "scripts/mod/empty.c") ||
		strings.HasSuffix(source, "scripts/mod/empty.o")
}

// isKbuildRealmodeObj matches Linux Kbuild's fixed
// arch/x86/realmode/rm/{header,trampoline_32,trampoline_64,stack,
// reboot}.o member list (arch/x86/realmode/rm/Makefile's realmode-y).
func isKbuildRealmodeObj(source string) bool {
	base := filepath.Base(source)
	if !strings.Contains(source, "arch/x86/realmode/rm/") {
		return false
	}
	stem := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(base, ".o"), ".S"), ".c")
	switch stem {
	case "header", "trampoline_32", "trampoline_64", "stack", "reboot":
		return true
	}
	return false
}

// isKbuildVDSO32Obj matches Linux Kbuild's fixed
// arch/x86/entry/vdso/vdso32/{note,system_call,sigreturn,
// vclock_gettime,vgetcpu}.o member list (vdso/Makefile's vobjs32-y) —
// vdso32.so.dbg's own link inputs. Passthrough'ing these lets its
// link fall to RealiseThunkArgsAndPassthrough's own passthrough with
// real files on disk; no separate link-side fix needed.
func isKbuildVDSO32Obj(source string) bool {
	if !strings.Contains(source, "arch/x86/entry/vdso/vdso32/") {
		return false
	}
	base := filepath.Base(source)
	stem := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(base, ".o"), ".S"), ".c")
	switch stem {
	case "note", "system_call", "sigreturn", "vclock_gettime", "vgetcpu":
		return true
	}
	return false
}

// writeSynthesizedDepfile writes a Makefile dependency rule at
// depfile — `<output>: <source> <header1> <header2> ...` — built from
// scan's own header list. Stands in for a real `-MD`-produced depfile
// so Kbuild's synchronous `cmd_and_fixdep`/`fixdep` step (which runs
// right after this shim returns, same recipe) finds a real file;
// fixdep doesn't distinguish a synthesized rule from genuine compiler
// output.
func writeSynthesizedDepfile(depfile, output, source string, headers []scan.Header) error {
	if err := os.MkdirAll(filepath.Dir(depfile), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", output, source)
	for _, h := range headers {
		fmt.Fprintf(&b, " \\\n %s", h.Abs)
	}
	b.WriteByte('\n')
	return os.WriteFile(depfile, []byte(b.String()), 0o644)
}
