#!/usr/bin/env bash
# Regression test: splitStdenv's (splitAtConfigure=true) early-cutoff
# actually holds.
#
# For each fixture (hello: autotools, fmt: cmake), builds the
# configure stage (the *-configure-<version> derivation) three ways
# and compares its ggtree OUTPUT PATH:
#
#   baseline   — real, unedited src
#   excluded   — src edited at a file configureSrcFilter's
#                includePatterns/existenceStubs DON'T cover
#   included   — src edited at a file the filter DOES cover
#
# excluded must match baseline (edit never reaches configure's input
# content); included must differ (negative control — if it also
# matched, the filter would be excluding everything).
#
# Env knobs:
#   ALT_STORE      root of the alt store (default /tmp/nixgg-cutoff-store)
#   PATCHED_NIX    path to a builder-rpc-v0-capable nix
#                  (default: ./.patched-nix, built from flake if missing)
#   KEEP_STORE=1   don't wipe ALT_STORE at start (for local iteration)
#   ONLY=name      only run one fixture (hello / fmt)

set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
nixgg_root="$(cd "$here/.." && pwd)"
fixture_nix="$here/configure-cache-cutoff-fixture.nix"

ALT_STORE="${ALT_STORE:-/tmp/nixgg-cutoff-store}"
PATCHED_NIX="${PATCHED_NIX:-$nixgg_root/.patched-nix}"
if [[ ! -x "$PATCHED_NIX/bin/nix" ]]; then
  echo "==> building patched nix (one-time; substituted from cache)" >&2
  nix build --no-eval-cache "$nixgg_root#patched-nix" -o "$PATCHED_NIX" >&2 || exit 2
fi

if [[ "${KEEP_STORE:-}" != "1" ]]; then
  chmod -R u+w "$ALT_STORE" 2>/dev/null || true
  rm -rf "$ALT_STORE"
fi
mkdir -p "$ALT_STORE"

export NIX_REMOTE=""
export NIX_CONFIG="
experimental-features = nix-command flakes ca-derivations dynamic-derivations configurable-impure-env
extra-system-features = builder-rpc-v0
store = local?root=$ALT_STORE
"

# configureStage_ggtree_path <edit-arg>
#
# Instantiates the fixture with the given edit, extracts the configure
# stage's own derivation, builds ONLY its "ggtree" output, and prints
# the resulting real store path.
configureStage_ggtree_path() {
  local edit_arg="$1"
  local outer_drv configure_stage_drv

  outer_drv=$("$PATCHED_NIX/bin/nix-instantiate" --impure \
    --arg flakeDir "$nixgg_root" \
    --argstr fixture "$fixture" \
    ${edit_arg:+--argstr edit "$edit_arg"} \
    "$fixture_nix" 2>/tmp/nixgg-cutoff-instantiate.log) || {
      echo "  instantiate failed (edit=$edit_arg); see /tmp/nixgg-cutoff-instantiate.log" >&2
      tail -10 /tmp/nixgg-cutoff-instantiate.log >&2
      return 1
    }

  configure_stage_drv=$("$PATCHED_NIX/bin/nix" show-derivation "$outer_drv" 2>/dev/null \
    | python3 -c "
import json, sys
d = json.load(sys.stdin)
info = list(d['derivations'].values())[0]
matches = [k for k in info['inputs']['drvs'] if '-configure-' in k]
print(matches[0] if matches else '')
")
  if [[ -z "$configure_stage_drv" ]]; then
    echo "  could not find configure stage derivation (edit=$edit_arg)" >&2
    return 1
  fi

  "$PATCHED_NIX/bin/nix" build --no-eval-cache --no-link --print-out-paths \
    "/nix/store/$configure_stage_drv^ggtree" 2>/tmp/nixgg-cutoff-build.log | tail -1 || {
      echo "  configure stage build failed (edit=$edit_arg); see /tmp/nixgg-cutoff-build.log" >&2
      tail -10 /tmp/nixgg-cutoff-build.log >&2
      return 1
    }
}

run_fixture() {
  local fixture="$1"
  printf '\033[1;36m===== %s =====\033[0m\n' "$fixture"

  local baseline excluded included
  baseline=$(configureStage_ggtree_path "") || return 1
  echo "  baseline: $baseline"

  excluded=$(configureStage_ggtree_path "excluded") || return 1
  echo "  excluded: $excluded"

  included=$(configureStage_ggtree_path "included") || return 1
  echo "  included: $included"

  local ok=1
  if [[ "$excluded" != "$baseline" ]]; then
    printf '\033[1;31m  FAIL\033[0m excluded-file edit changed configure stage output — early-cutoff broken\n' >&2
    ok=0
  fi
  if [[ "$included" == "$baseline" ]]; then
    printf '\033[1;31m  FAIL\033[0m included-file edit did NOT change configure stage output — filter not discriminating (excludes everything?)\n' >&2
    ok=0
  fi

  if [[ "$ok" == "1" ]]; then
    printf '\033[1;32m  PASS\033[0m %s: excluded-edit cached, included-edit invalidated\n' "$fixture"
    return 0
  fi
  return 1
}

fail=0
for fixture in hello fmt; do
  if [[ -z "${ONLY:-}" || "$ONLY" == "$fixture" ]]; then
    run_fixture "$fixture" || fail=1
    echo
  fi
done

if (( fail )); then
  echo "some fixtures failed early-cutoff verification."
  exit 1
fi
echo "all fixtures show correct early-cutoff behavior."
