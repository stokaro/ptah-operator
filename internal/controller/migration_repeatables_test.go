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
	"errors"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// Three h1 digests a repeatable migration's content can have.
var (
	repeatableContentA = "h1:" + strings.Repeat("A", 43) + "="
	repeatableContentB = "h1:" + strings.Repeat("B", 43) + "="
	repeatableContentC = "h1:" + strings.Repeat("C", 43) + "="
)

// syncedHistoryWith is a reading with version 2 applied and nothing of it
// pending, beside the repeatable migrations given.
func syncedHistoryWith(repeatables ...dataplane.MigrationRepeatable) dataplane.MigrationStatusReport {
	return dataplane.MigrationStatusReport{
		ContractVersion: dataplane.SupportedMigrationStatusContract,
		CurrentVersion:  2, TotalMigrations: 1,
		HasPendingChanges: slices.ContainsFunc(repeatables, dataplane.MigrationRepeatable.Pending),
		Migrations: []dataplane.MigrationRecord{
			{Version: 2, Checksum: "checksum-2", State: dataplane.MigrationStateApplied},
		},
		Repeatables: repeatables,
	}
}

// readRepeatableHistory runs one history read of report through a frame, as
// the manager receives it, under policy, and returns what it persisted and the
// API that holds any plan it published.
func readRepeatableHistory(
	t *testing.T,
	policy operatorv1alpha1.ApplyPolicy,
	unresolved *operatorv1alpha1.UnresolvedMigrationRunStatus,
	report dataplane.MigrationStatusReport,
) (*operatorv1alpha1.PtahMigration, client.WithWatch) {
	t.Helper()

	reconciler, api, migration := repeatableHistoryReader(t, policy, unresolved, nil, report)
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	return readMigration(t, api, migration), api
}

// repeatableHistoryReader is the reconciler, its API and the migration that
// readRepeatableHistory reads report into, before the read, over a stored
// history of previous.
func repeatableHistoryReader(
	t *testing.T,
	policy operatorv1alpha1.ApplyPolicy,
	unresolved *operatorv1alpha1.UnresolvedMigrationRunStatus,
	previous *operatorv1alpha1.MigrationHistoryStatus,
	report dataplane.MigrationStatusReport,
) (*MigrationReconciler, client.WithWatch, *operatorv1alpha1.PtahMigration) {
	t.Helper()

	migration := migrationFixture()
	migration.Spec.Policy.Apply = policy
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.Artifact = resolvedMigrationArtifact()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseReading
	migration.Status.UnresolvedRun = unresolved
	migration.Status.History = previous
	migration.Finalizers = []string{migrationOperationFinalizer}
	operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationHistory)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	frame := migrationFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationMigrationHistory,
		OperationID: operation.ID, ChildExitCode: 0,
		CoordinationDigest:   operation.CoordinationDigest,
		TargetIdentityDigest: testDigest,
		MigrationHistory:     &report,
	})
	reconciler, api := fakeMigrationReconciler(t, staticLogs{content: frame}, migration, job, pod, verificationPolicyConfigMap())
	return reconciler, api, migration
}

// publishedPlans lists every migration plan the API holds.
func publishedPlans(t *testing.T, api client.Client) []operatorv1alpha1.PtahMigrationPlan {
	t.Helper()

	plans := &operatorv1alpha1.PtahMigrationPlanList{}
	if err := api.List(context.Background(), plans); err != nil {
		t.Fatal(err)
	}
	return plans.Items
}

