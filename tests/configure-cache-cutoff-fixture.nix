# splitStdenv (splitAtConfigure=true) early-cutoff test fixture,
# driven by tests/configure-cache-cutoff.sh.
#
# Constructs the package DIRECTLY (not via pkgs.foo.override, which
# always re-invokes the wrapped function with its ORIGINAL args first,
# discarding any prior .overrideAttrs — confirmed directly while
# building hello-cache-filtered/fmt-cache-filtered), parameterized to
# drive three scenarios: baseline, an edit to a file the filter
# excludes, an edit to a file it includes.
{
  flakeDir,
  fixture, # "hello" or "fmt"
  edit ? null, # null | "excluded" | "included"
}:
let
  flake = builtins.getFlake (toString flakeDir);
  nixpkgsFlake = flake.inputs.nix-15793.inputs.nixpkgs;
  pkgs = nixpkgsFlake.legacyPackages.${builtins.currentSystem};
  splitStdenv = flake.outputs.packages.${builtins.currentSystem}.splitStdenv;
  configureSrcFilterPresets = flake.outputs.packages.${builtins.currentSystem}.configureSrcFilterPresets;

  fixtures = {
    hello = {
      realSrc = pkgs.hello.src;
      excludedEditPath = "src/hello.c";
      excludedEditLine = "/* excluded-file cutoff test */\n";
      includedEditPath = "configure.ac";
      includedEditLine = "dnl included-file cutoff test\n";
      configureSrcFilter = {
        includePatterns = configureSrcFilterPresets.autotools;
        existenceStubs = [ "src/hello.c" ];
      };
      mk =
        { stdenv, src }:
        stdenv.mkDerivation (finalAttrs: {
          pname = "hello";
          version = "2.12.3";
          inherit src;
          doCheck = true;
          doInstallCheck = true;
          nativeInstallCheckInputs = [ pkgs.versionCheckHook ];
          postInstallCheck = ''
            stat "${"$"}{!outputBin}/bin/${finalAttrs.meta.mainProgram}"
          '';
          meta.mainProgram = "hello";
        });
    };
    fmt = {
      realSrc = pkgs.fmt.src;
      excludedEditPath = "LICENSE";
      excludedEditLine = "// excluded-file cutoff test\n";
      includedEditPath = "CMakeLists.txt";
      includedEditLine = "# included-file cutoff test\n";
      configureSrcFilter = {
        includePatterns = configureSrcFilterPresets.cmake ++ [
          "include/fmt/*.h"
          "src/*.cc"
          "README.md"
          "ChangeLog.md"
          "support/cmake/*.in"
          "test"
          "test/*"
          "test/*/*"
        ];
      };
      mk =
        { stdenv, src }:
        stdenv.mkDerivation {
          pname = "fmt";
          version = "12.1.0";
          inherit src;
          nativeBuildInputs = [ pkgs.cmake pkgs.ninja ];
          cmakeFlags = [ "-DBUILD_SHARED_LIBS=TRUE" ];
          doCheck = false;
        };
    };
  };

  f = fixtures.${fixture};

  editedSrc =
    let
      path = if edit == "excluded" then f.excludedEditPath else f.includedEditPath;
      line = if edit == "excluded" then f.excludedEditLine else f.includedEditLine;
    in
    pkgs.runCommand "${fixture}-src-edited" { nativeBuildInputs = [ pkgs.gnutar pkgs.gzip ]; } ''
      mkdir -p "$out"
      if [ -d ${f.realSrc} ]; then
        cp -a ${f.realSrc}/. "$out"
      else
        tar xf ${f.realSrc} -C "$out" --strip-components=1
      fi
      chmod -R u+w "$out"
      printf '%s' ${builtins.toJSON line} >> "$out"/${path}
    '';

  src = if edit == null then f.realSrc else editedSrc;
in
f.mk {
  inherit src;
  stdenv = splitStdenv {
    stdenv = pkgs.stdenv;
    splitAtConfigure = true;
    configureSrcFilter = f.configureSrcFilter;
  };
}
