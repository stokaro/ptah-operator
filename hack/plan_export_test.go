package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/planstore"
)

const planPluginSource = "cmd/kubectl-ptah/main.go"

var declaredFlag = regexp.MustCompile(`flags\.(?:Bool|String|StringP|Duration)\("([a-z-]+)"`)

// The export procedure selects a plan's chunks by label, and the label has to
// be the one the plan store writes.
//
// It is the only way to find the ConfigMaps of a plan that is neither current
// nor applied, and a reader whose selector matches nothing has no way to tell
// that from a plan whose chunks are already gone.
func TestTheExportProcedureSelectsChunksByTheLabelTheStoreWrites(t *testing.T) {
	t.Parallel()
	guide := string(readOperationsGuide(t))
	if !strings.Contains(guide, planstore.LabelPlan+"=<plan-name>") {
		t.Fatalf("%s does not select plan chunks by %q, which is the label the store writes",
			operationsGuide, planstore.LabelPlan)
	}
}

// The procedure says `kubectl ptah plan` cannot render the plan being pruned,
// and builds a longer export around that.
//
// The claim is about the plugin's flags: it selects the current plan or the
// applied one, and takes no way to name a third. A flag that selected a plan
// by name would make the whole section unnecessary and, left unchanged, wrong.
func TestThePlanPluginStillCannotNameAHistoricalPlan(t *testing.T) {
	t.Parallel()
	source := readRepositorySource(t, planPluginSource)
	start := strings.Index(source, "\nfunc plan(")
	if start < 0 {
		t.Fatalf("%s declares no plan subcommand", planPluginSource)
	}
	end := strings.Index(source[start+1:], "\nfunc ")
	if end < 0 {
		t.Fatalf("%s does not end its plan subcommand", planPluginSource)
	}
	body := source[start : start+1+end]

	var declared []string
	for _, match := range declaredFlag.FindAllStringSubmatch(body, -1) {
		declared = append(declared, match[1])
	}
	sort.Strings(declared)
	want := []string{"applied", "context", "current", "kubeconfig", "namespace", "output", "timeout"}
	if strings.Join(declared, ",") != strings.Join(want, ",") {
		t.Fatalf("the plan subcommand declares %v, and the export procedure was written against %v;"+
			" a flag that names a plan makes that section wrong",
			declared, want)
	}
}

// And the fields the export reads have to be fields the API has. An export
// that silently produces an empty file is worse than one that fails: it is
// kept as evidence and read years later.
func TestTheExportProcedureReadsFieldsTheAPIHas(t *testing.T) {
	t.Parallel()
	guide := string(readOperationsGuide(t))
	section := exportSection(t, guide)
	crds := shippedPlanCRDs(t)
	for _, path := range []string{
		"spec.chunks",
		"spec.contentDigest",
		"spec.artifactDigest",
	} {
		if !strings.Contains(section, path) {
			t.Errorf("the export procedure no longer reads %s", path)
			continue
		}
		if !crds[path] {
			t.Errorf("the export procedure reads %s, which no shipped plan CRD has", path)
		}
	}
}

// exportSection returns the part of the guide the export procedure occupies.
func exportSection(t *testing.T, guide string) string {
	t.Helper()
	const heading = "### Export a plan before deleting it"
	start := strings.Index(guide, heading)
	if start < 0 {
		t.Fatalf("%s carries no export procedure", operationsGuide)
	}
	rest := guide[start+len(heading):]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		return rest[:end]
	}
	return rest
}

// shippedPlanCRDs reports which of the paths the export reads exist on the two
// plan kinds.
func shippedPlanCRDs(t *testing.T) map[string]bool {
	t.Helper()
	present := map[string]bool{}
	for _, name := range []string{
		"operator.ptah.run_ptahschemaplans.yaml",
		"operator.ptah.run_ptahmigrationplans.yaml",
	} {
		document := readRepositorySource(t, filepath.Join("config", "crd", "bases", name))
		for _, path := range []string{"spec.chunks", "spec.contentDigest", "spec.artifactDigest"} {
			field := path[strings.LastIndex(path, ".")+1:]
			if regexp.MustCompile(`(?m)^\s+` + field + `:`).MatchString(document) {
				present[path] = true
			}
		}
	}
	if len(present) == 0 {
		t.Fatal("no plan field was found, so this check would pass over nothing")
	}
	return present
}

func readRepositorySource(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(repositoryFile(t, path)) //nolint:gosec // A path under the repository.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}
