package main

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/controller"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// testDatabaseURL is the exact URL the Apply Job resolves; the target identity
// binds to it rather than to a placeholder.
const testDatabaseURL = "postgres://user:pass@db.example.svc.cluster.local:5432/appdb?sslmode=disable"

func fixtureSchema() *operatorv1alpha1.PtahSchema {
	return &operatorv1alpha1.PtahSchema{
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
			ControllerImage:        "registry.invalid/controller@" + digest('f'),
			ControllerRevision:     "0123456789abcdef0123456789abcdef01234567",
			ControllerStateVersion: 1,
			ExecutorImage:          "registry.invalid/ptah@" + digest('a'),
			RunnerImage:            "registry.invalid/runner@" + digest('b'), RunnerProtocolVersion: 4,
		}},
	}
}

func fixturePlanData() []byte {
	return []byte(`{"format_version":1,"name":"upgrade-proof","dialect":"postgres","from_fingerprint":"` +
		digest('c') + `","to_fingerprint":"` + digest('d') +
		`","destructive":false,"statements":[{"sql":"SELECT pg_advisory_lock(742019370001)","severity":"safe","reason":"upgrade quiescence proof"}]}` + "\n")
}

// TestBuildFixtureBindsTheCurrentPlanContract measures what the manager that
// dispatches this Apply will re-derive: the plan fingerprint, the manager
// identity the contract now carries, and the target identity the runner
// recomputes from the database URL. Each of these was wrong once, and each time
// the Apply exited before opening a connection.
func TestBuildFixtureBindsTheCurrentPlanContract(t *testing.T) {
	t.Parallel()

	schema := fixtureSchema()
	planData := fixturePlanData()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	bundle, err := buildFixture(schema, planData, "policy-uid", []byte("version: 1\n"), testDatabaseURL, now)
	if err != nil {
		t.Fatal(err)
	}

	wantIdentity, err := runner.TargetIdentityDigest(testDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if got := bundle.SchemaStatus.Target.IdentityDigest; got != wantIdentity {
		t.Fatalf("target identity digest = %q, want the runner's %q", got, wantIdentity)
	}
	if got := bundle.Plan.Spec.TargetIdentityDigest; got != wantIdentity {
		t.Fatalf("plan target identity digest = %q, want the runner's %q", got, wantIdentity)
	}

	binding := schema.Status.ExecutionBinding
	if bundle.Plan.Spec.ContractVersion != fingerprint.CurrentPlanContractVersion ||
		bundle.Plan.Spec.ControllerImage != binding.ControllerImage ||
		bundle.Plan.Spec.ControllerRevision != binding.ControllerRevision ||
		bundle.Plan.Spec.ControllerStateVersion != binding.ControllerStateVersion {
		t.Fatalf("plan does not carry the current manager contract: %#v", bundle.Plan.Spec)
	}
	if bundle.SchemaStatus.Plan == nil || bundle.SchemaStatus.Plan.Name != bundle.Plan.Name ||
		bundle.SchemaStatus.Plan.UID != "" || bundle.SchemaStatus.Phase != operatorv1alpha1.PhaseReadyToApply {
		t.Fatalf("schema status is not ready for an API-assigned plan UID: %#v", bundle.SchemaStatus)
	}
	if len(bundle.Plan.Spec.Chunks) != 1 ||
		bundle.Plan.Spec.Chunks[0].Digest != fingerprint.DigestBytes(planData) ||
		bundle.Plan.Spec.Chunks[0].Size != int32(len(planData)) {
		t.Fatalf("chunk binding = %#v", bundle.Plan.Spec.Chunks)
	}

	wantPolicy, err := controller.PolicyFingerprint(schema)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Plan.Spec.PolicyFingerprint != wantPolicy {
		t.Fatalf("policy fingerprint = %q, want the controller's %q", bundle.Plan.Spec.PolicyFingerprint, wantPolicy)
	}

	wantFingerprint, err := (fingerprint.PlanBinding{
		ContractVersion: fingerprint.CurrentPlanContractVersion, SchemaUID: string(schema.UID),
		PlanContentDigest: bundle.Plan.Spec.ContentDigest, ArtifactDigest: bundle.Plan.Spec.ArtifactDigest,
		CoordinationDigest: bundle.Plan.Spec.CoordinationDigest, TargetIdentityDigest: bundle.Plan.Spec.TargetIdentityDigest,
		ActualStateFingerprint: bundle.Plan.Spec.ActualStateFingerprint, DesiredStateFingerprint: bundle.Plan.Spec.DesiredStateFingerprint,
		PolicyFingerprint: bundle.Plan.Spec.PolicyFingerprint, VerificationPolicyUID: string(bundle.Plan.Spec.VerificationPolicyUID),
		VerificationPolicyDigest: bundle.Plan.Spec.VerificationPolicyDigest, ExecutionBindingID: bundle.Plan.Spec.ExecutionBindingID,
		ControllerImage: bundle.Plan.Spec.ControllerImage, ControllerRevision: bundle.Plan.Spec.ControllerRevision,
		ControllerStateVersion: bundle.Plan.Spec.ControllerStateVersion,
		PtahVersion:            bundle.Plan.Spec.PtahVersion, ExecutorImage: bundle.Plan.Spec.ExecutorImage,
		RunnerImage: bundle.Plan.Spec.RunnerImage, RunnerProtocolVersion: bundle.Plan.Spec.RunnerProtocolVersion,
	}).Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Plan.Spec.Fingerprint != wantFingerprint {
		t.Fatalf("fingerprint = %q, want %q", bundle.Plan.Spec.Fingerprint, wantFingerprint)
	}
	if !strings.HasPrefix(bundle.Plan.Name, "ptah-plan-") {
		t.Fatalf("plan name = %q, want the derived plan name", bundle.Plan.Name)
	}
}

// TestBuildFixtureRefusesABindingWithoutTheManager is the control on the check
// above: a plan whose manager fields are empty is refused by the admission
// guard over plan writes, so the fixture refuses to write one at all.
func TestBuildFixtureRefusesABindingWithoutTheManager(t *testing.T) {
	t.Parallel()

	schema := fixtureSchema()
	schema.Status.ExecutionBinding.ControllerImage = ""
	_, err := buildFixture(schema, fixturePlanData(), "policy", []byte("policy"), testDatabaseURL, time.Now())
	if err == nil || !strings.Contains(err.Error(), "carries no manager identity") {
		t.Fatalf("buildFixture() error = %v", err)
	}
}

// TestBuildFixtureRefusesADestructivePlan keeps the proof's premise: the Apply
// under test is held open by a database barrier, not by anything it changes.
func TestBuildFixtureRefusesADestructivePlan(t *testing.T) {
	t.Parallel()

	destructive := []byte(`{"format_version":1,"name":"upgrade-proof","dialect":"postgres","from_fingerprint":"` +
		digest('c') + `","to_fingerprint":"` + digest('d') +
		`","destructive":true,"statements":[{"sql":"DROP TABLE widgets","severity":"destructive","reason":"proof"}]}` + "\n")
	_, err := buildFixture(fixtureSchema(), destructive, "policy-uid", []byte("version: 1\n"), testDatabaseURL, time.Now())
	if err == nil || !strings.Contains(err.Error(), "non-destructive") {
		t.Fatalf("buildFixture() error = %v", err)
	}
}

func digest(fill byte) string { return "sha256:" + strings.Repeat(string(fill), 64) }
