// Package scan enumerates the header set for a compile invocation by
// running `<cc> -MM -MG` and parsing the emitted makefile fragment.
//
// Results are cached under .nixgg/scans/<key>.{deps,out} where <key> is
// sha256(compiler + source + flags). Cache is invalidated when any
// file in the recorded dep list has an mtime newer than what we saw.
//
// Two callers of this package:
//   - The compile shim: needs the header list + the "project root" (a
//     common ancestor of cwd + every user-supplied -I dir) to stage
//     sources; also needs the -I flags rewritten to be relative to
//     that project root, and the store-prefixed -I flags kept verbatim.
//   - The scan cache itself doesn't need to be exported, but the
//     Result type is.
package scan

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tbereknyei/nixgg/internal/paths"
)

// Header is a discovered dependency: `Abs` is the current on-disk
// location, `Rel` is where it should appear inside the staging dir
// (relative to the project root that Sources chose).
type Header struct {
	Abs, Rel string
}

// Result is the full output of a scan.
type Result struct {
	Headers []Header
	// ProjectRoot is the common ancestor of cwd + every user -I dir.
	// The staging dir mirrors this — headers land at
	//   $stagingDir/<rel-to-project-root>.
	ProjectRoot string
	// StagedIFlags are `-I<rel>` flags to be passed to gcc inside the
	// sandbox, relative to the staged project root. Always includes
	// "-I." so the source directory is on the include path.
	StagedIFlags []string
	// StoreIFlags are `-I/nix/store/...` flags passed verbatim (system
	// dependencies from wrapper env).
	StoreIFlags []string
	// StagedIncludeFlags are `-include <rel>` flags rewritten to point
	// at the staged copy of each force-included header, relative to the
	// staged project root. Emitted by rewriteFlags AFTER the -I flags so
	// the header resolves against the staged tree.
	//
	// A force-included header that resolves outside projectRoot (a
	// system or store path) is passed through verbatim instead — see
	// the loop that builds this.
	StagedIncludeFlags []string
}

// Run scans a single compile invocation.
//   - cc: absolute path to the compiler (matches TOOL — cc, gcc, c++, g++)
//   - source: caller's source file (relative to cwd is fine)
//   - flags: everything else on argv (no -c/-o).
//
// Uses the cache under l.Scans if all deps mtimes match.
func Run(l paths.Layout, cc, source string, flags []string) (*Result, error) {
	if err := os.MkdirAll(l.Scans, 0o755); err != nil {
		return nil, err
	}
	key := cacheKey(cc, source, flags)
	if r, ok := readCache(l, key); ok {
		return r, nil
	}
	r, deps, err := runScanner(cc, source, flags)
	if err != nil {
		return nil, err
	}
	writeCache(l, key, r, deps)
	return r, nil
}

// readCache returns the memoised Result for key when every file it was
// derived from still has the mtime the scan saw.
func readCache(l paths.Layout, key string) (*Result, bool) {
	if fresh, err := depsStillFresh(filepath.Join(l.Scans, key+".deps")); err != nil || !fresh {
		return nil, false
	}
	body, err := os.ReadFile(filepath.Join(l.Scans, key+".out"))
	if err != nil {
		return nil, false
	}
	var r Result
	if err := decodeResult(body, &r); err != nil {
		return nil, false
	}
	return &r, true
}

// writeCache is best-effort: a scan that cannot be memoised still
// returns its result, it just costs the same again next time. The deps
// file is written only after the result, so a half-written cache is
// never treated as fresh.
func writeCache(l paths.Layout, key string, r *Result, deps []depEntry) {
	body, err := encodeResult(r)
	if err != nil {
		return
	}
	if err := writeAtomic(filepath.Join(l.Scans, key+".out"), body); err == nil {
		_ = writeAtomic(filepath.Join(l.Scans, key+".deps"), encodeDeps(deps))
	}
}

// depEntry is one line in the .deps file: <abs>\t<mtime-nsec>.
type depEntry struct {
	abs   string
	mtime int64 // unix nanoseconds; -1 if the file was missing when scanned
}

