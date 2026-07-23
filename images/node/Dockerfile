# Node.js / TypeScript sandbox image.
#
# node:22-slim rather than the full image: ~200MB against ~1.1GB, and the
# difference is build toolchains sandboxed code rarely needs. Size matters more
# than usual here, since the image stays in the host page cache while any
# sandbox uses it.
FROM golang:1.26-trixie AS agent-builder
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal/ internal/
COPY cmd/ cmd/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/microvm-agent ./cmd/microvm-agent

FROM node:22-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# tsx runs TypeScript directly, with no build step and no tsconfig. The point of
# the sandbox is that a caller sends a .ts file and it runs; making them compile
# first would push our build system into their code.
#
# Installed globally at image build time so a sandbox needs no network to run
# TypeScript -- only to install its own dependencies.
RUN npm install -g tsx@4 typescript@5 \
 && npm cache clean --force

COPY --from=agent-builder /out/microvm-agent /sbin/microvm-agent
RUN ln -sf /sbin/microvm-agent /sbin/init

RUN mkdir -p /proc /sys /dev/pts /dev/shm /run /tmp /overlay /workspace \
 && chmod 1777 /tmp

# npm writes to its cache and config on every install. Both must point at the
# writable overlay; the default ~/.npm would work too, but being explicit keeps
# a stray HOME from sending them somewhere read-only.
ENV NPM_CONFIG_CACHE=/tmp/.npm
ENV NPM_CONFIG_UPDATE_NOTIFIER=false

# Node caches compiled module bytecode when NODE_COMPILE_CACHE is set (Node 22+).
# Point it at a fixed baked path -- not /tmp, which the agent shadows with a
# fresh tmpfs at boot -- and warm it below. A sandbox's `tsx` startup then reuses
# the cached compilation of tsx and its dependencies (read from the read-only
# base, new entries copied up into the overlay) instead of recompiling them
# every run. A stale cache is only ever ignored, never wrong.
ENV NODE_COMPILE_CACHE=/opt/node-cache
RUN mkdir -p /opt/node-cache \
 && printf 'console.log("warm")\n' > /tmp/warm.ts \
 && tsx /tmp/warm.ts >/dev/null \
 && rm -f /tmp/warm.ts \
 && chmod -R a+rX /opt/node-cache

WORKDIR /workspace
