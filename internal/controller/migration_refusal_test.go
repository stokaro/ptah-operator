package controller

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/telemetry"
)

// movedDigest is another artifact under the reference the fixture resolves.
var movedDigest = "sha256:" + strings.Repeat("b", 64)

func resolvedTo(digest string) runner.Result {
	return runner.Result{
		Operation: runner.OperationResolve, ChildExitCode: 0,
		ResolvedDigest:    digest,
		ResolvedReference: "oci://registry.example/team/migrations@" + digest,
		ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json", ResolvedSize: 321,
	}
}

func refusedBy(digest string) runner.Result {
	return runner.Result{
		Operation: runner.OperationVerify, ChildExitCode: 2, ResolvedDigest: digest,
		VerificationRequirements: []string{"require_signature"}, VerificationPolicyDigest: migrationVerificationPolicyDigest(),
		Error: &runner.ResultError{Code: "verification_refused", Message: "artifact does not satisfy the verification policy"},
	}
}

func acceptedBy(digest string) runner.Result {
	return runner.Result{
		Operation: runner.OperationVerify, ChildExitCode: 0,
		ObservedArtifactType: dataplane.MigrationArtifactType, ResolvedDigest: digest,
	}
}

// historyAnswer is the database's account of itself, for the claim the harness
// is holding.
func historyAnswer(harness *migrationLifecycle, report dataplane.MigrationStatusReport) runner.Result {
	return runner.Result{
		Operation: runner.OperationMigrationHistory, ChildExitCode: 0,
		CoordinationDigest: harness.coordinationDigest(), TargetIdentityDigest: testDigest,
		MigrationHistory: &report,
	}
}

func pendingReport() dataplane.MigrationStatusReport {
	return dataplane.MigrationStatusReport{
		ContractVersion: dataplane.SupportedMigrationStatusContract,
		TotalMigrations: 1, HasPendingChanges: true,
		Migrations: []dataplane.MigrationRecord{{Version: 1, Checksum: "checksum-1", State: dataplane.MigrationStatePending}},
	}
}

func inSyncReport() dataplane.MigrationStatusReport {
	return dataplane.MigrationStatusReport{
		ContractVersion: dataplane.SupportedMigrationStatusContract,
		CurrentVersion:  1, TotalMigrations: 1,
		Migrations: []dataplane.MigrationRecord{{Version: 1, Checksum: "checksum-1", State: dataplane.MigrationStateApplied}},
	}
}

// refuse takes the resource through one read of its artifact that the policy
// refuses, and leaves the clock at the instant the next read is due.
func (h *migrationLifecycle) refuse(clock *fixedClock) {
	h.t.Helper()

	h.claimed(operatorv1alpha1.MigrationOperationResolve)
	h.answer(resolvedTo(testDigest))
	h.claimed(operatorv1alpha1.MigrationOperationVerify)
	h.answer(refusedBy(testDigest))
	h.phase(operatorv1alpha1.MigrationPhaseBlocked)
	h.dueNow(clock)
}

// dueNow moves the clock past the deadline the resource persisted.
func (h *migrationLifecycle) dueNow(clock *fixedClock) {
	h.t.Helper()

	next := h.migration().Status.NextReconciliationTime
	if next == nil {
		h.t.Fatal("the resource scheduled no next read")
	}
	clock.now = next.Add(time.Second)
}

func wantMigrationCondition(
	t *testing.T,
	migration *operatorv1alpha1.PtahMigration,
	conditionType string,
	status metav1.ConditionStatus,
	reason operatorv1alpha1.ConditionReason,
) {
	t.Helper()

	condition := meta.FindStatusCondition(migration.Status.Conditions, conditionType)
	if condition == nil || condition.Status != status || condition.Reason != string(reason) {
		t.Fatalf("%s = %#v, want %s/%s", conditionType, condition, status, reason)
	}
}

// assertNoConditionNamesARefusal holds the whole list rather than the conditions
// a row thought of: a reason left on any of them is a resource that says it is
// refused by something that no longer refuses it.
func assertNoConditionNamesARefusal(t *testing.T, migration *operatorv1alpha1.PtahMigration) {
	t.Helper()

	if len(migration.Status.Conditions) == 0 {
		t.Fatal("the resource has no conditions, so this check read nothing")
	}
	for _, condition := range migration.Status.Conditions {
		switch operatorv1alpha1.ConditionReason(condition.Reason) {
		case operatorv1alpha1.ReasonPolicyRefused, operatorv1alpha1.ReasonRealmConflict, operatorv1alpha1.ReasonRealmNotAuthorized:
			t.Fatalf("%s still says %s in phase %s: %q", condition.Type, condition.Reason, migration.Status.Phase, condition.Message)
		}
	}
}

