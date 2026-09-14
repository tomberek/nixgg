# LESSONS.md

Hard-won, non-obvious knowledge about this codebase. Not a tutorial and not
an architecture doc (see `ARCHITECTURE.md`, `README.md`) — this is the
"what broke, why, and what to check before you re-break it" record. Every
entry cites a commit hash and/or file path; verify against current source
before trusting line numbers, since this file will drift.

---

## 1. Sandbox / dynamic-derivation constraints

### 1.1 `builder-rpc-v0` has no synchronous-build op — the wall

**Incident**: `examples/linux-kernel` (commit `0418bd3`). Linux Kbuild's own
recipe reads back artifacts it just built, synchronously, in the same
recursive `make`: `objcopy -j .modinfo` on `vmlinux.o`, `nm`+`sorttable` on
`vmlinux`, `arch/x86/tools/relocs` on `realmode.elf`, `checkundef.sh`
running `nm` on `vdso32.so.dbg`. Under sandbox mode, every nixgg output is a
*registered drv* (a `drvref` stub — see 1.2), not real bytes, until Nix's
outer scheduler resolves the whole graph after the sandbox build script
exits. So none of these mid-build reads can be satisfied at all: the
symptom is nm/objcopy failing on a stub file ("file format not recognized").

**Root cause, confirmed not assumed**: read the pinned Nix daemon's own
source, `src/libstore/daemon.cc`'s `performOp`. A `builder-rpc-v0` sandbox's
op allowlist for a `RecursiveSubmitted` connection is exactly:
`AddToStore{,Multiple,Nar,Scanning}`, `SubmitOutput`, `AddTempRoot`,
`IsValidPath`. `BuildDerivation`/`BuildPaths` exist in the switch but throw
`"Operation %d not allowed inside derivation"` — a deliberate upstream
choice to reduce nonreproducibility, not a client-side gap nixgg's Go code
can close.

**First (failed) fix attempt**: route the final `ld`/`nm`/`sorttable`
sequence through `mkNixggBuild`'s own `targets` mechanism as a second
dyn-drv phase. Hits the *identical* wall one level down — a
`nm -n vmlinux` immediately following `vmlinux`'s own submission in the
same script still reads an unresolved drvref stub.

**Actual fix**: a genuine two-phase split where **phase 2 is a plain
`stdenv.mkDerivation`, not another `mkNixggBuild` call** (see
`examples/linux-kernel/default.nix`). Phase 1 (`mkNixggBuild`, ~2800 real
compiles) declares its targets in phase 2's `buildInputs`; ordinary Nix
derivation semantics then force phase 1's outputs to real bytes before
phase 2's build script starts. Phase 2 has no nixgg shims on PATH, so it
never re-enters the wall.

**Principle**: if you hit this wall, don't push further into
`mkNixggBuild`'s target mechanism — it's provably self-similar to the outer
wall (any dyn-drv output is a stub until the *next* real derivation
boundary). Check whether the recipe has a clean stopping point *before* the
first synchronous read-back. If yes, split there with a plain
`stdenv.mkDerivation` for the tail. If the read-back is interleaved with
acceleration-needing work on both sides, there is currently no fix short of
a new Nix daemon RPC — don't spend time on a client-side workaround.

Related, smaller instance of the same constraint: `fd3810e` explicitly
declined to build a build-phase-caching analog to `configureCacheStdenv`
because real ELF binaries with baked-in RPATH exist by end of `buildPhase`
and text substitution can't retroactively fix that — a documented
non-starter, not an unsolved bug.

### 1.2 Why sandbox mode writes a `drvref` stub file, not a symlink

**Incident**: mosh's Makefile does `test -e ../crypto/libmoshcrypto.a` (a
prerequisite check) before linking. `builder-rpc-v0` registers `.drv` files
with the daemon but **never materialises them into the sandbox
filesystem** — so pointing the caller-visible output at the `.drv` with an
ordinary symlink would create a dangling symlink, and `test -e` on a
dangling symlink returns false, silently breaking the Makefile's own logic.

**Fix**: `internal/sandbox.PointOutputAtDrv` writes a small **regular
file** instead, `internal/drvref`'s wire format:
```
#!nixgg-drvref\n
/nix/store/<hash>-<name>.drv\n
```
bounded at 4096 bytes (`drvref.go`). A regular file passes `test -e`;
`classify.Target`/`resolveLibFlag` read the header to recognize it as an
unresolved sandbox output.

