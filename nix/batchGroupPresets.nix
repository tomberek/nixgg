# Starting-point batchGroups declarations. NOT guaranteed correct or
# complete for any specific package (see go/internal/batch's own
# package docstring): unlike configureSrcFilterPresets (which can
# silently produce a stale build), an over/under-inclusive pattern here
# only changes derivation SHAPE — which TUs fold into a combined batch
# vs. get their own — every matched TU still compiles.
#
# The judgment call these presets encode is "this subtree is rarely
# edited relative to how often the project rebuilds" — vendored/
# third-party dependency trees are the common case. Verify and likely
# extend per project.
#
# Usage:
#
#   mkNixggBuild {
#     # ...
#     batchGroups = [
#       { name = "vendor"; patterns = batchGroupPresets.vendorDeps; }
#     ];
#   }
{
  # Common vendored-dependency directory names, matched at any depth
  # via internal/batch's "**" extension (NOT plain filepath.Match
  # syntax). Covers redis's deps/, autotools-style third_party/ and
  # vendor/, and Go's own vendor/ convention, for C/C++ source files.
  vendorDeps = [
    "deps/**/*.c"
    "deps/**/*.cc"
    "deps/**/*.cpp"
    "third_party/**/*.c"
    "third_party/**/*.cc"
    "third_party/**/*.cpp"
    "vendor/**/*.c"
    "vendor/**/*.cc"
    "vendor/**/*.cpp"
  ];
}
