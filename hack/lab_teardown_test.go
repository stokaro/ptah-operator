package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// LAB_ENVIRONMENT is an input, and the teardown used to remove the directory it
// happened to sit in. Pointed at a file somewhere else, it deleted whatever
// shared that directory.
func TestLabDownRemovesNothingOutsideTheLabDirectory(t *testing.T) {
	t.Parallel()
	root := repositoryRoot(t)
	elsewhere := t.TempDir()
	bystander := filepath.Join(elsewhere, "somebody-elses-file")
	if err := os.WriteFile(bystander, []byte("not the lab's\n"), 0o600); err != nil {
		t.Fatalf("write the bystander: %v", err)
	}
	environment := filepath.Join(elsewhere, "environment")
	// A cluster and a context that do not exist: the teardown has to reach the
	// directory decision without depending on Docker being there at all.
	contents := strings.Join([]string{
		"E2E_KIND_CLUSTER_NAME=ptah-e2e-nothing-here",
		"E2E_DOCKER_CONTEXT=ptah-e2e-context-that-does-not-exist",
		"E2E_WORK_DIR=" + filepath.Join(elsewhere, "work"),
		"",
	}, "\n")
	if err := os.WriteFile(environment, []byte(contents), 0o600); err != nil {
		t.Fatalf("write the environment: %v", err)
	}

	output := removeLabDirectory(t, root, environment)

	if _, err := os.Stat(bystander); err != nil {
		t.Fatalf("lab down removed a file it does not own: %v\n%s", err, output)
	}
	if _, err := os.Stat(environment); !os.IsNotExist(err) {
		t.Fatalf("lab down left the environment file behind, so a lab still looks up: %v", err)
	}
	if !strings.Contains(string(output), "is not the lab directory") {
		t.Fatalf("lab down did not say why it kept the directory:\n%s", output)
	}
}

// The work directory holds the kubeconfig and the credential files, so leaving
// it behind leaves those on disk. It is removed only under the name the harness
// gives it, for the same reason as the directory above.
func TestLabDownRefusesAnUnexpectedWorkDirectory(t *testing.T) {
	t.Parallel()
	root := repositoryRoot(t)
	elsewhere := t.TempDir()
	work := filepath.Join(elsewhere, "not-a-harness-work-directory")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("make the work directory: %v", err)
	}
	environment := filepath.Join(elsewhere, "environment")
	contents := strings.Join([]string{
		"E2E_KIND_CLUSTER_NAME=ptah-e2e-nothing-here",
		"E2E_DOCKER_CONTEXT=ptah-e2e-context-that-does-not-exist",
		"E2E_WORK_DIR=" + work,
		"",
	}, "\n")
	if err := os.WriteFile(environment, []byte(contents), 0o600); err != nil {
		t.Fatalf("write the environment: %v", err)
	}

	output := removeWorkDirectory(t, root, environment, work)
	if _, err := os.Stat(work); err != nil {
		t.Fatalf("lab down removed a work directory that is not the harness's: %v", err)
	}
	if !strings.Contains(string(output), "refusing to remove unexpected work directory") {
		t.Fatalf("lab down did not say why it kept it:\n%s", output)
	}
}

// The bootstrap releases the harness's cleanup trap, so the teardown is the
// only thing that will ever release the task claim. Without it the next
// `make demo-up` is refused for an identity nobody is using.
func TestBootstrapHandsOverWhatTheTeardownRemoves(t *testing.T) {
	t.Parallel()
	contents, err := os.ReadFile(filepath.Join(repositoryRoot(t), e2eHarnessPath))
	if err != nil {
		t.Fatalf("read %s: %v", e2eHarnessPath, err)
	}
	for _, handed := range []string{
		"E2E_TASK_CLAIM_VOLUME=",
		"E2E_WORK_DIR=",
		"E2E_CREATED_IMAGE_REFS=",
	} {
		if !strings.Contains(string(contents), handed) {
			t.Fatalf("the bootstrap does not hand over %s, so a teardown cannot remove it", handed)
		}
	}
}

// The teardown's two directory decisions, driven one at a time. They are what
// these tests are about, and running the whole teardown would put a Docker
// daemon between the test and the decision.
func removeLabDirectory(t *testing.T, root, environment string) []byte {
	t.Helper()
	return labShell(t, root, environment, "lab_down_remove_lab_directory")
}

func removeWorkDirectory(t *testing.T, root, environment, work string) []byte {
	t.Helper()
	return labShell(t, root, environment, "E2E_WORK_DIR='"+work+"'; lab_down_remove_work_directory")
}

