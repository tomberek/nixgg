package wrapperenv

import (
	"os"
	"strings"
	"testing"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, b := range bases {
		t.Setenv(b, "")
		t.Setenv(b+"_x86_64_unknown_linux_gnu", "")
	}
	// t.Setenv("", "") still leaves the key present; detectTriple scans os.Environ() by prefix, so it must be Unset too.
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "NIX_CC_WRAPPER_TARGET_HOST_") {
			eq := strings.IndexByte(kv, '=')
			if eq > 0 {
				k := kv[:eq]
				t.Setenv(k, "")
				if err := os.Unsetenv(k); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

// wrapperEnvJSON is embedded verbatim in every compile/link/archive drv hash, and native mode
// (mkShellNoCC) sets no wrapper vars — a non-empty object here would diverge every hello/lua/mosh
// drv between modes. An all-whitespace NIX_LDFLAGS surviving as " " shipped once and was fixed in fe671b4.
func TestJSONEmptyEnvIsEmptyObject(t *testing.T) {
	clearEnv(t)
	got, err := JSON()
	if err != nil {
		t.Fatal(err)
	}
	if got != "{}" {
		t.Errorf("JSON() = %q, want %q — a non-empty object here diverges from "+
			"native mode, which sets no wrapper vars", got, "{}")
	}
}

// mkNixggBuild's preBuild scrubs -frandom-seed/-rpath from NIX_CFLAGS_COMPILE/NIX_LDFLAGS with sed;
// when those were the only contents, the result was a lone space, which JSON() must still treat as absent.
func TestJSONWhitespaceOnlyIsTreatedAsAbsent(t *testing.T) {
	for _, val := range []string{" ", "  ", "\t", " \n "} {
		t.Run(strings.ReplaceAll(val, "\n", "\\n"), func(t *testing.T) {
			clearEnv(t)
			t.Setenv("NIX_LDFLAGS", val)
			got, err := JSON()
			if err != nil {
				t.Fatal(err)
			}
			if got != "{}" {
				t.Errorf("whitespace-only NIX_LDFLAGS=%q leaked: JSON() = %q, want %q",
					val, got, "{}")
			}
		})
	}
}

// Emitting the trigger unconditionally would add a key to every drv's env, including
// empty-buildInputs builds where native mode emits nothing, diverging all 78 pinned hashes.
func TestJSONTriggerOnlyWithRealFlags(t *testing.T) {
	const triple = "x86_64_unknown_linux_gnu"
	const trigger = "NIX_CC_WRAPPER_TARGET_HOST_" + triple

	t.Run("trigger set but no flags -> empty", func(t *testing.T) {
		clearEnv(t)
		t.Setenv(trigger, "1")
		got, err := JSON()
		if err != nil {
			t.Fatal(err)
		}
		if got != "{}" {
			t.Errorf("trigger emitted with no flags to activate on: %q", got)
		}
	})

	t.Run("trigger plus a real flag -> both", func(t *testing.T) {
		clearEnv(t)
		t.Setenv(trigger, "1")
		t.Setenv("NIX_CFLAGS_COMPILE", "-isystem /nix/store/x/include")
		got, err := JSON()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, trigger) {
			t.Errorf("trigger missing with a real flag present — the inner "+
				"cc-wrapper will not inject buildInputs' paths: %q", got)
		}
		if !strings.Contains(got, "NIX_CFLAGS_COMPILE") {
			t.Errorf("flag missing: %q", got)
		}
	})

	t.Run("flag without trigger -> flag only", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("NIX_CFLAGS_COMPILE", "-isystem /nix/store/x/include")
		got, err := JSON()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(got, "NIX_CC_WRAPPER_TARGET_HOST_") {
			t.Errorf("trigger invented with no triple in env: %q", got)
		}
	})
}

// The output string is a drv hash input, so map iteration order must never reach it.
func TestJSONKeysAreSorted(t *testing.T) {
	clearEnv(t)
	t.Setenv("NIX_CFLAGS_COMPILE", "-Ione")
	t.Setenv("NIX_LDFLAGS", "-Ltwo")
	t.Setenv("NIX_HARDENING_ENABLE", "pic")

	first, err := JSON()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		got, err := JSON()
		if err != nil {
			t.Fatal(err)
		}
		if got != first {
			t.Fatalf("JSON() is not deterministic across calls:\n first: %q\n  then: %q", first, got)
		}
	}

	var keys []string
	for _, part := range strings.Split(strings.Trim(first, "{}"), ",") {
		if q := strings.Index(part, "\":"); q > 0 {
			keys = append(keys, strings.Trim(part[:q+1], "\""))
		}
	}
	for i := 1; i < len(keys); i++ {
		if keys[i-1] > keys[i] {
			t.Errorf("keys not sorted: %q (from %q)", keys, first)
			break
		}
	}
}
