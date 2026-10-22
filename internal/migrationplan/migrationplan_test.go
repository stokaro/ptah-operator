package migrationplan_test

import (
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/migrationplan"
)

func digest(b byte) string {
	return "sha256:" + strings.Repeat(string(b), 64)
}

func completeBinding() migrationplan.Binding {
	return migrationplan.Binding{
		MigrationUID:             "migration-uid",
		HistoryFingerprint:       digest('a'),
		SequenceDigest:           digest('b'),
		ArtifactDigest:           digest('c'),
		CoordinationDigest:       digest('d'),
		TargetIdentityDigest:     digest('e'),
		PolicyFingerprint:        digest('f'),
		VerificationPolicyUID:    "verification-policy-uid",
		VerificationPolicyDigest: digest('0'),
		ExecutionBindingID:       "v1-" + strings.Repeat("1", 32),
		ControllerStateVersion:   1,
		PtahVersion:              "v0.3.0",
		ExecutorImage:            "example.invalid/ptah@" + digest('3'),
		RunnerProtocolVersion:    5,
	}
}

// alternatives is a second valid value for every field of the binding. It is
// keyed by field name on purpose: a field added to Binding and not listed here
// fails the test that reads it, which is the only way a new input can be caught
// before it reaches an approval.
var alternatives = map[string]any{
	"MigrationUID":             types.UID("another-migration-uid"),
	"HistoryFingerprint":       digest('9'),
	"SequenceDigest":           digest('8'),
	"ArtifactDigest":           digest('7'),
	"CoordinationDigest":       digest('6'),
	"TargetIdentityDigest":     digest('5'),
	"PolicyFingerprint":        digest('4'),
	"VerificationPolicyUID":    types.UID("another-verification-policy-uid"),
	"VerificationPolicyDigest": digest('3'),
	"ExecutionBindingID":       "v1-" + strings.Repeat("2", 32),
	"ControllerImage":          "example.invalid/manager@" + digest('b'),
	"ControllerRevision":       "another-controller-revision",
	"ControllerStateVersion":   int32(2),
	"PtahVersion":              "v0.4.0",
	"ExecutorImage":            "example.invalid/ptah@" + digest('c'),
	"RunnerImage":              "example.invalid/operator@" + digest('d'),
	"RunnerProtocolVersion":    int32(6),
}

// TestEveryInputOfAMigrationPlanIsInsideItsFingerprint is the contract the
// package's doc comment states: everything a plan is decided from is inside its
// identity.
//
// A field that is in the binding and not in the digest makes two plans decided
// from different inputs share one identity. An approval names a fingerprint, so
// the approval for one of them would authorize the other -- and the one that
// runs is whichever plan the status happens to point at.
//
// The set is read off the type rather than listed, so a field added later fails
// here instead of quietly leaving the digest behind.
func TestEveryInputOfAMigrationPlanIsInsideItsFingerprint(t *testing.T) {
	t.Parallel()

	baseline, err := completeBinding().Fingerprint()
	if err != nil {
		t.Fatalf("the complete binding was refused: %v", err)
	}

	bindingType := reflect.TypeOf(migrationplan.Binding{})
	for index := range bindingType.NumField() {
		field := bindingType.Field(index)
		t.Run(field.Name, func(t *testing.T) {
			t.Parallel()

			if field.Name == "Source" {
				// A version 1 binding has no source; TestAMigrationPlanBindsItsSource
				// changes each field of a version 2 one.
				return
			}
			replacement, declared := alternatives[field.Name]
			if !declared {
				t.Fatalf("no alternative value is declared for %s, so nothing here reads it", field.Name)
			}
			mutated := completeBinding()
			reflect.ValueOf(&mutated).Elem().Field(index).Set(reflect.ValueOf(replacement))

			got, err := mutated.Fingerprint()
			if err != nil {
				t.Fatalf("the alternative value for %s is not a valid binding: %v", field.Name, err)
			}
			if got == baseline {
				t.Fatalf("%s is not inside the plan's identity: two plans decided from "+
					"different values share the fingerprint an approval names", field.Name)
			}
		})
	}
}

