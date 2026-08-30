package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const runnerInstallDestination = "/runner/ptah-runner"

func installSelf(destination string) error {
	if err := validateInstallDestination(destination, runnerInstallDestination); err != nil {
		return err
	}
	source, err := os.Executable()
	if err != nil {
		return errors.New("locate ptah-runner executable")
	}
	return copyExecutable(source, destination)
}

func validateInstallDestination(destination, requiredDestination string) error {
	if !filepath.IsAbs(destination) || destination != requiredDestination || filepath.Clean(destination) != destination {
		return fmt.Errorf("install destination must be %s", requiredDestination)
	}
	parent := filepath.Dir(destination)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return errors.New("install destination parent does not exist")
	}
	if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("install destination parent must be a real directory")
	}
	destinationInfo, err := os.Lstat(destination)
	if err == nil {
		if !destinationInfo.Mode().IsRegular() || destinationInfo.Mode()&os.ModeSymlink != 0 {
			return errors.New("existing install destination must be a regular file")
		}
	} else if !os.IsNotExist(err) {
		return errors.New("inspect install destination")
	}
	return nil
}

func copyExecutable(sourcePath, destinationPath string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return errors.New("open ptah-runner executable")
	}
	sourceInfo, err := source.Stat()
	if err != nil {
		_ = source.Close()
		return errors.New("stat ptah-runner executable")
	}
	if !sourceInfo.Mode().IsRegular() {
		_ = source.Close()
		return errors.New("ptah-runner executable is not a regular file")
	}

	destinationDirectory := filepath.Dir(destinationPath)
	temporary, err := os.CreateTemp(destinationDirectory, ".ptah-runner-*")
	if err != nil {
		_ = source.Close()
		return errors.New("create temporary runner executable")
	}
	temporaryPath := temporary.Name()
	installed := false
	defer func() {
		_ = source.Close()
		_ = temporary.Close()
		if !installed {
			_ = os.Remove(temporaryPath)
		}
	}()

	if _, err := io.Copy(temporary, source); err != nil {
		return errors.New("copy runner executable")
	}
	if err := temporary.Chmod(0o555); err != nil {
		return errors.New("set runner executable permissions")
	}
	if err := temporary.Sync(); err != nil {
		return errors.New("sync runner executable")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("close runner executable")
	}
	if err := os.Rename(temporaryPath, destinationPath); err != nil {
		return errors.New("install runner executable")
	}
	installed = true
	return nil
}
