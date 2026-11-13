package runnerharness_test

import (
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
	"github.com/stokaro/ptah-operator/internal/workload"
	"github.com/stokaro/ptah-operator/test/e2e/gitfixture"
	"github.com/stokaro/ptah-operator/test/e2e/gitrepo"
	"github.com/stokaro/ptah-operator/test/runnerharness"
	"github.com/stokaro/ptah-operator/test/runnerharness/volumes"
)

// A lifecycle's result the fault rows need: a plan each engine really saved, and
// a sequence each engine really selected.
type faultInputs struct {
	schema    *schemaRun
	plan      []byte
	migration *migrationRun
	planned   []operatorv1alpha1.PlannedMigration
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repositoryRoot, "testdata", "e2e", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func harnessFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "sources", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func ociSource(reference string) operatorv1alpha1.SourceSpec {
	return operatorv1alpha1.SourceSpec{
		OCIRef: reference,
		VerificationPolicyFrom: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: policyConfigMap},
			Key:                  policyKey,
		},
		Transport: operatorv1alpha1.OCITransportSpec{PlainHTTP: true},
	}
}

func storedAs(command, file string) func(*rowSpec) {
	return func(row *rowSpec) {
		if row.documents == nil {
			row.documents = map[string]string{}
		}
		row.documents[command] = file
	}
}

func savedAs(file string) func(*rowSpec) {
	return func(row *rowSpec) { row.savedPlan = file }
}

func wantCode(code string) func(*rowSpec) {
	return func(row *rowSpec) { row.wantCode = code }
}

func wantRefusal(word string) func(*rowSpec) {
	return func(row *rowSpec) { row.wantRefusal = word }
}

// schemaLifecycle runs a declared schema through every operation on one engine,
// from an OCI artifact: a table, a converged reading, a column added to it, and
// an Apply refused because the database moved under its plan.
func schemaLifecycle(t *testing.T, l *lab, engine string, inputs *faultInputs) {
	t.Helper()
	family := "schema-" + engine
	database := "harness_schema_" + engine
	l.createDatabase(t, engine, database)
	version := func(number string) []string {
		return []string{"--schema-file", "/fixtures/" + engineFile(engine) + "-" + number + ".sql", "--dialect", engineFile(engine)}
	}
	first := l.publish(t, "schema", "schemas/"+family, "v1", version("v1")...)
	second := l.publish(t, "schema", "schemas/"+family, "v2", version("v2")...)

	policy := fixture(t, "verification-policy-digest-pin.yaml")
	run := l.newSchema(t, family, engine, l.databaseURL(engine, database), ociSource(first))
	run.resolve("resolve-v1")
	// A tag fails the digest pin, so the Verify the manager acts on names the
	// digest; the requested reference is the digest it resolved to.
	run.schema.Spec.Desired.OCIRef = run.schema.Status.Source.ResolvedReference
	run.schema.Status.Source.RequestedReference = run.schema.Status.Source.ResolvedReference
	run.verify("verify-v1", policy)
	run.observe("observe-empty")
	planned := run.plan("plan-v1", nil)
	run.publishPlan("plan-v1", planned.plan)
	run.apply("apply-v1")
	run.observe("observe-converged")
	run.plan("plan-converged", nil)

	run.schema.Spec.Desired.OCIRef = second
	run.resolve("resolve-v2")
	run.schema.Spec.Desired.OCIRef = run.schema.Status.Source.ResolvedReference
	run.schema.Status.Source.RequestedReference = run.schema.Status.Source.ResolvedReference
	run.verify("verify-v2", policy)
	run.observe("observe-column")

	adjust := []func(*rowSpec){}
	if engine == enginePostgres {
		adjust = append(adjust,
			storedAs("schema plan", "schema-plan-report.json"),
			storedAs("schema apply --dry-run", "schema-apply-dry-run-report.json"))
	}
	column := run.plan("plan-column", nil, adjust...)

	// The database moves under the plan, and the Apply refuses it.
	l.sql(t, engine, database, "CREATE TABLE harness_moved (id int PRIMARY KEY)")
	run.publishPlan("plan-column", column.plan)
	run.apply("apply-stale", wantCode("stale_plan"))
	l.sql(t, engine, database, "DROP TABLE harness_moved")
	run.apply("apply-column")

	inputs.schema, inputs.plan = run, planned.plan
}

func engineFile(engine string) string {
	if engine == engineMySQL {
		return "mysql"
	}
	return "postgresql"
}

// verificationRefusals are the ways a Verify ends short of success: the runner's
// own refusal of a requested tag, Ptah's refusal, with one finding and with
// several, and a verification Ptah could not run.
//
// Ptah's refusal writes its report and exits 2, and also writes the findings to
// standard error. The runner reads that text as a refusal only when it is what
// dataplane.VerifyRefusalDiagnostics makes of the report, so these rows hold that
// text to the pinned image. A failure that is no verdict has a line on standard
// error too, and no report.
func verificationRefusals(t *testing.T, l *lab) {
	t.Helper()
	family := "schema-verify-refused"
	reference := l.publish(t, "schema", "schemas/"+family, "v1", "--schema-file", "/fixtures/postgresql-v1.sql", "--dialect", "postgresql")
	run := l.newSchema(t, family, enginePostgres, l.databaseURL(enginePostgres, "harness_absent"), ociSource(reference))
	run.resolve("resolve")
	run.verify("verify-digest-pin", fixture(t, "verification-policy-digest-pin.yaml"), wantCode("verification_refused"))
	run.schema.Spec.Desired.OCIRef = run.schema.Status.Source.ResolvedReference
	run.schema.Status.Source.RequestedReference = run.schema.Status.Source.ResolvedReference
	refused := run.verify("verify-other-type", harnessFixture(t, "verification-policy-other-type.yaml"), wantCode("verification_refused"))
	requireRequirements(t, refused, "artifact_types")
	refused = run.verify("verify-two-findings", harnessFixture(t, "verification-policy-two-findings.yaml"), wantCode("verification_refused"))
	requireRequirements(t, refused, "artifact_types", "require_signature")
	run.verify("verify-unreadable-policy", harnessFixture(t, "verification-policy-unreadable.yaml"), wantCode("invalid_verification_output"))

	migrationFamily := "migration-verify-refused"
	migrations := l.publish(t, "migrations", "migrations/"+migrationFamily, "v1", "--migrations-dir", "/fixtures/migrations-"+engineFile(enginePostgres))
	migration := l.newMigration(t, migrationFamily, enginePostgres, l.databaseURL(enginePostgres, "harness_absent"), ociSource(migrations))
	migration.resolve("resolve")
	refused = migration.verify("verify-other-type", harnessFixture(t, "verification-policy-other-type.yaml"), wantCode("verification_refused"))
	requireRequirements(t, refused, "artifact_types")
}

// requireRequirements holds a refusal's frame to the requirements Ptah named.
func requireRequirements(t *testing.T, outcome rowOutcome, want ...string) {
	t.Helper()
	if !slices.Equal(outcome.result.VerificationRequirements, want) {
		t.Errorf("the refusal names %v, want %v", outcome.result.VerificationRequirements, want)
	}
}

