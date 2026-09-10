# splitStdenv — one generator replacing the three formerly-separate
# dynDrvStdenv.nix / configureCacheStdenv.nix / dynDrvConfigureCacheStdenv.nix
# files. Those three hardcoded a fixed phase-split each; this file takes
# the split as data (two booleans) and builds whichever combination of
# stages that implies, sharing the actual stage-building logic instead of
# duplicating it per file — the duplication is exactly what caused two
# real bugs that had to be independently re-fixed in multiple copies (see
# git history: 1d605d5, 9bec045).
#
# splitAtConfigure cuts stdenv.mkDerivation between configurePhase and
# buildPhase. splitAtBuild cuts it between buildPhase and installPhase.
# Both are independent yes/no bits — there is no third cut point anywhere
# in this file, and no evidence anywhere in this repo that one is needed.
#
#   splitAtConfigure | splitAtBuild | stages | old equivalent
#   -----------------+--------------+--------+---------------------------
#   false             false          1        plain stdenv, untouched
#   false             true           2        dynDrvStdenv
#   true              false          2        configureCacheStdenv
#   true              true           3        dynDrvConfigureCacheStdenv
#
# A "configure" stage (when splitAtConfigure) runs unpack..configure,
# snapshots its tree into a "ggtree" output (optionally shrinking its own
# `src` via configureSrcFilter for early-cutoff), and is named
# "${name}-configure" — this exact substring and the "ggtree" output name
# are asserted on directly by tests/configure-cache-cutoff.sh and
# tests/dyndrv-configure-cache-cutoff.sh, so both must stay exactly as
# they are regardless of which combination is in play.
#
# A "build" stage (when splitAtBuild) runs configure..build (or, if a
# configure stage already ran, just build) under builder-rpc-v0 with
# nixgg's shims live, forced single-output ("${outerName}.drv", submitted
# whole via `nixgg assemble` in postBuild) — the same real per-TU
# acceleration mechanism dynDrvStdenv always used.
#
# The final stage is always present and has one of two shapes depending
# on what precedes it, picked automatically rather than via a separate
# flag: if a build stage ran, the final stage restores that build's
# single realized tree (DESTDIR-based multi-output split + rpath fixup —
# see restoreOutputsScript/elfRpathFixupScript below for why); if only a
# configure stage ran (no build stage), the final stage instead does its
# own fresh unpack+patch against the real (unfiltered) src, overlays the
# configure stage's multi-output ggtree snapshot on top, and continues
# straight through build..dist with no acceleration at all.
#
# Whether the configure stage's shims-on-PATH setup is added (present but
# inert — a genuine passthrough exec, see go/internal/shim/passthrough.go's
# bypassed()) is purely a function of splitAtBuild: needed only when a
# build stage follows and needs the shim's own store path already baked
# into whatever absolute compiler path the configure step generates.
# Likewise, whether the configure stage's own restore-rewrite target is a
# real output path or a placeholder string is purely a function of
# splitAtBuild: real when the final stage restores it directly (no build
# stage in between), placeholder when a build stage's own placeholder
# scheme is what's being fed instead.
#
# Usage:
#   hello = pkgs.hello.override {
#     stdenv = mkSplitStdenv { stdenv = pkgs.stdenv; splitAtBuild = true; };
#   };
{
  lib,
  patchedNix,
  nixgg, # nixggBin: $out/bin/nixgg + $out/shims/{cc,c++,ar,...}
  bash,
  coreutils,
  gcc,
  gnumake,
  system, # target platform for the JSON drv `nixgg assemble` builds.
  nixpkgsPath,
  config,
  stdenvNoCC, # for the configureSrcFilter derivation (copies files only).
}:

