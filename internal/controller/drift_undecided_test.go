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
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// undecidedDatabaseURL is the URL the reads below log in with. It names no
// real database: the executor answers for Ptah.
const undecidedDatabaseURL = "postgres://app:secret@db.internal/app?search_path=public"

// capturedPtah answers the runner as the pinned Ptah answered the runner
// harness row differs-publication-scoped: `schema drift` prints drift and
// `schema plan` prints plan, with the exit status Ptah gave. Any other command
// fails the test.
type capturedPtah struct {
	t           *testing.T
	drift, plan []byte
}

func (p capturedPtah) Execute(_ context.Context, spec runner.CommandSpec, stdout, _ io.Writer) (int, error) {
	switch {
	case len(spec.Args) >= 2 && spec.Args[0] == "schema" && spec.Args[1] == "drift":
		var report struct {
			Failed bool `json:"failed"`
		}
		if err := json.Unmarshal(p.drift, &report); err != nil {
			p.t.Fatal(err)
		}
		_, err := stdout.Write(p.drift)
		if report.Failed {
			return 1, err
		}
		return 0, err
	case len(spec.Args) >= 2 && spec.Args[0] == "schema" && spec.Args[1] == "plan":
		_, err := stdout.Write(p.plan)
		return 0, err
	}
	p.t.Errorf("the runner ran %s %v, which the capture has no answer for", spec.Path, spec.Args)
	return -1, context.Canceled
}

// undecidedCapture reads a document the runner harness row
// differs-publication-scoped stored.
func undecidedCapture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "dataplane", "testdata", "differing", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// withoutUndecided is document with nothing undecided in it: the same
// database, read where Ptah could decide every declared object.
func withoutUndecided(t *testing.T, document []byte) []byte {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(document, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "undecided")
	if _, drift := fields["drift"]; drift {
		fields["failed"] = json.RawMessage("false")
		delete(fields, "findings")
		delete(fields, "highest_severity")
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// runCapturedRead runs the runner on what job hands its operation container,
// with the URL file the kubelet would project holding undecidedDatabaseURL,
// against ptah, and frames what it returns.
func runCapturedRead(
	t *testing.T, reconciler *SchemaReconciler, job *batchv1.Job, operation operatorv1alpha1.OperationType, ptah runner.Executor,
) (runner.Result, []byte) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "url")
	if err := os.WriteFile(file, []byte(undecidedDatabaseURL), 0o400); err != nil {
		t.Fatal(err)
	}
	environment := []string{runner.EnvDatabaseURLFile + "=" + file}
	for name, variable := range executorEnv(t, job) {
		if variable.ValueFrom == nil && name != runner.EnvDatabaseURLFile {
			environment = append(environment, name+"="+variable.Value)
		}
	}
	result := runner.Run(context.Background(), runner.Config{
		Operation:   mutationlifecycle.SchemaOperation(operation).Runner,
		Environment: environment,
		Executor:    ptah,
		TempDir:     t.TempDir(),
		Clock:       reconciler.now,
	})
	return result, safetyRunnerFrame(t, result)
}

// sourceConfirmed is what a Resolve or Verify of the resource's unchanged
// artifact returns, which TestResolveResultAdvancesToVerification and
// TestVerificationResultCannotOutrunPolicyConfigMapUpdate hold the controller
// to. A new interval starts with both.
func sourceConfirmed(claim *operatorv1alpha1.ActiveOperationStatus) runner.Result {
	result := runner.Result{
		ProtocolVersion: runner.ProtocolVersion, OperationID: claim.ID, ResolvedDigest: testDigest,
	}
	if claim.Type == operatorv1alpha1.OperationResolve {
		result.Operation = runner.OperationResolve
		result.ResolvedReference = "oci://registry.example/team/schema@" + testDigest
		result.ResolvedMediaType = "application/vnd.oci.image.manifest.v1+json"
		result.ResolvedSize = 321
		return result
	}
	result.Operation = runner.OperationVerify
	result.ObservedArtifactType = dataplane.SchemaArtifactType
	result.VerificationPolicyDigest = fingerprint.DigestBytes([]byte("policy"))
	return result
}

