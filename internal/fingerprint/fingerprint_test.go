package fingerprint_test

import (
	"testing"

	"github.com/stokaro/ptah-operator/internal/fingerprint"
)

func TestNormalizeSet(t *testing.T) {
	t.Parallel()

	got := fingerprint.NormalizeSet([]string{" b ", "a", "", "b", "a"})
	want := []string{"a", "b"}
	if len(got) != len(want) {
		t.Fatalf("NormalizeSet() = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("NormalizeSet() = %#v, want %#v", got, want)
		}
	}
}

func TestPlanBindingEveryInputInvalidatesFingerprint(t *testing.T) {
	t.Parallel()

	base := fingerprint.PlanBinding{
		ContractVersion:          1,
		SchemaUID:                "schema-uid",
		PlanContentDigest:        "sha256:plan",
		ArtifactDigest:           "sha256:artifact",
		TargetIdentityDigest:     "sha256:target",
		ActualStateFingerprint:   "sha256:actual",
		DesiredStateFingerprint:  "sha256:desired",
		PolicyFingerprint:        "sha256:policy",
		VerificationPolicyDigest: "sha256:verification",
		PtahVersion:              "v0.3.0",
		ExecutorImage:            "example.invalid/ptah@sha256:executor",
		RunnerImage:              "example.invalid/operator@sha256:runner",
		RunnerProtocolVersion:    1,
	}
	want, err := base.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}

	mutations := map[string]func(*fingerprint.PlanBinding){
		"schema":       func(v *fingerprint.PlanBinding) { v.SchemaUID += "-new" },
		"plan":         func(v *fingerprint.PlanBinding) { v.PlanContentDigest += "-new" },
		"artifact":     func(v *fingerprint.PlanBinding) { v.ArtifactDigest += "-new" },
		"target":       func(v *fingerprint.PlanBinding) { v.TargetIdentityDigest += "-new" },
		"actual":       func(v *fingerprint.PlanBinding) { v.ActualStateFingerprint += "-new" },
		"desired":      func(v *fingerprint.PlanBinding) { v.DesiredStateFingerprint += "-new" },
		"policy":       func(v *fingerprint.PlanBinding) { v.PolicyFingerprint += "-new" },
		"verification": func(v *fingerprint.PlanBinding) { v.VerificationPolicyDigest += "-new" },
		"version":      func(v *fingerprint.PlanBinding) { v.PtahVersion += "-new" },
		"executor":     func(v *fingerprint.PlanBinding) { v.ExecutorImage += "-new" },
		"runner":       func(v *fingerprint.PlanBinding) { v.RunnerImage += "-new" },
		"protocol":     func(v *fingerprint.PlanBinding) { v.RunnerProtocolVersion++ },
	}
	for name, mutate := range mutations {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			changed := base
			mutate(&changed)
			got, err := changed.Fingerprint()
			if err != nil {
				t.Fatal(err)
			}
			if got == want {
				t.Fatalf("mutation %q did not change fingerprint %s", name, got)
			}
		})
	}
}

func TestOperationIDIgnoresMapInsertionOrder(t *testing.T) {
	t.Parallel()

	a, err := (fingerprint.OperationInput{
		ContractVersion: 1,
		SchemaUID:       "uid",
		Operation:       "Observe",
		Inputs:          map[string]any{"a": 1, "b": 2},
	}).ID()
	if err != nil {
		t.Fatal(err)
	}
	b, err := (fingerprint.OperationInput{
		ContractVersion: 1,
		SchemaUID:       "uid",
		Operation:       "Observe",
		Inputs:          map[string]any{"b": 2, "a": 1},
	}).ID()
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("map insertion order changed operation ID: %s != %s", a, b)
	}
}
