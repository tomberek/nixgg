package shim

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tbereknyei/nixgg/internal/classify"
	"github.com/tbereknyei/nixgg/internal/expr"
	"github.com/tbereknyei/nixgg/internal/mode"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/sandbox"
	"github.com/tbereknyei/nixgg/internal/scan"
	"github.com/tbereknyei/nixgg/internal/stage"
	"github.com/tbereknyei/nixgg/internal/storedeps"
	"github.com/tbereknyei/nixgg/internal/toolchain"
)

// Rustc is the shim entrypoint for a rustc crate compile.
//
// The unit of work is a crate, not a translation unit: one invocation
// reads a source tree and emits one artifact per `--emit`. Both ends
// differ from the compile shim, so this shares the staging and drv
// machinery with it but none of the argv handling.
//
// Reached through PATH, unlike every other shim: Kbuild's RUSTC
// defaults to a bare `rustc` and nixpkgs doesn't override it — that's
// what realRustc has to guard against.
func Rustc(args []string, cfg *toolchain.Config, l paths.Layout) error {
	real, err := realRustc()
	if err != nil {
		return err
	}
	if bypassed() || (!sandbox.Enabled() && !sandbox.EagerDrv()) {
		return Passthrough(real, args)
	}

	rc, why, ok := parseRustcArgs(args)
	if !ok {
		// Quiet by default: builds ask rustc questions constantly
		// (--print file-names, --version --verbose, -Zunpretty) and
		// every one lands here. A non-empty reason means something
		// else — a command line the parser doesn't understand — and
		// that must be visible, or the build quietly never accelerates.
		if why != "" {
			logf("rustc passthrough: %s", why)
		}
		return Passthrough(real, args)
	}

	if !isRegularFile(rc.Source) {
		logf("rustc passthrough: %s is not a regular file", rc.Source)
		return Passthrough(real, args)
	}

	// A proc-macro crate's output is dlopen'd by rustc itself when a
	// dependent crate expands its macros. Defer it and that dependent's
	// scan goes blind: an unresolvable proc-macro never expands, so any
	// file it would've pulled in via `include!` is missing from
	// dep-info. Leaving proc-macros as real files on disk keeps every
	// other scan honest. There are few of them, and they're host-only.
	if rc.ProcMacro {
		logf("rustc passthrough: %s is a proc-macro crate (must be loadable at scan time)", rc.Source)
		return Passthrough(real, args)
	}

	if mode.For(rc.Source) == mode.Passthrough {
		logf("rustc passthrough: caller declared this subtree")
		return Passthrough(real, args)
	}
	for _, e := range rc.Emits {
		if carvedOut(e.Path) {
			logf("rustc passthrough: %s is in a carved-out subtree", e.Path)
			return Passthrough(real, args)
		}
	}

	logf("rustc %s -> %s", rc.Source, joinBase(rc.emitPaths()))

	externs, ok, err := resolveCrateInputs(cfg, rc, l)
	if err != nil {
		return err
	}
	if !ok {
		return Passthrough(real, args)
	}

	// 1. Discover the crate's sources. rustc answers this itself — see
	// scan.RunRust for why the errors it prints along the way aren't a
	// problem.
	res, err := scan.RunRust(l, real, rc.Source, rc.ScanFlags)
	if err != nil {
		logf("rustc passthrough: scan failed: %v", err)
		return Passthrough(real, args)
	}

	// The caller asked rustc for a dep file, and the derivation won't
	// write one — the emit flags were replaced. Kbuild's
	// `if_changed_dep` runs fixdep over it and hard-fails without it;
	// the scan already resolved the exact source set.
	if rc.DepFile != "" {
		if err := writeRustDepFile(rc.DepFile, rc.Emits[0].Path, rc.Source, res.Headers); err != nil {
			return err
		}
	}

	// 2. Stage them. The crate root's own staged position is its place
	// under the project root, exactly like the modules around it —
	// unless it's already store content, in which case it stays where
	// it is and is mounted rather than copied (Rust's own `core` is
	// compiled straight out of the rustc source tree that way).
	srcAbs, err := filepath.Abs(rc.Source)
	if err != nil {
		return err
	}
	// Flags naming a file the compile reads — a custom target spec —
	// get their own store object; the scan can't see them, and the
	// filename has to survive intact. See storeFlagFiles.
	flags, flagDeps, err := storeFlagFiles(cfg, rc.Flags)
	if err != nil {
		return err
	}

	srcRel := srcAbs
	entries := make([]stage.Entry, 0, 1+len(res.Headers))
	if !strings.HasPrefix(srcAbs, "/nix/store/") {
		rel, err := filepath.Rel(res.ProjectRoot, srcAbs)
		if err != nil {
			return err
		}
		srcRel = rel
		entries = append(entries, stage.Entry{Abs: srcAbs, Rel: rel})
	}
	for _, h := range res.Headers {
		entries = append(entries, stage.Entry{Abs: h.Abs, Rel: h.Rel})
	}

	// Key the staging dir on the first emit's project-relative path,
	// the same rule the compile shim uses: unique across the project,
	// identical whatever absolute prefix the build tree sits under.
	crateKey := rc.Emits[0].Path
	if abs, err := filepath.Abs(crateKey); err == nil {
		if rel, err := filepath.Rel(res.ProjectRoot, abs); err == nil && !strings.HasPrefix(rel, "..") {
			crateKey = rel
		}
	}
	crateID := stage.TUID(crateKey)
	if _, err := stage.Sources(l, crateID, entries); err != nil {
		return err
	}
	srcStore, err := sandbox.StoreAddScan(cfg, crateID, filepath.Join(l.Srcs, crateID))
	if err != nil {
		return fmt.Errorf("stage crate to store: %w", err)
	}

	// storeDeps is computed BEFORE any spill: once the flags collapse
	// to a single `@<path>`, the store paths they mentioned are no
	// longer visible to scan for, and anything they referred to would
	// stop being mounted.
	storeDeps := append(storedeps.From(flags, "", cfg.KnownStorePaths), flagDeps...)
	flags, argfile, err := spillFlagsToArgfile(cfg, flags)
	if err != nil {
		return err
	}
	if argfile != "" {
		storeDeps = append(storeDeps, argfile)
	}

	// 3. Build the drv. The emit names are basenames: every artifact
	// lands flat in $out, and downstream stubs name it from there.
	emits := make([]expr.RustEmit, 0, len(rc.Emits))
	for _, e := range rc.Emits {
		emits = append(emits, expr.RustEmit{Kind: e.Kind, Name: filepath.Base(e.Path)})
	}
	name := "rs-" + filepath.Base(rc.Emits[0].Path)
	drv := expr.RustcJSON(expr.RustcJSONParams{
		Name:      name,
		System:    cfg.System,
		Bash:      cfg.BashRoot,
		Coreutils: cfg.CoreutilsRoot,
		RustcBin:  real,
		SrcStore:  srcStore,
		Source:    srcRel,
		Flags:     flags,
		Externs:   externs,
		Emits:     emits,
		StoreDeps: storeDeps,
		Env:       rustcEnv(res.ProjectRoot, srcStore),
		ExtraSrcs: []string{
			baseNameOf(cfg.BashRoot),
			baseNameOf(cfg.CoreutilsRoot),
			baseNameOf(toolRootOf(real)),
			baseNameOf(srcStore),
		},
	})
	drvPath, err := sandbox.DerivationAdd(cfg, drv)
	if err != nil {
		return err
	}

	// 4. One stub per emit, all naming the same drv. A consumer
	// resolves the stub to the drv and reaches its own artifact by
	// filename inside that output — which is why the metadata a
	// dependent crate resolves `--extern` against needs no derivation
	// of its own.
	for _, e := range rc.Emits {
		if err := sandbox.PointOutputAtDrv(e.Path, drvPath); err != nil {
			return err
		}
	}
	logf("  drv:        %s", drvPath)
	return nil
}

