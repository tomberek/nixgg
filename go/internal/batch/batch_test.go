package batch

import "testing"

func TestClassifyRedisDeps(t *testing.T) {
	cfg := Config{Groups: []Group{
		{Name: "vendor", Patterns: []string{"deps/**/*.c"}},
	}}

	tests := []struct {
		path      string
		wantGroup string
		wantOK    bool
	}{
		{"/build/source/deps/hiredis/hiredis.c", "vendor", true},
		{"/build/source/deps/jemalloc/src/jemalloc.c", "vendor", true},
		{"/build/source/deps/lua/src/lapi.c", "vendor", true},
		{"/build/source/src/t_string.c", "", false},
		{"/build/source/src/networking.c", "", false},
	}
	for _, tc := range tests {
		group, ok := cfg.Classify(tc.path)
		if ok != tc.wantOK || group != tc.wantGroup {
			t.Errorf("Classify(%q) = (%q, %v), want (%q, %v)",
				tc.path, group, ok, tc.wantGroup, tc.wantOK)
		}
	}
}

// Regression: compile.go used to pass Classify a path relative to internal/scan's ProjectRoot,
// which collapses to the TU's own dir when make recurses with no outside -I refs, so deps/**/*.c
// could never match (1/156 real TUs matched in a live redis build). Fix: pass the TU's absolute
// path and match unanchored. This pins that a truncated, root-collapsed path correctly does not match.
func TestClassifyUnanchoredAcrossVaryingRoots(t *testing.T) {
	cfg := Config{Groups: []Group{
		{Name: "vendor", Patterns: []string{"deps/**/*.c"}},
	}}

	full := "/build/source/deps/hiredis/sds.c"
	if g, ok := cfg.Classify(full); !ok || g != "vendor" {
		t.Fatalf("Classify(%q) = (%q, %v), want (vendor, true)", full, g, ok)
	}

	truncated := "sds.c"
	if _, ok := cfg.Classify(truncated); ok {
		t.Fatalf("Classify(%q) unexpectedly matched — a path with no deps/ segment at all must not match deps/**/*.c", truncated)
	}
}

func TestClassifyFirstMatchWins(t *testing.T) {
	cfg := Config{Groups: []Group{
		{Name: "hot", Patterns: []string{"vendor/hot/*.c"}},
		{Name: "vendor", Patterns: []string{"vendor/**/*.c"}},
	}}

	if g, ok := cfg.Classify("/build/source/vendor/hot/hot.c"); !ok || g != "hot" {
		t.Errorf("Classify(vendor/hot/hot.c) = (%q, %v), want (hot, true)", g, ok)
	}
	if g, ok := cfg.Classify("/build/source/vendor/cold/cold.c"); !ok || g != "vendor" {
		t.Errorf("Classify(vendor/cold/cold.c) = (%q, %v), want (vendor, true)", g, ok)
	}
}

func TestClassifyNoGroups(t *testing.T) {
	var cfg Config
	if g, ok := cfg.Classify("/build/source/src/anything.c"); ok || g != "" {
		t.Errorf("Classify on zero Config = (%q, %v), want (\"\", false)", g, ok)
	}
}

// A bare "*" segment matches one path segment, not arbitrary depth (configureSrcFilterPresets.nix's own convention).
func TestClassifySingleStar(t *testing.T) {
	cfg := Config{Groups: []Group{
		{Name: "one-level", Patterns: []string{"deps/*/*.c"}},
	}}
	if _, ok := cfg.Classify("/build/source/deps/hiredis/hiredis.c"); !ok {
		t.Error("deps/*/*.c should match deps/hiredis/hiredis.c")
	}
	if _, ok := cfg.Classify("/build/source/deps/hiredis/sub/deep.c"); ok {
		t.Error("deps/*/*.c should NOT match deps/hiredis/sub/deep.c (single-level only)")
	}
}

func TestClassifyDeepStar(t *testing.T) {
	cfg := Config{Groups: []Group{
		{Name: "vendor", Patterns: []string{"deps/**/*.c"}},
	}}
	if _, ok := cfg.Classify("/build/source/deps/hiredis.c"); !ok {
		t.Error("deps/**/*.c should match deps/hiredis.c (zero extra segments)")
	}
	if _, ok := cfg.Classify("/build/source/deps/a/b/c/deep.c"); !ok {
		t.Error("deps/**/*.c should match deps/a/b/c/deep.c (arbitrary depth)")
	}
	if _, ok := cfg.Classify("/build/source/other/hiredis.c"); ok {
		t.Error("deps/**/*.c should not match a path with no deps/ segment at all")
	}
}

func TestClassifyUnanchoredDoesNotFalseMatchSimilarNames(t *testing.T) {
	cfg := Config{Groups: []Group{
		{Name: "vendor", Patterns: []string{"deps/**/*.c"}},
	}}
	if _, ok := cfg.Classify("/build/source/mydeps/foo.c"); ok {
		t.Error(`deps/**/*.c should not match a "mydeps" segment`)
	}
	if _, ok := cfg.Classify("/build/source/depsx/foo.c"); ok {
		t.Error(`deps/**/*.c should not match a "depsx" segment`)
	}
}

// $NIXGG_BATCH_GROUPS carries this JSON-array-of-objects shape, mirroring $NIXGG_KNOWN_STORE_PATHS's convention.
func TestFromJSON(t *testing.T) {
	cfg := FromJSON(`[{"name":"vendor","patterns":["deps/**/*.c"]},{"name":"proto","patterns":["*.pb.cc"]}]`)
	if len(cfg.Groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(cfg.Groups))
	}
	if cfg.Groups[0].Name != "vendor" || cfg.Groups[1].Name != "proto" {
		t.Errorf("groups in wrong order or wrong names: %+v", cfg.Groups)
	}
	if g, ok := cfg.Classify("/build/source/deps/hiredis/hiredis.c"); !ok || g != "vendor" {
		t.Errorf("Classify after FromJSON = (%q, %v), want (vendor, true)", g, ok)
	}
}

func TestFromJSONEmptyOrInvalid(t *testing.T) {
	for _, s := range []string{"", "not json", "{}", "[1,2,3]"} {
		cfg := FromJSON(s)
		if g, ok := cfg.Classify("/build/source/anything.c"); ok || g != "" {
			t.Errorf("FromJSON(%q).Classify(...) = (%q, %v), want (\"\", false)", s, g, ok)
		}
	}
}
