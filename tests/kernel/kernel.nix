# A full Linux kernel built through nixgg's dynamic derivations.
#
# NOT wired into tests/drv-equivalence.sh, tests/smoke.sh, or the flake's
# checks, and deliberately so: a from-scratch run realises ~29,000
# derivations and takes hours. See ./README.md for how to run it.
#
#
# WHY THE TWO-PHASE SHAPE IS FORCED
#
# `make vmlinux_o` cannot work in one phase. Makefile:1236:
#
#   cmd_ar_vmlinux.a = rm -f $@;                                       \
#     $(AR) cDPrST $@ $(KBUILD_VMLINUX_OBJS);                          \
#     $(AR) mPiT $$($(AR) t $@ | sed -n 1p) $@ $$($(AR) t $@ | grep ...)
#
# One recipe creates vmlinux.a, then lists it with `ar t`, pipes the
# member names through sed/grep, and reorders members in place. It
# inspects its own output inline — the same undeferrable shape as
# realmode and libstub, but on the artifact that aggregates the whole
# kernel, so it cannot be carved out either: passing it through needs
# real inputs, and its inputs are every built-in.a in the tree.
#
# So vmlinux.a is where deferral has to end. Phase 1 models every object
# and per-directory archive; phase 2 runs the final assembly against the
# outputs Nix has realised.
{
  # The nixgg flake. The default resolves this repo, which needs
  # --impure because getFlake wants a real path.
  nixgg ? builtins.getFlake (toString ../..),
  system ? builtins.currentSystem,
  # Stage each TU as a symlink farm into per-file store objects rather
  # than copying. Without it a kernel stages ~150 GB of duplicated
  # headers; with it, a fraction of that.
  sharedStaging ? true,
}:

let
  pkgs = nixgg.inputs.nixpkgs.legacyPackages.${system};
  splitStdenv = nixgg.packages.${system}.splitStdenv;
  nixggBin = nixgg.packages.${system}.nixgg-bin;

  # The kernel needs the UNWRAPPED ld — common-flags.nix notes the
  # wrapper breaks it — so hand the shim that exact binary rather than
  # letting it guess from the compiler directory.
  ldReal = "${pkgs.stdenv.cc.bintools.bintools}/bin/${pkgs.stdenv.cc.targetPrefix}ld";

  # splitStdenv wraps every derivation built with it, including the
  # kernel's own dependencies. Only reshape the kernel itself.
  onlyKernel = g: finalAttrs: old: if (old.pname or "") == "linux" then g finalAttrs old else old;

  # Every top-level directory that produces a built-in.a, plus arch/x86.
  # Phase 1 builds these rather than `vmlinux`, stopping short of the
  # vmlinux.a step described above.
  dirs = [
    "init/"
    "usr/"
    "kernel/"
    "certs/"
    "mm/"
    "fs/"
    "ipc/"
    "security/"
    "crypto/"
    "block/"
    "io_uring/"
    "rust/"
    "drivers/"
    "sound/"
    "net/"
    "lib/"
    "virt/"
    "arch/x86/"
  ];

  # nixpkgs passes absolute tool paths precisely to defeat PATH
  # interposition, so every shim needs naming explicitly — putting them
  # on PATH accomplishes nothing here. Each entry cost a failed run to
  # learn; omitting OBJCOPY, for instance, silently accelerates nothing
  # while looking like it works.
  #
  # `objtool` is lowercase and assigned with `:=` in scripts/Makefile.lib
  # with no `override`, so a command-line assignment wins. Safe to
  # override: the rule that BUILDS objtool keys on `tools/objtool` and a
  # separate `objtool_O`, not on `$(objtool)`.
  shimFlags = [
    "CC=${nixggBin}/shims/gcc"
    "AR=${nixggBin}/shims/ar"
    "objtool=${nixggBin}/shims/objtool"
    "LD=${nixggBin}/shims/ld"
    "OBJCOPY=${nixggBin}/shims/objcopy"
  ];

  # The shims need the real binaries to store-add. objtool is built by
  # kbuild during `make prepare`, so it exists only once configurePhase
  # has run — hence a hook rather than an attribute.
  realToolEnv = ''
    export NIXGG_REAL_OBJTOOL="$buildRoot/tools/objtool/objtool"
    export NIXGG_REAL_LD="${ldReal}"
    export NIXGG_REAL_OBJCOPY="${pkgs.stdenv.cc}/bin/objcopy"

    # Variables the Rust crates read at COMPILE time, which have to
    # travel into each crate's derivation — see internal/shim/rustcenv.go
    # for why they cannot be inferred.
    #
    #   OBJTREE       rust/{kernel,bindings,uapi} do
    #                 include!(concat!(env!("OBJTREE"), "/rust/…")) to
    #                 reach bindgen's generated bindings. The shim
    #                 rewrites it onto the staged tree.
    #   RUST_MODFILE  the `module!` proc macro reads it to name the
    #                 module; every driver written in Rust expands it.
    #   RUSTC_BOOTSTRAP  Makefile:627 exports it so a stable rustc
    #                 accepts the -Z flags kbuild relies on. Without it
    #                 the derivation's rustc rejects them outright:
    #                 "the option `Z` is only accepted on the nightly
    #                 compiler".
    export NIXGG_RUSTC_ENV='["OBJTREE","RUST_MODFILE","RUSTC_BOOTSTRAP"]'
  '';
