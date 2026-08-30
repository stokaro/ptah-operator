package admission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
)

type fixedClock struct{ value time.Time }

func (c fixedClock) Now() time.Time { return c.value }

func TestApprovalCreateStampsAuthenticatedIdentity(t *testing.T) {
	t.Parallel()

	handler, approval := readyFixture(t, true, true)
	request := requestFor(t, approval, admissionv1.Create)
	request.UserInfo = authenticationv1.UserInfo{
		Username: "alice@example.com",
		UID:      "idp-123",
		Groups:   []string{"developers", "approvers", "developers"},
	}
	response := handler.Handle(context.Background(), request)
	if !response.Allowed {
		t.Fatalf("Handle() denied valid approval: %#v", response.Result)
	}
	if len(response.Patches) == 0 {
		t.Fatal("Handle() returned no identity patch")
	}
	patchJSON, err := json.Marshal(response.Patches)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"alice@example.com", "idp-123", "approvers", "admission-uid"} {
		if !containsJSON(patchJSON, want) {
			t.Fatalf("identity patch %s does not contain %q", patchJSON, want)
		}
	}
}

func TestApprovalCreateRejectsStalePlan(t *testing.T) {
	t.Parallel()

	handler, approval := readyFixture(t, false, true)
	request := requestFor(t, approval, admissionv1.Create)
	request.UserInfo = authenticationv1.UserInfo{Username: "alice"}
	response := handler.Handle(context.Background(), request)
	if response.Allowed {
		t.Fatal("Handle() accepted a plan without committed storage")
	}
}

func TestApprovalCreateHydratesDerivedPlanBindings(t *testing.T) {
	t.Parallel()

	handler, approval := readyFixture(t, true, true)
	approval.Spec.ArtifactDigest = ""
	approval.Spec.TargetIdentityDigest = ""
	approval.Spec.ActualStateFingerprint = ""
	approval.Spec.DesiredStateFingerprint = ""
	approval.Spec.PolicyFingerprint = ""
	approval.Spec.VerificationPolicyDigest = ""
	approval.Spec.PtahVersion = ""
	approval.Spec.ExecutorImage = ""
	approval.Spec.RunnerImage = ""
	approval.Spec.RunnerProtocolVersion = 0
	request := requestFor(t, approval, admissionv1.Create)
	request.UserInfo = authenticationv1.UserInfo{Username: "alice"}

	response := handler.Handle(context.Background(), request)
	if !response.Allowed {
		t.Fatalf("Handle() denied minimal exact-plan approval: %#v", response.Result)
	}
	patchJSON, err := json.Marshal(response.Patches)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"sha256:artifact", "sha256:target", "sha256:actual", "sha256:desired",
		"sha256:policy", "v0.3.0", "example.invalid/ptah@sha256:executor",
		"example.invalid/operator@sha256:runner", "runnerProtocolVersion",
	} {
		if !containsJSON(patchJSON, want) {
			t.Fatalf("hydration patch %s does not contain %q", patchJSON, want)
		}
	}
}

func TestApprovalCreateRejectsConflictingDerivedBinding(t *testing.T) {
	t.Parallel()

	handler, approval := readyFixture(t, true, true)
	approval.Spec.ArtifactDigest = "sha256:different"
	request := requestFor(t, approval, admissionv1.Create)
	request.UserInfo = authenticationv1.UserInfo{Username: "alice"}

	response := handler.Handle(context.Background(), request)
	if response.Allowed {
		t.Fatal("Handle() silently replaced a conflicting approval binding")
	}
}

func TestApprovalCreateRequiresExplicitPlanFingerprint(t *testing.T) {
	t.Parallel()

	handler, approval := readyFixture(t, true, true)
	approval.Spec.PlanFingerprint = ""
	request := requestFor(t, approval, admissionv1.Create)
	request.UserInfo = authenticationv1.UserInfo{Username: "alice"}

	response := handler.Handle(context.Background(), request)
	if response.Allowed {
		t.Fatal("Handle() accepted an approval without an explicit plan fingerprint")
	}
}

func TestApprovalUpdateRejectsSpecMutation(t *testing.T) {
	t.Parallel()

	handler, approval := readyFixture(t, true, false)
	old := approval.DeepCopy()
	approval.Spec.PlanFingerprint = "sha256:changed"
	request := requestFor(t, approval, admissionv1.Update)
	oldRaw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	request.OldObject = runtime.RawExtension{Raw: oldRaw}
	response := handler.Handle(context.Background(), request)
	if response.Allowed {
		t.Fatal("Handle() accepted an approval spec mutation")
	}
}

