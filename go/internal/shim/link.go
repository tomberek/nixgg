package shim

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tbereknyei/nixgg/internal/activitylog"
	"github.com/tbereknyei/nixgg/internal/batchpending"
	"github.com/tbereknyei/nixgg/internal/classify"
	"github.com/tbereknyei/nixgg/internal/dispatch"
	"github.com/tbereknyei/nixgg/internal/drvref"
	"github.com/tbereknyei/nixgg/internal/expr"
	"github.com/tbereknyei/nixgg/internal/mode"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/realise"
	"github.com/tbereknyei/nixgg/internal/sandbox"
	"github.com/tbereknyei/nixgg/internal/stage"
	"github.com/tbereknyei/nixgg/internal/storedeps"
	"github.com/tbereknyei/nixgg/internal/thunk"
	"github.com/tbereknyei/nixgg/internal/toolchain"
	"github.com/tbereknyei/nixgg/internal/wrapperenv"
)

// Link is the shim entrypoint for `cc ... foo.o bar.o -o baz` (no -c).
func Link(tool dispatch.Tool, args []string, cfg *toolchain.Config, l paths.Layout) error {
	realTool := realToolFor(cfg, tool)
	if bypassed() {
		// No logf: autoconf/cmake link probes treat any stderr as failure.
		return Passthrough(realTool, args)
	}
	for _, a := range args {
		switch a {
		case "-c", "-E", "-S", "-M", "-MM":
			logf("link passthrough: %s is a compile-family flag", a)
			activitylog.Emit("link", "passthrough", activitylog.Fields{"reason": "compile_family_flag", "flag": a, "argv": args})
			return Passthrough(realTool, args)
		}
	}

	var wholeArchiveInputs []string
	output, inputs, flags, group, ok := parseLinkArgsWholeArchive(args, &wholeArchiveInputs)
	if !ok {
		logf("link passthrough: unparseable link line (%s)", joinBase(args))
		activitylog.Emit("link", "passthrough", activitylog.Fields{"reason": "unparseable", "argv": args})
		// Not a bare Passthrough: other inputs on this line may still be
		// real nixgg thunk siblings that need realising first.
		return RealiseThunkArgsAndPassthrough(cfg, l, realTool, args, sandbox.Enabled())
	}

	logf("link %s <- %s", output, joinBase(inputs))

	// A linker-script flag (-Wl,--version-script=<path>, -Wl,-T,<path>,
	// -Xlinker --dynamic-list=<path>) names a file some unshimmed tool
	// in the caller's own build wrote moments earlier, never something
	// nixgg produced; the link's own sandbox never saw it and must
	// stage its content explicitly.
	//
	// Relative paths (openssl) are staged via stage.ContentFiles rather
	// than embedded as script text: a large script plus hundreds of
	// object paths on one link line can exceed the kernel's argv limit
	// (confirmed: openssl's libcrypto.so.3, "Argument list too long").
	// Absolute paths (QEMU/meson) bake the build tree's own path into
	// the generated file's name, so there's nothing to copy into —
	// embedded directly as script text instead (QEMU's is 59 lines,
	// well under the argv limit).
	var inlineFilesStore, absFilePath, absFileContent string
	if path := linkerScriptPath(args); path != "" {
		c := classify.Target(path, altStorePrefix(cfg.Store), l)
		if c.Kind != classify.Regular {
			logf("link passthrough: linker script %s is not a plain local file (%s)", path, c.Reason())
			return Passthrough(realTool, args)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			logf("link passthrough: linker script %s: %v", path, err)
			return Passthrough(realTool, args)
		}
		if filepath.IsAbs(path) {
			absFilePath = path
			absFileContent = string(content)
		} else {
			id := "inline-" + filepath.Base(output) + "-" + filepath.Base(path)
			stageDir, err := stage.ContentFiles(l, id, []stage.FileEntry{{Rel: path, Content: content}})
			if err != nil {
				return fmt.Errorf("stage linker script %s: %w", path, err)
			}
			if sandbox.Enabled() || sandbox.EagerDrv() {
				// Non-scanning upload, for store-path parity with native
				// mode (see compile.go's SrcStore).
				inlineFilesStore, err = sandbox.StoreAddDirectory(cfg, id, stageDir)
				if err != nil {
					return fmt.Errorf("stage linker script %s to store: %w", path, err)
				}
			} else {
				inlineFilesStore = stageDir
			}
		}
	}

	// Classify each input.
	altPrefix := altStorePrefix(cfg.Store)
	ci, err, ok := classifyInputs(cfg, inputs, altPrefix, l, "link", func() error {
		// The other, already-classified inputs may still be real nixgg
		// thunks that need realising before falling back to the real
		// linker — see RealiseThunkArgsAndPassthrough.
		return RealiseThunkArgsAndPassthrough(cfg, l, realTool, args, sandbox.Enabled())
	})
	if !ok {
		return err
	}

	wrapperEnvJSON, err := wrapperenv.JSON()
	if err != nil {
		return err
	}
	storeDeps := storedeps.From(flags, wrapperEnvJSON, cfg.KnownStorePaths)

	// Sandbox mode (or EagerDrv): emit JSON, register the drv.
	if sandbox.Enabled() || sandbox.EagerDrv() {
		return linkSandbox(cfg, tool, output, ci.JSON, ci.ExtraJSON, flags, group, wholeArchiveInputs, inlineFilesStore, absFilePath, absFileContent, storeDeps, wrapperEnvJSON)
	}

	wrapperEnv, err := decodeStringMap(wrapperEnvJSON)
	if err != nil {
		return err
	}
	e := expr.Link(expr.LinkParams{
		Helpers:            cfg.Helpers,
		Name:               multiTargetName(output),
		Tool:               tool.Basename(),
		OutName:            filepath.Base(output),
		Inputs:             ci.Link,
		ExtraInputs:        ci.ExtraLink,
		Flags:              flags,
		GroupInputs:        group,
		WholeArchiveInputs: wholeArchiveInputs,
		InlineFilesStore:   inlineFilesStore,
		AbsFilePath:        absFilePath,
		AbsFileContent:     absFileContent,
		StoreDeps:          storeDeps,
		WrapperEnv:         wrapperEnv,
	})

	// mode.ForLink: a narrow carveout for link outputs an unshimmed
	// tool reads back synchronously in the same recursive make (Linux
	// Kbuild's arch/x86/tools/relocs on realmode.elf — see mode.go).
	// "bin" is passed explicitly rather than guessed from output's
	// name, since realiseAndLink's own name-based guess is wrong for
	// vmlinux.o.
	if mode.ForLink(output) == mode.Realise {
		return realiseAndLink(e, output, "bin", cfg, l)
	}

	// Links are placeholder-mode by default: the resulting binary isn't
	// usually consumed inside the same make invocation. `nixgg force
	// <target>` — or `nixgg build --target …` — realises at the end.
	id := thunk.Compute(e)
	thunkPath, err := thunk.Write(l, id, e)
	if err != nil {
		return err
	}
	if err := thunk.LinkPlaceholder(l, output, thunkPath); err != nil {
		return err
	}
	if err := thunk.RecordSymlink(l, id, output); err != nil {
		return err
	}
	logf("  thunk:      %s", thunkPath)
	activitylog.Emit("link", "thunk", activitylog.Fields{"output": output, "thunk": thunkPath, "inputs": inputs})

	// NIXGG_AUTOFORCE=1: realise the link's DAG inline, so a plain
	// `NIXGG_AUTOFORCE=1 make` produces real binaries in the working
	// tree without a wrapper (`nixgg build …`). Only the link shim
	// does this; compile/archive shims stay placeholder so intermediate
	// .o/.a files aren't forced ahead of the link that consumes them.
	if os.Getenv("NIXGG_AUTOFORCE") == "1" {
		if err := realise.Realise(l, cfg, thunkPath, output); err != nil {
			return err
		}
	}
	return nil
}

