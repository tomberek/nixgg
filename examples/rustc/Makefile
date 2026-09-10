RUSTC ?= rustc
AR ?= ar
RUSTFLAGS ?= --edition=2021 -Cdebuginfo=0 -Ccodegen-units=1

all: libapp.a

# rustc's @-file format is one argument per line, verbatim — not the
# compiler drivers' whitespace-and-quotes tokenisation. Nothing else in
# the suite covers the difference, and getting it wrong killed a kernel
# build on its first rustc call.
cfg.args:
	printf -- '--cfg=feature_from_argfile\n' > $@

# One invocation, two artifacts: the object, and the metadata `app`
# resolves --extern against. A grouped target (&:) because that is what
# actually happens — without it make may run the recipe twice.
dep.o libdep.rmeta &: dep/lib.rs
	$(RUSTC) $(RUSTFLAGS) --crate-type=rlib --crate-name=dep \
	  --emit=obj=dep.o --emit=metadata=libdep.rmeta dep/lib.rs

app.o: app/main.rs app/gen.rs libdep.rmeta cfg.args
	$(RUSTC) $(RUSTFLAGS) @cfg.args --crate-type=rlib --crate-name=app \
	  --extern dep=libdep.rmeta --emit=obj=app.o app/main.rs

libapp.a: app.o dep.o
	$(AR) rcs $@ app.o dep.o

clean:
	rm -f app.o dep.o libdep.rmeta libapp.a cfg.args
