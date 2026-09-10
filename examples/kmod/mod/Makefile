# Out-of-tree module, built in two passes so that the steps which READ
# object bytes happen after Nix has realised them. See ../default.nix.
#
# KDIR comes from the caller (default.nix points it at
# ${kernel.dev}/lib/modules/<ver>/build).

obj-m += hello_mod.o
hello_mod-y := main.o helper.o

KDIR ?= $(error KDIR must point at a prepared kernel build tree)
PWD  := $(shell pwd)

# Phase 1: compile the parts and nothing else. These are kbuild
# "single targets" (top Makefile's `single-targets := … %.o …`), which
# sets single-build=1 and so skips the modpost goal entirely. Because
# hello_mod is a multi-object module, objtool is not run on these
# either — it is deferred to the multi-obj link in phase 2.
objects:
	$(MAKE) -C $(KDIR) M=$(PWD) main.o helper.o

# Phase 2: everything that needs to read real object bytes — the
# multi-obj `ld -r`, objtool, modpost, and modfinal.
modules:
	$(MAKE) -C $(KDIR) M=$(PWD) modules

clean:
	$(MAKE) -C $(KDIR) M=$(PWD) clean

.PHONY: objects modules clean