// parseLinkArgs pulls out -o OUT and every .o/.a input token, treating
// everything else as a flag.
//
// `-L<dir> -l<name>` pairs get resolved against local files: if
// `<dir>/lib<name>.a` exists as a nixgg drvref stub (or thunk
// symlink), it's promoted to an explicit input and the `-l<name>`
// is dropped. ffmpeg's Makefile writes its link line that way
// (`-Llibavcodec -lavcodec` instead of `libavcodec/libavcodec.a`)
// and the drv otherwise fails at ld with "cannot find -lavcodec"
// because the produced `.a` isn't on the sandbox's link path.

// isGroupBracket reports whether a token opens or closes a linker
// archive group. Both spellings ld accepts are handled.
func isGroupBracket(a string) bool {
	switch a {
	case "-Wl,--start-group", "-Wl,--end-group", "-Wl,-(", "-Wl,-)",
		"--start-group", "--end-group":
		return true
	}
	return false
}

// isWholeArchiveStart/isWholeArchiveEnd report whether a token opens
// or closes a --whole-archive span. Unlike isGroupBracket's single
// global group, --whole-archive's span changes archive MEMBER
// SELECTION (force every member in, vs. only members something else
// references), so it must track exactly which inputs the caller's own
// line put inside it.
func isWholeArchiveStart(a string) bool {
	return a == "-Wl,--whole-archive" || a == "--whole-archive"
}

