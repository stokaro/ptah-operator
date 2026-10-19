package main

import (
	"os/exec"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The ServiceMonitor is the chart's answer to "how is this scraped", and every
// way it can be wrong is silent. Rendered by default it breaks an install into a
// cluster without the Prometheus Operator CRDs. Rendered without its Service it
// selects nothing while reporting that monitoring is configured. Rendered with a
// pinned interval it decides a question the Prometheus configuration owns.
//
// So these render the chart and read what came out.

// requiredInstallValues are the four the chart refuses to render without, held
// to the same shapes the install page documents.
var requiredInstallValues = []string{
	"image.digest=sha256:" + strings.Repeat("a", 64),
	"execution.runnerImage=ghcr.io/stokaro/ptah-operator@sha256:" + strings.Repeat("a", 64),
	"execution.executorImage=ghcr.io/stokaro/ptah@sha256:" + strings.Repeat("b", 64),
	"execution.ptahVersion=v0.7.0",
}

func TestTheServiceMonitorIsAbsentUntilItIsAskedFor(t *testing.T) {
	t.Parallel()
	rendered, err := renderChartForMonitoring(t, nil)
	if err != nil {
		t.Fatalf("the default values do not render: %v\n%s", err, rendered)
	}
	if strings.Contains(rendered, "kind: ServiceMonitor") {
		t.Error("the default render carries a ServiceMonitor, so an install into a cluster without the Prometheus Operator CRDs fails on a kind the API server does not serve")
	}
}

// A ServiceMonitor selects Services by the Services' own labels, not by the
// Pods behind them. The webhook, result and metrics Services all select the
// manager Pods, so the selector has to name what only the metrics Service
// carries. v0.2.4 shipped a selector of Pod labels, which matched no Service:
// Prometheus had no target, and only a deployment check found it.
func TestTheServiceMonitorSelectsTheMetricsServiceAndNoOther(t *testing.T) {
	t.Parallel()
	rendered, err := renderChartForMonitoring(t, []string{
		"monitoring.serviceMonitor.enabled=true",
		"monitoring.serviceMonitor.labels.release=kube-prometheus-stack",
	})
	if err != nil {
		t.Fatalf("enabling the ServiceMonitor does not render: %v\n%s", err, rendered)
	}
	monitor := documentOfKind(t, rendered, "ServiceMonitor")
	var parsed struct {
		Spec struct {
			Selector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"selector"`
			Endpoints []struct {
				Port string `json:"port"`
			} `json:"endpoints"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(monitor), &parsed); err != nil {
		t.Fatalf("the ServiceMonitor does not parse: %v\n%s", err, monitor)
	}
	selector := parsed.Spec.Selector.MatchLabels
	if len(selector) == 0 {
		t.Fatalf("the ServiceMonitor selects every Service in the namespace:\n%s", monitor)
	}
	services := renderedServices(t, rendered)
	selected := 0
	for name, labels := range services {
		if !selects(selector, labels) {
			continue
		}
		selected++
		if !strings.HasSuffix(name, "-metrics") {
			t.Errorf("the ServiceMonitor selects Service %s, which is not the metrics Service", name)
		}
	}
	if selected != 1 {
		t.Errorf("the ServiceMonitor selects %d of the %d rendered Services, want the metrics Service alone; selector %v", selected, len(services), selector)
	}
	if len(parsed.Spec.Endpoints) != 1 || parsed.Spec.Endpoints[0].Port != "metrics" {
		t.Errorf("the ServiceMonitor does not scrape the port named metrics:\n%s", monitor)
	}
	if !strings.Contains(monitor, "release: kube-prometheus-stack") { // what a Prometheus selects the object by
		t.Errorf("the ServiceMonitor does not carry the labels it was given:\n%s", monitor)
	}
	// An interval nobody asked for is a decision about somebody else's
	// Prometheus, so an unset one renders nothing at all.
	for _, absent := range []string{"interval:", "scrapeTimeout:"} {
		if strings.Contains(monitor, absent) {
			t.Errorf("the ServiceMonitor pins %s without being asked to:\n%s", absent, monitor)
		}
	}
}

// The selector shipped in v0.2.4 must read as selecting nothing, and the
// metrics Service's labels as selected, or the test above proves nothing.
func TestSelectsRefusesTheSelectorThatShipped(t *testing.T) {
	t.Parallel()
	shipped := map[string]string{"app.kubernetes.io/name": "ptah-operator", "app.kubernetes.io/instance": "ptah-operator", "app.kubernetes.io/component": "controller"}
	metricsService := map[string]string{"app.kubernetes.io/name": "ptah-operator", "app.kubernetes.io/instance": "ptah-operator", "app.kubernetes.io/managed-by": "Helm"}
	if selects(shipped, metricsService) {
		t.Error("a selector naming a label the Service lacks was read as selecting it")
	}
	if !selects(map[string]string{"app.kubernetes.io/name": "ptah-operator"}, metricsService) {
		t.Error("a selector the Service's labels contain was read as not selecting it")
	}
}

// selects reports whether a matchLabels selector selects an object with these labels.
func selects(selector, labels map[string]string) bool {
	for key, value := range selector {
		if labels[key] != value {
			return false
		}
	}
	return true
}

// renderedServices returns every rendered Service's labels by name.
func renderedServices(t *testing.T, rendered string) map[string]map[string]string {
	t.Helper()
	services := map[string]map[string]string{}
	for _, document := range strings.Split(rendered, "\n---\n") {
		if !strings.Contains(document, "kind: Service\n") {
			continue
		}
		var service struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(document), &service); err != nil {
			t.Fatalf("a rendered Service does not parse: %v\n%s", err, document)
		}
		services[service.Metadata.Name] = service.Metadata.Labels
	}
	if len(services) < 2 {
		t.Fatalf("the render holds %d Services; the test needs the metrics Service and at least one other", len(services))
	}
	return services
}

func TestTheServiceMonitorCarriesTheScrapeTimingWhenItIsGiven(t *testing.T) {
	t.Parallel()
	rendered, err := renderChartForMonitoring(t, []string{
		"monitoring.serviceMonitor.enabled=true",
		"monitoring.serviceMonitor.interval=30s",
		"monitoring.serviceMonitor.scrapeTimeout=10s",
	})
	if err != nil {
		t.Fatalf("scrape timing does not render: %v\n%s", err, rendered)
	}
	monitor := documentOfKind(t, rendered, "ServiceMonitor")
	for _, wanted := range []string{`interval: "30s"`, `scrapeTimeout: "10s"`} {
		if !strings.Contains(monitor, wanted) {
			t.Errorf("the ServiceMonitor does not carry %s:\n%s", wanted, monitor)
		}
	}
}

// A ServiceMonitor without the Service it selects scrapes nothing, and nothing
// about the rendered object says so. The chart refuses the combination instead.
func TestTheServiceMonitorRefusesToMonitorAServiceThatIsNotThere(t *testing.T) {
	t.Parallel()
	rendered, err := renderChartForMonitoring(t, []string{
		"monitoring.serviceMonitor.enabled=true",
		"metrics.service.enabled=false",
	})
	if err == nil {
		t.Fatalf("a ServiceMonitor rendered without its Service:\n%s", rendered)
	}
	if !strings.Contains(rendered, "metrics.service.enabled") {
		t.Errorf("the refusal does not name what has to be turned on: %s", rendered)
	}
}

// documentOfKind returns the one rendered document of that kind.
func documentOfKind(t *testing.T, rendered, kind string) string {
	t.Helper()
	var found []string
	for _, document := range strings.Split(rendered, "\n---\n") {
		if strings.Contains(document, "kind: "+kind+"\n") {
			found = append(found, document)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the render holds %d documents of kind %s, want exactly one", len(found), kind)
	}
	return found[0]
}

func renderChartForMonitoring(t *testing.T, values []string) (string, error) {
	t.Helper()
	helm := helmOrSkip(t)
	arguments := []string{"template", "ptah-operator", repositoryFile(t, "charts/ptah-operator"), "--namespace", "ptah-system"}
	for _, value := range requiredInstallValues {
		arguments = append(arguments, "--set-string", value)
	}
	// --set rather than --set-string for the values under test: the schema types
	// enabled as a boolean, and --set-string would hand it the string "true".
	for _, value := range values {
		arguments = append(arguments, "--set", value)
	}
	output, err := exec.Command(helm, arguments...).CombinedOutput() //nolint:gosec // Arguments are read from the repository.
	return string(output), err
}
