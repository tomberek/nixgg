# Per-TU CA derivation. Takes toolchain paths as already-realised store
# paths (no <nixpkgs> dependency, no fetching) so parallel invocations
# don't race on nixpkgs evaluation.
#
# The compile command itself comes from Go (internal/expr
# Derivation.scriptTemplate) as a shell body with @NIXGG_*@ markers;
# sandbox mode bakes the same body straight into its JSON drv, so both
# modes hash identically for the same compile.
{
  compilerRoot  ? (import ./toolchain.nix).compilerRoot,
  bashRoot      ? (import ./toolchain.nix).bashRoot,
  coreutilsRoot ? (import ./toolchain.nix).coreutilsRoot,
  srcTree,                 # path expression → Nix imports the tree at eval time
  source,                  # relative path inside srcTree, e.g. "main.cc"
  outName,                 # object filename, e.g. "main.o"
  scriptTemplate,          # bash body from Go, with @<tag>_*@ markers
  markerTag,               # the tag those markers use; see resolve-script.nix
  storeDepsJSON ? "[]",    # JSON list of extra /nix/store/... deps the
                           # flags/wrapper env reference and must be present
  wrapperEnvJSON ? "{}",   # JSON object of NIX_CFLAGS_COMPILE etc. for the
                           # Nix gcc-wrapper
}:
let
  pureStorePath = import ./pure-store-path.nix;
  bash        = pureStorePath bashRoot;
  coreutils   = pureStorePath coreutilsRoot;
  compiler    = pureStorePath compilerRoot;
  src         = srcTree;
  storeDeps   = map pureStorePath (builtins.fromJSON storeDepsJSON);
  wrapperEnv  = builtins.fromJSON wrapperEnvJSON;
  script      = import ./resolve-script.nix {
    inherit scriptTemplate markerTag coreutils compiler;
    inputs = [ ];
  };
in
derivation ({
  name = "tu-${outName}";
  system = builtins.currentSystem;

  __contentAddressed = true;
  outputHashMode = "nar";
  outputHashAlgo = "sha256";

  builder = "${bash}/bin/bash";
  args = [ "-c" script ];

  inherit src source outName;
  _storeDeps = builtins.concatStringsSep ":" storeDeps;
} // wrapperEnv)
