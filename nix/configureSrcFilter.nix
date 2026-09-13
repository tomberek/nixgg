# configureSrcFilter — shrink a package's `src` to just the files
# configurePhase reads, so splitStdenv's configure stage doesn't
# invalidate when an unrelated source file changes.
#
# Doesn't use lib.fileset: that needs a real eval-time Path, but
# `pkgs.foo.src` is usually an unrealized fetcher derivation (would need
# import-from-derivation to force it). This filters inside an ordinary
# CA build step instead — src stays whatever it already is, realized
# normally. __contentAddressed gives the early-cutoff: an edit outside
# includePatterns still changes this derivation's input, so it re-runs,
# but if the copied-out subset is byte-identical, the output path
# doesn't change, so the configure stage never sees a different input.
{
  lib,
  stdenvNoCC,
}:
{
  name,
  src,
  # `find -path`-compatible glob patterns, relative to the unpacked
  # source root, e.g. [ "configure" "Makefile.am" "*/Makefile.am" ].
  # See configureSrcFilterPresets.nix for starting points. An
  # under-inclusive pattern (excluding a file configure actually
  # reads) produces a silently STALE build, not an error — verify by
  # building, not by inspection.
  includePatterns,
  # Paths to create as EMPTY stub files, for checks that only test
  # existence and never read content — e.g. autoconf's
  # AC_CONFIG_SRCDIR bakes a `test -r "$srcdir/$ac_unique_file"` into
  # every generated configure script. Anything with real content
  # belongs in includePatterns instead.
  existenceStubs ? [ ],
}:
stdenvNoCC.mkDerivation {
  inherit name src;
  __contentAddressed = true;
  outputHashMode = "nar";
  outputHashAlgo = "sha256";
  dontConfigure = true;
  dontBuild = true;
  dontFixup = true;
  installPhase = ''
    runHook preInstall
    mkdir -p "$out"
    ${lib.concatMapStrings (p: ''
      find . -path ${lib.escapeShellArg ("./" + p)} -exec cp -a --parents -t "$out" {} +
    '') includePatterns}
    ${lib.concatMapStrings (
      p:
      let
        escaped = lib.escapeShellArg p;
      in
      ''
        mkdir -p "$(dirname "$out"/${escaped})"
        [ -e "$out"/${escaped} ] || touch "$out"/${escaped}
      ''
    ) existenceStubs}
    runHook postInstall
  '';
}
