// Copyright 2026 The Ptah Operator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/historyview"
	"github.com/stokaro/ptah-operator/internal/operate"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetlock"
)

// plannedNothingPtah answers the runner's `schema plan` as the pinned Ptah
// answered it: report is the document it printed, with the exit status it
// gave. Where saved is set, Ptah saved that plan to the --output path, and the
// report names that path as the plan's. Any other command fails the test.
type plannedNothingPtah struct {
	t             *testing.T
	report, saved []byte
	exitCode      int
}

func (p plannedNothingPtah) Execute(_ context.Context, spec runner.CommandSpec, stdout, _ io.Writer) (int, error) {
	if len(spec.Args) < 2 || spec.Args[0] != "schema" || spec.Args[1] != "plan" {
		p.t.Errorf("the runner ran %s %v, which the capture has no answer for", spec.Path, spec.Args)
		return -1, context.Canceled
	}
	report := p.report
	if p.saved != nil {
		output := ""
		for index, argument := range spec.Args {
			if argument == "--output" && index+1 < len(spec.Args) {
				output = spec.Args[index+1]
			}
		}
		if output == "" {
			p.t.Fatalf("the plan read names no --output: %v", spec.Args)
		}
		if err := os.WriteFile(output, p.saved, 0o600); err != nil {
			p.t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(report, &fields); err != nil {
			p.t.Fatal(err)
		}
		path, err := json.Marshal(output)
		if err != nil {
			p.t.Fatal(err)
		}
		fields["plan_path"] = path
		if report, err = json.Marshal(fields); err != nil {
			p.t.Fatal(err)
		}
	}
	_, err := stdout.Write(report)
	return p.exitCode, err
}

func readCapture(t *testing.T, path ...string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(path...))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// notePlanPtah is the plan of notes the pinned Ptah saved against PostgreSQL 16
// for a generated column whose expression changed, which
// internal/runner/testdata/note-plan holds and says where it is from.
func notePlanPtah(t *testing.T) plannedNothingPtah {
	t.Helper()
	return plannedNothingPtah{
		t:      t,
		report: readCapture(t, "..", "runner", "testdata", "note-plan", "schema-plan-postgres-16.json"),
		saved:  readCapture(t, "..", "runner", "testdata", "note-plan", "saved-plan-postgres-16.json"),
	}
}

// fencedPlanPtah is the fence refusal the runner harness row plan-fenced
// stored for PostgreSQL, which Ptah printed with exit status 2.
func fencedPlanPtah(t *testing.T) plannedNothingPtah {
	t.Helper()
	return plannedNothingPtah{
		t: t, exitCode: 2,
		report: readCapture(t, "..", "dataplane", "testdata", "runner-harness", "documents", "rows-postgres", "plan-fenced", "1-schema-plan.json"),
	}
}

// fenceRegions is the fence plan-fenced ran under, on the schema and on the
// plan the Apply ran, which agree.
func fenceRegions(t *testing.T) func(*operatorv1alpha1.PtahSchema) {
	return func(schema *operatorv1alpha1.PtahSchema) {
		schema.Spec.Policy.ProtectedTables = []string{"regions"}
		schema.Status.PendingObservation.ProtectedTables = []string{"regions"}
		policy, err := policyFingerprint(schema)
		if err != nil {
			t.Fatal(err)
		}
		schema.Status.PendingObservation.Plan.PolicyFingerprint = policy
	}
}

// proofReadOf is a schema whose proof is about to dispatch its scoped Plan
// against undecidedDatabaseURL's target, with the real Job builder, and the
// recorder its Events go to.
func proofReadOf(
	t *testing.T, edits ...func(*operatorv1alpha1.PtahSchema),
) (*operatorv1alpha1.PtahSchema, *SchemaReconciler, client.Client, *record.FakeRecorder) {
	t.Helper()
	schema, reconciler, api := builtProofFixture(t, operatorv1alpha1.OperationPlan, append([]func(*operatorv1alpha1.PtahSchema){
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

// harvestProofRead runs the proof's Plan Job the controller built against ptah,
// holds the runner to the code it reports, and takes the pass that harvests it.
func harvestProofRead(
	t *testing.T, reconciler *SchemaReconciler, api client.Client, schema *operatorv1alpha1.PtahSchema,
	ptah runner.Executor, code string,
) *operatorv1alpha1.PtahSchema {
	t.Helper()
	job := reconcileUntilJobNamed(t, reconciler, api, schema, schema.Status.ActiveOperation.JobName)
	result, frame := runCapturedRead(t, reconciler, job, operatorv1alpha1.OperationPlan, ptah)
	if result.Error == nil || result.Error.Code != code || result.PlanOutcome != "" {
		t.Fatalf("the proof read returned error %#v with outcome %q, want %s and no outcome", result.Error, result.PlanOutcome, code)
	}
	finishBuiltJob(t, reconciler, api, job, frame)
	return reconcileSchema(t, reconciler, api, schema)
}

func jobNames(t *testing.T, api client.Client, namespace string) []string {
	t.Helper()
	jobs := &batchv1.JobList{}
	if err := api.List(context.Background(), jobs, client.InNamespace(namespace)); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(jobs.Items))
	for _, job := range jobs.Items {
		names = append(names, job.Name)
	}
	slices.Sort(names)
	return names
}

func verifiedEntries(schema *operatorv1alpha1.PtahSchema) []operatorv1alpha1.JournalEntry {
	var verified []operatorv1alpha1.JournalEntry
	if schema.Status.Journal != nil {
		for _, entry := range schema.Status.Journal.Entries {
			if entry.Type == operatorv1alpha1.JournalVerified {
				verified = append(verified, entry)
			}
		}
	}
	return verified
}

func warningsWithReason(recorder *record.FakeRecorder, reason string) []string {
	var warnings []string
	for {
		select {
		case event := <-recorder.Events:
			if strings.HasPrefix(event, corev1.EventTypeWarning+" "+reason+" ") {
				warnings = append(warnings, event)
			}
		default:
			return warnings
		}
	}
}

// The read after an Apply is the proof that it converged. One whose plan Ptah
// could not make -- a plan of notes, or a plan refused for a fenced table --
// found the database still differing from the artifact. Before, that ended the
// Plan as it ends one outside a proof and kept the proof, its finalizer and its
// Lease, so the next pass claimed the proof's read again at once and the
// database was read for the same difference, Job after Job, with nothing in the
// journal. The proof now ends there: no Applied record, the Lease released, the
// finalizer gone, the journal's Verified entry DriftRemains, the next read a
// whole interval away and an ordinary one.
//
// Both reads are what the pinned Ptah printed, run by the runner the
// controller's Jobs name. The control is the same proof whose plan changes
// nothing, which credits the Apply: without it, a fixture whose proof could
// never write Applied would pass the row.
func TestAProofWhosePlanPtahCouldNotMakeEndsWithoutApplied(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name   string
		ptah   func(*testing.T) plannedNothingPtah
		edits  []func(*operatorv1alpha1.PtahSchema)
		code   string
		reason operatorv1alpha1.ConditionReason
		event  string
		says   string
	}{
		{
			name: "a plan of notes", ptah: notePlanPtah, code: runner.CodeDifferenceNotPlannable,
			reason: operatorv1alpha1.ReasonDifferenceNotPlannable, event: "DifferenceNotPlannable", says: "public.tasks.code",
		},
		{
			name: "a fenced table", ptah: fencedPlanPtah, code: "protected_table",
			reason: operatorv1alpha1.ReasonProtectedTable, event: "ProtectedTableRefused", says: "public.regions",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			var edits []func(*operatorv1alpha1.PtahSchema)
			if row.reason == operatorv1alpha1.ReasonProtectedTable {
				edits = append(edits, fenceRegions(t))
			}
			schema, reconciler, api, recorder := proofReadOf(t, edits...)
			pending := schema.Status.PendingObservation
			if holder := proofLeaseHolder(t, reconciler, api); holder == "" {
				t.Fatal("the fixture's proof holds no Lease, so a released one shows nothing")
			}

			actual := harvestProofRead(t, reconciler, api, schema, row.ptah(t), row.code)

			if actual.Status.PendingObservation != nil || actual.Status.ActiveOperation != nil {
				t.Fatalf("the proof did not end: pending %#v, operation %#v", actual.Status.PendingObservation, actual.Status.ActiveOperation)
			}
			if actual.Status.Applied != nil {
				t.Fatalf("a proof that found a difference recorded the Apply as converged: %#v", actual.Status.Applied)
			}
			if slices.Contains(actual.Finalizers, activeOperationFinalizer) {
				t.Fatalf("the ended proof kept the finalizer: %v", actual.Finalizers)
			}
			if holder := proofLeaseHolder(t, reconciler, api); holder != "" {
				t.Fatalf("the ended proof still holds the Lease as %q", holder)
			}
			if actual.Status.PendingLockRelease != nil {
				t.Fatalf("the Lease release is still owed: %#v", actual.Status.PendingLockRelease)
			}
			if actual.Status.Phase != operatorv1alpha1.PhaseBlocked || actual.Status.ObservedGeneration != pending.ApplyGeneration {
				t.Fatalf("phase %s at observed generation %d, want Blocked at the Apply's generation %d",
					actual.Status.Phase, actual.Status.ObservedGeneration, pending.ApplyGeneration)
			}
			for _, expected := range []struct {
				condition string
				status    metav1.ConditionStatus
			}{
				{operatorv1alpha1.ConditionDriftDetected, metav1.ConditionTrue},
				{operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse},
				{operatorv1alpha1.ConditionInSync, metav1.ConditionFalse},
				{operatorv1alpha1.ConditionReady, metav1.ConditionFalse},
			} {
				if !conditionMatches(actual.Status.Conditions, expected.condition, expected.status, row.reason) {
					t.Fatalf("%s is not %s with %s: %#v", expected.condition, expected.status, row.reason, actual.Status.Conditions)
				}
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
			if len(verified) != 1 || verified[0].Outcome != operatorv1alpha1.JournalDriftRemains ||
				verified[0].Plan != pending.Plan.Name || verified[0].OperationID != pending.ApplyOperationID {
				t.Fatalf("the journal records %#v, want one Verified DriftRemains for plan %s and Apply %s",
					verified, pending.Plan.Name, pending.ApplyOperationID)
			}
			if warnings := warningsWithReason(recorder, row.event); len(warnings) != 1 || !strings.Contains(warnings[0], row.says) {
				t.Fatalf("Warnings = %q, want one %s naming %s", warnings, row.event, row.says)
			}

			// The Job's cleanup write wakes the next pass at once. It claims
			// nothing and builds no Job: before, it claimed the proof's read again.
			jobs := jobNames(t, api, schema.Namespace)
			again := reconcileSchema(t, reconciler, api, schema)
			if again.Status.ActiveOperation != nil || again.Status.PendingObservation != nil {
				t.Fatalf("the next pass claimed %#v with pending %#v", again.Status.ActiveOperation, again.Status.PendingObservation)
			}
			if after := jobNames(t, api, schema.Namespace); !slices.Equal(after, jobs) {
				t.Fatalf("the next pass built a Job: %v, before %v", after, jobs)
			}
			// At the interval the resource starts an ordinary cycle, with no proof.
			reached := next.Add(time.Second)
			reconciler.Clock = func() time.Time { return reached }
			cycle := reconcileSchema(t, reconciler, api, schema)
			if cycle.Status.PendingObservation != nil || cycle.Status.ActiveOperation == nil ||
				cycle.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve {
				t.Fatalf("at the interval the resource claimed %#v with pending %#v, want an ordinary Resolve",
					cycle.Status.ActiveOperation, cycle.Status.PendingObservation)
			}
		})
	}

	t.Run("control: a plan that changes nothing credits the Apply", func(t *testing.T) {
		t.Parallel()
		schema, reconciler, api, _ := proofReadOf(t)
		job := reconcileUntilJobNamed(t, reconciler, api, schema, schema.Status.ActiveOperation.JobName)
		result, frame := runCapturedRead(t, reconciler, job, operatorv1alpha1.OperationPlan, capturedPtah{
			t: t, plan: withoutUndecided(t, undecidedCapture(t, "plan-undecided-publication-postgres.json")),
		})
		if result.Error != nil || result.PlanOutcome != runner.PlanOutcomeNoChanges {
			t.Fatalf("the control's read returned error %#v with outcome %q, want %s", result.Error, result.PlanOutcome, runner.PlanOutcomeNoChanges)
		}
		finishBuiltJob(t, reconciler, api, job, frame)
		converged := reconcileSchema(t, reconciler, api, schema)
		if converged.Status.Applied == nil || converged.Status.Phase != operatorv1alpha1.PhaseInSync {
			t.Fatalf("the control did not credit the Apply, so the row measured nothing: phase %s, applied %#v",
				converged.Status.Phase, converged.Status.Applied)
		}
	})
}

// A proof that settles under a newer generation hands the resource to that
// generation, whatever its read found: the plan of notes ends the proof as
// Superseded, credits nothing, and leaves the next read to the newer inputs.
func TestAProofWhosePlanPtahCouldNotMakeUnderANewerGenerationIsSuperseded(t *testing.T) {
	t.Parallel()
	schema, reconciler, api, _ := proofReadOf(t, func(schema *operatorv1alpha1.PtahSchema) {
		schema.Generation = schema.Status.PendingObservation.ApplyGeneration + 1
	})
	pending := schema.Status.PendingObservation
	if schema.Generation == pending.ApplyGeneration {
		t.Fatal("the fixture's generation is the Apply's, so nothing newer is measured")
	}

	actual := harvestProofRead(t, reconciler, api, schema, notePlanPtah(t), runner.CodeDifferenceNotPlannable)

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
}

// An Apply whose proof found a difference no plan here may make did not
// converge, and each view that reads its end says so from the document the
// reconciler wrote. kubectl ptah approve --wait and wait --for applied, begun
// while the Apply ran, stop at the proof's end saying the database still
// differs; history puts the proof beside the Apply as Verified DriftRemains.
// Before, the proof never ended: both waits ran to their timeout, and history
// said the proof was still owed.
func TestTheViewsOfAnApplyWhoseProofPtahCouldNotPlanSayItStillDiffers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	schema, plan, policyConfig := dispatchedApplyWithPlan(t)
	_, _, approval, _ := safetyApprovalFixture(t)
	schema.Status.Plan.Approval = &operatorv1alpha1.ConsumedApprovalStatus{
		Name: approval.Name, UID: approval.UID, Approver: approval.Spec.Approver, ApprovedAt: approval.Spec.ApprovedAt,
	}
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	applied := runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationApply, OperationID: schema.Status.ActiveOperation.ID,
		MutationStarted: true, CoordinationDigest: schema.Status.Plan.CoordinationDigest,
		TargetIdentityDigest: schema.Status.Plan.TargetIdentityDigest,
	}
	reconciler, api := fakeReconciler(t, staticLogs{content: safetyRunnerFrame(t, applied)},
		schema, plan, policyConfig, approval, job, pod)
	reconciler.Locks = targetlock.New(api, api, nil)
	// What approve --wait and wait --for applied read first: the Apply running.
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
	observed := nextProofRead(t, reconciler, api, observing, "observe", target, nil)
	planning := reconcileSchema(t, reconciler, api, observed)
	if operation := planning.Status.ActiveOperation; operation == nil || operation.Type != operatorv1alpha1.OperationPlan {
		t.Fatalf("the pass claimed %#v, want the proof's scoped Plan", operation)
	}
	// What the runner frames for the plan of notes TestAProofWhosePlanPtahCouldNotMakeEndsWithoutApplied reads.
	ended := nextProofRead(t, reconciler, api, planning, "plan", target,
		&runner.ResultError{Code: runner.CodeDifferenceNotPlannable, Message: unplannableNote})
	if ended.Status.PendingObservation != nil {
		t.Fatalf("the proof did not end: phase %s, pending %#v", ended.Status.Phase, ended.Status.PendingObservation)
	}
	ref := operate.Ref{Family: operate.FamilySchema, Namespace: schema.Namespace, Name: schema.Name}

	for _, row := range []struct {
		name    string
		options operate.WaitOptions
	}{
		{"approve --wait", operate.WaitOptions{Target: operate.TargetApplied, Since: waiting, Plan: plan.Name}},
		{"wait --for applied", operate.WaitOptions{Target: operate.TargetApplied, Since: waiting}},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := waitOn(api, ref, row.options)

			var stop *operate.StopError
			if !errors.As(err, &stop) || stop.Reason != operate.StopNotConverged ||
				!strings.Contains(stop.Message, "after the Apply of plan "+plan.Name+" the database still differs") {
				t.Fatalf("wait = %v, want a %s stop saying the database still differs after plan %s",
					err, operate.StopNotConverged, plan.Name)
			}
		})
	}

	t.Run("history", func(t *testing.T) {
		view, err := historyview.Load(ctx, api, historyview.Schema, schema.Namespace, schema.Name)
		if err != nil {
			t.Fatal(err)
		}
		var applies []historyview.Entry
		for _, entry := range view.Journal.Entries {
			if entry.Type == operatorv1alpha1.JournalApplyFinished {
				applies = append(applies, entry)
			}
		}
		if len(applies) != 1 || applies[0].Proof == nil || applies[0].Proof.State != historyview.ProofVerified ||
			applies[0].Proof.Outcome != operatorv1alpha1.JournalDriftRemains {
			t.Fatalf("history joins %#v, want one Apply whose proof is Verified DriftRemains", applies)
		}
		var text bytes.Buffer
		if err := historyview.Render(&text, view, historyview.Text); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(text.String(), ", DriftRemains") || strings.Contains(text.String(), "still owed") {
			t.Fatalf("history prints:\n%s\nwant the Apply's proof as Verified DriftRemains", text.String())
		}
	})
}

