package scan

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tbereknyei/nixgg/internal/paths"
)

// RunRust enumerates the source set for one rustc crate compile.
//
//   - rustc:  absolute path to the real compiler
//   - source: the crate root the caller named (its `$<`)
//   - flags:  everything else on argv, minus -o and the --emit family
//
// The C scanner asks the preprocessor which headers a translation unit
// reads. Rust has no preprocessor, so the equivalent question can only
// be answered by the compiler front end — which rustc will answer
// directly via `--emit=dep-info`, listing every file the crate reaches:
// `mod` files, `include!`, `include_str!` and `include_bytes!`. That
// last group matters because it is precisely the case `gcc -M` cannot
// see (the `.incbin` problem the compile shim has to passthrough for);
// here it comes for free.
//
// The exit status is deliberately ignored. Under nixgg a crate's
// `--extern` dependencies are drvref stubs rather than real rlibs, so
// rustc reports "extern location … is of an unknown type" and exits
// non-zero. It writes the dep-info file anyway: dependency collection
// happens during expansion, before and independently of crate
// resolution. Verified across a missing extern, a stub extern and no
// extern at all — all three produce the same complete dep list as a
// build with the real rlib present.
//
// The one gap that leaves: a file pulled in by an `include!` that a
// PROC-MACRO generated is invisible, because an unresolvable
// proc-macro crate never expands. That is why shim.Rustc refuses to
// model proc-macro crates — keeping their output a real file on disk
// keeps every dependent crate's scan honest.
func RunRust(l paths.Layout, rustc, source string, flags []string) (*Result, error) {
	if err := os.MkdirAll(l.Scans, 0o755); err != nil {
		return nil, err
	}
	key := cacheKey(rustc, source, flags)
	if r, ok := readCache(l, key); ok {
		return r, nil
	}
	r, deps, err := runRustScanner(rustc, source, flags)
	if err != nil {
		return nil, err
	}
	writeCache(l, key, r, deps)
	return r, nil
}

func runRustScanner(rustc, source string, flags []string) (*Result, []depEntry, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil, err
	}
	srcAbs, err := filepath.Abs(source)
	if err != nil {
		return nil, nil, err
	}

	// rustc writes the crate's real artifacts unless told otherwise, and
	// the caller's --out-dir points into the build tree. Redirect both
	// the dep file and the artifacts into a scratch dir so a scan never
	// touches what the build is about to produce.
	tmp, err := os.MkdirTemp("", "nixgg-rustscan")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(tmp)
	depFile := filepath.Join(tmp, "dep-info")

	args := append(stripRustEmitFlags(flags),
		"--emit=dep-info="+depFile, "--out-dir", tmp, source)
	cmd := exec.Command(rustc, args...)
	// Kept, not discarded: when the dep file is missing this stderr is
	// the only account of why, and without it the shim can say no more
	// than "scan failed" about a crate that then silently builds
	// unaccelerated.
	var stderr strings.Builder
	cmd.Stderr = &stderr
	_ = cmd.Run() // see the docstring: the dep file is written regardless

	out, err := os.ReadFile(depFile)
	if err != nil {
		// No dep file at all means the front end never got far enough to
		// expand the crate — the source set is unknown, not empty, and
		// staging an empty tree would produce a drv that fails inside the
		// sandbox with an error pointing at the wrong thing.
		return nil, nil, fmt.Errorf("rustc wrote no dep-info for %s: %w\n%s",
			source, err, tail(stderr.String(), 2000))
	}

	projectRoot := commonAncestor([]string{cwd, filepath.Dir(srcAbs)})
	var resolved []string
	for _, tok := range parseMakeDeps(out) {
		abs, ok := resolveDep(tok, []string{cwd, filepath.Dir(srcAbs)})
		if !ok || abs == srcAbs {
			continue
		}
		if strings.HasPrefix(abs, "/nix/store/") {
			// Already content-addressed; referenced by absolute path and
			// mounted via storeDeps rather than copied into the tree.
			continue
		}
		resolved = append(resolved, abs)
	}
	for _, abs := range resolved {
		projectRoot = widen(projectRoot, filepath.Dir(abs))
	}

	seen := map[string]bool{}
	var headers []Header
	deps := []depEntry{statOr(srcAbs)}
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
	sort.Slice(headers, func(i, j int) bool { return headers[i].Rel < headers[j].Rel })

	return &Result{Headers: headers, ProjectRoot: projectRoot}, deps, nil
}

// stripRustEmitFlags drops the caller's output-selection flags. We
// supply our own --emit and --out-dir; leaving the caller's in place
// would make the scan write the artifact the build is waiting for, from
// a compile whose externs are stubs.
//
// -o is dropped for the same reason. Both attached (--emit=x) and
// separated (--emit x) spellings occur.
func stripRustEmitFlags(flags []string) []string {
	out := make([]string, 0, len(flags))
	for i := 0; i < len(flags); i++ {
		f := flags[i]
		switch {
		case f == "--emit" || f == "--out-dir" || f == "-o":
			i++ // also skip its value
		case strings.HasPrefix(f, "--emit=") || strings.HasPrefix(f, "--out-dir="):
		default:
			out = append(out, f)
		}
	}
	return out
}

// tail returns the last n bytes of s, so a compiler's diagnostic reaches
// the log without a thousand lines of lint output burying it.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
