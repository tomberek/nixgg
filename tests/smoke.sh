#!/usr/bin/env bash
# Smoke test: every example still builds, and its artifact is where we
# say it is and actually runs.
#
# This exists because tests/drv-equivalence.sh structurally cannot catch
# a whole class of bug. That test compares drv HASHES between native and
# sandbox mode; it never realises an output and never reads one. So when
# link outputs moved to $out/bin, it reported a clean 149/149 while every
# native build failed to collect its artifact and two phase-chaining
# examples referenced tool paths that no longer existed.
#
# It also covers the four examples drv-equivalence leaves out. That
# omission is structural, not an oversight: the gate resolves each
# fixture's native source from a single flake INPUT, and llvm/two-phase
# have no single `src` — they are multi-phase, one source per phase.
#
# Cheap by default (EXAMPLES=quick, ~2 min). The expensive ones are
# opt-in because llvm alone is ~1500 TUs. The splitAtBuild-only
# examples (hello-dyndrv/mosh-dyndrv/zstd-dyndrv) and the
# splitAtConfigure-only examples (hello-cache/zstd-cache) always run
# alongside QUICK/SLOW — see DYNDRV/CONFIGCACHE below for why they
# need `nix run` instead of a direct exec.
#
# Usage:
#   tests/smoke.sh                 # quick set + dyndrv/cache examples
#   EXAMPLES=all tests/smoke.sh    # everything, including llvm (~1h+)
#   EXAMPLES="hello gcc" tests/smoke.sh
#   ALT_STORE=/tmp/my-store tests/smoke.sh

set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
nixgg_root="$(cd "$here/.." && pwd)"

ALT_STORE="${ALT_STORE:-/tmp/nixgg-smoke-store}"
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

# attr | expected path inside $out | how to prove it works
#
# The expected path is the point of this test: it encodes the FHS layout
# (link -> bin/, ar -> lib/) so a change to output placement that forgets
# a consumer fails here loudly instead of in a user's terminal.
#
# "-" as the run command means the artifact is a library: existing at the
# right path with non-zero size is all we assert.
QUICK=(
  "hello|bin/hello|%s"
  "two-phase|bin/app|%s"
  "fmt|lib/libfmt.a|-"
  "lua|bin/lua|%s -v"
  "gcc|lib/libiberty.a|-"
  "rustc|lib/libapp.a|-"
  "mosh|bin/mosh-server|%s --version"
  "thin-archive|bin/thin-archive|%s"
)
SLOW=(
  # Not in QUICK because it needs the kernel's `dev` output — a ~3GB
  # closure — which is a poor fit for a set advertised as "cheap by
  # default, ~2 min".
  #
  # The version in this path tracks linuxPackages.kernel.modDirVersion
  # from the flake pin, so a `nix flake update` that bumps the kernel
  # needs this string bumped too. Kept explicit rather than globbed
  # because the whole point of the `want` column is to pin the exact
  # FHS location, and depmod cares that it is lib/modules/<ver>/.
  "kmod|lib/modules/6.18.41/extra/hello_mod.ko|-"
  "redis|bin/redis-server|%s --version"
  "ffmpeg|bin/ffmpeg_g|%s -version"
  "llvm|bin/llc|%s --version"
  "postgresql|bin/postgres|%s --version"
  "qemu|bin/qemu-system-x86_64|%s --version"
)

# splitAtBuild-only examples (nix/splitStdenv.nix) — an existing
# nixpkgs package wrapped verbatim, not a mkNixggBuild fixture.
# Structurally different bug class than the ones above: these exercise
# multi-output splitting and self-rpath rewriting across the
# build/install split, neither of which a single-target mkNixggBuild
# fixture can trigger. zstd-dyndrv's default output is "bin" (nixpkgs'
# outputsToInstall), which is exactly the output that multi-output
# collapse and rpath corruption hit — keep it pinned here rather than
# testing "out".
#
# Verified via `nix run`/`nix shell` ONLY, never a direct exec of the
# predicted $ALT_STORE path (unlike QUICK/SLOW below): these binaries'
# baked RPATH/RUNPATH entries point at real (non-fixed-output) sibling
# store paths — e.g. zstd-dyndrv's bin/zstd needs its own "out"
# output's libzstd.so.1 — that exist only inside $ALT_STORE, not at
# the real filesystem root. The dynamic loader always resolves an
# absolute RPATH against the real root, so direct exec of the disk
# path fails with "cannot open shared object file" even on a
# perfectly correct build; only `nix run`'s private mount namespace
# (which bind-mounts $ALT_STORE onto /nix/store) resolves it. Confirmed
# directly against both this fix and the unmodified upstream mosh/lua
# mkNixggBuild examples above, whose only runtime dep is glibc — glibc
# happens to already exist at the same path outside the alt store too
# (fixed-output derivation), which is why QUICK/SLOW's direct-exec
# check has never caught this class of bug before.
DYNDRV=(
  "hello-dyndrv|bin/hello|%s"
  "mosh-dyndrv|bin/mosh-server|%s --version"
  "zstd-dyndrv|bin/zstd|%s --version"
)