// featureLifecycle is what earlier repins missed: statements a feature owner
// renders and the drift it reports.
func featureLifecycle(t *testing.T, l *lab) {
	t.Helper()
	policy := fixture(t, "verification-policy-digest-pin.yaml")
	ready := func(run *schemaRun) {
		run.resolve("resolve")
		run.schema.Spec.Desired.OCIRef = run.schema.Status.Source.ResolvedReference
		run.schema.Status.Source.RequestedReference = run.schema.Status.Source.ResolvedReference
		run.verify("verify", policy)
	}

	t.Run("extension", func(t *testing.T) {
		t.Parallel()
		family, database := "extension", "harness_extension"
		l.createDatabase(t, enginePostgres, database)
		reference := l.publish(t, "schema", "schemas/"+family, "v1", "--schema-file", "/fixtures/extension-postgresql-v1.sql", "--dialect", "postgresql")
		run := l.newSchema(t, family, enginePostgres, l.databaseURL(enginePostgres, database), ociSource(reference))
		ready(run)
		planned := run.plan("plan", nil, savedAs("plan-owned-extension.json"))
		run.publishPlan("plan", planned.plan)
		run.apply("apply")
		l.sql(t, enginePostgres, database, "DROP EXTENSION pg_trgm")
		run.observe("observe-dropped", storedAs("schema drift", "drift-feature-extension.json"))
	})

	t.Run("policy-new-table", func(t *testing.T) {
		t.Parallel()
		family, database := "policy-new-table", "harness_policy_new"
		l.createDatabase(t, enginePostgres, database)
		reference := l.publish(t, "schema", "schemas/"+family, "v1", "--schema-file", "/fixtures/policy-postgresql.sql", "--dialect", "postgresql")
		run := l.newSchema(t, family, enginePostgres, l.databaseURL(enginePostgres, database), ociSource(reference))
		ready(run)
		planned := run.plan("plan", nil, savedAs("plan-owned-policy-new-table.json"))
		run.publishPlan("plan", planned.plan)
		run.apply("apply")
		run.observe("observe-converged")
	})

	t.Run("policy-existing-table", func(t *testing.T) {
		t.Parallel()
		family, database := "policy-existing-table", "harness_policy_existing"
		l.createDatabase(t, enginePostgres, database)
		l.sql(t, enginePostgres, database,
			"CREATE TABLE e2e_policy_notes (id bigint NOT NULL PRIMARY KEY, owner_name text NOT NULL)",
			"ALTER TABLE e2e_policy_notes ENABLE ROW LEVEL SECURITY")
		reference := l.publish(t, "schema", "schemas/"+family, "v1", "--schema-file", "/fixtures/policy-postgresql.sql", "--dialect", "postgresql")
		run := l.newSchema(t, family, enginePostgres, l.databaseURL(enginePostgres, database), ociSource(reference))
		ready(run)
		run.observe("observe-no-policy", storedAs("schema drift", "drift-feature-policy.json"))
		planned := run.plan("plan", nil, savedAs("plan-owned-policy-widens.json"))
		run.publishPlan("plan", planned.plan)
		run.apply("apply")
	})

	for _, engine := range []string{enginePostgres, engineMySQL} {
		t.Run("rows-"+engine, func(t *testing.T) {
			t.Parallel()
			family, database := "rows-"+engine, "harness_rows_"+engine
			l.createDatabase(t, engine, database)
			reference := l.publish(t, "schema", "schemas/"+family, "v1", "--root-dir", "/fixtures/reference")
			run := l.newSchema(t, family, engine, l.databaseURL(engine, database), ociSource(reference))
			ready(run)
			planned := run.plan("plan", nil)
			run.publishPlan("plan", planned.plan)
			run.apply("apply")
			l.sql(t, engine, database,
				"DELETE FROM regions WHERE code = 'amer'",
				"UPDATE regions SET name = 'Edited by hand' WHERE code = 'emea'",
				"INSERT INTO regions (code, name) VALUES ('apac', 'Asia and Pacific')")
			var adjust []func(*rowSpec)
			if engine == enginePostgres {
				adjust = append(adjust, storedAs("schema drift", "drift-managed-rows.json"))
			}
			run.observe("observe-edited", adjust...)
			run.plan("plan-edited", nil)
			// The fence holds declared rows, and these rows moved.
			run.plan("plan-fenced", []string{"regions"}, wantCode("protected_table"))
		})
	}
}

// migrationLifecycle runs a migration directory through every operation on one
// engine, from an OCI artifact.
func migrationLifecycle(t *testing.T, l *lab, engine string, inputs *faultInputs) {
	t.Helper()
	family := "migrations-" + engine
	database := "harness_migrations_" + engine
	l.createDatabase(t, engine, database)
	reference := l.publish(t, "migrations", "migrations/"+family, "v1", "--migrations-dir", "/fixtures/migrations-"+engineFile(engine))
	run := l.newMigration(t, family, engine, l.databaseURL(engine, database), ociSource(reference))
	run.resolve("resolve")
	run.verify("verify", fixture(t, "verification-policy-migrations.yaml"))
	before := run.history("history-pending")
	planned := pending(t, family, before)
	run.applyPlanned("apply", planned)
	run.history("history-applied")
	inputs.migration, inputs.planned = run, planned
}