// kubectl ptah settle --wait, after a proof stopped on a read of another target:
// the URL Secret holds the plan's target again, and the read of it finds a
// difference Ptah cannot plan. That read ends the proof without the statement,
// so the wait stops and says what the journal recorded. Before, the proof went
// on reading the plan's target, and the wait ran to its timeout.
func TestSettleWaitStopsWhereAProofPtahCouldNotPlanEnded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
	target := blocked.Status.PendingObservation.Plan.TargetIdentityDigest
	ref := operate.Ref{Family: operate.FamilySchema, Namespace: blocked.Namespace, Name: blocked.Name}
	settled, err := operate.Settle(ctx, api, ref, operate.SettleOptions{})
	if err != nil || !settled.Changed {
		t.Fatalf("Settle = %#v, %v; want the printed value written", settled, err)
	}

	observed := nextProofRead(t, reconciler, api, blocked, "second", target, nil)
	planning := reconcileSchema(t, reconciler, api, observed)
	if operation := planning.Status.ActiveOperation; operation == nil || operation.Type != operatorv1alpha1.OperationPlan {
		t.Fatalf("the pass claimed %#v, want the proof's scoped Plan of the plan's target", operation)
	}
	ended := nextProofRead(t, reconciler, api, planning, "plan", target,
		&runner.ResultError{Code: runner.CodeDifferenceNotPlannable, Message: unplannableNote})
	if ended.Status.PendingObservation != nil {
		t.Fatalf("the proof did not end: phase %s, pending %#v", ended.Status.Phase, ended.Status.PendingObservation)
	}

	_, err = waitOn(api, ref, operate.WaitOptions{
		Target: operate.TargetSettled, Since: settled.Object, Settlement: settled.Printed.Value,
	})

	var stop *operate.StopError
	if !errors.As(err, &stop) || stop.Reason != operate.StopProofEnded ||
		!strings.Contains(stop.Message, "journal records Verified DriftRemains") {
		t.Fatalf("settle --wait = %v, want a %s stop saying the journal records Verified DriftRemains", err, operate.StopProofEnded)
	}
}
