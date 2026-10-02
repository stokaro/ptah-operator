package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The job and epoch the migration filter self-test judged the
// isolated-node filters with.
const (
	miJob   = "u-isolated-apply"
	miEpoch = "v1-isolated-epoch"
)

// miReading is one document a filter is judged on, and whether it has to
// accept it.
type miReading struct {
	name     string
	document string
	accept   bool
}

// miMigration decodes a reading the way the API server's document reads, and
// refuses a field the type does not declare, so a fixture cannot quietly name
// something the predicate never sees.
func miMigration(t *testing.T, document string) *ptahv1alpha1.PtahMigration {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(document))
	decoder.DisallowUnknownFields()
	migration := &ptahv1alpha1.PtahMigration{}
	if err := decoder.Decode(migration); err != nil {
		t.Fatalf("decode the reading: %v", err)
	}
	return migration
}

func miJudge(t *testing.T, readings []miReading, judge func(*ptahv1alpha1.PtahMigration) bool) {
	t.Helper()
	for _, reading := range readings {
		t.Run(reading.name, func(t *testing.T) {
			t.Parallel()
			if got := judge(miMigration(t, reading.document)); got != reading.accept {
				t.Fatalf("judged %t, want %t", got, reading.accept)
			}
		})
	}
}

// The readings below are the self-test's own, byte for byte, and then the
// clauses it left without a refusal of their own.

func TestIsolatedApplyHeld(t *testing.T) {
	t.Parallel()
	miJudge(t, []miReading{
		{"the claim, renewed and waiting on its Job", `{"metadata":{"finalizers":["operator.ptah.run/migration-operation"]},
 "status":{"phase":"Applying",
 "activeOperation":{"type":"Apply","jobName":"ptah-m-apply-a","jobUID":"u-isolated-apply",
  "startedAt":"2026-09-26T10:00:00Z","leaseEpoch":"v1-isolated-epoch","leaseDurationSeconds":300,
  "dispatchStarted":true},
 "lastRun":{"outcome":"UpToDate","jobUID":"u-earlier"},
 "conditions":[{"type":"Progressing","status":"True","reason":"ApprovedPlan"}]}}`, true},
		{"the claim retired and the run recorded", `{"metadata":{"finalizers":["operator.ptah.run/migration-operation"]},
 "status":{"phase":"Blocked",
 "lastRun":{"outcome":"Unknown","jobUID":"u-isolated-apply"},
 "unresolvedRun":{"outcome":"Unknown","jobUID":"u-isolated-apply"}}}`, false},
		{"a claim naming a second Job", `{"metadata":{"finalizers":["operator.ptah.run/migration-operation"]},
 "status":{"activeOperation":{"type":"Apply","jobUID":"u-replacement","leaseEpoch":"v1-isolated-epoch"}}}`, false},
		{"a claim of another operation", `{"metadata":{"finalizers":["operator.ptah.run/migration-operation"]},
 "status":{"activeOperation":{"type":"Resolve","jobUID":"u-isolated-apply","leaseEpoch":"v1-isolated-epoch"}}}`, false},
		{"the realm taken again under another epoch", `{"metadata":{"finalizers":["operator.ptah.run/migration-operation"]},
 "status":{"activeOperation":{"type":"Apply","jobUID":"u-isolated-apply","leaseEpoch":"v1-another-epoch"}}}`, false},
		{"continuity lost under the claim", `{"metadata":{"finalizers":["operator.ptah.run/migration-operation"]},
 "status":{"activeOperation":{"type":"Apply","jobUID":"u-isolated-apply","leaseEpoch":"v1-isolated-epoch",
  "leaseContinuityLost":true}}}`, false},
		{"a release owed while the claim stands", `{"metadata":{"finalizers":["operator.ptah.run/migration-operation"]},
 "status":{"activeOperation":{"type":"Apply","jobUID":"u-isolated-apply","leaseEpoch":"v1-isolated-epoch"},
 "pendingLockRelease":{"leaseEpoch":"v1-isolated-epoch"}}}`, false},
		{"the run recorded while the claim stands", `{"metadata":{"finalizers":["operator.ptah.run/migration-operation"]},
 "status":{"activeOperation":{"type":"Apply","jobUID":"u-isolated-apply","leaseEpoch":"v1-isolated-epoch"},
 "lastRun":{"outcome":"Unknown","jobUID":"u-isolated-apply"}}}`, false},
		{"an unresolved record written early", `{"metadata":{"finalizers":["operator.ptah.run/migration-operation"]},
 "status":{"activeOperation":{"type":"Apply","jobUID":"u-isolated-apply","leaseEpoch":"v1-isolated-epoch"},
 "unresolvedRun":{"outcome":"Unknown","jobUID":"u-isolated-apply"}}}`, false},
		{"the finalizer released", `{"metadata":{"finalizers":[]},
 "status":{"activeOperation":{"type":"Apply","jobUID":"u-isolated-apply","leaseEpoch":"v1-isolated-epoch"}}}`, false},
		{"held with no last run at all", `{"metadata":{"finalizers":["operator.ptah.run/migration-operation"]},
 "status":{"activeOperation":{"type":"Apply","jobUID":"u-isolated-apply","leaseEpoch":"v1-isolated-epoch"}}}`, true},
		{"only another controller's finalizer", `{"metadata":{"finalizers":["example.com/other"]},
 "status":{"activeOperation":{"type":"Apply","jobUID":"u-isolated-apply","leaseEpoch":"v1-isolated-epoch"}}}`, false},
		{"no finalizers at all", `{"metadata":{},
 "status":{"activeOperation":{"type":"Apply","jobUID":"u-isolated-apply","leaseEpoch":"v1-isolated-epoch"}}}`, false},
		{"a claim with no epoch", `{"metadata":{"finalizers":["operator.ptah.run/migration-operation"]},
 "status":{"activeOperation":{"type":"Apply","jobUID":"u-isolated-apply"}}}`, false},
	}, func(migration *ptahv1alpha1.PtahMigration) bool {
		return isolatedApplyHeld(migration, miJob, miEpoch)
	})

	// jq's null equals no string, the empty one included: a claim that names no
	// Job is not held for a Job nobody named.
	held := miMigration(t, `{"metadata":{"finalizers":["operator.ptah.run/migration-operation"]},
 "status":{"activeOperation":{"type":"Apply"}}}`)
	if isolatedApplyHeld(held, "", "") {
		t.Fatal("a claim naming no Job and no epoch was held for empty ones")
	}
}