// A refusal ends when the artifact passes Verify, whatever the history read goes
// on to say. Only the last row publishes a plan, which is the one writer of
// ApprovalRequired a read has, and the failing row writes nothing but Progressing.
func TestMigrationResumesAfterAPolicyRefusalAtItsInterval(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name    string
		answer  func(*migrationLifecycle) runner.Result
		settled func(*testing.T, *operatorv1alpha1.PtahMigration)
	}{
		{
			name: "nothing pending reads as Ready",
			answer: func(harness *migrationLifecycle) runner.Result {
				return historyAnswer(harness, inSyncReport())
			},
			settled: func(t *testing.T, actual *operatorv1alpha1.PtahMigration) {
				t.Helper()
				if actual.Status.Phase != operatorv1alpha1.MigrationPhaseInSync {
					t.Fatalf("phase = %q, want InSync", actual.Status.Phase)
				}
				wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionTrue, operatorv1alpha1.ReasonHistoryMatched)
				wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse, operatorv1alpha1.ReasonPolicySatisfied)
			},
		},
		{
			name: "a history read that keeps failing",
			answer: func(harness *migrationLifecycle) runner.Result {
				return runner.Result{
					Operation: runner.OperationMigrationHistory, ChildExitCode: 1,
					CoordinationDigest: harness.coordinationDigest(),
					Error:              &runner.ResultError{Code: "child_exit", Message: "ptah exited with code 1"},
				}
			},
			settled: func(t *testing.T, actual *operatorv1alpha1.PtahMigration) {
				t.Helper()
				operation := actual.Status.ActiveOperation
				if operation == nil || operation.Type != operatorv1alpha1.MigrationOperationHistory || operation.Attempt != 2 {
					t.Fatalf("claim = %#v, want the history read retried", operation)
				}
				// What a wait that times out names: the first of Blocked and
				// Progressing that says something, so the failure has to be the
				// one left saying it.
				wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionFalse, operatorv1alpha1.ReasonPolicySatisfied)
				wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue, operatorv1alpha1.ReasonOperationFailed)
				wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse, operatorv1alpha1.ReasonPolicySatisfied)
				wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse, operatorv1alpha1.ReasonPolicySatisfied)
			},
		},
		{
			name: "a history a person has to repair",
			answer: func(harness *migrationLifecycle) runner.Result {
				report := inSyncReport()
				report.DirtyRevision = &dataplane.MigrationDirty{Version: 1, Applied: 1, Total: 2}
				return historyAnswer(harness, report)
			},
			settled: func(t *testing.T, actual *operatorv1alpha1.PtahMigration) {
				t.Helper()
				wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue, operatorv1alpha1.ReasonHistoryDirty)
				wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse, operatorv1alpha1.ReasonPolicySatisfied)
			},
		},
		{
			name: "a database ahead of the artifact",
			answer: func(harness *migrationLifecycle) runner.Result {
				report := inSyncReport()
				report.CurrentVersion = 2
				return historyAnswer(harness, report)
			},
			settled: func(t *testing.T, actual *operatorv1alpha1.PtahMigration) {
				t.Helper()
				wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue, operatorv1alpha1.ReasonHistoryAhead)
				wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse, operatorv1alpha1.ReasonPolicySatisfied)
			},
		},
		{
			name: "a pending migration publishes a plan",
			answer: func(harness *migrationLifecycle) runner.Result {
				return historyAnswer(harness, pendingReport())
			},
			settled: func(t *testing.T, actual *operatorv1alpha1.PtahMigration) {
				t.Helper()
				if actual.Status.Plan == nil || actual.Status.Phase != operatorv1alpha1.MigrationPhaseAwaitingApproval {
					t.Fatalf("plan %#v, phase %q, want a plan waiting for a person", actual.Status.Plan, actual.Status.Phase)
				}
				wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionFalse, operatorv1alpha1.ReasonHistoryMatched)
				wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionTrue, operatorv1alpha1.ReasonAwaitingApproval)
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			harness := newMigrationLifecycle(t, migrationFixture())
			clock := movableMigrationClock(harness.reconciler, harness.api)
			harness.refuse(clock)
			if harness.migration().Status.ActiveOperation != nil {
				t.Fatalf("the refusal left a claim: %#v", harness.migration().Status.ActiveOperation)
			}

			harness.claimed(operatorv1alpha1.MigrationOperationResolve)
			harness.answer(resolvedTo(testDigest))
			harness.claimed(operatorv1alpha1.MigrationOperationVerify)
			harness.answer(acceptedBy(testDigest))
			harness.claimed(operatorv1alpha1.MigrationOperationHistory)
			accepted := harness.migration()
			wantMigrationCondition(t, accepted, operatorv1alpha1.ConditionMigrationArtifactVerified, metav1.ConditionTrue, operatorv1alpha1.ReasonPolicySatisfied)
			wantMigrationCondition(t, accepted, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionFalse, operatorv1alpha1.ReasonPolicySatisfied)
			assertNoConditionNamesARefusal(t, accepted)

			harness.answer(row.answer(harness))
			actual := harness.migration()
			row.settled(t, actual)
			assertNoConditionNamesARefusal(t, actual)
		})
	}
}

// The acceptance row for a refusal that ends is held to this reading before any
// cluster runs it, so a predicate cannot pass a document the controller never
// writes. A change that moves these conditions has to move the reading and say
// what the row now waits for.
func TestTheRefusalRecoveryReadingIsTheReconcilers(t *testing.T) {
	t.Parallel()

	harness := newMigrationLifecycle(t, migrationFixture())
	clock := movableMigrationClock(harness.reconciler, harness.api)
	harness.refuse(clock)
	harness.claimed(operatorv1alpha1.MigrationOperationResolve)
	harness.answer(resolvedTo(testDigest))
	harness.claimed(operatorv1alpha1.MigrationOperationVerify)
	harness.answer(acceptedBy(testDigest))
	harness.claimed(operatorv1alpha1.MigrationOperationHistory)
	harness.answer(historyAnswer(harness, pendingReport()))
	actual := harness.migration()

	content, err := os.ReadFile("../../testdata/e2e/readings/native-refusal-recovered-migration.json")
	if err != nil {
		t.Fatal(err)
	}
	reading := &operatorv1alpha1.PtahMigration{}
	if err := json.Unmarshal(content, reading); err != nil {
		t.Fatal(err)
	}
	if reading.Status.Phase != actual.Status.Phase || (reading.Status.Plan == nil) != (actual.Status.Plan == nil) ||
		(reading.Status.ActiveOperation == nil) != (actual.Status.ActiveOperation == nil) {
		t.Fatalf("the reading is %s with plan %#v and claim %#v, and the reconciler wrote %s with plan %#v and claim %#v",
			reading.Status.Phase, reading.Status.Plan, reading.Status.ActiveOperation,
			actual.Status.Phase, actual.Status.Plan, actual.Status.ActiveOperation)
	}
	tuples := func(conditions []metav1.Condition) []string {
		names := make([]string, 0, len(conditions))
		for _, condition := range conditions {
			names = append(names, condition.Type+"="+string(condition.Status)+"/"+condition.Reason)
		}
		slices.Sort(names)
		return names
	}
	if got, want := tuples(reading.Status.Conditions), tuples(actual.Status.Conditions); !slices.Equal(got, want) || len(want) == 0 {
		t.Fatalf("the reading's conditions are %v, and the reconciler wrote %v", got, want)
	}
}

