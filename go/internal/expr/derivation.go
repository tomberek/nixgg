// Package expr constructs Nix derivations. A single Derivation struct
// holds every field that both wire formats — the `.nix` thunk file
// (native mode, via `nix-instantiate`) and the JSON drv description
// (sandbox mode, via `nix derivation add`) — need to agree on, so
// adding a field can't accidentally land in only one format.
//
// Invariant: same Derivation -> same drv hash, regardless of which
// serializer produces the wire bytes. Enforced externally by
// tests/drv-equivalence.sh (runs both paths, compares drv-store-paths).
package expr

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Kind identifies which of the three build steps this derivation
// represents. Different Kinds emit different shell scripts and
// slightly different helper Nix files, but share the same env-dict
// shape.
type Kind int

const (
	KindCompile Kind = iota // per-TU compile: builder.nix
	KindLink                // linker step: linker.nix
	KindArchive             // ar step: archiver.nix
)

// Derivation is the intermediate representation both serializers
// consume. Fields mirror the union of what builder.nix / linker.nix /
// archiver.nix accept plus the fields Nix itself requires (name,
// system, builder, args, env, inputs, outputs).
//
// Not every field applies to every Kind:
//   - Compile uses SrcStore, Source, OutName, Flags.
//   - Link uses Inputs, Flags.
//   - Archive uses Inputs, ARFlags.
//
// StoreDeps and WrapperEnv apply to all Kinds.
type Derivation struct {
	Kind Kind

	// The name Nix assigns to the derivation's .drv basename. For
	// compile this is "tu-<outName>"; link "bin-<outName>"; archive
	// "ar-<outName>".
	Name string

	// Nix system (e.g. "x86_64-linux").
	System string

	// Toolchain roots — full /nix/store/… paths. All three kinds use
	// bash (builder) + coreutils (PATH). Compile + link use the
	// compiler; archive uses the binutils dir (via AR).
	Bash, Coreutils string
	Compiler        string // gcc-wrapper root; unused by Archive
	AR              string // binutils root (parent of bin/ar); Archive only

	// Compile-specific.
	Tool     string // "cc", "gcc", "c++", "g++"; used by Compile + Link
	SrcStore string // /nix/store/…-<tuID>: the staged src tree
	Source   string // relative to SrcStore, e.g. "src/foo.c"
	OutName  string // e.g. "foo.o"

	// Link + Archive: inputs (either sibling drvs or already-realised
	// store paths). Compile leaves this empty.
	Inputs []derivInput

	// ExtraInputs are dependency-only: Nix must mount them into this
	// derivation's sandbox (same rendering path as Inputs, into
	// linker.nix/archiver.nix's `_extraInputs` env attr / envDict's
	// mirrored JSON-mode key), but they never appear on the actual
	// command line.
	//
	// The one producer: a thin archive (`ar T`) consumed by a later
	// link/archive step. Its bytes are path REFERENCES to members, not
	// embedded content, so the consumer's sandbox needs those member
	// paths mounted too — but putting them on argv as an ordinary Input
	// would also make the linker see each symbol twice ("multiple
	// definition of `foo'", confirmed when an earlier version merged
	// them into Inputs). ExtraInputs lets "must be mounted" and "must
	// appear in argv" be answered independently per input.
	ExtraInputs []derivInput

	// Link + Compile: compiler flags. Archive uses ARFlags instead.
	Flags []string

	// GroupInputs wraps the input list in --start-group/--end-group.
	// Set when the caller's link line had those brackets; they can't be
	// carried in Flags because they're positional (bracket whatever's
	// BETWEEN them) and buildScript emits all flags before all inputs.
	//
	// The re-emitted group spans every input rather than the caller's
	// exact span — widening is safe (verified against ld: objects
	// inside a group are harmless) and the narrow span can't be
	// expressed once inputs and flags are separated.
	GroupInputs bool

	// WholeArchiveInputs names (basenames, matched against derivInput.Name)
	// the subset of Inputs that fell between --whole-archive and
	// --no-whole-archive on the caller's link line. Unlike GroupInputs,
	// this can't be widened to span every input: --whole-archive changes
	// archive member SELECTION (forces every member in vs. only members
	// something else references), so wrapping an unintended input would
	// force in objects nothing needs. Confirmed against Linux Kbuild's
	// vmlinux.o link (cmd_ld_vmlinux.o: `--whole-archive vmlinux.a
	// --no-whole-archive --start-group $(KBUILD_VMLINUX_LIBS)
	// --end-group`) — treating it as one global span put vmlinux.a
	// inside --start-group's span instead, producing a vmlinux.o with no
	// sections at all.
	//
	// A name list rather than a boolean per Input, because
	// --whole-archive only applies to archives (.a) and Kbuild's recipe
	// wraps exactly one of several inputs, not all.
	WholeArchiveInputs []string

	// Archive-only: `ar` modifier string (e.g. "rcs").
	ARFlags string

	// Link-only: a store path (or Nix path literal, in native mode)
	// holding local, non-store, non-stub files the link line
	// references by relative path — a linker script generated moments
	// earlier by an unshimmed tool (e.g. openssl's `perl
	// util/mkdef.pl > libcrypto.ld`, referenced via
	// `-Wl,--version-script=libcrypto.ld`). The link shim reads the
	// content at classify time, stages it via stage.ContentFiles, and
	// passes the resulting directory here — same shape as Compile's
	// SrcStore, just for KindLink. A real derivation input (env["src"],
	// copied in before the link command runs), NOT text embedded in the
	// script: a large generated linker script plus hundreds of object
	// paths on one link line can exceed the kernel's argv limit if baked
	// directly into the script body (confirmed against openssl's
	// libcrypto.so.3 — "Argument list too long").
	InlineFilesStore string

	// Link-only: an ABSOLUTE-path linker-script reference (as opposed
	// to InlineFilesStore's relative-path case). meson (QEMU) bakes the
	// build tree's absolute path into a generated file's name at
	// configure time — e.g. `-Xlinker
	// --dynamic-list=/build/source/build/plugins/qemu-plugin.symbols`
	// — so `cp -a "$src/." .` (InlineFilesStore's mechanism, relative to
	// the link's own cwd) can't recreate it. Nix's sandbox always mounts
	// the build root at a fixed "/build", so the file's content can
	// just be written to that exact absolute path via a `mkdir -p`+
	// heredoc fragment prepended to the script. Safe to embed literally
	// (unlike InlineFilesStore): every fixture that hits this is a small
	// generated symbol-export list, not a large tree or hundreds of paths.
	AbsFilePath    string
	AbsFileContent string

	// /nix/store/… roots referenced by Flags or WrapperEnv content;
	// must be mounted in the sandbox. Serialized as _storeDeps env var
	// (colon-joined) and threaded into inputs.srcs (JSON mode) /
	// storeDepsJSON (native mode).
	StoreDeps []string

	// Nix's gcc-wrapper env (NIX_CFLAGS_COMPILE, NIX_LDFLAGS, etc.).
	// Preserved verbatim in the env dict.
	WrapperEnv map[string]string
}

