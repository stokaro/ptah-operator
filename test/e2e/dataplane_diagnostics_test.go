package e2e

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

const diagnosticSentinel = "DO_NOT_PRINT_CREDENTIAL_SENTINEL"

func decodeFixture[T any](t *testing.T, document string) T {
	t.Helper()
	var decoded T
	if err := json.Unmarshal([]byte(document), &decoded); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return decoded
}

// The fixtures are the ones the shell phase's projection was held to: every
// free-text field carries the sentinel, one schema's fields are malformed, one
// Event names the right schema by another UID, and one Lease is someone
// else's.
func cleanupFixtures(t *testing.T) ([]ptahv1alpha1.PtahSchema, []corev1.Event, []batchv1.Job, []coordinationv1.Lease) {
	schemas := decodeFixture[ptahv1alpha1.PtahSchemaList](t, `{"items": [
	  {"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahSchema",
	   "metadata": {"name": "schema-a", "uid": "schema-uid-a", "generation": 4},
	   "status": {
	     "observedGeneration": 4, "phase": "Failed", "nextReconciliationTime": "2026-09-01T00:00:00Z",
	     "activeOperation": {"leaseEpoch": "v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "leaseContinuityLost": true},
	     "pendingObservation": {"leaseEpoch": "malformed-DO_NOT_PRINT_CREDENTIAL_SENTINEL"},
	     "pendingLockRelease": {"leaseEpoch": "v1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	     "conditions": [{"type": "ReconciliationFailed", "status": "True", "reason": "OperationFailed",
	       "message": "invalid_plan_output: DO_NOT_PRINT_CREDENTIAL_SENTINEL"}]}},
	  {"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahSchema",
	   "metadata": {"name": "schema-b", "uid": "schema-uid-b", "generation": 2},
	   "status": {
	     "observedGeneration": 2, "phase": "not-a-phase-DO_NOT_PRINT_CREDENTIAL_SENTINEL",
	     "activeOperation": {"leaseEpoch": "v1-NOT-AN-EPOCH"},
	     "conditions": [{"type": "ReconciliationFailed", "status": "True", "reason": "OperationFailed",
	       "message": "bad__code: DO_NOT_PRINT_CREDENTIAL_SENTINEL"}]}}]}`).Items
	events := decodeFixture[corev1.EventList](t, `{"items": [
	  {"involvedObject": {"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahSchema", "name": "schema-a", "uid": "schema-uid-a"},
	   "type": "Warning", "reason": "OperationFailed", "message": "invalid_target: DO_NOT_PRINT_CREDENTIAL_SENTINEL",
	   "lastTimestamp": "2026-09-01T00:00:01Z"},
	  {"involvedObject": {"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahSchema", "name": "schema-a", "uid": "schema-uid-a"},
	   "type": "Warning", "reason": "PlanStale", "message": "DO_NOT_PRINT_CREDENTIAL_SENTINEL", "lastTimestamp": "2026-09-01T00:00:02Z"},
	  {"involvedObject": {"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahSchema", "name": "schema-a", "uid": "schema-uid-a"},
	   "type": "Warning", "reason": "LeaseContinuityLost", "message": "DO_NOT_PRINT_CREDENTIAL_SENTINEL",
	   "lastTimestamp": "2026-09-01T00:00:03Z"},
	  {"involvedObject": {"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahSchema", "name": "schema-a", "uid": "wrong-schema-uid"},
	   "type": "Warning", "reason": "ReconciliationFailed", "message": "wrong_uid_code: DO_NOT_PRINT_CREDENTIAL_SENTINEL"},
	  {"involvedObject": {"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahSchema", "name": "schema-b", "uid": "schema-uid-b"},
	   "type": "Warning", "reason": "OperationFailed", "message": "bad__code: DO_NOT_PRINT_CREDENTIAL_SENTINEL"}]}`).Items
	jobs := decodeFixture[batchv1.JobList](t, `{"items": [
	  {"metadata": {"name": "job-a", "uid": "job-uid-a", "creationTimestamp": "2026-09-01T00:00:00Z",
	     "ownerReferences": [{"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahSchema", "name": "schema-a",
	       "uid": "schema-uid-a", "controller": true}],
	     "labels": {"operator.ptah.run/operation": "plan"},
	     "annotations": {
	       "operator.ptah.run/operation-id": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	       "operator.ptah.run/input-fingerprint": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	       "unsafe": "DO_NOT_PRINT_CREDENTIAL_SENTINEL"}},
	   "status": {"startTime": "2026-09-01T00:00:00Z", "completionTime": "2026-09-01T00:00:01Z",
	     "conditions": [{"type": "Failed", "status": "True", "message": "DO_NOT_PRINT_CREDENTIAL_SENTINEL"}]}},
	  {"metadata": {"name": "job-b", "uid": "job-uid-b",
	     "ownerReferences": [{"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahSchema", "name": "schema-b",
	       "uid": "schema-uid-b", "controller": true}],
	     "labels": {"operator.ptah.run/operation": "DO_NOT_PRINT_CREDENTIAL_SENTINEL"}},
	   "status": {}}]}`).Items
	leases := decodeFixture[coordinationv1.LeaseList](t, `{"items": [
	  {"metadata": {"name": "lease-a", "uid": "lease-uid-a", "resourceVersion": "10",
	     "labels": {"app.kubernetes.io/managed-by": "ptah-operator", "operator.ptah.run/coordination": "database-target"},
	     "annotations": {"operator.ptah.run/lease-epoch": "v1-cccccccccccccccccccccccccccccccc"}},
	   "spec": {"holderIdentity": "ptah-h-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "leaseDurationSeconds": 960,
	     "leaseTransitions": 1}},
	  {"metadata": {"name": "lease-b", "uid": "lease-uid-b", "resourceVersion": "11",
	     "labels": {"app.kubernetes.io/managed-by": "ptah-operator", "operator.ptah.run/coordination": "database-target"},
	     "annotations": {"operator.ptah.run/lease-epoch": "malformed-DO_NOT_PRINT_CREDENTIAL_SENTINEL"}},
	   "spec": {"holderIdentity": "DO_NOT_PRINT_CREDENTIAL_SENTINEL"}},
	  {"metadata": {"name": "lease-unmanaged", "uid": "lease-uid-unmanaged",
	     "labels": {"app.kubernetes.io/managed-by": "someone-else"}},
	   "spec": {"holderIdentity": "DO_NOT_PRINT_CREDENTIAL_SENTINEL"}}]}`).Items
	return schemas, events, jobs, leases
}

