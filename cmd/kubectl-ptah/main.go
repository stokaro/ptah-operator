// Command kubectl-ptah reads what the Ptah operator stored.
//
// It is a read-only client. It creates nothing, changes nothing and connects to
// no database: what it does is resolve a stored plan, hand its reconstruction
// to the operator's own store, and print what came back.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/migrationview"
	"github.com/stokaro/ptah-operator/internal/planview"
)

// version is stamped at release time. A build from a checkout says so.
var version = "dev"

// Exit statuses, which scripts may rely on.
const (
	exitOK = 0
	// exitFailed covers a plan that could not be read or verified.
	exitFailed = 1
	// exitUsage covers a command line this program cannot act on.
	exitUsage = 2
	// exitAbsent covers a schema or a plan that is not there. It is separate
	// because "there is nothing to show" is an answer, and printing empty SQL
	// with a zero status would be a wrong one.
	exitAbsent = 3
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	if len(arguments) > 0 && arguments[0] == "--version" {
		fmt.Fprintf(stdout, "kubectl-ptah %s\n", version)
		return exitOK
	}
	if len(arguments) == 0 || arguments[0] == "-h" || arguments[0] == "--help" || arguments[0] == "help" {
		usage(stdout)
		if len(arguments) == 0 {
			return exitUsage
		}
		return exitOK
	}
	switch arguments[0] {
	case "plan":
		return plan(ctx, arguments[1:], stdout, stderr)
	case "migration":
		return migration(ctx, arguments[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "kubectl-ptah: unknown command %q\n\n", arguments[0])
		usage(stderr)
		return exitUsage
	}
}

func plan(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	// pflag rather than the standard library's flag, because a reader writes
	// `plan storefront --applied`, and flag stops parsing at the first
	// argument that is not a flag -- which would leave --applied read as a
	// second schema name.
	flags := pflag.NewFlagSet("kubectl ptah plan", pflag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { usage(stderr) }

	var (
		current     = flags.Bool("current", false, "the plan the operator would run next")
		applied     = flags.Bool("applied", false, "the plan the last confirmed apply ran")
		output      = flags.StringP("output", "o", string(planview.Text), "output format: text, sql or json")
		namespace   = flags.StringP("namespace", "n", "", "namespace of the schema (default: the kubeconfig context's)")
		kubeconfig  = flags.String("kubeconfig", "", "path to a kubeconfig file (default: KUBECONFIG, then ~/.kube/config)")
		kubeContext = flags.String("context", "", "kubeconfig context to use (default: the current one)")
		timeout     = flags.Duration("timeout", 30*time.Second, "how long to wait for the API server")
	)
	if err := flags.Parse(arguments); err != nil {
		// Asking for the help is not a mistake, and a script that reads the
		// status should not be told it made one.
		if errors.Is(err, pflag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintf(stderr, "kubectl-ptah: name one PtahSchema\n\n")
		usage(stderr)
		return exitUsage
	}
	if *current && *applied {
		fmt.Fprintf(stderr, "kubectl-ptah: --current and --applied are two different plans; ask for one\n")
		return exitUsage
	}
	selection := planview.Current
	if *applied {
		selection = planview.Applied
	}
	format := planview.Format(*output)
	if !known(format) {
		fmt.Fprintf(stderr, "kubectl-ptah: unknown output format %q; it is one of %v\n", *output, planview.Formats)
		return exitUsage
	}

	reader, resolved, err := connect(*kubeconfig, *kubeContext, *namespace, *timeout)
	if err != nil {
		fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
		return exitFailed
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	view, err := planview.Load(ctx, reader, resolved, flags.Arg(0), selection)
	if err != nil {
		fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
		if errors.Is(err, planview.ErrNoPlan) || errors.Is(err, planview.ErrSchemaNotFound) {
			return exitAbsent
		}
		return exitFailed
	}
	// Only here. Every check the store makes has held, so nothing half-verified
	// has reached the reader's terminal or the file they redirected it to.
	if err := planview.Render(stdout, view, format); err != nil {
		fmt.Fprintf(stderr, "kubectl-ptah: write the plan: %v\n", err)
		return exitFailed
	}
	return exitOK
}

// migration prints what the operator stored about one PtahMigration: where it
// stands, what the database's own history said, which sequence it would run
// next, and what the last run did.
func migration(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	flags := pflag.NewFlagSet("kubectl ptah migration", pflag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { usage(stderr) }

	var (
		output      = flags.StringP("output", "o", string(migrationview.Text), "output format: text or json")
		namespace   = flags.StringP("namespace", "n", "", "namespace of the migration (default: the kubeconfig context's)")
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
		fmt.Fprintf(stderr, "kubectl-ptah: name one PtahMigration\n\n")
		usage(stderr)
		return exitUsage
	}
	format := migrationview.Format(*output)
	if !knownMigrationFormat(format) {
		fmt.Fprintf(stderr, "kubectl-ptah: unknown output format %q; it is one of %v\n", *output, migrationview.Formats)
		return exitUsage
	}

	reader, resolved, err := connect(*kubeconfig, *kubeContext, *namespace, *timeout)
	if err != nil {
		fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
		return exitFailed
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	view, err := migrationview.Load(ctx, reader, resolved, flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
		if errors.Is(err, migrationview.ErrMigrationNotFound) {
			return exitAbsent
		}
		return exitFailed
	}
	if err := migrationview.Render(stdout, view, format); err != nil {
		fmt.Fprintf(stderr, "kubectl-ptah: write the migration: %v\n", err)
		return exitFailed
	}
	return exitOK
}

func knownMigrationFormat(format migrationview.Format) bool {
	for _, candidate := range migrationview.Formats {
		if candidate == format {
			return true
		}
	}
	return false
}

// connect builds a read-only client from the reader's own kubeconfig, and
// returns the namespace the command will use.
func connect(kubeconfig, contextName, namespace string, timeout time.Duration) (client.Reader, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if contextName != "" {
		overrides.CurrentContext = contextName
	}
	config := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	if namespace == "" {
		fromContext, _, err := config.Namespace()
		if err != nil {
			return nil, "", fmt.Errorf("read the namespace from the kubeconfig: %w", err)
		}
		namespace = fromContext
	}
	restConfig, err := config.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("read the kubeconfig: %w", err)
	}
	restConfig.Timeout = timeout
	restConfig.UserAgent = "kubectl-ptah/" + version

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, "", fmt.Errorf("register the core types: %w", err)
	}
	if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
		return nil, "", fmt.Errorf("register the operator types: %w", err)
	}
	// Reads go straight to the API server: a client that cached would answer
	// about a plan from whenever it last looked.
	reader, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, "", fmt.Errorf("connect to the cluster: %w", err)
	}
	return reader, namespace, nil
}

func known(format planview.Format) bool {
	for _, candidate := range planview.Formats {
		if candidate == format {
			return true
		}
	}
	return false
}

func usage(out io.Writer) {
	fmt.Fprint(out, `usage: kubectl ptah plan <schema> [flags]
       kubectl ptah migration <migration> [flags]

plan prints the SQL of a plan the Ptah operator stored, after the operator's
own checks on it have held. migration prints where one PtahMigration stands:
what the database's own history said, which sequence would run next, and what
the last run did.

  --current              the plan the operator would run next (the default)
  --applied              the plan the last confirmed apply ran
  -o, --output <format>  text (the default), sql, or json
  -n, --namespace <name> namespace of the schema; the kubeconfig context's by default
  --kubeconfig <path>    kubeconfig to read; KUBECONFIG, then ~/.kube/config
  --context <name>       kubeconfig context; the current one by default
  --timeout <duration>   how long to wait for the API server (default 30s)
  --version              print the version and exit

Current and applied are two different plans and neither stands in for the
other. Applied shows the stored plan the last confirmed apply ran; it is not a
log of what each statement did, and not a fresh reading of the database.

  kubectl ptah plan storefront -n application
  kubectl ptah plan storefront --applied -n application -o sql
  kubectl ptah plan storefront --applied -n application -o json > plan.json

migration takes -o text (the default) or json, plus the same -n, --kubeconfig,
--context and --timeout flags. It prints no SQL and no table row: a migration
plan records versions and checksums, and the statements live in the artifact.

  kubectl ptah migration orders -n application
  kubectl ptah migration orders -n application -o json

Exit status: 0 printed, 1 could not read or verify, 2 usage, 3 nothing stored
to print.
`)
}