**Contrast — EagerDrv mode (`NIXGG_EAGER_DRV=1`, commit `9f3ea7b`)**: the
*same* code path (`expr.CompileJSON`/`LinkJSON`, `sandbox.DerivationAdd`)
runs over an ordinary unrestricted daemon connection (e.g. `nix develop`)
instead of a live sandbox. Here the just-registered `.drv` **is already a
permanent store object**, so `PointOutputAtDrv` symlinks straight to it —
no dangling-symlink risk, because there's no live sandbox to leave a
dangling reference inside. This is the one place the "sandbox and
eager-drv share all drv-construction logic" claim is deliberately false —
verified: `grep -rn "sandbox.Enabled() || sandbox.EagerDrv()" go/internal/`
finds exactly 7 call sites, all pure "which serializer" branches; the
`PointOutputAtDrv` stub-vs-symlink branch is the one exception, and it's a
filesystem side-effect, not a derivation-content difference.

**Principle**: don't assume "sandbox mode" is one thing. Check
`sandbox.Enabled()` vs `sandbox.EagerDrv()` separately — most code treats
them as one bucket (correctly, since content is identical), but anything
touching *how the output is pointed at* needs the distinction.

### 1.3 The `builder-rpc-v0` op allowlist — verify against real source, not memory

**Incident**: `nixgg-sandbox.nix` (introduced dead-on-arrival in `1722fda`)
tried running `nix-instantiate` inside a `builder-rpc-v0` sandbox on
existing native-mode `.nix` thunks — an attempt to reuse native mode's
resolution machinery from inside the sandbox. Failed: the restricted RPC
op allowlist doesn't expose what `nix-instantiate` needs. Kept in-tree,
unused, as a documented dead end.

**Principle**: `go/internal/rpc`'s implemented op set (`opAddToStore` (7),
`opSubmitOutput` (1000), `opAddToStoreScanning` (1001) — see
`protocol.go`) is deliberately a small, hand-verified subset of the real
worker protocol, pinned to `NixOS/nix@8307c48` (PR #15793, protocol 1.39).
`SetOptions` is *never* sent — confirmed by direct experiment that it fails
inside a `RecursiveSubmitted` connection with `"Operation 19 not allowed
inside derivation"`. Before assuming any daemon op is available inside a
sandbox, check `src/libstore/daemon.cc`'s `performOp` at the pinned commit,
or run the experiment — don't infer from the general (non-sandboxed)
worker-protocol docs, which describe a superset.

### 1.4 `RealiseThunkArgsAndPassthrough`'s EagerDrv gap — accepted, not fixed

**Location**: `go/internal/shim/passthrough.go`, docstring on
`sandbox.go:231-236` (current source; the gap was introduced in
`9f3ea7b`, but comment-trimming since then has moved these lines).
When `Archive`/`Link` give
up modeling an invocation and fall back to raw `Passthrough`, native mode
first force-realizes any argv token that's a `classify.Thunk` symlink
(otherwise a still-unrealized `.nix` thunk path gets baked as a literal
member into e.g. a thin archive — this was itself a real bug fixed in
`0418bd3` for the Linux kernel's `built-in.a`).

**The gap**: under `NIXGG_EAGER_DRV=1`, an unresolved sibling is a
`classify.Drv` (real symlink to a `.drv`), not a `classify.Thunk` — and the
passthrough realize-loop only checks for `Thunk`. So a Drv-kind arg reaching
this fallback is handed to the real tool unresolved.

**Why left alone**: fixing it would require a synchronous
realize-a-registered-drv operation, which doesn't exist for sandbox mode
(same wall as 1.1) and has zero observed real-world trigger (not hit across
any fixture including a ~500-TU build, `examples/nix-full`). Compounding
three off-mainline conditions (unparseable link line → falls to
Passthrough → arg happens to be Drv-kind → under EagerDrv specifically) with
zero repros isn't worth solving speculatively.

**Principle**: don't "fix" this by mechanically extending the Thunk check
to Drv — there's no realize mechanism to call. Wait for a real repro.

---

## 2. Cross-mode correctness

### 2.1 The byte-identical-drv invariant, and what breaks it

Established in `0d5d6f4` ("sandbox drvs are now byte-identical to native
drvs"), pinned as a regression test by `3020a54`
(`tests/drv-equivalence.sh`). Four axes were found to diverge before that
commit and had to be forced identical:

1. Source-tree store name (`nix store add -n <name>` naming convention).
2. Compile build-script/env shape.
3. `tuID` computed from a workspace-**relative** path — an earlier version
   used the absolute path, which is mode-dependent (differs between a
   `nix develop` shell's cwd and a sandbox's build root) and therefore
   diverged the thunk-id/drv hash between modes for the same logical TU.
4. Link/archive env-shape parity (`builder`, `outputHashAlgo`,
   `outputHashMode`, `_storeDeps`).

**Mechanism that keeps this true going forward**: `go/internal/expr`'s
`Derivation.buildScript`/`envDict` are the *single* functions that render
shell text and env dict for both wire formats — native mode's `ToNix`
(`.nix` thunk, tag-templated markers resolved later by
`nix/resolve-script.nix`) and sandbox mode's `toJSON` (fully-resolved JSON
drv). Neither serializer has its own copy of this logic. Verified directly:
`buildScript`/`envDict`/`outSubdir`/`ArtifactSubdir` in
`expr/derivation.go` have **zero mode-conditional code** — the mode split
lives entirely in the two thin top-level entrypoints.

**Concrete asymmetries that were made deliberately symmetric** (each one a
place a "reasonable" implementation would diverge):
- `_extraInputs` env key is present, even when empty, in *both*
  serializers — an earlier asymmetry (present-but-empty vs. truly absent)
  itself changed the hash.
- `ArtifactSubdir(name)` is keyed on the artifact's **filename**
  (`.a`→lib, `.o`→flat, else→bin), never on the producing drv's `Kind` —
  because in native mode a sibling reference is a `.nix` thunk path (no
  Kind info available), while in sandbox mode it's a `.drv` path (Kind is
  legible from the name). Keying on Kind would silently diverge the two
  modes' rendered scripts.
- `StoreAddDirectory` (non-scanning `nix store add`) vs `StoreAddScan`
  (scanning `nix store add --scan`) — see 2.2, this is the sharpest
  instance of this pattern.
- `batch.Classify` matches against a TU's **absolute** path with
  unanchored segment search, not `scan.ProjectRoot`-relative — because
  `ProjectRoot` (common ancestor of cwd + `-I` dirs) is recomputed per
  compile call and is unstable across invocation directory.

**Principle for any new producer/consumer pair**: "byte-identical output
regardless of mode" forces agreement on something **mode-independent**.
Before adding a new field/lookup shared across native and sandbox code
paths, ask: does this value depend on cwd, on which mode is active, or on
information only one mode has cheaply available (e.g. Kind from a `.drv`
name)? If so, it's a latent divergence. This exact category of bug has
recurred at least 5 times in this repo's history (the four axes above plus
the scan bug in 2.2) — it's the single most common bug class here.

