# commit or branch for the extern/filecoin-ffi dependency
filecoin_ffi_branch = 5d00bb4365a97890

ffi_remote = https://github.com/filecoin-project/filecoin-ffi
ffi_dir = extern/filecoin-ffi
ffi_update = $(ffi_dir)-$(ffi_dir) # ffi exists
ffi_checkout = $(ffi_dir)- # ffi doesn't yet exist
ffi_target = $(ffi_dir)-$(wildcard $(ffi_dir)) # switch between update & checkout depending on status
# if ffi is cloned, check the ref we have and the ref we want so we can compare in ffi_update
ffi_expected_ref = $(shell [ -d "${ffi_dir}" ] && cd ${ffi_dir} && git rev-parse -q HEAD)
ffi_current_ref = $(shell [ -d "${ffi_dir}" ] && cd ${ffi_dir} && git rev-parse -q ${filecoin_ffi_branch})

all: autoretrieve
.PHONY: all

autoretrieve: | $(ffi_target)
	go build

$(ffi_update): # we have FFI checked out, make sure it's running the commit we want, update it if not
ifneq ($(ffi_expected_ref), $(ffi_current_ref))
	@echo Commit changed, updating and rebuilding FFI ...
	cd $(ffi_dir) && git fetch $(ffi_remote) && git checkout $(filecoin_ffi_branch) && make
endif

$(ffi_checkout): ffi_checkout # we don't have FFI checked out, clone and build
	cd $(ffi_dir) && make

ffi_checkout:
	@echo Cloning and rebuilding FFI ...
	git clone $(ffi_remote) -b $(filecoin_ffi_branch) $(ffi_dir)

clean:
	rm -rf extern autoretrieve
.PHONY: clean
