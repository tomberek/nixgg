# Everything the shim drivers and nix/{builder,linker,archiver}.nix need is
# realised from THIS flake, not the ambient <nixpkgs>, so flake.lock is the
# single source of truth for reproducibility.
{
  description = "nixgg — gg-style build accelerator using Nix CA derivations.";

  # Auto-enables the experimental features mkNixggBuild needs (users still
  # get prompted once, to trust the flake's nixConfig).
  nixConfig = {
    extra-experimental-features = [
      "ca-derivations"
      "dynamic-derivations"
      "configurable-impure-env"
    ];
    extra-system-features = [ "builder-rpc-v0" ];
  };

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  # Tracks NixOS/nix master for the builder-rpc-v0 / `nix store submit-output`
  # work (PR #15793), so the binary is substitutable from cache.nixos.org.
  inputs.nix-15793 = {
    url = "github:NixOS/nix";
  };

  # `flake = false`: plain source trees, passed as `src` to the matching
  # examples/<name>/default.nix.
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
    # Matches this flake's own pinned nixpkgs gcc (15.3.0). We only build
    # libiberty/, so the exact minor doesn't matter, but pinning the same
    # version avoids a second GCC mental model.
    url = "https://ftp.gnu.org/gnu/gcc/gcc-15.3.0/gcc-15.3.0.tar.xz";
    flake = false;
  };
  inputs.llvm-src = {
    # Monorepo checkout, not a release tarball: llvm/CMakeLists.txt
    # include()s from sibling cmake/ and third-party/, which the
    # standalone llvm-<v>.src.tar.xz doesn't carry.
    #
    # 19.x not 18.1.8: GCC 15 stopped pulling in <cstdint> transitively,
    # and 18.1.8's SmallVector.h relies on that (uint64_t/uint32_t without
    # an explicit include) — fails to compile under gcc-15.3.0. Fixed
    # upstream in llvm/llvm-project#101761, released in 19.
    url = "github:llvm/llvm-project/llvmorg-19.1.7";
    flake = false;
  };
  inputs.postgresql-src = {
    # We only build src/backend (not `make world`/docs/contrib). `-j` on
    # this subdir-only invocation hits a real recursive-make ordering race
    # in PostgreSQL's own Makefile.global (unrelated to nixgg) — sidestep
    # by running `make -C src/backend generated-headers` first, then a
    # non-`-j` build.
    url = "https://ftp.postgresql.org/pub/source/v17.2/postgresql-17.2.tar.gz";
    flake = false;
  };
  inputs.qemu-src = {
    url = "https://download.qemu.org/qemu-9.2.0.tar.xz";
    flake = false;
  };

  inputs.linux-src = {
    # See examples/linux-kernel for the scope-down (tinyconfig, vmlinux
    # only) and why it needs a two-phase mkNixggBuild split.
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

          # Nix built from PR 15793 (builder-rpc-v0 / submit-output), used
          # only by `nixgg emit`'s .sandboxed variant.
          patchedNix =
            (nix-15793.packages.${system}.nix-cli
              or nix-15793.packages.${system}.default);

          # libstore-c / libutil-c from the same pinned nix-15793 build —
          # C API (nix_derivation_from_json, nix_add_derivation, ...) for a
          # future cgo-bound nixgg to link against instead of fork+exec'ing
          # the `nix` CLI per translation unit. See ARCHITECTURE.md's
          # "What we don't (yet) do" for the per-invocation RPC-tax analysis.
          nixStoreC = nix-15793.packages.${system}.nix-store-c;
          nixUtilC = nix-15793.packages.${system}.nix-util-c;

          # examples/nix's buildInputs: nix-15793's own devShell set,
          # unfiltered, rather than this flake's own `pkgs` — avoids an
          # ABI/version mismatch between two different nixpkgs pins.
          # Unfiltered because configuring the whole top-level meson.build
          # resolves every subproject's `dependency()` calls, even though
          # only two ninja targets (libutil, libstore) actually get built.
          nixBuildInputs =
            let
              shell = nix-15793.devShells.${system}.default;
            in
            (shell.buildInputs or [ ]) ++ (shell.propagatedBuildInputs or [ ]);

          # nix/ (builder.nix, linker.nix, archiver.nix, pure-store-path.nix)
          # imported into the store once so drivers can `import` it by
          # absolute store path under pure-eval mode. toolchain.nix is
          # generated alongside with the pinned compiler/bash/coreutils
          # roots, so thunks import it instead of duplicating store paths.
          nixHelpers = pkgs.runCommand "nixgg-nix" { } ''
            cp -r ${./nix} $out
            chmod -R u+w $out
            cat > $out/toolchain.nix <<'EOF'
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

          # Bash-sourceable env block: `. $(nix build .#env-shell --print-out-paths)`
          # sets every NIXGG_* variable the driver needs.
          envShell = pkgs.writeTextFile {
            name = "nixgg-env.sh";
            executable = false;
            text = ''
              export NIXGG_COMPILER_ROOT="${toolchain.gcc}"
              export NIXGG_BASH_ROOT="${toolchain.bash}"
              export NIXGG_COREUTILS_ROOT="${toolchain.coreutils}"
              export NIXGG_GNUMAKE_ROOT="${toolchain.gnumake}"
              export NIXGG_REAL_CC="${toolchain.gcc}/bin/g++"
              export NIXGG_NIX="${toolchain.nix}/bin/nix"
              export NIXGG_NIX_HELPERS="${nixHelpers}"
              # Only needed by `nixgg emit .sandboxed`.
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
            # go/ is its own src so editing anything else in the repo
            # (nix/*.nix, examples, docs) can't change this derivation's
            # hash and move every -shell drvPath with it.
            src = ./go;
            vendorHash = null;  # no deps
            doCheck = false;
            postInstall = ''
              mkdir -p $out/shims
              # The canonical names plus clang/clang++, ld's personalities,
              # and host-triple-prefixed spellings a configure script may
              # pick. A shim only fires if a symlink with that exact name
              # is on PATH — not exhaustive by construction (the full
              # triple x version cross product is unbounded); add more if
              # a real project needs them.
              for t in ar c++ cc g++ gcc ranlib clang clang++ ld ld.bfd ld.gold ld.lld; do
                ln -s ../bin/nixgg $out/shims/$t
              done
              for t in gcc g++ cc c++ ar ranlib ld; do
                ln -s ../bin/nixgg $out/shims/x86_64-unknown-linux-gnu-$t
                ln -s ../bin/nixgg $out/shims/x86_64-linux-gnu-$t
              done
            '';
          };

          # Wraps a user command in a builder-rpc-v0 derivation whose
          # output IS a .drv file — the "final link" drv submitted from
          # inside the sandbox. Consumers get the compiled artifact via
          # `builtins.outputOf drv.outPath "out"`.
          mkNixggBuild = import ./nix/mkNixggBuild.nix {
            inherit (pkgs) lib stdenv mkShell coreutils gnumake bash;
            gcc         = toolchain.gcc;
            nixgg       = nixggBin;
            nixHelpers  = nixHelpers;
            patchedNix  = patchedNix;
          };

          # Wraps an EXISTING stdenv (nixpkgsFun's own, or any package's
          # `.stdenv`) so `pkgs.foo.override { stdenv = splitStdenv {
          # ...; splitAtBuild = true; }; }` runs foo's configure/build (or
          # just one) as a builder-rpc-v0 derivation, leaving whatever
          # comes after untouched. See nix/splitStdenv.nix's top comment
          # for the mechanism.
          splitStdenv = import ./nix/splitStdenv.nix {
            inherit (pkgs) lib config stdenvNoCC;
            inherit (pkgs) bash coreutils gnumake;
            gcc         = toolchain.gcc;
            nixgg       = nixggBin;
            patchedNix  = patchedNix;
            nixpkgsPath = nixpkgs;
            inherit system;
          };

          # splitStdenv { splitAtBuild = true; } applied to real, upstream
          # nixpkgs packages — no nixgg-specific package.nix. Distinct
          # names from the mkNixggBuild-based examples above (which build
          # nixgg's own example/ dir) since these instead prove the
          # "upgrade an existing nixpkgs derivation" story from README.md.
          #
          # Three build-system shapes:
          #   - hello: plain autotools, doCheck + postInstallCheck.
          #   - mosh:  autotools + autoreconfHook (setup-hook-injected
          #            phase — broke a naive hardcoded `phases` list).
          #   - zstd:  cmake, 4 outputs, a custom ctest checkPhase, and
          #            the "exec one of its own binaries mid-build" case
          #            (contrib/gen_html) — plain `.override` fails with
          #            "./gen_html: Permission denied" because gen_html
          #            is itself an unresolved drvref stub when zstd's
          #            cmake graph tries to exec it. Fixed via
          #            splitStdenv's extraBuildAttrs: a phase-chained
          #            mkNixggBuild call pre-builds gen_html, and
          #            extraBuildAttrs's postPatch points zstd's cmake at
          #            that already-resolved binary.
          dynDrvExamples = {
            hello-dyndrv = pkgs.hello.override { stdenv = splitStdenv { stdenv = pkgs.stdenv; splitAtBuild = true; }; };
            mosh-dyndrv = pkgs.mosh.override { stdenv = splitStdenv { stdenv = pkgs.stdenv; splitAtBuild = true; }; };
            zstd-dyndrv = import ./examples/zstd-dyndrv {
              inherit pkgs mkNixggBuild splitStdenv;
            };
          };

          # splitStdenv { splitAtConfigure = true; } splits at the
          # configure/build boundary instead of build/install — see
          # nix/splitStdenv.nix's top comment for the mechanism. hello
          # covers autotools/single-output; zstd covers cmake/
          # multi-output. Unlike zstd-dyndrv, zstd-cache needs no
          # extraBuildAttrs workaround for gen_html since there's no
          # sandbox for it to trip over.
          configureSrcFilterPresets = import ./nix/configureSrcFilterPresets.nix;
          batchGroupPresets = import ./nix/batchGroupPresets.nix;
          configureCacheExamples = {
            hello-cache = pkgs.hello.override { stdenv = splitStdenv { stdenv = pkgs.stdenv; splitAtConfigure = true; }; };
            zstd-cache = pkgs.zstd.override { stdenv = splitStdenv { stdenv = pkgs.stdenv; splitAtConfigure = true; }; };
            # An edit to a source file the autotools preset excludes never
            # touches configure's own input, so configure doesn't rerun.
            # existenceStubs needs "src/hello.c" — hello's
            # AC_CONFIG_SRCDIR argument, checked only for existence.
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
            # Same idea, cmake this time. zstd's own CMakeLists.txt uses
            # file(GLOB ...) so filtering can't preserve early-cutoff for
            # it; fmt lists its sources explicitly, so filtering means
            # something here. The extra patterns beyond the cmake preset
            # are real configure-time reads: fmt's CMakeLists.txt (main
            # library sources/headers, README.md/ChangeLog.md baked into
            # add_library, the .pc.in/.cmake.in templates) plus test/
            # wholesale, since BUILD_TESTING defaults on.
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

          # splitStdenv { splitAtConfigure = true; splitAtBuild = true; }
          # combines both: a configure-only stage (optionally
          # configureSrcFilter'd), a sandboxed build-only stage restored
          # on top of it, and an install-onward stage unchanged. See
          # nix/splitStdenv.nix's top comment for why pulling configure
          # out of the sandbox is sound.
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
            # autoreconfHook, the setup-hook-injected-phase case that once
            # broke a naive hardcoded `phases` list. The configure stage
            # here never hardcodes `phases` (uses dontBuild/dontInstall/...
            # toggles instead), so autoreconfHook's `appendToVar
            # preConfigurePhases autoreconfPhase` still applies normally.
            # No configureSrcFilter — mosh isn't in the verified preset set.
            mosh-dyndrv-configure-cached = pkgs.mosh.override {
              stdenv = splitStdenv { stdenv = pkgs.stdenv; splitAtConfigure = true; splitAtBuild = true; };
            };
            # zstd through the combined mechanism: same gen_html
            # mid-build-exec problem zstd-dyndrv documents above, but here
            # the fix must reach BOTH the configure stage (cmake's own
            # Makefile generation) and the build stage (which restores the
            # configure stage's tree and needs its own copy if it ever
            # reconfigures downstream). Patching only the build stage
            # reproduces the same "./gen_html: Permission denied" failure
            # as no patch at all. Both stages need identical patch text,
            # hence extraAttrs (applies to every stage) rather than
            # duplicating into two role-specific hatches. No
            # configureSrcFilter: zstd's CMakeLists.txt uses file(GLOB ...).
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

          # Concrete mkNixggBuild call sites, exposed as flake packages so
          # `nix build .#hello` / `.#lua` Just Work. Each is the resolved
          # final artifact (`builtins.outputOf` applied to the outer
          # text-mode drv), so consumers see a normal store path, not a
          # .drv. `.#hello` builds nixgg/example/ through the sandbox
          # path; the same source built natively via `make` in `nix
          # develop` produces byte-identical compile/link drvs.
          #
          # Every example is one entry: the directory to import and the
          # args it needs beyond `mkNixggBuild`. The -shell attr is
          # generated from this, not hand-written per entry.
          #
          # `nix build .#lua` builds the sandbox version; native
          # equivalence is pinned by tests/drv-equivalence.sh.

          # Shared nativeBuildInputs/src for mosh's/redis's plain and
          # -batch example variants — the -batch variant only adds a
          # batchGroups key.
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
            # Same fixture, batchGroups matching every lua source file —
            # the whole ~30-TU archive (liblua.a) becomes one combined
            # batch derivation instead of 30 compiles + 1 archive.
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
            # Same fixture, batchGroups covering all 6 of mosh's
            # lib*.a archives at once — mosh-server is the link target,
            # not any one archive, so batching should engage for all 6.
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
            # Same fixture, batchGroups = vendorDeps preset — 5 of
            # deps/'s 7 subtrees are reachable from redis-server
            # (jemalloc needs MALLOC=jemalloc, unset here; linenoise is
            # redis-cli-only) and batch cleanly: ~45 tu-*.o.drv + 5
            # ar-*.a.drv collapse into 5 batch-lib*.a.drv, a 158->113
            # total-drv reduction. jemalloc/linenoise are out of scope,
            # not missed coverage — jemalloc has its own autotools
            # build, and linenoise's Makefile links linenoise.o directly
            # without ever archiving it. See examples/redis/default.nix.
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
            # Same fixture, batchGroups covering the 4 (of 8) per-directory
            # static libs that batch cleanly at ffmpeg's ~1200-TU scale.
            #
            # "**" is required, not "*": subdirectories like
            # libavcodec/h264/ feed the same top-level archive, and
            # batching requires EVERY object in an archive to match the
            # same group (go/internal/shim/batcharchive.go's
            # collectSameGroupMembers) — missing even one silently falls
            # the whole archive back to per-TU.
            #
            # libavutil/libswscale each have two source files sharing a
            # basename in different subdirs (e.g. libavutil/cpu.c +
            # libavutil/x86/cpu.c). Colliding members get a deterministic
            # "-2"/"-3" suffix (go/internal/shim/batcharchive.go's
            # disambiguateOutNames) instead of clobbering one shared
            # $objroot slot — without it, libavutil silently dropped its
            # x86 cpu-detection symbols and libswscale silently dropped
            # its non-x86 fallback implementations.
            #
            # libavcodec/libavformat/libavfilter's combined batch scripts
            # are 400KB-1MB (350-550+ staged TU paths in one `bash -c`
            # argument) — over MAX_ARG_STRLEN (131072 bytes). Fixed via
            # Env["batchScript"] + passAsFile instead of Args
            # (go/internal/expr/batcharchive.go's BatchArchiveJSON,
            # nix/batchArchiver.nix), same fix as assemble.Build's openssl
            # tree-restore script.
            #
            # libpostproc is absent from this build's own --disable-*
            # configure flags, unrelated to batching; the other 7 all
            # batch correctly.
            #
            # batchArchiveScript backgrounds each member's compile and
            # bounds concurrency at $NIX_BUILD_CORES via a FIFO `wait
            # "$pid"` job runner rather than serializing the whole batch
            # onto one core — see that file's own docstring for why FIFO
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
            # GCC's own libiberty/ subdir, built via its standalone shipped
            # `./configure`, not gcc's top-level multi-package configure.ac
            # — sidesteps GMP/MPFR/MPC, LTO ar/ranlib decoration, thin
            # archives, and mid-build gengtype/genmodes exec that a full
            # cc1/cc1plus build would hit.
            gcc = {
              dir = ./examples/gcc;
              args = { src = gcc-src; };
            };
            # Same fixture, batchGroups matching every libiberty/*.c —
            # same target-is-the-archive limitation as fmt-batch
            # (libiberty.a is the submission target), on a larger
            # (~65-member) archive. Batching never actually engages.
            gcc-batch = {
              dir = ./examples/gcc;
              args = {
                src = gcc-src;
                batchGroups = [ { name = "gcc"; patterns = [ "libiberty/*.c" ]; } ];
              };
            };
            # PostgreSQL's own src/backend — ~1000+ TUs, via its standalone
            # ./configure && make -C src/backend (not `make world`). Never
            # uses `make -j`: a real, nixgg-unrelated recursive-make
            # ordering race in PostgreSQL's own Makefile.global.
            postgresql = {
              dir = ./examples/postgresql;
              args = { inherit (pkgs) bison flex perl; src = postgresql-src; };
            };
            # QEMU's own x86_64-softmmu target — meson+ninja, the one
            # build-system genre no other example exercises. Its build-time
            # codegen (QAPI, decodetree.py, tracetool.py) is pure
            # find_program-driven Python/shell, never a compiled-and-exec'd
            # QEMU binary, so no phase-chaining fix is needed here.
            qemu = {
              dir = ./examples/qemu;
              args = {
                inherit (pkgs) pkg-config meson ninja glib pixman ncurses zlib;
                pythonWithMesonDeps = pkgs.python3.withPackages (ps: [ ps.distlib ps.setuptools ]);
                src = qemu-src;
              };
            };
            # Nix's own C++ source (the nix-15793 flake input, previously
            # consumed only as pre-built binaries). Builds all 15
            # production subprojects, all 8 unit-test subprojects, and
            # clang-tidy-plugin. Named `nix-full` (not `nix-util`, not bare
            # `nix`): started as a libutil-only slice but now builds all
            # of Nix; bare `nix` was ruled out since `toolchain.nix` (this
            # same `packages` output) already claims that name.
            nix-full = {
              dir = ./examples/nix-full;
              args = {
                inherit (pkgs) meson ninja cmake bison flex busybox lsof;
                # nixpkgs' own default llvm output has no llvm-config
                # (that's the "dev" output) — reuse nix-15793's devShell
                # llvm, which already resolved to "dev".
                llvm = builtins.head (
                  builtins.filter (p: (p.pname or "") == "llvm") nix-15793.devShells.${system}.default.nativeBuildInputs
                );
                pkgConfig = pkgs.pkg-config;
                buildInputs = nixBuildInputs;
                src = nix-15793;
              };
            };
            # Same fixture, batchGroups covering libqemuutil.a's ~450
            # members — the one archive (of QEMU's 5) worth batching;
            # libvhost-user.a/libvhost-user-glib.a/libvduse.a each have
            # exactly 1 member, and the final link plus
            # libcommon.a/libqemu-x86_64-softmmu.a never go through `ar`
            # at all (meson's "extract objects" passes .o files straight
            # to the link line).
            #
            # QEMU's internal static libs are all built with `ar csrDT`
            # (thin) — the fixture that first exercised thin-archive
            # support in go/internal/expr/batcharchive.go: a thin batch
            # archive writes its member objects into $out/lib/.nixgg-objs/
            # (a permanent store output) rather than a build-tmp scratch
            # dir, so the archive's self-references survive after the
            # sandbox is torn down.
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
            # Two sources, no single `src`: phase 1 builds the codegen
            # tool, phase 2 execs it mid-build. Smoke test for the
            # phase-chaining pattern examples/llvm relies on.
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
            # archive's own subdirectory. phase1 (llvm-min-tblgen) links
            # exactly 3 archives — libLLVMDemangle.a, libLLVMSupport.a
            # (142 TUs), libLLVMTableGen.a (12 TUs) — none of which are
            # phase1's own link target.
            #
            # libLLVMSupport mixes plain .c/.S sources in with .cpp
            # (regcomp.c/regexec.c's BSD regex port, rpmalloc/, BLAKE3's
            # per-arch .c/.S variants), and collectSameGroupMembers
            # (go/internal/shim/batcharchive.go) requires EVERY input to
            # ar's invocation to be a same-group pending member — so all
            # three extensions must be listed, not just *.cpp.
            #
            # Support's combined batch script is 152853 bytes, over
            # MAX_ARG_STRLEN (131072) — same fix as ffmpeg-batch:
            # Env["batchScript"] + passAsFile instead of Args
            # (go/internal/expr/batcharchive.go's BatchArchiveJSON,
            # nix/batchArchiver.nix). phase1 goes from 186 to 13 total
            # derivations with all 3 archives batched.
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
          # the mkShell mirroring the sandbox env, used by
          # tests/drv-equivalence.sh to replay the build natively.
          # `.result` (the outputOf string) is still reachable via
          # `.#<name>.result`.
          exampleResults = builtins.mapAttrs (_: e: e.package) examples;
          exampleShells = pkgs.lib.mapAttrs' (n: e: pkgs.lib.nameValuePair "${n}-shell" e.shell) examples;
        in
        toolchain
        // exampleResults   # .#hello .#lua .#fmt .#mosh .#redis .#ffmpeg .#gcc .#two-phase .#llvm
        // exampleShells    # .#<name>-shell for each of the above
        // dynDrvExamples   # .#hello-dyndrv .#mosh-dyndrv .#zstd-dyndrv
        // configureCacheExamples   # .#hello-cache .#zstd-cache .#hello-cache-filtered .#fmt-cache-filtered
        // dynDrvConfigureCacheExamples   # .#hello-dyndrv-configure-cached .#mosh-dyndrv-configure-cached .#zstd-dyndrv-configure-cached .#gdbm-dyndrv-configure-cached
        // {
          # Extras an individual example exposes beyond .result/.shell.
          # llvm's two tblgen phases are separately buildable so the
          # chain can be smoke-tested a phase at a time.
          llvm-min-tblgen = examples.llvm.llvm-min-tblgen.package;
          llvm-tblgen = examples.llvm.llvm-tblgen.package;
          llvm-min-tblgen-batch = examples.llvm-batch.llvm-min-tblgen.package;
          llvm-tblgen-batch = examples.llvm-batch.llvm-tblgen.package;
          two-phase-codegen = examples.two-phase.codegen.package;
          # linux-kernel's phase1 (phase2 is a plain stdenv.mkDerivation
          # with no shims, nothing for drv-equivalence to compare) exposes
          # the full mkNixggBuild attrset, not just .package:
          # tests/drv-equivalence.sh's equiv_sandbox_drvs needs
          # "<attr>.drv.outputs" at this exact top-level name.
          linux-kernel-phase1 = examples.linux-kernel.linux-kernel-phase1;
          linux-kernel-phase1-shell = examples.linux-kernel.linux-kernel-phase1.shell;
          # mosh's second multi-target output (default .package is
          # mosh-server) — see mkNixggBuild.nix's `packages` attrset for
          # the general mechanism.
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
          # No `gcc` in packages: the cc-wrapper's setup hook injects
          # NIX_CFLAGS_COMPILE/NIX_LDFLAGS with a per-invocation
          # -frandom-seed and a workspace-relative -rpath, which would
          # break CA hash stability across shell entries and point at a
          # path that doesn't exist. The shim resolves the real compiler
          # via NIXGG_COMPILER_ROOT instead.
          nixggShell = pkgs.mkShellNoCC {
            name = "nixgg-shell";
            packages = [
              pkgs'.nixgg-bin
              pkgs.gnumake
              pkgs.coreutils
              pkgs.bash
            ];
            shellHook = ''
              # env-shell is the same block `nixgg env` prints; sourcing
              # it directly avoids a fork-and-eval per shell entry.
              . ${pkgs'.env-shell}

              # Per-user preference, not set by env-shell.
              : "''${NIXGG_STORE:=local?root=/tmp/nixgg-store}"
              export NIXGG_STORE

              # mkShellNoCC still synthesises its own $out, which can leak
              # NIX_CFLAGS_COMPILE/NIX_LDFLAGS with the same -frandom-seed/
              # rpath problem noted above even with no gcc in packages.
              unset NIX_CFLAGS_COMPILE NIX_CFLAGS_LINK NIX_LDFLAGS

              # bin/ first so `nixgg` itself is on PATH; shims/ so `cc`,
              # `c++`, `ar` etc. dispatch to it.
              export PATH="${pkgs'.nixgg-bin}/bin:${pkgs'.nixgg-bin}/shims:$PATH"

              # Opinionated default: link shim inline-realises, so plain
              # `make` produces real binaries. Set to 0 to opt out.
              : "''${NIXGG_AUTOFORCE:=1}"
              export NIXGG_AUTOFORCE

              # Sandbox mode needs the patched Nix, not ambient PATH — an
              # unpatched Nix fails opaquely (`attribute 'outputOf'
              # missing` at eval, or a submit-output error mid-build).
              # See README.md's "Invoking sandbox mode explicitly".
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
