package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/statusview"
	"github.com/stokaro/ptah-operator/pkg/planread/planreadtest"
)

// admitting is an API server that stamps the approver the way the mutating
// webhook does, or answers the create with a chosen error.
type admitting struct {
	client.Client
	refuse  error
	created []client.Object
	options []client.CreateOption
}

func (a *admitting) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	a.created = append(a.created, object)
	a.options = append(a.options, options...)
	if a.refuse != nil {
		return a.refuse
	}
	stampedAt := metav1.NewTime(time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC))
	switch approval := object.(type) {
	case *operatorv1alpha1.PtahSchemaApproval:
		approval.Spec.Approver.Username, approval.Spec.ApprovedAt = "alice@example.com", stampedAt
	case *operatorv1alpha1.PtahMigrationApproval:
		approval.Spec.Approver.Username, approval.Spec.ApprovedAt = "alice@example.com", stampedAt
	}
	return a.Client.Create(ctx, object, options...)
}

// deadlines is the API server as client-go reaches it: a request whose
// context is done is not sent. The in-memory client ignores the context.
type deadlines struct{ *admitting }

func (d deadlines) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return d.admitting.Get(ctx, key, object, options...)
}

func (d deadlines) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return d.admitting.Create(ctx, object, options...)
}

// person answers the prompt after reading the plan for a while.
type person struct {
	reads  time.Duration
	answer io.Reader
}

func (p *person) Read(buffer []byte) (int, error) {
	time.Sleep(p.reads)
	return p.answer.Read(buffer)
}

// approvable stores a schema whose plan waits for a person and returns the
// API server approve talks to, with the plan's fingerprint.
func approvable(t *testing.T, change func(*operatorv1alpha1.PtahSchemaStatus)) (*admitting, string) {
	t.Helper()
	lab := planreadtest.NewLab(t, "storefront")
	plan, _ := lab.Publish(t, []string{"ALTER TABLE orders DROP COLUMN legacy_note;"}, "drop")
	lab.SetCurrent(t, plan)
	schema := &operatorv1alpha1.PtahSchema{}
	if err := lab.Client().Get(context.Background(), client.ObjectKey{Namespace: planreadtest.Namespace, Name: "storefront"}, schema); err != nil {
		t.Fatalf("read the schema: %v", err)
	}
	schema.Status.Phase = operatorv1alpha1.PhaseAwaitingApproval
	meta.SetStatusCondition(&schema.Status.Conditions, metav1.Condition{
		Type: operatorv1alpha1.ConditionApprovalRequired, Status: metav1.ConditionTrue, Reason: "Waiting",
		Message: "Create an approval bound to the current plan fingerprint",
	})
	if change != nil {
		change(&schema.Status)
	}
	if err := lab.Client().Status().Update(context.Background(), schema); err != nil {
		t.Fatalf("update the schema status: %v", err)
	}
	return &admitting{Client: lab.Client()}, plan.Spec.Fingerprint
}

func runApprove(t *testing.T, api *admitting, input string, interactive bool, arguments ...string) (int, string, string) {
	t.Helper()
	return runApproveWith(t, api, planreadtest.Namespace, strings.NewReader(input), interactive, arguments...)
}

