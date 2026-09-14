# Two-phase mkNixggBuild — smoke test for the "phase-1 output as
# input to phase-2" pattern LLVM's tblgen mid-build execution needs.
#
#   phase1 (codegen/) → produces the `codegen` binary
#   phase2 (app/)     → buildInputs = [ phase1.result ]; CODEGEN=
#                       ${phase1.result}/bin/codegen passed to the
#                       Makefile so `codegen hello > generated.h` runs
#                       mid-build.
{
  mkNixggBuild,
  codegenSrc,
  appSrc,
}:

let
  phase1 = mkNixggBuild {
    pname = "two-phase-codegen";
    version = "0";
    src = codegenSrc;
    targets = [ { name = "codegen"; path = "codegen"; } ];
    buildCommand = "make -j\"$NIX_BUILD_CORES\"";
  };

  phase2 = mkNixggBuild {
    pname = "two-phase-app";
    version = "0";
    src = appSrc;
    targets = [ { name = "app"; path = "app"; } ];
    buildInputs = [ phase1.result ];
    buildCommand = ''
      make -j"$NIX_BUILD_CORES" CODEGEN=${phase1.result}/bin/codegen
    '';
  };
in
{
  inherit (phase2) drv shell result package;
  codegen = phase1;
}
