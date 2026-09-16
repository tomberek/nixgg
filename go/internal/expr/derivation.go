// Package expr constructs Nix derivations from a shared Derivation
// struct, which the native (.nix thunk) and sandbox (JSON drv)
// serializers must render to the same drv hash.
package expr

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Kind identifies which of the three build steps this derivation
// represents.
type Kind int

const (
	KindCompile Kind = iota // builder.nix
	KindLink                // linker.nix
	KindArchive             // archiver.nix
	// KindPartialLink: `ld -r` — combine several objects into one
	// object (not an executable, not an archive). Sandbox mode only:
	// no native-mode helper exists, since the tools that need it only
	// appear in builds that already require the sandbox.
	KindPartialLink
	// KindTransform: rewrite one existing object (objtool, in place)
	// or read one and write another (objcopy). Sandbox mode only, same
	// reasoning as KindPartialLink. Kept separate from the compile
	// that produced the object so the compile stays cacheable
	// independently of the transform's own flags.
	KindTransform
	// KindRustc: one rustc crate compile. Sandbox mode only. Unlike
	// KindCompile this isn't one source in, one object out: a crate
	// is a whole source tree, and one invocation can emit several
	// artifacts from it (an object, the .rmeta dependents resolve
	// `--extern` against) — they share a derivation because rustc
	// produces them from one front-end run.
	KindRustc
)

// Derivation is the intermediate representation both serializers
// consume.
//
// Not every field applies to every Kind:
//   - Compile uses SrcStore, Source, OutName, Flags.
//   - Link uses Inputs, Flags.
//   - Archive uses Inputs, ARFlags.
//
// StoreDeps and WrapperEnv apply to all Kinds.
type Derivation struct {
	Kind Kind

	Name string

	// Nix system (e.g. "x86_64-linux").
	System string

	// Toolchain roots — full /nix/store/… paths.
	Bash, Coreutils string
	Compiler        string // gcc-wrapper root; unused by Archive
	AR              string // binutils root (parent of bin/ar); Archive only

	// PartialLink/Transform-only: absolute /nix/store/…/bin/<tool>
	// path. Not taken from PATH like the compiler — ld is already an
	// absolute-or-PATH-resolved binary the caller named directly, and
	// a Transform tool is often one the wrapped project just built
	// itself (see shim.storeAddTool).
	ToolBin string

	// Rustc-only: absolute /nix/store/…/bin/rustc. Not taken from
	// PATH like the compiler — a Rust build pins its toolchain, and a
	// crate compiled by a different rustc can't be loaded by one that
	// wasn't (rustc rejects mismatched metadata).
	RustcBin string

	// Transform-only: does the tool rewrite its operand in place
	// (objtool: one operand) or read one file and write another
	// (objcopy: `objcopy <flags> <in> <out>`)? In-place needs the
	// input copied out of its read-only store path first; in/out does
	// not.
	ToolInPlace bool

	// Rustc-only: the artifacts this invocation emits, in the
	// caller's order. Each becomes an `--emit=<kind>=$out/<name>`
	// argument.
	Emits []RustEmit

	Tool     string // "cc", "gcc", "c++", "g++"; used by Compile + Link
	SrcStore string // staged src tree
	Source   string // relative to SrcStore, e.g. "src/foo.c"
	OutName  string // e.g. "foo.o"

	// Link + Archive only; Compile leaves this empty.
	Inputs []derivInput

	// ExtraInputs must be mounted into this derivation's sandbox but
	// never appear on the command line.
	//
	// The one producer: a thin archive (`ar T`) consumed by a later
	// link/archive step. Its bytes are path REFERENCES to members, not
	// embedded content, so the consumer's sandbox needs those member
	// paths mounted too — but putting them on argv as an ordinary Input
	// would also make the linker see each symbol twice ("multiple
	// definition of `foo'", confirmed when an earlier version merged
	// them into Inputs).
	ExtraInputs []derivInput

	// Link + Compile: compiler flags. Archive uses ARFlags instead.
	Flags []string

	// GroupInputs wraps the input list in --start-group/--end-group,
	// set when the caller's link line had those brackets; can't be
	// carried in Flags since buildScript emits all flags before all
	// inputs. The re-emitted group spans every input rather than the
	// caller's exact span — widening is safe (ld treats objects inside
	// a group as harmless) but the narrow span can't be expressed once
	// inputs and flags are separated.
	GroupInputs bool

	// WholeArchiveInputs names (basenames, matched against derivInput.Name)
	// the subset of Inputs that fell between --whole-archive and
	// --no-whole-archive on the caller's link line. Unlike GroupInputs
	// this can't be widened to span every input: --whole-archive changes
	// archive member SELECTION, so wrapping an unintended input would
	// force in objects nothing needs (confirmed against Linux Kbuild's
	// vmlinux.o link: a global span put vmlinux.a inside --start-group
	// instead, producing a vmlinux.o with no sections at all).
	WholeArchiveInputs []string

	ARFlags string // Archive-only: `ar` modifier string, e.g. "rcs".

	// Link-only: a store path (or Nix path literal, in native mode)
	// holding local, non-store files the link line references by
	// relative path — e.g. a linker script generated by an unshimmed
	// tool (`perl util/mkdef.pl > libcrypto.ld`, referenced via
	// `-Wl,--version-script=libcrypto.ld`). Rendered as a real
	// derivation input (env["src"]) rather than text embedded in the
	// script, because a large generated linker script plus hundreds of
	// object paths on one link line can exceed the kernel's argv limit
	// (confirmed against openssl's libcrypto.so.3 — "Argument list too
	// long").
	InlineFilesStore string

	// Link-only: an ABSOLUTE-path linker-script reference, as opposed
	// to InlineFilesStore's relative-path case. meson (QEMU) bakes the
	// build tree's absolute path into a generated file's name at
	// configure time (e.g. `-Xlinker
	// --dynamic-list=/build/source/build/plugins/qemu-plugin.symbols`),
	// which `cp -a "$src/." .` can't recreate since Nix's sandbox
	// build root is a fixed "/build". Embedded literally into the
	// script via mkdir+heredoc; safe because every fixture that hits
	// this is a small generated symbol-export list.
	AbsFilePath    string
	AbsFileContent string

	// /nix/store/… roots referenced by Flags or WrapperEnv content that
	// must be mounted in the sandbox.
	StoreDeps []string

	// Nix's gcc-wrapper env (NIX_CFLAGS_COMPILE, NIX_LDFLAGS, etc.).
	WrapperEnv map[string]string
}