// rustcEmit is one `--emit=<kind>=<path>` the caller asked for.
type rustcEmit struct {
	Kind string
	Path string
}

// rustcCall is a parsed rustc command line, reduced to what a
// derivation needs: the crate root, the artifacts to emit, the
// dependencies to bind, and everything else passed through.
type rustcCall struct {
	Source  string
	Emits   []rustcEmit
	Externs []rustcExtern
	// LibDirs are the caller's `-L [KIND=]PATH` search dirs, used to
	// resolve a bare `--extern <crate>` to a file. They don't survive
	// into the derivation: the dirs are the build tree.
	LibDirs []string
	// Flags is everything not consumed above, in the caller's order.
	Flags []string
	// ScanFlags is the caller's argv verbatim, minus the source. The
	// dependency scan needs the ORIGINAL command line, not the reduced
	// one: a scan without `-L`/`--extern` can't resolve `core` under
	// `--sysroot=/dev/null`, and rustc then fails at the driver level,
	// before writing any dep-info at all.
	ScanFlags []string
	ProcMacro bool
	// DepFile is the `--emit=dep-info=<path>` the caller asked for, if
	// any. Written by the shim rather than the derivation.
	DepFile string
}

type rustcExtern struct {
	Crate string
	Path  string // empty when the caller left it to -L resolution
}