func labShell(t *testing.T, root, environment, call string) []byte {
	t.Helper()
	script := "set -eu\n" +
		"LAB_ROOT=" + root + "\n" +
		"LAB_ENVIRONMENT=" + environment + "\n" +
		". " + filepath.Join(root, "demo", "lib", "support.sh") + "\n" +
		". " + filepath.Join(root, "demo", "lib", "lab.sh") + "\n" +
		call + "\n"
	command := exec.Command("/bin/sh", "-c", script)
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", call, err, output)
	}
	return output
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve the repository root: %v", err)
	}
	return root
}

// A teardown reports success only over what it saw removed.
//
// Docker's own failures used to be discarded: a list that could not run
// returned nothing, and nothing to remove reads exactly like a lab with
// nothing left in it. The teardown then deleted the environment file and the
// work directory, and the task-claim volume it had not removed went on
// refusing the next `make demo-up` with no metadata left to retry from.
func TestLabDownKeepsWhatARetryNeedsWhenDockerCannotFinish(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		volumes     string
		failOn      string
		wantMessage string
	}{
		{
			name:        "the listing could not run",
			failOn:      "volume ls",
			wantMessage: "could not ask which volumes this lab created",
		},
		{
			name:        "the removal could not run",
			volumes:     "ptah-e2e-task-claim",
			failOn:      "volume rm",
			wantMessage: "could not remove volume ptah-e2e-task-claim",
		},
		{
			// The removal reported success and the object is still there. No
			// command failed, so only asking again can tell.
			name:        "the removal removed nothing",
			volumes:     "ptah-e2e-task-claim",
			wantMessage: "volume left behind: ptah-e2e-task-claim",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			lab := newTeardownLab(t)
			output, err := lab.down(t, map[string]string{
				"STUB_VOLUMES": test.volumes,
				"STUB_FAIL":    test.failOn,
			})
			if err == nil {
				t.Fatalf("lab down reported success over an unfinished teardown:\n%s", output)
			}
			if !strings.Contains(string(output), test.wantMessage) {
				t.Fatalf("lab down did not say %q:\n%s", test.wantMessage, output)
			}
			if _, statErr := os.Stat(lab.environment); statErr != nil {
				t.Fatalf("lab down removed the environment file a retry reads: %v", statErr)
			}
			// The task-scoped Docker context lives in here, and it is what the
			// retry resolves to reach the daemon that still holds the rest.
			if _, statErr := os.Stat(lab.work); statErr != nil {
				t.Fatalf("lab down removed the work directory a retry needs: %v", statErr)
			}
		})
	}
}

// The control: with everything removed, the teardown does clean up after
// itself. Without it the test above passes over a teardown that never removes
// anything at all.
func TestLabDownRemovesItsOwnStateWhenTheTeardownFinished(t *testing.T) {
	t.Parallel()
	lab := newTeardownLab(t)
	output, err := lab.down(t, map[string]string{})
	if err != nil {
		t.Fatalf("lab down failed over a finished teardown: %v\n%s", err, output)
	}
	if _, statErr := os.Stat(lab.environment); !os.IsNotExist(statErr) {
		t.Fatalf("lab down left the environment file behind, so a lab still looks up: %v", statErr)
	}
	if _, statErr := os.Stat(lab.work); !os.IsNotExist(statErr) {
		t.Fatalf("lab down left the work directory behind: %v", statErr)
	}
}

// teardownLab is a lab on paper: an environment file, a work directory named
// the way the harness names one, and stubbed kind and docker commands.
type teardownLab struct {
	root        string
	environment string
	work        string
	tmp         string
	path        string
}

func newTeardownLab(t *testing.T) teardownLab {
	t.Helper()
	root := repositoryRoot(t)
	tmp := t.TempDir()
	work := filepath.Join(tmp, "ptah-operator-e2e.test")
	if err := os.MkdirAll(filepath.Join(work, "docker-cli"), 0o755); err != nil {
		t.Fatalf("make the work directory: %v", err)
	}
	environment := filepath.Join(t.TempDir(), "environment")
	contents := strings.Join([]string{
		"E2E_KIND_CLUSTER_NAME=ptah-e2e-stub",
		"E2E_DOCKER_CONTEXT=ptah-e2e-stub-context",
		"E2E_WORK_DIR=" + work,
		"",
	}, "\n")
	if err := os.WriteFile(environment, []byte(contents), 0o600); err != nil {
		t.Fatalf("write the environment: %v", err)
	}
	return teardownLab{root: root, environment: environment, work: work, tmp: tmp, path: stubCommands(t)}
}

