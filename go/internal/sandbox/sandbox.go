// Package sandbox holds sandbox-mode helpers: submitting JSON drvs to
// the outer nix daemon via `nix derivation add`, looking up drv paths
// for previously-produced outputs.
//
// This mode is selected by NIXGG_SANDBOX=1 and is only meaningful
// when nixgg is running inside a builder-rpc-v0 derivation (see
// nixgg/dyn-drv/NOTES.md).
package sandbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tbereknyei/nixgg/internal/aterm"
	"github.com/tbereknyei/nixgg/internal/drvref"
	"github.com/tbereknyei/nixgg/internal/expr"
	"github.com/tbereknyei/nixgg/internal/nar"
	"github.com/tbereknyei/nixgg/internal/rpc"
	"github.com/tbereknyei/nixgg/internal/toolchain"
)

// Enabled reports whether NIXGG_SANDBOX=1 is set.
func Enabled() bool {
	return os.Getenv("NIXGG_SANDBOX") == "1"
}

// rpcEnabled reports whether the raw worker-protocol client
// (internal/rpc) should be used in place of fork+exec'ing the `nix`
// CLI for the three sandbox ops nixgg makes per build (DerivationAdd,
// StoreAddScan, SubmitOutput). Opt-in pending a default-on decision —
// all three are wired and individually verified against real sandbox
// builds (drv-equivalence + smoke.sh's full quick set), but haven't
// yet run together end-to-end with StoreAddScan also on the RPC
// path. See ARCHITECTURE.md's "What we don't (yet) do" for why
// per-call fork+exec is worth avoiding here.
func rpcEnabled() bool {
	return os.Getenv("NIXGG_RPC") == "1"
}

// dialRPC connects to the daemon socket the sandbox already exposes
// via NIX_REMOTE (unix://<path> — see internal/rpc.Dial's own
// docstring for why this is always the sandbox's own .nix-socket,
// never a real daemon reachable another way).
func dialRPC() (*rpc.Conn, error) {
	remote := os.Getenv("NIX_REMOTE")
	if remote == "" {
		return nil, fmt.Errorf("NIX_REMOTE not set; not running inside a builder-rpc-v0 sandbox")
	}
	return rpc.Dial(remote)
}

// rpcBackend is satisfied by *rpc.Conn (NIXGG_RPC=1, a fresh dial per
// call) — the raw worker-protocol path nixgg speaks instead of
// fork+exec'ing the nix CLI. Letting DerivationAdd/StoreAddScan/
// SubmitOutput each pick it via selectBackend collapses what used to
// be near-identical rpc/CLI branches per function into one selection
// plus one CLI fallback.
type rpcBackend interface {
	AddDerivation(name string, contents []byte, refs []string) (string, error)
	AddToStoreScanning(name string, narDump []byte) (string, error)
	SubmitOutput(drvPath, output string) error
}

// selectBackend picks whether a call should use the direct-RPC path.
// ok=false (with a nil error) means RPC isn't enabled and the caller
// should fall back to the CLI; the returned close func is always
// non-nil when ok is true and must be called once the backend is no
// longer needed.
func selectBackend() (b rpcBackend, close func(), ok bool, err error) {
	if rpcEnabled() {
		conn, err := dialRPC()
		if err != nil {
			return nil, nil, false, err
		}
		return conn, func() { conn.Close() }, true, nil
	}
	return nil, nil, false, nil
}

// DerivationAdd pipes a JSON drv description to `nix derivation add`
// and returns the resulting drv store path. --offline: nix should
// have zero business contacting substituters for a `derivation add`
// (it's registering a drv description, not resolving inputs), but
// under some sandbox configurations it tries anyway and stalls on
// name-resolution failures. Cheap paranoia.
//
// Under NIXGG_RPC=1, renders the same ATerm bytes via internal/aterm
// and uploads them over internal/rpc's AddToStore op instead of
// fork+exec'ing `nix derivation add` — the highest-volume of the
// three sandbox ops (one call per compile TU, versus once per
// archive/link and once total for submit-output), so this is where
// ARCHITECTURE.md's "What we don't (yet) do" fork+exec-tax measurement
// (~44-90ms/call) actually adds up across a many-TU build.
func DerivationAdd(cfg *toolchain.Config, drv expr.JSONDrv) (string, error) {
	name := drv.Name + ".drv"
	if b, closeB, ok, err := selectBackend(); err != nil {
		return "", fmt.Errorf("rpc derivation add: %w", err)
	} else if ok {
		defer closeB()
		contents := aterm.Unparse(drv)
		refs := aterm.References(drv)
		path, err := b.AddDerivation(name, []byte(contents), refs)
		if err != nil {
			return "", fmt.Errorf("rpc derivation add %s: %w", drv.Name, err)
		}
		return path, nil
	}
	body, err := json.Marshal(drv)
	if err != nil {
		return "", fmt.Errorf("encode drv json: %w", err)
	}
	cmd := exec.Command(cfg.Nix, "--offline", "derivation", "add")
	cmd.Env = os.Environ()
	cmd.Stdin = bytes.NewReader(body)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("nix derivation add %s: %w\n%s", drv.Name, err, stderr.String())
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", fmt.Errorf("nix derivation add: empty output")
	}
	return path, nil
}

