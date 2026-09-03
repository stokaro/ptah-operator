package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
)

func TestBuildFixtureProducesExactContractV2Binding(t *testing.T) {
	t.Parallel()

	schema := &operatorv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "upgrade-proof", Name: "predecessor-running-apply",
			UID: types.UID("schema-uid"), Generation: 2,
		},
		Spec: operatorv1alpha1.PtahSchemaSpec{
			Target: operatorv1alpha1.DatabaseTargetSpec{
				Engine: operatorv1alpha1.DatabaseEnginePostgreSQL, CoordinationKey: "upgrade-proof",
			},
			Desired: operatorv1alpha1.OCIArtifactSourceSpec{
				OCIRef: "oci://example.invalid/schema@" + digest('e'),
			},
			Policy: operatorv1alpha1.ReconciliationPolicy{
				Apply: operatorv1alpha1.ApplyPolicyAlways, DriftSeverity: "all",
				LockTimeout: metav1.Duration{Duration: 30 * time.Second}, TransactionMode: "file",
			},
			Execution: operatorv1alpha1.ExecutionSpec{
				ConnectTimeout: metav1.Duration{Duration: 10 * time.Second},
			},
		},
		Status: operatorv1alpha1.PtahSchemaStatus{ExecutionBinding: &operatorv1alpha1.ExecutionBindingStatus{
			Epoch: "v1-11111111111111111111111111111111", PtahVersion: "v1.2.3",
			ExecutorImage: "registry.invalid/ptah@" + digest('a'),
			RunnerImage:   "registry.invalid/runner@" + digest('b'), RunnerProtocolVersion: 4,
		}},
	}
	planData := []byte(`{"format_version":1,"name":"upgrade-proof","dialect":"postgres","from_fingerprint":"` + digest('c') + `","to_fingerprint":"` + digest('d') + `","destructive":false,"statements":[{"sql":"SELECT pg_sleep(90)","severity":"safe","reason":"upgrade quiescence proof"}]}` + "\n")
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	bundle, err := buildFixture(schema, planData, "policy-uid", []byte("version: 1\n"), now)
	if err != nil {
		t.Fatal(err)
	}

	if bundle.Plan.Spec.ContractVersion != legacyPlanContractVersion ||
		bundle.Plan.Spec.ControllerImage != "" || bundle.Plan.Spec.ControllerRevision != "" ||
		bundle.Plan.Spec.ControllerStateVersion != 0 {
		t.Fatalf("plan does not have the exact predecessor contract: %#v", bundle.Plan.Spec)
	}
	if bundle.SchemaStatus.ExecutionBinding == nil ||
		bundle.SchemaStatus.ExecutionBinding.ControllerImage != "" ||
		bundle.SchemaStatus.ExecutionBinding.ControllerRevision != "" ||
		bundle.SchemaStatus.ExecutionBinding.ControllerStateVersion != 0 {
		t.Fatalf("execution binding does not have the predecessor shape: %#v", bundle.SchemaStatus.ExecutionBinding)
	}
	if bundle.SchemaStatus.Plan == nil || bundle.SchemaStatus.Plan.Name != bundle.Plan.Name ||
		bundle.SchemaStatus.Plan.UID != "" || bundle.SchemaStatus.Phase != operatorv1alpha1.PhaseReadyToApply {
		t.Fatalf("schema status is not ready for an API-assigned plan UID: %#v", bundle.SchemaStatus)
	}
	if len(bundle.Plan.Spec.Chunks) != 1 || bundle.Plan.Spec.Chunks[0].Digest != fingerprint.DigestBytes(planData) ||
		bundle.Plan.Spec.Chunks[0].Size != int32(len(planData)) {
		t.Fatalf("chunk binding = %#v", bundle.Plan.Spec.Chunks)
	}
	wantFingerprint, err := (fingerprint.PlanBinding{
		ContractVersion: legacyPlanContractVersion, SchemaUID: string(schema.UID),
		PlanContentDigest: bundle.Plan.Spec.ContentDigest, ArtifactDigest: bundle.Plan.Spec.ArtifactDigest,
		CoordinationDigest: bundle.Plan.Spec.CoordinationDigest, TargetIdentityDigest: bundle.Plan.Spec.TargetIdentityDigest,
		ActualStateFingerprint: bundle.Plan.Spec.ActualStateFingerprint, DesiredStateFingerprint: bundle.Plan.Spec.DesiredStateFingerprint,
		PolicyFingerprint: bundle.Plan.Spec.PolicyFingerprint, VerificationPolicyUID: string(bundle.Plan.Spec.VerificationPolicyUID),
		VerificationPolicyDigest: bundle.Plan.Spec.VerificationPolicyDigest, ExecutionBindingID: bundle.Plan.Spec.ExecutionBindingID,
		PtahVersion: bundle.Plan.Spec.PtahVersion, ExecutorImage: bundle.Plan.Spec.ExecutorImage,
		RunnerImage: bundle.Plan.Spec.RunnerImage, RunnerProtocolVersion: bundle.Plan.Spec.RunnerProtocolVersion,
	}).Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Plan.Spec.Fingerprint != wantFingerprint {
		t.Fatalf("fingerprint = %q, want %q", bundle.Plan.Spec.Fingerprint, wantFingerprint)
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"controllerImage", "controllerRevision", "controllerStateVersion"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("legacy fixture contains %s", forbidden)
		}
	}
}

func TestBuildFixtureRejectsCurrentControllerIdentity(t *testing.T) {
	t.Parallel()

	schema := &operatorv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: "n", Name: "s", UID: "u"},
		Status: operatorv1alpha1.PtahSchemaStatus{ExecutionBinding: &operatorv1alpha1.ExecutionBindingStatus{
			ControllerImage: "registry.invalid/controller@" + digest('a'),
		}},
	}
	_, err := buildFixture(schema, []byte(`{}`), "policy", []byte("policy"), time.Now())
	if err == nil || !strings.Contains(err.Error(), "supported predecessor shape") {
		t.Fatalf("buildFixture() error = %v", err)
	}
}

func digest(fill byte) string { return "sha256:" + strings.Repeat(string(fill), 64) }
