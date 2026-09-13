# Pure-eval-compatible replacement for `builtins.storePath`, which
# refuses to run under `--pure-eval`. Strips any existing string
# context, then attaches fresh store context tagged as a path reference
# — without reading the filesystem or calling the disallowed builtin.
#
# Behaves like `builtins.storePath` for interpolation, `${p}/bin/x`,
# derivation inputs, and `_storeDeps`. Unlike it, does NOT verify the
# path exists — callers must ensure that.
path:
let
  contextFree = builtins.unsafeDiscardStringContext path;
in
  builtins.appendContext contextFree { ${contextFree} = { path = true; }; }
