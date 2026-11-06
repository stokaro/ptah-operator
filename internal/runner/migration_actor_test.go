package runner

import (
	"slices"
	"strings"
	"testing"
)

// TestAMigrationActorReachesPtahThroughTheEnvironmentAlone pins the path a
// migration's actor takes. The Job sets PTAH_ACTOR, the runner hands it to the
// child unchanged and adds nothing to the command, and Ptah's own binding reads
// it as --actor. The runner removes the variables only it reads, so one added
// to that list by mistake would empty the log's actor with no other symptom.
func TestAMigrationActorReachesPtahThroughTheEnvironmentAlone(t *testing.T) {
	t.Parallel()

	const actor = "oidc:Jos%C3%A9 Garc%C3%ADa"
	child := childEnvironment([]string{"PTAH_ACTOR=" + actor, EnvOperationID + "=sha256:abc"})
	if !slices.Contains(child, "PTAH_ACTOR="+actor) {
		t.Fatalf("the child environment lost the actor: %q", child)
	}
	if slices.Contains(child, EnvOperationID+"=sha256:abc") {
		t.Fatalf("the child keeps what only the runner reads, so this row proves nothing about the filter: %q", child)
	}

	spec, err := BuildCommand("/usr/local/bin/ptah", OperationMigrationApply, Inputs{
		MigrationsDir: "/source/migrations", ExpectedSequencePath: "/tmp/expected.json",
	})
	if err != nil {
		t.Fatalf("BuildCommand() error = %v", err)
	}
	if len(spec.Args) == 0 {
		t.Fatal("the command has no arguments, so the row below examines nothing")
	}
	for _, argument := range spec.Args {
		if strings.Contains(argument, actor) || strings.Contains(argument, "--actor") {
			t.Fatalf("the runner put the actor on the command line: %q", spec.Args)
		}
	}
}
