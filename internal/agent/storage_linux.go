//go:build linux

package agent

import (
	"context"
	"log/slog"
	"net"
	"os"

	"github.com/mdlayher/vsock"
	"github.com/pablofdezr/microvm/internal/protocol"
	"github.com/pablofdezr/microvm/internal/storageclient"
	"github.com/pablofdezr/microvm/internal/storagefs"
)

// mountStorage brings up the object-storage filesystem, if this sandbox has one.
//
// The signal is the kernel command line, which the host sets only when it has
// actually stood up a storage server for this VM: "microvm.storage=rw" or "=ro",
// with "microvm.storage_path" naming where to mount. No flag means no storage,
// and the guest reaches for nothing.
//
// Every failure here is survivable. A sandbox without storage is a degraded
// sandbox, not a broken one -- code that needs no files runs fine -- so a
// missing /dev/fuse or a mount that will not take is logged and stepped over,
// never fatal. The most likely cause, a guest kernel built without
// CONFIG_FUSE_FS, is called out by name because it is invisible otherwise: the
// device node simply is not there.
func mountStorage(log *slog.Logger, cmdline kernelCmdline) {
	mode := cmdline.get("microvm.storage", "")
	if mode == "" {
		return // no storage provisioned for this sandbox
	}
	mountpoint := cmdline.get("microvm.storage_path", "/mnt/storage")
	readOnly := mode == "ro"

	// /dev/fuse is what the kernel exposes when CONFIG_FUSE_FS is present. Its
	// absence is the one failure worth naming: nothing else here will tell you
	// the kernel was built without FUSE.
	if _, err := os.Stat("/dev/fuse"); err != nil {
		log.Warn("storage requested but /dev/fuse is absent; "+
			"the guest kernel needs CONFIG_FUSE_FS. Continuing without storage.",
			"err", err)
		return
	}

	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		log.Error("cannot create storage mountpoint; continuing without storage",
			"path", mountpoint, "err", err)
		return
	}

	// The dial is the guest half of the vsock: connect to the host (CID 2) on the
	// storage port, which Firecracker routes to the Unix socket inside this
	// sandbox's jail. There is no handshake in this direction (see internal/vsock)
	// and no credential to present -- the socket is the identity. The client
	// dials lazily, so this closure does not run until something touches the
	// mount, by which point the host is accepting.
	client := storageclient.New(func(ctx context.Context) (net.Conn, error) {
		return vsock.Dial(protocol.HostCID, protocol.StoragePort, nil)
	})

	server, err := storagefs.Mount(mountpoint, client, readOnly, false)
	if err != nil {
		log.Error("mounting storage failed; continuing without it",
			"path", mountpoint, "err", err)
		return
	}
	// The server serves in its own goroutine, which holds the only reference it
	// needs. It lives for the life of the VM; there is no graceful storage
	// teardown because the VM's death is the teardown.
	_ = server
	log.Info("storage mounted", "path", mountpoint, "read_only", readOnly)
}