in
pkgs.linux.override {
  stdenv = splitStdenv {
    stdenv = pkgs.stdenv;
    splitAtBuild = true;
    inherit sharedStaging;

    # Subtrees whose build reads object BYTES inline, which no derivation
    # can model because the answer is needed before make continues. They
    # run as ordinary passthrough work instead.
    #
    # This list is why the parameter exists: every entry is specific to
    # Linux, most are specific to x86, and all are specific to a kernel
    # version. They used to be compiled into internal/mode.
    #
    #   realmode      cmd_pasyms: `$(NM) $(real-prereqs) | sed > $@`
    #                 produces a generated HEADER, not an artifact.
    #   libstub       cmd_stubcopy pipes objdump to grep and calls
    #                 /bin/false on a match — it decides whether to FAIL
    #                 the build from the text.
    #   vdso          links a shared object with its own linker script;
    #                 shim.LD models only `ld -r`.
    #   purgatory     re-links its own output as purgatory.chk purely to
    #                 verify it, and the check link meets the modelled .ro.
    #   scripts/mod   runs its own tool over empty.o (`mk_elfconfig <
    #                 scripts/mod/empty.o`). Whether that lands in
    #                 configure (harmless) or build (fatal) depends on
    #                 when make schedules prepare0 — observed both ways.
    #   test_fortify  compiles code that MUST NOT compile and greps the
    #                 diagnostic; a derivation would turn the expected
    #                 error into a dead build.
    #
    # Together well under 1% of a kernel's translation units.
    passthroughPaths = [
      "arch/x86/realmode/"
      "drivers/firmware/efi/libstub/"
      "arch/x86/entry/vdso/"
      "arch/x86/purgatory/"
      "scripts/mod/"
      "/test_fortify/"
    ];

    extraBuildAttrs = onlyKernel (
      finalAttrs: old:
      old
      // {
        buildFlags = [ "KBUILD_BUILD_VERSION=1-NixOS" ] ++ dirs;
        makeFlags = (old.makeFlags or [ ]) ++ shimFlags;
        preBuild = (old.preBuild or "") + realToolEnv;
      }
    );

    extraInstallAttrs = onlyKernel (
      finalAttrs: old:
      old
      // {
        # Phase 2 keeps the SAME makeFlags, shim paths included. That is
        # deliberate: kbuild's if_changed compares the command line
        # recorded in each .cmd file against the command it would run
        # now, and phase 1 recorded the shim paths. Dropping them here
        # would make every object look stale and rebuild the kernel.
        #
        # splitStdenv sets NIXGG_BYPASS=1 for the final stage, so the shims exec
        # the real tools: the command text matches while the work is
        # real, which is what the final assembly needs.
        makeFlags = (old.makeFlags or [ ]) ++ shimFlags;

        # Deliberately NOT an installPhase override.
        #
        # Earlier revisions of this file overrode it and copied artifacts
        # by hand. That yields a directory of loose files — fine for
        # inspecting a bzImage, useless as a package. nixpkgs' own
        # install is what makes this a real kernel: `make install` with
        # INSTALL_PATH=$out INSTALL_MOD_PATH=$modules, plus hooks that
        # lay out $modules/lib/modules/<version>/, run depmod, and
        # populate $dev. linuxPackagesFor and the NixOS module system
        # depend on exactly that shape, so ./boot-test.nix cannot boot
        # anything built otherwise.
        #
        # Phase 2's phases are `ggRestorePhase checkPhase installPhase
        # …` with no buildPhase, so the remaining kernel build happens
        # here, before the stock install.
        preInstall =
          realToolEnv
          + ''
            # Build the stock target list, not a subset. bzImage pulls in
            # the final vmlinux link (linker script plus both kallsyms
            # passes) then arch/x86/boot's compress-and-wrap; modules
            # covers the ~7,300 CONFIG=m .ko links, each needing modpost.
            #
            # These are precisely the stages phase 1 could not model:
            # arch/x86/boot, realmode and purgatory all run nm/objdump
            # over their own output and feed the TEXT to the next step,
            # which is why they are carveouts in mode.For. Here they run
            # as ordinary passthrough work against the objects the
            # derivations produced — the point of the two-phase split.
            #
            # scripts_gdb looks droppable and is not: nixpkgs' postInstall
            # copies scripts/gdb/linux/constants.py, which only that
            # target generates, so a shorter list installs a bzImage and
            # a full module tree and then dies on
            #   cp: cannot stat 'scripts/gdb/linux/constants.py'
            make "''${makeFlags[@]}" -j"$NIX_BUILD_CORES" \
              bzImage vmlinux scripts_gdb modules
          ''
          + (old.preInstall or "");
      }
    );
  };
}