// RustEmit is one `--emit=<kind>=$out/<name>` a KindRustc derivation
// writes.
type RustEmit struct {
	Kind string
	Name string
}

// derivInput is the internal per-input record used by Derivation's
// serializers, kept separate from the shim-facing Input type in
// expr.go. inputsFromExpr / inputsFromJSON convert between them.
type derivInput struct {
	// InputKind = "store" (already realised, use the store path as
	// builtins.storePath) or "nix" (a sibling drv/thunk — becomes
	// `import /path/thunk.nix` in native mode and an inputs.drvs entry
	// in JSON mode).
	InputKind string
	// Ref: for "store", a canonical /nix/store/<hash>-<name> root; for
	// "nix", the absolute path to the sibling drv/thunk.
	Ref string
	// Name: the artifact's path relative to that ref's output dir.
	// Usually a basename ("foo.o"), but carries a subdirectory when the
	// producing derivation puts its artifact under one — see
	// inputSubdirFor.
	Name string
	// Crate: KindRustc only. The name the consuming crate refers to
	// this dependency by, not derivable from the filename — a crate's
	// lib name and its `--crate-name` need not match.
	Crate string
}

// inputSubdirFor returns the FHS subdirectory a sibling derivation's
// artifact sits in, inferred from the artifact's own filename since
// producer and consumer are separate derivations built at different
// times and can't otherwise pass the agreement along:
//
//	*-prelink.o  -> bin   (a LINK drv wrote it, see ArtifactSubdir)
//	*.a          -> lib   (an ar drv wrote it to $out/lib/)
//	*.o          -> ""    (compile outputs stay flat)
//	anything else-> bin   (a link drv wrote it to $out/bin/)
//
// Keying on the filename rather than the producing drv's name is
// deliberate: in sandbox mode a sibling reference is a drv path whose
// kind is legible ("…-ar-libfoo.a.drv"), but in native mode it's a
// thunk path (".nixgg/thunks/<hash>.nix") that carries no kind at all.
// Inferring from the drv name would resolve to "lib" in sandbox mode
// and "" in native mode for the same input, breaking the
// same-Derivation-same-hash invariant.
func inputSubdirFor(name string) string { return ArtifactSubdir(name) }

