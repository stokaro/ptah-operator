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
