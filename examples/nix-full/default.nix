# Nix itself (github.com/NixOS/nix, the `nix-15793` flake input already
# pinned in this repo — previously only consumed as pre-built binaries,
# never built from source through nixgg).
#
# SCOPE: ALL of it. Nix's own top-level meson.build declares 15 real
# production Meson subprojects under src/: libutil, libstore,
# libfetchers, libexpr, libflake, libmain, libcmd (each with its own
# `-c` C-API wrapper subproject), the `nix` CLI itself, and nswrapper
# (Linux-only, unconditionally built here since this fixture only
# targets x86_64-linux) — PLUS all 8 real unit-test subprojects
# (libutil-test-support/-tests, libstore-test-support/-tests,
# libfetchers-tests, libexpr-test-support/-tests, libflake-tests;
# gated by `-Dunit-tests=true`, off by default in meson.options) —
# PLUS clang-tidy-plugin (`required: false` in the top-level
# meson.build, but genuinely buildable here: `llvm` is added to
# nativeBuildInputs — not just buildInputs, which doesn't put a
# package's bin/ on PATH — specifically so `dependency('LLVM',
# method: 'config-tool')` finds llvm-config; confirmed directly via a
# real configure: "llvm-config found: NO" before this fix, "YES
# 21.1.8" after, then a real compile+link of
# src/clang-tidy-plugin/libnix-clang-tidy.so through nixgg's own
# native-mode shim). This fixture configures the WHOLE top-level
# project once (doc-gen/functional-tests/json-schema-checks disabled —
# see below for why) and builds every one of those 24 real targets
# explicitly by ninja target name (`targets` below) — not relying on
# `nix`'s own link step to pull the libraries in transitively, since
# the `-c` wrapper libraries are NOT linked into `nix` by default
# (`plugin-c-api` defaults false) and would otherwise never get built
# at all, and the test/plugin binaries are never anyone's dependency
# at all.
#
# WHY configure the whole tree instead of just src/libutil (this
# fixture's own earlier, libutil-only design): libstore's
# meson.build does `dependency('nix-util')`, which meson resolves via
# `meson.override_dependency('nix-util', ...)` — REAL, in-tree
# subproject linkage (nix-meson-build-support/export/meson.build) that
# only exists when libutil is configured as an actual meson
# `subproject()` of a shared top-level project, not when it's its own
# standalone top-level project. Confirmed directly by trying the
# opposite (build libutil standalone, `ninja install` it, point
# libstore's own standalone configure at the installed .pc via
# PKG_CONFIG_PATH): it works, but `ninja install` unconditionally
# builds the full library first regardless of `--tags devel`
# filtering, meaning headers-only reuse would silently double the real
# compile work. Configuring the whole tree once and naming ninja
# targets explicitly avoids that entirely — meson's own subproject
# resolution finds libutil's real build outputs directly inside the
# SAME build directory, no install/pkg-config step at all.
#
# Every subproject's own external dependencies (boost, openssl,
# libarchive, libsodium, brotli, zstd, libcpuid, libblake3,
# nlohmann_json for libutil/libstore; curl, sqlite3, libseccomp,
# aws-crt-cpp for libstore; boehm-gc, toml11 for libexpr; lowdown,
# editline for libcmd; mimalloc for the nix CLI; gtest, rapidcheck,
# gbenchmark for the *-tests subprojects; llvm for clang-tidy-plugin)
# must be resolvable at CONFIGURE time — meson resolves every declared
# subproject's own `dependency()` calls eagerly during `meson setup`,
# not lazily at `ninja` time (confirmed directly: a real, unshimmed
# `meson setup` of the whole tree with every one of nix-15793's own
# devShell packages present, `-Dunit-tests=true`, and `llvm` on PATH,
# succeeded cleanly, resolving all 24 real subprojects — zero
# gracefully-declined ones). `buildInputs` below is therefore
# nix-15793's OWN devShell package set, not a hand-picked subset —
# reusing exactly the packages nix-15793's own CI validates against,
# with zero risk of an ABI/version mismatch from mixing two different
# nixpkgs pins for boost/openssl/etc.
#
# `-Ddefault_library=static`: meson's own project-level default for
# `default_library` is `shared` (confirmed: `if get_option
# ('default_library') == 'static')` in src/libutil/meson.build is a
# real conditional branch, not the unconditional path) — without this
# flag, meson produces libnixutil.so, not the .a this fixture's own
# `targets` entries expect.
#
# `-Db_lto=false`, no `-Dunity=...` flag at all: neither is a default
# in the PROJECT's own meson.build (confirmed: no `b_lto`/`unity`
# reference anywhere in src/libutil/meson.build or the top-level one).
# Both ARE turned on by nixpkgs' own per-component packaging
# (`nix-15793.packages.${system}.nix-util`'s `mkMesonLibrary` wrapper:
# `-Dunity=on -Dunity_size=8192` plus `mesonBuildType = "release"` →
# `-Db_lto=true` — confirmed directly via `nix eval` on that package's
# own `mesonFlags`/`mesonBuildType`) — this fixture deliberately does
# NOT go through that wrapper (calls `meson setup` directly, like every
# other nixgg fixture), so LTO/unity would otherwise just be meson's
# own plain defaults (LTO off already; unity off already) — `-Db_lto
# =false` is kept explicit here anyway as a guard against ever
# accidentally inheriting a `mesonBuildType`-driven default.
#
# `targets[].path` is the top-level-build-relative ninja target name
# (`src/libutil/libnixutil.a`, `src/nix/nix`, etc.), NOT a bare
# basename — confirmed directly: at the TOP-level build, cwd is the
# shared `build/` root and every subproject's own `ar`/link
# invocation is issued with a PATH-shaped output argument
# (`ar csrD src/libutil/libnixutil.a ...`), unlike a standalone
# per-subproject build (this fixture's own earlier libutil-only
# design) where cwd was already inside that subproject's own build
# dir and the same output was bare (`ar csrD libnixutil.a ...`).
# matchesTarget's basename fallback only fires when the DECLARED
# target itself is bare, so a bare `path` here would never match and
# the whole build would fail to submit ("failed to submit output
# path") — the same class of mismatch examples/linux-kernel's own
# history already documents for matchesTarget. Every target name/path
# pair below was read directly off a real `ninja -C build -t targets
# all` on this exact configure (not guessed from the meson.build
# source) — `library()`'s first positional arg gets an implicit `lib`
# prefix (`nixutil` -> `libnixutil.a`) exactly once per library.
#
# A genuinely new mechanism this fixture exercises for the first time
# in this repo: EVERY one of the 10 internal libraries with
# `prelink: true` (the 7 production libraries — libutil, libstore,
# libfetchers, libexpr, libflake, libmain, libcmd — plus 3 of the 4
# test-support libraries: libutil-test-support, libstore-test-support,
# libexpr-test-support; the 6 `-c` wrapper libraries, `nswrapper`'s
# executable, and the 5 *-tests executables do NOT use this) makes
# ninja run a PARTIAL LINK (`g++ -r -o <name>-prelink.o <every .o>`)
# and then archive that SINGLE prelinked object. Every prior nixgg
# fixture's archives contain per-TU compile outputs as members; these
# contain a single LINK output instead. `g++ -r ...` is correctly
# link-shaped (`-r`, not `-c`), so dispatch.IsCompile's existing
# `-c`-only check already routes it to shim.Link rather than
# shim.Compile — and this fixture's own real build found (and fixed,
# in go/internal/expr/derivation.go's ArtifactSubdir) that the
# prelinked object's `.o` extension made nixgg misplace it at the FLAT
# compile-output path instead of the LINK-output `bin/` subdir its
# actual producer wrote it to — a `-prelink.o` filename pattern is now
# checked before the generic `.o` case to fix this, mode-
# symmetrically (both native and sandbox mode agree, since the check
# is purely filename-based, same constraint every other
# inputSubdirFor case already has to satisfy).
#
# `nix` itself (the CLI executable, `src/nix/nix`) is one of seven real
# multi-input LINKs here, not archives: it statically links all 7
# production prelinked archives plus every external dependency in one
# `g++ ... -o nix`; the 5 `*-tests` executables (nix-util-tests,
# nix-store-tests, nix-fetchers-tests, nix-expr-tests, nix-flake-tests)
# are the same shape, linking their own subproject's real library plus
# gtest/rapidcheck; `libnix-clang-tidy.so` (a plain `shared_module`,
# not `prelink: true`) is the seventh, linking its own 2 TUs plus
# libLLVM directly. `nix-build`/`nix-channel`/etc. (the multicall-
# wrapper CUSTOM_COMMAND targets ninja also reports) are just symlinks
# to `nix` created post-link — not built here, since they're not
# compiler/linker/archiver invocations nixgg's shims ever see.
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
  # llvm is appended to buildInputs (NOT just nativeBuildInputs, see
  # below) so mkNixggBuild.nix's own knownStorePathInputs — which only
  # scans buildInputs/propagatedBuildInputs, deliberately never
  # nativeBuildInputs — picks up llvm-dev's own store path as a real,
  # declared derivation input. Confirmed as a REAL, reproducible bug
  # without this: llvm-dev's path appears in NIX_CFLAGS_COMPILE (via
  # its own nixpkgs setup-hook, triggered by nativeBuildInputs) but
  # was never in inputs.srcs, so Nix's own real build sandbox
  # (correctly) hid the path entirely — "fatal error:
  # llvm/ADT/IntrusiveRefCntPtr.h: No such file or directory" even
  # though the exact right -isystem flag was right there in the
  # compile command. This is the SAME bug class knownStorePathInputs'
  # own docstring already documents for zlib.h/openssl-dev (a
  # buildInputs package's `.dev` output not matching), just via a
  # different root cause (nativeBuildInputs entirely out of scope,
  # not a wrong-output selection within buildInputs).
  #
  # busybox is appended here for the IDENTICAL reason, a second real
  # instance of the same bug class: busybox's own store path is baked
  # into the `-Dlibstore:sandbox-shell=${busybox}/bin/busybox` meson
  # flag VALUE (a plain string, not a `dependency()`/nativeBuildInputs
  # mechanism at all), so nothing else ever told knownStorePathInputs
  # it needs to be a real declared input either. Confirmed as a real,
  # reproducible bug via a genuinely fresh native-mode build (a fresh
  # unpacked nix-15793 checkout, fresh alt store, no reused state):
  # meson's own configure log showed "Program
  # /nix/store/...-busybox-1.37.0/bin/busybox found: NO" — not because
  # the flag value was wrong (it was byte-identical to the confirmed-
  # working earlier session's own value), but because THIS store
  # genuinely never had that path on disk, and find_program on an
  # absolute path can't succeed against a path that doesn't exist.
  # store-config-private.hh ends up with NEITHER branch of libstore's
  # own `if embedded-sandbox-shell / elif busybox.found()` firing, so
  # HAVE_EMBEDDED_SANDBOX_SHELL is genuinely undefined — tripping
  # linux-derivation-builder.cc's bare `#if HAVE_EMBEDDED_SANDBOX_SHELL`
  # under -Werror=undef. A prior session's own verification runs
  # reused an alt store where busybox happened to already be present
  # from earlier, unrelated activity — ambient state that silently
  # masked this exact gap, the same false-positive class
  # tests/smoke.sh's own DYNDRV comment already warns about for a
  # different mechanism (glibc's own fixed-output path existing on
  # the real host by coincidence).
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
  # cmake is needed because src/libexpr/meson.build's own
  # `dependency('toml11', method: 'cmake', ...)` is a REQUIRED
  # dependency (no `required: false`) resolved via CMake's own
  # find_package machinery, not pkg-config — confirmed directly, a
  # real configure without cmake present fails with "CMake binary for
  # machine host machine not found" the moment libexpr's own
  # subproject gets executed. bison/flex are needed for the same
  # reason (libexpr's own parser.y/lexer.l custom_target()s,
  # confirmed directly — a real configure without them fails "Program
  # 'bison' not found" at the identical point).
  #
  # busybox is NOT added to nativeBuildInputs, deliberately — it's a
  # multi-call binary whose own `cp`/etc. shadow stdenv's real
  # coreutils on PATH (nativeBuildInputs prepends every package's
  # bin/ ahead of the base toolchain), which broke stdenv's own
  # unpackPhase outright ("cp: unrecognized option
  # '--preserve=timestamps'" — busybox's own cp doesn't support GNU
  # cp's long options at all) — confirmed directly as the very next
  # real failure after adding it there. Passed as an explicit
  # `-Dlibstore:sandbox-shell=<full path>` meson flag instead (below),
  # fully sidestepping find_program's PATH search — this is what
  # src/libstore/meson.build's own `sandbox-shell` string option
  # exists for. The `libstore:` prefix is required, not optional: this
  # option is declared in src/libstore/meson.options (a SUBPROJECT-
  # scoped option file), and at the WHOLE-tree configure this fixture
  # does, meson does not expose a subproject's own options as bare
  # `-D<option>=value` — confirmed directly (a bare `-Dsandbox-shell=`
  # fails with "Unknown option: sandbox-shell" at top-level configure)
  # and confirmed fixed (the namespaced form builds cleanly, both
  # sandbox and native mode). The namespace is `libstore` — the name
  # `subproject('libstore')` uses in the top-level meson.build — not
  # `nix-store`, that subproject's own internal `project()` name. It's
  # ALSO appended to buildInputs above (not nativeBuildInputs — see
  # that paragraph's own docstring for why this is the identical bug
  # class llvm hit).
  #
  # lsof IS added to nativeBuildInputs (unlike busybox): libstore's
  # own `find_program('lsof', required: false)` bakes whatever it
  # finds (or the bare string "lsof" if not found) into a generated
  # header (store-config-private.hh's `LSOF` macro) — confirmed as a
  # REAL cross-mode divergence, not a hypothetical one: native mode
  # (run via `nix develop`, inheriting the host's interactive PATH)
  # found the host's own `/usr/bin/lsof` and baked in that absolute
  # path, while sandbox mode's build PATH has no `/usr/bin` and fell
  # back to the bare "lsof" string — two different generated headers,
  # two different TU drv hashes for every libstore TU that includes
  # it. Adding `lsof` to nativeBuildInputs makes `find_program` always
  # resolve the SAME store path in both modes, matching every other
  # find_program-driven option this fixture already pins explicitly
  # (busybox) rather than letting it drift with the host environment.
  #
  # llvm is needed for clang-tidy-plugin's own `dependency('LLVM',
  # method: 'config-tool')` to resolve — it must be in
  # nativeBuildInputs specifically, not just buildInputs: buildInputs
  # only feeds NIX_CFLAGS_COMPILE/NIX_LDFLAGS/PKG_CONFIG_PATH, never
  # PATH, so `find_program('llvm-config')` (what `method: 'config-
  # tool'` shells out to) wouldn't find it there — confirmed directly,
  # llvm was already present in buildInputs (nix-15793's own devShell
  # set is unfiltered) yet configure still reported "llvm-config
  # found: NO" until llvm was added here too.
  #
  # llvm ALSO needs to stay in buildInputs (added explicitly above,
  # not just inherited from nix-15793's own unfiltered set) for a
  # SEPARATE reason: mkNixggBuild.nix's own knownStorePathInputs
  # (which decides which store paths storedeps.From may declare as
  # real derivation inputs) only scans buildInputs/
  # propagatedBuildInputs, deliberately never nativeBuildInputs — see
  # its own docstring's "zlib.h: No such file or directory" precedent
  # for a DIFFERENT root cause of the identical failure shape.
  # Confirmed directly as a real, reproducible bug without this: with
  # llvm in nativeBuildInputs only, llvm-dev's own store path appears
  # in NIX_CFLAGS_COMPILE (via its setup-hook) but was never in
  # inputs.srcs, so Nix's real build sandbox correctly hid that path
  # entirely — "fatal error: llvm/ADT/IntrusiveRefCntPtr.h: No such
  # file or directory" even though the exact right -isystem flag was
  # right there in the compile command; fixed once llvm was ALSO
  # in buildInputs, giving knownStorePathInputs (and thus
  # storedeps.From, and thus inputs.srcs) visibility into it.
  nativeBuildInputs = [ meson ninja pkgConfig cmake bison flex lsof llvm ];
  buildCommand = ''
    # Same NIXGG_BYPASS=1-on-configure rationale as fmt/qemu: meson's
    # own compiler probes need real object files, and meson hard-codes
    # the shim's PATH entry into its generated ninja build files, so
    # unsetting BYPASS only after setup completes is what routes the
    # real `ninja` compile/prelink/archive invocations through the
    # shim. functional-tests/json-schema-checks default ON in
    # meson.options — disabled here the same way llvm/qemu disable
    # their own doc/functional-test subprojects, since this fixture
    # never builds those targets and they'd only add configure-time
    # dependency surface for nothing. unit-tests is turned ON
    # (opposite of its own off-by-default), since the whole point of
    # widening this fixture was to cover the *-tests subprojects too.
    #
    # doc-gen (internal-api-docs/external-api-docs/nix-manual, off by
    # default already) was investigated and deliberately left out:
    # confirmed directly (grepping their meson.build files) that all
    # three declare ZERO executable()/library() targets — pure
    # doxygen/mdbook invocations, nothing for nixgg's cc/ar/ld shims
    # to ever see. functional-tests was ALSO investigated: its own
    # meson.build is almost entirely `find_program`/shell-test-suite
    # wiring (meson test, not ninja), with exactly one real compile
    # target anywhere in it — test-libstoreconsumer/main.cc, a single
    # trivial executable, and even that is `build_by_default: false`
    # (only built by `meson test`, never by a plain `ninja` with no
    # explicit target) — not worth the added configure-time surface
    # (a `nix-store`-found check, `dependency('nix-store')`) for one
    # TU that adds no new mechanism this fixture doesn't already
    # exercise via the 24 real targets above.
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
