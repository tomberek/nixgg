#!/usr/bin/env bash
# Guards the kernel's passthrough-path list.
#
# WHY THIS EXISTS
#
# The subtrees a kernel builds by reading object bytes inline — realmode,
# libstub, the vDSO, purgatory, scripts/mod, test_fortify — used to be
# compiled into internal/mode, where a 2ms unit test in a separate file
# caught any accidental change to the list. Moving them to a
# `passthroughPaths` parameter fixed a real layering problem (Linux-,
# x86- and version-specific knowledge inside a general-purpose tool) but
# lost that guard: internal/mode's tests now SUPPLY the list before
# asserting on it, so they pin the matching semantics and nothing else.
# Asserting against your own input cannot catch a dropped entry.
#
# Without this script, deleting an entry from tests/kernel/kernel.nix
# fails nothing until a multi-hour kernel build dies at the final vmlinux
# link with dozens of undefined references — a message that names neither
# the list nor the subtree. That is exactly how the thin-archive bug
# presented, and it took a day to trace.
#
# So: check the list by EVALUATION only. No build, no store writes, ~1s.
#
# It reads the value out of the real phase-1 derivation rather than
# re-reading a Nix list, so it also proves the plumbing — that
# passthroughPaths actually reaches the build as NIXGG_PASSTHROUGH_PATHS.
# A list compared against a list would pass even if the parameter were
# silently dropped between splitStdenv and the shims.
#
# This is a change-detector, deliberately. Whether these six are the
# RIGHT subtrees is only answerable by tests/kernel/boot-test.nix. What
# this catches is the accident.

set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
nixgg_root="$(cd "$here/.." && pwd)"
PATCHED_NIX="${PATCHED_NIX:-$nixgg_root/.patched-nix}"

if [[ ! -x "$PATCHED_NIX/bin/nix" ]]; then
  echo "SKIP: no patched nix at $PATCHED_NIX" >&2
  exit 0
fi

# Needs its own store even though it never builds: the daemon store
# rejects a text-hashed derivation output, so instantiating the kernel
# fails at EVALUATION with a dynamic-derivations error that does not
# mention the store. Only .drv files land here, so it stays tiny.
STORE="${PASSTHROUGH_STORE:-/tmp/nixgg-passthrough-store}"
mkdir -p "$STORE"

export NIX_CONFIG="
experimental-features = nix-command flakes ca-derivations dynamic-derivations
store = local?root=$STORE
"

cd "$nixgg_root"

drv=$("$PATCHED_NIX/bin/nix" eval --impure --raw \
  --expr '(import ./tests/kernel/kernel.nix {}).drvPath' 2>/dev/null)
if [[ -z "$drv" ]]; then
  echo "FAIL: tests/kernel/kernel.nix did not evaluate" >&2
  exit 1
fi

# -r walks the derivation closure, which is where phase 1 lives: the
# kernel package itself is phase 2, and the shim env is set in phase 1's
# postPatch.
got=$("$PATCHED_NIX/bin/nix" derivation show -r "$drv" 2>/dev/null \
  | tr -d '\\' | grep -o "NIXGG_PASSTHROUGH_PATHS='\[[^]]*\]'" | head -1)

if [[ -z "$got" ]]; then
  echo "FAIL: NIXGG_PASSTHROUGH_PATHS is not exported into the kernel build." >&2
  echo "      passthroughPaths is being dropped somewhere between" >&2
  echo "      tests/kernel/kernel.nix and splitStdenv's shim env." >&2
  exit 1
fi

expected=(
  "arch/x86/realmode/"
  "drivers/firmware/efi/libstub/"
  "arch/x86/entry/vdso/"
  "arch/x86/purgatory/"
  "scripts/mod/"
  "/test_fortify/"
)

missing=0
for want in "${expected[@]}"; do
  if [[ "$got" != *"\"$want\""* ]]; then
    echo "FAIL: missing passthrough path: $want" >&2
    missing=1
  fi
done

# An empty element substring-matches EVERY path, so one stray "" passes
# the entire build through. That surfaces as nixgg accelerating nothing
# rather than as an error, which is far worse than a hard failure.
# internal/mode drops them defensively; catch it here too, where the
# message can say what happened.
if [[ "$got" == *'""'* ]]; then
  echo "FAIL: empty string in passthroughPaths — it matches every path and" >&2
  echo "      would pass the whole build through silently." >&2
  missing=1
fi

if [[ $missing -ne 0 ]]; then
  echo >&2
  echo "got: $got" >&2
  exit 1
fi

echo "OK: kernel passthroughPaths reaches the build, all ${#expected[@]} entries present"
