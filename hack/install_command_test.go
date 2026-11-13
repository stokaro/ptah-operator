package main

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/hack/chartstamp"
)

// The install page carries the first command a reader runs. Helm ignores a
// --set path the chart does not have, and the chart refuses to render without
// four values it ships no default for, so the page and the chart can disagree
// in both directions with nothing failing until somebody's first install does.
//
// So this runs the documented command. The placeholders the page shows a reader
// -- <operator-image-digest> and the rest -- are filled with values of the shape
// the page asks for, and the chart has to render from exactly that.

const installPage = "docs/site/src/content/docs/start/install.md"

var (
	installSetting     = regexp.MustCompile(`--set-string ([A-Za-z0-9_.]+)=(\S+)`)
	installPlaceholder = regexp.MustCompile(`<[a-z-]+>`)
	installShellBlock  = regexp.MustCompile("(?s)```sh\n(.*?)```")
)

// placeholderValues are the shapes the page asks a reader for.
var placeholderValues = map[string]string{
	"<operator-image-digest>": strings.Repeat("a", 64),
	"<executor-image-digest>": strings.Repeat("b", 64),
	"<ptah-version>":          "v0.7.0",
}

func TestTheDocumentedInstallCommandRendersTheChart(t *testing.T) {
	t.Parallel()
	helm := helmOrSkip(t)
	settings := documentedInstallSettings(t)
	if len(settings) == 0 {
		t.Fatalf("%s documents no --set-string value, so this check runs nothing", installPage)
	}
	arguments := []string{"template", "ptah-operator", repositoryFile(t, "charts/ptah-operator"), "--namespace", "ptah-system"}
	for _, setting := range settings {
		arguments = append(arguments, "--set-string", setting)
	}
	output, err := exec.Command(helm, arguments...).CombinedOutput() //nolint:gosec // Arguments are read from the repository.
	if err != nil {
		t.Fatalf("the documented install command does not render the chart: %v\n%s", err, output)
	}
}

// And each documented value is one the chart refuses to render without, so the
// page asks a reader for nothing it does not need.
func TestEveryDocumentedInstallValueIsOneTheChartRequires(t *testing.T) {
	t.Parallel()
	helm := helmOrSkip(t)
	settings := documentedInstallSettings(t)
	if len(settings) == 0 {
		t.Fatalf("%s documents no --set-string value, so this check runs nothing", installPage)
	}
	for omitted := range settings {
		arguments := []string{"template", "ptah-operator", repositoryFile(t, "charts/ptah-operator"), "--namespace", "ptah-system"}
		for index, setting := range settings {
			if index == omitted {
				continue
			}
			arguments = append(arguments, "--set-string", setting)
		}
		if _, err := exec.Command(helm, arguments...).CombinedOutput(); err == nil { //nolint:gosec // Arguments are read from the repository.
			name, _, _ := strings.Cut(settings[omitted], "=")
			t.Errorf("the chart renders without %s, so the install page asks a reader for a value it does not need", name)
		}
	}
}

// documentedInstallSettings reads the page's --set-string flags with their
// placeholders filled in.
func documentedInstallSettings(t *testing.T) []string {
	t.Helper()
	var settings []string
	for _, match := range installSetting.FindAllStringSubmatch(readDocumentationPage(t, installPage), -1) {
		value := installPlaceholder.ReplaceAllStringFunc(match[2], func(placeholder string) string {
			replacement, ok := placeholderValues[placeholder]
			if !ok {
				t.Fatalf("%s uses the placeholder %s and this check has no value of that shape", installPage, placeholder)
			}
			return replacement
		})
		settings = append(settings, match[1]+"="+value)
	}
	return settings
}

// The first path on the page is one command for a published release, with its
// verification beside it. It names what a release publishes and asks for no value,
// which is the whole of what it offers over the path below it.
func TestTheInstallPageOpensWithTheRegistryCommand(t *testing.T) {
	t.Parallel()
	page := readDocumentationPage(t, installPage)
	_, section, found := strings.Cut(page, "## Choose a version, once\n")
	blocks := installShellBlock.FindAllStringSubmatch(section, -1)
	if !found || len(blocks) < 2 {
		t.Fatalf("%s shows %d commands under Choose a version, once, want the plugin's and the helm one it runs", installPage, len(blocks))
	}
	first, equivalent := blocks[0][1], blocks[1][1]
	if problem := registryInstallProblem(first); problem != "" {
		t.Errorf("%s: %s", installPage, problem)
	}
	for mistake, mutated := range map[string]string{
		"a value to copy":    strings.Replace(first, `--version "$VERSION"`, `--version "$VERSION" --values values.yaml`, 1),
		"an approver":        strings.Replace(first, `--version "$VERSION"`, `--version "$VERSION" --approver alice`, 1),
		"no verification":    strings.Replace(first, "gh attestation verify", "echo", 1),
		"another repository": strings.ReplaceAll(first, chartstamp.Repository, "ghcr.io/stokaro/other"),
		"helm instead":       strings.Replace(first, "kubectl ptah install", "helm upgrade --install ptah-operator", 1),
		"no version":         strings.Replace(first, ` --version "$VERSION"`, "", 1),
	} {
		if registryInstallProblem(mutated) == "" {
			t.Errorf("the check accepted %s", mistake)
		}
	}
	if !strings.Contains(equivalent, "helm upgrade --install ptah-operator oci://"+chartstamp.Repository) ||
		strings.Contains(equivalent, "--set") || strings.Contains(equivalent, "--values") {
		t.Errorf("%s follows the plugin with\n%s\nwhich is not the registry install it runs", installPage, equivalent)
	}
	registry, explicit := strings.Index(page, "helm upgrade --install ptah-operator oci://"), strings.Index(page, "helm upgrade --install ptah-operator ./charts")
	if registry < 0 || explicit < 0 || registry > explicit {
		t.Errorf("%s has the registry command at %d and the explicit values at %d, and the registry comes first", installPage, registry, explicit)
	}
}

func registryInstallProblem(block string) string {
	for _, required := range []string{
		"gh attestation verify \"oci://" + chartstamp.Repository + ":$VERSION\"",
		"--signer-workflow stokaro/ptah-operator/.github/workflows/release.yml",
		"kubectl ptah install --version \"$VERSION\"\n",
	} {
		if !strings.Contains(block, required) {
			return "the first command no longer says " + strings.TrimSpace(required)
		}
	}
	for _, value := range []string{"--set", "--values", "-f ", "--approver"} {
		if strings.Contains(block, value) {
			return "the first command asks for " + value
		}
	}
	return ""
}

func helmOrSkip(t *testing.T) string {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required to render the documented install command")
	}
	return helm
}