// down runs the teardown the way `make demo-down` does, through the entry
// point rather than through the library, so what is driven is the command a
// reader runs.
func (l teardownLab) down(t *testing.T, stub map[string]string) ([]byte, error) {
	t.Helper()
	command := exec.Command(filepath.Join(l.root, "demo", "bin", "lab"), "down")
	command.Dir = l.root
	command.Env = append(os.Environ(),
		"PATH="+l.path+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+l.tmp,
		"LAB_ROOT="+l.root,
		"LAB_ENVIRONMENT="+l.environment,
	)
	for name, value := range stub {
		command.Env = append(command.Env, name+"="+value)
	}
	output, err := command.CombinedOutput()
	return output, err
}

// stubCommands writes a kind and a docker that answer from the environment, so
// the teardown's decisions are driven rather than a daemon's.
func stubCommands(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	stubs := map[string]string{
		// The cluster is gone: the delete succeeds and the listing is empty.
		"kind": "#!/bin/sh\nexit 0\n",
		"docker": "#!/bin/sh\n" +
			"if [ \"$1\" = --context ]; then shift 2; fi\n" +
			"what=\"$1 $2\"\n" +
			"if [ \"$what\" = \"${STUB_FAIL:-}\" ]; then exit 1; fi\n" +
			"case \"$what\" in\n" +
			"\"container ls\") printf '%s' \"${STUB_CONTAINERS:-}\" ;;\n" +
			"\"volume ls\") printf '%s' \"${STUB_VOLUMES:-}\" ;;\n" +
			"\"image inspect\") exit 1 ;;\n" +
			"esac\n" +
			"exit 0\n",
	}
	for name, body := range stubs {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatalf("write the %s stub: %v", name, err)
		}
	}
	return directory
}

// A reset that could not empty the database is not a reset.
//
// Every scenario starts from an empty database. Carrying on from a drop that
// did not run records the next scenario against the last one's tables, and the
// failure then shows up as a plan nobody can explain.
func TestLabResetFailsWhenTheDatabaseCannotBeEmptied(t *testing.T) {
	t.Parallel()
	lab := newResetLab(t)

	output, err := lab.reset(t, map[string]string{"STUB_FAIL": "exec"})
	if err == nil {
		t.Fatalf("lab reset reported success over a database it did not empty:\n%s", output)
	}
	if !strings.Contains(string(output), "could not empty the demonstration database") {
		t.Fatalf("lab reset did not say what failed:\n%s", output)
	}
}

// The control: the same reset over a database it could empty.
func TestLabResetSucceedsWhenTheDatabaseWasEmptied(t *testing.T) {
	t.Parallel()
	lab := newResetLab(t)

	output, err := lab.reset(t, map[string]string{})
	if err != nil {
		t.Fatalf("lab reset failed over a database it emptied: %v\n%s", err, output)
	}
}

type resetLab struct {
	root        string
	environment string
	path        string
}

func newResetLab(t *testing.T) resetLab {
	t.Helper()
	root := repositoryRoot(t)
	environment := filepath.Join(t.TempDir(), "environment")
	contents := strings.Join([]string{
		"E2E_TEST_NAMESPACE=ptah-test-stub",
		"E2E_KUBECONFIG=" + filepath.Join(t.TempDir(), "kubeconfig"),
		"",
	}, "\n")
	if err := os.WriteFile(environment, []byte(contents), 0o600); err != nil {
		t.Fatalf("write the environment: %v", err)
	}
	directory := t.TempDir()
	stub := "#!/bin/sh\n" +
		"for argument in \"$@\"; do\n" +
		"  case \"$argument\" in\n" +
		"  \"${STUB_FAIL:-nothing-fails}\") exit 1 ;;\n" +
		"  esac\n" +
		"done\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(directory, "kubectl"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write the kubectl stub: %v", err)
	}
	return resetLab{root: root, environment: environment, path: directory}
}

func (l resetLab) reset(t *testing.T, stub map[string]string) ([]byte, error) {
	t.Helper()
	command := exec.Command(filepath.Join(l.root, "demo", "bin", "lab"), "reset")
	command.Dir = l.root
	command.Env = append(os.Environ(),
		"PATH="+l.path+string(os.PathListSeparator)+os.Getenv("PATH"),
		"LAB_ROOT="+l.root,
		"LAB_ENVIRONMENT="+l.environment,
	)
	for name, value := range stub {
		command.Env = append(command.Env, name+"="+value)
	}
	return command.CombinedOutput()
}
