package shim

import (
	"reflect"
	"testing"
)

// The modelled shape is `objcopy <flags> <in> <out>`. Only
// that two-operand rewrite is modelled; the in-place and read-only
// forms must bail so the real tool runs.
func TestParseObjcopyArgs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantFlags []string
		wantIn    string
		wantOut   string
		wantOK    bool
	}{
		{
			// symbol prefixing with a long flag taking a value
			// Real STUBCOPY_FLAGS-y use the ATTACHED form.
			name: "symbol prefixing",
			args: []string{"--remove-section=.note.gnu.property",
				"--prefix-alloc-sections=.init", "alignedmem.o", "alignedmem.stub.o"},
			wantFlags: []string{"--remove-section=.note.gnu.property",
				"--prefix-alloc-sections=.init"},
			wantIn: "alignedmem.o", wantOut: "alignedmem.stub.o", wantOK: true,
		},
		{
			name:      "output format override",
			args:      []string{"-O", "elf32-x86-64", "vdso.so.dbg", "vdso32.so"},
			wantFlags: []string{"-O", "elf32-x86-64"},
			wantIn:    "vdso.so.dbg", wantOut: "vdso32.so", wantOK: true,
		},
		{
			// In-place: one operand. Not modelled.
			name: "single operand bails", args: []string{"-R", ".comment", "foo.o"},
			wantOK: false,
		},
		{
			name: "no operands bails", args: []string{"--help"}, wantOK: false,
		},
		{
			// Three operands is not a shape we understand.
			name: "three operands bails", args: []string{"a.o", "b.o", "c.o"},
			wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flags, in, out, ok := parseObjcopyArgs(tc.args)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if in != tc.wantIn || out != tc.wantOut {
				t.Errorf("in/out = %q/%q, want %q/%q", in, out, tc.wantIn, tc.wantOut)
			}
			if !reflect.DeepEqual(flags, tc.wantFlags) {
				t.Errorf("flags = %q, want %q", flags, tc.wantFlags)
			}
		})
	}
}

// A long flag whose value is attached with `=` —
// the attached form, which needs no two-arg handling. This is the exact
// invocation that first failed with "gdt_idt.o: file format not
// recognized".
func TestParseObjcopyArgsAttachedFlags(t *testing.T) {
	flags, in, out, ok := parseObjcopyArgs(
		[]string{"--prefix-symbols=__pi_", "gdt_idt.o", "gdt_idt.pi.o"})
	if !ok {
		t.Fatal("attached-form flags rejected")
	}
	if in != "gdt_idt.o" || out != "gdt_idt.pi.o" {
		t.Errorf("in/out = %q/%q", in, out)
	}
	if len(flags) != 1 || flags[0] != "--prefix-symbols=__pi_" {
		t.Errorf("flags = %q", flags)
	}
}