// StoreAddScan uploads a directory via `nix store add --scan -n name
// path` and returns the resulting store path. --scan makes the
// daemon scan the tree for references to already-present store
// objects and record them — required inside a sandbox where
// unregistered references cause build-time errors.
//
// Under NIXGG_RPC=1, encodes path as a NAR via internal/nar and
// uploads it over internal/rpc's AddToStoreScanning op (opcode 1001)
// instead of fork+exec'ing `nix store add --scan` — the second-
// highest-volume of the three sandbox ops (one call per compile TU,
// same as DerivationAdd).
func StoreAddScan(cfg *toolchain.Config, name, path string) (string, error) {
	// Sanitized once, before a backend is chosen: every path below —
	// helper, direct RPC, and the CLI fallback — hands this name to the
	// daemon, and Nix's naming rules apply identically to each. Doing it
	// per-backend would let a new one be added without it.
	name = StoreName(name)
	if b, closeB, ok, err := selectBackend(); err != nil {
		return "", fmt.Errorf("rpc store add --scan: %w", err)
	} else if ok {
		defer closeB()
		var buf bytes.Buffer
		if err := nar.Dump(&buf, path); err != nil {
			return "", fmt.Errorf("rpc store add --scan %s: encode NAR: %w", path, err)
		}
		sp, err := b.AddToStoreScanning(name, buf.Bytes())
		if err != nil {
			return "", fmt.Errorf("rpc store add --scan %s: %w", path, err)
		}
		return sp, nil
	}
	cmd := exec.Command(cfg.Nix, "--offline", "store", "add", "--scan", "-n", name, path)
	cmd.Env = os.Environ()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("nix store add %s: %w\n%s", path, err, stderr.String())
	}
	sp := strings.TrimSpace(string(out))
	if sp == "" {
		return "", fmt.Errorf("nix store add: empty output")
	}
	return sp, nil
}

// SubmitOutput registers a .drv path as the currently-running outer
// derivation's named output. Only valid inside a builder-rpc-v0
// sandbox.
//
// Nix requires the submitted path's *basename* to match
// `outputPathName(outerDrvName, outputName)`. mkNixggBuild names the
// outer drv "bin-<target>.drv" precisely so our inner link drv
// (also "bin-<target>.drv") satisfies this without a rename step.
// If a rename is ever required, the mechanism would be: re-upload
// the drv ATerm bytes via `nix store add --mode text -n <canonical>`.
// That in turn requires access to the bytes — inside builder-rpc-v0
// the .drv file isn't materialised on disk, so the caller must
// capture bytes at submission time. See nix-ninja
// crates/nix-builder-rpc-client for the daemon-RPC approach.
//
// Under NIXGG_RPC=1, calls internal/rpc directly over the sandbox's
// own daemon socket instead of fork+exec'ing `nix store
// submit-output` — see internal/rpc's own docs for the wire protocol
// this replicates (WorkerProto::Op::SubmitOutput, opcode 1000).
func SubmitOutput(cfg *toolchain.Config, drvPath, outputName string) error {
	if b, closeB, ok, err := selectBackend(); err != nil {
		return fmt.Errorf("rpc submit-output: %w", err)
	} else if ok {
		defer closeB()
		if err := b.SubmitOutput(drvPath, outputName); err != nil {
			return fmt.Errorf("rpc submit-output %s %s: %w", drvPath, outputName, err)
		}
		return nil
	}
	cmd := exec.Command(cfg.Nix, "--offline", "store", "submit-output", drvPath, outputName)
	cmd.Env = os.Environ()
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nix store submit-output %s %s: %w", drvPath, outputName, err)
	}
	return nil
}

// PointOutputAtDrv writes a drvref stub at `output`, recording which
// drv produced the artifact that would otherwise live there. It is the
// sandbox-mode analogue of native mode's .nix-thunk symlink.
//
// See internal/drvref for the format and for why it is a regular file
// rather than a symlink.
func PointOutputAtDrv(output, drvPath string) error {
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	_ = os.Remove(output)
	body := drvref.Body(drvPath)
	if err := os.WriteFile(output, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write drvref %s -> %s: %w", output, drvPath, err)
	}
	return nil
}
