# Nix itself (github.com/NixOS/nix, the `nix-15793` flake input),
# built from source rather than consumed as pre-built binaries.
#
# Scope: the whole top-level meson project — all 15 production
# subprojects (libutil, libstore, libfetchers, libexpr, libflake,
# libmain, libcmd, each with a `-c` C-API wrapper, plus the `nix` CLI
# and nswrapper), all 8 unit-test subprojects (`-Dunit-tests=true`),
# and clang-tidy-plugin (optional upstream, buildable here since llvm
# is on nativeBuildInputs so `dependency('LLVM', method: 'config-
# tool')` finds llvm-config). All 24 targets are built explicitly by
# ninja target name rather than relying on `nix`'s link step to pull
# them in transitively, since the `-c` wrapper libs aren't linked into
# `nix` by default (`plugin-c-api` defaults false) and the test/plugin
# binaries are nobody's dependency.
#
# The whole tree is configured once (not per-subproject) because
# libstore's `dependency('nix-util')` resolves via
# `meson.override_dependency`, real in-tree subproject linkage that
# only exists inside a shared top-level project — a standalone
# libstore configure pointed at an installed libutil via
# PKG_CONFIG_PATH also works, but `ninja install` unconditionally
# rebuilds the full library regardless of `--tags devel`, doubling
# compile work.
#
# Every subproject's external deps must be resolvable at CONFIGURE
# time (meson resolves `dependency()` calls eagerly in `meson setup`,
# not lazily at `ninja` time), so `buildInputs` below is nix-15793's
# own unfiltered devShell package set rather than a hand-picked
# subset, avoiding any ABI/version mismatch from mixing nixpkgs pins.
#
# `-Ddefault_library=static`: meson's own default is `shared`; without
# this flag we'd get libnixutil.so, not the .a `targets` expects.
#
# `-Db_lto=false`, no `-Dunity=`: neither is meson's own project
# default (both off already); kept explicit as a guard against ever
# inheriting nixpkgs' `mkMesonLibrary` wrapper defaults
# (`-Dunity=on`/`-Db_lto=true`), which this fixture deliberately
# bypasses by calling `meson setup` directly.
#
# `targets[].path` is the top-level-build-relative ninja target name
# (e.g. `src/libutil/libnixutil.a`), not a bare basename: at this
# top-level build cwd is the shared `build/` root, so every
# subproject's `ar`/link invocation emits a path-shaped output
# argument. matchesTarget's basename fallback only fires when the
# declared target itself is bare, so a bare `path` here would fail to
# submit. Every name/path pair was read directly off a real
# `ninja -C build -t targets all` on this exact configure.
#
# The 10 internal libraries with `prelink: true` (7 production libs
# plus 3 of 4 test-support libs) make ninja run a partial link
# (`g++ -r -o <name>-prelink.o <every .o>`) and archive that single
# prelinked object, rather than archiving per-TU compile outputs
# directly like every other nixgg fixture. `g++ -r` is link-shaped
# (`-r`, not `-c`), so dispatch.IsCompile already routes it to
# shim.Link; the prelinked object's `.o` extension needed a
# `-prelink.o` filename check in derivation.go's ArtifactSubdir (added
# before the generic `.o` case) so it lands in the link-output `bin/`
# subdir rather than the flat compile-output path.
#
# `nix` itself (`src/nix/nix`) statically links all 7 production
# prelinked archives plus external deps in one `g++ ... -o nix`; the
# 5 `*-tests` executables link their own subproject's library plus
# gtest/rapidcheck; `libnix-clang-tidy.so` links its own 2 TUs plus
# libLLVM directly. `nix-build`/`nix-channel`/etc. are just symlinks
# to `nix` created post-link, not compiler/linker/archiver
# invocations, so nixgg's shims never see them.
{
  mkNixggBuild,
  src,
  meson,
  ninja,
  pkgConfig,
  cmake,
  bison,
  flex,
  busybox,
  lsof,
  llvm,
  buildInputs,
  batchGroups ? [ ],
}:

