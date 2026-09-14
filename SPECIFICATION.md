# nixgg specification

Reference tables/lists for implementers and integrators. Not a narrative —
see `DESIGN.md` for *why*, `LESSONS.md` for *what broke*. Every claim below
cites a file/line; line numbers drift, verify against current source before
relying on them for anything load-bearing.

---

## 1. Environment variables

All `NIXGG_*` variables are read via `os.Getenv`. Grep source of truth:
`grep -rn 'os.Getenv("NIXGG_' go/` (non-test call sites only).

### 1.1 Toolchain / bootstrap (`internal/toolchain`, `internal/cli/env.go`)

| Name | Effect | Default when unset | Mode(s) |
|---|---|---|---|
| `NIXGG_REAL_CC` | Absolute path to the real `g++` binary; `toolchain.Config.RealCC` — used only to derive the toolchain's `bin/` dir, never the tool name rendered into a drv (`toolchain.go:37-41,93`) | none — `FromEnv` fails with "missing env" (`toolchain.go:108-109`) | all |
| `NIXGG_COMPILER_ROOT` | `/nix/store/…-gcc-wrapper-…` root, mounted in every compile/link derivation (`toolchain.go:94,111-113`) | required, hard error | all |
| `NIXGG_BASH_ROOT` | bash store root; every drv's `builder` is `$NIXGG_BASH_ROOT/bin/bash` | required, hard error | all |
| `NIXGG_COREUTILS_ROOT` | coreutils store root, put on `PATH` inside every rendered script | required, hard error | all |
| `NIXGG_NIX_HELPERS` | `/nix/store/…-nixgg-nix` — root containing `builder.nix`/`linker.nix`/`archiver.nix`/`batchArchiver.nix`; every native thunk's `import` target (`toolchain.go:51-52`) | required, hard error | native (also exported in sandbox mode for dev-shell parity, unused by the shim there) |
| `NIXGG_NIX` | `nix` binary invoked by `force`/CLI-fallback ops; not used by the pure-shim path (`toolchain.go:54-55`) | required, hard error | all |
| `NIXGG_STORE` | Alt-store URL (`local?root=…` or `"auto"`), passed via `NIX_CONFIG`/used to compute `altStorePrefix` for `classify.Target` (`toolchain.go:57-59`, `cli/force.go:74,167-173`) | required, hard error (but `nixgg env` itself defaults to `local?root=/tmp/nixgg-store`, `cli/env.go:69-71`) | all |
| `NIXGG_SYSTEM` | Nix `system` string baked into sandbox-mode JSON drvs | `defaultSystem()` — compile-time GOOS/GOARCH mapping (`toolchain.go:100-103,21-33`) | sandbox/eager-drv |
| `NIXGG_KNOWN_STORE_PATHS` | JSON array of store-path strings; `storedeps.go` matches flag/env text against this list to find `StoreDeps` | `nil` if unset/unparseable — **never an error**, storedeps just finds nothing (`toolchain.go:140-150`) | all |
| `NIXGG_BATCH_GROUPS` | JSON array of `{name, patterns}`; opt-in TU-batching group definitions (`batch/env.go:5-30`) | zero `Config` (no groups) if unset/unparseable | all |
| `NIXGG_GNUMAKE_ROOT` | gnumake store root, added to `PATH` by `nixgg env` | optional; simply omitted from `PATH` if unset (`cli/env.go:111,289`) | native (`nixgg env`) |
| `NIXGG_PATCHED_NIX` | Patched-Nix (builder-rpc-v0 + submit-output) store root | optional, only consumed by `cli/env.go`'s pass-through list (`env.go:111`) — not separately read by the shim path | bootstrap only |
| `NIXGG_TOOLCHAIN_PATHS` | Opaque optional var passed through by `nixgg env`'s bootstrap | optional, unused elsewhere in Go source found | bootstrap only |

### 1.2 Mode selection (`internal/sandbox/sandbox.go`)

| Name | Effect | Default | Mode(s) |
|---|---|---|---|
| `NIXGG_SANDBOX` | `sandbox.Enabled() == (v == "1")` (`sandbox.go:27-30`) — gates all sandbox-mode code paths (`compileSandbox`, `linkSandbox`, `archiveSandbox`, `maybeSubmit`) | unset → native mode | selects sandbox mode |
| `NIXGG_EAGER_DRV` | `sandbox.EagerDrv() == (v == "1")` (`sandbox.go:32-40`) — reuses sandbox mode's JSON-drv construction over an *ordinary* (non-sandboxed) daemon connection; `PointOutputAtDrv` symlinks instead of writing a drvref stub | unset → not eager-drv | selects eager-drv mode (independent bit from `NIXGG_SANDBOX`, though in practice used without it) |
| `NIXGG_RPC` | `rpcEnabled() == (v == "1")` (`sandbox.go:42-47`) — use `internal/rpc`'s direct worker-protocol client instead of fork+exec'ing the `nix` CLI for `derivation add`/`store add`/`submit-output` | unset → CLI fork+exec fallback | sandbox/eager-drv only (`selectBackend`, `sandbox.go:72-81`) |
| `NIXGG_SANDBOX_TARGET` | Either a plain glob pattern (matched via `matchesTarget`, submits under output `"out"`) or a JSON object `{"<pattern>":"<outputKey>.drv"}` for multi-target builds; consulted by `maybeSubmit`/`targetOutputKey` (`shim/storeinput.go:200-269`) to decide whether/what a link or archive call submits via `SubmitOutput` | if unset, `defaultSubmit` (caller-supplied bool) decides; `splitStdenv`'s build stage sets it to the unmatchable literal `/nonexistent/nixgg-phase1-no-per-artifact-submit` so its own per-call submits never fire (`dynDrvShared.nix:36`) | sandbox/eager-drv |

### 1.3 Build-flow control

