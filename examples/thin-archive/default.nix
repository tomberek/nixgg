# Minimal thin-archive fixture, reproducing the shape QEMU's meson
# build hits at scale (`ar --thin`: the consuming link needs the
# archive's own members as inputs, not just the archive itself). See
# go/internal/members and tests/thin-archive-equivalence.sh.
{
  mkNixggBuild,
  lib,
}:

let
  thinArchiveSrc = lib.cleanSourceWith {
    src = ./src;
    filter = path: type:
      let name = baseNameOf path; in
      name == "main.c" || name == "foo.c" || name == "bar.c" || name == "Makefile";
  };
in

mkNixggBuild {
  pname = "thin-archive";
  version = "0";
  src = thinArchiveSrc;
  targets = [ { name = "thin-archive"; path = "thin-archive"; } ];
  buildCommand = "make";
}
