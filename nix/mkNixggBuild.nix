# mkNixggBuild — wrap a user build command in a builder-rpc-v0
# derivation whose output is one or more *derivation files* (.drv),
# one per declared target (see `targets` below). The consumer picks
# up each target's compiled artifact via `builtins.outputOf
# drv.outPath "<targetName>.drv"` — or just `.result`/`.package` for
# the common single-target case, which stays this call's own default
# shape (most callers have exactly one target).
#
# Built on stdenv.mkDerivation. Stdenv handles buildInputs →
# NIX_LDFLAGS / NIX_CFLAGS_COMPILE / PKG_CONFIG_PATH, setup hooks
# (autoreconfHook, cmake, etc.), propagatedBuildInputs (which pulls
# in a package's transitive dev-time deps automatically), gcc-
# wrapper activation, and much more that we'd otherwise reinvent.
#
# The one deviation: builder-rpc-v0 mode intentionally UNSETS $out
# (the builder is supposed to `submit-output` a store path instead
# of writing to $out). Stdenv assumes $out exists via _assignFirst,
# so we set out="/nonexistent" at the drv level — same trick
# nix-ninja's mkMesonPackage uses. Any accidental write to $out
# fails visibly with "no such directory".
#
# See nixgg/dyn-drv/NOTES.md for the underlying mechanism.
{
  lib,
  stdenv,
  mkShell,
  bash,
  coreutils,
  gnumake,
  gcc,
  nixgg,        # store path with bin/nixgg + shims/
  nixHelpers,   # nixgg-nix helper package (unused in sandbox mode,
                # kept for parity with native)
  patchedNix,   # nix with builder-rpc-v0 + submit-output
}:

{
  pname,
  version ? "0",
  src,
  # The command that produces every target. Runs during the
  # buildPhase; stdenv's setup has already handled configure hooks,
  # PATH, buildInputs → NIX_LDFLAGS etc.
  buildCommand,
  # One entry per binary/archive this build produces — a list of
  # { name; path; }, e.g. [ { name = "mosh-server"; path =
  # "mosh-server"; } { name = "mosh-client"; path = "mosh-client"; }
  # ]. `name` is this call's own label for the target (becomes the
  # outer derivation's own output key, "<name>.drv" — see below for
  # why the .drv suffix is load-bearing) and `path` is what the shim
  # matches against `-o <output>` to decide which link/archive step
  # produced it, same matching rule NIXGG_SANDBOX_TARGET always used
  # (basename / relative / absolute).
  #
  # A LIST, not an attrset: order is load-bearing — the FIRST entry
  # is "the" target for this call's own back-compat `result`/
  # `package` (below), and Nix attrsets have no defined order
  # (builtins.attrNames sorts alphabetically, confirmed directly: an
  # earlier version of this param used an attrset, and mosh's own
  # targets ended up with mosh-client silently picked as "the" result
  # instead of mosh-server, purely because "mosh-client" < "mosh-server"
  # alphabetically — a real, silent regression caught by
  # tests/smoke.sh's mosh fixture).
  #
  # Exactly one entry is the common case (every example before this
  # param existed had exactly one target) and remains the API's
  # default shape to reach for — multiple entries are for a build
  # that genuinely produces more than one binary/archive from ONE
  # buildCommand invocation (mosh's mosh-server + mosh-client, lua's
  # lua + luac), where splitting into N separate mkNixggBuild calls
  # would mean N redundant full builds of the same source tree.
  targets,
  # Passed straight through to stdenv.mkDerivation.
  nativeBuildInputs ? [ ],
  buildInputs ? [ ],
  propagatedBuildInputs ? [ ],
  # Opt-in, best-effort batch-group declarations — see
  # go/internal/batch's own package docstring for the mechanism and
  # scope: go/internal/shim/batcharchive.go's tryBatchArchive combines
  # a same-group archive's pending compiles into ONE derivation (N
  # compiles + 1 archive) via nix/batchArchiver.nix when it can, and
  # falls back to nixgg's normal one-derivation-per-TU path otherwise
  # (e.g. when the archive itself is the build's own
  # NIXGG_SANDBOX_TARGET — see examples/fmt/default.nix, examples/gcc/
  # default.nix for that negative case, and examples/mosh/default.nix
  # for the case where it actually engages).
  # A list of { name, patterns } — name is a short label for the
  # group (shows up in shim logs), patterns are filepath.Match-style
  # globs (PLUS a literal "**" segment for "zero or more path
  # segments", see internal/batch.matchPath) matched against each
  # compile's source path relative to the project root.
  #
  # This is an author/tooling judgment call, same shape as
  # configureSrcFilter's includePatterns: nixgg doesn't infer which
  # directories are "stable" — a project author (or a repo-inspection
  # script computing edit frequency from git history, e.g.) supplies
  # this list because they know which subtrees are vendored/rarely
  # touched vs. actively worked on. See
  # nix/batchGroupPresets.nix for a starting point covering common
  # vendored-dependency layouts (redis-style deps/, autotools-style
  # third_party/).
  #
  #   batchGroups = [
  #     { name = "vendor"; patterns = [ "deps/**/*.c" ]; }
  #   ];
  batchGroups ? [ ],
  # Stage each TU as a farm of symlinks into per-file store objects.
  # Sandbox mode only, off by default — see splitStdenv.sharedStaging.
  sharedStaging ? false,

  # Subtrees whose build reads object bytes inline, or expects a compile
  # to fail — see splitStdenv.passthroughPaths. Empty for most projects.
  passthroughPaths ? [ ],
}:

