package shim

import (
	"reflect"
	"testing"
)

// parseObjtoolArgs has to hold up against a real invocation,
// which is one object preceded by a long, config-determined flag list
// (scripts/Makefile.lib's objtool-args). None of objtool's options take
// a separate value token — they are bare or `--opt=value` — so the sole
// non-flag operand is the object.
func TestParseObjtoolArgs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantFlags []string
		wantObj   string
		wantOK    bool
	}{
		{
			name: "full real-world invocation",
			args: []string{
				"--link", "--module", "--ibt", "--orc", "--stackval",
				"--retpoline", "--rethunk", "--sls", "--static-call",
				"--uaccess", "--hacks=jump_label,noinstr", "lib/crc/crc4.o",
			},
			wantFlags: []string{
				"--link", "--module", "--ibt", "--orc", "--stackval",
				"--retpoline", "--rethunk", "--sls", "--static-call",
				"--uaccess", "--hacks=jump_label,noinstr",
			},
			wantObj: "lib/crc/crc4.o",
			wantOK:  true,
		},
		{
			name: "no flags at all", args: []string{"foo.o"},
			wantFlags: nil, wantObj: "foo.o", wantOK: true,
		},
		{
			name: "no operand bails", args: []string{"--orc"},
			wantOK: false,
		},
		{
			// Two operands is a shape we do not model; rewriting only
			// one of them would silently corrupt the other.
			name: "two operands bail", args: []string{"--orc", "a.o", "b.o"},
			wantOK: false,
		},
		{
			name: "empty argv bails", args: nil, wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flags, obj, ok := parseObjtoolArgs(tc.args)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if obj != tc.wantObj {
				t.Errorf("object = %q, want %q", obj, tc.wantObj)
			}
			if !reflect.DeepEqual(flags, tc.wantFlags) {
				t.Errorf("flags = %q, want %q", flags, tc.wantFlags)
			}
		})
	}
}