func isWholeArchiveEnd(a string) bool {
	return a == "-Wl,--no-whole-archive" || a == "--no-whole-archive"
}

func parseLinkArgs(args []string) (output string, inputs, flags []string, group, ok bool) {
	return parseLinkArgsWholeArchive(args, nil)
}

// parseLinkArgsWholeArchive is parseLinkArgs' real implementation,
// also returning the WholeArchiveInputs subset. Split out so
// parseLinkArgs' own signature stays unchanged; only the one caller
// that needs to plumb WholeArchiveInputs through to expr.Link/
// LinkJSON calls this directly.
func parseLinkArgsWholeArchive(args []string, wholeArchive *[]string) (output string, inputs, flags []string, group, ok bool) {
	var libDirs []string
	inWholeArchive := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-o":
			if i+1 >= len(args) {
				return
			}
			output = args[i+1]
			i++
		case strings.HasPrefix(a, "-o") && len(a) > 2:
			output = a[2:]
		// `-soname <name>` (raw ld's separated form). <name> is ELF
		// DT_SONAME metadata, not a link input, but isSharedLib's
		// generous `.so.N` match would otherwise misclassify a value
		// like Kbuild's `-soname linux-gate.so.1` as a shared-library
		// input.
		case a == "-soname":
			if i+1 < len(args) {
				flags = append(flags, a, args[i+1])
				i++
			} else {
				flags = append(flags, a)
			}
		// Group brackets are positional: they bracket the inputs
		// BETWEEN them, and our reassembly emits all flags before all
		// inputs, which would leave the pair spanning nothing. Record
		// that a group was requested and re-emit it around the whole
		// input list instead. Objects inside a group are harmless
		// (verified against ld), so widening the span is safe here.
		case isGroupBracket(a):
			group = true
		// --whole-archive is ALSO positional, but its span cannot be
		// widened like a group bracket's (see isWholeArchiveStart) —
		// track exactly which inputs fall inside it instead.
		case isWholeArchiveStart(a):
			inWholeArchive = true
		case isWholeArchiveEnd(a):
			inWholeArchive = false
		case isLinkInput(a):
			inputs = append(inputs, a)
			if inWholeArchive && wholeArchive != nil {
				*wholeArchive = append(*wholeArchive, filepath.Base(a))
			}
		case strings.HasPrefix(a, "-L") && len(a) > 2:
			libDirs = append(libDirs, a[2:])
			flags = append(flags, a)
		case a == "-L":
			if i+1 < len(args) {
				libDirs = append(libDirs, args[i+1])
				flags = append(flags, a, args[i+1])
				i++
			}
		case strings.HasPrefix(a, "-l") && len(a) > 2:
			if hit := resolveLibFlag(a[2:], libDirs); hit != "" {
				inputs = append(inputs, hit)
				if inWholeArchive && wholeArchive != nil {
					*wholeArchive = append(*wholeArchive, filepath.Base(hit))
				}
			} else {
				flags = append(flags, a)
			}
		// Drop dep-file flags — link-time -M is meaningless in our thunk.
		case a == "-M" || a == "-MM" || a == "-MG" || a == "-MP" || a == "-MD" || a == "-MMD":
			// skip
		case a == "-MF" || a == "-MT" || a == "-MQ":
			i++ // skip value
		// Drop `-Wl,--dependency-file=<path>`: CMake 4 emits this so ld
		// writes a link-time dep makefile fragment to a build-tree-
		// relative path that doesn't exist in the link drv's sandbox
		// (only staged inputs do), which otherwise fails "cannot open
		// dependency file". Dep tracking is the caller's build system's
		// concern; Nix's CA hashing already handles rebuild correctness.
		case strings.HasPrefix(a, "-Wl,--dependency-file="):
			// skip
		case a == "-Wl,--dependency-file":
			i++ // skip value (separated form)
		default:
			flags = append(flags, a)
		}
	}
	if len(inputs) == 0 || output == "" {
		return "", nil, nil, false, false
	}
	return output, inputs, flags, group, true
}

