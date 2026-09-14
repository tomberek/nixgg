# splitStdenv (splitAtBuild=true) applied to zstd, demonstrating the
# phase-chaining pattern for packages that exec one of their own
# binaries mid-build.
#
# zstd's cmake graph execs `contrib/gen_html` mid-build to render
# zstd_manual.html; in sandbox mode the link shim leaves a drvref stub
# there instead of a real executable, so `./gen_html` gets "Permission
# denied".
#
# A plain .overrideAttrs patch can't fix this: nixpkgs' own
# .override/.overrideAttrs reapplication rebuilds from ORIGINAL attrs
# first, closing over the build stage before any caller-supplied
# .overrideAttrs runs (verified: both orderings produce a
# byte-identical build-stage hash). Use splitStdenv's `extraBuildAttrs`
# instead — spliced in before the build stage is computed.
{
  pkgs,
  mkNixggBuild,
  splitStdenv,
}:

let
  genHtml = mkNixggBuild {
    pname = "zstd-gen-html";
    version = "0";
    src = pkgs.zstd.src;
    targets = [ { name = "gen_html"; path = "gen_html"; } ];
    buildCommand = ''
      cd contrib/gen_html
      g++ -O2 -c gen_html.cpp -o gen_html.o
      g++ gen_html.o -o gen_html
    '';
  };
in
pkgs.zstd.override {
  stdenv = splitStdenv {
    stdenv = pkgs.stdenv;
    splitAtBuild = true;
    extraBuildAttrs = finalAttrs: old: old // {
      # Removes gen_html's add_executable + DEPENDS edge, and points
      # GENHTML_BINARY at phase A's binary instead.
      postPatch =
        old.postPatch
        + ''
          substituteInPlace build/cmake/contrib/gen_html/CMakeLists.txt \
            --replace-fail \
              'add_executable(gen_html ''${GENHTML_DIR}/gen_html.cpp)' \
              "" \
            --replace-fail \
              'DEPENDS gen_html COMMENT "Update zstd manual")' \
              'COMMENT "Update zstd manual")' \
            --replace-fail \
              'set(GENHTML_BINARY ''${PROJECT_BINARY_DIR}/gen_html''${CMAKE_EXECUTABLE_SUFFIX})' \
              'set(GENHTML_BINARY ${genHtml.package}/bin/gen_html)'
        '';
    };
  };
}
