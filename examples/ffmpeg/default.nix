# ffmpeg — stress test for shim submission throughput: ~1200 TUs,
# core codecs only, custom (non-autotools) configure + recursive make.
{
  mkNixggBuild,
  src,
  pkg-config,
  perl,
  nasm,
  yasm,
  gnumake,
  which,
  batchGroups ? [ ],
}:

mkNixggBuild {
  pname = "ffmpeg";
  version = "7.1.2";
  inherit src batchGroups;
  # Unstripped: the link shim leaves a drvref stub, not a real ELF, so
  # ffmpeg's usual `strip ffmpeg_g -o ffmpeg` step would fail.
  targets = [ { name = "ffmpeg_g"; path = "ffmpeg_g"; } ];
  nativeBuildInputs = [ pkg-config perl nasm yasm gnumake which ];
  buildInputs = [ ];
  buildCommand = ''
    # NIXGG_BYPASS so configure's probe compilations get real binaries.
    NIXGG_BYPASS=1 ./configure \
      --cc=cc \
      --disable-doc \
      --disable-htmlpages \
      --disable-manpages \
      --disable-podpages \
      --disable-x86asm

    make -j"$NIX_BUILD_CORES" ffmpeg_g
  '';
}
