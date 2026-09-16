# Transform CA derivation (objtool/objcopy): rewrite one existing
# object in place, or read one and write another. Same { drv, name }
# input shape as archiver.nix/linker.nix, but only ever one input.
#
# toolBin is an already-realised full tool path in both modes:
# objcopy's is realBinutil's own resolution (a SUBPATH inside an
# existing package, e.g. ".../binutils/bin/objcopy"); objtool's is
# nix/stage-tool.nix's own output (a self-built binary staged through
# an ordinary derivation build so its RPATH deps get scanned and
# recorded — see go/internal/shim/objtool.go's stageToolForNative —
# and IS itself a store root, no subpath). pure-tool-path.nix handles
# either shape; plain pureStorePath would throw ("context key ... is
# not a store path") on objcopy's own subpath form.
{
  bashRoot      ? (import ./toolchain.nix).bashRoot,
  coreutilsRoot ? (import ./toolchain.nix).coreutilsRoot,
  outName,
  name,
  toolBin,
  inputs,
  scriptTemplate,
  markerTag,
  storeDepsJSON ? "[]",
  wrapperEnvJSON ? "{}",
}:
let
  pureStorePath = import ./pure-store-path.nix;
  pureToolPath  = import ./pure-tool-path.nix;
  bash        = pureStorePath bashRoot;
  coreutils   = pureStorePath coreutilsRoot;
  tool        = pureToolPath toolBin;
  storeDeps   = map pureStorePath (builtins.fromJSON storeDepsJSON);
  wrapperEnv  = builtins.fromJSON wrapperEnvJSON;
  script      = import ./resolve-script.nix {
    inherit scriptTemplate markerTag coreutils inputs;
    compiler = tool;
  };
in
derivation ({
  name = name;
  system = builtins.currentSystem;

  __contentAddressed = true;
  outputHashMode = "nar";
  outputHashAlgo = "sha256";

  builder = "${bash}/bin/bash";
  args = [ "-c" script ];

  _storeDeps = builtins.concatStringsSep ":" storeDeps;
} // wrapperEnv)
