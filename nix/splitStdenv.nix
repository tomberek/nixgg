# Generator for the configure/build phase-split combinations, driven by two
# independent yes/no bits (was dynDrvStdenv/configureCacheStdenv/
# dynDrvConfigureCacheStdenv).
#
# splitAtConfigure cuts between configurePhase and buildPhase.
# splitAtBuild cuts between buildPhase and installPhase.
#
#   splitAtConfigure | splitAtBuild | stages
#   -----------------+--------------+--------
#   false             false          1 (plain stdenv, untouched)
#   false             true           2
#   true              false          2
#   true              true           3
#
# Usage:
#   hello = pkgs.hello.override {
#     stdenv = mkSplitStdenv { stdenv = pkgs.stdenv; splitAtBuild = true; };
#   };
{
  lib,
  patchedNix,
  nixgg,
  bash,
  coreutils,
  gcc,
  gnumake,
  system,
  nixpkgsPath,
  config,
  stdenvNoCC,
}:

{
  stdenv,
  splitAtConfigure ? false,
  splitAtBuild ? false,
  # Composed before any role-specific hatch, so those can still override on
  # top. Safe for hook-shaped attrs (postPatch, preBuild...) but not
  # structural ones (name, outputs, phases) — those encode each stage's own
  # invariants.
  extraAttrs ? (finalAttrs: old: old),
  extraConfigureAttrs ? (finalAttrs: old: old),
  extraBuildAttrs ? (finalAttrs: old: old),
  extraInstallAttrs ? (finalAttrs: old: old),
  configureSrcFilter ? null,
  # Subtrees the wrapped package's build reads object BYTES inline, or
  # expects to fail and reads the diagnostic — neither can be modelled
  # as a derivation. Project-specific (a kernel names six; most
  # packages need none) — see internal/mode's NIXGG_PASSTHROUGH_PATHS.
  passthroughPaths ? [ ],
}:

let
  stdenv0 = stdenv;

  defaultMkDerivationFromStdenv =
    stdenv:
    (import "${nixpkgsPath}/pkgs/stdenv/generic/make-derivation.nix" lib config stdenv).mkDerivation;

  mkConfigureSrcFilter = import ./configureSrcFilter.nix { inherit lib stdenvNoCC; };

  shared = import ./dynDrvShared.nix {
    inherit
      lib
      patchedNix
      nixgg
      bash
      coreutils
      gcc
      gnumake
      system
      passthroughPaths
      ;
  };
  inherit (shared) ggShimsOnPath submitBuildTreeScript outputPlaceholder;

  # Replay the build stage's exports, gap-filling only: a variable the
  # final stage already set always wins, so its own outputs and
  # stdenv-managed state cannot be clobbered. Splitting one
  # mkDerivation across two derivations splits the shell too, and
  # packages routinely export in one stage and read in a later one
  # (nixpkgs' kernel: configurePhase exports buildRoot, linux-config's
  # installPhase reads it).
  ggRestoreEnv = ''
    if [ -f "$NIX_BUILD_TOP/.gg-env" ]; then
      while IFS= read -r ggLine; do
        case "$ggLine" in
          "declare -x "*) ;;
          *) continue ;;
        esac
        ggKV=''${ggLine#declare -x }
        ggName=''${ggKV%%=*}
        case "$ggName" in
          PATH|PWD|OLDPWD|HOME|SHLVL|_) continue ;;
          TMP|TMPDIR|TEMP|TEMPDIR) continue ;;
          NIX_BUILD_TOP|NIX_STORE|NIX_BUILD_CORES|NIX_LOG_FD) continue ;;
          out|outputs) continue ;;
          NIXGG_*) continue ;;
          # Phase-control attrs are exported like any other variable
          # (dontInstall=1, etc.) — gap-filling one would make the
          # final stage silently skip its own phase, exiting 0 having
          # run nothing.
          dont*|do[A-Z]*|phases|*Phase|*Phases) continue ;;
        esac
        if [ -z "''${!ggName+x}" ]; then
          eval "export $ggKV"
        fi
      done < "$NIX_BUILD_TOP/.gg-env"
    fi
  '';

  applyExtra = hatch: finalAttrs: base: hatch finalAttrs (extraAttrs finalAttrs base);
