//go:build linux || darwin

package main

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
)

// The lock inode is retained. Removing it would let another process lock a
// different inode while this observer still owns the original one.
func lockState(path string) (*os.File, error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("upgrade state already has a writer: %w", err)
	}
	return f, nil
}
