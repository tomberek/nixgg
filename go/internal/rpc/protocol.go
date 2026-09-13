// Package rpc speaks the Nix daemon's worker protocol directly over the
// Unix socket a builder-rpc-v0 sandbox exposes via NIX_REMOTE, replacing
// per-call `nix --offline derivation add` / `nix store add --scan` /
// `nix store submit-output` fork+execs with one persistent connection.
//
// Wire formats are pinned to nix-15793 (NixOS/nix@8307c48, protocol
// 1.39, PR #15793's builder-rpc-v0 work). Not a general worker-protocol
// client: only the ops nixgg's shims need, plus enough handshake/STDERR
// machinery to reach them.
package rpc

const (
	workerMagic1 = 0x6e697863
	workerMagic2 = 0x6478696f

	// This client's worker-protocol version, (major<<8)|minor. Negotiated
	// version is min(ours, daemon's).
	protoMajor = 1
	protoMinor = 39

	featureMinVersion = (1 << 8) | 38 // feature list exchange gated on this
)

// op is a WorkerProto::Op value. Only the ones this client sends.
type op uint64

const (
	opAddToStore         op = 7
	opSubmitOutput       op = 1000
	opAddToStoreScanning op = 1001
)

// STDERR wire messages (src/libstore/include/nix/store/worker-protocol.hh).
const (
	stderrNext          uint64 = 0x6f6c6d67
	stderrLast          uint64 = 0x616c7473
	stderrError         uint64 = 0x63787470
	stderrStartActivity uint64 = 0x53545254
	stderrStopActivity  uint64 = 0x53544f50
	stderrResult        uint64 = 0x52534c54
)

// featureAddToStoreScanning / featureSubmitOutput are the daemon
// handshake feature names gating the two builder-rpc-v0-only ops.
const (
	featureAddToStoreScanning = "add-to-store-scanning"
	featureSubmitOutput       = "submit-output"
)

// contentAddressText / contentAddressFixedRecursive are the two
// ContentAddressMethod prefixes this client needs: "text:" for a
// derivation's ATerm text, "fixed:r:" for a recursively-NAR-hashed
// directory dump.
const (
	contentAddressText           = "text:sha256"
	contentAddressFixedRecursive = "fixed:r:sha256"
)
