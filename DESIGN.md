# nixgg design

This document explains *why* nixgg is built the way it is. For a mechanical
walkthrough of files/functions, see `ARCHITECTURE.md`. For usage, see
`README.md`. This doc assumes you've skimmed both.

## 1. Core idea

Every `-c` compile, `ar` archive, and link in a build is intercepted by a
shim (a single static Go binary, symlinked as `cc`/`c++`/`ar`/`ld`/etc. —
see `ARCHITECTURE.md#binary-layout`). The shim turns the invocation into a
**content-addressed Nix derivation** instead of running the real tool
itself. Nix decides what's cached and what needs building; nixgg only
constructs the expression.

This buys correctness-for-free in a way a bespoke build-cache cannot:

- The cache key is Nix's own derivation hash — the same hash Nix already
  uses for its store-path and substitution logic. nixgg does not invent a
  second cache-invalidation scheme that has to be proven consistent with
  Nix's; there is only one cache, and it's the one Nix already trusts.
- A content-addressed (`ca-derivations`) output's *store path* is a
  function of the output's *content*, not of the derivation that produced
  it. Two structurally different builds that happen to produce
  byte-identical `.o` files collapse to the same store path automatically
  — nixgg doesn't compute this itself.
- Every invalidation question ("did this input change enough to force a
  rebuild") reduces to "did the derivation's hash change," which Nix has
  already spent 20 years getting right (fixed-output determinism, NAR
  hashing, `__contentAddressed`). nixgg's job is only to make sure the
  *same* logical compile produces the *same* derivation text regardless of
  which of the three registration mechanisms (below) is used — see §2 and
  `ARCHITECTURE.md#the-invariant`.

The per-invocation granularity is deliberate: a nixgg derivation is one
translation unit, one archive, or one link step — not the whole target,
and not the whole build. See §6 for why not per-target.

## 2. Three build-registration modes, one drv-construction core

nixgg ships three ways to turn a shim call into a registered Nix
derivation. They are not three different accelerators — they are three
different answers to "how do I *tell Nix* this derivation exists," sharing
one derivation-construction core.

| Mode | Trigger | Registration mechanism | Where you'd use it |
|---|---|---|---|
| **Native** | default | write a `.nix` "thunk" file to disk; `nix build --file` resolves the graph later | any recent Nix, ordinary `make`, iterative dev loop |
| **Sandbox / dyn-drv** | `NIXGG_SANDBOX=1`, inside a `builder-rpc-v0` sandbox | `nix derivation add` over the sandbox daemon connection, `nix store submit-output` at the end | `nix build .#target`, CI, reproducible from-scratch builds |
| **Eager drv** | `NIXGG_EAGER_DRV=1` | sandbox mode's own `nix derivation add` path, but from an *ordinary* (non-sandboxed) daemon connection | `nix develop`, inspecting a `.drv` immediately without a build/eval round trip |

### The shared-logic claim, verified

The design principle is: **all three modes differ only in registration/
pointer mechanism, never in derivation content.** This is checked, not
assumed. `go/internal/expr/derivation.go`'s `buildScript`, `envDict`,
`outSubdir`/`outPath`, and `ArtifactSubdir` — the functions that actually
decide what bytes go in the script and env of a derivation — contain
**zero** mode-conditional branches. The split lives entirely in two
top-level entry points, `ToNix` (native, renders `.nix` text) and `toJSON`
(sandbox/eager-drv, renders a `JSONDrv` struct), both of which call the
same private helpers.

At the shim layer, grepping for `sandbox.Enabled() || sandbox.EagerDrv()`
turns up exactly 7 call sites, all in `go/internal/shim/`, and every one is
a "which serializer / which registration call" branch — never a "what do I
put in the script" branch:

```
shim/compile.go:194       compileSandbox vs. native thunk write
shim/link.go:89,121       sandbox src upload / linkSandbox vs. native
shim/archive.go:65        archiveSandbox vs. native thunk write
shim/batchdefer.go:42     sandbox src upload vs. native SrcTreeLiteral
shim/batchresolve.go:36   sandbox drv submit vs. native thunk write
shim/batcharchive.go:98   sandbox vs. native combined-batch submission
```

Everything upstream of these 7 branches — argv parsing, header scanning
(`internal/scan`), source staging (`internal/stage`), flag rewriting,
`storedeps`/`wrapperenv` extraction, batch classification, and
`classifyInputs` — runs identically regardless of mode.

The one place the claim is *deliberately* not true:
`sandbox.PointOutputAtDrv` writes a `drvref` text stub in sandbox mode
(because `builder-rpc-v0` never materializes `.drv` files into the sandbox
filesystem — a dangling symlink would fail a Makefile `test -e`) versus a
real `os.Symlink` in eager-drv mode (the `.drv` is already a permanent
store object under an ordinary connection). That's a filesystem
side-effect about *how the result is pointed at*, not about what's in the
derivation — exactly the shape the principle predicts.

### Why this discipline matters in practice

`0d5d6f4` ("sandbox drvs are now byte-identical to native drvs") is the
commit that established byte-identity as a hard invariant, closing four
axes of divergence that had crept in independently: source-tree store
name, compile build-script/env shape, `tuID` computed from a
workspace-relative vs. absolute (mode-dependent) path, and link/archive
env-shape parity (`builder`, `outputHashAlgo`, `outputHashMode`,
`_storeDeps`). `tests/drv-equivalence.sh` now pins this as a regression
test (149 drvs across 5 fixtures as of the current suite) — every commit
message in this repo that touches `expr`/`shim`/`sandbox` since then cites
it. The payoff: a `.drv` built by hand under `nix develop` and a `.drv`
registered by a from-scratch `nix build .#target` are the *same store
object*, so work done in one mode is directly reusable by the other with
zero glue code (`tests/cross-mode-reuse.sh` proves actual substitution,
not just hash equality — see `ARCHITECTURE.md#correctness-properties`).

## 3. Component pipeline: dispatch → classify → expr → thunk/sandbox → realise

```
argv0 (cc/c++/ar/ld symlink)
   │
   ▼
dispatch.FromArgv0            strip version suffix / triple, map to
                               one of 7 canonical tool names
   │
   ▼
shim.Compile / .Link / .Archive
   │  scan.Run (cached -MM/-MG)     — header discovery
   │  stage.Sources                  — hardlink TU + headers into .nixgg/srcs/<id>/
   │  rewriteFlags / storedeps / wrapperenv
   │  classify.Target(each input)   — what IS this argv token, right now?
   │       Absent | Regular | Store | Thunk | Drv
   ▼
expr.Derivation{}              the shared IR: Kind, Tool, Inputs,
                               Flags, StoreDeps, WrapperEnv, ...
   │
   ├─ native:   Derivation.ToNix()   → .nix thunk text  → thunk.Write/LinkPlaceholder
   └─ sandbox/  Derivation.toJSON()  → JSONDrv          → sandbox.DerivationAdd
     eager-drv                                            (+ SubmitOutput if sandbox)
   │
   ▼
(later) realise.Realise            native-mode only: walks the thunk
                               import graph, ONE `nix build --file`,
                               copy-promotes results into the tree
```

- **dispatch** (`go/internal/dispatch`) is pure classification: no I/O
  beyond an rspfile read. `Tool.Basename()` always normalizes back to one
  of 7 canonical spellings (`cc`, `gcc`, `c++`, `g++`, `ar`, `ranlib`,
  `ld`) regardless of the caller's actual argv0 (`gcc-15`, `clang++-18`,
  `x86_64-linux-gnu-g++-14` all map through). This normalization is *why*
  widening the input-matching side can never change drv content: the
  script always names one of the 7 canonical tools, resolved against the
  pinned compiler root — not the caller's PATH.