// derivInput is the internal per-input record used by Derivation's
// serializers. Kept separate from the shim-facing Input type in
// expr.go (which uses Kind="store"/"nix" via a different field name
// for historical reasons). inputsFromExpr / inputsFromJSON convert.
type derivInput struct {
	// InputKind = "store" (already realised, use the store path as
	// builtins.storePath) or "nix" (a sibling drv/thunk we
	// reference — becomes `import /path/thunk.nix` in native mode
	// and inputs.drvs entry in JSON mode).
	InputKind string
	// Ref: for "store", a canonical /nix/store/<hash>-<name> root;
	// for "nix", the absolute path to the sibling drv/thunk.
	Ref string
	// Name: the path of the artifact relative to that ref's output dir.
	// Usually a basename ("foo.o"), but carries a subdirectory when the
	// producing derivation puts its artifact under one — see
	// inputSubdirFor.
	Name string
}

// inputSubdirFor returns the FHS subdirectory a sibling derivation's
// artifact sits in, inferred from the artifact's own filename.
//
// Producer and consumer are separate derivations built at different
// times, so the agreement between them cannot be passed along — it has
// to be derivable identically on both sides. The filename is the only
// thing both sides reliably share:
//
//	*-prelink.o  -> bin   (a LINK drv wrote it, see ArtifactSubdir)
//	*.a          -> lib   (an ar drv wrote it to $out/lib/)
//	*.o          -> ""    (compile outputs stay flat)
//	anything else-> bin   (a link drv wrote it to $out/bin/)
//
// Keying on the filename rather than on the producing drv's NAME is
// deliberate and load-bearing. In sandbox mode a sibling reference is a
// drv path ("…-ar-libfoo.a.drv") whose kind is legible; in native mode it
// is a thunk path (".nixgg/thunks/<hash>.nix") that carries no kind at
// all. Inferring from the drv name would therefore resolve to "lib" in
// sandbox mode and "" in native mode for the same input — the two would
// emit different scripts and produce different drv hashes, breaking the
// one invariant this project rests on.
func inputSubdirFor(name string) string { return ArtifactSubdir(name) }