func (rc *rustcCall) emitPaths() []string {
	out := make([]string, 0, len(rc.Emits))
	for _, e := range rc.Emits {
		out = append(out, e.Path)
	}
	return out
}

// rustcSeparatedFlags take their value as the NEXT argv element rather
// than after an `=`.
//
// An explicit list, and its completeness matters more than it looks. A
// separated flag missing here leaves its value looking like a bare
// operand, the parser sees two positionals where it expects one, and
// the whole invocation drops to passthrough. That fails SAFE — the
// build runs unaccelerated rather than wrong — but it's invisible, so
// it costs a full build to notice. `--remap-path-prefix` is here
// because it cost exactly that.
//
// Short options rustc also accepts attached (-Copt-level=2) appear
// here in their separated spelling; the attached form is handled by
// prefix matching in takeFlag's caller.
var rustcSeparatedFlags = map[string]bool{
	"--emit": true, "--extern": true, "-L": true, "-o": true,
	"--out-dir": true, "--crate-type": true, "--print": true,
	"--sysroot": true, "--target": true, "--crate-name": true,
	"--edition": true, "--cfg": true, "--check-cfg": true,
	"--cap-lints": true, "--error-format": true, "--json": true,
	"--color": true, "--diagnostic-width": true, "--explain": true,
	"--remap-path-prefix": true, "--env-set": true, "--extern-location": true,
	"--codegen": true, "--allow": true, "--warn": true,
	"--deny": true, "--forbid": true,
	"-C": true, "-Z": true, "-W": true, "-A": true, "-D": true,
	"-F": true, "-l": true,
}

// parseRustcArgs recognises a crate compile whose artifacts are all
// named explicitly, and rejects everything else.
//
// Naming is the whole test. `--emit=obj` without a path leaves rustc
// to choose the filename from the crate name and type, and a
// derivation can't write a stub for a file it can't name. Builds that
// do this pair it with `--out-dir`, asking for a directory of
// artifacts rather than a set of known ones — a different shape, not
// modelled here.
//
// Also rejected: `--print`/`--version` (questions, not artifacts — a
// build runs several before compiling anything), `-Zunpretty` (writes
// to stdout), and `-` as the source (stdin).
//
// The returned reason distinguishes the two kinds of rejection.
// "Question" invocations are expected and constant; anything else
// means a command line this parser doesn't understand, and the caller
// says so — a silent bail there is a build that quietly never
// accelerates.
func parseRustcArgs(args []string) (*rustcCall, string, bool) {
	rc := &rustcCall{}
	var positional []string
	positionalAt := map[int]bool{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case rustcSeparatedFlags[a]:
			if i+1 >= len(args) {
				return nil, "", false
			}
			if !rc.takeFlag(a, args[i+1]) {
				return nil, "", false
			}
			i++
		case strings.HasPrefix(a, "--"):
			eq := strings.IndexByte(a, '=')
			if eq < 0 {
				if !rc.takeFlag(a, "") {
					return nil, "", false
				}
				continue
			}
			if !rc.takeFlag(a[:eq], a[eq+1:]) {
				return nil, "", false
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			// Attached short option (-Cfoo, -Zbar, -Lpath).
			if !rc.takeFlag(a[:2], a[2:]) {
				return nil, "", false
			}
		default:
			positional = append(positional, a)
			positionalAt[i] = true
		}
	}
	if len(positional) == 1 && positional[0] == "-" {
		return nil, "", false // stdin: a probe, not a crate
	}
	if len(rc.Emits) == 0 {
		return nil, "", false
	}
	if len(positional) != 1 {
		// More than one operand means a separated flag's value was
		// taken for a source — see rustcSeparatedFlags. Name them:
		// this is the one bail that indicates a gap here rather than
		// an invocation we deliberately ignore.
		return nil, fmt.Sprintf("expected one source, got %d operands (%v) — "+
			"a two-argument flag is probably missing from rustcSeparatedFlags",
			len(positional), positional), false
	}
	rc.Source = positional[0]
	for i, a := range args {
		if !positionalAt[i] {
			rc.ScanFlags = append(rc.ScanFlags, a)
		}
	}
	return rc, "", true
}