// migrationFailures are the runs that stop part way: on PostgreSQL in a
// transaction that commits nothing of the file, and on MySQL with no transaction
// around it, so that two statements stay.
func migrationFailures(t *testing.T, l *lab) {
	t.Helper()
	t.Run("failed-postgres", func(t *testing.T) {
		t.Parallel()
		family, database := "migrations-failed-postgres", "harness_failed_postgres"
		l.createDatabase(t, enginePostgres, database)
		reference := l.publish(t, "migrations", "migrations/"+family, "v1", "--migrations-dir", "/fixtures/migrations-failed-postgresql")
		run := l.newMigration(t, family, enginePostgres, l.databaseURL(enginePostgres, database), ociSource(reference))
		run.resolve("resolve")
		before := run.history("history-pending")
		run.applyPlanned("apply", pending(t, family, before), storedAs("migrations up", "migrations-up-failed-postgres.json"), wantCode("child_exit"))
		run.history("history-dirty")
	})
	// A checkpoint is the floor a fresh database starts from, and the history
	// reports it. No other row reads checkpoint_version.
	t.Run("checkpoint-postgres", func(t *testing.T) {
		t.Parallel()
		family, database := "migrations-checkpoint-postgres", "harness_checkpoint_postgres"
		l.createDatabase(t, enginePostgres, database)
		reference := l.publish(t, "migrations", "migrations/"+family, "v1", "--migrations-dir", "/fixtures/migrations-checkpoint-postgresql")
		run := l.newMigration(t, family, enginePostgres, l.databaseURL(enginePostgres, database), ociSource(reference))
		run.resolve("resolve")
		before := run.history("history-pending")
		run.applyPlanned("apply", pending(t, family, before))
		run.history("history-applied")
	})
	// A file that declares it runs outside a transaction says so per migration.
	// No other row reads transaction_mode; the file itself waits to be stopped,
	// so the row reads the history and applies nothing.
	t.Run("no-transaction-postgres", func(t *testing.T) {
		t.Parallel()
		family, database := "migrations-no-transaction-postgres", "harness_no_transaction_postgres"
		l.createDatabase(t, enginePostgres, database)
		reference := l.publish(t, "migrations", "migrations/"+family, "v1", "--migrations-dir", "/fixtures/migrations-stopped-postgresql")
		run := l.newMigration(t, family, enginePostgres, l.databaseURL(enginePostgres, database), ociSource(reference))
		run.resolve("resolve")
		run.history("history-pending")
	})
	// A deferred foreign key fails at the commit, after every statement ran.
	// Ptah reports statement 0 beside commit true, and no migration of the
	// failed transaction is recorded applied.
	for _, row := range []struct{ name, fixtures, stored string }{
		{"commit-postgres", "migrations-commit-postgresql", "migrations-up-commit-failed.json"},
		{"commit-second-postgres", "migrations-commit-second-postgresql", "migrations-up-commit-failed-second.json"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			family, database := "migrations-"+row.name, "harness_"+strings.ReplaceAll(row.name, "-", "_")
			l.createDatabase(t, enginePostgres, database)
			reference := l.publish(t, "migrations", "migrations/"+family, "v1", "--migrations-dir", "/fixtures/"+row.fixtures)
			run := l.newMigration(t, family, enginePostgres, l.databaseURL(enginePostgres, database), ociSource(reference))
			run.resolve("resolve")
			before := run.history("history-pending")
			run.applyPlanned("apply", pending(t, family, before), storedAs("migrations up", row.stored), wantCode("child_exit"))
			run.history("history-dirty")
		})
	}
	t.Run("partial-mysql", func(t *testing.T) {
		t.Parallel()
		family, database := "migrations-partial-mysql", "harness_partial_mysql"
		l.createDatabase(t, engineMySQL, database)
		reference := l.publish(t, "migrations", "migrations/"+family, "v1", "--migrations-dir", "/fixtures/migrations-partial-mysql")
		run := l.newMigration(t, family, engineMySQL, l.databaseURL(engineMySQL, database), ociSource(reference))
		run.migration.Spec.Policy.TransactionMode = "none"
		run.resolve("resolve")
		before := run.history("history-pending")
		run.applyPlanned("apply", pending(t, family, before), storedAs("migrations up", "migrations-up-partial-mysql.json"), wantCode("child_exit"))
		run.history("history-dirty")
	})
	// The log table is there before the run, with an actor column narrower
	// than the name the manager passes for a run under apply: Always. MySQL
	// refuses each entry, Ptah applies every migration all the same, and the
	// run reports the entries it lost.
	t.Run("log-lost-mysql", func(t *testing.T) {
		t.Parallel()
		family, database := "migrations-log-lost-mysql", "harness_log_lost_mysql"
		l.createDatabase(t, engineMySQL, database)
		l.sql(t, engineMySQL, database, narrowMigrationLog)
		reference := l.publish(t, "migrations", "migrations/"+family, "v1", "--migrations-dir", "/fixtures/migrations-"+engineFile(engineMySQL))
		run := l.newMigration(t, family, engineMySQL, l.databaseURL(engineMySQL, database), ociSource(reference))
		run.actor = workload.MigrationActor(run.migration, nil)
		run.resolve("resolve")
		before := run.history("history-pending")
		run.applyPlanned("apply", pending(t, family, before), storedAs("migrations up", "migrations-up-log-lost-mysql.json"))
		run.history("history-applied")
	})
}

// narrowMigrationLog is Ptah's schema_migrations_log for MySQL with the actor
// column cut to 16 characters, which no name the manager records fits.
const narrowMigrationLog = `CREATE TABLE schema_migrations_log (
    run_id VARCHAR(64) NOT NULL,
    seq BIGINT NOT NULL,
    operation VARCHAR(32) NOT NULL,
    version BIGINT NOT NULL,
    state VARCHAR(32) NOT NULL,
    actor VARCHAR(16) NULL,
    actor_source VARCHAR(32) NOT NULL,
    checksum VARCHAR(64) NULL,
    logged_at TIMESTAMP NOT NULL,
    error TEXT NULL,
    PRIMARY KEY (run_id, seq)
) ENGINE=InnoDB`

// gitLifecycles read a schema and a migration directory from Git through the
// runner: the Resolve's pull, the fetch before each operation, and the refusals
// of both.
func gitLifecycles(t *testing.T, l *lab) {
	t.Helper()
	t.Run("schema-https", func(t *testing.T) {
		t.Parallel()
		family, database := "schema-git-https", "harness_git_https"
		l.createDatabase(t, enginePostgres, database)
		source := operatorv1alpha1.SourceSpec{Git: l.gitHTTPS(gitfixture.SchemaPath, operatorv1alpha1.GitRef{Branch: "main"})}
		run := l.newSchema(t, family, enginePostgres, l.databaseURL(enginePostgres, database), source)
		run.resolve("resolve")
		run.observe("observe-empty")
		planned := run.plan("plan", nil)
		run.publishPlan("plan", planned.plan)
		run.apply("apply")
		run.observe("observe-converged")
	})
	t.Run("schema-ssh", func(t *testing.T) {
		t.Parallel()
		family, database := "schema-git-ssh", "harness_git_ssh"
		l.createDatabase(t, enginePostgres, database)
		source := operatorv1alpha1.SourceSpec{Git: l.gitSSH(gitfixture.SchemaPath, operatorv1alpha1.GitRef{Tag: "v1"})}
		run := l.newSchema(t, family, enginePostgres, l.databaseURL(enginePostgres, database), source)
		run.resolve("resolve")
		run.observe("observe-empty")
		planned := run.plan("plan", nil)
		run.publishPlan("plan", planned.plan)
		run.apply("apply")
	})
	t.Run("refusals", func(t *testing.T) {
		t.Parallel()
		database := "harness_absent"
		for name, selected := range map[string]string{
			"resolve-submodule":   gitfixture.SubmodulePath,
			"resolve-lfs-pointer": gitfixture.LFSPath,
		} {
			source := operatorv1alpha1.SourceSpec{Git: l.gitHTTPS(selected, operatorv1alpha1.GitRef{Branch: "main"})}
			run := l.newSchema(t, "schema-git-refused", enginePostgres, l.databaseURL(enginePostgres, database), source)
			run.resolve(name, wantCode("child_exit"))
		}
		// The fetch of a commit whose content is not the one the Resolve read.
		source := operatorv1alpha1.SourceSpec{Git: l.gitHTTPS(gitfixture.SchemaPath, operatorv1alpha1.GitRef{Branch: "main"})}
		run := l.newSchema(t, "schema-git-refused", enginePostgres, l.databaseURL(enginePostgres, database), source)
		run.resolve("resolve-for-fetch")
		revision := run.schema.Status.Source.Git
		other, err := gitfixture.SchemaContent(0)
		if err != nil {
			t.Fatal(err)
		}
		revision.ContentDigest = other
		run.observe("observe-other-content", wantRefusal(runner.CodeGitSourceRefused))
	})
	t.Run("migrations-ssh", func(t *testing.T) {
		t.Parallel()
		family, database := "migrations-git-ssh", "harness_git_migrations"
		l.createDatabase(t, engineMySQL, database)
		source := operatorv1alpha1.SourceSpec{Git: l.gitSSH(gitfixture.MigrationPath, operatorv1alpha1.GitRef{Tag: "v1"})}
		run := l.newMigration(t, family, engineMySQL, l.databaseURL(engineMySQL, database), source)
		run.resolve("resolve")
		before := run.history("history-pending")
		run.applyPlanned("apply", pending(t, family, before))
		run.history("history-applied")
	})
	t.Run("migrations-https", func(t *testing.T) {
		t.Parallel()
		family, database := "migrations-git-https", "harness_git_migrations_https"
		l.createDatabase(t, enginePostgres, database)
		source := operatorv1alpha1.SourceSpec{Git: l.gitHTTPS(gitfixture.MigrationPath, operatorv1alpha1.GitRef{Branch: "main"})}
		run := l.newMigration(t, family, enginePostgres, l.databaseURL(enginePostgres, database), source)
		run.resolve("resolve")
		before := run.history("history-pending")
		run.applyPlanned("apply", pending(t, family, before))
	})
}

