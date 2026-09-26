package runner_test

import (
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/runner"
)

// TestBuildCommand_MigrationOperations pins what a migration Job runs: the
// materialized directory, and the machine-readable document.
func TestBuildCommand_MigrationOperations(t *testing.T) {
	const directory = "/source/migrations"
	tests := []struct {
		name      string
		operation runner.Operation
		want      []string
	}{
		{
			name:      "history reads and changes nothing",
			operation: runner.OperationMigrationHistory,
			want:      []string{"migrations", "status", "--migrations-dir", directory, "--json"},
		},
		{
			name:      "apply runs the approved migrations",
			operation: runner.OperationMigrationApply,
			want:      []string{"migrations", "up", "--migrations-dir", directory, "--json", "--expect-sequence", "/tmp/expected.json"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec, err := runner.BuildCommand("/usr/local/bin/ptah", test.operation, runner.Inputs{
				MigrationsDir:        directory,
				ExpectedSequencePath: "/tmp/expected.json",
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

// TestBuildCommand_MigrationRefusesAReference is the credential boundary. Ptah
// accepts `--migrations-dir oci://...` and fetches the artifact itself, which
// would put the registry in the same process as the database; the artifact is
// materialized by a fetch container that holds the registry credentials this
// one does not.
func TestBuildCommand_MigrationRefusesAReference(t *testing.T) {
	tests := []struct {
		name      string
		directory string
		message   string
	}{
		{
			name:      "an OCI reference",
			directory: "oci://registry.example/team/app-migrations@sha256:" + strings.Repeat("a", 64),
			message:   "must be a materialized local path, not a reference",
		},
		{
			name:      "a relative path",
			directory: "migrations",
			message:   "must be an absolute path",
		},
		{
			name:      "nothing at all",
			directory: "",
			message:   "migration directory is empty",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, operation := range []runner.Operation{
				runner.OperationMigrationHistory,
				runner.OperationMigrationApply,
			} {
				_, err := runner.BuildCommand("/usr/local/bin/ptah", operation, runner.Inputs{
					MigrationsDir: test.directory,
				})
				if err == nil || !strings.Contains(err.Error(), test.message) {
					t.Fatalf("BuildCommand(%s) error = %v, want %q", operation, err, test.message)
				}
			}
		})
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

// Every migration Apply names the sequence its plan approved, and nothing else
// does. `migrations status` takes no --expect-sequence, so the history read
// carrying it would die before it reported. An Apply with no sequence is not
// built at all: without it, Ptah runs whatever it selects.
func TestMigrationCommandNamesTheApprovedSequenceOnlyOnTheApply(t *testing.T) {
	t.Parallel()

	const directory = "/migrations"
	tests := []struct {
		name      string
		operation runner.Operation
		path      string
		want      []string
		wantError string
	}{
		{
			name:      "the apply names it",
			operation: runner.OperationMigrationApply,
			path:      "/tmp/expected.json",
			want:      []string{"migrations", "up", "--migrations-dir", directory, "--json", "--expect-sequence", "/tmp/expected.json"},
		},
		{
			name:      "the history read never does",
			operation: runner.OperationMigrationHistory,
			path:      "/tmp/expected.json",
			want:      []string{"migrations", "status", "--migrations-dir", directory, "--json"},
		},
		{
			name:      "an apply with no sequence is refused",
			operation: runner.OperationMigrationApply,
			wantError: "approved sequence",
		},
		{
			name:      "an apply with a relative sequence path is refused",
			operation: runner.OperationMigrationApply,
			path:      "expected.json",
			wantError: "approved sequence",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			spec, err := runner.BuildCommand("/usr/local/bin/ptah", test.operation, runner.Inputs{
				MigrationsDir:        directory,
				ExpectedSequencePath: test.path,
			})
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("BuildCommand() error = %v, want substring %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildCommand() error = %v", err)
			}
			if strings.Join(spec.Args, " ") != strings.Join(test.want, " ") {
				t.Fatalf("args = %v, want %v", spec.Args, test.want)
			}
		})
	}
}

// A mode nobody asked for has to leave the command exactly as it was. Every
// PtahMigration that sets no mode runs through this code, and a flag appearing
// for it would change how it executes without anyone editing it.
func TestMigrationCommandCarriesTheTransactionModeOnlyWhenAsked(t *testing.T) {
	t.Parallel()

	const directory = "/migrations"
	const sequence = "/tmp/expected.json"
	tests := []struct {
		name      string
		operation runner.Operation
		mode      string
		want      []string
		wantError string
	}{
		{
			name: "unset leaves the command as it was",
			mode: "",
			want: []string{"migrations", "up", "--migrations-dir", directory, "--json", "--expect-sequence", sequence},
		},
		{
			name: "blank is the same as unset",
			mode: "   ",
			want: []string{"migrations", "up", "--migrations-dir", directory, "--json", "--expect-sequence", sequence},
		},
		{
			// The history read is why this case exists. `migrations status`
			// has no --tx-mode, so carrying the mode there is an unknown flag
			// and the read dies before it reports -- which left a resource
			// that named a mode stuck in Reading, never reaching its gate.
			name:      "the history read never carries the mode",
			operation: runner.OperationMigrationHistory,
			mode:      "none",
			want:      []string{"migrations", "status", "--migrations-dir", directory, "--json"},
		},
		{
			name: "none is carried through",
			mode: "none",
			want: []string{"migrations", "up", "--migrations-dir", directory, "--json", "--tx-mode", "none", "--expect-sequence", sequence},
		},
		{
			name: "file is carried through",
			mode: "file",
			want: []string{"migrations", "up", "--migrations-dir", directory, "--json", "--tx-mode", "file", "--expect-sequence", sequence},
		},
		{
			name:      "a mode this build does not know is refused here",
			mode:      "statement",
			wantError: "unsupported transaction mode",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			operation := test.operation
			if operation == "" {
				operation = runner.OperationMigrationApply
			}
			spec, err := runner.BuildCommand("/usr/local/bin/ptah", operation, runner.Inputs{
				MigrationsDir:        directory,
				TransactionMode:      test.mode,
				ExpectedSequencePath: sequence,
			})
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("BuildCommand() error = %v, want substring %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildCommand() error = %v", err)
			}
			if strings.Join(spec.Args, " ") != strings.Join(test.want, " ") {
				t.Fatalf("args = %v, want %v", spec.Args, test.want)
			}
		})
	}
}
