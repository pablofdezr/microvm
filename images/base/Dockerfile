# The base layer every language image builds on: a minimal userland plus the
# agent installed as init. Nothing here is language-specific.
#
# The agent is built in a separate stage so that neither the Go toolchain nor
# the module cache ends up in the rootfs. Every megabyte here is multiplied by
# the page cache cost of keeping the image resident on the host.
ARG TARGETARCH

FROM --platform=$BUILDPLATFORM golang:1.26-trixie AS agent-builder
ARG TARGETARCH
WORKDIR /src

# Copy the manifests first so dependency download caches independently of the
# source, which changes far more often.
COPY go.mod go.sum ./
RUN go mod download

COPY internal/ internal/
COPY cmd/ cmd/

# CGO_ENABLED=0 makes the agent a static binary with no libc dependency, so it
# runs as PID 1 before any dynamic loader or filesystem is available.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/microvm-agent ./cmd/microvm-agent

FROM debian:trixie-slim

# ca-certificates is the one thing sandboxes universally need that a slim image
# lacks: without it every HTTPS call from guest code fails to verify.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# The kernel execs /sbin/init as PID 1. The agent detects that and takes on init
# duties (mounts, overlay root, networking, reaping) before serving vsock.
COPY --from=agent-builder /out/microvm-agent /sbin/microvm-agent
RUN ln -sf /sbin/microvm-agent /sbin/init

# Mount points init needs to exist before it can populate them, and the default
# working directory for execs.
RUN mkdir -p /proc /sys /dev/pts /dev/shm /run /tmp /overlay /workspace \
 && chmod 1777 /tmp

WORKDIR /workspace
