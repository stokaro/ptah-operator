//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// terminalDrain bounds how long the transcript is read after the command has
// exited. On macOS the read ends when the command exits, since the terminal
// goes with the session the command led. On Linux it ends only once no process
// holds the terminal, and a process the command left running in the
// background still does; the step would then wait for that process instead of
// for the command. Past the bound the recorder closes its end, which is what
// returns that read.
const terminalDrain = 5 * time.Second

// defaultPromptTimeout is how long a step waits for each prompt it answers,
// unless the recorder says otherwise. kubectl ptah approve asks within seconds
// of starting; a prompt that has not come in a minute is not coming, and
// waiting the step's whole timeout for it hides nothing but the reason.
const defaultPromptTimeout = time.Minute

// terminal runs one command on a pseudo-terminal, the way a person at a
// terminal runs it, and types each answer once the command has printed its
// prompt.
//
// The command's stdin, stdout and stderr are the terminal, so a command that
// asks a person for a decision asks here too, and refuses nothing it would
// accept from a person. What the command printed and what the terminal echoed
// of the answers come back as one stream, in the order the terminal showed
// them, as the outcome's stdout. A step that fails returns that stream too,
// and its error carries it: a prompt that never came is explained by what came
// instead.
func (r *recorder) terminal(ctx context.Context, command string, answers []answer) (outcome, error) {
	ctx, cancel := context.WithTimeout(ctx, r.stepTimeout)
	defer cancel()

	controller, replica, err := openTerminal()
	if err != nil {
		return outcome{}, fmt.Errorf("open a terminal for %q: %w", firstLine(command), err)
	}
	defer func() { _ = controller.Close() }()

	process := exec.Command("/bin/sh", "-c", command)
	process.Dir = r.root
	process.Env = r.environment
	process.Stdin, process.Stdout, process.Stderr = replica, replica, replica
	// A session of its own, with the terminal as its controlling terminal:
	// what a login shell gives the commands a person types.
	process.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := process.Start(); err != nil {
		_ = replica.Close()
		return outcome{}, fmt.Errorf("run %q: %w", firstLine(command), err)
	}
	// The command holds the terminal now. Closing this copy is what lets the
	// read below end once the command, and everything it started, has closed
	// its own.
	_ = replica.Close()

	talk := &conversation{controller: controller, answers: answers, typed: make(chan struct{}, len(answers))}
	read := make(chan struct{})
	go func() {
		defer close(read)
		talk.read()
	}()
	exited := make(chan error, 1)
	go func() { exited <- process.Wait() }()

	// transcript waits for the read to end and returns what the terminal
	// showed. The controller is still open for the read, and closing it past
	// the drain bound ends a read a background process would hold.
	transcript := func() string {
		select {
		case <-read:
		case <-time.After(terminalDrain):
			_ = controller.Close()
			<-read
		}
		return talk.text()
	}
	// stop kills the session leader's process group, the shell and
	// everything it started, so nothing is left holding the terminal.
	stop := func() {
		_ = syscall.Kill(-process.Process.Pid, syscall.SIGKILL)
		<-exited
	}
	failed := func(cause error) (outcome, error) {
		shown := transcript()
		return outcome{stdout: shown}, fmt.Errorf("%q: %w\n\nthe terminal showed:\n%s",
			firstLine(command), cause, indent(shown))
	}

	// A prompt has a deadline of its own while the command runs, counted from
	// the start and again from each answer: a prompt that is not coming would
	// otherwise hold the step for its whole timeout.
	promptTimeout := r.promptTimeout
	if promptTimeout <= 0 {
		promptTimeout = defaultPromptTimeout
	}
	prompt := time.NewTimer(promptTimeout)
	defer prompt.Stop()
	var waited error
wait:
	for {
		select {
		case waited = <-exited:
			break wait
		case <-talk.typed:
			if talk.unanswered("") == nil {
				prompt.Stop()
			} else {
				prompt.Reset(promptTimeout)
			}
		case <-prompt.C:
			missed := talk.unanswered("within " + promptTimeout.String())
			if missed == nil {
				// Typed as the deadline passed.
				continue
			}
			stop()
			return failed(missed)
		case <-ctx.Done():
			stop()
			cause := fmt.Errorf("did not finish within %s", r.stepTimeout)
			if missed := talk.unanswered("before it was stopped"); missed != nil {
				cause = fmt.Errorf("%w; %w", cause, missed)
			}
			return failed(cause)
		}
	}

	// The whole transcript first: a prompt the command printed just before it
	// exited is answered, or not, by the time the read ends.
	result := outcome{stdout: transcript()}
	var exit *exec.ExitError
	switch {
	case waited == nil:
	case errors.As(waited, &exit):
		result.exit = exit.ExitCode()
	default:
		return failed(waited)
	}
	if err := talk.unanswered("before it ended"); err != nil {
		return failed(err)
	}
	return result, nil
}

// conversation is what the terminal showed, and the answers typed into it.
type conversation struct {
	controller *os.File
	answers    []answer
	// typed receives once per answer typed. It holds one for every answer,
	// so typing never waits for the step to look.
	typed chan struct{}

	mu    sync.Mutex
	shown bytes.Buffer
	// next is the answer typed next, and from is where in shown its prompt is
	// looked for: a prompt counts once, and only after the one before it.
	next int
	from int
	err  error
}

// read copies the terminal into shown until the read fails: with EOF on
// macOS once the command has exited, with EIO on Linux once nothing holds the
// terminal, or because the recorder closed its end.
func (c *conversation) read() {
	buffer := make([]byte, 4096)
	for {
		count, err := c.controller.Read(buffer)
		if count > 0 {
			c.mu.Lock()
			c.shown.Write(buffer[:count])
			c.answerPrompts()
			c.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// answerPrompts types every answer whose prompt the terminal has shown.
func (c *conversation) answerPrompts() {
	for c.next < len(c.answers) && c.err == nil {
		current := c.answers[c.next]
		at := bytes.Index(c.shown.Bytes()[c.from:], []byte(current.Prompt))
		if at < 0 {
			return
		}
		c.from += at + len(current.Prompt)
		if _, err := c.controller.Write([]byte(current.Type + "\n")); err != nil {
			c.err = fmt.Errorf("type %q: %w", current.Type, err)
			return
		}
		c.next++
		c.typed <- struct{}{}
	}
}

// unanswered says why an answer is still owed, or nothing once every answer
// has been typed. when says when the prompt was due.
func (c *conversation) unanswered(when string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	if c.next == len(c.answers) {
		return nil
	}
	missed := c.answers[c.next]
	return fmt.Errorf("the command printed no %q %s, so %q was never typed", missed.Prompt, when, missed.Type)
}

// text is what the terminal showed, with its line endings as a file has them.
func (c *conversation) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.ReplaceAll(strings.ReplaceAll(c.shown.String(), "\r\n", "\n"), "\r", "")
}
