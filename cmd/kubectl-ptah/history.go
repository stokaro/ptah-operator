package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/pflag"

	"github.com/stokaro/ptah-operator/internal/historyview"
	"github.com/stokaro/ptah-operator/internal/operate"
)

// historyCommand prints what one schema or migration retained of its past:
// the lifecycle journal its status keeps, which period that covers, and every
// approval and run acknowledgment that names it. It reads; it changes nothing.
func historyCommand(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	flags := pflag.NewFlagSet("kubectl ptah history", pflag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { usage(stderr) }

	var (
		output      = flags.StringP("output", "o", string(historyview.Text), "output format: text or json")
		namespace   = flags.StringP("namespace", "n", "", "namespace of the resource (default: the kubeconfig context's)")
		kubeconfig  = flags.String("kubeconfig", "", "path to a kubeconfig file (default: KUBECONFIG, then ~/.kube/config)")
		kubeContext = flags.String("context", "", "kubeconfig context to use (default: the current one)")
		timeout     = flags.Duration("timeout", 30*time.Second, "how long to wait for the API server")
	)
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return exitOK
		}
		fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
		return exitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintf(stderr, "kubectl-ptah: name one resource, as schema/NAME or migration/NAME\n\n")
		usage(stderr)
		return exitUsage
	}
	// The namespace is resolved once the kubeconfig is read.
	ref, err := operate.ParseRef(flags.Arg(0), "unresolved")
	if err != nil {
		fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
		return exitUsage
	}
	format := historyview.Format(*output)
	if !knownHistoryFormat(format) {
		fmt.Fprintf(stderr, "kubectl-ptah: unknown output format %q; it is one of %v\n", *output, historyview.Formats)
		return exitUsage
	}
	kind := historyview.Schema
	if ref.Family == operate.FamilyMigration {
		kind = historyview.Migration
	}

	reader, resolved, err := connector(*kubeconfig, *kubeContext, *namespace, *timeout)
	if err == nil && resolved == "" {
		err = errors.New("no namespace: pass -n or set one in the kubeconfig context")
	}
	if err != nil {
		fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
		return exitFailed
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	view, err := historyview.Load(ctx, reader, kind, resolved, ref.Name)
	if err != nil {
		fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
		if errors.Is(err, historyview.ErrNotFound) {
			return exitAbsent
		}
		return exitFailed
	}
	if err := historyview.Render(stdout, view, format); err != nil {
		fmt.Fprintf(stderr, "kubectl-ptah: write the history: %v\n", err)
		return exitFailed
	}
	return exitOK
}

func knownHistoryFormat(format historyview.Format) bool {
	for _, candidate := range historyview.Formats {
		if candidate == format {
			return true
		}
	}
	return false
}