// A pending or changed repeatable migration is work: the history counts it,
// the plan carries it after the versions in Ptah's order, and the policy
// decides it as it decides a version. One that applied and did not change is
// not planned again.
func TestARepeatableMigrationIsPlannedWork(t *testing.T) {
	t.Parallel()

	pending := dataplane.MigrationRepeatable{
		Identity: "item_names", Path: "R__item_names.sql", Checksum: repeatableContentA, State: dataplane.RepeatableStatePending,
	}
	changed := dataplane.MigrationRepeatable{
		Identity: "order_totals", Path: "R__order_totals.sql", Checksum: repeatableContentB,
		AppliedChecksum: repeatableContentA, State: dataplane.RepeatableStateChanged,
	}
	applied := dataplane.MigrationRepeatable{
		Identity: "audit_view", Path: "R__audit_view.sql", Checksum: repeatableContentC,
		AppliedChecksum: repeatableContentC, State: dataplane.RepeatableStateApplied,
	}
	withVersion := func(report dataplane.MigrationStatusReport) dataplane.MigrationStatusReport {
		report.TotalMigrations = 2
		report.PendingMigrations = []int64{3}
		report.HasPendingChanges = true
		report.Migrations = append(report.Migrations,
			dataplane.MigrationRecord{Version: 3, Checksum: "checksum-3", State: dataplane.MigrationStatePending})
		return report
	}
	for _, test := range []struct {
		name            string
		policy          operatorv1alpha1.ApplyPolicy
		report          dataplane.MigrationStatusReport
		wantVersions    []int64
		wantRepeatables []operatorv1alpha1.PlannedRepeatableMigration
		wantPhase       operatorv1alpha1.MigrationPhase
		condition       string
		says            string
	}{
		{
			name:   "a repeatable migration alone, under OnApproval",
			policy: operatorv1alpha1.ApplyPolicyOnApproval,
			report: syncedHistoryWith(pending, applied),
			wantRepeatables: []operatorv1alpha1.PlannedRepeatableMigration{
				{Identity: "item_names", Checksum: repeatableContentA},
			},
			wantPhase: operatorv1alpha1.MigrationPhaseAwaitingApproval,
			condition: operatorv1alpha1.ConditionMigrationApprovalRequired,
			says:      "1 planned repeatable migration needs an approval naming this plan",
		},
		{
			name:         "a version and a changed repeatable migration, under Always",
			policy:       operatorv1alpha1.ApplyPolicyAlways,
			report:       withVersion(syncedHistoryWith(applied, changed)),
			wantVersions: []int64{3},
			wantRepeatables: []operatorv1alpha1.PlannedRepeatableMigration{
				{Identity: "order_totals", Checksum: repeatableContentB, AppliedChecksum: repeatableContentA},
			},
			wantPhase: operatorv1alpha1.MigrationPhasePlanning,
			condition: operatorv1alpha1.ConditionMigrationProgressing,
			says:      "An Apply of 1 planned migration and 1 planned repeatable migration is what happens next",
		},
		{
			name:   "two in the order Ptah lists them, under Never",
			policy: operatorv1alpha1.ApplyPolicyNever,
			report: syncedHistoryWith(pending, changed),
			wantRepeatables: []operatorv1alpha1.PlannedRepeatableMigration{
				{Identity: "item_names", Checksum: repeatableContentA},
				{Identity: "order_totals", Checksum: repeatableContentB, AppliedChecksum: repeatableContentA},
			},
			wantPhase: operatorv1alpha1.MigrationPhasePlanning,
			condition: operatorv1alpha1.ConditionMigrationReady,
			says:      "2 repeatable migrations are pending and the apply policy is Never",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			actual, api := readRepeatableHistory(t, test.policy, nil, test.report)
			if actual.Status.Phase != test.wantPhase {
				t.Fatalf("phase = %s, want %s (conditions %#v)", actual.Status.Phase, test.wantPhase, actual.Status.Conditions)
			}
			history := actual.Status.History
			if history == nil || int(history.PendingCount) != len(test.wantVersions) ||
				int(history.PendingRepeatableCount) != len(test.wantRepeatables) {
				t.Fatalf("History = %#v, want %d pending versions and %d pending repeatable migrations",
					history, len(test.wantVersions), len(test.wantRepeatables))
			}
			plans := publishedPlans(t, api)
			if len(plans) != 1 || actual.Status.Plan == nil || plans[0].Name != actual.Status.Plan.Name {
				t.Fatalf("published %d plans, status names %#v; want the one plan", len(plans), actual.Status.Plan)
			}
			plan := plans[0]
			versions := make([]int64, 0, len(plan.Spec.Migrations))
			for _, planned := range plan.Spec.Migrations {
				versions = append(versions, planned.Version)
			}
			if !slices.Equal(versions, test.wantVersions) {
				t.Fatalf("planned versions = %v, want %v", versions, test.wantVersions)
			}
			if !equality.Semantic.DeepEqual(plan.Spec.Repeatables, test.wantRepeatables) {
				t.Fatalf("planned repeatable migrations = %#v, want %#v", plan.Spec.Repeatables, test.wantRepeatables)
			}
			if plan.Spec.HistoryFingerprint != history.Fingerprint {
				t.Fatal("the plan does not name the history it was computed against")
			}
			assertMigrationCondition(t, actual, test.condition, test.says)
		})
	}
}