- **classify** (`go/internal/classify`) answers one question for any
  caller-visible path: what is this, *right now*? A `Thunk` (native,
  unrealised `.nix` symlink), a `Drv` (sandbox/eager-drv, registered but
  unrealised), a `Store` path (already realised, in either mode), or plain
  `Regular`/`Absent`. Every consumer (link, archive, `cli/force`) asks this
  one function instead of re-deriving the answer, which is what keeps the
  three stub wire formats (`.nix` extension, `#!nixgg-drvref` stub,
  `#!nixgg-batch-pending` stub) from ever being confused with each other.
- **expr** (`go/internal/expr`) is the shared IR described in §2.
- **thunk / sandbox** are the two registration backends. `thunk.Compute`
  is `sha256(expr-body)[:16 bytes hex]` — the thunk ID *is* the content
  address, computed client-side in Go, matching what Nix would independently
  derive from the same `.nix` text.
- **realise** (`go/internal/realise`, native-mode only) is the one place
  that ever shells out to `nix build`, and it does it once per DAG, not
  once per thunk: `force.go`'s original per-thunk `nix build` calls were
  batched into a single `nix build --file <helper> attr0 attr1 ...`
  invocation early in the Go rewrite (185 calls → 1 for redis), because
  each `nix build` invocation pays real per-process cost that has nothing
  to do with whether anything actually needs building.

