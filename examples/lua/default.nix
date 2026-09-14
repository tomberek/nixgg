# Lua 5.4 through nixgg. Plain Makefile, ~30 TUs, one archive
# (liblua.a), two link targets (lua, luac) via mkNixggBuild's
# multi-target `targets` param.
{
  mkNixggBuild,
  src,
  # batchGroups passthrough — see flake.nix's lua-batch entry: a small,
  # fast, single-archive fixture for tests/batch-drv-equivalence.sh.
  batchGroups ? [ ],
}:

mkNixggBuild {
  pname = "lua";
  version = "5.4.7";
  inherit src batchGroups;
  targets = [ { name = "lua"; path = "lua"; } { name = "luac"; path = "luac"; } ];
  buildCommand = ''
    cd src
    make linux CC=cc
  '';
}
