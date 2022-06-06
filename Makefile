# Extern dependencies commit hashes
filecoin-ffi-commit = 5d00bb4365a97890

all: autoretrieve
.PHONY: all

autoretrieve: extern/filecoin-ffi
	go build

extern/filecoin-ffi:
	git clone https://github.com/filecoin-project/filecoin-ffi -b $(filecoin-ffi-commit) extern/filecoin-ffi
	cd extern/filecoin-ffi && make

clean:
	rm -rf extern
.PHONY: clean