// TestAMigrationPlanRefusesAnIncompleteBinding is the other half. A fingerprint
// computed from a binding with a hole in it is an identity for inputs nobody
// supplied, so the refusal is what keeps the digest meaning what it says.
func TestAMigrationPlanRefusesAnIncompleteBinding(t *testing.T) {
	t.Parallel()

	// Each contract leaves out the fields the other one binds.
	for version, absent := range map[string]map[string]bool{
		"artifact": {"Source": true},
		"source":   {"ArtifactDigest": true, "VerificationPolicyUID": true, "VerificationPolicyDigest": true},
	} {
		bindingType := reflect.TypeOf(migrationplan.Binding{})
		for index := range bindingType.NumField() {
			field := bindingType.Field(index)
			if absent[field.Name] {
				continue
			}
			t.Run(version+"/"+field.Name, func(t *testing.T) {
				t.Parallel()

				mutated := completeBinding()
				if version == "source" {
					mutated = completeSourceBinding()
				}
				if _, err := mutated.Fingerprint(); err != nil {
					t.Fatalf("the complete binding was refused: %v", err)
				}
				value := reflect.ValueOf(&mutated).Elem().Field(index)
				value.Set(reflect.Zero(field.Type))

				if _, err := mutated.Fingerprint(); err == nil {
					t.Fatalf("a binding with no %s was given a fingerprint", field.Name)
				}
			})
		}
	}
}

// A plan computed from a ConfigMap binds the source in place of the artifact
// and the policy: another ConfigMap or other files make another plan, and a
// binding that mixes the two contracts has no identity.
func TestAMigrationPlanBindsItsSource(t *testing.T) {
	t.Parallel()

	source := completeSourceBinding()
	baseline, err := source.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if source.ContractVersion() != migrationplan.SourceContractVersion ||
		completeBinding().ContractVersion() != migrationplan.ContractVersion {
		t.Fatal("a binding does not carry the contract version of its source")
	}
	artifact, err := completeBinding().Fingerprint()
	if err != nil || artifact == baseline {
		t.Fatalf("a source plan and an artifact plan share a fingerprint: %v", err)
	}
	for name, mutate := range map[string]func(*migrationplan.Binding){
		"revision":       func(b *migrationplan.Binding) { b.Source.Revision = "another-configmap-uid" },
		"content digest": func(b *migrationplan.Binding) { b.Source.ContentDigest = digest('7') },
	} {
		mutated := completeSourceBinding()
		mutate(&mutated)
		got, err := mutated.Fingerprint()
		if err != nil || got == baseline {
			t.Errorf("another %s kept the fingerprint: %v", name, err)
		}
	}
	for name, mutate := range map[string]func(*migrationplan.Binding){
		"artifact digest": func(b *migrationplan.Binding) { b.ArtifactDigest = digest('c') },
		"policy UID":      func(b *migrationplan.Binding) { b.VerificationPolicyUID = "uid" },
		"policy digest":   func(b *migrationplan.Binding) { b.VerificationPolicyDigest = digest('0') },
		"source kind":     func(b *migrationplan.Binding) { b.Source.Kind = "" },
	} {
		mutated := completeSourceBinding()
		mutate(&mutated)
		if _, err := mutated.Fingerprint(); err == nil {
			t.Errorf("a source binding with a %s was given a fingerprint", name)
		}
	}
}

func completeSourceBinding() migrationplan.Binding {
	binding := completeBinding()
	binding.ArtifactDigest = ""
	binding.VerificationPolicyUID = ""
	binding.VerificationPolicyDigest = ""
	binding.Source = &fingerprint.PlanSource{Kind: "ConfigMap", Revision: "configmap-uid", ContentDigest: digest('2')}
	return binding
}

// A sequence the Apply Job could not carry is refused where it is selected,
// before it becomes a plan anyone is asked to approve. The same sequence with
// ordinary keys and checksums is selected.
func TestASequenceNoJobCouldCarryIsNotSelected(t *testing.T) {
	t.Parallel()
	report := func(key, checksum string) dataplane.MigrationStatusReport {
		records := make([]dataplane.MigrationRecord, 0, migrationplan.MaxMigrations)
		for version := int64(1); version <= migrationplan.MaxMigrations; version++ {
			records = append(records, dataplane.MigrationRecord{
				Version: version, VersionKey: key, Checksum: checksum, State: dataplane.MigrationStatePending,
			})
		}
		return dataplane.MigrationStatusReport{ContractVersion: dataplane.SupportedMigrationStatusContract, Migrations: records}
	}

	if _, err := migrationplan.Sequence(report("20260925120000", "sha256:"+strings.Repeat("a", 64))); err != nil {
		t.Fatalf("Sequence() of ordinary migrations: %v", err)
	}
	_, err := migrationplan.Sequence(report(strings.Repeat("é", 128), strings.Repeat("é", 128)))
	if err == nil || !strings.Contains(err.Error(), "an Apply Job can carry") {
		t.Fatalf("Sequence() error = %v, want the refusal of a sequence no Job could carry", err)
	}
}
