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
# The serial/TTY/PVH options in the .config fragment below exist for
# tests/kernel-boot-smoke.sh: `allnoconfig` (tinyconfig's base) leaves
# no console at all, and CONFIG_PVH embeds an ELF note
# (XEN_ELFNOTE_PHYS32_ENTRY) that lets QEMU's `-kernel` loader boot the
# plain `vmlinux` output directly — no bzImage assembly needed. See
# that script for what it proves and why a bzImage was tried and
# dropped (make bzImage re-walks the entire recursive Kbuild descend
# regardless of vmlinux's own freshness).
#
# CONFIG_BLK_DEV_INITRD/DEVTMPFS/DEVTMPFS_MOUNT/BINFMT_ELF/
# BINFMT_SCRIPT let that same script boot to a real userspace process
# instead of just the kernel's own deterministic "no rootfs" panic:
# without BLK_DEV_INITRD, QEMU's `-initrd` is silently ignored
# (confirmed directly — the kernel panics identically with or without
# it). BINFMT_ELF defaults to y upstream but, like every other option
# here, allnoconfig (tinyconfig's base) forces it off regardless —
# without it the kernel can't exec ANY ELF binary at all, confirmed
# directly: /init (a real static ELF) failed with "Failed to execute
# /init (error -2)" until this was added. BINFMT_SCRIPT is what lets
# /init's own `#!/bin/busybox sh` shebang line be interpreted.
# CONFIG_TMPFS is deliberately left off: it `depends on SHMEM` (off by
# default on this allnoconfig-derived base) and isn't needed anyway —
# init/do_mounts.c's own `rootfs_fs_type` already falls back to ramfs
# when `IS_ENABLED(CONFIG_TMPFS)` is false, confirmed by reading that
# fallback directly.
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
# from anything native/sandbox-specific. `KBUILD_BUILD_TIMESTAMP` pins
# the same class of thing for usr/gen_init_cpio.c's own default mtime
# (`time(NULL)` when unset — confirmed as the exact source of a
# usr/initramfs_data.{o,cpio} drv-equivalence mismatch that appeared
# once CONFIG_BLK_DEV_INITRD was added below; usr/Makefile already
# forwards this var to gen_initramfs.sh's own `-d` flag for exactly
# this purpose).
{
  mkNixggBuild,
  stdenv,
  src,
  flex,
  bison,
  elfutils,
  pkg-config,
  bc,
  nixggBin,
  pkgsStatic,
  cpio,
  # batchGroups passthrough — not wired up for this fixture yet.
  batchGroups ? [ ],
}:

