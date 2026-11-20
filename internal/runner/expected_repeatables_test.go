package runner

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/fingerprint"
)

// testRepeatables is the approved repeatable migrations an Apply Job of a plan
// that selected two carries.
func testRepeatables() string {
	return `[{"identity":"item_names","checksum":"h1:` + strings.Repeat("A", 43) + `=","applied_checksum":""},` +
		`{"identity":"order_totals","checksum":"h1:` + strings.Repeat("B", 43) + `=","applied_checksum":"h1:` +
		strings.Repeat("C", 43) + `="}]`
}

// sequenceDigestOf is the digest a plan binds for the two lists.
func sequenceDigestOf(t *testing.T, migrations, repeatables string) string {
	t.Helper()

	var entries []fingerprint.SequenceEntry
	if err := json.Unmarshal([]byte(migrations), &entries); err != nil {
		t.Fatal(err)
	}
	var approved []fingerprint.RepeatableEntry
	if repeatables != "" {
		if err := json.Unmarshal([]byte(repeatables), &approved); err != nil {
			t.Fatal(err)
		}
	}
	digest, err := fingerprint.MigrationSequenceDigest(entries, approved)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// The approved repeatable migrations reach Ptah in the file --expect-sequence
// names, after the versions, by identity and content digest in the plan's
// order, and the child does not inherit the variable they came in. A plan of
// repeatable migrations alone carries an empty versioned list.
func TestMigrationApplyHandsPtahTheApprovedRepeatables(t *testing.T) {
	t.Parallel()

	const repeatables = `"repeatables":[{"identity":"item_names","checksum":"h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="},` +
		`{"identity":"order_totals","checksum":"h1:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB="}]`
	for _, test := range []struct {
		name, migrations, want string
	}{
		{
			name:       "after two versions",
			migrations: testSequence(),
			want:       `{"migrations":[{"version":3,"version_key":"3"},{"version":4,"version_key":"4"}],` + repeatables + `}`,
		},
		{
			name:       "alone",
			migrations: `[]`,
			want:       `{"migrations":[],` + repeatables + `}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var sequence string
			var childEnvironment []string
			executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{
				stdout: migrationRunDocument("applied"),
				inspect: func(spec CommandSpec) {
					childEnvironment = spec.Env
					for index, argument := range spec.Args {
						if argument == "--expect-sequence" && index+1 < len(spec.Args) {
							content, err := os.ReadFile(spec.Args[index+1])
							if err != nil {
								t.Errorf("read the approved sequence while the child runs: %v", err)
							}
							sequence = string(content)
						}
					}
				},
			}}}
			environment := migrationApplyEnvironment(t, "migration-apply-repeatables")
			for index, variable := range environment {
				switch {
				case strings.HasPrefix(variable, envExpectedSequence+"="):
					environment[index] = envExpectedSequence + "=" + test.migrations
				case strings.HasPrefix(variable, envExpectedSequenceDigest+"="):
					environment[index] = envExpectedSequenceDigest + "=" + sequenceDigestOf(t, test.migrations, testRepeatables())
				}
			}
			environment = append(environment, envExpectedRepeatables+"="+testRepeatables())
			Run(context.Background(), Config{
				Operation:   OperationMigrationApply,
				Environment: withRunnerProtocol(environment),
				Executor:    executor,
				TempDir:     t.TempDir(),
			})
			if len(executor.calls) != 1 {
				t.Fatalf("executor calls = %d, want the child to have run", len(executor.calls))
			}
			if sequence != test.want {
				t.Fatalf("approved sequence file = %s, want %s", sequence, test.want)
			}
			for _, variable := range childEnvironment {
				if strings.HasPrefix(variable, envExpectedRepeatables+"=") {
					t.Fatalf("the child inherited %s", envExpectedRepeatables)
				}
			}
		})
	}
}

// The one digest binds both lists, so a Job that carries repeatable
// migrations its plan did not bind, or drops the ones it did, runs nothing.
// The control is the pair that agrees.
func TestTheApprovedSequenceBindsTheRepeatableMigrations(t *testing.T) {
	t.Parallel()

	both := sequenceDigestOf(t, testSequence(), testRepeatables())
	versionsOnly := sequenceDigestOf(t, testSequence(), "")
	if _, err := approvedSequenceDocument(testSequence(), testRepeatables(), both); err != nil {
		t.Fatalf("the agreeing pair: %v", err)
	}
	if both == versionsOnly {
		t.Fatal("the repeatable migrations moved nothing in the digest")
	}
	reordered := `[` + strings.SplitN(strings.TrimPrefix(testRepeatables(), "["), "},", 2)[1]
	reordered = strings.TrimSuffix(reordered, "]") + `,` + strings.SplitN(strings.TrimPrefix(testRepeatables(), "["), "},", 2)[0] + `}]`
	for _, test := range []struct {
		name, repeatables, digest, says string
	}{
		{name: "carried and not bound", repeatables: testRepeatables(), digest: versionsOnly, says: "does not match the digest"},
		{name: "bound and not carried", repeatables: "", digest: both, says: "does not match the digest"},
		{name: "carried in another order", repeatables: reordered, digest: both, says: "does not match the digest"},
		{name: "an empty list", repeatables: `[]`, digest: versionsOnly, says: "an empty list"},
		{name: "a field the digest does not bind", repeatables: strings.Replace(testRepeatables(), `"applied_checksum":""`,
			`"applied_checksum":"","path":"R__item_names.sql"`, 1), digest: both, says: "is not readable"},
		{name: "two documents", repeatables: testRepeatables() + testRepeatables(), digest: both, says: "more than one document"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := approvedSequenceDocument(testSequence(), test.repeatables, test.digest)
			if err == nil || !strings.Contains(err.Error(), test.says) {
				t.Fatalf("approvedSequenceDocument() error = %v, want one that says %q", err, test.says)
			}
		})
	}
	if _, err := approvedSequenceDocument(`[]`, "", versionsOnly); err == nil {
		t.Fatal("a sequence with neither list was accepted")
	}
}
