# An out-of-tree Linux kernel module through nixgg, in two phases.
#
# WHY THIS EXISTS
#
# It is the cheapest possible probe of "can nixgg accelerate a kbuild
# build?", which is the question standing between this project and the
# NixOS kernel. A full kernel is ~10-20k TUs and a 30-minute failure
# loop; this is 2 TUs and seconds.
#
# It works: `nix build .#kmod` produces a loadable hello_mod.ko with the
# right vermagic, from two per-TU content-addressed derivations.
#
# Getting here required two fixes to nixgg itself, both of which were
# latent bugs rather than kernel-specific special-casing — see
# internal/scan's StripWpDep and internal/shim's writeDepFile. Any build
# system that passes `-Wp,-MMD,<file>` (kbuild does, on every compile)
# was silently losing its ENTIRE header list, because that flag
# redirects dependency output to a file and so leaves the scanner's
# `gcc -M -MG -MF -` with empty stdout.
#
# THE OBSTACLE IT WORKS AROUND
#
# A single-phase `make modules` through the shims does NOT work, and the
# reason generalises to the whole kernel: **kbuild reads the bytes of
# every object it produces, inside the same make run.** nixgg's central
# bet is that a compile output can stay an unresolved drvref stub until
# Nix realises it later. kbuild breaks that bet three times per module:
#
#   1. objtool, in the same rule as the compile
#      (scripts/Makefile.build's `$(call if_changed_rule,cc_o_c)`),
#      fails with `gelf_getehdr failed: invalid 'Elf' handle` —
#      it is reading a stub, not an ELF file.
#   2. modpost, which reads each object's symbol table to synthesise
#      the module's .mod.c. Given a stub it derives no module at all,
#      and __modfinal then reports `No rule to make target
#      'hello_mod.ko'`.
#   3. the `ld -r` that fuses the parts into the .ko.
#
# (1) and (2) were both observed directly, in that order. To reproduce:
# point `objects` at `modules` instead, i.e. do the whole build in
# phase 1.
#
# THE SPLIT
#
# Phase 1 compiles the translation units and stops. Phase 2 does
# everything that needs to read object bytes, by which point Nix has
# realised phase 1's outputs into real store paths.
#
# Two pieces of kbuild machinery make the boundary land exactly where
# it needs to, rather than requiring us to fight kbuild:
#
#   - Building `main.o helper.o` rather than `modules` triggers kbuild's
#     own single-target mode (`single-targets := … %.o …` in the top
#     Makefile), which sets single-build=1 and skips the modpost goal.
#   - hello_mod is deliberately a MULTI-object module. This kernel is
#     built with CONFIG_X86_KERNEL_IBT=y, so `delay-objtool := y`, and
#     Makefile.build's `objtool-enabled = $(if $(is-standard-object),
#     $(if $(delay-objtool),$(is-single-obj-m),y))` therefore runs
#     objtool per-TU only for SINGLE-object modules. Splitting the
#     module across two TUs moves objtool to the multi-obj link — i.e.
#     into phase 2, on real objects. A one-file module cannot be built
#     this way; it would need option (a) below.
#
# Phase 1 hands the whole tree over rather than a single artifact,
# because phase 2 needs more than the objects: kbuild's `if_changed`
# compares the recorded command line in each `.<obj>.o.cmd` against the
# command it would run now, and a missing .cmd means "rebuild". So the
# handoff has to carry the .cmd files too. That is exactly what `nixgg
# assemble` already does for dynDrvStdenv's phase 1 — walk the tree,
# resolve every stub, submit the whole thing as one output — so this
# reuses it rather than inventing a second mechanism.
#
# WHAT THIS DOES NOT SOLVE
#
# The multi-object trick is a property of MODULE builds. It does not
# carry over to vmlinux's own built-in objects — but, on this kernel's
# config, it turns out not to need to: kbuild already defers objtool for
# built-ins on its own. scripts/Makefile.vmlinux_o says so directly —
# "For delay-objtool (IBT or LTO), objtool doesn't run on individual
# translation units. Instead it runs on vmlinux.o." NixOS builds with
# CONFIG_X86_KERNEL_IBT=y, and CONFIG_FTRACE_MCOUNT_USE_CC=y means
# -mrecord-mcount replaces the separate recordmcount pass too. So for
# ordinary built-in objects the per-TU pipeline is just compile+fixdep,
# both of which nixgg now handles.
#
# What this example does NOT cover, and what a real kernel hits next:
# built-in.a is a THIN archive at every level of the tree
# (scripts/Makefile.build's `cmd_ar_builtin` → `$(AR) cDPrST`), whose
# members are recorded as paths rather than bytes; and that ar runs
# under `xargs`, which splits into multiple appending invocations once a
# directory has enough objects — a shape internal/shim/archive.go
# explicitly does not model ("every archive we build is fresh").
{
  mkNixggBuild,
  stdenv,
  kernel,
  kmod,
  src,
}:

