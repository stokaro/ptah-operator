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

// recordingHelm stands in for helm: it keeps every argument list it was handed.
// It answers helm list with listed, no release when that is empty, or with
// listErr, and every other command with err.
type recordingHelm struct {
	calls   [][]string
	listed  string
	listErr error
	err     error
}

func (h *recordingHelm) run(_ context.Context, arguments []string, stdout, _ io.Writer) error {
	h.calls = append(h.calls, slices.Clone(arguments))
	if arguments[0] != "list" {
		return h.err
	}
	if h.listErr != nil {
		return h.listErr
	}
	listed := h.listed
	if listed == "" {
		listed = "[]"
	}
	_, err := io.WriteString(stdout, listed)
	return err
}

// writes is the commands that were not helm list.
func (h *recordingHelm) writes() [][]string {
	var writes [][]string
	for _, call := range h.calls {
		if call[0] != "list" {
			writes = append(writes, call)
		}
	}
	return writes
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
		{name: "values from standard input", arguments: []string{"-f", "-"}, says: `--values "-" is not a path install can hand helm`},
		{name: "values with a comma", arguments: []string{"-f", "team,a.yaml"}, says: `--values "team,a.yaml" is not a path install can hand helm`},
		{name: "values with a quote", arguments: []string{"-f", `a"b.yaml`}, says: `is not a path install can hand helm`},
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
				"--reset-values", "--create-namespace", "--wait", "--timeout", "5m0s"},
		},
		{
			name: "the plugin's own release, rendered", arguments: []string{"--yaml"},
			want: []string{"template", "ptah-operator", chart, "--version", "0.2.11", "--namespace", "ptah-system"},
		},
		{
			name: "a release named, with a v", pluginVersion: "dev", arguments: []string{"--version", "v0.2.12"},
			want: []string{"upgrade", "--install", "ptah-operator", chart, "--version", "0.2.12", "--namespace", "ptah-system",
				"--reset-values", "--create-namespace", "--wait", "--timeout", "5m0s"},
		},
		{
			name: "the first published release", arguments: []string{"--version", "0.2.10"},
			want: []string{"upgrade", "--install", "ptah-operator", chart, "--version", "0.2.10", "--namespace", "ptah-system",
				"--reset-values", "--create-namespace", "--wait", "--timeout", "5m0s"},
		},
		{
			name: "every flag",
			arguments: []string{"-n", "operators", "--approver", "alice@example.com", "--approver", `o'brien, "ops"`,
				"-f", "first.yaml", "--values", "second.yaml", "--kubeconfig", "/tmp/config", "--context", "kind-lab", "--timeout", "90s"},
			want: []string{"upgrade", "--install", "ptah-operator", chart, "--version", "0.2.11", "--namespace", "operators",
				"--values", "first.yaml", "--values", "second.yaml",
				"--set-json", `approverClusterRole.subjects=[{"kind":"User","name":"alice@example.com"},{"kind":"User","name":"o'brien, \"ops\""}]`,
				"--reset-values", "--create-namespace", "--wait", "--timeout", "1m30s", "--kubeconfig", "/tmp/config", "--kube-context", "kind-lab"},
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
			if writes := helm.writes(); len(writes) != 1 || !slices.Equal(writes[0], test.want) {
				t.Fatalf("install %v ran helm with\n%q\nwant\n%q", test.arguments, helm.calls, test.want)
			}
			// An install reads the release first, of the cluster it then writes;
			// a render reads nothing.
			var reads [][]string
			if test.want[0] != "template" {
				namespace := test.want[slices.Index(test.want, "--namespace")+1]
				read := []string{"list", "--namespace", namespace, "--filter", "^ptah-operator$", "--deployed", "--failed", "--pending", "--output", "json"}
				if at := slices.Index(test.want, "--kubeconfig"); at >= 0 {
					read = append(read, test.want[at:]...)
				}
				reads = [][]string{read}
			}
			if got := helm.calls[:len(helm.calls)-1]; !slices.EqualFunc(got, reads, slices.Equal) {
				t.Fatalf("install %v read the cluster with\n%q\nwant\n%q", test.arguments, got, reads)
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
		installed, rendered := helm.writes()[0], helm.writes()[1]
		if installed[0] != "upgrade" || installed[1] != "--install" || rendered[0] != "template" {
			t.Fatalf("the calls are %q", helm.calls)
		}
		upTo := slices.Index(installed, "--reset-values")
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

// install does not upgrade: a release of another chart version would be
// upgraded without --force-conflicts, which leaves the manager at no replicas.
// It reads the release first, and writes only over the same version.
func TestInstallWritesOnlyOverItsOwnVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		listed  string
		listErr error
		status  int
		says    string
	}{
		{name: "nothing installed", listed: "[]", status: exitOK},
		{name: "the same version", listed: `[{"name":"ptah-operator","namespace":"ptah-system","status":"deployed","chart":"ptah-operator-0.2.11"}]`, status: exitOK},
		{name: "the same version, failed", listed: `[{"name":"ptah-operator","status":"failed","chart":"ptah-operator-0.2.11"}]`, status: exitOK},
		{name: "a release whose name only contains it", listed: `[{"name":"ptah-operator-old","chart":"ptah-operator-0.2.10"}]`, status: exitOK},
		{
			name: "an older version", listed: `[{"name":"ptah-operator","status":"deployed","chart":"ptah-operator-0.2.10"}]`, status: exitUsage,
			says: "release ptah-operator in namespace ptah-system is 0.2.10, and this is 0.2.11: install does not upgrade. Upgrade with helm as Upgrade in the Operations page says",
		},
		{
			name: "a newer version", listed: `[{"name":"ptah-operator","status":"pending-upgrade","chart":"ptah-operator-0.2.12"}]`, status: exitUsage,
			says: "is 0.2.12, and this is 0.2.11: install does not upgrade",
		},
		{
			name: "another chart", listed: `[{"name":"ptah-operator","chart":"something-else-1.0.0"}]`, status: exitUsage,
			says: "release ptah-operator in namespace ptah-system is chart something-else-1.0.0, which install did not put there",
		},
		{name: "a list helm could not run", listErr: errors.New("helm list: exit status 1"), status: exitFailed, says: "read the installed release: helm list: exit status 1"},
		{name: "a list install cannot read", listed: "NAME\tNAMESPACE\n", status: exitFailed, says: "helm list printed what install cannot read"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			helm := &recordingHelm{listed: test.listed, listErr: test.listErr}
			var stderr bytes.Buffer

			status := releasedInstaller(helm).run(context.Background(), nil, io.Discard, &stderr)

			if status != test.status || !strings.Contains(stderr.String(), test.says) {
				t.Fatalf("install = %d, said:\n%s\nwant %d and %q", status, stderr.String(), test.status, test.says)
			}
			if wrote := len(helm.writes()) != 0; wrote != (test.status == exitOK) {
				t.Fatalf("install = %d and ran %q", status, helm.calls)
			}
		})
	}
}