// A pull a row writes out rather than the runner building it, so
// TestGitPullBuildsTheArgumentsTheCaptureRan compares two things. out is the
// path the fetch and the Resolve choose under their scratch directory.
type pullRow struct {
	name, family string
	ssh          bool
	selected     string
	flags        []string
	knownHosts   string
	refused      bool
	rewritten    bool
}

const (
	pullScratch    = "/fetch-work/ptah-git-fetch-capture"
	pullCAFile     = "/credentials/git-ca/ca.pem"
	pullKnownHosts = "/credentials/git-known-hosts/known_hosts"
	pullOtherHosts = "/credentials/git-known-hosts-other/known_hosts"
	pullSSHKey     = "/credentials/git/ssh-key"
)

func (row pullRow) args() []string {
	url := "git+https://" + gitHost + "/" + gitfixture.Repository
	if row.ssh {
		url = "git+ssh://git@" + gitHost + "/" + gitfixture.Repository
	}
	out := pullScratch + "/migrations"
	if row.family == "schema" {
		out = pullScratch + "/" + path.Base(row.selected)
	}
	args := []string{row.family, "pull", url, "--path", row.selected}
	args = append(args, row.flags...)
	switch {
	case !row.ssh:
		args = append(args, "--ca-file", pullCAFile)
	case row.knownHosts != "":
		args = append(args, "--known-hosts-file", row.knownHosts)
	default:
		args = append(args, "--known-hosts-file", pullKnownHosts)
	}
	return append(args, "--out", out, "--json")
}

func pullRows(t *testing.T) []pullRow {
	t.Helper()
	built, err := gitrepo.Build(gitfixture.RepositorySpec(2))
	if err != nil {
		t.Fatal(err)
	}
	first, tip := built.Commit(0).String(), built.Commit(2).String()
	schemaAtFirst, err := gitfixture.SchemaContent(0)
	if err != nil {
		t.Fatal(err)
	}
	schemaAtTip, err := gitfixture.SchemaContent(1)
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := gitfixture.MigrationContent()
	if err != nil {
		t.Fatal(err)
	}
	var rows []pullRow
	for _, family := range []struct{ name, selected, digest string }{
		{"schema", gitfixture.SchemaPath, schemaAtTip},
		{"migrations", gitfixture.MigrationPath, migrations},
	} {
		for _, transport := range []string{"https", "ssh"} {
			for _, ref := range []struct {
				name  string
				flags []string
			}{
				{"branch", []string{"--branch", "main"}},
				{"tag", []string{"--tag", "v1"}},
				{"commit", []string{"--commit", tip, "--expect-digest", family.digest}},
			} {
				rows = append(rows, pullRow{
					name: family.name + "-" + transport + "-" + ref.name, family: family.name,
					ssh: transport == "ssh", selected: family.selected, flags: ref.flags,
				})
			}
		}
	}
	return append(rows,
		// The fetch before an operation names the ref the Resolve read as well
		// as the commit: the phase's schema over HTTPS at main, and its
		// migration directory over SSH at v1.
		pullRow{
			name: "schema-https-branch-commit", family: "schema", selected: gitfixture.SchemaPath,
			flags: []string{"--branch", "main", "--commit", tip, "--expect-digest", schemaAtTip},
		},
		pullRow{
			name: "migrations-ssh-tag-commit", family: "migrations", ssh: true, selected: gitfixture.MigrationPath,
			flags: []string{"--tag", "v1", "--commit", first, "--expect-digest", migrations},
		},
		// What refusedSources sends, in the phase's words: a submodule and an
		// LFS pointer over HTTPS at main, and known hosts that pin another key.
		pullRow{
			name: "refused-submodule", family: "schema", selected: gitfixture.SubmodulePath,
			flags: []string{"--branch", "main"}, refused: true,
		},
		pullRow{
			name: "refused-lfs-pointer", family: "schema", selected: gitfixture.LFSPath,
			flags: []string{"--branch", "main"}, refused: true,
		},
		pullRow{
			name: "refused-host-key", family: "schema", ssh: true, selected: gitfixture.SchemaPath,
			flags: []string{"--branch", "main"}, knownHosts: pullOtherHosts, refused: true,
		},
		// The fetch form, holding the commit at main to the digest of another
		// revision of the same file.
		pullRow{
			name: "refused-digest", family: "schema", selected: gitfixture.SchemaPath,
			flags: []string{"--branch", "main", "--commit", tip, "--expect-digest", schemaAtFirst}, refused: true,
		},
		// After the rewrite the server no longer holds the old tip: the pinned
		// Resolve the phase's row reads, and the fetch of a source that
		// followed the branch.
		pullRow{
			name: "refused-commit-pinned", family: "schema", selected: gitfixture.SchemaPath,
			flags: []string{"--commit", tip}, refused: true, rewritten: true,
		},
		pullRow{
			name: "refused-commit-force-pushed", family: "schema", selected: gitfixture.SchemaPath,
			flags: []string{"--branch", "main", "--commit", tip, "--expect-digest", schemaAtTip}, refused: true, rewritten: true,
		},
	)
}

func runPull(t *testing.T, l *lab, row pullRow) {
	t.Helper()
	args := row.args()
	env := []string{"PTAH_GIT_PASSWORD=" + l.keys.Token}
	if row.ssh {
		env = []string{"PTAH_GIT_SSH_KEY_FILE=" + pullSSHKey}
	}
	stdout, stderr, exit := l.pull(t, args, env)
	name := "git-pull/" + row.name
	if row.refused != (exit != 0) {
		t.Errorf("%s: exit %d, refused=%v\nstdout: %s\nstderr: %s", name, exit, row.refused, stdout, stderr)
	}
	if err := runnerharness.DecodeDocument(args, exit, stdout); err != nil {
		t.Errorf("%s: %v\n%s", name, err, stdout)
	}
	child := runnerharness.Child{Command: runnerharness.CommandWords(args), Args: args, Exit: exit, Stderr: string(stderr)}
	if len(stdout) != 0 {
		child.Stdout = name + "/stdout"
		l.addDocument(t, name, child.Stdout, stdout)
	}
	l.addRow(runnerharness.Row{Name: name, Children: []runnerharness.Child{child}})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.counts.Rows++
	l.counts.Commands[child.Command]++
}

// directRow is a command a row runs on its own, for what it prints about a
// source Ptah cannot load, and the file its document is stored in. The sources
// are mounted at /work, whose paths the decoders' tests read back in the reasons.
type directRow struct {
	name    string
	args    []string
	env     []runnerharness.EnvVar
	target  bool
	refused bool
}