func runApproveWith(t *testing.T, api client.Client, namespace string, input io.Reader, interactive bool, arguments ...string) (int, string, string) {
	t.Helper()
	previous := connector
	connector = func(string, string, string, time.Duration) (client.Client, string, error) {
		return api, namespace, nil
	}
	t.Cleanup(func() { connector = previous })
	var stdout, stderr bytes.Buffer
	code := approveCommand(context.Background(), arguments, input, interactive, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// The commands share the package-level connector, so these tests do not run
// in parallel.

// unreadable is an API server whose current plan waits for a person and carries
// fields this plugin does not read, as a plan a newer Ptah saved does.
func unreadable(t *testing.T) (*admitting, string) {
	t.Helper()
	return waitingOn(t, planreadtest.DocumentWithUnreadFields(t, []string{"ALTER TABLE orders DROP COLUMN legacy_note;"}))
}

// waitingOn is an API server whose current plan is this document, waiting for a
// person.
func waitingOn(t *testing.T, document []byte) (*admitting, string) {
	t.Helper()
	lab := planreadtest.NewLab(t, "storefront")
	plan := lab.PublishDocument(t, document, "unread")
	lab.SetCurrent(t, plan)
	schema := &operatorv1alpha1.PtahSchema{}
	if err := lab.Client().Get(context.Background(), client.ObjectKey{Namespace: planreadtest.Namespace, Name: "storefront"}, schema); err != nil {
		t.Fatalf("read the schema: %v", err)
	}
	schema.Status.Phase = operatorv1alpha1.PhaseAwaitingApproval
	meta.SetStatusCondition(&schema.Status.Conditions, metav1.Condition{
		Type: operatorv1alpha1.ConditionApprovalRequired, Status: metav1.ConditionTrue, Reason: "Waiting",
		Message: "Create an approval bound to the current plan fingerprint",
	})
	if err := lab.Client().Status().Update(context.Background(), schema); err != nil {
		t.Fatalf("update the schema status: %v", err)
	}
	return &admitting{Client: lab.Client()}, plan.Spec.Fingerprint
}

// Every way of asking for an approval ends at the strict read: a person at a
// terminal who says yes, a script that passes the fingerprint plan printed, and
// a dry run that would print a manifest to apply later. None creates anything,
// and none prints a plan a person could take for the one reviewed.
func TestApproveRefusesAPlanItCannotReadWhole(t *testing.T) {
	for _, test := range []struct {
		name        string
		interactive bool
		arguments   func(fingerprint string) []string
	}{
		{"a yes at a terminal", true, func(string) []string { return []string{"schema/storefront"} }},
		{"the fingerprint plan printed", false, func(fingerprint string) []string {
			return []string{"schema/storefront", "--plan-fingerprint", fingerprint}
		}},
		{"a client dry run", false, func(fingerprint string) []string {
			return []string{"schema/storefront", "--plan-fingerprint", fingerprint, "--dry-run", "client", "-o", "yaml"}
		}},
		{"a server dry run", false, func(fingerprint string) []string {
			return []string{"schema/storefront", "--plan-fingerprint", fingerprint, "--dry-run", "server"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			api, fingerprint := unreadable(t)

			code, stdout, stderr := runApprove(t, api, "yes\n", test.interactive, test.arguments(fingerprint)...)

			if code != exitFailed {
				t.Fatalf("approve = %d, want %d; stderr %q", code, exitFailed, stderr)
			}
			if len(api.created) != 0 {
				t.Fatalf("approve created %d objects from a plan it cannot read whole", len(api.created))
			}
			if stdout != "" {
				t.Fatalf("approve wrote to stdout: %q", stdout)
			}
			hint := "kubectl ptah plan storefront -n " + planreadtest.Namespace
			if !strings.Contains(stderr, hint) || strings.Contains(stderr, planreadtest.UnreadValue) {
				t.Fatalf("the refusal does not say %q, or quotes a value: %q", hint, stderr)
			}
		})
	}
}

// A key this plugin would not print does not reach the terminal through approve,
// or through plan, in any format.
func TestNeitherCommandQuotesAKeyItWouldNotPrint(t *testing.T) {
	document := planreadtest.Document(t, []string{"ALTER TABLE orders DROP COLUMN legacy_note;"})
	document = append([]byte(`{"postgres://app:hunter2@db/shop":1,`), document[1:]...)
	api, _ := waitingOn(t, document)

	code, stdout, stderr := runApprove(t, api, "yes\n", true, "schema/storefront")
	if code != exitFailed || len(api.created) != 0 || strings.Contains(stdout+stderr, "hunter2") {
		t.Fatalf("approve = %d after %d creates; stdout %q; stderr %q", code, len(api.created), stdout, stderr)
	}
	for _, format := range []string{"text", "sql", "json"} {
		code, stdout, stderr := runPlan(t, api, "storefront", "-o", format)
		if code != exitFailed || stdout != "" || strings.Contains(stderr, "hunter2") {
			t.Fatalf("plan -o %s = %d; stdout %q; stderr %q", format, code, stdout, stderr)
		}
	}
}

// The control for the rows above: the fingerprint and the arguments are good,
// and the same command approves the same statement in a document it reads whole.
func TestApproveAcceptsThoseArgumentsOnAPlanItReadsWhole(t *testing.T) {
	api, fingerprint := approvable(t, nil)

	code, _, stderr := runApprove(t, api, "", false, "schema/storefront", "--plan-fingerprint", fingerprint)

	if code != exitOK || len(api.created) != 1 {
		t.Fatalf("approve = %d after %d creates; stderr %q", code, len(api.created), stderr)
	}
}

// runPlan runs the plan command against an API server that refuses every write.
func runPlan(t *testing.T, api client.Client, arguments ...string) (int, string, string) {
	t.Helper()
	previous := connector
	connector = func(string, string, string, time.Duration) (client.Client, string, error) {
		return readOnly{Client: api, t: t}, planreadtest.Namespace, nil
	}
	t.Cleanup(func() { connector = previous })
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), append([]string{"plan"}, arguments...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// readOnly fails a test on any write that reaches it.
type readOnly struct {
	client.Client
	t *testing.T
}

func (r readOnly) Create(context.Context, client.Object, ...client.CreateOption) error {
	r.t.Error("the plan command created an object")
	return nil
}

func (r readOnly) Update(context.Context, client.Object, ...client.UpdateOption) error {
	r.t.Error("the plan command updated an object")
	return nil
}

func (r readOnly) Patch(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
	r.t.Error("the plan command patched an object")
	return nil
}

func (r readOnly) Delete(context.Context, client.Object, ...client.DeleteOption) error {
	r.t.Error("the plan command deleted an object")
	return nil
}

// The same plan the rows above refuse to approve is the one a person can read:
// the statements and the fields' names, and only as text. The other formats are
// for a program, which would take what they print for the whole plan: a pipe into
// a database would run the statements of a plan that can run differently from
// them. Before a display read existed, each of them exited 1 too.
func TestPlanShowsAPlanItCannotReadWholeAsTextAlone(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		api, _ := unreadable(t)

		code, stdout, stderr := runPlan(t, api, "storefront")

		if code != exitOK || stderr != "" {
			t.Fatalf("plan = %d, want %d; stderr %q", code, exitOK, stderr)
		}
		for _, want := range []string{"Unread fields:  annotations, statements[].inputs (", "ALTER TABLE orders DROP COLUMN legacy_note;"} {
			if !strings.Contains(stdout, want) {
				t.Fatalf("stdout does not carry %q:\n%s", want, stdout)
			}
		}
		if strings.Contains(stdout, planreadtest.UnreadValue) {
			t.Fatalf("the command printed a value it did not read:\n%s", stdout)
		}
	})
	for _, format := range []string{"sql", "json"} {
		t.Run(format, func(t *testing.T) {
			api, _ := unreadable(t)

			code, stdout, stderr := runPlan(t, api, "storefront", "-o", format)

			if code != exitFailed || stdout != "" {
				t.Fatalf("plan = %d, want %d; stdout %q", code, exitFailed, stdout)
			}
			for _, want := range []string{"annotations, statements[].inputs", "-o " + format + " prints the whole plan"} {
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr does not carry %q: %q", want, stderr)
				}
			}
			if strings.Contains(stderr, planreadtest.UnreadValue) {
				t.Fatalf("the command printed a value it did not read: %q", stderr)
			}
		})
	}
}

// A plan read whole prints as it did, in every format, with no note.
func TestPlanPrintsAPlanItReadsWholeAsItWas(t *testing.T) {
	const statement = "ALTER TABLE orders DROP COLUMN legacy_note;\n"
	for format, holds := range map[string]func(string) bool{
		"text": func(stdout string) bool {
			return strings.Contains(stdout, statement) && !strings.Contains(stdout, "Unread")
		},
		"sql":  func(stdout string) bool { return stdout == statement },
		"json": func(stdout string) bool { return strings.HasPrefix(stdout, "{") },
	} {
		t.Run(format, func(t *testing.T) {
			api, _ := approvable(t, nil)

			code, stdout, stderr := runPlan(t, api, "storefront", "-o", format)

			if code != exitOK || stderr != "" || !holds(stdout) {
				t.Fatalf("plan = %d; stdout %q; stderr %q", code, stdout, stderr)
			}
		})
	}
}

func TestApproveShowsThePlanAndApprovesItOnYes(t *testing.T) {
	api, fingerprint := approvable(t, nil)

	code, stdout, stderr := runApprove(t, api, "yes\n", true, "schema/storefront")

	if code != exitOK || len(api.created) != 1 {
		t.Fatalf("approve = %d after %d creates; stderr %q", code, len(api.created), stderr)
	}
	for _, want := range []string{"ALTER TABLE orders DROP COLUMN legacy_note;", fingerprint,
		"recorded by admission as alice@example.com at 2026-11-01T10:00:00Z", "It binds plan"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("approve printed %q, missing %q", stdout, want)
		}
	}
	if !strings.Contains(stderr, "Type yes to approve") {
		t.Fatalf("approve did not ask: stderr %q", stderr)
	}
}