let
  initramfs = import ./initramfs.nix { inherit stdenv pkgsStatic cpio; };

  # Phase 1: every per-directory `built-in.a`/`lib.a` EXCEPT
  # `arch/x86/entry/` (vdso's own directory) and the top-level folds
  # that would aggregate it — those stay in phase2, unaccelerated, for
  # the reason below. This is the expensive part: ~2800 real compiles,
  # hundreds of nested archives — none of it touching the vmlinux.o/
  # vmlinux/modpost synchronous-read wall phase2's own docstring covers.
  #
  # WHY NOT ONE `vmlinux.a` TARGET (as this fixture used to declare)
  #
  # `arch/x86/entry/vdso/` links a shared object with its own linker
  # script, then reads it back synchronously (nm for vdso-image-64.c's
  # symbol table, objcopy for the stripped .so) — the same undeferrable
  # shape tests/kernel/kernel.nix documents for the full kernel build.
  # `NIXGG_PASSTHROUGH_PATHS` (nix/mkNixggBuild.nix's own
  # `passthroughPaths` param) can route around a subtree like this by
  # letting the shims exec real work instead of registering a drv —
  # but that only fixes vdso's OWN build; every `ar` invocation with an
  # unmodellable input passes through ENTIRELY —
  # go/internal/shim/storeinput.go's classifyInputs has no partial/
  # mixed mode — so once vdso's built-in.a is a real file instead of a
  # drv, `arch/x86/entry/built-in.a` is real bytes too, and so is
  # `arch/x86/built-in.a` (folds `entry/` in alongside nine clean
  # sibling directories) and the top-level `built-in.a`
  # (folds `arch/x86/` in). A `vmlinux.a` target built from that has NO
  # drv to submit — confirmed directly: `nix build` failed with
  # "failed to submit output path for 'vmlinux-a.drv'".
  #
  # This only surfaced once the kernel became genuinely x86_64 (see
  # ARCH=x86_64 below) — the accidental 32-bit build this fixture
  # produced before never linked a vdso at all.
  #
  # THE FIX: target every clean per-directory archive individually.
  # `arch/x86/Kbuild`'s own `obj-y` list is `entry/ events/
  # platform/pvh/ realmode/ kernel/ mm/ crypto/ platform/ net/ virt/`
  # (filtered to what this .config enables) — all ten fold into
  # `arch/x86/built-in.a`, and only `entry/` is poisoned. Declaring the
  # other nine (plus every top-level directory) as their own targets
  # keeps ~99% of the object graph accelerated; phase2 builds the
  # small unaccelerated remainder (`entry/`, `platform/pvh/` — no
  # single-target rule of its own, so it rides along as
  # `arch/x86/built-in.a`'s own prerequisite — and the two top-level
  # folds) by hand, the same way it already hand-builds `ld`/`nm`/
  # `sorttable`.
  #
  # `arch/x86/lib/built-in.a` (msr.o/msr-reg.o/iomem.o) and
  # `arch/x86/platform/pvh/built-in.a` both have NO single-target
  # Kbuild rule when named directly on the command line ("No rule to
  # make target") — confirmed by trying. `arch/x86/lib/lib.a` has the
  # identical problem, deterministically, whenever it's named alongside
  # `arch/x86/built-in.a`/`built-in.a` in the same `make` invocation —
  # confirmed reproducible with no nixgg shims involved at all, so not
  # a parallelism race. All three are cheap (well under 50 files
  # total) and get built in phase2 instead — see its own docstring for
  # the two-call `|| true`-then-retry shape that works around the
  # "No rule" report (the archive still gets built correctly despite
  # the reported error) — so none of them need their own phase1 targets.
  #
  # `lib/test_dhry.o` (CONFIG_TEST_DHRY) is built here too, alongside
  # the archive targets (Kbuild's `single-goals` mechanism folds
  # multiple named targets into one recursive descend) — a genuine
  # multi-object module (`test_dhry-objs := dhry_1.o dhry_2.o
  # dhry_run.o`) exercising Kbuild's `ld -r -o $@ @$<` response-file
  # link. Chosen for having no Kconfig deps/selects at all.
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
      { name = "usr-a"; path = "usr/built-in.a"; }
      { name = "certs-a"; path = "certs/built-in.a"; }
      { name = "ipc-a"; path = "ipc/built-in.a"; }
      { name = "sound-a"; path = "sound/built-in.a"; }
      { name = "samples-a"; path = "samples/built-in.a"; }
      { name = "crypto-a"; path = "crypto/built-in.a"; }
      { name = "virt-a"; path = "virt/built-in.a"; }
      { name = "security-a"; path = "security/built-in.a"; }
      { name = "init-a"; path = "init/built-in.a"; }
      { name = "fs-a"; path = "fs/built-in.a"; }
      { name = "drivers-a"; path = "drivers/built-in.a"; }
      { name = "kernel-a"; path = "kernel/built-in.a"; }
      { name = "mm-a"; path = "mm/built-in.a"; }
      { name = "lib-builtin-a"; path = "lib/built-in.a"; }
      { name = "lib-a"; path = "lib/lib.a"; }
      { name = "arch-crypto-a"; path = "arch/x86/crypto/built-in.a"; }
      { name = "arch-realmode-a"; path = "arch/x86/realmode/built-in.a"; }
      { name = "arch-net-a"; path = "arch/x86/net/built-in.a"; }
      { name = "arch-platform-a"; path = "arch/x86/platform/built-in.a"; }
      { name = "arch-virt-a"; path = "arch/x86/virt/built-in.a"; }
      { name = "arch-events-a"; path = "arch/x86/events/built-in.a"; }
      { name = "arch-mm-a"; path = "arch/x86/mm/built-in.a"; }
      { name = "arch-kernel-a"; path = "arch/x86/kernel/built-in.a"; }
      { name = "test-dhry-o"; path = "lib/test_dhry.o"; }
    ];
    nativeBuildInputs = [ flex bison pkg-config bc ];
    buildInputs = [ elfutils ];
    buildCommand = ''
      export KBUILD_BUILD_USER=nixgg
      export KBUILD_BUILD_HOST=nixgg
      export KBUILD_BUILD_TIMESTAMP="Thu Jan  1 00:00:00 UTC 1970"
      export ARCH=x86_64
      NIXGG_BYPASS=1 make tinyconfig
      NIXGG_BYPASS=1 cat >> .config <<'EOF'