func TestIsolatedRunUnknown(t *testing.T) {
	t.Parallel()
	miJudge(t, []miReading{
		{"recorded unknown against its own Job", `{"status":{"phase":"Blocked",
 "lastRun":{"outcome":"Unknown","jobName":"ptah-m-apply-a","jobUID":"u-isolated-apply",
  "finishedAt":"2026-09-26T10:06:00Z","message":"read the Apply result: ptah runner result frame not found"},
 "unresolvedRun":{"outcome":"Unknown","jobName":"ptah-m-apply-a","jobUID":"u-isolated-apply",
  "recordedAt":"2026-09-26T10:06:00Z"},
 "conditions":[{"type":"Blocked","status":"True","reason":"ApplyOutcomeUnknown"},
               {"type":"Ready","status":"False","reason":"ApplyOutcomeUnknown"}]}}`, true},
		{"read as applied", `{"status":{"phase":"VerifyingHistory",
 "lastRun":{"outcome":"Applied","jobUID":"u-isolated-apply"},
 "conditions":[{"type":"Progressing","status":"True","reason":"VerifyingConvergence"}]}}`, false},
		{"the run recorded partial", `{"status":{"lastRun":{"outcome":"Partial","jobUID":"u-isolated-apply"},
 "unresolvedRun":{"outcome":"Unknown","jobUID":"u-isolated-apply"},
 "conditions":[{"type":"Blocked","status":"True","reason":"ApplyOutcomeUnknown"}]}}`, false},
		{"the record naming another outcome", `{"status":{"lastRun":{"outcome":"Unknown","jobUID":"u-isolated-apply"},
 "unresolvedRun":{"outcome":"Partial","jobUID":"u-isolated-apply"},
 "conditions":[{"type":"Blocked","status":"True","reason":"ApplyOutcomeUnknown"}]}}`, false},
		{"the last run naming another Job", `{"status":{"lastRun":{"outcome":"Unknown","jobUID":"u-replacement"},
 "unresolvedRun":{"outcome":"Unknown","jobUID":"u-isolated-apply"},
 "conditions":[{"type":"Blocked","status":"True","reason":"ApplyOutcomeUnknown"}]}}`, false},
		{"the record already gone", `{"status":{"phase":"InSync",
 "lastRun":{"outcome":"Unknown","jobUID":"u-isolated-apply"},
 "conditions":[{"type":"Blocked","status":"False","reason":"HistoryMatched"}]}}`, false},
		{"a record of some other run", `{"status":{"lastRun":{"outcome":"Unknown","jobUID":"u-isolated-apply"},
 "unresolvedRun":{"outcome":"Unknown","jobUID":"u-earlier"},
 "conditions":[{"type":"Blocked","status":"True","reason":"ApplyOutcomeUnknown"}]}}`, false},
		{"the run of another Job", `{"status":{"lastRun":{"outcome":"Unknown","jobUID":"u-replacement"},
 "unresolvedRun":{"outcome":"Unknown","jobUID":"u-replacement"},
 "conditions":[{"type":"Blocked","status":"True","reason":"ApplyOutcomeUnknown"}]}}`, false},
		{"a claim still standing", `{"status":{"activeOperation":{"type":"Apply","jobUID":"u-isolated-apply"},
 "lastRun":{"outcome":"Unknown","jobUID":"u-isolated-apply"},
 "unresolvedRun":{"outcome":"Unknown","jobUID":"u-isolated-apply"},
 "conditions":[{"type":"Blocked","status":"True","reason":"ApplyOutcomeUnknown"}]}}`, false},
		{"blocked for another reason", `{"status":{"lastRun":{"outcome":"Unknown","jobUID":"u-isolated-apply"},
 "unresolvedRun":{"outcome":"Unknown","jobUID":"u-isolated-apply"},
 "conditions":[{"type":"Blocked","status":"True","reason":"RealmConflict"}]}}`, false},
		{"the reason on a Blocked that is not true", `{"status":{"lastRun":{"outcome":"Unknown","jobUID":"u-isolated-apply"},
 "unresolvedRun":{"outcome":"Unknown","jobUID":"u-isolated-apply"},
 "conditions":[{"type":"Blocked","status":"False","reason":"ApplyOutcomeUnknown"}]}}`, false},
		{"the reason on another condition", `{"status":{"lastRun":{"outcome":"Unknown","jobUID":"u-isolated-apply"},
 "unresolvedRun":{"outcome":"Unknown","jobUID":"u-isolated-apply"},
 "conditions":[{"type":"Ready","status":"True","reason":"ApplyOutcomeUnknown"}]}}`, false},
		{"no conditions", `{"status":{"lastRun":{"outcome":"Unknown","jobUID":"u-isolated-apply"},
 "unresolvedRun":{"outcome":"Unknown","jobUID":"u-isolated-apply"}}}`, false},
		{"no last run", `{"status":{"unresolvedRun":{"outcome":"Unknown","jobUID":"u-isolated-apply"},
 "conditions":[{"type":"Blocked","status":"True","reason":"ApplyOutcomeUnknown"}]}}`, false},
	}, func(migration *ptahv1alpha1.PtahMigration) bool {
		return isolatedRunUnknown(migration.Status, miJob)
	})
}

func TestIsolatedRunSettled(t *testing.T) {
	t.Parallel()
	miJudge(t, []miReading{
		{"settled by a later reading with nothing pending", `{"status":{"phase":"InSync",
 "lastRun":{"outcome":"Unknown","jobUID":"u-isolated-apply","finishedAt":"2026-09-26T10:06:00Z"},
 "history":{"observedAt":"2026-09-26T10:08:31Z","pendingCount":0,"appliedCount":3},
 "conditions":[{"type":"Ready","status":"True","reason":"HistoryMatched"}]}}`, true},
		{"timestamps written with fractions", `{"status":{"lastRun":{"outcome":"Unknown","jobUID":"u-isolated-apply","finishedAt":"2026-09-26T10:06:00.250Z"},
 "history":{"observedAt":"2026-09-26T10:06:01.100Z","pendingCount":0}}}`, true},
		{"the record still standing", `{"status":{"lastRun":{"outcome":"Unknown","jobUID":"u-isolated-apply","finishedAt":"2026-09-26T10:06:00Z"},
 "unresolvedRun":{"outcome":"Unknown","jobUID":"u-isolated-apply"},
 "history":{"observedAt":"2026-09-26T10:08:31Z","pendingCount":0}}}`, false},
		{"a reading taken before the run finished", `{"status":{"lastRun":{"outcome":"Unknown","jobUID":"u-isolated-apply","finishedAt":"2026-09-26T10:06:00Z"},
 "history":{"observedAt":"2026-09-26T09:59:00Z","pendingCount":0}}}`, false},
		{"a reading stamped with the run", `{"status":{"lastRun":{"outcome":"Unknown","jobUID":"u-isolated-apply","finishedAt":"2026-09-26T10:06:00Z"},
 "history":{"observedAt":"2026-09-26T10:06:00Z","pendingCount":0}}}`, false},
		{"work still pending", `{"status":{"lastRun":{"outcome":"Unknown","jobUID":"u-isolated-apply","finishedAt":"2026-09-26T10:06:00Z"},
 "history":{"observedAt":"2026-09-26T10:08:31Z","pendingCount":1}}}`, false},
		{"a later run recorded over this one", `{"status":{"lastRun":{"outcome":"Unknown","jobUID":"u-replacement","finishedAt":"2026-09-26T10:09:00Z"},
 "history":{"observedAt":"2026-09-26T10:10:00Z","pendingCount":0}}}`, false},
		{"the run rewritten as applied", `{"status":{"lastRun":{"outcome":"Applied","jobUID":"u-isolated-apply","finishedAt":"2026-09-26T10:06:00Z"},
 "history":{"observedAt":"2026-09-26T10:08:31Z","pendingCount":0}}}`, false},
		{"a new claim taken", `{"status":{"activeOperation":{"type":"Apply","jobUID":"u-replacement"},
 "lastRun":{"outcome":"Unknown","jobUID":"u-isolated-apply","finishedAt":"2026-09-26T10:06:00Z"},
 "history":{"observedAt":"2026-09-26T10:08:31Z","pendingCount":0}}}`, false},
		// jq dropped the fraction, so a reading later only within the second is
		// stamped with the run.
		{"a reading later only by a fraction", `{"status":{"lastRun":{"outcome":"Unknown","jobUID":"u-isolated-apply","finishedAt":"2026-09-26T10:06:00.100Z"},
 "history":{"observedAt":"2026-09-26T10:06:00.900Z","pendingCount":0}}}`, false},
		{"no reading at all", `{"status":{"lastRun":{"outcome":"Unknown","jobUID":"u-isolated-apply","finishedAt":"2026-09-26T10:06:00Z"}}}`, false},
		{"a run with no end", `{"status":{"lastRun":{"outcome":"Unknown","jobUID":"u-isolated-apply"},
 "history":{"observedAt":"2026-09-26T10:08:31Z","pendingCount":0}}}`, false},
		{"no run at all", `{"status":{"history":{"observedAt":"2026-09-26T10:08:31Z","pendingCount":0}}}`, false},
	}, func(migration *ptahv1alpha1.PtahMigration) bool {
		return isolatedRunSettled(migration.Status, miJob)
	})
}

func TestRefusedAtBoundary(t *testing.T) {
	t.Parallel()
	miJudge(t, []miReading{
		{"the fetch step ended the run", `{"status":{"conditions":[{"type":"Progressing","status":"True","reason":"OperationFailed",
 "message":"the fetch-migrations step exited 2, so the run never started"}]}}`, true},
		{"a missing frame, naming no boundary", `{"status":{"conditions":[{"type":"Progressing","status":"True","reason":"OperationFailed",
 "message":"read History result: ptah runner result frame not found"}]}}`, false},
		{"a different step failed", `{"status":{"conditions":[{"type":"Progressing","status":"True","reason":"OperationFailed",
 "message":"the install-runner step exited 1, so the run never started"}]}}`, false},
		{"the boundary named on another condition", `{"status":{"conditions":[{"type":"Ready","status":"False","reason":"OperationFailed",
 "message":"the fetch-migrations step exited 2, so the run never started"}]}}`, false},
		{"no conditions", `{"status":{}}`, false},
	}, func(migration *ptahv1alpha1.PtahMigration) bool {
		return refusedAtBoundary(migration.Status, "fetch-migrations")
	})
	if refusedAtBoundary(miMigration(t, `{"status":{"conditions":[{"type":"Progressing","message":"a (step"}]}}`).Status, "(") {
		t.Fatal("a step that is not a pattern matched a message")
	}
}

func TestActedOnNothing(t *testing.T) {
	t.Parallel()
	miJudge(t, []miReading{
		{"refused before anything was planned", `{"status":{"phase":"Reading","history":{"appliedCount":0},
 "conditions":[{"type":"Progressing","status":"True","reason":"OperationFailed",
 "message":"the fetch-migrations step exited 2, so the run never started"}]}}`, true},
		{"a plan was published", `{"status":{"phase":"Reading","plan":{"name":"ptah-mplan-0","uid":"u"},"history":{"appliedCount":0}}}`, false},
		{"a run was recorded", `{"status":{"phase":"Reading","lastRun":{"outcome":"Failed"},"history":{"appliedCount":0}}}`, false},
		{"it reached the approval gate", `{"status":{"phase":"AwaitingApproval","history":{"appliedCount":0}}}`, false},
		{"something was applied", `{"status":{"phase":"Reading","history":{"appliedCount":1}}}`, false},
		{"it reached InSync", `{"status":{"phase":"InSync","history":{"appliedCount":0}}}`, false},
		// Every clause is an absence, so a resource with no status yet holds
		// all of them, as it did for jq.
		{"no status yet", `{}`, true},
	}, func(migration *ptahv1alpha1.PtahMigration) bool {
		return actedOnNothing(migration.Status)
	})
}

