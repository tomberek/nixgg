package mode

import "testing"

// Each entry exists because a real project tripped it; narrowing these silently breaks that project's configure step.
func TestFor(t *testing.T) {
	for _, tc := range []struct {
		path string
		want Mode
	}{
		{"conftest.c", Realise},
		{"conftest", Realise},
		{"conftest.cpp", Realise},
		{"/tmp/build/conftest.c", Realise},

		{"testCCompiler.c", Realise},
		{"testCXXCompilerABI_C.cpp", Realise},
		{"CMakeCCompilerId.c", Realise},
		{"CMakeCXXCompilerABI_CXX.cpp", Realise},

		{"CheckFunctionExists.c", Realise},
		{"CheckIncludeFile.c", Realise},
		{"CheckCSourceCompiles.c", Realise},
		{"CheckCSourceRuns.c", Realise},
		{"CheckSymbolExists.c", Realise},
		{"CheckTypeSize.c", Realise},

		{"/b/CMakeFiles/CMakeScratch/x/src.c", Realise},
		{"/b/CMakeFiles/CMakeTmp/src.c", Realise},

		// Handled by a plain Passthrough in compile.go's isKbuildElfProbe instead: mode.Realise is incompatible with sandbox mode.
		{"scripts/mod/empty.c", Placeholder},
		{"scripts/mod/empty.o", Placeholder},
		{"/build/linux-6.12/scripts/mod/empty.c", Placeholder},

		// Same sandbox-mode reason, handled by compile.go's isKbuildRealmodeObj.
		{"arch/x86/realmode/rm/header.S", Placeholder},
		{"arch/x86/realmode/rm/header.o", Placeholder},
		{"arch/x86/realmode/rm/trampoline_32.S", Placeholder},
		{"arch/x86/realmode/rm/trampoline_64.o", Placeholder},
		{"arch/x86/realmode/rm/stack.o", Placeholder},
		{"arch/x86/realmode/rm/reboot.o", Placeholder},
		{"/build/linux-6.12/arch/x86/realmode/rm/reboot.o", Placeholder},

		{"main.c", Placeholder},
		{"src/util.cpp", Placeholder},
		{"parseutils.c", Placeholder},

		{"llvm-tblgen", Placeholder},
		{"llvm-min-tblgen", Placeholder},
		{"protoc", Placeholder},

		{"testing.c", Placeholder},
		{"Checkers.c", Placeholder},
		{"CMakeLists.txt", Placeholder},
		{"my-conftest-helper.c", Placeholder},
		{"empty.c", Placeholder},
		{"scripts/mod/modpost.c", Placeholder},
		{"arch/x86/realmode/rm/realmode.lds.S", Placeholder},
		{"arch/x86/kernel/head_32.S", Placeholder},
	} {
		t.Run(tc.path, func(t *testing.T) {
			if got := For(tc.path); got != tc.want {
				t.Errorf("For(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestForLink(t *testing.T) {
	for _, tc := range []struct {
		path string
		want Mode
	}{
		{"arch/x86/realmode/rm/realmode.elf", Realise},
		{"/build/linux-6.12/arch/x86/realmode/rm/realmode.elf", Realise},
		{"arch/x86/entry/vdso/vdso32.so.dbg", Realise},
		{"arch/x86/entry/vdso/vdsox32.so.dbg", Realise},
		{"/build/linux-6.12/arch/x86/entry/vdso/vdso32.so.dbg", Realise},
		{"scripts/mod/modpost", Realise},
		{"/build/linux-6.12/scripts/mod/modpost", Realise},
		{"vmlinux.o", Realise},
		{"/build/linux-6.12/vmlinux.o", Realise},
		{"vmlinux", Realise},
		{"/build/linux-6.12/vmlinux", Realise},

		// Near-misses and ordinary links must NOT realise.
		{"realmode.elf", Placeholder},
		{"arch/x86/realmode/rm/realmode.bin", Placeholder},
		{"arch/x86/entry/vdso/vdso32.so", Placeholder},
		{"scripts/mod/modpost.o", Placeholder},
		{"foo-vmlinux.o", Placeholder},
		{"foo-vmlinux", Placeholder},
		{"a.out", Placeholder},
		{"conftest", Placeholder},
	} {
		t.Run(tc.path, func(t *testing.T) {
			if got := ForLink(tc.path); got != tc.want {
				t.Errorf("ForLink(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}
