# Partial-link CA derivation (`ld -r`): combine several objects into
# one relocatable object, not an executable/archive. Same { drv, name }
# input shape as linker.nix/archiver.nix.
#
# toolBin is ld's own already-realised full tool path (realLDFor's
# resolution, e.g. ".../gcc-wrapper-15.3.0/bin/ld") — a SUBPATH inside
# an existing package, not a store root itself, unlike objtool's own
# stage-tool.nix output (which IS the root). pure-tool-path.nix
# handles either shape.
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
  ld          = pureToolPath toolBin;
  storeDeps   = map pureStorePath (builtins.fromJSON storeDepsJSON);
  wrapperEnv  = builtins.fromJSON wrapperEnvJSON;
  script      = import ./resolve-script.nix {
    inherit scriptTemplate markerTag coreutils inputs;
    compiler = ld;
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