// miWorker is the isolation worker as hack/e2e-kind.sh provisions it.
func miWorker() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "ptah-e2e-worker2", Labels: map[string]string{isolationNodeKey: "true"}},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{
			{Key: "example.com/other", Value: "x", Effect: corev1.TaintEffectNoExecute},
			{Key: isolationNodeKey, Value: "true", Effect: corev1.TaintEffectNoSchedule},
		}},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse},
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
		}},
	}
}

func TestIsolationWorkerReady(t *testing.T) {
	t.Parallel()
	if !isolationWorkerReady(miWorker()) {
		t.Fatal("the isolation worker was refused")
	}
	for name, mutate := range map[string]func(*corev1.Node){
		"unlabeled":          func(n *corev1.Node) { n.Labels = nil },
		"labeled false":      func(n *corev1.Node) { n.Labels[isolationNodeKey] = "false" },
		"untainted":          func(n *corev1.Node) { n.Spec.Taints = n.Spec.Taints[:1] },
		"tainted to execute": func(n *corev1.Node) { n.Spec.Taints[1].Effect = corev1.TaintEffectNoExecute },
		"tainted with false": func(n *corev1.Node) { n.Spec.Taints[1].Value = "false" },
		"tainted twice": func(n *corev1.Node) {
			n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{Key: isolationNodeKey, Value: "true", Effect: corev1.TaintEffectPreferNoSchedule})
		},
		"a taint with a time": func(n *corev1.Node) { n.Spec.Taints[1].TimeAdded = &metav1.Time{Time: time.Unix(1, 0)} },
		"not ready":           func(n *corev1.Node) { n.Status.Conditions[1].Status = corev1.ConditionUnknown },
		"no conditions":       func(n *corev1.Node) { n.Status.Conditions = nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			node := miWorker()
			mutate(node)
			if isolationWorkerReady(node) {
				t.Fatalf("a worker %s was accepted", name)
			}
		})
	}
}

func TestNodeReadiness(t *testing.T) {
	t.Parallel()
	node := miWorker()
	if !miNodeReady(node) || miNodeNotReady(node) {
		t.Fatal("a Ready node read as lost")
	}
	node.Status.Conditions[1].Status = corev1.ConditionUnknown
	if miNodeReady(node) || !miNodeNotReady(node) {
		t.Fatal("a node gone Unknown read as Ready")
	}
	node.Status.Conditions = nil
	if miNodeReady(node) || miNodeNotReady(node) {
		t.Fatal("a node with no Ready condition read as either")
	}
}

func TestIsolationRuleCount(t *testing.T) {
	t.Parallel()
	rules := []byte(`-P INPUT ACCEPT
-A INPUT -p tcp -m tcp --dport 10250 -m comment --comment ptah-e2e-isolated-node -j DROP
-A OUTPUT -p tcp -m tcp --dport 6443 -m comment --comment ptah-e2e-isolated-node -j DROP
-A KUBE-FORWARD -m comment --comment "kubernetes forwarding rules" -j ACCEPT
`)
	if got := isolationRuleCount(rules); got != 2 {
		t.Fatalf("counted %d rules, want 2", got)
	}
	if got := isolationRuleCount([]byte("-P INPUT ACCEPT\n")); got != 0 {
		t.Fatalf("counted %d rules on a clean node", got)
	}
	if got := isolationRuleCount(nil); got != 0 {
		t.Fatalf("counted %d rules in nothing", got)
	}
}

func TestIsolatedApplyClaim(t *testing.T) {
	t.Parallel()
	claimed := func() *ptahv1alpha1.PtahMigration {
		return &ptahv1alpha1.PtahMigration{Status: ptahv1alpha1.PtahMigrationStatus{
			ActiveOperation: &ptahv1alpha1.MigrationOperationStatus{
				Type: ptahv1alpha1.MigrationOperationApply, JobName: "apply-a", JobUID: miJob,
				LeaseEpoch: miEpoch, LeaseDurationSeconds: 30,
			},
		}}
	}
	claim, ok := isolatedApplyClaim(claimed())
	if !ok || claim != (isolatedClaim{job: "apply-a", jobUID: miJob, epoch: miEpoch, duration: 30}) {
		t.Fatalf("the claim was read as %+v %t", claim, ok)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.MigrationOperationStatus){
		"a History":   func(o *ptahv1alpha1.MigrationOperationStatus) { o.Type = ptahv1alpha1.MigrationOperationHistory },
		"no Job name": func(o *ptahv1alpha1.MigrationOperationStatus) { o.JobName = "" },
		"no Job UID":  func(o *ptahv1alpha1.MigrationOperationStatus) { o.JobUID = "" },
		"no epoch":    func(o *ptahv1alpha1.MigrationOperationStatus) { o.LeaseEpoch = "" },
		"no duration": func(o *ptahv1alpha1.MigrationOperationStatus) { o.LeaseDurationSeconds = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			migration := claimed()
			mutate(migration.Status.ActiveOperation)
			if _, ok := isolatedApplyClaim(migration); ok {
				t.Fatalf("a claim with %s was accepted", name)
			}
		})
	}
	if _, ok := isolatedApplyClaim(&ptahv1alpha1.PtahMigration{}); ok {
		t.Fatal("no claim was accepted")
	}
}

func miLease(name, epoch string, duration int32, acquired time.Time) coordinationv1.Lease {
	lease := coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ptah-system", Name: name},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity: ptr.To("manager-0"), LeaseDurationSeconds: ptr.To(duration),
			AcquireTime: &metav1.MicroTime{Time: acquired},
		},
	}
	if epoch != "" {
		lease.Annotations = map[string]string{annotationLeaseEpoch: epoch}
	}
	return lease
}

func TestIsolatedLease(t *testing.T) {
	t.Parallel()
	acquired := time.Date(2026, 9, 26, 10, 0, 0, 750_000_000, time.UTC)
	leases := func() []coordinationv1.Lease {
		return []coordinationv1.Lease{
			miLease("ptah-realm-other", "v1-other-epoch", 30, acquired),
			miLease("ptah-realm-isolated", miEpoch, 30, acquired),
			miLease("kube-scheduler", "", 15, acquired),
		}
	}
	lease, err := isolatedLease(leases(), miEpoch, 30)
	if err != nil || lease.Name != "ptah-realm-isolated" {
		t.Fatalf("the realm Lease was not found: %v", err)
	}
	// The acquisition without its fraction, plus the duration.
	if end, ok := leaseTermEnd(lease); !ok || end != time.Date(2026, 9, 26, 10, 0, 30, 0, time.UTC).Unix() {
		t.Fatalf("the term was read as %d %t", end, ok)
	}
	for name, mutate := range map[string]func([]coordinationv1.Lease) []coordinationv1.Lease{
		"no Lease at the epoch": func(l []coordinationv1.Lease) []coordinationv1.Lease { return l[:1] },
		"two at the epoch": func(l []coordinationv1.Lease) []coordinationv1.Lease {
			return append(l, miLease("ptah-realm-twin", miEpoch, 30, acquired))
		},
		"another duration": func(l []coordinationv1.Lease) []coordinationv1.Lease {
			l[1].Spec.LeaseDurationSeconds = ptr.To[int32](60)
			return l
		},
		"no duration": func(l []coordinationv1.Lease) []coordinationv1.Lease {
			l[1].Spec.LeaseDurationSeconds = nil
			return l
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := isolatedLease(mutate(leases()), miEpoch, 30); err == nil {
				t.Fatalf("a Lease list with %s was accepted", name)
			}
		})
	}
	unacquired := miLease("ptah-realm-isolated", miEpoch, 30, acquired)
	unacquired.Spec.AcquireTime = nil
	if _, ok := leaseTermEnd(&unacquired); ok {
		t.Fatal("a Lease with no acquisition had a term")
	}
}

func TestLeaseRenewedSince(t *testing.T) {
	t.Parallel()
	isolatedAt := time.Date(2026, 9, 26, 10, 0, 5, 0, time.UTC).Unix()
	renewed := func(at time.Time) *coordinationv1.Lease {
		lease := miLease("ptah-realm-isolated", miEpoch, 30, at)
		lease.Spec.RenewTime = &metav1.MicroTime{Time: at}
		return &lease
	}
	later := time.Date(2026, 9, 26, 10, 3, 0, 0, time.UTC)
	if !leaseRenewedSince(renewed(later), isolatedAt, later.Unix()+29) {
		t.Fatal("a Lease renewed after the isolation was refused")
	}
	if leaseRenewedSince(renewed(later), isolatedAt, later.Unix()+30) {
		t.Fatal("a Lease past the duration of its last renewal was accepted")
	}
	// The renewal is read to the second, as jq dropped the fraction.
	if leaseRenewedSince(renewed(time.Date(2026, 9, 26, 10, 0, 5, 900_000_000, time.UTC)), isolatedAt, isolatedAt+1) {
		t.Fatal("a renewal in the second of the isolation was read as after it")
	}
	unrenewed := renewed(later)
	unrenewed.Spec.RenewTime = nil
	if leaseRenewedSince(unrenewed, isolatedAt, later.Unix()) {
		t.Fatal("a Lease with no renewal was accepted")
	}
}