// takeFlag consumes one flag and its value, returning false when the
// flag means this invocation can't be modelled at all. value == ""
// means the flag stood alone on the command line.
func (rc *rustcCall) takeFlag(flag, value string) bool {
	switch flag {
	case "--print", "--version", "-V", "--help":
		return false
	case "-o", "--out-dir":
		// Replaced by the derivation's own output layout.
		return true
	case "--emit":
		// A comma-separated list of `kind[=path]`. dep-info is the
		// build's own bookkeeping and is regenerated below; every
		// other kind must name its file.
		for _, spec := range strings.Split(value, ",") {
			kind, path, named := strings.Cut(spec, "=")
			if kind == "dep-info" {
				rc.DepFile = path
				continue
			}
			if !named || path == "" {
				return false
			}
			rc.Emits = append(rc.Emits, rustcEmit{Kind: kind, Path: path})
		}
		return true
	case "--extern":
		crate, path, _ := strings.Cut(value, "=")
		rc.Externs = append(rc.Externs, rustcExtern{Crate: crate, Path: path})
		return true
	case "-L":
		// `-L [KIND=]PATH`; KIND is one of dependency/crate/native/….
		dir := value
		if _, rest, ok := strings.Cut(value, "="); ok {
			dir = rest
		}
		rc.LibDirs = append(rc.LibDirs, dir)
		return true
	case "--crate-type":
		if value == "proc-macro" {
			rc.ProcMacro = true
		}
		rc.Flags = append(rc.Flags, flag, value)
		return true
	case "-Z":
		if strings.HasPrefix(value, "unpretty") {
			return false
		}
		rc.Flags = append(rc.Flags, flag+value)
		return true
	case "-C", "-W", "-A", "-D", "-F", "-l":
		// Short options rustc accepts attached or separated; emit the
		// attached form so both spellings produce the same derivation.
		rc.Flags = append(rc.Flags, flag+value)
		return true
	}
	if value == "" {
		rc.Flags = append(rc.Flags, flag)
	} else {
		rc.Flags = append(rc.Flags, flag+"="+value)
	}
	return true
}

// resolveCrateInputs turns the crates this compile can reach into
// derivation inputs: the ones the caller named with `--extern`, plus
// every crate sitting in its `-L` search dirs.
//
// The named ones are resolved here rather than in the sandbox because
// the `-L` dirs rustc would search are the build tree: absent there,
// and holding drvref stubs outside it.
//
// The unnamed ones are needed because a crate's own dependencies
// travel in its metadata, not the command line — a kernel driver
// names two externs and rustc then loads ten crates. That over-
// approximates (a crate that happens to sit next to a real dependency
// becomes an input too), but search dirs hold a project's own crates
// and little else, so the cost is a few extra edges, not a rebuild
// storm — the alternative is asking rustc, which only answers under
// `-Zbinary_dep_depinfo=y`, a flag the caller may not pass.
//
// A crate that can't be modelled fails the whole invocation into
// passthrough rather than being dropped: dropping one would produce a
// derivation that compiles against a dependency Nix doesn't know
// about, which fails inside the sandbox naming the crate rather than
// the missing edge.
func resolveCrateInputs(cfg *toolchain.Config, rc *rustcCall, l paths.Layout) ([]expr.JSONDrvInput, bool, error) {
	var out []expr.JSONDrvInput
	seen := map[string]bool{}

	add := func(path, crate string) bool {
		abs, err := filepath.Abs(path)
		if err != nil {
			abs = path
		}
		if seen[abs] && crate == "" {
			return true // already pulled in as a search-path entry
		}
		seen[abs] = true

		c := classify.Target(path, altStorePrefix(cfg.Store), l)
		switch c.Kind {
		case classify.Drv:
			out = append(out, expr.JSONDrvInput{
				Kind: "drv", Ref: c.Ref, Name: filepath.Base(path), Crate: crate,
			})
		case classify.Store:
			out = append(out, expr.JSONDrvInput{
				Kind: "src", Ref: expr.StoreBasename(c.Ref), Name: filepath.Base(path), Crate: crate,
			})
		case classify.Regular:
			// A crate nixgg didn't produce — a proc-macro left as a
			// real file, or one built before the shims were on PATH.
			// Store it and depend on its content: bailing would make
			// THIS crate passthrough, making every crate above it
			// unmodellable in turn.
			sp, err := storeAddLooseFile(cfg, path)
			if err != nil {
				logf("  passthrough: store-add %s failed: %v", path, err)
				return false
			}
			out = append(out, expr.JSONDrvInput{
				Kind: "src", Ref: filepath.Base(sp), Name: filepath.Base(path), Crate: crate,
			})
		default:
			logf("  passthrough: can't model crate %s (%s)", path, c.Reason())
			return false
		}
		return true
	}

	for _, ex := range rc.Externs {
		path := ex.Path
		if path == "" {
			p, ok := findCrateFile(rc.LibDirs, ex.Crate)
			if !ok {
				logf("  passthrough: no file for --extern %s in %v", ex.Crate, rc.LibDirs)
				return nil, false, nil
			}
			path = p
		}
		if !add(path, ex.Crate) {
			return nil, false, nil
		}
	}
	for _, p := range crateFilesIn(rc.LibDirs) {
		if !add(p, "") {
			return nil, false, nil
		}
	}
	return out, true, nil
}

