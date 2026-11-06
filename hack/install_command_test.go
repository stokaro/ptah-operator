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
	block := installShellBlock.FindStringSubmatch(section)
	if !found || block == nil {
		t.Fatalf("%s shows no command under Choose a version, once", installPage)
	}
	if problem := registryInstallProblem(block[1]); problem != "" {
		t.Errorf("%s: %s", installPage, problem)
	}
	for mistake, mutated := range map[string]string{
		"a value to copy":    strings.Replace(block[1], "--create-namespace", "--create-namespace --set-string image.digest=x", 1),
		"no verification":    strings.Replace(block[1], "gh attestation verify", "echo", 1),
		"another repository": strings.ReplaceAll(block[1], chartstamp.Repository, "ghcr.io/stokaro/other"),
	} {
		if registryInstallProblem(mutated) == "" {
			t.Errorf("the check accepted %s", mistake)
		}
	}
	first, second := strings.Index(page, "helm upgrade --install ptah-operator oci://"), strings.Index(page, "helm upgrade --install ptah-operator ./charts")
	if first < 0 || second < 0 || first > second {
		t.Errorf("%s has the registry command at %d and the explicit values at %d, and the registry comes first", installPage, first, second)
	}
}

func registryInstallProblem(block string) string {
	for _, required := range []string{
		"gh attestation verify \"oci://" + chartstamp.Repository + ":$VERSION\"",
		"--signer-workflow stokaro/ptah-operator/.github/workflows/release.yml",
		"helm upgrade --install ptah-operator oci://" + chartstamp.Repository,
		"--version \"$VERSION\"",
		"--create-namespace",
	} {
		if !strings.Contains(block, required) {
			return "the first command no longer says " + required
		}
	}
	if strings.Contains(block, "--set") || strings.Contains(block, "--values") {
		return "the first command asks for a value"
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