// A repeatable migration whose last attempt failed or recorded no outcome is
// the repeatable record's dirty row: Ptah runs nothing while it exists, so the
// resource stops before a plan and names Ptah's repair, with --force while a
// file still carries the identity. A version pending beside it changes
// nothing.
func TestAnUnfinishedRepeatableMigrationStopsTheResource(t *testing.T) {
	t.Parallel()

	file := func(state string, noTransaction bool) dataplane.MigrationRepeatable {
		return dataplane.MigrationRepeatable{
			Identity: "names", Path: "R__names.sql", Checksum: repeatableContentB,
			AppliedChecksum: repeatableContentA, State: state, NoTransaction: noTransaction,
		}
	}
	for _, test := range []struct {
		name       string
		repeatable dataplane.MigrationRepeatable
		says       []string
		never      string
	}{
		{
			name:       "failed inside a transaction",
			repeatable: file(dataplane.RepeatableStateFailed, false),
			says: []string{
				"names failed inside a transaction, which took no effect",
				"(repair: `ptah migrations repair --repeatable names --force`)",
				"Ptah runs nothing while one is unfinished",
			},
		},
		{
			name:       "failed outside a transaction",
			repeatable: file(dataplane.RepeatableStateFailed, true),
			says:       []string{"names failed outside a transaction and may have left part of its work behind"},
		},
		{
			name:       "started and recorded no outcome",
			repeatable: file(dataplane.RepeatableStateRunning, false),
			says:       []string{"names recorded its start and no outcome, so whether its statements took effect is not known"},
		},
		{
			name: "failed, and no file carries it any more",
			repeatable: dataplane.MigrationRepeatable{
				Identity: "old_names", Orphaned: true, AppliedChecksum: repeatableContentA, State: dataplane.RepeatableStateFailed,
			},
			says:  []string{"no file of this artifact carries it", "(repair: `ptah migrations repair --repeatable old_names`)"},
			never: "--force",
		},
		{
			// Something other than Ptah wrote the row. Ptah's repair refuses
			// the identity, so the message quotes it and names no command.
			name: "failed, under an identity no file name carries",
			repeatable: dataplane.MigrationRepeatable{
				Identity: "old names\x1b[2J", Orphaned: true, AppliedChecksum: repeatableContentA, State: dataplane.RepeatableStateFailed,
			},
			says:  []string{`"old names\x1b[2J" failed inside a transaction`, "only deleting its row from schema_migrations_repeatable removes it"},
			never: "--repeatable old",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			report := pendingMigrationHistory()
			report.Repeatables = []dataplane.MigrationRepeatable{test.repeatable}
			actual, api := readRepeatableHistory(t, operatorv1alpha1.ApplyPolicyAlways, nil, report)
			assertMigrationBlockedFor(t, actual, operatorv1alpha1.ReasonRepeatableMigrationUnfinished)
			if actual.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked || actual.Status.Plan != nil || len(publishedPlans(t, api)) != 0 {
				t.Fatalf("phase %s with plan %#v, want Blocked and no plan", actual.Status.Phase, actual.Status.Plan)
			}
			blocked := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked)
			for _, want := range test.says {
				if !strings.Contains(blocked.Message, want) {
					t.Errorf("Blocked says %q, want it to carry %q", blocked.Message, want)
				}
			}
			if test.never != "" && strings.Contains(blocked.Message, test.never) {
				t.Errorf("Blocked says %q, which must not carry %q", blocked.Message, test.never)
			}
			if strings.Contains(blocked.Message, "\x1b") {
				t.Errorf("Blocked says %q, which carries a raw control character", blocked.Message)
			}
			if history := actual.Status.History; history == nil || !slices.Equal(history.UnfinishedRepeatables, []string{test.repeatable.Identity}) ||
				history.PendingCount != 1 {
				t.Fatalf("History = %#v, want the reading recorded with the unfinished record named", history)
			}
			if actual.Status.NextReconciliationTime == nil {
				t.Fatal("the refusal named no next reading")
			}
		})
	}
}

// orphanedRecord is an applied record of identity that no file carries.
func orphanedRecord(identity string) dataplane.MigrationRepeatable {
	return dataplane.MigrationRepeatable{
		Identity: identity, Orphaned: true, AppliedChecksum: repeatableContentA, State: dataplane.RepeatableStateOrphaned,
	}
}

