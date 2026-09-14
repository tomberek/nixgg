package mode

import (
	"reflect"
	"testing"
)

func TestParsePassthrough(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     []string
	}{
		{"two subtrees", `["src/embed/","tools/gen/"]`,
			[]string{"src/embed/", "tools/gen/"}},
		{"empty array", `[]`, nil},
		{"unset", "", nil},
		{"whitespace", "   ", nil},
		// Unparseable must not be fatal: a malformed value should cost
		// acceleration in one subtree, not fail every compile in the
		// build with a JSON error from inside a shim.
		{"malformed json", `["unterminated`, nil},
		{"not an array", `"src/embed/"`, nil},
		// THE dangerous case. An empty string substring-matches every
		// path, so a stray "" would pass the ENTIRE build through — and
		// that looks like nixgg silently accelerating nothing rather
		// than a configuration error, far worse than a hard failure.
		{"empty element is dropped", `["", "tools/gen/"]`, []string{"tools/gen/"}},
		{"all empty elements", `["", "  "]`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePassthrough(tc.in)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parsePassthrough(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A project that configures nothing must get no carveouts — that's
// every example build, and it's what keeps this change invisible to
// them.
func TestNoConfiguredPathsMeansNoCarveouts(t *testing.T) {
	SetPassthroughPaths(nil)
	defer SetPassthroughPaths([]string{
		"src/embed/", "tools/gen/",
	})

	for _, p := range []string{
		"src/embed/blob.S",
		"tools/gen/empty.c",
		"src/main.c",
	} {
		if got := For(p); got != Placeholder {
			t.Errorf("For(%q) = %v with no configuration, want Placeholder", p, got)
		}
	}
}

// Build-system conventions stay compiled in: they're properties of
// autoconf and cmake, identical in every project that uses them, so a
// caller must not have to declare them.
func TestConventionRulesSurviveWithoutConfiguration(t *testing.T) {
	SetPassthroughPaths(nil)
	defer SetPassthroughPaths([]string{"tools/gen/"})

	for _, tc := range []struct {
		path string
		want Mode
	}{
		{"conftest.c", Realise},
		{"/tmp/build/conftest.cpp", Realise},
		{"testCCompiler.c", Realise},
		{"/x/CMakeFiles/CMakeScratch/TryCompile-ab12/src.cxx", Realise},
	} {
		if got := For(tc.path); got != tc.want {
			t.Errorf("For(%q) = %v, want %v — convention rules must not "+
				"depend on caller configuration", tc.path, got, tc.want)
		}
	}
}

// A declared subtree is about where the OBJECT lands, not where its
// source lives: a build may compile a source from elsewhere into it.
// Keying on the source alone lets those objects through as stubs, and
// whatever the subtree does with their bytes then fails.
func TestPassthroughKeysOnOutputNotJustSource(t *testing.T) {
	SetPassthroughPaths([]string{"src/embed/"})
	defer SetPassthroughPaths([]string{"tools/gen/"})

	if got := For("../lib/cmdline.c"); got != Placeholder {
		t.Errorf("For(lib/cmdline.c) = %v, want Placeholder", got)
	}
	if got := For("src/embed/lib-cmdline.o"); got != Passthrough {
		t.Errorf("For(src/embed/lib-cmdline.o) = %v, want Passthrough", got)
	}
}
