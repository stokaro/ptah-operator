package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/operate"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// capturedSchemaReason is the reason the pinned Ptah image gave for a schema
// file, read from the `schema validate` output the harness captured, the way
// the runner reads it.
func capturedSchemaReason(t *testing.T, fixture string) string {
	t.Helper()
	reason, found := dataplane.SchemaSourceProblem(
		readCapture(t, "..", "dataplane", "testdata", "desired-source", fixture),
		string(operatorv1alpha1.DatabaseEnginePostgreSQL),
	)
	if !found {
		t.Fatalf("%s holds no reason about the file", fixture)
	}
	return reason
}

// relativeTimePtah answers the runner as Ptah v0.33.1 answers a PostgreSQL
// schema whose timestamptz column defaults to 'now': drift and plan fail with
// the parser's sentence, in the documents the harness captured for a file the
// parser refuses, and `schema validate` prints what the pinned image printed
// for that file.
type relativeTimePtah struct{ t *testing.T }

func (p relativeTimePtah) Execute(_ context.Context, spec runner.CommandSpec, stdout, _ io.Writer) (int, error) {
	if len(spec.Args) >= 2 && spec.Args[0] == "schema" {
		switch spec.Args[1] {
		case "drift":
			_, err := stdout.Write(relativeTimeFailure(p.t, "drift-sql.json"))
			return 2, err
		case "plan":
			_, err := stdout.Write(relativeTimeFailure(p.t, "plan-sql.json"))
			return 2, err
		case "validate":
			_, err := stdout.Write(readCapture(p.t, "..", "dataplane", "testdata", "desired-source", "validate-relative-time.txt"))
			return 1, err
		}
	}
	p.t.Errorf("the runner ran %s %v, which the capture has no answer for", spec.Path, spec.Args)
	return -1, context.Canceled
}

// relativeTimeFailure is the captured failure document with the sentence a
// relative time default gets.
func relativeTimeFailure(t *testing.T, fixture string) []byte {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(readCapture(t, "..", "dataplane", "testdata", "desired-source", fixture), &document); err != nil {
		t.Fatal(err)
	}
	document["error"] = map[string]any{"message": capturedSchemaReason(t, "validate-relative-time.txt")}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// capturedSchemaFault is the fault the pinned Ptah image gave for a schema it
// read and refuses as a whole, read the way the runner reads it: from the
// `schema validate` output the harness captured, held to the sentence the
// captured drift document failed with, and with the file the runner names in
// front of it.
func capturedSchemaFault(t *testing.T, validated, failed string) string {
	t.Helper()
	failure, err := dataplane.DecodeDriftFailure(readCapture(t, "..", "dataplane", "testdata", "desired-source", failed))
	if err != nil {
		t.Fatal(err)
	}
	fault, found := dataplane.SchemaStructuralProblem(
		readCapture(t, "..", "dataplane", "testdata", "desired-source", validated),
		string(operatorv1alpha1.DatabaseEnginePostgreSQL), failure.Message,
	)
	if !found {
		t.Fatalf("%s holds no fault %s stopped on", validated, failed)
	}
	return "schema.sql: " + fault
}

// domainFault is the fault Ptah v0.34.0 gives a timestamp domain whose own
// default is 'now' (stokaro/ptah#6386), as the runner reports it.
func domainFault(t *testing.T) string {
	t.Helper()
	fault := capturedSchemaFault(t, "validate-relative-time-domain.txt", "drift-relative-time-domain.json")
	if !strings.HasPrefix(fault, `schema.sql: domain "moment": DEFAULT 'now'`) {
		t.Fatalf("the captured fault %q is not the domain's relative time default", fault)
	}
	return fault
}

// relativeTimeDomainPtah answers the runner as Ptah v0.34.0 answers a
// PostgreSQL schema that declares a timestamp domain whose own default is
// 'now': drift and plan fail with the comparison's fault, naming the domain
// in the schema they placed it in, and `schema validate` prints the same fault
// with the name the file gives. Each answer is the document the harness
// captured from the pinned image.
type relativeTimeDomainPtah struct{ t *testing.T }

func (p relativeTimeDomainPtah) Execute(_ context.Context, spec runner.CommandSpec, stdout, _ io.Writer) (int, error) {
	if len(spec.Args) >= 2 && spec.Args[0] == "schema" {
		fixture, exit := "", 2
		switch spec.Args[1] {
		case "drift":
			fixture = "drift-relative-time-domain.json"
		case "plan":
			fixture = "plan-relative-time-domain.json"
		case "validate":
			fixture, exit = "validate-relative-time-domain.txt", 1
		}
		if fixture != "" {
			_, err := stdout.Write(readCapture(p.t, "..", "dataplane", "testdata", "desired-source", fixture))
			return exit, err
		}
	}
	p.t.Errorf("the runner ran %s %v, which the capture has no answer for", spec.Path, spec.Args)
	return -1, context.Canceled
}

// schemaRefusalRow is one of the answers that stop a schema's read: the code
// the runner reports, the reason the schema stops with, the words InSync
// uses for what Ptah did with the schema the Apply ran from, a word the
// reason has to hold, and a Ptah that answers as the pinned image answered
// for that file.
type schemaRefusalRow struct {
	name    string
	code    string
	reason  operatorv1alpha1.ConditionReason
	refused string
	says    string
	ptah    func(*testing.T) runner.Executor
}

func schemaRefusalRows() []schemaRefusalRow {
	return []schemaRefusalRow{
		{
			name: "a file Ptah cannot read", code: runner.CodeDesiredSourceUnreadable,
			reason: operatorv1alpha1.ReasonSchemaSourceUnreadable, refused: "could not read", says: "DEFAULT 'now'",
			ptah: func(t *testing.T) runner.Executor { return relativeTimePtah{t: t} },
		},
		{
			name: "a schema Ptah refuses", code: runner.CodeDesiredSchemaInvalid,
			reason: operatorv1alpha1.ReasonSchemaInvalid, refused: "refused", says: `domain "moment": DEFAULT 'now'`,
			ptah: func(t *testing.T) runner.Executor { return relativeTimeDomainPtah{t: t} },
		},
	}
}

func drainEvents(recorder *record.FakeRecorder) []string {
	var events []string
	for {
		select {
		case event := <-recorder.Events:
			events = append(events, event)
		default:
			return events
		}
	}
}

func eventsStartingWith(events []string, prefix string) []string {
	var found []string
	for _, event := range events {
		if strings.HasPrefix(event, prefix) {
			found = append(found, event)
		}
	}
	return found
}

// earlierInstant is a time the fixtures stamp what an earlier pass wrote with,
// apart from the reconciler's fixed clock, so a condition the pass rewrote and
// one it kept cannot read the same.
var earlierInstant = metav1.NewTime(readRequestClock.Add(-3 * time.Hour))

// unreadableReadFixture is a schema whose Observe or Plan finished with error,
// after an earlier attempt failed, a read opened the database, and a plan was
// approved: the approval it recorded exists.
func unreadableReadFixture(
	t *testing.T, claim operatorv1alpha1.OperationType, operation runner.Operation, failure *runner.ResultError,
	edits ...func(*operatorv1alpha1.PtahSchema),
) (*operatorv1alpha1.PtahSchema, *SchemaReconciler, client.Client, *record.FakeRecorder) {
	t.Helper()
	schema := readOnlySchemaClaim(t, claim)
	schema.Spec.Interval = metav1.Duration{Duration: 37 * time.Minute}
	schema.Status.NextReconciliationTime = earlierInstant.DeepCopy()
	for _, condition := range []metav1.Condition{
		{Type: operatorv1alpha1.ConditionReconciliationFailed, Status: metav1.ConditionTrue,
			Reason: string(operatorv1alpha1.ReasonOperationFailed), Message: "child_exit: an earlier attempt"},
		{Type: operatorv1alpha1.ConditionDatabaseReachable, Status: metav1.ConditionTrue,
			Reason: string(operatorv1alpha1.ReasonObserved), Message: "Database schema was observed"},
		{Type: operatorv1alpha1.ConditionApprovalRequired, Status: metav1.ConditionTrue,
			Reason: "ApprovalPending", Message: "The plan waits for an approval"},
	} {
		condition.LastTransitionTime = earlierInstant
		condition.ObservedGeneration = schema.Generation
		meta.SetStatusCondition(&schema.Status.Conditions, condition)
	}
	binding := schema.Status.ExecutionBinding
	schema.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{
		Name: "waiting-plan", UID: "waiting-plan-uid", ExecutionBindingID: binding.Epoch,
		ControllerStateVersion: binding.ControllerStateVersion, PtahVersion: binding.PtahVersion,
		ExecutorImage: binding.ExecutorImage, RunnerProtocolVersion: binding.RunnerProtocolVersion,
		Approval: &operatorv1alpha1.ConsumedApprovalStatus{Name: "recorded-approval", UID: "recorded-approval-uid"},
	}
	schema.Status.Source.VerificationPolicyUID = testPolicyUID
	schema.Status.Source.VerificationPolicyDigest = fingerprint.DigestBytes([]byte("policy"))
	for _, edit := range edits {
		edit(schema)
	}
	bindActiveInput(t, schema)
	approval := &operatorv1alpha1.PtahSchemaApproval{ObjectMeta: metav1.ObjectMeta{
		Namespace: schema.Namespace, Name: "recorded-approval", UID: "recorded-approval-uid", Generation: 1,
	}}
	policy := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: schema.Namespace, Name: schema.Spec.Desired.VerificationPolicyFrom.Name, UID: testPolicyUID},
		Immutable:  ptr(true),
		Data:       map[string]string{schema.Spec.Desired.VerificationPolicyFrom.Key: "policy"},
	}
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	frame := safetyRunnerFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: operation, OperationID: schema.Status.ActiveOperation.ID,
		ChildExitCode: 2, CoordinationDigest: schema.Status.ActiveOperation.CoordinationDigest, Error: failure,
	})
	reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod, approval, policy)
	reconciler.Locks = targetlock.New(api, api, nil)
	recorder := record.NewFakeRecorder(64)
	reconciler.Recorder = recorder
	return schema, reconciler, api, recorder
}