// StoreBasename returns the text after the last '/' in p.
func StoreBasename(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// ArtifactSubdir is inputSubdirFor, exported because native mode's
// PromoteToStore also needs to agree on where the realised artifact
// was written when copying it out of the store into the working tree.
func ArtifactSubdir(name string) string {
	base := StoreBasename(name)
	switch {
	// meson's `prelink: true` static-library convention names a
	// partial-link object "<libname>.a.p/<name>-prelink.o" — a LINK
	// output (shim.Link's `g++ -r`) that happens to end in ".o", same
	// "filename lies about Kind" shape as Kbuild's vmlinux.o. Must be
	// checked before the generic ".o" case below.
	case strings.HasSuffix(base, "-prelink.o"):
		return "bin"
	case strings.HasSuffix(base, ".o"):
		return ""
	case strings.HasSuffix(base, ".a"):
		return "lib"
	case strings.HasSuffix(base, ".rmeta"), strings.HasSuffix(base, ".rlib"):
		// Rust artifacts stay flat for the same reason .o does: a
		// crate's metadata is an intermediate only the next rustc
		// reads, not something buildEnv or `nix profile install`
		// looks for.
		return ""
	}
	return "bin"
}

// outputPlaceholder returns the `builtins.placeholder "out"` value.
func outputPlaceholder() string { return "/" + OutPlaceholderNix32 }

// Native mode's script carries markers for values only Nix knows at
// eval time (an unrealised sibling thunk's CA output placeholder, and
// the toolchain roots the helper receives as its own arguments);
// nix/resolve-script.nix substitutes them. Store context survives
// replaceStrings, so dependency edges stay intact.
//
// The tag is per-script rather than fixed, because markers are just
// text and a flag spelling one (`-DAT=@NIXGG_COMPILER@`) would get
// substituted too.
const markerTagBase = "NIXGG"

// markerTag returns a tag whose markers cannot collide with anything
// already in `body`. Deterministic: same body → same tag, since the
// tag ends up in the thunk text and thus in its hash.
func markerTag(body string) string {
	for n := 0; ; n++ {
		tag := markerTagBase
		if n > 0 {
			tag = markerTagBase + strconv.Itoa(n)
		}
		if !strings.Contains(body, "@"+tag+"_") {
			return tag
		}
	}
}

// Marker spellings; must match nix/resolve-script.nix's reconstruction.
func coreutilsMarker(tag string) string { return "@" + tag + "_COREUTILS@" }
func compilerMarker(tag string) string  { return "@" + tag + "_COMPILER@" }
func inputMarker(tag string, i int) string {
	return "@" + tag + "_INPUT" + strconv.Itoa(i) + "@"
}

// script returns the bash `-c` body with every store path resolved,
// baked directly into sandbox mode's JSON drv.
func (d *Derivation) script() string {
	return d.buildScript("", d.Coreutils, d.compilerOrAR())
}

// scriptTemplate returns the same script with markers where native
// mode cannot know the value yet, plus the tag those markers use.
func (d *Derivation) scriptTemplate() (template, tag string) {
	tag = markerTag(d.script())
	return d.buildScript(tag, coreutilsMarker(tag), compilerMarker(tag)), tag
}

// outSubdir is the FHS directory inside $out that this Kind's
// artifact belongs in ("" for none, since a per-TU .o is not a
// package and has no FHS home).
func (d *Derivation) outSubdir() string {
	switch d.Kind {
	case KindLink:
		return "bin"
	case KindArchive:
		return "lib"
	}
	return ""
}

// outPath renders the artifact's destination inside the builder, e.g.
// `$out/bin/llc`.
func (d *Derivation) outPath() string {
	if sub := d.outSubdir(); sub != "" {
		return "$out/" + sub + "/" + d.OutName
	}
	return "$out/" + d.OutName
}

// outDir renders the directory the builder must create first.
func (d *Derivation) outDir() string {
	if sub := d.outSubdir(); sub != "" {
		return "$out/" + sub
	}
	return "$out"
}

// inlineFilesScript renders shell that copies InlineFilesStore's
// staged directory into the build root before the link command runs.
func (d *Derivation) inlineFilesScript() string {
	if d.InlineFilesStore == "" {
		return ""
	}
	return "cp -a \"$src/.\" .\n"
}

// absFileScript renders shell that recreates AbsFilePath with
// AbsFileContent's exact bytes before the link command runs.
func (d *Derivation) absFileScript() string {
	if d.AbsFilePath == "" {
		return ""
	}
	dir := d.AbsFilePath[:strings.LastIndexByte(d.AbsFilePath, '/')]
	return fmt.Sprintf("mkdir -p %s\ncat > %s <<'NIXGG_ABS_FILE_EOF'\n%sNIXGG_ABS_FILE_EOF\n",
		shellQuote(dir), shellQuote(d.AbsFilePath), d.AbsFileContent)
}

// compilerOrAR reports which store path provides the tools on PATH:
// Compile and Link use the compiler; Archive uses whatever supplies
// `ar` (compilerRoot in native mode, AR in sandbox mode).
//
// Transform has no "compiler" of its own — d.ToolBin is the rewriting
// binary (objtool/objcopy), invoked the same way a compiler would be,
// so it rides the same tag/marker plumbing scriptTemplate already
// provides for KindArchive's AR.
func (d *Derivation) compilerOrAR() string {
	switch d.Kind {
	case KindArchive:
		return d.AR
	case KindTransform, KindPartialLink:
		return d.ToolBin
	}
	return d.Compiler
}

// isRawLinker reports whether d.Tool is a linker binary invoked
// directly (ld) rather than a compiler driver (cc/gcc/clang). The two
// accept different group-bracket spellings: `-Wl,--start-group` is a
// driver convention; a raw ld invocation rejects `-Wl,...` outright
// ("unrecognized option"), as confirmed against Linux Kbuild's
// vmlinux.o link.
func (d *Derivation) isRawLinker() bool {
	return d.Tool == "ld"
}

// buildScript is the single source of the shell body that both wire
// formats must agree on.
//
// tag == "" means resolve everything (sandbox mode). A non-empty tag
// means emit input markers with that tag (native mode); coreutils and
// compiler are then marker text too.
func (d *Derivation) buildScript(tag, coreutils, compiler string) string {
	pathPrefix := fmt.Sprintf(`export PATH="%s/bin:%s/bin"`, coreutils, compiler)

	wholeArchive := make(map[string]bool, len(d.WholeArchiveInputs))
	for _, n := range d.WholeArchiveInputs {
		wholeArchive[StoreBasename(n)] = true
	}
	inputs := func() string {
		parts := make([]string, 0, len(d.Inputs))
		for i, in := range d.Inputs {
			var part string
			switch {
			case tag != "":
				part = "'" + inputMarker(tag, i) + "'"
			case in.InputKind == "store":
				ref := in.Ref
				if !strings.HasPrefix(ref, "/nix/store/") {
					ref = "/nix/store/" + ref
				}
				part = fmt.Sprintf("'%s/%s'", ref, in.Name)
			case in.InputKind == "nix":
				name := in.Name
				if sub := inputSubdirFor(in.Name); sub != "" {
					name = sub + "/" + name
				}
				part = fmt.Sprintf("'%s/%s'", caOutputPlaceholder(in.Ref, "out"), name)
			}
			if part == "" {
				continue
			}
			if wholeArchive[StoreBasename(in.Name)] {
				part = "--whole-archive " + part + " --no-whole-archive"
			}
			parts = append(parts, part)
		}
		return strings.Join(parts, " ")
	}

	switch d.Kind {
	case KindCompile:
		return fmt.Sprintf(
			`set -euo pipefail
%s
mkdir -p "$out"
cd "$src"
"%s" %s -c "$source" -o "$out/$outName"
`, pathPrefix, d.Tool, shellQuoteFlags(d.Flags))
	case KindLink:
		// Split flags into non-`-l` and `-l<name>`: a single-pass ld
		// resolves libraries against object files mentioned BEFORE
		// them, so inputs go between the two groups, putting `-lm`/
		// `-lc` last. With no `-l` flags, fall through to the plain
		// flags-then-inputs layout so drv content stays byte-identical
		// to before this split existed.
		var lflags, nonLflags []string
		for _, f := range d.Flags {
			if strings.HasPrefix(f, "-l") && len(f) > 2 {
				lflags = append(lflags, f)
			} else {
				nonLflags = append(nonLflags, f)
			}
		}
		inputList := inputs()
		if d.GroupInputs && inputList != "" {
			if d.isRawLinker() {
				inputList = "--start-group " + inputList + " --end-group"
			} else {
				inputList = "-Wl,--start-group " + inputList + " -Wl,--end-group"
			}
		}
		if len(lflags) == 0 {
			return fmt.Sprintf(
				`set -euo pipefail
%s
mkdir -p "%s"
%s%s"%s" %s %s -o "%s"
`, pathPrefix, d.outDir(), d.absFileScript(), d.inlineFilesScript(), d.Tool, shellQuoteFlags(d.Flags), inputList, d.outPath())
		}
		return fmt.Sprintf(
			`set -euo pipefail
%s
mkdir -p "%s"
%s%s"%s" %s %s %s -o "%s"
`, pathPrefix, d.outDir(), d.absFileScript(), d.inlineFilesScript(), d.Tool, shellQuoteFlags(nonLflags), inputList, shellQuoteFlags(lflags), d.outPath())
	case KindArchive:
		// `ar` is taken from PATH (set above) and `D` is prepended to
		// arFlags for a deterministic archive.
		return fmt.Sprintf(
			`set -euo pipefail
%s
mkdir -p "%s"
ar D%s "%s" %s
`, pathPrefix, d.outDir(), d.ARFlags, d.outPath(), inputs())
	case KindPartialLink:
		// `-r` comes from the caller's own Flags, not added here — it's
		// what identified this as a partial link in the first place.
		// ToolBin arrives through the compiler parameter (see
		// compilerOrAR), same reasoning as KindTransform: native mode
		// needs a marker here too, so the emitted script carries a
		// real dependency edge on ld's own store path via
		// resolve-script.nix, not a bare unmarked literal.
		return fmt.Sprintf(
			`set -euo pipefail
export PATH="%s/bin"
mkdir -p "%s"
"%s" %s -o "%s" %s
`, coreutils, d.outDir(), compiler, shellQuoteFlags(d.Flags), d.outPath(), inputs())
	case KindRustc:
		// One front-end run, several artifacts. `--out-dir` is set even
		// though every emit is named explicitly: rustc drops temporaries
		// in the CURRENT directory otherwise, and the current directory
		// here is the read-only staged source tree.
		//
		// Every input contributes a `-L` search path, and those the
		// caller named also get an explicit `--extern <crate>=<path>`
		// — the explicit extern is what turns a filesystem search into
		// a derivation input (the caller's bare `--extern <crate>`
		// searches `-L` dirs that are the build tree, absent in the
		// sandbox). The search paths alone cover the crates the caller
		// never named: a crate's own dependencies travel in its
		// metadata, not the command line, so a kernel driver naming two
		// externs can still need rustc to load ten crates through -L.
		var parts []string
		seenDir := map[string]bool{}
		for _, in := range d.Inputs {
			ref := in.Ref
			name := in.Name
			dir := ref
			if in.InputKind == "store" {
				if !strings.HasPrefix(ref, "/nix/store/") {
					ref = "/nix/store/" + ref
					dir = ref
				}
			} else {
				ref = caOutputPlaceholder(in.Ref, "out")
				dir = ref
				// Only a sibling drv places its artifact under an FHS
				// subdir; a store input sits flat. Applying the subdir
				// to both would send the search path into a bin/ that
				// doesn't exist.
				if sub := inputSubdirFor(in.Name); sub != "" {
					name = sub + "/" + name
					dir = ref + "/" + sub
				}
			}
			// Plain `-L`, not `-L dependency=`: the narrower kind only
			// joins the search for a crate's own dependencies, and
			// rustc looks for `core` (which a kernel compiles itself
			// and passes here like any other crate) in the
			// unrestricted path — `dependency=` dies on "can't find
			// crate for `core`".
			if !seenDir[dir] {
				seenDir[dir] = true
				parts = append(parts, fmt.Sprintf("-L '%s'", dir))
			}
			if in.Crate != "" {
				parts = append(parts, fmt.Sprintf("--extern '%s=%s/%s'", in.Crate, ref, name))
			}
		}
		for _, e := range d.Emits {
			// Double quotes, not single: $out has to expand. Safe
			// because both fields are a rustc emit kind and a basename
			// this shim chose, never caller text.
			parts = append(parts, fmt.Sprintf(`"--emit=%s=$out/%s"`, e.Kind, e.Name))
		}
		return fmt.Sprintf(
			`set -euo pipefail
export PATH="%s/bin"
mkdir -p "$out"
cd "$src"
"%s" %s %s --out-dir "$out" "$source"
`, d.Coreutils, d.RustcBin, shellQuoteFlags(d.Flags), strings.Join(parts, " "))
	case KindTransform:
		// PATH carries coreutils only — no compiler involved, and the
		// transform binary is invoked by absolute path. That path
		// comes through the `compiler` parameter (see compilerOrAR),
		// not d.ToolBin directly, so native mode gets a marker here
		// too and the emitted script carries a real dependency edge
		// on the tool's store path via resolve-script.nix, same as
		// every other Kind's toolchain reference.
		if !d.ToolInPlace {
			// `tool <flags> <in> <out>` — objcopy's shape. Nothing to
			// copy: the tool reads the store path and writes $out.
			return fmt.Sprintf(
				`set -euo pipefail
export PATH="%s/bin"
mkdir -p "%s"
"%s" %s %s "%s"
`, coreutils, d.outDir(), compiler, shellQuoteFlags(d.Flags), inputs(), d.outPath())
		}
		// In-place tools (objtool) rewrite their operand, so the input
		// has to be copied out of its read-only store path first;
		// chmod because store paths arrive without write permission.
		return fmt.Sprintf(
			`set -euo pipefail
export PATH="%s/bin"
mkdir -p "%s"
cp %s "%s"
chmod u+w "%s"
"%s" %s "%s"
`, coreutils, d.outDir(), inputs(), d.outPath(), d.outPath(),
			compiler, shellQuoteFlags(d.Flags), d.outPath())
	}
	return ""
}

// ToNix serialises this Derivation as an `import <helper>.nix { … }`
// expression written to .nixgg/thunks/<id>.nix. helpers must be the
// /nix/store/…-nixgg-nix root containing builder.nix/linker.nix/
// archiver.nix.
func (d *Derivation) ToNix(helpers string) string {
	tmpl, tag := d.scriptTemplate()
	var b strings.Builder
	switch d.Kind {
	case KindCompile:
		fmt.Fprintf(&b, "import %s/builder.nix {\n", helpers)
		fmt.Fprintf(&b, "  srcTree        = %s;\n", d.SrcStore)
		fmt.Fprintf(&b, "  source         = %q;\n", d.Source)
		fmt.Fprintf(&b, "  outName        = %q;\n", d.OutName)
		fmt.Fprintf(&b, "  scriptTemplate = %s;\n", nixIndentedStringLiteral(tmpl))
		fmt.Fprintf(&b, "  markerTag      = %q;\n", tag)
		fmt.Fprintf(&b, "  storeDepsJSON  = ''%s'';\n", jsonArrayIndented(d.StoreDeps))
		fmt.Fprintf(&b, "  wrapperEnvJSON = ''%s'';\n", jsonObjectSorted(d.WrapperEnv))
	case KindLink:
		fmt.Fprintf(&b, "import %s/linker.nix {\n", helpers)
		fmt.Fprintf(&b, "  outName        = %q;\n", d.OutName)
		if d.Name != "" {
			fmt.Fprintf(&b, "  name           = %q;\n", d.Name)
		}
		fmt.Fprintf(&b, "  inputs         = %s;\n", derivInputsList(d.Inputs))
		if len(d.ExtraInputs) > 0 {
			fmt.Fprintf(&b, "  extraInputs    = %s;\n", derivInputsList(d.ExtraInputs))
		}
		fmt.Fprintf(&b, "  scriptTemplate = %s;\n", nixIndentedStringLiteral(tmpl))
		fmt.Fprintf(&b, "  markerTag      = %q;\n", tag)
		fmt.Fprintf(&b, "  storeDepsJSON  = ''%s'';\n", jsonArrayIndented(d.StoreDeps))
		fmt.Fprintf(&b, "  wrapperEnvJSON = ''%s'';\n", jsonObjectSorted(d.WrapperEnv))
		if d.InlineFilesStore != "" {
			fmt.Fprintf(&b, "  srcTree        = %s;\n", d.InlineFilesStore)
		}
	case KindArchive:
		fmt.Fprintf(&b, "import %s/archiver.nix {\n", helpers)
		fmt.Fprintf(&b, "  outName        = %q;\n", d.OutName)
		if d.Name != "" {
			fmt.Fprintf(&b, "  name           = %q;\n", d.Name)
		}
		fmt.Fprintf(&b, "  inputs         = %s;\n", derivInputsList(d.Inputs))
		if len(d.ExtraInputs) > 0 {
			fmt.Fprintf(&b, "  extraInputs    = %s;\n", derivInputsList(d.ExtraInputs))
		}
		fmt.Fprintf(&b, "  scriptTemplate = %s;\n", nixIndentedStringLiteral(tmpl))
		fmt.Fprintf(&b, "  markerTag      = %q;\n", tag)
		fmt.Fprintf(&b, "  storeDepsJSON  = ''%s'';\n", jsonArrayIndented(d.StoreDeps))
		fmt.Fprintf(&b, "  wrapperEnvJSON = ''%s'';\n", jsonObjectSorted(d.WrapperEnv))
	case KindTransform:
		fmt.Fprintf(&b, "import %s/transform.nix {\n", helpers)
		fmt.Fprintf(&b, "  outName        = %q;\n", d.OutName)
		fmt.Fprintf(&b, "  name           = %q;\n", d.Name)
		// toolBin is always an already-realised store path in native
		// mode too — objcopy's tool already is one (realBinutil), and
		// objtool's self-built binary is staged there first via
		// nix/stage-tool.nix (see go/internal/shim/objtool.go's
		// stageToolForNative): an ordinary Nix build whose own
		// reference scan records the binary's real RPATH deps, the
		// same set sandbox mode's `nix store add --scan` would.
		// Rendered as a quoted string, same as any other store-path
		// string field (transform.nix wraps it with pureStorePath).
		fmt.Fprintf(&b, "  toolBin        = %q;\n", d.ToolBin)
		fmt.Fprintf(&b, "  inputs         = %s;\n", derivInputsList(d.Inputs))
		fmt.Fprintf(&b, "  scriptTemplate = %s;\n", nixIndentedStringLiteral(tmpl))
		fmt.Fprintf(&b, "  markerTag      = %q;\n", tag)
		fmt.Fprintf(&b, "  storeDepsJSON  = ''%s'';\n", jsonArrayIndented(d.StoreDeps))
		fmt.Fprintf(&b, "  wrapperEnvJSON = ''%s'';\n", jsonObjectSorted(d.WrapperEnv))
	case KindPartialLink:
		fmt.Fprintf(&b, "import %s/partiallink.nix {\n", helpers)
		fmt.Fprintf(&b, "  outName        = %q;\n", d.OutName)
		fmt.Fprintf(&b, "  name           = %q;\n", d.Name)
		// ToolBin (ld) is an already-realised store path in native
		// mode too — the caller's own realLDFor resolution, same shape
		// objcopy's ToolBin already has. Rendered as a quoted string,
		// wrapped with pureStorePath by partiallink.nix.
		fmt.Fprintf(&b, "  toolBin        = %q;\n", d.ToolBin)
		fmt.Fprintf(&b, "  inputs         = %s;\n", derivInputsList(d.Inputs))
		fmt.Fprintf(&b, "  scriptTemplate = %s;\n", nixIndentedStringLiteral(tmpl))
		fmt.Fprintf(&b, "  markerTag      = %q;\n", tag)
		fmt.Fprintf(&b, "  storeDepsJSON  = ''%s'';\n", jsonArrayIndented(d.StoreDeps))
		fmt.Fprintf(&b, "  wrapperEnvJSON = ''%s'';\n", jsonObjectSorted(d.WrapperEnv))
	}
	b.WriteString("}\n")
	return b.String()
}

// derivInputsList renders `inputs = [ ... ]` for linker/archiver
// helpers.
func derivInputsList(inputs []derivInput) string {
	if len(inputs) == 0 {
		return "[ ]"
	}
	var b strings.Builder
	b.WriteString("[ ")
	for _, in := range inputs {
		var drv string
		switch in.InputKind {
		case "store":
			ref := in.Ref
			if strings.HasPrefix(ref, "/nix/store/") {
				drv = fmt.Sprintf("builtins.storePath %q", ref)
			} else {
				drv = fmt.Sprintf("builtins.storePath \"/nix/store/%s\"", ref)
			}
		case "nix":
			drv = "import " + in.Ref
		}
		name := in.Name
		if in.InputKind == "nix" {
			if sub := inputSubdirFor(in.Name); sub != "" {
				name = sub + "/" + name
			}
		}
		fmt.Fprintf(&b, "{ drv = %s; name = %q; } ", drv, name)
	}
	b.WriteString("]")
	return b.String()
}

// jsonObjectSorted renders a map as a sorted-key JSON object,
// byte-deterministic.
func jsonObjectSorted(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kj, _ := json.Marshal(k)
		vj, _ := json.Marshal(m[k])
		b.Write(kj)
		b.WriteByte(':')
		b.Write(vj)
	}
	b.WriteByte('}')
	return b.String()
}