func miPod(name, uid, node string, phase corev1.PodPhase) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ptah-e2e", Name: name, UID: types.UID(uid),
			Labels: map[string]string{labelMigration: "e2e-isolated-node-postgresql"},
		},
		Spec:   corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func TestIsolatedApplyPod(t *testing.T) {
	t.Parallel()
	running := []corev1.Pod{miPod("apply-a-x", "u-pod", "ptah-e2e-worker2", corev1.PodRunning)}
	if !isolatedApplyPodRunning(running, "ptah-e2e-worker2") {
		t.Fatal("the Apply Pod running on the worker was refused")
	}
	for name, pods := range map[string][]corev1.Pod{
		"no Pod":         nil,
		"two Pods":       append(slices.Clone(running), miPod("apply-a-y", "u-pod-2", "ptah-e2e-worker2", corev1.PodRunning)),
		"another node":   {miPod("apply-a-x", "u-pod", "ptah-e2e-worker", corev1.PodRunning)},
		"still pending":  {miPod("apply-a-x", "u-pod", "ptah-e2e-worker2", corev1.PodPending)},
		"not yet placed": {miPod("apply-a-x", "u-pod", "", corev1.PodPending)},
	} {
		if isolatedApplyPodRunning(pods, "ptah-e2e-worker2") {
			t.Errorf("%s was accepted", name)
		}
	}
	if podPlacedElsewhere([]corev1.Pod{miPod("apply-a-x", "u-pod", "", corev1.PodPending)}, "ptah-e2e-worker2") ||
		podPlacedElsewhere(running, "ptah-e2e-worker2") {
		t.Fatal("a Pod not yet placed, or placed on the worker, read as misplaced")
	}
	if !podPlacedElsewhere([]corev1.Pod{miPod("apply-a-x", "u-pod", "ptah-e2e-worker", corev1.PodPending)}, "ptah-e2e-worker2") {
		t.Fatal("a Pod on another node was not noticed")
	}
}

func TestUnreachableTolerationSeconds(t *testing.T) {
	t.Parallel()
	tolerating := func() *corev1.Pod {
		return &corev1.Pod{Spec: corev1.PodSpec{Tolerations: []corev1.Toleration{
			{Key: isolationNodeKey, Operator: corev1.TolerationOpEqual, Value: "true", Effect: corev1.TaintEffectNoSchedule},
			{Key: corev1.TaintNodeUnreachable, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr.To[int64](30)},
			{Key: corev1.TaintNodeNotReady, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr.To[int64](300)},
		}}}
	}
	if seconds, ok := unreachableTolerationSeconds(tolerating()); !ok || seconds != 30 {
		t.Fatalf("the toleration was read as %d %t", seconds, ok)
	}
	for name, mutate := range map[string]func(*corev1.Pod){
		"none":      func(p *corev1.Pod) { p.Spec.Tolerations = p.Spec.Tolerations[:1] },
		"unbounded": func(p *corev1.Pod) { p.Spec.Tolerations[1].TolerationSeconds = nil },
		"zero":      func(p *corev1.Pod) { p.Spec.Tolerations[1].TolerationSeconds = ptr.To[int64](0) },
		"two": func(p *corev1.Pod) {
			p.Spec.Tolerations = append(p.Spec.Tolerations, p.Spec.Tolerations[1])
		},
		"for scheduling only": func(p *corev1.Pod) { p.Spec.Tolerations[1].Effect = corev1.TaintEffectNoSchedule },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pod := tolerating()
			mutate(pod)
			if _, ok := unreachableTolerationSeconds(pod); ok {
				t.Fatalf("a Pod whose toleration is %s was accepted", name)
			}
		})
	}
}

func TestJobDeadlineAt(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	job := func() *batchv1.Job {
		return &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{UID: miJob},
			Spec:       batchv1.JobSpec{ActiveDeadlineSeconds: ptr.To[int64](240)},
			Status:     batchv1.JobStatus{StartTime: &metav1.Time{Time: started}},
		}
	}
	if at, err := jobDeadlineAt(job(), miJob); err != nil || at != started.Unix()+240 {
		t.Fatalf("the deadline was read as %d: %v", at, err)
	}
	for name, mutate := range map[string]func(*batchv1.Job){
		"another Job": func(j *batchv1.Job) { j.UID = "u-replacement" },
		"not started": func(j *batchv1.Job) { j.Status.StartTime = nil },
		"no deadline": func(j *batchv1.Job) { j.Spec.ActiveDeadlineSeconds = nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			candidate := job()
			mutate(candidate)
			if _, err := jobDeadlineAt(candidate, miJob); err == nil {
				t.Fatalf("a Job with %s had a deadline", name)
			}
		})
	}
}

func TestOnlyIsolatedApplyOnNode(t *testing.T) {
	t.Parallel()
	const namespace, migration = "ptah-e2e", "e2e-isolated-node-postgresql"
	pods := func() []corev1.Pod {
		daemon := miPod("kindnet-a", "u-kindnet", "ptah-e2e-worker2", corev1.PodRunning)
		daemon.Namespace, daemon.Labels = "kube-system", nil
		daemon.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "kindnet"}}
		finished := miPod("old-job", "u-old", "ptah-e2e-worker2", corev1.PodSucceeded)
		finished.Namespace, finished.Labels = "elsewhere", nil
		return []corev1.Pod{daemon, finished, miPod("apply-a-x", "u-pod", "ptah-e2e-worker2", corev1.PodRunning)}
	}
	if !onlyIsolatedApplyOnNode(pods(), namespace, migration, "u-pod") {
		t.Fatal("a node running the Apply and a DaemonSet was refused")
	}
	for name, mutate := range map[string]func([]corev1.Pod) []corev1.Pod{
		"an empty node": func([]corev1.Pod) []corev1.Pod { return nil },
		"no Apply Pod":  func(p []corev1.Pod) []corev1.Pod { return p[:2] },
		"another workload": func(p []corev1.Pod) []corev1.Pod {
			other := miPod("web", "u-web", "ptah-e2e-worker2", corev1.PodRunning)
			other.Namespace, other.Labels = "application", nil
			return append(p, other)
		},
		"another migration": func(p []corev1.Pod) []corev1.Pod {
			other := miPod("apply-b-x", "u-other", "ptah-e2e-worker2", corev1.PodPending)
			other.Labels[labelMigration] = "e2e-other"
			return append(p, other)
		},
		"an unlabeled Pod in the namespace": func(p []corev1.Pod) []corev1.Pod {
			other := miPod("debug", "u-debug", "ptah-e2e-worker2", corev1.PodRunning)
			other.Labels = nil
			return append(p, other)
		},
		"the Apply finished": func(p []corev1.Pod) []corev1.Pod {
			p[2].Status.Phase = corev1.PodFailed
			return p
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if onlyIsolatedApplyOnNode(mutate(pods()), namespace, migration, "u-pod") {
				t.Fatalf("a node with %s was accepted", name)
			}
		})
	}
	if !onlyPod([]corev1.Pod{miPod("apply-a-x", "u-pod", "", "")}, "u-pod") ||
		onlyPod(nil, "u-pod") ||
		onlyPod([]corev1.Pod{miPod("apply-a-x", "u-pod", "", ""), miPod("apply-a-y", "u-pod-2", "", "")}, "u-pod") {
		t.Fatal("onlyPod does not say whether the list is that one Pod")
	}
}

func TestIsolatedReading(t *testing.T) {
	t.Parallel()
	reading, ok := parseIsolatedReading("2,2,2,3")
	if !ok || reading != (isolatedReading{rows: 2, versions: 2, applied: 2, widgets: 3}) || !reading.ranOnce() {
		t.Fatalf("the reading was read as %+v %t", reading, ok)
	}
	for _, value := range []string{"", "2,2,2", "2,2,2,3,4", "a,2,2,3", "2,2,2,-3", "ERROR:relation"} {
		if _, ok := parseIsolatedReading(value); ok {
			t.Errorf("%q was read as a reading", value)
		}
	}
	for name, reading := range map[string]isolatedReading{
		"a fourth revision":          {rows: 4, versions: 4, applied: 3, widgets: 3},
		"a version recorded twice":   {rows: 3, versions: 2, applied: 2, widgets: 3},
		"four recorded applied":      {rows: 3, versions: 3, applied: 4, widgets: 3},
		"the first migration re-ran": {rows: 3, versions: 3, applied: 3, widgets: 6},
		"the first migration lost":   {rows: 3, versions: 3, applied: 3, widgets: 0},
	} {
		if reading.ranOnce() {
			t.Errorf("a database with %s read as run once", name)
		}
	}
	for _, engine := range []string{"postgresql", "mysql"} {
		for _, statement := range []string{isolatedReadingQuery(engine), appliedVersionsQuery(engine)} {
			if !strings.Contains(statement, "schema_migrations") {
				t.Errorf("the %s statement reads no revision table: %s", engine, statement)
			}
		}
	}
	if !strings.Contains(isolatedReadingQuery("mysql"), "CONCAT(") || strings.Contains(isolatedReadingQuery("postgresql"), "CONCAT(") {
		t.Fatal("each engine's reading has to join the counts in its own dialect")
	}
}

