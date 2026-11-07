// Command runner-harness-recorder is the runner from this commit, in the form
// the harness runs inside the executor image. It stands at the path the Job
// installs ptah-runner to and takes the arguments the Job passes it, and calls
// the runner package in process with a recording Executor where cmd/ptah-runner
// passes runner.OSExecutor. Instead of a frame on standard output it prints the
// transcript: every child, and the frame.
//
// A row that needs the plan validation's --dry-run where no operation reaches
// it adds --append-arg. The harness adds nothing else to the Job's arguments.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/test/runnerharness/recording"
	"github.com/stokaro/ptah-operator/test/runnerharness/volumes"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Environ()))
}

type repeated []string

func (r *repeated) String() string { return fmt.Sprint([]string(*r)) }

func (r *repeated) Set(value string) error {
	*r = append(*r, value)
	return nil
}

func run(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer, environment []string) int {
	flags := flag.NewFlagSet("runner-harness-recorder", flag.ContinueOnError)
	flags.SetOutput(stderr)
	ptahBinary := flags.String("ptah-binary", "ptah", "path to the Ptah executable")
	maxResultBytes := flags.Int64("max-result-bytes", runner.DefaultMaxResultBytes, "maximum retained bytes for each child output stream")
	maxPlanBytes := flags.Int64("max-plan-bytes", runner.DefaultMaxPlanBytes, "maximum complete executable plan bytes")
	operation := flags.String("operation", "", "the operation")
	validateOCISource := flags.String("validate-oci-source", "", "validate OCI source authority grants")
	snapshotOCICATo := flags.String("snapshot-oci-ca-to", "", "copy a validated OCI CA here")
	fetchGitSource := flags.String("fetch-git-source", "", "fetch the resolved commit of a Git source into this directory")
	fetchOCISchema := flags.String("fetch-oci-schema", "", "pull this schema artifact")
	fetchOCISchemaTo := flags.String("fetch-oci-schema-to", "", "file a pulled schema artifact is written to")
	prepareVolumes := flags.String("prepare-volumes", "", "unpack the volumes on standard input under this directory")
	var appendArgs repeated
	flags.Var(&appendArgs, "append-arg", "add this argument to every child")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "runner-harness-recorder: unexpected arguments")
		return 2
	}
	executor := &recording.Executor{AppendArgs: appendArgs}
	var transcript recording.Transcript
	code := 0
	switch {
	case *prepareVolumes != "":
		// Run as root on a volume, before the Pod's own containers, in place
		// of the kubelet.
		if err := volumes.Unpack(stdin, *prepareVolumes); err != nil {
			_, _ = fmt.Fprintln(stderr, "runner-harness-recorder: "+err.Error())
			return 2
		}
		return 0
	case *validateOCISource != "":
		if err := validate(*validateOCISource, *snapshotOCICATo, environment); err != nil {
			transcript.Refusal = err.Error()
			code = 2
		}
	case *fetchGitSource != "":
		// As cmd/ptah-runner does, the fetch is held to the protocol first.
		err := runner.CheckProtocolBinding(environment)
		if err != nil {
			err = fmt.Errorf("%s: %w", runner.CodeRunnerProtocolMismatch, err)
		} else {
			err = runner.FetchGitSource(ctx, *ptahBinary, environment, *fetchGitSource, "", executor, stderr)
		}
		if err != nil {
			transcript.Refusal = err.Error()
			code = 2
		}
	case *fetchOCISchema != "":
		// As cmd/ptah-runner does, the fetch is held to the protocol first. What
		// it would write to the termination message goes in the transcript: only
		// a Pod has that file.
		var fetched runner.SchemaFetch
		err := runner.CheckProtocolBinding(environment)
		if err != nil {
			err = fmt.Errorf("%s: %w", runner.CodeRunnerProtocolMismatch, err)
		} else {
			fetched, err = runner.FetchOCISchema(ctx, *ptahBinary, environment, *fetchOCISchema, *fetchOCISchemaTo, executor, stderr)
		}
		switch {
		case err != nil:
			transcript.Refusal = err.Error()
			code = 2
		case fetched.ExitCode != 0:
			transcript.Refusal = fmt.Sprintf("the schema pull exited %d", fetched.ExitCode)
			code = fetched.ExitCode
		}
		transcript.TerminationMessage = string(fetched.TerminationMessage)
	default:
		parsed := runner.Operation(*operation)
		if !parsed.Valid() {
			_, _ = fmt.Fprintf(stderr, "runner-harness-recorder: unknown operation %q\n", *operation)
			return 2
		}
		result := runner.Run(ctx, runner.Config{
			Operation:      parsed,
			PtahBinary:     *ptahBinary,
			MaxResultBytes: *maxResultBytes,
			MaxPlanBytes:   *maxPlanBytes,
			Environment:    environment,
			Diagnostics:    stderr,
			Executor:       executor,
		})
		encoded, err := runner.EncodeResult(result)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "runner-harness-recorder: could not write the result frame: "+err.Error())
			code = 2
			break
		}
		transcript.Frame = encoded.Frame
		if encoded.SummaryErr != nil {
			transcript.SummaryError = encoded.SummaryErr.Error()
		}
	}
	transcript.Children = executor.Children()
	if err := json.NewEncoder(stdout).Encode(transcript); err != nil {
		_, _ = fmt.Fprintln(stderr, "runner-harness-recorder: could not write the transcript: "+err.Error())
		return 2
	}
	return code
}

func validate(reference, snapshotTo string, environment []string) error {
	// The guard is held to the protocol as the main run is.
	if err := runner.CheckProtocolBinding(environment); err != nil {
		return fmt.Errorf("%s: %w", runner.CodeRunnerProtocolMismatch, err)
	}
	if snapshotTo == "" {
		return runner.ValidateOCISourceAccess(reference, environment)
	}
	return runner.SnapshotOCISourceAccess(reference, environment, snapshotTo)
}
