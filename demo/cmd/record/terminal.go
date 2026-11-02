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
// exited. The read ends by itself once nothing holds the terminal; a process
// the command left running in the background could hold it open, and the step
// would then wait for that process instead of for the command.
const terminalDrain = 5 * time.Second

// terminal runs one command on a pseudo-terminal, the way a person at a
// terminal runs it, and types each answer once the command has printed its
// prompt.
//
// The command's stdin, stdout and stderr are the terminal, so a command that
// asks a person for a decision asks here too, and refuses nothing it would
// accept from a person. What the command printed and what the terminal echoed
// of the answers come back as one stream, in the order the terminal showed
// them, as the outcome's stdout.
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

	talk := &conversation{controller: controller, answers: answers}
	read := make(chan struct{})
	go func() {
		defer close(read)
		talk.read()
	}()
	exited := make(chan error, 1)
	go func() { exited <- process.Wait() }()

	var waited error
	select {
	case waited = <-exited:
	case <-ctx.Done():
		// The session leader's process group is the shell and everything it
		// started, so killing it leaves nothing holding the terminal.
		_ = syscall.Kill(-process.Process.Pid, syscall.SIGKILL)
		<-exited
		drain(read)
		return outcome{stdout: talk.text()}, fmt.Errorf("%q did not finish within %s", firstLine(command), r.stepTimeout)
	}
	drain(read)

	result := outcome{stdout: talk.text()}
	var exit *exec.ExitError
	switch {
	case waited == nil:
	case errors.As(waited, &exit):
		result.exit = exit.ExitCode()
	default:
		return result, fmt.Errorf("run %q: %w", firstLine(command), waited)
	}
	if err := talk.failure(); err != nil {
		return result, fmt.Errorf("%q: %w\n\nthe terminal showed:\n%s", firstLine(command), err, indent(result.stdout))
	}
	return result, nil
}

func drain(read <-chan struct{}) {
	select {
	case <-read:
	case <-time.After(terminalDrain):
	}
}

// conversation is what the terminal showed, and the answers typed into it.
type conversation struct {
	controller *os.File
	answers    []answer

	mu    sync.Mutex
	shown bytes.Buffer
	// next is the answer typed next, and from is where in shown its prompt is
	// looked for: a prompt counts once, and only after the one before it.
	next int
	from int
	err  error
}

// read copies the terminal into shown until nothing holds the terminal: a
// read then fails, with EIO on Linux and EOF elsewhere.
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
	}
}

// text is what the terminal showed, with its line endings as a file has them.
func (c *conversation) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.ReplaceAll(strings.ReplaceAll(c.shown.String(), "\r\n", "\n"), "\r", "")
}

// failure says why the conversation did not go as the scenario wrote it.
func (c *conversation) failure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	if c.next < len(c.answers) {
		missed := c.answers[c.next]
		return fmt.Errorf("the command ended without printing %q, so %q was never typed", missed.Prompt, missed.Type)
	}
	return nil
}