func desiredSourceRows() []directRow {
	literal := func(name, value string) []runnerharness.EnvVar {
		return []runnerharness.EnvVar{{Name: name, Value: value}}
	}
	validate := func(name, dialect, file string, refused bool) directRow {
		return directRow{
			name: name + ".txt", args: []string{"schema", "validate", "--dialect=" + dialect},
			env: literal(runner.EnvSchemaFile, "/work/"+file), refused: refused,
		}
	}
	scratch := func(name, directory string, refused bool) directRow {
		return directRow{
			name: name + ".json", args: []string{"migrations", "status", "--db-url=sqlite:///:memory:", "--json"},
			env: literal(runner.EnvMigrationsDir, "/work/"+directory), refused: refused,
		}
	}
	drift := func(name, file string) directRow {
		return directRow{
			name: name + ".json", args: []string{"schema", "drift", "--format", "json"},
			env: literal(runner.EnvSchemaFile, "/work/"+file), target: true, refused: true,
		}
	}
	return []directRow{
		validate("validate-sql", "PostgreSQL", "schema.sql", true),
		validate("validate-sql-mysql", "MySQL", "schema.sql", true),
		validate("validate-sql-folded", "PostgreSQL", "folded.sql", true),
		validate("validate-yaml", "PostgreSQL", "bad.yaml", true),
		validate("validate-yaml-value", "PostgreSQL", "yaml-value.yaml", true),
		validate("validate-hcl", "PostgreSQL", "bad.hcl", true),
		validate("validate-dbml", "PostgreSQL", "bad.dbml", true),
		validate("validate-missing", "PostgreSQL", "absent.sql", true),
		validate("validate-index", "PostgreSQL", "structural.sql", true),
		validate("validate-good", "PostgreSQL", "good.sql", false),
		scratch("scratch-directive", "migrations-directive", true),
		scratch("scratch-name", "migrations-name", true),
		scratch("scratch-down", "migrations-down", true),
		scratch("scratch-good", "migrations-good", false),
		scratch("scratch-control-bytes", controlBytesDirectory, true),
		drift("drift-sql", "schema.sql"),
		{
			name: "plan-sql.json", args: []string{"schema", "plan", "--output", "/fetch-work/plan.json", "--json"},
			env: literal(runner.EnvSchemaFile, "/work/schema.sql"), target: true, refused: true,
		},
		{
			name: "status-directive.json", args: []string{"migrations", "status", "--json"},
			env: literal(runner.EnvMigrationsDir, "/work/migrations-directive"), target: true, refused: true,
		},
	}
}

// runDirect runs one directRow and records it.
func runDirect(t *testing.T, l *lab, row directRow, url string) {
	t.Helper()
	var secrets []string
	if row.target {
		secrets = []string{runner.EnvDatabaseURL + "=" + url}
	}
	stdout, stderr, exit := l.ptah(t, row.args, secrets, row.env, "desired-source", "/work")
	name := "desired-source/" + strings.TrimSuffix(strings.TrimSuffix(row.name, ".txt"), ".json")
	if row.refused != (exit != 0) {
		t.Errorf("%s: exit %d, refused=%v\nstdout: %s\nstderr: %s", name, exit, row.refused, stdout, stderr)
	}
	if err := runnerharness.DecodeDocument(row.args, exit, stdout); err != nil {
		t.Errorf("%s: %v\n%s", name, err, stdout)
	}
	file := "desired-source/" + row.name
	l.addDocument(t, name, file, stdout)
	command := runnerharness.CommandWords(row.args)
	l.addRow(runnerharness.Row{Name: name, Children: []runnerharness.Child{{
		Command: command, Args: row.args, Exit: exit, Stderr: l.masker.Text(string(stderr)), Stdout: file,
	}}})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.counts.Rows++
	l.counts.Commands[command]++
}