// linkerScriptPath scans a link line for a linker-script flag and
// returns the path it names, or "" if none is present. Handles:
//
//   - `-Wl,--version-script=<path>` / `-Wl,-T,<path>` (gcc's usual
//     comma-joined form)
//   - plain `-T <path>` / `-T<path>`
//   - `-Wl,--dynamic-list=<path>` — QEMU's meson build uses it for its
//     plugin-symbol-export list
//   - `-Xlinker <value>` (two argv tokens) — QEMU emits
//     `-Xlinker --dynamic-list=<path>` this way instead
//   - bare `--script=<path>` — used when raw `ld` is invoked directly;
//     Linux Kbuild's link-vmlinux.sh does this
//
// Excludes `-Ttext=`/`-Tdata=`/`-Tbss=` — same `-T` prefix, but an
// address override, not a script path.
func linkerScriptPath(args []string) string {
	// bareFlagValue recognizes the ld flag spellings that name a file
	// without any -Wl,/-Xlinker wrapper, shared by the comma-joined
	// and -Xlinker branches below so the two can't drift.
	bareFlagValue := func(a string) (string, bool) {
		if v, ok := strings.CutPrefix(a, "--version-script="); ok {
			return v, true
		}
		if v, ok := strings.CutPrefix(a, "--dynamic-list="); ok {
			return v, true
		}
		if v, ok := strings.CutPrefix(a, "--script="); ok {
			return v, true
		}
		return "", false
	}
	for i, a := range args {
		switch {
		case strings.HasPrefix(a, "-Wl,--version-script="):
			return strings.TrimPrefix(a, "-Wl,--version-script=")
		case strings.HasPrefix(a, "-Wl,--dynamic-list="):
			return strings.TrimPrefix(a, "-Wl,--dynamic-list=")
		case strings.HasPrefix(a, "-Wl,-T,"):
			return strings.TrimPrefix(a, "-Wl,-T,")
		case strings.HasPrefix(a, "-Wl,--script="):
			return strings.TrimPrefix(a, "-Wl,--script=")
		case a == "-Xlinker" && i+1 < len(args):
			if v, ok := bareFlagValue(args[i+1]); ok {
				return v
			}
		case a == "-T" && i+1 < len(args):
			return args[i+1]
		case strings.HasPrefix(a, "-T") && len(a) > 2 &&
			!strings.HasPrefix(a, "-Ttext=") && !strings.HasPrefix(a, "-Tdata=") && !strings.HasPrefix(a, "-Tbss="):
			return a[2:]
		default:
			if v, ok := bareFlagValue(a); ok {
				return v
			}
		}
	}
	return ""
}