## 4. Nix-side design: two different retrofit problems, two different mechanisms

nixgg's Nix-side surface splits into two files that solve *structurally
different* problems and should not be merged:

### `mkNixggBuild.nix` — you're writing the build yourself

You hand it a `buildCommand` and a list of `targets`. It wraps the whole
thing in one `builder-rpc-v0` sandbox derivation (`outputHashMode = "text"`,
its own output bytes *are* a serialized `.drv`), lets nixgg's shims
register the real per-TU/per-link/per-archive graph underneath, and
resolves each named target via `builtins.outputOf`. This is the
greenfield case: there is one command, one known set of outputs, and
nothing pre-exists that constrains how the build is shaped.

### `splitStdenv.nix` — you're retrofitting an existing nixpkgs package

`pkgs.foo.override { stdenv = mkSplitStdenv { stdenv = pkgs.stdenv; ... }; }`
has no `buildCommand` to wrap — it has an *existing*, opaque
`stdenv.mkDerivation` call with a `configurePhase`/`buildPhase`/
`installPhase` shape that nixgg didn't design and can't assume. There's no
single "target" to point `mkNixggBuild` at, because the whole point is to
accelerate a build recipe someone else already wrote.

`splitStdenv` solves this by cutting the *existing* phase sequence at up
to two boundaries (`splitAtConfigure`, `splitAtBuild`) into up to three
independent derivations, and discovering the shimmed outputs of the build
stage by **walking the resulting tree for `drvref` stubs** (`nixgg
assemble`, `go/internal/assemble`) rather than by argument-parsing a
target list, because a `stdenv.mkDerivation`'s `installPhase` copies
*whatever ended up in the build tree*, not a declared target set.

### Why two mechanisms, not one

These are genuinely different problems, not the same problem approached
twice:

- `mkNixggBuild` knows its own outputs up front (`targets` is an explicit
  parameter); `splitStdenv`'s build stage does not know what it produced
  until after the build runs, hence the tree-walk.
- `mkNixggBuild` controls 100% of the build script; `splitStdenv` controls
  0% of it — it only gets to choose *where to cut* an existing, external
  recipe and what to do at each cut (bypass shims, restore a snapshot,
  splice a "restore" phase in place of a skipped one).
- `splitStdenv`'s configure/build split has **no sandbox involvement at
  all** on the configure side (`nix/splitStdenv.nix`, `mkConfigureStage`):
  configure doesn't compile anything unknown, so there's nothing to shim.
  This is a plain CA (`outputHashMode = "nar"`) early-cutoff derivation —
  a completely different mechanism from the dyn-drv sandbox that
  `mkNixggBuild` and `splitStdenv`'s build stage both need.
