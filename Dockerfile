# Autoretrieve uses Docker Buildkit to speed up builds, make sure
# DOCKER_BUILDKIT=1 is set

FROM golang:1.18 AS builder

# Install rustup
ENV RUSTUP_HOME=/usr/local/rustup \
    CARGO_HOME=/usr/local/cargo \
    PATH=/usr/local/cargo/bin:$PATH
RUN wget "https://static.rust-lang.org/rustup/dist/x86_64-unknown-linux-gnu/rustup-init"; \
    chmod +x rustup-init && \
    ./rustup-init -y --no-modify-path --profile minimal --default-toolchain nightly

# Install build dependencies
RUN --mount=type=cache,target=/var/cache/apt \
    apt update && \
    apt install -y --no-install-recommends jq libhwloc-dev ocl-icd-opencl-dev

# Build app
WORKDIR /app
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/app/extern \
    make

FROM ubuntu:22.04
RUN --mount=type=cache,target=/var/cache/apt \
    apt-get update && \
    apt-get install -y --no-install-recommends jq libhwloc-dev ocl-icd-opencl-dev ca-certificates
WORKDIR /app
COPY --from=builder /app/autoretrieve autoretrieve

# Create the volume for the autoretrieve data directory
# This is the default directory for autoretrieve configuration for this docker container
VOLUME /root/.autoretrieve

# Libp2p port
EXPOSE 6746

# Http(s) ports
EXPOSE 80
EXPOSE 443

# This env var is required deep in lotus
# Hardcoding for now for convenience until we have a reason to make it easily configurable
ENV FULLNODE_API_INFO="wss://api.chain.love"

ENTRYPOINT [ "./autoretrieve" ]