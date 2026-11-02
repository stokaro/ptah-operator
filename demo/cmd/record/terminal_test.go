//go:build linux || darwin

package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// A step that answers a prompt runs on a terminal, because the command it
// demonstrates asks only a terminal: kubectl ptah approve refuses an answer
// piped into it. These rows run real shells on real pseudo-terminals, so what
// they hold is what the recorder does on the machine that records.
func TestTerminalTypesEachAnswerAfterItsPrompt(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name    string
		command string
		answers []answer
		exit    int
		shows   []string
		hides   []string
	}{
		{
			name:    "a command that asks once",
			command: `printf 'Approve? Type yes to approve: '; read reply; echo "decided: $reply"`,
			answers: []answer{{Prompt: "Type yes to approve: ", Type: "yes"}},
			// The terminal echoes the answer after the prompt, where a person
			// typed it, and the command's reply follows on the next line.
			shows: []string{"Type yes to approve: yes\ndecided: yes\n"},
		},
		{
			name:    "a command that checks it is on a terminal",
			command: `if [ -t 0 ] && [ -t 1 ] && [ -t 2 ]; then echo "on a terminal"; fi; printf 'go? '; read reply`,
			answers: []answer{{Prompt: "go? ", Type: "yes"}},
			shows:   []string{"on a terminal"},
		},
		{
			name: "two prompts, answered in order",
			command: `printf 'first: '; read one; printf 'second: '; read two; ` +
				`echo "got $one then $two"`,
			answers: []answer{{Prompt: "first: ", Type: "a"}, {Prompt: "second: ", Type: "b"}},
			shows:   []string{"first: a\nsecond: b\ngot a then b\n"},
		},
		{
			// stderr is the same terminal, and arrives where it was written
			// rather than after everything on stdout.
			name:    "both streams, in the order they were written",
			command: `echo out; echo err >&2; printf 'Q: '; read reply; echo after`,
			answers: []answer{{Prompt: "Q: ", Type: "yes"}},
			shows:   []string{"out\nerr\nQ: yes\nafter\n"},
		},
		{
			name:    "the exit status of the command",
			command: `printf 'Q: '; read reply; exit 4`,
			answers: []answer{{Prompt: "Q: ", Type: "no"}},
			exit:    4,
			shows:   []string{"Q: no"},
		},
		{
			// No carriage return reaches the transcript: the terminal writes
			// every line break as CR LF, and a file has LF.
			name:    "line endings as a file has them",
			command: `printf 'one\ntwo\n'; printf 'Q: '; read reply`,
			answers: []answer{{Prompt: "Q: ", Type: "yes"}},
			shows:   []string{"one\ntwo\nQ: yes\n"},
			hides:   []string{"\r"},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			result, err := terminalRecorder(t, 30*time.Second).terminal(context.Background(), row.command, row.answers)
			if err != nil {
				t.Fatalf("terminal: %v", err)
			}
			if result.exit != row.exit {
				t.Fatalf("exited %d, expected %d:\n%s", result.exit, row.exit, result.stdout)
			}
			for _, want := range row.shows {
				if !strings.Contains(result.stdout, want) {
					t.Fatalf("the terminal did not show %q:\n%q", want, result.stdout)
				}
			}
			for _, unwanted := range row.hides {
				if strings.Contains(result.stdout, unwanted) {
					t.Fatalf("the terminal showed %q:\n%q", unwanted, result.stdout)
				}
			}
			if result.stderr != "" {
				t.Fatalf("a terminal has one stream, and stderr carried %q", result.stderr)
			}
		})
	}
}

// An answer typed before the question would be echoed above it, and a person
// does not answer a question nobody asked. A command that ends without asking
// fails the step and says which prompt never came.
func TestTerminalRefusesAPromptThatNeverCame(t *testing.T) {
	t.Parallel()
	result, err := terminalRecorder(t, 30*time.Second).terminal(context.Background(),
		`echo "Nothing was approved."; exit 4`, []answer{{Prompt: "Type yes to approve: ", Type: "yes"}})
	if err == nil {
		t.Fatalf("a prompt that never came was accepted:\n%s", result.stdout)
	}
	for _, want := range []string{"Type yes to approve: ", "never typed", "Nothing was approved."} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not say %q: %v", want, err)
		}
	}
}

// A second prompt that matches only text printed before the first answer is
// not the second prompt: each is looked for after the one before it.
func TestTerminalLooksForEachPromptAfterTheOneBefore(t *testing.T) {
	t.Parallel()
	_, err := terminalRecorder(t, 30*time.Second).terminal(context.Background(),
		`printf 'again? '; read one; echo done`,
		[]answer{{Prompt: "again? ", Type: "yes"}, {Prompt: "again? ", Type: "yes"}})
	if err == nil || !strings.Contains(err.Error(), "never typed") {
		t.Fatalf("one prompt answered two answers: %v", err)
	}
}

// A command that hangs is stopped with everything it started, so the step
// ends at its timeout instead of waiting on a process that holds the terminal.
func TestTerminalStopsACommandThatOutlivesTheStep(t *testing.T) {
	t.Parallel()
	started := time.Now()
	_, err := terminalRecorder(t, 2*time.Second).terminal(context.Background(),
		`sleep 60 & printf 'Q: '; read reply; sleep 60`, []answer{{Prompt: "Q: ", Type: "yes"}})
	if err == nil || !strings.Contains(err.Error(), "did not finish within") {
		t.Fatalf("a command that outlived the step was not stopped: %v", err)
	}
	if waited := time.Since(started); waited > 20*time.Second {
		t.Fatalf("the step took %s to give up on a command with a two-second timeout", waited)
	}
}

func terminalRecorder(t *testing.T, timeout time.Duration) *recorder {
	t.Helper()
	return &recorder{
		root:        t.TempDir(),
		environment: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()},
		stepTimeout: timeout,
		log:         func(string, ...any) {},
	}
}
