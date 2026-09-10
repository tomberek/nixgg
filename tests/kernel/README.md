# Kernel fixtures

A full Linux kernel built through nixgg's dynamic derivations, and a
NixOS VM test that boots it.

**These are not run by `tests/drv-equivalence.sh`, `tests/smoke.sh`, or
`nix flake check`, and nothing in `flake.nix` references them.** They are
run by hand.

## Why they are opt-in

Cost, not confidence. A cold `kernel.nix` realises 29,268 derivations.
Phase 1 logs 19,167 compiles, 7,300 objtool steps, 2,065 partial links,
797 archives, 29 objcopy rewrites, 14 links and 3 rustc crates — a few
dozen of those bail to passthrough rather than becoming derivations,
which is why the parts total slightly more than the whole. Measured end
to end: 2h54m on 32 cores at `--max-jobs 128`. `boot-test.nix` then
builds a NixOS closure on top and runs QEMU (1m14s once the kernel is
built); kernel and boot closure together came to roughly 31 GB of
filesystem. The regular test scripts are meant to run in minutes on
every change.

That is also why they live here rather than in `flake.nix`: an attribute
in `packages` would be picked up by `nix flake check` and by anyone
running `nix flake show`.

## Running them

Both need the builder-rpc-capable nix built by this flake — the system
nix will not do — plus the dynamic-derivation experimental features and
`--impure` (the default `nixgg` argument calls
`builtins.getFlake` on a path).

They also need a store that is **not** the daemon store, which rejects
text-hashed derivation outputs, and one on a filesystem with room —
budget well over 100 GB from cold.

Build the kernel:

    ./.patched-nix/bin/nix build --impure --no-link -L \
      --store 'local?root=/path/to/scratch/store' \
      --extra-experimental-features "ca-derivations dynamic-derivations configurable-impure-env" \
      --extra-system-features builder-rpc-v0 \
      --expr '(import ./tests/kernel/kernel.nix {})'

Boot it (same flags):

    ./.patched-nix/bin/nix build --impure --no-link -L \
      --store 'local?root=/path/to/scratch/store' \
      --extra-experimental-features "ca-derivations dynamic-derivations configurable-impure-env" \
      --extra-system-features builder-rpc-v0 \
      --expr '(import ./tests/kernel/boot-test.nix {})'

The boot test additionally needs KVM: `/dev/kvm` accessible, and `kvm`
plus `nixos-test` in the daemon's `system-features`.

## Things that will bite you

- **The daemon store rejects text-hashed outputs.** Without
  `--store 'local?root=…'` the build fails at evaluation with a
  dynamic-derivations error that does not name the store as the cause.

- **Experimental features must be on the command line or in
  `NIX_CONFIG`.** The flake's own `nixConfig` is read too late — the
  feature set is frozen before it applies, so `builtins.outputOf` is
  already missing by then.

- **`fs.mount-max` may need raising, but the default was enough here.**
  Nix bind-mounts an input closure into each sandbox, so a large final
  closure can exhaust it; the symptom is `bind mount … failed: No space
  left on device` while the disk has plenty free. An earlier revision of
  this file said the default (100000) had to be raised for a kernel.
  Full cold builds have since completed at the default — including one
  whose single assembly derivation took every stub as a direct input —
  so treat this as a symptom to recognise rather than a step to perform.

- **`MAX_ARG_STRLEN` is not the constraint it looks like.** It is a
  compile-time kernel constant and cannot be raised, but the assembly
  derivation never puts its build script in `argv`:
  `assemble.Build` sets `passAsFile`, so the script reaches the builder
  as a file. Scripts naming tens of thousands of stubs are fine.

- **Watch liveness by log growth, not by `pgrep`.** A pattern like
  `pgrep -f mybuild` matches the checking command's own argv and will
  cheerfully report a dead build as running.

## What the boot test proves

1. The kernel reaches `multi-user.target` — init ran, userland is up.
2. `uname -r` matches `kernel.modDirVersion` — it is this kernel, not a
   fallback that a misconfigured `boot.kernelPackages` silently
   substituted.
3. `modprobe loop` loads and `losetup -f` works — `modules_install`,
   `depmod` and `modpost` produced a coherent tree, not just files.

Point 3 is the one that earns its cost. The thin-archive bug produced a
kernel that passed every structural check while missing whole subtrees;
"it built" is not evidence.
