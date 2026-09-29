package e2e

import (
	"errors"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func migrationReplacementFixture() (*ptahv1alpha1.PtahMigration, *ptahv1alpha1.PtahMigration, *ptahv1alpha1.PtahMigrationPlan, *ptahv1alpha1.PtahMigrationPlan) {
	old := &ptahv1alpha1.PtahMigration{ObjectMeta: metav1.ObjectMeta{Name: "same-name", Namespace: "test", UID: "old", Generation: 1}}
	old.Status = ptahv1alpha1.PtahMigrationStatus{
		ObservedGeneration: 1, Phase: ptahv1alpha1.MigrationPhaseAwaitingApproval,
		Plan:       &ptahv1alpha1.ImmutableObjectReference{Name: "old-plan", UID: "old-plan"},
		Conditions: []metav1.Condition{{Type: "ApprovalRequired", Status: metav1.ConditionTrue, ObservedGeneration: 1}},
	}
	oldPlan := &ptahv1alpha1.PtahMigrationPlan{
		ObjectMeta: metav1.ObjectMeta{Name: "old-plan", Namespace: "test", UID: "old-plan"},
		Spec: ptahv1alpha1.PtahMigrationPlanSpec{
			MigrationRef: ptahv1alpha1.ImmutableObjectReference{Name: old.Name, UID: old.UID}, Fingerprint: "old-fingerprint",
			ArtifactDigest: "artifact", TargetIdentityDigest: "target", CoordinationDigest: "realm", PolicyFingerprint: "policy", HistoryFingerprint: "history",
			Migrations: []ptahv1alpha1.PlannedMigration{{Version: 1, Checksum: "checksum"}},
		},
	}
	current, currentPlan := old.DeepCopy(), oldPlan.DeepCopy()
	current.UID, currentPlan.UID, currentPlan.Name = "current", "current-plan", "current-plan"
	currentPlan.Spec.MigrationRef.UID, currentPlan.Spec.Fingerprint = current.UID, "current-fingerprint"
	current.Status.Plan.Name, current.Status.Plan.UID = currentPlan.Name, currentPlan.UID
	return old, current, oldPlan, currentPlan
}

func TestReplacementDecisionRequiresTheSameWorkUnderNewIdentities(t *testing.T) {
	t.Parallel()
	old, current, oldPlan, currentPlan := migrationReplacementFixture()
	if err := replacedMigrationDecision(old, current, oldPlan, currentPlan); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahMigration, *ptahv1alpha1.PtahMigrationPlan){
		"same resource UID":   func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) { r.UID = old.UID },
		"different name":      func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) { r.Name = "other" },
		"different namespace": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) { r.Namespace = "other" },
		"spec edit":           func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) { r.Spec.Suspend = true },
		"old generation": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) {
			r.Status.ObservedGeneration = 0
		},
		"no approval gate": func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) { r.Status.Conditions = nil },
		"wrong plan name":  func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) { r.Status.Plan.Name = "other" },
		"wrong plan UID":   func(r *ptahv1alpha1.PtahMigration, _ *ptahv1alpha1.PtahMigrationPlan) { r.Status.Plan.UID = "other" },
		"plan bound to predecessor": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.MigrationRef.UID = old.UID
		},
		"reused fingerprint": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.Fingerprint = oldPlan.Spec.Fingerprint
		},
		"changed artifact": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.ArtifactDigest = "other"
		},
		"changed target": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.TargetIdentityDigest = "other"
		},
		"changed realm": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.CoordinationDigest = "other"
		},
		"changed policy": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.PolicyFingerprint = "other"
		},
		"changed history": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.HistoryFingerprint = "other"
		},
		"changed sequence": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) {
			p.Spec.Migrations[0].Checksum = "other"
		},
		"no sequence": func(_ *ptahv1alpha1.PtahMigration, p *ptahv1alpha1.PtahMigrationPlan) { p.Spec.Migrations = nil },
	} {
		t.Run(name, func(t *testing.T) {
			r, p := current.DeepCopy(), currentPlan.DeepCopy()
			mutate(r, p)
			if err := replacedMigrationDecision(old, r, oldPlan, p); err == nil {
				t.Fatal("an unrelated change passed as a same-name identity substitution")
			}
		})
	}
	if replacedMigrationDecision(nil, current, oldPlan, currentPlan) == nil || replacedMigrationDecision(old, current, nil, currentPlan) == nil {
		t.Fatal("missing original evidence passed")
	}
}

