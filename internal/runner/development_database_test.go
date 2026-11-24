package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testDevelopmentURL = "postgres://devuser:dev-password@dev.example/app"

// developmentPlanEnvironment is a Plan's environment, naming the development
// database file devFile when it is not empty.
func developmentPlanEnvironment(operationID, devFile string) []string {
	environment := []string{
		envOperationID + "=" + operationID,
		EnvDatabaseURL + "=postgres://app:app-password@db.example/app",
		envCoordinationDigest + "=" + testCoordinationDigest(),
		envExpectedDatabaseEngine + "=PostgreSQL",
		envPlanSealPublicKey + "=" + testPlanSealKey.PublicKey().Encode(),
		envSealedPlanJobName + "=ptah-plan-" + operationID,
	}
	if devFile != "" {
		environment = append(environment, EnvDevelopmentDatabaseURLFile+"="+devFile)
	}
	return withRunnerProtocol(environment)
}

// A Plan whose Job names a development database file is refused before its
// child when the file holds no URL: a SecretProviderClass that does not write
// the object mounts without the file, and a class or a Secret may hold the URL
// empty. Ptah plans without a scratch database when it is given none, so the
// plan it would compute is not the one the resource asked for. The refusal
// names no part of the URL, and the manager parses it.
func TestAPlanWhoseDevelopmentDatabaseFileHoldsNoURLIsRefused(t *testing.T) {
	t.Parallel()
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ name, devFile string }{
		{name: "a file the class did not write", devFile: filepath.Join(t.TempDir(), "dev-url")},
		{name: "an empty file", devFile: empty},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			executor := &scriptedExecutor{t: t}
			operationID := "dev-refused-" + strings.ReplaceAll(row.name, " ", "-")
			result := Run(context.Background(), Config{
				Operation:   OperationPlan,
				Environment: developmentPlanEnvironment(operationID, row.devFile),
				Executor:    executor,
			})
			if result.Error == nil || result.Error.Code != CodeMissingDev {
				t.Fatalf("error = %#v, want %s", result.Error, CodeMissingDev)
			}
			if len(executor.calls) != 0 || result.ChildExitCode != -1 {
				t.Fatalf("the child ran: calls=%d %#v", len(executor.calls), result)
			}
			frame, err := MarshalFrame(result)
			if err != nil {
				t.Fatalf("MarshalFrame() error = %v", err)
			}
			parsed, err := ParseResultFor(frame, OperationPlan, operationID)
			if err != nil {
				t.Fatalf("ParseResultFor() error = %v", err)
			}
			if parsed.Error == nil || parsed.Error.Code != CodeMissingDev {
				t.Fatalf("the manager parsed %#v, want %s", parsed.Error, CodeMissingDev)
			}
		})
	}
}

// The refusal above is held to what it must admit: a Plan whose file holds
// the URL, which reaches the child, and a Plan whose resource names no
// development database, whose Job names no file. Neither child is told where
// the file was.
func TestAPlanWithItsDevelopmentDatabaseOrWithoutOneRuns(t *testing.T) {
	t.Parallel()
	for _, row := range []struct{ name, devFile, wantURL string }{
		{name: "a file that holds the URL", devFile: writeCredential(t, testDevelopmentURL), wantURL: testDevelopmentURL},
		{name: "no development database"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			var child CommandSpec
			executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{inspect: func(spec CommandSpec) { child = spec }}}}
			result := Run(context.Background(), Config{
				Operation:   OperationPlan,
				Environment: developmentPlanEnvironment("dev-admitted", row.devFile),
				Executor:    executor,
			})
			if len(executor.calls) != 1 {
				t.Fatalf("Run() started %d commands, want the plan; error %#v", len(executor.calls), result.Error)
			}
			values := environmentMap(child.Env)
			if got, set := values[EnvDevelopmentDatabaseURL]; got != row.wantURL || set != (row.wantURL != "") {
				t.Fatalf("the child's %s = %q (set %t), want %q", EnvDevelopmentDatabaseURL, got, set, row.wantURL)
			}
			if _, leaked := values[EnvDevelopmentDatabaseURLFile]; leaked {
				t.Fatalf("the child received %s", EnvDevelopmentDatabaseURLFile)
			}
		})
	}
}