func TestCleanupProjectionCarriesNoFreeText(t *testing.T) {
	t.Parallel()
	projection := projectCleanupDiagnostic(cleanupFixtures(t))
	encoded, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(diagnosticSentinel)) {
		t.Fatalf("the projection disclosed its sentinel: %s", encoded)
	}
	if len(projection.Schemas) != 2 {
		t.Fatalf("projected %d schemas, want 2", len(projection.Schemas))
	}
	first, second := projection.Schemas[0], projection.Schemas[1]
	if first.UID != "schema-uid-a" || first.Failure == nil || stringOr(first.Failure.Code) != "invalid_plan_output" {
		t.Errorf("first schema = %+v", first)
	}
	if stringOr(first.ExpectedEpochs.ActiveOperation) != "v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" ||
		first.ExpectedEpochs.PendingObservation != nil ||
		stringOr(first.ExpectedEpochs.PendingLockRelease) != "v1-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" || !first.LeaseContinuityLost {
		t.Errorf("first schema's epochs = %+v, continuity lost %t", first.ExpectedEpochs, first.LeaseContinuityLost)
	}
	var reasons []string
	for _, event := range first.Events {
		reasons = append(reasons, event.Reason)
		if stringOr(event.Code) == "wrong_uid_code" {
			t.Error("an Event for another UID of the same name was projected")
		}
	}
	if strings.Join(reasons, ",") != "OperationFailed,PlanStale,LeaseContinuityLost" {
		t.Errorf("first schema's Events = %v", reasons)
	}
	if second.Phase != nil || second.Failure == nil || second.Failure.Code != nil ||
		second.ExpectedEpochs.ActiveOperation != nil || len(second.Jobs) != 1 || second.Jobs[0].Operation != nil {
		t.Errorf("second schema kept a malformed value: %+v", second)
	}
	if len(projection.Leases) != 2 {
		t.Fatalf("projected %d Leases, want the operator's two", len(projection.Leases))
	}
	if stringOr(projection.Leases[0].Epoch) != "v1-cccccccccccccccccccccccccccccccc" || !projection.Leases[0].HolderPresent ||
		!projection.Leases[0].HolderHashShape {
		t.Errorf("first Lease = %+v", projection.Leases[0])
	}
	if projection.Leases[1].Epoch != nil || !projection.Leases[1].HolderPresent || projection.Leases[1].HolderHashShape {
		t.Errorf("second Lease = %+v", projection.Leases[1])
	}
}

