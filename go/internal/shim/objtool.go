package shim

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tbereknyei/nixgg/internal/classify"
	"github.com/tbereknyei/nixgg/internal/expr"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/sandbox"
	"github.com/tbereknyei/nixgg/internal/storedeps"
	"github.com/tbereknyei/nixgg/internal/thunk"
	"github.com/tbereknyei/nixgg/internal/toolchain"
)

// Objtool is the shim entrypoint for `objtool <flags> foo.o`: a tool
// that rewrites an object IN PLACE rather than producing a new
// artifact from sources. Under nixgg the object is a drvref stub at
// that moment, so the real tool would fail on what looks to it like a
// corrupt ELF.
//
// Modelled as its own KindTransform derivation in both modes: sandbox
// mode registers a drv via `nix derivation add`; native mode writes a
// sibling .nix thunk the same way Archive/Link do, so an archive that
// includes this object references the POST-objtool rewrite rather
// than the raw compile output — otherwise native and sandbox modes
// would archive different bytes for the same source (confirmed via
// tests/drv-equivalence.sh's linux-kernel-phase1 fixture, the first to
// exercise objtool at all).
//
// Reached only when the caller points the build's own tool variable
// at this shim; build systems that pass absolute tool paths defeat
// PATH interposition, so PATH alone would never route here.
func Objtool(args []string, cfg *toolchain.Config, l paths.Layout) error {
	real := os.Getenv("NIXGG_REAL_OBJTOOL")
	if bypassed() || real == "" {
		// Say so when it's the missing binary rather than a deliberate
		// bypass — silently running nothing would corrupt the object.
		if real == "" && !bypassed() {
			logf("objtool passthrough: NIXGG_REAL_OBJTOOL unset")
		}
		// Native mode's own compiles may still be un-realized .nix
		// thunks at this point (mode.For decided Placeholder, not
		// Realise) — objtool operates in place on the object argv
		// names, so a thunk there needs resolving to real bytes
		// first, same as Archive/Link's own passthrough fallback. A
		// no-op in sandbox mode, so this is safe unconditionally.
		return RealiseThunkArgsAndPassthrough(cfg, l, objtoolFallback(real), args, sandbox.Enabled())
	}

	flags, object, ok := parseObjtoolArgs(args)
	if !ok {
		logf("objtool passthrough: no object operand (%s)", joinBase(args))
		return RealiseThunkArgsAndPassthrough(cfg, l, objtoolFallback(real), args, sandbox.Enabled())
	}

	if carvedOut(object) {
		logf("objtool passthrough: %s is in a carved-out subtree", object)
		return RealiseThunkArgsAndPassthrough(cfg, l, objtoolFallback(real), args, sandbox.Enabled())
	}

	// The object must be nameable as a derivation input: a stub names
	// its producing drv (sandbox mode), a sibling .nix thunk names its
	// producing compile (native mode), and an already-realised store
	// path is fine in either mode. Anything else we cannot model, and
	// passing through would rewrite a file no derivation knows about.
	t := classify.Target(object, altStorePrefix(cfg.Store), l)
	outName := filepath.Base(object)

	if sandbox.Enabled() || sandbox.EagerDrv() {
		var in expr.JSONDrvInput
		switch t.Kind {
		case classify.Drv:
			in = expr.JSONDrvInput{Kind: "drv", Ref: t.Ref, Name: outName}
		case classify.Store:
			in = expr.JSONDrvInput{Kind: "src", Ref: expr.StoreBasename(t.Ref), Name: outName}
		default:
			logf("objtool passthrough: can't model input %s (%s)", object, t.Reason())
			return Passthrough(objtoolFallback(real), args)
		}

		logf("objtool %s", object)

		toolStore, err := storeAddTool(cfg, "objtool", real)
		if err != nil {
			return err
		}

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
			// storeAddTool's own `nix store add --scan` already
			// records objtool's RPATH deps (elfutils, ...) as real
			// NAR references on toolStore, so Nix pulls them in
			// transitively without needing them here too — but
			// native mode has no scan RPC and must declare the SAME
			// set explicitly (see the native branch below), and a
			// drv's literal srcs list is part of its hashed content.
			// Declaring them here as well, redundant with the
			// implicit reference, keeps both modes' srcs sets equal.
			StoreDeps: storedeps.FromFile(real, cfg.KnownStorePaths),
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

	// Native mode: same rewrite, modelled as a sibling .nix thunk.
	// classify.Store is an already-realised input (same shape
	// Archive/Link's own storeInput builds); classify.Thunk is a
	// not-yet-realised sibling compile thunk — the transform script's
	// `import <path>` reference is exactly the mechanism
	// realise.CollectThunks already walks for Archive/Link, so no
	// synchronous realisation is needed here.
	var in expr.Input
	switch t.Kind {
	case classify.Store:
		in, _ = storeInput(t, object)
	case classify.Thunk:
		in = expr.Input{Kind: "nix", Ref: t.Ref, Name: outName}
	default:
		logf("objtool passthrough: can't model input %s (%s)", object, t.Reason())
		return RealiseThunkArgsAndPassthrough(cfg, l, objtoolFallback(real), args, sandbox.Enabled())
	}

	logf("objtool %s", object)

	// Stage the tool the same way sandbox mode's storeAddTool does —
	// same store path for the same file — but via an ordinary Nix
	// build instead of the scanning RPC (see stageToolForNative's own
	// docstring for why the RPC route doesn't work here).
	toolStore, err := stageToolForNative(cfg, "objtool", real)
	if err != nil {
		return err
	}

	e := expr.Transform(expr.TransformParams{
		Helpers:   cfg.Helpers,
		Name:      "ot-" + outName,
		OutName:   outName,
		ToolBin:   toolStore,
		InPlace:   true,
		Flags:     flags,
		Input:     in,
		StoreDeps: storedeps.FromFile(toolStore, cfg.KnownStorePaths),
	})

	thunkPath, err := submitTransformThunk(l, e, object)
	if err != nil {
		return err
	}
	logf("  thunk:      %s", thunkPath)
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

// stageToolForNative is storeAddTool's native-mode counterpart: same
// job (get a self-built tool binary into the store with its real
// RPATH references recorded), same memoisation shape, but reached
// through an ordinary `nix build --file` of nix/stage-tool.nix instead
// of `nix store add --scan`.
//
// Why not just call storeAddTool from native mode too: its scan is the
// AddToStoreScanning worker-protocol op, gated to run only inside a
// builder-rpc-v0/recursive-nix session — confirmed rejected outright
// ("perhaps this is not in a recursive-nix builder?") under a plain
// `nix develop` connection, which is all native mode ever has. Adding
// the binary as a bare path literal instead (Compile's own srcTree
// convention) records ZERO references — since a content-addressed
// store path is hashed over NAR bytes AND references together, that
// produces a DIFFERENT store path than sandbox's scanned copy even for
// byte-identical file content, which is what native/sandbox drv-
// equivalence actually compares.
//
// stage-tool.nix's own derivation build triggers Nix's ordinary,
// unconditional build-time reference scan (derivation-builder-impl.cc's
// scanForReferences, run for every derivation's output, never gated on
// recursive-nix) — confirmed empirically to produce the byte-identical
// store path to storeAddTool's own scanned copy, given the same name
// and storeDeps.
func stageToolForNative(cfg *toolchain.Config, name, path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	cache := ""
	if dir, err := scratchDir("tools"); err == nil {
		key := abs
		if st, err := os.Stat(abs); err == nil {
			key = fmt.Sprintf("%s|%d|%d", abs, st.ModTime().UnixNano(), st.Size())
		}
		cache = filepath.Join(dir,
			fmt.Sprintf("native-%s-%x", name, sha256.Sum256([]byte(key))))
		if b, err := os.ReadFile(cache); err == nil {
			if sp := strings.TrimSpace(string(b)); sp != "" {
				return sp, nil
			}
		}
	}

	storeDepsJSON, err := json.Marshal(storedeps.FromFile(abs, cfg.KnownStorePaths))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "import %q {\n", filepath.Join(cfg.Helpers, "stage-tool.nix"))
	fmt.Fprintf(&b, "  bashRoot      = %q;\n", cfg.BashRoot)
	fmt.Fprintf(&b, "  coreutilsRoot = %q;\n", cfg.CoreutilsRoot)
	fmt.Fprintf(&b, "  name          = %q;\n", name)
	fmt.Fprintf(&b, "  toolBinPath   = %s;\n", abs)
	fmt.Fprintf(&b, "  storeDepsJSON = %q;\n", string(storeDepsJSON))
	b.WriteString("}\n")

	dir, err := scratchDir("tools")
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, "stage-*.nix")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}

	sp, err := nixBuildFile(cfg, tmp.Name())
	if err != nil {
		return "", fmt.Errorf("stage-tool %s: %w", name, err)
	}

	if cache != "" {
		if tmpCache, err := os.CreateTemp(filepath.Dir(cache), ".tool-*"); err == nil {
			_, werr := tmpCache.WriteString(sp)
			cerr := tmpCache.Close()
			if werr == nil && cerr == nil {
				_ = os.Rename(tmpCache.Name(), cache)
			} else {
				_ = os.Remove(tmpCache.Name())
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

// submitTransformThunk writes e's thunk, symlinks output at it, and
// records the symlink — the KindTransform analog of compile.go's
// submitCompileThunk, shared by Objtool and Objcopy's native-mode
// paths.
func submitTransformThunk(l paths.Layout, e, output string) (thunkPath string, err error) {
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
