package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/pflag"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/operate"
	"github.com/stokaro/ptah-operator/internal/statusview"
)

// Exit statuses of a wait: kubectl ptah wait, and the commands that change
// something when they are asked to --wait.
const (
	// exitTimedOut covers a wait whose timeout passed before the resource
	// reached the target. It may still reach it.
	exitTimedOut = 7
	// exitWillNotReach covers a wait the resource ended: it recorded an
	// outcome the target cannot follow, it is being deleted, or another
	// resource now holds its name.
	exitWillNotReach = 8
)

const (
	// apiTimeout bounds one conversation with the API server: the default of
	// --timeout for a change, and the bound of each read a wait makes.
	apiTimeout = 30 * time.Second
	// defaultWaitTimeout is how long a wait waits unless told otherwise.
	defaultWaitTimeout = 5 * time.Minute
)

// commandFlags is a flag set with the flags every command that reaches the
// cluster takes.
type commandFlags struct {
	*pflag.FlagSet
	namespace   *string
	kubeconfig  *string
	kubeContext *string
	output      *string
}

func newCommandFlags(command string, stderr io.Writer) commandFlags {
	flags := pflag.NewFlagSet("kubectl ptah "+command, pflag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { usage(stderr) }
	return commandFlags{
		FlagSet:     flags,
		namespace:   flags.StringP("namespace", "n", "", "namespace of the resource (default: the kubeconfig context's)"),
		kubeconfig:  flags.String("kubeconfig", "", "path to a kubeconfig file (default: KUBECONFIG, then ~/.kube/config)"),
		kubeContext: flags.String("context", "", "kubeconfig context to use (default: the current one)"),
		output:      flags.StringP("output", "o", "text", "text, yaml or json"),
	}
}

// parse reads the command line and the one resource it names. It returns the
// status to exit with when the command should go no further.
func (flags commandFlags) parse(arguments []string, stderr io.Writer) (operate.Ref, int, bool) {
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return operate.Ref{}, exitOK, true
		}
		// pflag says nothing of an error it returns rather than exits on.
		fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
		return operate.Ref{}, exitUsage, true
	}
	if flags.NArg() != 1 {
		fmt.Fprintf(stderr, "kubectl-ptah: name one resource, as schema/NAME or migration/NAME\n\n")
		usage(stderr)
		return operate.Ref{}, exitUsage, true
	}
	// The namespace is resolved once the kubeconfig is read; a command line
	// that names no resource is refused before that.
	ref, err := operate.ParseRef(flags.Arg(0), "unresolved")
	if err != nil {
		fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
		return operate.Ref{}, exitUsage, true
	}
	switch *flags.output {
	case "text", "yaml", "json":
	default:
		fmt.Fprintf(stderr, "kubectl-ptah: unknown output format %q; it is one of [text yaml json]\n", *flags.output)
		return operate.Ref{}, exitUsage, true
	}
	return ref, exitOK, false
}

// connect opens the cluster for ref and resolves its namespace.
func (flags commandFlags) connect(ref operate.Ref, timeout time.Duration, stderr io.Writer) (client.Client, operate.Ref, bool) {
	cluster, resolved, err := connector(*flags.kubeconfig, *flags.kubeContext, *flags.namespace, timeout)
	if err == nil && resolved == "" {
		err = errors.New("no namespace: pass -n or set one in the kubeconfig context")
	}
	if err != nil {
		fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
		return nil, operate.Ref{}, false
	}
	ref.Namespace = resolved
	return cluster, ref, true
}

// waitFlags are the flags that make a change wait for the operator.
type waitFlags struct {
	wait    *bool
	timeout *time.Duration
}

func addWaitFlags(flags commandFlags, what string) waitFlags {
	return waitFlags{
		wait:    flags.Bool("wait", false, "wait until "+what),
		timeout: flags.Duration("wait-timeout", defaultWaitTimeout, "how long --wait waits"),
	}
}

func (w waitFlags) valid(stderr io.Writer) bool {
	if *w.wait && *w.timeout <= 0 {
		fmt.Fprintf(stderr, "kubectl-ptah: --wait-timeout has to be longer than zero, not %s\n", *w.timeout)
		return false
	}
	return true
}

