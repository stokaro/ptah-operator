package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/hack/chartstamp"
)

// recordingHelm stands in for helm: it keeps every argument list it was handed
// and answers with err.
type recordingHelm struct {
	calls [][]string
	err   error
}

func (h *recordingHelm) run(_ context.Context, arguments []string, _, _ io.Writer) error {
	h.calls = append(h.calls, slices.Clone(arguments))
	return h.err
}

func releasedInstaller(helm *recordingHelm) installer {
	return installer{repository: chartRepository, pluginVersion: "v0.2.11", helm: helm.run}
}

// The plugin installs from where the release publishes, and nothing in the
// plugin's own build reaches hack/, so the two constants are held together
// here.
func TestInstallReadsTheRepositoryTheReleasePublishesTo(t *testing.T) {
	t.Parallel()
	if chartRepository != chartstamp.Repository {
		t.Fatalf("install reads %s, and a release publishes to %s", chartRepository, chartstamp.Repository)
	}
}

// What install refuses before it reaches helm. Each row is a mistake a reader
// can make, and none of them may start helm.
func TestInstallRefusesBeforeHelm(t *testing.T) {
	t.Parallel()
	existing := t.TempDir()
	if err := os.Mkdir(filepath.Join(existing, releaseChartName), 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name          string
		pluginVersion string
		arguments     []string
		says          string
	}{
		{name: "a build from a checkout names no release", pluginVersion: "dev", says: `this kubectl-ptah is version "dev", which is no release; name the release to install with --version`},
		{name: "a version that is not one", arguments: []string{"--version", "latest"}, says: `--version "latest" is not a release version`},
		{name: "a version range", arguments: []string{"--version", ">=0.2.10"}, says: `is not a release version`},
		{name: "a version with two numbers", arguments: []string{"--version", "0.2"}, says: `is not a release version`},
		{name: "an empty version", arguments: []string{"--version", ""}, says: `--version "" is not a release version`},
		{name: "a release before the registry", arguments: []string{"--version", "0.2.9"}, says: "release 0.2.9 published no chart to ghcr.io/stokaro/charts/ptah-operator; the first that did is 0.2.10"},
		{name: "a plugin from before the registry", pluginVersion: "v0.2.9", says: "release 0.2.9 published no chart"},
		{name: "an empty approver", arguments: []string{"--approver", ""}, says: `--approver "" is not a username`},
		{name: "an approver padded with space", arguments: []string{"--approver", "alice "}, says: `--approver "alice " is not a username`},
		{name: "an empty namespace", arguments: []string{"--namespace", ""}, says: "--namespace is empty"},
		{name: "an out-dir without yaml", arguments: []string{"--out-dir", "deploy"}, says: "--out-dir is where --yaml writes"},
		{name: "a kubeconfig with yaml", arguments: []string{"--yaml", "--kubeconfig", "config"}, says: "--yaml reads no cluster"},
		{name: "a context with yaml", arguments: []string{"--yaml", "--context", "kind"}, says: "--yaml reads no cluster"},
		{name: "a timeout of nothing", arguments: []string{"--timeout", "0s"}, says: "--timeout must be positive"},
		{name: "an argument", arguments: []string{"ptah-operator"}, says: `install takes no arguments, and was given ["ptah-operator"]`},
		{name: "a flag install does not take", arguments: []string{"--set", "image.digest=x"}, says: "unknown flag: --set"},
		{name: "an out-dir an earlier render wrote", arguments: []string{"--yaml", "--out-dir", existing}, says: "already exists; remove it first"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			helm := &recordingHelm{}
			install := releasedInstaller(helm)
			if test.pluginVersion != "" {
				install.pluginVersion = test.pluginVersion
			}
			var stdout, stderr bytes.Buffer

			status := install.run(context.Background(), test.arguments, &stdout, &stderr)

			if status != exitUsage {
				t.Fatalf("install %v = %d, want %d\n%s", test.arguments, status, exitUsage, stderr.String())
			}
			if !strings.Contains(stderr.String(), test.says) {
				t.Fatalf("install %v said:\n%s\nwant %q", test.arguments, stderr.String(), test.says)
			}
			if len(helm.calls) != 0 || stdout.Len() != 0 {
				t.Fatalf("a refused install ran helm %v and printed %q", helm.calls, stdout.String())
			}
		})
	}
}

