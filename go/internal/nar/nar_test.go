package nar

import (
	"bytes"
	"os"
	"os/exec"
	"testing"
)

// Compares byte-for-byte against the real nix-store --dump rather than a golden file.
func TestDumpMatchesNixStoreDump(t *testing.T) {
	if _, err := exec.LookPath("nix-store"); err != nil {
		t.Skip("nix-store not on PATH")
	}

	dir := t.TempDir()
	mustWrite(t, dir+"/file.txt", "hello world\n", 0o644)
	mustWrite(t, dir+"/script.sh", "#!/bin/sh\n", 0o755)
	if err := os.Mkdir(dir+"/subdir", 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, dir+"/subdir/nested.txt", "nested\n", 0o644)
	if err := os.Symlink("file.txt", dir+"/link.txt"); err != nil {
		t.Fatal(err)
	}

	want, err := exec.Command("nix-store", "--dump", dir).Output()
	if err != nil {
		t.Fatalf("nix-store --dump: %v", err)
	}

	var got bytes.Buffer
	if err := Dump(&got, dir); err != nil {
		t.Fatalf("Dump: %v", err)
	}

	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("Dump mismatch: got %d bytes, want %d bytes\ngot:  %x\nwant: %x",
			got.Len(), len(want), truncate(got.Bytes()), truncate(want))
	}
}

func mustWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func truncate(b []byte) []byte {
	if len(b) > 200 {
		return b[:200]
	}
	return b
}

func TestDumpMatchesNixStoreDumpEdgeCases(t *testing.T) {
	if _, err := exec.LookPath("nix-store"); err != nil {
		t.Skip("nix-store not on PATH")
	}

	cases := map[string]func(t *testing.T) string{
		"empty directory": func(t *testing.T) string {
			return t.TempDir()
		},
		"directory with empty file": func(t *testing.T) string {
			dir := t.TempDir()
			mustWrite(t, dir+"/empty.txt", "", 0o644)
			return dir
		},
		"single regular file as root": func(t *testing.T) string {
			dir := t.TempDir()
			p := dir + "/solo.txt"
			mustWrite(t, p, "just one file\n", 0o644)
			return p
		},
	}

	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			root := setup(t)

			want, err := exec.Command("nix-store", "--dump", root).Output()
			if err != nil {
				t.Fatalf("nix-store --dump: %v", err)
			}

			var got bytes.Buffer
			if err := Dump(&got, root); err != nil {
				t.Fatalf("Dump: %v", err)
			}

			if !bytes.Equal(got.Bytes(), want) {
				t.Fatalf("Dump mismatch: got %d bytes, want %d bytes\ngot:  %x\nwant: %x",
					got.Len(), len(want), truncate(got.Bytes()), truncate(want))
			}
		})
	}
}