// suspendCommand runs suspend and resume: it sets spec.suspend, reports what
// it asked for beside what the operator had observed, and waits for the
// operator if asked to.
func suspendCommand(ctx context.Context, arguments []string, suspend bool, stdout, stderr io.Writer) int {
	verb, target, what := "resume", operate.TargetInSync, "the resource reaches --for"
	if suspend {
		verb, target, what = "suspend", operate.TargetSuspended, "the operator reports the resource Suspended with nothing in flight"
	}
	flags := newCommandFlags(verb, stderr)
	timeout := flags.Duration("timeout", apiTimeout, "how long to wait for the API server")
	waiting := addWaitFlags(flags, what)
	var waitFor *string
	if !suspend {
		waitFor = flags.String("for", string(operate.TargetInSync), "what --wait waits for: in-sync, approval or applied")
	}
	ref, code, done := flags.parse(arguments, stderr)
	if done {
		return code
	}
	if waitFor != nil && flags.Changed("for") {
		// Naming what to wait for is asking to wait.
		*waiting.wait = true
		parsed, err := operate.ParseTarget(*waitFor)
		switch {
		case err != nil:
			fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
			return exitUsage
		case parsed != operate.TargetInSync && parsed != operate.TargetApproval && parsed != operate.TargetApplied:
			fmt.Fprintf(stderr, "kubectl-ptah: resume waits for in-sync, approval or applied, not %s\n", parsed)
			return exitUsage
		}
		target = parsed
	}
	if !waiting.valid(stderr) {
		return exitUsage
	}

	cluster, ref, ok := flags.connect(ref, *timeout, stderr)
	if !ok {
		return exitFailed
	}
	changeCtx, cancel := context.WithTimeout(ctx, *timeout)
	result, err := operate.SetSuspend(changeCtx, cluster, ref, suspend)
	cancel()
	if err != nil {
		return changeFailed(stderr, ref, verb, err)
	}
	if *flags.output == "text" {
		writeSuspendReport(stdout, result, *waiting.wait)
	} else if !*waiting.wait {
		return printResource(stdout, stderr, ref, result.Object, *flags.output)
	}
	if !*waiting.wait {
		return exitOK
	}
	// The wait has a deadline of its own: the change's has been spent. The
	// answer to the change is its baseline, so nothing the resource showed
	// before the change counts.
	return waitAndReport(ctx, cluster, ref, operate.WaitOptions{
		Target: target, Timeout: *waiting.timeout, Since: result.Object, Generation: result.Generation,
	}, *flags.output, stdout, stderr)
}

func writeSuspendReport(out io.Writer, result operate.SuspendResult, waiting bool) {
	ref, view := result.Ref, viewCommand(result.Ref)
	var text strings.Builder
	field := statusview.FieldWriter(&text, "  ", len("Requested:"))
	switch {
	case !result.Changed:
		fmt.Fprintf(&text, "spec.suspend of %s already holds %t; nothing was sent.\n", ref, result.Suspend)
	case result.Suspend:
		fmt.Fprintf(&text, "Suspend requested for %s.\n", ref)
	default:
		fmt.Fprintf(&text, "Resume requested for %s.\n", ref)
	}
	field("Requested", fmt.Sprintf("spec.suspend %t, generation %d", result.Suspend, result.Generation))
	field("Observed", observed(result.ObservedGeneration, result.Phase))
	if result.Running != "" {
		field("Running", statusview.Clean(result.Running)+" operation")
	}
	switch {
	case !result.Changed:
	case result.Suspend && result.Running == string(operatorv1alpha1.OperationApply) && ref.Family == operate.FamilyMigration:
		fmt.Fprintf(&text, "The Apply runs to its end: suspension cancels no SQL. The change moved the generation that Apply "+
			"was decided from, so the operator records the run as one nobody accounted for (Unknown) without reading "+
			"its result. %s shows the record once the Job has ended.\n", view)
	case result.Suspend && result.Running == string(operatorv1alpha1.OperationApply):
		fmt.Fprintf(&text, "The Apply runs to its end: suspension cancels no SQL. The proof that it converged waits for "+
			"resume, and until then the schema keeps its database lock, so other resources on that database start "+
			"nothing that changes it.\n")
	case result.Suspend && result.Running != "":
		fmt.Fprintf(&text, "The %s operation runs to its end: suspension starts no new Job and cancels nothing.\n",
			statusview.Clean(result.Running))
	case result.Suspend:
		fmt.Fprintf(&text, "The operator starts no new Job once it observes generation %d. Suspension cancels "+
			"nothing: an operation already running is observed to its end.\n", result.Generation)
	default:
		fmt.Fprintf(&text, "Once the operator observes generation %d it reads the database again and decides "+
			"from what it finds.\n", result.Generation)
	}
	if !waiting {
		if result.Suspend {
			fmt.Fprintf(&text, "%s waits until the operator reports it Suspended.\n", waitLine(ref, "suspended"))
		} else {
			fmt.Fprintf(&text, "%s waits for a reading that begins after the wait does; --for approval waits for "+
				"a plan that needs an approval instead.\n", waitLine(ref, "in-sync"))
		}
	}
	_, _ = io.WriteString(out, text.String())
}

