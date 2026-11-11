package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// A selector variable in the Job's environment reaches no child of an Observe,
// a Plan, an Apply or either migration operation. Ptah binds the selectors on
// schema drift and schema plan and refuses them on schema apply --plan, so one
// that leaked would narrow a read or fail an Apply over a variable the runner
// never chose. Every child these operations start shares childEnvironment,
// which TestChildEnvironmentRemovesBothSelectors holds for the rest.
func TestNoChildInheritsASelectorVariable(t *testing.T) {
	t.Parallel()

	selectors := []string{ptahExcludeEnvironment + `="legacy,archive",audit.*`, ptahIncludeEnvironment + "=app.*"}
	// The plan binds the exclusion PTAH_EXCLUDE asks for, so a Plan reaches its
	// validation and every one of its children is examined.
	scopedPlan := strings.Replace(validPlanDocument("CREATE TABLE example (id bigint);"),
		`"destructive":false`, `"exclude":["audit.*","legacy,archive"],"destructive":false`, 1)
	plan := validPlanDocument("CREATE TABLE example (id bigint);")
	planDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(planDir, "000.plan"), []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}
	drift := `{"contract_version":1,"command":"schema drift","drift":false,"failed":false,"failure_threshold":"all","highest_severity":"","dialect":"postgres","findings":[],"diff":{}}`
	history := `{"contract_version":2,"command":"migrations status","current_version":2,"total_migrations":2,"has_pending_changes":false,"pending_migrations":[]}`

	for _, row := range []struct {
		name        string
		operation   Operation
		environment []string
		responses   []scriptedResponse
	}{
		{name: "observe", operation: OperationObserve,
			environment: databaseEnvironment("selectors-observe"),
			responses:   []scriptedResponse{{stdout: drift}}},
		{name: "plan reads and their validation", operation: OperationPlan,
			environment: databaseEnvironment("selectors-plan"),
			responses:   stablePlanResponses(t, scopedPlan)},
		{name: "apply", operation: OperationApply,
			environment: append(databaseEnvironment("selectors-apply"),
				envPlanDir+"="+planDir,
				envExpectedPlanDigest+"="+sha256Digest([]byte(plan)),
				envExpectedCoordination+"="+testCoordinationDigest(),
				envExpectedTargetDigest+"="+databaseTargetDigest(t)),
			responses: []scriptedResponse{{stdout: applyReport(t, dataplane.SchemaApplyOutcomeApplied, plan)}}},
		{name: "migration history", operation: OperationMigrationHistory,
			environment: migrationEnvironment("selectors-history"),
			responses:   []scriptedResponse{{stdout: history}}},
		{name: "migration apply", operation: OperationMigrationApply,
			environment: migrationApplyEnvironment(t, "selectors-migration-apply"),
			responses:   []scriptedResponse{{stdout: migrationRunDocument("applied")}}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			executor := &scriptedExecutor{t: t, responses: row.responses}
			result := Run(context.Background(), Config{
				Operation:   row.operation,
				Environment: withRunnerProtocol(append(append([]string(nil), row.environment...), selectors...)),
				Executor:    executor,
				TempDir:     t.TempDir(),
			})
			if result.Error != nil {
				t.Fatalf("Run() error = %#v, want the operation to reach every child it starts", result.Error)
			}
			// Every scripted child ran, so none of them escaped the check.
			if len(executor.calls) != len(row.responses) {
				t.Fatalf("ran %d children, want %d", len(executor.calls), len(row.responses))
			}
			for index, child := range executor.calls {
				values := environmentMap(child.Env)
				if _, present := values[envDatabaseURL]; !present {
					t.Fatalf("child %d (%v) lost the database URL, so its environment reads empty", index, child.Args)
				}
				for _, key := range []string{ptahExcludeEnvironment, ptahIncludeEnvironment} {
					if value, present := values[key]; present {
						t.Fatalf("child %d (%v) inherited %s=%s", index, child.Args, key, value)
					}
				}
			}
		})
	}
}

func TestChildEnvironmentRemovesBothSelectors(t *testing.T) {
	t.Parallel()

	got := environmentMap(childEnvironment([]string{
		"PTAH_DB_URL=postgres://app@db/app", ptahExcludeEnvironment + "=audit.*", ptahIncludeEnvironment + "=app.*",
	}))
	for _, key := range []string{ptahExcludeEnvironment, ptahIncludeEnvironment} {
		if _, present := got[key]; present {
			t.Fatalf("childEnvironment kept %s", key)
		}
	}
	if got["PTAH_DB_URL"] == "" {
		t.Fatal("childEnvironment dropped a variable it does not own")
	}
}
