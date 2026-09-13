// Package aterm renders the same ATerm-format derivation text `nix
// derivation add` computes internally (Store::writeDerivation ->
// derivation::unparse), from the same expr.JSONDrv struct the sandbox
// shim already builds for the CLI JSON path. This lets internal/sandbox
// register a derivation over the raw worker-protocol client
// (internal/rpc) instead of fork+exec'ing the CLI.
//
// Rules here mirror the pinned nix-15793 source (NixOS/nix@8307c48)
// src/libstore/derivation/aterm.cc's unparseDerivation<FullInputs,
// false>; verified byte-for-byte against real .drv files in
// aterm_test.go.
package aterm

import (
	"sort"
	"strings"

	"github.com/tbereknyei/nixgg/internal/expr"
)

// Unparse renders drv's ATerm text — the exact bytes
// Store::writeDerivation hashes to compute the drv's own store path,
// and the exact bytes internal/rpc.Conn.AddDerivation must upload.
//
// nixgg's inputDrvs are always already-resolved sibling .drv paths
// with a flat output-name list, never a dynamic-derivation childMap,
// so this only implements the "Derive(...)" traditional form.
func References(drv expr.JSONDrv) []string {
	refs := fullSrcPaths(drv.Inputs.Srcs)
	for b := range drv.Inputs.Drvs {
		refs = append(refs, storeDir+"/"+b)
	}
	sort.Strings(refs)
	return refs
}

func Unparse(drv expr.JSONDrv) string {
	var s strings.Builder
	s.Grow(4096)

	s.WriteString("Derive([")
	writeOutputs(&s, drv.Outputs)
	s.WriteString("],[")
	writeInputDrvs(&s, drv.Inputs.Drvs)
	s.WriteString("],")
	writeQuotedStrings(&s, fullSrcPaths(drv.Inputs.Srcs))
	s.WriteByte(',')
	writeQuotedString(&s, drv.System)
	s.WriteByte(',')
	writeEscapedString(&s, drv.Builder)
	s.WriteByte(',')
	writeEscapedStrings(&s, drv.Args)
	s.WriteString(",[")
	writeEnv(&s, drv.Env)
	s.WriteString("])")

	return s.String()
}

// writeOutputs renders the outputs list, sorted by name to match Nix's
// std::map iteration order.
//
// nixgg's JSONOut always describes a CAFloating output — path and hash
// unknown until build time — so path/hash are always written empty.
func writeOutputs(s *strings.Builder, outputs map[string]expr.JSONOut) {
	names := make([]string, 0, len(outputs))
	for name := range outputs {
		names = append(names, name)
	}
	sort.Strings(names)

	for i, name := range names {
		if i > 0 {
			s.WriteByte(',')
		}
		out := outputs[name]
		s.WriteByte('(')
		writeQuotedString(s, name)
		s.WriteByte(',')
		writeQuotedString(s, "") // path: unknown until build
		s.WriteByte(',')
		writeQuotedString(s, methodPrefix(out.Method)+out.HashAlgo)
		s.WriteByte(',')
		writeQuotedString(s, "") // hash: unknown until build
		s.WriteByte(')')
	}
}

// methodPrefix mirrors ContentAddressMethod::renderPrefix(): "r:" for
// a NAR-hashed (recursive) output, "" for a flat one. nixgg's JSONOut
// only ever uses "nar"/"flat" (see expr.go's own JSONOut docstring) —
// git-hashed outputs don't exist in this codebase.
func methodPrefix(method string) string {
	if method == "nar" {
		return "r:"
	}
	return ""
}

// writeInputDrvs renders the input-derivations list, sorted by
// basename to match Nix's std::map<StorePath, ...> iteration order.
//
// drvs' keys are basenames, not full store paths (see
// expr.JSONDrvInputs.Drvs) — storeDir must be prepended here, same gap
// fullSrcPaths handles for Srcs. Every nixgg input-drv entry is a flat
// output-name list, never a nested childMap.
func writeInputDrvs(s *strings.Builder, drvs map[string]expr.JSONDrvRef) {
	basenames := make([]string, 0, len(drvs))
	for b := range drvs {
		basenames = append(basenames, b)
	}
	sort.Strings(basenames)

	for i, b := range basenames {
		if i > 0 {
			s.WriteByte(',')
		}
		s.WriteByte('(')
		writeQuotedString(s, storeDir+"/"+b)
		s.WriteByte(',')
		writeQuotedStrings(s, sortedCopy(drvs[b].Outputs))
		s.WriteByte(')')
	}
}

// writeEnv renders the environment-variable list, sorted by key to
// match Nix's StringPairs (std::map) iteration order.
func writeEnv(s *strings.Builder, env map[string]string) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for i, k := range keys {
		if i > 0 {
			s.WriteByte(',')
		}
		s.WriteByte('(')
		writeEscapedString(s, k)
		s.WriteByte(',')
		writeEscapedString(s, env[k])
		s.WriteByte(')')
	}
}

func sortedCopy(ss []string) []string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}

// storeDir is Nix's default store directory. nixgg has no provision
// for a relocated store anywhere, so this is hardcoded.
const storeDir = "/nix/store"

// fullSrcPaths turns Srcs' basenames back into full "/nix/store/..."
// paths, sorted the same way Nix's StorePathSet iterates.
func fullSrcPaths(basenames []string) []string {
	out := make([]string, len(basenames))
	for i, b := range basenames {
		out[i] = storeDir + "/" + b
	}
	sort.Strings(out)
	return out
}

// writeQuotedString mirrors printUnquotedString: no escaping, for
// values that can never contain a quote/backslash (store paths,
// output names, method/hash strings).
func writeQuotedString(s *strings.Builder, str string) {
	s.WriteByte('"')
	s.WriteString(str)
	s.WriteByte('"')
}

func writeQuotedStrings(s *strings.Builder, strs []string) {
	s.WriteByte('[')
	for i, str := range strs {
		if i > 0 {
			s.WriteByte(',')
		}
		writeQuotedString(s, str)
	}
	s.WriteByte(']')
}

// writeEscapedString mirrors printString: backslash-escapes '"', '\\',
// '\n', '\r', '\t', for values that can contain arbitrary bytes
// (builder path, args, env keys/values).
func writeEscapedString(s *strings.Builder, str string) {
	s.WriteByte('"')
	for _, r := range str {
		switch r {
		case '"', '\\':
			s.WriteByte('\\')
			s.WriteRune(r)
		case '\n':
			s.WriteString(`\n`)
		case '\r':
			s.WriteString(`\r`)
		case '\t':
			s.WriteString(`\t`)
		default:
			s.WriteRune(r)
		}
	}
	s.WriteByte('"')
}

func writeEscapedStrings(s *strings.Builder, strs []string) {
	s.WriteByte('[')
	for i, str := range strs {
		if i > 0 {
			s.WriteByte(',')
		}
		writeEscapedString(s, str)
	}
	s.WriteByte(']')
}
