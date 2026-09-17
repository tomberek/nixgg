{
  description = "nixgg — gg-style build accelerator using Nix CA derivations.";

  nixConfig = {
    extra-experimental-features = [
      "ca-derivations"
      "dynamic-derivations"
      "configurable-impure-env"
    ];
    extra-system-features = [ "builder-rpc-v0" ];
  };

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  # Tracks NixOS/nix PR #15793 (builder-rpc-v0 / `nix store submit-output`),
  # unmerged upstream.
  inputs.nix-15793 = {
    url = "github:NixOS/nix";
  };

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
    url = "https://ftp.gnu.org/gnu/gcc/gcc-15.3.0/gcc-15.3.0.tar.xz";
    flake = false;
  };
  inputs.llvm-src = {
    # Monorepo checkout, not a release tarball: llvm/CMakeLists.txt
    # include()s from sibling cmake/ and third-party/.
    #
    # 19.x not 18.1.8: 18.1.8's SmallVector.h needs <cstdint> transitively,
    # which GCC 15 no longer pulls in; fixed upstream in llvm-project#101761.
    url = "github:llvm/llvm-project/llvmorg-19.1.7";
    flake = false;
  };
  inputs.postgresql-src = {
    # Builds only src/backend; `-j` on that subdir hits a real
    # recursive-make ordering race in PostgreSQL's own Makefile.global —
    # sidestep with `make -C src/backend generated-headers` first, then a
    # non-`-j` build.
    url = "https://ftp.postgresql.org/pub/source/v17.2/postgresql-17.2.tar.gz";
    flake = false;
  };
  inputs.qemu-src = {
    url = "https://download.qemu.org/qemu-9.2.0.tar.xz";
    flake = false;
  };

  inputs.linux-src = {
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

          patchedNix =
            (nix-15793.packages.${system}.nix-cli
              or nix-15793.packages.${system}.default);

          nixStoreC = nix-15793.packages.${system}.nix-store-c;
          nixUtilC = nix-15793.packages.${system}.nix-util-c;

          # nix-15793's own devShell buildInputs, unfiltered rather than this
          # flake's `pkgs`, to avoid an ABI/version mismatch between two
          # nixpkgs pins. Unfiltered because configuring the top-level
          # meson.build resolves every subproject's dependency() calls even
          # though only libutil/libstore get built.
          nixBuildInputs =
            let
              shell = nix-15793.devShells.${system}.default;
            in
            (shell.buildInputs or [ ]) ++ (shell.propagatedBuildInputs or [ ]);

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
              export NIXGG_PATCHED_NIX="${patchedNix}"
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

          # Its own src (not the whole repo) so editing nix/*.nix, examples,
          # or docs can't move this derivation's hash.
          nixggBin = pkgs.buildGoModule {
            pname = "nixgg";
            version = "0";
            src = ./go;
            vendorHash = null;
            doCheck = false;
            postInstall = ''
              mkdir -p $out/shims
              # A shim only fires if a symlink with that exact name is on
              # PATH; not exhaustive (the triple x version cross product is
              # unbounded) — add more if a real project needs them.
              # objtool/objcopy aren't compilers: they're per-object
              # rewriters, reached only when a caller points the build's
              # own tool variable (objtool=, OBJCOPY=) here — nothing
              # resolves them via PATH otherwise. rustc IS reached via
              # PATH (Kbuild's RUSTC default is a bare "rustc").
              for t in ar c++ cc g++ gcc ranlib clang clang++ ld ld.bfd ld.gold ld.lld objtool objcopy rustc; do
                ln -s ../bin/nixgg $out/shims/$t
              done
              for t in gcc g++ cc c++ ar ranlib ld; do
                ln -s ../bin/nixgg $out/shims/x86_64-unknown-linux-gnu-$t
                ln -s ../bin/nixgg $out/shims/x86_64-linux-gnu-$t
              done
            '';
          };

          # Output IS a .drv file (the "final link" drv submitted from
          # inside the sandbox); consumers resolve via `builtins.outputOf`.
          mkNixggBuild = import ./nix/mkNixggBuild.nix {
            inherit (pkgs) lib stdenv mkShell coreutils gnumake bash;
            gcc         = toolchain.gcc;
            nixgg       = nixggBin;
            nixHelpers  = nixHelpers;
            patchedNix  = patchedNix;
          };

          # See nix/splitStdenv.nix's top comment for the mechanism.
          splitStdenv = import ./nix/splitStdenv.nix {
            inherit (pkgs) lib config stdenvNoCC;
            inherit (pkgs) bash coreutils gnumake;
            gcc         = toolchain.gcc;
            nixgg       = nixggBin;
            patchedNix  = patchedNix;
            nixpkgsPath = nixpkgs;
            inherit system;
          };

          # splitAtBuild applied to real upstream nixpkgs packages, proving
          # the "upgrade an existing nixpkgs derivation" story from
          # README.md. mosh exercises autoreconfHook's setup-hook-injected
          # phase; zstd's cmake graph execs its own gen_html binary
          # mid-build, which a plain `.override` can't resolve inside the
          # sandbox ("./gen_html: Permission denied") — fixed via
          # splitStdenv's extraBuildAttrs pre-building gen_html and
          # pointing cmake at the resolved binary.
          dynDrvExamples = {
            hello-dyndrv = pkgs.hello.override { stdenv = splitStdenv { stdenv = pkgs.stdenv; splitAtBuild = true; }; };
            mosh-dyndrv = pkgs.mosh.override { stdenv = splitStdenv { stdenv = pkgs.stdenv; splitAtBuild = true; }; };
            zstd-dyndrv = import ./examples/zstd-dyndrv {
              inherit pkgs mkNixggBuild splitStdenv;
            };
          };

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
            # zstd's CMakeLists.txt uses file(GLOB ...) so filtering can't
            # preserve early-cutoff there; fmt lists sources explicitly, so
            # filtering matters. Extra patterns beyond the cmake preset are
            # real configure-time reads (README.md/ChangeLog.md baked into
            # add_library, .pc.in/.cmake.in templates, test/ since
            # BUILD_TESTING defaults on).
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
            mosh-dyndrv-configure-cached = pkgs.mosh.override {
              stdenv = splitStdenv { stdenv = pkgs.stdenv; splitAtConfigure = true; splitAtBuild = true; };
            };
            # gen_html mid-build-exec problem (see dynDrvExamples above);
            # here the fix must reach both the configure stage (cmake's
            # Makefile generation) and the build stage (which restores the
            # configure tree and would reconfigure with its own copy),
            # hence extraAttrs applying to every stage rather than a
            # role-specific hatch.
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
            # gdbm covers multi-output+filter together (hello alone only
            # covers single-output+filter, zstd only multi-output+no-filter).
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

          # Each example is one entry: the directory to import and the args
          # it needs beyond `mkNixggBuild`. The -shell attr is generated
          # from this. Native equivalence is pinned by
          # tests/drv-equivalence.sh.
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
            hello = {
              dir = ./dyn-drv/hello-mkbuild.nix;
              args = { inherit (pkgs) lib; };
            };
            lua = {
              dir = ./examples/lua;
              args = { src = lua-src; };
            };
            # Same fixture; batchGroups collapses the whole ~30-TU liblua.a
            # archive into one batch derivation instead of 30 compiles + 1
            # archive.
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
            # fmt's own target IS the archive (libfmt.a), so tryBatchArchive
            # refuses to batch it; expected to build with batching never
            # actually engaging.
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
            # mosh-server is the link target, not any one archive, so
            # batching engages for all 6 lib*.a archives.
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
            # vendorDeps preset: 5 of deps/'s 7 subtrees are reachable from
            # redis-server and batch cleanly (jemalloc needs MALLOC=jemalloc,
            # unset here; linenoise is redis-cli-only and never archived).
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
            # "**" (not "*") matters: subdirectories like libavcodec/h264/
            # feed the same top-level archive, and batching requires EVERY
            # member of an archive to match the same group
            # (collectSameGroupMembers in go/internal/shim/batcharchive.go)
            # or the whole archive falls back to per-TU.
            #
            # libavutil/libswscale each have two sources sharing a basename
            # in different subdirs; colliding members get a deterministic
            # "-2"/"-3" suffix (disambiguateOutNames) instead of clobbering
            # one shared $objroot slot.
            #
            # libavcodec/libavformat/libavfilter's combined batch scripts
            # exceed MAX_ARG_STRLEN (131072 bytes), fixed via
            # Env["batchScript"] + passAsFile instead of Args
            # (BatchArchiveJSON in go/internal/expr/batcharchive.go).
            #
            # libpostproc is disabled by this build's own configure flags,
            # unrelated to batching; the other 7 archives batch correctly.
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
            # libiberty/ built via its own standalone ./configure, not gcc's
            # top-level multi-package configure.ac — sidesteps GMP/MPFR/MPC,
            # LTO ar/ranlib decoration, thin archives, and mid-build
            # gengtype/genmodes exec that a full cc1/cc1plus build would hit.
            gcc = {
              dir = ./examples/gcc;
              args = { src = gcc-src; };
            };
            # Same target-is-the-archive limitation as fmt-batch
            # (libiberty.a is the submission target); batching never engages.
            gcc-batch = {
              dir = ./examples/gcc;
              args = {
                src = gcc-src;
                batchGroups = [ { name = "gcc"; patterns = [ "libiberty/*.c" ]; } ];
              };
            };
            # Standalone ./configure && make -C src/backend (not `make
            # world`). Never `make -j`: a real, nixgg-unrelated
            # recursive-make ordering race in PostgreSQL's own
            # Makefile.global.
            postgresql = {
              dir = ./examples/postgresql;
              args = { inherit (pkgs) bison flex perl; src = postgresql-src; };
            };
            # meson+ninja, the one build-system genre no other example
            # exercises; its codegen (QAPI, decodetree.py, tracetool.py) is
            # pure find_program-driven Python/shell, never a compiled-and-
            # exec'd QEMU binary, so no phase-chaining fix is needed here.
            qemu = {
              dir = ./examples/qemu;
              args = {
                inherit (pkgs) pkg-config meson ninja glib pixman ncurses zlib;
                pythonWithMesonDeps = pkgs.python3.withPackages (ps: [ ps.distlib ps.setuptools ]);
                src = qemu-src;
              };
            };
            # Named `nix-full` (not `nix-util`, not bare `nix`) since it
            # builds all of Nix, not just libutil; `nix` is already claimed
            # by toolchain.nix in this same packages output.
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
            # libqemuutil.a is the one archive (of QEMU's 5) worth batching;
            # the others have exactly 1 member or never go through `ar`
            # (meson's "extract objects" passes .o files straight to the
            # link line).
            #
            # QEMU's internal static libs are built with `ar csrDT` (thin)
            # — first fixture to exercise thin-archive support: a thin
            # batch archive writes its member objects into
            # $out/lib/.nixgg-objs/ (a permanent store output) rather than a
            # build-tmp scratch dir, so self-references survive after the
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
            # A single sandbox derivation can't satisfy Kbuild's synchronous
            # read-back-after-produce shape; see examples/linux-kernel's own
            # docstring for why phase 2 is a plain stdenv.mkDerivation
            # rather than another mkNixggBuild call.
            linux-kernel = {
              dir = ./examples/linux-kernel;
              args = {
                inherit (pkgs) stdenv flex bison elfutils pkg-config bc pkgsStatic cpio;
                inherit nixggBin;
                src = linux-src;
              };
            };
            # Two sources, no single `src`: phase 1 builds the codegen
            # tool, phase 2 execs it mid-build.
            two-phase = {
              dir = ./examples/two-phase;
              args = {
                codegenSrc = ./examples/two-phase/codegen;
                appSrc = ./examples/two-phase/app;
              };
            };
            # Minimal reproduction of the ar --thin mechanism QEMU's meson
            # build exercises at scale.
            thin-archive = {
              dir = ./examples/thin-archive;
              args = { inherit (pkgs) lib; };
            };
            # Two Rust crates built by make and bare rustc — the shape
            # Kbuild uses, and the only one the rustc shim models. See
            # its own docstring for what it covers that nothing else
            # does.
            rustc = {
              dir = ./examples/rustc;
              args = {
                inherit (pkgs) rustc;
                src = ./examples/rustc;
              };
            };
            llvm = {
              dir = ./examples/llvm;
              args = {
                inherit (pkgs) runCommand cmake ninja pkg-config python3 perl which
                  libffi libxml2 ncurses zlib;
                src = llvm-src;
              };
            };
            # phase1 (llvm-min-tblgen) links exactly 3 archives —
            # libLLVMDemangle.a, libLLVMSupport.a, libLLVMTableGen.a — none
            # of which are phase1's own link target.
            #
            # libLLVMSupport mixes plain .c/.S sources in with .cpp, and
            # collectSameGroupMembers requires every input to ar's
            # invocation to be a same-group pending member, so all three
            # extensions must be listed, not just *.cpp.
            #
            # Support's combined batch script exceeds MAX_ARG_STRLEN —
            # same Env["batchScript"]+passAsFile fix as ffmpeg-batch. phase1
            # goes from 186 to 13 total derivations with all 3 archives
            # batched.
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

          examples = builtins.mapAttrs
            (_: def: import def.dir ({ inherit mkNixggBuild; } // def.args))
            exampleDefs;

          exampleResults = builtins.mapAttrs (_: e: e.package) examples;
          exampleShells = pkgs.lib.mapAttrs' (n: e: pkgs.lib.nameValuePair "${n}-shell" e.shell) examples;
        in
        toolchain
        // exampleResults
        // exampleShells
        // dynDrvExamples
        // configureCacheExamples
        // dynDrvConfigureCacheExamples
        // {
          llvm-min-tblgen = examples.llvm.llvm-min-tblgen.package;
          llvm-tblgen = examples.llvm.llvm-tblgen.package;
          llvm-min-tblgen-batch = examples.llvm-batch.llvm-min-tblgen.package;
          llvm-tblgen-batch = examples.llvm-batch.llvm-tblgen.package;
          two-phase-codegen = examples.two-phase.codegen.package;
          # phase2 is a plain stdenv.mkDerivation with no shims, so only
          # phase1 exposes the full mkNixggBuild attrset (not just .package).
          linux-kernel-phase1 = examples.linux-kernel.linux-kernel-phase1;
          linux-kernel-phase1-shell = examples.linux-kernel.linux-kernel-phase1.shell;
          linux-kernel-initramfs = examples.linux-kernel.linux-kernel-initramfs;
          mosh-client = examples.mosh.packages.mosh-client;

          toolchain-json = toolchainJson;
          env-shell = envShell;
          mosh-env = moshEnv;
          fmt-env = fmtEnv;
          patched-nix = patchedNix;
          nix-store-c = nixStoreC;
          nix-util-c = nixUtilC;
          nixgg-bin = nixggBin;
          inherit mkNixggBuild;
          inherit splitStdenv;
          inherit configureSrcFilterPresets;
          default = envShell;
        }
      );

      devShells = forEachSystem (system: pkgs:
        let
          pkgs' = self.packages.${system};

          # No `gcc` in packages: the cc-wrapper's setup hook injects
          # NIX_CFLAGS_COMPILE/NIX_LDFLAGS with a per-invocation
          # -frandom-seed and a workspace-relative -rpath, breaking CA hash
          # stability across shell entries. The shim resolves the real
          # compiler via NIXGG_COMPILER_ROOT instead.
          nixggShell = pkgs.mkShellNoCC {
            name = "nixgg-shell";
            packages = [
              pkgs'.nixgg-bin
              pkgs.gnumake
              pkgs.coreutils
              pkgs.bash
            ];
            shellHook = ''
              . ${pkgs'.env-shell}

              : "''${NIXGG_STORE:=local?root=/tmp/nixgg-store}"
              export NIXGG_STORE

              # mkShellNoCC still synthesises its own $out, which can leak
              # the same NIX_CFLAGS_COMPILE/NIX_LDFLAGS problem noted above.
              unset NIX_CFLAGS_COMPILE NIX_CFLAGS_LINK NIX_LDFLAGS

              export PATH="${pkgs'.nixgg-bin}/bin:${pkgs'.nixgg-bin}/shims:$PATH"

              : "''${NIXGG_AUTOFORCE:=1}"
              export NIXGG_AUTOFORCE

              # An unpatched Nix fails opaquely (`attribute 'outputOf'
              # missing` at eval, or a submit-output error mid-build).
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