// recordedEvents drains what recorder holds.
func recordedEvents(recorder *record.FakeRecorder) []string {
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

// A record that applied and that no file carries is not a refusal. Ptah
// leaves it alone and reports up to date, and the resource converges as it
// would without the record: no plan, no Blocked condition, Ready. The history
// names the record, and one Warning Event names the command that deletes it,
// without --force. The control is the same reading with the file restored,
// which converges with no Event.
func TestAnOrphanedRepeatableMigrationConverges(t *testing.T) {
	t.Parallel()

	for name, report := range map[string]dataplane.MigrationStatusReport{
		"orphaned": syncedHistoryWith(orphanedRecord("old_grants")),
		"file restored": syncedHistoryWith(dataplane.MigrationRepeatable{
			Identity: "old_grants", Path: "R__old_grants.sql", Checksum: repeatableContentA,
			AppliedChecksum: repeatableContentA, State: dataplane.RepeatableStateApplied,
		}),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			reconciler, api, migration := repeatableHistoryReader(t, operatorv1alpha1.ApplyPolicyAlways, nil, nil, report)
			recorder := record.NewFakeRecorder(16)
			reconciler.Recorder = recorder
			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			if actual.Status.Phase != operatorv1alpha1.MigrationPhaseInSync || len(publishedPlans(t, api)) != 0 ||
				!meta.IsStatusConditionTrue(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationReady) ||
				meta.IsStatusConditionTrue(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked) {
				t.Fatalf("phase %s with conditions %#v, want InSync, Ready and no plan", actual.Status.Phase, actual.Status.Conditions)
			}
			events := recordedEvents(recorder)
			if name == "file restored" {
				if history := actual.Status.History; history == nil || len(history.OrphanedRepeatables) != 0 || len(events) != 0 {
					t.Fatalf("the control lists %#v and emitted %q", history, events)
				}
				return
			}
			if history := actual.Status.History; history == nil || !slices.Equal(history.OrphanedRepeatables, []string{"old_grants"}) {
				t.Fatalf("History = %#v, want the orphaned record named", history)
			}
			if len(events) != 1 || !strings.HasPrefix(events[0], "Warning RepeatableMigrationOrphaned ") {
				t.Fatalf("events %q, want one Warning RepeatableMigrationOrphaned", events)
			}
			for _, want := range []string{
				"repeatable migration old_grants as applied",
				"`ptah migrations repair --repeatable old_grants`",
			} {
				if !strings.Contains(events[0], want) {
					t.Errorf("the Event says %q, want it to carry %q", events[0], want)
				}
			}
			if strings.Contains(events[0], "--force") {
				t.Errorf("the Event says %q; a record no file carries is repaired without --force", events[0])
			}
		})
	}
}

// A record read from the database may carry an identity no file name can.
// Ptah's repair refuses such an identity, so the Event quotes it, escaping
// what a terminal would act on, and names no repair command. The control is
// TestAnOrphanedRepeatableMigrationConverges, whose identity a file carries.
func TestAnOrphanedRecordNoFileNameCarriesNamesNoRepair(t *testing.T) {
	t.Parallel()

	const identity = "old names\x1b[2J"
	reconciler, api, migration := repeatableHistoryReader(t, operatorv1alpha1.ApplyPolicyAlways, nil, nil,
		syncedHistoryWith(orphanedRecord(identity)))
	recorder := record.NewFakeRecorder(16)
	reconciler.Recorder = recorder
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.Phase != operatorv1alpha1.MigrationPhaseInSync ||
		!slices.Equal(actual.Status.History.OrphanedRepeatables, []string{identity}) {
		t.Fatalf("phase %s with history %#v, want InSync and the record named", actual.Status.Phase, actual.Status.History)
	}
	events := recordedEvents(recorder)
	if len(events) != 1 {
		t.Fatalf("events %q, want one", events)
	}
	for _, want := range []string{`repeatable migration "old names\x1b[2J" as applied`, "deleting its row from schema_migrations_repeatable"} {
		if !strings.Contains(events[0], want) {
			t.Errorf("the Event says %q, want it to carry %q", events[0], want)
		}
	}
	if strings.Contains(events[0], "\x1b") || strings.Contains(events[0], "--repeatable old") {
		t.Errorf("the Event says %q, which carries the raw identity or a repair Ptah refuses", events[0])
	}
}

