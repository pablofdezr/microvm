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
COPY sdk/go/go.mod sdk/go/
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

# The build cache lives at a fixed path in the read-only rootfs, pre-populated
# with the compiled standard library below. The guest overlays a writable tmpfs
# over the whole root, so those baked entries are read straight from the base
# image while a sandbox's own build output is copied up into the overlay -- a
# warm cache that costs nothing at boot. It must NOT sit under /tmp, /run or
# /dev/shm: the agent mounts a fresh tmpfs on each of those, which would shadow
# the baked cache with an empty one. The module cache stays writable-and-empty,
# since a sandbox's dependencies are its own and fetched at run time.
ENV GOCACHE=/opt/gocache
ENV GOMODCACHE=/tmp/.gomodcache
# Off by default: it phones home on every build, and a sandbox's network calls
# should be ones its own code made.
ENV GOTELEMETRY=off
ENV GOFLAGS=-buildvcs=false

# Prewarm: compile the standard library into the baked cache. Go 1.20+ no longer
# ships a precompiled stdlib, so without this a sandbox's first `go build`
# recompiles everything it imports from scratch -- ~37s cold on a Pi 5. With the
# warm cache that first build drops to ~0.6s. Worth the ~150MB it adds to an
# image whose whole reason to be large is that it carries a compiler.
RUN mkdir -p /opt/gocache && go build std && chmod -R a+rX /opt/gocache

WORKDIR /workspace
