// The SDK is its own module so that importing it does not drag in the daemon's
// dependencies -- netlink, vsock and the rest are host-side concerns a client
// has no business compiling.
module github.com/pablofdezr/microvm-sdk-go

go 1.26
