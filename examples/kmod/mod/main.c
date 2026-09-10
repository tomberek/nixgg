// Minimal out-of-tree kernel module, split across two translation
// units on purpose (see helper.c).

#include <linux/init.h>
#include <linux/module.h>
#include <linux/kernel.h>

#include "helper.h"

MODULE_LICENSE("GPL");
MODULE_DESCRIPTION("nixgg out-of-tree kbuild smoke module");
MODULE_VERSION("0");

static int __init hello_mod_init(void)
{
	pr_info("nixgg: hello from an out-of-tree module (%d)\n",
		nixgg_helper_value());
	return 0;
}

static void __exit hello_mod_exit(void)
{
	pr_info("nixgg: goodbye\n");
}

module_init(hello_mod_init);
module_exit(hello_mod_exit);
