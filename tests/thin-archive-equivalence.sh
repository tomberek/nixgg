#!/usr/bin/env bash
# Regression test: native and sandbox modes produce byte-identical
# drvs for a build that uses a THIN archive (`ar csrDT`), and the
# resulting binary actually runs — proving the member-propagation
# mechanism in go/internal/shim/storeinput.go's classifyInputs +
# go/internal/members works identically in both modes, not just in
# the isolated Go unit tests.
#
# Mirrors tests/drv-equivalence.sh's methodology (same shared
# scaffolding in tests/lib/drv-equiv-common.sh) but for a fixture with
# no flake-input source (examples/thin-archive/src is a small in-tree
# fixture, not a fetched tarball), so native src resolution is done
# directly here.
#
# Beyond the hash-set comparison, this script also realises and runs
# both builds' binaries directly, as cheap insurance against a bug
# that doesn't affect the hash but does affect the mechanism.
#
# Env knobs: same as drv-equivalence.sh (ALT_STORE, PATCHED_NIX,
# KEEP_STORE).

set -euo pipefail

# shellcheck source=lib/drv-equiv-common.sh
source "$(cd "$(dirname "$0")" && pwd)/lib/drv-equiv-common.sh"

equiv_common_setup "/tmp/nixgg-thin-archive-equiv-store"

attr="thin-archive"
label="$attr"

echo
printf '\033[1;36m===== %s =====\033[0m\n' "$label"

printf '==> sandbox: nix build .#%s (wrapper only) + derivation show -r\n' "$attr"
sb_drvs=$(equiv_sandbox_drvs "$attr") || exit 1

# Realise the sandbox build's own binary for the functional check
# below. Same alt store equiv_sandbox_drvs just registered into, so
# this only pays for the actual TU/link builds, not re-registration.
sb_log="/tmp/nixgg-thin-archive-equiv-sandbox-realise.log"
printf '==> sandbox: realising .#%s\n' "$attr"
sb_out=$("$PATCHED_NIX/bin/nix" build --no-eval-cache --no-link \
  --print-out-paths "$nixgg_root#$attr" \
  2>"$sb_log" | tail -1) || {
    echo "sandbox realise failed; see $sb_log:" >&2
    tail -20 "$sb_log" >&2
    exit 1
  }

# The binary's own content (ELF interpreter reference, glibc rpath,
# etc) always names the CANONICAL /nix/store/... path — execve()
# resolves those against the real filesystem root, so exec'ing
# straight out of "$ALT_STORE/nix/store/..." only works by coincidence
# (confirmed as a real CI failure on a fresh runner with no such
# cache). `nix copy` the binary's closure into the real (daemon) store
# first.
printf '==> functional: running sandbox-built binary\n'
"$PATCHED_NIX/bin/nix" copy --from "local?root=$ALT_STORE" --to daemon \
  --no-check-sigs "$sb_out" >>"$sb_log" 2>&1 || {
    echo "could not copy $sb_out to the real store; see $sb_log:" >&2
    tail -20 "$sb_log" >&2
    exit 1
  }
sb_bin="$sb_out/bin/thin-archive"
if [[ ! -x "$sb_bin" ]]; then
  echo "sandbox binary missing or not executable: $sb_bin" >&2
  exit 1
fi
sb_run_out=$("$sb_bin")
if [[ "$sb_run_out" != "thin-archive: ok" ]]; then
  echo "sandbox binary produced unexpected output: $sb_run_out" >&2
  exit 1
fi
printf '\033[1;32mOK\033[0m       sandbox binary runs: %s\n' "$sb_run_out"

workdir="$(mktemp -d)"
cp -a "$nixgg_root/examples/thin-archive/src"/. "$workdir/"
chmod -R u+w "$workdir"
rm -rf "$workdir/.nixgg" 2>/dev/null || true

nt_log="/tmp/nixgg-thin-archive-equiv-native.log"
printf '==> native: nix develop .#%s-shell in %s\n' "$attr" "$workdir"
(
  cd "$workdir"
  "$PATCHED_NIX/bin/nix" develop "$nixgg_root#$attr-shell" --command bash -c "
    export NIXGG_STORE='local?root=$ALT_STORE'
    export NIXGG_AUTOFORCE=1
    set -euo pipefail
    make
  "
) > "$nt_log" 2>&1 || {
  echo "native build failed; see $nt_log:" >&2
  tail -20 "$nt_log" >&2
  rm -rf "$workdir"
  exit 1
}

printf '==> functional: running native-built binary\n'
if [[ ! -x "$workdir/thin-archive" ]]; then
  echo "native binary missing or not executable: $workdir/thin-archive" >&2
  rm -rf "$workdir"
  exit 1
fi
nt_run_out=$("$workdir/thin-archive")
if [[ "$nt_run_out" != "thin-archive: ok" ]]; then
  echo "native binary produced unexpected output: $nt_run_out" >&2
  rm -rf "$workdir"
  exit 1
fi
printf '\033[1;32mOK\033[0m       native binary runs: %s\n' "$nt_run_out"

thunk_files=$(equiv_collect_thunks "$workdir")
if [[ -z "$thunk_files" ]]; then
  echo "native build produced no thunks; see $nt_log" >&2
  rm -rf "$workdir"
  exit 1
fi

nt_drvs=$(while IFS= read -r t; do
  equiv_thunk_drvpath "$t"
done <<<"$thunk_files" | sort -u)

if ! n_both=$(equiv_report_sets "$label" "drvs" "$sb_drvs" "$nt_drvs"); then
  rm -rf "$workdir"
  exit 1
fi

rm -rf "$workdir"

if (( n_both == 0 )); then
  printf '\033[1;31mNO DRVS\033[0m %s — filter matched nothing; the fixture regressed\n' "$label" >&2
  exit 1
fi

printf '\033[1;32mMATCH\033[0m    %s (%d drvs)\n' "$label" "$n_both"
echo
echo "thin-archive fixture: native/sandbox equivalent and both functional."