func TestIsolatedHoldClocks(t *testing.T) {
	t.Parallel()
	const timeout = 600
	if got := isolatedHoldBound(1000, 1200, timeout); got != 1600 {
		t.Fatalf("a deadline inside the Lease term's bound moved it to %d", got)
	}
	if got := isolatedHoldBound(1000, 1600, timeout); got != 2200 {
		t.Fatalf("a deadline at the Lease term's bound left it at %d", got)
	}

	deleted := metav1.NewTime(time.Date(2026, 9, 26, 10, 2, 0, 0, time.UTC))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &deleted, DeletionGracePeriodSeconds: ptr.To[int64](30)}}
	if at, ok := podStopRequestedAt(pod); !ok || at != deleted.Unix()-30 {
		t.Fatalf("the stop was read as %d %t", at, ok)
	}
	pod.DeletionGracePeriodSeconds = nil
	if at, ok := podStopRequestedAt(pod); !ok || at != deleted.Unix() {
		t.Fatalf("a stop with no grace was read as %d %t", at, ok)
	}
	if _, ok := podStopRequestedAt(&corev1.Pod{}); ok {
		t.Fatal("a Pod nobody deleted was asked to stop")
	}

	added := metav1.NewTime(time.Date(2026, 9, 26, 10, 1, 0, 0, time.UTC))
	node := miWorker()
	node.Spec.Taints = append(node.Spec.Taints,
		corev1.Taint{Key: corev1.TaintNodeUnreachable, Effect: corev1.TaintEffectNoSchedule, TimeAdded: &added},
		corev1.Taint{Key: corev1.TaintNodeUnreachable, Effect: corev1.TaintEffectNoExecute})
	if _, ok := unreachableTaintAddedAt(node); ok {
		t.Fatal("a taint that records no time, or one that only stops scheduling, dated the eviction")
	}
	node.Spec.Taints[len(node.Spec.Taints)-1].TimeAdded = &added
	if at, ok := unreachableTaintAddedAt(node); !ok || at != added.Unix() {
		t.Fatalf("the eviction taint was read as %d %t", at, ok)
	}
}

func TestIsolatedRunRecorded(t *testing.T) {
	t.Parallel()
	recorded := miMigration(t, `{"status":{"lastRun":{"outcome":"Unknown","jobUID":"u-isolated-apply"}}}`)
	if !isolatedRunRecorded(recorded.Status, miJob) {
		t.Fatal("the recorded run was refused")
	}
	for name, document := range map[string]string{
		"a claim standing":    `{"status":{"activeOperation":{"type":"Apply"},"lastRun":{"outcome":"Unknown","jobUID":"u-isolated-apply"}}}`,
		"the run of a Job":    `{"status":{"lastRun":{"outcome":"Unknown","jobUID":"u-replacement"}}}`,
		"no run recorded yet": `{"status":{}}`,
	} {
		if isolatedRunRecorded(miMigration(t, document).Status, miJob) {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestJobIdentitySets(t *testing.T) {
	t.Parallel()
	if got := newUIDs([]string{"a", "b"}, []string{"b", "c", "a", "d"}); !slices.Equal(got, []string{"c", "d"}) {
		t.Fatalf("new identities were read as %v", got)
	}
	if got := newUIDs([]string{"a", "b"}, []string{"a"}); len(got) != 0 {
		t.Fatalf("a Job that went away read as new: %v", got)
	}
	if got := miDistinct([]string{"b", "a", "b", "a"}); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("sort -u was read as %v", got)
	}
}

func TestUnknownLayerRevisionTablesQuery(t *testing.T) {
	t.Parallel()
	// MySQL's catalog spans the server, so its count names the database.
	if !strings.Contains(unknownLayerRevisionTablesQuery("mysql", "ptah_e2e_unknown_layer"), "table_schema = 'ptah_e2e_unknown_layer'") {
		t.Fatal("the MySQL count does not name its database")
	}
	if strings.Contains(unknownLayerRevisionTablesQuery("postgresql", "ptah_e2e_unknown_layer"), "table_schema") {
		t.Fatal("the PostgreSQL count is narrowed to a schema the connection already chose")
	}
}

func TestEgressExpectations(t *testing.T) {
	t.Parallel()
	var schema, migration []string
	for _, expectation := range egressExpectations {
		switch expectation.family {
		case "schema":
			schema = append(schema, expectation.operation)
		case "migration":
			migration = append(migration, expectation.operation)
		default:
			t.Fatalf("an expectation names the family %q", expectation.family)
		}
		// The reading sorts its lines, so the expectation has to be in that
		// order to compare equal to one.
		fields := strings.Fields(expectation.want())
		if !slices.IsSorted(fields) || len(fields) != 4 || fields[0] != "api=closed" {
			t.Errorf("the %s %s expectation is not a sorted reading: %s", expectation.family, expectation.operation, expectation.want())
		}
	}
	if !slices.Equal(schema, schemaOperations) {
		t.Fatalf("the schema rows name %v, and the family runs %v", schema, schemaOperations)
	}
	if !slices.Equal(migration, []string{"resolve", "verify", "history", "apply"}) {
		t.Fatalf("the migration rows name %v", migration)
	}
	if fields := strings.Fields(egressOpenReading); !slices.IsSorted(fields) {
		t.Fatal("the open reading is not in the order a reading sorts")
	}
}

func TestProbeReading(t *testing.T) {
	t.Parallel()
	for log, want := range map[string]string{
		"dns=open\ndatabase=closed\nregistry=open\napi=closed\n": "api=closed database=closed dns=open registry=open",
		"dns=open\napi=open": "api=open dns=open",
		"":                   "",
	} {
		if got := probeReading([]byte(log)); got != want {
			t.Errorf("%q read as %q, want %q", log, got, want)
		}
	}
}

// miEgressExample is the example the row adapts, read from the repository.
func miEgressExample(t *testing.T) []map[string]any {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("..", "..", "examples", "networkpolicy-egress.yaml"))
	if err != nil {
		t.Fatalf("read the egress example: %v", err)
	}
	items, err := egressExampleItems(content)
	if err != nil {
		t.Fatalf("decode the egress example: %v", err)
	}
	return items
}

func miPolicy(t *testing.T, items []map[string]any, name string) map[string]any {
	t.Helper()
	for _, item := range items {
		if miObjectName(item) == name {
			return item
		}
	}
	t.Fatalf("no policy %s", name)
	return nil
}

func TestEgressExampleShaped(t *testing.T) {
	t.Parallel()
	if !egressExampleShaped(miEgressExample(t)) {
		t.Fatal("the egress example is not the shape the row adapts")
	}
	for name, mutate := range map[string]func([]map[string]any) []map[string]any{
		"a policy dropped": func(items []map[string]any) []map[string]any { return items[1:] },
		"a registry policy renamed": func(items []map[string]any) []map[string]any {
			items[3]["metadata"].(map[string]any)["name"] = "ptah-schema-operations-oci"
			return items
		},
		"a database policy renamed": func(items []map[string]any) []map[string]any {
			items[5]["metadata"].(map[string]any)["name"] = "ptah-schema-operations-sql"
			return items
		},
		"a default deny renamed": func(items []map[string]any) []map[string]any {
			items[0]["metadata"].(map[string]any)["name"] = "ptah-schema-operations-deny"
			return items
		},
		"another kind": func(items []map[string]any) []map[string]any {
			items[2]["kind"] = "ConfigMap"
			return items
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if egressExampleShaped(mutate(miEgressExample(t))) {
				t.Fatalf("an example with %s was accepted", name)
			}
		})
	}
	// A List reads as its items, as kubectl printed it.
	list := []byte(`{"apiVersion":"v1","kind":"List","items":[{"kind":"NetworkPolicy","metadata":{"name":"a"}}]}`)
	if items, err := egressExampleItems(list); err != nil || len(items) != 1 || miObjectName(items[0]) != "a" {
		t.Fatalf("a List read as %v: %v", items, err)
	}
}

