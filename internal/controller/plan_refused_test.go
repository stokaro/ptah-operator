package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// harnessDocument reads a document the runner harness stored for a row.
func harnessDocument(t *testing.T, path ...string) []byte {
	t.Helper()
	return readCapture(t, append([]string{"..", "dataplane", "testdata", "runner-harness", "documents"}, path...)...)
}

// refusedViewPlanPtah answers `schema plan` as the pinned Ptah answered the
// runner harness row differs-view-invoker-unstated-postgres: the view holds
// security_invoker=true and the schema does not state it, so the plan is
// refused with the code view-security-option and exit status 2.
func refusedViewPlanPtah(t *testing.T) plannedNothingPtah {
	t.Helper()
	return plannedNothingPtah{
		t: t, exitCode: 2,
		report: harnessDocument(t, "differs-view-invoker-unstated-postgres", "plan", "1-schema-plan.json"),
	}
}

// planRefusingPtah answers an Observe and a Plan as the pinned Ptah answered
// the runner harness row differs-view-owner-missing-postgres, whose desired
// schema gives a view to a role the server does not hold: the drift report
// finds the view changed, the plan saves the ALTER VIEW ... OWNER TO, and the
// dry run of the saved plan answers dryRun. plan replaces the plan's answer
// where it is set. Every document is the stored one; the plan is saved where
// the runner asks, and its report and the dry run name the digest of what was
// saved there.
type planRefusingPtah struct {
	t      *testing.T
	plan   []byte
	dryRun func(saved []byte) []byte
}

// ownerPlan is the plan the harness row saved, as the bytes Ptah wrote and the
// report it printed.
func ownerPlan(t *testing.T) (saved, report []byte) {
	t.Helper()
	report = harnessDocument(t, "differs-view-owner-missing-postgres", "plan", "0-schema-plan.json")
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(report, &fields); err != nil {
		t.Fatal(err)
	}
	return fields["plan"], report
}

func (p planRefusingPtah) Execute(_ context.Context, spec runner.CommandSpec, stdout, _ io.Writer) (int, error) {
	if len(spec.Args) < 2 || spec.Args[0] != "schema" {
		p.t.Errorf("the runner ran %s %v, which the capture has no answer for", spec.Path, spec.Args)
		return -1, context.Canceled
	}
	saved, report := ownerPlan(p.t)
	switch spec.Args[1] {
	case "drift":
		_, err := stdout.Write(harnessDocument(p.t, "differs-view-owner-missing-postgres", "observe", "0-schema-drift.json"))
		return 1, err
	case "plan":
		if p.plan != nil {
			_, err := stdout.Write(p.plan)
			return 2, err
		}
		output := argumentAfter(spec.Args, "--output")
		if output == "" {
			p.t.Fatalf("the plan read names no --output: %v", spec.Args)
		}
		if err := os.WriteFile(output, saved, 0o600); err != nil {
			p.t.Fatal(err)
		}
		_, err := stdout.Write(withFields(p.t, report, map[string]any{"plan_path": output, "plan_digest": contentDigestOf(saved)}))
		return 0, err
	case "apply":
		if !slices.Contains(spec.Args, "--dry-run") {
			p.t.Fatalf("a Plan ran an apply that is not a dry run: %v", spec.Args)
		}
		answer := p.dryRun(saved)
		var outcome struct {
			Outcome string `json:"outcome"`
		}
		if err := json.Unmarshal(answer, &outcome); err != nil {
			p.t.Fatal(err)
		}
		if outcome.Outcome == "refused" {
			_, err := stdout.Write(answer)
			return 2, err
		}
		_, err := stdout.Write(answer)
		return 0, err
	}
	p.t.Errorf("the runner ran %s %v, which the capture has no answer for", spec.Path, spec.Args)
	return -1, context.Canceled
}

