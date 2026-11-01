package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"golang.org/x/term"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/stokaro/ptah-operator/internal/approve"
)

// Exit statuses of approve, beside the ones every command shares.
const (
	// exitNotApproved covers an approval this command did not send: the
	// plan changed during review, the operator would not admit one now, the
	// fingerprint given is not the current plan's, or the person declined.
	exitNotApproved = 4
	// exitRefused covers an approval the API server refused: RBAC or
	// admission, in their own words.
	exitRefused = 5
	// exitUnknown covers an approval that was sent and whose fate no answer
	// told. It may have been stored.
	exitUnknown = 6
)

func stdinIsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// connector opens the cluster for a command. Tests replace it.
var connector = connect

func approveCommand(ctx context.Context, arguments []string, stdin io.Reader, interactive bool, stdout, stderr io.Writer) int {
	flags := pflag.NewFlagSet("kubectl ptah approve", pflag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { usage(stderr) }

	var (
		fingerprint = flags.String("plan-fingerprint", "", "approve without asking, only if the current plan carries this fingerprint")
		name        = flags.String("name", "", "name of the approval (default: generated from the resource's name)")
		dryRun      = flags.String("dry-run", "none", "none, client (print the approval; send nothing) or server (admission checks it; nothing is stored)")
		output      = flags.StringP("output", "o", "text", "text, yaml or json")
		namespace   = flags.StringP("namespace", "n", "", "namespace of the resource (default: the kubeconfig context's)")
		kubeconfig  = flags.String("kubeconfig", "", "path to a kubeconfig file (default: KUBECONFIG, then ~/.kube/config)")
		kubeContext = flags.String("context", "", "kubeconfig context to use (default: the current one)")
		timeout     = flags.Duration("timeout", 30*time.Second, "how long to wait for the API server")
	)
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintf(stderr, "kubectl-ptah: name one resource, as schema/NAME or migration/NAME\n\n")
		usage(stderr)
		return exitUsage
	}
	family, resource, err := approve.ParseTarget(flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
		return exitUsage
	}
	switch *dryRun {
	case "none", "client", "server":
	default:
		fmt.Fprintf(stderr, "kubectl-ptah: --dry-run is none, client or server, not %q\n", *dryRun)
		return exitUsage
	}
	switch *output {
	case "text", "yaml", "json":
	default:
		fmt.Fprintf(stderr, "kubectl-ptah: unknown output format %q; it is one of [text yaml json]\n", *output)
		return exitUsage
	}

	cluster, resolved, err := connector(*kubeconfig, *kubeContext, *namespace, *timeout)
	if err != nil {
		fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
		return exitFailed
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	review, err := approve.Load(ctx, cluster, family, resolved, resource)
	switch {
	case errors.Is(err, approve.ErrNotFound), errors.Is(err, approve.ErrNothingToApprove):
		fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
		return exitAbsent
	case err != nil:
		fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
		return exitFailed
	}

	if *dryRun == "client" {
		// The manifest for a declarative workflow: exact identities, no
		// identity of the approver, which admission stamps from whoever
		// applies it. Nothing is sent.
		if !review.Open {
			fmt.Fprintf(stderr, "kubectl-ptah: %s Admission would refuse this approval now.\n", review.Closed)
		}
		format := *output
		if format == "text" {
			format = "yaml"
		}
		return printObject(stdout, stderr, approve.Manifest(review, *name), format)
	}

	if *fingerprint == "" {
		if !interactive {
			fmt.Fprintf(stderr, "kubectl-ptah: approve asks for a decision on a terminal. Without one, pass --plan-fingerprint with the fingerprint of the plan you reviewed (kubectl ptah plan %s shows it).\n", resource)
			return exitUsage
		}
		if err := approve.RenderReview(stdout, review); err != nil {
			fmt.Fprintf(stderr, "kubectl-ptah: write the plan: %v\n", err)
			return exitFailed
		}
	} else if *fingerprint != review.Fingerprint {
		fmt.Fprintf(stderr, "kubectl-ptah: the current plan of %s/%s is %s with fingerprint %s, not the %s given. Nothing was approved; review the current plan.\n",
			family, resource, review.PlanName, review.Fingerprint, *fingerprint)
		return exitNotApproved
	}
	if !review.Open {
		fmt.Fprintf(stderr, "kubectl-ptah: %s Nothing was approved.\n", review.Closed)
		return exitNotApproved
	}
	if *fingerprint == "" {
		fmt.Fprintf(stderr, "\nApprove plan %s of %s/%s in %s? Type yes to approve: ", review.PlanName, family, resource, resolved)
		answer, _ := bufio.NewReader(stdin).ReadString('\n')
		if strings.TrimSpace(answer) != "yes" {
			fmt.Fprintln(stderr, "Nothing was approved.")
			return exitNotApproved
		}
	}

	result := approve.Submit(ctx, cluster, review, approve.Options{Name: *name, ServerDryRun: *dryRun == "server"})
	switch result.Outcome {
	case approve.Admitted, approve.Previewed:
		if *output != "text" {
			return printObject(stdout, stderr, result.Approval, *output)
		}
		approver, at := result.Approver()
		verb := "Approved"
		if result.Outcome == approve.Previewed {
			verb = "Admission accepts this approval; nothing was stored (server dry run). It would be"
		}
		when := ""
		if at != nil && !at.IsZero() {
			when = " at " + at.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(stdout, "%s: %s %s/%s, recorded by admission as %s%s.\nIt binds plan %s, fingerprint %s.\n",
			verb, result.Approval.GetObjectKind().GroupVersionKind().Kind, resolved, result.Approval.GetName(), approver, when,
			review.PlanName, review.Fingerprint)
		if result.Outcome == approve.Admitted {
			fmt.Fprintf(stdout, "The operator runs it under the resource's policy; kubectl ptah %s %s -n %s shows how far it got.\n", family, resource, resolved)
		}
		return exitOK
	case approve.Changed, approve.Closed:
		fmt.Fprintf(stderr, "kubectl-ptah: %s\n", result.Message)
		return exitNotApproved
	case approve.Forbidden, approve.Refused:
		fmt.Fprintf(stderr, "kubectl-ptah: %s\nNothing was approved.\n", result.Message)
		return exitRefused
	case approve.Unknown:
		fmt.Fprintf(stderr, "kubectl-ptah: %s\n", result.Message)
		return exitUnknown
	default:
		fmt.Fprintf(stderr, "kubectl-ptah: %s\n", result.Message)
		return exitFailed
	}
}

// printObject writes an approval as YAML or JSON, the way kubectl prints one.
func printObject(stdout, stderr io.Writer, object client.Object, format string) int {
	object.SetManagedFields(nil)
	var (
		data []byte
		err  error
	)
	if format == "json" {
		data, err = json.MarshalIndent(object, "", "  ")
		data = append(data, '\n')
	} else {
		data, err = yaml.Marshal(object)
	}
	if err != nil {
		fmt.Fprintf(stderr, "kubectl-ptah: encode the approval: %v\n", err)
		return exitFailed
	}
	if _, err := stdout.Write(data); err != nil {
		fmt.Fprintf(stderr, "kubectl-ptah: write the approval: %v\n", err)
		return exitFailed
	}
	return exitOK
}