func TestRenderEgressPolicies(t *testing.T) {
	t.Parallel()
	example := miEgressExample(t)
	rendered, err := renderEgressPolicies(example, "ptah-e2e", "172.18.0.9", "e2e-postgresql", 5432, "operator-system", map[string]string{"app.kubernetes.io/instance": "test-release"})
	if err != nil {
		t.Fatalf("render the example: %v", err)
	}
	for _, policy := range rendered {
		metadata := policy["metadata"].(map[string]any)
		if metadata["namespace"] != "ptah-e2e" || metadata["labels"].(map[string]any)[egressProofLabel] != "egress" {
			t.Fatalf("%s was not moved into the namespace under the proof label", miObjectName(policy))
		}
	}
	registry := miPolicy(t, rendered, "ptah-migration-operations-registry")["spec"].(map[string]any)["egress"].([]any)[0].(map[string]any)
	if cidr := registry["to"].([]any)[0].(map[string]any)["ipBlock"].(map[string]any)["cidr"]; cidr != "172.18.0.9/32" ||
		len(registry["to"].([]any)) != 1 || registry["ports"] == nil {
		t.Fatalf("the registry rule was rendered as %v", registry)
	}
	database := miPolicy(t, rendered, "ptah-schema-operations-database")["spec"].(map[string]any)["egress"].([]any)[0].(map[string]any)
	selector := database["to"].([]any)[0].(map[string]any)["podSelector"].(map[string]any)["matchLabels"].(map[string]any)
	port := database["ports"].([]any)[0].(map[string]any)
	if selector["app.kubernetes.io/name"] != "e2e-postgresql" || port["port"] != int64(5432) || port["protocol"] != "TCP" {
		t.Fatalf("the database rule was rendered as %v", database)
	}
	resultRule := miPolicy(t, rendered, "ptah-operations-results")["spec"].(map[string]any)["egress"].([]any)[0].(map[string]any)
	peer := resultRule["to"].([]any)[0].(map[string]any)
	if peer["namespaceSelector"].(map[string]any)["matchLabels"].(map[string]any)["kubernetes.io/metadata.name"] != "operator-system" ||
		peer["podSelector"].(map[string]any)["matchLabels"].(map[string]any)["app.kubernetes.io/instance"] != "test-release" ||
		len(resultRule["to"].([]any)) != 1 {
		t.Fatalf("receiver destination was not bound to the installed manager: %v", resultRule)
	}
	// What the example does not tell a reader to replace stays the example's.
	dns := miPolicy(t, rendered, "ptah-operations-dns")["spec"]
	if !miEqualJSON(t, dns, miPolicy(t, example, "ptah-operations-dns")["spec"]) {
		t.Fatal("the DNS policy was changed")
	}
	if miPolicy(t, example, "ptah-migration-operations-registry")["metadata"].(map[string]any)["namespace"] != "application" {
		t.Fatal("rendering changed the example it read")
	}
	if !egressSelectorsKept(example, rendered) {
		t.Fatal("rendering read as moving a selector")
	}
	for name, mutate := range map[string]func(map[string]any){
		"a selector widened": func(spec map[string]any) {
			delete(spec["podSelector"].(map[string]any)["matchLabels"].(map[string]any), labelComponent)
		},
		"a policy type added": func(spec map[string]any) { spec["policyTypes"] = []any{"Egress", "Ingress"} },
	} {
		moved, err := renderEgressPolicies(example, "ptah-e2e", "172.18.0.9", "e2e-postgresql", 5432, "operator-system", map[string]string{"app.kubernetes.io/instance": "test-release"})
		if err != nil {
			t.Fatal(err)
		}
		mutate(miPolicy(t, moved, "ptah-schema-operations-registry")["spec"].(map[string]any))
		if egressSelectorsKept(example, moved) {
			t.Errorf("a rendering with %s was accepted", name)
		}
	}
	broken := runtime.DeepCopyJSON(map[string]any{"items": miToAny(example)})["items"].([]any)
	for _, item := range broken {
		if miObjectName(item.(map[string]any)) == "ptah-schema-operations-registry" {
			delete(item.(map[string]any)["spec"].(map[string]any), "egress")
		}
	}
	if _, err := renderEgressPolicies(miFromAny(broken), "ptah-e2e", "172.18.0.9", "e2e-postgresql", 5432, "operator-system", map[string]string{"app.kubernetes.io/instance": "test-release"}); err == nil {
		t.Fatal("a registry policy with no rules to adapt was rendered")
	}
}

func TestEgressProbeCopy(t *testing.T) {
	t.Parallel()
	render := func() []map[string]any {
		rendered, err := renderEgressPolicies(miEgressExample(t), "ptah-e2e", "172.18.0.9", "e2e-mysql", 3306, "operator-system", map[string]string{"app.kubernetes.io/instance": "test-release"})
		if err != nil {
			t.Fatal(err)
		}
		return rendered
	}
	rendered := render()
	copies, err := egressProbeCopy(rendered, egressProbeManager)
	if err != nil {
		t.Fatalf("copy the rendering: %v", err)
	}
	for _, probe := range copies {
		selector := probe["spec"].(map[string]any)["podSelector"].(map[string]any)["matchLabels"].(map[string]any)
		if !strings.HasSuffix(miObjectName(probe), "-probe") || selector[labelManagedBy] != egressProbeManager {
			t.Fatalf("%s does not select the probes", miObjectName(probe))
		}
	}
	if !egressProbeCopyFaithful(render(), copies) {
		t.Fatal("the copy read as differing by more than the manager")
	}
	if miPolicy(t, rendered, "ptah-operations-dns")["spec"].(map[string]any)["podSelector"].(map[string]any)["matchLabels"].(map[string]any)[labelManagedBy] != managedByOperator {
		t.Fatal("copying changed the rendering it read")
	}
	for name, mutate := range map[string]func([]map[string]any) []map[string]any{
		"a copy dropped": func(c []map[string]any) []map[string]any { return c[1:] },
		"a port moved": func(c []map[string]any) []map[string]any {
			rules := c[6]["spec"].(map[string]any)["egress"].([]any)
			rules[0].(map[string]any)["ports"] = []any{map[string]any{"protocol": "TCP", "port": int64(3307)}}
			return c
		},
		"a component moved": func(c []map[string]any) []map[string]any {
			c[0]["spec"].(map[string]any)["podSelector"].(map[string]any)["matchLabels"].(map[string]any)[labelComponent] = "other"
			return c
		},
		"a name moved": func(c []map[string]any) []map[string]any {
			c[2]["metadata"].(map[string]any)["name"] = "ptah-operations-dns-copy"
			return c
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			copies, err := egressProbeCopy(render(), egressProbeManager)
			if err != nil {
				t.Fatal(err)
			}
			if egressProbeCopyFaithful(render(), mutate(copies)) {
				t.Fatalf("a copy with %s was accepted", name)
			}
		})
	}
	unmanaged := render()
	unmanaged[2]["spec"].(map[string]any)["podSelector"].(map[string]any)["matchLabels"].(map[string]any)[labelManagedBy] = "helm"
	if _, err := egressProbeCopy(unmanaged, egressProbeManager); err == nil {
		t.Fatal("a policy that selects no operation Pod was copied")
	}
	unselected := render()
	delete(unselected[0]["spec"].(map[string]any)["podSelector"].(map[string]any), "matchLabels")
	if _, err := egressProbeCopy(unselected, egressProbeManager); err == nil {
		t.Fatal("a policy with no labels to select by was copied")
	}
}

func TestEgressAddresses(t *testing.T) {
	t.Parallel()
	endpoint := func(addresses ...string) discoveryv1.Endpoint { return discoveryv1.Endpoint{Addresses: addresses} }
	if address, ok := soleEndpointAddress([]discoveryv1.EndpointSlice{{Endpoints: []discoveryv1.Endpoint{endpoint("10.0.0.4")}}}); !ok || address != "10.0.0.4" {
		t.Fatalf("the registry address was read as %q %t", address, ok)
	}
	for name, endpointSlices := range map[string][]discoveryv1.EndpointSlice{
		"none":            nil,
		"no address":      {{Endpoints: []discoveryv1.Endpoint{endpoint()}}},
		"two in a slice":  {{Endpoints: []discoveryv1.Endpoint{endpoint("10.0.0.4", "10.0.0.5")}}},
		"one in each two": {{Endpoints: []discoveryv1.Endpoint{endpoint("10.0.0.4")}}, {Endpoints: []discoveryv1.Endpoint{endpoint("10.0.0.5")}}},
	} {
		if _, ok := soleEndpointAddress(endpointSlices); ok {
			t.Errorf("a Service with %s resolved to one address", name)
		}
	}

	withIP := func(ip string) corev1.Pod { return corev1.Pod{Status: corev1.PodStatus{PodIP: ip}} }
	if ip, ok := soleRunningPodIP([]corev1.Pod{withIP(""), withIP("10.244.1.7")}); !ok || ip != "10.244.1.7" {
		t.Fatalf("the database address was read as %q %t", ip, ok)
	}
	if _, ok := soleRunningPodIP([]corev1.Pod{withIP("10.244.1.7"), withIP("10.244.1.8")}); ok {
		t.Fatal("two database Pods resolved to one address")
	}
	if _, ok := soleRunningPodIP(nil); ok {
		t.Fatal("no database Pod resolved to an address")
	}

	running := func(image string) corev1.Pod {
		return corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: image}, {Image: "sidecar"}}}}
	}
	if image, ok := soleProbeImage([]corev1.Pod{running("postgres@sha256:aa"), running("postgres@sha256:aa")}); !ok || image != "postgres@sha256:aa" {
		t.Fatalf("the probe image was read as %q %t", image, ok)
	}
	if _, ok := soleProbeImage([]corev1.Pod{running("postgres@sha256:aa"), running("postgres@sha256:bb")}); ok {
		t.Fatal("two images read as one")
	}
	if _, ok := soleProbeImage(nil); ok {
		t.Fatal("no Pod read as an image")
	}

	api := []discoveryv1.EndpointSlice{
		{Endpoints: []discoveryv1.Endpoint{endpoint("172.18.0.2", "172.18.0.3")}, Ports: []discoveryv1.EndpointPort{{Port: ptr.To[int32](6443)}}},
		{Endpoints: []discoveryv1.Endpoint{endpoint("172.18.0.4")}, Ports: []discoveryv1.EndpointPort{{Port: ptr.To[int32](443)}}},
	}
	if address, port, ok := firstAPIEndpoint(api); !ok || address != "172.18.0.2" || port != "6443" {
		t.Fatalf("the API server endpoint was read as %s:%s %t", address, port, ok)
	}
	api[0].Ports[0].Port = nil
	if _, _, ok := firstAPIEndpoint(api); ok {
		t.Fatal("an endpoint whose first port is unnamed was dialed")
	}
	if _, _, ok := firstAPIEndpoint(nil); ok {
		t.Fatal("no endpoint was dialed")
	}
}