func depsStillFresh(path string) (bool, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	// Format: one line per file, `<abs>\t<mtime-nsec>`. Batch the stats
	// to keep the syscall count linear.
	lines := bytes.Split(bytes.TrimRight(body, "\n"), []byte{'\n'})
	for _, line := range lines {
		tab := bytes.IndexByte(line, '\t')
		if tab < 0 {
			return false, errors.New("malformed .deps line")
		}
		abs := string(line[:tab])
		recorded := string(line[tab+1:])
		info, err := os.Stat(abs)
		if err != nil {
			return false, nil // vanished file → invalidate
		}
		cur := fmt.Sprintf("%d", info.ModTime().UnixNano())
		if cur != recorded {
			return false, nil
		}
	}
	return true, nil
}

func encodeDeps(entries []depEntry) []byte {
	var b bytes.Buffer
	for _, e := range entries {
		fmt.Fprintf(&b, "%s\t%d\n", e.abs, e.mtime)
	}
	return b.Bytes()
}

func cacheKey(cc, source string, flags []string) string {
	h := sha256.New()
	fmt.Fprintln(h, cc)
	fmt.Fprintln(h, source)
	// Preserve order — flag order can affect semantics.
	for _, f := range flags {
		fmt.Fprintln(h, f)
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// runScanner executes `gcc -MM -MG` and turns its output into a Result.
// The scanner runs in the caller's cwd; that's where relative headers
// live and where -I flags are interpreted.
func runScanner(cc, source string, flags []string) (*Result, []depEntry, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil, err
	}
	// Collect user-supplied include dirs from flags. Both attached
	// (-I/path) and separated (-I /path) forms; also -isystem, -iquote,
	// -idirafter, -include.
	includeDirs := extractIncludeDirs(flags)
	forceIncludes := extractForceIncludes(flags)

	// The project root is the common ancestor of cwd + the source
	// dir + every non-store include dir. Include the source dir
	// up-front so cmake-style out-of-tree builds — `cc -c ../src/x.cc`
	// from a `build/` directory — get a projectRoot that contains
	// the source. Without this, `filepath.Rel(cwd, srcAbs)` returns
	// `../src/x.cc`, which then stages the file *outside* the tuID
	// dir and produces a broken thunk that references a path with
	// `..` in it. We widen for headers later too.
	srcAbs, err := filepath.Abs(source)
	if err != nil {
		return nil, nil, err
	}
	// callerDirs = the caller's -I/-isystem/etc dirs. Emitted as
	// -I<rel> flags for the drv.
	// projectRootHints = every dir that projectRoot must cover, so
	// srcAbs's dir + cwd are included even if the caller didn't ask
	// for them via -I. Keeping the two lists separate is important:
	// srcDir is a projectRoot hint but must NOT become a spurious -I,
	// or ffmpeg's `-I. -Ilibavutil` (cwd=ffmpeg-root, srcDir=libavutil)
	// gets a bogus `-Ilibavutil` that shadows glibc's <time.h> with
	// ffmpeg's own libavutil/time.h.
	callerDirs := []string{}
	projectRootHints := []string{cwd, filepath.Dir(srcAbs)}
	storeDirs := []string{}
	for _, d := range includeDirs {
		abs := d
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(cwd, d)
		}
		if strings.HasPrefix(abs, "/nix/store/") {
			storeDirs = append(storeDirs, abs)
		} else {
			callerDirs = append(callerDirs, abs)
			projectRootHints = append(projectRootHints, abs)
		}
	}
	projectRoot := commonAncestor(projectRootHints)

	// Strip -M* dep-generation flags from the scanner's argv — we
	// supply our own -MM -MG -MF -.
	scanFlags := stripDepFlags(flags)

	// -M (not -MM): gcc's own documented distinction is that -MM omits
	// headers it classifies as "system headers" (anything gcc's own
	// preprocessor internally flags 3/3-4 in -E output, e.g. `# 1
	// "lib/stddef.h" 1 3 4`) — which is exactly the gnulib
	// substitute-header trick: a directory ahead of gcc's own
	// system-include search order supplies e.g. lib/stddef.h in place
	// of the real one, and the preprocessor still marks the RESULT
	// "system" because of *where* it was found, not how it was
	// #include-d. -MM then silently drops it, staging a source tree
	// missing that header — confirmed directly: `-MM -MG` omitted
	// hello's own lib/stddef.h from closeout.c's dep list (though NOT
	// from hello.c's — a different #include chain happened to avoid
	// the system marking there), producing "implicit declaration of
	// function 'gl_unreachable'" three build stages removed from the
	// real cause. -M includes every header regardless of that
	// classification; -MG (accept a missing header as a bare name
	// rather than erroring) still applies.
	cmd := exec.Command(cc, append([]string{"-M", "-MG", "-MF", "-", source}, scanFlags...)...)
	cmd.Stderr = nil // best-effort — we tolerate cc's warnings
	out, err := cmd.Output()
	if err != nil {
		// Show stderr on failure so users can see what went wrong.
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		return nil, nil, fmt.Errorf("scan-headers %s %s: %w\n%s", cc, source, err, stderr)
	}
	tokens := parseMakeDeps(out)

	// Assembly sources (.s/.S) can reference arbitrary build-tree files
	// via GNU as's `.incbin "path"` directive — Linux Kbuild's own
	// arch/x86/realmode/rmpiggy.S does exactly this, embedding a
	// previously-linked binary blob into a later object file. `.incbin`
	// is processed by the ASSEMBLER, after preprocessing, so gcc's own
	// `-M`/`-MM` (which only tracks what the PREPROCESSOR consumed —
	// #include, not .incbin) never sees it: confirmed directly, `cc -M
	// -MG` on a .S with an .incbin produces a dependency list that
	// silently omits the incbin'd file entirely, no warning either way.
	//
	// The assembler itself DOES track this, via its own, separate
	// `--MD=<path>` flag (binutils `as`'s own dependency-output option,
	// unrelated to gcc's `-M` family) — confirmed directly against a
	// real fixdep-adjacent case: `gcc -c foo.S -Wa,--MD=/dev/stdout -o
	// /dev/null` lists the .incbin target in its dependency output
	// alongside the source itself. Real compile (not just a probe): -o
	// /dev/null and dep output to /dev/stdout both avoid leaving files
	// behind, at the cost of a second real compile per assembly TU (a
	// scan.go cache hit, same as the -M/-MG pass, avoids paying this on
	// every rebuild).
	if isAssemblySource(source) {
		incTokens, err := scanIncbinTargets(cc, source, scanFlags)
		if err != nil {
			return nil, nil, err
		}
		tokens = append(tokens, incTokens...)
	}

	// Resolve each token: absolute path, or search cwd + user -I dirs.
	// Widen projectRoot to cover any dep found outside its current span.
	var resolved []string
	for _, tok := range tokens {
		if tok == filepath.Base(source) || tok == source {
			continue
		}
		abs, ok := resolveDep(tok, projectRootHints)
		if !ok {
			continue // -MG bare name we couldn't find; ignore
		}
		if abs == srcAbs {
			continue
		}
		if strings.HasPrefix(abs, "/nix/store/") {
			continue // store header, referenced verbatim via -I flag
		}
		resolved = append(resolved, abs)
	}

	// Widen project root to include every resolved header.
	for _, abs := range resolved {
		projectRoot = widen(projectRoot, filepath.Dir(abs))
	}

	// Now build the Result.
	seen := make(map[string]bool)
	var headers []Header
	var deps []depEntry
	// Source itself is a dep for cache invalidation but not a header.
	deps = append(deps, statOr(srcAbs))
	for _, abs := range resolved {
		if seen[abs] {
			continue
		}
		seen[abs] = true
		rel, err := filepath.Rel(projectRoot, abs)
		if err != nil {
			return nil, nil, err
		}
		headers = append(headers, Header{Abs: abs, Rel: rel})
		deps = append(deps, statOr(abs))
	}
	// Sort headers by staging rel path so thunk content is stable.
	sort.Slice(headers, func(i, j int) bool { return headers[i].Rel < headers[j].Rel })

	iflags := stagedIFlags(projectRoot, callerDirs)
	// Store dirs pass through unchanged.
	var storeFlags []string
	for _, d := range storeDirs {
		storeFlags = append(storeFlags, "-I"+d)
	}

	// `-include <file>` rewritten to point at the staged copy. The file
	// itself is already in `headers` (scan's -MM -MG saw it as a
	// dependency), so it lands in the staging tree at its
	// projectRoot-relative path; the flag has to follow it there.
	//
	// A force-include resolving to /nix/store or otherwise outside
	// projectRoot isn't staged, so pass it through verbatim — the
	// sandbox mounts store paths, and storedeps.From will pick up the
	// reference from the flag text.
	var includeFlags []string
	for _, f := range forceIncludes {
		abs := f
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(cwd, f)
		}
		if strings.HasPrefix(abs, "/nix/store/") {
			includeFlags = append(includeFlags, "-include", abs)
			continue
		}
		rel, err := filepath.Rel(projectRoot, abs)
		if err != nil || strings.HasPrefix(rel, "..") {
			// Outside the staged tree entirely; keep the absolute path
			// rather than emitting a relative path that won't resolve.
			includeFlags = append(includeFlags, "-include", abs)
			continue
		}
		includeFlags = append(includeFlags, "-include", rel)
	}

	return &Result{
		Headers:            headers,
		ProjectRoot:        projectRoot,
		StagedIFlags:       iflags,
		StoreIFlags:        storeFlags,
		StagedIncludeFlags: includeFlags,
	}, deps, nil
}

