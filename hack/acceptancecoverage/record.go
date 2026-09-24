// The acceptance record #242 asks for, with everything the repository can
// answer already answered.
//
// The record was a blank page, and a blank page is why every requirement reads
// "Not assessed" whether or not anyone has looked. Most of the candidate's
// identity is in the tree at a commit: the schema and controller-state
// contracts, the Ptah build and its runner protocol, the supported minors, the
// suites and what each covers. Typing those by hand into a record is how they
// come to disagree with the release they describe.
//
// What a build or a deployment decides is left blank and named, because an
// unfilled capacity or recovery target leaves acceptance incomplete and a
// record that hides the blank reads as complete.
//
// No requirement is marked passed here. A closed issue, a merged fix and a
// green run against a pull request are none of them evidence about a
// candidate; the issue says so, and this prints dispositions rather than
// awarding them.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// requirement is one PA row of the record.
type requirement struct {
	id string
	// title is the requirement's own name in #242.
	title string
	// needs is what would have to exist for a disposition other than
	// "Not assessed": the evidence, not the fix.
	needs string
	// repository is what the tree already carries toward it, which is never a
	// pass on its own.
	repository string
}

// requirements are the twelve of #242, in order. The issue is authoritative
// for their text; this names them so the record can carry a disposition for
// every one rather than for the ones somebody remembered.
var requirements = []requirement{
	{"PA-01", "Identify the candidate and its coverage",
		"the candidate's image digests and chart digest, and a run whose jobs are named as evidence",
		"the coverage table below, derived from the catalogs and the driver"},
	{"PA-02", "Execute only authorized database work",
		"every binding mutated between planning, approval and dispatch on both engines and families, with database evidence of zero unauthorized statements",
		"the binding contracts and their unit proofs under internal/controller"},
	{"PA-03", "Preserve safety through interrupted Apply",
		"faults injected before Job creation, after dispatch, during SQL, after SQL before persistence, and during lock release, observed against the database and the Pod lifecycle",
		"the fault-injection scenarios in the data-plane suite and the mutation-lifecycle contract"},
	{"PA-04", "Make progress and refusal states actionable",
		"a measured progress target, dependency recovery inside it, and no hot loop on a permanent refusal",
		"the condition-reason map and the runbooks that name a recovery path"},
	{"PA-05", "Enforce the API and authority boundaries",
		"boundary-value API cases, impersonated forbidden writes, and network policies on a cluster with a CNI that enforces",
		"the CRD schema history gates and the stored-object compatibility check"},
	{"PA-06", "Exercise installation and release transitions",
		"a real cluster reaching the documented state on every supported minor, including interrupted upgrade recovery and uninstall",
		"the lifecycle suite and the release-lifecycle reference"},
	{"PA-07", "Restore operator and database state safely",
		"a restore drill with a database ahead of the restored Kubernetes state, proving no unapproved replay",
		"the recovery runbook and its derived inventory check"},
	{"PA-08", "Establish capacity and failure limits",
		"declared thresholds and a soak long enough to measure repeated cycles and retention",
		"the plan and chunk ceilings, and the pruning runbook"},
	{"PA-09", "Detect and diagnose operational failures",
		"alerts firing inside a declared detection target and reaching a configured receiver",
		"the family-labelled metrics and the observability section"},
	{"PA-10", "Make documentation executable and usable",
		"the documented install and primary examples executed from a fresh environment on the candidate's versions",
		"the runbook shape, access and contract-page gates under hack/"},
	{"PA-11", "Verify the artifacts that will be installed",
		"checksums, signatures, provenance and a vulnerability scan against the published digests",
		"the release contract and provenance checks"},
	{"PA-12", "Retain evidence and make a bounded decision",
		"the retained evidence of every executed requirement and a recorded decision for the stated profile",
		"this record"},
}

// candidateField is one line of the identity block. supplied is empty where
// only a build or a deployment can answer.
type candidateField struct {
	name     string
	value    string
	supplied string
}

// readMakefileVersion returns a version the Makefile declares.
func readMakefileVersion(root, name string) string {
	raw, err := os.ReadFile(filepath.Join(root, "Makefile")) //nolint:gosec // A path built from the repository root.
	if err != nil {
		return ""
	}
	body := string(raw)
	match := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + ` := ([0-9]+)$`).FindStringSubmatch(body)
	if match == nil {
		return ""
	}
	return match[1]
}

