# pureStorePath variant for a full TOOL path that may include a
# subpath below the store root (e.g. ".../gcc-wrapper-15.3.0/bin/ld"),
# not just the root itself. pure-store-path.nix's own context tagging
# only accepts a genuine store root — confirmed directly: tagging a
# full ".../bin/ld" path throws "context key ... is not a store path"
# — so this splits root from subpath first (same split builder.nix
# already does for compilerRoot + "bin/g++", just computed from a
# single already-joined string instead of two separate parameters)
# and only tags the root.
#
# A root-only path (no subpath, e.g. objtool's own stage-tool.nix
# output) passes through unchanged other than the tagging itself.
path:
let
  pureStorePath = import ./pure-store-path.nix;
  rest = builtins.substring 11 (-1) path; # strip the "/nix/store/" prefix
  m = builtins.match "([^/]*)(/.*)?" rest;
  root = "/nix/store/" + builtins.elemAt m 0;
  sub = let s = builtins.elemAt m 1; in if s == null then "" else s;
in
"${pureStorePath root}${sub}"