// resolveLibFlag maps a `-l` argument to a file in one of the `-L`
// directories, but only when that file is something nixgg produced.
//
// `name` is the text after `-l`. Two spellings:
//   - `-lfoo`        → name "foo",        look for lib<name>.a
//   - `-l:libfoo.a`  → name ":libfoo.a",  look for that exact filename
//
// The `:` form is how build systems pin a static archive when a shared
// one also exists; ld takes the name literally rather than expanding
// lib…/.a. ffmpeg and some autotools projects emit it.
//
// Returns "" when nothing matched, or when the match is a file we did
// not create — a vendored or system archive must stay a `-l` flag so
// the linker resolves it normally.
func resolveLibFlag(name string, libDirs []string) string {
	if name == "" {
		return ""
	}
	var files []string
	if strings.HasPrefix(name, ":") {
		exact := name[1:]
		if exact == "" || strings.ContainsRune(exact, filepath.Separator) {
			return ""
		}
		files = []string{exact}
	} else {
		// Order matters: ld searches lib<name>.so before lib<name>.a in
		// each -L directory and takes the first hit (verified against
		// the real linker with -Wl,-t). Checking .a first would claim
		// the static archive for a `-lfoo` the linker would have
		// resolved to the shared object, silently changing what gets
		// linked.
		files = []string{"lib" + name + ".so", "lib" + name + ".a"}
	}
	for _, d := range libDirs {
		for _, f := range files {
			cand := filepath.Join(d, f)
			fi, err := os.Lstat(cand)
			if err != nil {
				continue
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				return cand
			}
			// Sandbox mode: drvref stub is a small regular file with
			// our magic header. batchpending.Is covers the deferred-
			// batch-member case: a still-pending compile matched via
			// -l rather than a direct path.
			if fi.Mode().IsRegular() && fi.Size() < 4096 {
				if drvref.Is(cand) || batchpending.Is(cand) {
					return cand
				}
			}
		}
	}
	return ""
}

// isLinkInput reports whether a token is a file the linker consumes,
// as opposed to a flag.
//
// A token we fail to recognize here does NOT fall through to
// Passthrough — parseLinkArgs files it under `flags`, so it gets baked
// into the drv as a bare relative path, which doesn't exist inside
// the sandbox. Recognizing a token is what routes it through
// classify.Target, which is the actual safety net: an unowned file
// classifies as Regular and triggers Passthrough.
//
// Anything starting with `-` is a flag, never a file. Without that
// guard `-l:libfoo.a` (the exact-name form of -l) has filepath.Ext
// ".a" and is mistaken for an archive; resolveLibFlag handles that
// form properly.
func isLinkInput(a string) bool {
	if a == "" || strings.HasPrefix(a, "-") {
		return false
	}
	// .o (object), .a (archive), .xo (redis's position-independent
	// object for its shared-object test modules), .lo (libtool object).
	ext := strings.ToLower(filepath.Ext(a))
	if ext == ".o" || ext == ".a" || ext == ".xo" || ext == ".lo" {
		return true
	}
	return isSharedLib(a)
}

