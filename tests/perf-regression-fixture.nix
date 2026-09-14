# mkNixggBuild per-TU rebuild-scope test fixture, driven by
# tests/perf-regression.sh. Constructs the package directly from the
# pinned flake input (not via the flake's own `.#lua` output, whose
# src is fixed and can't be threaded with an edit) through the same
# mkNixggBuild call examples/lua/default.nix makes.
{
  flakeDir,
  edit ? null, # null | a relative path inside lua's src/ to touch
}:
let
  flake = builtins.getFlake (toString flakeDir);
  system = builtins.currentSystem;
  pkgs = flake.inputs.nixpkgs.legacyPackages.${system};
  mkNixggBuild = flake.outputs.packages.${system}.mkNixggBuild;

  realSrc = flake.inputs.lua-src;

  editedSrc =
    pkgs.runCommand "lua-src-edited" { } ''
      mkdir -p "$out"
      cp -a ${realSrc}/. "$out"
      chmod -R u+w "$out"
      printf '\n/* perf-regression touch */\n' >> "$out"/${edit}
    '';

  src = if edit == null then realSrc else editedSrc;
in
import ../examples/lua { inherit mkNixggBuild src; }