// stagedIFlags rewrites the caller's include dirs to be relative to the
// staged project root, and is the sole producer of Result.StagedIFlags.
//
// Only `callerDirs` — dirs the caller actually passed via
// -I/-isystem/-iquote/-idirafter — may become -I flags. cwd and the
// source's own directory are deliberately NOT included: they are
// projectRoot *hints* (see projectRootHints in runScanner) and nothing
// more.
//
// Regression origin: those two lists used to be one. Compiling
// ffmpeg's libavutil/parseutils.c from the ffmpeg root meant srcDir was
// libavutil/, so it leaked out as a spurious `-Ilibavutil`. Inside the
// drv (cwd = staged root) that resolved to $src/libavutil, whose own
// time.h then shadowed glibc's <time.h> — because -I dirs are searched
// before -isystem ones. Every TU reaching SmallVector-style code failed
// with "'struct tm' has no member".
//
// -I order is semantics, not style: the first directory containing
// the named file wins, so reordering changes which header a TU
// compiles against. Emit the caller's dirs in their original order,
// appending a synthetic -I. only if they never named the root.
func stagedIFlags(projectRoot string, callerDirs []string) []string {
	var iflags []string
	seenRel := map[string]bool{}
	for _, p := range callerDirs {
		rel := "."
		if p != projectRoot {
			r, err := filepath.Rel(projectRoot, p)
			if err != nil {
				continue
			}
			rel = r
		}
		if seenRel[rel] {
			continue
		}
		seenRel[rel] = true
		iflags = append(iflags, "-I"+rel)
	}
	// The staged root has to be reachable even when the caller never
	// named it — a bare `cc -c foo.c` relies on it. Last, so it cannot
	// shadow a directory the caller did name.
	if !seenRel["."] {
		iflags = append(iflags, "-I.")
	}
	return iflags
}

