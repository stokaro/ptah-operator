package fingerprint_test

import (
	"strings"
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

func TestDatabaseCoordinationDigestBindsEngineAndExactKey(t *testing.T) {
	t.Parallel()

	postgres, err := fingerprint.DatabaseCoordinationDigest("PostgreSQL", "prod/payments-primary")
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"postgres", "postgresql", "pgx"} {
		aliasDigest, err := fingerprint.DatabaseCoordinationDigest(alias, "prod/payments-primary")
		if err != nil {
			t.Fatal(err)
		}
		if aliasDigest != postgres {
			t.Fatalf("engine alias %q produced %q, want %q", alias, aliasDigest, postgres)
		}
	}

	mysql, err := fingerprint.DatabaseCoordinationDigest("MySQL", "prod/payments-primary")
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := fingerprint.DatabaseCoordinationDigest("PostgreSQL", "prod/payments-replica")
	if err != nil {
		t.Fatal(err)
	}
	if mysql == postgres || otherKey == postgres {
		t.Fatal("engine or exact coordination-key change retained the digest")
	}
	if strings.Contains(postgres, "payments-primary") {
		t.Fatal("coordination digest exposed the plaintext key")
	}
}

func TestDatabaseCoordinationDigestRejectsNonCanonicalKey(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"", " production", "production ", "Production", "prod key", strings.Repeat("a", 254)} {
		if _, err := fingerprint.DatabaseCoordinationDigest("PostgreSQL", key); err == nil {
			t.Fatalf("DatabaseCoordinationDigest accepted %q", key)
		}
	}
	if _, err := fingerprint.DatabaseCoordinationDigest("SQLite", "production"); err == nil {
		t.Fatal("DatabaseCoordinationDigest accepted an unsupported engine")
	}
}

func TestPlanBindingEveryInputInvalidatesFingerprint(t *testing.T) {
	t.Parallel()

	base := fingerprint.PlanBinding{
		ContractVersion:          2,
		SchemaUID:                "schema-uid",
		PlanContentDigest:        "sha256:plan",
		ArtifactDigest:           "sha256:artifact",
		CoordinationDigest:       "sha256:coordination",
		TargetIdentityDigest:     "sha256:target",
		ActualStateFingerprint:   "sha256:actual",
		DesiredStateFingerprint:  "sha256:desired",
		PolicyFingerprint:        "sha256:policy",
		VerificationPolicyUID:    "verification-policy-uid",
		VerificationPolicyDigest: "sha256:verification",
		ExecutionBindingID:       "v1-33333333333333333333333333333333",
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
		"schema":           func(v *fingerprint.PlanBinding) { v.SchemaUID += "-new" },
		"plan":             func(v *fingerprint.PlanBinding) { v.PlanContentDigest += "-new" },
		"artifact":         func(v *fingerprint.PlanBinding) { v.ArtifactDigest += "-new" },
		"coordination":     func(v *fingerprint.PlanBinding) { v.CoordinationDigest += "-new" },
		"target":           func(v *fingerprint.PlanBinding) { v.TargetIdentityDigest += "-new" },
		"actual":           func(v *fingerprint.PlanBinding) { v.ActualStateFingerprint += "-new" },
		"desired":          func(v *fingerprint.PlanBinding) { v.DesiredStateFingerprint += "-new" },
		"policy":           func(v *fingerprint.PlanBinding) { v.PolicyFingerprint += "-new" },
		"verification UID": func(v *fingerprint.PlanBinding) { v.VerificationPolicyUID += "-new" },
		"verification":     func(v *fingerprint.PlanBinding) { v.VerificationPolicyDigest += "-new" },
		"execution epoch":  func(v *fingerprint.PlanBinding) { v.ExecutionBindingID = "v1-44444444444444444444444444444444" },
		"version":          func(v *fingerprint.PlanBinding) { v.PtahVersion += "-new" },
		"executor":         func(v *fingerprint.PlanBinding) { v.ExecutorImage += "-new" },
		"runner":           func(v *fingerprint.PlanBinding) { v.RunnerImage += "-new" },
		"protocol":         func(v *fingerprint.PlanBinding) { v.RunnerProtocolVersion++ },
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

func TestPlanBindingExecutionEpochCompatibility(t *testing.T) {
	t.Parallel()

	legacy := fingerprint.PlanBinding{
		ContractVersion:          1,
		SchemaUID:                "schema-uid",
		PlanContentDigest:        "sha256:plan",
		ArtifactDigest:           "sha256:artifact",
		CoordinationDigest:       "sha256:coordination",
		TargetIdentityDigest:     "sha256:target",
		ActualStateFingerprint:   "sha256:actual",
		DesiredStateFingerprint:  "sha256:desired",
		PolicyFingerprint:        "sha256:policy",
		VerificationPolicyUID:    "verification-policy-uid",
		VerificationPolicyDigest: "sha256:verification",
		PtahVersion:              "v0.3.0",
		ExecutorImage:            "example.invalid/ptah@sha256:executor",
		RunnerImage:              "example.invalid/operator@sha256:runner",
		RunnerProtocolVersion:    1,
	}
	legacyFingerprint, err := legacy.Fingerprint()
	if err != nil {
		t.Fatalf("legacy v1 binding without execution epoch: %v", err)
	}
	if legacyFingerprint != "sha256:f1ea9f2864032cffccab3976470b524c794c4bd3c58917d48ffd161b0ddc9bdf" {
		t.Fatalf("legacy v1 fingerprint = %q, want backward-compatible digest", legacyFingerprint)
	}

	current := legacy
	current.ContractVersion = 2
	if _, err := current.Fingerprint(); err == nil || !strings.Contains(err.Error(), "execution binding ID") {
		t.Fatalf("v2 binding without execution epoch error = %v, want execution binding refusal", err)
	}
	current.ExecutionBindingID = "v1-33333333333333333333333333333333"
	if _, err := current.Fingerprint(); err != nil {
		t.Fatalf("v2 binding with execution epoch: %v", err)
	}
	current.ExecutionBindingID = "retired-epoch"
	if _, err := current.Fingerprint(); err == nil || !strings.Contains(err.Error(), "valid execution binding ID") {
		t.Fatalf("v2 binding with malformed execution epoch error = %v, want format refusal", err)
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
