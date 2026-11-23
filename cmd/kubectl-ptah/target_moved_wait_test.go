package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// storedSchemaReading is a schema document internal/controller holds to what the
// reconciler writes (TestTheTargetMovedReadingsAreWhatTheReconcilerStores there).
func storedSchemaReading(t *testing.T, name string) *operatorv1alpha1.PtahSchema {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "controller", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	schema := &operatorv1alpha1.PtahSchema{}
	if err := json.Unmarshal(data, schema); err != nil {
		t.Fatal(err)
	}
	schema.UID = "uid-app"
	return schema
}

// targetMovedStop is the reconciler's document of a proof whose read named
// another target: Blocked on TargetIdentityChanged, with the proof still owed and
// the claim between two attempts. settledProof is the one the read after the
// statement leaves: the proof given up, and the journal's Verified Unknown for
// its plan.
func targetMovedStop(t *testing.T) *operatorv1alpha1.PtahSchema {
	t.Helper()
	stop := storedSchemaReading(t, "schema-proof-target-moved.json")
	failed := meta.FindStatusCondition(stop.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
	if stop.Status.Phase != operatorv1alpha1.PhaseBlocked || stop.Status.PendingObservation == nil || stop.Status.ActiveOperation == nil ||
		failed == nil || failed.Status != metav1.ConditionTrue || failed.Reason != string(operatorv1alpha1.ReasonTargetIdentityChanged) {
		t.Fatalf("the reading is not a proof stopped on a moved target: phase %s, proof %#v, claim %#v, ReconciliationFailed %#v",
			stop.Status.Phase, stop.Status.PendingObservation, stop.Status.ActiveOperation, failed)
	}
	return stop
}

func settledProof(t *testing.T) *operatorv1alpha1.PtahSchema {
	t.Helper()
	settled := storedSchemaReading(t, "schema-proof-settled.json")
	journal := settled.Status.Journal
	if settled.Status.PendingObservation != nil || journal == nil || len(journal.Entries) == 0 {
		t.Fatalf("the reading is not a settled proof: proof %#v, journal %#v", settled.Status.PendingObservation, journal)
	}
	if last := journal.Entries[len(journal.Entries)-1]; last.Type != operatorv1alpha1.JournalVerified || last.Outcome != operatorv1alpha1.JournalUnknown {
		t.Fatalf("the settled reading's journal ends with %#v, want Verified Unknown", last)
	}
	return settled
}

// runCommandIn runs the plugin against api in namespace, the one the
// reconciler's documents name.
func runCommandIn(t *testing.T, api client.Client, namespace string, arguments ...string) (int, string, string) {
	t.Helper()
	previous := connector
	connector = func(string, string, string, time.Duration) (client.Client, string, error) {
		return api, namespace, nil
	}
	t.Cleanup(func() { connector = previous })
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), arguments, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// stokaro/ptah-operator#1175. A wait on a schema whose proof read another target
// keeps waiting: the stop waits for a person, and settle is the way out, so a
// wait for in-sync, applied or approval does not exit 8 with advice that does not
// apply to it. refresh --wait says the operator takes no read request while that
// stop holds, and settle --wait ends on the reconciler's document of the
// settlement. Every document is the reconciler's own.
func TestAWaitOnAProofWhoseTargetMovedKeepsWaitingAndSettleEndsIt(t *testing.T) {
	stop := targetMovedStop(t)
	ref := "schema/" + stop.Name
	for _, target := range []string{"in-sync", "applied", "approval"} {
		t.Run("wait --for "+target, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				api := newCluster(t, stop.DeepCopy())

				code, stdout, stderr := runCommandIn(t, api, stop.Namespace, "wait", ref, "--for", target, "--timeout", "1m")

				if code != exitTimedOut || stdout != "" || strings.Contains(stderr, "will not reach") {
					t.Fatalf("wait --for %s on a proof whose target moved = %d, stdout %q, stderr %q; want it to keep waiting",
						target, code, stdout, stderr)
				}
				requireSays(t, "the timeout", stderr, "did not reach "+target+" within 1m0s.", "Phase:       Blocked",
					"Held by:     ReconciliationFailed=True, TargetIdentityChanged: ")
			})
		})
	}
	t.Run("refresh --wait", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			api := newCluster(t, stop.DeepCopy())

			code, stdout, stderr := runCommandIn(t, api, stop.Namespace, "refresh", ref, "--wait", "--wait-timeout", "1m")

			if code != exitTimedOut || strings.Contains(stderr, "ended Blocked") {
				t.Fatalf("refresh --wait on a proof whose target moved = %d, stdout %q, stderr %q", code, stdout, stderr)
			}
			requireSays(t, "refresh's report", stdout, "The resource is Blocked (TargetIdentityChanged): the operator checks "+
				"that before it reaches a read request, so it takes none until the block ends.")
			requireSays(t, "the timeout", stderr, "did not reach read within 1m0s.",
				"neither its engine, its database realm nor a proof whose target moved blocks it")
		})
	})
	t.Run("settle --wait", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			settled := settledProof(t)
			api := newCluster(t, stop.DeepCopy())
			api.then(func(object client.Object) {
				object.(*operatorv1alpha1.PtahSchema).Status = *settled.Status.DeepCopy()
			})

			code, stdout, stderr := runCommandIn(t, api, stop.Namespace, "settle", ref, "--wait", "--wait-timeout", "1m")

			if code != exitOK {
				t.Fatalf("settle --wait on a proof whose target moved = %d, stdout %q, stderr %q", code, stdout, stderr)
			}
			requireSays(t, "settle --wait's report", stdout, "Settlement requested for", "reached settled.",
				"The operator gave the proof up, and status.applied records no Apply.")
			if held := settledAnnotationOf(t, api, stop); !strings.HasSuffix(failedMessage(stop), held+" --overwrite") {
				t.Fatalf("settle wrote %q, which the stop does not print", held)
			}
		})
	})
}

func failedMessage(schema *operatorv1alpha1.PtahSchema) string {
	return meta.FindStatusCondition(schema.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed).Message
}

func settledAnnotationOf(t *testing.T, api *cluster, schema *operatorv1alpha1.PtahSchema) string {
	t.Helper()
	stored := &operatorv1alpha1.PtahSchema{}
	if err := api.Client.Get(context.Background(), client.ObjectKeyFromObject(schema), stored); err != nil {
		t.Fatal(err)
	}
	held, ok := stored.Annotations[operatorv1alpha1.SettledApplyAnnotation]
	if !ok || held == "" {
		t.Fatalf("settle wrote no %s annotation: %#v", operatorv1alpha1.SettledApplyAnnotation, stored.Annotations)
	}
	return operatorv1alpha1.SettledApplyAnnotation + "=" + held
}