// A refusal is an answer about one artifact under one policy. While both stand
// it holds through the read that asks again, and through a Verify that cannot
// answer: the last answer the resource has is still that refusal. This is what
// the rows below it have to differ from.
func TestAPolicyRefusalHoldsThroughAReadOfTheSameArtifact(t *testing.T) {
	t.Parallel()

	harness := newMigrationLifecycle(t, migrationFixture())
	clock := movableMigrationClock(harness.reconciler, harness.api)
	harness.refuse(clock)

	harness.claimed(operatorv1alpha1.MigrationOperationResolve)
	harness.answer(resolvedTo(testDigest))
	harness.claimed(operatorv1alpha1.MigrationOperationVerify)
	harness.answer(runner.Result{
		Operation: runner.OperationVerify, ChildExitCode: 1, ResolvedDigest: testDigest,
		Error: &runner.ResultError{Code: "child_exit", Message: "ptah exited with code 1"},
	})
	actual := harness.migration()
	wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue, operatorv1alpha1.ReasonPolicyRefused)
	wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse, operatorv1alpha1.ReasonPolicyRefused)
	wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue, operatorv1alpha1.ReasonOperationFailed)
}

// A refusal says nothing about another source, so it goes when the source does,
// whatever becomes of that source next.
func TestAnotherSourceEndsAPolicyRefusal(t *testing.T) {
	t.Parallel()

	editSource := func(t *testing.T, harness *migrationLifecycle, source operatorv1alpha1.SourceSpec) {
		t.Helper()
		current := harness.migration()
		current.Spec.Artifact = source
		current.Generation++
		if err := harness.api.Update(context.Background(), current); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a ConfigMap source", func(t *testing.T) {
		t.Parallel()

		harness := newMigrationLifecycle(t, migrationFixture())
		clock := movableMigrationClock(harness.reconciler, harness.api)
		harness.refuse(clock)

		source := sourceConfigMap("unused")
		source.Data = map[string]string{"0001.up.sql": "CREATE TABLE a (id int);\n"}
		if err := harness.api.Create(context.Background(), source); err != nil {
			t.Fatal(err)
		}
		editSource(t, harness, operatorv1alpha1.SourceSpec{ConfigMap: &operatorv1alpha1.ConfigMapSourceSpec{
			Name:  source.Name,
			Items: []operatorv1alpha1.ConfigMapSourceItem{{Key: "0001.up.sql", Path: "0001_init.up.sql"}},
		}})
		// The read of a ConfigMap is one pass of the manager, and the next claims
		// the history. Read what the first one wrote.
		if _, err := harness.reconciler.Reconcile(context.Background(), migrationRequest(harness.migrationObject())); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		actual := harness.migration()
		if actual.Status.Phase != operatorv1alpha1.MigrationPhaseReading {
			t.Fatalf("phase = %q, want the history read next", actual.Status.Phase)
		}
		if verified := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationArtifactVerified); verified != nil {
			t.Fatalf("a ConfigMap source reports a verification it never had: %#v", verified)
		}
		wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionFalse, operatorv1alpha1.ReasonSnapshotTaken)
		wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse, operatorv1alpha1.ReasonSnapshotTaken)
		wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse, operatorv1alpha1.ReasonSnapshotTaken)
		assertNoConditionNamesARefusal(t, actual)
	})

	t.Run("a Git source", func(t *testing.T) {
		t.Parallel()

		harness := newMigrationLifecycle(t, migrationFixture())
		clock := movableMigrationClock(harness.reconciler, harness.api)
		harness.refuse(clock)

		spec := gitSourceSpec()
		spec.Path = "db/migrations"
		editSource(t, harness, operatorv1alpha1.SourceSpec{Git: spec})
		harness.claimed(operatorv1alpha1.MigrationOperationResolve)
		wantMigrationCondition(t, harness.migration(), operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue, operatorv1alpha1.ReasonPolicyRefused)
		harness.answer(runner.Result{
			Operation: runner.OperationResolve, ChildExitCode: 0,
			ResolvedCommit: gitCommitA, ResolvedContentDigest: gitContentOne,
		})
		actual := harness.migration()
		if verified := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationArtifactVerified); verified != nil {
			t.Fatalf("a Git source reports a verification it never had: %#v", verified)
		}
		wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionFalse, operatorv1alpha1.ReasonCommitResolved)
		wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationReady, metav1.ConditionFalse, operatorv1alpha1.ReasonCommitResolved)
		wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse, operatorv1alpha1.ReasonCommitResolved)
		assertNoConditionNamesARefusal(t, actual)
	})

	t.Run("a tag moved to another digest", func(t *testing.T) {
		t.Parallel()

		harness := newMigrationLifecycle(t, migrationFixture())
		clock := movableMigrationClock(harness.reconciler, harness.api)
		harness.refuse(clock)

		harness.claimed(operatorv1alpha1.MigrationOperationResolve)
		harness.answer(resolvedTo(movedDigest))
		harness.claimed(operatorv1alpha1.MigrationOperationVerify)
		// The new artifact gets no answer, and the old one's refusal is not
		// that answer.
		harness.answer(runner.Result{
			Operation: runner.OperationVerify, ChildExitCode: 1, ResolvedDigest: movedDigest,
			Error: &runner.ResultError{Code: "child_exit", Message: "ptah exited with code 1"},
		})
		actual := harness.migration()
		wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationArtifactVerified, metav1.ConditionUnknown, operatorv1alpha1.ReasonDigestPinned)
		wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionFalse, operatorv1alpha1.ReasonDigestPinned)
		wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse, operatorv1alpha1.ReasonDigestPinned)
		wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue, operatorv1alpha1.ReasonOperationFailed)
		assertNoConditionNamesARefusal(t, actual)
	})
}

