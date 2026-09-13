# mkNixggBuild — wraps a user build command in a builder-rpc-v0
# derivation whose output is one or more derivation files (.drv), one
# per declared target. Consumer gets each target's artifact via
# `builtins.outputOf drv.outPath "<targetName>.drv"`, or `.result`/
# `.package` for the single-target case. Built on stdenv.mkDerivation
# to get buildInputs/setup-hooks/propagatedBuildInputs for free.
#
# builder-rpc-v0 intentionally unsets $out (the builder submits a
# store path via `submit-output` instead). stdenv's _assignFirst needs
# some value there, so out="/nonexistent" — any accidental write fails
# visibly. See nixgg/dyn-drv/NOTES.md for the underlying mechanism.
{
  lib,
  stdenv,
  mkShell,
  bash,
  coreutils,
  gnumake,
  gcc,
  nixgg,        # store path with bin/nixgg + shims/
  nixHelpers,   # nixgg-nix helper package (unused in sandbox mode, kept for parity with native)
  patchedNix,   # nix with builder-rpc-v0 + submit-output
}:

{
  pname,
  version ? "0",
  src,
  # Runs during buildPhase; stdenv's setup already handled configure hooks,
  # PATH, buildInputs -> NIX_LDFLAGS etc.
  buildCommand,
  # One entry per binary/archive this build produces: { name; path; }.
  # `name` becomes the outer derivation's output key ("<name>.drv");
  # `path` is what the shim matches against `-o <output>` to decide which
  # link/archive step produced it (basename / relative / absolute).
  #
  # Must be a LIST, not an attrset: the FIRST entry is "the" target for
  # the back-compat `result`/`package` below, and attrset iteration order
  # is alphabetical, not declaration order — an earlier attrset-based
  # version silently picked mosh-client over mosh-server as "the" result
  # for exactly that reason (caught by tests/smoke.sh's mosh fixture).
  #
  # Multiple entries are for one buildCommand invocation that genuinely
  # produces more than one binary/archive (mosh's mosh-server + mosh-client,
  # lua's lua + luac) — splitting into N separate mkNixggBuild calls would
  # mean N redundant full builds of the same source tree.
  targets,
  # Passed straight through to stdenv.mkDerivation.
  nativeBuildInputs ? [ ],
  buildInputs ? [ ],
  propagatedBuildInputs ? [ ],
  # Opt-in batch-group declarations: go/internal/shim/batcharchive.go's
  # tryBatchArchive combines a same-group archive's pending compiles into
  # ONE derivation via nix/batchArchiver.nix when it can, else falls back
  # to one-derivation-per-TU (see examples/fmt, examples/gcc for the
  # negative case, examples/mosh for where it engages).
  #
  # A list of { name, patterns } — patterns are filepath.Match-style globs
  # (plus a literal "**" for "zero or more path segments", see
  # internal/batch.matchPath) matched against each compile's source path
  # relative to the project root. Which subtrees are "stable" is an
  # author/tooling judgment call nixgg doesn't infer; see
  # nix/batchGroupPresets.nix for common vendored-dependency layouts.
  #
  #   batchGroups = [
  #     { name = "vendor"; patterns = [ "deps/**/*.c" ]; }
  #   ];
  batchGroups ? [ ],
}:

