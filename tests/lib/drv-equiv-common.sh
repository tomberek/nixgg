# Shared plumbing for tests/drv-equivalence.sh and
# tests/batch-drv-equivalence.sh: both prove the same invariant
# (native and sandbox modes produce byte-identical drvs for a given
# Kind) against the same scaffolding, differing only in which drv
# names they care about. Meant to be `source`d, not executed.
#
# Env knobs (same names both callers already documented):
#   ALT_STORE      root of the alt store
#   PATCHED_NIX    path to a builder-rpc-v0-capable nix
#   KEEP_STORE=1   don't wipe ALT_STORE at start (for local iteration)

# equiv_common_setup <default-alt-store-dir>
#
# Sets $here / $nixgg_root (relative to the CALLER's $0) and
# $ALT_STORE / $PATCHED_NIX, builds patched-nix if missing, wipes and
# recreates ALT_STORE (unless KEEP_STORE=1), points NIX_CONFIG at it,
# and seeds it with patched-nix itself so the outer .drv.drv can be
# built.
equiv_common_setup() {
  local default_alt_store="$1"

  here="$(cd "$(dirname "$0")" && pwd)"
  nixgg_root="$(cd "$here/.." && pwd)"

  ALT_STORE="${ALT_STORE:-$default_alt_store}"
  PATCHED_NIX="${PATCHED_NIX:-$nixgg_root/.patched-nix}"
  if [[ ! -x "$PATCHED_NIX/bin/nix" ]]; then
    echo "==> building patched nix (one-time; substituted from cache)" >&2
    nix build --no-eval-cache "$nixgg_root#patched-nix" \
      -o "$PATCHED_NIX" >&2 || {
        echo "failed to build $nixgg_root#patched-nix" >&2
        exit 2
      }
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

  # Seed alt store with patched-nix so the outer .drv.drv can be built.
  if [[ ! -e "$ALT_STORE/$(readlink -f "$PATCHED_NIX")" ]]; then
    "$PATCHED_NIX/bin/nix" copy --from daemon --to "local?root=$ALT_STORE" \
        --no-check-sigs "$(readlink -f "$PATCHED_NIX")" >/dev/null 2>&1 || true
  fi
}

