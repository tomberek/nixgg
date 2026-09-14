package expr

import (
	"strings"
	"testing"
)

// TestCAOutputPlaceholder pins the placeholder algorithm against a
// vector captured from patched Nix (NixOS/nix#15793) via
// builtins.outputOf.
func TestCAOutputPlaceholder(t *testing.T) {
	for _, tc := range []struct {
		name, drv, output, want string
	}{
		{
			name:   "leaf out",
			drv:    "/nix/store/p4hkhkx55dhqcxslgi6qgiasl2974n76-leaf.drv",
			output: "out",
			want:   "/0jdl66mqxficvnh6dw0z1aplacg14qdgsh8ngxrk1x09p2c2rhk4",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := caOutputPlaceholder(tc.drv, tc.output)
			if len(got) != 53 || got[0] != '/' {
				t.Fatalf("placeholder shape wrong: %q", got)
			}
			for _, c := range got[1:] {
				if !isNix32Char(byte(c)) {
					t.Fatalf("placeholder contains non-nix32 char %q in %q", c, got)
				}
			}
			if got != tc.want {
				t.Errorf("placeholder mismatch:\n  want %q\n   got %q", tc.want, got)
			}
		})
	}
}

func isNix32Char(b byte) bool {
	for i := 0; i < len(nix32Chars); i++ {
		if nix32Chars[i] == b {
			return true
		}
	}
	return false
}

// TestNix32Encode: a 32-byte input (sha256 of empty string) encodes
// to exactly 52 chars.
func TestNix32Encode(t *testing.T) {
	var digest [32]byte
	for i, b := range [...]byte{
		0xe3, 0xb0, 0xc4, 0x42, 0x98, 0xfc, 0x1c, 0x14,
		0x9a, 0xfb, 0xf4, 0xc8, 0x99, 0x6f, 0xb9, 0x24,
		0x27, 0xae, 0x41, 0xe4, 0x64, 0x9b, 0x93, 0x4c,
		0xa4, 0x95, 0x99, 0x1b, 0x78, 0x52, 0xb8, 0x55,
	} {
		digest[i] = b
	}
	got := nix32Encode(digest[:])
	if len(got) != 52 {
		t.Fatalf("expected 52 chars, got %d: %q", len(got), got)
	}
	for _, c := range got {
		if !isNix32Char(byte(c)) {
			t.Fatalf("non-nix32 char %q in %q", c, got)
		}
	}
}

// TestLinkScriptEmitsLibFlagsAfterInputs pins that -l flags land after
// inputs: a single-pass linker only resolves a library against objects
// already seen, so -lm/-latomic ahead of libavutil.a broke ffmpeg with
// "undefined reference to `sqrt'".
//
// Also pins the len(lflags)==0 fallback, which keeps the drv byte-
// identical to the pre-split layout for hello/lua/mosh (no -l flags,
// 78 pinned hashes).
func TestLinkScriptEmitsLibFlagsAfterInputs(t *testing.T) {
	base := func(flags []string) *Derivation {
		return &Derivation{
			Kind:      KindLink,
			Tool:      "cc",
			OutName:   "prog",
			Coreutils: "/COREUTILS",
			Compiler:  "/GCC",
			Flags:     flags,
			Inputs: []derivInput{
				{InputKind: "store", Ref: "/nix/store/" + strings.Repeat("a", 32) + "-tu-main.o", Name: "main.o"},
				{InputKind: "store", Ref: "/nix/store/" + strings.Repeat("b", 32) + "-ar-libx.a", Name: "libx.a"},
			},
		}
	}

	t.Run("-l comes after inputs", func(t *testing.T) {
		s := base([]string{"-O2", "-lm", "-Wl,-E", "-ldl"}).script()

		iLast := strings.LastIndex(s, "libx.a'")
		for _, lf := range []string{"'-lm'", "'-ldl'"} {
			at := strings.Index(s, lf)
			if at < 0 {
				t.Fatalf("%s missing from script:\n%s", lf, s)
			}
			if at < iLast {
				t.Errorf("%s appears BEFORE the last input — single-pass ld will "+
					"not resolve symbols that inputs reference from it\nscript:\n%s", lf, s)
			}
		}
		if o := strings.Index(s, "'-O2'"); o > iLast {
			t.Errorf("-O2 moved after inputs; only -l flags should be relocated\n%s", s)
		}
		if w := strings.Index(s, "'-Wl,-E'"); w > iLast {
			t.Errorf("-Wl,-E moved after inputs; it is not a -l flag\n%s", s)
		}
	})

	t.Run("no -l flags keeps the historical layout", func(t *testing.T) {
		s := base([]string{"-O2", "-Wl,-E"}).script()
		if strings.Contains(s, "  ") {
			t.Errorf("double space in script — the -l split must fall back to the "+
				"pre-split layout when there are no -l flags, or the 78 pinned "+
				"hello/lua/mosh drv hashes change:\n%q", s)
		}
	})

	t.Run("bare -l is not treated as a lib flag", func(t *testing.T) {
		s := base([]string{"-l"}).script()
		iLast := strings.LastIndex(s, "libx.a'")
		if at := strings.Index(s, "'-l'"); at > iLast {
			t.Errorf("bare -l was relocated as if it named a library:\n%s", s)
		}
	})
}