# splitAtConfigure-only examples (nix/splitStdenv.nix) — same "wrap an
# existing nixpkgs package" story as DYNDRV above, but split at the
# configure/build boundary instead of build/install, and with no
# sandbox/shims/RPC at all. Same `nix run`-only verification rationale
# as DYNDRV: zstd-cache's multi-output bin/zstd needs its own sibling
# "out" output's libzstd.so.1, which exists only inside $ALT_STORE.
# hello-cache-filtered and fmt-cache-filtered additionally exercise
# configureSrcFilter's real early-cutoff win (shrinking the configure
# stage's own `src` input so an edit outside the filtered set never
# re-runs configure) — this smoke test only covers "does it still
# build and run correctly", not the caching behavior itself. See
# tests/configure-cache-cutoff.sh for the automated check of the
# caching mechanism itself.
CONFIGCACHE=(
  "hello-cache|bin/hello|%s"
  "zstd-cache|bin/zstd|%s --version"
  "hello-cache-filtered|bin/hello|%s"
  "fmt-cache-filtered|lib/libfmt.so.12.1.0|-"
)

# splitAtConfigure+splitAtBuild examples (nix/splitStdenv.nix) —
# combines DYNDRV's per-TU sandboxed acceleration with CONFIGCACHE's
# configure-step early-cutoff, splitting into three stages instead of
# two. Same `nix run`-only rationale as DYNDRV: the final restored
# tree needs sibling outputs from inside $ALT_STORE.
DYNCONFIGCACHE=(
  "hello-dyndrv-configure-cached|bin/hello|%s"
  "mosh-dyndrv-configure-cached|bin/mosh-server|%s --version"
  "zstd-dyndrv-configure-cached|bin/zstd|%s --version"
  "gdbm-dyndrv-configure-cached|bin/gdbmtool|%s --version"
)

