package main

import (
	"fmt"
	"os/exec"
	"strings"
)

// gitLines runs one git command and returns its output as lines.
//
// Only the trailing newline is removed. `git status --porcelain` puts the
// staged and unstaged status in the first two columns, and the first of them is
// a space for an unstaged change -- so trimming the output as a whole eats a
// column and takes the first character of the first path with it.
func gitLines(command string) ([]string, error) {
	fields := strings.Fields(command)
	output, err := exec.Command(fields[0], fields[1:]...).Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", command, err)
	}
	text := strings.TrimRight(string(output), "\n")
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}

// gitOutput runs one git command and returns its trimmed output.
func gitOutput(command string) (string, error) {
	fields := strings.Fields(command)
	output, err := exec.Command(fields[0], fields[1:]...).Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w", command, err)
	}
	return strings.TrimSpace(string(output)), nil
}
