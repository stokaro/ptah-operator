package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// openTerminal opens a pseudo-terminal pair: the controller the recorder reads
// and types into, and the replica the command runs on.
func openTerminal() (controller, replica *os.File, err error) {
	controller, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			_ = controller.Close()
		}
	}()
	fd := controller.Fd()
	if err = ioctl(fd, syscall.TIOCPTYGRANT, 0); err != nil {
		return nil, nil, fmt.Errorf("grant the terminal: %w", err)
	}
	if err = ioctl(fd, syscall.TIOCPTYUNLK, 0); err != nil {
		return nil, nil, fmt.Errorf("unlock the terminal: %w", err)
	}
	// TIOCPTYGNAME writes the replica's path into a buffer of 128 bytes.
	name := make([]byte, 128)
	if err = ioctl(fd, syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); err != nil {
		return nil, nil, fmt.Errorf("name the terminal: %w", err)
	}
	end := bytes.IndexByte(name, 0)
	if end <= 0 {
		return nil, nil, errors.New("name the terminal: the kernel returned no path")
	}
	replica, err = os.OpenFile(string(name[:end]), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	return controller, replica, nil
}

func ioctl(fd, request, argument uintptr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, argument); errno != 0 {
		return errno
	}
	return nil
}
