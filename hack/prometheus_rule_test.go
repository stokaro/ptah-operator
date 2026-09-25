package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The rules are the chart's answer to "who is told when an Apply was never
// accounted for", and every way they can be wrong is silent. Rendered by
// default they break an install into a cluster without the Prometheus Operator
// CRDs. Rendered with a window nobody measured they page on a number this chart
// made up. Naming a metric the manager does not export, they evaluate to
// nothing forever and report themselves healthy. And an expression that reads
// correctly can still fire on a follower, stay quiet on a lost target, or page
// through every leader change.
//
// So the chart is rendered and read, and the rules it renders are run through
// Prometheus's own rule-test tool against the cases they exist for.

// The window every test below renders the rules with. The scenarios in
// testdata/prometheusrule are written against it.
const testViewUnsyncedFor = "10m"

var ruleMetricName = regexp.MustCompile(`\bptah_operator_[a-z_]+\b`)

// exportedMetricName matches the two ways the telemetry package names a family:
// the full name in a descriptor, and a namespace with a name in the options.
var exportedMetricName = regexp.MustCompile(`"(ptah_operator_[a-z_]+)"|Namespace:\s*"ptah_operator",\s*Name:\s*"([a-z_]+)"`)

func TestThePrometheusRuleIsAbsentUntilItIsAskedFor(t *testing.T) {
	t.Parallel()
	rendered, err := renderChartForMonitoring(t, nil)
	if err != nil {
		t.Fatalf("the default values do not render: %v\n%s", err, rendered)
	}
	if strings.Contains(rendered, "kind: PrometheusRule") {
		t.Error("the default render carries a PrometheusRule, so an install into a cluster without the Prometheus Operator CRDs fails on a kind the API server does not serve")
	}
}

// How long a manager takes to synchronize its view depends on the cluster it
// runs in, so the chart takes that number from whoever measured it and has none
// of its own.
func TestThePrometheusRuleRefusesToGuessTheSynchronizationWindow(t *testing.T) {
	t.Parallel()
	rendered, err := renderChartForMonitoring(t, []string{"monitoring.prometheusRule.enabled=true"})
	if err == nil {
		t.Fatalf("the rules rendered without viewUnsyncedFor, so the chart chose a window nobody measured:\n%s", rendered)
	}
	if !strings.Contains(rendered, "monitoring.prometheusRule.viewUnsyncedFor") {
		t.Errorf("the refusal does not name the value to supply: %s", rendered)
	}
}

