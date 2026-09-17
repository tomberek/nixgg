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
# examples/linux-kernel/default.nix enables CONFIG_HYPERVISOR_GUEST/
# CONFIG_PVH, which embeds a XEN_ELFNOTE_PHYS32_ENTRY note in vmlinux
# that QEMU's own `-kernel` loader reads to boot the raw ELF directly —
# no bzImage, no real-mode setup stage, no compressed decompression
# stub. (A bzImage was tried first and dropped: `make bzImage`
# unconditionally re-walks the entire recursive Kbuild descend
# regardless of vmlinux's own freshness, so phase 2 would have needed
# ~35 more hand-replicated compiles just to reach the boot sector.)
#
# The kernel boots to a REAL userspace process, not just its own entry
# point: examples/linux-kernel/default.nix's linux-kernel-initramfs
# output (a single static busybox, built by examples/linux-kernel/
# initramfs.nix) is passed via `-initrd`. Its /init script echoes a
# fixed marker string then reboots, so a clean QEMU exit (not a
# timeout, not a panic) is itself part of the pass condition — proof a
# real program ran under nixgg's own kernel build, not just that the
# kernel's own printk/start_kernel path executed.
#
# Usage:
#   tests/kernel-boot-smoke.sh
#   PATCHED_NIX=/path/to/patched-nix tests/kernel-boot-smoke.sh
#   ALT_STORE=/tmp/my-store tests/kernel-boot-smoke.sh
#
# QEMU itself is run via `nix shell nixpkgs#qemu --command
# qemu-system-x86_64 ...` rather than a bare exec of its predicted
# on-disk path: qemu-system-x86_64 is a dynamically-linked ELF, and its
# interpreter/RPATH entries are resolved by the KERNEL's own ELF loader
# against the real filesystem root, which never has $ALT_STORE's
# closure — a bare exec fails "No such file or directory" for a reason
# unrelated to the build. `nix shell --command` runs inside Nix's own
# private mount-namespace bind mount of $ALT_STORE onto /nix/store, the
# same mechanism tests/smoke.sh's own `nix run` fallback relies on for
# exactly this reason (see its check_example's own comment). The
# initramfs cpio itself needs no such detour — QEMU only ever reads
# its bytes, never execs it on the host — so it uses the same plain
# $ALT_STORE-prefixed path vmlinux already does.

set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
nixgg_root="$(cd "$here/.." && pwd)"

ALT_STORE="${ALT_STORE:-/tmp/nixgg-kernel-boot-smoke-store}"
PATCHED_NIX="${PATCHED_NIX:-$nixgg_root/.patched-nix}"
if [[ ! -x "$PATCHED_NIX/bin/nix" ]]; then
  echo "==> building patched nix (one-time)" >&2
  nix build --no-eval-cache --accept-flake-config "$nixgg_root#patched-nix" -o "$PATCHED_NIX" >&2 || exit 2
fi
mkdir -p "$ALT_STORE"

export NIX_REMOTE=""
export NIX_CONFIG="
experimental-features = nix-command flakes ca-derivations dynamic-derivations configurable-impure-env
extra-system-features = builder-rpc-v0
store = local?root=$ALT_STORE
"

# build_flake_attr <attr> <result-root> <log-file>
#
# Builds "$nixgg_root#$attr" with a real GC root (-o, not --no-link —
# a --no-link result has nothing keeping it alive between this build
# and the QEMU run below) and echoes the printed out-path, or prints
# the log's tail and returns non-zero. Callers prefix the echoed path
# with $ALT_STORE to get the real on-disk path.
build_flake_attr() {
  local attr="$1" result_root="$2" log="$3"
  local out
  if ! out=$("$PATCHED_NIX/bin/nix" build --no-eval-cache -o "$result_root" \
       --print-out-paths "$nixgg_root#$attr" 2>"$log") || [[ -z "$out" ]]; then
    echo "BUILD FAILED: $attr; see $log:" >&2
    tail -20 "$log" >&2
    return 1
  fi
  echo "$out"
}

echo "==> building .#linux-kernel" >&2
out=$(build_flake_attr linux-kernel /tmp/nixgg-kernel-boot-smoke.result \
  /tmp/nixgg-kernel-boot-smoke-build.log) || exit 1
vmlinux="$ALT_STORE$out/vmlinux"
if [[ ! -s "$vmlinux" ]]; then
  echo "MISSING: $out/vmlinux" >&2
  exit 1
fi

echo "==> building .#linux-kernel-initramfs" >&2
initramfs_out=$(build_flake_attr linux-kernel-initramfs \
  /tmp/nixgg-kernel-boot-smoke-initramfs.result \
  /tmp/nixgg-kernel-boot-smoke-initramfs.log) || exit 1
initramfs="$ALT_STORE$initramfs_out/initramfs.cpio"
if [[ ! -s "$initramfs" ]]; then
  echo "MISSING: $initramfs_out/initramfs.cpio" >&2
  exit 1
fi

echo "==> booting $vmlinux under QEMU (TCG, no KVM)" >&2
boot_log="/tmp/nixgg-kernel-boot-smoke-boot.log"
qemu_log="/tmp/nixgg-kernel-boot-smoke-qemu.log"
"$PATCHED_NIX/bin/nix" shell --no-eval-cache nixpkgs#qemu --command \
  timeout 30 qemu-system-x86_64 \
  -kernel "$out/vmlinux" \
  -initrd "$initramfs" \
  -nographic -no-reboot -m 256 \
  -append "console=ttyS0 panic=-1" \
  -serial mon:stdio \
  >"$boot_log" 2>"$qemu_log"
qemu_status=$?
if [[ $qemu_status -ne 0 ]]; then
  echo "FAIL: qemu exited $qemu_status (want 0 — a clean reboot); see $qemu_log:" >&2
  tail -20 "$qemu_log" >&2
  echo "boot log ($boot_log):" >&2
  tail -30 "$boot_log" >&2
  exit 1
fi

fail=0
if ! grep -q "Linux version 6.12.0" "$boot_log"; then
  echo "FAIL: no 'Linux version 6.12.0' in boot log" >&2
  fail=1
fi
if ! grep -q "NIXGG_INIT_OK" "$boot_log"; then
  echo "FAIL: no 'NIXGG_INIT_OK' marker in boot log — /init never ran" >&2
  fail=1
fi

if [[ $fail -ne 0 ]]; then
  echo "boot log ($boot_log):" >&2
  tail -30 "$boot_log" >&2
  exit 1
fi

printf '\033[1;32mOK\033[0m       kernel booted, ran /init, and powered off cleanly\n'