func TestEgressProbePod(t *testing.T) {
	t.Parallel()
	target := egressProbeTarget{
		image: "postgres@sha256:aa", registry: "172.18.0.9", database: "10.244.1.7", databasePort: "5432",
		api: "172.18.0.2", apiPort: "6443",
	}
	pod := egressProbePod("ptah-e2e", "egress-probe-migration-apply",
		egressProbeLabels(egressProbeManager, "migration-operation", "apply"), target)
	labels := pod["metadata"].(map[string]any)["labels"].(map[string]any)
	if labels[egressProbeLabel] != "egress" || labels[labelManagedBy] != egressProbeManager ||
		labels[labelComponent] != "migration-operation" || labels[labelOperation] != "apply" {
		t.Fatalf("the probe carries %v", labels)
	}
	container := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	env := map[string]any{}
	for _, variable := range container["env"].([]any) {
		env[variable.(map[string]any)["name"].(string)] = variable.(map[string]any)["value"]
	}
	if env["REGISTRY"] != "172.18.0.9" || env["DATABASE"] != "10.244.1.7" || env["DATABASE_PORT"] != "5432" ||
		env["API"] != "172.18.0.2" || env["API_PORT"] != "6443" || container["image"] != target.image {
		t.Fatalf("the probe dials %v with %v", env, container["image"])
	}
	unselected := egressProbePod("ptah-e2e", "egress-probe-unselected", nil, target)
	if got := unselected["metadata"].(map[string]any)["labels"].(map[string]any); len(got) != 1 {
		t.Fatalf("an unselected probe carries %v", got)
	}
	for _, line := range []string{"reach \"$DATABASE\" \"$DATABASE_PORT\" database", "reach \"$REGISTRY\" 5000 registry",
		"reach \"$API\" \"$API_PORT\" api", "nslookup kubernetes.default.svc.cluster.local"} {
		if !strings.Contains(egressProbeScript, line) {
			t.Errorf("the probe script does not %s", line)
		}
	}
}

func miOperationJob(uid, operation, manager, component string) batchv1.Job {
	job := batchv1.Job{ObjectMeta: metav1.ObjectMeta{UID: types.UID(uid)}}
	job.Spec.Template.Labels = map[string]string{labelOperation: operation, labelManagedBy: manager, labelComponent: component}
	return job
}

func TestEgressJobsSelected(t *testing.T) {
	t.Parallel()
	jobs := []batchv1.Job{
		miOperationJob("u-resolve", "resolve", managedByOperator, migrationOperationComponent),
		miOperationJob("u-history", "history", managedByOperator, migrationOperationComponent),
		miOperationJob("u-resolve", "resolve", managedByOperator, migrationOperationComponent),
		miOperationJob("u-apply", "apply", managedByOperator, migrationOperationComponent),
	}
	if !egressJobsSelected(jobs) {
		t.Fatal("the Jobs of a converged migration were refused")
	}
	for name, mutate := range map[string]func([]batchv1.Job) []batchv1.Job{
		"no Apply":        func(j []batchv1.Job) []batchv1.Job { return j[:3] },
		"no Jobs at all":  func([]batchv1.Job) []batchv1.Job { return nil },
		"another manager": func(j []batchv1.Job) []batchv1.Job { j[1].Spec.Template.Labels[labelManagedBy] = "helm"; return j },
		"a schema operation": func(j []batchv1.Job) []batchv1.Job {
			j[1].Spec.Template.Labels[labelComponent] = schemaOperationComponent
			return j
		},
		"a template with no component": func(j []batchv1.Job) []batchv1.Job {
			delete(j[3].Spec.Template.Labels, labelComponent)
			return j
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			candidate := make([]batchv1.Job, 0, len(jobs))
			for _, job := range jobs {
				candidate = append(candidate, *job.DeepCopy())
			}
			if egressJobsSelected(mutate(candidate)) {
				t.Fatalf("Jobs with %s were accepted", name)
			}
		})
	}
	converged := miMigration(t, `{"status":{"phase":"InSync","lastRun":{"outcome":"Applied"}}}`)
	if !egressConverged(converged.Status) {
		t.Fatal("a converged migration was refused")
	}
	for name, document := range map[string]string{
		"between cycles":   `{"status":{"phase":"Reading","lastRun":{"outcome":"Applied"}}}`,
		"up to date":       `{"status":{"phase":"InSync","lastRun":{"outcome":"UpToDate"}}}`,
		"no run recorded":  `{"status":{"phase":"InSync"}}`,
		"awaiting a phase": `{}`,
	} {
		if egressConverged(miMigration(t, document).Status) {
			t.Errorf("a migration %s read as converged", name)
		}
	}
}

func TestRetargetRefused(t *testing.T) {
	t.Parallel()
	miJudge(t, []miReading{
		{"refused for the repointed target", `{"status":{"unresolvedRun":{"outcome":"Unknown","jobUID":"u-retarget"},
 "conditions":[{"type":"Blocked","status":"True","reason":"ApplyOutcomeUnknown",
 "message":"the runner refused the Apply: target_binding_mismatch: the database is not the one the plan names"}]}}`, true},
		{"no record", `{"status":{"conditions":[{"type":"Blocked","status":"True","reason":"ApplyOutcomeUnknown",
 "message":"target_binding_mismatch"}]}}`, false},
		{"the record of another Job", `{"status":{"unresolvedRun":{"outcome":"Unknown","jobUID":"u-other"},
 "conditions":[{"type":"Blocked","status":"True","reason":"ApplyOutcomeUnknown","message":"target_binding_mismatch"}]}}`, false},
		{"refused for another reason", `{"status":{"unresolvedRun":{"outcome":"Unknown","jobUID":"u-retarget"},
 "conditions":[{"type":"Blocked","status":"True","reason":"ApplyOutcomeUnknown","message":"ptah runner result frame not found"}]}}`, false},
		{"blocked under another reason", `{"status":{"unresolvedRun":{"outcome":"Unknown","jobUID":"u-retarget"},
 "conditions":[{"type":"Blocked","status":"True","reason":"RealmConflict","message":"target_binding_mismatch"}]}}`, false},
		{"not blocked", `{"status":{"unresolvedRun":{"outcome":"Unknown","jobUID":"u-retarget"},
 "conditions":[{"type":"Blocked","status":"False","reason":"ApplyOutcomeUnknown","message":"target_binding_mismatch"}]}}`, false},
		{"the mismatch on another condition", `{"status":{"unresolvedRun":{"outcome":"Unknown","jobUID":"u-retarget"},
 "conditions":[{"type":"Ready","status":"True","reason":"ApplyOutcomeUnknown","message":"target_binding_mismatch"}]}}`, false},
		{"no conditions", `{"status":{"unresolvedRun":{"outcome":"Unknown","jobUID":"u-retarget"}}}`, false},
	}, func(migration *ptahv1alpha1.PtahMigration) bool {
		return retargetRefused(migration.Status, "u-retarget")
	})
}

