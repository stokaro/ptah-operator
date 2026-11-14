package runner

import (
	"slices"
	"strings"
	"testing"
)

// TestAnActorReachesPtahThroughTheEnvironmentAlone pins the path an Apply's
// actor and its origin take, for a migration and for a schema. The Job sets
// PTAH_ACTOR and PTAH_ACTOR_ORIGIN, the runner hands both to the child
// unchanged and adds neither to the command, and Ptah's own binding reads them
// as --actor and --actor-origin. The runner removes the variables only it
// reads, so one added to that list by mistake would empty the log's actor with
// no other symptom.
func TestAnActorReachesPtahThroughTheEnvironmentAlone(t *testing.T) {
	t.Parallel()

	const actor, origin = "oidc:Jos%C3%A9 Garc%C3%ADa", "admission"
	child := childEnvironment([]string{"PTAH_ACTOR=" + actor, "PTAH_ACTOR_ORIGIN=" + origin, EnvOperationID + "=sha256:abc"})
	for _, variable := range []string{"PTAH_ACTOR=" + actor, "PTAH_ACTOR_ORIGIN=" + origin} {
		if !slices.Contains(child, variable) {
			t.Fatalf("the child environment lost %s: %q", variable, child)
		}
	}
	if slices.Contains(child, EnvOperationID+"=sha256:abc") {
		t.Fatalf("the child keeps what only the runner reads, so this row proves nothing about the filter: %q", child)
	}

	for operation, inputs := range map[Operation]Inputs{
		OperationMigrationApply: {MigrationsDir: "/source/migrations", ExpectedSequencePath: "/tmp/expected.json"},
		OperationApply:          {PlanPath: "/tmp/plan.json"},
	} {
		spec, err := BuildCommand("/usr/local/bin/ptah", operation, inputs)
		if err != nil {
			t.Fatalf("BuildCommand(%s) error = %v", operation, err)
		}
		if len(spec.Args) == 0 {
			t.Fatalf("the %s command has no arguments, so the row below examines nothing", operation)
		}
		for _, argument := range spec.Args {
			if strings.Contains(argument, actor) || strings.Contains(argument, "--actor") {
				t.Fatalf("the runner put the actor on the %s command line: %q", operation, spec.Args)
			}
		}
	}
}
