// Package dispatch handles the busybox-style multi-call binary
// dispatch: given argv, decide which shim behavior (compile, link,
// archive, ranlib, passthrough) to run. Also expands @rspfile args
// in-place — some build systems (ninja) hand the compiler an
// @path/to/rspfile pointing at a text file listing the real argv;
// downstream shim logic needs to see the flattened form.
package dispatch

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// Tool is the classified role that a shim should play.
type Tool int

const (
	ToolUnknown Tool = iota
	ToolCC
	ToolGCC
	ToolCXX
	ToolGXX
	ToolAR
	ToolRanlib
	ToolLD
	// ToolObjtool: a tool that rewrites an object in place after
	// compiling it. Reached only when the caller points the build's
	// `objtool=` make variable at our shim.
	ToolObjtool
	// ToolRustc: a rustc crate compile. Unlike the C compilers, one
	// invocation consumes a whole crate — a source file plus every
	// module, `include!` and macro file it reaches — and may emit
	// several artifacts from it.
	ToolRustc
	// ToolObjcopy: a generic object rewrite
	// (scripts/Makefile.lib cmd_objcopy), used for symbol prefixing
	// and section stripping.
	ToolObjcopy
)

// Basename returns the argv[0] name we advertise to the sandbox.
// Nix's gcc-wrapper dispatches by the invocation name so `cc` and
// `g++` behave differently for the same underlying binary.
func (t Tool) Basename() string {
	switch t {
	case ToolCC:
		return "cc"
	case ToolGCC:
		return "gcc"
	case ToolCXX:
		return "c++"
	case ToolGXX:
		return "g++"
	case ToolAR:
		return "ar"
	case ToolRanlib:
		return "ranlib"
	case ToolLD:
		return "ld"
	case ToolObjtool:
		return "objtool"
	case ToolObjcopy:
		return "objcopy"
	case ToolRustc:
		return "rustc"
	}
	return ""
}

// FromArgv0 classifies argv[0] (usually a symlink name like "cc"). We
// look at the file basename, not the target of the symlink.
// FromArgv0 classifies the tool role from the name we were invoked as.
//
// Beyond the six canonical names, real build systems invoke compilers
// under target-triple and version decorations:
//
//	x86_64-unknown-linux-gnu-gcc   (cross / explicit-triple toolchains)
//	gcc-15, clang-18               (versioned, Debian/Fedora style)
//	x86_64-linux-gnu-g++-14        (both at once)
//	llvm-ar, x86_64-…-ranlib       (binutils equivalents)
//
// A name we fail to classify returns ToolUnknown, and the shim then has
// no role to play — acceleration silently never engages for that tool.
// No error, no drv, nothing in the log: the build just quietly runs
// unaccelerated. That is why this matcher is generous.
//
// Widening the INPUT side cannot change derivation content: every
// accepted spelling maps onto one of the six roles, and Tool.Basename()
// — which is what the drv's compile command invokes — returns one of the
// same six canonical strings it always did. `gcc-15` is dispatched as
// ToolGCC and the drv still says "gcc", so the sandbox resolves the
// pinned compiler rather than the caller's versioned one. That is
// deliberate: the drv must name a tool that exists inside it.
//
// clang/clang++ map onto the gcc/g++ roles for the same reason. nixgg
// pins its own compiler, so the role only selects C vs C++ mode.
func FromArgv0(argv0 string) Tool {
	base := filepath.Base(argv0)

	// Strip a trailing version decoration: `gcc-15`, `clang++-18`,
	// `g++-14.2`. Only digits and dots, so a real name containing a
	// dash (`x86_64-linux-gnu-gcc`) is untouched here.
	base = stripVersionSuffix(base)

	// Strip a leading target triple: `x86_64-unknown-linux-gnu-gcc`.
	// The tool name is the final dash-separated field; taking only the
	// last field also handles `llvm-ar` and `llvm-ranlib`.
	if i := strings.LastIndexByte(base, '-'); i >= 0 && i+1 < len(base) {
		base = base[i+1:]
	}

	switch base {
	case "cc":
		return ToolCC
	case "gcc", "clang":
		return ToolGCC
	case "c++", "cxx", "CC":
		return ToolCXX
	case "g++", "clang++":
		return ToolGXX
	case "ar":
		return ToolAR
	case "ranlib":
		return ToolRanlib
	case "ld", "ld.bfd", "ld.gold", "ld.lld":
		return ToolLD
	case "objtool":
		return ToolObjtool
	case "objcopy":
		return ToolObjcopy
	case "rustc":
		return ToolRustc
	}
	return ToolUnknown
}