func TestDrillReadings(t *testing.T) {
	t.Parallel()
	converged := miMigration(t, `{"status":{"phase":"Reading","conditions":[{"type":"Ready","status":"True","reason":"HistoryMatched"}]}}`)
	if !drillConverged(converged.Status) {
		t.Fatal("a converged drill was refused")
	}
	for name, document := range map[string]string{
		"ready for another reason": `{"status":{"conditions":[{"type":"Ready","status":"True","reason":"Applied"}]}}`,
		"matched but not ready":    `{"status":{"conditions":[{"type":"Ready","status":"False","reason":"HistoryMatched"}]}}`,
		"no conditions":            `{"status":{"phase":"InSync"}}`,
	} {
		if drillConverged(miMigration(t, document).Status) {
			t.Errorf("a drill %s read as converged", name)
		}
	}

	awaiting := `{"status":{"plan":{"name":"ptah-mplan-3","uid":"u-plan"},
 "conditions":[{"type":"ApprovalRequired","status":"True","reason":"AwaitingApproval"}]}}`
	if plan, ok := drillAwaitingDecision(miMigration(t, awaiting).Status); !ok || plan != "ptah-mplan-3" {
		t.Fatalf("the decision was read as %q %t", plan, ok)
	}
	for name, document := range map[string]string{
		"no plan": `{"status":{"conditions":[{"type":"ApprovalRequired","status":"True","reason":"AwaitingApproval"}]}}`,
		"a plan with no name": `{"status":{"plan":{"name":"","uid":"u-plan"},
 "conditions":[{"type":"ApprovalRequired","status":"True","reason":"AwaitingApproval"}]}}`,
		"approval no longer asked": `{"status":{"plan":{"name":"ptah-mplan-3","uid":"u-plan"},
 "conditions":[{"type":"ApprovalRequired","status":"False","reason":"Approved"}]}}`,
	} {
		if _, ok := drillAwaitingDecision(miMigration(t, document).Status); ok {
			t.Errorf("a drill with %s asked for a decision", name)
		}
	}

	if !drillApplyClaimed(miMigration(t, `{"status":{"activeOperation":{"type":"Apply","jobName":"apply-a"}}}`).Status) {
		t.Fatal("a claimed Apply was refused")
	}
	for name, document := range map[string]string{
		"no claim":         `{"status":{}}`,
		"a History":        `{"status":{"activeOperation":{"type":"History","jobName":"history-a"}}}`,
		"no Job named yet": `{"status":{"activeOperation":{"type":"Apply"}}}`,
	} {
		if drillApplyClaimed(miMigration(t, document).Status) {
			t.Errorf("a drill with %s read as claimed", name)
		}
	}
}

func TestDrillRebuiltIdle(t *testing.T) {
	t.Parallel()
	for name, row := range map[string]struct {
		document string
		accept   bool
	}{
		"asking and idle":        {`{"status":{"plan":{"name":"p"},"phase":"AwaitingApproval"}}`, true},
		"reading its history":    {`{"status":{"activeOperation":{"type":"History"}}}`, true},
		"a run recorded":         {`{"status":{"lastRun":{"outcome":"Applied"}}}`, false},
		"a run key stored empty": {`{"status":{"lastRun":null}}`, false},
		"an Apply claimed":       {`{"status":{"activeOperation":{"type":"Apply"}}}`, false},
		"no status":              {`{"metadata":{"name":"e2e-drill-postgresql"}}`, false},
	} {
		var document map[string]any
		if err := json.Unmarshal([]byte(row.document), &document); err != nil {
			t.Fatal(err)
		}
		if got := drillRebuiltIdle(document); got != row.accept {
			t.Errorf("%s judged %t, want %t", name, got, row.accept)
		}
	}
}

func TestDrillOwnedApplyUIDs(t *testing.T) {
	t.Parallel()
	owned := miOperationJob("u-apply-new", "apply", managedByOperator, migrationOperationComponent)
	owned.OwnerReferences = []metav1.OwnerReference{{Kind: "PtahMigration", UID: "u-rebuilt"}}
	earlier := miOperationJob("u-apply-old", "apply", managedByOperator, migrationOperationComponent)
	earlier.OwnerReferences = []metav1.OwnerReference{{Kind: "PtahMigration", UID: "u-original"}}
	orphan := miOperationJob("u-orphan", "apply", managedByOperator, migrationOperationComponent)
	if got := drillOwnedApplyUIDs([]batchv1.Job{earlier, owned, orphan}, "u-rebuilt"); !slices.Equal(got, []string{"u-apply-new"}) {
		t.Fatalf("the rebuilt resource's Jobs were read as %v", got)
	}
	if got := drillOwnedApplyUIDs([]batchv1.Job{orphan}, ""); len(got) != 0 {
		t.Fatalf("an owner nobody named owned %v", got)
	}
}

func TestStrippedForRestore(t *testing.T) {
	t.Parallel()
	var backup map[string]any
	if err := json.Unmarshal([]byte(`{"apiVersion":"operator.ptah.run/v1alpha1","kind":"PtahMigration",
 "metadata":{"name":"e2e-drill-postgresql","namespace":"ptah-e2e","uid":"u","resourceVersion":"9",
  "creationTimestamp":"2026-09-26T10:00:00Z","generation":2,"managedFields":[{}],"finalizers":["f"],
  "ownerReferences":[{}],"deletionTimestamp":"2026-09-26T10:09:00Z","deletionGracePeriodSeconds":0,
  "labels":{"team":"a"},"annotations":{"note":"kept"}},
 "spec":{"interval":"1h"},"status":{"phase":"Applying"}}`), &backup); err != nil {
		t.Fatal(err)
	}
	stripped := strippedForRestore(backup)
	want := `{"apiVersion":"operator.ptah.run/v1alpha1","kind":"PtahMigration",
 "metadata":{"name":"e2e-drill-postgresql","namespace":"ptah-e2e","labels":{"team":"a"},"annotations":{"note":"kept"}},
 "spec":{"interval":"1h"}}`
	var expected map[string]any
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		t.Fatal(err)
	}
	if !miEqualJSON(t, stripped, expected) {
		t.Fatalf("the backup was stripped to %v", stripped)
	}
	if backup["status"] == nil || backup["metadata"].(map[string]any)["uid"] != "u" {
		t.Fatal("stripping changed the backup it read")
	}
}

func TestDrillPlanVersionsAndQueries(t *testing.T) {
	t.Parallel()
	plan := &ptahv1alpha1.PtahMigrationPlan{}
	plan.Spec.Migrations = []ptahv1alpha1.PlannedMigration{{Version: 3}, {Version: 4}}
	if got := drillPlanVersions(plan); got != "3 4" {
		t.Fatalf("the plan approves %q", got)
	}
	if got := drillPlanVersions(&ptahv1alpha1.PtahMigrationPlan{}); got != "" {
		t.Fatalf("an empty plan approves %q", got)
	}
	// Every row in any state is a row the drill counts.
	for _, engine := range []string{"postgresql", "mysql"} {
		if strings.Contains(drillRevisionsQuery(engine), "state") {
			t.Errorf("the %s revision reading filters by state", engine)
		}
		if !strings.Contains(drillMarkerQuery(engine), "e2e_drill_marker") {
			t.Errorf("the %s marker reading looks for another table", engine)
		}
	}
}

func TestTxmodeHistoryApplied(t *testing.T) {
	t.Parallel()
	applied := func() ptahv1alpha1.PtahMigrationStatus {
		return miMigration(t, `{"status":{"phase":"InSync",
 "history":{"currentVersion":3,"appliedCount":3,"pendingCount":0},
 "lastRun":{"outcome":"Applied"},
 "conditions":[{"type":"Ready","status":"True","reason":"HistoryMatched"}]}}`).Status
	}
	if !txmodeHistoryApplied(applied()) {
		t.Fatal("the applied sequence was refused")
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigrationStatus){
		"no history":           func(s *ptahv1alpha1.PtahMigrationStatus) { s.History = nil },
		"at version 2":         func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.CurrentVersion = 2 },
		"two applied":          func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.AppliedCount = 2 },
		"one pending":          func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.PendingCount = 1 },
		"dirty":                func(s *ptahv1alpha1.PtahMigrationStatus) { s.History.Dirty = true },
		"no run":               func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun = nil },
		"a run that failed":    func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.Outcome = ptahv1alpha1.MigrationRunOutcomeFailed },
		"not ready":            func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions[0].Status = metav1.ConditionFalse },
		"no conditions at all": func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions = nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			status := applied()
			mutate(&status)
			if txmodeHistoryApplied(status) {
				t.Fatalf("a sequence with %s was accepted", name)
			}
		})
	}
}

func TestMiClip(t *testing.T) {
	t.Parallel()
	if got := miClip("ab", 5); got != "ab" {
		t.Fatalf("a short message was clipped to %q", got)
	}
	if got := miClip("äbcdef", 3); got != "äbc" {
		t.Fatalf("a message was clipped to %q, not by character", got)
	}
}

// miEqualJSON compares two decoded documents by their JSON, so a number decoded
// as float64 and one written as int64 compare as jq compared them.
func miEqualJSON(t *testing.T, a, b any) bool {
	t.Helper()
	left, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	right, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(left, right)
}

func miToAny(items []map[string]any) []any {
	out := make([]any, 0, len(items))
	for _, item := range items {
		out = append(out, item)
	}
	return out
}

func miFromAny(items []any) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, item.(map[string]any))
	}
	return out
}
