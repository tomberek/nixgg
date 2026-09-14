# redis — plain-Makefile server. Cross-referenced against nixpkgs'
# redis package.nix; same nativeBuildInputs, same use-system-lua
# strategy. Top-level Makefile recurses into deps/{hiredis,linenoise,
# lua,hdr_histogram,fpconv,fast_float,jemalloc}, each its own make
# producing a .a.
#
# SOURCE_DATE_EPOCH is pinned: src/mkreleasehdr.sh bakes
# `<hostname>-<epoch>` into release.o, so without a fixed epoch every
# build produces a different CA hash for redis-server.
{
  mkNixggBuild,
  src,
  which,
  pkg-config,
  python3,
  lua,
  gnugrep,
  gnused,
  gawk,
  # batchGroups passthrough — see flake.nix's redis-batch entry (uses
  # nix/batchGroupPresets.nix's vendorDeps preset). Only 5 of deps/'s 7
  # subtrees are reachable from redis-server (MALLOC=libc excludes
  # jemalloc; linenoise is redis-cli-only); all 5 batch cleanly into 5
  # batch-lib*.a.drv derivations.
  batchGroups ? [ ],
}:

mkNixggBuild {
  pname = "redis";
  version = "8.2.2";
  inherit src batchGroups;
  targets = [ { name = "redis-server"; path = "redis-server"; } ];
  nativeBuildInputs = [ pkg-config which python3 gnugrep gnused gawk ];
  buildInputs = [ lua ];
  buildCommand = ''
    export SOURCE_DATE_EPOCH=1700000000

    make -j"$NIX_BUILD_CORES" MALLOC=libc redis-server
  '';
}
