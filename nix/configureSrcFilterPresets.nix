# Starting-point configureSrcFilter include-pattern lists. NOT
# guaranteed correct for any specific package — an under-inclusive
# pattern silently produces a stale build, not an error, so verify by
# building. `find -path` glob depth is literal ("*/x" matches one
# level, not any depth) — deeply-nested packages may need more
# patterns or a coarser include.
#
# Pair with a package-specific `existenceStubs` at the call site for
# autoconf's AC_CONFIG_SRCDIR (one author-chosen file, tested for
# existence only):
#
#   configureSrcFilter = {
#     includePatterns = configureSrcFilterPresets.autotools;
#     existenceStubs = [ "src/hello.c" ];
#   };
{
  # Autotools: configure + everything autoreconf/automake read to
  # regenerate it (Makefile.am/configure.ac, aclocal/m4 macros,
  # build-aux scripts), every *.in template config.status substitutes
  # into a generated file, *.mk fragments (a MISSING one makes `make`
  # unconditionally reinvoke automake), and po/ wholesale (gettext's
  # translation subsystem has no small glob that captures it, so
  # all-or-nothing is the pragmatic choice).
  autotools = [
    "configure"
    "configure.ac"
    "configure.in"
    "Makefile.am"
    "*/Makefile.am"
    "*/*/Makefile.am"
    "Makefile.in"
    "*/Makefile.in"
    "*/*/Makefile.in"
    "*.in"
    "*/*.in"
    "*/*/*.in"
    "*.mk"
    "*/*.mk"
    "*/*/*.mk"
    "aclocal.m4"
    "config.h.in"
    "*.m4"
    "*/*.m4"
    "build-aux"
    "build-aux/*"
    "m4"
    "m4/*"
    "po"
    "po/*"
  ];

  # CMake: CMakeLists.txt at any nesting level, *.cmake modules, and
  # *.in templates configure_file() reads directly. Most real cmake
  # packages ALSO need explicit source-file patterns added at the call
  # site, since add_library()/add_executable() need real .c/.cc sources
  # present at CONFIGURE time (cmake generates the build system from
  # the full target graph, unlike autotools). If a package's
  # CMakeLists.txt uses `file(GLOB ...)` over a whole directory (zstd
  # does this for lib/**/*.c), there's no smaller filter that preserves
  # early-cutoff — configure needs the whole globbed directory present.
  cmake = [
    "CMakeLists.txt"
    "*/CMakeLists.txt"
    "*/*/CMakeLists.txt"
    "*/*/*/CMakeLists.txt"
    "*.cmake"
    "*/*.cmake"
    "cmake"
    "cmake/*"
    "*.in"
    "*/*.in"
    "*/*/*.in"
    "*/*/*/*.in"
  ];
}
