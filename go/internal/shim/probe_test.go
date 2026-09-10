package shim

import (
	"os"
	"path/filepath"
	"testing"
)

// Build systems ask the compiler questions by compiling /dev/null:
//
//	$(CC) -Werror $(FLAGS) <option> -c -x c /dev/null -o "$$TMP"
//
// The caller keys on the exit status alone, so any error the shim
// raises reads as "the compiler does not support this option" and the
// build carries on with a degraded flag set. A non-regular source must
// never be modelled; pin isRegularFile on the shapes that reach it.
func TestIsRegularFile(t *testing.T) {
	dir := t.TempDir()

	real := filepath.Join(dir, "real.c")
	if err := os.WriteFile(real, []byte("int x;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.c")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling.c")
	if err := os.Symlink(filepath.Join(dir, "nope.c"), dangling); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{"ordinary source", real, true},
		// Shared staging symlinks staged sources into the store, so a
		// symlink to a regular file has to keep counting as one.
		{"symlink to a regular file", link, true},
		// A character device has no NAR representation, so it cannot be
		// added to the store at all.
		{"/dev/null", "/dev/null", false},
		{"directory", dir, false},
		{"missing file", filepath.Join(dir, "absent.c"), false},
		{"dangling symlink", dangling, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRegularFile(tc.path); got != tc.want {
				t.Errorf("isRegularFile(%s) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// .incbin names a file in a string the preprocessor never reads, so
// `gcc -M` cannot report it and the TU dies in the ASSEMBLER instead.
//
// Detection is a substring search by design — the directive appears in C
// string literals, in .S files, and behind #ifdefs, so anything short of
// running the preprocessor is an approximation. The costs are asymmetric:
// a false positive is one un-accelerated compile, a false negative is a
// build failure far from its cause.
func TestUsesIncbin(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Inline asm inside a C source, the common shape.
	configsC := write("configs.c", `
#include <linux/init.h>
asm (
"	.section \".rodata\", \"a\"		\n"
"	.global config_data			\n"
"config_data:					\n"
"	.incbin \"build/embedded_data.bin\"	\n"
);
`)
	// A bare .S user.
	initramfsS := write("initramfs_data.S", "	.incbin \"build/payload.bin\"\n")
	plain := write("plain.c", "int main(void){return 0;}\n")

	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{"inline asm in C", configsC, true},
		{"bare .S", initramfsS, true},
		{"ordinary source", plain, false},
		{"missing file is not a blocker", filepath.Join(dir, "absent.c"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := usesIncbin(tc.path); got != tc.want {
				t.Errorf("usesIncbin(%s) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}