// toJSON serialises this Derivation for `nix derivation add`. Inputs
// split into inputs.drvs (Kind=="nix") / inputs.srcs (Kind=="store");
// ExtraInputs fold into the same maps so Nix mounts them without them
// appearing in d.script()'s argv.
func (d *Derivation) toJSON(extraSrcs []string) JSONDrv {
	drvs := map[string]JSONDrvRef{}
	srcs := append([]string{}, extraSrcs...)
	seenSrc := map[string]bool{}
	for _, s := range srcs {
		seenSrc[s] = true
	}
	addInput := func(in derivInput) {
		switch in.InputKind {
		case "nix":
			refKey := StoreBasename(in.Ref)
			ref := drvs[refKey]
			ref.Outputs = appendUnique(ref.Outputs, "out")
			if ref.DynamicOutputs == nil {
				ref.DynamicOutputs = map[string]any{}
			}
			drvs[refKey] = ref
		case "store":
			base := StoreBasename(in.Ref)
			if !seenSrc[base] {
				srcs = append(srcs, base)
				seenSrc[base] = true
			}
		}
	}
	for _, in := range d.Inputs {
		addInput(in)
	}
	for _, in := range d.ExtraInputs {
		addInput(in)
	}
	for _, sd := range d.StoreDeps {
		base := StoreBasename(sd)
		if !seenSrc[base] {
			srcs = append(srcs, base)
			seenSrc[base] = true
		}
	}
	return JSONDrv{
		Name:    d.Name,
		System:  d.System,
		Builder: d.Bash + "/bin/bash",
		Args:    []string{"-c", d.script()},
		Env:     d.envDict(),
		Inputs: JSONDrvInputs{
			Drvs: drvs,
			Srcs: srcs,
		},
		Outputs: map[string]JSONOut{
			"out": {Method: "nar", HashAlgo: "sha256"},
		},
		Version: 4,
	}
}

