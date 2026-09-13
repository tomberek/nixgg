# Combined compile+archive CA derivation for a same-group batch (see
# go/internal/batch and go/internal/expr/batcharchive.go).
#
# Unlike builder.nix/archiver.nix, this Kind never needs resolve-script.nix's
# marker substitution: go/internal/shim's tryBatchArchive already confirmed
# every input belongs to this one batch, never a not-yet-realized sibling
# drv/thunk. Go renders each member's compile line fully resolved
# (compileLine); the only thing left to interpolate here is each member's
# own srcTree path literal.
#
# `compilerRoot` is whatever provides `ar` on PATH — batcharchive.go puts
# the same root on PATH for every member's own compile (one toolchain per
# build, see BatchArchiveJSONParams.AR).
{
  compilerRoot ? (import ./toolchain.nix).compilerRoot,
  bashRoot ? (import ./toolchain.nix).bashRoot,
  coreutilsRoot ? (import ./toolchain.nix).coreutilsRoot,
  outName,
  arFlags,
  # [ { srcTree, outName, compileLine } ... ], in the archive's ar argv
  # order. srcTree is a Nix path literal; outName is the member's object
  # filename ($objroot/<outName>); compileLine is a complete, already
  # shell-quoted compile invocation missing only its own `cd`.
  members,
  storeDepsJSON ? "[]",
  wrapperEnvJSON ? "{}",
}:
let
  pureStorePath = import ./pure-store-path.nix;
  bash = pureStorePath bashRoot;
  coreutils = pureStorePath coreutilsRoot;
  compiler = pureStorePath compilerRoot;
  storeDeps = map pureStorePath (builtins.fromJSON storeDepsJSON);
  wrapperEnv = builtins.fromJSON wrapperEnvJSON;

  # One `(cd <srcTree> && <compileLine>) &` + `gg_after $!` per member,
  # then a wait-out of every still-running member, then one `ar`
  # invocation over every member's own $objroot/<outName>. Mirrors
  # go/internal/expr/batcharchive.go's batchArchiveScript exactly —
  # everything but each member's srcTree is already-resolved text from
  # Go, copied here verbatim so native and sandbox modes can't drift.
  #
  # Plain builtins.concatStringsSep, not lib.concatMapStrings(Sep) —
  # this file must not depend on <nixpkgs> (parallel invocations would
  # race on nixpkgs input evaluation), so it can't take `lib` as a param.
  #
  # Quoted exactly like batcharchive.go quotes its own `cd "<SrcStore>"`:
  # drv-hash equivalence between native and sandbox modes requires
  # identical script TEXT, not just equivalent behavior (checked by
  # tests/batch-drv-equivalence.sh).
  compileLines = builtins.concatStringsSep ""
    (map (m: "(cd \"${m.srcTree}\" && ${m.compileLine}) &\ngg_after $!\n") members);
  objList = builtins.concatStringsSep " "
    (map (m: ''"$objroot/${m.outName}"'') members);

  # $objroot's location depends on arFlags: a THIN archive (`T`) stores
  # each member's file PATH, not its bytes, so those paths must survive
  # after this derivation's build sandbox is torn down — put them under
  # $out. A non-thin archive copies members' bytes INTO the archive
  # itself, so a scratch dir is fine. Byte-identical to batcharchive.go's
  # own batchArchiveScript.
  isThin = builtins.match ".*T.*" arFlags != null;
  objrootSetup =
    if isThin
    then ''
      mkdir -p "$out/lib/.nixgg-objs"
      objroot="$out/lib/.nixgg-objs"
    ''
    else ''
      mkdir -p "$out/lib" .nixgg-objs
      objroot="$PWD/.nixgg-objs"
    '';

  # Byte-identical to batcharchive.go's own batchConcurrencyPreamble/
  # batchConcurrencyDrain constants — Go is the one place either mode's
  # script text is authored, this file only splices in per-member Nix
  # path literals.
  concurrencyPreamble = ''
    gg_max="''${NIX_BUILD_CORES:-1}"
    case "$gg_max" in '''|*[!0-9]*) gg_max=1 ;; esac
    [ "$gg_max" -ge 1 ] || gg_max=1
    gg_pids=""
    gg_fail=0
    gg_after() {
      gg_pids="$gg_pids $1"
      set -- $gg_pids
      if [ "$#" -ge "$gg_max" ]; then
        wait "$1" || gg_fail=1
        shift
        gg_pids="$*"
      fi
    }
  '';
  concurrencyDrain = ''
    for gg_pid in $gg_pids; do
      wait "$gg_pid" || gg_fail=1
    done
    [ "$gg_fail" -eq 0 ] || exit 1
  '';

  script = ''
    set -euo pipefail
    export PATH="${coreutils}/bin:${compiler}/bin"
    ${objrootSetup}${concurrencyPreamble}${compileLines}${concurrencyDrain}ar D${arFlags} "$out/lib/${outName}" ${objList}
  '';
in
derivation ({
  name = "batch-${outName}";
  system = builtins.currentSystem;

  __contentAddressed = true;
  outputHashMode = "nar";
  outputHashAlgo = "sha256";

  builder = "${bash}/bin/bash";
  args = [ "-c" ''source "$batchScriptPath"'' ];
  passAsFile = [ "batchScript" ];
  batchScript = script;

  _storeDeps = builtins.concatStringsSep ":" storeDeps;
} // wrapperEnv)