func argumentAfter(arguments []string, flag string) string {
	for index, argument := range arguments {
		if argument == flag && index+1 < len(arguments) {
			return arguments[index+1]
		}
		if value, found := strings.CutPrefix(argument, flag+"="); found {
			return value
		}
	}
	return ""
}

func contentDigestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// withFields is document with each of fields set, and removed where its value
// is nil.
func withFields(t *testing.T, document []byte, fields map[string]any) []byte {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(document, &decoded); err != nil {
		t.Fatal(err)
	}
	for name, value := range fields {
		if value == nil {
			delete(decoded, name)
			continue
		}
		decoded[name] = value
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// ownerRefused is the dry run the harness row stored: missing-owner-role.
func ownerRefused(t *testing.T) func([]byte) []byte {
	return func(saved []byte) []byte {
		return withFields(t, readCapture(t, "..", "dataplane", "testdata", "differing", "apply-owner-missing-refused.json"),
			map[string]any{"plan_digest": contentDigestOf(saved)})
	}
}

// dryRunRefusedWith is that dry run refused with refusal instead.
func dryRunRefusedWith(t *testing.T, refusal string) func([]byte) []byte {
	return func(saved []byte) []byte {
		var decoded any
		if err := json.Unmarshal([]byte(refusal), &decoded); err != nil {
			t.Fatal(err)
		}
		return withFields(t, ownerRefused(t)(saved), map[string]any{"refusal": decoded})
	}
}

// ownerHeld is the dry run of the same plan once the role exists: it lists the
// plan's statement, as a dry run Ptah would make no refusal of.
func ownerHeld(t *testing.T) func([]byte) []byte {
	return func(saved []byte) []byte {
		return withFields(t, ownerRefused(t)(saved), map[string]any{"outcome": "dry-run", "refusal": nil, "error": nil})
	}
}

// plansRefusedWith is the view row's refused plan with the code code.
func plansRefusedWith(t *testing.T, code string) []byte {
	t.Helper()
	return withFields(t, harnessDocument(t, "differs-view-invoker-unstated-postgres", "plan", "1-schema-plan.json"),
		map[string]any{"refusal": map[string]any{"code": code}})
}

// refusedPlanCycle takes one interval of schema with the real Job builder: the
// Observe and the Plan Jobs the controller builds, each run by the runner
// against ptah and handed back as its Pod's log, and the pass that harvests
// the Plan. It returns what the runner said of the Plan and the stored schema.
func refusedPlanCycle(
	t *testing.T, reconciler *SchemaReconciler, api client.Client, schema *operatorv1alpha1.PtahSchema, ptah runner.Executor,
) (runner.Result, *operatorv1alpha1.PtahSchema) {
	t.Helper()
	var planned runner.Result
	for _, operation := range []operatorv1alpha1.OperationType{operatorv1alpha1.OperationObserve, operatorv1alpha1.OperationPlan} {
		job := reconcileUntilOperationJob(t, reconciler, api, schema, operation)
		result, frame := runCapturedRead(t, reconciler, job, operation, ptah)
		if operation == operatorv1alpha1.OperationObserve && result.Error != nil {
			t.Fatalf("the Observe failed: %#v", result.Error)
		}
		planned = result
		finishBuiltJob(t, reconciler, api, job, frame)
	}
	return planned, reconcileSchema(t, reconciler, api, schema)
}

// planCycleFixture is a schema about to observe with the real Job builder and
// a plan store that publishes, and the recorder its Events go to.
func planCycleFixture(t *testing.T) (*operatorv1alpha1.PtahSchema, *SchemaReconciler, client.Client, *record.FakeRecorder) {
	t.Helper()
	schema, reconciler, api, recorder := undecidedReconciler(t)
	reconciler.Plans = planstore.Store{Client: &planUIDAssigningClient{Client: api}, Reader: api}
	return schema, reconciler, api, recorder
}

func schemaPlanNames(t *testing.T, api client.Client, namespace string) []string {
	t.Helper()
	plans := &operatorv1alpha1.PtahSchemaPlanList{}
	if err := api.List(context.Background(), plans, client.InNamespace(namespace)); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(plans.Items))
	for _, plan := range plans.Items {
		names = append(names, plan.Name)
	}
	slices.Sort(names)
	return names
}

// assertStoppedOnTheRefusal holds a schema whose Plan Ptah refused to the stop:
// Blocked with no claim and no plan, Ready saying said, nothing recorded as a
// failure, the database read, one PlanRefused Warning, and the next read a
// whole interval away.
func assertStoppedOnTheRefusal(
	t *testing.T, reconciler *SchemaReconciler, actual *operatorv1alpha1.PtahSchema, events []string, said string,
) {
	t.Helper()
	if actual.Status.Phase != operatorv1alpha1.PhaseBlocked || actual.Status.ActiveOperation != nil || actual.Status.Plan != nil {
		t.Fatalf("phase %s, claim %#v, plan %#v; want Blocked with neither", actual.Status.Phase, actual.Status.ActiveOperation, actual.Status.Plan)
	}
	assertPlanRefusedSays(t, actual, said)
	for _, condition := range []string{
		operatorv1alpha1.ConditionDriftDetected, operatorv1alpha1.ConditionInSync,
	} {
		if !conditionMatches(actual.Status.Conditions, condition, metav1.ConditionUnknown, operatorv1alpha1.ReasonPlanRefused) {
			t.Fatalf("%s = %#v, want Unknown PlanRefused", condition, findCondition(actual.Status.Conditions, condition))
		}
	}
	if !conditionMatches(actual.Status.Conditions, operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse, operatorv1alpha1.ReasonPlanRefused) {
		t.Fatalf("PlanReady = %#v, want False PlanRefused", findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionPlanReady))
	}
	assertDatabaseReached(t, actual.Status.Conditions, "Database schema was observed")
	if want := reconciler.now().Add(interval(actual)); actual.Status.NextReconciliationTime == nil ||
		!actual.Status.NextReconciliationTime.Time.Equal(want) {
		t.Fatalf("NextReconciliationTime = %v, want %v", actual.Status.NextReconciliationTime, want)
	}
	warning := corev1.EventTypeWarning + " PlanRefused "
	if found := eventsStartingWith(events, warning); len(found) != 1 || found[0] != warning+said {
		t.Fatalf("PlanRefused Warnings = %q, want one saying %q", found, said)
	}
	if found := eventsStartingWith(events, corev1.EventTypeWarning+" OperationFailed"); len(found) != 0 {
		t.Fatalf("the refusal was reported as a failed operation: %q", found)
	}
}

