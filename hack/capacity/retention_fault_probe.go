package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"time"
)

func newRetentionFaultProbe(probe, state, inputs, evidence string) func(context.Context, string, int, string) error {
	var sequence atomic.Int64
	return func(ctx context.Context, action string, round int, identities string) error {
		logPath := filepath.Join(evidence, fmt.Sprintf("retention-fault-%s-%03d.private.log", action, sequence.Add(1)))
		output, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer output.Close()
		command := exec.CommandContext(ctx, "python3", probe, action, "--state", state, "--inputs", inputs, "--evidence", filepath.Join(evidence, "retention-fault"), "--round", fmt.Sprint(round), "--identities", identities)
		command.Stdout, command.Stderr = output, output
		command.Cancel = func() error { return command.Process.Signal(os.Interrupt) }
		command.WaitDelay = 20 * time.Second
		if err := command.Run(); err != nil {
			return fmt.Errorf("retention fault %s: %w (diagnostics: %s)", action, err, logPath)
		}
		return nil
	}
}