in

stdenv0.override (
  old:
  {
    mkDerivationFromStdenv =
      stdenvSelf:
      let
        mkDerivationSuper = (old.mkDerivationFromStdenv or defaultMkDerivationFromStdenv) stdenvSelf;
      in
      argsOrFn:
      let
        # argsOrFn may be a `finalAttrs: {...}` function — never collapse
        # it, that destroys makeDerivationExtensible's fixed point.
        # probeArgs is a throwaway {} application for static pname/version/
        # outputs reads that don't depend on finalAttrs.
        probeArgs = lib.toFunction argsOrFn { };
        drvName = if probeArgs ? name then probeArgs.name else "${probeArgs.pname}-${probeArgs.version}";
        outerName = "gg-build-${drvName}";

        # Honour what the package asked for: phases written against
        # structuredAttrs use bash array syntax, which does not exist
        # when it is off. Default matches make-derivation.nix.
        structuredAttrs = probeArgs.__structuredAttrs or (config.structuredAttrsByDefault or false);

        # builder-rpc-v0 wants $out unset; /nonexistent keeps stdenv's
        # _assignFirst happy while making a real write fail loudly.
        # Under structuredAttrs only `env` reaches the derivation as
        # environment variables, so the placeholder goes there instead.
        nonexistentOut =
          if structuredAttrs then
            { env = (probeArgs.env or { }) // { out = "/nonexistent"; }; }
          else
            { out = "/nonexistent"; };

        knownStorePathsJSON = builtins.toJSON (
          map toString (
            builtins.concatMap (p: p.all or [ p ]) (
              (probeArgs.buildInputs or [ ]) ++ (probeArgs.propagatedBuildInputs or [ ])
              ++ [ bash coreutils gcc nixgg patchedNix ]
            )
          )
        );

        # multiple-outputs.sh's `_overrideFirst` chain collapses every
        # output name to "$out" at configure time unless a same-named bash
        # var already exists — give each non-"out" output its own subdir of
        # the one tree via outputPlaceholder so the final stage can split
        # the restored tree back into its real outputs.
        realOutputs = probeArgs.outputs or [ "out" ];
        extraOutputs = builtins.filter (o: o != "out") realOutputs;

        existenceStubs = if configureSrcFilter == null then [ ] else configureSrcFilter.existenceStubs or [ ];

        # ---- configure stage --------------------------------------------
        #
        # bypassShims: true only when a build stage follows — shims must
        # already be on PATH so an absolute compiler path baked into a
        # generated Makefile is the shim's, not the real compiler's.
        # restoreTargetFor: for each real output, the sed replacement
        # target the final stage's restore step should aim at — "$out"
        # when the final stage restores this snapshot directly, or a
        # placeholder string when a build stage sits in between.
        mkConfigureStage =
          {
            bypassShims,
            restoreTargetFor,
          }:
          let
            snapshotScript = ''
              mkdir -p ${lib.concatMapStrings (o: "\"$" + o + "\" ") realOutputs} "$ggtree"
              cp -a "$NIX_BUILD_TOP/$sourceRoot/." "$ggtree/tree"
              ${lib.concatMapStrings (
                p: "rm -f \"$ggtree\"/tree/${lib.escapeShellArg p}\n"
              ) existenceStubs}
              realpath --relative-to="$NIX_BUILD_TOP/$sourceRoot" "$PWD" > "$ggtree/.gg-cwd"
              printf '%s' "$NIX_BUILD_TOP/$sourceRoot" > "$ggtree/.gg-buildroot"
            '';

            withAttrs =
              finalAttrs:
              let
                # Real finalAttrs fixed point, not probeArgs — some
                # packages' src is self-referential through finalAttrs.
                orig = lib.toFunction argsOrFn finalAttrs;

                configureSrc =
                  if configureSrcFilter == null then
                    orig.src
                  else
                    mkConfigureSrcFilter {
                      name = "${orig.pname or orig.name}-configure-src";
                      src = orig.src;
                      includePatterns = configureSrcFilter.includePatterns;
                      existenceStubs = configureSrcFilter.existenceStubs or [ ];
                    };

                base =
                  orig
                  // {
                    name =
                      if orig ? name then "${orig.name}-configure" else "${orig.pname}-configure-${orig.version}";
                    __structuredAttrs = false;
                    dontBuild = true;
                    dontInstall = true;
                    dontFixup = true;
                    doCheck = false;
                    doInstallCheck = false;
                    doDist = false;
                    src = configureSrc;
                    outputs = realOutputs ++ [ "ggtree" ];
                    __contentAddressed = true;
                    outputHashMode = "nar";
                    outputHashAlgo = "sha256";
                    postPatch =
                      (orig.postPatch or "")
                      + lib.optionalString bypassShims ''
                        export NIXGG_BYPASS=1
                        ${ggShimsOnPath knownStorePathsJSON}
                      '';
                    postConfigure = (orig.postConfigure or "") + snapshotScript;
                  };
              in
              applyExtra extraConfigureAttrs finalAttrs base;

            stage = mkDerivationSuper withAttrs;

            # stage.${o} is always a unique real store path, so iteration
            # order never risks a prefix collision.
            pathRewriteScript =
              let
                sedExprs = lib.concatMapStrings (
                  o: " -e \"s|" + toString stage.${o} + "|" + restoreTargetFor o + "|g\""
                ) (extraOutputs ++ [ "out" ]);
              in
              ''
                while IFS= read -r -d "" gg_f; do
                  gg_ref="$(mktemp)"
                  touch -r "$gg_f" "$gg_ref"
                  sed -i${sedExprs} "$gg_f"
                  touch -r "$gg_ref" "$gg_f"
                  rm -f "$gg_ref"
                done < <(grep -rlZI -F "/nix/store/" "$NIX_BUILD_TOP" 2>/dev/null)
              '';

            # gg_oldroot/newroot rewrite handles tools that bake an
            # absolute build directory as literal text (e.g. automake's
            # generated build-aux/missing) rather than a relative path.
            restorePhase = ''
              runHook preGgRestore
              cp -a ${stage.ggtree}/tree/. .
              chmod -R u+w .
              gg_oldroot="$(cat ${stage.ggtree}/.gg-buildroot)"
              gg_newroot="$PWD"
              if [ "$gg_oldroot" != "$gg_newroot" ]; then
                grep -rlZI -F "$gg_oldroot" . 2>/dev/null | while IFS= read -r -d "" gg_f; do
                  gg_ref="$(mktemp)"
                  touch -r "$gg_f" "$gg_ref"
                  sed -i "s|$gg_oldroot|$gg_newroot|g" "$gg_f"
                  touch -r "$gg_ref" "$gg_f"
                  rm -f "$gg_ref"
                done
              fi
              cd "$(cat ${stage.ggtree}/.gg-cwd)"
              ${pathRewriteScript}
              runHook postGgRestore
            '';
          in
          { inherit stage restorePhase; };

        # ---- build stage -------------------------------------------------
        #
        # configureStage: null for a solo build (configure runs naturally,
        # inline, with shims bypassed through it — phases stays unset so
        # setup-hook-injected phases like autoreconfHook's aren't dropped);
        # otherwise the mkConfigureStage result to restore from instead of
        # configuring fresh (phases hardcoded to splice ggRestorePhase in
        # place of configurePhase).
        mkBuildStage =
          { configureStage }:
          let
            withAttrs =
              finalAttrs:
              let
                orig = lib.toFunction argsOrFn finalAttrs;
                base =
                  orig
                  // {
                    name = "${outerName}.drv"; # required by submit-output's naming convention
                    doCheck = false;
                    dontInstall = true;
                    dontFixup = true;
                    doInstallCheck = false;
                    doDist = false;
                    # A "*.drv"-named derivation must be single-output; the
                    # final stage keeps the real `outputs`.
                    outputs = [ "out" ];
                    # make-derivation.nix appends "debug" to outputs at its
                    # own layer when separateDebugInfo = true (e.g. openssl),
                    # so outputs = ["out"] alone doesn't prevent it.
                    separateDebugInfo = false;
                  }
                  // lib.optionalAttrs (configureStage != null) {
                    # dontConfigure alone would skip configurePhase's hooks
                    # too, so the restore needs its own always-run phase.
                    # ggSubmitPhase is named explicitly here — setup.sh
                    # only splices postPhases when `phases` is UNSET, and
                    # this branch sets it; the configureStage == null
                    # path below leaves it unset and gets the submit via
                    # postPhases instead.
                    phases = "unpackPhase patchPhase ggRestorePhase buildPhase ggSubmitPhase";
                    ggRestorePhase = configureStage.restorePhase;
                  }
                  // builtins.listToAttrs (
                    map (o: {
                      name = o;
                      value = outputPlaceholder o;
                    }) extraOutputs
                  )
                  // {
                    # Honoured, not forced: the package's phases may be
                    # written against structuredAttrs. The `out`
                    # placeholder moves into `env` to match — see
                    # nonexistentOut.
                    __structuredAttrs = structuredAttrs;
                    requiredSystemFeatures = (orig.requiredSystemFeatures or [ ]) ++ [ "builder-rpc-v0" ];
                    __contentAddressed = true;
                    outputHashMode = "text";
                    outputHashAlgo = "sha256";
                    nativeBuildInputs = (orig.nativeBuildInputs or [ ]) ++ [ patchedNix ];
                    # Must stay set through configure (autoreconfHook, cmake
                    # probes, or the restored configure snapshot) — only
                    # buildPhase needs real acceleration.
                    postPatch = (orig.postPatch or "") + ''
                      export NIXGG_BYPASS=1
                      ${ggShimsOnPath knownStorePathsJSON}
                    '';
                    preBuild = ''
                      unset NIXGG_BYPASS
                    '' + (orig.preBuild or "");
                    # A real phase, not a postBuild hook: postBuild only
                    # runs if the package's buildPhase calls runHook, and
                    # many hand-written ones do not (nixpkgs' own
                    # linux-config among them) — those built fine,
                    # submitted nothing, and failed with Nix's opaque
                    # "failed to submit output path for 'out'". postPhases
                    # is spliced on unconditionally by setup.sh.
                    postPhases = (lib.toList (orig.postPhases or [ ])) ++ [ "ggSubmitPhase" ];
                    ggSubmitPhase = submitBuildTreeScript outerName;
                  }
                  // nonexistentOut;
              in
              applyExtra extraBuildAttrs finalAttrs base;

            stage = mkDerivationSuper withAttrs;
            builtTree = builtins.outputOf stage.outPath "out";
          in
          { inherit stage builtTree; };

        # ---- final stage, restoring a built tree -------------------------
        finalFromBuiltTree =
          { builtTree }:
          let
            restoreOutputsScript = shared.restoreOutputsScript outputPlaceholder realOutputs;
            elfRpathFixupScript = shared.elfRpathFixupScript outputPlaceholder realOutputs extraOutputs;
          in
          mkDerivationSuper (
            finalAttrs:
            let
              orig = lib.toFunction argsOrFn finalAttrs;
              base =
                orig
                // {
                  phases = "ggRestorePhase checkPhase installPhase fixupPhase installCheckPhase distPhase";
                  dontUnpack = true;
                  # Honoured, not forced — same reasoning as the build
                  # stage above. This stage owns the package's REAL
                  # outputs, so it needs no placeholder.
                  __structuredAttrs = structuredAttrs;
                  ggRestorePhase = ''
                    runHook preGgRestore
                    cp -a ${builtTree}/. "$NIX_BUILD_TOP/"
                    chmod -R u+w "$NIX_BUILD_TOP"
                    cd "$NIX_BUILD_TOP/$(cat "$NIX_BUILD_TOP/.gg-cwd")"
                    ${ggRestoreEnv}

                    # Shims stay reachable but inert here: build systems
                    # bake absolute tool paths at configure time (cmake
                    # does; a caller can via makeFlags), so a baked-in
                    # path still gets invoked in this stage whether or
                    # not the build was planned to hit it. Without this
                    # env the shim can't build its own config and exits
                    # non-zero; BYPASS makes it exec the real tool since
                    # everything it would model is already built.
                    export NIXGG_BYPASS=1
                    ${ggShimsOnPath knownStorePathsJSON}
                    export DESTDIR="$NIX_BUILD_TOP/.gg-destdir"
                    # installFlagsArray, not installFlags: under
                    # __structuredAttrs, installFlags is a bash ARRAY,
                    # and assigning a scalar to an array name writes
                    # element 0 — silently welding this onto the
                    # package's first real flag. Appended at RUNTIME
                    # (not baked into a Nix string) because some
                    # packages' own Makefile (openssl's
                    # unix-Makefile.tmpl) assigns DESTDIR= itself, and
                    # GNU Make lets its own command-line value override
                    # an inherited env var of the same name — only a
                    # value on make's own argv wins over that.
                    # setup.sh concatenates installFlagsArray in both
                    # modes, so appending here is mode-independent.
                    installFlagsArray+=( "DESTDIR=$DESTDIR" )
                    runHook postGgRestore
                  '';
                  installFlags = (orig.installFlags or "");
                  postInstall = restoreOutputsScript + (orig.postInstall or "");
                  # Must run before fixupPhase's patchelf-based rpath
                  # shrinking, which drops any rpath entry pointing at a
                  # path that doesn't exist — exactly what the placeholder
                  # rpaths still are if this runs any later.
                  preFixup = elfRpathFixupScript + (orig.preFixup or "");
                };
            in
            applyExtra extraInstallAttrs finalAttrs base
          );

        # ---- final stage, restoring a configure snapshot -----------------
        # Only when splitAtConfigure but not splitAtBuild: no build stage
        # exists, so this unpacks+patches fresh against the real unfiltered
        # src, overlays the configure snapshot, and continues build..dist
        # with no acceleration.
        finalFromConfigure =
          { configureStage }:
          mkDerivationSuper (
            finalAttrs:
            let
              orig = lib.toFunction argsOrFn finalAttrs;
              base =
                orig
                // {
                  phases = "unpackPhase patchPhase ggRestorePhase buildPhase checkPhase installPhase fixupPhase installCheckPhase distPhase";
                  __structuredAttrs = false;
                  dontConfigure = true;
                  ggRestorePhase = configureStage.restorePhase;
                };
            in
            applyExtra extraInstallAttrs finalAttrs base
          );
      in
      if splitAtConfigure && splitAtBuild then
        let
          configureStage = mkConfigureStage {
            bypassShims = true;
            restoreTargetFor = outputPlaceholder;
          };
          buildStage = mkBuildStage { inherit configureStage; };
        in
        finalFromBuiltTree { inherit (buildStage) builtTree; }
      else if splitAtConfigure then
        let
          configureStage = mkConfigureStage {
            bypassShims = false;
            restoreTargetFor = o: "$" + o;
          };
        in
        finalFromConfigure { inherit configureStage; }
      else if splitAtBuild then
        let
          buildStage = mkBuildStage { configureStage = null; };
        in
        finalFromBuiltTree { inherit (buildStage) builtTree; }
      else
        mkDerivationSuper (finalAttrs: applyExtra extraInstallAttrs finalAttrs (lib.toFunction argsOrFn finalAttrs));
  }
)
