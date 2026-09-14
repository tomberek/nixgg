#![no_std]

include!("gen.rs");

// The cfg comes from an @-argfile. rustc's argfile format is one
// argument per line, verbatim — tokenising it the compiler-driver way
// mangles the value and the cfg silently never arrives. Fail loudly
// instead: a silent miss here is exactly the bug this guards.
#[cfg(not(feature_from_argfile))]
compile_error!("--cfg from the @-argfile did not reach rustc");

pub fn total() -> u32 {
    dep::value() + EXTRA
}