{
  stdenv,
  splitAtConfigure ? false,
  splitAtBuild ? false,
  # Applies to every stage that exists for this combination, composed
  # BEFORE any role-specific hatch below (so a role-specific hatch can
  # still refine/override on top for just that one stage). Safe for
  # hook-shaped attrs (postPatch, preBuild, ...) — a stage that never
  # runs the phase a hook is attached to just never sources it. NOT safe
  # for structural attrs (name, outputs, phases) — those encode each
  # stage's own naming/output-shape invariants, and a caller reaching for
  # this hatch specifically because they want "the same patch everywhere"
  # is exactly the person likely to clobber one of those by accident.
  extraAttrs ? (finalAttrs: old: old),
  # Only meaningful when splitAtConfigure; no-op (never called) otherwise.
  extraConfigureAttrs ? (finalAttrs: old: old),
  # Only meaningful when splitAtBuild; no-op (never called) otherwise.
  extraBuildAttrs ? (finalAttrs: old: old),
  # The final (always-present) stage — see this file's own top comment
  # for its two possible shapes.
  extraInstallAttrs ? (finalAttrs: old: old),
  # Only meaningful when splitAtConfigure. Opt-in, default null (no
  # filtering — always safe). Same shape as the old configureCacheStdenv
  # param — see nix/configureSrcFilter.nix.
  configureSrcFilter ? null,
  # Stage each TU as a symlink farm into per-file store objects rather
  # than copying. Without it a kernel stages ~150 GB of duplicated
  # headers; with it, a fraction of that. Threaded into ggShimsOnPath's
  # NIXGG_SHARED_STAGE — see internal/stage's SourcesShared.
  sharedStaging ? false,
  # Subtrees whose build reads object BYTES inline, which no derivation
  # can model. The shims pass work under them straight through — see
  # internal/mode. Threaded into ggShimsOnPath's
  # NIXGG_PASSTHROUGH_PATHS.
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
      passthroughPaths
      sharedStaging
      system
      ;
  };
  inherit (shared) ggShimsOnPath submitBuildTreeScript outputPlaceholder;

  # Replay the build stage's exports, gap-filling only: a variable the
  # final stage already set always wins, so its own outputs and
  # stdenv-managed state cannot be clobbered. Splitting one
  # mkDerivation across two derivations splits the shell too, and
  # packages routinely export in one stage and read in a later one.
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
          # Phase control. Derivation attributes are exported like any
          # other variable, so the build stage's own `dontInstall = true`
          # arrives here as dontInstall=1 — and the final stage, which
          # never sets it, would gap-fill it and then SKIP ITS OWN
          # installPhase. That failure is silent: the builder exits 0
          # having run nothing, and Nix reports only "failed to produce
          # output path". Same hazard for dontFixup/doCheck/doDist and
          # for the phase list itself.
          dont*|do[A-Z]*|phases|*Phase|*Phases) continue ;;
        esac
        # Gap-fill only: never override what the final stage decided.
        if [ -z "''${!ggName+x}" ]; then
          eval "export $ggKV"
        fi
      done < "$NIX_BUILD_TOP/.gg-env"
    fi
  '';

  # extraAttrs runs first as a shared baseline; the role-specific hatch
  # composes on top and wins — see extraAttrs's own docstring above.
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
        # argsOrFn may be a plain attrset or a `finalAttrs: {...}`
        # function. Never collapse it to a plain set — that destroys
        # makeDerivationExtensible's real fixed point. probeArgs is a
        # throwaway {} application, used only for static pname/version/
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
        # environment variables, so the placeholder goes there.
        nonexistentOut =
          if structuredAttrs then
            { env = (probeArgs.env or { }) // { out = "/nonexistent"; }; }
          else
            { out = "/nonexistent"; };

        # Store paths the shim's storedeps matcher needs to recognize in
        # -I/-L flags — same computation as mkNixggBuild.nix's
        # knownStorePathInputs. Only forced (and thus only costs
        # anything) when a stage that puts shims on PATH actually
        # references it.
        knownStorePathsJSON = builtins.toJSON (
          map toString (
            builtins.concatMap (p: p.all or [ p ]) (
              (probeArgs.buildInputs or [ ]) ++ (probeArgs.propagatedBuildInputs or [ ])
              ++ [ bash coreutils gcc nixgg patchedNix ]
            )
          )
        );

        # A build stage's own derivation must declare exactly one output
        # ("out") — submit-output's ".drv" naming convention requires
        # it — but build systems bake bin/dev/man/etc. install paths into
        # the Makefile/CMakeCache at configure time via
        # multiple-outputs.sh's `_overrideFirst` chain, which silently
        # collapses every output name to "$out" unless a same-named bash
        # var already exists first. Give each of the package's REAL
        # non-"out" outputs its own subdir of the one tree via
        # outputPlaceholder, so the final stage can split the restored
        # tree back apart into its real outputs.
        realOutputs = probeArgs.outputs or [ "out" ];
        extraOutputs = builtins.filter (o: o != "out") realOutputs;

        existenceStubs = if configureSrcFilter == null then [ ] else configureSrcFilter.existenceStubs or [ ];

        # ---- configure stage --------------------------------------------
        #
        # bypassShims: true only when a build stage follows (shims must
        # already be on PATH so an absolute compiler path baked into a
        # generated Makefile is the shim's, not the real compiler's —
        # confirmed directly: without this, cmake bakes the real
        # gcc-wrapper path and nothing ever routes through the shim, no
        # error at all). restoreTargetFor: for each real output, the sed
        # replacement target the final stage's restore step should aim
        # at — a bash variable reference ("$out") when the final stage
        # restores this snapshot directly with real output paths, or a
        # placeholder string when a build stage's placeholder scheme sits
        # in between.
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
                    # Distinct from the final stage's own name (which
                    # keeps the real package name) — tests grep for this
                    # exact "-configure" substring, and it also makes
                    # build/log output show which derivation is which.
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

            # Real output-path-to-restoreTarget rewrite. Source side
            # (stage.${o}) is always a unique real store path, so
            # iteration order never risks a prefix collision regardless
            # of what restoreTargetFor produces.
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

            # Restores this stage's snapshot into whatever the caller's
            # cwd is at the point they splice this in. gg_oldroot/newroot
            # rewrite handles tools that bake an absolute build directory
            # as literal text (e.g. automake's generated Makefile
            # invoking build-aux/missing) rather than a relative path.
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
        # configureStage: null when this is dynDrvStdenv's old
        # solo-build shape (configure runs naturally, inline, with shims
        # bypassed through it — phases stays unset so setup-hook-injected
        # phases like autoreconfHook's aren't dropped); otherwise the
        # mkConfigureStage result to restore from instead of configuring
        # fresh (phases hardcoded to splice ggRestorePhase in place of
        # configurePhase).
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
                    name = "${outerName}.drv"; # submit-output requires this
                    # to match outputPathName(outerName, "out")
                    doCheck = false;
                    dontInstall = true;
                    dontFixup = true;
                    doInstallCheck = false;
                    doDist = false;
                    # Forced single-output regardless of what the package
                    # declares — a multi-output build stage can't be
                    # named "*.drv" (Nix requires single-output for that
                    # suffix). The final stage keeps the package's real
                    # `outputs`.
                    outputs = [ "out" ];
                    # make-derivation.nix computes `outputs' = outputs ++
                    # optional separateDebugInfo' "debug"` at its OWN
                    # layer, downstream of this override, so `outputs =
                    # ["out"]` alone doesn't stop a package's own
                    # `separateDebugInfo = true` (e.g. openssl) from
                    # reintroducing a second output.
                    separateDebugInfo = false;
                  }
                  // lib.optionalAttrs (configureStage != null) {
                    # dontConfigure means runPhase skips configurePhase
                    # (and any hook attached to it) entirely, so
                    # restoring the configure stage's tree needs its own
                    # always-run phase name instead.
                    # ggSubmitPhase is named explicitly here because
                    # setup.sh only splices postPhases when `phases` is
                    # UNSET. The configureStage == null path below leaves
                    # it unset and gets the submit via postPhases; this
                    # branch would silently drop it, and a build that
                    # submits nothing fails with Nix's opaque "failed to
                    # submit output path for 'out'".
                    phases = "unpackPhase patchPhase ggRestorePhase buildPhase ggSubmitPhase";
                    ggRestorePhase = configureStage.restorePhase;
                  }
                  # Extra outputs (bin/dev/man/...) are plain env vars
                  # here, not declared Nix derivation outputs — this
                  # stage stays single-output. Nothing is ever written to
                  # these paths during this stage itself; they only get
                  # baked as literal absolute paths into generated build
                  # files, for the final stage's DESTDIR-relative install
                  # to act on later.
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
                    # NIXGG_BYPASS gates acceleration per shim invocation,
                    # not whether shims are on PATH. Shims go on PATH from
                    # postPatch, not preBuild, for the same absolute-path
                    # reason as the configure stage's own bypassShims —
                    # BYPASS itself must stay set through configure
                    # (autoreconfHook, cmake probes, or the restored
                    # configure snapshot) — only buildPhase needs real
                    # acceleration.
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
                    # "failed to submit output path for 'out'".
                    # postPhases is spliced on unconditionally by setup.sh.
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

        # ---- final stage, "restore a built tree" flavor ------------------
        # Used whenever splitAtBuild is true, regardless of splitAtConfigure.
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

                    # The final stage needs the shims reachable but
                    # inert: build systems bake absolute tool paths at
                    # configure time (cmake does; a caller can via
                    # makeFlags), so those paths get invoked here whether
                    # or not we planned for it. Without the env the shim
                    # cannot build its config and exits non-zero. Nothing
                    # is left to accelerate, so BYPASS makes each one
                    # exec the real tool.
                    export NIXGG_BYPASS=1
                    ${ggShimsOnPath knownStorePathsJSON}
                    export DESTDIR="$NIX_BUILD_TOP/.gg-destdir"
                    # `export DESTDIR` alone is not enough: some packages'
                    # own Configure/Makefile (openssl's
                    # Configurations/unix-Makefile.tmpl is one) contain a
                    # plain `DESTDIR=` assignment of their own, which GNU
                    # Make's variable-precedence rules let silently
                    # override an inherited environment variable of the
                    # same name — only a value on make's OWN command line
                    # wins over that. Appended here, at ggRestorePhase
                    # RUNTIME (not baked into installFlags as a Nix
                    # string), so bash — not Nix, not make — expands
                    # $NIX_BUILD_TOP into a plain path with no literal `$`
                    # left in it: installFlags is passed straight through
                    # to make's argv, and a literal `$` there gets
                    # reinterpreted as make's OWN `$X`-style variable
                    # reference. Confirmed necessary directly: openssl's
                    # own `make install_sw` wrote straight to its literal
                    # `/nonexistent` prefix, never under $DESTDIR, until
                    # this was added.
                    # installFlagsArray, not installFlags: under
                    # __structuredAttrs `installFlags` is a bash ARRAY, and
                    # assigning a scalar to an array name writes element 0
                    # — which silently welds this onto the package's first
                    # real flag. nixpkgs' kernel sets INSTALL_PATH=$out
                    # there, so `make install` received one token
                    # "INSTALL_PATH=… DESTDIR=…" and died on `cp: target
                    # 'DESTDIR=…': No such file or directory` — after a
                    # 2h54m build that had otherwise fully succeeded.
                    # setup.sh concatenates installFlagsArray in both
                    # modes (concatTo, installPhase), and it is always a
                    # plain array, so appending there is mode-independent.
                    installFlagsArray+=( "DESTDIR=$DESTDIR" )
                    runHook postGgRestore
                  '';
                  installFlags = (orig.installFlags or "");
                  postInstall = restoreOutputsScript + (orig.postInstall or "");
                  # Must run before fixupPhase's own patchelf-based
                  # rpath-shrinking, which (correctly) drops any rpath
                  # entry pointing at a path that doesn't exist — exactly
                  # what the placeholder rpaths would still be if this
                  # ran any later.
                  preFixup = elfRpathFixupScript + (orig.preFixup or "");
                };
            in
            applyExtra extraInstallAttrs finalAttrs base
          );

        # ---- final stage, "restore a configure snapshot" flavor ---------
        # Used only when splitAtConfigure is true and splitAtBuild is
        # false: no build stage exists, so this stage does its own fresh
        # unpack+patch against the REAL (unfiltered) src, overlays the
        # configure stage's real multi-output ggtree snapshot on top, and
        # continues straight through build..dist with no acceleration.
        finalFromConfigure =
          { configureStage }:
          mkDerivationSuper (
            finalAttrs:
            let
              orig = lib.toFunction argsOrFn finalAttrs;
              base =
                orig
                // {
                  # Own real unpack+patch (cheap: tar extraction + patch
                  # application) against the REAL, unfiltered src — this
                  # is what makes the restore below work whether
                  # configureSrcFilter is set or not: this stage's own
                  # sourceRoot always matches the real source, so the
                  # overlay lands at the right depth with no
                  # special-casing needed.
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
        # Degenerate case: no cut at all. extraInstallAttrs still
        # applies, consistent with its docstring describing it as the
        # always-present final stage — here that's the whole build.
        mkDerivationSuper (finalAttrs: applyExtra extraInstallAttrs finalAttrs (lib.toFunction argsOrFn finalAttrs));
  }
)
