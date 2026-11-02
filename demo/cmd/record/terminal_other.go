//go:build !linux && !darwin

package main

import (
	"context"
	"fmt"
	"runtime"
)

// terminal is where a step with answers runs. The recorder opens
// pseudo-terminals on Linux and macOS, which is where the lab runs.
func (r *recorder) terminal(_ context.Context, command string, _ []answer) (outcome, error) {
	return outcome{}, fmt.Errorf("%q answers a prompt, and the recorder opens no terminal on %s",
		firstLine(command), runtime.GOOS)
}
