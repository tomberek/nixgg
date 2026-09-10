# Pinned toolchain for nixgg.
#
# Everything the shim drivers and nix/{builder,linker,archiver}.nix
# need is realised from THIS flake, not the ambient <nixpkgs>. That
# makes every CA derivation reproducible across machines and time —
# flake.lock is the single source of truth.
{
  description = "nixgg — gg-style build accelerator using Nix CA derivations.";

  # Auto-enable the experimental features mkNixggBuild needs. Users
  # still get prompted the first time they build (Nix asks before
  # trusting a flake's nixConfig), but after that
  # `nix build .#hello` / `.#lua` Just Works.
  nixConfig = {
    extra-experimental-features = [
      "ca-derivations"
      "dynamic-derivations"
      "configurable-impure-env"
    ];
    extra-system-features = [ "builder-rpc-v0" ];
  };

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  # The NixOS/nix master branch now contains the builder-rpc-v0 +
  # `nix store submit-output` work from PR #15793. Track master so
  # the resulting nix binary is substitutable from cache.nixos.org
  # instead of forcing a full local build.
  inputs.nix-15793 = {
    url = "github:NixOS/nix";
  };

  # Sources for the example builds. `flake = false` because these are
  # plain source trees, not nix flakes. Each is passed as `src` to
  # the matching examples/<name>/default.nix.
  inputs.lua-src = {
    url = "https://www.lua.org/ftp/lua-5.4.7.tar.gz";
    flake = false;
  };
  inputs.fmt-src = {
    url = "github:fmtlib/fmt/11.0.2";
    flake = false;
  };
  inputs.mosh-src = {
    url = "github:mobile-shell/mosh";
    flake = false;
  };
  inputs.redis-src = {
    url = "github:redis/redis/8.2.2";
    flake = false;
  };
  inputs.ffmpeg-src = {
    url = "https://ffmpeg.org/releases/ffmpeg-7.1.2.tar.xz";
    flake = false;
  };
  inputs.gcc-src = {
    # GCC 15.3.0 — matches this flake's own pinned nixpkgs gcc
    # (nixpkgs#gcc.cc is 15.3.0; see llvm-src's comment above). We
    # only build libiberty/ (see examples/gcc's docstring for why),
    # so the exact minor doesn't matter for correctness, but pinning
    # the same version as the ambient toolchain avoids a pointless
    # second GCC-version mental model.
    url = "https://ftp.gnu.org/gnu/gcc/gcc-15.3.0/gcc-15.3.0.tar.xz";
    flake = false;
  };
  inputs.llvm-src = {
    # LLVM monorepo checkout. We only build llvm/ (no clang/lld/etc.),
    # targeting X86 to keep the TU count and wall-clock reasonable while
    # still exercising a real C++ codebase through shims + drv graph.
    #
    # Why a git checkout of the monorepo rather than the per-component
    # release tarballs: llvm/CMakeLists.txt `include()`s from sibling
    # `cmake/` and `third-party/` directories, which the standalone
    # llvm-<v>.src.tar.xz doesn't carry. The monorepo has all three side
    # by side, so no reassembly step is needed.
    #
    # Why 19.x rather than 18.1.8: LLVM 18 predates GCC 15, which stopped
    # pulling in <cstdint> transitively via other libstdc++ headers.
    # 18.1.8's SmallVector.h uses uint64_t/uint32_t without including
    # <cstdint>, so every TU that reaches it fails with "'uint64_t' was
    # not declared in this scope" under our pinned gcc-15.3.0. Upstream
    # fixed this in llvm/llvm-project#101761, which shipped in 19 —
    # nixpkgs backports that same commit for release_version < 19. Pinning
    # 19.x gets the fix as released, no patch needed.
    url = "github:llvm/llvm-project/llvmorg-19.1.7";
    flake = false;
  };
  inputs.postgresql-src = {
    # PostgreSQL 17.2 — a much larger real-world autoconf project than
    # mosh/redis (src/backend alone is ~1000+ TUs). We only build
    # src/backend (the postgres server binary itself), not `make
    # world`/docs/contrib — same "deliberately smaller target" scoping
    # as examples/gcc's libiberty-only build. Confirmed directly: a
    # sequential (non -j) `make -C src/backend` builds cleanly and
    # produces a real, runnable `postgres --version`; `-j` on this
    # SAME subdir-only invocation hits a real recursive-make ordering
    # race in PostgreSQL's own Makefile.global (submake-generated-headers
    # isn't a dependency of every parallel sub-target the way a
    # top-level `make -j` run would see it) — "No rule to make target
    # '../../src/common/libpgcommon_srv.a'" — unrelated to nixgg;
    # sidestepped by running `make -C src/backend generated-headers`
    # first, then the real (still non -j, for the same reason) build.
    url = "https://ftp.postgresql.org/pub/source/v17.2/postgresql-17.2.tar.gz";
    flake = false;
  };
  inputs.qemu-src = {
    # QEMU 9.2.0 — meson+ninja, a build-system genre no other example
    # uses (plain Makefile/autotools/cmake+ninja are the existing
    # ones). See examples/qemu's own docstring for the deliberate
    # scope-down (--target-list=x86_64-softmmu, tools/tests/docs
    # disabled) and confirmation that QEMU's own build-time codegen
    # (QAPI, decodetree.py, tracetool.py) is pure find_program-driven
    # Python/shell, never a compiled-and-exec'd QEMU binary — so
    # unlike zstd/gcc, no phase-chaining fix is needed here.
    url = "https://download.qemu.org/qemu-9.2.0.tar.xz";
    flake = false;
  };

  inputs.linux-src = {
    # Linux 6.12 — the largest, most structurally demanding fixture in
    # this repo. See examples/linux-kernel's own docstring for the
    # deliberate scope-down (tinyconfig, vmlinux target only, no
    # modules), the real shim/expression gaps this fixture found and
    # fixed, and why it needed a two-phase mkNixggBuild split (plus a
    # plain stdenv.mkDerivation for phase 2) to make sandbox mode work
    # — both native and sandbox modes are verified end to end.
    url = "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.12.tar.xz";
    flake = false;
  };

  outputs =
    { self, nixpkgs, nix-15793, lua-src, fmt-src, mosh-src, redis-src, ffmpeg-src,
      gcc-src, llvm-src, postgresql-src, qemu-src, linux-src }:
    let
      forEachSystem = f: builtins.mapAttrs (system: pkgs: f system pkgs) nixpkgs.legacyPackages;
    in
    {
      packages = forEachSystem (
        system: pkgs:
        let
          toolchain = {
            gcc = pkgs.gcc;
            bash = pkgs.bash;
            coreutils = pkgs.coreutils;
            gnumake = pkgs.gnumake;
            nix = pkgs.nixVersions.stable;
          };

          # Nix built from PR 15793 (builder-rpc-v0 / submit-output).
          # Only used by `nixgg emit`'s .sandboxed variant. `patched-nix`
          # follows the same flake schema as upstream, so its default
          # package is the nix CLI.
          patchedNix =
            (nix-15793.packages.${system}.nix-cli
              or nix-15793.packages.${system}.default);

          # libstore-c / libutil-c from the SAME pinned nix-15793 build —
          # C API for talking to the daemon (nix_derivation_from_json,
          # nix_add_derivation, ...) without shelling out to the `nix`
          # CLI per call. nixpkgs' packaging splits every Nix component
          # into its own derivation with real dev/out/debug outputs
          # (confirmed: nix-store-c and nix-util-c are both directly
          # exposed flake outputs, no rebuild needed), unlike the
          # `nix`/`nix-cli` package above which bundles everything and
          # exposes no headers. Exists to let a future cgo-bound nixgg
          # binary link against libnixstorec.so/libnixutilc.so instead
          # of fork+exec'ing `nix derivation add`/`nix store add --scan`
          # per translation unit — see the per-invocation RPC-tax
          # analysis in ARCHITECTURE.md's "What we don't (yet) do".
          nixStoreC = nix-15793.packages.${system}.nix-store-c;
          nixUtilC = nix-15793.packages.${system}.nix-util-c;

          # The nix/ helper directory (builder.nix, linker.nix,
          # archiver.nix, pure-store-path.nix) imported into the store
          # once so drivers can `import` them by absolute store path
          # under pure-eval mode. We also generate toolchain.nix
          # alongside them with the pinned compiler/bash/coreutils
          # roots, so every thunk can `import ./toolchain.nix` instead
          # of duplicating those store paths inline.
          nixHelpers = pkgs.runCommand "nixgg-nix" { } ''
            cp -r ${./nix} $out
            chmod -R u+w $out
            cat > $out/toolchain.nix <<'EOF'
            # Toolchain paths pinned by nixgg's flake.lock. Every thunk
            # imports this file rather than duplicating the store paths
            # in its own body — shrinks thunks, and toolchain rev-bumps
            # only touch this file's content-hash.
            {
              compilerRoot  = "${toolchain.gcc}";
              bashRoot      = "${toolchain.bash}";
              coreutilsRoot = "${toolchain.coreutils}";
            }
            EOF
          '';

          toolchainJson = pkgs.writeTextFile {
            name = "nixgg-toolchain.json";
            text = builtins.toJSON {
              gcc = "${toolchain.gcc}";
              bash = "${toolchain.bash}";
              coreutils = "${toolchain.coreutils}";
              nix = "${toolchain.nix}";
              real_cc = "${toolchain.gcc}/bin/g++";
              nix_helpers = "${nixHelpers}";
              patched_nix = "${patchedNix}";
            };
          };

          # Bash-sourceable env block. `. $(nix build .#env-shell --print-out-paths)`
          # sets every NIXGG_* variable the driver needs, with no jq / python /
          # eval required by the consumer.
          envShell = pkgs.writeTextFile {
            name = "nixgg-env.sh";
            executable = false;
            text = ''
              # nixgg toolchain env, pinned by flake.lock. Source this file.
              export NIXGG_COMPILER_ROOT="${toolchain.gcc}"
              export NIXGG_BASH_ROOT="${toolchain.bash}"
              export NIXGG_COREUTILS_ROOT="${toolchain.coreutils}"
              export NIXGG_GNUMAKE_ROOT="${toolchain.gnumake}"
              export NIXGG_REAL_CC="${toolchain.gcc}/bin/g++"
              export NIXGG_NIX="${toolchain.nix}/bin/nix"
              export NIXGG_NIX_HELPERS="${nixHelpers}"
              # PR 15793's nix build. Only needed by `nixgg emit`
              # .sandboxed; every other subcommand ignores it.
              export NIXGG_PATCHED_NIX="${patchedNix}"
              # Store paths the driver may need to copy into an alt store:
              export NIXGG_TOOLCHAIN_PATHS="${toolchain.gcc} ${toolchain.bash} ${toolchain.coreutils} ${toolchain.gnumake} ${toolchain.nix} ${nixHelpers} ${patchedNix}"
            '';
          };

          moshEnv = pkgs.stdenv.mkDerivation {
            name = "nixgg-mosh-env";
            nativeBuildInputs = with pkgs; [
              autoconf automake libtool pkg-config perl gnumake protobuf which
            ];
            buildInputs = with pkgs; [ ncurses openssl zlib protobuf ];
            dontUnpack = true;
            installPhase = "mkdir -p $out";
          };

          fmtEnv = pkgs.stdenv.mkDerivation {
            name = "nixgg-fmt-env";
            nativeBuildInputs = with pkgs; [
              cmake ninja gnumake pkg-config which
            ];
            dontUnpack = true;
            installPhase = "mkdir -p $out";
          };

          # The nixgg Go binary + shims tree, built from THIS repo's
          # source. `mkNixggBuild` (below) pulls this in as a build
          # input so the sandboxed builder can invoke shims.
          #
          # Static (netgo + osusergo) so it works regardless of what
          # the sandbox mounts.
          nixggBin = pkgs.buildGoModule {
            pname = "nixgg";
            version = "0";
            # The Go tree lives under go/ so that editing anything else
            # in the repo — nix/*.nix, examples, docs — cannot change
            # this derivation's hash. It used to be ./. with an
            # exclusion filter, which meant every doc tweak rebuilt the
            # binary and moved every -shell drvPath with it.
            src = ./go;
            vendorHash = null;  # no deps
            doCheck = false;
            postInstall = ''
              mkdir -p $out/shims
              # The six canonical names, plus clang/clang++, ld's
              # personalities, and the host-triple-prefixed spellings a
              # configure script may pick. dispatch.FromArgv0 also
              # strips version suffixes (gcc-15) and triple prefixes,
              # but a shim only fires if a symlink with that exact name
              # is on PATH — a name we don't link is a tool nixgg
              # silently never accelerates.
              #
              # Not exhaustive by construction: the full cross product of
              # triples and versions is unbounded. These cover what the
              # examples and common autotools/cmake probes actually
              # invoke; add more if a real project needs them.
              # objtool is not a compiler: it is a per-object
              # rewriter, reached only when a caller points the build's
              # `objtool=` make variable here (nothing resolves it via
              # PATH). Linked anyway so that pointing at it works.
              for t in ar c++ cc g++ gcc ranlib clang clang++ ld ld.bfd ld.gold ld.lld objtool objcopy rustc; do
                ln -s ../bin/nixgg $out/shims/$t
              done
              for t in gcc g++ cc c++ ar ranlib ld; do
                ln -s ../bin/nixgg $out/shims/x86_64-unknown-linux-gnu-$t
                ln -s ../bin/nixgg $out/shims/x86_64-linux-gnu-$t
              done
            '';
          };

          # mkNixggBuild wraps a user command in a builder-rpc-v0
          # derivation whose output IS a .drv file — the "final link"
          # drv submitted from inside the sandbox. Consumers get the
          # compiled artifact via `builtins.outputOf drv.outPath "out"`.
          mkNixggBuild = import ./nix/mkNixggBuild.nix {
            inherit (pkgs) lib stdenv mkShell coreutils gnumake bash;
            gcc         = toolchain.gcc;
            nixgg       = nixggBin;
            nixHelpers  = nixHelpers;
            patchedNix  = patchedNix;
          };

          # splitStdenv wraps an EXISTING stdenv (nixpkgsFun's own, or
          # any package's `.stdenv`) so `pkgs.foo.override { stdenv =
          # splitStdenv { ...; splitAtBuild = true; }; }` runs foo's
          # configure/build (or just build, or just configure) as a
          # builder-rpc-v0 derivation depending on which of
          # splitAtConfigure/splitAtBuild is set, while leaving
          # whatever comes after untouched. See nix/splitStdenv.nix's
          # own top comment for the mechanism and README.md for usage.
          splitStdenv = import ./nix/splitStdenv.nix {
            inherit (pkgs) lib config stdenvNoCC;
            inherit (pkgs) bash coreutils gnumake;
            gcc         = toolchain.gcc;
            nixgg       = nixggBin;
            patchedNix  = patchedNix;
            nixpkgsPath = nixpkgs;
            inherit system;
          };

          # splitStdenv { splitAtBuild = true; } applied to real,
          # upstream nixpkgs packages — unmodified `pkgs.foo.override {
          # stdenv = ...; }`, no nixgg-specific package.nix anywhere.
          # Distinct names from the mkNixggBuild-based `hello`/`mosh`
          # examples above (which build nixgg's own example/ dir and a
          # hand-written mosh call, respectively) — these instead
          # prove the "upgrade an EXISTING nixpkgs derivation" story
          # from README.md, so they need to stay visibly separate
          # rather than shadow those.
          #
          # Three distinct build-system shapes, matching what was
          # verified directly while building this mechanism (see
          # nix/splitStdenv.nix's own top comment and README.md's
          # "Upgrade an existing nixpkgs package" section):
          #   - hello: plain autotools, doCheck + postInstallCheck.
          #   - mosh:  autotools + autoreconfHook (setup-hook-injected
          #            phase — the case that broke a naive hardcoded
          #            `phases` list).
          #   - zstd:  cmake, 4 outputs (out/bin/dev/man), a fully
          #            custom checkPhase running `ctest` — AND the
          #            "exec one of its own binaries mid-build" case
          #            (contrib/gen_html). Plain `pkgs.zstd.override {
          #            stdenv = splitStdenv { stdenv = pkgs.stdenv;
          #            splitAtBuild = true; }; }` fails with
          #            "./gen_html: Permission denied" — confirmed
          #            directly — because gen_html is itself an
          #            unresolved drvref stub at the moment zstd's own
          #            cmake graph tries to exec it, and no plain
          #            nixpkgs-level `.overrideAttrs` can reach the
          #            build stage to patch it (see
          #            examples/zstd-dyndrv/default.nix for exactly
          #            why). Fixed via splitStdenv's extraBuildAttrs
          #            escape hatch — a phase-chained mkNixggBuild call
          #            pre-builds gen_html, and extraBuildAttrs's
          #            postPatch points zstd's own cmake at that
          #            already-resolved binary instead of letting
          #            cmake build+exec its own.
          dynDrvExamples = {
            hello-dyndrv = pkgs.hello.override { stdenv = splitStdenv { stdenv = pkgs.stdenv; splitAtBuild = true; }; };
            mosh-dyndrv = pkgs.mosh.override { stdenv = splitStdenv { stdenv = pkgs.stdenv; splitAtBuild = true; }; };
            zstd-dyndrv = import ./examples/zstd-dyndrv {
              inherit pkgs mkNixggBuild splitStdenv;
            };
          };

          # splitStdenv { splitAtConfigure = true; } splits
          # stdenv.mkDerivation at the configure/build boundary, not
          # build/install like above — see nix/splitStdenv.nix's top
          # comment for the mechanism. hello covers autotools,
          # single-output; zstd covers cmake, multi-output
          # (out/bin/dev/man). Unlike zstd-dyndrv, zstd-cache needs no
          # extraBuildAttrs workaround for gen_html, since there's no
          # sandbox for it to trip over.
          configureSrcFilterPresets = import ./nix/configureSrcFilterPresets.nix;
          batchGroupPresets = import ./nix/batchGroupPresets.nix;
          configureCacheExamples = {
            hello-cache = pkgs.hello.override { stdenv = splitStdenv { stdenv = pkgs.stdenv; splitAtConfigure = true; }; };
            zstd-cache = pkgs.zstd.override { stdenv = splitStdenv { stdenv = pkgs.stdenv; splitAtConfigure = true; }; };
            # Same as hello-cache, but with configureSrcFilter: an
            # edit to a source file the autotools preset excludes
            # never touches configure's own input, so configure
            # doesn't rerun. existenceStubs needs "src/hello.c" —
            # that's hello's AC_CONFIG_SRCDIR argument, checked for
            # existence only by the generated configure script.
            hello-cache-filtered = pkgs.hello.override {
              stdenv = splitStdenv {
                stdenv = pkgs.stdenv;
                splitAtConfigure = true;
                configureSrcFilter = {
                  includePatterns = configureSrcFilterPresets.autotools;
                  existenceStubs = [ "src/hello.c" ];
                };
              };
            };
            # Same idea, cmake this time. zstd's own CMakeLists.txt
            # uses file(GLOB ...) to collect its library/test/contrib
            # sources, so filtering it can't preserve early-cutoff —
            # configure needs the whole globbed directory regardless.
            # fmt lists its sources explicitly, so filtering actually
            # means something here. The extra patterns beyond the
            # cmake preset are all real configure-time reads: fmt's
            # own CMakeLists.txt (main library sources/headers,
            # README.md/ChangeLog.md baked into add_library, the
            # fmt.pc.in/fmt-config.cmake.in templates) plus test/
            # wholesale, since BUILD_TESTING is on by default and
            # test/CMakeLists.txt enumerates a dozen targets that'd
            # otherwise need chasing one at a time.
            fmt-cache-filtered = pkgs.fmt.override {
              stdenv = splitStdenv {
                stdenv = pkgs.stdenv;
                splitAtConfigure = true;
                configureSrcFilter = {
                  includePatterns = configureSrcFilterPresets.cmake ++ [
                    "include/fmt/*.h"
                    "src/*.cc"
                    "README.md"
                    "ChangeLog.md"
                    "support/cmake/*.in"
                    "test"
                    "test/*"
                    "test/*/*"
                  ];
                };
              };
            };
          };

          # splitStdenv { splitAtConfigure = true; splitAtBuild =
          # true; } combines both tricks above: a configure-only stage
          # (optionally configureSrcFilter'd), a build-only sandboxed
          # stage restored on top of it, and an install-onward stage
          # unchanged. See nix/splitStdenv.nix's top comment for why
          # pulling configure out of the sandbox is sound (bypassed()
          # passthrough) and README.md for usage.
          dynDrvConfigureCacheExamples = {
            hello-dyndrv-configure-cached = pkgs.hello.override {
              stdenv = splitStdenv {
                stdenv = pkgs.stdenv;
                splitAtConfigure = true;
                splitAtBuild = true;
                configureSrcFilter = {
                  includePatterns = configureSrcFilterPresets.autotools;
                  existenceStubs = [ "src/hello.c" ];
                };
              };
            };
            # mosh through the combined mechanism: autotools +
            # autoreconfHook, the setup-hook-injected-phase case that
            # broke a naive hardcoded `phases` list in this
            # mechanism's own early history (see nix/splitStdenv.nix's
            # top comment). The configure stage here never hardcodes
            # `phases` (uses dontBuild/dontInstall/... toggles
            # instead), so autoreconfHook's `appendToVar
            # preConfigurePhases autoreconfPhase` still applies
            # normally. No configureSrcFilter — mosh isn't in the
            # verified preset set.
            mosh-dyndrv-configure-cached = pkgs.mosh.override {
              stdenv = splitStdenv { stdenv = pkgs.stdenv; splitAtConfigure = true; splitAtBuild = true; };
            };
            # zstd through the combined mechanism: multi-output
            # (out/bin/dev/man), a real ctest-based checkPhase, and
            # the same gen_html mid-build-exec problem zstd-dyndrv
            # documents above — but here the fix has to reach BOTH the
            # configure stage (where cmake's own Makefile generation
            # happens) and the build stage (which restores the
            # configure stage's tree and needs its own copy of the
            # patched CMakeLists.txt if it ever reconfigures anything
            # downstream). Patching only the build stage reproduces
            # the exact same "./gen_html: Permission denied" failure
            # as no patch at all — confirmed directly. Both stages
            # need the identical patch text, so this uses extraAttrs
            # (applies to every stage) rather than duplicating it into
            # two role-specific hatches. No configureSrcFilter here:
            # zstd's own CMakeLists.txt uses file(GLOB ...), so
            # filtering can't preserve early-cutoff for it (same
            # reasoning as zstd-cache above).
            zstd-dyndrv-configure-cached =
              let
                genHtml = mkNixggBuild {
                  pname = "zstd-gen-html";
                  version = "0";
                  src = pkgs.zstd.src;
                  targets = [ { name = "gen_html"; path = "gen_html"; } ];
                  buildCommand = ''
                    cd contrib/gen_html
                    g++ -O2 -c gen_html.cpp -o gen_html.o
                    g++ gen_html.o -o gen_html
                  '';
                };
                genHtmlPatch = ''
                  substituteInPlace build/cmake/contrib/gen_html/CMakeLists.txt \
                    --replace-fail \
                      'add_executable(gen_html ''${GENHTML_DIR}/gen_html.cpp)' \
                      "" \
                    --replace-fail \
                      'DEPENDS gen_html COMMENT "Update zstd manual")' \
                      'COMMENT "Update zstd manual")' \
                    --replace-fail \
                      'set(GENHTML_BINARY ''${PROJECT_BINARY_DIR}/gen_html''${CMAKE_EXECUTABLE_SUFFIX})' \
                      'set(GENHTML_BINARY ${genHtml.package}/bin/gen_html)'
                '';
              in
              pkgs.zstd.override {
                stdenv = splitStdenv {
                  stdenv = pkgs.stdenv;
                  splitAtConfigure = true;
                  splitAtBuild = true;
                  extraAttrs = finalAttrs: old: old // {
                    postPatch = old.postPatch + genHtmlPatch;
                  };
                };
              };
            # gdbm through the combined mechanism, WITH
            # configureSrcFilter: covers multi-output+filter together
            # (hello alone only covers single-output+filter, zstd
            # only multi-output+no-filter). gdbm is autotools, plain
            # configure (no autoreconfHook), and multi-output
            # (out/dev/info/lib/man) — its own AC_CONFIG_SRCDIR
            # argument is src/gdbmdefs.h.
            gdbm-dyndrv-configure-cached = pkgs.gdbm.override {
              stdenv = splitStdenv {
                stdenv = pkgs.stdenv;
                splitAtConfigure = true;
                splitAtBuild = true;
                configureSrcFilter = {
                  includePatterns = configureSrcFilterPresets.autotools;
                  existenceStubs = [ "src/gdbmdefs.h" ];
                };
              };
            };
          };

          # Concrete mkNixggBuild call sites, exposed as flake
          # packages so `nix build .#hello` / `.#lua` Just Work. Each
          # is the resolved final artifact — `builtins.outputOf`
          # applied to the outer text-mode drv — so consumers see a
          # normal store path, not a .drv.
          # `.#hello` builds the two-file project in nixgg/example/
          # through the sandbox / dyn-drv path. The exact same source
          # can also be built natively via `cd nixgg/example && make`
          # inside `nix develop /path/to/nixgg`; the resulting drvs
          # (compile drvs for main.o + util.o, link drv for hello)
          # are byte-identical between the two modes.
          # Every example is one entry here: the directory to import and
          # the args it needs beyond `mkNixggBuild`. Adding an example
          # used to mean editing three places (an import block, a
          # `foo = fooBuild.result;`, and a `foo-shell` in the output
          # set) — which is how two-phase ended up with no -shell attr.
          # Now it is one entry, and the -shell is generated.
          #
          # `nix build .#lua` builds the sandbox version; native
          # equivalence is pinned by tests/drv-equivalence.sh.

          # Shared nativeBuildInputs/src for mosh's/redis's plain and
          # -batch example variants — the -batch variant only adds a
          # batchGroups key, so keep the dependency list itself as a
          # single source of truth instead of repeating it twice.
          moshArgs = {
            inherit (pkgs)
              autoconf automake libtool pkg-config perl protobuf which
              gnum4 gnugrep gnused gawk file
              ncurses openssl zlib abseil-cpp;
            src = mosh-src;
          };
          redisArgs = {
            inherit (pkgs) which pkg-config python3 lua gnugrep gnused gawk;
            src = redis-src;
          };
          exampleDefs = {
            # hello lives in dyn-drv/ rather than examples/: it is the
            # in-tree smoke fixture, built from nixgg/example/.
            hello = {
              dir = ./dyn-drv/hello-mkbuild.nix;
              args = { inherit (pkgs) lib; };
            };
            lua = {
              dir = ./examples/lua;
              args = { src = lua-src; };
            };
            # Same fixture, batchGroups matching every lua source file
            # — the whole ~30-TU archive (liblua.a) becomes one
            # combined batch derivation instead of 30 compiles + 1
            # archive. Small and fast: tests/batch-drv-equivalence.sh's
            # own dedicated native/sandbox parity check for the
            # batch-archive shape itself, distinct from
            # redis-batch's larger, partial-match/negative-path
            # coverage.
            lua-batch = {
              dir = ./examples/lua;
              args = {
                src = lua-src;
                batchGroups = [ { name = "lua"; patterns = [ "src/**/*.c" ]; } ];
              };
            };
            fmt = {
              dir = ./examples/fmt;
              args = { inherit (pkgs) cmake ninja pkg-config; src = fmt-src; };
            };
            # Same fixture, batchGroups matching every fmt source file
            # — a real test of a documented limitation: fmt's own
            # target IS the archive (libfmt.a), so tryBatchArchive
            # refuses to batch it (see examples/fmt/default.nix's own
            # comment). Expected to build correctly with batching
            # never actually engaging.
            fmt-batch = {
              dir = ./examples/fmt;
              args = {
                inherit (pkgs) cmake ninja pkg-config;
                src = fmt-src;
                batchGroups = [ { name = "fmt"; patterns = [ "src/*.cc" ]; } ];
              };
            };
            mosh = {
              dir = ./examples/mosh;
              args = moshArgs;
            };
            # Same fixture, batchGroups covering every one of mosh's 6
            # lib*.a archives (crypto/network/terminal/util/
            # statesync/protobufs) at once — mosh-server itself is
            # the LINK target, not any one archive, so unlike
            # fmt-batch/gcc-batch none of these archives collide with
            # NIXGG_SANDBOX_TARGET; batching should actually engage
            # for all 6.
            mosh-batch = {
              dir = ./examples/mosh;
              args = moshArgs // {
                batchGroups = [ { name = "mosh"; patterns = [ "src/*/*.cc" ]; } ];
              };
            };
            redis = {
              dir = ./examples/redis;
              args = redisArgs;
            };
            # Same fixture, batchGroups = vendorDeps preset — a real
            # multi-directory build confirming internal/batch's
            # classification reaches the shim and matches redis's own
            # deps/{hiredis,linenoise,lua,jemalloc,hdr_histogram,
            # fpconv,fast_float}/ tree correctly. No longer prototype
            # scope: batching now really engages here — 5 of deps/'s 7
            # subtrees are reachable from redis-server at all
            # (jemalloc needs MALLOC=jemalloc, unset here; linenoise
            # is redis-cli-only), and all 5 batch cleanly: ~45
            # individual tu-*.o.drv + 5 ar-*.a.drv collapse into 5
            # batch-lib*.a.drv, a 158->113 total-drv reduction
            # (verified directly via
            # .claude/skills/nixgg-drv-graph-breakdown). jemalloc and
            # linenoise are out of scope, not missed coverage:
            # jemalloc's own build is a separate autotools
            # ./configure + make; linenoise's Makefile links
            # linenoise.o directly as a single object and never
            # archives it, so it can never be a batch target
            # regardless of MALLOC. See examples/redis/default.nix's
            # own batchGroups docstring.
            redis-batch = {
              dir = ./examples/redis;
              args = redisArgs // {
                batchGroups = [
                  { name = "vendor"; patterns = batchGroupPresets.vendorDeps; }
                ];
              };
            };
            ffmpeg = {
              dir = ./examples/ffmpeg;
              args = {
                inherit (pkgs) pkg-config perl nasm yasm gnumake which;
                src = ffmpeg-src;
              };
            };
            # Same fixture, batchGroups covering the 4 of ffmpeg's 8
            # per-directory static libs that batch both cleanly AND
            # correctly — the largest TU count in the repo (~1200),
            # the real test of whether batching's win scales, not
            # just whether it engages at all (that's mosh-batch's job,
            # at 6 archives/~30 TUs).
            #
            # "**" is required, not "*": libavcodec/h264/,
            # libavcodec/hevc/, libavfilter/dnn/, libavutil/tests/, and
            # libswscale/tests/ are all real subdirectories whose
            # objects feed the same top-level archive — a single-star
            # pattern misses them, and since batching requires EVERY
            # object in an archive to match the same group
            # (go/internal/shim/batcharchive.go's
            # collectSameGroupMembers), missing even one silently
            # falls the whole archive back to per-TU (confirmed
            # directly: all 5 non-trivial libs failed to batch with a
            # single-star pattern, and batched cleanly once fixed).
            #
            # libavutil and libswscale were ALSO excluded here for a
            # while: both have two source files sharing a basename in
            # different subdirectories (libavutil/cpu.c +
            # libavutil/x86/cpu.c; libswscale/swscale.c +
            # libswscale/x86/swscale.c, same for
            # rgb2rgb.c/yuv2rgb.c) — batching used to write every
            # member's object into one shared $objroot keyed by
            # basename ALONE, so the second compile silently
            # overwrote the first's .o before `ar` packaged it.
            # Confirmed directly at the time: batching libavutil
            # produced a REAL LINK FAILURE ("undefined reference to
            # `av_cpu_count'"/`av_get_cpu_flags'`, symbols only
            # libavutil/x86/cpu.c defines); libswscale's own batch
            # archive "succeeded" but `ar t` showed
            # swscale.o/rgb2rgb.o/yuv2rgb.o each listed twice — plain,
            # non-x86 implementations silently dropped. Fixed by
            # go/internal/shim/batcharchive.go's
            # disambiguateOutNames — colliding members now get a
            # deterministic "-2"/"-3"/... suffix before the extension
            # (cpu.o, cpu-2.o, ...) instead of clobbering each other.
            #
            # libavcodec/libavformat/libavfilter were ALSO excluded for
            # a while: each one's combined batch script is 400KB-1MB
            # (350-550+ TUs' worth of staged source paths in one
            # `bash -c` argument), blowing straight through the
            # MAX_ARG_STRLEN = 131072-byte ceiling ARCHITECTURE.md's
            # "What we don't (yet) do" already documented for
            # link/archive lines — confirmed directly, failed at BUILD
            # time with "Argument list too long". Fixed the same way
            # assemble.Build already fixed the identical problem for
            # openssl's own tree-restore script: the combined script
            # now goes through Env["batchScript"] + passAsFile instead
            # of Args (go/internal/expr/batcharchive.go's
            # BatchArchiveJSON, nix/batchArchiver.nix), so Args stays a
            # short, fixed string regardless of member count. Verified
            # directly: libavcodec's own batch script is 1014079 bytes
            # and now builds successfully.
            #
            # All 7 archives that actually exist in this build now
            # batch (libpostproc is absent because of this build's own
            # --disable-* configure flags, unrelated to batching) —
            # verified: full build succeeds, `nix run .#ffmpeg-batch --
            # -version` prints the real banner.
            #
            # Formerly a known real cost, now fixed: batching used to
            # serialize every member's own compile into ONE
            # derivation's ONE builder process with no parallelism at
            # all — confirmed directly via `ps aux` while building
            # libavcodec's own batch: exactly one gcc/cc1 process
            # running at a time, one core, for the whole ~350-TU
            # batch's duration. batchArchiveScript
            # (go/internal/expr/batcharchive.go, mirrored byte-
            # identically in nix/batchArchiver.nix) now backgrounds
            # each member's compile and bounds concurrency at
            # $NIX_BUILD_CORES via a FIFO `wait "$pid"` job runner —
            # confirmed directly via `ps aux` while building
            # mosh-batch: up to 18 concurrent compiler processes
            # observed. See that file's own docstring for why FIFO
            # wait, not `wait -n`.
            ffmpeg-batch = {
              dir = ./examples/ffmpeg;
              args = {
                inherit (pkgs) pkg-config perl nasm yasm gnumake which;
                src = ffmpeg-src;
                batchGroups = [
                  {
                    name = "ffmpeg";
                    patterns = [
                      "libavdevice/**/*.c"
                      "libswresample/**/*.c"
                      "libswscale/**/*.c"
                      "libavutil/**/*.c"
                      "libavcodec/**/*.c"
                      "libavformat/**/*.c"
                      "libavfilter/**/*.c"
                    ];
                  }
                ];
              };
            };
            # GCC's own libiberty/ subdir, built via ITS standalone
            # shipped `./configure` — not gcc's top-level multi-package
            # configure.ac. See examples/gcc's docstring for why this
            # (deliberately smaller) target sidesteps every GCC-specific
            # hazard (GMP/MPFR/MPC, LTO --plugin ar/ranlib decoration,
            # thin archives, mid-build gengtype/genmodes exec) that a
            # full cc1/cc1plus build would hit.
            gcc = {
              dir = ./examples/gcc;
              args = { src = gcc-src; };
            };
            # Same fixture, batchGroups matching every libiberty/*.c
            # file — same target-is-the-archive limitation as
            # fmt-batch (libiberty.a is this build's own submission
            # target), on a larger (~65-member) archive. Expected to
            # build correctly with batching never actually engaging.
            gcc-batch = {
              dir = ./examples/gcc;
              args = {
                src = gcc-src;
                batchGroups = [ { name = "gcc"; patterns = [ "libiberty/*.c" ]; } ];
              };
            };
            # PostgreSQL's own src/backend — the real postgres server
            # binary, ~1000+ TUs, built via its standalone
            # ./configure && make -C src/backend. See
            # examples/postgresql's own docstring for the deliberate
            # scope-down (no `make world`, no docs/contrib/libpq) and
            # why this fixture never uses `make -j` (a real,
            # nixgg-unrelated recursive-make ordering race in
            # PostgreSQL's own Makefile.global).
            postgresql = {
              dir = ./examples/postgresql;
              args = { inherit (pkgs) bison flex perl; src = postgresql-src; };
            };
            # QEMU's own x86_64-softmmu target — meson+ninja, the one
            # build-system genre no other example exercises. See
            # examples/qemu's own docstring for the deliberate
            # scope-down (one target, tools/tests/docs disabled) and
            # confirmation that its own build-time codegen never execs
            # a compiled QEMU binary (pure find_program-driven Python/
            # shell), so no phase-chaining fix is needed here unlike
            # zstd/gcc.
            qemu = {
              dir = ./examples/qemu;
              args = {
                inherit (pkgs) pkg-config meson ninja glib pixman ncurses zlib;
                pythonWithMesonDeps = pkgs.python3.withPackages (ps: [ ps.distlib ps.setuptools ]);
                src = qemu-src;
              };
            };
            # Same fixture, batchGroups covering libqemuutil.a's own
            # 450 members (util/, stubs/, qobject/, qapi/, crypto/,
            # trace/, plus meson's generated build/qapi/ and
            # build/trace/ QAPI code) — the one archive, of QEMU's 5,
            # that's actually worth batching (verified directly via
            # .claude/skills/nixgg-drv-graph-breakdown's own drv-graph
            # walk: libvhost-user.a/libvhost-user-glib.a/libvduse.a
            # each have exactly 1 member, not worth it; the final
            # link itself and libcommon.a/libqemu-x86_64-softmmu.a
            # never go through `ar` at all — meson's own "extract
            # objects" optimization passes their .o files to the link
            # line directly, so there's no archive step there to
            # batch in the first place).
            #
            # QEMU's own internal static libs are ALL built with
            # `ar csrDT` (THIN) — meson applies the same link_args
            # uniformly, so there's no way to pick "safe" (non-thin)
            # patterns here the way every earlier *-batch entry could.
            # This is the fixture that first exercised go/internal/
            # expr/batcharchive.go's own thin-archive support (see
            # batchArchiveScript's own docstring): a thin batch
            # archive writes its member objects into $out/lib/
            # .nixgg-objs/ (this derivation's own permanent store
            # output) rather than a build-tmp scratch dir, so the
            # resulting archive's self-references survive after the
            # batch derivation's own build sandbox is torn down —
            # confirmed via a real end-to-end test
            # (TestBatchArchiveScriptThinArchiveSurvivesObjectDeletion)
            # before this entry existed.
            qemu-batch = {
              dir = ./examples/qemu;
              args = {
                inherit (pkgs) pkg-config meson ninja glib pixman ncurses zlib;
                pythonWithMesonDeps = pkgs.python3.withPackages (ps: [ ps.distlib ps.setuptools ]);
                src = qemu-src;
                batchGroups = [
                  {
                    name = "qemuutil";
                    patterns = [
                      "util/*.c"
                      "stubs/*.c"
                      "qobject/*.c"
                      "qapi/*.c"
                      "crypto/*.c"
                      "trace/*.c"
                      "build/qapi/*.c"
                      "build/trace/*.c"
                    ];
                  }
                ];
              };
            };
            # Two-phase mkNixggBuild split — see examples/linux-kernel's
            # own docstring for why a single sandbox derivation can't
            # satisfy Kbuild's synchronous read-back-after-produce
            # shape, and why phase 2 is a plain stdenv.mkDerivation
            # rather than another mkNixggBuild call.
            linux-kernel = {
              dir = ./examples/linux-kernel;
              args = { inherit (pkgs) stdenv flex bison elfutils pkg-config bc; src = linux-src; };
            };
            # Out-of-tree kernel module — the cheap kbuild probe that
            # stands in for the NixOS kernel. See its docstring for
            # what it does and does not cover. Not in smoke.sh's QUICK
            # set until it passes.
            kmod = {
              dir = ./examples/kmod;
              args = {
                inherit (pkgs) kmod stdenv;
                kernel = pkgs.linuxPackages.kernel;
                src = ./examples/kmod/mod;
              };
            };
            # Two sources, no single `src`: phase 1 builds the codegen
            # tool, phase 2 execs it mid-build. Smoke test for the
            # phase-chaining pattern examples/llvm relies on.
            # Two Rust crates built by make and bare rustc — the shape
            # kbuild uses, and the only one the rustc shim models. See
            # its docstring for what it covers that nothing else does.
            rustc = {
              dir = ./examples/rustc;
              args = {
                inherit (pkgs) rustc;
                src = ./examples/rustc;
              };
            };
            two-phase = {
              dir = ./examples/two-phase;
              args = {
                codegenSrc = ./examples/two-phase/codegen;
                appSrc = ./examples/two-phase/app;
              };
            };
            # Minimal reproduction of the ar --thin mechanism QEMU's
            # meson build exercises at scale (see examples/thin-archive's
            # own docstring and go/internal/members' package docstring).
            # tests/thin-archive-equivalence.sh is this fixture's own
            # dedicated native/sandbox byte-identity check.
            thin-archive = {
              dir = ./examples/thin-archive;
              args = { inherit (pkgs) lib; };
            };
            # llvm-src is a monorepo checkout, which already has llvm/,
            # cmake/, and third-party/ side by side — exactly the layout
            # llvm/CMakeLists.txt's `include()`s expect. No reassembly.
            llvm = {
              dir = ./examples/llvm;
              args = {
                inherit (pkgs) runCommand cmake ninja pkg-config python3 perl which
                  libffi libxml2 ncurses zlib;
                src = llvm-src;
              };
            };
            # Same fixture, batchGroups covering every libLLVM<Name>.a
            # archive's own subdirectory (llvm/lib/Support/,
            # llvm/lib/TableGen/, llvm/lib/IR/, llvm/lib/MC/, etc.) —
            # the same per-subsystem-archive shape ffmpeg's per-codec
            # libavcodec.a/libavutil.a/etc. already exercises, at
            # LLVM's own larger scale (~2000 TUs across all 3 phases).
            # Verified via .claude/skills/nixgg-drv-graph-breakdown
            # before wiring the real patterns list here: phase1
            # (llvm-min-tblgen) links exactly 3 archives —
            # libLLVMDemangle.a, libLLVMSupport.a (142 TUs),
            # libLLVMTableGen.a (12 TUs) — none of which are phase1's
            # own link target, so no NIXGG_SANDBOX_TARGET collision
            # risk the way fmt-batch/gcc-batch's archive-is-the-target
            # case has.
            #
            # libLLVMSupport was ALSO excluded here for a while: even
            # after fixing an earlier "Support/**/*.cpp alone silently
            # falls the WHOLE archive back to per-TU" bug (Support/
            # mixes in plain .c/.S sources — regcomp.c/regexec.c's BSD
            # regex port, rpmalloc/, and the BLAKE3/ hash
            # implementation's per-arch .c/.S variants — and
            # collectSameGroupMembers,
            # go/internal/shim/batcharchive.go, requires EVERY input to
            # ar's own invocation to be a same-group pending member),
            # Support's own combined batch script is 152853 bytes —
            # over the MAX_ARG_STRLEN = 131072-byte ceiling this
            # repo's own ffmpeg-batch entry already hit at a similar TU
            # count. Confirmed directly at the time: batching it
            # produced "error: executing '.../bash': Argument list too
            # long" at BUILD time.
            #
            # Fixed the same way assemble.Build already fixed the
            # identical problem for openssl's own tree-restore script:
            # the combined script now goes through Env["batchScript"] +
            # passAsFile instead of Args
            # (go/internal/expr/batcharchive.go's BatchArchiveJSON,
            # nix/batchArchiver.nix), so Args stays a short, fixed
            # string regardless of member count. Verified directly:
            # full build succeeds, `nix run .#llvm-min-tblgen-batch --
            # --version` prints the real LLVM banner, phase1 goes from
            # 186 to 13 total derivations with all 3 archives batched.
            #
            # Same formerly-known cost as ffmpeg-batch, now fixed the
            # same way: Support's own ~142 TUs now compile with bounded
            # concurrency (capped at $NIX_BUILD_CORES) instead of one
            # at a time — see batchArchiveScript's own docstring
            # (go/internal/expr/batcharchive.go).
            llvm-batch = {
              dir = ./examples/llvm;
              args = {
                inherit (pkgs) runCommand cmake ninja pkg-config python3 perl which
                  libffi libxml2 ncurses zlib;
                src = llvm-src;
                batchGroups = [
                  {
                    name = "llvm";
                    patterns = [
                      "llvm/lib/Demangle/**/*.cpp"
                      "llvm/lib/TableGen/**/*.cpp"
                      "llvm/lib/Support/**/*.cpp"
                      "llvm/lib/Support/**/*.c"
                      "llvm/lib/Support/**/*.S"
                    ];
                  }
                ];
              };
            };
          };

          # name -> the example's full attrset (.result, .shell, extras).
          examples = builtins.mapAttrs
            (_: def: import def.dir ({ inherit mkNixggBuild; } // def.args))
            exampleDefs;

          # .#<name> is a real derivation (mkNixggBuild's `.package`) so
          # `nix run` / `nix profile install` / flake-check all work the
          # way they do for any other Nix package. `.#<name>-shell` is
          # the mkShell mirroring the sandbox env, which
          # tests/drv-equivalence.sh uses to replay the build natively.
          #
          # Both test scripts only care that `--print-out-paths` succeeds
          # and that the compile/link/archive drvs land in the store —
          # neither inspects the top-level attr's type, so switching this
          # from `.result` (a string) to `.package` (a derivation) needs
          # no change on their side. `.result` is still reachable via
          # `.#<name>.result` for anyone who was depending on the string
          # shape directly.
          exampleResults = builtins.mapAttrs (_: e: e.package) examples;
          exampleShells = pkgs.lib.mapAttrs' (n: e: pkgs.lib.nameValuePair "${n}-shell" e.shell) examples;

          # `.#<name>-shared` is the same example definition with
          # sharedStaging flipped on — staged source trees become symlink
          # farms into per-file store objects instead of per-TU copies.
          #
          # Generated rather than hand-written so these cannot drift from
          # the definitions above, and wrapped at the mkNixggBuild seam so
          # each example keeps its own args untouched.
          #
          # These exist for tests/shared-closure.sh, which has to build for
          # real: the property it checks (that `nix store add --scan`
          # records symlink targets as references) is unobservable outside
          # a recursive-nix builder. They are ordinary derivations, so
          # they are also the way to try shared staging on any example.
          sharedExamples = pkgs.lib.mapAttrs'
            (n: def: pkgs.lib.nameValuePair "${n}-shared"
              (import def.dir ({
                mkNixggBuild = args: mkNixggBuild (args // { sharedStaging = true; });
              } // def.args)).package)
            exampleDefs;
        in
        toolchain
        // exampleResults   # .#hello .#lua .#fmt .#mosh .#redis .#ffmpeg .#gcc .#two-phase .#llvm
        // exampleShells    # .#<name>-shell for each of the above
        // sharedExamples   # .#<name>-shared: same, with sharedStaging on
        // dynDrvExamples   # .#hello-dyndrv .#mosh-dyndrv .#zstd-dyndrv
        // configureCacheExamples   # .#hello-cache .#zstd-cache .#hello-cache-filtered .#fmt-cache-filtered
        // dynDrvConfigureCacheExamples   # .#hello-dyndrv-configure-cached .#mosh-dyndrv-configure-cached .#zstd-dyndrv-configure-cached .#gdbm-dyndrv-configure-cached
        // {
          # Extras an individual example exposes beyond .result/.shell.
          # llvm's two tblgen phases are separately buildable so the
          # chain can be smoke-tested a phase at a time.
          # Derivations (not outputOf strings) for the same reason
          # exampleResults above uses .package: `nix build`/`nix run`
          # these directly for isolated phase smoke-testing.
          llvm-min-tblgen = examples.llvm.llvm-min-tblgen.package;
          llvm-tblgen = examples.llvm.llvm-tblgen.package;
          llvm-min-tblgen-batch = examples.llvm-batch.llvm-min-tblgen.package;
          llvm-tblgen-batch = examples.llvm-batch.llvm-tblgen.package;
          two-phase-codegen = examples.two-phase.codegen.package;
          # mosh's second multi-target output (mosh's default .package
          # is mosh-server, the first entry in examples/mosh/
          # default.nix's own targets list) — see mkNixggBuild.nix's
          # own `packages` attrset for the general mechanism any
          # multi-target build's non-primary targets are reachable
          # through.
          mosh-client = examples.mosh.packages.mosh-client;

          toolchain-json = toolchainJson;
          env-shell = envShell;
          mosh-env = moshEnv;
          fmt-env = fmtEnv;
          patched-nix = patchedNix;
          nix-store-c = nixStoreC;
          nix-util-c = nixUtilC;
          nixgg-bin = nixggBin;
          # mkNixggBuild is a function; expose so consumers can build
          # their own targets in downstream flakes.
          inherit mkNixggBuild;
          # splitStdenv is likewise a function (stdenv -> stdenv);
          # expose so `pkgs.foo.override { stdenv =
          # nixgg.packages.${system}.splitStdenv { splitAtBuild = true; };
          # }` works from any downstream flake, no vendoring required.
          inherit splitStdenv;
          inherit configureSrcFilterPresets;
          default = envShell;
        }
      );

      devShells = forEachSystem (system: pkgs:
        let
          pkgs' = self.packages.${system};

          # Shell that has `nixgg` on PATH plus the shims/ dir prefixed
          # ahead of the toolchain — so `cc foo.c -o foo` picks up
          # nixgg's shim, not the raw compiler.
          #
          # We deliberately don't put `gcc` in packages: pulling in
          # the cc-wrapper's setup hook injects NIX_CFLAGS_COMPILE /
          # NIX_LDFLAGS with -frandom-seed and workspace-relative
          # -rpath /…/outputs/out/lib, which would end up in every
          # thunk's wrapperEnv and (a) break CA hash stability across
          # shell entries because -frandom-seed is per-invocation,
          # (b) point at a path that doesn't exist. The nixgg shim
          # resolves the real compiler via NIXGG_COMPILER_ROOT
          # instead — no wrapper env needed.
          nixggShell = pkgs.mkShellNoCC {
            name = "nixgg-shell";
            packages = [
              pkgs'.nixgg-bin
              pkgs.gnumake
              pkgs.coreutils
              pkgs.bash
            ];
            shellHook = ''
              # Source the pinned NIXGG_* env plus PATH prefix.
              # env-shell is the same block `nixgg env` prints;
              # sourcing it directly avoids a fork-and-eval per shell
              # entry.
              . ${pkgs'.env-shell}

              # Alt store default (env-shell doesn't set this — it's
              # a per-user preference). Overridable.
              : "''${NIXGG_STORE:=local?root=/tmp/nixgg-store}"
              export NIXGG_STORE

              # Belt-and-braces: even without gcc in packages, some
              # setup hooks leak NIX_CFLAGS_COMPILE / NIX_LDFLAGS
              # with a -frandom-seed and an rpath pointing at
              # $out/lib (mkShellNoCC still tries to synthesise an
              # $out for its own installPhase). Wipe them so
              # wrapperenv doesn't bake per-shell-entry noise into
              # every thunk. The shim's derivations run under Nix's
              # own gcc-wrapper anyway; anything a caller actually
              # needs to set should go into a project-local shellHook.
              unset NIX_CFLAGS_COMPILE NIX_CFLAGS_LINK NIX_LDFLAGS

              # Prepend shims/ so `cc`, `c++`, `ar` etc. dispatch to
              # the nixgg shim binary. bin/ first so `nixgg` itself
              # is also on PATH.
              export PATH="${pkgs'.nixgg-bin}/bin:${pkgs'.nixgg-bin}/shims:$PATH"

              # Opinionated default: link shim inline-realises, so
              # plain `make` produces real binaries. Set to 0 to opt
              # out.
              : "''${NIXGG_AUTOFORCE:=1}"
              export NIXGG_AUTOFORCE

              # Sandbox mode's own `nix` (below) must be the patched
              # build, not whatever's on the ambient PATH — an
              # unpatched Nix fails opaquely (`attribute 'outputOf'
              # missing` at eval, or "Submit outputs for a currently
              # running derivation not supported by store 'local'"
              # mid-build if outputOf itself resolves but
              # submit-output doesn't exist). Put it first on PATH so
              # every `nix build .#hello`-style invocation in this
              # shell automatically uses it — see README.md's
              # "Invoking sandbox mode explicitly" for what each piece
              # below is for.
              echo "nixgg shell: prepending patched Nix and pointing NIX_CONFIG at an alt store — see README.md's 'Invoking sandbox mode explicitly'" >&2
              export PATH="${pkgs'."patched-nix"}/bin:$PATH"
              export NIX_CONFIG="
              extra-experimental-features = ca-derivations dynamic-derivations configurable-impure-env
              extra-system-features = builder-rpc-v0
              store = ''${NIXGG_STORE}
              "

              echo "nixgg shell — 'nixgg --help', 'cc -v', 'which make', 'nix build .#hello'" >&2
            '';
          };
        in
        {
          default = nixggShell;
          # Keep the toolchain-dev shells reachable under explicit names.
          mosh = pkgs'.mosh-env;
          fmt  = pkgs'.fmt-env;
        });

      apps = forEachSystem (system: pkgs:
        let bin = "${self.packages.${system}.nixgg-bin}/bin/nixgg"; in
        {
          nixgg = { type = "app"; program = bin; };
          default = { type = "app"; program = bin; };
        });
    };
}

# control-probe