### 2.2 `StoreAddScan` vs `StoreAddDirectory` — the reference-tracking divergence

**Incident**: uploading a compile TU's staged source tree via the scanning
`nix store add --scan` picks up *any incidental* `/nix/store/...` substring
inside the staged content — e.g. a generated header baking in a runtime
tool's own store path (`sandbox.go`'s own comment cites this against
`store-api.cc`'s TU, but says `examples/nix-util`, a directory that does
not exist in this repo — the actual fixture that builds `store-api.cc`,
part of libstore, is `examples/nix-full`; this looks like a stale
reference baked into the source comment itself, not just this doc) — and
records it as a real NAR reference. Native mode's
plain `import <path>` of the same bytes never does this. Result: sandbox
mode computed a *different* store path than native mode for byte-identical
staged content, breaking the drv-equivalence invariant (2.1).

**Fix** (commit `9f3ea7b`): split into two functions with different
semantics, both in `go/internal/sandbox/sandbox.go`:
- `StoreAddScan` — scanning, kept only for the one place scanning is
  actually required: `cli/assemble.go`'s tree-restore path (splitStdenv's
  build-stage postBuild, where unregistered references would genuinely
  break the build).
- `StoreAddDirectory` — plain, non-scanning `nix store add -n name path`,
  used for every per-TU source-tree upload (`compile.go`, `batchdefer.go`).
  Its reference set is always empty, matching native mode's plain
  path-literal import.

**Principle**: "upload a directory to the store" is not one operation.
Scanning and non-scanning add return the *same bytes but different
reference sets*, which means different store paths whenever the content
contains a `/nix/store/...` substring anywhere. Any new sandbox-mode
directory-upload call site must consciously choose scanning-vs-not to
match what native mode's own import mechanism does for that content — not
copy whichever call happens to already be in the file being edited.

### 2.3 Why the drv-equivalence invariant needed a THIRD (and fourth, fifth) check beyond hash equality

`tests/drv-equivalence.sh` (149 drvs across 5 fixtures) proves native and
sandbox drv **hashes match**, computed in **separate stores**. This
sounds like the whole invariant, but it structurally cannot catch several
real bug classes — each one got its own dedicated test after being
identified as a gap (README.md's own framing, verified against current
`tests/`):

1. **Hash-match without functional correctness.** drv-equivalence.sh never
   *realises* an output. README/smoke.sh's own header states plainly: it
   "once reported a clean 149/149 while link outputs had silently moved
   and native builds failed to collect their artifact." Closed by
   `tests/smoke.sh` — builds, places, and *runs* every example.