// The stored history is the record of what an Event has named, so a read that
// lists an orphaned record the history before it listed names nothing, a read
// that adds one names only that one, and pending work beside an orphaned
// record is planned as it would be without it. A database ahead of its
// artifact stops as HistoryAhead and lists and names no record as orphaned,
// since the artifact is old rather than the file gone.
func TestAnOrphanedRepeatableMigrationIsNamedOnce(t *testing.T) {
	t.Parallel()

	withPending := pendingMigrationHistory()
	withPending.Repeatables = []dataplane.MigrationRepeatable{orphanedRecord("old_grants")}
	// An artifact that ends at version 2 over a database at version 3: the
	// tag moved back, and the file is missing because the artifact is old.
	ahead := syncedHistoryWith(orphanedRecord("old_grants"))
	ahead.CurrentVersion = 3
	for _, test := range []struct {
		name     string
		previous []string
		report   dataplane.MigrationStatusReport
		named    []string
		phase    operatorv1alpha1.MigrationPhase
		unlisted bool
	}{
		{
			name: "listed by the read before", previous: []string{"old_grants"},
			report: syncedHistoryWith(orphanedRecord("old_grants")), phase: operatorv1alpha1.MigrationPhaseInSync,
		},
		{
			name: "one more than the read before", previous: []string{"old_grants"},
			report: syncedHistoryWith(orphanedRecord("old_grants"), orphanedRecord("old_totals")),
			named:  []string{"old_totals"}, phase: operatorv1alpha1.MigrationPhaseInSync,
		},
		{
			name: "back after a read that did not list it", previous: []string{"old_totals"},
			report: syncedHistoryWith(orphanedRecord("old_grants")),
			named:  []string{"old_grants"}, phase: operatorv1alpha1.MigrationPhaseInSync,
		},
		{
			name: "beside pending work", report: withPending,
			named: []string{"old_grants"}, phase: operatorv1alpha1.MigrationPhasePlanning,
		},
		{
			name: "in a database ahead of its artifact", report: ahead,
			phase: operatorv1alpha1.MigrationPhaseBlocked, unlisted: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var previous *operatorv1alpha1.MigrationHistoryStatus
			if test.previous != nil {
				previous = &operatorv1alpha1.MigrationHistoryStatus{OrphanedRepeatables: test.previous}
			}
			reconciler, api, migration := repeatableHistoryReader(t, operatorv1alpha1.ApplyPolicyAlways, nil, previous, test.report)
			recorder := record.NewFakeRecorder(16)
			reconciler.Recorder = recorder
			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			if actual.Status.Phase != test.phase {
				t.Fatalf("phase %s, want %s", actual.Status.Phase, test.phase)
			}
			if listed := actual.Status.History.OrphanedRepeatables; test.unlisted != (len(listed) == 0) {
				t.Fatalf("the history lists %q orphaned; listed %t, want %t", listed, len(listed) > 0, !test.unlisted)
			}
			if test.unlisted {
				assertMigrationBlockedFor(t, actual, operatorv1alpha1.ReasonHistoryAhead)
			}
			if test.phase == operatorv1alpha1.MigrationPhasePlanning {
				plans := publishedPlans(t, api)
				if len(plans) != 1 || len(plans[0].Spec.Repeatables) != 0 || len(plans[0].Spec.Migrations) != 1 {
					t.Fatalf("plans %#v, want one plan of the pending version alone", plans)
				}
			}
			var named []string
			for _, event := range recordedEvents(recorder) {
				if !strings.HasPrefix(event, "Warning RepeatableMigrationOrphaned ") {
					continue
				}
				for _, identity := range []string{"old_grants", "old_totals"} {
					if strings.Contains(event, "repeatable migration "+identity+" as applied") {
						named = append(named, identity)
					}
				}
			}
			if !slices.Equal(named, test.named) {
				t.Fatalf("the Events named %q, want %q", named, test.named)
			}
		})
	}
}

