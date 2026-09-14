#!/usr/bin/env bash
# Regression test: native and sandbox modes produce byte-identical
# .drv files for the same source.
#
# Runs each fixture two ways:
#
#   sandbox:  build the outer wrapper's own per-target outputs only
#             (never the outputOf-resolved result), which registers
#             every drv but compiles nothing for real, then `nix
#             derivation show -r` on the resolved target paths for
#             the exact drv set.
#   native:   fetch same source, unpack into a tempdir, `nix develop`
#             and run the same buildCommand there. Shims write .nix
#             thunks; nix-instantiate on each thunk → drv path.
#
# Shared alt-store/patched-nix scaffolding, sandbox-drv collection,
# native-src resolution, the native build invocation, and the
# match/mismatch reporting live in tests/lib/drv-equiv-common.sh
# (shared with tests/batch-drv-equivalence.sh and
# tests/thin-archive-equivalence.sh). Only the fixture list is this
# script's own.
#
# Env knobs:
#   ALT_STORE      root of the alt store (default /tmp/nixgg-equiv-store)
#   PATCHED_NIX    path to a builder-rpc-v0-capable nix
#                  (default: ./.patched-nix, built from flake if missing)
#   KEEP_STORE=1   don't wipe ALT_STORE at start (for local iteration)
#   ONLY=name      only run one fixture (hello / lua / fmt / …)

set -euo pipefail

# shellcheck source=lib/drv-equiv-common.sh
source "$(cd "$(dirname "$0")" && pwd)/lib/drv-equiv-common.sh"

equiv_common_setup "/tmp/nixgg-equiv-store"

# Fixtures. Each entry is:
#
#   attr | native-src-flake-input | native-src-subdir
#
# - attr:        the flake attribute to `nix build`
# - native-src:  either a flake-input attr name (e.g. "lua-src") for
#                the same tarball the sandbox pulls in, or "example"
#                (path-relative to nixgg_root) for the local example
# - native-src-subdir: cd into this dir inside the unpacked src
#                (empty = src root)
#
# The build command comes from the flake itself:
# `.#$attr-shell.passthru.buildCommand` — same string mkNixggBuild
# passes to buildPhase. No per-fixture logic lives in this script.
run_fixture() {
  local attr="$1" src_input="$2" subdir="$3"
  local label="$attr"

  echo
  printf '\033[1;36m===== %s =====\033[0m\n' "$label"

  printf '==> sandbox: nix build .#%s (wrapper only) + derivation show -r\n' "$attr"
  local sb_drvs
  sb_drvs=$(equiv_sandbox_drvs "$attr") || return 1

  local workdir="$(mktemp -d)"
  local src
  src=$(equiv_resolve_native_src "$src_input") || true
  if [[ -z "$src" || ! -e "$src" ]]; then
    echo "could not resolve native src for $attr (input=$src_input)" >&2
    return 1
  fi

  local nt_log="/tmp/nixgg-equiv-$attr-native.log"
  equiv_native_build "$attr" "$subdir" "$workdir" "$nt_log" || {
    rm -rf "$workdir"
    return 1
  }

  local thunk_files
  thunk_files=$(equiv_collect_thunks "$workdir")
  if [[ -z "$thunk_files" ]]; then
    echo "native build produced no thunks; see $nt_log" >&2
    rm -rf "$workdir"
    return 1
  fi

  local nt_drvs
  nt_drvs=$(while IFS= read -r t; do
    equiv_thunk_drvpath "$t"
  done <<<"$thunk_files" | sort -u)

  local n_both
  if ! n_both=$(equiv_report_sets "$label" "drvs" "$sb_drvs" "$nt_drvs"); then
    rm -rf "$workdir"
    return 1
  fi
  printf '\033[1;32mMATCH\033[0m    %s (%d drvs)\n' "$label" "$n_both"
  rm -rf "$workdir"
}

fail=0

if [[ -z "${ONLY:-}" || "$ONLY" == "hello" ]]; then
  run_fixture "hello" "example" "" || fail=1
fi
if [[ -z "${ONLY:-}" || "$ONLY" == "lua" ]]; then
  run_fixture "lua" "lua-src" "" || fail=1
fi
if [[ -z "${ONLY:-}" || "$ONLY" == "fmt" ]]; then
  run_fixture "fmt" "fmt-src" "" || fail=1
fi
if [[ -z "${ONLY:-}" || "$ONLY" == "mosh" ]]; then
  run_fixture "mosh" "mosh-src" "" || fail=1
fi
if [[ -z "${ONLY:-}" || "$ONLY" == "gcc" ]]; then
  run_fixture "gcc" "gcc-src" "" || fail=1
fi
# linux-kernel is two-phase (phase1 = mkNixggBuild, phase2 = a plain
# stdenv.mkDerivation with zero nixgg shims). Only phase1 fits
# run_fixture's shape, exposed as "linux-kernel-phase1". Not in the
# default set — real linux-6.12 source, ~2800 real compiles natively,
# several minutes even on a warm cache; opt in via
# ONLY=linux-kernel-phase1.
if [[ "${ONLY:-}" == "linux-kernel-phase1" ]]; then
  run_fixture "linux-kernel-phase1" "linux-src" "" || fail=1
fi
# nix-full builds ALL of Nix itself (24 real Meson subprojects/
# targets, ~700 TUs) from the nix-15793 flake input — real, but far
# too slow for the default set (many minutes even warm). Opt in via
# ONLY=nix-full.
if [[ "${ONLY:-}" == "nix-full" ]]; then
  run_fixture "nix-full" "nix-15793" "" || fail=1
fi

echo
if (( fail )); then
  echo "some fixtures diverged."
  exit 1
fi
echo "all fixtures equivalent."