// The whole helm command line, for each shape of install. Every argument is
// listed, so an image value, a --set, or a flag that skips the chart's checks
// such as --skip-schema-validation or --post-renderer fails a row here.
func TestInstallHandsHelmTheseArguments(t *testing.T) {
	t.Parallel()
	const chart = "oci://ghcr.io/stokaro/charts/ptah-operator"
	tests := []struct {
		name          string
		pluginVersion string
		arguments     []string
		want          []string
	}{
		{
			name: "the plugin's own release",
			want: []string{"upgrade", "--install", "ptah-operator", chart, "--version", "0.2.11", "--namespace", "ptah-system",
				"--create-namespace", "--wait", "--timeout", "5m0s"},
		},
		{
			name: "the plugin's own release, rendered", arguments: []string{"--yaml"},
			want: []string{"template", "ptah-operator", chart, "--version", "0.2.11", "--namespace", "ptah-system"},
		},
		{
			name: "a release named, with a v", pluginVersion: "dev", arguments: []string{"--version", "v0.2.12"},
			want: []string{"upgrade", "--install", "ptah-operator", chart, "--version", "0.2.12", "--namespace", "ptah-system",
				"--create-namespace", "--wait", "--timeout", "5m0s"},
		},
		{
			name: "the first published release", arguments: []string{"--version", "0.2.10"},
			want: []string{"upgrade", "--install", "ptah-operator", chart, "--version", "0.2.10", "--namespace", "ptah-system",
				"--create-namespace", "--wait", "--timeout", "5m0s"},
		},
		{
			name: "every flag",
			arguments: []string{"-n", "operators", "--approver", "alice@example.com", "--approver", `o'brien, "ops"`,
				"-f", "first.yaml", "--values", "second.yaml", "--kubeconfig", "/tmp/config", "--context", "kind-lab", "--timeout", "90s"},
			want: []string{"upgrade", "--install", "ptah-operator", chart, "--version", "0.2.11", "--namespace", "operators",
				"--values", "first.yaml", "--values", "second.yaml",
				"--set-json", `approverClusterRole.subjects=[{"kind":"User","name":"alice@example.com"},{"kind":"User","name":"o'brien, \"ops\""}]`,
				"--create-namespace", "--wait", "--timeout", "1m30s", "--kubeconfig", "/tmp/config", "--kube-context", "kind-lab"},
		},
		{
			name:      "every flag, rendered into a directory",
			arguments: []string{"-n", "operators", "--approver", "alice@example.com", "-f", "first.yaml", "--yaml", "--out-dir", "deploy"},
			want: []string{"template", "ptah-operator", chart, "--version", "0.2.11", "--namespace", "operators",
				"--values", "first.yaml",
				"--set-json", `approverClusterRole.subjects=[{"kind":"User","name":"alice@example.com"}]`,
				"--output-dir", "deploy"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			helm := &recordingHelm{}
			install := releasedInstaller(helm)
			if test.pluginVersion != "" {
				install.pluginVersion = test.pluginVersion
			}
			var stdout, stderr bytes.Buffer

			if status := install.run(context.Background(), test.arguments, &stdout, &stderr); status != exitOK {
				t.Fatalf("install %v = %d\n%s", test.arguments, status, stderr.String())
			}
			if len(helm.calls) != 1 || !slices.Equal(helm.calls[0], test.want) {
				t.Fatalf("install %v ran helm with\n%q\nwant\n%q", test.arguments, helm.calls, test.want)
			}
			// Printed first, as a person would paste it into a shell.
			if want := "kubectl-ptah: running " + shellQuote(append([]string{"helm"}, test.want...)) + "\n"; stderr.String() != want {
				t.Fatalf("install printed\n%s\nwant\n%s", stderr.String(), want)
			}
		})
	}
}