let
  targetNames = map (t: t.name) targets;

  # Every target's submitted drv is named "<outerName>-<targetName>"
  # (see go/internal/shim/storeinput.go's maybeSubmit for the naming
  # scheme submit-output requires once more than one target shares an
  # outer wrapper). Single-target builds use this same "nixgg-<pname>"
  # outer name too, since a bare "<target>" outer name only worked
  # because "out" is the one output key outputPathName leaves unsuffixed.
  outerName = "nixgg-${pname}";

  # NIXGG_SANDBOX_TARGET: one entry per target, keyed by the caller's
  # path/basename pattern, valued by the outer output key the shim should
  # submit under. Every value ends in ".drv" as part of the output KEY's
  # text — unrelated to `nix derivation add`'s separate, automatic ".drv"
  # suffix on whatever name it's given. outputPathName only omits a
  # suffix for outputKey == "out"; every other key gets "-<outputKey>"
  # appended, so the key must already carry ".drv" to match.
  sandboxTargetJSON = builtins.toJSON (
    lib.listToAttrs (
      map (t: lib.nameValuePair t.path "${t.name}.drv") targets
    )
  );

  # Store-path inputs the shims treat as real references, shared by both
  # modes (preBuild for sandbox, shellHook for native) so their drv
  # hashes stay comparable.
  #
  # Each package expands to *every* output via `.all`, not just its
  # default output: zlib/ncurses/openssl put headers under a separate
  # `dev` output at a different store path, and stdenv's setup-hooks add
  # -isystem/-L for both. Using only the default output meant the `-dev`
  # path never matched anything here, so storedeps.From found nothing for
  # it and the sandbox build failed with "fatal error: zlib.h: No such
  # file or directory" even with the right CFLAGS.
  #
  # Deliberately NOT each output's transitive closure (exportReferencesGraph
  # would give that in sandbox mode, but native mode has no eval-time
  # equivalent) — that asymmetry broke drv-hash equivalence for every TU
  # when tried. The plain output list matches what setup-hooks actually
  # emit into NIX_CFLAGS_COMPILE/NIX_LDFLAGS, not their deps' deps.
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

  # Wire format for $NIXGG_BATCH_GROUPS — see go/internal/batch's
  # jsonGroup for the Go-side parse.
  batchGroupsJSON = builtins.toJSON batchGroups;

  # NIXGG_* vars every shim invocation needs, in both modes. Bound once so
  # `drv` and `shell` can't drift apart.
  #
  # NIXGG_SANDBOX_TARGET/name live here (not just on drv's attrs) because
  # native mode's Link/Archive calls need the same naming-override inputs
  # sandbox mode's linkSandbox/archiveSandbox use — otherwise native mode
  # would keep the old naming forever while sandbox mode moved on,
  # breaking drv-hash equivalence for every build.
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

  # Shell prelude shared by the sandbox build (`preBuild`) and the
  # native-replay dev shell (`shellHook`) — they MUST produce identical
  # NIX_CFLAGS_COMPILE/NIX_LDFLAGS, or the two modes' drv hashes diverge
  # (the invariant tests/drv-equivalence.sh exists to protect). One
  # source, interpolated twice, after a real quoting bug came from
  # hand-syncing two copies of this text.
  #
  # Rationale for each scrub:
  #   - NIX_HARDENING_ENABLE: outer cc-wrapper marker. Inner drvs get
  #     their own wrapper with its own defaults; leaking this makes
  #     sandbox-produced drvs diverge from native (mkShellNoCC) ones.
  #   - CC/CXX/AR/LD/...: stdenv defaults them to cc/c++/ar. Under
  #     mkShellNoCC they aren't set, so the caller's Makefile picks its
  #     own default (usually `c++` via `?=`). Unsetting lets both modes
  #     converge on the same tool name.
  #   - -frandom-seed=... : per-invocation, so poisonous to CA-hash
  #     stability.
  #   - -rpath <...>/outputs/out/lib and -rpath /nonexistent/lib:
  #     bintools-wrapper always injects one off the derivation's $out,
  #     which is /nonexistent in the sandbox (see the out= trick below)
  #     and <workdir>/outputs/out under `nix develop`. Per-path, and not
  #     real linker information for a build whose actual output is a
  #     submitted drv.
  #
  # NOT scrubbed: NIX_CC_WRAPPER_TARGET_HOST_<triple>. Bypass-mode
  # configure steps exec-passthrough to the outer gcc-wrapper, which
  # needs that trigger to inject buildInputs' -isystem / -L. wrapperenv
  # gates propagation into the inner drv on non-empty flags, so
  # empty-buildInputs builds still hash identically to native.
  scrubWrapperEnv = ''
    export PATH="${nixgg}/bin:${nixgg}/shims:${patchedNix}/bin:$PATH"
    unset NIX_HARDENING_ENABLE
    unset CC CXX LD AR RANLIB NM STRIP OBJCOPY OBJDUMP READELF SIZE
    NIX_CFLAGS_COMPILE=$(printf '%s' "''${NIX_CFLAGS_COMPILE:-}" | sed -e 's| *-frandom-seed=[^ ]*||g')
    NIX_LDFLAGS=$(printf '%s' "''${NIX_LDFLAGS:-}" | sed -e 's| *-rpath [^ ]*/outputs/out/lib||g' -e 's| *-rpath /nonexistent/lib||g')
    export NIX_CFLAGS_COMPILE NIX_LDFLAGS
    export NIXGG_KNOWN_STORE_PATHS=${lib.escapeShellArg knownStorePathsJSON}
    export NIXGG_BATCH_GROUPS=${lib.escapeShellArg batchGroupsJSON}
  '';

  drv = stdenv.mkDerivation (
    {
      name = outerName;
      inherit src;
      inherit nativeBuildInputs buildInputs propagatedBuildInputs;

      # `out` stays a plain derivation attribute (satisfies stdenv's
      # _assignFirst) but is deliberately absent from `outputs`: every
      # real target gets its own "<name>.drv" key instead (see
      # sandboxTargetJSON above). Nix requires every declared output to
      # actually be submitted, and a dotted name like "mosh-server.drv"
      # isn't a legal bash identifier, so `out` can't double as one.
      out = "/nonexistent";
      outputs = map (n: "${n}.drv") targetNames;

      dontUnpack = false;
      dontConfigure = true;
      dontInstall = true;
      dontFixup = true;

      # Speeds up the shim submission phase; per-drv build parallelism
      # itself happens in Nix's outer pass.
      enableParallelBuilding = true;

      requiredSystemFeatures = [ "builder-rpc-v0" ];

      __contentAddressed = true;
      outputHashMode = "text";
      outputHashAlgo = "sha256";

      # nix-command + ca + dyn-drv for the inner nix invocations our
      # shims make (nix derivation add / nix store add / submit-output).
      NIX_CONFIG = ''
        extra-experimental-features = nix-command ca-derivations dynamic-derivations
      '';

      # NIXGG_* the shims read. Toolchain roots — and NIXGG_SANDBOX_TARGET
      # /name — come from toolchainEnv (merged below) so they can't
      # drift from shell's shellHook.
      NIXGG_STORE          = "auto";
      NIXGG_SANDBOX        = "1";
      # Raw worker-protocol client for the sandbox's daemon socket
      # (internal/rpc), replacing per-call fork+exec of `nix derivation
      # add`/`nix store add --scan`/`nix store submit-output`. NIXGG_RPC=0
      # is the escape hatch back to the CLI fallback.
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

  # Plain mkShell mirroring `drv`'s stdenv env, so `nix develop` can enter
  # it (a text-hashed dyn-drv can't be an env-target). Used by
  # tests/drv-equivalence.sh to run the same buildCommand natively and
  # verify inner drvs hash-match. Not NoCC: bypass-mode configure steps
  # (autoconf, cmake probes) exec-passthrough to the outer cc-wrapper,
  # which needs the activation trigger to inject buildInputs' -isystem/-L.
  shell = mkShell {
    name = "${outerName}-shell";
    inherit nativeBuildInputs buildInputs propagatedBuildInputs;
    # Lets tests/drv-equivalence.sh replay the exact sandbox command
    # natively via `nix eval --raw .#<attr>-shell.passthru.buildCommand`.
    passthru.buildCommand = buildCommand;
    shellHook = scrubWrapperEnv + toolchainEnvShellHook;
  };
  # `builtins.outputOf` returns a string with store context, not a
  # derivation — `nix run`/`nix profile install`/buildInputs all need
  # `.type`/`.drvPath`/`.outPath`, which a string lacks (`nix run .#hello`
  # failed with "attribute 'type' does not exist" for this reason).
  # `package` wraps it in an ordinary stdenv.mkDerivation that copies the
  # bytes out; a plain derivation CAN depend on an outputOf string, and
  # Nix resolves the whole dyn-drv chain into the wrapper's closure.
  #
  # Copy rather than symlink so `nix profile install`/`ldd`/`readlink`
  # show this package's own path, not an internal implementation detail.
  #
  # An archive (target path ending in .a) has no program to run, so
  # meta.mainProgram stays unset there.
  isProgramTarget = path: !(lib.hasSuffix ".a" (baseNameOf path));

  # drv."<name>.drv" (not drv.outPath) is this target's own output on the
  # multi-output outer derivation. outputOf's second arg is "out" — the
  # INNER link/archive drv's single output name, not the outer key again.
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

  # First entry in `targets` (list, so unambiguous) — the back-compat
  # result/package shape below.
  primaryTargetName = (builtins.head targets).name;
in
{
  inherit drv shell results packages;

  # Back-compat shape: the FIRST target's own result/package.
  result = results.${primaryTargetName};
  package = packages.${primaryTargetName};
}