- The `.override`/`.overrideAttrs` reapplication problem is unique to
  `splitStdenv`: nixpkgs' contract always re-invokes the *original*
  package function before applying an override, but `splitStdenv` has
  already split that function's attrs into separately-submitted
  derivations before any override callback runs — so a plain
  `.overrideAttrs` on the returned package can only ever reach the
  *install* stage. This is why `splitStdenv` exposes
  `extraConfigureAttrs`/`extraBuildAttrs`/`extraInstallAttrs`/`extraAttrs`
  as call-site parameters spliced in *before* each stage's attrset is
  finalized, rather than relying on `.overrideAttrs` at all (see
  README.md's "How it works" and "Packages that exec their own binaries
  mid-build" sections for the worked zstd/`gen_html` example).
  `mkNixggBuild` never needs this escape hatch because the caller writes
  `buildCommand` directly — there's no pre-existing function to be
  re-invoked around.

Concretely, `splitStdenv` is a strict generalization that replaced three
earlier hand-duplicated wrappers (`dynDrvStdenv`, `configureCacheStdenv`,
`dynDrvConfigureCacheStdenv` — one docstring admitted being "the other two
cut-and-pasted together") with one generator taking the split as data
(`f26295b`, "Unify dynDrvStdenv/configureCacheStdenv/
dynDrvConfigureCacheStdenv"). That unification was a hard cutover with no
back-compat aliases — a deliberate one-way migration, not an incremental
deprecation, because keeping three names alive as thin wrappers would have
reintroduced exactly the duplication being removed.

### The synchronous-realize wall, and why it forces a second Nix-side pattern

`builder-rpc-v0`'s daemon connection has **no synchronous build op at
all** — confirmed directly against the pinned Nix daemon's own
`performOp` switch (`src/libstore/daemon.cc`): a sandboxed
`RecursiveSubmitted` connection's allowlist is `AddToStore*`,
`SubmitOutput`, `AddTempRoot`, `IsValidPath` — `BuildDerivation` and
`BuildPaths` exist in the switch but explicitly throw
`"Operation %d not allowed inside derivation"` for that connection kind.
This is deliberate upstream design (reduce nondeterminism surface inside a
sandboxed build), not a gap nixgg can close with more Go code.

This bit `examples/linux-kernel` directly: Kbuild's own recipe reads back
just-produced artifacts synchronously in the same recursive `make`
(`objcopy` on `vmlinux.o`, `nm`+`sorttable` on `vmlinux`, `relocs` on
`realmode.elf`). A registered-but-unresolved drv can't satisfy that read.
The fix that generalizes: **split into two Nix-level derivations, where
the second is a plain `stdenv.mkDerivation`, not another `mkNixggBuild`
call** — because routing the final `ld`/`nm`/`sorttable` sequence through
`mkNixggBuild`'s own target mechanism hits the *identical* wall one level
down (confirmed by trying it first and getting `"file format not
recognized"` reading back a still-unresolved drvref stub). An ordinary
second derivation has no such wall: its `buildInputs` on phase 1's targets
force Nix to resolve those to real bytes via ordinary derivation semantics
*before* phase 2's build script starts. This only works when the recipe
has a clean phase boundary reachable via `buildInputs`, before the first
synchronous read-back — a build whose read-back is interleaved with
acceleration-needing work on both sides is a real, currently-unsolved gap
that would need a new upstream daemon op, not a client-side workaround.

## 5. Performance-oriented design decisions

Each of these is a "we measured X, it helped/didn't, so we did Y" story —
not a speculative optimization. Numbers below are from this repo's own
commits/README, not estimates.

### 5.1 Direct worker-protocol RPC instead of CLI fork+exec

**Problem:** every sandbox-mode shim call previously forked+exec'd the
`nix` CLI (`nix derivation add`, `nix store add --scan`, `nix store
submit-output`) — 20-90ms per call, dominated by process startup and
daemon reconnect, not real work.

**Fix:** `go/internal/rpc` speaks the Nix worker protocol directly against
the sandbox's own daemon socket (`internal/aterm` renders the same ATerm
text `nix derivation add` computes internally; `internal/nar` renders the
same NAR bytes `nix store add --scan` would dump) — pinned to
`NixOS/nix@8307c48`, protocol 1.39, PR #15793.

**Tradeoff:** this is a from-scratch, deliberately partial client — no
`SetOptions` (fails inside a sandboxed connection: `"Operation 19 not
allowed inside derivation"`), no multi-frame streaming upload, no
error-position deserialization, only the 3 ops nixgg's shims actually
need (`AddToStore`, `AddToStoreScanning`, `SubmitOutput`). That's a real
maintenance liability — it has to be re-verified against every Nix pin
bump — accepted because a full worker-protocol client is far more surface
than nixgg needs. `NIXGG_RPC=0` is the escape hatch back to CLI fork+exec.

**Measured:** ~48% faster warm-rebuild shim pass on lua's 34 TUs (1.46s
RPC vs 2.83s CLI, 5 runs averaged, single-file edit, substituters off).
Verified byte-identical drv hashes to the CLI path before flipping the
default (`tests/drv-equivalence.sh`, `tests/smoke.sh`). The win scales with
TU count and rebuild frequency, and nearly vanishes on a cold build where
real compiler invocations dominate — see README.md's "Talking to the
daemon directly instead of shelling out."

### 5.2 TU batching (fold N compiles + 1 archive into one derivation)

**Problem, discovered by elimination:** a persistent connection-pooling
daemon-side helper (`internal/helper`, added then *removed* — see §5.4)
was built to amortize the ~4.3ms daemon handshake cost across a build.
Measured three times with increasing rigor (mosh: ~3%; redis: ~0%,
statistically indistinguishable; isolated build-phase-only: t-stat 0.61,
indistinguishable from zero) before being killed. Root cause: Nix's own
per-derivation overhead — forking a builder, sandboxing, mounting the
store — is **~10-20x** the 4.3ms handshake it would amortize. Handshake
pooling had nothing left to cut once fork+exec (§5.1) was already gone.

**The actual lever:** derivation *count* itself. `internal/batch` +
`expr/batcharchive.go` + `nix/batchArchiver.nix` combine N member compiles
and 1 archive into a single derivation for author-declared groups
(`batchGroups = [{name, patterns}]`), trading away per-TU Nix-level
parallelism/caching granularity for fewer, larger derivations.

**Tradeoff, explicit:** a batch derivation's CA hash covers every member —
touching *one* file in the batch forces real recompilation of every
*unchanged* sibling too. This only pays off for source that's stable
relative to rebuild frequency (vendored dependency trees are the intended
case: they compile once on a cold build with nothing to lose, and rarely
change afterward). It is opt-in per project, never inferred, and
`shim/batcharchive.go` refuses to batch an archive that is itself the
build's own submission target (`submit-output`'s naming convention would
break).

