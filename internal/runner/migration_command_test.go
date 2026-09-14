package runner_test

import (
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/runner"
)

// TestBuildCommand_MigrationOperations pins what a migration Job runs: the
// artifact by digest, and the machine-readable document.
func TestBuildCommand_MigrationOperations(t *testing.T) {
	pinned := "oci://registry.example/team/app-migrations@sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		name      string
		operation runner.Operation
		want      []string
	}{
		{
			name:      "history reads and changes nothing",
			operation: runner.OperationMigrationHistory,
			want:      []string{"migrations", "status", "--migrations-dir", pinned, "--json"},
		},
		{
			name:      "apply runs the pending migrations",
			operation: runner.OperationMigrationApply,
			want:      []string{"migrations", "up", "--migrations-dir", pinned, "--json"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec, err := runner.BuildCommand("/usr/local/bin/ptah", test.operation, runner.Inputs{
				ResolvedReference: pinned,
			})
			if err != nil {
				t.Fatalf("BuildCommand() error = %v", err)
			}
			if strings.Join(spec.Args, " ") != strings.Join(test.want, " ") {
				t.Fatalf("args = %v, want %v", spec.Args, test.want)
			}
		})
	}
}

// TestBuildCommand_MigrationRefusesAnUnpinnedArtifact is the boundary that
// keeps a Job from resolving a tag of its own: the controller resolved and
// verified exact bytes, and those are the bytes the Job must run.
func TestBuildCommand_MigrationRefusesAnUnpinnedArtifact(t *testing.T) {
	for _, operation := range []runner.Operation{
		runner.OperationMigrationHistory,
		runner.OperationMigrationApply,
	} {
		_, err := runner.BuildCommand("/usr/local/bin/ptah", operation, runner.Inputs{
			ResolvedReference: "oci://registry.example/team/app-migrations:stable",
		})
		if err == nil || !strings.Contains(err.Error(), "not pinned to a digest") {
			t.Fatalf("BuildCommand(%s) error = %v, want a refusal of the unpinned reference", operation, err)
		}
	}
}

// TestOperationMutating separates the operations that may change a database
// from the ones that may not.
func TestOperationMutating(t *testing.T) {
	tests := []struct {
		operation runner.Operation
		mutating  bool
	}{
		{operation: runner.OperationResolve, mutating: false},
		{operation: runner.OperationVerify, mutating: false},
		{operation: runner.OperationObserve, mutating: false},
		{operation: runner.OperationPlan, mutating: false},
		{operation: runner.OperationApply, mutating: true},
		{operation: runner.OperationMigrationHistory, mutating: false},
		{operation: runner.OperationMigrationApply, mutating: true},
	}

	for _, test := range tests {
		t.Run(string(test.operation), func(t *testing.T) {
			if !test.operation.Valid() {
				t.Fatalf("%s is not a valid operation", test.operation)
			}
			if got := test.operation.Mutating(); got != test.mutating {
				t.Fatalf("%s mutating = %v, want %v", test.operation, got, test.mutating)
			}
		})
	}
}
