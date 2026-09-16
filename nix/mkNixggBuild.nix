# Wraps a build command in a builder-rpc-v0 derivation whose output is one
# or more submitted .drv targets; see nixgg/dyn-drv/NOTES.md.
{
  lib,
  stdenv,
  mkShell,
  bash,
  coreutils,
  gnumake,
  gcc,
  nixgg,
  nixHelpers,   # nixgg-nix helper; shim doesn't use it in sandbox mode, but native mode's dev shell needs it for parity
  patchedNix,   # nix built with builder-rpc-v0 + submit-output
}:

{
  pname,
  version ? "0",
  src,
  buildCommand,
  # [{ name, path }, ...] — NOT an attrset: attrset iteration is alphabetical,
  # not declaration order, which once silently picked the wrong entry as the
  # back-compat `result`/`package`. `name` becomes the outer output key
  # ("<name>.drv"); `path` is matched against the shim's `-o <output>`.
  targets,
  nativeBuildInputs ? [ ],
  buildInputs ? [ ],
  propagatedBuildInputs ? [ ],
  # [{ name, patterns }, ...] — patterns are filepath.Match-style globs (plus
  # "**") matched against each compile's source path; go/internal/shim/
  # batcharchive.go's tryBatchArchive batches matching groups into one
  # derivation instead of one-per-TU.
  batchGroups ? [ ],
  # Subtrees the caller has declared unmodellable (internal/mode's
  # NIXGG_PASSTHROUGH_PATHS); the shims pass work under them straight
  # through instead of registering a derivation. Empty by default — see
  # nix/dynDrvShared.nix's own copy of this parameter for the mechanism
  # this mirrors (splitStdenv's equivalent).
  passthroughPaths ? [ ],
}:

let
  targetNames = map (t: t.name) targets;

  # go/internal/shim/storeinput.go's maybeSubmit names every submitted drv
  # "<outerName>-<targetName>", so single-target builds use this same
  # "nixgg-<pname>" outer name too rather than a bare "<target>" name.
  outerName = "nixgg-${pname}";

  # NIXGG_SANDBOX_TARGET: caller path/basename -> outer output key the shim
  # should submit under. Keys must end in ".drv" as literal text — that's
  # separate from `nix derivation add`'s automatic ".drv" suffix, which it
  # only omits for outputKey == "out".
  sandboxTargetJSON = builtins.toJSON (
    lib.listToAttrs (
      map (t: lib.nameValuePair t.path "${t.name}.drv") targets
    )
  );

  # Expand each input via `.all` (not just the default output): stdenv's
  # setup-hooks add -isystem/-L for dev outputs too (zlib, ncurses, openssl).
  # Deliberately not each output's transitive closure either — that broke
  # native/sandbox drv-hash equivalence when tried.
  knownStorePathInputs =
    builtins.concatMap (p: p.all or [ p ]) (
      buildInputs
      ++ propagatedBuildInputs
      ++ [
        bash
        coreutils
        gcc
        nixgg
        nixHelpers
        patchedNix
      ]
    );
  knownStorePathsJSON = builtins.toJSON (map toString knownStorePathInputs);

  # Wire format for $NIXGG_BATCH_GROUPS — see go/internal/batch's jsonGroup
  # for the Go-side parse.
  batchGroupsJSON = builtins.toJSON batchGroups;

  # Bound once so `drv` and `shell` can't drift apart. NIXGG_SANDBOX_TARGET
  # and name live here because native mode's Link/Archive calls need the
  # same naming inputs sandbox mode's linkSandbox/archiveSandbox use.
  toolchainEnv = {
    NIXGG_ROOT           = "${nixgg}";
    NIXGG_COMPILER_ROOT  = "${gcc}";
    NIXGG_BASH_ROOT      = "${bash}";
    NIXGG_COREUTILS_ROOT = "${coreutils}";
    NIXGG_GNUMAKE_ROOT   = "${gnumake}";
    NIXGG_REAL_CC        = "${gcc}/bin/g++";
    NIXGG_NIX            = "${patchedNix}/bin/nix";
    NIXGG_NIX_HELPERS    = "${nixHelpers}";
    NIXGG_SANDBOX_TARGET = sandboxTargetJSON;
    name                 = outerName;
  };
  toolchainEnvShellHook = lib.concatStrings (
    lib.mapAttrsToList (k: v: "export ${k}=${lib.escapeShellArg v}\n") toolchainEnv
  );

  # Shared between preBuild (sandbox) and shellHook (native) so both modes
  # produce identical NIX_CFLAGS_COMPILE/NIX_LDFLAGS and their drv hashes
  # stay comparable.
  #
  #   - NIX_HARDENING_ENABLE unset: inner drvs get their own cc-wrapper;
  #     leaking the outer one diverges sandbox vs native (mkShellNoCC) drvs.
  #   - CC/CXX/AR/... unset: stdenv sets these but mkShellNoCC doesn't, so
  #     unsetting lets both converge on the caller's Makefile default.
  #   - -frandom-seed=... stripped: per-invocation, poisonous to CA-hash
  #     stability.
  #   - -rpath .../outputs/out/lib and -rpath /nonexistent/lib stripped:
  #     bintools-wrapper injects one from $out, which differs between the
  #     sandbox (/nonexistent) and `nix develop` (<workdir>/outputs/out).
  #
  # NOT scrubbed: NIX_CC_WRAPPER_TARGET_HOST_<triple> — bypass-mode configure
  # steps need it to trigger the outer wrapper's -isystem/-L injection.
  scrubWrapperEnv = ''
    export PATH="${nixgg}/bin:${nixgg}/shims:${patchedNix}/bin:$PATH"
    unset NIX_HARDENING_ENABLE
    unset CC CXX LD AR RANLIB NM STRIP OBJCOPY OBJDUMP READELF SIZE
    NIX_CFLAGS_COMPILE=$(printf '%s' "''${NIX_CFLAGS_COMPILE:-}" | sed -e 's| *-frandom-seed=[^ ]*||g')
    NIX_LDFLAGS=$(printf '%s' "''${NIX_LDFLAGS:-}" | sed -e 's| *-rpath [^ ]*/outputs/out/lib||g' -e 's| *-rpath /nonexistent/lib||g')
    export NIX_CFLAGS_COMPILE NIX_LDFLAGS
    export NIXGG_KNOWN_STORE_PATHS=${lib.escapeShellArg knownStorePathsJSON}
    export NIXGG_BATCH_GROUPS=${lib.escapeShellArg batchGroupsJSON}
    export NIXGG_PASSTHROUGH_PATHS=${lib.escapeShellArg (builtins.toJSON passthroughPaths)}
  '';

  drv = stdenv.mkDerivation (
    {
      name = outerName;
      inherit src;
      inherit nativeBuildInputs buildInputs propagatedBuildInputs;

      # builder-rpc-v0 unsets $out (the builder submits a store path via
      # submit-output instead); stdenv's _assignFirst still needs some
      # value here, and a dotted name like "mosh-server.drv" can't be a
      # bash identifier, so `out` stays out of `outputs`.
      out = "/nonexistent";
      outputs = map (n: "${n}.drv") targetNames;

      dontUnpack = false;
      dontConfigure = true;
      dontInstall = true;
      dontFixup = true;

      enableParallelBuilding = true;

      requiredSystemFeatures = [ "builder-rpc-v0" ];

      __contentAddressed = true;
      outputHashMode = "text";
      outputHashAlgo = "sha256";

      NIX_CONFIG = ''
        extra-experimental-features = nix-command ca-derivations dynamic-derivations
      '';

      NIXGG_STORE          = "auto";
      NIXGG_SANDBOX        = "1";
      # Worker-protocol RPC client for the sandbox daemon socket
      # (internal/rpc), replacing per-call fork+exec of the nix CLI.
      # NIXGG_RPC=0 is the escape hatch back to that CLI fallback.
      NIXGG_RPC            = "1";
      preBuild = scrubWrapperEnv;

      buildPhase = ''
        runHook preBuild
        ${buildCommand}
        runHook postBuild
      '';
    }
    // toolchainEnv
  );

  # A text-hashed dyn-drv can't be a `nix develop` env-target, so this
  # mirrors `drv`'s env in a plain shell. Not NoCC: bypass-mode configure
  # steps (autoconf, cmake probes) exec-passthrough to the outer
  # cc-wrapper, which needs the activation trigger for buildInputs'
  # -isystem/-L.
  shell = mkShell {
    name = "${outerName}-shell";
    inherit nativeBuildInputs buildInputs propagatedBuildInputs;
    passthru.buildCommand = buildCommand;
    shellHook = scrubWrapperEnv + toolchainEnvShellHook;
  };

  # outputOf returns a string with store context, not a derivation (no
  # .type/.drvPath), which nix run/profile install/buildInputs all need —
  # this wraps it in a plain stdenv.mkDerivation that copies the bytes out.
  # Copy rather than symlink so nix profile install/ldd/readlink resolve to
  # this package's own path.
  isProgramTarget = path: !(lib.hasSuffix ".a" (baseNameOf path));

  # drv."<name>.drv" (not drv.outPath) is this target's own output on the
  # multi-output outer derivation; outputOf's second arg "out" is the INNER
  # link/archive drv's single output name, not the outer key again.
  results = lib.listToAttrs (
    map (t: lib.nameValuePair t.name (builtins.outputOf drv."${t.name}.drv".outPath "out")) targets
  );
  packages = lib.listToAttrs (
    map (t: lib.nameValuePair t.name (
      stdenv.mkDerivation ({
        pname = t.name;
        version = "0";
        dontUnpack = true;
        installPhase = ''
          mkdir -p "$out"
          cp -a ${results.${t.name}}/. "$out/"
        '';
        passthru = { inherit drv shell packages results; result = results.${t.name}; };
      } // lib.optionalAttrs (isProgramTarget t.path) {
        meta.mainProgram = t.name;
      })
    )) targets
  );

  primaryTargetName = (builtins.head targets).name;
in
{
  inherit drv shell results packages;

  result = results.${primaryTargetName};
  package = packages.${primaryTargetName};
}
