# Link CA derivation.
#
# `inputs` is a native Nix list of { drv, name }; `drv` is either an
# unrealised sibling derivation or a `pureStorePath` result. Either way
# `${item.drv}/${item.name}` interpolates to the linker CLI.
#
# The link command (including ffmpeg's `-l`-after-inputs ordering) comes
# from Go — see resolve-script.nix — so native and sandbox mode can't
# diverge on flag order the way they once did.
{
  compilerRoot  ? (import ./toolchain.nix).compilerRoot,
  bashRoot      ? (import ./toolchain.nix).bashRoot,
  coreutilsRoot ? (import ./toolchain.nix).coreutilsRoot,
  outName,
  # Derivation's own name — a multi-target mkNixggBuild build overrides
  # this to "<outerBuildName>-<targetKey>" to match Nix's
  # outputPathName(outerName, outputKey) check; see
  # go/internal/shim/link.go's linkSandbox.
  name ? "bin-${outName}",
  inputs,
  # Dependency-only inputs: same { drv, name } shape as `inputs`, but
  # never interpolated into the script — only into this derivation's
  # dependency edges (via _extraInputs below). A thin archive's members
  # must be MOUNTED (the archive stores absolute path references, not
  # bytes) without appearing a second time on the link/ar command line,
  # which would make the linker see each symbol twice.
  extraInputs ? [ ],
  scriptTemplate,
  markerTag,
  storeDepsJSON ? "[]",
  wrapperEnvJSON ? "{}",
  # Staged directory of local files the link command needs present
  # (e.g. a generated linker script for -Wl,--version-script=<relpath>).
  # Same Nix-path-literal convention as builder.nix's srcTree; null when
  # the link has none.
  srcTree ? null,
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
  # Dependency-only: Nix's string-context scan picks up each
  # `${item.drv}/${item.name}` reference and adds the inputDrvs/inputSrcs
  # edge, but this text never reaches the script.
  _extraInputs = builtins.concatStringsSep ":"
    (map (i: "${i.drv}/${i.name}") extraInputs);
} // wrapperEnv // (if srcTree == null then { } else { src = srcTree; }))
