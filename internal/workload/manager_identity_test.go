package workload

import (
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/planseal"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// realPlanJobPair builds two independently-generated Plan Jobs, the shape a
// two-replica install actually produces: one manager dispatches a Job sealed
// to its own key, and another rebuilds "expected" from the same claim with
// its own, different key. It returns the digest a claim recorded for the
// dispatching key alongside both Jobs.
func realPlanJobPair(t *testing.T) (expected, live *batchv1.Job, dispatcherDigest string) {
	t.Helper()

	dispatcherKey, err := planseal.Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	validatingKey, err := planseal.Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	schema := schemaFixture()
	operation := operationFixture(operatorv1alpha1.OperationPlan)

	dispatcher := builderFixture()
	dispatcher.PlanSealPublicKey = dispatcherKey.PublicKey()
	live, err = dispatcher.Build(schema, operation, nil)
	if err != nil {
		t.Fatalf("Build(dispatcher) error = %v", err)
	}

	validating := builderFixture()
	validating.PlanSealPublicKey = validatingKey.PublicKey()
	expected, err = validating.Build(schema, operation, nil)
	if err != nil {
		t.Fatalf("Build(validating) error = %v", err)
	}

	dispatcherDigest = fingerprint.DigestBytes([]byte(dispatcherKey.PublicKey().Encode()))
	return expected, live, dispatcherDigest
}

func TestCarrySealedPlanKeyAdmitsTheDigestTheClaimRecorded(t *testing.T) {
	t.Parallel()

	expected, live, dispatcherDigest := realPlanJobPair(t)
	liveKey, ok := containerEnvValue(live, runner.EnvPlanSealPublicKey)
	if !ok {
		t.Fatal("test fixture Plan Job carries no seal key")
	}
	expectedKeyBefore, ok := containerEnvValue(expected, runner.EnvPlanSealPublicKey)
	if !ok {
		t.Fatal("test fixture Plan Job carries no seal key")
	}
	if expectedKeyBefore == liveKey {
		t.Fatal("the two builders in this fixture produced the same key, so this test proves nothing")
	}

	operation := operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationPlan, PlanSealPublicKeyDigest: dispatcherDigest,
	}
	if err := CarrySealedPlanKey(expected, live, operation); err != nil {
		t.Fatalf("CarrySealedPlanKey() error = %v", err)
	}
	expectedKeyAfter, ok := containerEnvValue(expected, runner.EnvPlanSealPublicKey)
	if !ok || expectedKeyAfter != liveKey {
		t.Fatalf("expected's seal key = %q, want the live Job's key %q", expectedKeyAfter, liveKey)
	}
}

func TestCarrySealedPlanKeyRefusesAKeyTheDigestDoesNotName(t *testing.T) {
	t.Parallel()

	expected, live, _ := realPlanJobPair(t)
	stray, err := planseal.Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	operation := operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationPlan,
		// A digest of a key nobody involved holds -- neither live's nor
		// expected's -- so nothing this fixture built could satisfy it.
		PlanSealPublicKeyDigest: fingerprint.DigestBytes([]byte(stray.PublicKey().Encode())),
	}
	if err := CarrySealedPlanKey(expected, live, operation); err == nil {
		t.Fatal("CarrySealedPlanKey() accepted a key the recorded digest does not name")
	} else if !strings.Contains(err.Error(), "does not match the digest") {
		t.Fatalf("CarrySealedPlanKey() error = %v, want one naming the digest mismatch", err)
	}
}

func TestCarrySealedPlanKeyRefusesAMissingRecordedDigest(t *testing.T) {
	t.Parallel()

	expected, live, _ := realPlanJobPair(t)
	operation := operatorv1alpha1.ActiveOperationStatus{Type: operatorv1alpha1.OperationPlan}
	if err := CarrySealedPlanKey(expected, live, operation); err == nil {
		t.Fatal("CarrySealedPlanKey() accepted a claim with no recorded digest")
	}
}

func TestCarrySealedPlanKeyRefusesAMalformedLiveKey(t *testing.T) {
	t.Parallel()

	expected, live, dispatcherDigest := realPlanJobPair(t)
	if !setContainerEnvValue(live, runner.EnvPlanSealPublicKey, "not base64!!") {
		t.Fatal("test fixture Plan Job carries no seal key to tamper with")
	}
	operation := operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationPlan, PlanSealPublicKeyDigest: dispatcherDigest,
	}
	if err := CarrySealedPlanKey(expected, live, operation); err == nil {
		t.Fatal("CarrySealedPlanKey() accepted a malformed live key")
	} else if !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("CarrySealedPlanKey() error = %v, want one naming the malformed key", err)
	}
}

func TestCarrySealedPlanKeyRefusesALiveJobWithNoKey(t *testing.T) {
	t.Parallel()

	expected, live, dispatcherDigest := realPlanJobPair(t)
	live.Spec.Template.Spec.Containers[0].Env = nil
	operation := operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationPlan, PlanSealPublicKeyDigest: dispatcherDigest,
	}
	if err := CarrySealedPlanKey(expected, live, operation); err == nil {
		t.Fatal("CarrySealedPlanKey() accepted a live Job with no seal key at all")
	}
}

// TestCarrySealedPlanKeyIsANoOpOutsidePlan proves the guard other operation
// types rely on: they carry no seal key on either side, and this function
// must not manufacture a requirement neither side has.
func TestCarrySealedPlanKeyIsANoOpOutsidePlan(t *testing.T) {
	t.Parallel()

	for _, operationType := range []operatorv1alpha1.OperationType{
		operatorv1alpha1.OperationResolve,
		operatorv1alpha1.OperationVerify,
		operatorv1alpha1.OperationObserve,
		operatorv1alpha1.OperationApply,
	} {
		operationType := operationType
		t.Run(string(operationType), func(t *testing.T) {
			t.Parallel()

			schema := schemaFixture()
			builder := builderFixture()
			operation := operationFixture(operationType)
			var plan *operatorv1alpha1.PtahSchemaPlan
			if operationType == operatorv1alpha1.OperationApply {
				plan = planFixture(schema, builder)
			}
			job, err := builder.Build(schema, operation, plan)
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			active := operatorv1alpha1.ActiveOperationStatus{Type: operationType}
			if err := CarrySealedPlanKey(job.DeepCopy(), job, active); err != nil {
				t.Fatalf("CarrySealedPlanKey() error = %v, want a no-op for %s", err, operationType)
			}
		})
	}
}