let
  kdir = "${kernel.dev}/lib/modules/${kernel.modDirVersion}/build";

  # Kept as bindings because the assemble call below has to reproduce
  # mkNixggBuild's own naming exactly; spelling either out twice would
  # rot the moment one changed.
  partsPname = "nixgg-kmod-parts";
  partsTarget = "modtree";

  # Phase 1 — accelerated. Every cc invocation becomes its own
  # content-addressed derivation, exactly as in every other example.
  parts = mkNixggBuild {
    pname = partsPname;
    version = kernel.modDirVersion;
    inherit src;

    # Never matched by any link/archive shim: `nixgg assemble` owns the
    # single submit-output call for this derivation, and a per-artifact
    # submit racing it would be wrong. Same reasoning as dynDrvStdenv's
    # NIXGG_SANDBOX_TARGET=/nonexistent/…, just spelled differently
    # because mkNixggBuild derives its drv name from `targets`.
    targets = [ { name = partsTarget; path = partsTarget; } ];

    nativeBuildInputs = [ kmod ];

    # kernel.dev must be a real buildInput, not merely interpolated into
    # buildCommand below. mkNixggBuild derives NIXGG_KNOWN_STORE_PATHS
    # from buildInputs ++ propagatedBuildInputs, and storedeps.From only
    # promotes a /nix/store path into a per-TU drv's inputs.srcs if it
    # matches that list. kbuild's compile line carries
    # `-I<kernel-dev>/…/source/include` and
    # `-include <kernel-dev>/…/compiler-version.h`; without this the
    # inner drvs build in a sandbox where none of that exists and fail
    # with `compiler-version.h: No such file or directory`. Same failure
    # mode the zlib/ncurses `-dev`-output note on knownStorePathInputs
    # in nix/mkNixggBuild.nix describes.
    buildInputs = [ kernel.dev ];

    # The assemble name and output key must agree with what
    # submit-output enforces: the submitted drv has to be named
    # outputPathName(<outer name>, <output key>), which is the outer
    # derivation's name plus "-" plus the key minus its ".drv" suffix.
    # mkNixggBuild names the outer derivation "nixgg-${pname}" and
    # declares one output per entry in `targets`, keyed "<name>.drv" —
    # there is no "out" output at all, so `nixgg assemble` has to be
    # told the key explicitly or it fails with "submitted unknown
    # output 'out'".
    buildCommand = ''
      make objects KDIR=${kdir}
      nixgg assemble "$NIX_BUILD_TOP" "nixgg-${partsPname}-${partsTarget}" "${partsTarget}.drv"
    '';
  };

  # Phase 2 — deliberately NOT accelerated, and a plain derivation
  # rather than a second mkNixggBuild. Everything left to do reads
  # object bytes, which is precisely what the shims cannot serve; there
  # is nothing here for them to speed up.
  module = stdenv.mkDerivation {
  pname = "nixgg-kmod";
  version = kernel.modDirVersion;
  dontUnpack = true;
  nativeBuildInputs = [ kmod ];

  buildPhase = ''
    runHook preBuild

    # phase 1's tree, with every stub replaced by its realised artifact.
    cp -a ${parts.result}/. .
    chmod -R u+w .
    cd mod

    make modules KDIR=${kdir}

    runHook postBuild
  '';

  installPhase = ''
    runHook preInstall

    mkdir -p "$out/lib/modules/${kernel.modDirVersion}/extra"
    cp hello_mod.ko "$out/lib/modules/${kernel.modDirVersion}/extra/"

      runHook postInstall
    '';
  };
in
{
  # Same shape every other example returns, so flake.nix's exampleDefs
  # mapAttrs picks it up unchanged: `.package` is the thing to build,
  # `.shell` mirrors the accelerated phase's stdenv env.
  inherit (parts) drv shell result;
  package = module;
  # Phase 1 on its own, so the accelerated half can be built and
  # inspected without running phase 2 — same reason examples/two-phase
  # exposes `codegen` and examples/llvm exposes its tblgen phases.
  inherit parts;
}
