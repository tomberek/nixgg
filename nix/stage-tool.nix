# Copies a real, working-tree binary into the store via an ordinary CA
# derivation, so Nix's own build-time reference scan
# (scanForReferences, unconditional for any derivation output, not a
# recursive-nix-only mechanism) records its real dependencies — the
# native-mode equivalent of sandbox mode's `nix store add --scan`.
#
# Why this exists: a self-built tool (objtool, compiled by the wrapped
# project itself) is a dynamically-linked ELF with an absolute RPATH
# into the store (elfutils, glibc, ...). Sandbox mode's storeAddTool
# uploads it via the scanning op, which only works inside a
# builder-rpc-v0/recursive-nix session — confirmed rejected outright
# ("perhaps this is not in a recursive-nix builder?") under a plain
# `nix develop` connection, which is all native mode ever has. A bare
# path-literal add (Compile's own srcTree convention) records ZERO
# references, so the resulting store path — content-addressed over
# NAR bytes AND references together — differs from sandbox's scanned
# copy even for byte-identical file content.
#
# The fix needs no daemon RPC at all: routing the copy through an
# ordinary derivation build makes Nix's OWN build machinery scan the
# output exactly like every other derivation's output already is
# (derivation-builder-impl.cc's scanForReferences, run for every build
# unless unsafeDiscardReferences is set) — referenceablePaths there is
# the derivation's own inputSrcs/inputDrvs, which storeDeps below
# populates with the same known-store-paths list
# (go/internal/storedeps.FromFile) sandbox mode's scan would find.
# Confirmed empirically: staging with the SAME `name` and `storeDeps`
# as storeAddTool's sandbox-mode call produces the byte-identical
# store path (same hash, same references, same content).
{
  bashRoot      ? (import ./toolchain.nix).bashRoot,
  coreutilsRoot ? (import ./toolchain.nix).coreutilsRoot,
  name,
  toolBinPath,   # Nix path literal: the real binary in the working tree
  storeDepsJSON ? "[]",
}:
let
  pureStorePath = import ./pure-store-path.nix;
  bash        = pureStorePath bashRoot;
  coreutils   = pureStorePath coreutilsRoot;
  storeDeps   = map pureStorePath (builtins.fromJSON storeDepsJSON);
in
derivation {
  name = name;
  system = builtins.currentSystem;

  __contentAddressed = true;
  outputHashMode = "nar";
  outputHashAlgo = "sha256";

  builder = "${bash}/bin/bash";
  args = [
    "-c"
    ''
      export PATH="${coreutils}/bin"
      cp ${toolBinPath} "$out"
      chmod u+w "$out"
    ''
  ];

  # Dependency-only: makes elfutils/glibc/... part of referenceablePaths
  # for this build's own scanForReferences pass — never appears on the
  # copy command line, matches how every other Kind's _storeDeps works.
  _storeDeps = builtins.concatStringsSep ":" storeDeps;
}
