package main

import (
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/controller"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
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
			ControllerStateVersion: 1,
			ExecutorImage:          "registry.invalid/ptah@" + digest('a'),
			RunnerProtocolVersion:  4,
		}},
	}
}

// fixtureManager is the release of the manager that published the plan, read
// from a Job it dispatched.
func fixtureManager() managerIdentity {
	return managerIdentity{
		controllerImage:    "registry.invalid/controller@" + digest('f'),
		controllerRevision: "0123456789abcdef0123456789abcdef01234567",
		runnerImage:        "registry.invalid/runner@" + digest('b'),
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
	manager := fixtureManager()
	bundle, err := buildFixture(schema, manager, planData, "policy-uid", []byte("version: 1\n"), testDatabaseURL, now)
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
		bundle.Plan.Spec.ControllerImage != manager.controllerImage ||
		bundle.Plan.Spec.ControllerRevision != manager.controllerRevision ||
		bundle.Plan.Spec.RunnerImage != manager.runnerImage ||
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

	// The webhook recomputes the fingerprint from the published spec through
	// planstore.Binding; the fixture computes it on its own, and the two must
	// agree or the plan is refused on admission.
	wantFingerprint, err := planstore.Binding(schema.UID, bundle.Plan.Spec).Fingerprint()
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

// TestBuildFixtureRefusesAPlanWithoutItsPublisher is the control on the check
// above: a plan whose manager fields are empty is refused by the admission
// guard over plan writes, so the fixture refuses to write one at all.
func TestBuildFixtureRefusesAPlanWithoutItsPublisher(t *testing.T) {
	t.Parallel()

	for name, mutate := range map[string]func(*managerIdentity){
		"controller image":    func(manager *managerIdentity) { manager.controllerImage = "" },
		"controller revision": func(manager *managerIdentity) { manager.controllerRevision = "" },
		"runner image":        func(manager *managerIdentity) { manager.runnerImage = "" },
	} {
		mutate := mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			manager := fixtureManager()
			mutate(&manager)
			_, err := buildFixture(fixtureSchema(), manager, fixturePlanData(), "policy", []byte("policy"),
				testDatabaseURL, time.Now())
			if err == nil || !strings.Contains(err.Error(), "identity is incomplete") {
				t.Fatalf("buildFixture() error = %v", err)
			}
		})
	}
}

// TestManagerIdentityOfReadsTheDispatchingManager reads the publisher's
// identity from a Job the manager dispatched, and refuses a Job that records
// none of it.
func TestManagerIdentityOfReadsTheDispatchingManager(t *testing.T) {
	t.Parallel()

	// The identity is read from the Pod template, which the admission
	// snapshot pins; the builder writes the same values on the Job itself.
	job := &batchv1.Job{}
	job.Spec.Template.Annotations = map[string]string{
		workload.AnnotationControllerImage:    "registry.invalid/controller@" + digest('f'),
		workload.AnnotationControllerRevision: "release-1",
	}
	job.Spec.Template.Spec.InitContainers = []corev1.Container{{
		Name: "install-runner", Image: "registry.invalid/runner@" + digest('b'),
	}}
	manager, err := managerIdentityOf(job)
	if err != nil {
		t.Fatal(err)
	}
	if manager.controllerImage != "registry.invalid/controller@"+digest('f') ||
		manager.controllerRevision != "release-1" || manager.runnerImage != "registry.invalid/runner@"+digest('b') {
		t.Fatalf("managerIdentityOf() = %#v", manager)
	}
	job.Spec.Template.Spec.InitContainers = nil
	if _, err := managerIdentityOf(job); err == nil {
		t.Fatal("managerIdentityOf() accepted a Job that records no runner image")
	}
}

// TestBuildFixtureRefusesADestructivePlan keeps the proof's premise: the Apply
// under test is held open by a database barrier, not by anything it changes.
func TestBuildFixtureRefusesADestructivePlan(t *testing.T) {
	t.Parallel()

	destructive := []byte(`{"format_version":1,"name":"upgrade-proof","dialect":"postgres","from_fingerprint":"` +
		digest('c') + `","to_fingerprint":"` + digest('d') +
		`","destructive":true,"statements":[{"sql":"DROP TABLE widgets","severity":"destructive","reason":"proof"}]}` + "\n")
	_, err := buildFixture(fixtureSchema(), fixtureManager(), destructive, "policy-uid", []byte("version: 1\n"),
		testDatabaseURL, time.Now())
	if err == nil || !strings.Contains(err.Error(), "non-destructive") {
		t.Fatalf("buildFixture() error = %v", err)
	}
}

// TestBuildFixtureRefusesAPrivilegedPlan holds the fixture to the state it
// writes: ReadyToApply under Always is what the controller publishes for a plan
// that changes no privilege, and for no other.
func TestBuildFixtureRefusesAPrivilegedPlan(t *testing.T) {
	t.Parallel()

	privileged := []byte(`{"format_version":1,"name":"upgrade-proof","dialect":"postgres","from_fingerprint":"` +
		digest('c') + `","to_fingerprint":"` + digest('d') +
		`","destructive":false,"statements":[{"sql":"GRANT SELECT ON widgets TO PUBLIC","severity":"safe","reason":"proof"}]}` + "\n")
	_, err := buildFixture(fixtureSchema(), fixtureManager(), privileged, "policy-uid", []byte("version: 1\n"), testDatabaseURL, time.Now())
	if err == nil || !strings.Contains(err.Error(), "must change no privilege") {
		t.Fatalf("buildFixture() error = %v", err)
	}
}

func digest(fill byte) string { return "sha256:" + strings.Repeat(string(fill), 64) }
