package shim

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tbereknyei/nixgg/internal/classify"
	"github.com/tbereknyei/nixgg/internal/expr"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/sandbox"
	"github.com/tbereknyei/nixgg/internal/storedeps"
	"github.com/tbereknyei/nixgg/internal/toolchain"
)

// Objtool is the shim entrypoint for `objtool <flags> foo.o`: a tool
// that rewrites an object IN PLACE rather than producing a new
// artifact from sources. Under nixgg the object is a drvref stub at
// that moment, so the real tool would fail on what looks to it like a
// corrupt ELF.
//
// Modelled as its own KindTransform derivation: take the producing drv
// as input, copy its object, rewrite the copy, leave a fresh stub
// behind — the ar/link shims already resolve stubs, so nothing else
// changes.
//
// Reached only when the caller points the build's own tool variable
// at this shim; build systems that pass absolute tool paths defeat
// PATH interposition, so PATH alone would never route here.
func Objtool(args []string, cfg *toolchain.Config, l paths.Layout) error {
	real := os.Getenv("NIXGG_REAL_OBJTOOL")
	if bypassed() || (!sandbox.Enabled() && !sandbox.EagerDrv()) || real == "" {
		// Say so when it's the missing binary rather than a deliberate
		// bypass — silently running nothing would corrupt the object.
		if real == "" && !bypassed() {
			logf("objtool passthrough: NIXGG_REAL_OBJTOOL unset")
		}
		return Passthrough(objtoolFallback(real), args)
	}

	flags, object, ok := parseObjtoolArgs(args)
	if !ok {
		logf("objtool passthrough: no object operand (%s)", joinBase(args))
		return Passthrough(objtoolFallback(real), args)
	}

	// The object must be nameable as a derivation input: a stub names
	// its producing drv, an already-realised store path is equally
	// fine. Anything else we cannot model, and passing through would
	// rewrite a file no derivation knows about.
	t := classify.Target(object, altStorePrefix(cfg.Store), l)
	var in expr.JSONDrvInput
	switch t.Kind {
	case classify.Drv:
		in = expr.JSONDrvInput{Kind: "drv", Ref: t.Ref, Name: filepath.Base(object)}
	case classify.Store:
		in = expr.JSONDrvInput{Kind: "src", Ref: expr.StoreBasename(t.Ref), Name: filepath.Base(object)}
	default:
		logf("objtool passthrough: can't model input %s (%s)", object, t.Reason())
		return Passthrough(objtoolFallback(real), args)
	}

	logf("objtool %s", object)

	toolStore, err := storeAddTool(cfg, "objtool", real)
	if err != nil {
		return err
	}

	outName := filepath.Base(object)
	drv := expr.TransformJSON(expr.TransformJSONParams{
		Name:      "ot-" + outName,
		OutName:   outName,
		System:    cfg.System,
		Bash:      cfg.BashRoot,
		Coreutils: cfg.CoreutilsRoot,
		ToolBin:   toolStore,
		InPlace:   true,
		Flags:     flags,
		Input:     in,
		StoreDeps: storedeps.From(nil, "", cfg.KnownStorePaths),
		ExtraSrcs: []string{
			baseNameOf(cfg.BashRoot),
			baseNameOf(cfg.CoreutilsRoot),
			baseNameOf(toolStore),
		},
	})
	drvPath, err := sandbox.DerivationAdd(cfg, drv)
	if err != nil {
		return err
	}
	if err := sandbox.PointOutputAtDrv(object, drvPath); err != nil {
		return err
	}
	logf("  drv:        %s", drvPath)
	return nil
}

// parseObjtoolArgs splits objtool's argv into flags and the object it
// operates on. objtool's grammar is `objtool <actions/options...>
// file.o` — the object is the sole non-flag operand and comes last;
// its options are bare long flags or `--opt=value`, none of which
// consume a following token.
func parseObjtoolArgs(args []string) (flags []string, object string, ok bool) {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			continue
		}
		if object != "" {
			// A second operand means a shape we do not model.
			return nil, "", false
		}
		object = a
	}
	if object == "" {
		return nil, "", false
	}
	return flags, object, true
}

// storeAddTool puts a binary the wrapped project built itself into the
// store so a derivation can depend on it. The tool is often built by
// the build itself, inside the sandbox this shim runs in, so it's an
// ordinary file rather than a store path already; `nix store add`
// fixes that, and since the binary links only against store paths
// (elfutils/glibc/zlib/zstd) the scan records correct references.
//
// Memoised in a scratch file beside the build root: every object in
// the build asks for the same binary, and a fork+exec per object would
// cost more than the compile derivations it's protecting.
func storeAddTool(cfg *toolchain.Config, name, path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	cache := ""
	if dir, err := scratchDir("tools"); err == nil {
		// Key on path AND mtime+size: Kbuild rebuilds its own objtool
		// in place when the config or scripts/ change, and a path-only
		// key would keep handing out the store object made from the
		// PREVIOUS binary — objects silently rewritten by a stale tool.
		key := abs
		if st, err := os.Stat(abs); err == nil {
			key = fmt.Sprintf("%s|%d|%d", abs, st.ModTime().UnixNano(), st.Size())
		}
		cache = filepath.Join(dir,
			fmt.Sprintf("%s-%x", name, sha256.Sum256([]byte(key))))
		if b, err := os.ReadFile(cache); err == nil {
			if sp := strings.TrimSpace(string(b)); sp != "" {
				return sp, nil
			}
		}
	}
	sp, err := sandbox.StoreAddScan(cfg, name, abs)
	if err != nil {
		return "", fmt.Errorf("store-add %s: %w", name, err)
	}
	if cache != "" {
		// Write to a unique temp and rename: shims run concurrently
		// under `make -j`, and every object asks for the same key, so a
		// plain WriteFile lets a reader see a half-written file (the
		// truncated store path would survive TrimSpace and get used as
		// ToolBin). Best-effort otherwise — a failed write costs a
		// re-add, not correctness.
		if tmp, err := os.CreateTemp(filepath.Dir(cache), ".tool-*"); err == nil {
			_, werr := tmp.WriteString(sp)
			cerr := tmp.Close()
			if werr == nil && cerr == nil {
				_ = os.Rename(tmp.Name(), cache) // atomic; losing a race is harmless
			} else {
				_ = os.Remove(tmp.Name())
			}
		}
	}
	return sp, nil
}

// objtoolFallback picks what Passthrough should exec: the real binary
// the caller told us about, or the bare name so PATH resolution
// produces a recognisable "not found" rather than an empty-argv panic.
func objtoolFallback(real string) string {
	if real != "" {
		return real
	}
	return "objtool"
}
