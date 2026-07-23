# Rust sandbox image.
#
# rust:1-slim-trixie drops the docs and extra targets from the full image, but a
# compiler is still a compiler: this is the largest of the four by some margin,
# and unavoidably so, since running Rust means compiling it.
FROM golang:1.26-trixie AS agent-builder
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
COPY sdk/go/go.mod sdk/go/
RUN go mod download
COPY internal/ internal/
COPY cmd/ cmd/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/microvm-agent ./cmd/microvm-agent

FROM rust:1-slim-trixie

# rustc drives a linker, which it does not ship: without gcc, every compile
# fails at the link step with an error that reads like a missing crate. mold is a
# drop-in linker far faster than the default; linking is a large slice of a small
# program's build, so it directly shaves the run-once path a sandbox exists for.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates gcc libc6-dev mold \
 && rm -rf /var/lib/apt/lists/*

COPY --from=agent-builder /out/microvm-agent /sbin/microvm-agent
RUN ln -sf /sbin/microvm-agent /sbin/init

RUN mkdir -p /proc /sys /dev/pts /dev/shm /run /tmp /overlay /workspace \
 && chmod 1777 /tmp

# CARGO_HOME defaults into the read-only base. Cargo writes its registry index
# and package cache on the first build, so leaving it there fails any sandbox
# that adds a dependency.
ENV CARGO_HOME=/tmp/.cargo
ENV CARGO_TARGET_DIR=/tmp/target
# The sparse protocol fetches only the index entries a build needs, instead of
# cloning the whole crates.io index. In an ephemeral VM that clone is pure cost:
# it is downloaded once and thrown away with the sandbox.
ENV CARGO_REGISTRIES_CRATES_IO_PROTOCOL=sparse
# Link with mold (installed above). rustc invokes gcc as the linker driver and
# passes it -fuse-ld=mold. gcc in trixie supports the flag; a small program then
# links in a fraction of the default linker's time.
ENV RUSTFLAGS="-C link-arg=-fuse-ld=mold"

WORKDIR /workspace
