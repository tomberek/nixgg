package shim

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tbereknyei/nixgg/internal/mode"
)

// The two links that killed a 31-minute kernel build: Kbuild runs ld
// with cwd inside the object dir, so the carveout has to be tested
// against the resolved path, not the bare `-o` value.
func TestLDCarveoutResolvesRelativeOutput(t *testing.T) {
	// SetPassthroughPaths, not the env var: the list is parsed once per
	// process (mode.ptOnce), so a t.Setenv here is silently ignored
	// whenever another test in this package has already triggered that
	// parse — the test then passes alone and fails in a full run.
	mode.SetPassthroughPaths([]string{
		"arch/x86/realmode/", "drivers/firmware/efi/libstub/",
		"arch/x86/entry/vdso/", "arch/x86/purgatory/",
		"scripts/mod/", "/test_fortify/",
	})
	root := t.TempDir()
	for _, tc := range []struct{ dir, out string }{
		{"linux-6.18.41/arch/x86/realmode/rm", "realmode.elf"},
		{"linux-6.18.41/arch/x86/entry/vdso", "vdso64.so"},
	} {
		d := filepath.Join(root, tc.dir)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		wd, _ := os.Getwd()
		if err := os.Chdir(d); err != nil {
			t.Fatal(err)
		}
		got := ldOutputArg([]string{"-m", "elf_x86_64", "-o", tc.out, "a.o"})
		abs, _ := filepath.Abs(got)
		carved := carvedOut(abs)
		bare := carvedOut(got)
		os.Chdir(wd)
		if got != tc.out {
			t.Errorf("ldOutputArg = %q, want %q", got, tc.out)
		}
		if bare {
			t.Errorf("%s: bare name unexpectedly carved out", tc.out)
		}
		if !carved {
			t.Errorf("%s: absolute path NOT carved out — the stub bug is still live", tc.out)
		}
	}
}
