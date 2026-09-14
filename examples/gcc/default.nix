# GCC 15.3.0's libiberty/ — a real static archive (~65 .o members)
# built via libiberty's own standalone ./configure, not gcc's top-level
# multi-package build.
#
# Deliberately scoped to libiberty alone, not cc1/cc1plus/xgcc: a full
# GCC bootstrap hits hazards nixgg's shims don't model yet, all
# injected by the TOP-LEVEL build (GMP/MPFR/MPC version checks, `ar
# --plugin` decoration via top-level configure.ac, `ar rcT` thin
# archives for libbackend.a, generated sources exec'd mid-build) —
# none of which libiberty/Makefile.in triggers on its own. Verified
# directly against the real gcc-15.3.0 tarball: libiberty's build log
# shows plain `ar rc ./libiberty.a <65 .o files>` / `ranlib`, no
# --plugin, no `T`, no generated-tool exec.
{
  mkNixggBuild,
  src,
  batchGroups ? [ ],
}:

mkNixggBuild {
  pname = "gcc-libiberty";
  version = "15.3.0";
  inherit src batchGroups;
  targets = [ { name = "libiberty"; path = "libiberty.a"; } ];
  buildCommand = ''
    cd libiberty
    NIXGG_BYPASS=1 ./configure --disable-shared
    make -j"$NIX_BUILD_CORES"
  '';
}
