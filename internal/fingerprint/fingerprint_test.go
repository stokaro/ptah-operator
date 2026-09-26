package fingerprint_test

import (
	"reflect"
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

	base := completePlanBinding()
	want, err := base.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}

	mutations := map[string]func(*fingerprint.PlanBinding){
		"SchemaUID":                func(v *fingerprint.PlanBinding) { v.SchemaUID += "-new" },
		"PlanContentDigest":        func(v *fingerprint.PlanBinding) { v.PlanContentDigest += "-new" },
		"ArtifactDigest":           func(v *fingerprint.PlanBinding) { v.ArtifactDigest += "-new" },
		"CoordinationDigest":       func(v *fingerprint.PlanBinding) { v.CoordinationDigest += "-new" },
		"TargetIdentityDigest":     func(v *fingerprint.PlanBinding) { v.TargetIdentityDigest += "-new" },
		"ActualStateFingerprint":   func(v *fingerprint.PlanBinding) { v.ActualStateFingerprint += "-new" },
		"DesiredStateFingerprint":  func(v *fingerprint.PlanBinding) { v.DesiredStateFingerprint += "-new" },
		"PolicyFingerprint":        func(v *fingerprint.PlanBinding) { v.PolicyFingerprint += "-new" },
		"VerificationPolicyUID":    func(v *fingerprint.PlanBinding) { v.VerificationPolicyUID += "-new" },
		"VerificationPolicyDigest": func(v *fingerprint.PlanBinding) { v.VerificationPolicyDigest += "-new" },
		"ExecutionBindingID":       func(v *fingerprint.PlanBinding) { v.ExecutionBindingID = "v1-44444444444444444444444444444444" },
		"ControllerImage": func(v *fingerprint.PlanBinding) {
			v.ControllerImage = "example.invalid/manager@sha256:" + strings.Repeat("d", 64)
		},
		"ControllerRevision":     func(v *fingerprint.PlanBinding) { v.ControllerRevision += "-new" },
		"ControllerStateVersion": func(v *fingerprint.PlanBinding) { v.ControllerStateVersion++ },
		"PtahVersion":            func(v *fingerprint.PlanBinding) { v.PtahVersion += "-new" },
		"ExecutorImage":          func(v *fingerprint.PlanBinding) { v.ExecutorImage += "-new" },
		"RunnerImage":            func(v *fingerprint.PlanBinding) { v.RunnerImage += "-new" },
		"RunnerProtocolVersion":  func(v *fingerprint.PlanBinding) { v.RunnerProtocolVersion++ },
	}
	// Keyed by field name, and checked against the type: a field added to the
	// binding with no case here fails instead of going unmeasured. Prose keys
	// read better and cannot be checked against anything.
	bindingType := reflect.TypeOf(fingerprint.PlanBinding{})
	for index := range bindingType.NumField() {
		name := bindingType.Field(index).Name
		if name == "ContractVersion" {
			// Not an input: it is the contract these fields are read under,
			// and ValidatePlanContractVersion is what holds it.
			continue
		}
		if _, declared := mutations[name]; !declared {
			t.Fatalf("no case mutates %s, so nothing here says whether it is inside the fingerprint", name)
		}
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

// TestPlanBindingAcceptsOnlyTheCurrentContract pins the one plan contract this
// manager reads. Every other version is refused before any field is looked at,
// so an earlier or a future plan is never given an identity under today's
// approval semantics.
func TestPlanBindingAcceptsOnlyTheCurrentContract(t *testing.T) {
	t.Parallel()

	current := completePlanBinding()
	got, err := current.Fingerprint()
	if err != nil {
		t.Fatalf("current binding: %v", err)
	}
	// The digest is also computed outside Go, by the acceptance fixtures, so a
	// change to the encoding has to show up here first.
	if got != currentPlanBindingFingerprint {
		t.Fatalf("current fingerprint = %q, want %q", got, currentPlanBindingFingerprint)
	}

	for _, version := range []int32{0, 1, 2, fingerprint.CurrentPlanContractVersion + 1} {
		other := current
		other.ContractVersion = version
		if _, err := other.Fingerprint(); err == nil || !strings.Contains(err.Error(), "unsupported plan contract version") {
			t.Fatalf("contract version %d error = %v, want unsupported-version refusal", version, err)
		}
		if err := fingerprint.ValidatePlanContractVersion(version); err == nil {
			t.Fatalf("ValidatePlanContractVersion(%d) accepted a version this manager does not write", version)
		}
	}
	if err := fingerprint.ValidatePlanContractVersion(fingerprint.CurrentPlanContractVersion); err != nil {
		t.Fatalf("ValidatePlanContractVersion(current): %v", err)
	}

	for name, test := range map[string]struct {
		mutate func(*fingerprint.PlanBinding)
		want   string
	}{
		"malformed execution epoch": {
			mutate: func(b *fingerprint.PlanBinding) { b.ExecutionBindingID = "retired-epoch" },
			want:   "valid execution binding ID",
		},
		"tag-pinned manager image": {
			mutate: func(b *fingerprint.PlanBinding) { b.ControllerImage = "example.invalid/manager:latest" },
			want:   "controller image",
		},
		"control-character revision": {
			mutate: func(b *fingerprint.PlanBinding) { b.ControllerRevision = "release\ncandidate" },
			want:   "control characters",
		},
		"negative state version": {
			mutate: func(b *fingerprint.PlanBinding) { b.ControllerStateVersion = -1 },
			want:   "controller state version",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			binding := completePlanBinding()
			test.mutate(&binding)
			if _, err := binding.Fingerprint(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want a refusal naming %q", err, test.want)
			}
		})
	}
}

const currentPlanBindingFingerprint = "sha256:9f0c4f01e635cb3d36229ce273efbc4b6eeea56d08a83a38027ffa222157ea0b"

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

// TestPlanBindingRefusesAnIncompleteBinding is the other half of the contract
// above, and it has the same blind spot to close: the requirements are a map,
// so one empty field exercises the whole loop and an entry that goes missing is
// invisible to coverage. Three could be deleted with every package green.
//
// A binding with a hole in it must not be given an identity. An empty policy
// fingerprint, say, would let two plans decided under different policies -- one
// of which failed to produce one -- share the digest an approval names.
func TestPlanBindingRefusesAnIncompleteBinding(t *testing.T) {
	t.Parallel()

	bindingType := reflect.TypeOf(fingerprint.PlanBinding{})
	for index := range bindingType.NumField() {
		field := bindingType.Field(index)
		t.Run(field.Name, func(t *testing.T) {
			t.Parallel()

			binding := completePlanBinding()
			reflect.ValueOf(&binding).Elem().Field(index).Set(reflect.Zero(field.Type))
			if _, err := binding.Fingerprint(); err == nil {
				t.Fatalf("a binding with no %s was given a fingerprint", field.Name)
			}
		})
	}
}

// completePlanBinding is a binding every check accepts, which is what makes a
// single emptied field attributable to the check that rejects it.
func completePlanBinding() fingerprint.PlanBinding {
	return fingerprint.PlanBinding{
		ContractVersion:          fingerprint.CurrentPlanContractVersion,
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
		ControllerImage:          "example.invalid/manager@sha256:" + strings.Repeat("c", 64),
		ControllerRevision:       "controller-test-revision",
		ControllerStateVersion:   1,
		PtahVersion:              "v0.3.0",
		ExecutorImage:            "example.invalid/ptah@sha256:executor",
		RunnerImage:              "example.invalid/operator@sha256:runner",
		RunnerProtocolVersion:    1,
	}
}
