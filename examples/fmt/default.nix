# {fmt} — cmake+ninja build producing a static library (libfmt.a),
# exercising the archive shim path and cmake's compiler-probe carveout.
{
  mkNixggBuild,
  src,
  cmake,
  ninja,
  pkg-config,
  # batchGroups passthrough — see flake.nix's fmt-batch entry. fmt's own
  # target IS the archive (libfmt.a), and tryBatchArchive (go/internal/
  # shim/batcharchive.go) refuses to batch whichever archive matches
  # NIXGG_SANDBOX_TARGET, so fmt-batch builds correctly but batching does
  # not actually engage here.
  batchGroups ? [ ],
}:

mkNixggBuild {
  pname = "fmt";
  version = "11.0.2";
  inherit src batchGroups;
  targets = [ { name = "libfmt"; path = "libfmt.a"; } ];
  nativeBuildInputs = [ cmake ninja pkg-config ];
  buildCommand = ''
    # NIXGG_BYPASS=1 so cmake's compiler probes get real binaries; cmake
    # hard-codes the shim path into generated build files, so unsetting
    # BYPASS only after setup routes the real build through the shim.
    NIXGG_BYPASS=1 cmake -S . -B build -G Ninja \
      -DCMAKE_BUILD_TYPE=Release \
      -DFMT_TEST=OFF -DFMT_DOC=OFF \
      -DBUILD_SHARED_LIBS=OFF

    cmake --build build --target fmt
  '';
}
