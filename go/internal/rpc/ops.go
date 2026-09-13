package rpc

import "fmt"

// AddDerivation registers a derivation's ATerm text with the daemon,
// replacing `nix --offline derivation add`'s fork+exec. Content-addressed
// as text, with the drv's own srcs/drv-input references (already
// collected by the caller), never repairing.
//
// Deliberately skips Store::writeDerivation's own isValidPath
// short-circuit: getting Nix's makeStorePath/compressHash formula wrong
// would silently upload every time instead of failing loudly, and the
// daemon does the equivalent check server-side anyway — this client
// only pays one avoidable round trip per call, not a correctness risk.
func (c *Conn) AddDerivation(name string, contents []byte, refs []string) (storePath string, err error) {
	if err := c.w.writeUint64(uint64(opAddToStore)); err != nil {
		return "", fmt.Errorf("rpc: AddDerivation: write op: %w", err)
	}
	if err := c.w.writeString(name); err != nil {
		return "", fmt.Errorf("rpc: AddDerivation: write name: %w", err)
	}
	if err := c.w.writeString(contentAddressText); err != nil {
		return "", fmt.Errorf("rpc: AddDerivation: write cam: %w", err)
	}
	if err := c.w.writeStrings(refs); err != nil {
		return "", fmt.Errorf("rpc: AddDerivation: write refs: %w", err)
	}
	if err := c.w.writeUint64(0); err != nil { // repair: false
		return "", fmt.Errorf("rpc: AddDerivation: write repair: %w", err)
	}
	if err := c.w.flush(); err != nil {
		return "", fmt.Errorf("rpc: AddDerivation: flush request: %w", err)
	}
	if err := c.w.writeFramed(contents); err != nil {
		return "", fmt.Errorf("rpc: AddDerivation: upload contents: %w", err)
	}
	if err := c.w.flush(); err != nil {
		return "", fmt.Errorf("rpc: AddDerivation: flush upload: %w", err)
	}
	if err := c.w.drainStderr(); err != nil {
		return "", fmt.Errorf("rpc: AddDerivation: %w", err)
	}
	return c.readValidPathInfoPath()
}

// AddToStoreScanning uploads a directory as a NAR, letting the daemon
// scan it for references to already-present store objects — the
// builder-rpc-v0/recursive-nix-only op behind `nix store add --scan`.
// Requires the daemon to have advertised the add-to-store-scanning
// handshake feature; errors immediately if not, same as the real
// client does before writing the request.
//
// narDump must already be a complete NAR-format dump of the directory.
func (c *Conn) AddToStoreScanning(name string, narDump []byte) (storePath string, err error) {
	if !c.hasFeature(featureAddToStoreScanning) {
		return "", fmt.Errorf("rpc: AddToStoreScanning: daemon does not support add-to-store-scanning (not in a builder-rpc-v0/recursive-nix derivation?)")
	}
	if err := c.w.writeUint64(uint64(opAddToStoreScanning)); err != nil {
		return "", fmt.Errorf("rpc: AddToStoreScanning: write op: %w", err)
	}
	if err := c.w.writeString(name); err != nil {
		return "", fmt.Errorf("rpc: AddToStoreScanning: write name: %w", err)
	}
	if err := c.w.writeString(contentAddressFixedRecursive); err != nil {
		return "", fmt.Errorf("rpc: AddToStoreScanning: write cam: %w", err)
	}
	if err := c.w.flush(); err != nil {
		return "", fmt.Errorf("rpc: AddToStoreScanning: flush request: %w", err)
	}
	if err := c.w.writeFramed(narDump); err != nil {
		return "", fmt.Errorf("rpc: AddToStoreScanning: upload NAR: %w", err)
	}
	if err := c.w.flush(); err != nil {
		return "", fmt.Errorf("rpc: AddToStoreScanning: flush upload: %w", err)
	}
	if err := c.w.drainStderr(); err != nil {
		return "", fmt.Errorf("rpc: AddToStoreScanning: %w", err)
	}
	return c.readValidPathInfoPath()
}

// AddDirectory uploads a directory as a NAR via the plain
// (non-scanning) AddToStore op, content-addressed as "fixed:r:sha256"
// with an empty reference set — the sandbox-mode analogue of
// `nix-store --add` (no `--scan`), matching what native mode's plain
// `srcTree = ../srcs/<tu-id>;` path-literal import produces so both
// modes land on the same store path for identical bytes.
//
// narDump must already be a complete NAR-format dump of the directory.
func (c *Conn) AddDirectory(name string, narDump []byte) (storePath string, err error) {
	if err := c.w.writeUint64(uint64(opAddToStore)); err != nil {
		return "", fmt.Errorf("rpc: AddDirectory: write op: %w", err)
	}
	if err := c.w.writeString(name); err != nil {
		return "", fmt.Errorf("rpc: AddDirectory: write name: %w", err)
	}
	if err := c.w.writeString(contentAddressFixedRecursive); err != nil {
		return "", fmt.Errorf("rpc: AddDirectory: write cam: %w", err)
	}
	if err := c.w.writeStrings(nil); err != nil { // refs: none
		return "", fmt.Errorf("rpc: AddDirectory: write refs: %w", err)
	}
	if err := c.w.writeUint64(0); err != nil { // repair: false
		return "", fmt.Errorf("rpc: AddDirectory: write repair: %w", err)
	}
	if err := c.w.flush(); err != nil {
		return "", fmt.Errorf("rpc: AddDirectory: flush request: %w", err)
	}
	if err := c.w.writeFramed(narDump); err != nil {
		return "", fmt.Errorf("rpc: AddDirectory: upload NAR: %w", err)
	}
	if err := c.w.flush(); err != nil {
		return "", fmt.Errorf("rpc: AddDirectory: flush upload: %w", err)
	}
	if err := c.w.drainStderr(); err != nil {
		return "", fmt.Errorf("rpc: AddDirectory: %w", err)
	}
	return c.readValidPathInfoPath()
}

