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

// EagerDrv reports whether NIXGG_EAGER_DRV=1 is set: a variant of
// sandbox mode that runs over an ordinary, unrestricted daemon
// connection (e.g. `nix develop`) rather than a live builder-rpc-v0
// sandbox. It shares sandbox mode's drv-emission path, but since the
// registered .drv is a real, permanent store object right away,
// PointOutputAtDrv can symlink straight to it instead of writing a
// drvref stub (see PointOutputAtDrv).
func EagerDrv() bool {
	return os.Getenv("NIXGG_EAGER_DRV") == "1"
}

// rpcEnabled reports whether NIXGG_RPC=1 is set: use the raw
// worker-protocol client (internal/rpc) instead of fork+exec'ing the
// `nix` CLI for sandbox ops, to avoid per-call fork+exec overhead.
func rpcEnabled() bool {
	return os.Getenv("NIXGG_RPC") == "1"
}

// dialRPC connects to the daemon socket the sandbox exposes via
// NIX_REMOTE (unix://<path>).
func dialRPC() (*rpc.Conn, error) {
	remote := os.Getenv("NIX_REMOTE")
	if remote == "" {
		return nil, fmt.Errorf("NIX_REMOTE not set; not running inside a builder-rpc-v0 sandbox")
	}
	return rpc.Dial(remote)
}

// rpcBackend is satisfied by *rpc.Conn (NIXGG_RPC=1). Letting
// DerivationAdd/StoreAddScan/StoreAddDirectory/SubmitOutput each pick
// it via selectBackend collapses the rpc/CLI branching into one
// selection plus one CLI fallback per function.
type rpcBackend interface {
	AddDerivation(name string, contents []byte, refs []string) (string, error)
	AddToStoreScanning(name string, narDump []byte) (string, error)
	AddDirectory(name string, narDump []byte) (string, error)
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
// and returns the resulting drv store path. --offline: this should
// never need substituters, but some sandbox configs try anyway and
// stall on name resolution.
//
// Under NIXGG_RPC=1, renders the same ATerm bytes via internal/aterm
// and uploads them over internal/rpc's AddToStore op instead of
// fork+exec'ing `nix derivation add`.
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
// uploads it over internal/rpc's AddToStoreScanning op instead of
// fork+exec'ing `nix store add --scan`.
func StoreAddScan(cfg *toolchain.Config, name, path string) (string, error) {
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

// StoreAddDirectory uploads a directory via a plain (non-scanning)
// `nix store add -n name path` and returns the resulting store path.
//
// Unlike StoreAddScan, this does not scan content for
// /nix/store/... substrings, so the result's reference set is always
// empty — matching what native mode's plain path-literal import
// produces for identical bytes. Using --scan here would let an
// incidental /nix/store/... substring in staged content (e.g. a
// generated header baking in a runtime tool's store path) become a
// real NAR reference on the sandbox side, producing a different,
// mode-divergent store path for otherwise byte-identical content
// (confirmed against examples/nix-util's store-api.cc TU).
func StoreAddDirectory(cfg *toolchain.Config, name, path string) (string, error) {
	if b, closeB, ok, err := selectBackend(); err != nil {
		return "", fmt.Errorf("rpc store add: %w", err)
	} else if ok {
		defer closeB()
		var buf bytes.Buffer
		if err := nar.Dump(&buf, path); err != nil {
			return "", fmt.Errorf("rpc store add %s: encode NAR: %w", path, err)
		}
		sp, err := b.AddDirectory(name, buf.Bytes())
		if err != nil {
			return "", fmt.Errorf("rpc store add %s: %w", path, err)
		}
		return sp, nil
	}
	cmd := exec.Command(cfg.Nix, "--offline", "store", "add", "-n", name, path)
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
// Nix requires the submitted path's basename to match
// outputPathName(outerDrvName, outputName). mkNixggBuild names the
// outer drv "bin-<target>.drv" precisely so our inner link drv
// (also "bin-<target>.drv") satisfies this without a rename step.
//
// Under NIXGG_RPC=1, calls internal/rpc directly over the sandbox's
// daemon socket instead of fork+exec'ing `nix store submit-output`.
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

// PointOutputAtDrv records which drv produced the artifact that
// would otherwise live at `output`. Two forms, chosen by EagerDrv():
//
//   - Default (sandbox mode): writes a drvref text stub. Required
//     because builder-rpc-v0 registers .drv files with the daemon
//     but never materialises them into the sandbox filesystem, so a
//     symlink would dangle — fatal for a downstream Makefile `test
//     -e` check mid-build. See internal/drvref for the format.
//   - EagerDrv: writes a real symlink straight at drvPath. Safe only
//     because this mode never runs inside a live builder-rpc-v0
//     sandbox, so the just-registered .drv is a real, permanent
//     store object immediately. classify.Target and resolveLibFlag
//     already treat a symlink to a .drv as Kind.Drv without changes.
//
// KNOWN LIMITATION (EagerDrv only): RealiseThunkArgsAndPassthrough's
// `!sandboxEnabled` fallback only realises classify.Thunk args before
// an unshimmed passthrough exec, never classify.Drv (a .drv symlink).
// Not the mainline path — a real ~500-TU build (examples/nix-full)
// never hit it — but a gap if a link line is unparseable enough to
// reach that fallback.
func PointOutputAtDrv(output, drvPath string) error {
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	_ = os.Remove(output)
	if EagerDrv() {
		if err := os.Symlink(drvPath, output); err != nil {
			return fmt.Errorf("symlink %s -> %s: %w", output, drvPath, err)
		}
		return nil
	}
	body := drvref.Body(drvPath)
	if err := os.WriteFile(output, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write drvref %s -> %s: %w", output, drvPath, err)
	}
	return nil
}
