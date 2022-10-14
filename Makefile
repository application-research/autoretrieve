# commit or branch for the extern/filecoin-ffi dependency
filecoin_ffi_branch = 32afd6e1f1419b6b

ffi_remote = https://github.com/filecoin-project/filecoin-ffi
ffi_dir = extern/filecoin-ffi
ffi_update = $(ffi_dir)-$(ffi_dir) # ffi exists
ffi_checkout = $(ffi_dir)- # ffi doesn't yet exist
ffi_target = $(ffi_dir)-$(wildcard $(ffi_dir)) # switch between update & checkout depending on status
# if ffi is cloned, check the ref we have and the ref we want so we can compare in ffi_update
ffi_expected_ref = $(shell [ -d "${ffi_dir}" ] && cd ${ffi_dir} && git rev-parse -q HEAD)
ffi_current_ref = $(shell [ -d "${ffi_dir}" ] && cd ${ffi_dir} && git rev-parse -q ${filecoin_ffi_branch})
os_uname = $(shell uname)

all: autoretrieve
.PHONY: all

autoretrieve: | $(ffi_target)
	go build

# we have FFI checked out, make sure it's running the commit we want, update it if not
$(ffi_update):
ifneq ($(ffi_expected_ref), $(ffi_current_ref))
	@echo Commit changed, updating and rebuilding FFI ...
	cd $(ffi_dir) && git fetch $(ffi_remote) && git checkout $(filecoin_ffi_branch) && make
endif

# we don't have FFI checked out, clone and build
$(ffi_checkout): ffi_checkout
	cd $(ffi_dir) && make

ffi_checkout:
	@echo Cloning and rebuilding FFI ...
	git clone $(ffi_remote) -b $(filecoin_ffi_branch) $(ffi_dir)

# intended to be used in CI to prepare the environemnt to install FFI
ffi_install_dependencies:
ifeq ($(os_uname),Darwin)
	# assumes availability of Homebrew
	brew install hwloc
endif
ifeq ($(os_uname),Linux)
	# assumes an APT based Linux
	sudo apt-get install ocl-icd-opencl-dev libhwloc-dev -y
endif

.PHONY: install
install: autoretrieve
	install -C autoretrieve /usr/local/bin/autoretrieve

.PHONY: install-autoretrieve-service
install-autoretrieve-service:
	cp scripts/autoretrieve-service/autoretrieve-register.service /etc/systemd/system/autoretrieve-register.service
	cp scripts/autoretrieve-service/autoretrieve.service /etc/systemd/system/autoretrieve.service
	mkdir -p /etc/autoretrieve
	cp scripts/autoretrieve-service/config.env /etc/autoretrieve/config.env

	#TODO: if service changes to autoretrieve user/group, need to chown the /etc/autoretrieve dir and contents

	systemctl daemon-reload

	#Edit config values in /etc/autoretrieve/config.env before running any autoretrieve service files
	#Run 'sudo systemctl start autoretrieve-setup.service' to complete setup
	#Run 'sudo systemctl enable --now autoretrieve.service' once ready to enable and start autoretrieve service

clean:
	rm -rf extern autoretrieve
.PHONY: clean
