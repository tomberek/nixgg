package activitylog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func resetState(t *testing.T) {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	if logFile != nil {
		logFile.Close()
	}
	logFile = nil
	logPath = ""
	pathOnce = sync.Once{}
}

func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var lines []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("unmarshal line %q: %v", sc.Text(), err)
		}
		lines = append(lines, m)
	}
	return lines
}

func TestEmitDisabledByDefault(t *testing.T) {
	resetState(t)
	os.Unsetenv("NIXGG_LOG")
	os.Unsetenv("NIXGG_SANDBOX")

	dir := t.TempDir()
	path := filepath.Join(dir, "activity.ndjson")
	Emit("compile", "thunk", Fields{"source": "foo.c"})

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Emit with NIXGG_LOG unset created a file: %v", err)
	}
}

// Line shape must match the old bash nixgg::emit schema for existing jq tooling.
func TestEmitWritesEnvelopeAndFields(t *testing.T) {
	resetState(t)
	os.Unsetenv("NIXGG_SANDBOX")
	dir := t.TempDir()
	path := filepath.Join(dir, "activity.ndjson")
	t.Setenv("NIXGG_LOG", path)

	Emit("compile", "thunk", Fields{"source": "foo.c", "output": "foo.o"})

	lines := readLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	line := lines[0]
	if line["event"] != "compile" {
		t.Errorf("event = %v, want \"compile\"", line["event"])
	}
	if line["kind"] != "thunk" {
		t.Errorf("kind = %v, want \"thunk\"", line["kind"])
	}
	if line["source"] != "foo.c" || line["output"] != "foo.o" {
		t.Errorf("caller fields missing or wrong: %v", line)
	}
	if _, ok := line["ts"]; !ok {
		t.Error("missing ts field")
	}
	if _, ok := line["cwd"]; !ok {
		t.Error("missing cwd field")
	}
}

func TestEmitAppendsMultipleLines(t *testing.T) {
	resetState(t)
	os.Unsetenv("NIXGG_SANDBOX")
	dir := t.TempDir()
	path := filepath.Join(dir, "activity.ndjson")
	t.Setenv("NIXGG_LOG", path)

	Emit("compile", "thunk", Fields{"source": "a.c"})
	Emit("compile", "thunk", Fields{"source": "b.c"})
	Emit("link", "drv", Fields{"output": "app"})

	lines := readLines(t, path)
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	if lines[0]["source"] != "a.c" || lines[1]["source"] != "b.c" || lines[2]["event"] != "link" {
		t.Errorf("lines out of order or wrong content: %v", lines)
	}
}

// A builder-rpc-v0 sandbox's own filesystem writes never reach the host, so Emit must skip the attempt entirely under NIXGG_SANDBOX=1.
func TestEmitNoOpUnderSandbox(t *testing.T) {
	resetState(t)
	t.Setenv("NIXGG_SANDBOX", "1")
	dir := t.TempDir()
	path := filepath.Join(dir, "activity.ndjson")
	t.Setenv("NIXGG_LOG", path)

	Emit("compile", "thunk", Fields{"source": "foo.c"})

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Emit wrote a file under NIXGG_SANDBOX=1: %v", err)
	}
}

func TestEmitBadPathDoesNotPanic(t *testing.T) {
	resetState(t)
	os.Unsetenv("NIXGG_SANDBOX")
	t.Setenv("NIXGG_LOG", "/nonexistent-dir-nixgg-test/activity.ndjson")

	Emit("compile", "thunk", Fields{"source": "foo.c"})
}