// A realm refusal writes ApprovalRequired too, and a resource that goes on to
// read in sync publishes no plan to rewrite it.
func TestARealmRefusalLeavesNoReasonOnAResourceInSync(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name   string
		reason operatorv1alpha1.ConditionReason
		setup  func() (*operatorv1alpha1.PtahMigration, client.Object)
		lift   func(*testing.T, client.Client, client.Object)
	}{
		{
			name:   "a realm that admits it at last",
			reason: operatorv1alpha1.ReasonRealmNotAuthorized,
			setup: func() (*operatorv1alpha1.PtahMigration, client.Object) {
				return realmBoundMigrationFixture("orders-primary", false), nil
			},
			lift: func(t *testing.T, api client.Client, _ client.Object) {
				t.Helper()
				realm := realmFixture("orders-primary", operatorv1alpha1.RealmSharingExclusive, "team-a")
				if err := api.Create(context.Background(), realm); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "a claimant that went away",
			reason: operatorv1alpha1.ReasonRealmConflict,
			setup: func() (*operatorv1alpha1.PtahMigration, client.Object) {
				migration := migrationFixture()
				return migration, realmSchemaFixture(migration.Namespace, "orders", migration.Spec.Target.CoordinationKey, false)
			},
			lift: func(t *testing.T, api client.Client, competitor client.Object) {
				t.Helper()
				if err := api.Delete(context.Background(), competitor); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			migration, competitor := row.setup()
			logs := &replayLogs{}
			objects := []client.Object{migration, verificationPolicyConfigMap()}
			if competitor != nil {
				objects = append(objects, competitor)
			}
			reconciler, api := fakeMigrationReconciler(t, logs, objects...)
			harness := &migrationLifecycle{t: t, reconciler: reconciler, api: api, logs: logs, key: client.ObjectKeyFromObject(migration)}
			harness.settle()
			clock := movableMigrationClock(reconciler, api)

			refused := harness.migration()
			wantMigrationCondition(t, refused, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue, row.reason)
			wantMigrationCondition(t, refused, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse, row.reason)

			row.lift(t, api, competitor)
			harness.dueNow(clock)
			harness.claimed(operatorv1alpha1.MigrationOperationResolve)
			harness.answer(resolvedTo(testDigest))
			harness.claimed(operatorv1alpha1.MigrationOperationVerify)
			harness.answer(acceptedBy(testDigest))
			harness.claimed(operatorv1alpha1.MigrationOperationHistory)
			harness.answer(historyAnswer(harness, inSyncReport()))

			actual := harness.migration()
			if actual.Status.Phase != operatorv1alpha1.MigrationPhaseInSync {
				t.Fatalf("phase = %q, want InSync", actual.Status.Phase)
			}
			wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionFalse, operatorv1alpha1.ReasonPolicySatisfied)
			assertNoConditionNamesARefusal(t, actual)
		})
	}
}

// waitingForAnApproval reads the fixture to a published plan.
func waitingForAnApproval(t *testing.T) (*migrationLifecycle, *fixedClock, *operatorv1alpha1.PtahMigrationPlan) {
	t.Helper()

	harness := newMigrationLifecycle(t, migrationFixture())
	clock := movableMigrationClock(harness.reconciler, harness.api)
	harness.claimed(operatorv1alpha1.MigrationOperationResolve)
	harness.answer(resolvedTo(testDigest))
	harness.claimed(operatorv1alpha1.MigrationOperationVerify)
	harness.answer(acceptedBy(testDigest))
	harness.claimed(operatorv1alpha1.MigrationOperationHistory)
	harness.answer(historyAnswer(harness, pendingReport()))
	waiting := harness.migration()
	if waiting.Status.Phase != operatorv1alpha1.MigrationPhaseAwaitingApproval || waiting.Status.Plan == nil {
		t.Fatalf("phase %q, plan %#v, want a plan waiting for a person", waiting.Status.Phase, waiting.Status.Plan)
	}
	plan := &operatorv1alpha1.PtahMigrationPlan{}
	key := client.ObjectKey{Namespace: waiting.Namespace, Name: waiting.Status.Plan.Name}
	if err := harness.api.Get(context.Background(), key, plan); err != nil {
		t.Fatal(err)
	}
	return harness, clock, plan
}

