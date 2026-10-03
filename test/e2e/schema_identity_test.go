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

func schemaReplacementFixture() (*ptahv1alpha1.PtahSchema, *ptahv1alpha1.PtahSchema, *ptahv1alpha1.PtahSchemaPlan, *ptahv1alpha1.PtahSchemaPlan) {
	old := &ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Name: "same-name", Namespace: "test", UID: "old", Generation: 1}}
	old.Status = ptahv1alpha1.PtahSchemaStatus{
		ObservedGeneration: 1, Phase: ptahv1alpha1.PhaseAwaitingApproval,
		Plan:       &ptahv1alpha1.CurrentPlanStatus{Name: "old-plan", UID: "old-plan"},
		Conditions: []metav1.Condition{{Type: "ApprovalRequired", Status: metav1.ConditionTrue, ObservedGeneration: 1}},
	}
	oldPlan := &ptahv1alpha1.PtahSchemaPlan{ObjectMeta: metav1.ObjectMeta{Name: "old-plan", Namespace: "test", UID: "old-plan"}, Spec: ptahv1alpha1.PtahSchemaPlanSpec{
		SchemaRef: ptahv1alpha1.ImmutableObjectReference{Name: old.Name, UID: old.UID}, Fingerprint: "old-fingerprint",
		ArtifactDigest: "artifact", TargetIdentityDigest: "target", CoordinationDigest: "realm", PolicyFingerprint: "policy",
		ActualStateFingerprint: "actual", DesiredStateFingerprint: "desired", VerificationPolicyUID: "verification", VerificationPolicyDigest: "verification-digest",
		StatementCount: 1, Dialect: "postgresql",
	}}
	current, currentPlan := old.DeepCopy(), oldPlan.DeepCopy()
	current.UID, currentPlan.UID, currentPlan.Name = "current", "current-plan", "current-plan"
	currentPlan.Spec.SchemaRef.UID, currentPlan.Spec.Fingerprint = current.UID, "current-fingerprint"
	current.Status.Plan.Name, current.Status.Plan.UID = currentPlan.Name, currentPlan.UID
	return old, current, oldPlan, currentPlan
}

func TestSchemaReplacementRequiresTheSameWorkWithNewIdentities(t *testing.T) {
	t.Parallel()
	old, current, oldPlan, currentPlan := schemaReplacementFixture()
	if err := replacedSchemaDecision(old, current, oldPlan, currentPlan); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahSchema, *ptahv1alpha1.PtahSchemaPlan){
		"same resource":         func(r *ptahv1alpha1.PtahSchema, _ *ptahv1alpha1.PtahSchemaPlan) { r.UID = old.UID },
		"different name":        func(r *ptahv1alpha1.PtahSchema, _ *ptahv1alpha1.PtahSchemaPlan) { r.Name = "other" },
		"different namespace":   func(r *ptahv1alpha1.PtahSchema, _ *ptahv1alpha1.PtahSchemaPlan) { r.Namespace = "other" },
		"spec edit":             func(r *ptahv1alpha1.PtahSchema, _ *ptahv1alpha1.PtahSchemaPlan) { r.Spec.Suspend = true },
		"unobserved generation": func(r *ptahv1alpha1.PtahSchema, _ *ptahv1alpha1.PtahSchemaPlan) { r.Status.ObservedGeneration = 0 },
		"no gate":               func(r *ptahv1alpha1.PtahSchema, _ *ptahv1alpha1.PtahSchemaPlan) { r.Status.Conditions = nil },
		"stale gate": func(r *ptahv1alpha1.PtahSchema, _ *ptahv1alpha1.PtahSchemaPlan) {
			r.Status.Conditions[0].ObservedGeneration = 0
		},
		"wrong plan": func(r *ptahv1alpha1.PtahSchema, _ *ptahv1alpha1.PtahSchemaPlan) { r.Status.Plan.UID = "other" },
		"old plan":   func(_ *ptahv1alpha1.PtahSchema, p *ptahv1alpha1.PtahSchemaPlan) { p.UID = oldPlan.UID },
		"old fingerprint": func(_ *ptahv1alpha1.PtahSchema, p *ptahv1alpha1.PtahSchemaPlan) {
			p.Spec.Fingerprint = oldPlan.Spec.Fingerprint
		},
		"old resource binding": func(_ *ptahv1alpha1.PtahSchema, p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.SchemaRef.UID = old.UID },
		"changed artifact":     func(_ *ptahv1alpha1.PtahSchema, p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ArtifactDigest = "other" },
		"changed target": func(_ *ptahv1alpha1.PtahSchema, p *ptahv1alpha1.PtahSchemaPlan) {
			p.Spec.TargetIdentityDigest = "other"
		},
		"changed realm":  func(_ *ptahv1alpha1.PtahSchema, p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.CoordinationDigest = "other" },
		"changed policy": func(_ *ptahv1alpha1.PtahSchema, p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.PolicyFingerprint = "other" },
		"changed actual state": func(_ *ptahv1alpha1.PtahSchema, p *ptahv1alpha1.PtahSchemaPlan) {
			p.Spec.ActualStateFingerprint = "other"
		},
		"changed desired state": func(_ *ptahv1alpha1.PtahSchema, p *ptahv1alpha1.PtahSchemaPlan) {
			p.Spec.DesiredStateFingerprint = "other"
		},
		"changed verification UID": func(_ *ptahv1alpha1.PtahSchema, p *ptahv1alpha1.PtahSchemaPlan) {
			p.Spec.VerificationPolicyUID = "other"
		},
		"changed verification content": func(_ *ptahv1alpha1.PtahSchema, p *ptahv1alpha1.PtahSchemaPlan) {
			p.Spec.VerificationPolicyDigest = "other"
		},
		"empty work":          func(_ *ptahv1alpha1.PtahSchema, p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.StatementCount = 0 },
		"changed dialect":     func(_ *ptahv1alpha1.PtahSchema, p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.Dialect = "other" },
		"changed destruction": func(_ *ptahv1alpha1.PtahSchema, p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.Destructive = true },
	} {
		t.Run(name, func(t *testing.T) {
			r, p := current.DeepCopy(), currentPlan.DeepCopy()
			mutate(r, p)
			if replacedSchemaDecision(old, r, oldPlan, p) == nil {
				t.Fatal("unrelated change passed as an identity substitution")
			}
		})
	}
	if replacedSchemaDecision(nil, current, oldPlan, currentPlan) == nil || replacedSchemaDecision(old, current, nil, currentPlan) == nil {
		t.Fatal("missing evidence passed")
	}
}

