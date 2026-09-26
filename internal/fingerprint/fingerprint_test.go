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

func TestDatabaseCoordinationDigestBindsEngineNamespaceAndExactKey(t *testing.T) {
	t.Parallel()

	postgres, err := fingerprint.DatabaseCoordinationDigest("PostgreSQL", "payments", "prod/payments-primary")
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"postgres", "postgresql", "pgx"} {
		aliasDigest, err := fingerprint.DatabaseCoordinationDigest(alias, "payments", "prod/payments-primary")
		if err != nil {
			t.Fatal(err)
		}
		if aliasDigest != postgres {
			t.Fatalf("engine alias %q produced %q, want %q", alias, aliasDigest, postgres)
		}
	}

	mysql, err := fingerprint.DatabaseCoordinationDigest("MySQL", "payments", "prod/payments-primary")
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := fingerprint.DatabaseCoordinationDigest("PostgreSQL", "payments", "prod/payments-replica")
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

// The attack a coordination key used to allow: anybody who could create a
// resource in another namespace wrote the same key and joined the realm. The
// namespace is part of the identity, so the same string elsewhere is another
// realm, another census and another Lease.
func TestTheSameKeyInAnotherNamespaceIsAnotherRealm(t *testing.T) {
	t.Parallel()

	owner, err := fingerprint.DatabaseCoordinationDigest("PostgreSQL", "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	intruder, err := fingerprint.DatabaseCoordinationDigest("PostgreSQL", "team-b", "orders")
	if err != nil {
		t.Fatal(err)
	}
	if intruder == owner {
		t.Fatal("a key written in another namespace named the same realm")
	}
	// A key cannot spell its way into another namespace either: the
	// namespace is a field of its own, not a prefix of the key.
	spelled, err := fingerprint.DatabaseCoordinationDigest("PostgreSQL", "team-b", "team-a/orders")
	if err != nil {
		t.Fatal(err)
	}
	if spelled == owner {
		t.Fatal("a key that spells another namespace named that namespace's realm")
	}
}

func TestDatabaseCoordinationDigestRejectsNonCanonicalKeyOrNamespace(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"", " production", "production ", "Production", "prod key", strings.Repeat("a", 254)} {
		if _, err := fingerprint.DatabaseCoordinationDigest("PostgreSQL", "payments", key); err == nil {
			t.Fatalf("DatabaseCoordinationDigest accepted %q", key)
		}
	}
	for _, namespace := range []string{"", "Payments", "team/a", "-team", strings.Repeat("a", 64)} {
		if _, err := fingerprint.DatabaseCoordinationDigest("PostgreSQL", namespace, "production"); err == nil {
			t.Fatalf("DatabaseCoordinationDigest accepted namespace %q", namespace)
		}
	}
	if _, err := fingerprint.DatabaseCoordinationDigest("SQLite", "payments", "production"); err == nil {
		t.Fatal("DatabaseCoordinationDigest accepted an unsupported engine")
	}
}

// A realm is named once, at cluster scope, and every namespace it admits
// derives the same identity from it -- which no namespace key can derive.
func TestDatabaseRealmDigestIsOneIdentityNoKeyReaches(t *testing.T) {
	t.Parallel()

	realm, err := fingerprint.DatabaseRealmDigest("PostgreSQL", "orders-primary")
	if err != nil {
		t.Fatal(err)
	}
	alias, err := fingerprint.DatabaseRealmDigest("postgres", "orders-primary")
	if err != nil {
		t.Fatal(err)
	}
	if alias != realm {
		t.Fatalf("engine alias produced %q, want %q", alias, realm)
	}
	mysql, err := fingerprint.DatabaseRealmDigest("MySQL", "orders-primary")
	if err != nil {
		t.Fatal(err)
	}
	other, err := fingerprint.DatabaseRealmDigest("PostgreSQL", "orders-replica")
	if err != nil {
		t.Fatal(err)
	}
	if mysql == realm || other == realm {
		t.Fatal("engine or realm name change retained the digest")
	}
	for _, namespace := range []string{"orders-primary", "default", "team-a"} {
		key, err := fingerprint.DatabaseCoordinationDigest("PostgreSQL", namespace, "orders-primary")
		if err != nil {
			t.Fatal(err)
		}
		if key == realm {
			t.Fatalf("the key orders-primary in namespace %s derived the realm's digest", namespace)
		}
	}

	for _, name := range []string{"", "Orders", "orders/primary", "orders_primary", "-orders", strings.Repeat("a", 254)} {
		if _, err := fingerprint.DatabaseRealmDigest("PostgreSQL", name); err == nil {
			t.Fatalf("DatabaseRealmDigest accepted %q", name)
		}
	}
	if _, err := fingerprint.DatabaseRealmDigest("SQLite", "orders-primary"); err == nil {
		t.Fatal("DatabaseRealmDigest accepted an unsupported engine")
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
		"ControllerStateVersion":   func(v *fingerprint.PlanBinding) { v.ControllerStateVersion++ },
		"PtahVersion":              func(v *fingerprint.PlanBinding) { v.PtahVersion += "-new" },
		"ExecutorImage":            func(v *fingerprint.PlanBinding) { v.ExecutorImage += "-new" },
		"RunnerProtocolVersion":    func(v *fingerprint.PlanBinding) { v.RunnerProtocolVersion++ },
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

// TestPlanBindingLeavesTheManagerOut holds the binding to what decides a plan's
// meaning when it runs. The manager's image and revision, and the runner image
// built beside it, change with every release of the operator; a fingerprint
// that held them would retire every pending approval on a patch release. The
// runner's enforcement is bound through RunnerProtocolVersion instead.
func TestPlanBindingLeavesTheManagerOut(t *testing.T) {
	t.Parallel()

	bindingType := reflect.TypeOf(fingerprint.PlanBinding{})
	for _, name := range []string{"ControllerImage", "ControllerRevision", "RunnerImage"} {
		if _, found := bindingType.FieldByName(name); found {
			t.Errorf("PlanBinding carries %s, so a manager release would change every plan's fingerprint", name)
		}
	}
	for _, name := range []string{"ControllerStateVersion", "PtahVersion", "ExecutorImage", "RunnerProtocolVersion"} {
		if _, found := bindingType.FieldByName(name); !found {
			t.Errorf("PlanBinding lost %s, which decides what the plan means when it runs", name)
		}
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

const currentPlanBindingFingerprint = "sha256:d1de8ce758589df3bdcd8687f85c264ee6241e6b26e3e90be4690824aa80ceef"

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
		ControllerStateVersion:   1,
		PtahVersion:              "v0.3.0",
		ExecutorImage:            "example.invalid/ptah@sha256:executor",
		RunnerProtocolVersion:    1,
	}
}
