package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	approvaladmission "github.com/stokaro/ptah-operator/internal/admission"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// legacyCoordinationDigest is the realm a coordination key named before the
// namespace was part of it: the digest hack/e2e-assert.sh wrote into its plan
// fixture as a literal, and the one run 36267860770 was refused for.
func legacyCoordinationDigest(t *testing.T, engine, key string) string {
	t.Helper()
	digest, err := fingerprint.DigestCanonicalJSON(struct {
		ContractVersion int    `json:"contract_version"`
		Engine          string `json:"engine"`
		CoordinationKey string `json:"coordination_key"`
	}{ContractVersion: 1, Engine: engine, CoordinationKey: key})
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// A plan this controller published is one the approval webhook admits: the
// two derive the realm through the same function, so a person approving what
// the controller asked about is never refused for the realm it is bound to.
//
// The other row is the acceptance failure after #483. The harness wrote its own
// plan and status with the realm digest as a literal, computed before the
// namespace joined the derivation, and the webhook -- which recomputes the
// realm from the schema's spec -- refused the approval as naming a target that
// had changed. A plan carrying that digest is refused here with the same
// message the run logged.
func TestAPlanTheControllerPublishedIsApprovableThroughTheWebhook(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		// rebind rewrites the realm the stored plan and status carry, the way
		// the harness wrote them, or leaves the controller's own.
		rebind  func(t *testing.T, schema *operatorv1alpha1.PtahSchema) string
		allowed bool
	}{
		{name: "the controller's own binding", allowed: true},
		{
			name: "a realm digest derived without the namespace",
			rebind: func(t *testing.T, schema *operatorv1alpha1.PtahSchema) string {
				return legacyCoordinationDigest(t, "postgresql", schema.Spec.Target.CoordinationKey)
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			schema, api := publishSchemaPlanThroughTheController(t)
			published := safetyGetSchema(t, api, schema)
			if published.Status.Phase != operatorv1alpha1.PhaseAwaitingApproval || published.Status.Plan == nil {
				t.Fatalf("the controller published no plan awaiting approval: %#v", published.Status)
			}
			plan := &operatorv1alpha1.PtahSchemaPlan{}
			if err := api.Get(context.Background(),
				client.ObjectKey{Namespace: published.Namespace, Name: published.Status.Plan.Name}, plan); err != nil {
				t.Fatalf("read the published plan: %v", err)
			}
			want, err := fingerprint.DatabaseCoordinationDigest("PostgreSQL", published.Namespace, published.Spec.Target.CoordinationKey)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Spec.CoordinationDigest != want || published.Status.Target.CoordinationDigest != want {
				t.Fatalf("the controller bound realm %s to the plan and %s to the status, want %s",
					plan.Spec.CoordinationDigest, published.Status.Target.CoordinationDigest, want)
			}
			// An API server stamps a UID on the plan it accepts and the
			// controller records it; the fake client stamps none. The UID is
			// the only thing supplied here, and it is supplied to both at once.
			if plan.UID == "" && published.Status.Plan.UID == "" {
				plan.UID = "published-plan-uid"
				if err := api.Update(context.Background(), plan); err != nil {
					t.Fatal(err)
				}
				published.Status.Plan.UID = plan.UID
				if err := api.Status().Update(context.Background(), published); err != nil {
					t.Fatal(err)
				}
				published = safetyGetSchema(t, api, schema)
			}

			if row.rebind != nil {
				legacy := row.rebind(t, published)
				if legacy == want {
					t.Fatal("the harness digest equals the operator's, so this reproduces nothing")
				}
				plan.Spec.CoordinationDigest = legacy
				if err := api.Update(context.Background(), plan); err != nil {
					t.Fatal(err)
				}
				published.Status.Target.CoordinationDigest = legacy
				if err := api.Status().Update(context.Background(), published); err != nil {
					t.Fatal(err)
				}
			}

			approval := &operatorv1alpha1.PtahSchemaApproval{
				TypeMeta:   metav1.TypeMeta{APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "PtahSchemaApproval"},
				ObjectMeta: metav1.ObjectMeta{Namespace: published.Namespace, Name: "approve-published-plan"},
				Spec: operatorv1alpha1.PtahSchemaApprovalSpec{
					SchemaRef:       operatorv1alpha1.ImmutableObjectReference{Name: published.Name, UID: published.UID},
					PlanRef:         operatorv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID},
					PlanFingerprint: plan.Spec.Fingerprint,
				},
			}
			raw, err := json.Marshal(approval)
			if err != nil {
				t.Fatal(err)
			}
			scheme := runtime.NewScheme()
			if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			// The webhook runs beside the manager that published the plan,
			// so it serves the execution the plan was computed under.
			handler := &approvaladmission.ApprovalHandler{
				Reader: api, Decoder: cradmission.NewDecoder(scheme), Mutate: true,
				Execution: approvaladmission.Execution{
					ControllerStateVersion: plan.Spec.ControllerStateVersion,
					PtahVersion:            plan.Spec.PtahVersion,
					ExecutorImage:          plan.Spec.ExecutorImage,
					RunnerProtocolVersion:  plan.Spec.RunnerProtocolVersion,
				},
			}
			response := handler.Handle(context.Background(), cradmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
				UID:       "approve-published-plan",
				Namespace: approval.Namespace,
				Name:      approval.Name,
				Operation: admissionv1.Create,
				UserInfo:  authenticationv1.UserInfo{Username: "approver"},
				Object:    runtime.RawExtension{Raw: raw},
			}})
			if row.allowed {
				if !response.Allowed {
					t.Fatalf("the webhook refused an approval of the plan the controller published: %#v", response.Result)
				}
				return
			}
			if response.Allowed || response.Result == nil ||
				!strings.Contains(response.Result.Message, "schema source or target changed after the plan was generated") {
				t.Fatalf("the webhook's answer to a plan bound to the legacy realm = %#v", response.Result)
			}
		})
	}
}

