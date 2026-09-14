# Linux kernel — the largest, most structurally demanding fixture in
# this repo: real Kbuild recursive-make, thousands of TUs, hundreds of
# nested `built-in.a` archives, a raw (non-gcc-driven) `ld` link step,
# GNU as's `.incbin` directive, and host tools (fixdep, Kconfig's
# `conf`, modpost) whose own build/exec timing doesn't fit any prior
# fixture's shape.
#
# Scope: `make tinyconfig`, target `vmlinux` plus a small set of
# loadable modules (`CONFIG_TEST_DHRY`, a self-contained multi-object
# module with zero Kconfig deps, and `CONFIG_SAMPLE_KOBJECT`, two
# single-object modules) — enough to exercise the real module
# compile→link→modpost→.ko chain without a full `x86_64_defconfig`'s
# build time. genksyms (CONFIG_MODVERSIONS) and BTF-for-modules
# (CONFIG_DEBUG_INFO_BTF_MODULES) are off by default and not exercised
# here.
#
# Sandbox mode requires the two-phase split below because a single
# mkNixggBuild derivation can't satisfy Kbuild's recipe shape:
# scripts/link-vmlinux.sh reads vmlinux.o/vmlinux back synchronously,
# in the same recursive make, moments after producing them (objcopy
# for .modinfo; nm+sorttable for System.map/table-sorting). A
# builder-rpc-v0 sandbox's op allowlist for a RecursiveSubmitted
# connection (src/libstore/daemon.cc's performOp) has no operation
# that builds a derivation at all — every nixgg output is a
# registered drv, not real bytes, until Nix's outer scheduler resolves
# it, which only happens after the build script exits — too late for
# a script that needs to read its own output back first. See phase2's
# docstring for why the fix is a plain `stdenv.mkDerivation`, not
# another mkNixggBuild call.
#
# `cmd_ar_vmlinux.a` is overridden on the command line to skip the
# real recipe's second `ar mPiT <member> vmlinux.a <members>` step (a
# member-reordering pass for architectures with a `head-object-
# list.txt` entry; x86 has none, so the reorder is a no-op here). That
# second `ar` call runs `ar t vmlinux.a` synchronously on an archive
# nixgg just registered as its own derivation — the same
# "no synchronous realize" wall several other Kbuild steps hit, and
# (unlike modpost/vmlinux.o/vmlinux) not fixable by another
# mode.ForLink carveout since it's an archive read, not a link. `T` is
# kept (thin archive, required for nixgg's own archive model) but `S`
# is dropped (suppress-symbol-table) so ar still builds vmlinux.a's
# real index itself, since the skipped second call was also
# responsible for rebuilding it as a side effect of the reorder —
# keeping `S` produces "archive has no index; run ranlib to add one"
# at the final `ld`.
#
# `KCFLAGS` overrides route around this environment's GCC being newer
# than what linux-6.12 was written against:
# `-Wno-error=unterminated-string-initialization` silences a strict
# new warning in ACPI table code (off in tinyconfig, kept for
# robustness against config drift), and `-std=gnu11` avoids GCC 15's
# C23-by-default `bool`/`true`/`false` keyword conflict with the
# kernel's pre-C23 `typedef _Bool bool` compat shim in
# include/linux/stddef.h.
#
# `KBUILD_BUILD_USER`/`KBUILD_BUILD_HOST` are pinned in both phases:
# scripts/mkcompile_h bakes `$(whoami)`/`uname -n` into
# include/generated/compile.h by default, so two separate invocations
# (native vs sandbox, different tempdirs) would otherwise produce
# different content and diverging drv hashes purely from that, not
# from anything native/sandbox-specific.
{
  mkNixggBuild,
  stdenv,
  src,
  flex,
  bison,
  elfutils,
  pkg-config,
  bc,
  # batchGroups passthrough — not wired up for this fixture yet.
  batchGroups ? [ ],
}:

let
  # Phase 1: everything through the top-level `built-in.a` (Kbuild's
  # bare-dot descend target, `build-dir := .`), plus the two `lib.a`s
  # vmlinux.o's link needs and one extra `ar` call to fold built-in.a
  # into vmlinux.a. This is the expensive part: ~2800 real compiles,
  # hundreds of nested archives — none of it touching the vmlinux.o/
  # vmlinux/modpost synchronous-read wall phase2's docstring covers.
  #
  # `vmlinux.a` (not bare `built-in.a`) is the target name used here:
  # `built-in.a`'s basename recurs at dozens of nesting levels and
  # would basename-collide as a target (see matchesTarget's docstring).
  # `ar cDPrT vmlinux.a ./built-in.a` after `make .` reproduces Kbuild's
  # own cmd_ar_vmlinux.a, minus its second `ar mPiT` reorder call (see
  # the buildCommand comment on that).
  #
  # `lib/test_dhry.o` (CONFIG_TEST_DHRY) is built here too, as an extra
  # explicit single-target alongside `.` in the same `make` invocation
  # (Kbuild's `single-goals` mechanism folds it into the one recursive
  # descend) — a genuine multi-object module (`test_dhry-objs :=
  # dhry_1.o dhry_2.o dhry_run.o`) exercising Kbuild's `ld -r -o $@
  # @$<` response-file link. Chosen for having no Kconfig deps/selects
  # at all. `make .` alone does not build it: an obj-m entry is
  # invisible to the bare `build-dir` target unless named explicitly.
  #
  # CONFIG_SAMPLE_KOBJECT is also enabled (needed so `.config`/
  # `modules.order` know about it), but its two objects
  # (kobject-example.o/kset-example.o) are deliberately not phase1
  # targets: each is a bare compile output with no link step of its
  # own, and mkNixggBuild's targets mechanism only ever submits LINK/
  # ARCHIVE outputs. Declaring a bare compile output as a target fails
  # the whole derivation. Phase 2 rebuilds both from `src` directly
  # instead (cheap: 2 TUs).
  phase1 = mkNixggBuild {
    pname = "linux-kernel-phase1";
    version = "6.12";
    inherit src batchGroups;
    targets = [
      { name = "vmlinux-a"; path = "vmlinux.a"; }
      { name = "lib-a"; path = "lib/lib.a"; }
      { name = "arch-lib-a"; path = "arch/x86/lib/lib.a"; }
      { name = "test-dhry-o"; path = "lib/test_dhry.o"; }
    ];
    nativeBuildInputs = [ flex bison pkg-config bc ];
    buildInputs = [ elfutils ];
    buildCommand = ''
      export KBUILD_BUILD_USER=nixgg
      export KBUILD_BUILD_HOST=nixgg
      NIXGG_BYPASS=1 make tinyconfig
      NIXGG_BYPASS=1 cat >> .config <<'EOF'
CONFIG_MODULES=y
CONFIG_RUNTIME_TESTING_MENU=y
CONFIG_TEST_DHRY=m
CONFIG_SAMPLES=y
CONFIG_SAMPLE_KOBJECT=m
EOF
      NIXGG_BYPASS=1 make olddefconfig
      NIXGG_BYPASS=1 make prepare
      make -j"$NIX_BUILD_CORES" \
        KCFLAGS="-Wno-error=unterminated-string-initialization -std=gnu11" \
        lib/test_dhry.o .
      ar cDPrT vmlinux.a ./built-in.a
    '';
  };

  # Phase 2: the final link, entirely outside Kbuild's own recursive
  # make. phase1.result* (real, Nix-resolved store paths by the time
  # phase 2's build starts, since they're ordinary buildInputs) are
  # mounted directly and used as-is — phase 2 does not hand the
  # problem back to `make` (which has no idea phase 1 already built
  # vmlinux.a/lib.a; handing it a tree with those present but their
  # upstream inputs, built-in.a's ~2800 objects, absent would make it
  # try to rebuild the world). Instead it replicates Kbuild's last few
  # steps by hand, using the real tools directly: `ld` (the exact
  # flags scripts/link-vmlinux.sh's cmd_link_vmlinux uses for this
  # config — no kallsyms, no BTF, no objtool, no arch postlink pass;
  # all four are off in tinyconfig), then `nm`+mksysmap for
  # System.map, then `sorttable` in place. Verified byte-for-byte
  # reproducible against this fixture's own complete single-phase
  # native-mode build (md5sum-identical vmlinux, modulo BuildID/mtime
  # noise).
  #
  # `NIXGG_BYPASS=1 make tinyconfig && make prepare` reruns the same
  # cheap pre-pass phase 1 already did to regenerate
  # `scripts/sorttable` fresh (a byproduct of `make prepare`, a
  # hostprogs link never routed through nixgg's own graph even in the
  # single-phase build). `arch/x86/kernel/vmlinux.lds` is not a
  # byproduct of `prepare` — it's only reached during the full
  # recursive descend — so buildCommand names it as its own explicit
  # single-target goal (Kbuild's single-target dispatch handles
  # `%.lds` directly).
  #
  # phase2 is a plain stdenv.mkDerivation, not mkNixggBuild:
  # mkNixggBuild's `targets` mechanism is "register a dyn-drv output
  # now, resolve it to real bytes later" — the real bytes only exist
  # after Nix's outer scheduler resolves the derivation, i.e. after
  # this build script has already exited. That is fundamentally
  # incompatible with `nm`/`sorttable` needing to read `vmlinux` back
  # synchronously, moments later, in the same script that just linked
  # it — routing this same ld/nm/sorttable sequence through
  # mkNixggBuild submitted vmlinux correctly as a registered drv, but
  # the very next `nm -n vmlinux` in the same script read the still-
  # unresolved drvref-stub marker ("file format not recognized" /
  # "unrecognized ELF data encoding"). None of `ld`/`nm`/`sorttable`
  # need any nixgg acceleration here — each runs exactly once — so a
  # plain derivation with no nixgg shims on PATH at all is correct.
  phase2 = stdenv.mkDerivation {
    pname = "linux-kernel";
    version = "6.12";
    inherit src;
    nativeBuildInputs = [ flex bison pkg-config bc ];
    buildInputs = [
      elfutils
      phase1.results.vmlinux-a
      phase1.results.lib-a
      phase1.results.arch-lib-a
    ];
    dontConfigure = true;
    buildPhase = ''
      runHook preBuild

      export KBUILD_BUILD_USER=nixgg
      export KBUILD_BUILD_HOST=nixgg
      make tinyconfig
      cat >> .config <<'EOF'
CONFIG_MODULES=y
CONFIG_RUNTIME_TESTING_MENU=y
CONFIG_TEST_DHRY=m
CONFIG_SAMPLES=y
CONFIG_SAMPLE_KOBJECT=m
EOF
      make olddefconfig
      make prepare
      # Only reached during the full recursive descend, which phase 2
      # deliberately never runs — Kbuild's single-target dispatch
      # builds vmlinux.lds directly without pulling in anything else.
      make arch/x86/kernel/vmlinux.lds

      ln -s ${phase1.results.vmlinux-a}/lib/vmlinux.a vmlinux.a
      mkdir -p lib arch/x86/lib
      ln -s ${phase1.results.lib-a}/lib/lib.a lib/lib.a
      ln -s ${phase1.results.arch-lib-a}/lib/lib.a arch/x86/lib/lib.a

      ld -m elf_i386 -z noexecstack --no-warn-rwx-segments \
        --build-id=sha1 --orphan-handling=warn \
        -o vmlinux \
        -T arch/x86/kernel/vmlinux.lds \
        --whole-archive vmlinux.a --no-whole-archive \
        --start-group lib/lib.a arch/x86/lib/lib.a --end-group

      nm -n vmlinux | sed -f scripts/mksysmap > System.map
      scripts/sorttable vmlinux

      # Module chain, entirely outside Kbuild's recursive make, same
      # rationale as vmlinux's own link above: `make modules` needs
      # vmlinux.o, which Kbuild's single-targets list refuses to build
      # directly ("No rule to make target vmlinux.o"), so it's linked
      # by hand instead, replicating scripts/Makefile.vmlinux_o's
      # cmd_ld_vmlinux.o exactly for this config (no LTO, so no
      # .tmp_initcalls.lds; no CONFIG_BUILTIN_MODULE_RANGES, so no
      # -Map=).
      ld -m elf_i386 -z noexecstack --no-warn-rwx-segments \
        -r -o vmlinux.o \
        --whole-archive vmlinux.a --no-whole-archive \
        --start-group lib/lib.a arch/x86/lib/lib.a --end-group

      # `make modules` rebuilds test_dhry/kobject-example/kset-example
      # from src directly here, not borrowed from phase1's own
      # test-dhry-o result: vmlinux.a/lib.a are pure link inputs, but a
      # module's .o participates in `make modules`'s own staleness
      # tracking (if_changed's .cmd files), and a symlinked-in .o with
      # no matching .cmd here would read as "unknown, must rebuild"
      # and try to re-run test_dhry.o's own `ld -r` link, which needs
      # dhry_1.o/dhry_2.o/dhry_run.o, absent in this sparse tree.
      # Cheap enough (5 TUs total) to rebuild plain, unaccelerated.
      make -j"$NIX_BUILD_CORES" \
        KCFLAGS="-Wno-error=unterminated-string-initialization -std=gnu11" \
        modules

      runHook postBuild
    '';
    installPhase = ''
      runHook preInstall
      mkdir -p "$out"
      cp -a vmlinux System.map "$out/"
      cp -a lib/test_dhry.ko samples/kobject/kobject-example.ko samples/kobject/kset-example.ko "$out/"
      runHook postInstall
    '';
  };
in
phase2 // {
  # flake.nix's `examples`/`exampleResults`/`exampleShells` wiring
  # expects every example to carry `.package`/`.shell`. phase2 is a
  # plain stdenv.mkDerivation, so it has neither natively — `package`
  # is just itself (already a real derivation), and `shell` is itself
  # too (`nix develop` on a plain stdenv.mkDerivation already drops
  # into its own build environment).
  package = phase2;
  shell = phase2;
  linux-kernel-phase1 = phase1;
}

