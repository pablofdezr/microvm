# Python sandbox image.
#
# The slim variant is the whole point: python:3.13 is ~1GB against slim's
# ~150MB, and the difference is build toolchains and docs that sandboxed code
# almost never needs. Size matters more here than in a normal container, because
# every image sits in the host page cache for as long as any sandbox uses it.
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

FROM python:3.13-slim

# ca-certificates: without it every HTTPS call from guest code fails to verify,
# including pip itself.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*

COPY --from=agent-builder /out/microvm-agent /sbin/microvm-agent
RUN ln -sf /sbin/microvm-agent /sbin/init

RUN mkdir -p /proc /sys /dev/pts /dev/shm /run /tmp /overlay /workspace \
 && chmod 1777 /tmp

# Unbuffered output: Python buffers stdout when it is not a tty, which the
# sandbox's pipe is not. Without this, a program's prints arrive only when it
# exits -- destroying the streaming the rest of the system works to provide,
# and making a killed program look like it printed nothing at all.
ENV PYTHONUNBUFFERED=1
# Writing .pyc files into the read-only base is pointless: they land in the
# tmpfs overlay and are discarded with the VM.
ENV PYTHONDONTWRITEBYTECODE=1

WORKDIR /workspace