func TestApproveSendsNothingWithoutAYes(t *testing.T) {
	for _, answer := range []string{"\n", "no\n", "y\n", "YES please\n", ""} {
		api, _ := approvable(t, nil)

		code, _, stderr := runApprove(t, api, answer, true, "schema/storefront")

		if code != exitNotDone || len(api.created) != 0 || !strings.Contains(stderr, "Nothing was approved") {
			t.Fatalf("approve after %q = %d, %d creates, stderr %q", answer, code, len(api.created), stderr)
		}
	}
}

func TestApproveWithoutATerminalNeedsTheReviewedFingerprint(t *testing.T) {
	api, fingerprint := approvable(t, nil)

	code, _, stderr := runApprove(t, api, "yes\n", false, "schema/storefront")

	if code != exitUsage || len(api.created) != 0 || !strings.Contains(stderr, "--plan-fingerprint") {
		t.Fatalf("approve without a terminal = %d, %d creates, stderr %q", code, len(api.created), stderr)
	}

	code, stdout, stderr := runApprove(t, api, "", false, "schema/storefront", "--plan-fingerprint", fingerprint)

	if code != exitOK || len(api.created) != 1 || strings.Contains(stdout, "ALTER TABLE") {
		t.Fatalf("approve with the fingerprint = %d, %d creates, stdout %q, stderr %q", code, len(api.created), stdout, stderr)
	}
}