// refreshCommand writes a read request and, if asked to, waits for the read.
func refreshCommand(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	flags := newCommandFlags("refresh", stderr)
	timeout := flags.Duration("timeout", apiTimeout, "how long to wait for the API server")
	waiting := addWaitFlags(flags, "the read the request asks for has ended")
	ref, code, done := flags.parse(arguments, stderr)
	if done {
		return code
	}
	if !waiting.valid(stderr) {
		return exitUsage
	}

	cluster, ref, ok := flags.connect(ref, *timeout, stderr)
	if !ok {
		return exitFailed
	}
	changeCtx, cancel := context.WithTimeout(ctx, *timeout)
	result, err := operate.Refresh(changeCtx, cluster, ref, time.Now())
	cancel()
	if err != nil {
		return changeFailed(stderr, ref, "refresh", err)
	}
	if *flags.output == "text" {
		writeRefreshReport(stdout, result, *waiting.wait)
	} else if !*waiting.wait {
		return printResource(stdout, stderr, ref, result.Object, *flags.output)
	}
	if !*waiting.wait {
		return exitOK
	}
	// The answer to the patch is the baseline: it carries the request, and
	// a read that started before it was written cannot count as its answer.
	return waitAndReport(ctx, cluster, ref, operate.WaitOptions{
		Target: operate.TargetRead, Timeout: *waiting.timeout, Since: result.Object, ReadRequest: result.Request,
	}, *flags.output, stdout, stderr)
}

func writeRefreshReport(out io.Writer, result operate.RefreshResult, waiting bool) {
	ref := result.Ref
	status := statusOf(result.Object)
	var text strings.Builder
	field := statusview.FieldWriter(&text, "  ", len("Replaced:"))
	fmt.Fprintf(&text, "Read requested for %s.\n", ref)
	field("Request", operatorv1alpha1.ReadRequestAnnotation+"="+result.Request)
	if result.Previous != "" && operatorv1alpha1.ReadRequestToken(result.Previous) != status.lastReadRequest {
		field("Replaced", statusview.Clean(result.Previous)+", which the operator had not taken yet")
	}
	field("Observed", fmt.Sprintf("phase %s, %s", orNone(statusview.Clean(status.phase)), lastRead(status.observedAt)))
	text.WriteString("The request is written, not answered. The operator takes it once no operation is running, " +
		"the resource is not suspended and no retry delay is pending, and a read already under way does not " +
		"answer it. It starts no Apply and approves nothing.\n")
	if result.Suspended {
		text.WriteString("The resource is suspended: the operator takes the request after it is resumed.\n")
	}
	if !waiting {
		fmt.Fprintf(&text, "%s waits for that read; refresh --wait asks and waits in one command.\n",
			waitLine(ref, "read", "--read-request", result.Request))
	}
	_, _ = io.WriteString(out, text.String())
}

