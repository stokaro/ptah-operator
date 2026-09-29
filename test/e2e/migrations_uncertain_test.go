package e2e

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

const (
	muDigest    = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	muOperation = "sha256:77"
)

var muInstant = metav1.NewTime(time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC))

func muCondition(kind string, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{Type: kind, Status: status, Reason: reason, Message: message}
}

// muRefused is the status an Apply nobody could read leaves.
func muRefused() *ptahv1alpha1.PtahMigration {
	return &ptahv1alpha1.PtahMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-uncertain-postgresql", UID: "u-migration"},
		Status: ptahv1alpha1.PtahMigrationStatus{
			Phase:   ptahv1alpha1.MigrationPhaseBlocked,
			LastRun: &ptahv1alpha1.MigrationRunStatus{Outcome: ptahv1alpha1.MigrationRunOutcomeUnknown},
			UnresolvedRun: &ptahv1alpha1.UnresolvedMigrationRunStatus{
				Outcome: ptahv1alpha1.MigrationRunOutcomeUnknown, OperationID: muOperation,
				JobName: "apply-job", JobUID: "u-apply", TargetIdentityDigest: muDigest, RecordedAt: muInstant,
			},
			Conditions: []metav1.Condition{
				muCondition("Blocked", metav1.ConditionTrue, "ApplyOutcomeUnknown", ""),
				muCondition("Ready", metav1.ConditionFalse, "ApplyOutcomeUnknown", ""),
			},
		},
	}
}

func TestUncertainApplyClaimed(t *testing.T) {
	t.Parallel()
	claimed := func() *ptahv1alpha1.PtahMigration {
		return &ptahv1alpha1.PtahMigration{Status: ptahv1alpha1.PtahMigrationStatus{
			ActiveOperation: &ptahv1alpha1.MigrationOperationStatus{
				Type: ptahv1alpha1.MigrationOperationApply, JobName: "apply-job", JobUID: "u-apply",
			},
		}}
	}
	if name, uid, ok := uncertainApplyClaimed(claimed()); !ok || name != "apply-job" || uid != "u-apply" {
		t.Fatalf("the claim was read as %q %q %t", name, uid, ok)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigration){
		"no operation": func(m *ptahv1alpha1.PtahMigration) { m.Status.ActiveOperation = nil },
		"a History": func(m *ptahv1alpha1.PtahMigration) {
			m.Status.ActiveOperation.Type = ptahv1alpha1.MigrationOperationHistory
		},
		"no Job name": func(m *ptahv1alpha1.PtahMigration) { m.Status.ActiveOperation.JobName = "" },
		"no Job UID":  func(m *ptahv1alpha1.PtahMigration) { m.Status.ActiveOperation.JobUID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			migration := claimed()
			mutate(migration)
			if _, _, ok := uncertainApplyClaimed(migration); ok {
				t.Fatalf("a claim with %s was accepted", name)
			}
		})
	}
}

func TestUncertainRunRefused(t *testing.T) {
	t.Parallel()
	if err := uncertainRunRefused(muRefused().Status); err != nil {
		t.Fatalf("the refusal was not accepted: %v", err)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigrationStatus){
		"between cycles":    func(s *ptahv1alpha1.PtahMigrationStatus) { s.Phase = ptahv1alpha1.MigrationPhaseReading },
		"no last run":       func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun = nil },
		"a failed last run": func(s *ptahv1alpha1.PtahMigrationStatus) { s.LastRun.Outcome = ptahv1alpha1.MigrationRunOutcomeFailed },
		"an operation": func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{}
		},
		"a plan":              func(s *ptahv1alpha1.PtahMigrationStatus) { s.Plan = &ptahv1alpha1.ImmutableObjectReference{Name: "p"} },
		"no conditions":       func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions = nil },
		"blocked for a realm": func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions[0].Reason = "RealmConflict" },
		"not blocked":         func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions[0].Status = metav1.ConditionFalse },
		"called ready":        func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions[1].Status = metav1.ConditionTrue },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			migration := muRefused()
			mutate(&migration.Status)
			if uncertainRunRefused(migration.Status) == nil {
				t.Fatalf("a refusal with %s was accepted", name)
			}
		})
	}
}