// reconcileUntilOperationJob takes passes until the resource has claimed
// operation and the claim's Job exists. A Resolve or Verify on the way, as a
// new interval starts with, confirms the artifact the resource holds.
func reconcileUntilOperationJob(
	t *testing.T, reconciler *SchemaReconciler, api client.Client, schema *operatorv1alpha1.PtahSchema,
	operation operatorv1alpha1.OperationType,
) *batchv1.Job {
	t.Helper()
	for range 16 {
		stored := reconcileSchema(t, reconciler, api, schema)
		claim := stored.Status.ActiveOperation
		if claim == nil || claim.JobName == "" {
			continue
		}
		job := &batchv1.Job{}
		err := api.Get(context.Background(), client.ObjectKey{Namespace: schema.Namespace, Name: claim.JobName}, job)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		switch claim.Type {
		case operation:
			return job
		case operatorv1alpha1.OperationResolve, operatorv1alpha1.OperationVerify:
			finishBuiltJob(t, reconciler, api, job, safetyRunnerFrame(t, sourceConfirmed(claim)))
		}
	}
	actual := storedSchemaOf(t, api, schema)
	t.Fatalf("no %s Job was dispatched after sixteen passes: phase=%q, active=%#v, ReconciliationFailed=%#v",
		operation, actual.Status.Phase, actual.Status.ActiveOperation,
		findCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed))
	return nil
}

// undecidedCycle is one interval of a resource whose reads cannot decide a
// declared object: the Observe Job and the Plan Job the controller builds,
// each run by the runner against ptah and handed back as its Pod's log.
func undecidedCycle(
	t *testing.T, reconciler *SchemaReconciler, api client.Client, schema *operatorv1alpha1.PtahSchema, ptah capturedPtah,
) *operatorv1alpha1.PtahSchema {
	t.Helper()
	for _, operation := range []operatorv1alpha1.OperationType{operatorv1alpha1.OperationObserve, operatorv1alpha1.OperationPlan} {
		job := reconcileUntilOperationJob(t, reconciler, api, schema, operation)
		result, frame := runCapturedRead(t, reconciler, job, operation, ptah)
		if result.Error != nil {
			t.Fatalf("the %s read failed: %#v", operation, result.Error)
		}
		finishBuiltJob(t, reconciler, api, job, frame)
		after := reconcileSchema(t, reconciler, api, schema)
		if failed := findCondition(after.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed); failed != nil &&
			failed.Status == metav1.ConditionTrue {
			t.Fatalf("the %s read was reported as a failure: %#v", operation, failed)
		}
		if operation == operatorv1alpha1.OperationObserve &&
			meta.IsStatusConditionTrue(after.Status.Conditions, operatorv1alpha1.ConditionInSync) {
			t.Fatalf("an Observe that could not decide a declared object reads as in sync: %#v", after.Status.Conditions)
		}
	}
	return storedSchemaOf(t, api, schema)
}

// undecidedReconciler is a resource about to observe, with the real Job
// builder, and the recorder its Events go to.
func undecidedReconciler(t *testing.T) (*operatorv1alpha1.PtahSchema, *SchemaReconciler, client.Client, *record.FakeRecorder) {
	t.Helper()
	claim := &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationObserve, ID: "observe-undecided", Attempt: 1, StartedAt: metav1.Now(),
	}
	schema, policy := planClaimFixture(t, claim)
	schema.Status.Phase = operatorv1alpha1.PhaseObserving
	builder, key := leaderBuilder(t)
	reconciler, api := fakeReconciler(t, staticLogs{}, schema, policy)
	reconciler.Client = jobUIDByNameClient{Client: api}
	reconciler.Jobs = builder
	reconciler.SealKey = key
	recorder := record.NewFakeRecorder(256)
	reconciler.Recorder = recorder
	return storedSchemaOf(t, api, schema), reconciler, api, recorder
}

// undecidedWarnings drains recorder and returns the DriftUndecided Warnings.
func undecidedWarnings(recorder *record.FakeRecorder) []string {
	var warnings []string
	for {
		select {
		case event := <-recorder.Events:
			if strings.HasPrefix(event, corev1.EventTypeWarning+" DriftUndecided ") {
				warnings = append(warnings, event)
			}
		default:
			return warnings
		}
	}
}