// The reproduction of stokaro/ptah-operator#1166. A Plan that Ptah refuses
// under a code of its own ended in plan_refused, which the operator retried
// at every failure retry: a Job and the database Lease every 30 seconds by
// default, ReconciliationFailed and an OperationFailed Warning each time,
// until a person changed the schema or the server. Another Plan of the same
// schema against the same server gets the same answer, so it now stops at
// Blocked with PlanRefused and the runner's sentence, which names the code
// and what the refusal lists, publishes no plan, marks the recorded approval
// stale, says so once in a Warning, and reads again at the interval. The
// refusal came after Ptah compared the schema with the database, or after the
// dry run read the server's roles or sql_mode, so the read opened the
// database, from a fixture whose earlier read did not.
//
// Each family is the frame the runner harness captured from the pinned image:
// a view refusal from the plan, an owner refusal and a backslash-mode refusal
// from the dry run of the saved plan, and a declaration refusal the schema
// does not explain. A code this operator does not know stops the same way.
//
// The control is plan_refused on an Observe, which the runner never writes,
// since an Observe plans nothing: it is a failed read as before.
func TestAPlanPtahRefusesStopsBlocked(t *testing.T) {
	t.Parallel()
	rows := []readStopRow{}
	for _, captured := range []struct{ name, row, code string }{
		{"a view whose security option the schema does not state", "differs-view-invoker-unstated-postgres/plan", "view-security-option"},
		{"a view another account defined", "differs-view-definer-other-mysql/plan", "view-definer"},
		{"an owner the server does not hold, from the dry run", "differs-view-owner-missing-postgres/plan", "missing-owner-role"},
		{"an owner the login cannot give the view to, from the dry run", "differs-view-owner-transfer-postgres/plan", "owner-transfer"},
		{"a backslash the server's sql_mode reads another way, from the dry run", "differs-backslash-plan-mysql/plan", "backslash-mode"},
		{"a declaration refusal the schema does not explain", "structural-other-and-key/plan", "declaration-refused"},
	} {
		failure := capturedPlanFailure(t, captured.row)
		if failure.Code != runner.CodePlanRefused || !strings.HasPrefix(failure.Message, "ptah refused to plan the change ("+captured.code) {
			t.Fatalf("%s framed %#v, want plan_refused with the code %s", captured.row, failure, captured.code)
		}
		rows = append(rows, readStopRow{
			name: captured.name, claim: operatorv1alpha1.OperationPlan, runner: runner.OperationPlan,
			failure: *failure, reason: operatorv1alpha1.ReasonPlanRefused, unreached: true,
		})
	}
	rows = append(rows,
		readStopRow{
			name: "a code this operator does not know", claim: operatorv1alpha1.OperationPlan, runner: runner.OperationPlan,
			failure: runner.ResultError{Code: runner.CodePlanRefused, Message: "ptah refused to plan the change (some-later-code)"},
			reason:  operatorv1alpha1.ReasonPlanRefused, unreached: true,
		},
		readStopRow{
			name: "control: plan_refused on an Observe", claim: operatorv1alpha1.OperationObserve, runner: runner.OperationObserve,
			failure: runner.ResultError{Code: runner.CodePlanRefused, Message: "ptah refused to plan the change (view-security-option)"},
		},
	)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			holdReadStop(t, row)
		})
	}
}