CONFIG_MODULES=y
CONFIG_RUNTIME_TESTING_MENU=y
CONFIG_TEST_DHRY=m
CONFIG_SAMPLES=y
CONFIG_SAMPLE_KOBJECT=m
CONFIG_TTY=y
CONFIG_SERIAL_8250=y
CONFIG_SERIAL_8250_CONSOLE=y
CONFIG_PRINTK=y
CONFIG_EARLY_PRINTK=y
CONFIG_HYPERVISOR_GUEST=y
CONFIG_PVH=y
CONFIG_BLK_DEV_INITRD=y
CONFIG_DEVTMPFS=y
CONFIG_DEVTMPFS_MOUNT=y
CONFIG_BINFMT_ELF=y
CONFIG_BINFMT_SCRIPT=y
EOF
      NIXGG_BYPASS=1 make olddefconfig
      NIXGG_BYPASS=1 make prepare
      # A real x86_64 build unconditionally selects HAVE_OBJTOOL
      # (arch/x86/Kconfig's `select HAVE_OBJTOOL if X86_64`), unlike the
      # accidental 32-bit build this fixture produced before ARCH=x86_64
      # was added above — objtool now runs on every object. It's
      # lowercase and assigned with `:=` in scripts/Makefile.lib with no
      # `override`, so a command-line assignment wins (same override
      # tests/kernel/kernel.nix already uses). objtool is built by
      # `make prepare`, so NIXGG_REAL_OBJTOOL is set after it exists.
      export NIXGG_REAL_OBJTOOL="$PWD/tools/objtool/objtool"
      # objtool's own build embeds DWARF debug info naming the absolute
      # build directory (comp_dir) — /build/source under the sandbox's
      # fixed chroot, a fresh mktemp'd path every native-mode run.
      # Since this binary becomes an input to every ot-*.o.drv, that
      # divergence alone would make every one of them hash differently
      # between modes even though objtool's actual behavior (and the
      # rewritten .o output) is identical. Confirmed byte-for-byte via
      # `strip --strip-debug`: stripped copies from both modes are
      # cmp-identical. Stripping here, once, before any object is
      # rewritten, removes the only source of that non-determinism.
      strip --strip-debug "$NIXGG_REAL_OBJTOOL"
      make -j"$NIX_BUILD_CORES" \
        objtool=${nixggBin}/shims/objtool \
        KCFLAGS="-Wno-error=unterminated-string-initialization -std=gnu11" \
        usr/built-in.a certs/built-in.a ipc/built-in.a sound/built-in.a \
        samples/built-in.a crypto/built-in.a virt/built-in.a security/built-in.a \
        init/built-in.a fs/built-in.a drivers/built-in.a kernel/built-in.a \
        mm/built-in.a lib/built-in.a lib/lib.a \
        arch/x86/crypto/built-in.a arch/x86/realmode/built-in.a arch/x86/net/built-in.a \
        arch/x86/platform/built-in.a arch/x86/virt/built-in.a arch/x86/events/built-in.a \
        arch/x86/mm/built-in.a arch/x86/kernel/built-in.a \
        lib/test_dhry.o
    '';
  };

  # Phase 2: everything phase1 couldn't accelerate — arch/x86/entry/
  # (vdso) and platform/pvh/ (no single-target rule of its own), the
  # two top-level archive folds those poison, and the final link —
  # entirely outside Kbuild's own recursive make for the vmlinux/
  # modpost part. phase1.result* (real, Nix-resolved store paths by
  # the time phase 2's build starts, since they're ordinary
  # buildInputs) are mounted directly and used as-is — phase 2 does
  # not hand the problem back to `make` (which has no idea phase 1
  # already built these pieces; handing it a tree with them present
  # but their upstream inputs, ~2800 objects, absent would make it try
  # to rebuild the world). Instead it symlinks every phase1 result into
  # place, lets `make` build only the small unaccelerated remainder
  # (`arch/x86/built-in.a`'s own prerequisites — entry/, platform/pvh/
  # — plus the two folds), then replicates Kbuild's last few steps by
  # hand using the real tools directly: `ld` (the exact flags
  # scripts/link-vmlinux.sh's cmd_link_vmlinux uses for this config —
  # no kallsyms, no BTF, no arch postlink pass; both off in tinyconfig),
  # then `nm`+mksysmap for System.map, then `sorttable` in place.
  # Verified byte-for-byte reproducible against this fixture's own
  # complete single-phase native-mode build (md5sum-identical vmlinux,
  # modulo BuildID/mtime noise).
  #
  # `make tinyconfig && make prepare` reruns the same cheap pre-pass
  # phase 1 already did to regenerate `scripts/sorttable` fresh (a
  # byproduct of `make prepare`, a hostprogs link never routed through
  # nixgg's own graph even in the single-phase build). `arch/x86/kernel/
  # vmlinux.lds` is not a byproduct of `prepare` — it's only reached
  # during the full recursive descend — so buildCommand names it as
  # its own explicit single-target goal (Kbuild's single-target
  # dispatch handles `%.lds` directly).
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
      phase1.results.usr-a
      phase1.results.certs-a
      phase1.results.ipc-a
      phase1.results.sound-a
      phase1.results.samples-a
      phase1.results.crypto-a
      phase1.results.virt-a
      phase1.results.security-a
      phase1.results.init-a
      phase1.results.fs-a
      phase1.results.drivers-a
      phase1.results.kernel-a
      phase1.results.mm-a
      phase1.results.lib-builtin-a
      phase1.results.lib-a
      phase1.results.arch-crypto-a
      phase1.results.arch-realmode-a
      phase1.results.arch-net-a
      phase1.results.arch-platform-a
      phase1.results.arch-virt-a
      phase1.results.arch-events-a
      phase1.results.arch-mm-a
      phase1.results.arch-kernel-a
    ];
    dontConfigure = true;
    buildPhase = ''
      runHook preBuild

      export KBUILD_BUILD_USER=nixgg
      export KBUILD_BUILD_HOST=nixgg
      export KBUILD_BUILD_TIMESTAMP="Thu Jan  1 00:00:00 UTC 1970"
      export ARCH=x86_64
      make tinyconfig
      cat >> .config <<'EOF'