// stokaro/ptah#3850 under the URL of the runner harness row
// differs-publication-scoped. Ptah could not decide whether the declared
// publication exists, so its drift check failed without finding drift and its
// plan saved nothing. Both reads are what the pinned Ptah printed, run by the
// runner the controller's Jobs name. The resource is not in sync, drift is
// unknown, there is nothing to approve, Ready and a Warning name the
// publication and Ptah's reason, and the next read is a whole interval away.
// A second interval does the same and moves neither DriftDetected's nor
// Ready's transition time.
// The control is the same reads with the publication decided, which reach
// InSync: without it a fixture that never converged would pass the row.
func TestReadsThatCouldNotDecideAPublicationReachDriftUndecided(t *testing.T) {
	t.Parallel()

	ptah := capturedPtah{
		t:     t,
		drift: undecidedCapture(t, "drift-undecided-publication-postgres.json"),
		plan:  undecidedCapture(t, "plan-undecided-publication-postgres.json"),
	}
	schema, reconciler, api, recorder := undecidedReconciler(t)
	first := undecidedCycle(t, reconciler, api, schema, ptah)

	if first.Status.Plan != nil {
		t.Fatalf("a plan of undecided objects was published: %#v", first.Status.Plan)
	}
	if first.Status.Phase != operatorv1alpha1.PhaseBlocked || first.Status.ActiveOperation != nil {
		t.Fatalf("phase %s with operation %#v, want Blocked and no operation", first.Status.Phase, first.Status.ActiveOperation)
	}
	for _, expected := range []struct {
		condition string
		status    metav1.ConditionStatus
	}{
		{operatorv1alpha1.ConditionDriftDetected, metav1.ConditionUnknown},
		{operatorv1alpha1.ConditionPlanReady, metav1.ConditionFalse},
		{operatorv1alpha1.ConditionInSync, metav1.ConditionFalse},
		{operatorv1alpha1.ConditionReady, metav1.ConditionFalse},
	} {
		if !conditionMatches(first.Status.Conditions, expected.condition, expected.status, operatorv1alpha1.ReasonDriftUndecided) {
			t.Fatalf("%s is not %s with %s: %#v", expected.condition, expected.status,
				operatorv1alpha1.ReasonDriftUndecided, first.Status.Conditions)
		}
	}
	if meta.IsStatusConditionTrue(first.Status.Conditions, operatorv1alpha1.ConditionApprovalRequired) {
		t.Fatalf("a plan of undecided objects asks for an approval: %#v", first.Status.Conditions)
	}
	ready := findCondition(first.Status.Conditions, operatorv1alpha1.ConditionReady)
	for _, named := range []string{
		"PostgreSQL publication differs_notes_pub (whether the publication exists was not established",
		"use a URL that reads the whole database",
	} {
		if !strings.Contains(ready.Message, named) {
			t.Fatalf("Ready says %q, want %q in it", ready.Message, named)
		}
	}
	interval := interval(first)
	next := first.Status.NextReconciliationTime
	if next == nil || next.Sub(reconciler.now()) < interval-time.Second {
		t.Fatalf("the next read is at %v, want %v after %v", next, interval, reconciler.now())
	}
	warnings := undecidedWarnings(recorder)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "PostgreSQL publication differs_notes_pub") {
		t.Fatalf("Warnings = %q, want one DriftUndecided naming the publication", warnings)
	}

	// The next interval reads the same database the same way. A transition
	// time is the wall clock, and both intervals run within a second, so the
	// first interval's times are backdated an hour: a condition the second
	// interval moved reads now, and one it left reads the hour. Every
	// interval's source refresh sets PlanReady Unknown, so PlanReady moves,
	// and is the control that a moved time shows.
	backdated := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	stored := storedSchemaOf(t, api, schema)
	for _, condition := range []string{
		operatorv1alpha1.ConditionDriftDetected, operatorv1alpha1.ConditionPlanReady, operatorv1alpha1.ConditionReady,
	} {
		findCondition(stored.Status.Conditions, condition).LastTransitionTime = backdated
	}
	if err := api.Status().Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	reached := next.Add(time.Second)
	reconciler.Clock = func() time.Time { return reached }
	second := undecidedCycle(t, reconciler, api, schema, ptah)
	for _, condition := range []string{operatorv1alpha1.ConditionDriftDetected, operatorv1alpha1.ConditionReady} {
		after := findCondition(second.Status.Conditions, condition)
		if after == nil || after.Reason != string(operatorv1alpha1.ReasonDriftUndecided) || !after.LastTransitionTime.Equal(&backdated) {
			t.Fatalf("the second interval moved %s: %#v, want its transition at %v", condition, after, backdated)
		}
	}
	if planReady := findCondition(second.Status.Conditions, operatorv1alpha1.ConditionPlanReady); planReady == nil ||
		planReady.Reason != string(operatorv1alpha1.ReasonDriftUndecided) || planReady.LastTransitionTime.Equal(&backdated) {
		t.Fatalf("PlanReady reads %#v; the refresh should have moved it, so the backdating shows nothing", planReady)
	}
	if next := second.Status.NextReconciliationTime; next == nil || next.Sub(reached) < interval-time.Second {
		t.Fatalf("the second interval reads next at %v, want %v after %v", next, interval, reached)
	}
	if warnings := undecidedWarnings(recorder); len(warnings) != 1 {
		t.Fatalf("the second interval emitted %q, want one DriftUndecided", warnings)
	}

	control, controlReconciler, controlAPI, _ := undecidedReconciler(t)
	decided := undecidedCycle(t, controlReconciler, controlAPI, control, capturedPtah{
		t: t, drift: withoutUndecided(t, ptah.drift), plan: withoutUndecided(t, ptah.plan),
	})
	if !meta.IsStatusConditionTrue(decided.Status.Conditions, operatorv1alpha1.ConditionInSync) {
		t.Fatalf("the control did not reach InSync, so the row measured nothing: phase %s, %#v",
			decided.Status.Phase, decided.Status.Conditions)
	}
}