// isSharedLib matches `libfoo.so` and versioned `libfoo.so.1[.2[.3]]`.
// Checked as a `.so` segment rather than a suffix so a file merely
// ending in a number (`foo.1`) doesn't match.
func isSharedLib(a string) bool {
	base := strings.ToLower(filepath.Base(a))
	if strings.HasSuffix(base, ".so") {
		return true
	}
	i := strings.Index(base, ".so.")
	if i < 0 {
		return false
	}
	for _, seg := range strings.Split(base[i+len(".so."):], ".") {
		if seg == "" {
			return false
		}
		for _, r := range seg {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func altStorePrefix(storeURL string) string {
	const prefix = "local?root="
	if strings.HasPrefix(storeURL, prefix) {
		return strings.TrimPrefix(storeURL, prefix)
	}
	return ""
}

func joinBase(inputs []string) string {
	var b strings.Builder
	for i, in := range inputs {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(filepath.Base(in))
	}
	return b.String()
}

// linkSandbox handles NIXGG_SANDBOX=1: emit a JSON drv describing
// this link, hand it to `nix derivation add`, and submit the returned
// .drv as the current outer derivation's output.
//
// Every link in a sandbox is a candidate final output. `nix store
// submit-output` only allows one submission per output name, so a
// build with multiple links must arrange for only one to be "the"
// output (via NIXGG_SANDBOX_TARGET, matched against the -o output).
// Other link steps just add their drvs to the store without
// submitting; the consumer's `builtins.outputOf` reaches them
// transitively through the target's inputs.drvs.
//
// A multi-target build needs this link's own drv NAMED to match
// Nix's outputPathName($name, outputKey) check, not just submitted
// under the right key — see LinkJSONParams.Name's docstring for why
// "bin-<outName>" alone isn't enough once there's more than one
// target sharing an outer wrapper.
func linkSandbox(
	cfg *toolchain.Config,
	tool dispatch.Tool,
	output string,
	inputs []expr.JSONDrvInput,
	extraInputs []expr.JSONDrvInput,
	flags []string,
	group bool,
	wholeArchiveInputs []string,
	inlineFilesStore string,
	absFilePath string,
	absFileContent string,
	storeDeps []string,
	wrapperEnvJSON string,
) error {
	outName := filepath.Base(output)
	wrapperEnv, err := decodeStringMap(wrapperEnvJSON)
	if err != nil {
		return err
	}
	extraSrcs := []string{
		baseNameOf(cfg.BashRoot),
		baseNameOf(cfg.CoreutilsRoot),
		baseNameOf(cfg.CompilerRoot),
	}
	if inlineFilesStore != "" {
		extraSrcs = append(extraSrcs, baseNameOf(inlineFilesStore))
	}
	name := "bin-" + outName
	if override := multiTargetName(output); override != "" {
		name = override
	}
	drv := expr.LinkJSON(expr.LinkJSONParams{
		Name:               name,
		OutName:            outName,
		System:             cfg.System,
		Bash:               cfg.BashRoot,
		Coreutils:          cfg.CoreutilsRoot,
		Compiler:           cfg.CompilerRoot,
		Tool:               tool.Basename(),
		Inputs:             inputs,
		ExtraInputs:        extraInputs,
		Flags:              flags,
		GroupInputs:        group,
		WholeArchiveInputs: wholeArchiveInputs,
		InlineFilesStore:   inlineFilesStore,
		AbsFilePath:        absFilePath,
		AbsFileContent:     absFileContent,
		StoreDeps:          storeDeps,
		Placeholder:        "/" + expr.OutPlaceholderNix32,
		ExtraSrcs:          extraSrcs,
		Env:                wrapperEnv,
	})

	drvPath, err := sandbox.DerivationAdd(cfg, drv)
	if err != nil {
		return err
	}
	if err := sandbox.PointOutputAtDrv(output, drvPath); err != nil {
		return err
	}
	logf("  drv:        %s", drvPath)
	activitylog.Emit("link", "drv", activitylog.Fields{"output": output, "drv": drvPath, "inputs": inputs})

	// Single-target builds: mkNixggBuild names the outer drv
	// "bin-<target>.drv" to match our inner link drv's name, so no
	// rename is needed there.
	maybeSubmit(cfg, drvPath, output, true)
	return nil
}

// matchesTarget returns true if `target` (which may be a basename,
// a relative path, or an absolute path) refers to `output`.
//
// The basename fallback ONLY fires when target itself has no
// directory component — a caller that declared a bare name expects it
// to match wherever the real build put it. If target is itself a
// relative/absolute PATH (contains a "/"), matching drops to exact/
// absolute comparison only — necessary because Linux Kbuild's
// recursive build produces many archives sharing a basename
// ("lib.a" at both lib/lib.a and arch/x86/lib/lib.a); a path-aware
// target still falling through to basename comparison would match
// both, and maybeSubmit would try to submit the same output key
// twice for two different drvs.
func matchesTarget(target, output string) bool {
	if target == output {
		return true
	}
	if abs, err := filepath.Abs(output); err == nil && target == abs {
		return true
	}
	if target == filepath.Base(target) && filepath.Base(target) == filepath.Base(output) {
		return true
	}
	return false
}
