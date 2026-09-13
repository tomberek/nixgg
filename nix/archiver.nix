# Archive CA derivation. Same `inputs`/`extraInputs` shape as linker.nix;
# the `ar` command comes from Go — see resolve-script.nix.
#
# `compilerRoot` here is whatever provides `ar` (binutils), not a
# compiler — it's the third positional toolchain root every helper
# takes. Go's side is the Derivation.AR field.
{
  compilerRoot  ? (import ./toolchain.nix).compilerRoot,
  bashRoot      ? (import ./toolchain.nix).bashRoot,
  coreutilsRoot ? (import ./toolchain.nix).coreutilsRoot,
  outName,
  name ? "ar-${outName}",
  inputs,
  extraInputs ? [ ],  # see linker.nix — same mount-not-relist mechanism
  scriptTemplate,
  markerTag,
  storeDepsJSON ? "[]",
  wrapperEnvJSON ? "{}",
}:
let
  pureStorePath = import ./pure-store-path.nix;
  bash        = pureStorePath bashRoot;
  coreutils   = pureStorePath coreutilsRoot;
  compiler    = pureStorePath compilerRoot;
  storeDeps   = map pureStorePath (builtins.fromJSON storeDepsJSON);
  wrapperEnv  = builtins.fromJSON wrapperEnvJSON;
  script      = import ./resolve-script.nix {
    inherit scriptTemplate markerTag coreutils compiler inputs;
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
  _extraInputs = builtins.concatStringsSep ":"
    (map (i: "${i.drv}/${i.name}") extraInputs);
} // wrapperEnv)