**Measured:** ffmpeg 2093 → 23 derivations (99% reduction); LLVM's phase-1
libraries 186 → 13; qemu-batch (thin archive, see §5.3) 1383 → 933.

**Three real bugs found scaling this up**, each worth remembering as a
class of failure this pattern reliably produces:
1. **Object-basename collisions** — a shared scratch dir keyed only on
   output basename silently clobbered ffmpeg's `libavutil/cpu.c` and
   `libavutil/x86/cpu.c` (same basename, different directories). Fixed by
   `disambiguateOutNames` (deterministic `-2`/`-3` suffixes).
2. **`MAX_ARG_STRLEN` (131072 bytes)** — a combined script for
   `libLLVMSupport` was 152853 bytes; embedding it directly as builder
   `Args` hit the kernel argv limit ("Argument list too long"). Fixed via
   `passAsFile`/`Env["batchScript"]` — the *same* fix pattern independently
   applied earlier to `assemble.Build`'s tree-restore script (openssl:
   2230 stubs, `04aa745`). Two different subsystems hit the identical
   failure mode independently — a sign this class of bug (large generated
   shell text handed to a derivation's `args`) should be assumed whenever
   a script is built by concatenating one line per item over an unbounded
   item count.
3. **Serialized compilation inside one derivation** — a combined batch
   derivation is *one* builder process, so it doesn't get Nix's own
   per-derivation build parallelism for free; confirmed via `ps aux`
   showing exactly one `cc1` process at a time for a ~350-TU libavcodec
   batch. Fixed with an explicit-pid FIFO wait loop bounded by
   `$NIX_BUILD_CORES` (deliberately *not* `wait -n`, which has a real
   job-reaping race that can silently lose a compile failure's exit code).

### 5.3 Thin-archive (`ar T`) support

**Why it's here at all:** `ar --thin`/`T` archives store member *paths*,
not embedded bytes. QEMU's meson build uses this shape for every internal
static library (`libqemuutil.a`, 450 members). This was reportedly
dismissed once as structurally impossible in a per-derivation-sandbox
model, on the reasoning that a thin archive would dangle once its
producing derivation's build directory was torn down — wrong, because
nixgg already resolves every archive member to a permanent, immutable
`/nix/store/<hash>-name/...` path *before* invoking `ar`, so the archive
stays valid for as long as Nix's own input-declaration model keeps that
path alive. See `LESSONS.md` §4.1 for the full incident (the disproving
`/tmp` experiment, the relative-vs-absolute-path wrinkle, and the
narrower batching-specific instance where the original torn-down-sandbox
concern turned out to be real after all).

**Tradeoff — a new sidecar format, not an extension of `drvref`:** a thin
archive's member list is propagated via `go/internal/members`, a separate
JSON sidecar (`.nixgg/members/<key>.json`), rather than stuffed into the
`drvref` stub format. `drvref`'s wire format is bounded to ~4096 bytes
("stubs are two short lines"); a thin archive can have hundreds of
members. Consumers pull propagated members in as `Derivation.ExtraInputs`
— declared as dependency edges but *never* rendered onto the actual `ar`/
`cc` command line, because the members are already reachable from inside
the thin archive's own bytes; re-listing them on the link line would make
the linker see duplicate symbol definitions.

**Lesson generalized:** per the repo's own recorded feedback, a
"structurally impossible because the sandbox tears down X" claim should be
checked against a real experiment, not settled by reasoning alone, before
being written into docs as a limitation. The contrast case in
`ARCHITECTURE.md`'s "What we don't (yet) do" (sandbox mode's
synchronous-realize wall) shows the other side of that same discipline: it
*is* confirmed genuinely impossible, but only because it was checked
directly against the pinned Nix daemon's own `performOp` source rather
than assumed.

### 5.4 What was tried and explicitly reverted: rpcHelper

Kept here because it's the cleanest build→measure→kill cycle in the
repo's history and is directly load-bearing for why §5.2 (batching) exists
instead. `internal/helper` (a persistent daemon-side RPC-connection-pooling
relay, opt-in via `rpcHelper = true`) was built, wired into
`mkNixggBuild`/`dynDrvConfigureCacheStdenv`, and measured three separate
times at increasing rigor (mosh ~3%, redis ~0%/non-scaling, isolated
build-phase-only ~1%/t-stat 0.61) before being removed outright
(`0f1281c`, "Remove rpcHelper (go/internal/helper): measured,
non-beneficial") rather than left as opt-in dead infrastructure. The
commit message ties the removal directly to motivating batching instead.
Don't re-propose per-shim connection pooling without first re-measuring
whether Nix's own per-derivation overhead has dropped by an order of
magnitude — as of this measurement it hadn't, and it dominates any
handshake-amortization gain by 10-20x.

## 6. Why not X

### Why not ccache (or another content-hash object cache) instead of Nix?

ccache hashes preprocessed source + compiler flags and caches the `.o`
bytes in its own store, keyed by its own scheme, checked by its own
lookup logic. That's a second cache with its own correctness story,
running alongside Nix's — you'd need to separately convince yourself
ccache's key never disagrees with Nix's notion of "this output is the
same," and every quirky compiler/flag interaction that could desync the
two caches (e.g. `-frandom-seed`, absolute paths baked into debug info)
becomes nixgg's problem to track down twice. Building each compile as a
real CA derivation means there's exactly one cache, one hashing scheme,
one substitution mechanism — the one Nix already ships and that already
has to be correct for the rest of the build. `mkNixggBuild.nix`'s
`scrubWrapperEnv` (stripping `-frandom-seed=...`/`-rpath ...` before
hashing) is nixgg's version of exactly the class of problem ccache would
also have to solve — but nixgg only has to solve it once, inside the one
cache that already exists.

### Why not a custom build-graph scheduler, like the original gg?

gg (Stanford SNR, ATC '19) models every build step as a content-addressed
thunk dispatched by its own scheduler to a cluster of workers. nixgg keeps
the thunk model but deliberately drops the scheduler: Nix already ships an
evaluator, a store, a substitution/caching layer, and remote-build
machinery. Writing a second scheduler would mean re-solving distributed
build dispatch, remote caching, and substituter protocols that Nix has
already solved — and, worse, running it *alongside* Nix's own build
graph rather than *as* it, reintroducing the two-cache correctness problem
from the previous paragraph at graph scale instead of object scale. Every
gg thunk becomes a Nix derivation; every gg fingerprint becomes a Nix
output path (README.md's "Prior art"). The entire native-mode "write a
`.nix` thunk, resolve later" design in `go/internal/thunk`/`realise` is
this idea taken literally.

### Why per-TU derivations instead of per-target?

A per-target derivation (one derivation per final binary/archive) would
have to invalidate on any change to any of its inputs — you'd be back to
"the whole target rebuilds because one file changed," which is exactly
the problem this project exists to avoid. Per-TU granularity is what
makes the openssl measurement possible at all: a one-file patch rebuilds
2 of 2213 translation units and 13 non-TU drvs (7 engine `.so`s plus the
top-level link/wrapper outputs) — not because nixgg is clever about
dependency analysis, but because Nix's own CA hashing naturally only
invalidates a derivation whose *own* input actually changed, and each TU
is its own derivation with its own, narrow input set. `internal/batch`
(§5.2) is the one deliberate, opt-in exception to this — and it exists
specifically because *for a class of inputs* (vendored dependencies that
rarely change and would all be built on a cold cache anyway) the
per-target-scale invalidation blast radius costs nothing, while
collapsing derivation count is a real, measured win. That tradeoff is
explicit and scoped by pattern-matching, not the default.

`tests/perf-regression.sh` is the in-repo, CI-enforced version of this
property (lua, 34 TUs): editing one file must rebuild *exactly* that
file's own TU derivation, and the other 33 must stay cache hits — not
just "the build still succeeds," which per-target granularity would also
satisfy trivially and uselessly.

### Why three registration modes instead of one?

Because they answer to three different environments a build step can find
itself running in, not three different design philosophies:

- Native mode needs no `builder-rpc-v0`/dynamic-derivations support at
  all — it works against any recent Nix daemon, which matters for
  bisecting against an unpatched Nix or for tooling that can't assume the
  experimental features are on.
- Sandbox mode is what makes `nix build .#target`/`nix run .#target` work
  as ordinary, cacheable, substitutable flake outputs — required for CI
  and for from-scratch reproducible builds, but only available where
  `builder-rpc-v0` is enabled.
- Eager-drv mode exists for the case where you want sandbox mode's
  registration semantics (a real, immediately-inspectable `.drv`) but
  you're in an ordinary `nix develop` shell with no live sandbox to
  register *into* — there is no sandbox-daemon connection to speak
  `builder-rpc-v0` against, so `PointOutputAtDrv` just symlinks straight
  to the already-permanent store object instead of writing a stub for one.

Collapsing these into one mode would mean picking one environment's
constraints and failing to run in the other two.

## 7. Cross-references

- Data-flow-per-mode diagrams (native/sandbox/eager-drv, full compile+link
  cycle) and the `classify.Target` decision tree: see `ARCHITECTURE.md`
  §"Flow: one compile shim invocation" onward.
- Test suite and the "five gaps" drv-equivalence alone can't close
  (artifact placement/execution, batch/thin-archive derivation shapes,
  rebuild blast-radius, configure-time early cutoff, actual cross-mode
  substitution): see `README.md`'s "Architecture" section and
  `tests/*.sh`.
- Environment variable / CLI / on-disk wire-format reference: see
  `ARCHITECTURE.md`'s workspace layout sections and the `nixgg env`/
  `force`/`assemble` usage text in `go/internal/cli/main.go`.
