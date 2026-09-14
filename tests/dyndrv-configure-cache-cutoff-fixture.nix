# splitStdenv (splitAtConfigure=true, splitAtBuild=true) early-cutoff
# test fixture, driven by tests/dyndrv-configure-cache-cutoff.sh. Same
# shape as tests/configure-cache-cutoff-fixture.nix (constructs the
# package directly, not via pkgs.foo.override, which always
# re-invokes the wrapped function with its original args first,
# discarding any prior .overrideAttrs).
#
# `package` selects "hello" (single-output) or "gdbm" (multi-output:
# out/dev/info/lib/man, AC_CONFIG_SRCDIR = src/gdbmdefs.h), so the same
# cutoff mechanics get exercised against both.
{
  flakeDir,
  edit ? null, # null | "excluded" | "included"
  package ? "hello", # "hello" | "gdbm"
}:
let
  flake = builtins.getFlake (toString flakeDir);
  nixpkgsFlake = flake.inputs.nix-15793.inputs.nixpkgs;
  pkgs = nixpkgsFlake.legacyPackages.${builtins.currentSystem};
  splitStdenv = flake.outputs.packages.${builtins.currentSystem}.splitStdenv;
  configureSrcFilterPresets = flake.outputs.packages.${builtins.currentSystem}.configureSrcFilterPresets;

  fixtures = {
    hello = {
      pname = "hello";
      version = "2.12.3";
      src = pkgs.hello.src;
      excludedEditPath = "src/hello.c";
      includedEditPath = "configure.ac";
      existenceStubs = [ "src/hello.c" ];
    };
    gdbm = {
      pname = "gdbm";
      version = pkgs.gdbm.version;
      src = pkgs.gdbm.src;
      excludedEditPath = "src/avail.c";
      includedEditPath = "configure.ac";
      existenceStubs = [ "src/gdbmdefs.h" ];
    };
  };
  f = fixtures.${package};

  excludedEditLine = "/* excluded-file cutoff test */\n";
  includedEditLine = "dnl included-file cutoff test\n";

  editedSrc =
    let
      path = if edit == "excluded" then f.excludedEditPath else f.includedEditPath;
      line = if edit == "excluded" then excludedEditLine else includedEditLine;
    in
    pkgs.runCommand "${f.pname}-src-edited" { nativeBuildInputs = [ pkgs.gnutar pkgs.gzip ]; } ''
      mkdir -p "$out"
      if [ -d ${f.src} ]; then
        cp -a ${f.src}/. "$out"
      else
        tar xf ${f.src} -C "$out" --strip-components=1
      fi
      chmod -R u+w "$out"
      printf '%s' ${builtins.toJSON line} >> "$out"/${path}
    '';

  src = if edit == null then f.src else editedSrc;
in
(splitStdenv {
  stdenv = pkgs.stdenv;
  splitAtConfigure = true;
  splitAtBuild = true;
  configureSrcFilter = {
    includePatterns = configureSrcFilterPresets.autotools;
    existenceStubs = f.existenceStubs;
  };
}).mkDerivation {
  pname = f.pname;
  version = f.version;
  inherit src;
}
