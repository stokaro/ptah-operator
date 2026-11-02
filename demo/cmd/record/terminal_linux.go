package main

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
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
	var number uint32
	var ioctlErr error
	if err = raw.Control(func(fd uintptr) {
		if ioctlErr = unix.IoctlSetPointerInt(int(fd), unix.TIOCSPTLCK, 0); ioctlErr != nil {
			ioctlErr = fmt.Errorf("unlock the terminal: %w", ioctlErr)
			return
		}
		if number, ioctlErr = unix.IoctlGetUint32(int(fd), unix.TIOCGPTN); ioctlErr != nil {
			ioctlErr = fmt.Errorf("name the terminal: %w", ioctlErr)
		}
	}); err != nil {
		return nil, nil, err
	}
	if err = ioctlErr; err != nil {
		return nil, nil, err
	}
	replica, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	return controller, replica, nil
}
