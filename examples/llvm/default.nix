# Three-phase build: phase1 builds bootstrap llvm-min-tblgen; phase2
# builds full llvm-tblgen against it (-DLLVM_TABLEGEN=phase1); phase3
# builds `llc` via -DLLVM_NATIVE_TOOL_DIR so downstream .inc generation
# execs real ELFs instead of drvref stubs. Target is `llc`, not
# `llvm-config`: llvm-config's graph is only Support+TargetParser+
# Demangle (~480 TUs) and never touches the compiler.
#
# Scope: llvm only, X86 target only. Pinned to 19.x for GCC 15 compat;
# see flake.nix's llvm-src comment.
{
  mkNixggBuild,
  runCommand,
  src,               # llvm monorepo: llvm/, cmake/, third-party/ as siblings
  cmake,
  ninja,
  pkg-config,
  python3,
  perl,
  which,
  libffi,
  libxml2,
  ncurses,
  zlib,
  # batchGroups passthrough — see flake.nix's llvm-min-tblgen-batch entry.
  batchGroups ? [ ],
}:

let
  # One line, not a multi-line ''…'' block: a phase appending its own
  # `\`-continued flag would hit the block's trailing newline first and
  # end the command early.
  commonCmakeFlags = builtins.concatStringsSep " " [
    "-DCMAKE_BUILD_TYPE=Release"
    "-DLLVM_TARGETS_TO_BUILD=X86"
    "-DLLVM_ENABLE_PROJECTS="
    "-DLLVM_ENABLE_RTTI=ON"
    "-DLLVM_LINK_LLVM_DYLIB=OFF"
    "-DLLVM_BUILD_LLVM_DYLIB=OFF"
    "-DLLVM_BUILD_TESTS=OFF"
    "-DLLVM_INCLUDE_TESTS=OFF"
    "-DLLVM_BUILD_EXAMPLES=OFF"
    "-DLLVM_INCLUDE_EXAMPLES=OFF"
    "-DLLVM_BUILD_DOCS=OFF"
    "-DLLVM_INCLUDE_DOCS=OFF"
    "-DLLVM_BUILD_BENCHMARKS=OFF"
    "-DLLVM_INCLUDE_BENCHMARKS=OFF"
    "-DLLVM_ENABLE_ZSTD=OFF"
    "-DLLVM_ENABLE_LIBEDIT=OFF"
  ];

  commonNativeBuildInputs = [ cmake ninja pkg-config python3 perl which ];
  commonBuildInputs = [ libffi libxml2 ncurses zlib ];

  phase1 = mkNixggBuild {
    pname = "llvm-min-tblgen";
    version = "19.1.7";
    inherit src batchGroups;
    # name must be a plain name, not a path — becomes the output key
    # "llvm-min-tblgen.drv" and a "/" would be an invalid Nix output name.
    targets = [ { name = "llvm-min-tblgen"; path = "bin/llvm-min-tblgen"; } ];
    nativeBuildInputs = commonNativeBuildInputs;
    buildInputs = commonBuildInputs;
    buildCommand = ''
      NIXGG_BYPASS=1 cmake -S llvm -B build -G Ninja ${commonCmakeFlags}
      ninja -C build -j"$NIX_BUILD_CORES" llvm-min-tblgen
    '';
  };

  phase2 = mkNixggBuild {
    pname = "llvm-tblgen";
    version = "19.1.7";
    inherit src batchGroups;
    targets = [ { name = "llvm-tblgen"; path = "bin/llvm-tblgen"; } ];
    nativeBuildInputs = commonNativeBuildInputs;
    buildInputs = commonBuildInputs ++ [ phase1.result ];
    buildCommand = ''
      NIXGG_BYPASS=1 cmake -S llvm -B build -G Ninja ${commonCmakeFlags} \
        -DLLVM_TABLEGEN=${phase1.result}/bin/llvm-min-tblgen
      ninja -C build -j"$NIX_BUILD_CORES" llvm-tblgen
    '';
  };

  toolbin = runCommand "llvm-toolbin-19.1.7" { } ''
    mkdir -p $out
    ln -s ${phase1.result}/bin/llvm-min-tblgen $out/llvm-min-tblgen
    ln -s ${phase2.result}/bin/llvm-tblgen $out/llvm-tblgen
  '';

  phase3 = mkNixggBuild {
    pname = "llvm";
    version = "19.1.7";
    inherit src batchGroups;
    targets = [ { name = "llc"; path = "bin/llc"; } ];
    nativeBuildInputs = commonNativeBuildInputs;
    buildInputs = commonBuildInputs ++ [ toolbin ];
    buildCommand = ''
      NIXGG_BYPASS=1 cmake -S llvm -B build -G Ninja ${commonCmakeFlags} \
        -DLLVM_NATIVE_TOOL_DIR=${toolbin}
      ninja -C build -j"$NIX_BUILD_CORES" llc
    '';
  };
in
{
  inherit (phase3) drv shell result package;
  # Exposed for isolated smoke tests / debugging.
  llvm-min-tblgen = phase1;
  llvm-tblgen = phase2;
  llvm-toolbin = toolbin;
}
