//go:build windows

package main

import (
	"errors"
	"os"
)

func validatePrivatePermissions(_ string, _ os.FileInfo) error {
	return errors.New("hook log capture requires Unix mode 0600 destination semantics")
}