func readMigrationApproval(
	t *testing.T,
	api client.Client,
	approval *operatorv1alpha1.PtahMigrationApproval,
) *operatorv1alpha1.PtahMigrationApproval {
	t.Helper()

	actual := &operatorv1alpha1.PtahMigrationApproval{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(approval), actual); err != nil {
		t.Fatal(err)
	}
	return actual
}

// A read of a waiting plan ends at the same plan when nothing moved, and the
// approval a person wrote meanwhile is still the decision. It is what the next
// test must differ from, and what stops marking approvals stale on every read.
func TestAnApprovalCrossesAReadThatRefusesNothing(t *testing.T) {
	t.Parallel()

	harness, clock, plan := waitingForAnApproval(t)
	approval := migrationApprovalFor(harness.migration(), plan)
	if err := harness.api.Create(context.Background(), approval); err != nil {
		t.Fatal(err)
	}
	harness.dueNow(clock)

	harness.claimed(operatorv1alpha1.MigrationOperationResolve)
	harness.answer(resolvedTo(testDigest))
	harness.claimed(operatorv1alpha1.MigrationOperationVerify)
	harness.answer(acceptedBy(testDigest))
	harness.claimed(operatorv1alpha1.MigrationOperationHistory)
	// Nothing refused it, so a passed Verify leaves the gate as it found it: this
	// is how a person's decision taken during the read knows the gate was open.
	wantMigrationCondition(t, harness.migration(), operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionTrue, operatorv1alpha1.ReasonAwaitingApproval)
	harness.answer(historyAnswer(harness, pendingReport()))
	harness.settle()

	operation := harness.migration().Status.ActiveOperation
	if operation == nil || operation.Type != operatorv1alpha1.MigrationOperationApply ||
		operation.ApprovalRef == nil || operation.ApprovalRef.UID != approval.UID {
		t.Fatalf("claim = %#v, want the Apply the approval authorized", operation)
	}
	if meta.IsStatusConditionTrue(readMigrationApproval(t, harness.api, approval).Status.Conditions, operatorv1alpha1.ConditionApprovalStale) {
		t.Fatal("a read that refused nothing withdrew the approval")
	}
}

// A plan a reading moved away from can come back: the same fingerprint is the
// same name, and the object stored under it keeps its UID, so nothing about the
// plan says anything came between. Each row ends a read somewhere other than the
// approved plan, brings the plan back, and needs a person again. The approval a
// run already spent stays the record of that run.
func TestAnApprovalDoesNotOutliveTheReadThatEndedItsPlan(t *testing.T) {
	t.Parallel()

	two := func() dataplane.MigrationStatusReport {
		report := pendingReport()
		report.TotalMigrations = 2
		report.Migrations = append(report.Migrations, dataplane.MigrationRecord{
			Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStatePending,
		})
		return report
	}
	dirty := func() dataplane.MigrationStatusReport {
		report := inSyncReport()
		report.DirtyRevision = &dataplane.MigrationDirty{Version: 1, Applied: 1, Total: 2}
		return report
	}

	for _, row := range []struct {
		name   string
		report func() dataplane.MigrationStatusReport
		phase  operatorv1alpha1.MigrationPhase
		// published says the reading ended at another plan.
		published bool
	}{
		{name: "another plan replaced it", report: two, phase: operatorv1alpha1.MigrationPhaseAwaitingApproval, published: true},
		{name: "a reading left nothing to apply", report: inSyncReport, phase: operatorv1alpha1.MigrationPhaseInSync},
		{name: "a reading a person has to repair", report: dirty, phase: operatorv1alpha1.MigrationPhaseBlocked},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			harness, clock, plan := waitingForAnApproval(t)
			observed := &telemetryObservation{}
			harness.reconciler.Telemetry = observed
			waiting := harness.migration()
			approval := migrationApprovalFor(waiting, plan)
			spent := migrationApprovalFor(waiting, plan)
			spent.Name, spent.UID = "orders-approval-spent", "approval-spent-uid"
			spent.Status.Conditions = []metav1.Condition{{
				Type: operatorv1alpha1.ConditionApprovalConsumed, Status: metav1.ConditionTrue,
				Reason: string(operatorv1alpha1.ReasonDispatchCommitted), Message: "An earlier Apply carried it out",
				LastTransitionTime: metav1.Now(),
			}}
			for _, stored := range []*operatorv1alpha1.PtahMigrationApproval{approval, spent} {
				if err := harness.api.Create(context.Background(), stored); err != nil {
					t.Fatal(err)
				}
			}
			// The next pass finds the deadline passed, and the read runs first.
			read := func(report dataplane.MigrationStatusReport) {
				t.Helper()
				harness.dueNow(clock)
				harness.claimed(operatorv1alpha1.MigrationOperationResolve)
				harness.answer(resolvedTo(testDigest))
				harness.claimed(operatorv1alpha1.MigrationOperationVerify)
				harness.answer(acceptedBy(testDigest))
				harness.claimed(operatorv1alpha1.MigrationOperationHistory)
				harness.answer(historyAnswer(harness, report))
				harness.settle()
			}

			read(row.report())
			moved := harness.migration()
			if moved.Status.Phase != row.phase {
				t.Fatalf("phase = %q, want %q", moved.Status.Phase, row.phase)
			}
			if row.published != (moved.Status.Plan != nil) || (moved.Status.Plan != nil && moved.Status.Plan.UID == plan.UID) {
				t.Fatalf("plan = %#v, want another plan: %t, and never %s", moved.Status.Plan, row.published, plan.UID)
			}
			withdrawn := readMigrationApproval(t, harness.api, approval)
			stale := meta.FindStatusCondition(withdrawn.Status.Conditions, operatorv1alpha1.ConditionApprovalStale)
			if stale == nil || stale.Status != metav1.ConditionTrue || stale.Reason != string(operatorv1alpha1.ReasonPlanNoLongerCurrent) {
				t.Fatalf("Stale = %#v, want True/PlanNoLongerCurrent", stale)
			}
			if stale := staleCount(observed); stale != 1 {
				t.Fatalf("stale approvals counted = %d in %v, want one for the one decision withdrawn", stale, observed.approvals)
			}

			read(pendingReport())
			back := harness.migration()
			if back.Status.Plan == nil || back.Status.Plan.UID != plan.UID {
				t.Fatalf("plan = %#v, want plan %s back under its UID, which is the case this row is for", back.Status.Plan, plan.UID)
			}
			if operation := back.Status.ActiveOperation; operation != nil {
				t.Fatalf("claimed %#v under an approval the plan's replacement withdrew", operation)
			}
			if back.Status.Phase != operatorv1alpha1.MigrationPhaseAwaitingApproval {
				t.Fatalf("phase = %q, want the plan waiting for a person", back.Status.Phase)
			}
			wantMigrationCondition(t, back, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionTrue, operatorv1alpha1.ReasonAwaitingApproval)
			kept := readMigrationApproval(t, harness.api, spent)
			if meta.FindStatusCondition(kept.Status.Conditions, operatorv1alpha1.ConditionApprovalStale) != nil ||
				!meta.IsStatusConditionTrue(kept.Status.Conditions, operatorv1alpha1.ConditionApprovalConsumed) {
				t.Fatalf("the approval a run spent was rewritten: %#v", kept.Status.Conditions)
			}

			again := migrationApprovalFor(back, plan)
			again.Name, again.UID = "orders-approval-again", "approval-again-uid"
			if err := harness.api.Create(context.Background(), again); err != nil {
				t.Fatal(err)
			}
			harness.settle()
			operation := harness.migration().Status.ActiveOperation
			if operation == nil || operation.Type != operatorv1alpha1.MigrationOperationApply ||
				operation.ApprovalRef == nil || operation.ApprovalRef.UID != again.UID {
				t.Fatalf("claim = %#v, want the Apply the new approval authorized", operation)
			}
		})
	}
}

