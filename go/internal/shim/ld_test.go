package shim

import (
	"reflect"
	"testing"
)

// parseLDArgs must recognise both partial-link shapes and
// nothing else. Full links belong to link.go, which sees the compiler
// driver's command line rather than ld's.
func TestParseLDArgs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		wantFlags  []string
		wantOut    string
		wantInputs []string
		wantOK     bool
	}{
		{
			// cmd_ld_multi_m, after ExpandRspfiles has flattened the
			// `@…mod` response file into the member list.
			name: "multi-object module",
			args: []string{"-m", "elf_x86_64", "-r", "-o", "raid6_pq.o",
				"algos.o", "recov.o"},
			wantFlags:  []string{"-m", "elf_x86_64", "-r"},
			wantOut:    "raid6_pq.o",
			wantInputs: []string{"algos.o", "recov.o"},
			wantOK:     true,
		},
		{
			// cmd_ld_single: one input, temp output, make mv's it back.
			name:       "single-object module",
			args:       []string{"-r", "-o", ".tmp_crc4.o", "crc4.o"},
			wantFlags:  []string{"-r"},
			wantOut:    ".tmp_crc4.o",
			wantInputs: []string{"crc4.o"},
			wantOK:     true,
		},
		{
			name:   "full link is not ours",
			args:   []string{"-o", "prog", "head.o", "main.o"},
			wantOK: false,
		},
		{
			name:   "-r without -o bails",
			args:   []string{"-r", "a.o", "b.o"},
			wantOK: false,
		},
		{
			name:   "-r with no inputs bails",
			args:   []string{"-r", "-o", "out.o"},
			wantOK: false,
		},
		{
			// A linker script or other positional we cannot model:
			// dropping it silently would produce a wrong object.
			name:   "unmodellable positional bails",
			args:   []string{"-r", "-o", "out.o", "a.o", "link.lds"},
			wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flags, out, inputs, ok := parseLDArgs(tc.args)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if out != tc.wantOut {
				t.Errorf("output = %q, want %q", out, tc.wantOut)
			}
			if !reflect.DeepEqual(flags, tc.wantFlags) {
				t.Errorf("flags = %q, want %q", flags, tc.wantFlags)
			}
			if !reflect.DeepEqual(inputs, tc.wantInputs) {
				t.Errorf("inputs = %q, want %q", inputs, tc.wantInputs)
			}
		})
	}
}