// --yaml writes what the install would apply only if both hand helm the same
// chart, version, namespace and values. The rows above list each command line;
// this holds the shared part of every pair to one list.
func TestInstallAndItsRenderShareTheChartArguments(t *testing.T) {
	t.Parallel()
	for _, flags := range [][]string{
		nil,
		{"--version", "0.2.12"},
		{"-n", "operators", "--approver", "alice@example.com", "--approver", "bob", "-f", "a.yaml", "-f", "b.yaml"},
	} {
		helm := &recordingHelm{}
		install := releasedInstaller(helm)
		for _, arguments := range [][]string{flags, append(slices.Clone(flags), "--yaml", "--out-dir", t.TempDir())} {
			if status := install.run(context.Background(), arguments, io.Discard, io.Discard); status != exitOK {
				t.Fatalf("install %v = %d", arguments, status)
			}
		}
		installed, rendered := helm.calls[0], helm.calls[1]
		if installed[0] != "upgrade" || installed[1] != "--install" || rendered[0] != "template" {
			t.Fatalf("the calls are %q", helm.calls)
		}
		upTo := slices.Index(installed, "--create-namespace")
		renderedUpTo := slices.Index(rendered, "--output-dir")
		if upTo < 0 || renderedUpTo < 0 || !slices.Equal(installed[2:upTo], rendered[1:renderedUpTo]) {
			t.Fatalf("with %v, the install hands helm\n%q\nand the render\n%q", flags, installed, rendered)
		}
	}
}

func TestInstallReportsAHelmThatFailed(t *testing.T) {
	t.Parallel()
	helm := &recordingHelm{err: errors.New("helm upgrade: exit status 1")}
	var stderr bytes.Buffer

	status := releasedInstaller(helm).run(context.Background(), nil, io.Discard, &stderr)

	if status != exitFailed || !strings.Contains(stderr.String(), "kubectl-ptah: helm upgrade: exit status 1") {
		t.Fatalf("install = %d, said:\n%s", status, stderr.String())
	}
}

func TestInstallRunsHelm4OrNewer(t *testing.T) {
	t.Parallel()
	for reported, refusal := range map[string]string{
		"v4.3.1\n":          "",
		"v4.0.0":            "",
		"v10.1.0":           "",
		"v3.19.2":           "helm is v3.19.2, and install runs Helm 4 or newer",
		"v2.17.0":           "helm is v2.17.0, and install runs Helm 4 or newer",
		"4.3.1":             `helm reports version "4.3.1", which this command cannot read`,
		"":                  `helm reports version "", which this command cannot read`,
		"version.BuildInfo": "which this command cannot read",
	} {
		err := requireHelm(reported)
		switch {
		case refusal == "" && err != nil:
			t.Errorf("requireHelm(%q) = %v, want it accepted", reported, err)
		case refusal != "" && (err == nil || !strings.Contains(err.Error(), refusal)):
			t.Errorf("requireHelm(%q) = %v, want %q", reported, err, refusal)
		}
	}
}

func TestShellQuoteKeepsEachArgumentWhole(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		command []string
		want    string
	}{
		{[]string{"helm", "--version", "0.2.11"}, "helm --version 0.2.11"},
		{[]string{"helm", "--set-json", `approverClusterRole.subjects=[{"kind":"User","name":"o'brien"}]`},
			`helm --set-json 'approverClusterRole.subjects=[{"kind":"User","name":"o'\''brien"}]'`},
		{[]string{"helm", "--values", "my values.yaml"}, "helm --values 'my values.yaml'"},
		{[]string{"helm", "--version", ""}, "helm --version ''"},
	} {
		if got := shellQuote(row.command); got != row.want {
			t.Errorf("shellQuote(%q) = %s, want %s", row.command, got, row.want)
		}
	}
}

// The install page shows the helm command install runs, for a reader without
// the plugin. A flag added to one and not the other would install two
// different ways from one page.
func TestTheInstallPageShowsTheCommandInstallRuns(t *testing.T) {
	t.Parallel()
	const page = "../../docs/site/src/content/docs/start/install.md"
	content, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(content), "## Choose a version, once\n")
	var shown string
	for _, block := range strings.Split(section, "```sh\n")[1:] {
		body, _, _ := strings.Cut(block, "```")
		if strings.HasPrefix(body, "helm upgrade --install ") {
			shown = body
			break
		}
	}
	if !found || shown == "" {
		t.Fatalf("%s shows no helm command under Choose a version, once", page)
	}
	shown = strings.Join(strings.Fields(strings.ReplaceAll(shown, "\\\n", " ")), " ")
	shown = strings.ReplaceAll(shown, `"$VERSION"`, "0.2.11")

	helm := &recordingHelm{}
	if status := releasedInstaller(helm).run(context.Background(), []string{"--version", "0.2.11"}, io.Discard, io.Discard); status != exitOK {
		t.Fatalf("install = %d", status)
	}
	if runs := shellQuote(append([]string{"helm"}, helm.calls[0]...)); shown != runs {
		t.Fatalf("%s shows\n%s\nand install runs\n%s", page, shown, runs)
	}
}
