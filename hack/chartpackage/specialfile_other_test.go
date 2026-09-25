//go:build !unix

package main

import "errors"

// makeSpecialFile has nothing to create on a platform without named pipes, and
// the row that calls it skips rather than pretending to measure the refusal.
func makeSpecialFile(string) error {
	return errors.New("named pipes are not available on this platform")
}