// Without a terminal, approve says where the fingerprint is. Each family's row
// runs the command the message names against the same plan, and finds the
// fingerprint there: kubectl ptah plan reads a schema's plan and no other.
func TestApproveWithoutATerminalNamesACommandThatShowsTheFingerprint(t *testing.T) {
	t.Run("schema", func(t *testing.T) {
		api, fingerprint := approvable(t, nil)

		code, _, stderr := runApprove(t, api, "", false, "schema/storefront")

		if code != exitUsage || !strings.Contains(stderr, "(kubectl ptah plan storefront shows it)") {
			t.Fatalf("approve = %d, stderr %q", code, stderr)
		}
		if code, stdout, stderr := runPlan(t, api.Client, "storefront"); code != exitOK || !strings.Contains(stdout, fingerprint) {
			t.Fatalf("the command it names = %d, stdout %q, stderr %q; want the fingerprint %s", code, stdout, stderr, fingerprint)
		}
	})
	t.Run("migration", func(t *testing.T) {
		server := newFakeAPIServer(t)
		stored := server.namespaced["ptahmigrationplans/ptah-mplan-orders"].(*operatorv1alpha1.PtahMigrationPlan)
		stored.Spec.ContractVersion = 1
		named := server.namespaced["ptahmigrations/orders"].(*operatorv1alpha1.PtahMigration)
		api := &admitting{Client: planreadtest.NewClient(t, named.DeepCopy(), stored.DeepCopy())}

		code, _, stderr := runApproveWith(t, api, "application", strings.NewReader(""), false, "migration/orders")

		const names = "(kubectl ptah migration orders -o json shows it as plan.fingerprint)"
		if code != exitUsage || !strings.Contains(stderr, names) || strings.Contains(stderr, "kubectl ptah plan") {
			t.Fatalf("approve = %d, stderr %q; want it to name %s", code, stderr, names)
		}

		if code, stdout, _ := runPlan(t, api.Client, "orders"); code != exitAbsent || stdout != "" {
			t.Fatalf("kubectl ptah plan on a migration = %d, stdout %q; want it to have nothing to say", code, stdout)
		}

		var stdout, viewErr bytes.Buffer
		arguments := []string{"migration", "orders", "-o", "json", "-n", "application", "--kubeconfig", writeKubeconfig(t, server.URL)}
		if status := run(context.Background(), arguments, &stdout, &viewErr); status != exitOK {
			t.Fatalf("the command it names = %d, stderr %q", status, viewErr.String())
		}
		var view struct {
			Plan struct {
				Fingerprint string `json:"fingerprint"`
			} `json:"plan"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &view); err != nil {
			t.Fatalf("the command it names printed %q: %v", stdout.String(), err)
		}
		if view.Plan.Fingerprint != stored.Spec.Fingerprint {
			t.Fatalf("plan.fingerprint = %q, want %q", view.Plan.Fingerprint, stored.Spec.Fingerprint)
		}
	})
}

func TestApproveRefusesAFingerprintThatIsNotTheCurrentPlans(t *testing.T) {
	api, _ := approvable(t, nil)

	code, _, stderr := runApprove(t, api, "", false, "schema/storefront", "--plan-fingerprint", "sha256:reviewed-yesterday")

	if code != exitNotDone || len(api.created) != 0 || !strings.Contains(stderr, "not the sha256:reviewed-yesterday given") {
		t.Fatalf("approve = %d, %d creates, stderr %q", code, len(api.created), stderr)
	}
}

func TestApproveSaysWhyNothingCanBeApprovedNow(t *testing.T) {
	// A gate the operator does not reopen by itself: the policy holds the
	// plan.
	closed := func(status *operatorv1alpha1.PtahSchemaStatus) {
		status.Phase = operatorv1alpha1.PhaseBlocked
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: operatorv1alpha1.ConditionApprovalRequired,
			Status: metav1.ConditionFalse, Reason: "DestructiveChangesDisabled"})
	}
	api, fingerprint := approvable(t, closed)

	code, _, stderr := runApprove(t, api, "", false, "schema/storefront", "--plan-fingerprint", fingerprint)

	if code != exitNotDone || len(api.created) != 0 ||
		!strings.Contains(stderr, "spec.policy holds this plan (DestructiveChangesDisabled), and no approval is asked for until the spec changes.") {
		t.Fatalf("approve = %d, %d creates, stderr %q", code, len(api.created), stderr)
	}

	// On a terminal the reason comes before the review: a plan nobody may
	// approve now is not put in front of a person as if they could.
	api, _ = approvable(t, closed)

	code, stdout, stderr := runApprove(t, api, "yes\n", true, "schema/storefront")

	if code != exitNotDone || stdout != "" || len(api.created) != 0 ||
		!strings.Contains(stderr, "spec.policy holds this plan") || strings.Contains(stderr, "Type yes") {
		t.Fatalf("approve on a terminal = %d, %d creates, stdout %q, stderr %q", code, len(api.created), stdout, stderr)
	}
}

// rereading is the API server with the operator reading the schema again at
// its interval once the person has answered: from then on each read of the
// schema first applies the next step to its status.
type rereading struct {
	*admitting
	t     *testing.T
	mu    sync.Mutex
	armed bool
	steps []func(*operatorv1alpha1.PtahSchemaStatus)
}

func (r *rereading) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	var step func(*operatorv1alpha1.PtahSchemaStatus)
	if readsSchema(object) {
		r.mu.Lock()
		if r.armed && len(r.steps) > 0 {
			step, r.steps = r.steps[0], r.steps[1:]
		}
		r.mu.Unlock()
	}
	if step != nil {
		stored := &operatorv1alpha1.PtahSchema{}
		if err := r.admitting.Client.Get(ctx, key, stored); err != nil {
			r.t.Fatalf("read the schema to change its status: %v", err)
		}
		step(&stored.Status)
		if err := r.admitting.Client.Status().Update(ctx, stored); err != nil {
			r.t.Fatalf("write the status the operator would: %v", err)
		}
	}
	return r.admitting.Get(ctx, key, object, options...)
}

// readsSchema reports a read of a schema, typed or not: approve reads it as
// unstructured.
func readsSchema(object client.Object) bool {
	if _, typed := object.(*operatorv1alpha1.PtahSchema); typed {
		return true
	}
	raw, ok := object.(*unstructured.Unstructured)
	return ok && raw.GetKind() == "PtahSchema"
}

func (r *rereading) arm() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.armed = true
}

// decides is a person who reads the plan for a while and answers, at which
// point the operator starts its read.
type decides struct {
	reads  time.Duration
	answer io.Reader
	then   func()
}

func (d *decides) Read(buffer []byte) (int, error) {
	time.Sleep(d.reads)
	d.then()
	return d.answer.Read(buffer)
}

func readStep(phase operatorv1alpha1.ReconciliationPhase, operation operatorv1alpha1.OperationType) func(*operatorv1alpha1.PtahSchemaStatus) {
	return func(status *operatorv1alpha1.PtahSchemaStatus) {
		status.Phase = phase
		status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{Type: operation, ID: "read"}
	}
}

func backAtTheGate(status *operatorv1alpha1.PtahSchemaStatus) {
	status.Phase, status.ActiveOperation = operatorv1alpha1.PhaseAwaitingApproval, nil
}

// The operator reads a waiting schema again at its interval, and admission
// refuses an approval while it does. A person who answers then is not told
// there is nothing to approve: the answer is sent when the read ends with the
// reviewed plan back at the gate.
func TestApproveWaitsForTheGateWhenTheOperatorReadsAgain(t *testing.T) {
	t.Run("the person answers during the read", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			api, fingerprint := approvable(t, nil)
			server := &rereading{admitting: api, t: t, steps: []func(*operatorv1alpha1.PtahSchemaStatus){
				readStep(operatorv1alpha1.PhaseResolving, operatorv1alpha1.OperationResolve),
				readStep(operatorv1alpha1.PhaseVerifying, operatorv1alpha1.OperationVerify),
				backAtTheGate,
			}}
			person := &decides{reads: 2 * time.Minute, answer: strings.NewReader("yes\n"), then: server.arm}

			code, stdout, stderr := runApproveWith(t, server, planreadtest.Namespace, person, true, "schema/storefront")

			if code != exitOK || len(api.created) != 1 || len(server.steps) != 0 {
				t.Fatalf("approve = %d after %d creates with %d steps left; stderr %q", code, len(api.created), len(server.steps), stderr)
			}
			plan := approvedPlan(t, api)
			requireSays(t, "approve's stderr", stderr, "Type yes to approve: ",
				"Waiting up to 5m for the operator to end its read of schema/storefront. If plan "+plan+
					" is back at the approval gate then, the approval goes to admission.\n")
			requireSays(t, "approve's report", stdout, "Approved:", fingerprint)
		})
	})
	t.Run("the read was under way before the review", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			api, _ := approvable(t, readStep(operatorv1alpha1.PhaseObserving, operatorv1alpha1.OperationObserve))
			server := &rereading{admitting: api, t: t, steps: []func(*operatorv1alpha1.PtahSchemaStatus){backAtTheGate}}
			person := &decides{reads: 10 * time.Second, answer: strings.NewReader("yes\n"), then: server.arm}

			code, _, stderr := runApproveWith(t, server, planreadtest.Namespace, person, true, "schema/storefront")

			if code != exitOK || len(api.created) != 1 {
				t.Fatalf("approve = %d after %d creates; stderr %q", code, len(api.created), stderr)
			}
			notice := "The operator is reading this schema again (Observe); admission takes an approval once it is back at the approval gate. " +
				"A decision taken now is sent when that read ends, if this plan is still the one waiting."
			requireSays(t, "approve's stderr", stderr, notice, "Type yes to approve: ")
			// Said once, before the prompt; the read had ended by the answer.
			if strings.Count(stderr, "reading this schema again") != 1 || strings.Contains(stderr, "Waiting up to") {
				t.Fatalf("approve repeats what it found, or waits for a read that ended: %q", stderr)
			}
		})
	})
	t.Run("the read does not end", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			api, _ := approvable(t, nil)
			server := &rereading{admitting: api, t: t, steps: []func(*operatorv1alpha1.PtahSchemaStatus){
				readStep(operatorv1alpha1.PhaseObserving, operatorv1alpha1.OperationObserve),
			}}
			person := &decides{reads: time.Minute, answer: strings.NewReader("yes\n"), then: server.arm}

			code, _, stderr := runApproveWith(t, server, planreadtest.Namespace, person, true, "schema/storefront")

			if code != exitNotDone || len(api.created) != 0 {
				t.Fatalf("approve = %d after %d creates; stderr %q", code, len(api.created), stderr)
			}
			requireSays(t, "approve's stderr", stderr, "The operator was still reading schema/storefront again after 5m. Nothing was approved")
		})
	})
}

// approvedPlan is the name of the plan the schema names.
func approvedPlan(t *testing.T, api *admitting) string {
	t.Helper()
	schema := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: planreadtest.Namespace, Name: "storefront"}, schema); err != nil {
		t.Fatalf("read the schema: %v", err)
	}
	return schema.Status.Plan.Name
}

// A gate no read of the operator reopens is answered at once, in the words
// approve used before it learned to wait: ApprovalRequired was not True when
// the read began.
func TestApproveAnswersAtOnceAGateNoReadReopens(t *testing.T) {
	t.Run("a migration under apply Never at rest", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			migration := &operatorv1alpha1.PtahMigration{
				ObjectMeta: metav1.ObjectMeta{Namespace: "application", Name: "shipments", UID: "uid-shipments"},
				Spec:       operatorv1alpha1.PtahMigrationSpec{Policy: operatorv1alpha1.MigrationPolicy{Apply: operatorv1alpha1.ApplyPolicyNever}},
				Status: operatorv1alpha1.PtahMigrationStatus{
					Phase:   operatorv1alpha1.MigrationPhasePlanning,
					History: &operatorv1alpha1.MigrationHistoryStatus{CurrentVersion: 13},
					Plan:    &operatorv1alpha1.ImmutableObjectReference{Name: "shipments-p-14", UID: "uid-p-14"},
					Conditions: []metav1.Condition{{
						Type: operatorv1alpha1.ConditionMigrationApprovalRequired, Status: metav1.ConditionFalse, Reason: "NotRequired",
						Message: "The apply policy is Never, so no approval is consulted",
					}},
				},
			}
			plan := &operatorv1alpha1.PtahMigrationPlan{
				ObjectMeta: metav1.ObjectMeta{Namespace: "application", Name: "shipments-p-14", UID: "uid-p-14"},
				Spec: operatorv1alpha1.PtahMigrationPlanSpec{ContractVersion: 1, Fingerprint: "sha256:14",
					MigrationRef: operatorv1alpha1.ImmutableObjectReference{Name: "shipments", UID: "uid-shipments"}},
			}
			api := &admitting{Client: planreadtest.NewClient(t, migration, plan)}
			began := time.Now()

			code, stdout, stderr := runApproveWith(t, api, "application", strings.NewReader("yes\n"), true, "migration/shipments")

			if code != exitNotDone || stdout != "" || len(api.created) != 0 || time.Since(began) != 0 {
				t.Fatalf("approve = %d after %s and %d creates, stdout %q, stderr %q", code, time.Since(began), len(api.created), stdout, stderr)
			}
			if stderr != "kubectl-ptah: The migration is Planning, not waiting for an approval. Nothing was approved.\n" {
				t.Fatalf("approve said %q", stderr)
			}
		})
	})
	t.Run("a schema the policy holds, read at its interval", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			api, fingerprint := approvable(t, func(status *operatorv1alpha1.PtahSchemaStatus) {
				readStep(operatorv1alpha1.PhaseResolving, operatorv1alpha1.OperationResolve)(status)
				meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: operatorv1alpha1.ConditionApprovalRequired,
					Status: metav1.ConditionFalse, Reason: "DestructiveChangesDisabled", Message: "Plan contains destructive changes"})
			})
			began := time.Now()

			code, _, stderr := runApprove(t, api, "", false, "schema/storefront", "--plan-fingerprint", fingerprint)

			if code != exitNotDone || len(api.created) != 0 || time.Since(began) != 0 {
				t.Fatalf("approve = %d after %s and %d creates, stderr %q", code, time.Since(began), len(api.created), stderr)
			}
			if stderr != "kubectl-ptah: spec.policy holds this plan (DestructiveChangesDisabled), and no approval is asked for "+
				"until the spec changes: Plan contains destructive changes. Nothing was approved.\n" {
				t.Fatalf("approve said %q", stderr)
			}
		})
	})
}

// The help names the bound approve waits for an interval read by, as the
// code holds it.
func TestApproveHelpNamesTheGateWait(t *testing.T) {
	var help bytes.Buffer
	usage(&help)
	for _, want := range []string{"waits up to " + statusview.Duration(gateTimeout) + " for an interval read",
		"bounded by " + statusview.Duration(gateTimeout) + ", not by this", "approve waits up to " + statusview.Duration(gateTimeout)} {
		if !strings.Contains(strings.Join(strings.Fields(help.String()), " "), want) {
			t.Fatalf("the help does not say %q", want)
		}
	}
}

// The manifest is the decision and nothing else, under a name the API server
// generates when it is created: no fixed name, no empty approver, no null time,
// no status.
func TestApprovePrintsTheManifestOnAClientDryRun(t *testing.T) {
	api, fingerprint := approvable(t, nil)
	schema := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: planreadtest.Namespace, Name: "storefront"}, schema); err != nil {
		t.Fatalf("read the schema: %v", err)
	}

	code, stdout, stderr := runApprove(t, api, "", false, "schema/storefront", "--dry-run", "client", "--plan-fingerprint", fingerprint)

	if code != exitOK || len(api.created) != 0 {
		t.Fatalf("approve --dry-run=client = %d, %d creates, stderr %q", code, len(api.created), stderr)
	}
	var manifest map[string]any
	if err := yaml.Unmarshal([]byte(stdout), &manifest); err != nil {
		t.Fatalf("the manifest %q is not YAML: %v", stdout, err)
	}
	want := map[string]any{
		"apiVersion": "operator.ptah.run/v1alpha1",
		"kind":       "PtahSchemaApproval",
		"metadata": map[string]any{
			"generateName": "storefront-",
			"namespace":    planreadtest.Namespace,
		},
		"spec": map[string]any{
			"schemaRef":       map[string]any{"name": "storefront", "uid": string(schema.UID)},
			"planRef":         map[string]any{"name": schema.Status.Plan.Name, "uid": string(schema.Status.Plan.UID)},
			"planFingerprint": fingerprint,
		},
	}
	if !reflect.DeepEqual(manifest, want) {
		t.Fatalf("the manifest is\n%s\nwant exactly %v", stdout, want)
	}
	// stdout is the YAML and nothing else, so a redirect is a file kubectl
	// create takes; who should create it goes to stderr.
	if document, err := yaml.Marshal(want); err != nil || stdout != string(document) {
		t.Fatalf("stdout is\n%s\nwant exactly\n%s", stdout, document)
	}
	const creator = "Create this approval as the person who decided: admission records whoever creates it as the approver, " +
		"a GitOps controller that applies it included.\n"
	if strings.Count(stderr, creator) != 1 || strings.Contains(stdout, "GitOps") {
		t.Fatalf("stderr %q does not carry %q once, or stdout carries it", stderr, creator)
	}

	_, again, _ := runApprove(t, api, "", false, "schema/storefront", "--dry-run", "client", "--plan-fingerprint", fingerprint)
	_, named, _ := runApprove(t, api, "", false, "schema/storefront", "--dry-run", "client", "--plan-fingerprint", fingerprint, "--name", "storefront-reviewed")
	if again != stdout || !strings.Contains(named, "  name: storefront-reviewed\n") || strings.Contains(named, "generateName") {
		t.Fatalf("printing the approval of one plan again gave\n%s\nand --name gave\n%s", again, named)
	}
}

// A printed approval and a sent one are named the same way, so a file behaves
// as the command does when it is created. The metadata is compared with what
// the command sent, not with a name written here.
func TestApproveNamesAPrintedApprovalAsItNamesASentOne(t *testing.T) {
	for _, test := range []struct {
		name  string
		flags []string
	}{
		{name: "no name given"},
		{name: "a name given", flags: []string{"--name", "storefront-reviewed"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			api, fingerprint := approvable(t, nil)
			arguments := append([]string{"schema/storefront", "--plan-fingerprint", fingerprint}, test.flags...)

			code, stdout, stderr := runApprove(t, api, "", false, append(arguments, "--dry-run", "client")...)
			if code != exitOK {
				t.Fatalf("approve --dry-run=client = %d, stderr %q", code, stderr)
			}
			var manifest struct {
				Metadata map[string]string `json:"metadata"`
			}
			if err := yaml.Unmarshal([]byte(stdout), &manifest); err != nil {
				t.Fatalf("the manifest %q is not YAML: %v", stdout, err)
			}

			code, _, stderr = runApprove(t, api, "", false, arguments...)
			if code != exitOK || len(api.created) != 1 {
				t.Fatalf("approve = %d after %d creates, stderr %q", code, len(api.created), stderr)
			}
			sent := api.created[0]
			if got := manifest.Metadata["generateName"]; got != sent.GetGenerateName() {
				t.Fatalf("the manifest has generateName %q, the command sent %q", got, sent.GetGenerateName())
			}
			if len(test.flags) == 0 && (sent.GetGenerateName() == "" || manifest.Metadata["name"] != "") {
				t.Fatalf("with no name given the command sent generateName %q and the manifest has name %q",
					sent.GetGenerateName(), manifest.Metadata["name"])
			}
			if len(test.flags) != 0 && manifest.Metadata["name"] != sent.GetName() {
				t.Fatalf("the manifest is named %q, the command sent %q", manifest.Metadata["name"], sent.GetName())
			}
		})
	}
}

// A manifest is an approval waiting to be created, so it takes the same
// decision: the fingerprint of the plan reviewed, or a review answered yes.
func TestApproveOnAClientDryRunTakesTheSameDecision(t *testing.T) {
	t.Run("a fingerprint that is not the current plan's", func(t *testing.T) {
		api, _ := approvable(t, nil)

		code, stdout, stderr := runApprove(t, api, "", false, "schema/storefront", "--dry-run", "client", "--plan-fingerprint", "sha256:reviewed-yesterday")

		if code != exitNotDone || stdout != "" || !strings.Contains(stderr, "not the sha256:reviewed-yesterday given") {
			t.Fatalf("approve --dry-run=client = %d, stdout %q, stderr %q", code, stdout, stderr)
		}
	})
	t.Run("no fingerprint and no terminal", func(t *testing.T) {
		api, _ := approvable(t, nil)

		code, stdout, stderr := runApprove(t, api, "", false, "schema/storefront", "--dry-run", "client")

		if code != exitUsage || stdout != "" || !strings.Contains(stderr, "--plan-fingerprint") {
			t.Fatalf("approve --dry-run=client = %d, stdout %q, stderr %q", code, stdout, stderr)
		}
	})
	t.Run("a review answered yes", func(t *testing.T) {
		api, fingerprint := approvable(t, nil)

		code, stdout, stderr := runApprove(t, api, "yes\n", true, "schema/storefront", "--dry-run", "client")

		// The review goes beside the prompt; stdout is the manifest alone.
		var manifest map[string]any
		if code != exitOK || yaml.Unmarshal([]byte(stdout), &manifest) != nil || manifest["kind"] != "PtahSchemaApproval" {
			t.Fatalf("approve --dry-run=client after yes = %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		if !strings.Contains(stderr, "ALTER TABLE orders DROP COLUMN legacy_note;") || !strings.Contains(stderr, fingerprint) ||
			!strings.Contains(stderr, "Type yes to print it") || strings.Contains(stdout, "ALTER TABLE") {
			t.Fatalf("the review did not go to stderr: stdout %q, stderr %q", stdout, stderr)
		}
	})
	t.Run("a review answered no", func(t *testing.T) {
		api, _ := approvable(t, nil)

		code, stdout, stderr := runApprove(t, api, "no\n", true, "schema/storefront", "--dry-run", "client")

		if code != exitNotDone || stdout != "" || len(api.created) != 0 {
			t.Fatalf("approve --dry-run=client after no = %d, stdout %q, stderr %q", code, stdout, stderr)
		}
	})
	t.Run("a gate closed for now", func(t *testing.T) {
		// The operator reading again at its interval closes the gate for a
		// while and leaves the plan current: the manifest is still that
		// plan's, and the person is told admission would refuse it now.
		api, fingerprint := approvable(t, func(status *operatorv1alpha1.PtahSchemaStatus) {
			status.Phase = operatorv1alpha1.PhaseObserving
			status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{Type: operatorv1alpha1.OperationObserve, ID: "op"}
		})

		code, stdout, stderr := runApprove(t, api, "", false, "schema/storefront", "--dry-run", "client", "--plan-fingerprint", fingerprint)

		if code != exitOK || !strings.Contains(stdout, "planFingerprint: "+fingerprint) || !strings.Contains(stderr, "Admission would refuse this approval now") {
			t.Fatalf("approve --dry-run=client = %d, stdout %q, stderr %q", code, stdout, stderr)
		}
	})
}

// --timeout bounds each conversation with the API server. A person who reads
// the plan for longer than that has not let the create's deadline pass.
func TestApproveGivesTheCreateItsOwnDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api, _ := approvable(t, nil)
		reader := &person{reads: time.Minute, answer: strings.NewReader("yes\n")}

		code, _, stderr := runApproveWith(t, deadlines{api}, planreadtest.Namespace, reader, true, "schema/storefront", "--timeout", "30s")

		if code != exitOK || len(api.created) != 1 {
			t.Fatalf("approve after a minute's review = %d after %d creates; stderr %q", code, len(api.created), stderr)
		}
	})
}

// The answer to an approval is printed as the API server holds it, kind and
// all; with -o yaml or json stdout carries that object and nothing else.
func TestApprovePrintsTheApprovalAsStored(t *testing.T) {
	api, fingerprint := approvable(t, nil)

	code, stdout, _ := runApprove(t, api, "", false, "schema/storefront", "--plan-fingerprint", fingerprint)

	if code != exitOK || !strings.HasPrefix(stdout, "Approved: PtahSchemaApproval "+planreadtest.Namespace+"/storefront-") {
		t.Fatalf("approve = %d, printed %q", code, stdout)
	}

	for _, format := range []string{"yaml", "json"} {
		api, _ := approvable(t, nil)

		code, stdout, stderr := runApprove(t, api, "yes\n", true, "schema/storefront", "-o", format)

		var approval operatorv1alpha1.PtahSchemaApproval
		if code != exitOK || yaml.UnmarshalStrict([]byte(stdout), &approval) != nil {
			t.Fatalf("approve -o %s = %d, stdout %q, stderr %q", format, code, stdout, stderr)
		}
		if approval.APIVersion != "operator.ptah.run/v1alpha1" || approval.Kind != "PtahSchemaApproval" ||
			approval.Spec.Approver.Username != "alice@example.com" || approval.Spec.PlanFingerprint != fingerprint {
			t.Fatalf("approve -o %s printed %+v", format, approval)
		}
		if !strings.Contains(stderr, "ALTER TABLE orders DROP COLUMN legacy_note;") {
			t.Fatalf("approve -o %s did not show the review on stderr: %q", format, stderr)
		}
	}
}

// A migration whose status names a plan that the object stored under the
// plan's name is not: nothing can approve it, so no mode shows, prints or
// sends anything bound to that object.
func TestApproveRefusesAMigrationPlanThatIsNotStored(t *testing.T) {
	for _, test := range []struct {
		name        string
		input       string
		interactive bool
		arguments   []string
	}{
		{"on a terminal", "yes\n", true, nil},
		{"with the other object's fingerprint", "", false, []string{"--plan-fingerprint", "sha256:another"}},
		{"printing a manifest", "yes\n", true, []string{"--dry-run", "client"}},
		{"printing a manifest with the other object's fingerprint", "", false, []string{"--dry-run", "client", "--plan-fingerprint", "sha256:another"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			migration := &operatorv1alpha1.PtahMigration{
				ObjectMeta: metav1.ObjectMeta{Namespace: "application", Name: "shipments", UID: "uid-shipments"},
				Status: operatorv1alpha1.PtahMigrationStatus{
					Phase:   operatorv1alpha1.MigrationPhaseAwaitingApproval,
					History: &operatorv1alpha1.MigrationHistoryStatus{CurrentVersion: 13},
					Plan:    &operatorv1alpha1.ImmutableObjectReference{Name: "shipments-p-14", UID: "uid-p-14"},
					Conditions: []metav1.Condition{{
						Type: operatorv1alpha1.ConditionMigrationApprovalRequired, Status: metav1.ConditionTrue, Reason: "Waiting",
					}},
				},
			}
			another := &operatorv1alpha1.PtahMigrationPlan{
				ObjectMeta: metav1.ObjectMeta{Namespace: "application", Name: "shipments-p-14", UID: "uid-p-14-again"},
				Spec: operatorv1alpha1.PtahMigrationPlanSpec{
					ContractVersion: 1,
					MigrationRef:    operatorv1alpha1.ImmutableObjectReference{Name: "shipments", UID: "uid-shipments"},
					Fingerprint:     "sha256:another",
				},
			}
			api := &admitting{Client: planreadtest.NewClient(t, migration, another)}

			code, stdout, stderr := runApproveWith(t, api, "application", strings.NewReader(test.input), test.interactive,
				append([]string{"migration/shipments"}, test.arguments...)...)

			if code != exitNotDone || stdout != "" || len(api.created) != 0 ||
				!strings.Contains(stderr, "PtahMigrationPlan application/shipments-p-14 is not the plan PtahMigration shipments names") {
				t.Fatalf("approve = %d after %d creates, stdout %q, stderr %q", code, len(api.created), stdout, stderr)
			}
		})
	}
}

func TestApproveAsksAdmissionOnAServerDryRun(t *testing.T) {
	api, fingerprint := approvable(t, nil)

	code, stdout, _ := runApprove(t, api, "", false, "schema/storefront", "--plan-fingerprint", fingerprint, "--dry-run", "server")

	if code != exitOK || len(api.created) != 1 || !strings.Contains(stdout, "nothing was stored") {
		t.Fatalf("approve --dry-run=server = %d, %d creates, stdout %q", code, len(api.created), stdout)
	}
	if len(api.options) != 1 || api.options[0] != client.DryRunAll {
		t.Fatalf("the create was sent with %v, want a server dry run", api.options)
	}
}

func TestApproveExitsByWhatTheAPIServerSaid(t *testing.T) {
	approvals := operatorv1alpha1.GroupVersion.WithResource("ptahschemaapprovals").GroupResource()
	for _, test := range []struct {
		name   string
		refuse error
		code   int
		says   string
	}{
		{"RBAC", &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: http.StatusForbidden,
			Reason:  metav1.StatusReasonForbidden,
			Message: `ptahschemaapprovals.operator.ptah.run is forbidden: User "bob" cannot create resource "ptahschemaapprovals" in API group "operator.ptah.run" in the namespace "application"`}},
			exitRefused, `User "bob" cannot create`},
		{"admission", &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: http.StatusForbidden,
			Reason:  metav1.StatusReasonForbidden,
			Message: `admission webhook "vptahschemaapproval.operator.ptah.run" denied the request: the author of the change may not approve it`}},
			exitRefused, "may not approve it"},
		// Admission passing on a refusal of the manager's own read is
		// admission's answer, not this identity's RBAC.
		{"admission wrapping the manager's refusal", &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: http.StatusForbidden,
			Message: `admission webhook "mapproval.operator.ptah.run" denied the request: read referenced plan: ptahschemaplans.operator.ptah.run "p" is forbidden: User "system:serviceaccount:ptah-system:ptah-operator" cannot get resource "ptahschemaplans" in API group "operator.ptah.run" in the namespace "application"`}},
			exitRefused, "denied the request"},
		{"the name taken", apierrors.NewAlreadyExists(approvals, "mine"), exitRefused, "The name is taken"},
		{"the plan vanished under admission", &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: http.StatusNotFound,
			Message: `admission webhook "mapproval.operator.ptah.run" denied the request: read referenced plan: ptahschemaplans.operator.ptah.run "p" not found`}},
			exitRefused, "something the approval names is gone"},
		{"unauthorized", apierrors.NewUnauthorized("Unauthorized"), exitRefused, "did not accept your credentials"},
		{"too many requests", apierrors.NewTooManyRequests("the server has received too many requests", 1), exitRefused, "Run the command again"},
		{"no answer", context.DeadlineExceeded, exitUnknown, "may have been stored"},
		{"a server error", apierrors.NewInternalError(errors.New("etcd")), exitUnknown, "may have been stored"},
	} {
		t.Run(test.name, func(t *testing.T) {
			api, fingerprint := approvable(t, nil)
			api.refuse = test.refuse

			code, _, stderr := runApprove(t, api, "", false, "schema/storefront", "--plan-fingerprint", fingerprint, "--name", "mine")

			if code != test.code || !strings.Contains(stderr, test.says) {
				t.Fatalf("approve = %d, stderr %q; want %d naming %q", code, stderr, test.code, test.says)
			}
		})
	}
}

func TestApproveNamesTheResourceItApproves(t *testing.T) {
	api, _ := approvable(t, nil)
	for _, arguments := range [][]string{{"storefront"}, {"schemas/storefront"}, {}, {"schema/storefront", "schema/other"}} {
		if code, _, _ := runApprove(t, api, "", false, arguments...); code != exitUsage {
			t.Fatalf("approve %q = %d, want usage", arguments, code)
		}
	}
	if code, _, _ := runApprove(t, api, "", false, "schema/absent", "--plan-fingerprint", "sha256:x"); code != exitAbsent {
		t.Fatalf("approve of no schema = %d, want absent", code)
	}
	if code, _, _ := runApprove(t, api, "", false, "migration/storefront", "--plan-fingerprint", "sha256:x"); code != exitAbsent {
		t.Fatalf("approve of a migration named like a schema = %d, want absent", code)
	}
	if len(api.created) != 0 {
		t.Fatalf("%d approvals were sent for commands that named nothing approvable", len(api.created))
	}
}

// approve reads the family as every other verb does: kubectl's spelling of
// the kind names the same resource.
func TestApproveTakesTheKindAsKubectlSpellsIt(t *testing.T) {
	api, fingerprint := approvable(t, nil)

	code, _, stderr := runApprove(t, api, "", false, "ptahschema/storefront", "--plan-fingerprint", fingerprint)

	if code != exitOK || len(api.created) != 1 {
		t.Fatalf("approve ptahschema/storefront = %d after %d creates; stderr %q", code, len(api.created), stderr)
	}
}