func TestThePrometheusRuleNamesOnlyMetricsTheManagerExports(t *testing.T) {
	t.Parallel()
	rule := renderedPrometheusRuleWith(t, stateThresholds...)
	named := map[string]bool{}
	for _, name := range ruleMetricName.FindAllString(rule, -1) {
		named[name] = true
	}
	if len(named) == 0 {
		t.Fatal("the rules name no ptah_operator metric, so this check reads them and holds none of it")
	}
	exported := exportedMetricNames(t)
	var unknown []string
	for name := range named {
		if !exported[name] {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Errorf("the rules name metrics the manager does not export, so those alerts can never fire: %v", unknown)
	}
}

// The scenarios the rules exist for, run by promtool against the rules exactly
// as the chart renders them.
func TestThePrometheusRuleFiresOnWhatItIsFor(t *testing.T) {
	t.Parallel()
	runPromtool(t, renderedPrometheusRule(t), "unresolved.test.yaml")
}

// runPromtool writes the rendered rule groups beside one scenario file and runs
// promtool over them.
func runPromtool(t *testing.T, rule, scenarioFile string) {
	t.Helper()
	promtool := promtoolOrSkip(t)
	var document struct {
		Spec map[string]any `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(rule), &document); err != nil {
		t.Fatalf("the rendered PrometheusRule is not YAML: %v", err)
	}
	if len(document.Spec) == 0 {
		t.Fatal("the rendered PrometheusRule has no spec to test")
	}
	rules, err := yaml.Marshal(document.Spec)
	if err != nil {
		t.Fatalf("encode the rule groups: %v", err)
	}
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "rules.yaml"), rules, 0o600); err != nil {
		t.Fatal(err)
	}
	scenarios, err := os.ReadFile(repositoryFile(t, filepath.Join("hack/testdata/prometheusrule", scenarioFile)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, scenarioFile), scenarios, 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(promtool, "test", "rules", scenarioFile) //nolint:gosec // The binary is the one on PATH.
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("promtool test rules: %v\n%s", err, output)
	}
}

// stateThresholds are the values testdata/prometheusrule/state.test.yaml is
// written against.
var stateThresholds = []string{
	"monitoring.prometheusRule.overdueAfterSeconds=900",
	"monitoring.prometheusRule.operationStalledAfterSeconds=1800",
	"monitoring.prometheusRule.lockReleaseOwedFor=15m",
	"monitoring.prometheusRule.certificateExpiresWithinSeconds=604800",
	"monitoring.prometheusRule.failures.window=30m",
	"monitoring.prometheusRule.failures.count=20",
	"monitoring.prometheusRule.admissionFailingFor=5m",
}

// A threshold nobody set renders no rule. The state alerts are one number each
// about one cluster, and the chart has none of its own.
func TestAStateAlertRendersOnlyWhereItsThresholdIsSet(t *testing.T) {
	t.Parallel()
	rule := renderedPrometheusRule(t)
	for _, alert := range []string{
		"PtahOperatorResourceOverdue", "PtahOperatorOperationStalled", "PtahOperatorLockReleaseOwed",
		"PtahOperatorWebhookCertificateExpiring", "PtahOperatorOperationsFailing", "PtahOperatorAdmissionUnavailable",
	} {
		if strings.Contains(rule, alert) {
			t.Errorf("%s rendered with no threshold set:\n%s", alert, rule)
		}
	}
	rendered, err := renderChartForMonitoring(t, []string{
		"monitoring.prometheusRule.enabled=true",
		"monitoring.prometheusRule.viewUnsyncedFor=" + testViewUnsyncedFor,
		"monitoring.prometheusRule.failures.window=30m",
	})
	if err == nil || !strings.Contains(rendered, "failures.count") {
		t.Errorf("a failure window without a count rendered, or the refusal does not name the count:\n%s", rendered)
	}
}

// The state alerts against the cases they exist for.
func TestTheStateAlertsFireOnWhatTheyAreFor(t *testing.T) {
	t.Parallel()
	runPromtool(t, renderedPrometheusRuleWith(t, stateThresholds...), "state.test.yaml")
}

// renderedPrometheusRule is the one PrometheusRule the chart renders with the
// rules on and the window set.
func renderedPrometheusRule(t *testing.T) string {
	t.Helper()
	return renderedPrometheusRuleWith(t)
}

func renderedPrometheusRuleWith(t *testing.T, extra ...string) string {
	t.Helper()
	rendered, err := renderChartForMonitoring(t, append([]string{
		"monitoring.prometheusRule.enabled=true",
		"monitoring.prometheusRule.viewUnsyncedFor=" + testViewUnsyncedFor,
	}, extra...))
	if err != nil {
		t.Fatalf("the rules do not render: %v\n%s", err, rendered)
	}
	return documentOfKind(t, rendered, "PrometheusRule")
}

// promtoolOrSkip finds Prometheus's rule-test tool. CI installs the version
// support/tools.json pins and sets PTAH_REQUIRE_PROMTOOL, so there a missing
// binary fails rather than skipping: a skipped rule test reads exactly like a
// passing one.
func promtoolOrSkip(t *testing.T) string {
	t.Helper()
	promtool, err := exec.LookPath("promtool")
	if err == nil {
		return promtool
	}
	if os.Getenv("PTAH_REQUIRE_PROMTOOL") == "1" {
		t.Fatal("PTAH_REQUIRE_PROMTOOL is set and promtool is not on PATH")
	}
	t.Skip("promtool is required to run the alerting rules against their scenarios")
	return ""
}

// exportedMetricNames reads the names the telemetry package registers.
func exportedMetricNames(t *testing.T) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	directory := repositoryFile(t, filepath.Join("internal", "telemetry"))
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read %s: %v", directory, err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(directory, entry.Name())) //nolint:gosec // A path under the repository.
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		for _, match := range exportedMetricName.FindAllStringSubmatch(string(content), -1) {
			if match[1] != "" {
				names[match[1]] = true
			}
			if match[2] != "" {
				names["ptah_operator_"+match[2]] = true
			}
		}
	}
	if len(names) == 0 {
		t.Fatal("the telemetry package registers no ptah_operator metric this can read")
	}
	return names
}
