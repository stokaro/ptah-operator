// Command resultassert extracts one exact, integrity-bound runner result from
// an E2E Pod log. It deliberately reuses the production parser so the test
// cannot accept a frame the controller would reject.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/stokaro/ptah-operator/internal/runner"
)

const (
	frameHeader = "PTAH_RUNNER_RESULT_V1 "
	frameFooter = "\nPTAH_RUNNER_RESULT_END_V1"
	maxLogBytes = runner.DefaultMaxFrameBytes + 1<<20
)

func main() {
	logsPath := flag.String("logs", "", "path to the exact ptah-runner container log")
	operation := flag.String("operation", "", "expected runner operation")
	operationID := flag.String("operation-id", "", "expected immutable operation ID")
	flag.Parse()
	if flag.NArg() != 0 || *logsPath == "" || *operationID == "" ||
		!runner.Operation(*operation).Valid() {
		fmt.Fprintln(os.Stderr, "resultassert: --logs, --operation, and --operation-id are required")
		os.Exit(2)
	}

	logs, err := readBounded(*logsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "resultassert: read bounded runner log")
		os.Exit(1)
	}
	result, err := parseExactResult(logs, runner.Operation(*operation), *operationID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resultassert: %v\n", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, "resultassert: encode validated result")
		os.Exit(1)
	}
}

func readBounded(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	logs, err := io.ReadAll(io.LimitReader(file, maxLogBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(logs)) > maxLogBytes {
		return nil, errors.New("runner log exceeds the E2E parser limit")
	}
	return logs, nil
}

func parseExactResult(logs []byte, operation runner.Operation, operationID string) (runner.Result, error) {
	if bytes.Count(logs, []byte(frameHeader)) != 1 ||
		bytes.Count(logs, []byte(frameFooter)) != 1 {
		return runner.Result{}, errors.New("runner log does not contain exactly one complete result frame")
	}
	result, err := runner.ParseResultFor(logs, operation, operationID)
	if err != nil {
		return runner.Result{}, fmt.Errorf("parse integrity-bound runner result: %w", err)
	}
	return result, nil
}