| Name | Effect | Default | Mode(s) |
|---|---|---|---|
| `NIXGG_AUTOFORCE` | `=="1"`: the link shim calls `realise.Realise` inline right after writing the link thunk, so a plain `NIXGG_AUTOFORCE=1 make` produces real binaries with no separate `nixgg force` step (`shim/link.go:173-182`). Only the link shim checks this — compile/archive stay placeholder | unset → thunks stay unrealised until `nixgg force` | native mode only (meaningless in sandbox — no synchronous realize op, see §9) |
| `NIXGG_BYPASS` | Any non-empty, non-`"0"` value: `bypassed()` returns true and the shim execs the real tool directly, skipping nixgg's derivation path entirely (`shim/passthrough.go:67-82`). Intended for `./configure`/cmake-probe phases | unset/`"0"` → shims fire normally | all (native and sandbox both honor it; `splitStdenv` sets/unsets it around configure vs. build phases, `splitStdenv.nix:174,283,287`) |
| `NIXGG_LOG` | Path to an ndjson activity log; `activitylog.Emit` appends one line per shim decision if set | unset → `Emit` no-ops before any JSON marshalling (`activitylog.go:33-53`) | native mode only — silently a no-op under `sandbox.Enabled()` since sandbox filesystem writes never leave the sandbox's private mount (`activitylog.go:38-49`) |

### 1.4 Workspace layout (`internal/paths/paths.go`)

| Name | Effect | Default | Mode(s) |
|---|---|---|---|
| `NIXGG_THUNKS_DIR` | Pins the thunks directory explicitly | resolved by walking up from `$PWD` for an existing `.nixgg/`, else for a `.git` root (seeding `.nixgg/` there), else `$PWD/.nixgg/thunks` (`paths.go:43-55,68-89`) | native mode |
| `NIXGG_SRCS_DIR` | Staged-TU-source directory | `<thunks-parent>/srcs` | native/sandbox (staging happens in both before upload) |
| `NIXGG_SCANS_DIR` | Cached `-MM`/`-MG` header-scan directory | `<thunks-parent>/scans` | all |
| `NIXGG_SYMLINKS_DIR` | Per-thunk manifest of caller-visible symlink paths | `<thunks-parent>/symlinks` | native mode |
| `NIXGG_PROMOTED_DIR` | `sha1(abs-path) -> {thunkID, storePath}` registry for force-promoted regular files | `<thunks-parent>/promoted` | native mode (`force`) |
| `NIXGG_BATCHES_DIR` | Pending batch-member records | `<thunks-parent>/batches` | native/sandbox (batching applies to both) |
| `NIXGG_MEMBERS_DIR` | Thin-archive member-list JSON sidecars | `<thunks-parent>/members` | all |

All six subdir vars are read via `envOr(key, fallback)` (`paths.go:113-118`); each is independently overridable even when `NIXGG_THUNKS_DIR` is left to auto-discovery.

### 1.5 Test/dev-only (not part of the runtime contract)

`NIXGG_RPC_SMOKE_SOCKET`, `NIXGG_COMPILER_ROOT` (test override in `scan_test.go`) — read only from `_test.go` files; excluded from the tables above.

---

## 2. CLI (`go/internal/cli`)

Invoked when the nixgg binary runs as `nixgg` (not through a shim symlink). `Main(args)` dispatches on `args[0]` (`main.go:20-39`); unknown/empty/`-h`/`--help`/`help` all fall through to `usage()` (`main.go:33-38`).

### 2.1 `nixgg env [--store <url>] [--print-only]`

Prints `export` lines meant for `eval "$(nixgg env)"` (`cli/env.go:17-100`).

