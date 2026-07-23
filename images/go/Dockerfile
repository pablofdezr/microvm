# Go sandbox image.
#
# Unlike Python or Node, there is no slim option worth taking: running Go code
# means compiling it, so the toolchain has to be present and it is most of the
# image's size. golang:1.26-trixie is the smallest official image that still
# has a working compiler.
FROM golang:1.26-trixie AS agent-builder
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal/ internal/
COPY cmd/ cmd/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/microvm-agent ./cmd/microvm-agent

FROM golang:1.26-trixie

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*

COPY --from=agent-builder /out/microvm-agent /sbin/microvm-agent
RUN ln -sf /sbin/microvm-agent /sbin/init

RUN mkdir -p /proc /sys /dev/pts /dev/shm /run /tmp /overlay /workspace \
 && chmod 1777 /tmp

# The build and module caches must live in the writable overlay. Their defaults
# are under HOME, which resolves into the read-only base image -- `go run` then
# fails on its first compile with a permission error that says nothing about
# caches.
ENV GOCACHE=/tmp/.gocache
ENV GOMODCACHE=/tmp/.gomodcache
# Off by default: it phones home on every build, and a sandbox's network calls
# should be ones its own code made.
ENV GOTELEMETRY=off
# Sandboxes are ephemeral, so a warm cache never survives to be reused. Trading
# the linker's parallelism for it is not worth it.
ENV GOFLAGS=-buildvcs=false

WORKDIR /workspace