// statementErrors are Applies the database refuses, in both families and on both
// engines, with what each row of the runner writes to its Pod log. The line has to
// say what the database answered and hold nothing the statement holds.
func statementErrors(t *testing.T, l *lab) {
	t.Helper()
	const (
		pgOrders = "CREATE TABLE orders (id bigint PRIMARY KEY, customer_id bigint NOT NULL, note text)"
		myOrders = "CREATE TABLE orders (id bigint PRIMARY KEY, customer_email varchar(200) NOT NULL)"
		items    = "CREATE TABLE items (id bigint PRIMARY KEY, qty text)"
		erased   = "CONCAT('esc-',CHAR(27),'[2K-',CHAR(13),'FORGED-TEXT')"
	)
	duplicatePG := []string{pgOrders, "INSERT INTO orders VALUES (1, 42, 'row-note-one'), (2, 42, 'row-note-two')"}
	duplicateMy := []string{myOrders, "INSERT INTO orders VALUES (1, 'row-value@example.com'), (2, 'row-value@example.com')"}
	apply := func(t *testing.T, name, engine, file string, seed []string) rowOutcome {
		t.Helper()
		family := "statement-errors-" + name
		database := "harness_" + strings.ReplaceAll(strings.TrimSuffix(name, "-apply"), "-", "_")
		l.createDatabase(t, engine, database)
		l.sql(t, engine, database, seed...)
		reference := l.publish(t, "schema", "schemas/"+family, "v1", "--schema-file", "/fixtures/statement-errors/"+file, "--dialect", engineFile(engine))
		run := l.newSchema(t, family, engine, l.databaseURL(engine, database), ociSource(reference))
		run.resolve("resolve")
		run.schema.Spec.Desired.OCIRef = run.schema.Status.Source.ResolvedReference
		run.schema.Status.Source.RequestedReference = run.schema.Status.Source.ResolvedReference
		run.verify("verify", fixture(t, "verification-policy-digest-pin.yaml"))
		run.observe("observe")
		planned := run.plan("plan", nil)
		run.publishPlan("plan", planned.plan)
		// PostgreSQL rolls the refused statement's transaction back, and since
		// Ptah v0.26.0 the report says so (stokaro/ptah#5737). MySQL commits
		// each DDL statement on its own, so Ptah still reports such a run as
		// unknown there.
		want := "apply_outcome_unknown"
		if engine == enginePostgres {
			want = "apply_failed"
		}
		return run.apply("apply", storedAs("schema apply", "statement-errors/"+name+".json"), wantCode(want))
	}
	up := func(t *testing.T, name, engine string, seed []string) rowOutcome {
		t.Helper()
		family := "statement-errors-" + name
		database := "harness_" + strings.ReplaceAll(strings.TrimSuffix(name, "-up"), "-", "_") + "_up"
		l.createDatabase(t, engine, database)
		l.sql(t, engine, database, seed...)
		reference := l.publish(t, "migrations", "migrations/"+family, "v1", "--migrations-dir", "/fixtures/statement-errors/"+name)
		run := l.newMigration(t, family, engine, l.databaseURL(engine, database), ociSource(reference))
		run.resolve("resolve")
		before := run.history("history")
		return run.applyPlanned("apply", pending(t, family, before), storedAs("migrations up", "statement-errors/"+name+".json"), wantCode("child_exit"))
	}
	const hidden = "[in the statement]"
	for _, row := range []struct {
		name, want, refuse string
		run                func(t *testing.T, name string) rowOutcome
	}{
		{
			name: "pg-unique-apply", want: `could not create unique index "` + hidden + `" (SQLSTATE 23505)`, refuse: "orders_one_per_customer",
			run: func(t *testing.T, name string) rowOutcome {
				return apply(t, name, enginePostgres, name+".sql", duplicatePG)
			},
		},
		{
			name: "my-unique-apply", want: `Duplicate entry 'row-value@example.com' for key 'orders.orders_one_per_customer'`, refuse: "CREATE UNIQUE",
			run: func(t *testing.T, name string) rowOutcome {
				return apply(t, name, engineMySQL, name+".sql", duplicateMy)
			},
		},
		{
			name: "my-cast-apply", want: `Incorrect integer value: 'row-value-not-a-number' for column '` + hidden + `' at row 1`, refuse: "MODIFY",
			run: func(t *testing.T, name string) rowOutcome {
				return apply(t, name, engineMySQL, name+".sql", []string{items, "INSERT INTO items VALUES (1, 'row-value-not-a-number')"})
			},
		},
		{
			name: "pg-unique-up", want: `could not create unique index "` + hidden + `" (SQLSTATE 23505)`, refuse: "orders_one_per_customer",
			run: func(t *testing.T, name string) rowOutcome { return up(t, name, enginePostgres, duplicatePG) },
		},
		{
			name: "pg-rowcast-up", want: `invalid input syntax for type integer: "row-value-not-a-number" (SQLSTATE 22P02)`, refuse: "USING",
			run: func(t *testing.T, name string) rowOutcome {
				return up(t, name, enginePostgres, []string{items, "INSERT INTO items VALUES (1, 'row-value-not-a-number')"})
			},
		},
		{
			name: "pg-insert-up", want: `duplicate key value violates unique constraint "orders_pkey" (SQLSTATE 23505)`, refuse: "literal-in-the-migration-file",
			run: func(t *testing.T, name string) rowOutcome {
				return up(t, name, enginePostgres, []string{pgOrders, "INSERT INTO orders VALUES (1, 42, 'row-note-one')"})
			},
		},
		{
			name: "pg-literal-up", want: `invalid input syntax for type integer: "` + hidden + `" (SQLSTATE 22P02)`, refuse: "declared-literal-in-plan",
			run: func(t *testing.T, name string) rowOutcome {
				return up(t, name, enginePostgres, []string{"CREATE TABLE items (id bigint PRIMARY KEY, qty integer)"})
			},
		},
		{
			name: "my-unique-up", want: `Duplicate entry 'row-value@example.com' for key 'orders.orders_one_per_customer'`, refuse: "CREATE UNIQUE",
			run: func(t *testing.T, name string) rowOutcome { return up(t, name, engineMySQL, duplicateMy) },
		},
		{
			name: "my-syntax-up", want: `to use near '` + hidden + `' at line 1`, refuse: "plan-secret-literal",
			run: func(t *testing.T, name string) rowOutcome { return up(t, name, engineMySQL, []string{"SELECT 1"}) },
		},
		{
			name: "my-literal-up", want: `Duplicate entry '` + hidden + `' for key 'orders.customer_email'`, refuse: "declared-literal-in-plan",
			run: func(t *testing.T, name string) rowOutcome {
				return up(t, name, engineMySQL, []string{
					"CREATE TABLE orders (id bigint PRIMARY KEY, customer_email varchar(200) NOT NULL UNIQUE)",
					"INSERT INTO orders VALUES (1, 'declared-literal-in-plan@example.com')",
				})
			},
		},
		{
			name: "my-control-up", want: `Duplicate entry 'esc- [2K- FORGED-TEXT' for key 'orders.orders_one_per_customer'`, refuse: "\x1b",
			run: func(t *testing.T, name string) rowOutcome {
				return up(t, name, engineMySQL, []string{myOrders, "INSERT INTO orders VALUES (1, " + erased + "), (2, " + erased + ")"})
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			logs := string(row.run(t, row.name).logs)
			if !strings.Contains(logs, "ptah-runner: ptah reported: ") || !strings.Contains(logs, row.want) {
				t.Errorf("the Pod log does not say %q:\n%s", row.want, logs)
			}
			if strings.Contains(logs, row.refuse) {
				t.Errorf("the Pod log holds %q:\n%s", row.refuse, logs)
			}
		})
	}
}

// unreadableSources runs the operations over a source Ptah cannot load, so the
// runner's second child, the one that asks why with no database, is a row. The
// Job is the real one for a Git source, and the fetch has nothing to add, so the
// files are already on the volume it would have left them on.
func unreadableSources(t *testing.T, l *lab) {
	t.Helper()
	database := "harness_unreadable"
	l.createDatabase(t, enginePostgres, database)
	url := l.databaseURL(enginePostgres, database)

	t.Run("direct", func(t *testing.T) {
		for _, row := range desiredSourceRows() {
			t.Run(row.name, func(t *testing.T) {
				t.Parallel()
				runDirect(t, l, row, url)
			})
		}
	})
	schemaFile := harnessFixture(t, "desired-source/schema.sql")
	schemaVolume := l.name("unreadable-schema")
	l.fillSource(t, schemaVolume, map[string][]byte{"schema/schema.sql": schemaFile})
	schema := l.newSchema(t, "desired-source-schema", enginePostgres, url, operatorv1alpha1.SourceSpec{Git: l.gitHTTPS(gitfixture.SchemaPath, operatorv1alpha1.GitRef{Branch: "main"})})
	commit, content := fetchShapeRevision(t)
	git := schema.schema.Spec.Desired.Git
	schema.schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{Git: &operatorv1alpha1.GitSourceRevision{
		URL: git.URL, Ref: git.Ref, Path: git.Path, Commit: commit, ContentDigest: content,
	}}
	unreadable := func(volume string) func(*rowSpec) {
		return func(row *rowSpec) {
			row.sharedSource = volume
			row.wantCode = runner.CodeDesiredSourceUnreadable
		}
	}
	schema.observe("observe", unreadable(schemaVolume))
	schema.plan("plan", nil, unreadable(schemaVolume))

	migrationVolume := l.name("unreadable-migrations")
	l.fillSource(t, migrationVolume, map[string][]byte{
		"migrations/0000000001_init.up.sql":   harnessFixture(t, "desired-source/migrations-directive/0000000001_init.up.sql"),
		"migrations/0000000001_init.down.sql": harnessFixture(t, "desired-source/migrations-directive/0000000001_init.down.sql"),
	})
	migrationGit := l.gitHTTPS(gitfixture.MigrationPath, operatorv1alpha1.GitRef{Branch: "main"})
	migration := l.newMigration(t, "desired-source-migration", enginePostgres, url, operatorv1alpha1.SourceSpec{Git: migrationGit})
	migrationContent, err := gitfixture.MigrationContent()
	if err != nil {
		t.Fatal(err)
	}
	migration.migration.Status.Artifact = &operatorv1alpha1.SourceAccessBinding{Git: sourcebinding.GitAccess(*migrationGit, operatorv1alpha1.GitSourceRevision{
		URL: migrationGit.URL, Ref: migrationGit.Ref, Path: migrationGit.Path, Commit: commit, ContentDigest: migrationContent,
	})}
	migration.history("history", unreadable(migrationVolume))
}