// The reproduction of stokaro/ptah-operator#1150 and #1158. Since Ptah v0.33.1
// a schema whose date or time column defaults to a relative time string, as
// 'now', is refused as a fault of the file, and every Observe and Plan fails
// with desired_source_unreadable. Since v0.34.0 a timestamp domain whose own
// default is 'now' is refused as a fault of the schema as a whole, which the
// runner reports as desired_schema_invalid, as it does a foreign key into a
// table the schema does not declare. The operator retried each at every
// failure retry: a Job and the database Lease every 30 seconds by default,
// ReconciliationFailed and an OperationFailed Warning each time, until a
// person changed the file. Another read of the same file gets the same answer,
// so the read now stops at Blocked with a reason for each answer and Ptah's
// sentence, publishes no plan, marks the recorded approval stale, says so once
// in a Warning, and reads again at the interval.
//
// An unreadable file says nothing about the target, so DatabaseReachable keeps
// what the earlier read said, which the rows give as opened and as not opened.
// A schema refused as a whole was compared with the database, so the refusal
// says the read opened it, from a fixture whose earlier read did not.
//
// The controls fail with codes a retry may change: each is a failed read as
// before, and runs again.
func TestAReadOfASchemaPtahCannotReadStopsBlocked(t *testing.T) {
	t.Parallel()
	relativeTime := capturedSchemaReason(t, "validate-relative-time.txt")
	if !strings.Contains(relativeTime, "DEFAULT 'now'") {
		t.Fatalf("the captured reason %q is not about a relative time default", relativeTime)
	}
	domain := domainFault(t)
	foreignKey := capturedSchemaFault(t, "validate-foreign-key.txt", "drift-foreign-key.json")
	unreadable := func(message string) runner.ResultError {
		return runner.ResultError{Code: runner.CodeDesiredSourceUnreadable, Message: message}
	}
	invalid := func(message string) runner.ResultError {
		return runner.ResultError{Code: runner.CodeDesiredSchemaInvalid, Message: message}
	}
	for _, row := range []struct {
		name    string
		claim   operatorv1alpha1.OperationType
		runner  runner.Operation
		failure runner.ResultError
		// reason is what the read stops with; none is a failure that is
		// retried.
		reason operatorv1alpha1.ConditionReason
		// unreached is an earlier read that did not open the database.
		unreached bool
	}{
		{
			name: "an Observe of a relative time default", claim: operatorv1alpha1.OperationObserve, runner: runner.OperationObserve,
			failure: unreadable(relativeTime), reason: operatorv1alpha1.ReasonSchemaSourceUnreadable,
		},
		{
			name: "an Observe of a relative time default after a read that did not open the database", claim: operatorv1alpha1.OperationObserve,
			runner: runner.OperationObserve, failure: unreadable(relativeTime), reason: operatorv1alpha1.ReasonSchemaSourceUnreadable, unreached: true,
		},
		{
			name: "a Plan of a relative time default", claim: operatorv1alpha1.OperationPlan, runner: runner.OperationPlan,
			failure: unreadable(relativeTime), reason: operatorv1alpha1.ReasonSchemaSourceUnreadable,
		},
		{
			name: "a Plan of a table option the parser refuses", claim: operatorv1alpha1.OperationPlan, runner: runner.OperationPlan,
			failure: unreadable(unreadableReason), reason: operatorv1alpha1.ReasonSchemaSourceUnreadable,
		},
		{
			name: "an Observe of a domain's relative time default", claim: operatorv1alpha1.OperationObserve, runner: runner.OperationObserve,
			failure: invalid(domain), reason: operatorv1alpha1.ReasonSchemaInvalid, unreached: true,
		},
		{
			name: "a Plan of a domain's relative time default", claim: operatorv1alpha1.OperationPlan, runner: runner.OperationPlan,
			failure: invalid(domain), reason: operatorv1alpha1.ReasonSchemaInvalid, unreached: true,
		},
		{
			name: "an Observe of a foreign key into a table the schema does not declare", claim: operatorv1alpha1.OperationObserve,
			runner: runner.OperationObserve, failure: invalid(foreignKey), reason: operatorv1alpha1.ReasonSchemaInvalid, unreached: true,
		},
		{
			name: "control: an Observe that ended with an exit status", claim: operatorv1alpha1.OperationObserve, runner: runner.OperationObserve,
			failure: runner.ResultError{Code: "child_exit", Message: "ptah exited with code 2"},
		},
		{
			// What the domain's default read as before the runner placed the
			// domain's name.
			name: "control: a Plan Ptah refused with no fault the runner tied to the file", claim: operatorv1alpha1.OperationPlan,
			runner:  runner.OperationPlan,
			failure: runner.ResultError{Code: "plan_refused", Message: "ptah refused to plan the change (declaration-refused)"},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			schema, reconciler, api, recorder := unreadableReadFixture(t, row.claim, row.runner, &row.failure,
				func(schema *operatorv1alpha1.PtahSchema) {
					if !row.unreached {
						return
					}
					meta.SetStatusCondition(&schema.Status.Conditions, metav1.Condition{
						Type: operatorv1alpha1.ConditionDatabaseReachable, Status: metav1.ConditionFalse,
						Reason: string(operatorv1alpha1.ReasonDatabaseUnreachable), Message: "the database did not answer",
						ObservedGeneration: schema.Generation, LastTransitionTime: earlierInstant,
					})
				})
			attempt := schema.Status.ActiveOperation.Attempt

			actual := reconcileSchema(t, reconciler, api, schema)

			said := row.failure.Code + ": " + row.failure.Message
			events := drainEvents(recorder)
			if row.reason == "" {
				failed := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
				if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Attempt != attempt+1 ||
					actual.Status.Phase != operatorv1alpha1.PhaseFailed || failed == nil || failed.Status != metav1.ConditionTrue ||
					failed.Reason != string(operatorv1alpha1.ReasonOperationFailed) || failed.Message != said {
					t.Fatalf("the failed read was not retried: phase %s, claim %#v, ReconciliationFailed %#v",
						actual.Status.Phase, actual.Status.ActiveOperation, failed)
				}
				for _, reason := range []operatorv1alpha1.ConditionReason{operatorv1alpha1.ReasonSchemaSourceUnreadable, operatorv1alpha1.ReasonSchemaInvalid} {
					if found := eventsStartingWith(events, corev1.EventTypeWarning+" "+string(reason)); len(found) != 0 {
						t.Fatalf("a failure a retry may change was reported as %s: %q", reason, found)
					}
				}
				return
			}

			if actual.Status.ActiveOperation != nil || actual.Status.Phase != operatorv1alpha1.PhaseBlocked ||
				actual.Status.ObservedGeneration != schema.Generation || actual.Status.Plan != nil {
				t.Fatalf("status after the refusal: phase %s, claim %#v, observed %d, plan %#v",
					actual.Status.Phase, actual.Status.ActiveOperation, actual.Status.ObservedGeneration, actual.Status.Plan)
			}
			if slices.Contains(actual.Finalizers, activeOperationFinalizer) {
				t.Fatalf("the refusal kept the operation finalizer: %v", actual.Finalizers)
			}
			// One interval from the read's end, on the reconciler's clock, and
			// not the failure retry; the fixture's deadline had passed.
			if want := reconciler.now().Add(37 * time.Minute); actual.Status.NextReconciliationTime == nil ||
				!actual.Status.NextReconciliationTime.Time.Equal(want) {
				t.Fatalf("NextReconciliationTime = %v, want %v", actual.Status.NextReconciliationTime, want)
			}
			for _, expected := range []struct {
				condition string
				status    metav1.ConditionStatus
				reason    operatorv1alpha1.ConditionReason
				message   string
			}{
				{operatorv1alpha1.ConditionReady, metav1.ConditionFalse, row.reason, said},
				{operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, row.reason, ""},
				{operatorv1alpha1.ConditionDriftDetected, metav1.ConditionUnknown, row.reason, ""},
				{operatorv1alpha1.ConditionInSync, metav1.ConditionUnknown, row.reason, ""},
				{operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionFalse, operatorv1alpha1.ReasonPlanNoLongerCurrent, ""},
				{operatorv1alpha1.ConditionReconciliationFailed, metav1.ConditionFalse, operatorv1alpha1.ReasonSucceeded, ""},
			} {
				found := findCondition(actual.Status.Conditions, expected.condition)
				if found == nil || found.Status != expected.status || found.Reason != string(expected.reason) ||
					(expected.message != "" && found.Message != expected.message) {
					t.Fatalf("%s = %#v, want %s with %s %q", expected.condition, found, expected.status, expected.reason, expected.message)
				}
			}
			reachable := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionDatabaseReachable)
			switch {
			case row.reason == operatorv1alpha1.ReasonSchemaInvalid:
				// The comparison that refused the schema read the database.
				if reachable == nil || reachable.Status != metav1.ConditionTrue || reachable.Reason != string(operatorv1alpha1.ReasonObserved) ||
					reachable.LastTransitionTime.Equal(&earlierInstant) {
					t.Fatalf("DatabaseReachable = %#v, want True Observed, written by this read", reachable)
				}
			case row.unreached:
				if reachable == nil || reachable.Status != metav1.ConditionFalse || !reachable.LastTransitionTime.Equal(&earlierInstant) {
					t.Fatalf("DatabaseReachable = %#v, want the earlier read's False, from %v", reachable, earlierInstant)
				}
			default:
				// Nothing in the refusal says the target answered or did not.
				if reachable == nil || reachable.Status != metav1.ConditionTrue || !reachable.LastTransitionTime.Equal(&earlierInstant) {
					t.Fatalf("DatabaseReachable = %#v, want the earlier read's, from %v", reachable, earlierInstant)
				}
			}
			approval := &operatorv1alpha1.PtahSchemaApproval{}
			if err := api.Get(context.Background(), client.ObjectKey{Namespace: schema.Namespace, Name: "recorded-approval"}, approval); err != nil {
				t.Fatal(err)
			}
			if stale := findCondition(approval.Status.Conditions, operatorv1alpha1.ConditionApprovalStale); stale == nil ||
				stale.Status != metav1.ConditionTrue || stale.Reason != string(operatorv1alpha1.ReasonPlanNoLongerCurrent) {
				t.Fatalf("ApprovalStale = %#v, want the recorded approval stale", stale)
			}
			warning := corev1.EventTypeWarning + " " + string(row.reason)
			if found := eventsStartingWith(events, warning+" "); len(found) != 1 || found[0] != warning+" "+said {
				t.Fatalf("%s Warnings = %q, want one saying %q", row.reason, found, said)
			}
			if found := eventsStartingWith(events, corev1.EventTypeWarning+" OperationFailed"); len(found) != 0 {
				t.Fatalf("the refusal was reported as a failed operation: %q", found)
			}
			if actual.Status.Journal != nil {
				for _, entry := range actual.Status.Journal.Entries {
					if entry.Type == operatorv1alpha1.JournalOperationFailed {
						t.Fatalf("the journal records the refusal as a failure: %#v", entry)
					}
				}
			}
		})
	}
}

