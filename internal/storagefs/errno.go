package storagefs

import (
	"syscall"

	"github.com/pablofdezr/microvm/internal/storageclient"
)

// toErrno turns a storage client error into the errno the kernel expects.
//
// This is the reason the host bothers to send an errno at all: a filesystem's
// caller is a program calling open(), and it can act on EDQUOT or EROFS where it
// can do nothing with a string. The host already decided the errno next to the
// code that knew what went wrong (see storage.Server.fail); this just carries
// that decision the last step, into the syscall.Errno the FUSE layer returns.
//
// A nil error is errno 0, which go-fuse reads as success. An error the host did
// not tag, or one that never reached the host at all (a dial failure), becomes
// EIO -- the honest answer for "the filesystem could not do that", and the one a
// program is already prepared to see from any real disk.
func toErrno(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	switch storageclient.ErrnoOf(err) {
	case "ENOENT":
		return syscall.ENOENT
	case "EDQUOT":
		return syscall.EDQUOT
	case "EROFS":
		return syscall.EROFS
	case "EOPNOTSUPP":
		return syscall.EOPNOTSUPP
	case "EINVAL":
		return syscall.EINVAL
	default:
		return syscall.EIO
	}
}