// envDict is the env-var dict every derivation gets. Both serializers
// use it as-is — that's what pins them to the same hash.
func (d *Derivation) envDict() map[string]string {
	env := map[string]string{
		"out":            outputPlaceholder(),
		"name":           d.Name,
		"system":         d.System,
		"builder":        d.Bash + "/bin/bash",
		"outputHashAlgo": "sha256",
		"outputHashMode": "nar",
		"_storeDeps":     strings.Join(d.StoreDeps, ":"),
	}
	// Both sides must agree the `_extraInputs` key EXISTS even when
	// empty, mirroring linker.nix/archiver.nix's unconditional
	// `_extraInputs` — otherwise JSON-mode and native-mode envs hash
	// differently despite identical content.
	switch d.Kind {
	case KindLink, KindArchive:
		parts := make([]string, 0, len(d.ExtraInputs))
		for _, in := range d.ExtraInputs {
			name := in.Name
			if in.InputKind == "nix" {
				if sub := inputSubdirFor(in.Name); sub != "" {
					name = sub + "/" + name
				}
				parts = append(parts, caOutputPlaceholder(in.Ref, "out")+"/"+name)
			} else {
				ref := in.Ref
				if !strings.HasPrefix(ref, "/nix/store/") {
					ref = "/nix/store/" + ref
				}
				parts = append(parts, ref+"/"+name)
			}
		}
		env["_extraInputs"] = strings.Join(parts, ":")
	}
	switch {
	case d.Kind == KindCompile:
		env["src"] = d.SrcStore
		env["source"] = d.Source
		env["outName"] = d.OutName
	case d.Kind == KindRustc:
		// Same staged-tree slot as Compile, minus outName: a crate's
		// artifacts are named by the --emit flags, not one output.
		env["src"] = d.SrcStore
		env["source"] = d.Source
	case d.Kind == KindLink && d.InlineFilesStore != "":
		env["src"] = d.InlineFilesStore
	}
	for k, v := range d.WrapperEnv {
		env[k] = v
	}
	return env
}

// nixIndentedStringLiteral renders `s` as a Nix indented-string
// literal, escaping the two constructs that aren't literal inside
// that form: a doubled single-quote (closes the string) and `${`
// (opens interpolation). The quote escape must run first, or it would
// also rewrite the quotes introduced by escaping `${`.
func nixIndentedStringLiteral(s string) string {
	e := strings.ReplaceAll(s, "''", "'''")
	e = strings.ReplaceAll(e, "${", "''${")
	return "''" + e + "''"
}
