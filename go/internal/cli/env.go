package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// cmdEnv prints shell `export` lines that bootstrap nixgg: NIXGG_ROOT
// and PATH (shims/ prepended), the NIXGG_* toolchain roots (from the
// flake's env-shell if not already set), NIXGG_STORE, and CC/CXX.
// Intended use: `eval "$(nixgg env)"`.
func cmdEnv(args []string) error {
	var (
		storeOverride string
		printOnly     bool
	)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--store":
			if i+1 >= len(args) {
				return fmt.Errorf("--store requires an argument")
			}
			storeOverride = args[i+1]
			i++
		case "--print-only":
			printOnly = true
		case "-h", "--help":
			fmt.Fprint(os.Stderr, `usage: nixgg env [--store <url>] [--print-only]

Prints shell `+"`export`"+` lines that fully bootstrap nixgg. Intended use:

    eval "$(nixgg env)"

If required NIXGG_* vars aren't already set in the environment, env
tries to build them from the flake at ../flake.nix (relative to the
nixgg binary). --print-only disables that fallback: env only prints
what it already knows and errors out if anything's missing.
`)
			return nil
		default:
			return fmt.Errorf("unknown flag %q", args[i])
		}
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	root := filepath.Dir(filepath.Dir(exe)) // bin/nixgg → ..
	shims := filepath.Join(root, "shims")

	// Toolchain env: honor what's already set; bootstrap from
	// .#env-shell for anything missing.
	env, err := loadToolchainEnv(root, printOnly)
	if err != nil {
		return err
	}

	// NIXGG_STORE: CLI flag > env > default alt store.
	store := storeOverride
	if store == "" {
		store = os.Getenv("NIXGG_STORE")
	}
	if store == "" {
		store = "local?root=/tmp/nixgg-store"
	}

	// Print in an order that's readable in a terminal and safe to eval.
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	fmt.Fprintf(w, "export NIXGG_ROOT=%s\n", shellQuote(root))
	// PATH order matters (first entry wins): bin/ (nixgg CLI), shims/
	// (cc/gcc/c++/g++/ar/ranlib/ld), then toolchain bins (unshimmed
	// tools + gnumake/coreutils so plain `make` works), then $PATH.
	bin := filepath.Join(root, "bin")
	toolchainBins := toolchainBinDirs(env)
	pathParts := []string{shellQuote(bin), shellQuote(shims)}
	for _, d := range toolchainBins {
		pathParts = append(pathParts, shellQuote(d))
	}
	fmt.Fprintf(w, "export PATH=%s:${PATH}\n", strings.Join(pathParts, ":"))
	fmt.Fprintln(w, "export CC=cc")
	fmt.Fprintln(w, "export CXX=c++")
	fmt.Fprintf(w, "export NIXGG_STORE=%s\n", shellQuote(store))
	// The toolchain vars — sorted for stable output.
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "export %s=%s\n", k, shellQuote(env[k]))
	}
	return nil
}

// loadToolchainEnv returns the NIXGG_* toolchain vars that every shim
// needs. Any missing vars are filled in by building and reading the
// flake's `.#env-shell` output. `printOnly` disables that fallback.
func loadToolchainEnv(root string, printOnly bool) (map[string]string, error) {
	required := []string{
		"NIXGG_REAL_CC", "NIXGG_NIX", "NIXGG_NIX_HELPERS",
		"NIXGG_COMPILER_ROOT", "NIXGG_BASH_ROOT", "NIXGG_COREUTILS_ROOT",
	}
	optional := []string{
		"NIXGG_PATCHED_NIX", "NIXGG_TOOLCHAIN_PATHS", "NIXGG_GNUMAKE_ROOT",
	}
	env := make(map[string]string)
	var missing []string
	for _, k := range required {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		} else {
			missing = append(missing, k)
		}
	}
	for _, k := range optional {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	if len(missing) == 0 {
		return env, nil
	}
	if printOnly {
		return nil, fmt.Errorf("missing env: %v (run without --print-only to bootstrap from flake)", missing)
	}

	// Bootstrap: nix build <flake>#env-shell and parse its export lines.
	flake, err := findFlake(root)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: %w", err)
	}
	nix, err := findNix()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(nix, "build", flake+"#env-shell", "--no-link", "--print-out-paths")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("nix build %s#env-shell: %w", flake, err)
	}
	envPath := strings.TrimSpace(string(out))
	if envPath == "" {
		return nil, fmt.Errorf("nix build %s#env-shell returned nothing", flake)
	}
	parsed, err := parseExportFile(envPath)
	if err != nil {
		return nil, err
	}
	for _, k := range append(required, optional...) {
		if v, ok := parsed[k]; ok {
			env[k] = v
		}
	}
	// Verify required vars are now filled in.
	var stillMissing []string
	for _, k := range required {
		if env[k] == "" {
			stillMissing = append(stillMissing, k)
		}
	}
	if len(stillMissing) > 0 {
		return nil, fmt.Errorf("env-shell %s didn't set: %v", envPath, stillMissing)
	}
	return env, nil
}