mkNixggBuild {
  pname = "nix-full";
  version = "2.36.0";
  inherit src batchGroups;
  # llvm and busybox are appended to buildInputs (not just
  # nativeBuildInputs) so mkNixggBuild's knownStorePathInputs — which
  # only scans buildInputs/propagatedBuildInputs, never
  # nativeBuildInputs — picks up their store paths as declared
  # derivation inputs. Without this, llvm-dev's path reaches
  # NIX_CFLAGS_COMPILE via its setup-hook but never inputs.srcs, so the
  # real sandbox hides it ("fatal error: llvm/ADT/IntrusiveRefCntPtr.h:
  # No such file or directory") even though the -isystem flag is right
  # there; busybox's path is baked into the
  # `-Dlibstore:sandbox-shell=${busybox}/bin/busybox` meson flag VALUE
  # (a plain string, no dependency()/nativeBuildInputs mechanism at
  # all) so nothing else tells knownStorePathInputs it's needed either
  # — same bug class knownStorePathInputs' own docstring documents for
  # zlib.h/openssl-dev, different root cause.
  buildInputs = buildInputs ++ [ llvm busybox ];
  targets = [
    { name = "libnixutil"; path = "src/libutil/libnixutil.a"; }
    { name = "libnixstore"; path = "src/libstore/libnixstore.a"; }
    { name = "libnixfetchers"; path = "src/libfetchers/libnixfetchers.a"; }
    { name = "libnixexpr"; path = "src/libexpr/libnixexpr.a"; }
    { name = "libnixflake"; path = "src/libflake/libnixflake.a"; }
    { name = "libnixmain"; path = "src/libmain/libnixmain.a"; }
    { name = "libnixcmd"; path = "src/libcmd/libnixcmd.a"; }
    { name = "libnixutilc"; path = "src/libutil-c/libnixutilc.a"; }
    { name = "libnixstorec"; path = "src/libstore-c/libnixstorec.a"; }
    { name = "libnixfetchersc"; path = "src/libfetchers-c/libnixfetchersc.a"; }
    { name = "libnixexprc"; path = "src/libexpr-c/libnixexprc.a"; }
    { name = "libnixflakec"; path = "src/libflake-c/libnixflakec.a"; }
    { name = "libnixmainc"; path = "src/libmain-c/libnixmainc.a"; }
    { name = "nix"; path = "src/nix/nix"; }
    { name = "nswrapper"; path = "src/nswrapper/nix-nswrapper"; }
    { name = "libnix-util-test-support"; path = "src/libutil-test-support/libnix-util-test-support.a"; }
    { name = "nix-util-tests"; path = "src/libutil-tests/nix-util-tests"; }
    { name = "libnix-store-test-support"; path = "src/libstore-test-support/libnix-store-test-support.a"; }
    { name = "nix-store-tests"; path = "src/libstore-tests/nix-store-tests"; }
    { name = "nix-fetchers-tests"; path = "src/libfetchers-tests/nix-fetchers-tests"; }
    { name = "libnix-expr-test-support"; path = "src/libexpr-test-support/libnix-expr-test-support.a"; }
    { name = "nix-expr-tests"; path = "src/libexpr-tests/nix-expr-tests"; }
    { name = "nix-flake-tests"; path = "src/libflake-tests/nix-flake-tests"; }
    { name = "libnix-clang-tidy"; path = "src/clang-tidy-plugin/libnix-clang-tidy.so"; }
  ];
  # cmake is needed because src/libexpr/meson.build's `dependency
  # ('toml11', method: 'cmake', ...)` is required and resolved via
  # CMake's find_package, not pkg-config. bison/flex are needed for
  # libexpr's parser.y/lexer.l custom_target()s.
  #
  # busybox is NOT added to nativeBuildInputs: its multi-call `cp`
  # shadows stdenv's real coreutils on PATH and breaks unpackPhase
  # ("cp: unrecognized option '--preserve=timestamps'"). Passed
  # instead as an explicit `-Dlibstore:sandbox-shell=<full path>` meson
  # flag (below), sidestepping find_program's PATH search entirely.
  # The `libstore:` prefix is required: this option lives in
  # src/libstore/meson.options (subproject-scoped), and at a
  # whole-tree configure meson does not expose subproject options as
  # bare `-D<option>=value`. The namespace is `libstore` (the name
  # `subproject('libstore')` uses), not `nix-store` (that subproject's
  # own internal `project()` name).
  #
  # lsof IS added to nativeBuildInputs (unlike busybox): libstore's
  # `find_program('lsof', required: false)` bakes whatever it finds
  # into a generated header (store-config-private.hh's `LSOF` macro).
  # Native mode (inheriting the host's PATH) would bake in the host's
  # `/usr/bin/lsof`, while sandbox mode's PATH has no `/usr/bin` and
  # falls back to the bare "lsof" string — two different generated
  # headers, two different TU drv hashes for every libstore TU that
  # includes it. Pinning it here makes both modes resolve the same
  # store path.
  #
  # llvm is needed in nativeBuildInputs (not just buildInputs) for
  # clang-tidy-plugin's `dependency('LLVM', method: 'config-tool')`:
  # that resolves via `find_program('llvm-config')`, which only
  # searches PATH, not NIX_CFLAGS_COMPILE/NIX_LDFLAGS/PKG_CONFIG_PATH.
  # It also has to stay in buildInputs (see above) for
  # knownStorePathInputs to see it independently of PATH visibility.
  nativeBuildInputs = [ meson ninja pkgConfig cmake bison flex lsof llvm ];
  buildCommand = ''
    # Same NIXGG_BYPASS=1-on-configure rationale as fmt/qemu: meson's
    # compiler probes need real object files, and meson hard-codes the
    # shim's PATH entry into its generated ninja build files, so
    # unsetting BYPASS only after setup completes is what routes the
    # real `ninja` compile/prelink/archive invocations through the
    # shim. functional-tests/json-schema-checks (default ON) are
    # disabled since this fixture never builds those targets; doc-gen
    # subprojects declare zero executable()/library() targets (pure
    # doxygen/mdbook) and functional-tests has exactly one real compile
    # target (`build_by_default: false`), so neither adds anything
    # nixgg's shims would ever see.
    NIXGG_BYPASS=1 meson setup build \
      -Ddefault_library=static \
      -Db_lto=false \
      -Dunit-tests=true \
      -Dfunctional-tests=false \
      -Djson-schema-checks=false \
      -Dlibstore:sandbox-shell=${busybox}/bin/busybox

    unset NIXGG_BYPASS
    ninja -C build -j"$NIX_BUILD_CORES" \
      src/libutil/libnixutil.a \
      src/libstore/libnixstore.a \
      src/libfetchers/libnixfetchers.a \
      src/libexpr/libnixexpr.a \
      src/libflake/libnixflake.a \
      src/libmain/libnixmain.a \
      src/libcmd/libnixcmd.a \
      src/libutil-c/libnixutilc.a \
      src/libstore-c/libnixstorec.a \
      src/libfetchers-c/libnixfetchersc.a \
      src/libexpr-c/libnixexprc.a \
      src/libflake-c/libnixflakec.a \
      src/libmain-c/libnixmainc.a \
      src/nix/nix \
      src/nswrapper/nix-nswrapper \
      src/libutil-test-support/libnix-util-test-support.a \
      src/libutil-tests/nix-util-tests \
      src/libstore-test-support/libnix-store-test-support.a \
      src/libstore-tests/nix-store-tests \
      src/libfetchers-tests/nix-fetchers-tests \
      src/libexpr-test-support/libnix-expr-test-support.a \
      src/libexpr-tests/nix-expr-tests \
      src/libflake-tests/nix-flake-tests \
      src/clang-tidy-plugin/libnix-clang-tidy.so
  '';
}