// StoreBasename returns the text after the last '/' in p — the
// hash+name basename `nix derivation add` wants for an inputs.srcs
// entry, or a drv's own store-path basename.
func StoreBasename(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// ArtifactSubdir is inputSubdirFor, exported for the one caller outside
// this package that has to agree with it: native mode's PromoteToStore
// copies the realised artifact out of the store into the working tree, so
// it must look where the emitted script actually wrote it.
//
// That call site is not covered by tests/drv-equivalence.sh, which
// compares drv HASHES and never reads a realised output. Missing it left
// `make` in native mode failing with "expected …/bin-hello/hello to
// exist" after outputs moved under bin/ — the drvs were correct and
// identical across modes, but nothing fetched the result.
func ArtifactSubdir(name string) string {
	base := StoreBasename(name)
	switch {
	// meson's `prelink: true` static-library convention names a
	// partial-link object "<libname>.a.p/<name>-prelink.o" — a LINK
	// output (shim.Link's `g++ -r`) that happens to end in ".o", same
	// "filename lies about Kind" shape as Kbuild's vmlinux.o. Must be
	// checked before the generic ".o" case below, or archiving this
	// output fails looking for it flat instead of under bin/.
	case strings.HasSuffix(base, "-prelink.o"):
		return "bin"
	case strings.HasSuffix(base, ".o"):
		return ""
	case strings.HasSuffix(base, ".a"):
		return "lib"
	}
	return "bin"
}

// outputPlaceholder returns the `builtins.placeholder "out"` value —
// what every derivation's `$out` env var interpolates to at build
// time. Same digest for every CA derivation with a single `out`
// output; caching for one call site is fine.
func outputPlaceholder() string { return "/" + OutPlaceholderNix32 }

// Native mode's script carries markers where only Nix knows the value at
// eval time: an input may be an unrealised sibling thunk whose drv hash —
// hence CA output placeholder — doesn't exist yet, and the toolchain roots
// are the helper's own arguments. nix/resolve-script.nix substitutes them;
// store context survives replaceStrings, so dependency edges stay intact.
//
// The tag is per-script, not fixed. Markers are just text, so a flag
// spelling one (`-DAT=@NIXGG_COMPILER@`) would be substituted too.
const markerTagBase = "NIXGG"

// markerTag returns a tag whose markers cannot collide with anything
// already in `body`. Deterministic: same body → same tag, which matters
// because the tag ends up in the thunk text and thus in its hash.
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

// Marker spellings. The helper reconstructs these from the tag it is
// passed, so any change here needs the matching change in
// nix/resolve-script.nix — TestNativeTemplateResolvesToSandboxScript runs
// the real helper and fails if they disagree.
func coreutilsMarker(tag string) string { return "@" + tag + "_COREUTILS@" }
func compilerMarker(tag string) string  { return "@" + tag + "_COMPILER@" }
func inputMarker(tag string, i int) string {
	return "@" + tag + "_INPUT" + strconv.Itoa(i) + "@"
}

// script returns the bash `-c` body with every store path resolved.
// This is what sandbox mode bakes into its JSON drv.
func (d *Derivation) script() string {
	return d.buildScript("", d.Coreutils, d.compilerOrAR())
}

// scriptTemplate returns the same script with markers where native mode
// cannot know the value yet, plus the tag those markers use. The thunk
// passes both to nix/resolve-script.nix.
func (d *Derivation) scriptTemplate() (template, tag string) {
	// Choose the tag against the resolved body: it contains the same flag
	// text as the template, and unlike the template it has no markers of
	// its own to confuse the scan.
	tag = markerTag(d.script())
	return d.buildScript(tag, coreutilsMarker(tag), compilerMarker(tag)), tag
}

// outSubdir is the FHS directory inside $out that this Kind's artifact
// belongs in, "" for none. Nix store paths follow FHS internally
// (executables at bin/, libraries at lib/) — that's what makes a store
// path installable via `nix profile install`/`nix run`/buildEnv/NixOS's
// environment.systemPackages, all of which scan bin/, lib/, share/ etc.
//
// Compile outputs stay flat deliberately: a per-TU .o is not a package
// and has no FHS home.
func (d *Derivation) outSubdir() string {
	switch d.Kind {
	case KindLink:
		return "bin"
	case KindArchive:
		return "lib"
	}
	return ""
}

// The producer side (outSubdir, keyed on Kind) and the consumer side
// (inputSubdirFor, keyed on the artifact filename) must agree for every
// artifact nixgg produces, or a link would look for an archive somewhere
// the archive drv never wrote it. They are separate functions because
// each side only has one of those two pieces of information.
// TestOutSubdirAgreesWithInputSubdirFor pins the agreement.

// outPath renders the artifact's destination inside the builder, e.g.
// `$out/bin/llc`. Callers embed this directly in the shell script.
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

// inlineFilesScript renders shell that copies every file out of
// $src (the staged directory referenced by InlineFilesStore, set via
// the same "src" env var / derivation attribute Compile already uses
// for its own SrcStore — see envDict/ToNix) into the build root,
// before the link command runs. A real `cp`, not text embedded in
// the script: see InlineFilesStore's own docstring for why (kernel
// argv limit).
func (d *Derivation) inlineFilesScript() string {
	if d.InlineFilesStore == "" {
		return ""
	}
	return "cp -a \"$src/.\" .\n"
}

// absFileScript renders shell that recreates AbsFilePath (a linker-
// referenced generated file whose name is an absolute build-tree
// path — see AbsFilePath's own docstring) with AbsFileContent's exact
// bytes, before the link command runs. `mkdir -p` on the dirname
// handles the fact that this derivation's sandbox starts with none
// of the caller's build-tree directory structure.
func (d *Derivation) absFileScript() string {
	if d.AbsFilePath == "" {
		return ""
	}
	dir := d.AbsFilePath[:strings.LastIndexByte(d.AbsFilePath, '/')]
	return fmt.Sprintf("mkdir -p %s\ncat > %s <<'NIXGG_ABS_FILE_EOF'\n%sNIXGG_ABS_FILE_EOF\n",
		shellQuote(dir), shellQuote(d.AbsFilePath), d.AbsFileContent)
}

// compilerOrAR reports which store path provides the tools on PATH.
// Compile and Link use the compiler; Archive uses whatever supplies
// `ar` — which native mode passes as compilerRoot and sandbox mode as
// AR. Two names, one meaning.
func (d *Derivation) compilerOrAR() string {
	if d.Kind == KindArchive {
		return d.AR
	}
	return d.Compiler
}

// isRawLinker reports whether d.Tool is a linker binary invoked
// directly (ld) rather than a compiler driver (cc/gcc/clang) that
// forwards flags to whatever linker it wraps. The two accept different
// group-bracket spellings: `-Wl,--start-group` is a driver convention;
// a raw ld invocation needs the bare form and rejects `-Wl,...`
// outright ("unrecognized option"). Confirmed against Linux Kbuild's
// vmlinux.o link, which invokes `ld` raw with bare `--start-group`.
func (d *Derivation) isRawLinker() bool {
	return d.Tool == "ld"
}

// buildScript is the single source of the shell body — the layout,
// quoting, and argv order that both wire formats must agree on.
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
				// A sibling drv we reference. Its artifact sits under the
				// FHS subdir its own Kind implies, so reach it there.
				name := in.Name
				if sub := inputSubdirFor(in.Name); sub != "" {
					name = sub + "/" + name
				}
				part = fmt.Sprintf("'%s/%s'", caOutputPlaceholder(in.Ref, "out"), name)
			}
			if part == "" {
				continue
			}
			// --whole-archive wraps a NAMED SUBSET of inputs (unlike
			// GroupInputs' single global span — see WholeArchiveInputs),
			// so wrap each matching input individually.
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
		// to before this split existed (hello/lua/mosh never had `-l`
		// flags to begin with).
		var lflags, nonLflags []string
		for _, f := range d.Flags {
			if strings.HasPrefix(f, "-l") && len(f) > 2 {
				lflags = append(lflags, f)
			} else {
				nonLflags = append(nonLflags, f)
			}
		}
		// Re-emit the group around the whole input list, only when the
		// caller asked for it (GroupInputs), so the no-group layout
		// stays byte-identical for every existing derivation. Spelling
		// depends on whether d.Tool is a compiler driver or a raw
		// linker — see isRawLinker.
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
	}
	return ""
}