// The same stop through the runner the controller's Jobs name, against the
// documents the pinned Ptah printed for differs-view-owner-missing-postgres:
// the Observe finds the view changed, and the Plan saves the change and stops
// on its dry run's refusal. The runner's sentence is the one the harness
// captured for the row. A refusal of the plan itself, and one under a code
// this operator does not know, from the plan or from its dry run, stop the
// same way.
//
// A dry run or a plan document that found the database moved under the plan,
// or that another session's lock held back, refuses nothing about the plan:
// the runner reports each as invalid_plan_output, and the Plan is tried again
// at the failure retry. Ptah v0.34.0's schema plan raises neither code, so the
// plan document rows hold the day it does. The control is the same Plan whose
// dry run lists the statement, which publishes the plan: without it, a fixture
// that could never publish would pass the rows that say nothing was published.
func TestARefusedPlanThroughTheRunnerStopsBlocked(t *testing.T) {
	t.Parallel()
	owner := capturedPlanFailure(t, "differs-view-owner-missing-postgres/plan")
	view := capturedPlanFailure(t, "differs-view-invoker-unstated-postgres/plan")
	for _, row := range []struct {
		name    string
		ptah    func(*testing.T) planRefusingPtah
		failure runner.ResultError
		stops   bool
	}{
		{
			name:    "an owner the server does not hold",
			ptah:    func(t *testing.T) planRefusingPtah { return planRefusingPtah{t: t, dryRun: ownerRefused(t)} },
			failure: *owner, stops: true,
		},
		{
			name: "a view whose security option the schema does not state",
			ptah: func(t *testing.T) planRefusingPtah {
				return planRefusingPtah{t: t, plan: refusedViewPlanPtah(t).report, dryRun: ownerRefused(t)}
			},
			failure: *view, stops: true,
		},
		{
			name: "a code this operator does not know",
			ptah: func(t *testing.T) planRefusingPtah {
				return planRefusingPtah{t: t, plan: plansRefusedWith(t, "some-later-code"), dryRun: ownerRefused(t)}
			},
			failure: runner.ResultError{Code: runner.CodePlanRefused, Message: "ptah refused to plan the change (some-later-code)"},
			stops:   true,
		},
		{
			name: "a dry run code this operator does not know",
			ptah: func(t *testing.T) planRefusingPtah {
				return planRefusingPtah{t: t, dryRun: dryRunRefusedWith(t, `{"code":"some-later-code"}`)}
			},
			failure: runner.ResultError{Code: runner.CodePlanRefused, Message: "ptah refused to plan the change (some-later-code)"},
			stops:   true,
		},
		{
			name: "control: a dry run that found the database moved",
			ptah: func(t *testing.T) planRefusingPtah {
				return planRefusingPtah{t: t, dryRun: dryRunRefusedWith(t, `{"code":"stale-plan","changed":"schema"}`)}
			},
			failure: runner.ResultError{Code: "invalid_plan_output", Message: "plan validation reported refused (stale-plan) rather than a dry run"},
		},
		{
			name: "control: a dry run another session's lock held back",
			ptah: func(t *testing.T) planRefusingPtah {
				return planRefusingPtah{t: t, dryRun: dryRunRefusedWith(t, `{"code":"lock-timeout"}`)}
			},
			failure: runner.ResultError{Code: "invalid_plan_output", Message: "plan validation reported refused (lock-timeout) rather than a dry run"},
		},
		{
			name: "control: a plan document that found the database moved",
			ptah: func(t *testing.T) planRefusingPtah {
				return planRefusingPtah{t: t, plan: plansRefusedWith(t, "stale-plan"), dryRun: ownerRefused(t)}
			},
			failure: runner.ResultError{Code: "invalid_plan_output", Message: "plan command reported refused (stale-plan) rather than a plan"},
		},
		{
			name: "control: a plan document another session's lock held back",
			ptah: func(t *testing.T) planRefusingPtah {
				return planRefusingPtah{t: t, plan: plansRefusedWith(t, "lock-timeout"), dryRun: ownerRefused(t)}
			},
			failure: runner.ResultError{Code: "invalid_plan_output", Message: "plan command reported refused (lock-timeout) rather than a plan"},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			schema, reconciler, api, recorder := planCycleFixture(t)

			result, actual := refusedPlanCycle(t, reconciler, api, schema, row.ptah(t))

			if result.Error == nil || result.Error.Code != row.failure.Code || result.Error.Message != row.failure.Message {
				t.Fatalf("the Plan returned %#v, want %#v", result.Error, row.failure)
			}
			said := row.failure.Code + ": " + row.failure.Message
			events := drainEvents(recorder)
			if names := schemaPlanNames(t, api, schema.Namespace); len(names) != 0 {
				t.Fatalf("a refused Plan published %v", names)
			}
			if row.stops {
				assertStoppedOnTheRefusal(t, reconciler, actual, events, said)
				jobs := jobNames(t, api, schema.Namespace)
				if again := reconcileSchema(t, reconciler, api, schema); again.Status.ActiveOperation != nil {
					t.Fatalf("a pass with nothing changed claimed %#v", again.Status.ActiveOperation)
				}
				if after := jobNames(t, api, schema.Namespace); !slices.Equal(after, jobs) {
					t.Fatalf("a pass with nothing changed built a Job: %v, before %v", after, jobs)
				}
				return
			}
			failed := findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
			if actual.Status.Phase != operatorv1alpha1.PhaseFailed || actual.Status.ActiveOperation == nil ||
				actual.Status.ActiveOperation.Type != operatorv1alpha1.OperationPlan || actual.Status.ActiveOperation.Attempt != 2 ||
				failed == nil || failed.Status != metav1.ConditionTrue || failed.Message != said {
				t.Fatalf("the Plan was not tried again: phase %s, claim %#v, ReconciliationFailed %#v",
					actual.Status.Phase, actual.Status.ActiveOperation, failed)
			}
			if found := eventsStartingWith(events, corev1.EventTypeWarning+" PlanRefused"); len(found) != 0 {
				t.Fatalf("a dry run that refused nothing about the plan was reported as PlanRefused: %q", found)
			}
		})
	}

	t.Run("control: a dry run that lists the plan", func(t *testing.T) {
		t.Parallel()
		schema, reconciler, api, _ := planCycleFixture(t)

		result, actual := refusedPlanCycle(t, reconciler, api, schema, planRefusingPtah{t: t, dryRun: ownerHeld(t)})

		if result.Error != nil || result.PlanOutcome != runner.PlanOutcomeChanges {
			t.Fatalf("the control's Plan returned %#v with outcome %q", result.Error, result.PlanOutcome)
		}
		if actual.Status.Plan == nil || len(schemaPlanNames(t, api, schema.Namespace)) != 1 {
			t.Fatalf("the control published no plan: status %#v, plans %v", actual.Status.Plan, schemaPlanNames(t, api, schema.Namespace))
		}
		if actual.Status.Phase == operatorv1alpha1.PhaseBlocked ||
			conditionMatches(actual.Status.Conditions, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonPlanRefused) {
			t.Fatalf("the control stopped: phase %s, conditions %#v", actual.Status.Phase, actual.Status.Conditions)
		}
	})
}