func readyFixture(t *testing.T, ready, mutate bool) (*ApprovalHandler, *operatorv1alpha1.PtahSchemaApproval) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))
	policyBytes := []byte("version: 1\n")
	policyDigest := fingerprint.DigestBytes(policyBytes)
	schema := &operatorv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "app", UID: "schema-uid"},
		Spec: operatorv1alpha1.PtahSchemaSpec{Desired: operatorv1alpha1.OCIArtifactSourceSpec{
			VerificationPolicyFrom: corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "policy"}, Key: "policy.yaml"},
		}},
		Status: operatorv1alpha1.PtahSchemaStatus{
			Source: operatorv1alpha1.SchemaSourceStatus{Digest: "sha256:artifact", VerificationPolicyDigest: policyDigest},
			Target: operatorv1alpha1.TargetStatus{IdentityDigest: "sha256:target", ObservedStateFingerprint: "sha256:actual"},
			Plan:   &operatorv1alpha1.CurrentPlanStatus{UID: "plan-uid", Fingerprint: "sha256:plan"},
		},
	}
	plan := &operatorv1alpha1.PtahSchemaPlan{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "app-plan", UID: "plan-uid", Generation: 1},
		Spec: operatorv1alpha1.PtahSchemaPlanSpec{
			SchemaRef:                operatorv1alpha1.ImmutableObjectReference{Name: "app", UID: "schema-uid"},
			Fingerprint:              "sha256:plan",
			ArtifactDigest:           "sha256:artifact",
			TargetIdentityDigest:     "sha256:target",
			ActualStateFingerprint:   "sha256:actual",
			DesiredStateFingerprint:  "sha256:desired",
			PolicyFingerprint:        "sha256:policy",
			VerificationPolicyDigest: policyDigest,
			PtahVersion:              "v0.3.0",
			ExecutorImage:            "example.invalid/ptah@sha256:executor",
			RunnerImage:              "example.invalid/operator@sha256:runner",
			RunnerProtocolVersion:    1,
		},
		Status: operatorv1alpha1.PtahSchemaPlanStatus{ObservedGeneration: 1},
	}
	if ready {
		plan.Status.Conditions = []metav1.Condition{{
			Type: operatorv1alpha1.ConditionPlanStorageReady, Status: metav1.ConditionTrue,
			Reason: "Published", LastTransitionTime: now,
		}}
	}
	approval := &operatorv1alpha1.PtahSchemaApproval{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "approve-app"},
		Spec: operatorv1alpha1.PtahSchemaApprovalSpec{
			SchemaRef:                operatorv1alpha1.ImmutableObjectReference{Name: "app", UID: "schema-uid"},
			PlanRef:                  operatorv1alpha1.ImmutableObjectReference{Name: "app-plan", UID: "plan-uid"},
			PlanFingerprint:          "sha256:plan",
			ArtifactDigest:           "sha256:artifact",
			TargetIdentityDigest:     "sha256:target",
			ActualStateFingerprint:   "sha256:actual",
			DesiredStateFingerprint:  "sha256:desired",
			PolicyFingerprint:        "sha256:policy",
			VerificationPolicyDigest: policyDigest,
			PtahVersion:              "v0.3.0",
			ExecutorImage:            "example.invalid/ptah@sha256:executor",
			RunnerImage:              "example.invalid/operator@sha256:runner",
			RunnerProtocolVersion:    1,
		},
	}
	policyConfigMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "policy"}, BinaryData: map[string][]byte{"policy.yaml": policyBytes}}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(schema, plan, policyConfigMap).Build()
	return &ApprovalHandler{
		Reader: reader, Decoder: cradmission.NewDecoder(scheme),
		Clock: fixedClock{value: now.Time}, Mutate: mutate,
	}, approval
}

func requestFor(t *testing.T, approval *operatorv1alpha1.PtahSchemaApproval, operation admissionv1.Operation) cradmission.Request {
	t.Helper()
	raw, err := json.Marshal(approval)
	if err != nil {
		t.Fatal(err)
	}
	return cradmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID: "admission-uid", Namespace: approval.Namespace, Name: approval.Name,
		Operation: operation, Object: runtime.RawExtension{Raw: raw},
	}}
}

func containsJSON(data []byte, value string) bool {
	encoded, _ := json.Marshal(value)
	return strings.Contains(string(data), strings.Trim(string(encoded), `"`))
}