func TestEmitScannedWithholdsWhatItCannotClear(t *testing.T) {
	t.Parallel()
	projection := projectCleanupDiagnostic(cleanupFixtures(t))
	safe, err := newCredentialScanner("NEVER_MATCH_CLEANUP_DIAGNOSTIC")
	if err != nil {
		t.Fatal(err)
	}
	matching, err := newCredentialScanner(diagnosticSentinel)
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		scanner    credentialScanner
		projection any
	}{
		"no protected pattern": {credentialScanner{}, projection},
		"a match":              {matching, map[string]string{"unsafe": diagnosticSentinel}},
		"nothing to encode":    {safe, func() {}},
	} {
		var output bytes.Buffer
		emitScanned(&output, test.scanner, test.projection)
		if output.String() != cleanupSuppressed+"\n" {
			t.Errorf("%s emitted %q, want the fixed suppression line alone", name, output.String())
		}
	}
	var output bytes.Buffer
	emitScanned(&output, safe, projection)
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	encoded, err := canonicalJSON(projection)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0] != "e2e data plane: credential-safe reconciliation diagnostic projection" ||
		lines[1] != string(encoded) {
		t.Fatalf("a safe projection emitted %q", output.String())
	}
}

func TestSchemaWaitReportBoundsConditionMessages(t *testing.T) {
	t.Parallel()
	schema := &ptahv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-postgresql", Generation: 3},
		Status: ptahv1alpha1.PtahSchemaStatus{
			Phase: ptahv1alpha1.PhaseBlocked,
			Plan:  &ptahv1alpha1.CurrentPlanStatus{Name: "plan", Destructive: true},
			Conditions: []metav1.Condition{{
				Type: "ApprovalRequired", Status: metav1.ConditionFalse, Reason: "DestructiveChangesDisabled",
				Message: strings.Repeat("é", 300),
			}},
		},
	}
	report := schemaWaitReport(schema)
	if report.Plan == nil || !report.Plan.Destructive || report.ActiveOperation != nil {
		t.Fatalf("report = %+v", report)
	}
	if got := len([]rune(report.Conditions[0].Message)); got != conditionMessageBound {
		t.Fatalf("the message kept %d characters, want %d", got, conditionMessageBound)
	}
}

func TestTimelineOrdersJobsByCreationAndChain(t *testing.T) {
	t.Parallel()
	timeline := timelineSince([]observedJob{
		{UID: "4", Name: "plan", Operation: "plan", Created: "2026-09-01T00:00:01Z"},
		{UID: "3", Name: "observe", Operation: "observe", Created: "2026-09-01T00:00:01Z"},
		{UID: "1", Name: "resolve", Operation: "resolve", Created: "2026-09-01T00:00:00Z"},
		{UID: "2", Name: "odd", Operation: "unknown", Created: "2026-09-01T00:00:01Z"},
	})
	var order []string
	for _, job := range timeline.Jobs {
		order = append(order, job.UID)
	}
	if strings.Join(order, ",") != "1,3,4,2" {
		t.Fatalf("timeline order = %v", order)
	}
	if timeline.Jobs[3].Operation != nil {
		t.Fatal("an unknown operation was printed")
	}
}