# equiv_resolve_native_src <src_input>
#
# Echoes the resolved, existing on-disk path for a fixture's native
# source: "example" resolves to $nixgg_root/example; anything else is
# a flake-input name resolved straight out of flake.lock (NOT `nix
# flake archive` / `builtins.getFlake`, both of which pay the cost of
# locking every OTHER input's transitive graph too before returning
# even one field — confirmed directly as a failure mode in a cold/
# isolated store). Every non-"example" input here is `flake = false`,
# so its locked node has no further inputs to chase.
#
# Echoes nothing (caller must check) if resolution fails. Requires
# $nixgg_root / $ALT_STORE / $PATCHED_NIX from equiv_common_setup.
equiv_resolve_native_src() {
  local src_input="$1"

  if [[ "$src_input" == "example" ]]; then
    echo "$nixgg_root/example"
    return 0
  fi

  local locked
  locked=$(python3 -c "
import json
d = json.load(open('$nixgg_root/flake.lock'))
print(json.dumps(json.dumps(d['nodes']['$src_input']['locked'])))
" 2>/dev/null)
  [[ -z "$locked" ]] && return 1

  local src
  src=$("$PATCHED_NIX/bin/nix" eval --impure --raw \
    --expr "(builtins.fetchTree (builtins.fromJSON $locked)).outPath" 2>/dev/null)
  [[ -z "$src" ]] && return 1

  # fetchTree ran against store = local?root=$ALT_STORE (this script's
  # own NIX_CONFIG), so the returned path lives under $ALT_STORE, not
  # /nix/store directly — a missing prefix here can go unnoticed on a
  # machine with its own ambient /nix/store, but fails in CI's
  # genuinely fresh store.
  echo "$ALT_STORE$src"
}

# equiv_sandbox_drvs <attr>
#
# Echoes the basename of every TU/link/archive drv a sandbox build of
# `.#<attr>` produces. Building the outer wrapper's own per-target
# outputs runs the shims, which REGISTER each drv (`nix derivation
# add`) rather than compiling for real, so this triggers every
# registration with no real compilation; `nix derivation show -r` on
# the resulting output paths then returns the complete, deduplicated
# drv set with zero filename filtering needed (toolchain deps are
# referenced via inputs.srcs, never inputs.drvs, so show -r has
# nothing to recurse into for them).
#
# Requires $nixgg_root, $PATCHED_NIX in scope (from
# equiv_common_setup). Logs failures to /tmp/nixgg-equiv-<attr>-
# sandbox.log; on failure, tails it to stderr and returns 1.
equiv_sandbox_drvs() {
  local attr="$1"
  local log="/tmp/nixgg-equiv-$attr-sandbox.log"

  local outputs_json
  outputs_json=$("$PATCHED_NIX/bin/nix" eval --json "$nixgg_root#$attr.drv.outputs" 2>"$log") || {
    echo "could not read $attr.drv.outputs; see $log:" >&2
    tail -20 "$log" >&2
    return 1
  }

  local -a keys=()
  while IFS= read -r k; do
    [[ -n "$k" ]] && keys+=("$k")
  done < <(python3 -c "
import json, sys
for k in json.loads(sys.stdin.read()):
    print(k)
" <<<"$outputs_json")

  if [[ ${#keys[@]} -eq 0 ]]; then
    echo "$attr.drv.outputs is empty" >&2
    return 1
  fi

  # One nix build invocation for every output — shares one build phase
  # for the whole multi-output derivation rather than re-running
  # buildCommand per output.
  local -a attrs=()
  for k in "${keys[@]}"; do
    attrs+=("$nixgg_root#$attr.drv.\"$k\"")
  done

  local out_paths
  out_paths=$("$PATCHED_NIX/bin/nix" build --no-eval-cache --no-link \
    --print-out-paths "${attrs[@]}" 2>"$log") || {
    echo "sandbox build failed; see $log:" >&2
    tail -20 "$log" >&2
    return 1
  }

  local -a target_paths=()
  while IFS= read -r p; do
    [[ -n "$p" ]] && target_paths+=("$p")
  done <<<"$out_paths"

  if [[ ${#target_paths[@]} -eq 0 ]]; then
    echo "$attr's outer wrapper produced no output paths" >&2
    return 1
  fi

  local show_json
  show_json=$("$PATCHED_NIX/bin/nix" derivation show -r "${target_paths[@]}" 2>"$log") || {
    echo "nix derivation show -r failed for $attr; see $log:" >&2
    tail -20 "$log" >&2
    return 1
  }

  python3 -c "
import json, sys
d = json.loads(sys.stdin.read())
for k in sorted(d['derivations'].keys()):
    print(k)
" <<<"$show_json"
}

# equiv_native_build <attr> <subdir> <workdir> <log-file>
#
# Copies the already-resolved native src (expected at $src, set by
# the caller) into workdir, `make clean`s it if it looks like a
# recursive-make fixture, then runs `.#$attr-shell`'s own buildCommand
# under `nix develop` there — same buildInputs/env-scrub/NIXGG_*
# exports mkNixggBuild.nix's real preBuild uses, pulled straight from
# the flake so no build recipe is duplicated in these test scripts.
#
# Requires $src (native source root), $nixgg_root, $ALT_STORE,
# $PATCHED_NIX in scope. Logs to <log-file>; on failure, tails it to
# stderr and returns 1.
equiv_native_build() {
  local attr="$1" subdir="$2" workdir="$3" nt_log="$4"

  # Store paths are read-only, so we need our own writable copy.
  cp -a "$src"/. "$workdir/"
  chmod -R u+w "$workdir"

  rm -rf "$workdir/.nixgg" 2>/dev/null || true

  # "example" is a live working directory; a stray `make` there leaves
  # main.o/util.o/hello behind, and a verbatim copy would then skip
  # both compiles (zero thunks, looking like a nixgg bug). Use the
  # fixture's own `clean` target so it can't drift from the build
  # rules.
  if [[ -e "$workdir/${subdir}/Makefile" ]]; then
    ( cd "$workdir/${subdir}" && make clean ) >/dev/null 2>&1 || true
  fi

  local build_cmd
  build_cmd="$("$PATCHED_NIX/bin/nix" eval --raw \
    "$nixgg_root#$attr-shell.passthru.buildCommand" 2>/dev/null)" || {
      echo "could not read buildCommand for $attr" >&2
      return 1
    }

  printf '==> native: nix develop .#%s-shell in %s\n' "$attr" "$workdir/${subdir}"
  # `.#<attr>-shell` is a plain mkShell whose shellHook is a byte-copy
  # of mkNixggBuild.nix:preBuild.
  (
    cd "$workdir/${subdir}"
    "$PATCHED_NIX/bin/nix" develop "$nixgg_root#$attr-shell" --command bash -c "
      export NIXGG_STORE='local?root=$ALT_STORE'
      export NIXGG_AUTOFORCE=0
      set -euo pipefail
      $build_cmd
    "
  ) > "$nt_log" 2>&1 || {
    echo "native build failed; see $nt_log:" >&2
    tail -20 "$nt_log" >&2
    return 1
  }
}

# equiv_collect_thunks <workdir>
#
# Echoes every *.nix thunk file nixgg's shim wrote under workdir.
# nixgg's auto-seed walks up to the nearest `.git` and lands `.nixgg/`
# there; under our tempdir (no .git) it lands wherever the shim ran —
# for a recursive-make build like mosh's that's one
# `.nixgg/thunks/` per subdir the shim was invoked in, so this
# collects from ALL of them.
equiv_collect_thunks() {
  local workdir="$1"
  find "$workdir" -type f -path '*/.nixgg/thunks/*.nix' 2>/dev/null
}

# equiv_thunk_drvpath <thunk-file>
#
# Echoes the basename of the .drv a single thunk file resolves to.
# Requires $PATCHED_NIX in scope.
equiv_thunk_drvpath() {
  local t="$1"
  "$PATCHED_NIX/bin/nix" eval --no-eval-cache --impure --raw \
    --file "$t" drvPath 2>/dev/null | xargs -n1 basename
}

# equiv_report_sets <label> <noun> <sb-drvs> <nt-drvs>
#
# Compares the SET of drv-hashes sandbox vs. native produced (already
# filtered down to whatever this Kind cares about by the caller),
# prints the match/only-sandbox/only-native counts, and on any
# mismatch prints the offending basenames to stderr. Echoes the
# common count on success. Returns 1 on any mismatch.
equiv_report_sets() {
  local label="$1" noun="$2" sb_drvs="$3" nt_drvs="$4"

  local only_sandbox only_native both
  only_sandbox=$(comm -23 <(echo "$sb_drvs") <(echo "$nt_drvs"))
  only_native=$(comm -13 <(echo "$sb_drvs") <(echo "$nt_drvs"))
  both=$(comm -12 <(echo "$sb_drvs") <(echo "$nt_drvs"))

  # `wc -l` undercounts content with no trailing newline; grep -c . avoids that.
  local n_both n_only_sb n_only_nt
  n_both=$(printf '%s\n' "$both" | grep -c . || true)
  n_only_sb=$(printf '%s\n' "$only_sandbox" | grep -c . || true)
  n_only_nt=$(printf '%s\n' "$only_native" | grep -c . || true)
  n_both=${n_both:-0}
  n_only_sb=${n_only_sb:-0}
  n_only_nt=${n_only_nt:-0}

  # Callers capture our stdout as the returned count (`n_both=$(...)`),
  # so all human-readable status must go to stderr — only the final
  # `echo "$n_both"` is allowed on stdout.
  printf '   %d %s match, %d only-sandbox, %d only-native\n' \
    "$n_both" "$noun" "$n_only_sb" "$n_only_nt" >&2

  if (( n_only_sb > 0 || n_only_nt > 0 )); then
    printf '\033[1;31mMISMATCH\033[0m %s\n' "$label" >&2
    (( n_only_sb > 0 )) && printf '  only in sandbox:\n%s\n' "$only_sandbox" >&2
    (( n_only_nt > 0 )) && printf '  only in native:\n%s\n' "$only_native" >&2
    return 1
  fi

  echo "$n_both"
}
