package rpc

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// wire wraps a connection with the worker protocol's primitive
// encodings: little-endian uint64, 8-byte-padded length-prefixed
// strings, and the STDERR side-channel every op reply goes through.
type wire struct {
	r *bufio.Reader
	w *bufio.Writer
}

func newWire(rw io.ReadWriter) *wire {
	return &wire{r: bufio.NewReader(rw), w: bufio.NewWriter(rw)}
}

func (w *wire) flush() error { return w.w.Flush() }

func (w *wire) writeUint64(v uint64) error {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], v)
	_, err := w.w.Write(buf[:])
	return err
}

func (w *wire) readUint64() (uint64, error) {
	var buf [8]byte
	if _, err := io.ReadFull(w.r, buf[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(buf[:]), nil
}

// maxStringLen bounds a single string read so a protocol desync
// produces a clear error instead of an OOM attempt.
const maxStringLen = 256 * 1024 * 1024

func (w *wire) writeString(s string) error {
	if err := w.writeUint64(uint64(len(s))); err != nil {
		return err
	}
	if _, err := w.w.WriteString(s); err != nil {
		return err
	}
	if pad := padLen(len(s)); pad > 0 {
		var zeros [8]byte
		if _, err := w.w.Write(zeros[:pad]); err != nil {
			return err
		}
	}
	return nil
}

func (w *wire) readString() (string, error) {
	n, err := w.readUint64()
	if err != nil {
		return "", err
	}
	if n > maxStringLen {
		return "", fmt.Errorf("rpc: string length %d exceeds sanity limit", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(w.r, buf); err != nil {
		return "", err
	}
	if pad := padLen(int(n)); pad > 0 {
		if _, err := w.r.Discard(pad); err != nil {
			return "", err
		}
	}
	return string(buf), nil
}

func padLen(n int) int { return (8 - n%8) % 8 }

func (w *wire) writeStrings(ss []string) error {
	if err := w.writeUint64(uint64(len(ss))); err != nil {
		return err
	}
	for _, s := range ss {
		if err := w.writeString(s); err != nil {
			return err
		}
	}
	return nil
}

func (w *wire) readStrings() ([]string, error) {
	n, err := w.readUint64()
	if err != nil {
		return nil, err
	}
	ss := make([]string, n)
	for i := range ss {
		if ss[i], err = w.readString(); err != nil {
			return nil, fmt.Errorf("read string %d/%d: %w", i, n, err)
		}
	}
	return ss, nil
}

// writeFramed uploads data as a sequence of (len, bytes) frames
// terminated by a zero-length frame. One frame is enough for anything
// nixgg uploads; no need to chunk.
func (w *wire) writeFramed(data []byte) error {
	if len(data) > 0 {
		if err := w.writeUint64(uint64(len(data))); err != nil {
			return err
		}
		if _, err := w.w.Write(data); err != nil {
			return err
		}
	}
	return w.writeUint64(0)
}

// protoError is the structured error format daemons speak from
// protocol 1.26 onward (type string + level + name + msg + trace).
type protoError struct {
	level int32
	msg   string
	trace []string
}

func (e *protoError) Error() string {
	if len(e.trace) == 0 {
		return e.msg
	}
	return e.msg + "\n  " + joinLines(e.trace)
}

func joinLines(ss []string) string {
	out := ss[0]
	for _, s := range ss[1:] {
		out += "\n  " + s
	}
	return out
}

func (w *wire) readProtoError() (*protoError, error) {
	typ, err := w.readString()
	if err != nil {
		return nil, err
	}
	if typ != "Error" {
		return nil, fmt.Errorf("rpc: unexpected error type %q", typ)
	}
	level, err := w.readUint64()
	if err != nil {
		return nil, err
	}
	if _, err := w.readString(); err != nil { // name, unused/removed upstream too
		return nil, err
	}
	msg, err := w.readString()
	if err != nil {
		return nil, err
	}
	havePos, err := w.readUint64()
	if err != nil {
		return nil, err
	}
	if havePos != 0 {
		return nil, fmt.Errorf("rpc: error position deserialisation not supported")
	}
	nrTraces, err := w.readUint64()
	if err != nil {
		return nil, err
	}
	trace := make([]string, 0, nrTraces)
	for i := uint64(0); i < nrTraces; i++ {
		hp, err := w.readUint64()
		if err != nil {
			return nil, err
		}
		if hp != 0 {
			return nil, fmt.Errorf("rpc: trace position deserialisation not supported")
		}
		hint, err := w.readString()
		if err != nil {
			return nil, err
		}
		trace = append(trace, hint)
	}
	return &protoError{level: int32(level), msg: msg, trace: trace}, nil
}

// drainStderr reads STDERR_* messages until STDERR_LAST, discarding
// logging/activity noise, and returns the daemon error if one arrived.
// No STDERR_READ/STDERR_WRITE handling: those are for recursive-nix
// builder callbacks, which nixgg's shims never trigger.
func (w *wire) drainStderr() error {
	for {
		msg, err := w.readUint64()
		if err != nil {
			return fmt.Errorf("read stderr marker: %w", err)
		}
		switch msg {
		case stderrNext:
			if _, err := w.readString(); err != nil {
				return fmt.Errorf("read stderr text: %w", err)
			}
		case stderrError:
			pe, err := w.readProtoError()
			if err != nil {
				return fmt.Errorf("read stderr error: %w", err)
			}
			return pe
		case stderrStartActivity:
			if err := w.skipActivity(); err != nil {
				return err
			}
		case stderrStopActivity:
			if _, err := w.readUint64(); err != nil { // activity id
				return fmt.Errorf("read activity id: %w", err)
			}
		case stderrResult:
			if err := w.skipResult(); err != nil {
				return err
			}
		case stderrLast:
			return nil
		default:
			return fmt.Errorf("rpc: unknown stderr marker %#x", msg)
		}
	}
}

// skipActivity discards a STDERR_START_ACTIVITY payload: activityId,
// verbosity, activityType, string, fields, parent activityId.
func (w *wire) skipActivity() error {
	for i := 0; i < 3; i++ { // activityId, verbosity, activityType
		if _, err := w.readUint64(); err != nil {
			return fmt.Errorf("read activity field %d: %w", i, err)
		}
	}
	if _, err := w.readString(); err != nil {
		return fmt.Errorf("read activity string: %w", err)
	}
	if err := w.skipFields(); err != nil {
		return err
	}
	if _, err := w.readUint64(); err != nil { // parent
		return fmt.Errorf("read activity parent: %w", err)
	}
	return nil
}

// skipResult discards a STDERR_RESULT payload: activityId,
// resultType, fields.
func (w *wire) skipResult() error {
	if _, err := w.readUint64(); err != nil { // activity id
		return fmt.Errorf("read result activity id: %w", err)
	}
	if _, err := w.readUint64(); err != nil { // result type
		return fmt.Errorf("read result type: %w", err)
	}
	return w.skipFields()
}

// skipFields discards a Logger::Field list: count, then per-field a
// type tag (0 = uint64, 1 = string) and a value of that type.
func (w *wire) skipFields() error {
	n, err := w.readUint64()
	if err != nil {
		return fmt.Errorf("read field count: %w", err)
	}
	for i := uint64(0); i < n; i++ {
		typ, err := w.readUint64()
		if err != nil {
			return fmt.Errorf("read field %d type: %w", i, err)
		}
		switch typ {
		case 0:
			if _, err := w.readUint64(); err != nil {
				return fmt.Errorf("read field %d int value: %w", i, err)
			}
		case 1:
			if _, err := w.readString(); err != nil {
				return fmt.Errorf("read field %d string value: %w", i, err)
			}
		default:
			return fmt.Errorf("rpc: unsupported field type %d", typ)
		}
	}
	return nil
}
