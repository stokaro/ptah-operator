package main

import (
	"bytes"
	"context"
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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
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

	if code != exitNotDone || len(api.created) != 0 || !strings.Contains(stderr, "The schema is Blocked, not waiting for an approval") {
		t.Fatalf("approve = %d, %d creates, stderr %q", code, len(api.created), stderr)
	}

	// On a terminal the reason comes before the review: a plan nobody may
	// approve now is not put in front of a person as if they could.
	api, _ = approvable(t, closed)

	code, stdout, stderr := runApprove(t, api, "yes\n", true, "schema/storefront")

	if code != exitNotDone || stdout != "" || len(api.created) != 0 ||
		!strings.Contains(stderr, "is Blocked") || strings.Contains(stderr, "Type yes") {
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
	if _, schema := object.(*operatorv1alpha1.PtahSchema); schema {
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
			requireSays(t, "approve's stderr", stderr, "Type yes to approve: ",
				"The operator is reading this schema again (Resolve); admission takes an approval once it is back at the approval gate.",
				"is approved when it is back at the approval gate, if it is still the plan waiting; waiting up to 5m0s.")
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
			requireSays(t, "approve's stderr", stderr,
				"The operator is reading this schema again (Observe); admission takes an approval once it is back at the approval gate. "+
					"A decision taken now is sent when that read ends, if this plan is still the one waiting.",
				"Type yes to approve: ")
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
			requireSays(t, "approve's stderr", stderr, "The operator was still reading schema/storefront again after 5m0s. Nothing was approved")
		})
	})
}

// The manifest is the decision and nothing else, under a name kubectl create
// takes: no generateName, no empty approver, no null time, no status.
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
			"name":      "storefront-" + strings.TrimPrefix(fingerprint, "sha256:")[:12],
			"namespace": planreadtest.Namespace,
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
	if again != stdout || !strings.Contains(named, "name: storefront-reviewed\n") {
		t.Fatalf("printing the approval of one plan again gave\n%s\nand --name gave\n%s", again, named)
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