func statOr(abs string) depEntry {
	info, err := os.Stat(abs)
	if err != nil {
		return depEntry{abs: abs, mtime: -1}
	}
	return depEntry{abs: abs, mtime: info.ModTime().UnixNano()}
}

// extractIncludeDirs walks flags and pulls out user-supplied include
// paths from all the -I family (both attached and separated forms).
// extractIncludeDirs walks flags and pulls out user-supplied include
// DIRECTORIES. `-include` is deliberately absent from pathFlags: its
// value is a FILE to force-include, not a directory to search. Treating
// it as a dir emitted a bogus `-I<file>` (gcc: "not a directory") and,
// worse, made rewriteFlags drop the `-include` entirely — silently
// compiling with different preprocessor state. See extractForceIncludes.
func extractIncludeDirs(flags []string) []string {
	var out []string
	pathFlags := map[string]bool{
		"-I": true, "-isystem": true, "-iquote": true,
		"-idirafter": true,
	}
	for i := 0; i < len(flags); i++ {
		f := flags[i]
		if pathFlags[f] && i+1 < len(flags) {
			out = append(out, flags[i+1])
			i++
			continue
		}
		if strings.HasPrefix(f, "-I") && len(f) > 2 {
			out = append(out, f[2:])
		}
	}
	return out
}

// extractForceIncludes pulls out `-include <file>` values — headers the
// caller wants prepended to the TU before its own text, the standard
// autoconf/CMake way to inject a generated `config.h` full of
// HAVE_XXX defines.
//
// These must survive into the drv. The header itself is already staged
// (scan's own `gcc -MM -MG` reports it as a dependency like any other),
// but the FLAG has to be re-emitted pointing at the staged copy, or the
// TU compiles without those defines and the build succeeds with the
// wrong preprocessor state and no diagnostic at all.
//
// Only the separated `-include <file>` form exists; gcc has no
// `-include<file>` spelling, so there is no attached form to handle.
func extractForceIncludes(flags []string) []string {
	var out []string
	for i := 0; i < len(flags); i++ {
		if flags[i] == "-include" && i+1 < len(flags) {
			out = append(out, flags[i+1])
			i++
		}
	}
	return out
}