// readChartVersion returns the chart's own version.
func readChartVersion(root string) string {
	raw, err := os.ReadFile(filepath.Join(root, "charts", "ptah-operator", "Chart.yaml")) //nolint:gosec // A path built from the repository root.
	if err != nil {
		return ""
	}
	match := regexp.MustCompile(`(?m)^version: (.+)$`).FindStringSubmatch(string(raw))
	if match == nil {
		return ""
	}
	return strings.TrimSpace(match[1])
}

// readCommit returns the commit the record describes.
func readCommit(root string) string {
	output, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output() //nolint:gosec // A fixed command over the repository root.
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// recordMarkdown renders the acceptance record around the coverage table.
func (c *coverage) recordMarkdown(root string, declared *profile) string {
	var out strings.Builder
	out.WriteString("# Acceptance record\n\n")
	out.WriteString("Generated by `make acceptance-record`. Every value below is read from the tree at the\n")
	out.WriteString("commit it names. A line reading `to be supplied` is one only a build or a deployment\n")
	out.WriteString("can answer, and leaving it blank leaves acceptance incomplete rather than passing.\n\n")
	out.WriteString("No requirement is marked passed here by the tree. A closed issue, a merged fix and a\n")
	out.WriteString("green run against a pull request are none of them evidence about a candidate. A\n")
	out.WriteString("disposition other than `Not assessed` comes from a declared profile passed with\n")
	out.WriteString("`-profile`, which is refused unless it names evidence and fixes every value above.\n\n")

	out.WriteString("## Candidate\n\n| Field | Value |\n| --- | --- |\n")
	for _, field := range candidateFields(root, c, declared) {
		value := field.value
		if value == "" {
			value = "_to be supplied: " + field.supplied + "_"
		}
		fmt.Fprintf(&out, "| %s | %s |\n", field.name, value)
	}

	out.WriteString("\n## Requirements\n\n")
	out.WriteString("| Requirement | Disposition | What a disposition needs | What the tree already carries |\n")
	out.WriteString("| --- | --- | --- | --- |\n")
	for _, entry := range requirements {
		verdict := declared.verdictFor(entry.id)
		disposed := verdict.Disposition
		if evidence := strings.TrimSpace(verdict.Evidence); evidence != "" {
			disposed = disposed + " — " + evidence
		}
		fmt.Fprintf(&out, "| **%s** %s | %s | %s | %s |\n",
			entry.id, entry.title, disposed, entry.needs, entry.repository)
	}

	out.WriteString("\n")
	out.WriteString(c.markdown())
	return out.String()
}

// candidateFields is the identity block: what the tree answers, and what it
// cannot.
func candidateFields(root string, c *coverage, declared *profile) []candidateField {
	supplied := &profile{}
	if declared != nil {
		supplied = declared
	}
	return []candidateField{
		{name: "Source commit", value: readCommit(root), supplied: "the commit the candidate was built from"},
		{name: "Chart version", value: readChartVersion(root)},
		{name: "CRD schema version", value: readMakefileVersion(root, "CRD_SCHEMA_VERSION")},
		{name: "Controller-state version", value: readMakefileVersion(root, "CONTROLLER_STATE_VERSION")},
		{name: "Ptah build", value: fmt.Sprintf("%s (`%s`)", c.ptah.PtahRelease, c.ptah.PtahCommit)},
		{name: "Runner protocol", value: fmt.Sprintf("%d", c.ptah.RunnerProtocolVersion)},
		{name: "Supported Kubernetes minors", value: strings.Join(c.minors, ", ")},
		{name: "Manager image digest", value: supplied.ManagerImageDigest, supplied: "the digest the release publishes"},
		{name: "Runner image digest", value: supplied.RunnerImageDigest, supplied: "the digest the release publishes"},
		{name: "Executor image digest", value: supplied.ExecutorImageDigest, supplied: "the digest the release publishes"},
		{name: "Chart digest", value: supplied.ChartDigest, supplied: "the digest of the packaged chart"},
		{name: "Effective installation values", value: supplied.InstallationValues, supplied: "the values the qualified profile installs with"},
		{name: "Operating targets", value: supplied.OperatingTargets, supplied: "the latency, throughput and capacity limits the profile claims"},
		{name: "Recovery objectives", value: supplied.RecoveryObjectives, supplied: "the recovery point and time objectives, recorded separately for the database and for operator state"},
		{name: "Run evidence", value: supplied.RunEvidence, supplied: "the CI run and job identities the executed scenarios come from"},
	}
}
