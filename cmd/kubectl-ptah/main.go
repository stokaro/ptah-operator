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
	// or the person declined; the resource suspend, resume, refresh, set-source
	// or settle read was deleted and created again before the change reached
	// it; set-source met a source that is not an OCI artifact, an Apply
	// running, or a source another writer changed after it was read; or
	// settle met a schema that has not stopped on a proof whose target moved,
	// a stop that prints no value it reads, or a value that no longer matches
	// the resource or is not the one --expect names.
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
	case "history":
		return historyCommand(ctx, arguments[1:], stdout, stderr)
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
	case "install":
		return installCommand(ctx, arguments[1:], stdout, stderr)
	case "settle":
		return settleCommand(ctx, arguments[1:], stdout, stderr)
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

	reader, resolved, err := connector(*kubeconfig, *kubeContext, *namespace, *timeout)
	if err != nil {
		fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
		return exitFailed
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	show, err := readPlan(ctx, reader, resolved, flags.Arg(0), selection, format, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
		if errors.Is(err, planread.ErrNoPlan) || errors.Is(err, planread.ErrSchemaNotFound) {
			return exitAbsent
		}
		if errors.Is(err, planread.ErrUnreadFields) {
			fmt.Fprintf(stderr, "kubectl-ptah: -o %s prints the whole plan, and this plugin does not read all of it. "+
				"Without -o this command shows what it reads; a plugin at the operator's version prints the rest.\n", format)
		}
		return exitFailed
	}
	// Only here. Every check the store makes has held, so nothing half-verified
	// has reached the reader's terminal or the file they redirected it to.
	if err := show(); err != nil {
		fmt.Fprintf(stderr, "kubectl-ptah: write the plan: %v\n", err)
		return exitFailed
	}
	return exitOK
}