// readValidPathInfoPath reads a ValidPathInfo response (StorePath +
// UnkeyedValidPathInfo) and returns just the path; every other field
// is daemon bookkeeping the caller doesn't need.
func (c *Conn) readValidPathInfoPath() (string, error) {
	path, err := c.w.readString() // StorePath: plain printed path text
	if err != nil {
		return "", fmt.Errorf("rpc: read ValidPathInfo path: %w", err)
	}
	if err := c.skipUnkeyedValidPathInfo(); err != nil {
		return "", err
	}
	return path, nil
}

// skipUnkeyedValidPathInfo discards deriver, narHash, references,
// registrationTime, narSize, ultimate, sigs, and ca.
func (c *Conn) skipUnkeyedValidPathInfo() error {
	if _, err := c.w.readString(); err != nil { // deriver (optional StorePath, "" if none)
		return fmt.Errorf("rpc: read deriver: %w", err)
	}
	if _, err := c.w.readString(); err != nil { // narHash
		return fmt.Errorf("rpc: read narHash: %w", err)
	}
	if _, err := c.w.readStrings(); err != nil { // references
		return fmt.Errorf("rpc: read references: %w", err)
	}
	if _, err := c.w.readUint64(); err != nil { // registrationTime
		return fmt.Errorf("rpc: read registrationTime: %w", err)
	}
	if _, err := c.w.readUint64(); err != nil { // narSize
		return fmt.Errorf("rpc: read narSize: %w", err)
	}
	if _, err := c.w.readUint64(); err != nil { // ultimate (bool as uint64)
		return fmt.Errorf("rpc: read ultimate: %w", err)
	}
	if _, err := c.w.readStrings(); err != nil { // sigs
		return fmt.Errorf("rpc: read sigs: %w", err)
	}
	if _, err := c.w.readString(); err != nil { // ca
		return fmt.Errorf("rpc: read ca: %w", err)
	}
	return nil
}

// SubmitOutput registers drvPath as the currently-running outer
// derivation's `output`, replacing `nix store submit-output`'s
// fork+exec. Only valid inside a builder-rpc-v0 sandbox with the outer
// drv's requiredSystemFeatures set accordingly; the daemon enforces
// this and returns a protocol error otherwise.
//
// path is always sent as SingleDerivedPath::Opaque (tag 0) — a plain
// already-built .drv StorePath. nixgg never submits a Built path (a
// "drv^output" reference to another not-yet-resolved derivation's
// output); a future caller needing that needs its own tagged encoding.
func (c *Conn) SubmitOutput(drvPath, output string) error {
	if !c.hasFeature(featureSubmitOutput) {
		return fmt.Errorf("rpc: SubmitOutput: daemon does not support submit-output (not in a derivation with the builder-rpc-v0 feature?)")
	}
	if err := c.w.writeUint64(uint64(opSubmitOutput)); err != nil {
		return fmt.Errorf("rpc: SubmitOutput: write op: %w", err)
	}
	if err := c.w.writeUint64(0); err != nil { // SingleDerivedPath::Opaque tag
		return fmt.Errorf("rpc: SubmitOutput: write path tag: %w", err)
	}
	if err := c.w.writeString(drvPath); err != nil {
		return fmt.Errorf("rpc: SubmitOutput: write path: %w", err)
	}
	if err := c.w.writeString(output); err != nil {
		return fmt.Errorf("rpc: SubmitOutput: write output name: %w", err)
	}
	if err := c.w.flush(); err != nil {
		return fmt.Errorf("rpc: SubmitOutput: flush request: %w", err)
	}
	if err := c.w.drainStderr(); err != nil {
		return fmt.Errorf("rpc: SubmitOutput: %w", err)
	}
	// RemoteStore::submitOutput reads a trailing result code after
	// STDERR drains clean; unused, but must be read to stay in sync
	// with the daemon (upstream's own client discards it too).
	if _, err := c.w.readUint64(); err != nil {
		return fmt.Errorf("rpc: SubmitOutput: read result: %w", err)
	}
	return nil
}