case "${EXAMPLES:-quick}" in
  quick) SET=("${QUICK[@]}"); DYNSET=("${DYNDRV[@]}" "${CONFIGCACHE[@]}" "${DYNCONFIGCACHE[@]}") ;;
  all)   SET=("${QUICK[@]}" "${SLOW[@]}"); DYNSET=("${DYNDRV[@]}" "${CONFIGCACHE[@]}" "${DYNCONFIGCACHE[@]}") ;;
  *)     SET=(); DYNSET=()
         for want in ${EXAMPLES}; do
           for e in "${QUICK[@]}" "${SLOW[@]}"; do
             [[ "${e%%|*}" == "$want" ]] && SET+=("$e")
           done
           for e in "${DYNDRV[@]}" "${CONFIGCACHE[@]}" "${DYNCONFIGCACHE[@]}"; do
             [[ "${e%%|*}" == "$want" ]] && DYNSET+=("$e")
           done
         done
         if [[ ${#SET[@]} -eq 0 && ${#DYNSET[@]} -eq 0 ]]; then
           echo "no known examples in EXAMPLES='$EXAMPLES'" >&2; exit 2
         fi ;;
esac

fail=0

# direct_exec=1: also exec $ALT_STORE's on-disk path directly, matching
# QUICK/SLOW's original behavior. direct_exec=0: DYNDRV's path — skip
# straight to `nix run`/`nix shell`, see DYNDRV's own comment above for
# why a direct exec is expected to fail there regardless of build
# correctness.
check_example() {
  local attr="$1" want="$2" run="$3" direct_exec="$4"
  printf '\033[1;36m===== %s =====\033[0m\n' "$attr"

  # Cheap (no-build) check: mainProgram must be set iff the artifact is
  # meant to be run. A library incorrectly claiming a mainProgram would
  # send `nix run` at a nonsense path; a binary missing one degrades
  # silently, since Nix's own pname fallback happens to paper over it
  # for every current example (pname == the target basename always) —
  # this is the only check that would catch that regressing.
  #
  # Skipped for DYNDRV (direct_exec=0): these wrap an arbitrary
  # upstream nixpkgs package verbatim, so meta.mainProgram is whatever
  # that package's own package.nix set (or didn't) — not something
  # splitStdenv itself controls. mosh-dyndrv is a real example:
  # upstream nixpkgs' mosh package sets no mainProgram at all.
  local is_lib="0"; [[ "$want" == lib/* ]] && is_lib="1"
  local has_mp="0"
  "$PATCHED_NIX/bin/nix" eval --no-eval-cache \
    "$nixgg_root#$attr.meta.mainProgram" >/dev/null 2>&1 && has_mp="1"
  if [[ "$direct_exec" == "1" ]]; then
    if [[ "$is_lib" == "1" && "$has_mp" == "1" ]]; then
      printf '\033[1;31m  BAD META\033[0m %s has meta.mainProgram but is a library\n' "$attr" >&2
      fail=1; return
    fi
    if [[ "$is_lib" == "0" && "$has_mp" == "0" ]]; then
      printf '\033[1;31m  BAD META\033[0m %s has no meta.mainProgram; nix run would '"'"'guess'"'"' \n' "$attr" >&2
      fail=1; return
    fi
  fi

  local log="/tmp/nixgg-smoke-$attr.log"
  local outs out disk
  outs=$("$PATCHED_NIX/bin/nix" build --no-eval-cache --no-link \
        --print-out-paths "$nixgg_root#$attr" 2>"$log")
  if [[ -z "$outs" ]]; then
    echo "  BUILD FAILED; see $log:" >&2
    tail -8 "$log" >&2
    fail=1; return
  fi

  # A multi-output default build (e.g. zstd-dyndrv's bin+man) prints
  # one line per realized output, in no guaranteed order — the wanted
  # artifact isn't necessarily on the last line. Pick whichever printed
  # output actually has it, rather than assuming a single line.
  out=""
  while IFS= read -r candidate; do
    if [[ -e "$ALT_STORE$candidate/$want" ]]; then
      out="$candidate"
      break
    fi
  done <<<"$outs"
  if [[ -z "$out" ]]; then
    out=$(tail -1 <<<"$outs")
  fi

  # The artifact must be at the documented FHS path.
  disk="$ALT_STORE$out/$want"
  if [[ ! -e "$disk" ]]; then
    printf '\033[1;31m  MISSING\033[0m %s\n' "\$out/$want" >&2
    echo "  what is actually there (across all realized outputs):" >&2
    while IFS= read -r candidate; do
      ( cd "$ALT_STORE$candidate" 2>/dev/null && find . -maxdepth 2 | sed 's|^|      |' )
    done <<<"$outs" >&2
    fail=1; return
  fi
  if [[ ! -s "$disk" ]]; then
    echo "  EMPTY: \$out/$want" >&2; fail=1; return
  fi

  if [[ "$run" == "-" ]]; then
    printf '\033[1;32m  OK\033[0m       $out/%s (%s bytes)\n' \
      "$want" "$(stat -c%s "$disk")"
    return
  fi

  if [[ "$direct_exec" == "1" ]]; then
    # shellcheck disable=SC2059
    local cmd out_txt
    cmd=$(printf "$run" "$disk")
    if out_txt=$(eval "$cmd" 2>&1 | head -1); then
      printf '\033[1;32m  OK\033[0m       $out/%s -> %s\n' "$want" "$out_txt"
    else
      # A raw exec of the on-disk $ALT_STORE path can fail for a
      # reason that has nothing to do with the build: it resolves its
      # ELF interpreter/RPATH against the REAL filesystem root, so it
      # only works if a fixed-output dep like glibc ALSO happens to
      # already exist at that same store path outside the alt store —
      # an ambient-host-state coincidence (see DYNDRV's own comment
      # below on this exact mechanism), not something this build
      # controls. Don't hard-fail on it; fall through to the `nix run`
      # check below, which uses Nix's own private mount-namespace bind
      # mount of $ALT_STORE onto /nix/store and is the actually
      # reliable verification path — confirmed missing entirely on a
      # fresh CI runner (this dev box's real /nix/store already had
      # glibc cached from unrelated prior use, masking the gap).
      printf '\033[1;33m  DIRECT EXEC SKIPPED\033[0m $out/%s -> %s (relying on nix run below)\n' "$want" "$out_txt"
    fi
  fi

  # Also drive it through `nix run` itself, not just a direct exec of
  # the path we predicted — this is the actual point of `.package`
  # being a derivation rather than an outputOf string. Reuses the
  # build above (same drv, cached), so this is nearly free.
  local runlog="/tmp/nixgg-smoke-$attr-run.log"
  if "$PATCHED_NIX/bin/nix" run --no-eval-cache "$nixgg_root#$attr" \
       -- --version >"$runlog" 2>&1 \
     || "$PATCHED_NIX/bin/nix" run --no-eval-cache "$nixgg_root#$attr" \
       >>"$runlog" 2>&1; then
    printf '\033[1;32m  OK\033[0m       nix run .#%s\n' "$attr"
  else
    printf '\033[1;31m  NIX RUN FAILED\033[0m .#%s; see %s\n' "$attr" "$runlog" >&2
    tail -4 "$runlog" >&2
    fail=1
  fi
}

for entry in "${SET[@]}"; do
  IFS='|' read -r attr want run <<<"$entry"
  check_example "$attr" "$want" "$run" 1
done

for entry in "${DYNSET[@]}"; do
  IFS='|' read -r attr want run <<<"$entry"
  check_example "$attr" "$want" "$run" 0
done

echo
if [[ $fail -eq 0 ]]; then
  printf '\033[1;32mall examples build and run.\033[0m\n'
else
  printf '\033[1;31msome examples failed.\033[0m\n'
fi
exit $fail