- `--store <url>`: overrides `NIXGG_STORE` in the printed output. Precedence: flag > existing `$NIXGG_STORE` > `local?root=/tmp/nixgg-store` (`env.go:64-71`).
- `--print-only`: disables the flake-bootstrap fallback; errors immediately if any required toolchain var is missing from the environment (`env.go:130-132`).
- Required toolchain vars (`env.go:106-109`): `NIXGG_REAL_CC`, `NIXGG_NIX`, `NIXGG_NIX_HELPERS`, `NIXGG_COMPILER_ROOT`, `NIXGG_BASH_ROOT`, `NIXGG_COREUTILS_ROOT`. Optional pass-throughs (`env.go:110-112`): `NIXGG_PATCHED_NIX`, `NIXGG_TOOLCHAIN_PATHS`, `NIXGG_GNUMAKE_ROOT`.
- If any required var is missing and `--print-only` is absent: runs `nix build <flake>#env-shell --no-link --print-out-paths` (flake located by walking up to 5 parent dirs from the binary's `../` for a `flake.nix`, `env.go:178-191`), parses the resulting shell-export fragment (`parseExportFile`, a deliberately narrow `export KEY=value`/`export KEY="value"` parser, `env.go:206-241`), and fails if any required var is *still* unset afterward (`env.go:162-171`).
- Printed `PATH` order (first wins): `bin/` (nixgg CLI) : `shims/` (cc/gcc/c++/g++/ar/ranlib/ld) : compiler-root `bin/` : gnumake-root `bin/` : coreutils-root `bin/` : bash-root `bin/` : `$PATH` (`env.go:77-86`, `toolchainBinDirs`, `env.go:267-298` — coreutils deliberately placed after the compiler root so gcc-wrapper's own `ar`/`nm`/`strings` win on collision).
- Also exports `CC=cc`, `CXX=c++`, `NIXGG_STORE`, and every resolved toolchain var, keys sorted (`env.go:87-98`).

### 2.2 `nixgg force [--thunks-dir DIR] [--roots] [target…]`

Escape hatch: materializes thunks left in the tree after a build run without `NIXGG_AUTOFORCE=1` (`force.go:17-115`).

- `--thunks-dir DIR`: sets `NIXGG_THUNKS_DIR` for this process before `paths.Resolve()` (`force.go:48-51`).
- `--roots`: scans `.nixgg/thunks/` for thunk files not `import`ed by any other thunk (leaf/root outputs — computed via `realise.FindThunkImports` over every `.nix` file's body, `force.go:117-141`), and appends one recorded symlink per root as an implicit target.
- Requires at least one target (explicit or via `--roots`); else errors `"force: no targets"` (`force.go:70-72`).
- Per target: first calls `shim.ResolvePendingMember` (a still-deferred batch member skips `classifyInputs`' fallback prologue when forced manually, and would otherwise misclassify as `Regular`, `force.go:76-83`), then `classify.Target`:
  - `Absent` → warns, continues.
  - `Regular` → warns "not a nixgg symlink", continues.
  - `Store` with a known `ThunkID` (from the promoted registry) whose thunk file still exists → re-`Realise`s that DAG (Nix's own eval cache decides if anything rebuilds); otherwise warns "already realised".
  - `Thunk` → `Realise`s the referenced thunk.
  - (`Drv` is not handled by `force` at all — sandbox/eager-drv outputs have no synchronous realize path; see §9.)

### 2.3 `nixgg assemble <root> <name>`

Sandbox-mode only (`assemble.go:1-71`). splitStdenv's build-stage `postBuild` step:

1. `assemble.Walk(root)` finds every `drvref` stub under `root` in deterministic lexical order.
2. `assemble.StageForScan(root)` copies `root` into `root/.gg-stage`, excluding `.nix-socket`/`.gg-stage` themselves (scanning can't ingest a live socket).
3. `sandbox.StoreAddScan(cfg, name+"-tree", staged)` uploads the staged tree with reference-scanning (the tree may legitimately reference other store paths).
4. `assemble.Build(...)` constructs a `JSONDrv` that restores the tree from the uploaded store path, then overlays every stub's `RelPath` with its resolved artifact (path computed via `expr.ArtifactSubdir`+`expr.CAOutputPlaceholder`, script routed through `passAsFile`/`Env["buildScript"]` since stub count can be in the thousands, `assemble/build.go:32-36`).
5. Submits the drv and calls `sandbox.SubmitOutput(cfg, drvPath, "out")`.

Argument count is checked exactly (`len(args) != 2` errors); no flags.

---

## 3. `mkNixggBuild` parameter contract (`nix/mkNixggBuild.nix`)

Two-stage call: `mkNixggBuild { <toolchain deps> }` returns a function taking the build-spec attrset below.

### 3.1 Fixed (toolchain) arguments

`lib, stdenv, mkShell, bash, coreutils, gnumake, gcc, nixgg, nixHelpers, patchedNix` — all required, no defaults (`mkNixggBuild.nix:3-14`). `nixHelpers` is unused by the sandbox-mode shim itself but kept for native-mode dev-shell parity (comment, line 12).

### 3.2 Build-spec arguments

| Param | Type | Default | Notes |
|---|---|---|---|
| `pname` | string | required | |
| `version` | string | `"0"` | |
| `src` | derivation/path | required | |
| `buildCommand` | string (shell) | required | spliced verbatim between `runHook preBuild`/`runHook postBuild` inside `buildPhase` (`mkNixggBuild.nix:161-165`) |
| `targets` | `[{ name, path }]` — **list, not attrset** | required | attrset iteration is alphabetical in Nix, not declaration order; an earlier attrset version silently mis-picked the back-compat `result`/`package` entry because of this (`mkNixggBuild.nix:21-24`, pinned as a "don't re-attrset this" note in `LESSONS.md` §7). `name` becomes the outer output key (`"<name>.drv"`); `path` is matched against the shim's `-o <output>` via `NIXGG_SANDBOX_TARGET`. |
| `nativeBuildInputs` | list | `[ ]` | passed through to the outer `stdenv.mkDerivation` |
| `buildInputs` | list | `[ ]` | also folded (via `.all or [p]`, non-transitively) into `knownStorePathInputs` |
| `propagatedBuildInputs` | list | `[ ]` | same as above |
| `batchGroups` | `[{ name, patterns }]` | `[ ]` | `patterns` are `filepath.Match`-style globs (plus `**`) matched against each compile's source path; exported as `NIXGG_BATCH_GROUPS` JSON |

### 3.3 Return shape

An attrset with:

- `drv` — the outer `builder-rpc-v0` sandbox derivation. `__contentAddressed = true`, `outputHashMode = "text"` (its own output bytes *are* a serialized `.drv`), `outputHashAlgo = "sha256"`, `requiredSystemFeatures = [ "builder-rpc-v0" ]`, `out = "/nonexistent"` (stdenv's `_assignFirst` needs *some* value; a dotted name can't be a bash identifier so `out` is excluded from `outputs`), `outputs = map (n: "${n}.drv") targetNames` (`mkNixggBuild.nix:123-168`).
- `shell` — an `mkShell` mirroring `drv`'s env for `nix develop` (a text-hashed dyn-drv can't itself be a develop target); NOT `mkShellNoCC`, because bypass-mode configure steps need the cc-wrapper activation trigger (`mkNixggBuild.nix:170-180`).
- `results` — attrset `name -> builtins.outputOf drv."<name>.drv".outPath "out"` (a context-carrying string, not a derivation).
- `packages` — attrset `name -> stdenv.mkDerivation` that copies `results.<name>` into its own `$out` (copy, not symlink, so `nix profile install`/`ldd`/`readlink` resolve correctly), with `passthru = { drv; shell; packages; results; result = results.<name>; }` and `meta.mainProgram = name` when the target's path doesn't end in `.a` (`isProgramTarget`, `mkNixggBuild.nix:187-209`).
- `result` = `results.<primaryTargetName>`, `package` = `packages.<primaryTargetName>`, where `primaryTargetName = (builtins.head targets).name` (`mkNixggBuild.nix:212-219`).

### 3.4 Preconditions / edge cases

- `targets` must be non-empty (`builtins.head targets` on an empty list is a Nix eval error).
- `knownStorePathInputs` expands each input via `.all or [p]` — **not** a transitive closure; expanding transitively was tried and reverted because it broke native/sandbox drv-hash equivalence (`mkNixggBuild.nix:54-57`, confirmed in `LESSONS.md` §7).
- `scrubWrapperEnv` (shared by `preBuild` in sandbox mode and `shellHook` in native mode) strips `-frandom-seed=…` and any `-rpath …/outputs/out/lib` / `-rpath /nonexistent/lib` from `NIX_CFLAGS_COMPILE`/`NIX_LDFLAGS`, and unsets `NIX_HARDENING_ENABLE`/`CC`/`CXX`/`LD`/`AR`/`RANLIB`/`NM`/`STRIP`/`OBJCOPY`/`OBJDUMP`/`READELF`/`SIZE` — required for native/sandbox drv-hash parity (`mkNixggBuild.nix:100-121`). `NIX_CC_WRAPPER_TARGET_HOST_<triple>` is deliberately **not** scrubbed — bypass-mode configure needs it to trigger `-isystem`/`-L` injection.
- Submit-output naming: `SubmitOutput` requires the submitted path's basename to equal `outputPathName(outerDrvName, outputName)`; `mkNixggBuild` names the outer drv `"nixgg-<pname>"` and per-target outputs `"<name>.drv"` specifically so the shim's own link/archive drv names satisfy this without a rename step (`mkNixggBuild.nix:39-42`, `sandbox.go:193-200`).

---

## 4. `splitStdenv` parameter contract (`nix/splitStdenv.nix`)

Two-stage call: `mkSplitStdenv { <toolchain deps> }` returns a function taking the per-package spec below, and *that* returns a `stdenv`-shaped value meant for `pkgs.foo.override { stdenv = mkSplitStdenv {...}; }`.

### 4.1 Fixed (toolchain) arguments

`lib, patchedNix, nixgg, bash, coreutils, gcc, gnumake, system, nixpkgsPath, config, stdenvNoCC` (`splitStdenv.nix:19-31`).

### 4.2 Per-package spec

| Param | Type | Default | Semantics |
|---|---|---|---|
| `stdenv` | stdenv | required | the stdenv being wrapped |
| `splitAtConfigure` | bool | `false` | cuts between `configurePhase` and `buildPhase` |
| `splitAtBuild` | bool | `false` | cuts between `buildPhase` and `installPhase` |
| `extraAttrs` | `finalAttrs: old: old` | identity | composed **first**, before any stage-specific hatch, via `applyExtra` (`splitStdenv.nix:71`); safe for hook-shaped attrs (`postPatch`, `preBuild`, …) but not structural ones (`name`, `outputs`, `phases`) since those encode each stage's own invariants |
| `extraConfigureAttrs` | `finalAttrs: old: old` | identity | spliced into the configure-stage attrset, after `extraAttrs` |
| `extraBuildAttrs` | `finalAttrs: old: old` | identity | spliced into the build-stage attrset |
| `extraInstallAttrs` | `finalAttrs: old: old` | identity | spliced into the final (install) stage's attrset |
| `configureSrcFilter` | `{ includePatterns; existenceStubs ? []; }` or `null` | `null` | shrinks the configure stage's `src` — see §5 |

The 2×2 `splitAtConfigure`×`splitAtBuild` combination selects the stage count (`splitStdenv.nix:6-13`):

| `splitAtConfigure` | `splitAtBuild` | Stages | Nix function used |
|---|---|---|---|
| false | false | 1 (plain stdenv, untouched) | `mkDerivationSuper` directly |
| false | true | 2 | `mkBuildStage{configureStage=null}` → `finalFromBuiltTree` |
| true | false | 2 | `mkConfigureStage{bypassShims=false}` → `finalFromConfigure` |
| true | true | 3 | `mkConfigureStage{bypassShims=true}` → `mkBuildStage` → `finalFromBuiltTree` |

### 4.3 The `.overrideAttrs` reapplication problem — precise statement

nixpkgs' override contract always re-invokes the *original* package function (with un-patched attrs) before layering the override on top. `splitStdenv` has already split that function's attrs into separate, already-submitted stage derivations (configure-stage `stage`, build-stage `stage`) *before* any `.override`/`.overrideAttrs` callback on the final returned package ever runs. Consequence: a plain `.overrideAttrs` on the package `mkSplitStdenv` produces can only ever reach the **final (install) stage's** attrs — `old` inside that callback is the install stage's own computed attrs, never the configure or build stage's.

This is why `extraConfigureAttrs`/`extraBuildAttrs` exist as call-site parameters instead of being left to `.overrideAttrs`: each is applied via `applyExtra extraXAttrs finalAttrs base` *while that specific stage's attrset is being constructed* (`mkConfigureStage`'s `withAttrs`, line 180; `mkBuildStage`'s `withAttrs`, line 292), so `old` inside the hatch genuinely is that stage's own already-computed attrs, not the final stage's. `extraInstallAttrs` exists "mostly for symmetry" — install-stage attrs ARE already reachable by an ordinary `.overrideAttrs` on the returned package; only configure/build have the reapplication gap (`LESSONS.md` §7, verified end-to-end on zstd's `gen_html` case).

### 4.4 Notable mechanics

- Multi-output handling: `multiple-outputs.sh`'s `_overrideFirst` chain collapses every output name to `$out` at configure time unless a same-named bash var already exists. Each non-`"out"` output gets its own placeholder subdir (`outputPlaceholder o = if o == "out" then "/nonexistent" else "/nonexistent-${o}"`, `dynDrvShared.nix:59`) of one shared tree, restored/split back into real outputs by the final stage (`restoreOutputsScript`, `dynDrvShared.nix:91-103`) with an ELF-rpath fixup pass afterward (`elfRpathFixupScript`, `dynDrvShared.nix:114-133`) run in `preFixup`, **before** `fixupPhase`'s own patchelf-based rpath shrinking would otherwise drop the still-dangling placeholder rpath.
- The build stage's own derivation must be single-output and named `"${outerName}.drv"` (submit-output's naming convention); `separateDebugInfo = false` is forced because `make-derivation.nix` appends `"debug"` to `outputs` at its own layer whenever `separateDebugInfo = true` (e.g. openssl), which `outputs = ["out"]` alone doesn't prevent (`splitStdenv.nix:251-258`).
- Configure-stage restore rewrites two classes of baked-in path: the store output paths of the configure stage itself (`pathRewriteScript`, sed over every `/nix/store/` match) and the sandbox's absolute build root if it differs from the restoring stage's (`gg_oldroot`/`gg_newroot`, for tools like automake's generated `build-aux/missing` that bake an absolute path as literal text) — `splitStdenv.nix:186-223`.
- `NIXGG_BYPASS` is exported (`=1`) throughout the configure stage's `postPatch` (and the build stage's `postPatch`, unset again in the build stage's own `preBuild`) so shims are on `PATH` but inert until the exact phase where real acceleration is wanted (`splitStdenv.nix:173-176,282-288`).

---

## 5. `configureSrcFilter` parameter contract (`nix/configureSrcFilter.nix`)

```
mkConfigureSrcFilter = import ./configureSrcFilter.nix { lib, stdenvNoCC };
mkConfigureSrcFilter {
  name: string;              # required — output derivation name
  src: derivation/path;      # required — the package's real, unfiltered src
  includePatterns: [string]; # required — `find -path`-compatible globs,
                              #   relative to the unpacked root, e.g.
                              #   ["configure" "Makefile.am" "*/Makefile.am"]
  existenceStubs ? [];       # paths created as EMPTY files for existence-only
                              #   checks (e.g. autoconf's AC_CONFIG_SRCDIR)
}
```

Returns a `stdenvNoCC.mkDerivation` with `__contentAddressed = true`, `outputHashMode = "nar"`: copies every file matching an `includePatterns` glob (`find . -path "./<p>" -exec cp -a --parents -t "$out" {} +`) plus creates each `existenceStubs` entry as an empty file if not already present (`configureSrcFilter.nix:42-59`).

**Preconditions / failure mode:** `find -path` glob depth is literal (`"*/x"` matches exactly one nesting level, not any depth) — deep packages need more explicit patterns. An under-inclusive pattern (excluding a file `configure` actually reads) produces a **silently stale build**, not an error — must be verified by an actual before/after build comparison, not by inspecting the pattern list (comment, `configureSrcFilter.nix:23-25`; `LESSONS.md` "Miscellaneous" section).

**Structural limitation, not a bug:** if the package's own build system enumerates its source tree via glob *at configure time* (e.g. CMake's `file(GLOB ...)`, zstd's case), no smaller-than-everything filter preserves early-cutoff, because the glob result itself is part of configure's output — any filtered-out file changes the glob and thus the configure output. `configureSrcFilterPresets.nix`'s own `cmake` preset comment states this explicitly; `flake.nix`'s zstd example deliberately skips `configureSrcFilter` for this reason.

`configureSrcFilterPresets.nix` ships two starting-point pattern lists, `autotools` and `cmake`, explicitly documented as "NOT guaranteed correct for any specific package."

---

## 6. On-disk / wire formats

### 6.1 Native thunk file (`.nixgg/thunks/<id>.nix`)

Written by `thunk.Write` (`go/internal/thunk/thunk.go:41-74`), content produced by `Derivation.ToNix` (`go/internal/expr/derivation.go:415-462`). Shape, by `Kind`:

```
import <helpers>/builder.nix {
  srcTree        = <Nix path literal, unquoted>;
  source         = "<relative path>";
  outName        = "<name>";
  scriptTemplate = ''<indented-string, @NIXGG_*@ markers>'';
  markerTag      = "<tag>";
  storeDepsJSON  = ''[
  "…", …
]'';
  wrapperEnvJSON = ''{"K":"V",…}'';
}
```

(Compile shape; Link uses `linker.nix` with `inputs`/`extraInputs` in place of `srcTree`/`source`, plus an optional `srcTree` when `InlineFilesStore != ""`; Archive uses `archiver.nix` with the same `inputs`/`extraInputs` shape, no `srcTree`.) Verified against a real captured thunk at `.nixgg/thunks/1b5848ea44864d9ab5f72b5f87d60311.nix` in this repo (a `builder.nix` compile thunk for `lexer-tab.cc`).

Field notes:
- `scriptTemplate` uses `@<tag>_COREUTILS@`, `@<tag>_COMPILER@`, `@<tag>_INPUT<i>@` markers (`derivation.go:218-223`), resolved later by `nix/resolve-script.nix`. `tag` defaults to `"NIXGG"`, bumped to `"NIXGG1"`, `"NIXGG2"`, … if the literal script body already contains `@NIXGG_`-shaped text, so a marker can never collide with real content (`markerTag`, `derivation.go:206-216`).
- `storeDepsJSON`/`wrapperEnvJSON` are rendered as Nix indented-string literals wrapping literal JSON text (`jsonArrayIndented`/`jsonObjectSorted`, `expr.go:139-156`, `derivation.go:497-522`) — indentation/formatting is exact and load-bearing for `thunk.Compute`'s hash (see §8).
- `nixIndentedStringLiteral` escapes a doubled single-quote as `'''` and `${` as `''${`, escaping quotes first so the `${`-escape's own inserted quotes aren't re-escaped (`derivation.go:633-642`).

**Thunk ID**: `thunk.Compute(exprBody) = hex(sha256(exprBody))[:32 chars]` (`thunk.go:26-29`) — 16 raw bytes, 32 hex chars, computed client-side in Go and matching what Nix would independently derive from the same `.nix` text. Identical expression bytes always collapse to the same file (`thunk.Write` is a no-op if the destination already exists, tmp+rename otherwise for race-safety, `thunk.go:41-74`).

**Caller-visible symlink**: `thunk.LinkPlaceholder(l, output, thunkPath)` replaces `output` with `os.Symlink(thunkPath, output)`, bumps the thunk file's mtime to "now" (so `make`'s `stat`-based staleness check doesn't skip the consuming step even when the byte-identical thunk file wasn't rewritten), and clears any stale `RecordPromoted` entry for `output` (`thunk.go:87-103`).

**Promoted registry** (`.nixgg/promoted/<sha1(abs-target)>`): two lines, `<thunk-id>\n<store-path>\n` (`thunk.go:105-136`), read by `LookupPromoted`/`classify.Target`.

### 6.2 `drvref` stub (`internal/drvref/drvref.go`)

Written by `sandbox.PointOutputAtDrv` in sandbox mode (never in eager-drv mode — that writes a real symlink instead, `sandbox.go:242-252`). Byte layout:

```
#!nixgg-drvref\n
/nix/store/<hash>-<name>.drv\n
```

- `Header = "#!nixgg-drvref\n"` is the literal first line (`drvref.go:36`).
- Second line is the drv store path, newline-terminated.
- `maxSize = 4096` bytes read when probing (`drvref.go:44-46`) — the format is deliberately bounded ("stubs are two short lines"); a payload needing more (thin-archive member lists) goes through a separate sidecar (§6.4), never through `drvref` itself.
- Regular file, not a symlink — a symlink to an unmaterialized sandbox `.drv` would dangle, and a dangling symlink fails a Makefile `test -e` prerequisite check (mosh's `test -e ../crypto/libmoshcrypto.a`); a regular file passes `test -e` while still telling nixgg's own shims which drv produced it (`drvref.go:1-27`).
- Readers: `classify.Target` (classifies as `Kind.Drv`), `shim.resolveLibFlag` (claims a `-l` archive reference), `assemble.Walk` (tree-walk discovery for splitStdenv's build stage).

### 6.3 JSON drv schema (`nix derivation add` input, `go/internal/expr/expr.go`)

```go
type JSONDrv struct {
    Name    string             `json:"name"`
    System  string             `json:"system"`
    Builder string             `json:"builder"`
    Args    []string           `json:"args"`
    Env     map[string]string  `json:"env"`
    Inputs  JSONDrvInputs      `json:"inputs"`
    Outputs map[string]JSONOut `json:"outputs"`
    Version int                `json:"version"`
}
type JSONDrvInputs struct {
    Drvs map[string]JSONDrvRef `json:"drvs"` // keys are BASENAMES, not full paths
    Srcs []string               `json:"srcs"` // BASENAMES, not full paths
}
type JSONDrvRef struct {
    Outputs        []string       `json:"outputs"`
    DynamicOutputs map[string]any `json:"dynamicOutputs"` // must be present, even {}
}
type JSONOut struct {
    Method   string `json:"method"`   // "nar"
    HashAlgo string `json:"hashAlgo"` // "sha256"
}
```

`Version: 4` on every emitted drv (`derivation.go:579`, `batcharchive.go:162`, `assemble/build.go:87`). A `nix derivation add`-illegal path (a full `/nix/store/...` string instead of a basename inside `inputs.srcs`) fails with `"illegal base-32 character '/'"` (comment, `expr.go:161-163`) — this is why every basename-conversion helper (`StoreBasename`) exists at every call site that populates `Inputs`/`Env`.

**Worked example** (compile, hand-constructed from `CompileJSON`'s fields, mirroring the shape actually sent over the wire):

```json
{
  "name": "tu-main.o",
  "system": "x86_64-linux",
  "builder": "/nix/store/aaaa…-bash-5.2/bin/bash",
  "args": ["-c", "set -euo pipefail\nexport PATH=\"/nix/store/bbbb…-coreutils-9.5/bin:/nix/store/cccc…-gcc-wrapper-15/bin\"\nmkdir -p \"$out\"\ncd \"$src\"\n\"cc\" '-O2' -c \"$source\" -o \"$out/$outName\"\n"],
  "env": {
    "out": "/1rz4g4znpzjwh1xymhjpm42vipw92pr73vdgl6xs1hycac8kf2n9",
    "name": "tu-main.o",
    "system": "x86_64-linux",
    "builder": "/nix/store/aaaa…-bash-5.2/bin/bash",
    "outputHashAlgo": "sha256",
    "outputHashMode": "nar",
    "_storeDeps": "",
    "src": "<staged-src-tree>",
    "source": "main.c",
    "outName": "main.o"
  },
  "inputs": { "drvs": {}, "srcs": ["<staged-src-tree-basename>"] },
  "outputs": { "out": { "method": "nar", "hashAlgo": "sha256" } },
  "version": 4
}
```

(`envDict`, `derivation.go:585-631`, always includes `out`/`name`/`system`/`builder`/`outputHashAlgo`/`outputHashMode`/`_storeDeps` unconditionally, plus `_extraInputs` unconditionally for `KindLink`/`KindArchive` even when empty — the empty-vs-absent distinction was itself a discovered hash divergence, see §8.)

**CA output placeholder formula** (`caOutputPlaceholder`, `expr.go:350-370`, mirroring `src/libstore/downstream-placeholder.cc:unknownCaOutput`):
`nix32Encode(sha256("nix-upstream-output:" + hashPart + ":" + pathName))`, prefixed with `/`, where `hashPart` is the drv basename's leading 32-char nix32 hash and `pathName` is `drvName` (or `drvName + "-" + output` when `output != "out"`). Pinned against a real captured vector in `expr_test.go:9-34` (`leaf.drv`/`out` → `/0jdl66mqxficvnh6dw0z1aplacg14qdgsh8ngxrk1x09p2c2rhk4`). `OutPlaceholderNix32 = "1rz4g4znpzjwh1xymhjpm42vipw92pr73vdgl6xs1hycac8kf2n9"` is the fixed nix32 encoding of `sha256("nix-output:out")`, shared by every single-`"out"`-output derivation (`expr.go:381-384`).

### 6.4 Members sidecar (`go/internal/members/members.go`)

Path: `.nixgg/members/<key>.json`, where `<key>` is the same id the archive's own thunk/drv path already uses (so a consumer computes the same lookup key with no extra state threaded through, package doc comment `members.go:19-21`).

```go
type Record struct {
    Kind string // "store" or "nix" — same vocabulary as expr.Input/JSONDrvInput
    Ref  string
    Name string
}
```

Body: `json.Marshal([]Record)` — a plain JSON array, no envelope. `Write` uses temp-file-then-rename (`members.go:49-75`); `Read` returns `(nil, false, nil)` — not an error — when no sidecar exists for `key`, the normal case for any archive that isn't a thin archive (`members.go:80-92`).

Exists as a **separate** sidecar rather than an extension of `drvref` specifically because `drvref`'s wire format is bounded to ~4096 bytes while a thin archive can have hundreds of members (QEMU's `libqemuutil.a`, 450) (package doc, `members.go:15-17` — that comment itself still says "~280", a stale pre-measurement estimate; the precise count is pinned by `843bbb0`'s commit message and `examples/qemu/default.nix`).

---

## 7. `classify.Target` contract (`go/internal/classify/classify.go`)

```go
func Target(path, altStorePrefix string, l paths.Layout) Result
type Result struct {
    Kind    Kind   // Absent | Regular | Store | Thunk | Drv
    Ref     string
    Sub     string // relative path below Ref, when Kind==Store and target isn't Ref itself
    Err     error  // set iff Kind fell back to Regular because Lstat/readlink failed
    ThunkID string // set iff Kind==Store AND a promoted-registry entry named the producing thunk
}
```

### 7.1 Kind values and exact precondition (in the order `Target` actually checks them)

`Target` first `os.Lstat`s `path`. Branch order below is precedence order — once a check matches, later ones are never reached for that call.

1. **`Absent`** — `os.Lstat` fails with `IsNotExist`.
2. Any other `Lstat` error (e.g. `EACCES`, `ELOOP`) → **`Regular`** with `Err` set (distinguishable from a genuine ordinary file via `Result.Reason()`).
3. If the stat'd entry is **not a symlink** (`info.Mode()&os.ModeSymlink == 0`), in order:
   a. `drvref.Path(path) != ""` → **`Drv`**, `Ref` = the recorded drv path. (A sandbox-mode drvref stub is a regular file, not a symlink — this is the only way a non-symlink Drv arises.)
   b. `l.Promoted != ""` and a promoted-registry entry exists for `path` → **`Store`**, `Ref` = recorded store path, `ThunkID` = recorded thunk id.
   c. Path (after stripping `altStorePrefix` if it's a `<prefix>/nix/store/...` path) starts with `/nix/store/` → **`Store`**, split into `Ref`/`Sub` via `splitStorePath` (handles the case where an unshimmed tool like meson/CMake writes a literal absolute store path as a link argument instead of a symlink — QEMU's `libz.a` did this).
   d. Otherwise → **`Regular`**.
4. If it **is** a symlink: resolve via `readlinkFollow` (tries `filepath.EvalSymlinks` first — follows a symlink chain, e.g. output→thunk; falls back to raw `os.Readlink` if that fails, e.g. the chain leads nowhere yet). Any resolution error → **`Regular`** with `Err` set. Otherwise, in order against the resolved target (`dest`, canonicalized by stripping `altStorePrefix` the same way):
   a. Under `/nix/store/` **and** ends in `.drv` → **`Drv`**, `Ref` = the canonical `.drv` path. (Checked before the generic store-path branch since `.drv` paths are themselves under `/nix/store/`.)
   b. Under `/nix/store/` (any other suffix) → **`Store`**, split into `Ref`/`Sub`.
   c. Raw (unstripped) target ends in `.nix` → **`Thunk`**, `Ref` = the `.nix` path.
   d. `drvref.Path(dest) != ""` → **`Drv`**, `Ref` = that stub's recorded drv path, `Sub` = `filepath.Base(dest)` (handles a SONAME-alias chain like `libfoo.so -> libfoo.so.1.2.3` where the alias points at nixgg's own stub for the real link output — `Sub` must be the real output basename, not the alias name, so the emitted link line reaches for a file the drv actually produces; confirmed against openssl's `engines/*.so`).
   e. Otherwise → **`Regular`**.

### 7.2 Notes

- `ArgvPath(name)` reconstructs the full command-line path for a `Store` result: `Ref + "/" + Sub` if `Sub` is set, else `Ref + "/" + name` (`classify.go:83-89`).
- Passing a zero-value `paths.Layout` (`l.Promoted == ""`) skips the promoted-registry check entirely (step 3b above never fires) — used by callers that don't need/want that lookup.
- `Reason()` returns `"stat failed: " + err` when `Err != nil`, else the plain `Kind.String()` — used to distinguish "nixgg can't inspect this" from "nixgg doesn't own this" in passthrough diagnostics.

---

## 8. Content-addressing contract: what must stay byte-identical

Established as a hard invariant by `0d5d6f4` ("sandbox drvs are now byte-identical to native drvs"), pinned by `tests/drv-equivalence.sh` (149 drvs across 5 fixtures as of the current suite). The two serializers — `Derivation.ToNix` (native) and `Derivation.toJSON` (sandbox/eager-drv) — both call the same private helpers (`buildScript`, `envDict`, `outSubdir`/`outPath`, `ArtifactSubdir`), which the repo verifies directly contain **zero** mode-conditional branches (`derivation.go`).

### 8.1 Must be byte-identical across native / sandbox / eager-drv, for the same logical compile/link/archive

- **The rendered shell script body** (`buildScript`) — same `PATH` prefix, same flag quoting (`shellQuoteFlags`/`shellQuote`), same input ordering, same `-l`/non-`-l` flag split for link steps, same `--whole-archive`/`--start-group` bracketing.
- **The env dict** (`envDict`) — `out`, `name`, `system`, `builder`, `outputHashAlgo`, `outputHashMode`, `_storeDeps` are always present; `_extraInputs` is always present (even as `""`) for `KindLink`/`KindArchive` specifically because an earlier version's present-vs-absent asymmetry changed the hash (`derivation.go:594-598`).
- **`ArtifactSubdir(name)`** — keyed on the artifact's filename suffix only (`*-prelink.o`→`bin`, `*.o`→`""`, `*.a`→`lib`, else→`bin`), never on the producing derivation's `Kind` — because in native mode a sibling reference is a `.nix` thunk path (no Kind info available there), while in sandbox mode it's a `.drv` path (Kind is legible from the name). Keying on Kind would silently diverge the two modes' rendered scripts for the identical input (`derivation.go:141-158,168-187`).
- **`tuID`/thunk id inputs** — computed from a workspace-**relative** path, never an absolute one (an absolute path is mode-dependent: differs between a `nix develop` cwd and a sandbox build root).
- **`batch.Classify`** — matches a TU's **absolute** path with unanchored segment search, not a `scan.ProjectRoot`-relative one, because `ProjectRoot` (common ancestor of cwd + `-I` dirs) is recomputed per compile call and unstable across invocation directory.
- **`StoreAddDirectory`'s reference set** — always empty (non-scanning `nix store add -n name path`), matching native mode's plain `import <path>` semantics for per-TU source-tree uploads. `StoreAddScan` (scanning) is used **only** where scanning is actually semantically required — `cli/assemble.go`'s tree-restore path — never for a per-TU upload, because scanning an incidental `/nix/store/...` substring in staged content would record a spurious reference native mode's plain import never records, diverging the resulting store path (§2.2 of `LESSONS.md`).
- **`NIXGG_KNOWN_STORE_PATHS`** — exported identically by both sandbox `preBuild` and native `shellHook` via the shared `scrubWrapperEnv` string (`mkNixggBuild.nix:112-121`); if these two ever diverged, `storedeps.go` would find a different `StoreDeps` set per mode and diverge the hash.

### 8.2 Deliberately allowed to vary (not part of the hash, or side-effects only)

- **`PointOutputAtDrv`'s stub-vs-symlink choice** — sandbox mode writes a `drvref` text stub, eager-drv mode writes a real symlink. This is a filesystem side-effect about *how the caller-visible output is pointed at*, not a difference in the derivation's own content — the one place the "sandbox and eager-drv share all drv-construction logic" claim is deliberately false, and it's scoped to exactly this function (verified: `grep -rn "sandbox.Enabled() || sandbox.EagerDrv()"` finds exactly 7 call sites, all "which serializer/registration call" branches).
- **`knownStorePathInputs`'s expansion depth** — `.all or [p]` per input, non-transitive. Expanding transitively was tried and reverted because it broke hash equivalence (adding enough extra path text to diverge the two modes' `NIXGG_KNOWN_STORE_PATHS` rendering in practice, even though in principle both modes would see the same expanded set — the revert was because the *change itself*, not an asymmetry, broke equivalence during the transition; treat `.all`-only as the pinned contract going forward).
- **Registration mechanism itself** — `.nix` thunk file + `import` (native) vs. `nix derivation add`/ATerm text (sandbox/eager-drv) — differs by construction, is never expected to produce identical bytes, and isn't part of this invariant (the invariant is about the **resulting derivation's hash**, not the wire format used to register it).

### 8.3 What the invariant does *not* prove (see `tests/` for the closing tests)

Hash equality across two separately-computed stores does not by itself prove: the output was ever actually realised correctly (`tests/smoke.sh`), that batch/thin-archive derivation shapes (which drv-equivalence.sh's regex-based Kind matching never inspects) are correct, that rebuild blast-radius is narrow (`tests/perf-regression.sh`), that configure-time early-cutoff is correct (`tests/configure-cache-cutoff.sh`), or that a drv built in one mode is actually **substituted** (not rebuilt) by the other mode in the *same* store (`tests/cross-mode-reuse.sh`). See `LESSONS.md` §2.3 for the full breakdown.

---

## 9. Nix worker-protocol subset (`go/internal/rpc`)

`internal/rpc` is a deliberately partial worker-protocol client, pinned to `NixOS/nix@8307c48` (protocol version `1.39`, PR #15793's builder-rpc-v0 work) — `protocol.go:6-22`.

### 9.1 Ops this client sends

| Op | Numeric value | Go method | Real-protocol name/number (`worker-protocol.hh:230-280`) |
|---|---|---|---|
| `opAddToStore` | 7 | `Conn.AddDerivation` (name → ATerm text, CA method `text:sha256`), `Conn.AddDirectory` (name → NAR dump, CA method `fixed:r:sha256`, empty refs) | `WorkerProto::Op::AddToStore = 7` |
| `opAddToStoreScanning` | 1001 | `Conn.AddToStoreScanning` (NAR dump, CA method `fixed:r:sha256`, daemon scans for references) | `WorkerProto::Op::AddToStoreScanning = 1001` — matches exactly |
| `opSubmitOutput` | 1000 | `Conn.SubmitOutput` (`SingleDerivedPath::Opaque` tag 0 only — nixgg never submits a `Built` / not-yet-resolved-output reference) | `WorkerProto::Op::SubmitOutput = 1000` — matches exactly |

No other op is ever sent by this client. `SetOptions` (op 19) is explicitly never sent — confirmed by direct experiment that it fails with `"Operation 19 not allowed inside derivation"` inside a `RecursiveSubmitted` connection (`protocol.go` comment area is silent on this; see `LESSONS.md` §1.3 for the experiment).

### 9.2 Comparison to the real protocol's full op set

The real protocol (`nix-clone/src/libstore/include/nix/store/worker-protocol.hh:230-280`) defines ~30 live ops (`IsValidPath=1`, `QueryReferrers=6`, `AddToStore=7`, `BuildPaths=9`, `AddTempRoot=11`, `SetOptions=19`, `QueryPathInfo=26`, `BuildDerivation=36`, `AddToStoreNar=39`, `QueryMissing=40`, `AddMultipleToStore=44`, `SubmitOutput=1000`, `AddToStoreScanning=1001`, plus several `// obsolete`/`// removed` numeric gaps). `internal/rpc` implements exactly 3 of these — `AddToStore`, `SubmitOutput`, `AddToStoreScanning` — plus enough handshake/STDERR-drain machinery to reach them (`Conn.handshake`, `drainStderr`, `readValidPathInfoPath`/`skipUnkeyedValidPathInfo` for parsing a `ValidPathInfo` response).

`internal/rpc`'s implemented set is also a strict subset of what a `RecursiveSubmitted` sandbox connection actually allows, confirmed directly against `src/libstore/daemon.cc`'s `performOp` (lines 329-346 of this repo's `nix-clone` checkout):

```cpp
static constexpr std::array validOperations = {
    WorkerProto::Op::AddToStore,
    WorkerProto::Op::AddMultipleToStore,
    WorkerProto::Op::AddToStoreNar,
    WorkerProto::Op::AddToStoreScanning,
    WorkerProto::Op::SubmitOutput,
    WorkerProto::Op::AddTempRoot,
    WorkerProto::Op::IsValidPath,
};
```

Any op not in this list throws `"Operation %d not allowed inside derivation"` inside a `RecursiveSubmitted` connection — this is the exact source of the "no synchronous build op" wall documented in `DESIGN.md` §4/`LESSONS.md` §1.1: `BuildDerivation` (36) and `BuildPaths` (9) exist in the protocol and in `performOp`'s `switch`, but are unreachable from inside a builder-rpc-v0 sandbox by construction, not by omission in nixgg's client. `internal/rpc` implements `AddToStore`/`AddToStoreScanning`/`SubmitOutput` — 3 of the 7 sandbox-allowed ops — and has no code path that would even attempt any of the other 4 allowed-but-unused ops (`AddMultipleToStore`, `AddToStoreNar`, `AddTempRoot`, `IsValidPath`) or any disallowed op.

`NIXGG_RPC=0` is the always-available escape hatch back to fork+exec'ing the `nix` CLI for the same 3 logical operations (`derivation add`, `store add [--scan]`, `store submit-output`) — verified byte-identical drv hashes between the two paths before the RPC path became the default (`tests/drv-equivalence.sh`, `tests/smoke.sh`; ~48% faster warm-rebuild shim pass measured on lua's 34 TUs, see `DESIGN.md` §5.1).

---

## Appendix: file/line index of primary sources cited above

- `go/internal/toolchain/toolchain.go`
- `go/internal/sandbox/sandbox.go`
- `go/internal/paths/paths.go`
- `go/internal/batch/env.go`
- `go/internal/activitylog/activitylog.go`
- `go/internal/cli/{main,env,force,assemble}.go`
- `go/internal/mode/mode.go`
- `go/internal/expr/{expr,derivation,batcharchive}.go`
- `go/internal/thunk/thunk.go`
- `go/internal/drvref/drvref.go`
- `go/internal/members/members.go`
- `go/internal/classify/classify.go`
- `go/internal/assemble/{assemble,build}.go`
- `go/internal/rpc/{protocol,ops,conn}.go`
- `nix/{mkNixggBuild,splitStdenv,configureSrcFilter,configureSrcFilterPresets,dynDrvShared,builder}.nix`
- `nix-clone/src/libstore/{daemon.cc,include/nix/store/worker-protocol.hh}`
