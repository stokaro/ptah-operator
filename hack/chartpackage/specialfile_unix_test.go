//go:build unix

package main

import "syscall"

// makeSpecialFile creates a file that is neither a directory, a regular file
// nor a symbolic link, which is what the packager has to refuse.
//
// A named pipe rather than a Unix socket: a socket's path is bounded at around
// a hundred bytes, and a macOS temporary directory spends most of that before
// the name is added, so the row that used a socket skipped on every local run
// and only ever measured anything in CI. A FIFO has no such bound.
func makeSpecialFile(path string) error {
	return syscall.Mkfifo(path, 0o600)
}
