# Does the kernel nixgg built actually BOOT?
#
# NOT wired into tests/drv-equivalence.sh, tests/smoke.sh, or the flake's
# checks. It depends on ./kernel.nix (hours from cold) and then builds a
# NixOS closure and runs QEMU. See ./README.md.
#
#
# WHY A BOOT TEST AND NOT MORE STRUCTURAL CHECKS
#
# Structural checks — correct bzImage headers, a complete symbol table,
# the expected module count — rule out obvious corruption and nothing
# else. The failure modes this design actually produces are subtler: a
# dropped object, a mis-ordered archive member, a stale .cmd file. Each
# leaves an image that passes every structural check and dies at boot.
#
# That is not hypothetical. The thin-archive bug (see internal/shim/
# thinar.go) silently dropped three whole subtrees while producing a
# vmlinux.o of plausible size with a valid ELF header. Only the final
# link caught it, and only because 49 symbols happened to be referenced.
# A quieter version of the same bug would have produced a kernel that
# built, installed, and panicked.
#
# So: boot it, and make it prove three things.
{
  nixgg ? builtins.getFlake (toString ../..),
  system ? builtins.currentSystem,
  kernel ? import ./kernel.nix { inherit nixgg system; },
}:

let
  pkgs = nixgg.inputs.nixpkgs.legacyPackages.${system};

  # linuxPackagesFor wants a real kernel derivation — modDirVersion,
  # passthru, the $dev and $modules outputs. That is why kernel.nix
  # leaves nixpkgs' installPhase alone.
  kernelPackages = pkgs.linuxPackagesFor kernel;
in
pkgs.testers.runNixOSTest {
  name = "nixgg-kernel-boots";

  nodes.machine =
    { ... }:
    {
      boot.kernelPackages = kernelPackages;

      # Keep the VM minimal so a kernel problem surfaces as a kernel
      # problem, rather than as some unrelated unit failing to start.
      virtualisation.memorySize = 2048;
      virtualisation.graphics = false;
    };

  testScript = ''
    machine.start()

    # 1. Userland came up at all: init ran and systemd reached its
    #    default target.
    machine.wait_for_unit("multi-user.target")

    # 2. It is OUR kernel. Compared against the version the derivation
    #    declares rather than a literal, so this cannot pass by matching
    #    a stale string — or by silently booting a fallback kernel, which
    #    is exactly what a misconfigured boot.kernelPackages would do.
    want = "${kernel.modDirVersion}"
    got = machine.succeed("uname -r").strip()
    assert got == want, f"running kernel is {got!r}, expected {want!r}"

    # 3. The module tree is coherent. Loading a module exercises
    #    modules_install, depmod's modules.dep and modpost's symbol
    #    versioning together, and losetup proves the module actually
    #    works rather than merely loading. This is the check that would
    #    have caught the thin-archive bug: "it built" is not evidence.
    machine.succeed("modprobe loop")
    machine.succeed("lsmod | grep -q '^loop'")
    machine.succeed("losetup -f")

    print(machine.succeed("uname -a"))
  '';
}
