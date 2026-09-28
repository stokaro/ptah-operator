// Command resultassert extracts one exact, integrity-bound runner result from
// an E2E Pod log and prints it as JSON. It is package resultframe behind three
// flags, for the shell phases that read results: it deliberately reuses the
// production parser so a phase cannot accept a frame the controller would
// reject.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/test/e2e/resultframe"
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

	logs, err := resultframe.ReadBounded(*logsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "resultassert: read bounded runner log")
		os.Exit(1)
	}
	result, err := resultframe.Parse(logs, runner.Operation(*operation), *operationID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resultassert: %v\n", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, "resultassert: encode validated result")
		os.Exit(1)
	}
}
