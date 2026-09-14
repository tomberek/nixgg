# QEMU — meson+ninja, a build-system genre no other example covers
# (plain Makefile, autotools, cmake+ninja). Needs zero meson-specific
# code: go/internal/dispatch.ExpandRspfiles already generically
# flattens ninja's @rspfile convention.
#
# Builds ONE target (x86_64-softmmu) via `--target-list`, tools/tests/
# docs disabled — still ~1700 real build steps.
#
# `python3.withPackages` needs distlib+setuptools explicitly: meson's
# mkvenv bootstrap fails ("found no usable distlib") against a bare
# python3, since nixpkgs' python3 doesn't ship distlib by default.
#
# The final link script measured at ~38.7KB, well under the
# MAX_ARG_STRLEN ceiling (131072 bytes, see ARCHITECTURE.md).
{
  mkNixggBuild,
  src,
  pkg-config,
  meson,
  ninja,
  glib,
  pixman,
  ncurses,
  zlib,
  pythonWithMesonDeps,
  # batchGroups passthrough — see flake.nix's qemu-batch entry, which
  # batches libqemuutil.a's own 450 members.
  batchGroups ? [ ],
}:

mkNixggBuild {
  pname = "qemu";
  version = "9.2.0";
  inherit src batchGroups;
  targets = [ { name = "qemu-system-x86_64"; path = "build/qemu-system-x86_64"; } ];
  nativeBuildInputs = [ pkg-config meson ninja pythonWithMesonDeps ];
  buildInputs = [ glib pixman ncurses zlib ];
  buildCommand = ''
    # mkNixggBuild sets dontFixup = true, so patchShebangs never runs
    # over the source tree; meson's plugins/meson.build execs
    # qemu-plugin-symbols.py directly by its raw #!/usr/bin/env python3
    # shebang, which fails since /usr/bin/env doesn't exist in the
    # sandbox.
    patchShebangs scripts/

    NIXGG_BYPASS=1 ./configure \
      --target-list=x86_64-softmmu \
      --disable-docs \
      --disable-fdt

    unset NIXGG_BYPASS
    ninja -C build -j"$NIX_BUILD_CORES" qemu-system-x86_64
  '';
}