// crateFilesIn lists every crate file in the given search dirs, in a
// deterministic order — the drv's content depends on it.
func crateFilesIn(dirs []string) []string {
	var out []string
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			n := e.Name()
			if !strings.HasPrefix(n, "lib") || e.IsDir() {
				continue
			}
			if strings.HasSuffix(n, ".rlib") || strings.HasSuffix(n, ".rmeta") ||
				strings.HasSuffix(n, ".so") {
				out = append(out, filepath.Join(d, n))
			}
		}
	}
	sort.Strings(out)
	return out
}

// findCrateFile mirrors rustc's own search for a bare `--extern
// <crate>`: each -L dir in order, rlib before rmeta before dylib.
func findCrateFile(dirs []string, crate string) (string, bool) {
	for _, d := range dirs {
		for _, ext := range []string{".rlib", ".rmeta", ".so"} {
			p := filepath.Join(d, "lib"+crate+ext)
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
				return p, true
			}
		}
	}
	return "", false
}

// realRustc resolves the compiler this shim stands in for.
//
// Unlike every other shim, this one is normally reached through PATH
// rather than an explicit `RUSTC=` — Kbuild's default is a bare
// `rustc`, and nixpkgs doesn't override it. So the fallback can't be a
// PATH lookup for "rustc": that finds this shim again and the build
// forks until it dies. Skip any candidate that is this executable.
//
// No sibling-of-cc fallback either: a Rust build pins its toolchain —
// crate metadata can only be read by the rustc that wrote it — so
// guessing isn't a recoverable error.
func realRustc() (string, error) {
	if v := os.Getenv("NIXGG_REAL_RUSTC"); v != "" {
		return v, nil
	}
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("rustc shim: cannot determine own path (%w); "+
			"refusing to search PATH, which would resolve back to this shim — set NIXGG_REAL_RUSTC", err)
	}
	if resolved, rerr := filepath.EvalSymlinks(self); rerr == nil {
		self = resolved
	}
	selfInfo, _ := os.Stat(self)

	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		cand := filepath.Join(dir, "rustc")
		st, serr := os.Stat(cand)
		if serr != nil || st.IsDir() || st.Mode()&0o111 == 0 {
			continue
		}
		// Two independent identity checks: SameFile catches a hardlink
		// or a differently-spelled path to the same binary; the
		// EvalSymlinks comparison catches the symlink farm
		// mkNixggBuild actually lays out.
		if selfInfo != nil && os.SameFile(st, selfInfo) {
			continue
		}
		if resolved, rerr := filepath.EvalSymlinks(cand); rerr == nil && resolved == self {
			continue
		}
		return cand, nil
	}
	// Deliberately an error, not a bare "rustc": returning the literal
	// would be re-resolved through PATH by exec, finding this shim
	// again — the very thing the loop above rules out.
	return "", fmt.Errorf("rustc shim: no rustc on PATH other than this shim; set NIXGG_REAL_RUSTC")
}

// writeRustDepFile emits the make-format fragment rustc would have
// written, reconstructed from the scan. Same shape as the compile
// shim's: `target: prereq…`. Store-path dependencies are absent
// because the scan drops them — they're immutable, so a rebuild
// trigger on them would never fire anyway.
func writeRustDepFile(path, target, source string, sources []scan.Header) error {
	var b strings.Builder
	b.WriteString(target)
	b.WriteString(": ")
	b.WriteString(source)
	for _, s := range sources {
		b.WriteString(" \\\n  ")
		b.WriteString(s.Abs)
	}
	b.WriteString("\n")

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("depfile dir %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("write depfile %s: %w", path, err)
	}
	return nil
}