// ToNix serialises this Derivation as an `import <helper>.nix { … }`
// expression suitable for writing to .nixgg/thunks/<id>.nix. The
// helper (builder.nix / linker.nix / archiver.nix, all in nix/) still
// calls `derivation`; this just fills in its arguments from our
// shared struct. helpers must be the /nix/store/…-nixgg-nix root.
//
// Byte-equivalent to the pre-Derivation-struct emitters; verified
// externally by tests/drv-equivalence.sh.
func (d *Derivation) ToNix(helpers string) string {
	tmpl, tag := d.scriptTemplate()
	var b strings.Builder
	switch d.Kind {
	case KindCompile:
		fmt.Fprintf(&b, "import %s/builder.nix {\n", helpers)
		fmt.Fprintf(&b, "  srcTree        = %s;\n", d.SrcStore) // Nix path literal, unquoted
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
			fmt.Fprintf(&b, "  srcTree        = %s;\n", d.InlineFilesStore) // Nix path literal, unquoted
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
	}
	b.WriteString("}\n")
	return b.String()
}

// derivInputsList renders `inputs = [ ... ]` for linker/archiver
// helpers, so they can interpolate each input into the script template.
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
		// Same FHS reach-in as the JSON path: resolve-script.nix renders
		// "${i.drv}/${i.name}", so name must carry the producer's subdir
		// or native mode would look for an archive at the output root
		// while the archive drv wrote it to lib/. One rule, both formats.
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
// byte-deterministic. Used for wrapperEnvJSON.
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

