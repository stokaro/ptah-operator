// Package resultframe extracts one exact, integrity-bound runner result from
// an acceptance Pod's log. It reuses the production parser, so an acceptance
// row cannot accept a frame the controller would reject. The data-plane phase
// reads every result it asserts on through it.
package resultframe

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"

	"github.com/stokaro/ptah-operator/internal/runner"
)

const (
	frameHeader = "PTAH_RUNNER_RESULT_V1 "
	frameFooter = "\nPTAH_RUNNER_RESULT_END_V1"

	// MaxLogBytes bounds a log the parser reads: the largest frame the runner
	// writes, and a megabyte of output before it.
	MaxLogBytes = runner.DefaultMaxFrameBytes + 1<<20
)

// arrivingPattern matches the refusals Parse gives a log that may still be
// arriving, and nothing else. A caller that can read the log again waits on
// these and gives up at once on any other; a test holds every alternative to a
// log shape that is genuinely still arriving.
const arrivingPattern = `never finished arriving|frame not found|no end of line within its bounds`

var arriving = regexp.MustCompile(arrivingPattern)

// StillArriving reports whether err is a refusal a later read of the same log
// can turn into a frame.
func StillArriving(err error) bool {
	return err != nil && arriving.MatchString(err.Error())
}

// Parse reads the one frame the log must hold. It separates the two answers a
// caller that can read the log again has to decide between, because one
// sentence covering both is a refusal such a caller cannot act on.
//
// Fewer markers than a closed frame carries is a log read while the runner's
// last write was still being copied into it: the frame may still be arriving,
// and reading again can produce it. Nothing is decided here for that case.
// runner.ParseResultWithOptions already separates a header that has not
// finished, a payload short of the length its header declares and a payload
// with no footer from a frame that is present and malformed, and it names each.
// Answering all of them with a sentence of this package's own is what hid them:
// the callers that read a transport again do so only for the parser's own
// incomplete reasons, so from #155 until this was split, a log that was still
// arriving was refused on its first read and the bounded wait never ran for the
// case it was written for.
//
// More than one header or footer is the other answer. The log is all there and
// holds more than the one frame this package extracts -- the production parser
// returns the last valid frame it finds, so it would accept a log carrying two
// -- and no later read can reduce them. That refusal says so, in words no
// caller mistakes for a reason to wait.
func Parse(logs []byte, operation runner.Operation, operationID string) (runner.Result, error) {
	if int64(len(logs)) > MaxLogBytes {
		return runner.Result{}, errors.New("runner log exceeds the E2E parser limit")
	}
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
