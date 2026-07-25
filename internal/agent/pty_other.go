//go:build !linux

package agent

import "os"

// This file lets the agent package build off Linux, where the guest never runs
// but the tests do. A pty is a Linux kernel facility reached through Linux-only
// ioctls, so there is nothing to allocate here: openPTY fails, and a TTY exec is
// refused the same way any other unsupported request is. See pty_linux.go for the
// real implementation.

type ptyPair struct{}

func openPTY(rows, cols uint16) (*ptyPair, error) { return nil, errTTYUnsupported }

func (p *ptyPair) slaveFile() *os.File            { return nil }
func (p *ptyPair) masterFile() *os.File           { return nil }
func (p *ptyPair) closeSlave()                    {}
func (p *ptyPair) close()                         {}
func (p *ptyPair) resize(rows, cols uint16) error { return errTTYUnsupported }