// A schema stopped on a refused plan reads again only when something could
// have changed the answer: at the interval, on a read request, when its spec
// changes, or when its ConfigMap source changes. Between those, a pass claims
// nothing and builds no Job. A refusal can be the server's to lift, as a role
// an administrator creates, which nothing the operator watches says, so a read
// request is how a person asks for the Plan again: here the role exists by
// then, and the Plan publishes and leaves Blocked. An edited ConfigMap is read
// at once, and a schema Ptah no longer refuses to plan leaves Blocked too.
func TestASchemaStoppedOnARefusedPlanReadsAgain(t *testing.T) {
	t.Parallel()
	refused := func(t *testing.T) (*operatorv1alpha1.PtahSchema, *SchemaReconciler, client.Client) {
		t.Helper()
		schema, reconciler, api, _ := planCycleFixture(t)
		_, blocked := refusedPlanCycle(t, reconciler, api, schema, planRefusingPtah{t: t, dryRun: ownerRefused(t)})
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

	t.Run("when its spec changes", func(t *testing.T) {
		t.Parallel()
		blocked, reconciler, api := refused(t)
		stored := storedSchemaOf(t, api, blocked)
		stored.Spec.Interval = metav1.Duration{Duration: 2 * interval(stored)}
		stored.Generation++
		if err := api.Update(context.Background(), stored); err != nil {
			t.Fatal(err)
		}
		cycle := reconcileSchema(t, reconciler, api, blocked)
		if cycle.Status.ActiveOperation == nil || cycle.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve {
			t.Fatalf("after the spec changed the schema claimed %#v in phase %s, want a Resolve",
				cycle.Status.ActiveOperation, cycle.Status.Phase)
		}
	})

	t.Run("on a read request, once the server holds the role", func(t *testing.T) {
		t.Parallel()
		blocked, reconciler, api := refused(t)
		request := reconciler.now().Format(time.RFC3339)
		annotateSchema(t, api, blocked, request)
		answered := reconcileSchema(t, reconciler, api, blocked)
		if answered.Status.LastReadRequest != operatorv1alpha1.ReadRequestToken(request) || answered.Status.NextReconciliationTime == nil ||
			answered.Status.NextReconciliationTime.After(reconciler.now()) {
			t.Fatalf("the request was not answered: lastReadRequest %q, next %v", answered.Status.LastReadRequest, answered.Status.NextReconciliationTime)
		}

		result, planned := refusedPlanCycle(t, reconciler, api, blocked, planRefusingPtah{t: t, dryRun: ownerHeld(t)})

		if result.Error != nil {
			t.Fatalf("the Plan after the request returned %#v", result.Error)
		}
		if planned.Status.Plan == nil || planned.Status.Phase == operatorv1alpha1.PhaseBlocked ||
			conditionMatches(planned.Status.Conditions, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonPlanRefused) {
			t.Fatalf("the schema did not leave the refusal: phase %s, plan %#v, Ready %#v", planned.Status.Phase, planned.Status.Plan,
				findCondition(planned.Status.Conditions, operatorv1alpha1.ConditionReady))
		}
		if !meta.IsStatusConditionTrue(planned.Status.Conditions, operatorv1alpha1.ConditionPlanReady) {
			t.Fatalf("PlanReady = %#v, want True", findCondition(planned.Status.Conditions, operatorv1alpha1.ConditionPlanReady))
		}
	})

	t.Run("when its ConfigMap source changes", func(t *testing.T) {
		t.Parallel()
		schema := configMapSchemaFixture()
		schema.Spec.Interval = metav1.Duration{Duration: time.Hour}
		reconciler, api := fakeReconciler(t, staticLogs{}, schema, sourceConfigMap("CREATE VIEW differs_notes_view AS SELECT 1;\n"))
		builder, key := leaderBuilder(t)
		reconciler.Client = uidByNameClient{Client: api}
		reconciler.Jobs = builder
		reconciler.SealKey = key
		reconciler.Locks = targetlock.New(api, api, nil)
		reconciler.Plans = planstore.Store{Client: &planUIDAssigningClient{Client: uidByNameClient{Client: api}}, Reader: api}
		if resolved := reconcileSchema(t, reconciler, api, schema); resolved.Status.Phase != operatorv1alpha1.PhaseObserving {
			t.Fatalf("the fixture did not read its source: phase %s", resolved.Status.Phase)
		}

		_, blocked := refusedPlanCycle(t, reconciler, api, schema, planRefusingPtah{t: t, dryRun: ownerRefused(t)})

		if blocked.Status.Phase != operatorv1alpha1.PhaseBlocked ||
			!conditionMatches(blocked.Status.Conditions, operatorv1alpha1.ConditionReady, metav1.ConditionFalse, operatorv1alpha1.ReasonPlanRefused) {
			t.Fatalf("the ConfigMap schema did not stop: phase %s, conditions %#v", blocked.Status.Phase, blocked.Status.Conditions)
		}
		first := blocked.Status.Source.Snapshot.DeepCopy()
		if again := reconcileSchema(t, reconciler, api, schema); again.Status.ActiveOperation != nil {
			t.Fatalf("a pass with nothing changed claimed %#v", again.Status.ActiveOperation)
		}

		editSource(t, api, func(source *corev1.ConfigMap) {
			source.Data["schema.sql"] = "CREATE VIEW differs_notes_view AS SELECT 2;\n"
		})

		reread := reconcileSchema(t, reconciler, api, schema)
		if reread.Status.Phase != operatorv1alpha1.PhaseObserving || reread.Status.Source.Snapshot == nil ||
			reread.Status.Source.Snapshot.ContentDigest == first.ContentDigest {
			t.Fatalf("the edited source was not read at once: phase %s, snapshot %#v, before %#v",
				reread.Status.Phase, reread.Status.Source.Snapshot, first)
		}
		result, planned := refusedPlanCycle(t, reconciler, api, schema, planRefusingPtah{t: t, dryRun: ownerHeld(t)})
		if result.Error != nil || planned.Status.Plan == nil || planned.Status.Phase == operatorv1alpha1.PhaseBlocked {
			t.Fatalf("the edited schema did not leave the refusal: error %#v, phase %s, plan %#v",
				result.Error, planned.Status.Phase, planned.Status.Plan)
		}
	})
}
