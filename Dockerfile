# Autoretrieve uses Docker Buildkit to speed up builds, make sure
# DOCKER_BUILDKIT=1 is set

FROM golang:1.17 AS builder

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
    apt-get install -y --no-install-recommends jq libhwloc-dev ocl-icd-opencl-dev
WORKDIR /app
COPY --from=builder /app/autoretrieve autoretrieve

# Create the /app/data volume for the autoretrieve data directory
VOLUME /app/data
ENV AUTORETRIEVE_DATA_DIR=/app/data

CMD ["./autoretrieve"]