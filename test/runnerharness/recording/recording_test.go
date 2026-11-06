package recording_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/test/runnerharness/recording"
)

// The recording Executor stands where runner.OSExecutor stands in a Pod, so a
// child that exits, a child that cannot start and a canceled context have to
// read the same to the runner from either.
func TestTheRecordingExecutorAnswersAsTheOSExecutorDoes(t *testing.T) {
	t.Parallel()
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, row := range []struct {
		name string
		ctx  context.Context
		spec runner.CommandSpec
	}{
		{"success", t.Context(), runner.CommandSpec{Path: "/bin/sh", Args: []string{"-c", "echo out; echo err >&2"}}},
		{"a nonzero exit", t.Context(), runner.CommandSpec{Path: "/bin/sh", Args: []string{"-c", "echo out; exit 3"}}},
		{"no such binary", t.Context(), runner.CommandSpec{Path: "/no/such/binary"}},
		{"a canceled context", canceled, runner.CommandSpec{Path: "/bin/sh", Args: []string{"-c", "exit 0"}}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			var wantOut, wantErr, gotOut, gotErr bytes.Buffer
			wantExit, wantError := runner.OSExecutor{}.Execute(row.ctx, row.spec, &wantOut, &wantErr)
			executor := &recording.Executor{}
			gotExit, gotError := executor.Execute(row.ctx, row.spec, &gotOut, &gotErr)
			if gotExit != wantExit {
				t.Errorf("exit = %d, and runner.OSExecutor says %d", gotExit, wantExit)
			}
			if (gotError == nil) != (wantError == nil) || errors.Is(gotError, context.Canceled) != errors.Is(wantError, context.Canceled) {
				t.Errorf("error = %v, and runner.OSExecutor says %v", gotError, wantError)
			}
			if gotOut.String() != wantOut.String() || gotErr.String() != wantErr.String() {
				t.Errorf("output = %q, %q, and runner.OSExecutor wrote %q, %q", gotOut.String(), gotErr.String(), wantOut.String(), wantErr.String())
			}
		})
	}
}

func TestTheRecordingExecutorKeepsWhatAChildDid(t *testing.T) {
	t.Parallel()
	executor := &recording.Executor{AppendArgs: []string{"appended"}}
	spec := runner.CommandSpec{Path: "/bin/sh", Args: []string{"-c", `echo "out $0"; echo err >&2; exit 4`}}
	var stdout, stderr bytes.Buffer
	if exit, err := executor.Execute(t.Context(), spec, &stdout, &stderr); exit != 4 || err != nil {
		t.Fatalf("Execute() = %d, %v", exit, err)
	}
	children := executor.Children()
	if len(children) != 1 {
		t.Fatalf("recorded %d children", len(children))
	}
	child := children[0]
	if strings.Join(child.Args, " ") != strings.Join(spec.Args, " ")+" appended" {
		t.Errorf("args = %q", child.Args)
	}
	if string(child.Stdout) != "out appended\n" || string(child.Stderr) != "err\n" || child.Exit != 4 || child.Error != "" {
		t.Errorf("child = %+v", child)
	}
	if child.PeakKiB <= 0 {
		t.Errorf("peak memory = %d, want a measurement", child.PeakKiB)
	}
	if stdout.String() != "out appended\n" || stderr.String() != "err\n" {
		t.Errorf("the runner read %q and %q", stdout.String(), stderr.String())
	}
}
