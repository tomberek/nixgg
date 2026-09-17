# Minimal initramfs for tests/kernel-boot-smoke.sh: a single static
# busybox providing /init, packed as a Linux-format (newc) cpio archive.
#
# Deliberately a plain stdenv.mkDerivation, not routed through
# mkNixggBuild/nixgg's own shims — this is unaccelerated packaging (one
# `cp`, one `cpio` invocation), the same category as
# examples/linux-kernel/default.nix's phase2 hand-replicating Kbuild's
# last few steps directly.
#
# pkgsStatic.busybox (not the ordinary pkgs.busybox) is required: the
# ordinary build links against glibc, needing glibc's own loader/.so
# closure staged into this tiny rootfs too. pkgsStatic's is a genuinely
# static ELF (confirmed via `file`: "statically linked", no PT_INTERP),
# so it runs directly once the guest kernel unpacks this cpio into its
# own initramfs tmpfs — no /lib*, no loader, nothing else needed.
{
  stdenv,
  pkgsStatic,
  cpio,
}:
let
  busybox = pkgsStatic.busybox;
in
stdenv.mkDerivation {
  pname = "linux-kernel-initramfs";
  version = "1";
  dontUnpack = true;
  nativeBuildInputs = [ cpio ];
  buildPhase = ''
    runHook preBuild

    mkdir -p rootfs/bin rootfs/dev rootfs/proc rootfs/sys
    cp ${busybox}/bin/busybox rootfs/bin/busybox
    chmod +w rootfs/bin/busybox
    ln -s busybox rootfs/bin/sh

    # tests/kernel-boot-smoke.sh greps the boot log for NIXGG_INIT_OK —
    # a real userspace process running under nixgg's own kernel build,
    # not just the kernel's own entry point — then reboots so QEMU
    # exits 0 instead of needing the timeout safety net. `poweroff -f`
    # was tried first and doesn't work here: pm_power_off is never
    # registered (no ACPI/platform power-off handler in this tinyconfig
    # build), so the kernel just halts instead ("Power off not
    # available: System halted instead"), hanging QEMU until the
    # timeout. `reboot -f` (LINUX_REBOOT_CMD_RESTART) always works —
    # QEMU's own `-no-reboot` flag (tests/kernel-boot-smoke.sh's own
    # invocation) turns that into a clean process exit instead of an
    # actual reboot loop.
cat > rootfs/init <<'EOF'
#!/bin/busybox sh
echo NIXGG_INIT_OK
/bin/busybox reboot -f
EOF
    chmod +x rootfs/init

    ( cd rootfs && find . -print0 | cpio --null -o --format=newc ) > initramfs.cpio

    runHook postBuild
  '';
  installPhase = ''
    runHook preInstall
    mkdir -p "$out"
    cp initramfs.cpio "$out/"
    runHook postInstall
  '';
}