// The read after an Apply is the proof that it converged. A plan that could
// not decide a declared object proves nothing, so the proof ends there: no
// Applied record, the Lease released, the resource not in sync, the next read
// an interval away, and the journal's Verified entry Undecided. Before, the
// undecided-only drift report this proof reads was refused as malformed at
// every attempt.
func TestAProofThatCouldNotDecideEndsWithoutApplied(t *testing.T) {
	t.Parallel()

	schema, reconciler, api := builtProofFixture(t, operatorv1alpha1.OperationPlan, func(schema *operatorv1alpha1.PtahSchema) {
		target, err := runner.TargetIdentityDigest(undecidedDatabaseURL)
		if err != nil {
			t.Fatal(err)
		}
		schema.Status.PendingObservation.Plan.TargetIdentityDigest = target
		schema.Status.Target.IdentityDigest = target
	})
	job := reconcileUntilJobNamed(t, reconciler, api, schema, schema.Status.ActiveOperation.JobName)
	result, frame := runCapturedRead(t, reconciler, job, operatorv1alpha1.OperationPlan, capturedPtah{
		t: t, plan: undecidedCapture(t, "plan-undecided-publication-postgres.json"),
	})
	if result.Error != nil || result.PlanOutcome != runner.PlanOutcomeUndecided {
		t.Fatalf("the proof read returned error %#v with outcome %q, want %s", result.Error, result.PlanOutcome, runner.PlanOutcomeUndecided)
	}
	finishBuiltJob(t, reconciler, api, job, frame)
	actual := reconcileSchema(t, reconciler, api, schema)

	if actual.Status.PendingObservation != nil || actual.Status.ActiveOperation != nil {
		t.Fatalf("the proof did not end: pending %#v, operation %#v", actual.Status.PendingObservation, actual.Status.ActiveOperation)
	}
	if actual.Status.Applied != nil {
		t.Fatalf("a proof that decided nothing recorded the Apply as converged: %#v", actual.Status.Applied)
	}
	if !conditionMatches(actual.Status.Conditions, operatorv1alpha1.ConditionInSync, metav1.ConditionFalse, operatorv1alpha1.ReasonDriftUndecided) ||
		actual.Status.Phase != operatorv1alpha1.PhaseBlocked {
		t.Fatalf("phase %s, %#v, want Blocked and InSync False with %s", actual.Status.Phase, actual.Status.Conditions,
			operatorv1alpha1.ReasonDriftUndecided)
	}
	if next := actual.Status.NextReconciliationTime; next == nil || next.Sub(reconciler.now()) < interval(actual)-time.Second {
		t.Fatalf("the next read is at %v, want %v after %v", next, interval(actual), reconciler.now())
	}
	if holder := proofLeaseHolder(t, reconciler, api); holder != "" {
		t.Fatalf("the ended proof still holds the Lease as %q", holder)
	}
	// The journal says how the proof ended, and it is neither in sync nor a
	// newer generation's.
	var verified []operatorv1alpha1.JournalEntry
	if actual.Status.Journal != nil {
		for _, entry := range actual.Status.Journal.Entries {
			if entry.Type == operatorv1alpha1.JournalVerified {
				verified = append(verified, entry)
			}
		}
	}
	pending := schema.Status.PendingObservation
	if len(verified) != 1 || verified[0].Outcome != operatorv1alpha1.JournalUndecided ||
		verified[0].Plan != pending.Plan.Name || verified[0].OperationID != pending.ApplyOperationID {
		t.Fatalf("the journal records %#v, want one Verified Undecided for plan %s and Apply %s",
			verified, pending.Plan.Name, pending.ApplyOperationID)
	}
}