// readPlan reads a plan and returns what prints it.
//
// The text format is for a person, so it reads a plan a newer Ptah added a field
// to, and says so. The other formats are for a program, which takes what it gets
// for the whole plan: a pipe into a database would run the statements of a plan
// that can run differently from them. They read through planread.Load, as
// approve does, and refuse such a plan.
func readPlan(
	ctx context.Context,
	reader client.Reader,
	namespace, name string,
	selection planread.Selection,
	format planview.Format,
	stdout io.Writer,
) (func() error, error) {
	if format == planview.Text {
		shown, err := planread.LoadForDisplay(ctx, reader, namespace, name, selection)
		return func() error { return planview.RenderDisplay(stdout, shown) }, err
	}
	whole, err := planread.Load(ctx, reader, namespace, name, selection)
	return func() error { return planview.Render(stdout, whole, format) }, err
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
		details     = flags.Bool("details", false, "also print every status field this plugin knows: the operation claimed, the lock owed, the revision rows")
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

	load := migrationview.Load
	if *details {
		load = migrationview.LoadWithDetails
	}
	view, err := load(ctx, reader, resolved, flags.Arg(0))
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
		details     = flags.Bool("details", false, "also print every status field this plugin knows: the operation claimed, the proof owed, the lock owed")
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

	load := schemaview.Load
	if *details {
		load = schemaview.LoadWithDetails
	}
	view, err := load(ctx, reader, resolved, flags.Arg(0))
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
       kubectl ptah history schema/<name>|migration/<name> [flags]
       kubectl ptah approve schema/<name>|migration/<name> [flags]
       kubectl ptah suspend schema/<name>|migration/<name> [flags]
       kubectl ptah resume schema/<name>|migration/<name> [flags]
       kubectl ptah refresh schema/<name>|migration/<name> [flags]
       kubectl ptah set-source schema/<name>|migration/<name> --oci <ref> [flags]
       kubectl ptah settle schema/<name> [flags]
       kubectl ptah wait schema/<name>|migration/<name> --for <target> [flags]
       kubectl ptah install [flags]

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

A plan a newer Ptah wrote can carry fields this plugin does not read. plan names
them and prints no value of theirs. -o sql and -o json refuse such a plan, since
a program would take what they print for the whole plan, and approve refuses it
because an approval is a decision about the whole document.

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

--details adds, after the view, every status field this plugin's API version
knows: the operation the resource has claimed and what it knows of its Job,
the proof an Apply still owes, a Lease release still owed, when it was last
read and is read next, the execution binding, and for a migration the revision
rows and each run whole. With -o json it adds details.status, the status as
this plugin decodes it; a field a newer operator added is not in it.

  kubectl ptah schema storefront -n application --details
  kubectl ptah migration orders -n application --details -o json

Listing the approvals and acknowledgments takes list on them, which reading the
resource does not. Without it the command still prints the resource, with one
line that says why they are missing, and exits 0. Each view prints the five
newest; history prints them all. Events are kubectl's own:

  kubectl events -n application --for ptahschema/storefront
  kubectl events -n application --for ptahmigration/orders

history prints what a schema or a migration retained of its past: the
lifecycle journal the operator keeps in its status, entry by entry in the
order it recorded them, and every approval and run acknowledgment that names
this resource by name and UID, oldest first. It says from when the journal is
complete, tells a status with no journal from a journal with no entries, and
marks a plan or a decision an entry names that is no longer stored. The
journal keeps at most 64 entries, none more than 30 days older than the
newest: it is the operator's record, not an audit archive. It takes -o text
(the default) or json, and the same -n, --kubeconfig, --context and --timeout
flags. It needs get on the resource; listing its plans, approvals and
acknowledgments is optional, the way it is for schema and migration.

  kubectl ptah history schema/storefront -n application
  kubectl ptah history migration/orders -n application -o json

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
  --name <name>            name the approval (by default the API server
                           generates one from the resource's name, for a
                           printed approval too, so one file can be created
                           again; kubectl apply needs a name)
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
engine, its database realm or a proof whose target moved. A read-only
operation that failed gives way to the request when its retry delay ends; a
read already under way does not answer it. The request starts no Apply and
approves nothing, and that it was written does not mean the read has happened.

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

settle gives up the proof a schema owes its last Apply when the URL Secret
now names another target than the plan the Apply ran from: the schema stops
Blocked with ReconciliationFailed reason TargetIdentityChanged, and its message
ends with a kubectl annotate command. settle reads the value from that
message and writes the operator.ptah.run/settled-apply annotation as that
command does, and nothing else. It prints how the journal records the Apply's
end and what the operator gives up when it takes the statement: the proof, the
plan, the approval it consumed and the database lock, with no Apply recorded.
Under apply Always the next plan then applies without a person, unless it is
destructive or changes privileges. The operator takes the statement at the
next read of that target, up to spec.execution.failureRetryInterval later;
removing the annotation before then takes it back. settle sends nothing when
the annotation already holds the value. It changes nothing on a schema that
has not stopped there, on a stop that prints no value it reads, or on a value
that names another Apply than the proof owed, or that another read replaced
after settle read it. Without --expect it settles the stop it reads when it
runs, whichever target that names; --expect binds it to the value you read.

  --expect <value>         the value the stop printed when you read it, after
                           settled-apply= in its command; settle writes that
                           value or nothing
  --wait                   wait until the operator has taken the statement
                           and given the proof up; a proof a read of the
                           first target ends instead stops the wait
  --wait-timeout <dur>     how long --wait waits (default 5m)
  -o, --output <format>    text (the default), or yaml or json

  kubectl ptah settle schema/storefront -n application --expect "$VALUE" --wait

wait reads a schema or a migration until it reaches a target. Apart from a plan
already waiting for an approval, it counts nothing that was already true when
it began: a reading of the database has to begin after the wait does. For
in-sync, approval and suspended the operator has to have observed the
generation the resource had then.

  --for <target>           required; one of
                             in-sync    the resource rests in sync
                             approval   a plan waits for an approval, one
                                        already waiting included
                             applied    an Apply or a migration run ended
                                        after the wait began, confirmed by
                                        the reading afterwards the operator
                                        records on it
                             suspended  the operator reports it Suspended,
                                        with nothing in flight
                             read       the read a refresh asked for ended
  --read-request <value>   with --for read: the request refresh printed
  --plan <name|fp>         with --for applied: only this plan
  --timeout <duration>     how long to wait (default 5m)
  -o, --output <format>    text (the default), yaml or json

  kubectl ptah wait schema/storefront --for in-sync --timeout 10m -n application

A wait that times out names the phase and the condition that holds the
resource, as the last read found them, and what it still waits for. One that
cannot reach its target, such as an Apply that failed or a run whose outcome
nobody established, stops at once. So does a wait on a plan the resource's own
policy holds (a schema plan that is destructive while allowDestructive is
false, or any plan under apply Never), once the operator has planned it again
after the wait began, and one for in-sync, approval or applied on an artifact
the verification policy refuses. A run that applied before such a refusal ends
the wait with CompletionNotProved, naming the run, and so does one whose next
reading was of another database. A wait for read stops on a refusal the read it
asked for ended in, and not on one from before it. Two managers' clocks may
date the operator's records, so a wait counts a reading by a new stamp rather
than by which time is later, except on a run an operator older than 0.2.10
recorded.

install puts the operator into the cluster from the chart a release published
to oci://ghcr.io/stokaro/charts/ptah-operator. That chart carries its release's
manager, runner and executor digests and the verified Ptah version, so install
passes no image and every image stays pinned by digest. It runs helm upgrade
--install with Helm 4 or newer from PATH, prints that command line first, and
waits for the release to become ready. Each run installs exactly its flags,
with --reset-values: run again with the same flags, it applies the same
objects as a new Helm revision, and a run without --approver removes the
binding an earlier --approver made. It refuses a namespace whose release is
another version, since an upgrade needs helm --force-conflicts. Verify the
chart first, as the install page shows.

  --version <version>      the release to install (default: this plugin's
                           own; a build from a checkout has none, so it needs
                           this)
  -n, --namespace <name>   the release namespace (default ptah-system)
  --approver <username>    bind the chart's approver ClusterRole to this user
                           in every namespace; repeat it for more
  -f, --values <file>      a values file for the chart; repeat it for more.
                           The chart takes an image only with its pair, as
                           it does from helm
  --yaml                   print the objects install would apply instead,
                           rendered by helm template from the same chart,
                           version and values; reads no cluster, so the
                           chart's cluster checks do not run, and holds a
                           webhook CA key generated for this render
  --out-dir <dir>          with --yaml, write them under <dir>/ptah-operator
                           rather than to standard output
  --timeout <duration>     how long helm waits for the release (default 5m)

  kubectl ptah install --version 0.2.11
  kubectl ptah install --version 0.2.11 --approver alice@example.com
  kubectl ptah install --version 0.2.11 --yaml --out-dir ./deploy

suspend, resume, refresh, set-source and settle patch the resource as you:
they need get and patch on ptahschemas or ptahmigrations. wait needs get.
install needs what the chart needs, which is cluster-admin. The resource is
named as schema/NAME or migration/NAME, or by the kind's plural or short name
as kubectl spells it.

Exit status: 0 printed, changed, reached or installed, 1 could not read or
verify, or helm did not install or render, 2 usage, 3 nothing stored to print,
approve, change or wait on, 4 nothing approved or changed (the plan or the spec
changed, the operator would not admit an approval now, the fingerprint is not
the current plan's, you declined, the resource was deleted and created again,
set-source met a source that is not an OCI artifact, an Apply running, or a
source changed after it was read, or settle met a schema that has not stopped
on a moved target, a stop that prints no value, or a value that no longer
matches or is not the one --expect names), 5 refused by RBAC or the API server,
6 sent with no answer: the change may have been stored, 7 the wait timed out, 8
the resource will not reach the target.
`)
}
