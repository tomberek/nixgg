# PostgreSQL — a much larger real-world autoconf project than mosh
# (src/backend alone is ~1000+ TUs).
#
# Builds src/backend ONLY (the postgres server binary), not `make
# world`, docs, contrib/, libpq, or the src/bin/ tools.
#
# --without-icu/--without-readline/--without-zlib trim nixpkgs' full
# postgresql recipe down to the minimum that still produces a real,
# runnable postgres binary.
#
# CFLAGS="-std=gnu17" works around GCC 15's C23 default treating `bool`
# as a reserved keyword, which breaks src/include/c.h's own
# `typedef unsigned char bool;` compat shim.
#
# `make -C src/backend generated-headers` runs first, unshimmed and
# non-parallel: it materializes gram.c/scan.c (bison/flex), errcodes.h,
# and the catalog headers.
#
# Deliberately not run with `make -j`: confirmed directly that `-j` on
# this subdir-only invocation hits a real recursive-make ordering race
# in PostgreSQL's own Makefile.global ("No rule to make target
# '../../src/common/libpgcommon_srv.a'"), unrelated to nixgg's shims.
{
  mkNixggBuild,
  src,
  bison,
  flex,
  perl,
  batchGroups ? [ ],
}:

mkNixggBuild {
  pname = "postgresql";
  version = "17.2";
  inherit src batchGroups;
  targets = [ { name = "postgres"; path = "src/backend/postgres"; } ];
  nativeBuildInputs = [ bison flex perl ];
  buildCommand = ''
    NIXGG_BYPASS=1 ./configure \
      --without-icu --without-readline --without-zlib \
      CFLAGS="-std=gnu17 -O2"
    NIXGG_BYPASS=1 make -C src/backend generated-headers
    make -C src/backend
  '';
}