// structuralFaults runs Observe and Plan on schemas that load and that Ptah
// refuses. The fault crosses only when the failed command stopped on it, in the
// words of a child that held no credential. Since Ptah v0.26.0 drift and plan
// check every index first and stop on the first one they refuse, in validate's
// words, so a schema with a faulty index says that fault whatever else is
// wrong with it. The last row is the near miss: validate finds a fault there,
// and drift and plan stop on something else, so the runner has to say what it
// says without the fault: drift's exit status, and the code of the refusal
// Ptah planned. Its fixed sentences hold neither the row the database was
// seeded with nor anything drift said of the schema the database holds, which
// no check of a code would show.
func structuralFaults(t *testing.T, l *lab) {
	t.Helper()
	const (
		invalid    = runner.CodeDesiredSchemaInvalid
		noReason   = "ptah exited with code 2"
		foreignKey = `invalid foreign key: constraint "t_nope" references unknown table "nope"`
		indexTable = `index "i": names table "nowhere", which no declaration defines`
		indexKey   = `index "i": names column "missing_column", which table "t" does not declare`
		// Since Ptah v0.23.0 the plan refuses what it cannot compare as
		// declaration-refused, and the runner asks the desired-source check
		// why. Where validate finds the fault in the file, the Plan says that;
		// where drift stopped on something else, it says the refusal's code.
		declarationRefused = "ptah refused to plan the change (declaration-refused)"
	)
	seed := func(engine string) []string {
		note := "text"
		if engine == engineMySQL {
			note = "varchar(64)"
		}
		return []string{
			"CREATE TABLE seeded (id bigint PRIMARY KEY, note " + note + ")",
			"INSERT INTO seeded VALUES (1, 'row-value-of-the-target')",
		}
	}
	otherSchema := []string{"CREATE SCHEMA other", "CREATE TABLE other.nope (id bigint PRIMARY KEY)"}
	commit, content := fetchShapeRevision(t)
	for _, row := range []struct {
		name, engine, file string
		// extra is what the database holds besides the seeded row.
		extra                   []string
		observeCode, planCode   string
		observeWords, planWords string
	}{
		{
			name: "foreign-key", engine: enginePostgres, file: "structural2.sql",
			observeCode: invalid, planCode: invalid,
			observeWords: `schema.sql: invalid foreign key: field "id" references unknown table "nope"`,
			planWords:    `schema.sql: invalid foreign key: field "id" references unknown table "nope"`,
		},
		{
			name: "key-and-index", engine: enginePostgres, file: "key-and-index.sql",
			observeCode: invalid, planCode: invalid,
			observeWords: "schema.sql: " + indexKey, planWords: "schema.sql: " + indexKey,
		},
		{
			name: "key-mysql", engine: engineMySQL, file: "key-constraint.sql",
			observeCode: invalid, planCode: invalid,
			observeWords: "schema.sql: " + foreignKey, planWords: "schema.sql: " + foreignKey,
		},
		{
			name: "not-unique", engine: enginePostgres, file: "not-unique.sql",
			observeCode: invalid, planCode: invalid,
			observeWords: `schema.sql: postgres requires referenced columns code on table "parent" to be declared unique`,
			planWords:    `schema.sql: postgres requires referenced columns code on table "parent" to be declared unique`,
		},
		{
			name: "index-table", engine: enginePostgres, file: "index-table.sql",
			observeCode: invalid, planCode: invalid,
			observeWords: "schema.sql: " + indexTable, planWords: "schema.sql: " + indexTable,
		},
		{
			name: "other-and-index", engine: enginePostgres, file: "other-and-index.sql", extra: otherSchema,
			observeCode: invalid, planCode: invalid,
			observeWords: "schema.sql: " + indexKey, planWords: "schema.sql: " + indexKey,
		},
		{
			name: "other-and-key", engine: enginePostgres, file: "other-and-key.sql", extra: otherSchema,
			observeCode: "child_exit", planCode: "plan_refused", observeWords: noReason, planWords: declarationRefused,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			family := "structural-" + row.name
			database := "harness_structural_" + strings.ReplaceAll(row.name, "-", "_")
			l.createDatabase(t, row.engine, database)
			l.sql(t, row.engine, database, append(seed(row.engine), row.extra...)...)
			volume := l.name(family)
			l.fillSource(t, volume, map[string][]byte{"schema/schema.sql": harnessFixture(t, "desired-source/"+row.file)})
			schema := l.newSchema(t, family, row.engine, l.databaseURL(row.engine, database),
				operatorv1alpha1.SourceSpec{Git: l.gitHTTPS(gitfixture.SchemaPath, operatorv1alpha1.GitRef{Branch: "main"})})
			git := schema.schema.Spec.Desired.Git
			schema.schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{Git: &operatorv1alpha1.GitSourceRevision{
				URL: git.URL, Ref: git.Ref, Path: git.Path, Commit: commit, ContentDigest: content,
			}}
			shared := func(spec *rowSpec) { spec.sharedSource = volume }
			says := func(stage string, outcome rowOutcome, want string) {
				t.Helper()
				if outcome.result.Error == nil || outcome.result.Error.Message != want {
					t.Errorf("%s said %+v, want %q", stage, outcome.result.Error, want)
				}
			}
			says("Observe", schema.observe("observe", shared, wantCode(row.observeCode),
				storedAs("schema drift", "desired-source/drift-"+row.name+".json"),
				storedAs("schema validate", "desired-source/validate-"+row.name+".txt")), row.observeWords)
			says("Plan", schema.plan("plan", nil, shared, wantCode(row.planCode),
				storedAs("schema plan", "desired-source/plan-"+row.name+".json")), row.planWords)
		})
	}
}

// fillSource makes a source volume holding these files, readable by the Pod.
func (l *lab) fillSource(t *testing.T, volume string, files map[string][]byte) {
	t.Helper()
	entries := []volumes.Entry{{Volume: sourceVolumeName, Path: ".", Directory: true, Mode: 0o770, GID: operationGroup}}
	directories := map[string]bool{}
	for file, data := range files {
		if directory := path.Dir(file); !directories[directory] {
			directories[directory] = true
			entries = append(entries, volumes.Entry{Volume: sourceVolumeName, Path: directory, Directory: true, Mode: 0o770, GID: operationGroup})
		}
		entries = append(entries, volumes.Entry{Volume: sourceVolumeName, Path: file, Data: data, Mode: 0o440, GID: operationGroup})
	}
	stream, err := volumes.Pack(entries)
	if err != nil {
		t.Fatal(err)
	}
	l.docker.must(t, nil, "volume", "create", volume)
	if _, stderr, exit, err := l.docker.run(t.Context(), stream, nil, "run", "--interactive", "--rm", "--pull", "never", "--name", volume+"-fill", "--network", "none", "--user", "0:0",
		"--mount", "type=volume,src="+volume+",dst=/volumes/"+sourceVolumeName,
		"--entrypoint", "/recorder", l.images.tools, "--prepare-volumes", "/volumes"); err != nil || exit != 0 {
		t.Fatalf("fill %s: exit %d, %v: %s", volume, exit, err, stderr)
	}
}

// operationGroup is the group an operation Pod's volumes belong to.
const operationGroup = 65532

