# One generator for the configure/build phase-split combinations, driven
# by two independent yes/no bits instead of separate hardcoded files (was
# dynDrvStdenv/configureCacheStdenv/dynDrvConfigureCacheStdenv).
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
# Configure stage (splitAtConfigure): runs unpack..configure, snapshots
# the tree into a "ggtree" output (optionally shrunk via
# configureSrcFilter). Named "${name}-configure" — tests/configure-cache-
# cutoff.sh and tests/dyndrv-configure-cache-cutoff.sh grep for that exact
# substring and for the "ggtree" output name.
#
# Build stage (splitAtBuild): runs configure..build (or just build, if a
# configure stage already ran) under builder-rpc-v0 with nixgg's shims
# live, forced single-output ("${outerName}.drv", submitted via `nixgg
# assemble` in postBuild).
#
# Final stage is always present: if a build stage ran, it restores that
# build's realized tree (DESTDIR-based multi-output split + rpath fixup);
# if only a configure stage ran, it does its own fresh unpack+patch
# against the real unfiltered src, overlays the configure snapshot, and
# continues build..dist with no acceleration.
#
# Usage:
#   hello = pkgs.hello.override {
#     stdenv = mkSplitStdenv { stdenv = pkgs.stdenv; splitAtBuild = true; };
#   };
{
  lib,
  patchedNix,
  nixgg, # $out/bin/nixgg + $out/shims/{cc,c++,ar,...}
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
  # Composed BEFORE any role-specific hatch (extraConfigureAttrs etc.), so
  # those can still override on top. Safe for hook-shaped attrs (postPatch,
  # preBuild...) but not for structural attrs (name, outputs, phases) —
  # those encode each stage's own invariants.
  extraAttrs ? (finalAttrs: old: old),
  # Only meaningful when splitAtConfigure; no-op otherwise.
  extraConfigureAttrs ? (finalAttrs: old: old),
  # Only meaningful when splitAtBuild; no-op otherwise.
  extraBuildAttrs ? (finalAttrs: old: old),
  # The final (always-present) stage.
  extraInstallAttrs ? (finalAttrs: old: old),
  # Only meaningful when splitAtConfigure. Same shape as the old
  # configureCacheStdenv param — see nix/configureSrcFilter.nix.
  configureSrcFilter ? null,
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
      ;
  };
  inherit (shared) ggShimsOnPath submitBuildTreeScript outputPlaceholder;

  # extraAttrs runs first as a shared baseline; the role-specific hatch
  # composes on top and wins.
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
        # argsOrFn may be a plain attrset or a `finalAttrs: {...}` function
        # — never collapse it, that destroys makeDerivationExtensible's
        # fixed point. probeArgs is a throwaway {} application for static
        # pname/version/outputs reads that don't depend on finalAttrs.
        probeArgs = lib.toFunction argsOrFn { };
        drvName = if probeArgs ? name then probeArgs.name else "${probeArgs.pname}-${probeArgs.version}";
        outerName = "gg-build-${drvName}";

        # Store paths the shim's storedeps matcher needs to recognize in
        # -I/-L flags. Only forced when a stage with shims on PATH
        # actually references it.
        knownStorePathsJSON = builtins.toJSON (
          map toString (
            builtins.concatMap (p: p.all or [ p ]) (
              (probeArgs.buildInputs or [ ]) ++ (probeArgs.propagatedBuildInputs or [ ])
              ++ [ bash coreutils gcc nixgg patchedNix ]
            )
          )
        );

        # A build stage's own derivation must declare exactly one output
        # ("out") for submit-output's ".drv" naming, but build systems bake
        # bin/dev/man/etc. install paths into the Makefile/CMakeCache at
        # configure time via multiple-outputs.sh's `_overrideFirst` chain,
        # which collapses every output name to "$out" unless a same-named
        # bash var already exists. Give each real non-"out" output its own
        # subdir of the one tree via outputPlaceholder, so the final stage
        # can split the restored tree back into its real outputs.
        realOutputs = probeArgs.outputs or [ "out" ];
        extraOutputs = builtins.filter (o: o != "out") realOutputs;

        existenceStubs = if configureSrcFilter == null then [ ] else configureSrcFilter.existenceStubs or [ ];

        # ---- configure stage --------------------------------------------
        #
        # bypassShims: true only when a build stage follows (shims must
        # already be on PATH so an absolute compiler path baked into a
        # generated Makefile is the shim's, not the real compiler's —
        # without this, cmake bakes the real gcc-wrapper path and nothing
        # routes through the shim). restoreTargetFor: for each real output,
        # the sed replacement target the final stage's restore step should
        # aim at — a bash variable reference ("$out") when the final stage
        # restores this snapshot directly, or a placeholder string when a
        # build stage's placeholder scheme sits in between.
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
                    # Distinct from the final stage's name (real package
                    # name) — tests grep for this exact "-configure" substring.
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
                    name = "${outerName}.drv"; # required for submit-output's naming convention
                    doCheck = false;
                    dontInstall = true;
                    dontFixup = true;
                    doInstallCheck = false;
                    doDist = false;
                    # Forced single-output — a "*.drv"-named derivation must
                    # be single-output. The final stage keeps the real
                    # `outputs`.
                    outputs = [ "out" ];
                    out = "/nonexistent";
                    # make-derivation.nix appends "debug" to outputs at its
                    # own layer (downstream of this override) when a
                    # package sets separateDebugInfo = true (e.g. openssl),
                    # so outputs = ["out"] alone doesn't prevent a second
                    # output reappearing.
                    separateDebugInfo = false;
                  }
                  // lib.optionalAttrs (configureStage != null) {
                    # dontConfigure alone would skip configurePhase's hooks
                    # too, so the restore needs its own always-run phase.
                    phases = "unpackPhase patchPhase ggRestorePhase buildPhase";
                    ggRestorePhase = configureStage.restorePhase;
                  }
                  # Extra outputs (bin/dev/man/...) are plain env vars here,
                  # not declared derivation outputs — this stage stays
                  # single-output. They only get baked as literal absolute
                  # paths into generated build files, for the final stage's
                  # DESTDIR-relative install to act on later.
                  // builtins.listToAttrs (
                    map (o: {
                      name = o;
                      value = outputPlaceholder o;
                    }) extraOutputs
                  )
                  // {
                    __structuredAttrs = false;
                    requiredSystemFeatures = (orig.requiredSystemFeatures or [ ]) ++ [ "builder-rpc-v0" ];
                    __contentAddressed = true;
                    outputHashMode = "text";
                    outputHashAlgo = "sha256";
                    nativeBuildInputs = (orig.nativeBuildInputs or [ ]) ++ [ patchedNix ];
                    # NIXGG_BYPASS gates acceleration per shim invocation,
                    # separate from whether shims are on PATH. It must stay
                    # set through configure (autoreconfHook, cmake probes,
                    # or the restored configure snapshot) — only buildPhase
                    # needs real acceleration.
                    postPatch = (orig.postPatch or "") + ''
                      export NIXGG_BYPASS=1
                      ${ggShimsOnPath knownStorePathsJSON}
                    '';
                    preBuild = ''
                      unset NIXGG_BYPASS
                    '' + (orig.preBuild or "");
                    postBuild = (orig.postBuild or "") + submitBuildTreeScript outerName;
                  };
              in
              applyExtra extraBuildAttrs finalAttrs base;

            stage = mkDerivationSuper withAttrs;
            builtTree = builtins.outputOf stage.outPath "out";
          in
          { inherit stage builtTree; };

        # ---- final stage, restoring a built tree -------------------------
        # Used whenever splitAtBuild is true.
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
                  __structuredAttrs = false;
                  ggRestorePhase = ''
                    runHook preGgRestore
                    cp -a ${builtTree}/. "$NIX_BUILD_TOP/"
                    chmod -R u+w "$NIX_BUILD_TOP"
                    cd "$NIX_BUILD_TOP/$(cat "$NIX_BUILD_TOP/.gg-cwd")"
                    export DESTDIR="$NIX_BUILD_TOP/.gg-destdir"
                    # `export DESTDIR` alone isn't enough: some packages'
                    # own Makefile (openssl's unix-Makefile.tmpl) assigns
                    # DESTDIR= itself, which GNU Make lets override an
                    # inherited env var of the same name — only a value on
                    # make's own command line wins. Appended here at
                    # runtime (not baked into installFlags as a Nix
                    # string) so bash, not make, expands $NIX_BUILD_TOP —
                    # a literal `$` in installFlags would otherwise be
                    # reinterpreted as make's own variable syntax.
                    installFlags="''${installFlags-} DESTDIR=$DESTDIR"
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
        # Used only when splitAtConfigure is true and splitAtBuild is
        # false: no build stage exists, so this does its own fresh
        # unpack+patch against the real unfiltered src, overlays the
        # configure snapshot on top, and continues build..dist with no
        # acceleration.
        finalFromConfigure =
          { configureStage }:
          mkDerivationSuper (
            finalAttrs:
            let
              orig = lib.toFunction argsOrFn finalAttrs;
              base =
                orig
                // {
                  # Fresh unpack+patch against the real unfiltered src
                  # (not the configureSrcFilter-shrunk one) so sourceRoot
                  # matches the real source and the overlay below lands at
                  # the right depth regardless of whether configureSrcFilter
                  # was set.
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
        # No cut at all. extraInstallAttrs still applies, since it's the
        # always-present final stage — here that's the whole build.
        mkDerivationSuper (finalAttrs: applyExtra extraInstallAttrs finalAttrs (lib.toFunction argsOrFn finalAttrs));
  }
)
