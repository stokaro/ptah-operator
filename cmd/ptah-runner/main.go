package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/stokaro/ptah-operator/internal/runner"
)

func main() {
	os.Exit(runUntilSignaled(os.Args[1:], os.Stdout, os.Stderr, os.Environ(), runner.TerminationMessagePath))
}

// runUntilSignaled runs the runner under a context that SIGTERM and SIGINT
// cancel.
//
// The runner is PID 1 in its container, so the kubelet's SIGTERM on eviction,
// preemption, a drain or a Pod deadline reaches it and not the child. Without a
// handler it died where it stood, the child was killed with it, and the Job
// ended with no frame, which the controller can only record as unknown. With
// one, the cancellation reaches the child as SIGTERM, the runner reads what the
// child wrote once it stopped, and frames that, all inside the Pod's grace.
//
// Delivery stays captured until the process exits, so a second signal does not
// kill the runner while it writes its frame: the kubelet's SIGKILL at the end
// of the grace is the only bound, and the stop delay is sized under it.
func runUntilSignaled(arguments []string, stdout, stderr io.Writer, environment []string, terminationLog string) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	return run(ctx, arguments, stdout, stderr, environment, terminationLog)
}

func run(ctx context.Context, arguments []string, stdout, stderr io.Writer, environment []string, terminationLog string) int {
	flags := flag.NewFlagSet("ptah-runner", flag.ContinueOnError)
	flags.SetOutput(stderr)
	ptahBinary := flags.String("ptah-binary", "ptah", "path to the Ptah executable")
	maxResultBytes := flags.Int64("max-result-bytes", runner.DefaultMaxResultBytes, "maximum retained bytes for each child output stream")
	maxPlanBytes := flags.Int64("max-plan-bytes", runner.DefaultMaxPlanBytes, "maximum complete executable plan bytes")
	operationFlag := flags.String("operation", "", "operation: resolve, verify, observe, plan, apply, migration-history, or migration-apply")
	installTo := flags.String("install-to", "", "copy this executable to the fixed Job runner path")
	validateOCISource := flags.String("validate-oci-source", "", "validate OCI source authority grants without network access")
	resultEndpoint := flags.String("result-endpoint", "", "HTTPS origin for durable result delivery")
	resultCredentials := flags.String("result-credentials", "", "directory containing result-delivery tls.crt, tls.key, and ca.crt")
	snapshotOCICATo := flags.String("snapshot-oci-ca-to", "", "copy a validated OCI CA to an exclusive snapshot path")
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
	validateMode := false
	flags.Visit(func(current *flag.Flag) {
		if current.Name == "validate-oci-source" {
			validateMode = true
		}
	})
	if validateMode {
		unexpectedFlag := false
		flags.Visit(func(current *flag.Flag) {
			if current.Name != "validate-oci-source" && current.Name != "snapshot-oci-ca-to" {
				unexpectedFlag = true
			}
		})
		if unexpectedFlag || flags.NArg() != 0 {
			_, _ = fmt.Fprintln(stderr, "ptah-runner: OCI source validation mode accepts only --validate-oci-source")
			return 2
		}
		// The guard authorizes the fetch that runs next with the registry
		// credentials, so it is held to the protocol as the main run is.
		if err := runner.CheckProtocolBinding(environment); err != nil {
			_, _ = fmt.Fprintln(stderr, "ptah-runner: "+runner.CodeRunnerProtocolMismatch+": "+err.Error())
			return 2
		}
		var err error
		if *snapshotOCICATo == "" {
			err = runner.ValidateOCISourceAccess(*validateOCISource, environment)
		} else {
			err = runner.SnapshotOCISourceAccess(*validateOCISource, environment, *snapshotOCICATo)
		}
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "ptah-runner: OCI source access is not authorized")
			return 2
		}
		return 0
	}
	snapshotMode := false
	flags.Visit(func(current *flag.Flag) {
		if current.Name == "snapshot-oci-ca-to" {
			snapshotMode = true
		}
	})
	if snapshotMode {
		_, _ = fmt.Fprintln(stderr, "ptah-runner: --snapshot-oci-ca-to requires --validate-oci-source")
		return 2
	}
	operation := runner.Operation(*operationFlag)
	if operation == "" && flags.NArg() == 1 {
		operation = runner.Operation(flags.Arg(0))
	} else if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "ptah-runner: provide exactly one operation")
		return 2
	}
	if !operation.Valid() {
		_, _ = fmt.Fprintln(stderr, "ptah-runner: operation must be resolve, verify, observe, plan, apply, migration-history, or migration-apply")
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

	var delivery *runnerDelivery
	deliveryRequested := false
	flags.Visit(func(current *flag.Flag) {
		if current.Name == "result-endpoint" || current.Name == "result-credentials" {
			deliveryRequested = true
		}
	})
	if deliveryRequested {
		// A foreign protocol cannot publish a receipt under this Job's contract.
		// Refuse before contacting the receiver or starting the executor.
		if err := runner.CheckProtocolBinding(environment); err != nil {
			_, _ = fmt.Fprintln(stderr, "ptah-runner: "+runner.CodeRunnerProtocolMismatch+": "+err.Error())
			return 2
		}
		var err error
		delivery, err = prepareDelivery(*resultEndpoint, *resultCredentials, operation, environment)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "ptah-runner: invalid result delivery configuration")
			return 2
		}
		defer delivery.sender.Close()
		if err := delivery.sender.Check(ctx); err != nil {
			_, _ = fmt.Fprintln(stderr, "ptah-runner: result receiver preflight failed")
			return 2
		}
	}

	result := runner.Run(ctx, runner.Config{
		DurableResult:  delivery != nil,
		Operation:      operation,
		PtahBinary:     *ptahBinary,
		MaxResultBytes: *maxResultBytes,
		MaxPlanBytes:   *maxPlanBytes,
		Environment:    environment,
		Diagnostics:    stderr,
	})
	if delivery != nil {
		return delivery.deliver(ctx, result, stderr, terminationLog)
	}
	encoded, err := runner.EncodeResult(result)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "ptah-runner: could not write the result frame")
		return 2
	}
	// The summary goes first. It is small and local, while the frame may be
	// megabytes through the container runtime's log pipe, so a runner killed
	// part way through its frame has usually left the summary behind it. The
	// summary names the frame's digest, so a log holding part of that frame
	// still agrees with it.
	switch {
	case encoded.SummaryErr != nil:
		_, _ = fmt.Fprintln(stderr, "ptah-runner: the result has no termination summary: "+encoded.SummaryErr.Error())
	case terminationLog != "":
		if err := runner.WriteTerminationSummary(terminationLog, encoded.Summary); err != nil {
			_, _ = fmt.Fprintln(stderr, "ptah-runner: could not write the termination summary")
		}
	}
	written, err := stdout.Write(encoded.Frame)
	if err == nil && written != len(encoded.Frame) {
		err = io.ErrShortWrite
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "ptah-runner: could not write the result frame")
		return 2
	}
	// A complete frame is the Job transport success. Child and operation-level
	// failures remain explicit in the authenticated result payload.
	return 0
}
