package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A recording names a commit so somebody can check it out and produce the same
// transcripts. These are the trees where that claim would be false.
func TestRefuseDirtyTree(t *testing.T) {
	t.Parallel()
	root := newRepository(t)
	output := filepath.Join(root, "demo", "recordings", "runs.json")

	if err := refuseDirtyTree(root, output); err != nil {
		t.Fatalf("a clean tree was refused: %v", err)
	}

	// The recording itself is the exception: writing it is the work.
	write(t, output, `{"scenarios":[]}`)
	if err := refuseDirtyTree(root, output); err != nil {
		t.Fatalf("the recording's own change was refused: %v", err)
	}

	tests := []struct {
		name string
		make func()
		want string
	}{
		{
			name: "an edited scenario",
			make: func() { write(t, filepath.Join(root, "demo", "scenarios", "drift.yaml"), "id: edited\n") },
			want: "demo/scenarios/drift.yaml",
		},
		{
			name: "an edited file whose name starts the line",
			make: func() { write(t, filepath.Join(root, "Makefile"), "edited\n") },
			want: "Makefile",
		},
		{
			name: "a staged addition",
			make: func() {
				write(t, filepath.Join(root, "demo", "scenarios", "new.yaml"), "id: new\n")
				gitRun(t, root, "git", "add", "demo/scenarios/new.yaml")
			},
			want: "demo/scenarios/new.yaml",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Not parallel: each case dirties the one repository in turn, and
			// what is being measured is which paths the refusal names.
			test.make()
			err := refuseDirtyTree(root, output)
			if err == nil {
				t.Fatalf("refuseDirtyTree accepted %s", test.name)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("refuseDirtyTree said %q, which does not name %q", err, test.want)
			}
			gitRun(t, root, "git", "checkout", "--", ".")
			gitRun(t, root, "git", "reset", "-q")
			_ = os.Remove(filepath.Join(root, "demo", "scenarios", "new.yaml"))
		})
	}
}

// An untracked file is not a change to what the commit holds, so it is not a
// reason to refuse: the lab writes its scratch files inside the repository.
func TestRefuseDirtyTreeIgnoresUntrackedFiles(t *testing.T) {
	t.Parallel()
	root := newRepository(t)
	write(t, filepath.Join(root, "demo", ".lab", "environment"), "E2E_KUBECONFIG=/tmp/x\n")
	if err := refuseDirtyTree(root, filepath.Join(root, "demo", "recordings", "runs.json")); err != nil {
		t.Fatalf("an untracked file was refused: %v", err)
	}
}

func newRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gitRun(t, root, "git", "init", "-q", "-b", "master")
	gitRun(t, root, "git", "config", "user.email", "recorder@example.test")
	gitRun(t, root, "git", "config", "user.name", "Recorder")
	write(t, filepath.Join(root, "Makefile"), "demo:\n")
	write(t, filepath.Join(root, "demo", "scenarios", "drift.yaml"), "id: drift\n")
	write(t, filepath.Join(root, "demo", "recordings", "runs.json"), "{}\n")
	gitRun(t, root, "git", "add", ".")
	gitRun(t, root, "git", "commit", "-q", "-m", "first")
	return root
}

func gitRun(t *testing.T, dir string, name string, arguments ...string) {
	t.Helper()
	command := exec.Command(name, arguments...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(arguments, " "), err, output)
	}
}