// TestAbsFileScriptRecreatesGeneratedFileBeforeLinking pins that
// AbsFilePath/AbsFileContent recreate the referenced file at its exact
// absolute path BEFORE the link command runs — needed for QEMU's
// `-Xlinker --dynamic-list=...qemu-plugin.symbols`.
func TestAbsFileScriptRecreatesGeneratedFileBeforeLinking(t *testing.T) {
	d := &Derivation{
		Kind:           KindLink,
		Tool:           "cc",
		OutName:        "qemu-system-x86_64",
		Coreutils:      "/COREUTILS",
		Compiler:       "/GCC",
		AbsFilePath:    "/build/source/build/plugins/qemu-plugin.symbols",
		AbsFileContent: "{\n  qemu_plugin_foo;\n};\n",
		Inputs: []derivInput{
			{InputKind: "store", Ref: "/nix/store/" + strings.Repeat("a", 32) + "-tu-main.o", Name: "main.o"},
		},
	}
	s := d.script()

	wantDir := "mkdir -p '/build/source/build/plugins'"
	if !strings.Contains(s, wantDir) {
		t.Errorf("script missing %q:\n%s", wantDir, s)
	}
	wantHeredoc := "cat > '/build/source/build/plugins/qemu-plugin.symbols' <<'NIXGG_ABS_FILE_EOF'\n{\n  qemu_plugin_foo;\n};\nNIXGG_ABS_FILE_EOF"
	if !strings.Contains(s, wantHeredoc) {
		t.Errorf("script missing heredoc write:\n%s\n---got---\n%s", wantHeredoc, s)
	}
	if at, atCC := strings.Index(s, wantDir), strings.Index(s, `"cc"`); at < 0 || atCC < 0 || at > atCC {
		t.Errorf("mkdir+write must appear BEFORE the link command; got mkdir@%d cc@%d\n%s", at, atCC, s)
	}
}

// TestAbsFileScriptEmptyIsANoOp pins that a derivation with no
// AbsFilePath renders exactly as before this field existed.
func TestAbsFileScriptEmptyIsANoOp(t *testing.T) {
	d := &Derivation{
		Kind:      KindLink,
		Tool:      "cc",
		OutName:   "prog",
		Coreutils: "/COREUTILS",
		Compiler:  "/GCC",
		Inputs: []derivInput{
			{InputKind: "store", Ref: "/nix/store/" + strings.Repeat("a", 32) + "-tu-main.o", Name: "main.o"},
		},
	}
	if s := d.script(); strings.Contains(s, "NIXGG_ABS_FILE_EOF") {
		t.Errorf("empty AbsFilePath must not emit any heredoc:\n%s", s)
	}
}