CONFIG_MODULES=y
CONFIG_RUNTIME_TESTING_MENU=y
CONFIG_TEST_DHRY=m
CONFIG_SAMPLES=y
CONFIG_SAMPLE_KOBJECT=m
CONFIG_TTY=y
CONFIG_SERIAL_8250=y
CONFIG_SERIAL_8250_CONSOLE=y
CONFIG_PRINTK=y
CONFIG_EARLY_PRINTK=y
CONFIG_HYPERVISOR_GUEST=y
CONFIG_PVH=y
CONFIG_BLK_DEV_INITRD=y
CONFIG_DEVTMPFS=y
CONFIG_DEVTMPFS_MOUNT=y
CONFIG_BINFMT_ELF=y
CONFIG_BINFMT_SCRIPT=y
EOF
      make olddefconfig
      make prepare
      # Only reached during the full recursive descend, which phase 2
      # deliberately never runs — Kbuild's single-target dispatch
      # builds vmlinux.lds directly without pulling in anything else.
      make arch/x86/kernel/vmlinux.lds

      ln -s ${phase1.results.usr-a}/lib/built-in.a usr/built-in.a
      ln -s ${phase1.results.certs-a}/lib/built-in.a certs/built-in.a
      ln -s ${phase1.results.ipc-a}/lib/built-in.a ipc/built-in.a
      ln -s ${phase1.results.sound-a}/lib/built-in.a sound/built-in.a
      ln -s ${phase1.results.samples-a}/lib/built-in.a samples/built-in.a
      ln -s ${phase1.results.crypto-a}/lib/built-in.a crypto/built-in.a
      ln -s ${phase1.results.virt-a}/lib/built-in.a virt/built-in.a
      ln -s ${phase1.results.security-a}/lib/built-in.a security/built-in.a
      ln -s ${phase1.results.init-a}/lib/built-in.a init/built-in.a
      ln -s ${phase1.results.fs-a}/lib/built-in.a fs/built-in.a
      ln -s ${phase1.results.drivers-a}/lib/built-in.a drivers/built-in.a
      ln -s ${phase1.results.kernel-a}/lib/built-in.a kernel/built-in.a
      ln -s ${phase1.results.mm-a}/lib/built-in.a mm/built-in.a
      ln -s ${phase1.results.lib-builtin-a}/lib/built-in.a lib/built-in.a
      ln -s ${phase1.results.lib-a}/lib/lib.a lib/lib.a
      mkdir -p arch/x86/lib
      ln -s ${phase1.results.arch-crypto-a}/lib/built-in.a arch/x86/crypto/built-in.a
      ln -s ${phase1.results.arch-realmode-a}/lib/built-in.a arch/x86/realmode/built-in.a
      ln -s ${phase1.results.arch-net-a}/lib/built-in.a arch/x86/net/built-in.a
      ln -s ${phase1.results.arch-platform-a}/lib/built-in.a arch/x86/platform/built-in.a
      ln -s ${phase1.results.arch-virt-a}/lib/built-in.a arch/x86/virt/built-in.a
      ln -s ${phase1.results.arch-events-a}/lib/built-in.a arch/x86/events/built-in.a
      ln -s ${phase1.results.arch-mm-a}/lib/built-in.a arch/x86/mm/built-in.a
      ln -s ${phase1.results.arch-kernel-a}/lib/built-in.a arch/x86/kernel/built-in.a

      # The only unaccelerated compiling left: arch/x86/entry/ (vdso's
      # own directory — see phase1's docstring for why it can't be a
      # target), arch/x86/platform/pvh/ (no single-target rule of its
      # own, confirmed by trying — it rides along as a prerequisite of
      # arch/x86/built-in.a instead), and arch/x86/lib/'s own
      # `lib.a`/`built-in.a` — the top-level `built-in.a`'s recipe
      # reads every `real-obj-y` member, and `arch/x86/lib/built-in.a`
      # (msr.o/msr-reg.o/msr-reg-export.o/hweight.o/iomem.o) is one,
      # confirmed by the final `ar cDPrT vmlinux.a ./built-in.a` step
      # failing with "No such file or directory" when it's missing.
      #
      # `arch/x86/lib/lib.a` and `arch/x86/lib/built-in.a`'s
      # single-target rules genuinely aren't registered on a cold
      # tree — Kbuild reports "No rule to make target" for either one
      # and exits 2, but builds the archive anyway as a side effect of
      # the same invocation (confirmed: both files exist afterward,
      # byte-identical to a clean build). `|| true` on this first
      # call, then requesting them again below — where Kbuild now sees
      # both up to date — turns that into a clean exit.
      make -j"$NIX_BUILD_CORES" \
        KCFLAGS="-Wno-error=unterminated-string-initialization -std=gnu11" \
        arch/x86/lib/lib.a arch/x86/lib/built-in.a || true
      make -j"$NIX_BUILD_CORES" \
        KCFLAGS="-Wno-error=unterminated-string-initialization -std=gnu11" \
        arch/x86/lib/lib.a arch/x86/lib/built-in.a arch/x86/built-in.a built-in.a
      ar cDPrT vmlinux.a ./built-in.a

      ld -m elf_x86_64 -z noexecstack --no-warn-rwx-segments \
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
      ld -m elf_x86_64 -z noexecstack --no-warn-rwx-segments \
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
  linux-kernel-initramfs = initramfs;
}

