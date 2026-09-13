// Package activitylog emits one ndjson line per shim decision to
// NIXGG_LOG, when set. Off by default: with NIXGG_LOG unset, Emit's
// first check is the env lookup, so no JSON marshaling happens.
//
// Every event line has the same envelope — event, kind, ts (unix
// seconds, float), cwd — plus whatever event-specific fields the
// caller passes in Fields.
package activitylog

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/tbereknyei/nixgg/internal/sandbox"
)

var (
	mu       sync.Mutex
	pathOnce sync.Once
	logPath  string
	logFile  *os.File
)

// Fields is the event-specific payload Emit merges into the common
// envelope. A plain map, not a typed struct per event, since events
// differ in shape per (event, kind) pair — a single struct covering
// every field any event ever needs would carry mostly-empty fields on
// every line.
type Fields map[string]any

// Emit appends one ndjson line to NIXGG_LOG, if set. event is the
// shim family ("compile", "link", "ar", "batch"); kind is the
// decision this invocation made ("thunk", "drv", "passthrough",
// "cache_hit", "argv_cache_hit", ...).
//
// A no-op under sandbox.Enabled(): a sandboxed derivation's filesystem
// writes never reach the host (they land in the sandbox's own private
// mount), so NIXGG_LOG can only ever produce a readable file in native
// mode.
//
// Failure to open/write the log file is deliberately silent
// (best-effort): losing an activity-log line must never fail a real
// build.
func Emit(event, kind string, fields Fields) {
	if sandbox.Enabled() {
		return
	}
	path := os.Getenv("NIXGG_LOG")
	if path == "" {
		return
	}

	line := Fields{
		"event": event,
		"kind":  kind,
		"ts":    float64(time.Now().UnixNano()) / 1e9,
	}
	if cwd, err := os.Getwd(); err == nil {
		line["cwd"] = cwd
	}
	for k, v := range fields {
		line[k] = v
	}

	b, err := json.Marshal(line)
	if err != nil {
		return
	}
	b = append(b, '\n')

	mu.Lock()
	defer mu.Unlock()
	f := openLogFile(path)
	if f == nil {
		return
	}
	// One Write call for the whole line: O_APPEND makes a single
	// write(2) atomic against other processes appending to the same
	// file concurrently (make -jN runs many shim invocations, each
	// its own process) — multiple smaller writes could interleave.
	_, _ = f.Write(b)
}

// openLogFile opens NIXGG_LOG once per process and reuses the handle
// for the process's lifetime.
func openLogFile(path string) *os.File {
	pathOnce.Do(func() {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		logFile = f
		logPath = path
	})
	if logPath != path {
		return nil
	}
	return logFile
}
