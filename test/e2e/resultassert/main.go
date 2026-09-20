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

// parseExactResult reads the one frame the log must hold. It separates the two
// answers a caller that can read the log again has to decide between, because
// one sentence covering both is a refusal such a caller cannot act on.
//
// Fewer markers than a closed frame carries is a log read while the runner's
// last write was still being copied into it: the frame may still be arriving,
// and reading again can produce it. Nothing is decided here for that case.
// runner.ParseResultWithOptions already separates a header that has not
// finished, a payload short of the length its header declares and a payload
// with no footer from a frame that is present and malformed, and it names each.
// Answering all of them with a sentence of this command's own is what hid them:
// read_result_transport in hack/e2e-dataplane.sh and hack/e2e-faults.sh reads a
// transport again only for the parser's own incomplete reasons, so from #155
// until this was split, a log that was still arriving was refused on its first
// read and the bounded wait never ran for the case it was written for.
//
// More than one header or footer is the other answer. The log is all there and
// holds more than the one frame this command extracts -- the production parser
// returns the last valid frame it finds, so it would accept a log carrying two
// -- and no later read can reduce them. That refusal says so, in words no
// caller mistakes for a reason to wait.
func parseExactResult(logs []byte, operation runner.Operation, operationID string) (runner.Result, error) {
	if headers := bytes.Count(logs, []byte(frameHeader)); headers > 1 {
		return runner.Result{}, fmt.Errorf(
			"the runner log carries %d result frame headers rather than one, and reading it again cannot reduce them",
			headers,
		)
	}
	if footers := bytes.Count(logs, []byte(frameFooter)); footers > 1 {
		return runner.Result{}, fmt.Errorf(
			"the runner log carries %d result frame footers rather than one, and reading it again cannot reduce them",
			footers,
		)
	}
	result, err := runner.ParseResultFor(logs, operation, operationID)
	if err != nil {
		return runner.Result{}, fmt.Errorf("parse integrity-bound runner result: %w", err)
	}
	return result, nil
}