// findFlake locates a flake.nix in `root` or a parent directory. Falls
// back to `root` itself if no flake.nix is found — the caller's `nix
// build` will produce a clear error in that case.
func findFlake(root string) (string, error) {
	dir := root
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(filepath.Join(dir, "flake.nix")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("no flake.nix found near %s", root)
}

// findNix returns the `nix` binary to invoke for bootstrapping. Prefers
// $NIX (explicit user override) then whatever's on PATH.
func findNix() (string, error) {
	if p := os.Getenv("NIX"); p != "" {
		return p, nil
	}
	p, err := exec.LookPath("nix")
	if err != nil {
		return "", fmt.Errorf("no `nix` on PATH; install Nix or set $NIX")
	}
	return p, nil
}

// parseExportFile reads a shell fragment full of `export FOO="bar"` and
// returns FOO → bar. Handles the shapes the flake's writeTextFile
// output uses: `export KEY="value"` and `export KEY=value` with the
// value containing spaces + no shell metacharacters we care about.
//
// This is deliberately narrow; it's not a shell parser. If the flake
// output diverges, extend here.
func parseExportFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := make(map[string]string)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "export ") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		k := line[:eq]
		v := line[eq+1:]
		// Strip a single surrounding pair of quotes.
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out, sc.Err()
}

// shellQuote wraps a value in single quotes safe for shell eval.
// Any embedded single quote is escaped as `'\”`.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	// Fast path: no shell metacharacters, no whitespace → bare.
	needsQuote := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '"' || r == '\'' || r == '$' ||
			r == '`' || r == '\\' || r == '|' || r == '&' || r == ';' ||
			r == '<' || r == '>' || r == '(' || r == ')' || r == '#' ||
			r == '*' || r == '?' || r == '[' || r == ']' || r == '{' ||
			r == '}' {
			needsQuote = true
			break
		}
	}
	if !needsQuote {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// toolchainBinDirs returns the `<store>/bin` directories from the
// toolchain roots we already know about. Order deliberately puts
// coreutils LAST so the toolchain (gcc-wrapper's ld/ar/nm) wins if
// there's a name collision.
func toolchainBinDirs(env map[string]string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(root string) {
		if root == "" {
			return
		}
		bin := filepath.Join(root, "bin")
		if seen[bin] {
			return
		}
		seen[bin] = true
		out = append(out, bin)
	}
	// gcc-wrapper carries the sibling tools (ar, nm, strings, etc)
	// keyed to this compiler, plus of course cc/c++.
	add(env["NIXGG_COMPILER_ROOT"])
	// gnumake — so `make` is on PATH from a plain `eval $(nixgg env)`.
	add(env["NIXGG_GNUMAKE_ROOT"])
	// coreutils gives us `ls`, `mkdir`, `rm`, and friends — the shim
	// invokes some (via os.exec.Command) and downstream Makefiles
	// expect them on PATH too.
	add(env["NIXGG_COREUTILS_ROOT"])
	// bash last — mostly here for scripts that hardcode /bin/sh
	// lookups. We don't actively need it on PATH.
	add(env["NIXGG_BASH_ROOT"])
	return out
}
