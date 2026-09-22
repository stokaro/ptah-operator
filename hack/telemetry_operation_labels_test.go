package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/telemetry"
)

// enumMarker matches the marker that decides which values the API server
// accepts for a type, and the declaration it governs.
var enumMarker = regexp.MustCompile(
	`(?m)^// \+kubebuilder:validation:Enum=([A-Za-z;]+)\ntype ([A-Za-z]+) ([a-z]+)$`)

// Every operation the API accepts has to reach a label of its own.
//
// The two enums are different sets: a migration reads its recorded history
// where a schema observes the live database. Telemetry used to carry one label
// space for both, so History arrived as "unknown" -- a real operation, counted
// under the name reserved for a value nothing recognizes, in the metric an
// operator reads to find out which stage is slow or failing.
//
// This reads the markers rather than a list written here, so an operation
// added to either enum fails until it has a label.
func TestEveryOperationTheAPIAcceptsHasALabel(t *testing.T) {
	t.Parallel()
	schemaOperations := declaredEnumValues(t, "api/v1alpha1/ptahschema_types.go", "OperationType")
	migrationOperations := declaredEnumValues(t, "api/v1alpha1/ptahmigration_types.go", "MigrationOperationType")
	if len(schemaOperations) == 0 || len(migrationOperations) == 0 {
		t.Fatal("no operation values were read, so this check would pass over nothing")
	}
	for _, value := range schemaOperations {
		operation := telemetry.OperationForSchema(operatorv1alpha1.OperationType(value))
		if operation == "" {
			t.Errorf("schema operation %q reaches telemetry with no label", value)
		}
		if stage := telemetry.StageForOperation(operatorv1alpha1.OperationType(value)); stage == telemetry.FailureStageController {
			t.Errorf("schema operation %q fails as the controller's own failure", value)
		}
	}
	for _, value := range migrationOperations {
		operation := telemetry.OperationForMigration(operatorv1alpha1.MigrationOperationType(value))
		if operation == "" {
			t.Errorf("migration operation %q reaches telemetry with no label", value)
		}
		stage := telemetry.StageForMigrationOperation(operatorv1alpha1.MigrationOperationType(value))
		if stage == telemetry.FailureStageController {
			t.Errorf("migration operation %q fails as the controller's own failure", value)
		}
	}
}

// declaredEnumValues reads the values a +kubebuilder:validation:Enum marker
// admits for the named type.
func declaredEnumValues(t *testing.T, relative, typeName string) []string {
	t.Helper()
	path := filepath.Join(repositoryRoot(t), relative)
	source, err := os.ReadFile(path) //nolint:gosec // A path this test built from the repository root.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, match := range enumMarker.FindAllStringSubmatch(string(source), -1) {
		if match[2] != typeName {
			continue
		}
		return strings.Split(match[1], ";")
	}
	t.Fatalf("%s declares no enum marker for %s", relative, typeName)
	return nil
}

// The reader has to refuse what would leave the check above green over
// nothing: a marker on another type, a type with no marker, and a marker whose
// values it cannot separate.
func TestDeclaredEnumValuesReadsOnlyTheMarkerItWasAskedFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		source string
		want   []string
	}{
		{
			name:   "the marker this repository writes",
			source: "// +kubebuilder:validation:Enum=Resolve;Verify;History;Apply\ntype Wanted string\n",
			want:   []string{"Resolve", "Verify", "History", "Apply"},
		},
		{
			name:   "a marker on a neighboring type",
			source: "// +kubebuilder:validation:Enum=One;Two\ntype Other string\n",
		},
		{
			name:   "a type with no marker",
			source: "// Wanted is documented and unconstrained.\ntype Wanted string\n",
		},
		{
			name:   "a marker separated from its type by a blank line",
			source: "// +kubebuilder:validation:Enum=One;Two\n\ntype Wanted string\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var found []string
			for _, match := range enumMarker.FindAllStringSubmatch(test.source, -1) {
				if match[2] != "Wanted" {
					continue
				}
				found = strings.Split(match[1], ";")
			}
			if fmt.Sprint(found) != fmt.Sprint(test.want) {
				t.Fatalf("read %v, want %v", found, test.want)
			}
		})
	}
}