// toJSON serialises this Derivation for `nix derivation add`. The
// caller passes extraSrcs (basenames of already-realised store paths
// the sandbox must mount) and optional wrapper-env overrides via
// WrapperEnv on the struct. Inputs are split into inputs.drvs (for
// Kind=="nix") and inputs.srcs (for Kind=="store"). ExtraInputs are
// folded into the SAME drvs/srcs maps — Nix mounts everything named
// there regardless of whether the script text references it — but
// (unlike Inputs) never appear in d.script()'s argv; see
// Derivation.ExtraInputs' own docstring.
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
	// Link and Archive get an `_extraInputs` key unconditionally (empty
	// string when there are none), mirroring linker.nix/archiver.nix's
	// own unconditional `_extraInputs`. Both sides must agree on
	// whether the key EXISTS, not just its value — an earlier version
	// left it out of envDict, so JSON-mode env had one fewer key than
	// native-mode and the two hashed differently even with zero
	// ExtraInputs.
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
	// Compile derivations pass source/outName/src via env vars too
	// (that's what builder.nix's script consults — see the
	// `cd "$src"` / `"$source"` / `"$outName"` in script()). Link
	// derivations reuse the same "src" slot for InlineFilesStore, when
	// set — see inlineFilesScript's own docstring.
	switch {
	case d.Kind == KindCompile:
		env["src"] = d.SrcStore
		env["source"] = d.Source
		env["outName"] = d.OutName
	case d.Kind == KindLink && d.InlineFilesStore != "":
		env["src"] = d.InlineFilesStore
	}
	for k, v := range d.WrapperEnv {
		env[k] = v
	}
	return env
}

// nixIndentedStringLiteral renders `s` as a Nix indented-string literal.
//
// Two constructs inside that form are not literal, and both are reachable
// from user flags: a doubled apostrophe closes the string, and `${` opens
// an interpolation. Unescaped, they give a thunk that won't parse or that
// interpolates at eval time.
//
// Order matters: the apostrophe rule runs first, or it would also rewrite
// the apostrophes introduced when escaping `${`.
func nixIndentedStringLiteral(s string) string {
	e := strings.ReplaceAll(s, "''", "'''")
	e = strings.ReplaceAll(e, "${", "''${")
	return "''" + e + "''"
}