// changeFailed tells the person what became of a change that returned err.
func changeFailed(stderr io.Writer, ref operate.Ref, verb string, err error) int {
	message := statusview.Clean(err.Error())
	switch operate.Classify(err) {
	case operate.Absent:
		fmt.Fprintf(stderr, "kubectl-ptah: %s\n", message)
		return exitAbsent
	case operate.Replaced:
		fmt.Fprintf(stderr, "kubectl-ptah: %s.\nNothing was changed: the resource under that name is not the one "+
			"read. Run the command again to %s the one there now.\n", message, verb)
		return exitNotDone
	case operate.Forbidden:
		fmt.Fprintf(stderr, "kubectl-ptah: %s\nNothing was changed. %s needs get and patch on %s in namespace %s.\n",
			message, verb, resourceName(ref), ref.Namespace)
		return exitRefused
	case operate.Refused:
		fmt.Fprintf(stderr, "kubectl-ptah: %s\nNothing was changed.\n", message)
		return exitRefused
	case operate.Unknown:
		again := "Running it again sends nothing when spec.suspend already holds the value."
		if verb == "refresh" {
			again = "Running it again writes a newer request, and the operator reads once for the two."
		}
		fmt.Fprintf(stderr, "kubectl-ptah: %s\nThe change was sent and no answer said whether it was stored. %s "+
			"shows what is stored. %s\n", message, viewCommand(ref), again)
		return exitUnknown
	default:
		fmt.Fprintf(stderr, "kubectl-ptah: %s\n", message)
		return exitFailed
	}
}

// waitCommand reads a resource until it reaches the target the command line
// names.
func waitCommand(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	flags := newCommandFlags("wait", stderr)
	var (
		forTarget   = flags.String("for", "", "what to wait for: read, in-sync, approval, applied or suspended")
		timeout     = flags.Duration("timeout", defaultWaitTimeout, "how long to wait")
		readRequest = flags.String("read-request", "", "with --for read: the request kubectl ptah refresh wrote")
		plan        = flags.String("plan", "", "with --for applied, for a schema: the name or fingerprint of the plan")
	)
	ref, code, done := flags.parse(arguments, stderr)
	if done {
		return code
	}
	if *forTarget == "" {
		fmt.Fprintf(stderr, "kubectl-ptah: wait needs --for: read, in-sync, approval, applied or suspended\n")
		return exitUsage
	}
	target, err := operate.ParseTarget(*forTarget)
	if err != nil {
		fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
		return exitUsage
	}
	options := operate.WaitOptions{Target: target, Timeout: *timeout, ReadRequest: *readRequest, Plan: *plan}
	if err := checkWaitOptions(ref, options); err != nil {
		fmt.Fprintf(stderr, "kubectl-ptah: %v\n", err)
		return exitUsage
	}

	cluster, ref, ok := flags.connect(ref, apiTimeout, stderr)
	if !ok {
		return exitFailed
	}
	return waitAndReport(ctx, cluster, ref, options, *flags.output, stdout, stderr)
}

// checkWaitOptions refuses a command line a wait cannot honor before the
// cluster is reached. operate.Wait refuses the same, later.
func checkWaitOptions(ref operate.Ref, options operate.WaitOptions) error {
	switch {
	case options.Timeout <= 0:
		return fmt.Errorf("--timeout has to be longer than zero, not %s", options.Timeout)
	case options.Target == operate.TargetRead && options.ReadRequest == "":
		return errors.New("--for read needs --read-request, the value kubectl ptah refresh printed; refresh --wait asks and waits in one command")
	case options.Target != operate.TargetRead && options.ReadRequest != "":
		return errors.New("--read-request goes with --for read")
	case options.ReadRequest != "":
		if _, err := time.Parse(time.RFC3339, options.ReadRequest); err != nil {
			return fmt.Errorf("--read-request %q is not an RFC 3339 instant, so it asked for no read", options.ReadRequest)
		}
	case options.Plan != "" && (options.Target != operate.TargetApplied || ref.Family != operate.FamilySchema):
		return errors.New("--plan goes with --for applied on a schema: a migration run records no plan")
	}
	return nil
}