// rustcFileFlags are flags whose value is a FILE the compile reads, so
// it has to travel into the derivation like a source.
//
// An explicit list, for the reason objcopyTwoArg is one: a flag's
// value isn't self-identifying, and a wrong guess here either drops a
// real input or rewrites something that was never a path. The scan
// can't find these — rustc's dep-info reports what the CRATE reads,
// and a target specification is read by the driver before any crate
// exists.
//
//	--target   a custom target spec (--target=…/scripts/target.json).
//	           Also spelled as a builtin triple name, which is not a
//	           file and is left alone.
var rustcFileFlags = map[string]bool{"--target": true}

// storeFlagFiles gives each file-valued flag its own store object and
// rewrites the flag to point there.
//
// A store object rather than a staged tree entry, because the
// FILENAME is load-bearing and staging doesn't preserve it: rustc
// names a custom target after its spec file's stem, and a compile's
// own metadata is only loadable by a compile whose target has the
// same name. storeAddLooseFile keeps the basename intact inside its
// own directory, so the stem stays put however the file got there.
func storeFlagFiles(cfg *toolchain.Config, flags []string) ([]string, []string, error) {
	out := make([]string, len(flags))
	copy(out, flags)
	var deps []string
	for i, f := range out {
		name, value, ok := strings.Cut(f, "=")
		if !ok || !rustcFileFlags[name] {
			continue
		}
		abs, err := filepath.Abs(value)
		if err != nil || !isRegularFile(abs) {
			continue
		}
		if strings.HasPrefix(abs, "/nix/store/") {
			continue // already content-addressed and mounted
		}
		sp, err := storeAddLooseFile(cfg, abs)
		if err != nil {
			return nil, nil, fmt.Errorf("store %s value %s: %w", name, abs, err)
		}
		out[i] = name + "=" + filepath.Join(sp, filepath.Base(abs))
		deps = append(deps, sp)
	}
	return out, deps, nil
}

// MaxInlineFlagBytes caps how much flag text may be rendered directly
// into a derivation's build script.
//
// The script is one argument to `bash -c`, and the kernel's execve
// caps a single argument at MAX_ARG_STRLEN — 32 pages, 131072 bytes, a
// compile-time constant with no runtime knob. Exceeding it fails the
// builder with "Argument list too long".
//
// Rust reaches this where C does not: a kernel's generated cfg file is
// hundreds of KB — one `--cfg=CONFIG_…` per config symbol — and rustc
// takes it as an @-file precisely so it never has to be a command
// line. Expanding it inline to model the compile puts all of it back
// into one argument.
const MaxInlineFlagBytes = 32 << 10

// spillFlagsToArgfile moves an oversized flag list into a store object
// and returns the flags replaced by a single `@<path>` referring to
// it, plus that path so the caller can mount it.
//
// Not a size trick at the expense of correctness: rustc reads @-files
// one argument per line, the object is content-addressed, and it
// enters the derivation as an input — the compile still depends on
// every flag byte exactly as it did inline. Two crates with identical
// flags share one object.
//
// Below the threshold nothing changes, so small builds keep the
// legible inline form and their existing drv hashes.
func spillFlagsToArgfile(cfg *toolchain.Config, flags []string) ([]string, string, error) {
	n, fits := flagsFitInline(flags)
	if fits {
		return flags, "", nil
	}

	dir, err := os.MkdirTemp(os.Getenv("NIX_BUILD_TOP"), "gg-rustargs-")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(dir)
	// One argument per line — the whole of rustc's @-file grammar, see
	// dispatch.ExpandRustArgfiles.
	path := filepath.Join(dir, "rustc-args")
	if err := os.WriteFile(path, []byte(strings.Join(flags, "\n")+"\n"), 0o644); err != nil {
		return nil, "", err
	}
	sp, err := storeAddLooseFile(cfg, path)
	if err != nil {
		return nil, "", err
	}
	logf("  argfile:    %d flags (%d bytes) -> %s", len(flags), n, sp)
	return []string{"@" + sp + "/rustc-args"}, sp, nil
}

// flagsFitInline reports the rendered size of a flag list and whether
// it may go into the build script directly.
func flagsFitInline(flags []string) (int, bool) {
	n := 0
	for _, f := range flags {
		n += len(f) + 1
	}
	return n, n <= MaxInlineFlagBytes
}

func isRegularFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}
