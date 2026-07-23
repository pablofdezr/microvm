//go:build linux

package cgroup

import "golang.org/x/sys/unix"

// fsMagic returns the filesystem type magic number for the mount holding path.
func fsMagic(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	// Type is int64 on amd64 but int32 on some 32-bit arches, so widen it here
	// rather than at every call site.
	return int64(st.Type), nil
}