var depOneArg = map[string]bool{"-M": true, "-MM": true, "-MG": true, "-MP": true, "-MD": true, "-MMD": true}
var depTwoArg = map[string]bool{"-MF": true, "-MT": true, "-MQ": true}

// StripWpDep removes dependency-generation directives from a `-Wp,…`
// preprocessor-passthrough flag, returning the remainder and whether
// anything survived.
//
// A `-Wp,-MMD,<file>` redirects dependency output to that file, which
// silences the scanner's own `-M -MG -MF -`. It is equally invalid
// inside the derivation, for the reason parseCompileArgs documents for
// the bare -MD/-MMD forms.
//
// Shared with the compile shim so the two cannot disagree about which
// flags are dependency plumbing.
func StripWpDep(f string) (string, bool) {
	const pfx = "-Wp,"
	if !strings.HasPrefix(f, pfx) {
		return f, true
	}
	// Inside -Wp, the filename is a comma element rather than a separate
	// argv token, so -MD/-MMD take a value here even though their bare
	// argv spellings do not. Check the takes-a-value set first.
	wpTwoArg := map[string]bool{"-MMD": true, "-MD": true, "-MF": true, "-MT": true, "-MQ": true}
	wpOneArg := map[string]bool{"-M": true, "-MM": true, "-MG": true, "-MP": true}

	parts := strings.Split(f[len(pfx):], ",")
	var kept []string
	for i := 0; i < len(parts); i++ {
		p := parts[i]
		if wpTwoArg[p] {
			i++ // the filename rides along as the next comma element
			continue
		}
		if wpOneArg[p] {
			continue
		}
		kept = append(kept, p)
	}
	if len(kept) == 0 {
		return "", false
	}
	return pfx + strings.Join(kept, ","), true
}

func stripDepFlags(flags []string) []string {
	var out []string
	for i := 0; i < len(flags); i++ {
		f := flags[i]
		if depOneArg[f] {
			continue
		}
		if depTwoArg[f] {
			i++ // skip the value too
			continue
		}
		if kept, ok := StripWpDep(f); ok {
			out = append(out, kept)
		}
	}
	return out
}

// isAssemblySource matches the .s/.S extensions the compile shim's own
// isSource (go/internal/shim/compile.go) treats as assembly — kept as
// a separate, narrower check here rather than importing that package,
// since scan must not depend on shim (shim already depends on scan).
func isAssemblySource(source string) bool {
	ext := filepath.Ext(source)
	return ext == ".s" || ext == ".S"
}

// scanIncbinTargets runs a REAL compile of an assembly source with
// `-Wa,--MD=<path>` (binutils as's own dependency-output flag, passed
// through by gcc's driver the same way any other `-Wa,` option is) to
// discover any `.incbin "path"` targets, returning them as raw
// (unresolved) tokens in the same shape parseMakeDeps produces — the
// caller resolves them identically to ordinary header tokens.
//
// A REAL compile, not a probe: unlike gcc's `-M -MG`, `as` has no
// "-MG-equivalent" tolerant mode that accepts a missing dependency as
// a bare name — an .incbin target that doesn't exist on disk is a
// real assembler error either way, so there is no tolerant-scan
// option to prefer here. -o /dev/null discards the object; the
// caller's own real compile (deferred to a derivation, same as any
// other TU) produces the object that actually matters.
func scanIncbinTargets(cc, source string, flags []string) ([]string, error) {
	args := append([]string{"-c", source, "-o", "/dev/null", "-Wa,--MD=/dev/stdout"}, flags...)
	cmd := exec.Command(cc, args...)
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		return nil, fmt.Errorf("scan-incbin %s %s: %w\n%s", cc, source, err, stderr)
	}
	return parseMakeDeps(out), nil
}

func parseMakeDeps(out []byte) []string {
	// Join line-continuations first.
	var buf bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Bytes()
		line = bytes.TrimRight(line, " \t")
		if len(line) > 0 && line[len(line)-1] == '\\' {
			buf.Write(line[:len(line)-1])
			buf.WriteByte(' ')
			continue
		}
		buf.Write(line)
		buf.WriteByte(' ')
	}
	// Strip the "target:" prefix.
	joined := buf.String()
	if colon := strings.IndexByte(joined, ':'); colon >= 0 {
		joined = joined[colon+1:]
	}
	// Split on whitespace.
	var toks []string
	for _, f := range strings.Fields(joined) {
		toks = append(toks, f)
	}
	return toks
}

