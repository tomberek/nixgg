# Shell-script-generating helpers for nix/splitStdenv.nix's build stage
# and final-stage restore logic.
{
  lib,
  patchedNix,
  nixgg,
  bash,
  coreutils,
  gcc,
  gnumake,
  system,
}:

{
  # NIXGG_SANDBOX_TARGET is set to an unmatchable path rather than left
  # unset: link.go's maybeSubmit defaults to submitting whatever it
  # links as the outer drv's "out" when TARGET is empty — wrong here,
  # where `nixgg assemble` (postBuild) owns the one "out" submission
  # for the whole tree.
  #
  # knownStorePathsJSON is a parameter, not computed once here, because
  # the real store paths vary per wrapped package (probeArgs.buildInputs).
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
    export NIXGG_KNOWN_STORE_PATHS=${lib.escapeShellArg knownStorePathsJSON}
    # Worker-protocol client for the sandbox daemon socket (internal/rpc)
    # instead of per-call fork+exec. NIXGG_RPC=0 is the CLI-fallback escape hatch.
    export NIXGG_RPC=1
    export NIX_CONFIG="extra-experimental-features = nix-command ca-derivations dynamic-derivations"
  '';

  # Phase 2 needs to cd back to $NIX_BUILD_TOP (cmake's `mkdir build &&
  # cd build` means $PWD here isn't it), then builds one assembly drv
  # that walks $NIX_BUILD_TOP, restores the tree, resolves every drvref
  # stub the shims left, and submits it as this derivation's "out".
  # See go/internal/cli/assemble.go / go/internal/assemble/.
  submitBuildTreeScript = drvName: ''
    realpath --relative-to="$NIX_BUILD_TOP" "$PWD" > "$NIX_BUILD_TOP/.gg-cwd"
    export -p > "$NIX_BUILD_TOP/.gg-env"
    ${nixgg}/bin/nixgg assemble "$NIX_BUILD_TOP" "${drvName}"
  '';

  # Phase 1's derivation must declare exactly one output ("out") —
  # submit-output's ".drv" naming convention requires it — but build
  # systems bake bin/dev/man/etc. install paths in at configure time.
  # Give each real non-"out" output its own placeholder subdir of the
  # one tree, so a later restore/split step can rebuild the real outputs.
  outputPlaceholder = o: if o == "out" then "/nonexistent" else "/nonexistent-${o}";

  # restoreOutputsScript and elfRpathFixupScript take outputPlaceholder/
  # realOutputs/extraOutputs as explicit params rather than closing over
  # a shared binding, since each caller computes them from its own probeArgs.

  # For every real output: make the dir and copy its placeholder subtree
  # (installed by phase 2's own installPhase, DESTDIR-relative) into it.
  # `v`/`ph` are built via string concatenation, not `${}` antiquotation,
  # so the emitted script contains a literal shell var reference (e.g.
  # "$bin"), not a Nix-side interpolation of it.
  #
  # `[ -d ... ] &&` guards each cp: multiple-outputs.sh's automatic
  # per-output DESTDIR routing only fires when the package's own build
  # system threads $bin/$dev/... through to its install step. Some
  # packages don't — openssl installs everything flat under out's
  # placeholder and does its own `mv $out/bin $bin/bin` in postInstall
  # instead — so failing on a missing dir there would break every such
  # package; skipping it just leaves that output empty for this script
  # and lets the package's own postInstall populate it afterward.
  #
  # `mkdir -p "${v}"` lives inside that same guard: a package whose own
  # postInstall creates the output dir itself may use a bare `mkdir`
  # (no `-p`), expecting a not-yet-existing dir — openssl's `mkdir $dev`
  # right before `mv $out/include $dev/` fails with "File exists" if we
  # pre-create it unconditionally.
  #
  # Two possible source locations, not one: a package's makeFlags can
  # point an output at its own absolute final path instead of the
  # placeholder scheme (openssl's `MANDIR=$(man)/share/man`) — once
  # DESTDIR is threaded onto make's command line, that absolute path
  # also ends up DESTDIR-prefixed, never under the placeholder tree.
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
  # binary/lib at LINK time, using whichever placeholder was live then.
  # Without this rewrite, fixupPhase's patchelf-based shrinkRPath step
  # (correctly) drops the dangling placeholder rpath since nothing
  # exists at that literal path, leaving shared-lib consumers unable to
  # find it at runtime (`nix run` -> "cannot open shared object file").
  # Longest-placeholder-first order (extraOutputs before "out") matters:
  # "/nonexistent" is a literal prefix of "/nonexistent-bin", so
  # substituting it first would mangle the longer placeholders too.
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
