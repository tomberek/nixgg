#!/usr/bin/env bash
# CI-friendly QEMU boot smoke test for examples/linux-kernel.
#
# tests/kernel/boot-test.nix (cherry-picked from PR #1) boots a full
# NixOS closure on top of tests/kernel/kernel.nix — 29,268 derivations,
# ~3h, ~31GB from cold; deliberately manual-only, never run in CI. This
# is the CI-friendly counterpart: it reuses examples/linux-kernel (the
# small, already-CI-adjacent tinyconfig fixture) instead, and boots the
# plain `vmlinux` output directly under QEMU rather than assembling a
# bzImage.
#
# No rootfs is provided. examples/linux-kernel/default.nix enables
# CONFIG_HYPERVISOR_GUEST/CONFIG_PVH, which embeds a
# XEN_ELFNOTE_PHYS32_ENTRY note in vmlinux that QEMU's own `-kernel`
# loader reads to boot the raw ELF directly — no bzImage, no real-mode
# setup stage, no compressed decompression stub. (A bzImage was tried
# first and dropped: `make bzImage` unconditionally re-walks the entire
# recursive Kbuild descend regardless of vmlinux's own freshness, so
# phase 2 would have needed ~35 more hand-replicated compiles just to
# reach the boot sector.)
#
# With no CONFIG_BLK_DEV_INITRD and no root= on the cmdline, the kernel
# reaches kernel_init_freeable(), fails to exec any of
# /sbin/init,/etc/init,/bin/init,/bin/sh, and panics with a fixed,
# version-independent message. panic=-1 plus QEMU's -no-reboot turns
# that into a clean process exit instead of a reboot loop.
#
# Two lines in the serial log are the pass condition:
#   "Linux version 6.12.0"       - real entry ran; printk/console live
#   "No working init found"      - reached kernel_init_freeable()
#
# Usage:
#   tests/kernel-boot-smoke.sh
#   PATCHED_NIX=/path/to/patched-nix tests/kernel-boot-smoke.sh
#   ALT_STORE=/tmp/my-store tests/kernel-boot-smoke.sh

set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
nixgg_root="$(cd "$here/.." && pwd)"

ALT_STORE="${ALT_STORE:-/tmp/nixgg-kernel-boot-smoke-store}"
PATCHED_NIX="${PATCHED_NIX:-$nixgg_root/.patched-nix}"
if [[ ! -x "$PATCHED_NIX/bin/nix" ]]; then
  echo "==> building patched nix (one-time)" >&2
  nix build --no-eval-cache "$nixgg_root#patched-nix" -o "$PATCHED_NIX" >&2 || exit 2
fi
mkdir -p "$ALT_STORE"

export NIX_REMOTE=""
export NIX_CONFIG="
experimental-features = nix-command flakes ca-derivations dynamic-derivations configurable-impure-env
extra-system-features = builder-rpc-v0
store = local?root=$ALT_STORE
"

echo "==> building .#linux-kernel" >&2
build_log="/tmp/nixgg-kernel-boot-smoke-build.log"
# -o (not --no-link) pins a GC root: CI runners are disk-constrained
# enough that Nix's automatic GC can trigger under this build's own
# closure size, and a --no-link result has nothing keeping it alive
# between this build and the QEMU exec below — confirmed directly
# (qemu's own binary vanished mid-script in CI with "No such file or
# directory" despite the build having just reported success).
build_root="/tmp/nixgg-kernel-boot-smoke.result"
out=$("$PATCHED_NIX/bin/nix" build --no-eval-cache -o "$build_root" \
  --print-out-paths "$nixgg_root#linux-kernel" 2>"$build_log")
if [[ -z "$out" ]]; then
  echo "BUILD FAILED; see $build_log:" >&2
  tail -20 "$build_log" >&2
  exit 1
fi

vmlinux="$ALT_STORE$out/vmlinux"
if [[ ! -s "$vmlinux" ]]; then
  echo "MISSING: $out/vmlinux" >&2
  exit 1
fi

echo "==> building nixpkgs#qemu (substituted, one-time)" >&2
qemu_log="/tmp/nixgg-kernel-boot-smoke-qemu.log"
qemu_root="/tmp/nixgg-kernel-boot-smoke-qemu.result"
qemu_out=$("$PATCHED_NIX/bin/nix" build --no-eval-cache -o "$qemu_root" \
  --print-out-paths "nixpkgs#qemu" 2>"$qemu_log")
if [[ -z "$qemu_out" ]]; then
  echo "QEMU BUILD FAILED; see $qemu_log:" >&2
  tail -20 "$qemu_log" >&2
  exit 1
fi
qemu_bin="$ALT_STORE$qemu_out/bin/qemu-system-x86_64"
if [[ ! -x "$qemu_bin" ]]; then
  echo "MISSING: $qemu_bin" >&2
  exit 1
fi

echo "==> booting $vmlinux under QEMU (TCG, no KVM)" >&2
boot_log="/tmp/nixgg-kernel-boot-smoke-boot.log"
timeout 30 "$qemu_bin" \
  -kernel "$vmlinux" \
  -nographic -no-reboot -m 256 \
  -append "console=ttyS0 panic=-1" \
  -serial mon:stdio \
  >"$boot_log" 2>&1

fail=0
if ! grep -q "Linux version 6.12.0" "$boot_log"; then
  echo "FAIL: no 'Linux version 6.12.0' in boot log" >&2
  fail=1
fi
if ! grep -q "No working init found" "$boot_log"; then
  echo "FAIL: no 'No working init found' panic in boot log" >&2
  fail=1
fi

if [[ $fail -ne 0 ]]; then
  echo "boot log ($boot_log):" >&2
  tail -30 "$boot_log" >&2
  exit 1
fi

printf '\033[1;32mOK\033[0m       kernel booted and reached its expected panic\n'
