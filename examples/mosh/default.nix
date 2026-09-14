# mosh — mobile shell. Real-world autoconf + protoc + libtool +
# pkg-config example.
{
  mkNixggBuild,
  src,
  autoconf,
  automake,
  libtool,
  pkg-config,
  perl,
  protobuf,
  which,
  gnum4,
  gnugrep,
  gnused,
  gawk,
  file,
  ncurses,
  openssl,
  zlib,
  abseil-cpp,
  # batchGroups passthrough — see flake.nix's mosh-batch entry, covering
  # all 6 of mosh's lib*.a archives at once.
  batchGroups ? [ ],
}:

mkNixggBuild {
  pname = "mosh";
  version = "unstable";
  inherit src batchGroups;
  # A LIST, not an attrset — order matters: the first entry is "the"
  # target for .#mosh's own result/package.
  targets = [ { name = "mosh-server"; path = "mosh-server"; } { name = "mosh-client"; path = "mosh-client"; } ];
  nativeBuildInputs = [
    autoconf
    automake
    libtool
    pkg-config
    perl
    protobuf
    which
    gnum4
    gnugrep
    gnused
    gawk
    file
  ];
  buildInputs = [ ncurses openssl zlib protobuf abseil-cpp ];
  buildCommand = ''
    [[ -x configure ]] || NIXGG_BYPASS=1 ./autogen.sh
    # --disable-dependency-tracking: the shim drops -M* dep-file flags,
    # so the .Tpo sidecar files autoconf's Makefiles expect never
    # appear, tripping `mv: cannot stat`.
    NIXGG_BYPASS=1 ./configure --disable-hardening --disable-dependency-tracking

    make -j"$NIX_BUILD_CORES"
  '';
}