var updateRefusalReading = flag.Bool("update", false,
	"rewrite the readings under testdata from what the reconciler stores now")

// refusalReadingPath and invalidReadingPath are the stored schemas of an
// Observe that could not read its file and of one that met a schema Ptah
// refuses. The e2e predicates that recognize each refusal in a cluster are
// tested against them (test/e2e/unreadable_source_test.go).
var (
	refusalReadingPath = filepath.Join("testdata", "schema-source-unreadable.json")
	invalidReadingPath = filepath.Join("testdata", "schema-invalid.json")
)

// TestTheRefusalReadingIsWhatTheReconcilerStores keeps those readings to the
// documents the reconciler stores, so the e2e predicates cannot pass against
// a state the controller never writes. The condition transition times come
// from the wall clock and are left out; every other field the refusal writes
// is what the reconciler wrote, on its fixed clock.
func TestTheRefusalReadingIsWhatTheReconcilerStores(t *testing.T) {
	t.Parallel()
	for _, reading := range []struct {
		path    string
		failure runner.ResultError
	}{
		{refusalReadingPath, runner.ResultError{Code: runner.CodeDesiredSourceUnreadable, Message: capturedSchemaReason(t, "validate-relative-time.txt")}},
		{invalidReadingPath, runner.ResultError{Code: runner.CodeDesiredSchemaInvalid, Message: domainFault(t)}},
	} {
		t.Run(filepath.Base(reading.path), func(t *testing.T) {
			t.Parallel()
			schema, reconciler, api, _ := unreadableReadFixture(t, operatorv1alpha1.OperationObserve, runner.OperationObserve, &reading.failure)
			stored := reconcileSchema(t, reconciler, api, schema)
			if stored.Status.Phase != operatorv1alpha1.PhaseBlocked {
				t.Fatalf("the fixture did not stop: phase %s", stored.Status.Phase)
			}
			conditions := slices.Clone(stored.Status.Conditions)
			for index := range conditions {
				conditions[index].LastTransitionTime = metav1.Time{}
			}
			document := map[string]any{
				"apiVersion": operatorv1alpha1.GroupVersion.String(),
				"kind":       "PtahSchema",
				"metadata":   map[string]any{"namespace": stored.Namespace, "name": stored.Name, "generation": stored.Generation},
				"status": map[string]any{
					"phase":                  stored.Status.Phase,
					"observedGeneration":     stored.Status.ObservedGeneration,
					"lastAttemptTime":        stored.Status.LastAttemptTime,
					"nextReconciliationTime": stored.Status.NextReconciliationTime,
					"activeOperation":        stored.Status.ActiveOperation,
					"plan":                   stored.Status.Plan,
					"conditions":             conditions,
				},
			}
			data, err := json.MarshalIndent(document, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			data = append(data, '\n')
			if *updateRefusalReading {
				if err := os.MkdirAll(filepath.Dir(reading.path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(reading.path, data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(reading.path)
			if err != nil {
				t.Fatalf("%v: run go test ./internal/controller -run TestTheRefusalReadingIsWhatTheReconcilerStores -update", err)
			}
			if !bytes.Equal(data, want) {
				t.Fatalf("the reconciler stores another reading than %s; run go test ./internal/controller "+
					"-run TestTheRefusalReadingIsWhatTheReconcilerStores -update and review the diff:\n%s", reading.path, data)
			}
		})
	}
}

// A schema stopped on its file reads again only when something could have
// changed the answer: at the interval, on a read request, or when its
// ConfigMap source changes. Between those, a pass claims nothing and builds no
// Job, where the retry it replaces dispatched one every failure retry. A file
// Ptah cannot read and a schema Ptah refuses stop the same way.
func TestASchemaStoppedOnItsFileReadsAgain(t *testing.T) {
	t.Parallel()
	for _, refusal := range schemaRefusalRows() {
		t.Run(refusal.name, func(t *testing.T) {
			t.Parallel()
			schemaStoppedOnItsFileReadsAgain(t, refusal)
		})
	}
}

func schemaStoppedOnItsFileReadsAgain(t *testing.T, refusal schemaRefusalRow) {
	message := capturedSchemaReason(t, "validate-relative-time.txt")
	source := "CREATE TABLE events (id bigint PRIMARY KEY, created_at timestamptz DEFAULT 'now');\n"
	if refusal.code == runner.CodeDesiredSchemaInvalid {
		message = domainFault(t)
		source = "CREATE DOMAIN moment AS timestamp DEFAULT 'now';\nCREATE TABLE events (id bigint PRIMARY KEY, created_at moment NOT NULL);\n"
	}
	refused := func(t *testing.T) (*operatorv1alpha1.PtahSchema, *SchemaReconciler, client.Client) {
		t.Helper()
		schema, reconciler, api, _ := unreadableReadFixture(t, operatorv1alpha1.OperationObserve, runner.OperationObserve,
			&runner.ResultError{Code: refusal.code, Message: message})
		blocked := reconcileSchema(t, reconciler, api, schema)
		if blocked.Status.Phase != operatorv1alpha1.PhaseBlocked || blocked.Status.ActiveOperation != nil {
			t.Fatalf("the fixture did not stop: phase %s, claim %#v", blocked.Status.Phase, blocked.Status.ActiveOperation)
		}
		return blocked, reconciler, api
	}

	t.Run("nothing before the interval", func(t *testing.T) {
		t.Parallel()
		blocked, reconciler, api := refused(t)
		jobs := jobNames(t, api, blocked.Namespace)
		// A minute before the deadline, well past any failure retry.
		almost := blocked.Status.NextReconciliationTime.Add(-time.Minute)
		reconciler.Clock = func() time.Time { return almost }
		for range 3 {
			again := reconcileSchema(t, reconciler, api, blocked)
			if again.Status.ActiveOperation != nil || again.Status.Phase != operatorv1alpha1.PhaseBlocked {
				t.Fatalf("a pass before the interval claimed %#v in phase %s", again.Status.ActiveOperation, again.Status.Phase)
			}
		}
		if after := jobNames(t, api, blocked.Namespace); !slices.Equal(after, jobs) {
			t.Fatalf("a pass before the interval built a Job: %v, before %v", after, jobs)
		}
	})

	t.Run("at the interval", func(t *testing.T) {
		t.Parallel()
		blocked, reconciler, api := refused(t)
		reached := blocked.Status.NextReconciliationTime.Add(time.Second)
		reconciler.Clock = func() time.Time { return reached }
		cycle := reconcileSchema(t, reconciler, api, blocked)
		if cycle.Status.ActiveOperation == nil || cycle.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve {
			t.Fatalf("at the interval the schema claimed %#v, want a Resolve", cycle.Status.ActiveOperation)
		}
	})

	t.Run("on a read request", func(t *testing.T) {
		t.Parallel()
		blocked, reconciler, api := refused(t)
		request := reconciler.now().Format(time.RFC3339)
		annotateSchema(t, api, blocked, request)
		answered := reconcileSchema(t, reconciler, api, blocked)
		if answered.Status.LastReadRequest != operatorv1alpha1.ReadRequestToken(request) || answered.Status.NextReconciliationTime == nil ||
			answered.Status.NextReconciliationTime.After(reconciler.now()) {
			t.Fatalf("the request was not answered: lastReadRequest %q, next %v", answered.Status.LastReadRequest, answered.Status.NextReconciliationTime)
		}
		cycle := reconcileSchema(t, reconciler, api, blocked)
		if cycle.Status.ActiveOperation == nil || cycle.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve {
			t.Fatalf("after the request the schema claimed %#v, want a Resolve", cycle.Status.ActiveOperation)
		}
	})

	t.Run("when its ConfigMap source changes", func(t *testing.T) {
		t.Parallel()
		schema := configMapSchemaFixture()
		schema.Spec.Interval = metav1.Duration{Duration: time.Hour}
		reconciler, api := fakeReconciler(t, staticLogs{}, schema, sourceConfigMap(source))
		builder, key := leaderBuilder(t)
		reconciler.Client = uidByNameClient{Client: api}
		reconciler.Jobs = builder
		reconciler.SealKey = key
		reconciler.Locks = targetlock.New(api, api, nil)
		if resolved := reconcileSchema(t, reconciler, api, schema); resolved.Status.Phase != operatorv1alpha1.PhaseObserving {
			t.Fatalf("the fixture did not read its source: phase %s", resolved.Status.Phase)
		}
		claimed := reconcileSchema(t, reconciler, api, schema)
		claim := claimed.Status.ActiveOperation
		if claim == nil || claim.Type != operatorv1alpha1.OperationObserve {
			t.Fatalf("the fixture did not claim its Observe: %#v", claim)
		}
		first := claimed.Status.Source.Snapshot.DeepCopy()
		job := reconcileUntilJobNamed(t, reconciler, api, claimed, claim.JobName)
		result, frame := runCapturedRead(t, reconciler, job, operatorv1alpha1.OperationObserve, refusal.ptah(t))
		if result.Error == nil || result.Error.Code != refusal.code {
			t.Fatalf("the Observe returned %#v, want %s", result.Error, refusal.code)
		}
		finishBuiltJob(t, reconciler, api, job, frame)
		blocked := reconcileSchema(t, reconciler, api, schema)
		if blocked.Status.Phase != operatorv1alpha1.PhaseBlocked || blocked.Status.ActiveOperation != nil ||
			!conditionMatches(blocked.Status.Conditions, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, refusal.reason) {
			t.Fatalf("the ConfigMap schema did not stop: phase %s, claim %#v, conditions %#v",
				blocked.Status.Phase, blocked.Status.ActiveOperation, blocked.Status.Conditions)
		}
		if again := reconcileSchema(t, reconciler, api, schema); again.Status.ActiveOperation != nil {
			t.Fatalf("a pass with nothing changed claimed %#v", again.Status.ActiveOperation)
		}

		editSource(t, api, func(source *corev1.ConfigMap) {
			source.Data["schema.sql"] = "CREATE TABLE events (id bigint PRIMARY KEY, created_at timestamptz DEFAULT now());\n"
		})

		reread := reconcileSchema(t, reconciler, api, schema)
		if reread.Status.Phase != operatorv1alpha1.PhaseObserving || reread.Status.Source.Snapshot == nil ||
			reread.Status.Source.Snapshot.ContentDigest == first.ContentDigest {
			t.Fatalf("the edited source was not read at once: phase %s, snapshot %#v, before %#v",
				reread.Status.Phase, reread.Status.Source.Snapshot, first)
		}
	})
}

// uidByNameClient gives every object it creates the UID the API server would
// have assigned, as jobUIDByNameClient does a Job's: the source snapshot a Job
// mounts is named by UID too.
type uidByNameClient struct{ client.Client }

func (c uidByNameClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if object.GetUID() == "" {
		object.SetUID(types.UID(object.GetName() + "-uid"))
	}
	return c.Client.Create(ctx, object, options...)
}

// unreadableProofRead is a schema whose proof is about to dispatch its operation
// against undecidedDatabaseURL's target, with the real Job builder.
func unreadableProofRead(
	t *testing.T, operation operatorv1alpha1.OperationType, edits ...func(*operatorv1alpha1.PtahSchema),
) (*operatorv1alpha1.PtahSchema, *SchemaReconciler, client.Client, *record.FakeRecorder) {
	t.Helper()
	schema, reconciler, api := builtProofFixture(t, operation, append([]func(*operatorv1alpha1.PtahSchema){
		func(schema *operatorv1alpha1.PtahSchema) {
			target, err := runner.TargetIdentityDigest(undecidedDatabaseURL)
			if err != nil {
				t.Fatal(err)
			}
			schema.Status.PendingObservation.Plan.TargetIdentityDigest = target
			schema.Status.Target.IdentityDigest = target
		},
	}, edits...)...)
	recorder := record.NewFakeRecorder(64)
	reconciler.Recorder = recorder
	return schema, reconciler, api, recorder
}

// harvestUnreadableProofRead runs the proof's Job the controller built against
// the refusal's Ptah, holds the runner to the refusal's code and words, and
// takes the pass that harvests it. It returns what the runner said.
func harvestUnreadableProofRead(
	t *testing.T, reconciler *SchemaReconciler, api client.Client, schema *operatorv1alpha1.PtahSchema, refusal schemaRefusalRow,
) (*operatorv1alpha1.PtahSchema, string) {
	t.Helper()
	operation := schema.Status.ActiveOperation.Type
	job := reconcileUntilJobNamed(t, reconciler, api, schema, schema.Status.ActiveOperation.JobName)
	result, frame := runCapturedRead(t, reconciler, job, operation, refusal.ptah(t))
	if result.Error == nil || result.Error.Code != refusal.code || !strings.Contains(result.Error.Message, refusal.says) {
		t.Fatalf("the proof's %s returned %#v, want %s saying %q", operation, result.Error, refusal.code, refusal.says)
	}
	finishBuiltJob(t, reconciler, api, job, frame)
	return reconcileSchema(t, reconciler, api, schema), result.Error.Code + ": " + result.Error.Message
}

// The read after an Apply reads the snapshot the Apply ran from, which no edit
// changes, so a retry would get the same refusal at every failure retry while
// it held the database Lease. Kept as a pending observation with the claim
// retired, the proof would be claimed again at once, which is the loop #1143
// removed for a proof whose plan Ptah could not make.
//
// Here the execution binding the Apply ran under is still the configured one:
// the executor that planned the snapshot refuses it on the read after. A Ptah
// upgrade between the two retires the binding instead, and that proof ends
// Superseded (TestAProofWhoseReadCannotReadTheSchemaAfterAPtahUpgradeIsSuperseded).
//
// So the proof ends: nothing established what the Apply did, so no Applied
// record, the journal's Verified entry Unknown with the reason, the Lease and
// the finalizer released, the schema Blocked with Ptah's sentence until the
// interval, and the next pass claims nothing. Both reads are run by the runner
// the controller's Jobs name, against what the pinned image printed for the
// file.
func TestAProofWhoseReadCannotReadTheSchemaEnds(t *testing.T) {
	t.Parallel()
	for _, refusal := range schemaRefusalRows() {
		for _, operation := range []operatorv1alpha1.OperationType{operatorv1alpha1.OperationObserve, operatorv1alpha1.OperationPlan} {
			t.Run(refusal.name+"/"+string(operation), func(t *testing.T) {
				t.Parallel()
				proofWhoseReadCannotReadTheSchemaEnds(t, refusal, operation)
			})
		}
	}

	t.Run("control: a proof Observe that reads the schema keeps the proof and the Lease", func(t *testing.T) {
		t.Parallel()
		schema, reconciler, api, _ := unreadableProofRead(t, operatorv1alpha1.OperationObserve)
		job := reconcileUntilJobNamed(t, reconciler, api, schema, schema.Status.ActiveOperation.JobName)
		result, frame := runCapturedRead(t, reconciler, job, operatorv1alpha1.OperationObserve, capturedPtah{
			t: t, drift: withoutUndecided(t, undecidedCapture(t, "drift-undecided-publication-postgres.json")),
		})
		if result.Error != nil {
			t.Fatalf("the control's read returned %#v", result.Error)
		}
		finishBuiltJob(t, reconciler, api, job, frame)
		observed := reconcileSchema(t, reconciler, api, schema)
		if observed.Status.PendingObservation == nil || !observed.Status.PendingObservation.PlanRequired {
			t.Fatalf("the control's proof did not go on to its Plan: pending %#v", observed.Status.PendingObservation)
		}
		if holder := proofLeaseHolder(t, reconciler, api); holder == "" {
			t.Fatal("the control's proof released its Lease, so the row's release measured nothing")
		}
	})
}

// proofWhoseReadCannotReadTheSchemaEnds is one row of the test above.
func proofWhoseReadCannotReadTheSchemaEnds(t *testing.T, refusal schemaRefusalRow, operation operatorv1alpha1.OperationType) {
	schema, reconciler, api, recorder := unreadableProofRead(t, operation)
	pending := schema.Status.PendingObservation
	if holder := proofLeaseHolder(t, reconciler, api); holder == "" {
		t.Fatal("the fixture's proof holds no Lease, so a released one shows nothing")
	}

	actual, said := harvestUnreadableProofRead(t, reconciler, api, schema, refusal)

	if actual.Status.PendingObservation != nil || actual.Status.ActiveOperation != nil {
		t.Fatalf("the proof did not end: pending %#v, operation %#v", actual.Status.PendingObservation, actual.Status.ActiveOperation)
	}
	if actual.Status.Applied != nil {
		t.Fatalf("a proof that read nothing recorded the Apply as converged: %#v", actual.Status.Applied)
	}
	if slices.Contains(actual.Finalizers, activeOperationFinalizer) {
		t.Fatalf("the ended proof kept the finalizer: %v", actual.Finalizers)
	}
	if holder := proofLeaseHolder(t, reconciler, api); holder != "" || actual.Status.PendingLockRelease != nil {
		t.Fatalf("the ended proof still holds the Lease as %q, release owed %#v", holder, actual.Status.PendingLockRelease)
	}
	if actual.Status.Phase != operatorv1alpha1.PhaseBlocked || actual.Status.ObservedGeneration != pending.ApplyGeneration {
		t.Fatalf("phase %s at observed generation %d, want Blocked at the Apply's generation %d",
			actual.Status.Phase, actual.Status.ObservedGeneration, pending.ApplyGeneration)
	}
	if ready := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReady); ready == nil ||
		ready.Reason != string(refusal.reason) || ready.Message != said {
		t.Fatalf("Ready = %#v, want %s saying %q", ready, refusal.reason, said)
	}
	if inSync := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionInSync); inSync == nil ||
		inSync.Status != metav1.ConditionUnknown || !strings.Contains(inSync.Message, "Nothing proved the Apply converged") {
		t.Fatalf("InSync = %#v, want Unknown saying nothing proved the Apply", inSync)
	}
	if failed := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed); failed != nil &&
		failed.Status == metav1.ConditionTrue {
		t.Fatalf("the proof's end was reported as a failure: %#v", failed)
	}
	next := actual.Status.NextReconciliationTime
	if next == nil || next.Sub(reconciler.now()) < interval(actual)-time.Second {
		t.Fatalf("the next read is at %v, want %v after %v", next, interval(actual), reconciler.now())
	}
	verified := verifiedEntries(actual)
	if len(verified) != 1 || verified[0].Outcome != operatorv1alpha1.JournalUnknown ||
		verified[0].Reason != string(refusal.reason) ||
		verified[0].Plan != pending.Plan.Name || verified[0].OperationID != pending.ApplyOperationID {
		t.Fatalf("the journal records %#v, want one Verified Unknown, reason %s, for plan %s and Apply %s",
			verified, refusal.reason, pending.Plan.Name, pending.ApplyOperationID)
	}
	if warnings := warningsWithReason(recorder, string(refusal.reason)); len(warnings) != 1 ||
		warnings[0] != corev1.EventTypeWarning+" "+string(refusal.reason)+" "+said {
		t.Fatalf("Warnings = %q, want one %s saying %q", warnings, refusal.reason, said)
	}

	// The Job's cleanup write wakes the next pass at once. It claims
	// nothing and builds no Job.
	jobs := jobNames(t, api, schema.Namespace)
	again := reconcileSchema(t, reconciler, api, schema)
	if again.Status.ActiveOperation != nil || again.Status.PendingObservation != nil {
		t.Fatalf("the next pass claimed %#v with pending %#v", again.Status.ActiveOperation, again.Status.PendingObservation)
	}
	if after := jobNames(t, api, schema.Namespace); !slices.Equal(after, jobs) {
		t.Fatalf("the next pass built a Job: %v, before %v", after, jobs)
	}
	reached := next.Add(time.Second)
	reconciler.Clock = func() time.Time { return reached }
	cycle := reconcileSchema(t, reconciler, api, schema)
	if cycle.Status.PendingObservation != nil || cycle.Status.ActiveOperation == nil ||
		cycle.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve {
		t.Fatalf("at the interval the resource claimed %#v with pending %#v, want an ordinary Resolve",
			cycle.Status.ActiveOperation, cycle.Status.PendingObservation)
	}
}

// A proof that ends under a newer generation hands the resource to that
// generation: the refusal ends the proof as Superseded, credits nothing, and
// the next read is the newer inputs' at once, which may carry a file Ptah can
// read.
func TestAProofWhoseReadCannotReadTheSchemaUnderANewerGenerationIsSuperseded(t *testing.T) {
	t.Parallel()
	for _, refusal := range schemaRefusalRows() {
		t.Run(refusal.name, func(t *testing.T) {
			t.Parallel()
			proofUnderANewerGenerationIsSuperseded(t, refusal)
		})
	}
}

func proofUnderANewerGenerationIsSuperseded(t *testing.T, refusal schemaRefusalRow) {
	schema, reconciler, api, _ := unreadableProofRead(t, operatorv1alpha1.OperationPlan, func(schema *operatorv1alpha1.PtahSchema) {
		schema.Generation = schema.Status.PendingObservation.ApplyGeneration + 1
	})
	if schema.Generation == schema.Status.PendingObservation.ApplyGeneration {
		t.Fatal("the fixture's generation is the Apply's, so nothing newer is measured")
	}

	actual, _ := harvestUnreadableProofRead(t, reconciler, api, schema, refusal)

	if actual.Status.PendingObservation != nil || actual.Status.Applied != nil {
		t.Fatalf("pending %#v, applied %#v; want the proof ended and nothing credited",
			actual.Status.PendingObservation, actual.Status.Applied)
	}
	if actual.Status.Phase != operatorv1alpha1.PhasePending || actual.Status.NextReconciliationTime != nil {
		t.Fatalf("phase %s, next %v; want Pending with the next read now", actual.Status.Phase, actual.Status.NextReconciliationTime)
	}
	if !conditionMatches(actual.Status.Conditions, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonDesiredStateChanged) {
		t.Fatalf("Ready does not name the newer generation: %#v", actual.Status.Conditions)
	}
	if holder := proofLeaseHolder(t, reconciler, api); holder != "" {
		t.Fatalf("the ended proof still holds the Lease as %q", holder)
	}
	if verified := verifiedEntries(actual); len(verified) != 1 || verified[0].Outcome != operatorv1alpha1.JournalSuperseded {
		t.Fatalf("the journal records %#v, want one Verified Superseded", verified)
	}
	next := reconcileSchema(t, reconciler, api, schema)
	if next.Status.ActiveOperation == nil || next.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve {
		t.Fatalf("the pass after it claimed %#v, want the newer generation's Resolve", next.Status.ActiveOperation)
	}
}

// A Ptah upgrade between the Apply and its proof is how such a read usually
// meets a file Ptah cannot read: the new release refuses what the old one
// read. PtahVersion and ExecutorImage are execution-binding components, so the
// upgrade retires the binding, and the proof's read runs under the new epoch
// while its plan names the retired one (executionBindingChanged keeps the
// proof). The fixture is the state that rotation leaves.
//
// The binding decides what follows, as it does for a proof that read the
// database: the proof ends Superseded, the resource reads Pending with
// ExecutionBindingChanged, and the next pass reads the source at once under
// the new binding, where an ordinary read stops Blocked with the refusal's
// reason while the file stays as it is. Only the Warning Event names the file
// for the proof itself.
func TestAProofWhoseReadCannotReadTheSchemaAfterAPtahUpgradeIsSuperseded(t *testing.T) {
	t.Parallel()
	for _, refusal := range schemaRefusalRows() {
		t.Run(refusal.name, func(t *testing.T) {
			t.Parallel()
			proofAfterAPtahUpgradeIsSuperseded(t, refusal)
		})
	}
}

func proofAfterAPtahUpgradeIsSuperseded(t *testing.T, refusal schemaRefusalRow) {
	retired := "v1-" + strings.Repeat("ab", 16)
	schema, reconciler, api, recorder := unreadableProofRead(t, operatorv1alpha1.OperationObserve, func(schema *operatorv1alpha1.PtahSchema) {
		schema.Status.PendingObservation.Plan.ExecutionBindingID = retired
		schema.Status.PendingObservation.Plan.PtahVersion = "v0.33.0"
	})
	if schema.Status.ExecutionBinding == nil || schema.Status.ExecutionBinding.Epoch == retired {
		t.Fatalf("the fixture's binding is %#v, so the plan's epoch was not retired", schema.Status.ExecutionBinding)
	}
	if conditionMatches(schema.Status.Conditions, operatorv1alpha1.ConditionDatabaseReachable, metav1.ConditionUnknown, operatorv1alpha1.ReasonExecutionBindingChanged) {
		t.Fatalf("the fixture already says DatabaseReachable is unknown under the new binding, so the read could not be shown to leave it so: %#v",
			findCondition(schema.Status.Conditions, operatorv1alpha1.ConditionDatabaseReachable))
	}

	actual, said := harvestUnreadableProofRead(t, reconciler, api, schema, refusal)

	if actual.Status.PendingObservation != nil || actual.Status.ActiveOperation != nil || actual.Status.Applied != nil {
		t.Fatalf("pending %#v, operation %#v, applied %#v; want the proof ended and nothing credited",
			actual.Status.PendingObservation, actual.Status.ActiveOperation, actual.Status.Applied)
	}
	if actual.Status.Phase != operatorv1alpha1.PhasePending || actual.Status.NextReconciliationTime != nil {
		t.Fatalf("phase %s, next %v; want Pending with the next read now", actual.Status.Phase, actual.Status.NextReconciliationTime)
	}
	if !conditionMatches(actual.Status.Conditions, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonExecutionBindingChanged) ||
		!conditionMatches(actual.Status.Conditions, operatorv1alpha1.ConditionInSync, metav1.ConditionUnknown, operatorv1alpha1.ReasonExecutionBindingChanged) {
		t.Fatalf("Ready and InSync do not name the new binding: %#v", actual.Status.Conditions)
	}
	// The read ran under the retired binding, so what it says of the target is
	// historical evidence, even where Ptah refused the schema after it opened
	// the database.
	if !conditionMatches(actual.Status.Conditions, operatorv1alpha1.ConditionDatabaseReachable, metav1.ConditionUnknown, operatorv1alpha1.ReasonExecutionBindingChanged) {
		t.Fatalf("DatabaseReachable = %#v, want Unknown with ExecutionBindingChanged",
			findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionDatabaseReachable))
	}
	if verified := verifiedEntries(actual); len(verified) != 1 || verified[0].Outcome != operatorv1alpha1.JournalSuperseded {
		t.Fatalf("the journal records %#v, want one Verified Superseded", verified)
	}
	if holder := proofLeaseHolder(t, reconciler, api); holder != "" || actual.Status.PendingLockRelease != nil {
		t.Fatalf("the ended proof still holds the Lease as %q, release owed %#v", holder, actual.Status.PendingLockRelease)
	}
	if slices.Contains(actual.Finalizers, activeOperationFinalizer) {
		t.Fatalf("the ended proof kept the finalizer: %v", actual.Finalizers)
	}
	if warnings := warningsWithReason(recorder, string(refusal.reason)); len(warnings) != 1 ||
		warnings[0] != corev1.EventTypeWarning+" "+string(refusal.reason)+" "+said {
		t.Fatalf("Warnings = %q, want one %s saying %q", warnings, refusal.reason, said)
	}
	next := reconcileSchema(t, reconciler, api, schema)
	if next.Status.ActiveOperation == nil || next.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve ||
		next.Status.PendingObservation != nil {
		t.Fatalf("the pass after it claimed %#v with pending %#v, want a Resolve under the new binding",
			next.Status.ActiveOperation, next.Status.PendingObservation)
	}
}

// A verification policy that changed while the proof ran decides what follows
// as it does for a proof that read the database: the artifact is verified
// again first. InSync does not say the proof completed.
func TestAProofWhoseReadCannotReadTheSchemaUnderAChangedPolicyVerifiesAgain(t *testing.T) {
	t.Parallel()
	for _, refusal := range schemaRefusalRows() {
		t.Run(refusal.name, func(t *testing.T) {
			t.Parallel()
			proofUnderAChangedPolicyVerifiesAgain(t, refusal)
		})
	}
}

func proofUnderAChangedPolicyVerifiesAgain(t *testing.T, refusal schemaRefusalRow) {
	schema, reconciler, api, _ := unreadableProofRead(t, operatorv1alpha1.OperationPlan)
	policy := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: schema.Namespace, Name: schema.Spec.Desired.VerificationPolicyFrom.Name}
	if err := api.Get(context.Background(), key, policy); err != nil {
		t.Fatal(err)
	}
	if err := api.Delete(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	policy.ResourceVersion = ""
	policy.UID = "replaced-policy-uid"
	if err := api.Create(context.Background(), policy); err != nil {
		t.Fatal(err)
	}

	actual, _ := harvestUnreadableProofRead(t, reconciler, api, schema, refusal)

	if actual.Status.PendingObservation != nil || actual.Status.Applied != nil || actual.Status.Source.Verified {
		t.Fatalf("pending %#v, applied %#v, verified %t; want the proof ended, nothing credited and the source unverified",
			actual.Status.PendingObservation, actual.Status.Applied, actual.Status.Source.Verified)
	}
	if actual.Status.Phase != operatorv1alpha1.PhaseVerifying {
		t.Fatalf("phase %s, want Verifying", actual.Status.Phase)
	}
	if inSync := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionInSync); inSync == nil ||
		inSync.Reason != string(operatorv1alpha1.ReasonPolicyChanged) || strings.Contains(inSync.Message, "proof completed") ||
		!strings.Contains(inSync.Message, "Ptah "+refusal.refused+" the desired schema") {
		t.Fatalf("InSync = %#v, want PolicyChanged saying Ptah %s the schema", inSync, refusal.refused)
	}
	if verified := verifiedEntries(actual); len(verified) != 1 || verified[0].Outcome != operatorv1alpha1.JournalSuperseded {
		t.Fatalf("the journal records %#v, want one Verified Superseded", verified)
	}
	if holder := proofLeaseHolder(t, reconciler, api); holder != "" {
		t.Fatalf("the ended proof still holds the Lease as %q", holder)
	}
}