// The Event follows the status write that stored the history. A write that
// failed names nothing, and the pass after it, which stores the history,
// names the record once.
func TestAnOrphanedRepeatableMigrationIsNamedAfterTheWrite(t *testing.T) {
	t.Parallel()

	reconciler, api, migration := repeatableHistoryReader(t, operatorv1alpha1.ApplyPolicyAlways, nil, nil,
		syncedHistoryWith(orphanedRecord("old_grants")))
	recorder := record.NewFakeRecorder(16)
	reconciler.Recorder = recorder
	refused := false
	reconciler.Client = interceptor.NewClient(api, interceptor.Funcs{
		SubResourcePatch: func(
			ctx context.Context, inner client.Client, subResource string, object client.Object,
			patch client.Patch, options ...client.SubResourcePatchOption,
		) error {
			if _, ok := object.(*operatorv1alpha1.PtahMigration); ok && subResource == "status" && !refused {
				refused = true
				return apierrors.NewConflict(operatorv1alpha1.GroupVersion.WithResource("ptahmigrations").GroupResource(),
					object.GetName(), errors.New("the object has been modified"))
			}
			return inner.SubResource(subResource).Patch(ctx, object, patch, options...)
		},
	})
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err == nil || !refused {
		t.Fatalf("Reconcile() error = %v after the refused write %t, want the conflict", err, refused)
	}
	if events := recordedEvents(recorder); len(events) != 0 {
		t.Fatalf("a refused status write emitted %q", events)
	}
	reconciler.Client = api
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if history := actual.Status.History; history == nil || !slices.Equal(history.OrphanedRepeatables, []string{"old_grants"}) {
		t.Fatalf("History = %#v, want the orphaned record named", history)
	}
	if events := recordedEvents(recorder); len(events) != 1 {
		t.Fatalf("events %q, want one after the write", events)
	}
}

// A run nobody accounted for may have stopped after its versions and before
// its repeatable migrations, so one still pending is work the run may have
// half-done, and it does not settle the record. The control is the same
// database with the repeatable migration applied, which settles it.
func TestAPendingRepeatableMigrationDoesNotSettleAnUnresolvedRun(t *testing.T) {
	t.Parallel()

	unresolved := func() *operatorv1alpha1.UnresolvedMigrationRunStatus {
		return &operatorv1alpha1.UnresolvedMigrationRunStatus{
			Outcome: operatorv1alpha1.MigrationRunOutcomeUnknown, OperationID: "earlier-apply",
			TargetIdentityDigest: testDigest,
		}
	}
	pending := dataplane.MigrationRepeatable{
		Identity: "item_names", Path: "R__item_names.sql", Checksum: repeatableContentA, State: dataplane.RepeatableStatePending,
	}
	held, api := readRepeatableHistory(t, operatorv1alpha1.ApplyPolicyAlways, unresolved(), syncedHistoryWith(pending))
	assertMigrationBlockedFor(t, held, operatorv1alpha1.ReasonApplyOutcomeUnknown)
	assertMigrationCondition(t, held, operatorv1alpha1.ConditionMigrationBlocked,
		"1 repeatable migration is pending and the last run's outcome was Unknown, so it may not run again")
	if held.Status.UnresolvedRun == nil || len(publishedPlans(t, api)) != 0 {
		t.Fatalf("the record was settled or a plan published: %#v", held.Status.UnresolvedRun)
	}

	applied := pending
	applied.AppliedChecksum = repeatableContentA
	applied.State = dataplane.RepeatableStateApplied
	settled, _ := readRepeatableHistory(t, operatorv1alpha1.ApplyPolicyAlways, unresolved(), syncedHistoryWith(applied))
	if settled.Status.UnresolvedRun != nil || settled.Status.Phase != operatorv1alpha1.MigrationPhaseInSync {
		t.Fatalf("the control did not settle the record: phase %s, unresolved %#v", settled.Status.Phase, settled.Status.UnresolvedRun)
	}
	if settled.Status.ResolvedRun == nil || settled.Status.ResolvedRun.OperationID != "earlier-apply" {
		t.Fatalf("ResolvedRun = %#v, want the settled run", settled.Status.ResolvedRun)
	}
}

// The history counts each kind of repeatable record it names, within the
// bound the CRD gives the lists, and each identity once.
func TestRepeatableIdentitiesAreBoundedAndOnce(t *testing.T) {
	t.Parallel()

	var records []dataplane.MigrationRepeatable
	for range 3 {
		records = append(records, dataplane.MigrationRepeatable{Identity: "same"})
	}
	for index := range maxListedRepeatables + 5 {
		records = append(records, dataplane.MigrationRepeatable{Identity: "r" + strings.Repeat("x", index)})
	}
	listed := repeatableIdentities(records, maxListedRepeatables)
	once := slices.Compact(slices.Sorted(slices.Values(listed)))
	if len(listed) != maxListedRepeatables || listed[0] != "same" || len(once) != len(listed) {
		t.Fatalf("repeatableIdentities() = %d identities starting %q, want %d, each once", len(listed), listed[0], maxListedRepeatables)
	}
	if repeatableIdentities(nil, maxListedRepeatables) != nil {
		t.Fatal("no records listed something")
	}
}
