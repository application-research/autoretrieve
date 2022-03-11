# Extern dependencies from GitHub .tar.gz of source
filecoin-ffi-url = https://github.com/filecoin-project/filecoin-ffi/archive/refs/tags/8e377f906ae40239.tar.gz

all: autoretrieve
.PHONY: all

autoretrieve: extern/filecoin-ffi
	go build

extern/filecoin-ffi:
	mkdir -p extern/filecoin-ffi
	curl -L $(filecoin-ffi-url) | tar xzvf - -C extern/filecoin-ffi --strip-components 1
	cd extern/filecoin-ffi && make

clean:
	rm -rf extern
.PHONY: clean