func staleCount(observed *telemetryObservation) int {
	stale := 0
	for _, outcome := range observed.approvals {
		if outcome == telemetry.ApprovalStale {
			stale++
		}
	}
	return stale
}

// The same artifact accepted again publishes the same plan under the same
// fingerprint, so nothing about the plan says a refusal came between. A person
// decides again, and the refusal can be a signature withdrawn on purpose.
func TestAnApprovalDoesNotCrossAPolicyRefusal(t *testing.T) {
	t.Parallel()

	harness, clock, plan := waitingForAnApproval(t)
	recorder := record.NewFakeRecorder(128)
	harness.reconciler.Recorder = recorder
	observed := &telemetryObservation{}
	harness.reconciler.Telemetry = observed
	waiting := harness.migration()

	// Admitted while admission's conditions held: AwaitingApproval, no claim. The
	// next pass finds the deadline passed, and the due branch runs first.
	current := migrationApprovalFor(waiting, plan)
	// A decision on a plan the status no longer names comes back with that plan.
	earlier := migrationApprovalFor(waiting, plan)
	earlier.Name, earlier.UID = "orders-approval-earlier", "approval-earlier-uid"
	earlier.Spec.PlanRef = operatorv1alpha1.ImmutableObjectReference{Name: "ptah-mplan-earlier", UID: "earlier-plan-uid"}
	// Filters the withdrawal has to refuse.
	foreign := migrationApprovalFor(waiting, plan)
	foreign.Name, foreign.UID = "another-migrations-approval", "approval-foreign-uid"
	foreign.Spec.MigrationRef = operatorv1alpha1.ImmutableObjectReference{Name: "another", UID: "another-migration-uid"}
	spent := migrationApprovalFor(waiting, plan)
	spent.Name, spent.UID = "orders-approval-spent", "approval-spent-uid"
	spent.Status.Conditions = []metav1.Condition{{
		Type: operatorv1alpha1.ConditionApprovalConsumed, Status: metav1.ConditionTrue,
		Reason: string(operatorv1alpha1.ReasonDispatchCommitted), Message: "An earlier Apply carried it out",
		LastTransitionTime: metav1.Now(),
	}}
	for _, approval := range []*operatorv1alpha1.PtahMigrationApproval{current, earlier, foreign, spent} {
		if err := harness.api.Create(context.Background(), approval); err != nil {
			t.Fatal(err)
		}
	}
	harness.dueNow(clock)

	harness.claimed(operatorv1alpha1.MigrationOperationResolve)
	harness.answer(resolvedTo(testDigest))
	harness.claimed(operatorv1alpha1.MigrationOperationVerify)
	harness.answer(refusedBy(testDigest))
	if refused := harness.migration(); refused.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked || refused.Status.Plan != nil {
		t.Fatalf("phase %q, plan %#v, want the refusal to stop the resource", refused.Status.Phase, refused.Status.Plan)
	}

	for _, withdrawn := range []*operatorv1alpha1.PtahMigrationApproval{current, earlier} {
		actual := readMigrationApproval(t, harness.api, withdrawn)
		stale := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionApprovalStale)
		if stale == nil || stale.Status != metav1.ConditionTrue || stale.Reason != string(operatorv1alpha1.ReasonPolicyRefused) {
			t.Fatalf("%s: Stale = %#v, want True/PolicyRefused", withdrawn.Name, stale)
		}
		if actual.Status.ObservedGeneration != withdrawn.Generation {
			t.Fatalf("%s: observedGeneration = %d, want %d", withdrawn.Name, actual.Status.ObservedGeneration, withdrawn.Generation)
		}
	}
	for _, kept := range []*operatorv1alpha1.PtahMigrationApproval{foreign, spent} {
		actual := readMigrationApproval(t, harness.api, kept)
		if meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionApprovalStale) != nil {
			t.Fatalf("%s was marked stale: %#v", kept.Name, actual.Status.Conditions)
		}
	}
	if stale := staleCount(observed); stale != 2 {
		t.Fatalf("stale approvals counted = %d in %v, want the two the refusal withdrew", stale, observed.approvals)
	}
	if event := findEvent(recorder, corev1.EventTypeWarning+" ApprovalStale"); !strings.Contains(event, "verification policy refused") {
		t.Fatalf("the approval's Event says %q", event)
	}

	harness.dueNow(clock)
	harness.claimed(operatorv1alpha1.MigrationOperationResolve)
	harness.answer(resolvedTo(testDigest))
	harness.claimed(operatorv1alpha1.MigrationOperationVerify)
	harness.answer(acceptedBy(testDigest))
	harness.claimed(operatorv1alpha1.MigrationOperationHistory)
	harness.answer(historyAnswer(harness, pendingReport()))
	harness.settle()

	republished := harness.migration()
	if republished.Status.Plan == nil || republished.Status.Plan.UID != plan.UID {
		t.Fatalf("plan = %#v, want the same plan %s, which is the case this row is for", republished.Status.Plan, plan.UID)
	}
	if operation := republished.Status.ActiveOperation; operation != nil {
		t.Fatalf("claimed %#v under an approval from before the refusal", operation)
	}
	if republished.Status.Phase != operatorv1alpha1.MigrationPhaseAwaitingApproval {
		t.Fatalf("phase = %q, want the plan waiting for a person", republished.Status.Phase)
	}
	wantMigrationCondition(t, republished, operatorv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionTrue, operatorv1alpha1.ReasonAwaitingApproval)

	// And a person can decide again.
	again := migrationApprovalFor(republished, plan)
	again.Name, again.UID = "orders-approval-again", "approval-again-uid"
	if err := harness.api.Create(context.Background(), again); err != nil {
		t.Fatal(err)
	}
	harness.settle()
	operation := harness.migration().Status.ActiveOperation
	if operation == nil || operation.Type != operatorv1alpha1.MigrationOperationApply ||
		operation.ApprovalRef == nil || operation.ApprovalRef.UID != again.UID {
		t.Fatalf("claim = %#v, want the Apply the new approval authorized", operation)
	}
}