func TestUnresolvedRunRecorded(t *testing.T) {
	t.Parallel()
	if err := unresolvedRunRecorded(muRefused().Status, "apply-job", "u-apply"); err != nil {
		t.Fatalf("the record was not accepted: %v", err)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.UnresolvedMigrationRunStatus){
		"another outcome": func(r *ptahv1alpha1.UnresolvedMigrationRunStatus) {
			r.Outcome = ptahv1alpha1.MigrationRunOutcomePartial
		},
		"another Job":        func(r *ptahv1alpha1.UnresolvedMigrationRunStatus) { r.JobName = "apply-job-2" },
		"another Job UID":    func(r *ptahv1alpha1.UnresolvedMigrationRunStatus) { r.JobUID = "u-apply-2" },
		"no Job UID":         func(r *ptahv1alpha1.UnresolvedMigrationRunStatus) { r.JobUID = "" },
		"no target identity": func(r *ptahv1alpha1.UnresolvedMigrationRunStatus) { r.TargetIdentityDigest = "" },
		"a malformed target": func(r *ptahv1alpha1.UnresolvedMigrationRunStatus) { r.TargetIdentityDigest = "sha256:ff" },
		"no record time":     func(r *ptahv1alpha1.UnresolvedMigrationRunStatus) { r.RecordedAt = metav1.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			migration := muRefused()
			mutate(migration.Status.UnresolvedRun)
			if unresolvedRunRecorded(migration.Status, "apply-job", "u-apply") == nil {
				t.Fatalf("a record with %s was accepted", name)
			}
		})
	}
	t.Run("no record", func(t *testing.T) {
		t.Parallel()
		migration := muRefused()
		migration.Status.UnresolvedRun = nil
		if unresolvedRunRecorded(migration.Status, "apply-job", "u-apply") == nil {
			t.Fatal("a status with no record was accepted")
		}
	})
	t.Run("an empty Job name asked for", func(t *testing.T) {
		t.Parallel()
		migration := muRefused()
		migration.Status.UnresolvedRun.JobName = ""
		if unresolvedRunRecorded(migration.Status, "", "u-apply") == nil {
			t.Fatal("an absent Job name matched an empty one, which jq's null never does")
		}
	})
}

func TestRealmConflictBlocked(t *testing.T) {
	t.Parallel()
	status := muRefused().Status
	if realmConflictBlocked(status) {
		t.Fatal("the run's own refusal was read as the realm conflict's")
	}
	status.Conditions[0].Reason = "RealmConflict"
	if !realmConflictBlocked(status) {
		t.Fatal("the realm conflict was not read")
	}
	status.Conditions[0].Status = metav1.ConditionFalse
	if realmConflictBlocked(status) {
		t.Fatal("a released realm conflict was read as blocking")
	}
	if realmConflictBlocked(ptahv1alpha1.PtahMigrationStatus{}) {
		t.Fatal("a status with no conditions was read as blocking")
	}
}

// muCopy puts a copy of the record on the metadata, as JSON.
func muCopy(t *testing.T, migration *ptahv1alpha1.PtahMigration) {
	t.Helper()
	content, err := json.Marshal(migration.Status.UnresolvedRun)
	if err != nil {
		t.Fatal(err)
	}
	migration.Annotations = map[string]string{ptahv1alpha1.UnresolvedRunAnnotation: string(content)}
}

func TestUnresolvedRunCopied(t *testing.T) {
	t.Parallel()
	migration := muRefused()
	muCopy(t, migration)
	if !unresolvedRunCopied(migration) {
		t.Fatal("the copy of the record was not accepted")
	}
	for name, annotation := range map[string]string{
		"no copy":           "",
		"another operation": `{"operationID":"sha256:78","outcome":"Unknown","targetIdentityDigest":"` + muDigest + `"}`,
		"another outcome":   `{"operationID":"sha256:77","outcome":"Partial","targetIdentityDigest":"` + muDigest + `"}`,
		"another target":    `{"operationID":"sha256:77","outcome":"Unknown","targetIdentityDigest":"sha256:ff"}`,
		"no target":         `{"operationID":"sha256:77","outcome":"Unknown"}`,
		"not JSON":          `not json`,
		"a JSON string":     `"sha256:77"`,
		"a JSON array":      `[1]`,
		"a JSON true":       `true`,
		"a JSON null":       `null`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			migration := muRefused()
			migration.Annotations = map[string]string{ptahv1alpha1.UnresolvedRunAnnotation: annotation}
			if unresolvedRunCopied(migration) {
				t.Fatalf("a copy with %s was accepted", name)
			}
		})
	}
	t.Run("neither record nor copy", func(t *testing.T) {
		t.Parallel()
		// jq compared null with null, which is equal: with no record the copy
		// names nothing either, and the reading holds. The row reads the
		// record first and refuses its absence there.
		migration := muRefused()
		migration.Status.UnresolvedRun = nil
		if !unresolvedRunCopied(migration) {
			t.Fatal("no record and no copy did not compare as jq compared them")
		}
	})
}