// stripVersionSuffix removes a trailing `-<digits>[.<digits>…]`, the
// shape package managers use for parallel-installable compilers
// (`gcc-15`, `clang++-18`, `g++-14.2`). Anything else is left alone, so
// a triple like `…-linux-gnu-gcc` keeps its final field intact.
func stripVersionSuffix(base string) string {
	i := strings.LastIndexByte(base, '-')
	if i < 0 || i+1 >= len(base) {
		return base
	}
	for _, r := range base[i+1:] {
		if (r < '0' || r > '9') && r != '.' {
			return base
		}
	}
	return base[:i]
}

// Action is what a compiler-family shim decides to do based on argv.
type Action int

const (
	ActionCompile Action = iota // has -c
	ActionLink                  // no -c, produces an executable/shared lib
)

// IsCompile returns true iff argv contains -c (or -E/-S which we treat
// as passthrough, but the shim never gets called for those in practice
// since they're rare and we can leave passthrough logic to each driver).
func IsCompile(argv []string) bool {
	for _, a := range argv {
		if a == "-c" {
			return true
		}
	}
	return false
}

// ExpandRspfiles walks argv and replaces any @rspfile entries with the
// contents of the referenced file, tokenised the way the compiler drivers
// do it: whitespace separates arguments, except inside quotes. Returns a
// fresh slice.
//
// If no @-file is present the input is returned unchanged (no copy).
func ExpandRspfiles(argv []string) []string {
	return expandArgfiles(argv, splitRspLine)
}

// ExpandRustArgfiles is ExpandRspfiles for rustc, whose @-file format is
// NOT the compiler-driver one: each LINE is exactly one argument, taken
// verbatim, with no quote processing at all.
//
// The difference is not cosmetic. A kernel's generated cfg file holds
// lines like
//
//	--cfg=CONFIG_RTC_DRV_CROS_EC="m"
//
// and rustc REQUIRES those quotes — `--cfg key="value"` is its grammar.
// Tokenising the line the compiler-driver way strips them and rustc
// rejects the result outright:
//
//	error: invalid `--cfg` argument: `CONFIG_RTC_DRV_CROS_EC=m`
//
// In the other direction a value containing spaces, legal on one line
// here, would be split into several arguments.
func ExpandRustArgfiles(argv []string) []string {
	return expandArgfiles(argv, func(line string) []string { return []string{line} })
}

func expandArgfiles(argv []string, split func(string) []string) []string {
	// Fast path: no @-arg → no allocation.
	hasRsp := false
	for _, a := range argv {
		if len(a) > 1 && a[0] == '@' {
			// Verify it's a real file — plenty of legitimate flags start
			// with @ (e.g. some assembler features). @filename is only
			// meaningful if the file exists.
			if _, err := os.Stat(a[1:]); err == nil {
				hasRsp = true
				break
			}
		}
	}
	if !hasRsp {
		return argv
	}
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		if len(a) > 1 && a[0] == '@' {
			if body, err := readArgfile(a[1:], split); err == nil {
				out = append(out, body...)
				continue
			}
		}
		out = append(out, a)
	}
	return out
}

func readArgfile(path string, split func(string) []string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		out = append(out, split(sc.Text())...)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// splitRspLine tokenises one response-file line: whitespace separates
// arguments, but not inside quotes.
//
// This used to be `strings.Fields` followed by a per-token unquote, which
// is the wrong order. A quoted argument containing a space — which is the
// only reason to quote in the first place — was split at that space
// before unquoting ever ran, so
//
//	"-DGREETING=hello world"
//
// became the two argv entries `"-DGREETING=hello` and `world"`, each
// carrying a stray quote character. cmake and ninja emit exactly this
// shape for defines with spaces, and ExpandRspfiles runs on every shim
// invocation, so the flag reached the compiler corrupted.
//
// Both quote styles are handled, and a quote can open mid-token
// (`-DX="a b"`) as the drivers allow. A backslash escapes the next
// character inside double quotes only — matching GCC's documented
// behaviour, where single quotes are literal throughout. An unterminated
// quote yields what has accumulated rather than an error: the compiler
// would reject the flag anyway, and failing here would turn a bad flag
// into a nixgg crash.
func splitRspLine(line string) []string {
	var (
		out   []string
		cur   strings.Builder
		inTok bool
		quote byte // 0, '\'' or '"'
	)
	flush := func() {
		if inTok {
			out = append(out, cur.String())
			cur.Reset()
			inTok = false
		}
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' && i+1 < len(line) {
				i++
				cur.WriteByte(line[i])
				continue
			}
			if c == quote {
				quote = 0
				continue
			}
			cur.WriteByte(c)
		case c == '\'' || c == '"':
			// Opening a quoted run. The token exists even if the quotes
			// turn out to be empty, so `-DX=""` yields `-DX=`.
			quote = c
			inTok = true
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			flush()
		default:
			cur.WriteByte(c)
			inTok = true
		}
	}
	flush()
	return out
}