// fault is one way a target cannot be opened, and the kind Ptah names for it.
type fault struct {
	name string
	url  func(*lab) string
	kind dataplane.TargetFailureKind
}

func faults() []fault {
	postgresURL := func(credentials, host, database, parameters string) func(*lab) string {
		return func(l *lab) string {
			return "postgres://" + strings.ReplaceAll(credentials, "PASSWORD", l.databasePassword) + "@" + host + "/" + database + parameters
		}
	}
	mysqlURL := func(credentials, host, database, parameters string) func(*lab) string {
		return func(l *lab) string {
			return "mysql://" + strings.ReplaceAll(credentials, "PASSWORD", l.databasePassword) + "@" + host + "/" + database + parameters
		}
	}
	const disabled = "?sslmode=disable"
	return []fault{
		{"pg-unresolvable", postgresURL("postgres:PASSWORD", missingHost+":5432", faultDatabase, disabled), dataplane.TargetDatabaseUnreachable},
		{"pg-refused", postgresURL("postgres:PASSWORD", postgresHost+":5439", faultDatabase, disabled), dataplane.TargetDatabaseUnreachable},
		{"pg-password", postgresURL("postgres:wrong-password", postgresHost+":5432", faultDatabase, disabled), dataplane.TargetAuthenticationFailed},
		{"pg-role", postgresURL("nobody:wrong-password", postgresHost+":5432", faultDatabase, disabled), dataplane.TargetAuthenticationFailed},
		{"pg-database", postgresURL("postgres:PASSWORD", postgresHost+":5432", "no_such_db", disabled), dataplane.TargetDatabaseNotFound},
		{"pg-connect", postgresURL("noconnect:PASSWORD", postgresHost+":5432", faultRefusedDatabase, disabled), dataplane.TargetPermissionDenied},
		{"pg-tls", postgresURL("postgres:PASSWORD", postgresHost+":5432", faultDatabase, "?sslmode=verify-full"), dataplane.TargetTLSFailed},
		{"my-unresolvable", mysqlURL("root:PASSWORD", missingHost+":3306", faultDatabase, ""), dataplane.TargetDatabaseUnreachable},
		{"my-refused", mysqlURL("root:PASSWORD", mysqlHost+":3309", faultDatabase, ""), dataplane.TargetDatabaseUnreachable},
		{"my-password", mysqlURL("root:wrong-password", mysqlHost+":3306", faultDatabase, ""), dataplane.TargetAuthenticationFailed},
		{"my-database", mysqlURL("root:PASSWORD", mysqlHost+":3306", "no_such_db", ""), dataplane.TargetDatabaseNotFound},
		{"my-connect", mysqlURL("nodb:PASSWORD", mysqlHost+":3306", faultRefusedDatabase, ""), dataplane.TargetPermissionDenied},
		{"my-tls", mysqlURL("root:PASSWORD", mysqlHost+":3306", faultDatabase, "?tls=true"), dataplane.TargetTLSFailed},
	}
}

const (
	faultDatabase        = "harness_fault"
	faultRefusedDatabase = "harness_refused"
)

func prepareFaultDatabases(t *testing.T, l *lab) {
	t.Helper()
	for _, engine := range []string{enginePostgres, engineMySQL} {
		l.createDatabase(t, engine, faultDatabase)
		l.createDatabase(t, engine, faultRefusedDatabase)
	}
	l.sql(t, enginePostgres, "postgres",
		"CREATE ROLE noconnect LOGIN PASSWORD '"+l.databasePassword+"'",
		"REVOKE CONNECT ON DATABASE "+faultRefusedDatabase+" FROM PUBLIC")
	l.sql(t, engineMySQL, "mysql",
		"CREATE USER 'nodb'@'%' IDENTIFIED BY '"+l.databasePassword+"'")
}

// faultRows runs one fault through each command that opens a target. Each row's
// Job is the real one for its operation, with the fault's URL in its Secret and
// the fetched artifact shared between rows, since the fetch has no bearing on
// what the target answers.
func faultRows(t *testing.T, l *lab, f fault, inputs *faultInputs, sources faultSources) {
	t.Helper()
	engine := enginePostgres
	if strings.HasPrefix(f.name, "my-") {
		engine = engineMySQL
	}
	url := f.url(l)
	stored := func(command, file string) func(*rowSpec) {
		return func(row *rowSpec) {
			storedAs(command, path.Join("target-failures", f.name, file+".json"))(row)
			row.wantCode = string(f.kind)
		}
	}
	shared := func(volume string) func(*rowSpec) {
		return func(row *rowSpec) { row.sharedSource = volume }
	}
	schema := l.newSchema(t, "faults/"+f.name, engine, url, inputs.schema.schema.Spec.Desired)
	schema.schema.Status.Source = inputs.schema.schema.Status.Source
	schema.policy = inputs.schema.policy
	schema.observe("drift", stored("schema drift", "drift"), shared(sources.schema))
	schema.plan("plan", nil, stored("schema plan", "plan"), shared(sources.schema))
	schema.publishPlan("apply", inputs.plan)
	schema.apply("apply", stored("schema apply", "apply"))
	// The plan validation runs only after two plan reads succeeded, so a target
	// that fails never reaches it through a Plan. This is the same command: an
	// Apply of the same plan, with the flag the validation adds.
	schema.apply("apply-dry-run", stored("schema apply --dry-run", "apply-dry-run"), func(row *rowSpec) {
		row.appendArgs = []string{"--dry-run"}
	})

	migration := l.newMigration(t, "faults/"+f.name, engine, url, inputs.migration.migration.Spec.Artifact)
	migration.migration.Status.Artifact = inputs.migration.migration.Status.Artifact
	migration.history("status", stored("migrations status", "status"), shared(sources.migration))
	migration.applyPlanned("up", inputs.planned, stored("migrations up", "up"), shared(sources.migration))
}

// faultSources names the volumes holding what a fault row would have fetched.
type faultSources struct{ schema, migration string }

// seedSource fills a volume by running a Job's init containers once.
func (l *lab) seedSource(t *testing.T, row rowSpec, volume string) {
	t.Helper()
	pod, err := runnerharness.Translate(row.job, row.cluster)
	if err != nil {
		t.Fatalf("%s: %v", row.name, err)
	}
	l.docker.must(t, nil, "volume", "create", volume)
	stream, err := packRoot(volume, pod)
	if err != nil {
		t.Fatal(err)
	}
	if _, stderr, exit, err := l.docker.run(t.Context(), stream, nil, "run", "--interactive", "--rm", "--pull", "never", "--name", volume+"-fill", "--network", "none", "--user", "0:0",
		"--mount", "type=volume,src="+volume+",dst=/volumes/"+sourceVolumeName,
		"--entrypoint", "/recorder", l.images.tools, "--prepare-volumes", "/volumes"); err != nil || exit != 0 {
		t.Fatalf("%s: fill the volumes: exit %d, %v: %s", row.name, exit, err, stderr)
	}
	l.withVolumes(t, row.name, pod, volume, func(mounts map[string]podVolume) {
		for _, container := range pod.Containers[:len(pod.Containers)-1] {
			if _, stderr, ok := l.runContainer(t, row.name, container, pod.InstallTo, mounts, nil); !ok {
				t.Fatalf("%s: seed the source with %s: %s", row.name, container.Name, stderr)
			}
		}
	})
}