// publishSchemaPlanThroughTheController takes a schema from a harvested Plan
// Job to a published plan awaiting approval, through Reconcile, and returns the
// schema and the fake API server holding the result.
func publishSchemaPlanThroughTheController(t *testing.T) (*operatorv1alpha1.PtahSchema, client.Client) {
	t.Helper()
	policyBytes := "policy"
	policyDigest := fingerprint.DigestBytes([]byte(policyBytes))
	schema := schemaFixture()
	schema.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyOnApproval
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhasePlanning
	schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
		ResolvedReference:        "oci://registry.example/team/schema@" + testDigest,
		Digest:                   testDigest,
		ArtifactType:             dataplane.SchemaArtifactType,
		Verified:                 true,
		VerificationPolicyUID:    testPolicyUID,
		VerificationPolicyDigest: policyDigest,
	}
	schema.Status.Target = operatorv1alpha1.TargetStatus{
		CoordinationDigest: testCoordinationDigest,
		IdentityDigest:     testDigest,
		DriftReportDigest:  safetyOtherDigest,
	}
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type:                    operatorv1alpha1.OperationPlan,
		ID:                      "published-plan-operation",
		JobName:                 "published-plan-job",
		JobUID:                  "job-uid",
		StartedAt:               metav1.Now(),
		Attempt:                 1,
		PlanSealPublicKeyDigest: planSealPublicKeyDigest(testSchemaSealKey.PublicKey()),
	}
	bindActiveInput(t, schema)
	planDocument := safetyPlanDocument(t, "observed-state")
	frame := safetyRunnerFrame(t, runner.Result{
		ProtocolVersion:      runner.ProtocolVersion,
		Operation:            runner.OperationPlan,
		OperationID:          schema.Status.ActiveOperation.ID,
		ChildExitCode:        0,
		Stdout:               safetySealPlan(t, planDocument),
		CoordinationDigest:   schema.Status.Target.CoordinationDigest,
		TargetIdentityDigest: schema.Status.Target.IdentityDigest,
		PlanContentDigest:    fingerprint.DigestBytes(planDocument),
		PlanOutcome:          runner.PlanOutcomeChanges,
	})
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	immutable := true
	policyConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: schema.Namespace,
			Name:      schema.Spec.Desired.VerificationPolicyFrom.Name,
			UID:       testPolicyUID,
		},
		Immutable: &immutable,
		Data:      map[string]string{schema.Spec.Desired.VerificationPolicyFrom.Key: policyBytes},
	}
	reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod, policyConfigMap)
	reconciler.Plans = planstore.Store{Client: api, Reader: api}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	// Publication commits the plan, then marks its storage ready on a later
	// pass; drive it until the plan is one a person could approve.
	for pass := range 4 {
		if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	return schema, api
}