func resolveDep(tok string, userDirs []string) (string, bool) {
	// Both branches must return a *cleaned* absolute path: gcc -MM
	// emits paths like "common/../common/zstd_deps.h" that resolve to
	// the same underlying file as "common/zstd_deps.h", and we dedup
	// downstream on the abs string. filepath.Clean collapses "..".
	if filepath.IsAbs(tok) {
		clean := filepath.Clean(tok)
		if _, err := os.Stat(clean); err == nil {
			return clean, true
		}
		return "", false
	}
	for _, d := range userDirs {
		p := filepath.Join(d, tok) // Join cleans
		if _, err := os.Stat(p); err == nil {
			abs, err := filepath.Abs(p)
			if err == nil {
				return filepath.Clean(abs), true
			}
		}
	}
	return "", false
}

// commonAncestor returns the longest path that's a prefix of every
// input. Returns "/" if there's no shared prefix.
func commonAncestor(dirs []string) string {
	if len(dirs) == 0 {
		return "/"
	}
	root := filepath.Clean(dirs[0])
	for _, d := range dirs[1:] {
		root = widen(root, d)
	}
	return root
}

// widen shrinks `root` until `dir` is a descendant (or equal).
func widen(root, dir string) string {
	dir = filepath.Clean(dir)
	for !isPrefixOfPath(root, dir) {
		parent := filepath.Dir(root)
		if parent == root {
			return "/"
		}
		root = parent
	}
	return root
}

func isPrefixOfPath(root, path string) bool {
	if root == path {
		return true
	}
	if root == "/" {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

// The scan cache stores enough to reconstruct a Result without redoing
// the fs walk. We use a compact newline-delimited format instead of
// JSON to keep parse cost low.
//
//	line 0:   PROJECT_ROOT=<path>
//	line 1..: STAGED_IFLAG=<flag>  (repeated, order preserved)
//	line ..:  STORE_IFLAG=<flag>   (repeated)
//	line ..:  HEADER=<abs>\t<rel>  (repeated)
func encodeResult(r *Result) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "PROJECT_ROOT=%s\n", r.ProjectRoot)
	for _, f := range r.StagedIFlags {
		fmt.Fprintf(&b, "STAGED_IFLAG=%s\n", f)
	}
	for _, f := range r.StoreIFlags {
		fmt.Fprintf(&b, "STORE_IFLAG=%s\n", f)
	}
	// One line per element, not per pair: StagedIncludeFlags is a flat
	// ["-include", "<rel>", ...] slice and decode appends in order, so
	// the pairing is preserved without special-casing.
	for _, f := range r.StagedIncludeFlags {
		fmt.Fprintf(&b, "STAGED_INCLUDE=%s\n", f)
	}
	for _, h := range r.Headers {
		fmt.Fprintf(&b, "HEADER=%s\t%s\n", h.Abs, h.Rel)
	}
	return b.Bytes(), nil
}

func decodeResult(body []byte, r *Result) error {
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "PROJECT_ROOT="):
			r.ProjectRoot = strings.TrimPrefix(line, "PROJECT_ROOT=")
		case strings.HasPrefix(line, "STAGED_IFLAG="):
			r.StagedIFlags = append(r.StagedIFlags, strings.TrimPrefix(line, "STAGED_IFLAG="))
		case strings.HasPrefix(line, "STORE_IFLAG="):
			r.StoreIFlags = append(r.StoreIFlags, strings.TrimPrefix(line, "STORE_IFLAG="))
		case strings.HasPrefix(line, "STAGED_INCLUDE="):
			r.StagedIncludeFlags = append(r.StagedIncludeFlags, strings.TrimPrefix(line, "STAGED_INCLUDE="))
		case strings.HasPrefix(line, "HEADER="):
			rest := strings.TrimPrefix(line, "HEADER=")
			tab := strings.IndexByte(rest, '\t')
			if tab < 0 {
				return errors.New("malformed HEADER line")
			}
			r.Headers = append(r.Headers, Header{Abs: rest[:tab], Rel: rest[tab+1:]})
		}
	}
	return sc.Err()
}

func writeAtomic(dst string, body []byte) error {
	f, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp.*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(name, dst)
}
