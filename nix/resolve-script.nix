# Resolve a Go-authored script template into the final bash body.
#
# The Go driver (internal/expr Derivation.buildScript) emits the whole
# shell command — PATH, flag quoting, argv order. Sandbox mode bakes
# that text straight into a JSON drv; native mode routes it through
# here, because a few values only exist once Nix evaluates the thunk:
#
#   @<tag>_COREUTILS@   coreutilsRoot
#   @<tag>_COMPILER@    compilerRoot (or the `ar` provider, for archiver)
#   @<tag>_INPUT<i>@    inputs[i] — possibly an unrealised sibling whose
#                       CA output placeholder needs instantiation first
#
# Substituting rather than re-deriving the command means this file can't
# silently disagree with Go's layout logic the way two past divergences
# (`'` quoting, `-l` ordering) did, which hashed differently for the
# same compile.
#
# `tag` is chosen per script — Go takes the first unoccupied NIXGG,
# NIXGG1, … so a flag whose own text spells a marker can't collide. See
# markerTag in internal/expr/derivation.go.
#
# String context survives builtins.replaceStrings, so substituting a
# derivation still records the inputDrvs edge as `"${drv}/name"` would.
{
  scriptTemplate,
  markerTag ? "NIXGG",
  coreutils,
  compiler,
  inputs ? [ ],  # { drv, name } list; drv is a derivation or pureStorePath
}:
let
  # Must match coreutilsMarker / compilerMarker / inputMarker in
  # internal/expr/derivation.go. TestNativeTemplateResolvesToSandboxScript
  # runs this file against Go's output and fails if they disagree.
  inputMarkers = builtins.genList
    (i: "@${markerTag}_INPUT${toString i}@")
    (builtins.length inputs);
  inputValues = map (i: "${i.drv}/${i.name}") inputs;
in
builtins.replaceStrings
  ([ "@${markerTag}_COREUTILS@" "@${markerTag}_COMPILER@" ] ++ inputMarkers)
  ([ "${coreutils}" "${compiler}" ] ++ inputValues)
  scriptTemplate