let
  targetNames = map (t: t.name) targets;

  # The outer derivation's own name — every target's own submitted
  # drv is named "<outerName>-<todaysNaturalName>" (e.g.
  # "nixgg-mosh-bin-mosh-server"), NOT the target's own name directly.
  # This is the naming scheme submit-output's own check requires once
  # more than one target shares an outer wrapper — see
  # go/internal/shim/storeinput.go's maybeSubmit docstring for the
  # full mechanism and the outputPathName math it rests on. A single-
  # target build no longer gets the old bare "bin-<target>.drv" outer
  # name (that shape can't generalize past one target — its own name
  # WAS the target's name, which only worked because "out" is the one
  # output key outputPathName leaves unsuffixed); every build,
  # including single-target ones, now uses this "nixgg-<pname>"
  # outer name uniformly.
  outerName = "nixgg-${pname}";

  # NIXGG_SANDBOX_TARGET's JSON-map shape (see maybeSubmit's own
  # docstring): one entry per target, keyed by the caller's own
  # path/basename pattern, valued by the outer output key the shim
  # should submit under. Every value ends in ".drv" — literally part
  # of the output KEY's own text, unrelated to nix derivation add's
  # separate, automatic ".drv" suffix on whatever NAME it's given.
  # Confirmed directly: outputPathName(outerName, outputKey) only
  # omits a suffix for outputKey == "out"; every other key gets
  # "-<outputKey>" appended verbatim, so the key itself must already
  # carry ".drv" for the submitted leaf's own (separately, always
  # ".drv"-suffixed) name to match.
  sandboxTargetJSON = builtins.toJSON (
    lib.listToAttrs (
      map (t: lib.nameValuePair t.path "${t.name}.drv") targets
    )
  );

  # The set of "known" store-path inputs the shims are allowed to treat
  # as real references — passed to both modes as the literal JSON in
  # knownStorePathsJSON below (preBuild for sandbox, shellHook for
  # native). Computed once at eval time so both modes see byte-identical
  # input, which is what makes their drv hashes comparable at all.
  #
  # Each package expands to *every* output it has (via `.all`, which
  # every multi-output derivation carries — lib/customisation.nix), not
  # just its default output. Packages like zlib/ncurses/openssl put
  # headers under a separate `dev` output at a different store path
  # than the default `out`; stdenv's setup-hooks add -isystem/-L for
  # both. Using only `toString pkg` (the default output) meant the
  # `-dev` path never matched anything in this list, so storedeps.From
  # found nothing for it and it never made it into inputs.srcs — the
  # sandbox build then failed with "fatal error: zlib.h: No such file
  # or directory" even though the CFLAGS were pointing right at it.
  #
  # Deliberately NOT each output's transitive closure (which
  # exportReferencesGraph would give in sandbox mode, but has no
  # eval-time equivalent for native mode): the closure of e.g.
  # openssl-dev includes libxcrypt, which then gets matched into
  # inputs.srcs on the sandbox side only, since native mode has no way
  # to compute that closure without a build — an asymmetry that broke
  # drv-hash equivalence across every single TU. The plain output list
  # is already the right scope: what we're matching against is text
  # setup-hooks of exactly these packages emit into
  # NIX_CFLAGS_COMPILE/NIX_LDFLAGS, not their dependencies' dependencies.
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
  # jsonGroup for the Go-side parse. Computed once at eval time, same
  # as knownStorePathsJSON above, so preBuild (sandbox) and shellHook
  # (native) can't drift apart.
  batchGroupsJSON = builtins.toJSON batchGroups;

  # NIXGG_* vars every shim invocation needs, in both modes. Bound once
  # so `drv` (as derivation attrs) and `shell` (as shellHook exports)
  # can't drift apart — the same fix scrubWrapperEnv already applies to
  # NIX_CFLAGS_COMPILE/NIX_LDFLAGS.
  #
  # NIXGG_SANDBOX_TARGET/name are here (not just on drv's own attrs
  # the way they used to be) because native mode's Link/Archive calls
  # now need the SAME naming-override inputs sandbox mode's
  # linkSandbox/archiveSandbox already had — without this, native
  # mode would keep the old "bin-<outName>" naming forever while
  # sandbox mode moved to "nixgg-<pname>-bin-<outName>", breaking
  # drv-hash equivalence for EVERY build, not just multi-target ones
  # (the outer wrapper's own name changed for single-target builds
  # too — see outerName's own docstring). Nix already sets $name
  # automatically for any derivation (confirmed directly); exporting
  # it here too just keeps native mode's shellHook symmetric with
  # what the sandbox build gets for free, since `nix develop` doesn't
  # go through a real derivation the same way.
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
  # native-replay dev shell (`shellHook`).
  #
  # These two MUST produce identical NIX_CFLAGS_COMPILE / NIX_LDFLAGS.
  # `preBuild` is what the shims capture in sandbox mode; `shellHook` is
  # what they capture when tests/drv-equivalence.sh replays the same
  # build natively under `nix develop`. Any difference between them
  # shows up as a drv-hash divergence between the two modes — i.e. it
  # breaks the invariant the whole test suite exists to protect.
  #
  # It used to be two hand-synced copies of this text. They happened to
  # agree, but a one-sided edit would only ever be caught by the slow
  # integration test (which needs a builder-rpc-v0 nix and a remote
  # builder) — the same "kept identical by discipline" trap that
  # produced a real quoting bug between internal/expr's shellQuoteFlags
  # and nix/{builder,linker}.nix. One source, interpolated twice.
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
    # Must match the sandbox-side attr below. mode.Passthrough reads this
    # in BOTH modes, so a native replay that lacks it models TUs the
    # sandbox passed through — different drv sets, and drv-equivalence
    # fails for any package that sets passthroughPaths.
    export NIXGG_PASSTHROUGH_PATHS=${lib.escapeShellArg (builtins.toJSON passthroughPaths)}
  '';

  drv = stdenv.mkDerivation (
    {
      name = outerName;
      inherit src;
      inherit nativeBuildInputs buildInputs propagatedBuildInputs;

      # builder-rpc-v0 wants $out unset. /nonexistent keeps stdenv's
      # _assignFirst happy while making any actual write visibly fail.
      # `out` stays a plain derivation ATTRIBUTE (satisfying
      # _assignFirst's need for some bash-legal env var) but is
      # deliberately absent from `outputs` below — every real target
      # gets its own "<name>.drv" key instead (see sandboxTargetJSON
      # above), and Nix requires every declared output to actually be
      # submitted; confirmed directly that a declared-but-never-
      # submitted "out" fails the whole build with "failed to submit
      # output path for 'out'". A dotted name like "mosh-server.drv"
      # isn't a legal bash identifier either way, so `out` couldn't
      # double as one of the real targets' own var even if we wanted
      # it to.
      out = "/nonexistent";
      outputs = map (n: "${n}.drv") targetNames;

      # We set our own build phase; skip cd-into-src (we do that
      # ourselves after copying to a writable location), install, fixup.
      dontUnpack = false;    # stdenv unpacks src/ into $NIX_BUILD_TOP
      dontConfigure = true;  # user's buildCommand does what it needs
      dontInstall = true;
      dontFixup = true;

      # Populates $NIX_BUILD_CORES (used by build scripts via
      # `make -j"$NIX_BUILD_CORES"`). Actual per-drv build parallelism
      # still happens in Nix's outer pass — this only speeds up the
      # shim submission phase.
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
      # Raw worker-protocol client for the sandbox's own daemon socket
      # (internal/rpc), replacing per-call fork+exec of `nix
      # derivation add`/`nix store add --scan`/`nix store
      # submit-output`. Verified byte-identical drv hashes across the
      # full tests/drv-equivalence.sh sweep (149/149) and every
      # tests/smoke.sh example (including EXAMPLES=all's redis/ffmpeg/
      # llvm). NIXGG_RPC=0 is the escape hatch back to the CLI
      # fallback if something unforeseen turns up.
      NIXGG_RPC            = "1";
    }
    // lib.optionalAttrs sharedStaging { NIXGG_SHARED_STAGE = "1"; }
    // { NIXGG_PASSTHROUGH_PATHS = builtins.toJSON passthroughPaths; }
    // {

      # See scrubWrapperEnv above for what this does and why. Two points
      # specific to the sandbox side: the shims/ dir goes ahead of the
      # toolchain stdenv already put on PATH so cc/c++/ar dispatch through
      # us, and patchedNix's bin/ is what those shims exec for `nix
      # derivation add`. buildInputs' own contribution (-isystem / -L /
      # -rpath into real store paths) survives the scrub: stdenv's
      # setup-hooks add those AFTER the per-run noise, so filtering the
      # noise keeps the per-input flags.
      preBuild = scrubWrapperEnv;

      buildPhase = ''
        runHook preBuild
        ${buildCommand}
        runHook postBuild
      '';
    }
    // toolchainEnv
  );

  # Devshell that mirrors `drv`'s stdenv env (same buildInputs +
  # setup-hooks + NIX_CFLAGS_COMPILE/NIX_LDFLAGS/PKG_CONFIG_PATH),
  # but as a plain mkShell so `nix develop` can enter it (a
  # text-hashed dyn-drv can't be an env-target). Used by
  # tests/drv-equivalence.sh to run the same buildCommand natively
  # under the same tool env and verify inner drvs hash-match.
  #
  # We replay preBuild's scrubbing in shellHook so what the caller
  # inherits matches what the sandbox's shims capture. Anything after
  # here (make, cmake, autogen.sh) is up to the caller.
  # Plain mkShell (not NoCC): mirrors the outer drv's stdenv env
  # including cc-wrapper's activation trigger. Bypass-mode configure
  # steps (autoconf, cmake probes) exec-passthrough to the outer
  # cc-wrapper, which needs the trigger to inject buildInputs
  # -isystem / -L. The setup-hook's -rpath <outputs>/out/lib and
  # -frandom-seed noise gets scrubbed below.
  shell = mkShell {
    name = "${outerName}-shell";
    inherit nativeBuildInputs buildInputs propagatedBuildInputs;
    # The exact command string the sandbox runs, passed through so
    # tests/drv-equivalence.sh can replay it natively without
    # duplicating build recipes. Consumers pull it out with
    # `nix eval --raw .#<attr>-shell.passthru.buildCommand`.
    passthru.buildCommand = buildCommand;
    shellHook = scrubWrapperEnv + toolchainEnvShellHook;
  };
  # Consumer entrypoint: `builtins.outputOf` walks the dyn-drv chain
  # and returns the final compiled artifact as a string with store
  # context — NOT a derivation. `nix run`, `nix profile install`,
  # `nix shell`, `nix flake check`, and putting this in another
  # derivation's buildInputs all inspect `.type`/`.drvPath`/`.outPath`,
  # none of which a string has. `nix run .#hello` failed with
  # "attribute 'type' does not exist" for exactly this reason.
  #
  # `package` fixes that with one ordinary stdenv.mkDerivation copying
  # bytes out of the dyn-drv result. Tested directly before writing
  # this: a plain derivation CAN depend on an outputOf string — Nix
  # resolves the whole chain (compile drvs -> link/archive drv ->
  # this wrapper) and the wrapper's closure correctly references the
  # inner store path (confirmed via `nix path-info --json`). No new
  # experimental feature, no patched-nix requirement beyond what the
  # inner build already needs.
  #
  # Copy rather than symlink: `nix profile install` should install
  # this package's own path, not a link one hop into an internal
  # implementation detail, and `ldd`/`readlink` on the result should
  # show a path a user recognizes. Cost is the artifact's own size
  # (16 KB for hello, tens of MB for something llc-sized) — cheap next
  # to the compile time that produced it.
  #
  # mainProgram only makes sense for something FHS placed under bin/
  # — a link output. An archive (a target's own path ending in .a)
  # has no program to run; leaving meta.mainProgram unset there means
  # `nix run` fails with Nix's own "does not have meta.mainProgram"
  # rather than nixgg claiming a program that doesn't exist.
  isProgramTarget = path: !(lib.hasSuffix ".a" (baseNameOf path));

  # One outputOf placeholder + one copying package per target — see
  # targets' own docstring. results/packages are the multi-target-
  # native shape; result/package (below) stay as the single-target
  # shape for every caller that has exactly one entry in `targets`.
  #
  # drv."<name>.drv" (NOT drv.outPath) is this target's own output
  # attribute on the multi-output outer derivation — each carries its
  # own .outPath/.drvPath, confirmed directly (`d ? "mosh-server.drv"`
  # is true, and `(d."mosh-server.drv").outPath` resolves to that
  # output's own placeholder, distinct per output). outputOf's own
  # second argument is "out" — the INNER link/archive drv's own
  # single output name (every inner drv this Kind ever emits has
  # exactly one output called "out"), NOT the outer key again.
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

  # "The" target for the result/package back-compat shape below: the
  # FIRST entry in `targets` — a LIST, so this is unambiguous and
  # caller-controlled (unlike an attrset, whose iteration order isn't
  # the declaration order — see targets' own docstring for the real
  # regression this caused). A multi-target caller should still
  # prefer packages.<name>/results.<name> by name for anything beyond
  # "give me the one the caller cares about most".
  primaryTargetName = (builtins.head targets).name;
in
{
  inherit drv shell results packages;

  # Back-compat shape: the FIRST target's own result/package.
  result = results.${primaryTargetName};
  package = packages.${primaryTargetName};
}
