// Package testexec writes the scripts a test then executes.
//
// A file a test writes and then executes can fail with "text file busy"
// (ETXTBSY) when another goroutine of the same test binary forks while the
// file is still open for writing: the child holds a copy of that descriptor
// until it execs, and the kernel refuses to execute a file that is open for
// writing. Parallel tests that run commands fork all the time, so the failure
// comes and goes with the load on the machine.
package testexec

import (
	"os"
	"syscall"
	"testing"
)

// WriteFile writes content to path with mode, as os.WriteFile does, while
// holding syscall.ForkLock for reading. A fork takes that lock for writing, so
// no child of this process is created while the file is open, and none holds
// the descriptor when the test executes the file.
func WriteFile(t testing.TB, path string, content []byte, mode os.FileMode) {
	t.Helper()
	syscall.ForkLock.RLock()
	err := os.WriteFile(path, content, mode)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
}
