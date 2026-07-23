//go:build !linux

package cgroup

import "errors"

// fsMagic is unavailable off Linux. cgroups only exist there; this keeps the
// package building on a developer's machine.
func fsMagic(path string) (int64, error) {
	return 0, errors.New("cgroups are only available on linux")
}