// waitAndReport waits and tells the person how it ended. Why a wait stopped
// short goes to stderr. In text stdout says what the wait read; in YAML or
// JSON it carries the resource the wait ended on, and nothing when it did
// not reach the target.
func waitAndReport(ctx context.Context, reader client.Reader, ref operate.Ref, options operate.WaitOptions, output string, stdout, stderr io.Writer) int {
	result, code := waitFor(ctx, reader, ref, options, output == "text", stderr)
	switch {
	case code != exitOK:
		return code
	case output != "text":
		return printResource(stdout, stderr, ref, result.Object, output)
	default:
		writeReached(stdout, result)
		return exitOK
	}
}

// waitFor waits, and says on stderr why a wait stopped short. In text it
// also says what it is waiting for, since a wait can take minutes.
func waitFor(ctx context.Context, reader client.Reader, ref operate.Ref, options operate.WaitOptions, text bool, stderr io.Writer) (operate.Result, int) {
	if text {
		fmt.Fprintf(stderr, "Waiting up to %s for %s to reach %s.\n", options.Timeout, ref, options.Target)
	}
	result, err := operate.Wait(ctx, reader, ref, options)
	if err != nil {
		return operate.Result{}, waitFailed(stderr, ref, err)
	}
	return result, exitOK
}

func writeReached(out io.Writer, result operate.Result) {
	ref := result.Ref
	status := statusOf(result.Object)
	var text strings.Builder
	field := statusview.FieldWriter(&text, "  ", len("Generation:"))
	fmt.Fprintf(&text, "%s reached %s.\n", ref, result.Target)
	field("Phase", statusview.Clean(result.Phase))
	field("Generation", fmt.Sprintf("%d, observed by the operator", result.ObservedGeneration))
	field("Database", lastRead(result.ObservedAt))
	next := viewCommand(ref) + " shows where it stands."
	switch result.Target {
	case operate.TargetApproval:
		field("Plan", statusview.Clean(status.plan))
		next = fmt.Sprintf("kubectl ptah approve %s/%s -n %s reviews it.", ref.Family, ref.Name, ref.Namespace)
	case operate.TargetApplied:
		if ref.Family == operate.FamilySchema {
			field("Applied", statusview.Clean(status.applied))
		} else {
			field("Last run", statusview.Clean(status.lastRun))
		}
	}
	text.WriteString(next + "\n")
	_, _ = io.WriteString(out, text.String())
}

// waitFailed tells the person why a wait ended without the target.
func waitFailed(stderr io.Writer, ref operate.Ref, err error) int {
	var (
		timeout *operate.TimeoutError
		stop    *operate.StopError
		text    strings.Builder
	)
	field := statusview.FieldWriter(&text, "  ", len("Waiting for:"))
	switch {
	case errors.As(err, &timeout):
		fmt.Fprintf(&text, "kubectl-ptah: %s did not reach %s within %s.\n", ref, timeout.Target, timeout.Waited.Round(time.Second))
		field("Phase", orNone(statusview.Clean(timeout.Phase)))
		if blocker := timeout.Blocker; blocker != nil {
			field("Held by", statusview.Clean(blocker.Type)+"="+statusview.Summary(statusview.FromConditions([]metav1.Condition{*blocker})[0]))
		}
		field("Waiting for", statusview.Clean(timeout.Waiting))
		if timeout.LastError != nil {
			field("Last read", statusview.Clean(timeout.LastError.Error()))
		}
		fmt.Fprintf(&text, "%s shows every condition with its message.\n", viewCommand(ref))
		_, _ = io.WriteString(stderr, text.String())
		return exitTimedOut
	case errors.As(err, &stop):
		fmt.Fprintf(&text, "kubectl-ptah: %s will not reach %s: %s.\n", ref, stop.Target, stop.Reason)
		field("Phase", orNone(statusview.Clean(stop.Phase)))
		field("Why", statusview.Clean(stop.Message))
		fmt.Fprintf(&text, "%s shows where it stands.\n", viewCommand(ref))
		_, _ = io.WriteString(stderr, text.String())
		return exitWillNotReach
	case errors.Is(err, operate.ErrReplaced):
		fmt.Fprintf(stderr, "kubectl-ptah: %s\nThe wait stopped: it was waiting for the resource that was there "+
			"when it began.\n", statusview.Clean(err.Error()))
		return exitWillNotReach
	case apierrors.IsNotFound(err):
		fmt.Fprintf(stderr, "kubectl-ptah: %s\n", statusview.Clean(err.Error()))
		return exitAbsent
	default:
		fmt.Fprintf(stderr, "kubectl-ptah: %s\n", statusview.Clean(err.Error()))
		return exitFailed
	}
}

