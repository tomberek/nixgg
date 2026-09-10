package sandbox

import (
	"strings"
	"testing"
)

// Nix rejects a store name containing ',', and real source trees have
// filenames that use it.
func TestStoreName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"util.h", "util.h"},
		{"vendor,device.h", "vendor_device.h"},
		{"main.c", "main.c"},
		// Every character Nix actually permits must survive untouched,
		// or we would rename files needlessly and lose sharing.
		{"a+b-c.d_e?f=g.h", "a+b-c.d_e?f=g.h"},
		// A leading dot is rejected by Nix.
		{".config", "config"},
		{"..", "unnamed"},
		{"", "unnamed"},
		// Non-ASCII and shell metacharacters.
		// Non-ASCII: range yields runes, so one multi-byte char -> one "_".
		{"naïve.h", "na_ve.h"},
		{"a b;c.h", "a_b_c.h"},
	} {
		if got := StoreName(tc.in); got != tc.want {
			t.Errorf("StoreName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Whatever comes out must satisfy Nix's rules, or the rename accomplished
// nothing. Checked as a property so a future edit to the allowed set
// cannot quietly reintroduce an illegal character.
func TestStoreNameAlwaysValid(t *testing.T) {
	const legal = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+-._?="
	for _, in := range []string{
		"vendor,device.h", "", ".", "..", ".hidden", "a/b.h", "x\x00y.h",
		strings.Repeat("long", 200) + ".h", "日本語.h", "a b;c|d&e.h",
		// Over-length, with a dot landing exactly on the truncation
		// boundary: the name has no leading dot, but the last 200 chars
		// start with one. Trimming before the clamp would let it through.
		"a." + strings.Repeat("x", 199),
	} {
		got := StoreName(in)
		if got == "" {
			t.Errorf("StoreName(%q) produced an empty name", in)
		}
		if strings.HasPrefix(got, ".") {
			t.Errorf("StoreName(%q) = %q starts with a dot", in, got)
		}
		if len(got) > 211 {
			t.Errorf("StoreName(%q) is %d chars, over Nix's 211 limit", in, len(got))
		}
		for _, r := range got {
			if !strings.ContainsRune(legal, r) {
				t.Errorf("StoreName(%q) = %q contains illegal %q", in, got, r)
			}
		}
	}
}
