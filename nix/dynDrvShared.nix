# Shell-script-generating helpers used by nix/splitStdenv.nix's build
# stage and final-stage restore logic. Originally extracted because
# they were byte-for-byte identical text duplicated across two now-
# deleted files (dynDrvStdenv.nix, dynDrvConfigureCacheStdenv.nix; see
# git history — 1d605d5, 9bec045 — both bugs had to be fixed by hand
# in both copies before this extraction). Kept as its own file now
# that there's only one caller: these are pure shell-script factories,
# a different kind of thing from splitStdenv.nix's own Nix-level
# stage-wiring logic.
#
# Every binding here closes over exactly the params splitStdenv.nix's
# build stage needs (lib, nixgg, patchedNix, bash, coreutils, gcc,
# gnumake, system), plus two optional flags (passthroughPaths,
# sharedStaging) — its configure-only stage needs none of these.
{
  lib,
  patchedNix,
  nixgg,
  bash,
  coreutils,
  gcc,
  gnumake,
  # Subtrees the caller has declared unmodellable; the shims pass work
  # under them straight through (internal/mode). Empty by default so
  # dynDrvConfigureCacheStdenv, which does not pass it, emits exactly
  # the environment it did before this parameter existed.
  passthroughPaths ? [ ],
  # Stage each TU as a symlink farm into per-file store objects rather
  # than copying (internal/stage's SourcesShared). Off by default so
  # dynDrvConfigureCacheStdenv, which does not pass it, emits exactly
  # the environment it did before this parameter existed.
  sharedStaging ? false,
  system,
}:

{
  # NIXGG_* vars match mkNixggBuild.nix's toolchainEnv.
  #
  # NIXGG_SANDBOX_TARGET is set to an unmatchable path rather than left
  # unset: link.go's maybeSubmit defaults to submitting whatever it
  # links as the outer drv's "out" when TARGET is empty — correct for
  # mkNixggBuild's single-target builds, wrong here, where `nixgg
  # assemble` (postBuild) owns the one "out" submission for the whole
  # tree.
  #
  # knownStorePathsJSON is a parameter (not computed once here) because
  # splitStdenv wraps an arbitrary existing package — the real store
  # paths the shim needs to recognize vary per package and are only
  # known from probeArgs.buildInputs, read per-call by each caller.
  ggShimsOnPath = knownStorePathsJSON: ''
    export PATH="${nixgg}/shims:${patchedNix}/bin:$PATH"
    export NIXGG_ROOT="${nixgg}"
    export NIXGG_COMPILER_ROOT="${gcc}"
    export NIXGG_BASH_ROOT="${bash}"
    export NIXGG_COREUTILS_ROOT="${coreutils}"
    export NIXGG_GNUMAKE_ROOT="${gnumake}"
    export NIXGG_REAL_CC="${gcc}/bin/g++"
    export NIXGG_NIX="${patchedNix}/bin/nix"
    export NIXGG_NIX_HELPERS="${nixgg}"
    export NIXGG_SANDBOX=1
    export NIXGG_STORE="auto"
    export NIXGG_SYSTEM="${system}"
    export NIXGG_SANDBOX_TARGET="/nonexistent/nixgg-phase1-no-per-artifact-submit"
    ${lib.optionalString sharedStaging "export NIXGG_SHARED_STAGE=1"}
    export NIXGG_PASSTHROUGH_PATHS=${lib.escapeShellArg (builtins.toJSON passthroughPaths)}
    export NIXGG_KNOWN_STORE_PATHS=${lib.escapeShellArg knownStorePathsJSON}
    # Raw worker-protocol client for the sandbox's own daemon socket
    # (internal/rpc) instead of per-call fork+exec — see
    # mkNixggBuild.nix's own comment on this same var for the
    # verification this default rests on. NIXGG_RPC=0 is the escape
    # hatch back to the CLI fallback.
    export NIXGG_RPC=1
    export NIX_CONFIG="extra-experimental-features = nix-command ca-derivations dynamic-derivations"
  '';

  # Records $PWD's offset relative to $NIX_BUILD_TOP (phase 2 needs to
  # cd back there — cmake's `mkdir build && cd build` means $PWD here
  # isn't $NIX_BUILD_TOP) and phase 1's exported environment (phase 2
  # replays it gap-filling; see splitStdenv's ggRestoreEnv), then
  # walks $NIX_BUILD_TOP for every drvref stub the shims left, builds
  # one assembly drv that restores the tree and resolves each stub, and
  # submits it as this derivation's "out".
  #
  # The environment dump earns its keep because splitting one
  # mkDerivation across two derivations splits the SHELL too, and
  # packages routinely export a variable in one phase and read it in a
  # later one: linux-config's configurePhase does `export
  # buildRoot=...` and its installPhase is `mv $buildRoot/.config
  # $out`, which in phase 2 became `mv /.config` — "cannot stat
  # '/.config'".
  # See go/internal/cli/assemble.go / go/internal/assemble/.
  submitBuildTreeScript = drvName: ''
    realpath --relative-to="$NIX_BUILD_TOP" "$PWD" > "$NIX_BUILD_TOP/.gg-cwd"
    export -p > "$NIX_BUILD_TOP/.gg-env"
    ${nixgg}/bin/nixgg assemble "$NIX_BUILD_TOP" "${drvName}"
  '';

  # Phase 1's own derivation must declare exactly one output ("out") —
  # submit-output's ".drv" naming convention requires it — but build
  # systems bake bin/dev/man/etc. install paths into the
  # Makefile/CMakeCache at configure time. Give each of the package's
  # REAL non-"out" outputs its own subdir of the one tree via this
  # placeholder scheme, so a later restore/split step can rebuild the
  # real outputs from it.
  outputPlaceholder = o: if o == "out" then "/nonexistent" else "/nonexistent-${o}";

  # restoreOutputsScript and elfRpathFixupScript both take
  # `outputPlaceholder`, `realOutputs`, and `extraOutputs` as explicit
  # params (rather than closing over a shared `outputPlaceholder`
  # binding) — each caller's own realOutputs/extraOutputs are computed
  # from that caller's own probeArgs, so those two stay per-call.

  # Phase 2's restore/split step: for every real output, make the real
  # output dir and copy its placeholder subtree (installed by phase 2's
  # own installPhase, DESTDIR-relative) into it. `v`/`ph` are built via
  # string concatenation, not Nix's `${}` antiquotation, so the emitted
  # script contains a literal shell variable reference (e.g. "$bin"),
  # not a Nix-side interpolation of it.
  #
  # `[ -d ... ] &&` guards each cp: multiple-outputs.sh's automatic
  # per-output DESTDIR routing only takes effect when the package's OWN
  # build system actually threads $bin/$dev/... through to its install
  # step. Some packages don't — openssl installs everything flat under
  # out's placeholder and does its own real `mv $out/bin $bin/bin` in
  # postInstall instead — so `$DESTDIR<placeholder>` for that output is
  # never created at all. Failing on a missing directory there would
  # break every such package outright; skipping it just leaves that
  # real output empty for THIS script and lets the package's own
  # postInstall (which runs right after, per nixpkgs hook order)
  # populate it from the real $out however it normally does. Confirmed
  # necessary directly: openssl's own bin split hit exactly this.
  #
  # `mkdir -p "${v}"` lives INSIDE that same guard, not before it: a
  # package whose own postInstall creates that output dir itself may do
  # so with a bare `mkdir` (no `-p`), expecting a not-yet-existing
  # directory — openssl's own `mkdir $dev` (right before `mv
  # $out/include $dev/`) is exactly this. Pre-creating "$dev"
  # unconditionally here made that `mkdir` fail with "File exists".
  # Confirmed necessary directly.
  #
  # Two possible source locations, not one: a package's
  # makeFlags/installFlags can point an output at its own real,
  # absolute final path instead of the placeholder scheme here
  # (openssl's `MANDIR=$(man)/share/man`). Once DESTDIR is threaded
  # onto make's own command line, THAT absolute path also ends up
  # DESTDIR-prefixed — "$DESTDIR/nix/store/...-man", never under the
  # placeholder tree at all. Confirmed directly against openssl's man
  # output.
  restoreOutputsScript =
    outputPlaceholder: realOutputs:
    lib.concatMapStrings (
      o:
      let
        v = "$" + o;
        ph = outputPlaceholder o;
      in
      ''
        if [ -d "$DESTDIR${ph}" ]; then mkdir -p "${v}"; cp -a "$DESTDIR${ph}/." "${v}/"; fi
        if [ -d "$DESTDIR${v}" ]; then mkdir -p "${v}"; cp -a "$DESTDIR${v}/." "${v}/"; fi
      ''
    ) realOutputs;

  # cc-wrapper's ld-wrapper bakes a self-rpath into every linked
  # binary/shared-lib at LINK time, using whatever output path was live
  # then — one of the placeholders above. Confirmed directly: without
  # this rewrite, the dangling placeholder rpath survives, and
  # fixupPhase's own patchelf-based shrinkRPath step (correctly) drops
  # it since nothing exists at that literal path — leaving a shared-lib
  # consumer unable to find it at runtime, `nix run` failing with
  # "cannot open shared object file". Longest-placeholder-first order
  # (extraOutputs before "out") matters: "/nonexistent" is a literal
  # prefix of "/nonexistent-bin", so substituting it first would also
  # mangle the longer placeholders.
  elfRpathFixupScript =
    outputPlaceholder: realOutputs: extraOutputs:
    let
      sedOrder = extraOutputs ++ [ "out" ];
      sedExprs = lib.concatMapStrings (
        o: " -e \"s|" + outputPlaceholder o + "|$" + o + "|g\""
      ) sedOrder;
      outputDirsList = lib.concatMapStrings (o: "\"$" + o + "\" ") realOutputs;
    in
    ''
      while IFS= read -r -d "" gg_f; do
        gg_rp="$(patchelf --print-rpath "$gg_f" 2>/dev/null)" || continue
        [ -z "$gg_rp" ] && continue
        gg_newrp="$(printf '%s' "$gg_rp" | sed${sedExprs})"
        if [ "$gg_newrp" != "$gg_rp" ]; then
          chmod u+w "$gg_f"
          patchelf --set-rpath "$gg_newrp" "$gg_f"
        fi
      done < <(find ${outputDirsList} -type f -print0 2>/dev/null)
    '';
}
