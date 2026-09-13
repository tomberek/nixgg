package rpc

import (
	"fmt"
	"net"
	"strings"
)

// Conn is one worker-protocol session with a Nix daemon, reached over
// the Unix socket a builder-rpc-v0 sandbox exposes via NIX_REMOTE
// (unix://<path>). Not pooled or reused across processes; nixgg's shim
// is a fresh OS process per compile, so one Conn's lifetime is one shim
// invocation — still saves the CLI's process-startup/config-load/connect
// cost (~20-30ms) versus fork+exec'ing `nix` per operation.
type Conn struct {
	nc       net.Conn
	w        *wire
	version  int // negotiated (major<<8)|minor
	features map[string]bool
}

// Dial connects to the daemon socket named by NIX_REMOTE and performs
// the full client handshake (magic, version, feature exchange,
// affinity/reserve-space stubs, daemon version + trust status).
//
// remote must be "unix://<path>" — the only form a builder-rpc-v0
// sandbox sets. Any other scheme means this isn't running inside a
// sandbox that speaks the raw protocol, and callers should fall back
// to the nix CLI instead of calling Dial at all.
func Dial(remote string) (*Conn, error) {
	path, ok := strings.CutPrefix(remote, "unix://")
	if !ok {
		return nil, fmt.Errorf("rpc: NIX_REMOTE %q is not a unix:// socket", remote)
	}
	nc, err := net.Dial("unix", path)
	if err != nil {
		return nil, fmt.Errorf("rpc: dial %s: %w", path, err)
	}
	c := &Conn{nc: nc, w: newWire(nc), features: map[string]bool{}}
	if err := c.handshake(); err != nil {
		nc.Close()
		return nil, err
	}
	return c, nil
}

func (c *Conn) Close() error { return c.nc.Close() }

// handshake replicates WorkerProto::BasicClientConnection::handshake +
// postHandshake, client side: write magic+version, exchange feature
// lists if both sides are >= 1.38, write the two obsolete
// affinity/reserve-space stub values, then read back
// ClientHandshakeInfo (daemon version string + trust flag).
func (c *Conn) handshake() error {
	if err := c.w.writeUint64(workerMagic1); err != nil {
		return fmt.Errorf("rpc: write client magic: %w", err)
	}
	if err := c.w.writeUint64(uint64(protoMajor<<8 | protoMinor)); err != nil {
		return fmt.Errorf("rpc: write client version: %w", err)
	}
	if err := c.w.flush(); err != nil {
		return fmt.Errorf("rpc: flush handshake: %w", err)
	}

	magic, err := c.w.readUint64()
	if err != nil {
		return fmt.Errorf("rpc: read daemon magic: %w", err)
	}
	if magic != workerMagic2 {
		return fmt.Errorf("rpc: protocol mismatch: daemon magic %#x", magic)
	}
	daemonVersionWire, err := c.w.readUint64()
	if err != nil {
		return fmt.Errorf("rpc: read daemon version: %w", err)
	}
	daemonVersion := int(daemonVersionWire) & 0xffff
	if daemonVersion>>8 != protoMajor {
		return fmt.Errorf("rpc: daemon protocol major version %d unsupported (want %d)", daemonVersion>>8, protoMajor)
	}

	negotiated := daemonVersion
	if own := protoMajor<<8 | protoMinor; own < negotiated {
		negotiated = own
	}
	c.version = negotiated

	if negotiated >= featureMinVersion {
		// Must advertise these explicitly: they aren't in
		// WorkerProto::latest by default, only added conditionally on
		// the daemon side, so omitting them here would negotiate them
		// away even if the daemon supports both.
		ourFeatures := []string{featureAddToStoreScanning, featureSubmitOutput}
		if err := c.w.writeStrings(ourFeatures); err != nil {
			return fmt.Errorf("rpc: write feature list: %w", err)
		}
		if err := c.w.flush(); err != nil {
			return fmt.Errorf("rpc: flush feature list: %w", err)
		}
		daemonFeatures, err := c.w.readStrings()
		if err != nil {
			return fmt.Errorf("rpc: read daemon features: %w", err)
		}
		want := map[string]bool{featureAddToStoreScanning: true, featureSubmitOutput: true}
		for _, f := range daemonFeatures {
			if want[f] {
				c.features[f] = true
			}
		}
	}

	// postHandshake's two obsolete stub writes (CPU affinity, reserve
	// space); every supported protocol version is past both thresholds.
	if err := c.w.writeUint64(0); err != nil { // affinity: none
		return fmt.Errorf("rpc: write affinity stub: %w", err)
	}
	if err := c.w.writeUint64(0); err != nil { // reserveSpace: false
		return fmt.Errorf("rpc: write reserve-space stub: %w", err)
	}
	if err := c.w.flush(); err != nil {
		return fmt.Errorf("rpc: flush post-handshake: %w", err)
	}

	// ClientHandshakeInfo: daemon version string, then trust flag.
	if _, err := c.w.readString(); err != nil { // daemon version string, unused
		return fmt.Errorf("rpc: read daemon version string: %w", err)
	}
	if _, err := c.w.readUint64(); err != nil { // trust tag, unused
		return fmt.Errorf("rpc: read trust flag: %w", err)
	}

	// RemoteStore::initConnection drains one more STDERR sequence right
	// after postHandshake before the connection is usable — skipping
	// this leaves the daemon's post-handshake STDERR_LAST unread,
	// silently shifting every subsequent read by one message.
	if err := c.w.drainStderr(); err != nil {
		return fmt.Errorf("rpc: post-handshake stderr: %w", err)
	}

	// SetOptions is deliberately never sent: the real daemon connection
	// this client talks to (a builder-rpc-v0 sandbox's
	// RecursiveSubmitted daemon) doesn't allow it — sending it fails
	// every call, confirmed against a real sandbox build
	// ("Operation 19 not allowed inside derivation").

	return nil
}

// hasFeature reports whether the daemon advertised the named
// builder-rpc-v0 worker-protocol feature during handshake.
func (c *Conn) hasFeature(name string) bool { return c.features[name] }
