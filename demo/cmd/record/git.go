package main

import (
	"fmt"
	"os/exec"
	"strings"
)

// gitOutput runs one git command and returns its trimmed output.
func gitOutput(command string) (string, error) {
	fields := strings.Fields(command)
	output, err := exec.Command(fields[0], fields[1:]...).Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w", command, err)
	}
	return strings.TrimSpace(string(output)), nil
}