func TestMigrationIdentityRefusalNamesTheIntendedAdmissionGuard(t *testing.T) {
	t.Parallel()
	const reason = "approval migration reference does not match the plan"
	const message = `admission webhook "mmigrationapproval.operator.ptah.run" denied the request: ` + reason
	forbidden := func(message string) error {
		return apierrors.NewForbidden(schema.GroupResource{Group: "operator.ptah.run", Resource: "ptahmigrationapprovals"}, "approval", errors.New(message))
	}
	if !migrationIdentityApprovalRefusal(forbidden(message), reason) {
		t.Fatal("the intended API refusal failed")
	}
	for _, err := range []error{nil, errors.New(message), forbidden(reason), forbidden("admission webhook timeout"), forbidden(`admission webhook "another" denied the request: ` + reason)} {
		if migrationIdentityApprovalRefusal(err, reason) {
			t.Fatalf("unrelated refusal passed: %v", err)
		}
	}
	if migrationIdentityApprovalRefusal(forbidden(message), "") || migrationIdentityApprovalRefusal(forbidden(message), "referenced plan UID does not match") {
		t.Fatal("another binding or an empty reason passed")
	}
}

func TestReplacementSQLRequiresBothResourceControls(t *testing.T) {
	t.Parallel()
	clients := map[string]operationSQLClient{
		"10.1.1.1": {resourceUID: "old", jobUID: "job-old", podUID: "pod-old", operation: "history"},
		"10.1.1.2": {resourceUID: "current", jobUID: "job-current", podUID: "pod-current", operation: "history"},
	}
	counts := map[string]int{"10.1.1.1": 17, "10.1.1.2": 17}
	if err := migrationReplacementSQLControls(clients, counts, "old", "current"); err != nil {
		t.Fatal(err)
	}
	for _, broken := range []map[string]int{nil, {"10.1.1.1": 17}, {"10.1.1.2": 17}, {"127.0.0.1": 34}, {"10.1.1.1": 17, "10.1.1.2": 0}} {
		if migrationReplacementSQLControls(clients, broken, "old", "current") == nil {
			t.Fatal("missing control SQL passed")
		}
	}
	for _, actor := range []operationSQLClient{
		{resourceUID: "unknown", jobUID: "job", podUID: "pod", operation: "history"},
		{resourceUID: "current", podUID: "pod", operation: "history"},
		{resourceUID: "current", jobUID: "job", operation: "history"},
		{resourceUID: "current", jobUID: "job", podUID: "pod", operation: "apply"},
	} {
		clients["10.1.1.2"] = actor
		if migrationReplacementSQLControls(clients, counts, "old", "current") == nil {
			t.Fatal("unattributed or mutating control passed")
		}
	}
}

func TestReplacementSQLClientsKeepBothExactOwnershipChains(t *testing.T) {
	t.Parallel()
	old, current, _, _ := migrationReplacementFixture()
	var jobs []batchv1.Job
	var pods []corev1.Pod
	for index, resource := range []*ptahv1alpha1.PtahMigration{old, current} {
		job := batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "job-" + string(resource.UID), Namespace: resource.Namespace, UID: types.UID("job-" + resource.UID),
			Labels:          map[string]string{labelMigration: resource.Name, labelOperation: "history"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: ptahSchemaAPIVersion, Kind: "PtahMigration", Name: resource.Name, UID: resource.UID, Controller: ptr.To(true)}}}}
		pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-" + string(resource.UID), Namespace: resource.Namespace, UID: types.UID("pod-" + resource.UID),
			Labels: job.Labels, OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: ptr.To(true)}}},
			Status: corev1.PodStatus{Phase: corev1.PodSucceeded, PodIP: []string{"10.1.1.1", "10.1.1.2"}[index]}}
		jobs, pods = append(jobs, job), append(pods, pod)
	}
	clients, err := migrationSQLClients(current, jobs, pods, old)
	if err != nil || len(clients) != 2 || clients["10.1.1.1"].resourceUID != string(old.UID) || clients["10.1.1.2"].resourceUID != string(current.UID) {
		t.Fatalf("replacement attribution: %v %v", clients, err)
	}
	if _, err := migrationSQLClients(current, jobs, pods); err == nil {
		t.Fatal("an undeclared predecessor was attributed to the current resource")
	}
	for _, predecessor := range []*ptahv1alpha1.PtahMigration{nil, current, {ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: old.Namespace, UID: old.UID}}} {
		if _, err := migrationSQLClients(current, jobs, pods, predecessor); err == nil {
			t.Fatal("invalid predecessor passed")
		}
	}
	pods[1].Status.PodIP = pods[0].Status.PodIP
	if _, err := migrationSQLClients(current, jobs, pods, old); err == nil {
		t.Fatal("a reused IP silently joined two resource lifetimes")
	}
}