func TestUnresolvedRunStands(t *testing.T) {
	t.Parallel()
	migration := muRefused()
	muCopy(t, migration)
	if !unresolvedRunStands(migration, muOperation) {
		t.Fatal("the standing record was not accepted")
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigration){
		"no record":         func(m *ptahv1alpha1.PtahMigration) { m.Status.UnresolvedRun = nil },
		"another operation": func(m *ptahv1alpha1.PtahMigration) { m.Status.UnresolvedRun.OperationID = "sha256:78" },
		"no copy":           func(m *ptahv1alpha1.PtahMigration) { m.Annotations = nil },
		"an empty copy":     func(m *ptahv1alpha1.PtahMigration) { m.Annotations[ptahv1alpha1.UnresolvedRunAnnotation] = "" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			migration := muRefused()
			muCopy(t, migration)
			mutate(migration)
			if unresolvedRunStands(migration, muOperation) {
				t.Fatalf("a record with %s was accepted", name)
			}
		})
	}
}

// muDecode reads a document the self-test judged into a migration.
func muDecode(t *testing.T, document string) *ptahv1alpha1.PtahMigration {
	t.Helper()
	migration := &ptahv1alpha1.PtahMigration{}
	if err := json.Unmarshal([]byte(document), migration); err != nil {
		t.Fatalf("decode %s: %v", document, err)
	}
	return migration
}