// What a person waiting on the Apply reads when its proof could not read the
// schema: wait --for applied, begun while the Apply ran, stops and says why,
// rather than that a person gave the proof up. settle --wait, after a proof
// stopped on another target, stops when the plan's target comes back and its
// read cannot read the schema: the statement did not end the proof.
//
// Each wait is read twice: on the document that ended the proof, and on the
// one the next pass writes when a read request reached the resource while the
// proof ran. That pass claims a Resolve and rewrites Ready, and a poll every
// two seconds can read it first.
func TestTheWaitsOfAProofThatCouldNotReadTheSchemaSayWhy(t *testing.T) {
	t.Parallel()
	for _, refusal := range []struct {
		name    string
		failure *runner.ResultError
		reason  operatorv1alpha1.ConditionReason
		refused string
	}{
		{"a file Ptah cannot read", &runner.ResultError{Code: runner.CodeDesiredSourceUnreadable, Message: unreadableReason},
			operatorv1alpha1.ReasonSchemaSourceUnreadable, "could not read"},
		{"a schema Ptah refuses", &runner.ResultError{Code: runner.CodeDesiredSchemaInvalid, Message: domainFault(t)},
			operatorv1alpha1.ReasonSchemaInvalid, "refused"},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			t.Parallel()
			// readAfterTheProof answers a read request on the pass after the proof
			// ended, and takes the pass that claims the read it asks for.
			readAfterTheProof := func(t *testing.T, reconciler *SchemaReconciler, api client.Client, schema *operatorv1alpha1.PtahSchema) {
				t.Helper()
				stored := storedSchema(t, api, schema)
				if stored.Annotations == nil {
					stored.Annotations = map[string]string{}
				}
				stored.Annotations[operatorv1alpha1.ReadRequestAnnotation] = reconciler.now().Format(time.RFC3339)
				if err := api.Update(context.Background(), stored); err != nil {
					t.Fatal(err)
				}
				reconcileSchema(t, reconciler, api, schema)
				claimed := reconcileSchema(t, reconciler, api, schema)
				if claimed.Status.ActiveOperation == nil || claimed.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve {
					t.Fatalf("the read request claimed %#v, want a Resolve", claimed.Status.ActiveOperation)
				}
				if ready := findCondition(claimed.Status.Conditions, operatorv1alpha1.ConditionReady); ready == nil ||
					ready.Reason == string(refusal.reason) {
					t.Fatalf("Ready = %#v after the claim, so the later document measures nothing", ready)
				}
			}

			t.Run("wait --for applied", func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				schema, plan, policyConfig := dispatchedApplyWithPlan(t)
				job, pod := terminalWorkload(schema, batchv1.JobComplete)
				applied := runner.Result{
					ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationApply, OperationID: schema.Status.ActiveOperation.ID,
					MutationStarted: true, CoordinationDigest: schema.Status.Plan.CoordinationDigest,
					TargetIdentityDigest: schema.Status.Plan.TargetIdentityDigest,
				}
				reconciler, api := fakeReconciler(t, staticLogs{content: safetyRunnerFrame(t, applied)}, schema, plan, policyConfig, job, pod)
				reconciler.Locks = targetlock.New(api, api, nil)
				waiting := safetyGetSchema(t, api, schema)
				if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
					t.Fatalf("Reconcile() error = %v", err)
				}
				harvested := safetyGetSchema(t, api, schema)
				target := harvested.Status.PendingObservation.Plan.TargetIdentityDigest
				for _, object := range []client.Object{pod, job} {
					if err := api.Delete(ctx, object); err != nil {
						t.Fatal(err)
					}
				}
				observing := reconcileSchema(t, reconciler, api, harvested)
				ended := nextProofRead(t, reconciler, api, observing, "observe", target, refusal.failure)
				if ended.Status.PendingObservation != nil {
					t.Fatalf("the proof did not end: phase %s, pending %#v", ended.Status.Phase, ended.Status.PendingObservation)
				}

				ref := operate.Ref{Family: operate.FamilySchema, Namespace: schema.Namespace, Name: schema.Name}
				for _, read := range []string{"the document that ended the proof", "the document the next pass wrote"} {
					if read == "the document the next pass wrote" {
						readAfterTheProof(t, reconciler, api, schema)
					}

					_, err := waitOn(api, ref, operate.WaitOptions{Target: operate.TargetApplied, Since: waiting})

					var stop *operate.StopError
					if !errors.As(err, &stop) || stop.Reason != operate.StopUnresolved ||
						!strings.Contains(stop.Message, "plan "+plan.Name+" ended because Ptah "+refusal.refused+" the desired schema") {
						t.Fatalf("wait on %s = %v, want a %s stop saying Ptah %s the schema after plan %s",
							read, err, operate.StopUnresolved, refusal.refused, plan.Name)
					}
				}
			})

			t.Run("settle --wait", func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
				target := blocked.Status.PendingObservation.Plan.TargetIdentityDigest
				ref := operate.Ref{Family: operate.FamilySchema, Namespace: blocked.Namespace, Name: blocked.Name}
				settled, err := operate.Settle(ctx, api, ref, operate.SettleOptions{})
				if err != nil || !settled.Changed {
					t.Fatalf("Settle = %#v, %v; want the printed value written", settled, err)
				}
				ended := nextProofRead(t, reconciler, api, blocked, "second", target, refusal.failure)
				if ended.Status.PendingObservation != nil {
					t.Fatalf("the proof did not end: phase %s, pending %#v", ended.Status.Phase, ended.Status.PendingObservation)
				}
				if entry := lastJournalEntry(t, ended); entry.Type != operatorv1alpha1.JournalVerified || entry.Outcome != operatorv1alpha1.JournalUnknown {
					t.Fatalf("journal ends with %#v, want Verified Unknown, which a settlement records too", entry)
				}

				for _, read := range []string{"the document that ended the proof", "the document the next pass wrote"} {
					if read == "the document the next pass wrote" {
						readAfterTheProof(t, reconciler, api, blocked)
					}

					_, err = waitOn(api, ref, operate.WaitOptions{
						Target: operate.TargetSettled, Since: settled.Object, Settlement: settled.Printed.Value,
					})

					var stop *operate.StopError
					if !errors.As(err, &stop) || stop.Reason != operate.StopProofEnded ||
						!strings.Contains(stop.Message, "because Ptah "+refusal.refused+" the desired schema") {
						t.Fatalf("settle --wait on %s = %v, want a %s stop saying Ptah %s the schema",
							read, err, operate.StopProofEnded, refusal.refused)
					}
				}
			})
		})
	}
}