// The approvals go before the status that says why. Whichever write a pass does
// not reach, the next one finds the same Job and finishes, with the decision
// never left standing beside a refusal.
func TestARefusalWithdrawsApprovalsBeforeItSaysSo(t *testing.T) {
	t.Parallel()

	refusal := refusedBy(testDigest)
	lost := errors.New("the manager stopped here")

	refused := func(t *testing.T, api client.Client, migration *operatorv1alpha1.PtahMigration) {
		t.Helper()
		if actual := readMigration(t, api, migration); actual.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked ||
			actual.Status.ActiveOperation != nil {
			t.Fatalf("phase %q, claim %#v, want the refusal recorded", actual.Status.Phase, actual.Status.ActiveOperation)
		}
	}
	unrecorded := func(t *testing.T, api client.Client, migration *operatorv1alpha1.PtahMigration) {
		t.Helper()
		if actual := readMigration(t, api, migration); actual.Status.Phase == operatorv1alpha1.MigrationPhaseBlocked ||
			actual.Status.ActiveOperation == nil {
			t.Fatalf("phase %q, claim %#v, want the Verify still claimed", actual.Status.Phase, actual.Status.ActiveOperation)
		}
	}
	approvalOf := func(migration *operatorv1alpha1.PtahMigration) *operatorv1alpha1.PtahMigrationApproval {
		return migrationApprovalFor(migration, &operatorv1alpha1.PtahMigrationPlan{
			ObjectMeta: metav1.ObjectMeta{Name: "ptah-mplan-previous", UID: types.UID("previous-plan-uid")},
		})
	}

	t.Run("the approval's write fails", func(t *testing.T) {
		t.Parallel()

		migration, reconciler, api, _ := verifyingMigration(t, refusal)
		approval := approvalOf(migration)
		if err := api.Create(context.Background(), approval); err != nil {
			t.Fatal(err)
		}
		reconciler.Client = interceptor.NewClient(api, interceptor.Funcs{
			SubResourcePatch: func(
				ctx context.Context, inner client.Client, subResource string, object client.Object, patch client.Patch,
				options ...client.SubResourcePatchOption,
			) error {
				if _, ok := object.(*operatorv1alpha1.PtahMigrationApproval); ok {
					return lost
				}
				return inner.SubResource(subResource).Patch(ctx, object, patch, options...)
			},
		})
		if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); !errors.Is(err, lost) {
			t.Fatalf("Reconcile() error = %v, want the failed write", err)
		}
		unrecorded(t, api, migration)

		reconciler.Client = api
		if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		refused(t, api, migration)
		if !meta.IsStatusConditionTrue(readMigrationApproval(t, api, approval).Status.Conditions, operatorv1alpha1.ConditionApprovalStale) {
			t.Fatal("the next pass did not withdraw the approval")
		}
	})

	t.Run("the refusal's write fails", func(t *testing.T) {
		t.Parallel()

		migration, reconciler, api, _ := verifyingMigration(t, refusal)
		approval := approvalOf(migration)
		if err := api.Create(context.Background(), approval); err != nil {
			t.Fatal(err)
		}
		observed := &telemetryObservation{}
		reconciler.Telemetry = observed
		reconciler.Client = interceptor.NewClient(api, interceptor.Funcs{
			SubResourcePatch: func(
				ctx context.Context, inner client.Client, subResource string, object client.Object, patch client.Patch,
				options ...client.SubResourcePatchOption,
			) error {
				if _, ok := object.(*operatorv1alpha1.PtahMigration); ok {
					return lost
				}
				return inner.SubResource(subResource).Patch(ctx, object, patch, options...)
			},
		})
		if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); !errors.Is(err, lost) {
			t.Fatalf("Reconcile() error = %v, want the failed write", err)
		}
		unrecorded(t, api, migration)
		written := readMigrationApproval(t, api, approval)
		if !meta.IsStatusConditionTrue(written.Status.Conditions, operatorv1alpha1.ConditionApprovalStale) {
			t.Fatal("the status write was attempted with the approval still good")
		}

		reconciler.Client = api
		if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		refused(t, api, migration)
		if again := readMigrationApproval(t, api, approval); again.ResourceVersion != written.ResourceVersion {
			t.Fatalf("the next pass wrote the approval again: resourceVersion %s -> %s", written.ResourceVersion, again.ResourceVersion)
		}
		if stale := staleCount(observed); stale != 1 {
			t.Fatalf("stale approvals counted = %d in %v, want one for one approval", stale, observed.approvals)
		}
	})
}

