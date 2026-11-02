// Command kubectl-ptah reads what the Ptah operator stored, and records the
// decisions a person makes about it.
//
// The read commands create nothing and change nothing: they resolve a stored
// plan, hand its reconstruction to the operator's own store, and print what
// came back. approve creates one approval of the plan a person reviewed, as
// that person, and only while the cluster still names that plan. No command
// connects to a database.
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
	"github.com/stokaro/ptah-operator/internal/schemaview"
	"github.com/stokaro/ptah-operator/pkg/planread"
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
	// exitAbsent covers a resource or a plan that is not there. It is
	// separate because "there is nothing to show" is an answer, and printing
	// empty SQL with a zero status would be a wrong one.
	exitAbsent = 3
	// exitNotDone covers a decision or a change this program did not make:
	// approve's plan or spec changed during review, the operator would not
	// admit an approval now, the fingerprint given is not the current plan's,
	// or the person declined; the resource suspend, resume, refresh or set-source
	// read was deleted and created again before the change reached it; or
	// set-source met a source that is not an OCI artifact, an Apply running,
	// or a source another writer changed after it was read.
	exitNotDone = 4
	// exitRefused covers a write the API server refused with a 4xx, in its
	// own words: RBAC, admission, the schema, a name already taken, a
	// credential it did not accept, or a request it was too busy to take.
	// Nothing was stored.
	exitRefused = 5
	// exitUnknown covers a write that was sent and whose fate no answer told.
	// It may have been stored.
	exitUnknown = 6
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
	case "schema":
		return schema(ctx, arguments[1:], stdout, stderr)
	case "approve":
		return approveCommand(ctx, arguments[1:], os.Stdin, stdinIsTerminal(), stdout, stderr)
	case "suspend":
		return suspendCommand(ctx, arguments[1:], true, stdout, stderr)
	case "resume":
		return suspendCommand(ctx, arguments[1:], false, stdout, stderr)
	case "refresh":
		return refreshCommand(ctx, arguments[1:], stdout, stderr)
	case "wait":
		return waitCommand(ctx, arguments[1:], stdout, stderr)
	case "set-source":
		return setSourceCommand(ctx, arguments[1:], stdout, stderr)
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
		fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
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
	selection := planread.Current
	if *applied {
		selection = planread.Applied
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

	view, err := planread.Load(ctx, reader, resolved, flags.Arg(0), selection)
	if err != nil {
		fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
		if errors.Is(err, planread.ErrNoPlan) || errors.Is(err, planread.ErrSchemaNotFound) {
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
// stands and what holds it, what the database's own history said, which
// sequence it would run next, what the last run did, a run nobody accounted
// for, and the approvals and run acknowledgments that name it.
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
		fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
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

// schema prints where one PtahSchema stands: what holds it, what the last
// observation found, including how far the declared reference rows sit from
// the database, which plan is waiting for a decision, and the approvals that
// name it.
func schema(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	flags := pflag.NewFlagSet("kubectl ptah schema", pflag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { usage(stderr) }

	var (
		output      = flags.StringP("output", "o", string(schemaview.Text), "output format: text or json")
		namespace   = flags.StringP("namespace", "n", "", "namespace of the schema (default: the kubeconfig context's)")
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
		fmt.Fprintf(stderr, "kubectl-ptah: name one PtahSchema\n\n")
		usage(stderr)
		return exitUsage
	}
	format := schemaview.Format(*output)
	if !knownSchemaFormat(format) {
		fmt.Fprintf(stderr, "kubectl-ptah: unknown output format %q; it is one of %v\n", *output, schemaview.Formats)
		return exitUsage
	}

	reader, resolved, err := connect(*kubeconfig, *kubeContext, *namespace, *timeout)
	if err != nil {
		fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
		return exitFailed
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	view, err := schemaview.Load(ctx, reader, resolved, flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
		if errors.Is(err, schemaview.ErrSchemaNotFound) {
			return exitAbsent
		}
		return exitFailed
	}
	if err := schemaview.Render(stdout, view, format); err != nil {
		fmt.Fprintf(stderr, "kubectl-ptah: write the schema: %v\n", err)
		return exitFailed
	}
	return exitOK
}

func knownSchemaFormat(format schemaview.Format) bool {
	for _, candidate := range schemaview.Formats {
		if candidate == format {
			return true
		}
	}
	return false
}

func knownMigrationFormat(format migrationview.Format) bool {
	for _, candidate := range migrationview.Formats {
		if candidate == format {
			return true
		}
	}
	return false
}

// connect builds an uncached client from the person's own kubeconfig, and
// returns the namespace the command will use. The client carries that
// person's credentials and whatever RBAC grants them: the read commands only
// read through it, and approve creates one approval through it, as them.
func connect(kubeconfig, contextName, namespace string, timeout time.Duration) (client.Client, string, error) {
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
       kubectl ptah schema <schema> [flags]
       kubectl ptah migration <migration> [flags]
       kubectl ptah approve schema/<name>|migration/<name> [flags]
       kubectl ptah suspend schema/<name>|migration/<name> [flags]
       kubectl ptah resume schema/<name>|migration/<name> [flags]
       kubectl ptah refresh schema/<name>|migration/<name> [flags]
       kubectl ptah set-source schema/<name>|migration/<name> --oci <ref> [flags]
       kubectl ptah wait schema/<name>|migration/<name> --for <target> [flags]

plan prints the SQL of a plan the Ptah operator stored, after the operator's
own checks on it have held, each statement under a line that gives its severity
and the reason Ptah gave. schema prints where one PtahSchema stands: what holds
it, the policy its spec asks for, what the last observation found, how far its
declared reference rows sit from the database, which plan is waiting, the
approvals that name it, and every condition with its message. migration prints
where one PtahMigration stands: what holds it, what the database's own history
said, which sequence would run next, what the last run did, a run nobody has
accounted for, the approvals and run acknowledgments that name it, and every
condition with its message.

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

-o sql prints the statements alone, with no review line, for a script or a
diff.

schema and migration each take -o text (the default) or json, plus the same
-n, --kubeconfig, --context and --timeout flags. Neither prints SQL or a table
row. A migration plan records versions and checksums, and the statements live
in the artifact. A schema reports its declared reference rows as counts -- how
many the database is missing, holds differently, and holds that the declaration
no longer has -- because which rows those are is in the plan, and reading a
data plan is reading values.

  kubectl ptah schema storefront -n application
  kubectl ptah schema storefront -n application -o json
  kubectl ptah migration orders -n application
  kubectl ptah migration orders -n application -o json

Listing the approvals and acknowledgments takes list on them, which reading the
resource does not. Without it the command still prints the resource, with one
line that says why they are missing, and exits 0. Events are kubectl's own:

  kubectl events -n application --for ptahschema/storefront
  kubectl events -n application --for ptahmigration/orders

approve shows the current plan of a schema or a migration and the identities
an approval of it carries, asks for a decision, and creates the approval as
you. It reads the resource again first and sends nothing if the plan was
replaced, the resource was recreated, its spec changed other than by a
suspend or a resume, or the operator would not admit an approval now. The
operator reads a waiting resource again at its interval, and admission takes
no approval while it does: an answer given then is sent when that read ends,
if the same plan is waiting again, and approve waits up to 5m for that.
Admission records who approved and when; RBAC decides whether you may.

  --plan-fingerprint <fp>  decide without asking, only if the current plan
                           carries this fingerprint (for a script that
                           reviewed it); like an answer on a terminal, it
                           waits up to 5m for an interval read of the
                           operator to end before it sends
  --dry-run client         print the approval as YAML and send nothing; it
                           takes the same decision as an approval does.
                           Whoever creates the printed object is recorded
                           as its approver, a GitOps controller included:
                           create it as the person who decided
  --dry-run server         let admission check the approval; store nothing
  --name <name>            name the approval (generated by default; with
                           --dry-run client, from the resource's name and
                           the plan's fingerprint)
  --wait                   once the approval is stored, wait until the
                           operator has applied the plan and read the
                           database afterwards
  --timeout <duration>     how long each request to the API server may take
                           (default 30s); the wait for an interval read is
                           bounded by 5m, not by this
  -o, --output <format>    text (the default), yaml or json

  kubectl ptah approve schema/storefront -n application
  kubectl ptah approve migration/orders -n application --wait
  kubectl ptah approve schema/storefront -n application --dry-run client > approval.yaml
  kubectl create -f approval.yaml

Admission records whoever creates a printed approval as its approver, a GitOps
controller that applies it included: create it as the person who decided,
with kubectl create rather than apply.

suspend and resume set spec.suspend on a schema or a migration and change
nothing else; resume clears it. Suspension prevents new Jobs and cancels no
SQL. An Apply already running is observed to its end; any proof still owed
after an Apply waits for resume; a read under way, a migration's read of its
history after a run included, is dropped, since it changes nothing.
Suspending while an Apply runs records its outcome as unknown, so let an
Apply finish first. Each command prints the state it asked for beside what
the operator had observed and what the suspension does to the work in
flight, and sends nothing when spec.suspend already holds the value. After
resume the operator runs any proof still owed, reads the database again and
decides from what it finds.

refresh asks the operator to read the database now rather than at the next
interval, through the operator.ptah.run/read-requested-at annotation. The
operator takes the request once no operation is running, the resource is not
suspended and no retry delay is pending, and not while it is Blocked by its
engine or its database realm; a read already under way does not answer it.
The request starts no Apply and approves nothing, and that it was written
does not mean the read has happened.

  --wait                   wait for the operator: suspend until it reports
                           Suspended with nothing in flight, resume until
                           the resource reaches --for, refresh until the
                           read it asked for has ended
  --for <target>           resume only: in-sync (the default), approval or
                           applied; naming one implies --wait
  --wait-timeout <dur>     how long --wait waits (default 5m)
  -o, --output <format>    text (the default), or yaml or json: the
                           resource as the API server answered the change,
                           or as the wait last read it

  kubectl ptah suspend schema/storefront -n application --wait
  kubectl ptah resume migration/orders -n application --for approval
  kubectl ptah refresh migration/orders -n application --wait

set-source points a schema or a migration at another OCI artifact. It sets
spec.desired.ociRef of a schema or spec.artifact.ociRef of a migration and
nothing else: the verification policy, the registry credential, the transport
and the apply policy stay as they are, and the new artifact is held to them.
It takes the reference pinned by digest, so the resource runs the artifact you
name and not whatever a tag names later, and it resolves no tag: ptah schema
push --format reference and ptah migrations push --format reference print the
reference of the artifact a push wrote, and ptah oci resolve <reference> the
one a tag names now. It sends nothing when the field already holds the
reference or while an Apply runs, since a spec change then records the
Apply's outcome as unknown, and it changes nothing when another writer changed
the source after it was read. It changes no ConfigMap or Git source: moving to
an artifact also names the verification policy it is held to, and that is an
edit of the resource.

  --oci <reference>        required: oci://HOST/REPOSITORY@sha256:<64 hex>
  --wait                   wait until the operator has planned from the new
                           reference: a plan waits for an approval, or the
                           resource rests in sync, whichever comes first.
                           It stops on a schema plan the policy refuses
  --for <target>           approval or in-sync, to wait for that one alone;
                           naming one implies --wait. A wait for approval
                           stops if the resource rests in sync instead
  --wait-timeout <dur>     how long --wait waits (default 5m)
  -o, --output <format>    text (the default), or yaml or json

  kubectl ptah set-source schema/storefront -n application \
    --oci oci://registry.example/acme/storefront@sha256:<digest> --wait

The operator resolves the new reference, verifies it, reads the database and
plans again, and a plan or an approval of the previous artifact does not carry
over. Under apply policy OnApproval the new plan waits for an approval; under
Always it runs without a person, except a schema plan that is destructive or
changes privileges. On a suspended resource nothing of this happens until it
is resumed.

wait reads a schema or a migration until it reaches a target, and counts
nothing that was already true when it began: a reading of the database has to
begin after the wait does, and for in-sync, approval and suspended the
operator has to have observed the generation the resource had then.

  --for <target>           required; one of
                             in-sync    the resource rests in sync
                             approval   a fresh plan waits for an approval
                             applied    an Apply or a migration run ended
                                        after the wait began, confirmed by
                                        a reading afterwards
                             suspended  the operator reports it Suspended,
                                        with nothing in flight
                             read       the read a refresh asked for ended
  --read-request <value>   with --for read: the request refresh printed
  --plan <name|fp>         with --for applied, for a schema: only this plan
  --timeout <duration>     how long to wait (default 5m)
  -o, --output <format>    text (the default), yaml or json

  kubectl ptah wait schema/storefront --for in-sync --timeout 10m -n application

A wait that times out names the phase and the condition that holds the
resource, as the last read found them, and what it still waits for. One that
cannot reach its target, such as an Apply that failed, a run whose outcome
nobody established, or a schema plan its own policy refuses (destructive while
allowDestructive is false, or any plan under apply Never), stops at once.

suspend, resume, refresh and set-source patch the resource as you: they need
get and patch on ptahschemas or ptahmigrations. wait needs get. The resource is
named as schema/NAME or migration/NAME, or by the kind's plural or short name
as kubectl spells it.

Exit status: 0 printed, changed or reached, 1 could not read or verify, 2
usage, 3 nothing stored to print, approve, change or wait on, 4 nothing
approved or changed (the plan or the spec changed, the operator would not
admit an approval now, the fingerprint is not the current plan's, you
declined, the resource was deleted and created again, or set-source met a
source that is not an OCI artifact, an Apply running, or a source changed
after it was read), 5
refused by RBAC or the API server, 6 sent with no answer: the change may have
been stored, 7 the wait timed out, 8 the resource will not reach the target.
`)
}
