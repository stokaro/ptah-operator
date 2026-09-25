package main

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
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
)

// placeholderValues are the shapes the page asks a reader for.
var placeholderValues = map[string]string{
	"<operator-image-digest>": strings.Repeat("a", 64),
	"<ptah-image-digest>":     strings.Repeat("b", 64),
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

func helmOrSkip(t *testing.T) string {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required to render the documented install command")
	}
	return helm
}
