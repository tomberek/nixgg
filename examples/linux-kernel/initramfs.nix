# Minimal initramfs for tests/kernel-boot-smoke.sh: a single static
# busybox providing /init, packed as a Linux-format (newc) cpio archive.
#
# Deliberately a plain stdenv.mkDerivation, not routed through
# mkNixggBuild/nixgg's own shims — this is unaccelerated packaging, the
# same category as examples/linux-kernel/default.nix's phase2
# hand-replicating Kbuild's last few steps directly.
#
# pkgsStatic.busybox (not the ordinary pkgs.busybox) is required: the
# ordinary build links against glibc, needing glibc's own loader/.so
# closure staged into this tiny rootfs too. pkgsStatic's is a
# genuinely static ELF, so it runs directly once the guest kernel
# unpacks this cpio — no /lib*, no loader, nothing else needed.
{
  stdenv,
  pkgsStatic,
  cpio,
}:
stdenv.mkDerivation {
  pname = "linux-kernel-initramfs";
  version = "1";
  dontUnpack = true;
  nativeBuildInputs = [ cpio ];
  buildPhase = ''
    runHook preBuild

    mkdir -p rootfs/bin
    cp ${pkgsStatic.busybox}/bin/busybox rootfs/bin/busybox
    chmod +w rootfs/bin/busybox
    ln -s busybox rootfs/bin/sh

    # tests/kernel-boot-smoke.sh greps for NIXGG_INIT_OK to confirm a
    # real process ran, then `reboot -f` — not `poweroff -f`, which
    # hangs here: pm_power_off is never registered (no ACPI/platform
    # power-off handler in this tinyconfig build). QEMU's own
    # `-no-reboot` flag turns the reboot into a clean process exit.
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