// printResource writes the resource as YAML or JSON, the way kubectl prints
// one, with the kind the typed client leaves out of what it decodes.
func printResource(stdout, stderr io.Writer, ref operate.Ref, object client.Object, format string) int {
	object = object.DeepCopyObject().(client.Object)
	object.GetObjectKind().SetGroupVersionKind(operatorv1alpha1.GroupVersion.WithKind(ref.Kind()))
	return printObject(stdout, stderr, object, format)
}

// resourceStatus is what a report reads from a resource's status.
type resourceStatus struct {
	phase           string
	observedAt      time.Time
	lastReadRequest string
	plan            string
	applied         string
	lastRun         string
}

func statusOf(object client.Object) resourceStatus {
	switch typed := object.(type) {
	case *operatorv1alpha1.PtahSchema:
		status := resourceStatus{phase: string(typed.Status.Phase), lastReadRequest: typed.Status.LastReadRequest}
		if observed := typed.Status.Target.LastObservedAt; observed != nil {
			status.observedAt = observed.Time
		}
		if plan := typed.Status.Plan; plan != nil {
			status.plan = plan.Name + ", fingerprint " + plan.Fingerprint
		}
		if applied := typed.Status.Applied; applied != nil {
			status.applied = fmt.Sprintf("plan %s, fingerprint %s, at %s",
				applied.PlanRef.Name, applied.PlanFingerprint, statusview.Time(applied.CompletedAt))
		}
		return status
	case *operatorv1alpha1.PtahMigration:
		status := resourceStatus{phase: string(typed.Status.Phase), lastReadRequest: typed.Status.LastReadRequest}
		if history := typed.Status.History; history != nil {
			status.observedAt = history.ObservedAt.Time
		}
		if plan := typed.Status.Plan; plan != nil {
			status.plan = plan.Name
		}
		if run := typed.Status.LastRun; run != nil {
			status.lastRun = fmt.Sprintf("%s, Job %s", run.Outcome, run.JobName)
		}
		return status
	default:
		return resourceStatus{}
	}
}

// viewCommand is the read command that prints the resource.
func viewCommand(ref operate.Ref) string {
	return fmt.Sprintf("kubectl ptah %s %s -n %s", ref.Family, ref.Name, ref.Namespace)
}

// waitLine is the wait command for target, with any flags it needs beside
// --for.
func waitLine(ref operate.Ref, target string, flags ...string) string {
	return strings.Join(append([]string{"kubectl ptah wait", string(ref.Family) + "/" + ref.Name, "--for", target},
		append(flags, "-n", ref.Namespace)...), " ")
}

// resourceName is the resource's plural, as RBAC names it.
func resourceName(ref operate.Ref) string {
	return strings.ToLower(ref.Kind()) + "s"
}

func observed(generation int64, phase string) string {
	if generation == 0 && phase == "" {
		return "nothing yet: the operator has written no status"
	}
	return fmt.Sprintf("generation %d, phase %s", generation, orNone(statusview.Clean(phase)))
}

func orNone(value string) string {
	if value == "" {
		return "none"
	}
	return value
}

// lastRead says when the database was last read, as the status records it.
func lastRead(at time.Time) string {
	if at.IsZero() {
		return "not read yet"
	}
	return "last read at " + statusview.Time(metav1.NewTime(at))
}
