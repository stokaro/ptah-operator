package main

import (
	"regexp"
	"strings"
	"testing"

	utilversion "k8s.io/apimachinery/pkg/util/version"
)

// API compatibility tells a reader that an upgrade can require them to edit
// their resources and that the release notes are where that is said. Nothing
// in the published release carries it: the release body is the authenticated
// manifest, twelve key=value records of digests and identities, by design.
//
// So the notes are a page in this repository, and these hold the promise to
// it. The one that matters is the first: a version published with no entry
// reads to a reader exactly like a version that changed nothing.

const (
	releaseNotesPage       = "docs/site/src/content/docs/support/release-notes.md"
	upgradeLead            = "**Before you upgrade.**"
	releaseNotesLinkTarget = "../release-notes/"
)

var (
	releaseNotesVersion = regexp.MustCompile(`(?m)^## ([0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?)$`)
	chartVersionRecord  = regexp.MustCompile(`(?m)^version: ([0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?)$`)
)

// The version a release publishes is the one the chart declares: the workflow
// packages `ptah-operator-<chart version>.tgz` and refuses a tag that names
// anything else. So the chart is what says which entry has to exist.
func TestTheReleaseNotesCoverTheVersionTheChartPublishes(t *testing.T) {
	t.Parallel()
	version := chartVersion(t)
	for _, entry := range releaseNotesVersions(t) {
		if entry == version {
			return
		}
	}
	t.Fatalf("the chart publishes %s and the release notes have no entry for it, so that release would tell a reader it changed nothing", version)
}

// A reader opens this page to find the version they are moving to, and reads
// down until they have passed the one they are on. That only works in order.
func TestTheReleaseNotesReadNewestFirst(t *testing.T) {
	t.Parallel()
	versions := releaseNotesVersions(t)
	if len(versions) == 0 {
		t.Fatalf("%s names no version, so this check reads the page and holds none of it", releaseNotesPage)
	}
	seen := map[string]bool{}
	for index, version := range versions {
		if seen[version] {
			t.Errorf("%s is listed twice, so one of the two entries is the one nobody reads", version)
		}
		seen[version] = true
		if index == 0 {
			continue
		}
		if compareVersions(t, versions[index-1], version) <= 0 {
			t.Errorf("%s is listed after %s; the page reads newest first", version, versions[index-1])
		}
	}
}

// Every entry answers the question the page exists for. "Nothing" is an
// answer and silence is not: a reader cannot tell an entry that asks nothing
// of them from one whose author did not consider it.
func TestEveryReleaseNoteSaysWhatToChangeBeforeUpgrading(t *testing.T) {
	t.Parallel()
	page := readDocumentationPage(t, releaseNotesPage)
	sections := releaseNotesVersion.FindAllStringSubmatchIndex(page, -1)
	if len(sections) == 0 {
		t.Fatalf("%s names no version, so this check reads the page and holds none of it", releaseNotesPage)
	}
	for index, section := range sections {
		end := len(page)
		if index+1 < len(sections) {
			end = sections[index+1][0]
		}
		version := page[section[2]:section[3]]
		body := page[section[1]:end]
		lead := strings.Index(body, upgradeLead)
		if lead < 0 {
			t.Errorf("the %s entry does not say what to change before upgrading, so a reader cannot tell it asks nothing of them from an author who did not ask", version)
			continue
		}
		answer := strings.TrimSpace(body[lead+len(upgradeLead):])
		if answer == "" || strings.HasPrefix(answer, "##") {
			t.Errorf("the %s entry leads with %q and says nothing after it", version, upgradeLead)
		}
	}
}

// The promise and the page that keeps it, wired together. A link check
// catches an address that resolves to nothing; this catches the sentence
// going back to naming the release notes in the abstract.
func TestTheCompatibilityPagePointsAtTheReleaseNotes(t *testing.T) {
	t.Parallel()
	page := readDocumentationPage(t, apiCompatibilityPage)
	if !strings.Contains(page, releaseNotesLinkTarget) {
		t.Fatalf("%s promises release notes and links to none, and the published release body is a digest manifest", apiCompatibilityPage)
	}
}

// chartVersion reads the version the release workflow packages and tags.
func chartVersion(t *testing.T) string {
	t.Helper()
	match := chartVersionRecord.FindStringSubmatch(readDocumentationPage(t, chartPath))
	if match == nil {
		t.Fatalf("%s carries no version record", chartPath)
	}
	return match[1]
}

// releaseNotesVersions lists the versions the page carries, in page order.
func releaseNotesVersions(t *testing.T) []string {
	t.Helper()
	var versions []string
	for _, match := range releaseNotesVersion.FindAllStringSubmatch(readDocumentationPage(t, releaseNotesPage), -1) {
		versions = append(versions, match[1])
	}
	return versions
}

// compareVersions uses semantic precedence, including numeric prerelease parts.
func compareVersions(t *testing.T, left, right string) int {
	t.Helper()
	version, err := utilversion.ParseSemantic(left)
	if err != nil {
		t.Fatalf("invalid release version %q: %v", left, err)
	}
	comparison, err := version.Compare(right)
	if err != nil {
		t.Fatalf("invalid release version %q: %v", right, err)
	}
	return comparison
}

func TestReleaseNotesRecognizePrereleases(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"0.1.0", "0.1.0-rc.1", "0.1.0-rc.10"} {
		for _, fixture := range []struct {
			pattern *regexp.Regexp
			line    string
		}{
			{releaseNotesVersion, "## " + version},
			{chartVersionRecord, "version: " + version},
		} {
			match := fixture.pattern.FindStringSubmatch(fixture.line)
			if len(match) != 2 || match[1] != version {
				t.Fatalf("release version was not read from %q: %v", fixture.line, match)
			}
		}
	}
	for _, pair := range [][2]string{
		{"0.1.0", "0.1.0-rc.10"},
		{"0.1.0-rc.10", "0.1.0-rc.2"},
		{"0.1.0-rc.1", "0.1.0-beta.1"},
		{"0.10.0-rc.1", "0.9.0"},
	} {
		if compareVersions(t, pair[0], pair[1]) != 1 || compareVersions(t, pair[1], pair[0]) != -1 {
			t.Errorf("release precedence must order %s after %s", pair[0], pair[1])
		}
	}
	if compareVersions(t, "0.1.0-rc.1", "0.1.0-rc.1") != 0 {
		t.Error("equal prereleases must compare equally")
	}
}