// helmOnPath runs the helm a person has, after asking it its version. The
// rows put a helm of their own first on PATH, so they share the process
// environment and do not run in parallel.
func TestInstallRunsOnlyAHelmItCanUse(t *testing.T) {
	directory := t.TempDir()
	ran := filepath.Join(directory, "ran")
	for _, row := range []struct {
		name, version, says string
		runs                bool
	}{
		{name: "Helm 4", version: "v4.3.1", runs: true},
		{name: "Helm 3", version: "v3.19.2", says: "helm is v3.19.2, and install runs Helm 4 or newer"},
		{name: "a version it cannot read", version: "unknown", says: "which this command cannot read"},
	} {
		t.Run(row.name, func(t *testing.T) {
			_ = os.Remove(ran)
			script := "#!/bin/sh\nif [ \"$1\" = version ]; then printf '%s' '" + row.version + "'; exit 0; fi\n: > '" + ran + "'\n"
			if err := os.WriteFile(filepath.Join(directory, "helm"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", directory)

			err := helmOnPath(nil)(context.Background(), []string{"upgrade", "--install"}, io.Discard, io.Discard)

			_, statErr := os.Stat(ran)
			ranIt := statErr == nil
			if row.runs {
				if err != nil || !ranIt {
					t.Fatalf("helm %s: err = %v, and it ran: %v", row.version, err, ranIt)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), row.says) || ranIt {
				t.Fatalf("helm %s: err = %v, want %q and nothing run (ran: %v)", row.version, err, row.says, ranIt)
			}
		})
	}
	t.Run("no helm", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		err := helmOnPath(nil)(context.Background(), []string{"upgrade"}, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "install runs Helm 4 or newer, and no helm is on PATH") {
			t.Fatalf("err = %v", err)
		}
	})
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
	if runs := shellQuote(append([]string{"helm"}, helm.writes()[0]...)); shown != runs {
		t.Fatalf("%s shows\n%s\nand install runs\n%s", page, shown, runs)
	}
}
