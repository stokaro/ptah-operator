//go:build !windows

package main

import (
	"fmt"
	"os"
)

func validatePrivatePermissions(path string, info os.FileInfo) error {
	if info.Mode().Perm() != 0o600 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fmt.Errorf("destination %q must have mode 0600", path)
	}
	return nil
}
