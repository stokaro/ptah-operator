package migrationplan_test

import (
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

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
		ControllerImage:          "example.invalid/manager@" + digest('2'),
		ControllerRevision:       "controller-revision",
		ControllerStateVersion:   1,
		PtahVersion:              "v0.3.0",
		ExecutorImage:            "example.invalid/ptah@" + digest('3'),
		RunnerImage:              "example.invalid/operator@" + digest('4'),
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

	bindingType := reflect.TypeOf(migrationplan.Binding{})
	for index := range bindingType.NumField() {
		field := bindingType.Field(index)
		t.Run(field.Name, func(t *testing.T) {
			t.Parallel()

			mutated := completeBinding()
			value := reflect.ValueOf(&mutated).Elem().Field(index)
			value.Set(reflect.Zero(field.Type))

			if _, err := mutated.Fingerprint(); err == nil {
				t.Fatalf("a binding with no %s was given a fingerprint", field.Name)
			}
		})
	}
}
