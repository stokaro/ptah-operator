package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/stokaro/ptah-operator/internal/runner"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, os.Environ()))
}

func run(ctx context.Context, arguments []string, stdout, stderr io.Writer, environment []string) int {
	flags := flag.NewFlagSet("ptah-runner", flag.ContinueOnError)
	flags.SetOutput(stderr)
	ptahBinary := flags.String("ptah-binary", "ptah", "path to the Ptah executable")
	maxResultBytes := flags.Int64("max-result-bytes", runner.DefaultMaxResultBytes, "maximum retained bytes for each child output stream")
	maxPlanBytes := flags.Int64("max-plan-bytes", runner.DefaultMaxPlanBytes, "maximum complete executable plan bytes")
	operationFlag := flags.String("operation", "", "operation: resolve, verify, observe, plan, or apply")
	installTo := flags.String("install-to", "", "copy this executable to the fixed Job runner path")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if *installTo != "" {
		unexpectedFlag := false
		flags.Visit(func(current *flag.Flag) {
			if current.Name != "install-to" {
				unexpectedFlag = true
			}
		})
		if unexpectedFlag || flags.NArg() != 0 {
			_, _ = fmt.Fprintln(stderr, "ptah-runner: install mode accepts only --install-to")
			return 2
		}
		if err := installSelf(*installTo); err != nil {
			_, _ = fmt.Fprintln(stderr, "ptah-runner: "+err.Error())
			return 2
		}
		return 0
	}
	operation := runner.Operation(*operationFlag)
	if operation == "" && flags.NArg() == 1 {
		operation = runner.Operation(flags.Arg(0))
	} else if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "ptah-runner: provide exactly one operation")
		return 2
	}
	if !operation.Valid() {
		_, _ = fmt.Fprintln(stderr, "ptah-runner: operation must be resolve, verify, observe, plan, or apply")
		return 2
	}
	if *maxResultBytes <= 0 {
		_, _ = fmt.Fprintln(stderr, "ptah-runner: max-result-bytes must be positive")
		return 2
	}
	if *maxPlanBytes <= 0 {
		_, _ = fmt.Fprintln(stderr, "ptah-runner: max-plan-bytes must be positive")
		return 2
	}
	if *maxResultBytes > runner.DefaultMaxResultBytes || *maxPlanBytes > runner.DefaultMaxPlanBytes {
		_, _ = fmt.Fprintln(stderr, "ptah-runner: byte limits exceed the supported execution contract")
		return 2
	}
	if (operation == runner.OperationPlan || operation == runner.OperationApply) && *maxPlanBytes > *maxResultBytes {
		_, _ = fmt.Fprintln(stderr, "ptah-runner: max-plan-bytes must not exceed max-result-bytes")
		return 2
	}

	result := runner.Run(ctx, runner.Config{
		Operation:      operation,
		PtahBinary:     *ptahBinary,
		MaxResultBytes: *maxResultBytes,
		MaxPlanBytes:   *maxPlanBytes,
		Environment:    environment,
		Diagnostics:    stderr,
	})
	if err := runner.WriteFrame(stdout, result); err != nil {
		_, _ = fmt.Fprintln(stderr, "ptah-runner: could not write the result frame")
		return 2
	}
	// A complete frame is the Job transport success. Child and operation-level
	// failures remain explicit in the authenticated result payload.
	return 0
}
