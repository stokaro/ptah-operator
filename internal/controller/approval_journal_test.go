package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

const (
	earlierApprovalUID = types.UID("earlier-approval-uid")
	earlierApprover    = "earlier-approver@example.test"
)

// earlierDecision is the journal a consumed approval left, with its object gone:
// nothing in the API server holds its UID any more. uid is empty for an entry a
// release before the field wrote. The journal is stamped by the wall clock and
// drops what is a month older than its newest entry, so this one is an hour old.
func earlierDecision(name string, uid types.UID) *operatorv1alpha1.LifecycleJournal {
	earlier := metav1.NewTime(time.Now().Add(-time.Hour))
	return &operatorv1alpha1.LifecycleJournal{
		CoveredSince: earlier,
		Entries: []operatorv1alpha1.JournalEntry{{
			Type: operatorv1alpha1.JournalApprovalAccepted, At: earlier,
			Plan: "an-earlier-plan", Approval: name, ApprovalUID: uid, Approver: earlierApprover,
		}},
	}
}

func acceptedEntries(journal *operatorv1alpha1.LifecycleJournal) []operatorv1alpha1.JournalEntry {
	var entries []operatorv1alpha1.JournalEntry
	if journal != nil {
		for _, entry := range journal.Entries {
			if entry.Type == operatorv1alpha1.JournalApprovalAccepted {
				entries = append(entries, entry)
			}
		}
	}
	return entries
}

// A new approval under the name of a consumed one is a new decision, though its
// predecessor's object is gone and only the journal remembers the name. The rows
// differ in what the earlier entry says: a UID, or nothing, as a release before
// the field wrote it. Neither is this approval.
func TestAReusedMigrationApprovalNameIsANewDecision(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		uid  types.UID
	}{
		{"an earlier entry with its UID", earlierApprovalUID},
		{"an earlier entry from before the field", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			migration, plan := awaitingApprovalFixture(t)
			approval := migrationApprovalFor(migration, plan)
			migration.Status.Journal = earlierDecision(approval.Name, row.uid)
			reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration, plan, approval, verificationPolicyConfigMap())

			for pass := range 3 {
				if _, err := reconciler.Reconcile(ctx, migrationRequest(migration)); err != nil {
					t.Fatalf("pass %d: Reconcile() error = %v", pass, err)
				}
				actual := readMigration(t, api, migration)
				if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.ApprovalRef == nil ||
					actual.Status.ActiveOperation.ApprovalRef.UID != approval.UID {
					t.Fatalf("pass %d: the claim does not name the new approval: %#v", pass, actual.Status.ActiveOperation)
				}
				entries := acceptedEntries(actual.Status.Journal)
				if len(entries) != 2 {
					t.Fatalf("pass %d: the journal holds %d decisions, want the earlier one and this one: %#v", pass, len(entries), entries)
				}
				if earlier := entries[0]; earlier.ApprovalUID != row.uid || earlier.Plan != "an-earlier-plan" || earlier.Approver != earlierApprover {
					t.Fatalf("pass %d: the earlier decision changed: %#v", pass, earlier)
				}
				current := entries[1]
				if current.Approval != approval.Name || current.ApprovalUID != approval.UID || current.Plan != plan.Name {
					t.Fatalf("pass %d: the new decision reads %#v, want approval %s UID %s on plan %s", pass, current, approval.Name, approval.UID, plan.Name)
				}
			}
		})
	}
}

// The same for a schema, whose status keeps the plan's approval and so names the
// approver: each decision keeps its own.
func TestAReusedSchemaApprovalNameIsANewDecision(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		uid  types.UID
	}{
		{"an earlier entry with its UID", earlierApprovalUID},
		{"an earlier entry from before the field", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			schema, plan, approval, policyConfig := safetyApprovalFixture(t)
			next := metav1.NewTime(time.Date(2026, 8, 30, 12, 4, 0, 0, time.UTC))
			schema.Status.NextReconciliationTime = &next
			schema.Status.Journal = earlierDecision(approval.Name, row.uid)
			approval.Spec.Approver.Username = "current-approver@example.test"
			reconciler, api := fakeReconciler(t, staticLogs{}, schema, plan, approval, policyConfig)

			for pass := range 2 {
				if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
					t.Fatalf("pass %d: Reconcile() error = %v", pass, err)
				}
				actual := safetyGetSchema(t, api, schema)
				if actual.Status.Plan == nil || actual.Status.Plan.Approval == nil || actual.Status.Plan.Approval.UID != approval.UID {
					t.Fatalf("pass %d: the plan does not record the new approval: %#v", pass, actual.Status.Plan)
				}
				entries := acceptedEntries(actual.Status.Journal)
				if len(entries) != 2 {
					t.Fatalf("pass %d: the journal holds %d decisions, want the earlier one and this one: %#v", pass, len(entries), entries)
				}
				if earlier := entries[0]; earlier.ApprovalUID != row.uid || earlier.Plan != "an-earlier-plan" || earlier.Approver != earlierApprover {
					t.Fatalf("pass %d: the earlier decision changed: %#v", pass, earlier)
				}
				current := entries[1]
				if current.Approval != approval.Name || current.ApprovalUID != approval.UID || current.Plan != plan.Name ||
					current.Approver != "current-approver@example.test" {
					t.Fatalf("pass %d: the new decision reads %#v, want %s UID %s on plan %s by its own approver", pass, current, approval.Name, approval.UID, plan.Name)
				}
			}
		})
	}
}