// A run nobody accounted for is somewhere a refusal cannot reach. The refusal
// stops the history read that would clear the record, keeps the record, and
// leaves both other ways out of it standing.
func TestAPolicyRefusalKeepsTheUnresolvedRun(t *testing.T) {
	t.Parallel()

	recordedRun := func() *operatorv1alpha1.PtahMigration {
		migration := migrationFixture()
		migration.Status.UnresolvedRun = &operatorv1alpha1.UnresolvedMigrationRunStatus{
			OperationID:          testDigest,
			Outcome:              operatorv1alpha1.MigrationRunOutcomeUnknown,
			PlanRef:              operatorv1alpha1.ImmutableObjectReference{Name: "ptah-mplan-x", UID: "plan-x"},
			TargetIdentityDigest: testDigest,
			RecordedAt:           metav1.Now(),
		}
		migration.Status.Phase = operatorv1alpha1.MigrationPhaseBlocked
		return migration
	}
	refused := func(t *testing.T) *migrationLifecycle {
		t.Helper()
		harness := newMigrationLifecycle(t, recordedRun())
		clock := movableMigrationClock(harness.reconciler, harness.api)
		harness.refuse(clock)
		actual := harness.migration()
		if actual.Status.UnresolvedRun == nil || actual.Status.UnresolvedRun.OperationID != testDigest {
			t.Fatalf("the record went with the refusal: %#v", actual.Status.UnresolvedRun)
		}
		if _, copied := actual.Annotations[operatorv1alpha1.UnresolvedRunAnnotation]; !copied {
			t.Fatal("the copy that survives a restore went with the refusal")
		}
		if actual.Status.ActiveOperation != nil || actual.Status.History != nil {
			t.Fatalf("claim %#v, history %#v, want a resource resting before any read of the database",
				actual.Status.ActiveOperation, actual.Status.History)
		}
		wantMigrationCondition(t, actual, operatorv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue, operatorv1alpha1.ReasonPolicyRefused)
		return harness
	}

	t.Run("a person's acknowledgment still settles it", func(t *testing.T) {
		t.Parallel()

		harness := refused(t)
		actual := harness.migration()
		acknowledgment := runAcknowledgment(actual, "orders-run-acknowledged", actual.Status.UnresolvedRun.OperationID, time.Now())
		if err := harness.api.Create(context.Background(), acknowledgment); err != nil {
			t.Fatal(err)
		}
		if _, err := harness.reconciler.Reconcile(context.Background(), migrationRequest(harness.migrationObject())); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		settled := harness.migration()
		if settled.Status.UnresolvedRun != nil || settled.Status.ResolvedRun == nil ||
			settled.Status.ResolvedRun.Resolution != operatorv1alpha1.MigrationRunResolvedByAcknowledgment {
			t.Fatalf("record %#v, resolution %#v, want the acknowledgment to settle it",
				settled.Status.UnresolvedRun, settled.Status.ResolvedRun)
		}
	})

	t.Run("the history read clears it once the refusal ends", func(t *testing.T) {
		t.Parallel()

		harness := refused(t)
		harness.claimed(operatorv1alpha1.MigrationOperationResolve)
		harness.answer(resolvedTo(testDigest))
		harness.claimed(operatorv1alpha1.MigrationOperationVerify)
		harness.answer(acceptedBy(testDigest))
		harness.claimed(operatorv1alpha1.MigrationOperationHistory)
		if harness.migration().Status.UnresolvedRun == nil {
			t.Fatal("the record went before the read that settles it")
		}
		harness.answer(historyAnswer(harness, inSyncReport()))
		settled := harness.migration()
		if settled.Status.UnresolvedRun != nil || settled.Status.ResolvedRun == nil ||
			settled.Status.ResolvedRun.Resolution != operatorv1alpha1.MigrationRunResolvedByHistoryRead {
			t.Fatalf("record %#v, resolution %#v, want the reading to settle it",
				settled.Status.UnresolvedRun, settled.Status.ResolvedRun)
		}
		assertNoConditionNamesARefusal(t, settled)
	})
}
