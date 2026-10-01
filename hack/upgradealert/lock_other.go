//go:build !linux && !darwin

package main

import (
	"errors"
	"os"
)

func lockState(string) (*os.File, error) {
	return nil, errors.New("the upgrade observer requires Linux or macOS file locking")
}
