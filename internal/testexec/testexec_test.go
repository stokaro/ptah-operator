package testexec_test

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stokaro/ptah-operator/internal/testexec"
)

// forkMeanwhile keeps forking from other goroutines until the test ends, as
// the parallel tests of a package that runs commands do.
func forkMeanwhile(t *testing.T) {
	t.Helper()
	stop := make(chan struct{})
	var forks sync.WaitGroup
	for range 8 {
		forks.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = exec.Command("/bin/sh", "-c", "exit 0").Run()
			}
		})
	}
	t.Cleanup(func() {
		close(stop)
		forks.Wait()
	})
}

// Each script runs the moment it is written while eight goroutines fork. On
// Linux, writing it with os.WriteFile instead fails this row with "text file
// busy" within its first few dozen scripts.
func TestAScriptRunsAsSoonAsItIsWrittenWhileOthersFork(t *testing.T) {
	forkMeanwhile(t)
	directory := t.TempDir()
	for i := range 500 {
		path := filepath.Join(directory, fmt.Sprintf("script-%d", i))
		testexec.WriteFile(t, path, []byte("#!/bin/sh\nexit 0\n"), 0o700)
		if output, err := exec.Command(path).CombinedOutput(); err != nil {
			t.Fatalf("script %d did not run: %v: %s", i, err, output)
		}
	}
}
