# A two-crate Rust build driven by make and bare rustc — the shape
# kbuild uses, and the only shape the rustc shim models.
#
# Deliberately not cargo. Cargo drives rustc through its own protocol
# and picks the compiler from a toolchain file rather than PATH, so a
# shim never sees the invocation. A build system that calls rustc
# directly does, which is what this covers.
#
# What it exercises that nothing else does:
#
#   - one invocation, two artifacts. `dep` emits both an object and the
#     .rmeta that `app` resolves --extern against, and both become
#     drvref stubs pointing at the same derivation.
#   - a crate compiled against a dependency that is a STUB. The
#     dependency scan runs while libdep.rmeta is a drvref stub rather
#     than a loadable crate, which is the normal case under nixgg and
#     the reason scan.RunRust ignores rustc's exit status.
#   - `include!`, which no preprocessor-style scanner can see.
#   - an @-argfile, whose format is rustc's own and not the compiler
#     drivers'. app/main.rs fails to compile if the cfg it carries goes
#     missing, so a regression there is loud rather than silent.
#   - the ar shim over rustc-produced stubs.
#
# Not in tests/drv-equivalence.sh, and not an oversight: the rustc shim
# is sandbox-only, so native mode runs rustc for real and produces no
# derivations to compare against. There is no equivalence to pin.
{
  mkNixggBuild,
  rustc,
  src,
}:

mkNixggBuild {
  pname = "rustc-example";
  version = "0";
  inherit src;
  targets = [ { name = "libapp"; path = "libapp.a"; } ];
  nativeBuildInputs = [ rustc ];
  buildCommand = "make -j\"$NIX_BUILD_CORES\"";
}