2. **Different derivation Kinds it doesn't check at all.** The batch-archive
   Kind (`internal/expr/batcharchive.go`) and the thin-archive
   member-propagation mechanism (`internal/members`) are structurally
   different derivation shapes drv-equivalence.sh's regex-based
   TU/archive/link Kind matching never sees. Closed by
   `tests/batch-drv-equivalence.sh` (also does a *functional* check: `ar t`
   on the resulting archive, non-empty non-zero-size members) and
   `tests/thin-archive-equivalence.sh` (goes further than hash matching —
   `nix copy`s the closure into the *real* daemon store and actually
   `exec`s both binaries, because a thin archive's member-propagation bug
   doesn't touch the drv hash at all, only the link/run result).

3. **Rebuild blast-radius.** Hash-set equality says nothing about "one
   TU recompiled" vs "every TU recompiled" for a one-line edit — both
   states can pass drv-equivalence.sh and smoke.sh simultaneously. Closed
   by `tests/perf-regression.sh` (asserts exactly 1 of lua's 34 TUs
   rebuilds on a one-file edit, and that it's specifically the right one —
   count *and* identity both checked).

4. **Configure-time early-cutoff correctness.** Closed by
   `tests/configure-cache-cutoff.sh` (native `splitAtConfigure`) and
   `tests/dyndrv-configure-cache-cutoff.sh` (combined
   `splitAtConfigure+splitAtBuild`, structurally different drv nesting —
   the configure-stage drv sits one level deeper, inside the build stage's
   own `.drv.drv` input).

5. **Structural equality without actual store-level substitution.**
   drv-equivalence.sh compares hashes across two *separate* stores — it
   never proves that a drv one mode built is actually **reused** (not
   rebuilt) by the other mode in the *same* store. `tests/cross-mode-reuse.sh`
   closes this: hand-build 2 of lua's 32 TUs under native mode in a real
   `nix develop` shell, force-realize them, then run a full sandboxed
   `nix build .#lua` with substituters disabled and assert (a) those 2
   TUs' native-built drv paths never appear in this run's "building"
   log, **and** (b) they *are* referenced in some drv the sandbox build
   actually produced. Both halves matter — checking only (a) can't tell
   "substituted" apart from "hash mismatch made them simply irrelevant."
   A negative control (the other 30 TUs *do* show ≥25 genuine "building"
   lines) guards against a stale store making everything look
   pre-existing for the wrong reason.

**Principle**: hash equality between two build modes is necessary but not
sufficient for "these are interchangeable." Before trusting a structural
equivalence check, ask what it *cannot* see: realization, a different
derivation shape entirely, blast radius, and same-store substitution are
four separate properties that all needed their own test in this repo.

---

## 3. Performance measurement discipline

### 3.1 rpcHelper: a real, measured null result — not an assumption

**What was tried**: `go/internal/helper` (added `976a930`) — a persistent,
connection-pooling daemon-side relay meant to amortize the ~4.3ms
per-connection worker-protocol handshake across a whole build, layered on
top of the already-landed direct-RPC path (see 3.2).

**Measured three times, increasing rigor, each commit citing the prior
one's data**:
1. `b2015ab`, mosh (30 TUs): 48.7s → 47.1s, ~3% faster, but lower variance
   (stdev ~1.9s→~0.4s). Framed as small because mosh's ~54 RPC calls only
   amortize ~230ms out of a ~48s wall clock.
2. `0322d7c`, redis (175 TUs, ~6x mosh's TU count) — testing the hypothesis
   that the win scales with TU count. It didn't: 65.4s vs 64.7s, well
   within both runs' own ~3.4-5.3s stdev. Redis's own per-drv bookkeeping
   overhead scales up right alongside TU count, swallowing the extra
   amortized handshakes.
3. `a855385` — isolated the actual shim-heavy phase (most of the wall clock
   is flake eval + autoconf configure, untouched by RPC/helper either way)
   down to ~4.7s of a 20-48s build. Final result, 8 runs each way,
   single-file edit, configure cached: **2.74s direct-RPC vs 2.71s with
   the helper — t-stat 0.61, statistically indistinguishable from zero.**

**Root cause of the null result** (the reusable insight, not just the
number): the helper amortizes a ~4.3ms handshake, but Nix's own
per-derivation overhead — forking a builder, sandboxing, mounting the
store — is **~10-20x that** (roughly 40-90ms). An order of magnitude too
large for handshake savings to move. Contrast with 3.2's *real* win, which
eliminated something comparable in size to that per-derivation overhead
(a whole `nix` CLI process fork+exec, ~50-90ms/call) — that's why one
optimization landed and the other didn't, and it's a good heuristic for
judging any future optimization proposal in this codebase: compare its
savings-per-call against Nix's own ~40-90ms per-derivation floor before
building it.

**What happened to the code**: fully removed in `0f1281c` ("Remove
rpcHelper (go/internal/helper): measured, non-beneficial"), including its
`cli/main.go` wiring, the `rpcHelper` param from
`mkNixggBuild.nix`/`dynDrvConfigureCacheStdenv.nix`, and every `-helper`
example fixture. README.md keeps the measurement writeup permanently
("Removed rather than kept as dead infrastructure; the measurement and
design are preserved in git history if per-derivation overhead ever drops
enough to revisit") — this is intentional: don't re-propose
connection-pooling for sandbox RPC without first re-measuring whether
Nix's per-derivation overhead has actually dropped by an order of
magnitude.

**Bonus finding from the same effort**: while wiring the helper through
`dynDrvConfigureCacheStdenv.nix` to isolate the measurement, `nixgg
assemble`'s tree walk turned out to have no exclusion for the helper's own
`.nixgg-helper.sock`/`.pid` files (same class of gap `.nix-socket` already
had an exclusion for) — `nix store add --scan` failed outright whenever
the helper combined with tree assembly. Fixed as a byproduct of the
measurement work, not the original goal — a reminder that isolating a
performance measurement properly can itself surface real bugs.

### 3.2 Direct-RPC's 48% win — measured, and why it's real where the helper wasn't

**Problem**: before `internal/rpc` (`e780253`), every sandbox-mode shim
call forked+exec'd the `nix` CLI once per op (`nix derivation add`,
`nix store add --scan`, `nix store submit-output`) — ~20-90ms/call
depending on load, dominated by CLI process startup + daemon reconnect,
not real work.

**Measured** (README.md, lua fixture, 34 TUs, isolating shim-pass overhead
from real compile time — single-file edit, warm store, substituters off, 5
runs averaged each way): **~1.46s with the RPC path vs ~2.83s via CLI
fallback — ~48% faster.** `internal/rpc` speaks the Nix worker protocol
directly over the sandbox daemon's own socket; `internal/aterm`/
`internal/nar` render the exact bytes the CLI ops would compute
(byte-exact verified against the pinned Nix source, `NixOS/nix@8307c48`).

**On by default**: `NIXGG_RPC=1` in every sandbox-mode env block
(`nix/dynDrvShared.nix`, `nix/mkNixggBuild.nix`); `NIXGG_RPC=0` is the CLI
fallback escape hatch, still exercised by `sandbox.selectBackend()`.

**Explicit scope caveat** (don't quote this number out of context): "the
win scales with TU count and rebuild frequency, not with compiler time...
and nearly vanishes on a cold build, where real `g++` invocations dominate
either way." It's a warm-rebuild, shim-overhead-isolated number, not an
end-to-end wall-clock claim.

**Two real bugs found while verifying byte-for-byte against a live
`.drv`** (worth knowing if you touch `internal/aterm`/`internal/rpc`):
a missing post-handshake STDERR drain, and `JSONDrvInputs.Drvs`' map keys
being basenames rather than full paths — contradicting its own doc comment
at the time.

**Principle**: this is the contrast case to 3.1. Both optimizations were
built, both were rigorously measured before being trusted. One survived
because it cut something on the same order as Nix's own per-derivation
floor (a whole CLI process); the other didn't because it cut something an
order of magnitude smaller. When proposing a new "make sandbox mode
faster" change, estimate which category it falls into *before* building
it — this repo now has calibration data for both.

---

## 4. Things that looked impossible but were not

### 4.1 Thin-archive (`ar T`) support

**The wrong analysis, first pass**: a thin archive (`ar csrDT`) stores
member file *paths*, not embedded bytes. First analysis concluded this
was structurally impossible in nixgg's per-derivation-sandbox model: an
archive-creation derivation's build directory is torn down after the
build, so a thin archive referencing paths inside it would dangle once
that sandbox is gone.

**Why that was wrong**: nixgg already resolves every archive member to a
**permanent, immutable** `/nix/store/<hash>-name/...` path before ever
invoking `ar` (verified directly in `expr/derivation.go`'s script
template) — so a thin archive built from those store paths stays valid for
as long as Nix's own derivation-input-declaration model keeps the
referenced path alive, which it already guarantees. Confirmed by a direct
experiment under `/tmp`: an absolute-path thin archive linked successfully
from an unrelated directory *after* the archive-creation sandbox was
deleted. (A relative-path thin archive did break in the experiment, but
for an unrelated, GNU-`ar`-specific reason — `ar` resolves relative member
paths against the archive file's own location, not cwd — not a nixgg/Nix
problem, and not a path nixgg ever produces.)

**What was actually missing** (once "impossible" was disproven), fixed in
`9f09f15`/`843bbb0`:
1. `isARModifiers` didn't accept `T` at all — every thin-archive call fell
   to unaccelerated `Passthrough`.
2. A later consumer of a thin archive (typically a link step) must
   separately declare the archive's own resolved members as inputs, since
   Nix only mounts what a derivation's `inputs.drvs`/`inputs.srcs`
   declare, and a thin archive's on-disk bytes are just paths. Fixed via
   a new sidecar, `go/internal/members` — kept separate from `drvref`
   specifically because `drvref`'s wire format is bounded to ~4096 bytes
   and a thin archive can have hundreds of members (QEMU's
   `libqemuutil.a` has 450).
3. A real second bug found wiring this up: an early version merged
   propagated members straight into the rendered *argv*, which
   immediately produced `"multiple definition of 'foo'"` — the members
   are already reachable through the thin archive's own bytes, so listing
   them again on the link line makes the linker see each symbol twice.
   Fixed by keeping `ExtraInputs` dependency-declaration-only: it flows
   through the same input-list code paths (so both wire formats declare
   the dependency edge, preserving the drv-equivalence invariant of §2)
   but is excluded from the rendered script text.
4. A batching-specific instance of the *original* torn-down-sandbox
   concern turned out to be real, just in a narrower spot: a combined
   compile+archive batch derivation (`batcharchive.go`) used to write
   member objects to a build-tmp scratch dir, which genuinely is torn
   down with that derivation's own sandbox. Confirmed by experiment (a
   thin archive built from a tmp-relative scratch dir broke once that tmp
   was deleted) and fixed by writing thin batch archive members to
   `$out/lib/.nixgg-objs/` — the derivation's own **permanent** output —
   so Nix rewrites the self-reference to the final resolved store path.

**Principle** (this is the one the user's own memory notes flag
explicitly): this project has burned itself once already declaring a
sandbox-model conflict "structurally impossible" without running the real
experiment. Before writing "X can't work because the sandbox tears down
Y" into a doc or a design decision, run the actual experiment (a `/tmp`
repro is usually cheap) — the *general* shape of the concern
("does a resource still exist after teardown") is often right, but the
*scope* of what's actually affected is easy to overestimate without
checking. Item 4 above is the calibration case: the same-sounding concern
was wrong for plain archives but right, in a narrower form, for batched
ones — you have to actually check which case you're in.

---

## 5. Batching pitfalls — three distinct bugs in one feature

TU-batching (`internal/batch`, `internal/batchmember`,
`internal/batchpending`, `expr/batcharchive.go`) folds N per-TU compiles +
1 archive into a single derivation, motivated directly by 3.1's finding
that Nix's own per-derivation overhead (not connection/handshake cost) is
the real bottleneck — so the next lever is derivation **count** itself.
Getting this right at real scale (ffmpeg, LLVM, QEMU) surfaced three
separate, unrelated bugs:

### 5.1 Object-collision (fixed `c12e581`)
Batch member compiles all wrote into one shared scratch directory keyed
only by output **basename**. ffmpeg has real files sharing a basename
across subdirectories: `libavutil/cpu.c` + `libavutil/x86/cpu.c`,
`libswscale/swscale.c` + `libswscale/x86/swscale.c`. The second compile
silently overwrote the first's object — no build error, just a link
failure downstream (`undefined reference to 'av_get_cpu_flags'`) and an
`.a` whose `ar t` listing showed the same member name twice. Fixed with
`disambiguateOutNames` — deterministic `-2`/`-3`... suffixing for
colliding basenames. **Principle**: a scratch dir keyed on basename alone
is never safe once source trees have >1 subdirectory; key on something
that includes the source path.

### 5.2 `MAX_ARG_STRLEN` (fixed `3310f69`, hit twice independently)
A combined batch script embedded as literal derivation `args` text blows
the kernel's 131072-byte argv limit once enough members accumulate.
Measured concretely: LLVM's `libLLVMSupport` batch script was 152853
bytes; ffmpeg's `libavcodec` batch script hit ~1MB. Confirmed at build
time as literally "Argument list too long." Fixed via `passAsFile`
(`Env["batchScript"]` instead of `Args`) — mirrored in both
`expr/batcharchive.go` (sandbox JSON drv) and `nix/batchArchiver.nix`
(native). **This exact fix pattern was independently discovered twice**:
first for `nixgg assemble`'s own restore script (`04aa745`, motivated by
openssl's 2230 stubs), then again for batch archives (`3310f69`). An
earlier attempt at the assemble.Build fix staged the script via
`nix store add` ahead of derivation creation — this broke every CA-output
example, because store-added files don't get CA-placeholder substitution
and `passAsFile` does. **Principle**: any generated shell script embedded
as derivation `args` or a single env string has a hard kernel-level size
ceiling; route it through `passAsFile` from the start if its size scales
with input count (member count, stub count, file count) rather than
waiting to hit the limit on a large enough real package.

### 5.3 Parallelism loss (fixed `889b65a`)
A combined batch derivation is **one builder process** running N compiles
serially with no `&`/`wait` — a concern that simply doesn't exist for
ordinary per-TU derivations, where Nix's own scheduler runs each one
concurrently. Confirmed via `ps aux`: exactly one `gcc`/`cc1` process alive
at a time for the entire ~350-TU libavcodec batch, versus many concurrent
processes for the unbatched path. Fixed by backgrounding each member
compile, bounded at `$NIX_BUILD_CORES` via an explicit-pid FIFO wait
runner (deliberately **not** `wait -n` — the script's own comment notes
`wait -n` has a real job-reaping race that can silently lose a compile
failure's exit code). Verified post-fix: up to 18 concurrent compiler
processes observed on a real mosh-batch build.

**Net effect once all three were fixed**: ffmpeg's derivation count went
2093 → 23 (99% reduction); LLVM's phase-1 drv count 186 → 13; QEMU's
(thin-archive, see 4.1 item 4) 1383 → 933.

**Principle for this whole section**: "combine N derivations into one" is
not a purely additive change — it silently removes properties the N
separate derivations gave you for free (per-derivation output-name
uniqueness, an argv-size ceiling per derivation instead of per batch, and
Nix's own build-scheduling parallelism). Each of those had to be
re-implemented by hand inside the combined derivation's own build script.
When reviewing or extending a batching feature, check all three
categories explicitly rather than assuming "it built successfully" means
it's correct or fast.

---

## 6. Test-environment traps

### 6.1 Asserting on a value that depends on host toolchain specifics

**Incident** (`7d1a680`, "Fix flaky TestRunScannerFindsIncbinTargets on
real system gcc (CI)"). The test asserted on `Header.Rel` — a path
relative to `projectRoot` — from `internal/scan`'s incbin scanner. A real
system gcc injects an implicit predef header (glibc's `stdc-predef.h`)
that lands *outside* `projectRoot`, which widens `projectRoot` all the way
to `"/"`, shifting `Rel` unpredictably. This repo's own dev shell uses
nixgg's hermetic gcc-wrapper, whose implicit headers all live under
`/nix/store` — so it never widened `projectRoot` and never hit this. Only
CI, which used `nix shell nixpkgs#gcc` (a real, non-hermetic system gcc),
triggered it.

**Fix**: assert on `Header.Abs` instead — the value staging actually
copies, and the thing the test is actually trying to prove (that the
`.incbin` target was discovered), not an incidental relative-path
computation that's coupled to which compiler produced the implicit-include
list.

**Principle**: any test assertion on a *relative*-path-derived field
computed from header-scanning/`projectRoot` output is implicitly coupled
to which compiler produced it. Assert on the absolute path (structurally
guaranteed) unless the relative form is literally the thing under test.
More generally: if a test only fails in CI, check first whether CI's
toolchain differs from the dev shell's hermetic one before assuming a
real regression.

### 6.2 Direct-exec checks masked by ambient host `/nix/store` state

**Incident** (`82148fc`, related `5bc25dd`). `smoke.sh`'s direct-exec
check ran a sandbox-built binary straight from its `$ALT_STORE`-relative
on-disk path. This "worked" on the dev machine only by coincidence: a
binary's ELF interpreter reference and RPATH always name the *canonical*
`/nix/store/...` path, never the alt-store's `local?root=...` prefix — a
raw `execve()` only resolves if the real `/nix/store` happens to already
have that exact hash cached from unrelated Nix usage. The dev box had this
by ambient history; a fresh CI runner had none, so the identical build
failed only there.

**Fix**: on a failed direct exec, warn and fall through to `nix run`
instead of hard-failing — `nix run` privately bind-mounts `$ALT_STORE`
onto `/nix/store`, correctly resolving RPATH/interpreter, and is the
actually-reliable check. Same root cause hit `thin-archive-equivalence.sh`
separately (`5bc25dd`) — fixed there by `nix copy`-ing into the real store
before exec.

**Principle**: any test that `exec`s a binary resolved through an
alt-store path (rather than via `nix run` or `nix copy` into the real
store first) is silently depending on ambient host `/nix/store` cache
state for ELF loading to work. This class of bug is invisible on any
machine with normal Nix usage history and surfaces only on a genuinely
fresh store — the first thing to suspect for "works on my machine, fails
only in CI" when the failure involves ELF loading.

### 6.3 "Fix verified locally" can be a false pass if local state is warm

**Incident** (`9e88ab4` → `ff23873`, two-commit fix for the same bug).
First attempt fixed "native-src resolution racing/failing in a fresh
isolated store" by swapping `nix flake archive --json` for
`builtins.getFlake ... .outPath`. Locally this appeared to work — but the
local re-test was a **false pass**: the local store already had every
flake input warm from earlier session work, masking the exact same
"fresh store" failure mode the fix was supposed to solve. `getFlake` still
has to lock the *entire* transitive input graph before returning even one
field, so it paid the identical cost/failure mode as the thing it
replaced, just wasn't caught until CI (a genuinely fresh store) failed
again. Actual fix: read the flake-lock node directly for inputs that are
`flake = false` (self-contained, no sub-inputs) and fetch via
`builtins.fetchTree`, verified under `env -i` with a throwaway `HOME`.

**Principle**: (1) there is no cheap way to resolve "just one flake
input" via any `getFlake`-family call — it still locks the whole graph.
(2) When verifying a fix for a "fails on fresh/empty state" bug, a local
retest is untrustworthy unless you deliberately manufacture fresh state
(`env -i`, throwaway `HOME`, empty store) — a warm local cache will
falsely validate a fix that doesn't actually address the fresh-state case
at all.

### 6.4 Misleading errors from sandbox/CI infra, not the build itself

**Incident** (`b6385bd`). Every CI job failed identically at the *very
first* derivation build (a plain `runCommand` copying `./nix`) with
`error: executing '.../bash': No such file or directory` — before any
nixgg-specific logic ran. The file obviously exists; the real cause is
`ubuntu-latest`'s AppArmor policy blocking unprivileged user-namespace
creation, which Nix's build sandbox needs to construct its mount
namespace — the mount-namespace failure surfaces as a bogus ENOENT on the
builder binary. Fixed with `sudo sysctl -w
kernel.apparmor_restrict_unprivileged_userns=0` as a CI step before the
Nix installer action, on every job (must be repeated per job — each runs
on a fresh VM).

**Principle**: a "file not found" for a binary that plainly exists,
occurring at the very first build step before any project logic runs, is
the signature of a sandbox/userns setup failure, not a missing-file bug —
check host sandboxing config first.

---

## 7. Miscellaneous durable decisions worth not re-litigating

- **`.override`/`.overrideAttrs` cannot patch `splitStdenv`'s configure or
  build stage** — nixpkgs' override contract always re-invokes the
  original package function with unpatched attrs *before* applying the
  override, but by then `splitStdenv` has already built separate,
  already-submitted stage derivations from the original attrs. An
  `.overrideAttrs` on the final returned package only ever reaches the
  outermost (install) stage. Fixed with call-site escape hatches
  (`extraConfigureAttrs`/`extraBuildAttrs`/`extraInstallAttrs`/
  `extraAttrs`, in `nix/splitStdenv.nix`) that splice in *before* each
  stage's attrset is finalized, so `old` in the callback is genuinely that
  stage's own already-computed attrs. `extraInstallAttrs` exists "mostly
  for symmetry" — install-stage IS reachable by ordinary `.overrideAttrs`
  on the returned package; only configure/build have the reapplication
  problem. Verified end-to-end on zstd, whose cmake build execs its own
  freshly-compiled `gen_html` helper mid-build — a case sandbox mode can't
  satisfy (same wall as §1.1) — fixed by building `gen_html` standalone via
  a separate `mkNixggBuild` call and patching CMakeLists.txt to point at
  it via `extraBuildAttrs`/`extraAttrs`.

- **`configureSrcFilter` cannot help when the package's own build system
  globs its source tree at configure time.** `file(GLOB ...)` in
  CMakeLists.txt (zstd's case) is evaluated by CMake *during* configure to
  build the real target graph — any file's presence/absence changes the
  glob result, so the include filter would have to include the entire
  globbed subtree, defeating the point. This is a structural limitation of
  the filtering approach, not a bug: `zstd-cache`/`zstd-dyndrv-*` in
  `flake.nix` deliberately don't use `configureSrcFilter` at all, relying
  only on the plain CA early-cutoff. `fmt` is the contrast case (its
  CMakeLists.txt lists sources explicitly, no glob) where filtering does
  help. **Principle**: before adding `configureSrcFilter` to a new
  package, check whether its build system enumerates sources via glob at
  configure time — if so, don't bother; verify with a real build
  (`tests/configure-cache-cutoff.sh`-style before/after path comparison),
  not by inspecting the filter list, since an under-inclusive filter fails
  silently (stale build, not an error).

- **`targets` on `mkNixggBuild` is a list of `{name, path}`, never an
  attrset** — an attrset iterates alphabetically in Nix, not in
  declaration order, and an earlier version of this code silently picked
  the wrong entry for the singular back-compat `result`/`package` fields
  because of this. If you're tempted to "clean up" this API into an
  attrset, don't — it was an attrset once and that was the bug.

- **`knownStorePathInputs` uses `.all or [p]`, not a transitive closure**,
  over each buildInput. Expanding transitively was tried and reverted:
  it broke native/sandbox drv-hash equivalence (§2.1's invariant) when
  tried. `.all` (multi-output packages' own output list) is enough to
  catch dev outputs like zlib/ncurses/openssl's `-isystem`/`-L` additions
  without pulling in enough extra path text to diverge the two modes'
  hashes.
