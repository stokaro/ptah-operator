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
	// Through Control rather than Fd: Fd puts the descriptor in blocking mode,
	// and a blocking read is one that closing the file cannot end.
	raw, err := controller.SyscallConn()
	if err != nil {
		return nil, nil, err
	}
	// TIOCPTYGNAME writes the replica's path into a buffer of 128 bytes.
	name := make([]byte, 128)
	var ioctlErr error
	if err = raw.Control(func(fd uintptr) {
		if ioctlErr = ioctl(fd, syscall.TIOCPTYGRANT, 0); ioctlErr != nil {
			ioctlErr = fmt.Errorf("grant the terminal: %w", ioctlErr)
			return
		}
		if ioctlErr = ioctl(fd, syscall.TIOCPTYUNLK, 0); ioctlErr != nil {
			ioctlErr = fmt.Errorf("unlock the terminal: %w", ioctlErr)
			return
		}
		if ioctlErr = ioctl(fd, syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); ioctlErr != nil {
			ioctlErr = fmt.Errorf("name the terminal: %w", ioctlErr)
		}
	}); err != nil {
		return nil, nil, err
	}
	if err = ioctlErr; err != nil {
		return nil, nil, err
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