func TestSchemaIdentityApprovalRequiresTheIntendedGuard(t *testing.T) {
	t.Parallel()
	const reason = "approval schema reference does not match the plan"
	const message = `admission webhook "mapproval.operator.ptah.run" denied the request: ` + reason
	forbidden := func(message string) error {
		return apierrors.NewForbidden(schema.GroupResource{Group: "operator.ptah.run", Resource: "ptahschemaapprovals"}, "approval", errors.New(message))
	}
	if !schemaIdentityApprovalRefusal(forbidden(message), reason) {
		t.Fatal("the intended API refusal failed")
	}
	for _, err := range []error{nil, errors.New(message), forbidden(reason), forbidden("webhook timeout"), forbidden(`admission webhook "mmigrationapproval.operator.ptah.run" denied the request: ` + reason)} {
		if schemaIdentityApprovalRefusal(err, reason) {
			t.Fatalf("unrelated refusal passed: %v", err)
		}
	}
	if schemaIdentityApprovalRefusal(forbidden(message), "") || schemaIdentityApprovalRefusal(forbidden(message), "referenced plan UID does not match") {
		t.Fatal("wrong binding reason passed")
	}
}

func TestSchemaSQLClientsBindBothResourceLifetimes(t *testing.T) {
	t.Parallel()
	old, current, _, _ := schemaReplacementFixture()
	owner := func(version, kind, name string, uid types.UID) metav1.OwnerReference {
		return metav1.OwnerReference{APIVersion: version, Kind: kind, Name: name, UID: uid, Controller: ptr.To(true)}
	}
	var jobs []batchv1.Job
	var pods []corev1.Pod
	for i, resource := range []*ptahv1alpha1.PtahSchema{old, current} {
		job := batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: string(resource.UID) + "-observe", Namespace: resource.Namespace, UID: types.UID("job-" + string(resource.UID)),
			Labels: map[string]string{labelSchema: resource.Name, labelOperation: "observe"}, OwnerReferences: []metav1.OwnerReference{owner(ptahSchemaAPIVersion, "PtahSchema", resource.Name, resource.UID)}}}
		pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-pod", Namespace: resource.Namespace, UID: types.UID("pod-" + string(resource.UID)),
			Labels: map[string]string{labelSchema: resource.Name, labelOperation: "observe"}, OwnerReferences: []metav1.OwnerReference{owner("batch/v1", "Job", job.Name, job.UID)}},
			Status: corev1.PodStatus{Phase: corev1.PodSucceeded, PodIP: []string{"10.0.0.1", "10.0.0.2"}[i]}}
		jobs, pods = append(jobs, job), append(pods, pod)
	}
	clients, err := schemaSQLClients(current, jobs, pods, old)
	if err != nil || len(clients) != 2 || clients["10.0.0.1"].resourceUID != string(old.UID) || clients["10.0.0.2"].resourceUID != string(current.UID) {
		t.Fatalf("both lifetimes: %v %v", clients, err)
	}
	if _, err := schemaSQLClients(current, jobs, pods); err == nil {
		t.Fatal("undeclared predecessor passed")
	}
	if _, err := schemaSQLClients(current, jobs, pods, current); err == nil {
		t.Fatal("duplicate resource identity passed")
	}
	for name, mutate := range map[string]func(*batchv1.Job, *corev1.Pod){
		"wrong family":    func(j *batchv1.Job, _ *corev1.Pod) { j.OwnerReferences[0].Kind = "PtahMigration" },
		"wrong owner UID": func(j *batchv1.Job, _ *corev1.Pod) { j.OwnerReferences[0].UID = "other" },
		"wrong Pod owner": func(_ *batchv1.Job, p *corev1.Pod) { p.OwnerReferences[0].UID = "other" },
		"wrong namespace": func(_ *batchv1.Job, p *corev1.Pod) { p.Namespace = "other" },
		"wrong label":     func(_ *batchv1.Job, p *corev1.Pod) { p.Labels[labelSchema] = "other" },
		"IP reuse":        func(_ *batchv1.Job, p *corev1.Pod) { p.Status.PodIP = pods[1].Status.PodIP },
		"nonterminal":     func(_ *batchv1.Job, p *corev1.Pod) { p.Status.Phase = corev1.PodRunning },
	} {
		t.Run(name, func(t *testing.T) {
			j, p := jobs[0].DeepCopy(), pods[0].DeepCopy()
			mutate(j, p)
			if _, err := schemaSQLClients(current, []batchv1.Job{*j, jobs[1]}, []corev1.Pod{*p, pods[1]}, old); err == nil {
				t.Fatal("ambiguous or unrelated attribution passed")
			}
		})
	}
}
