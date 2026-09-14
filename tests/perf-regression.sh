#!/usr/bin/env bash
# Regression test: an edit to ONE translation unit only rebuilds that
# TU (and its downstream archive/link), not the whole package.
#
# README.md's "Measured incremental-rebuild cost" table has a
# hand-measured number (openssl, 2/2213 TUs) against a separate flake;
# nothing in this repo re-checks it. This script turns that into an
# automated count on a cheap fixture (lua, 34 TUs), catching a
# regression that widens the invalidation blast radius while still
# passing the binary cutoff checks in tests/configure-cache-cutoff.sh
# and tests/dyndrv-configure-cache-cutoff.sh.
#
# Mechanism: build .#lua once to warm the store, then build the
# fixture's `.package` derivation with one source file edited, `^out`
# (forces the whole outputOf chain: compile drvs -> archive drv ->
# link drv), substituters disabled so a remote build-trace match can't
# quietly satisfy a drv without a `building '...'` line. Assert
# exactly one such line names a `tu-*.drv`, and it is the touched
# file's own object.
#
# Deliberately does NOT assert that ar-liblua.a.drv/bin-lua.drv rebuild
# too — a new drv is trivially registered for them whenever any input
# changes (that's just CA hashing, not a build), and `-Lv`'s progress
# lines don't reliably surface every quick build. The property worth
# guarding is the negative one: every OTHER TU stays a cache hit.
#
# Env knobs:
#   ALT_STORE      root of the alt store (default /tmp/nixgg-perf-store)
#   PATCHED_NIX    path to a builder-rpc-v0-capable nix
#                  (default: ./.patched-nix, built from flake if missing)
#   KEEP_STORE=1   don't wipe ALT_STORE at start (for local iteration)

set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
nixgg_root="$(cd "$here/.." && pwd)"
fixture_nix="$here/perf-regression-fixture.nix"

ALT_STORE="${ALT_STORE:-/tmp/nixgg-perf-store}"
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

echo "==> warming the store: nix build .#lua"
"$PATCHED_NIX/bin/nix" build --no-eval-cache --no-link \
  "$nixgg_root#lua" >/tmp/nixgg-perf-warm.log 2>&1 || {
    echo "warm build failed; see /tmp/nixgg-perf-warm.log" >&2
    tail -10 /tmp/nixgg-perf-warm.log >&2
    exit 1
  }

# rebuilt_tu_drv_names <edited-relative-path>
#
# Instantiates the fixture's `.package` with the given file edited,
# builds its derivation's `^out` with substituters disabled, and
# prints the basename of every tu-*.drv actually realised this run.
rebuilt_tu_drv_names() {
  local edit_path="$1"
  local pkg_drv log

  pkg_drv=$("$PATCHED_NIX/bin/nix-instantiate" --impure \
    --arg flakeDir "$nixgg_root" \
    --argstr edit "$edit_path" \
    -A package "$fixture_nix" 2>/tmp/nixgg-perf-instantiate.log) || {
      echo "  instantiate failed (edit=$edit_path); see /tmp/nixgg-perf-instantiate.log" >&2
      tail -10 /tmp/nixgg-perf-instantiate.log >&2
      return 1
    }

  log="/tmp/nixgg-perf-build-$(basename "$edit_path").log"
  "$PATCHED_NIX/bin/nix" build --no-eval-cache --no-link -Lv \
    --option substituters "" \
    "${pkg_drv}^out" >"$log" 2>&1 || {
      echo "  build failed (edit=$edit_path); see $log" >&2
      tail -10 "$log" >&2
      return 1
    }

  grep -oP "(?<=^building ')[^']*tu-[^']*(?=')" "$log" | xargs -r -n1 basename
}

edit_file="src/lmathlib.c"
echo "==> editing $edit_file, rebuilding through the same warm store"
rebuilt_tus=$(rebuilt_tu_drv_names "$edit_file") || exit 1

echo "  rebuilt TUs:"
printf '    %s\n' $rebuilt_tus

tu_count=$(printf '%s\n' $rebuilt_tus | grep -c '.' || true)

ok=1

if [[ "$tu_count" -ne 1 || "$rebuilt_tus" != *-tu-lmathlib.o.drv ]]; then
  printf '\033[1;31m  FAIL\033[0m expected exactly 1 tu-*.drv rebuild (*-tu-lmathlib.o.drv), got %d: %s\n' \
    "$tu_count" "$(printf '%s ' $rebuilt_tus)" >&2
  ok=0
fi

if [[ "$ok" != "1" ]]; then
  echo "per-TU rebuild-scope verification failed."
  exit 1
fi
printf '\033[1;32m  PASS\033[0m editing %s rebuilt exactly its own TU, all 33 others stayed cache hits\n' \
  "$edit_file"