// TestRunAcknowledged holds runAcknowledged to every reading
// the migration filter self-test held migration-run-acknowledged.jq
// to, judged with operation=sha256:77, acknowledgment=run-accounted-for and
// person=e2e-acknowledger.
func TestRunAcknowledged(t *testing.T) {
	t.Parallel()
	judge := func(document string) bool {
		return runAcknowledged(muDecode(t, document), muOperation, "run-accounted-for", "e2e-acknowledger")
	}
	if !judge(`{"metadata":{"annotations":{"operator.ptah.run/last-spec-writer-username":"e2e"}},
 "status":{"resolvedRun":{"operationID":"sha256:77","outcome":"Unknown","resolution":"Acknowledged",
  "acknowledgmentRef":{"name":"run-accounted-for","uid":"u-ack"},
  "acknowledgedBy":{"username":"e2e-acknowledger","groups":["e2e:acknowledgers"]},
  "resolvedAt":"2026-09-28T10:00:00Z"}}}`) {
		t.Fatal("the record settled in the acknowledger's name was refused")
	}
	for name, document := range map[string]string{
		"the record still stands": `{"status":{"unresolvedRun":{"operationID":"sha256:77","outcome":"Unknown"},
 "resolvedRun":{"operationID":"sha256:77","resolution":"Acknowledged",
  "acknowledgmentRef":{"name":"run-accounted-for"},"acknowledgedBy":{"username":"e2e-acknowledger"}}}}`,
		"the copy a restore keeps still stands": `{"metadata":{"annotations":{"operator.ptah.run/unresolved-run":"{\"operationID\":\"sha256:77\"}"}},
 "status":{"resolvedRun":{"operationID":"sha256:77","resolution":"Acknowledged",
  "acknowledgmentRef":{"name":"run-accounted-for"},"acknowledgedBy":{"username":"e2e-acknowledger"}}}}`,
		"an empty copy still stands": `{"metadata":{"annotations":{"operator.ptah.run/unresolved-run":""}},
 "status":{"resolvedRun":{"operationID":"sha256:77","resolution":"Acknowledged",
  "acknowledgmentRef":{"name":"run-accounted-for"},"acknowledgedBy":{"username":"e2e-acknowledger"}}}}`,
		"settled by a reading, in nobody's name": `{"status":{"resolvedRun":{"operationID":"sha256:77","resolution":"HistoryRead"}}}`,
		"another run settled": `{"status":{"resolvedRun":{"operationID":"sha256:78","resolution":"Acknowledged",
  "acknowledgmentRef":{"name":"run-accounted-for"},"acknowledgedBy":{"username":"e2e-acknowledger"}}}}`,
		"settled in somebody else's name": `{"status":{"resolvedRun":{"operationID":"sha256:77","resolution":"Acknowledged",
  "acknowledgmentRef":{"name":"run-accounted-for"},"acknowledgedBy":{"username":"system:serviceaccount:ptah:manager"}}}}`,
		"settled by another acknowledgment": `{"status":{"resolvedRun":{"operationID":"sha256:77","resolution":"Acknowledged",
  "acknowledgmentRef":{"name":"an-older-acknowledgment"},"acknowledgedBy":{"username":"e2e-acknowledger"}}}}`,
		"settled with nobody named": `{"status":{"resolvedRun":{"operationID":"sha256:77","resolution":"Acknowledged",
  "acknowledgmentRef":{"name":"run-accounted-for"}}}}`,
		"nothing settled at all": `{"status":{"phase":"Blocked"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if judge(document) {
				t.Fatalf("a reading where %s was accepted", name)
			}
		})
	}
}

// muAcknowledgment is an acknowledgment admission stamped, and the migration
// whose resolution names it.
func muAcknowledgment() (*ptahv1alpha1.PtahMigrationRunAcknowledgment, *ptahv1alpha1.PtahMigration) {
	acknowledgment := &ptahv1alpha1.PtahMigrationRunAcknowledgment{
		ObjectMeta: metav1.ObjectMeta{Name: "run-accounted-for", UID: "u-ack"},
		Spec: ptahv1alpha1.PtahMigrationRunAcknowledgmentSpec{
			AcknowledgedBy: ptahv1alpha1.ApprovalIdentity{Username: "e2e-acknowledger", Groups: []string{"system:authenticated", "e2e:acknowledgers"}},
		},
		Status: ptahv1alpha1.PtahMigrationRunAcknowledgmentStatus{Conditions: []metav1.Condition{
			muCondition("Consumed", metav1.ConditionTrue, "RunAcknowledged", ""),
		}},
	}
	migration := &ptahv1alpha1.PtahMigration{Status: ptahv1alpha1.PtahMigrationStatus{
		ResolvedRun: &ptahv1alpha1.ResolvedMigrationRunStatus{
			OperationID: muOperation, Resolution: ptahv1alpha1.MigrationRunResolvedByAcknowledgment,
			AcknowledgmentRef: &ptahv1alpha1.ImmutableObjectReference{Name: "run-accounted-for", UID: "u-ack"},
			AcknowledgedBy:    &ptahv1alpha1.ApprovalIdentity{Username: "e2e-acknowledger"},
			ResolvedAt:        muInstant,
		},
	}}
	return acknowledgment, migration
}

func TestAcknowledgmentNamesResolution(t *testing.T) {
	t.Parallel()
	acknowledgment, migration := muAcknowledgment()
	if !acknowledgmentNamesResolution(acknowledgment, migration, "e2e-acknowledger", "e2e:acknowledgers") {
		t.Fatal("the acknowledgment the resolution names was refused")
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigrationRunAcknowledgment, *ptahv1alpha1.PtahMigration){
		"stamped for somebody else": func(a *ptahv1alpha1.PtahMigrationRunAcknowledgment, _ *ptahv1alpha1.PtahMigration) {
			a.Spec.AcknowledgedBy.Username = "someone-else"
		},
		"stamped without the group": func(a *ptahv1alpha1.PtahMigrationRunAcknowledgment, _ *ptahv1alpha1.PtahMigration) {
			a.Spec.AcknowledgedBy.Groups = []string{"system:authenticated"}
		},
		"no resolution": func(_ *ptahv1alpha1.PtahMigrationRunAcknowledgment, m *ptahv1alpha1.PtahMigration) {
			m.Status.ResolvedRun = nil
		},
		"a resolution naming no acknowledgment": func(_ *ptahv1alpha1.PtahMigrationRunAcknowledgment, m *ptahv1alpha1.PtahMigration) {
			m.Status.ResolvedRun.AcknowledgmentRef = nil
		},
		"a resolution naming another acknowledgment": func(_ *ptahv1alpha1.PtahMigrationRunAcknowledgment, m *ptahv1alpha1.PtahMigration) {
			m.Status.ResolvedRun.AcknowledgmentRef.UID = "u-other"
		},
		"a resolution in nobody's name": func(_ *ptahv1alpha1.PtahMigrationRunAcknowledgment, m *ptahv1alpha1.PtahMigration) {
			m.Status.ResolvedRun.AcknowledgedBy = nil
		},
		"a resolution in another name": func(_ *ptahv1alpha1.PtahMigrationRunAcknowledgment, m *ptahv1alpha1.PtahMigration) {
			m.Status.ResolvedRun.AcknowledgedBy.Username = "someone-else"
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			acknowledgment, migration := muAcknowledgment()
			mutate(acknowledgment, migration)
			if acknowledgmentNamesResolution(acknowledgment, migration, "e2e-acknowledger", "e2e:acknowledgers") {
				t.Fatalf("an acknowledgment with %s was accepted", name)
			}
		})
	}
}

func TestAcknowledgmentConsumed(t *testing.T) {
	t.Parallel()
	acknowledgment, _ := muAcknowledgment()
	if !acknowledgmentConsumed(acknowledgment) {
		t.Fatal("a consumed acknowledgment was refused")
	}
	acknowledgment.Status.Conditions[0].Status = metav1.ConditionFalse
	if acknowledgmentConsumed(acknowledgment) {
		t.Fatal("an acknowledgment not consumed was accepted")
	}
	acknowledgment.Status.Conditions = []metav1.Condition{muCondition("Stale", metav1.ConditionTrue, "RunNotUnresolved", "")}
	if acknowledgmentConsumed(acknowledgment) {
		t.Fatal("a stale acknowledgment was accepted as consumed")
	}
}

func TestHistoryReadAfter(t *testing.T) {
	t.Parallel()
	after := muInstant.Time
	read := func(observed time.Time) *ptahv1alpha1.PtahMigration {
		return &ptahv1alpha1.PtahMigration{Status: ptahv1alpha1.PtahMigrationStatus{
			History: &ptahv1alpha1.MigrationHistoryStatus{ObservedAt: metav1.NewTime(observed)},
		}}
	}
	if !historyReadAfter(read(after.Add(time.Second)), after) {
		t.Fatal("a reading a second later was refused")
	}
	for name, migration := range map[string]*ptahv1alpha1.PtahMigration{
		"the same second":        read(after.Add(500 * time.Millisecond)),
		"before the resolution":  read(after.Add(-time.Minute)),
		"no reading":             {},
		"a reading with no time": {Status: ptahv1alpha1.PtahMigrationStatus{History: &ptahv1alpha1.MigrationHistoryStatus{}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if historyReadAfter(migration, after) {
				t.Fatalf("%s was taken for a later reading", name)
			}
		})
	}
}

func TestLatePlanPublished(t *testing.T) {
	t.Parallel()
	waiting := &ptahv1alpha1.PtahMigration{Status: ptahv1alpha1.PtahMigrationStatus{
		Phase: ptahv1alpha1.MigrationPhaseAwaitingApproval, Plan: &ptahv1alpha1.ImmutableObjectReference{Name: "ptah-mplan-1"},
	}}
	if plan, ok := latePlanPublished(waiting); !ok || plan != "ptah-mplan-1" {
		t.Fatalf("the published plan was read as %q %t", plan, ok)
	}
	for name, migration := range map[string]*ptahv1alpha1.PtahMigration{
		"still reading": {Status: ptahv1alpha1.PtahMigrationStatus{
			Phase: ptahv1alpha1.MigrationPhaseReading, Plan: &ptahv1alpha1.ImmutableObjectReference{Name: "ptah-mplan-1"},
		}},
		"no plan": {Status: ptahv1alpha1.PtahMigrationStatus{Phase: ptahv1alpha1.MigrationPhaseAwaitingApproval}},
		"a plan with no name": {Status: ptahv1alpha1.PtahMigrationStatus{
			Phase: ptahv1alpha1.MigrationPhaseAwaitingApproval, Plan: &ptahv1alpha1.ImmutableObjectReference{},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, ok := latePlanPublished(migration); ok {
				t.Fatalf("%s was taken for a published plan", name)
			}
		})
	}
}

func TestLateApplyClaimed(t *testing.T) {
	t.Parallel()
	window := metav1.NewTime(muInstant.Add(5 * time.Minute))
	claimed := func() *ptahv1alpha1.PtahMigration {
		return &ptahv1alpha1.PtahMigration{Status: ptahv1alpha1.PtahMigrationStatus{
			ActiveOperation: &ptahv1alpha1.MigrationOperationStatus{
				Type: ptahv1alpha1.MigrationOperationApply, JobName: "apply-job", JobUID: "u-apply", DispatchNotAfter: &window,
			},
		}}
	}
	claim, ok := lateApplyClaimed(claimed())
	if !ok || claim.jobName != "apply-job" || claim.jobUID != "u-apply" || !claim.dispatchNotAfter.Equal(window.Time) {
		t.Fatalf("the claim was read as %+v %t", claim, ok)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.MigrationOperationStatus){
		"no window":     func(a *ptahv1alpha1.MigrationOperationStatus) { a.DispatchNotAfter = nil },
		"a zero window": func(a *ptahv1alpha1.MigrationOperationStatus) { a.DispatchNotAfter = &metav1.Time{} },
		"no Job UID":    func(a *ptahv1alpha1.MigrationOperationStatus) { a.JobUID = "" },
		"no Job name":   func(a *ptahv1alpha1.MigrationOperationStatus) { a.JobName = "" },
		"not an Apply":  func(a *ptahv1alpha1.MigrationOperationStatus) { a.Type = ptahv1alpha1.MigrationOperationVerify },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			migration := claimed()
			mutate(migration.Status.ActiveOperation)
			if _, ok := lateApplyClaimed(migration); ok {
				t.Fatalf("a claim with %s was accepted", name)
			}
		})
	}
}

func TestAnyPodPlaced(t *testing.T) {
	t.Parallel()
	held := corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}
	placed := corev1.Pod{Spec: corev1.PodSpec{NodeName: "kind-worker"}, Status: corev1.PodStatus{Phase: corev1.PodPending}}
	if anyPodPlaced(nil) || anyPodPlaced([]corev1.Pod{held}) {
		t.Fatal("Pods on no node were read as placed")
	}
	if !anyPodPlaced([]corev1.Pod{held, placed}) {
		t.Fatal("a Pod on a node was not read as placed")
	}
}

func TestLateRefusalRecorded(t *testing.T) {
	t.Parallel()
	refused := func() ptahv1alpha1.PtahMigrationStatus {
		status := muRefused().Status
		status.Conditions[0].Message = "The Apply's runner refused to start: dispatch_deadline_expired"
		return status
	}
	if !lateRefusalRecorded(refused(), "u-apply") {
		t.Fatal("the runner's refusal was not read")
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigrationStatus){
		"no record":               func(s *ptahv1alpha1.PtahMigrationStatus) { s.UnresolvedRun = nil },
		"a record of another Job": func(s *ptahv1alpha1.PtahMigrationStatus) { s.UnresolvedRun.JobUID = "u-replacement" },
		"another refusal":         func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions[0].Message = "the Job failed" },
		"blocked for a realm":     func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions[0].Reason = "RealmConflict" },
		"no longer blocked":       func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions[0].Status = metav1.ConditionFalse },
		"the words on Ready alone": func(s *ptahv1alpha1.PtahMigrationStatus) {
			s.Conditions[0].Message = ""
			s.Conditions[1].Message = "dispatch_deadline_expired"
		},
		"no conditions": func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions = nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			status := refused()
			mutate(&status)
			if lateRefusalRecorded(status, "u-apply") {
				t.Fatalf("a reading with %s was accepted", name)
			}
		})
	}
}

func TestRestoreInSyncApplied(t *testing.T) {
	t.Parallel()
	settled := ptahv1alpha1.PtahMigrationStatus{
		Phase: ptahv1alpha1.MigrationPhaseInSync, LastRun: &ptahv1alpha1.MigrationRunStatus{Outcome: ptahv1alpha1.MigrationRunOutcomeApplied},
	}
	if !restoreInSyncApplied(settled) {
		t.Fatal("an applied, settled migration was refused")
	}
	unsettled := settled
	unsettled.Phase = ptahv1alpha1.MigrationPhaseReading
	upToDate := settled
	upToDate.LastRun = &ptahv1alpha1.MigrationRunStatus{Outcome: ptahv1alpha1.MigrationRunOutcomeUpToDate}
	noRun := settled
	noRun.LastRun = nil
	for name, status := range map[string]ptahv1alpha1.PtahMigrationStatus{
		"between cycles": unsettled, "an up-to-date run": upToDate, "no run": noRun,
	} {
		if restoreInSyncApplied(status) {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestDecisionOnNewPlan(t *testing.T) {
	t.Parallel()
	asking := func() ptahv1alpha1.PtahMigrationStatus {
		return ptahv1alpha1.PtahMigrationStatus{
			Phase: ptahv1alpha1.MigrationPhaseReading,
			Plan:  &ptahv1alpha1.ImmutableObjectReference{Name: "ptah-mplan-2"},
			Conditions: []metav1.Condition{
				muCondition("ApprovalRequired", metav1.ConditionTrue, "AwaitingApproval", ""),
			},
		}
	}
	// The phase is not asked for: it moves on every read the resource makes.
	if plan, ok := decisionOnNewPlan(asking(), "ptah-mplan-1"); !ok || plan != "ptah-mplan-2" {
		t.Fatalf("the decision was read as %q %t", plan, ok)
	}
	if _, ok := decisionOnNewPlan(asking(), ""); !ok {
		t.Fatal("a first decision was refused")
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigrationStatus){
		"the previous plan":     func(s *ptahv1alpha1.PtahMigrationStatus) { s.Plan.Name = "ptah-mplan-1" },
		"no plan":               func(s *ptahv1alpha1.PtahMigrationStatus) { s.Plan = nil },
		"a plan with no name":   func(s *ptahv1alpha1.PtahMigrationStatus) { s.Plan.Name = "" },
		"no approval asked for": func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions[0].Status = metav1.ConditionFalse },
		"approval for a reason": func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions[0].Reason = "Blocked" },
		"no conditions":         func(s *ptahv1alpha1.PtahMigrationStatus) { s.Conditions = nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			status := asking()
			mutate(&status)
			if _, ok := decisionOnNewPlan(status, "ptah-mplan-1"); ok {
				t.Fatalf("a reading with %s was accepted", name)
			}
		})
	}
}

func TestPlanVersionList(t *testing.T) {
	t.Parallel()
	plan := &ptahv1alpha1.PtahMigrationPlan{Spec: ptahv1alpha1.PtahMigrationPlanSpec{
		Migrations: []ptahv1alpha1.PlannedMigration{{Version: 2}, {Version: 3}},
	}}
	if got := planVersionList(plan); got != "2 3" {
		t.Fatalf("planVersionList = %q", got)
	}
	if got := planVersionList(&ptahv1alpha1.PtahMigrationPlan{}); got != "" {
		t.Fatalf("an empty plan lists %q", got)
	}
}

func TestRestoreRefused(t *testing.T) {
	t.Parallel()
	refused := func() ptahv1alpha1.PtahMigrationStatus {
		return ptahv1alpha1.PtahMigrationStatus{LastRun: &ptahv1alpha1.MigrationRunStatus{
			Outcome: ptahv1alpha1.MigrationRunOutcomeFailed, JobUID: "u-apply",
			Message: restoreSelectionRefusal + "; approve a plan of the history the database holds now",
		}}
	}
	if !restoreRefused(refused(), "u-apply") {
		t.Fatal("the selection refusal was not read")
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.MigrationRunStatus){
		"another run":    func(r *ptahv1alpha1.MigrationRunStatus) { r.JobUID = "u-other" },
		"no Job":         func(r *ptahv1alpha1.MigrationRunStatus) { r.JobUID = "" },
		"an applied run": func(r *ptahv1alpha1.MigrationRunStatus) { r.Outcome = ptahv1alpha1.MigrationRunOutcomeApplied },
		"a first migration failing": func(r *ptahv1alpha1.MigrationRunStatus) {
			r.Message = "migration 2 failed: column already exists"
		},
		"the words later in the message": func(r *ptahv1alpha1.MigrationRunStatus) {
			r.Message = "note: " + restoreSelectionRefusal
		},
		"another selection": func(r *ptahv1alpha1.MigrationRunStatus) {
			r.Message = strings.Replace(restoreSelectionRefusal, "[2 3]", "[3]", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			status := refused()
			mutate(status.LastRun)
			if restoreRefused(status, "u-apply") {
				t.Fatalf("a run with %s was accepted", name)
			}
		})
	}
	if restoreRefused(ptahv1alpha1.PtahMigrationStatus{}, "u-apply") {
		t.Fatal("a status with no run was accepted")
	}
}

func TestRestoredHistoryAppliedRequiresANewCompletedSelection(t *testing.T) {
	t.Parallel()
	settled := func() *ptahv1alpha1.PtahMigration {
		return &ptahv1alpha1.PtahMigration{
			ObjectMeta: metav1.ObjectMeta{Generation: 2},
			Status: ptahv1alpha1.PtahMigrationStatus{
				ObservedGeneration: 2,
				Phase:              ptahv1alpha1.MigrationPhaseInSync,
				LastRun: &ptahv1alpha1.MigrationRunStatus{
					Outcome: ptahv1alpha1.MigrationRunOutcomeApplied, JobName: "fresh-apply", JobUID: "u-fresh",
					AppliedVersions: []int64{2, 3}, FinishedAt: &muInstant,
				},
			},
		}
	}
	if !restoredHistoryApplied(settled(), "u-initial", "u-refused") {
		t.Fatal("the newly approved completed selection was refused")
	}
	reordered := settled()
	reordered.Status.LastRun.AppliedVersions = []int64{3, 2}
	if !restoredHistoryApplied(reordered, "u-initial", "u-refused") {
		t.Fatal("the applied-version set was treated as an ordered sequence")
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigration){
		"no generation":        func(m *ptahv1alpha1.PtahMigration) { m.Generation = 0 },
		"stale generation":     func(m *ptahv1alpha1.PtahMigration) { m.Status.ObservedGeneration-- },
		"not converged":        func(m *ptahv1alpha1.PtahMigration) { m.Status.Phase = ptahv1alpha1.MigrationPhaseApplying },
		"no run":               func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun = nil },
		"failed run":           func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.Outcome = ptahv1alpha1.MigrationRunOutcomeFailed },
		"no Job name":          func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.JobName = "" },
		"no Job UID":           func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.JobUID = "" },
		"initial run":          func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.JobUID = "u-initial" },
		"refused run":          func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.JobUID = "u-refused" },
		"no completion time":   func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.FinishedAt = nil },
		"zero completion time": func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.FinishedAt = &metav1.Time{} },
		"initial selection":    func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.AppliedVersions = []int64{1, 2} },
		"stale selection":      func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.AppliedVersions = []int64{3} },
		"no applied versions":  func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.AppliedVersions = nil },
		"extra version":        func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.AppliedVersions = []int64{1, 2, 3} },
		"repeated version":     func(m *ptahv1alpha1.PtahMigration) { m.Status.LastRun.AppliedVersions = []int64{2, 2} },
		"active operation": func(m *ptahv1alpha1.PtahMigration) {
			m.Status.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{}
		},
		"unresolved run": func(m *ptahv1alpha1.PtahMigration) {
			m.Status.UnresolvedRun = &ptahv1alpha1.UnresolvedMigrationRunStatus{}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reading := settled()
			mutate(reading)
			if restoredHistoryApplied(reading, "u-initial", "u-refused") {
				t.Fatal("accepted an invalid restored-history result")
			}
		})
	}
	if restoredHistoryApplied(nil, "u-initial", "u-refused") ||
		restoredHistoryApplied(settled(), "", "u-refused") ||
		restoredHistoryApplied(settled(), "u-initial", "") ||
		restoredHistoryApplied(settled(), "same", "same") {
		t.Fatal("accepted incomplete or ambiguous prior-run identities")
	}
}

func TestRestoreRevisionsQuery(t *testing.T) {
	t.Parallel()
	if query := restoreRevisionsQuery("postgresql"); !strings.Contains(query, "string_agg(version::text, ','") {
		t.Fatalf("the PostgreSQL query does not join the rows: %s", query)
	}
	if query := restoreRevisionsQuery("mysql"); !strings.Contains(query, "GROUP_CONCAT(version ORDER BY version SEPARATOR ',')") {
		t.Fatalf("the MySQL query does not join the rows: %s", query)
	}
}

func TestLateDispatchReportCarriesStateNotRows(t *testing.T) {
	t.Parallel()
	migration := muRefused()
	migration.Status.Conditions[0].Message = strings.Repeat("x", 300)
	start := metav1.NewTime(muInstant.Time)
	jobs := []batchv1.Job{{
		ObjectMeta: metav1.ObjectMeta{Name: "apply-job", Labels: map[string]string{labelOperation: "apply"}},
		Status: batchv1.JobStatus{Active: 1, StartTime: &start, Conditions: []batchv1.JobCondition{
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded"},
		}},
	}}
	pods := []corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Name: "apply-pod"},
		Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable"},
			{Type: corev1.PodInitialized, Status: corev1.ConditionTrue},
		}},
	}}
	report := lateDispatchReport(migration, jobs, pods, []string{"kind-worker"})
	content, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, want := range []string{
		`"phase":"Blocked"`, `"unresolvedRun":true`, `"operation":"apply"`, `"Failed=True(DeadlineExceeded)"`,
		`"PodScheduled:Unschedulable"`, `"gateOpenOn":["kind-worker"]`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the report lacks %s: %s", want, text)
		}
	}
	if strings.Contains(text, "Initialized") {
		t.Error("the report names a Pod condition that was met")
	}
	if strings.Contains(text, strings.Repeat("x", 161)) {
		t.Error("the report carries a condition message longer than 160 bytes")
	}
	if absent := lateDispatchReport(nil, nil, nil, nil); absent["migration"] != nil {
		t.Errorf("a migration that could not be read was reported: %v", absent["migration"])
	}
}
