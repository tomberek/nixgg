#!/usr/bin/env bash
# Regression test for shared (symlink-farm) staging.
#
# With sharedStaging, a staged source tree holds no file contents — every
# entry is a symlink into a content-addressed store object, so the same
# header is stored once instead of once per translation unit — several
# times smaller on examples/gcc, and the ratio grows with TU count.
#
# That makes the farm's dependency on its headers INDIRECT: the NAR contains
# target path strings, not bytes. Nix only turns those strings into real
# dependency edges because the shim store-adds farms with `--scan`, which
# records symlink targets as references.
#
# What happens if --scan is dropped is worth being precise about, because
# the obvious guess is wrong. It does NOT produce a farm that works locally
# and breaks on a fresh machine: a compile derivation's sandbox mounts only
# the closure of its inputs, so unreferenced targets are absent even in the
# store that just created them, and the compile fails at once with
#
#   cc1plus: fatal error: util.cc: No such file or directory
#
# Verified by mutation — removing --scan from StoreAddScan fails the hello
# build immediately. So the reference edge is enforced by Nix itself, not
# merely relied upon.
#
# The gap this test fills is therefore narrower, and real: NOTHING ELSE in
# the suite turns sharedStaging on. drv-equivalence and smoke both run with
# it off (it is opt-in precisely because flipping it changes every drv
# hash), so absent this script a regression anywhere in the shared-staging
# path is invisible. It also turns an opaque missing-header error into a
# named diagnosis.
#
# It has to build for real: `nix store add --scan` only works inside a
# recursive-nix derivation builder, so no Go test can reach it. The hashing
# half of the scheme — that a header edit changes the farm's hash — is
# pinned by TestSourcesSharedHashTracksContent, which can.
#
# Env knobs:
#   SHARED_STORE   root of the store to build in
#                  (default /tmp/nixgg-shared-store; NOT drv-equivalence's
#                  ALT_STORE, which that script wipes on every run)
#   PATCHED_NIX    path to a builder-rpc-v0-capable nix
#   EXAMPLE        example to build (default hello)
#   KEEP_STORE=1   don't wipe the store at start

set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
nixgg_root="$(cd "$here/.." && pwd)"

SHARED_STORE="${SHARED_STORE:-/tmp/nixgg-shared-store}"
PATCHED_NIX="${PATCHED_NIX:-$nixgg_root/.patched-nix}"
EXAMPLE="${EXAMPLE:-hello}"

if [[ ! -x "$PATCHED_NIX/bin/nix" ]]; then
  echo "==> building patched nix (one-time; substituted from cache)" >&2
  nix build --no-eval-cache "$nixgg_root#patched-nix" -o "$PATCHED_NIX" >&2 || {
    echo "failed to build $nixgg_root#patched-nix" >&2
    exit 2
  }
fi
nix_bin="$PATCHED_NIX/bin/nix"

if [[ "${KEEP_STORE:-}" != "1" ]]; then
  chmod -R u+w "$SHARED_STORE" 2>/dev/null || true
  rm -rf "$SHARED_STORE"
fi
mkdir -p "$SHARED_STORE"

export NIX_REMOTE=""
export NIX_CONFIG="
experimental-features = nix-command flakes ca-derivations dynamic-derivations configurable-impure-env
extra-system-features = builder-rpc-v0
store = local?root=$SHARED_STORE
"

if [[ ! -e "$SHARED_STORE/$(readlink -f "$PATCHED_NIX")" ]]; then
  "$nix_bin" copy --from daemon --to "local?root=$SHARED_STORE" \
      --no-check-sigs "$(readlink -f "$PATCHED_NIX")" >/dev/null 2>&1 || true
fi

# `.#<name>-shared` is the shipped example definition with sharedStaging
# flipped on (see flake.nix), so this tests the same build everything else
# does rather than a recipe written for the test.
echo "==> building $EXAMPLE-shared into $SHARED_STORE" >&2
"$nix_bin" build --no-eval-cache --no-link "$nixgg_root#$EXAMPLE-shared" >&2

# ---------------------------------------------------------------
# Inspect every staged farm.
#
# Identify them structurally rather than by name, so a naming change
# doesn't silently skip the check — but the discriminator has to be
# EXACT, not merely sufficient. "Store path containing symlinks" is not:
# it matches ncurses (1060 links), glibc, coreutils and most of stdenv,
# whose references are of course correct, so the check passed on nixpkgs
# paths while never looking at a farm at all.
#
# A staged farm is the only thing in the store with symlinks and ZERO
# regular files — that is precisely what shared staging produces. On
# hello this selects exactly the two TU trees (main, util) and nothing
# else.
# ---------------------------------------------------------------
store_dir="$SHARED_STORE/nix/store"
farms=0
links=0
bad=0

while IFS= read -r farm; do
  [[ -n "$(find "$farm" -type f -print -quit 2>/dev/null)" ]] && continue
  mapfile -t targets < <(find "$farm" -type l -exec readlink {} \; 2>/dev/null \
    | grep '^/nix/store/' | sort -u)
  [[ ${#targets[@]} -eq 0 ]] && continue

  farms=$((farms + 1))
  echo "  farm $(basename "$farm") (${#targets[@]} targets)" >&2
  logical="/nix/store/$(basename "$farm")"
  mapfile -t closure < <("$nix_bin" path-info -r "$logical" 2>/dev/null)

  for t in "${targets[@]}"; do
    links=$((links + 1))
    # A target may itself be a symlink chain; compare on the store path
    # root, which is what references record.
    root="/nix/store/$(echo "${t#/nix/store/}" | cut -d/ -f1)"
    if ! printf '%s\n' "${closure[@]}" | grep -qxF "$root"; then
      echo "FAIL: $logical symlinks to $root but does not reference it" >&2
      bad=$((bad + 1))
    fi
  done
done < <(find "$store_dir" -maxdepth 1 -mindepth 1 -type d 2>/dev/null)

echo
if [[ $farms -eq 0 ]]; then
  echo "FAIL: no staged farms found in $SHARED_STORE" >&2
  echo "      sharedStaging did not take effect, so this test proved nothing." >&2
  exit 1
fi

echo "checked $farms staged farms, $links symlink targets"
if [[ $bad -gt 0 ]]; then
  echo "FAIL: $bad symlink target(s) missing from their farm's closure" >&2
  echo "      Substituting these farms onto a fresh machine yields dangling" >&2
  echo "      symlinks. Check that StoreAddScan still passes --scan." >&2
  exit 1
fi
echo "OK: every staged farm references all of its symlink targets"
