// The second translation unit. Its only job is to make hello_mod a
// MULTI-object module, which is load-bearing — see default.nix: with
// CONFIG_X86_KERNEL_IBT (hence delay-objtool), kbuild runs objtool on
// the per-TU object only for single-object modules. Two TUs push
// objtool to the multi-obj link step, which is the phase boundary this
// example splits on.

#include <linux/kernel.h>
#include "helper.h"

int nixgg_helper_value(void)
{
	return 42;
}
