package e2e

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// alChart is the chart the phase renders its rules from.
var alChart = filepath.Join("..", "..", "charts", "ptah-operator")

// alHelm runs helm offline against the chart in this repository. The rules
// and the runbook base are read from what the chart renders, so the tests
// that hold their readers need the renderer; without helm they cannot say
// anything about the chart and skip, as the chart's own tests under hack/ do.
func alHelm(t *testing.T, arguments ...string) string {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is required to render the chart the alerting phase reads its rules from")
	}
	output, err := exec.Command(helm, arguments...).Output() //nolint:gosec // Arguments are this test's own.
	if err != nil {
		t.Fatalf("helm %v: %v", arguments, err)
	}
	return string(output)
}

// alRenderedRule renders the chart's PrometheusRule the way the phase does:
// the monitoring switches it needs laid over the values an install requires.
func alRenderedRule(t *testing.T) string {
	t.Helper()
	return alHelm(t, "template", "ptah-operator", alChart, "--namespace", "ptah-system",
		"--set-string", "image.digest=sha256:"+strings.Repeat("a", 64),
		"--set-string", "execution.runnerImage=ghcr.io/stokaro/ptah-operator@sha256:"+strings.Repeat("a", 64),
		"--set-string", "execution.executorImage=ghcr.io/stokaro/ptah@sha256:"+strings.Repeat("b", 64),
		"--set-string", "execution.ptahVersion=v0.7.0",
		"--set", "monitoring.prometheusRule.enabled=true",
		"--set", "monitoring.prometheusRule.viewUnsyncedFor=60s",
		"--set", "monitoring.prometheusRule.operationStalledAfterSeconds=60",
		"--set", "monitoring.prometheusRule.certificateExpiresWithinSeconds=86400",
		"--set", "monitoring.prometheusRule.admissionFailingFor=300s",
		"--show-only", "templates/prometheusrule.yaml")
}

// The rule file is the PrometheusRule's spec, and nothing is lost or added on
// the way: read back as YAML, it is the spec the chart rendered.
func TestAlRuleFileIsTheRenderedSpec(t *testing.T) {
	t.Parallel()
	rendered := alRenderedRule(t)
	rules, err := alRuleFile(rendered)
	if err != nil {
		t.Fatal(err)
	}
	if first, _, _ := strings.Cut(rules, "\n"); first != "groups:" {
		t.Fatalf("the rule file starts with %q", first)
	}
	var document struct {
		Spec map[string]any `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(rendered), &document); err != nil {
		t.Fatal(err)
	}
	var file map[string]any
	if err := yaml.Unmarshal([]byte(rules), &file); err != nil {
		t.Fatalf("the rule file is not YAML: %v", err)
	}
	if !reflect.DeepEqual(file, document.Spec) {
		t.Fatalf("the rule file is not the rendered spec:\n%s", rules)
	}
	groups, _ := file["groups"].([]any)
	if len(groups) == 0 {
		t.Fatal("the rule file carries no group")
	}
}

func TestAlRuleFileRefusals(t *testing.T) {
	t.Parallel()
	rendered := alRenderedRule(t)
	if _, err := alRuleFile(rendered); err != nil {
		t.Fatalf("the chart's rendering was refused: %v", err)
	}
	for name, mutate := range map[string]func(string) string{
		"no spec":       func(r string) string { return strings.Replace(r, "\nspec:\n", "\nspecification:\n", 1) },
		"indented spec": func(r string) string { return strings.Replace(r, "\nspec:\n", "\n  spec:\n", 1) },
		"no groups first": func(r string) string {
			return strings.Replace(r, "\nspec:\n  groups:\n", "\nspec:\n  rules:\n", 1)
		},
		"nothing under spec": func(r string) string { before, _, _ := strings.Cut(r, "\nspec:\n"); return before + "\nspec:" },
		"no unresolved alert": func(r string) string {
			return strings.Replace(r, "alert: "+alUnresolvedApply+"\n", "alert: Renamed\n", 1)
		},
		"no view alert": func(r string) string {
			return strings.Replace(r, "alert: "+alViewNotSynced+"\n", "alert: Renamed\n", 1)
		},
		"no stalled alert": func(r string) string {
			return strings.Replace(r, "alert: "+alOperationStall+"\n", "alert: Renamed\n", 1)
		},
		"no certificate alert": func(r string) string {
			return strings.Replace(r, "alert: "+alCertificateAlert+"\n", "alert: Renamed\n", 1)
		},
		"no read-failure alert": func(r string) string {
			return strings.Replace(r, "alert: "+alViewReadAlert+"\n", "alert: Renamed\n", 1)
		},
		"no admission alert": func(r string) string {
			return strings.Replace(r, "alert: "+alAdmissionAlert+"\n", "alert: Renamed\n", 1)
		},
		// The stalled rule renders only where its threshold is set.
		"a name only as a prefix": func(r string) string {
			return strings.Replace(r, "alert: "+alOperationStall+"\n", "alert: "+alOperationStall+"Soon\n", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mutated := mutate(rendered)
			if mutated == rendered {
				t.Fatal("the mutation changed nothing")
			}
			if _, err := alRuleFile(mutated); err == nil {
				t.Fatal("the rendering was accepted")
			}
		})
	}
}

// The runbook base is the one the chart's values name.
func TestAlRunbookBaseIsTheChartValue(t *testing.T) {
	t.Parallel()
	values := alHelm(t, "show", "values", alChart)
	base, err := alRunbookBase(values)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Monitoring struct {
			PrometheusRule struct {
				RunbookBaseURL string `json:"runbookBaseURL"`
			} `json:"prometheusRule"`
		} `json:"monitoring"`
	}
	if err := yaml.Unmarshal([]byte(values), &parsed); err != nil {
		t.Fatal(err)
	}
	if want := parsed.Monitoring.PrometheusRule.RunbookBaseURL; want == "" || base != want {
		t.Fatalf("the runbook base read as %q; the values name %q", base, want)
	}
}

func TestAlRunbookBaseRefusals(t *testing.T) {
	t.Parallel()
	line := `    runbookBaseURL: "https://example.invalid/operations/"`
	if base, err := alRunbookBase("monitoring:\n" + line + "\n"); err != nil || base != "https://example.invalid/operations/" {
		t.Fatalf("alRunbookBase = %q, %v", base, err)
	}
	for name, values := range map[string]string{
		"none":     "monitoring:\n  enabled: true\n",
		"empty":    "    runbookBaseURL: \"\"\n",
		"unquoted": "    runbookBaseURL: https://example.invalid/\n",
		"twice":    line + "\n" + line + "\n",
		"trailing": line + " # a comment\n",
	} {
		if base, err := alRunbookBase(values); err == nil {
			t.Errorf("%s: read %q", name, base)
		}
	}
}

func TestAlPrometheusConfig(t *testing.T) {
	t.Parallel()
	var config struct {
		Global struct {
			ScrapeInterval     string            `json:"scrape_interval"`
			ScrapeTimeout      string            `json:"scrape_timeout"`
			EvaluationInterval string            `json:"evaluation_interval"`
			ExternalLabels     map[string]string `json:"external_labels"`
		} `json:"global"`
		RuleFiles []string `json:"rule_files"`
		Alerting  struct {
			Alertmanagers []struct {
				StaticConfigs []struct {
					Targets []string `json:"targets"`
				} `json:"static_configs"`
			} `json:"alertmanagers"`
		} `json:"alerting"`
		ScrapeConfigs []struct {
			JobName     string `json:"job_name"`
			Discoveries []struct {
				Role       string `json:"role"`
				Namespaces struct {
					Names []string `json:"names"`
				} `json:"namespaces"`
			} `json:"kubernetes_sd_configs"`
			Relabel []struct {
				SourceLabels []string `json:"source_labels"`
				Regex        string   `json:"regex"`
				Action       string   `json:"action"`
				TargetLabel  string   `json:"target_label"`
			} `json:"relabel_configs"`
		} `json:"scrape_configs"`
	}
	if err := yaml.UnmarshalStrict([]byte(alPrometheusConfig("monitoring", "operator", "ptah-metrics")), &config); err != nil {
		t.Fatal(err)
	}
	if config.Global.ScrapeInterval != "5s" || config.Global.ScrapeTimeout != "4s" || config.Global.EvaluationInterval != "5s" ||
		!reflect.DeepEqual(config.Global.ExternalLabels, map[string]string{"operator_namespace": "operator", "operator_metrics_service": "ptah-metrics"}) {
		t.Errorf("global = %+v", config.Global)
	}
	if !reflect.DeepEqual(config.RuleFiles, []string{"/etc/prometheus/rules.yaml"}) {
		t.Errorf("rule_files = %v", config.RuleFiles)
	}
	if len(config.Alerting.Alertmanagers) != 1 || len(config.Alerting.Alertmanagers[0].StaticConfigs) != 1 ||
		!reflect.DeepEqual(config.Alerting.Alertmanagers[0].StaticConfigs[0].Targets, []string{"alertmanager.monitoring.svc:9093"}) {
		t.Errorf("alerting = %+v", config.Alerting)
	}
	if len(config.ScrapeConfigs) != 1 {
		t.Fatalf("scrape_configs = %+v", config.ScrapeConfigs)
	}
	scrape := config.ScrapeConfigs[0]
	if scrape.JobName != alScrapeJob || len(scrape.Discoveries) != 1 || scrape.Discoveries[0].Role != "endpointslice" ||
		!reflect.DeepEqual(scrape.Discoveries[0].Namespaces.Names, []string{"operator"}) {
		t.Errorf("scrape = %+v", scrape)
	}
	type relabel = struct {
		SourceLabels []string `json:"source_labels"`
		Regex        string   `json:"regex"`
		Action       string   `json:"action"`
		TargetLabel  string   `json:"target_label"`
	}
	want := []relabel{
		{SourceLabels: []string{"__meta_kubernetes_service_name"}, Regex: "ptah-metrics", Action: "keep"},
		{SourceLabels: []string{"__meta_kubernetes_endpointslice_port_name"}, Regex: "metrics", Action: "keep"},
		{SourceLabels: []string{"__meta_kubernetes_pod_name"}, TargetLabel: "pod"},
	}
	if !reflect.DeepEqual(scrape.Relabel, want) {
		t.Errorf("relabel_configs = %+v", scrape.Relabel)
	}
}

func TestAlAlertmanagerConfig(t *testing.T) {
	t.Parallel()
	var config struct {
		Route struct {
			Receiver       string   `json:"receiver"`
			GroupBy        []string `json:"group_by"`
			GroupWait      string   `json:"group_wait"`
			GroupInterval  string   `json:"group_interval"`
			RepeatInterval string   `json:"repeat_interval"`
		} `json:"route"`
		Receivers []struct {
			Name     string `json:"name"`
			Webhooks []struct {
				URL          string `json:"url"`
				SendResolved bool   `json:"send_resolved"`
			} `json:"webhook_configs"`
		} `json:"receivers"`
	}
	if err := yaml.UnmarshalStrict([]byte(alAlertmanagerConfig("monitoring")), &config); err != nil {
		t.Fatal(err)
	}
	route := config.Route
	if route.Receiver != "sink" || !reflect.DeepEqual(route.GroupBy, []string{"operator_namespace", "operator_metrics_service", "alertname", "family", "operation"}) ||
		route.GroupWait != "5s" || route.GroupInterval != "10s" || route.RepeatInterval != "1h" {
		t.Errorf("route = %+v", route)
	}
	if len(config.Receivers) != 1 || config.Receivers[0].Name != "sink" || len(config.Receivers[0].Webhooks) != 1 ||
		config.Receivers[0].Webhooks[0].URL != "http://alert-sink.monitoring.svc:8080/alerts" ||
		!config.Receivers[0].Webhooks[0].SendResolved {
		t.Errorf("receivers = %+v", config.Receivers)
	}
}

func TestAlWorkload(t *testing.T) {
	t.Parallel()
	sink, sinkService := alWorkload{
		namespace: "monitoring", name: "alert-sink", image: "registry/fixture@sha256:1", port: 8080,
		args: []string{"-listen", ":8080"}, command: []string{"/e2e-alert-sink"},
		serviceAccount: "default", pullSecret: alPullSecret,
	}.objects()
	pod := sink.Spec.Template.Spec
	container := pod.Containers[0]
	if *sink.Spec.Replicas != 1 || !reflect.DeepEqual(sink.Spec.Selector.MatchLabels, map[string]string{"app": "alert-sink"}) ||
		!reflect.DeepEqual(sink.Spec.Template.Labels, map[string]string{"app": "alert-sink"}) {
		t.Errorf("the sink Deployment selects %+v", sink.Spec)
	}
	if *pod.AutomountServiceAccountToken || pod.ServiceAccountName != "default" {
		t.Error("the sink, which runs as the default ServiceAccount, is given a token")
	}
	if !reflect.DeepEqual(container.Command, []string{"/e2e-alert-sink"}) || !reflect.DeepEqual(container.Args, []string{"-listen", ":8080"}) {
		t.Errorf("the sink runs %v %v", container.Command, container.Args)
	}
	if len(pod.Volumes) != 1 || pod.Volumes[0].EmptyDir == nil || len(container.VolumeMounts) != 1 ||
		container.VolumeMounts[0].MountPath != "/data" {
		t.Errorf("the sink, which has no configuration, mounts %+v", container.VolumeMounts)
	}
	if !reflect.DeepEqual(pod.ImagePullSecrets, []corev1.LocalObjectReference{{Name: alPullSecret}}) {
		t.Errorf("the sink pulls with %v", pod.ImagePullSecrets)
	}
	security := pod.SecurityContext
	if !*security.RunAsNonRoot || *security.RunAsUser != 65534 || *security.RunAsGroup != 65534 || *security.FSGroup != 65534 ||
		security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("the sink Pod runs with %+v", security)
	}
	restricted := container.SecurityContext
	if *restricted.AllowPrivilegeEscalation || !*restricted.ReadOnlyRootFilesystem ||
		!reflect.DeepEqual(restricted.Capabilities.Drop, []corev1.Capability{"ALL"}) {
		t.Errorf("the sink container runs with %+v", restricted)
	}
	if container.ReadinessProbe.TCPSocket.Port.IntVal != 8080 || container.ReadinessProbe.PeriodSeconds != 2 ||
		!reflect.DeepEqual(container.Ports, []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}}) {
		t.Errorf("the sink is probed at %+v and listens on %+v", container.ReadinessProbe, container.Ports)
	}
	if !reflect.DeepEqual(sinkService.Spec.Selector, map[string]string{"app": "alert-sink"}) || len(sinkService.Spec.Ports) != 1 ||
		sinkService.Spec.Ports[0].Name != "http" || sinkService.Spec.Ports[0].Port != 8080 ||
		sinkService.Spec.Ports[0].TargetPort.IntVal != 8080 {
		t.Errorf("the sink Service is %+v", sinkService.Spec)
	}

	prometheus, _ := alWorkload{
		namespace: "monitoring", name: "prometheus", image: "registry/prometheus@sha256:1", port: 9090,
		args:           []string{"--config.file=/etc/prometheus/prometheus.yml", "--storage.tsdb.path=/data"},
		serviceAccount: "prometheus", configMap: "prometheus", pullSecret: alPullSecret,
	}.objects()
	pod = prometheus.Spec.Template.Spec
	if !*pod.AutomountServiceAccountToken || pod.ServiceAccountName != "prometheus" {
		t.Error("Prometheus, which discovers the managers as its own ServiceAccount, is given no token")
	}
	if pod.Containers[0].Command != nil {
		t.Errorf("Prometheus replaces its entrypoint with %v", pod.Containers[0].Command)
	}
	mounts := pod.Containers[0].VolumeMounts
	if len(mounts) != 2 || mounts[1].Name != "config" || mounts[1].MountPath != "/etc/prometheus" ||
		len(pod.Volumes) != 2 || pod.Volumes[1].ConfigMap == nil || pod.Volumes[1].ConfigMap.Name != "prometheus" {
		t.Errorf("Prometheus mounts %+v from %+v", mounts, pod.Volumes)
	}
}

func TestAlPullSecretFor(t *testing.T) {
	t.Parallel()
	secret := alPullSecretFor("monitoring", "registry.invalid:5000", "ptah", "pw")
	if secret.Namespace != "monitoring" || secret.Name != alPullSecret || secret.Type != corev1.SecretTypeDockerConfigJson ||
		len(secret.Data) != 1 {
		t.Fatalf("the pull Secret is %+v", secret.ObjectMeta)
	}
	var config struct {
		Auths map[string]struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Auth     string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(secret.Data[corev1.DockerConfigJsonKey], &config); err != nil {
		t.Fatal(err)
	}
	entry, ok := config.Auths["registry.invalid:5000"]
	if !ok || len(config.Auths) != 1 || entry.Username != "ptah" || entry.Password != "pw" ||
		entry.Auth != base64.StdEncoding.EncodeToString([]byte("ptah:pw")) {
		t.Fatalf("the pull credential is %+v", config)
	}
}

func TestAlDiscoveryRBAC(t *testing.T) {
	t.Parallel()
	role, binding := alDiscoveryRBAC("operator", "monitoring")
	if role.Namespace != "operator" || role.Name != alDiscoveryRole || len(role.Rules) != 2 {
		t.Fatalf("the Role is %+v", role)
	}
	read := []string{"get", "list", "watch"}
	if !reflect.DeepEqual(role.Rules[0].APIGroups, []string{""}) || !reflect.DeepEqual(role.Rules[0].Resources, []string{"pods", "services"}) ||
		!reflect.DeepEqual(role.Rules[0].Verbs, read) ||
		!reflect.DeepEqual(role.Rules[1].APIGroups, []string{"discovery.k8s.io"}) ||
		!reflect.DeepEqual(role.Rules[1].Resources, []string{"endpointslices"}) || !reflect.DeepEqual(role.Rules[1].Verbs, read) {
		t.Errorf("the Role grants %+v", role.Rules)
	}
	if binding.Namespace != "operator" || binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != alDiscoveryRole ||
		binding.RoleRef.APIGroup != "rbac.authorization.k8s.io" || len(binding.Subjects) != 1 ||
		binding.Subjects[0].Kind != "ServiceAccount" || binding.Subjects[0].Namespace != "monitoring" ||
		binding.Subjects[0].Name != "prometheus" {
		t.Errorf("the RoleBinding is %+v", binding)
	}
}

// The held schema names only fields the API has: read strictly into the type,
// nothing is left over, and the gate is the one node label no node carries.
func TestAlHeldSchema(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(alHeldSchema("held"))
	if err != nil {
		t.Fatal(err)
	}
	var schema ptahv1alpha1.PtahSchema
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&schema); err != nil {
		t.Fatalf("the held schema names a field the API does not have: %v", err)
	}
	spec := schema.Spec
	if schema.Namespace != "held" || schema.Name != alStalledSchema || spec.Target.Engine != "PostgreSQL" ||
		spec.Target.URLFrom.Name != alDatabaseURLSecret || spec.Desired.VerificationPolicyFrom.Name != alPolicyConfigMap ||
		!spec.Desired.Transport.PlainHTTP || spec.Interval.Duration != 24*time.Hour {
		t.Errorf("the held schema is %+v", schema)
	}
	execution := spec.Execution
	if !reflect.DeepEqual(execution.NodeSelector, map[string]string{alGateLabel: "open"}) ||
		execution.FailureRetryInterval.Duration != time.Hour || execution.ActiveDeadlineSeconds != 600 ||
		execution.ServiceAccountName != "default" ||
		!reflect.DeepEqual(execution.ImagePullSecrets, []corev1.LocalObjectReference{{Name: alPullSecret}}) {
		t.Errorf("the held schema executes as %+v", execution)
	}
}

// alSinkLine is one line as test/e2e/alertsink writes it.
func alSinkLine(t *testing.T, status, alertName string, labels map[string]string, received time.Time) string {
	t.Helper()
	all := map[string]string{"alertname": alertName}
	for key, value := range labels {
		all[key] = value
	}
	encoded, err := json.Marshal(map[string]any{
		"receiver": "ptah", "status": status, "alertname": alertName, "labels": all,
		"annotations": map[string]string{"summary": "1 migration resources carry an Apply nobody accounted for"},
		"startsAt":    "2026-09-28T10:00:00Z", "endsAt": "0001-01-01T00:00:00Z", "receivedAt": received.Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestAlDeliveries(t *testing.T) {
	t.Parallel()
	received := time.Date(2026, 9, 28, 10, 1, 0, 123456789, time.UTC)
	log := strings.Join([]string{
		"2026/09/28 10:00:00 alertsink: listening on :8080",
		alSinkLine(t, "firing", alUnresolvedApply, map[string]string{"family": "migration"}, received),
		alSinkLine(t, "resolved", alOperationStall, map[string]string{"family": "schema", "operation": "Resolve"}, received),
		"",
	}, "\n")
	deliveries, err := alDeliveries([]byte(log))
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 2 {
		t.Fatalf("read %d deliveries, want the two object lines", len(deliveries))
	}
	first := deliveries[0]
	if first.Status != "firing" || first.AlertName != alUnresolvedApply || first.Labels["family"] != "migration" ||
		!first.ReceivedAt.Equal(received) || first.Annotations["summary"] == "" {
		t.Errorf("the first delivery read as %+v", first)
	}
	if deliveries[1].Status != "resolved" || deliveries[1].Labels["operation"] != "Resolve" {
		t.Errorf("the second delivery read as %+v", deliveries[1])
	}
	if empty, err := alDeliveries(nil); err != nil || len(empty) != 0 {
		t.Errorf("an empty log read as %v, %v", empty, err)
	}
	if _, err := alDeliveries([]byte("{not a delivery\n")); err == nil {
		t.Error("an object line that is not a delivery was read")
	}
	if _, err := alDeliveries([]byte(`{"receivedAt": "yesterday"}` + "\n")); err == nil {
		t.Error("a delivery with an unreadable instant was read")
	}
}

func TestAlMatch(t *testing.T) {
	t.Parallel()
	match := alMatch{status: "firing", alertName: alOperationStall, labels: map[string]string{"family": "schema", "operation": "Resolve"}}
	delivery := func() alDelivery {
		return alDelivery{Status: "firing", AlertName: alOperationStall, Labels: map[string]string{
			"alertname": alOperationStall, "family": "schema", "operation": "Resolve", "severity": "warning",
		}}
	}
	if !match.matches(delivery()) {
		t.Fatal("the delivery was not matched")
	}
	for name, mutate := range map[string]func(*alDelivery){
		"resolved":          func(d *alDelivery) { d.Status = "resolved" },
		"another alert":     func(d *alDelivery) { d.AlertName = alViewNotSynced },
		"another family":    func(d *alDelivery) { d.Labels["family"] = "migration" },
		"another operation": func(d *alDelivery) { d.Labels["operation"] = "Apply" },
		"no operation":      func(d *alDelivery) { delete(d.Labels, "operation") },
		"no labels":         func(d *alDelivery) { d.Labels = nil },
	} {
		candidate := delivery()
		mutate(&candidate)
		if match.matches(candidate) {
			t.Errorf("%s: matched", name)
		}
	}
	// A match that names no label takes the alert whatever it carries.
	if !(alMatch{status: "firing", alertName: alViewNotSynced}).matches(alDelivery{Status: "firing", AlertName: alViewNotSynced}) {
		t.Error("a delivery with no labels was not matched by a match that names none")
	}
}

func TestAlFirstDelivery(t *testing.T) {
	t.Parallel()
	firing := alMatch{status: "firing", alertName: alViewNotSynced}
	deliveries := []alDelivery{
		{Status: "firing", AlertName: alViewNotSynced, raw: "old"},
		{Status: "resolved", AlertName: alViewNotSynced, raw: "old resolution"},
		{Status: "firing", AlertName: alUnresolvedApply, raw: "another alert"},
		{Status: "firing", AlertName: alViewNotSynced, raw: "new"},
	}
	if got, index, ok := alFirstDelivery(deliveries, 0, firing); !ok || index != 0 || got.raw != "old" {
		t.Errorf("from the start: %q at %d, %t", got.raw, index, ok)
	}
	// A delivery logged before the event cannot be the event's.
	if got, index, ok := alFirstDelivery(deliveries, 1, firing); !ok || index != 3 || got.raw != "new" {
		t.Errorf("after the first: %q at %d, %t", got.raw, index, ok)
	}
	if _, _, ok := alFirstDelivery(deliveries, 4, firing); ok {
		t.Error("a delivery was found past the end of the log")
	}
	if _, _, ok := alFirstDelivery(nil, 0, firing); ok {
		t.Error("a delivery was found in an empty log")
	}
}

func alTargetsBody(t *testing.T, targets ...alTarget) []byte {
	t.Helper()
	var body alTargets
	body.Status = "success"
	body.Data.ActiveTargets = targets
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestAlTargetsReady(t *testing.T) {
	t.Parallel()
	manager := func(pod, health string) alTarget {
		return alTarget{Labels: map[string]string{"job": alScrapeJob, "pod": pod}, Health: health}
	}
	other := alTarget{Labels: map[string]string{"job": "kubelet"}, Health: "down"}
	if !alTargetsReady(alTargetsBody(t, manager("a", "up"), manager("b", "up"), other), 2) {
		t.Fatal("two manager targets up, with another job's target down beside them, were refused")
	}
	for name, body := range map[string][]byte{
		"one replica of two":       alTargetsBody(t, manager("a", "up")),
		"one target down":          alTargetsBody(t, manager("a", "up"), manager("b", "down")),
		"one target unknown":       alTargetsBody(t, manager("a", "up"), manager("b", "unknown")),
		"a third target":           alTargetsBody(t, manager("a", "up"), manager("b", "up"), manager("c", "up")),
		"another job counted":      alTargetsBody(t, manager("a", "up"), alTarget{Labels: map[string]string{"job": "kubelet"}, Health: "up"}),
		"no job label":             alTargetsBody(t, manager("a", "up"), alTarget{Labels: map[string]string{"pod": "b"}, Health: "up"}),
		"no targets":               alTargetsBody(t),
		"no data":                  []byte(`{"status":"success"}`),
		"not JSON":                 []byte(`<html>502</html>`),
		"activeTargets not a list": []byte(`{"data":{"activeTargets":{}}}`),
	} {
		if alTargetsReady(body, 2) {
			t.Errorf("%s: ready", name)
		}
	}
	// jq's length == 0 and all() over nothing: a release asking for no
	// replica would pass here, which is why the phase refuses one first.
	if !alTargetsReady(alTargetsBody(t), 0) {
		t.Error("no targets for no replicas was refused, where jq accepts it")
	}
}

func TestAlRulesLoaded(t *testing.T) {
	t.Parallel()
	body := func(rules ...string) []byte {
		entries := []string{`{"type":"alerting","name":"` + alViewReadAlert + `"}`}
		for index := 0; index+1 < len(rules); index += 2 {
			entries = append(entries, `{"type":"`+rules[index]+`","name":"`+rules[index+1]+`"}`)
		}
		return []byte(`{"status":"success","data":{"groups":[{"rules":[` + strings.Join(entries, ",") + `]}]}}`)
	}
	if !alRulesLoaded(body("alerting", alUnresolvedApply, "alerting", alViewNotSynced, "alerting", alOperationStall, "alerting", alCertificateAlert, "alerting", alAdmissionAlert)) {
		t.Fatal("the chart's rules were not recognized")
	}
	for name, answer := range map[string][]byte{
		"no read-failure rule":       []byte(strings.ReplaceAll(string(body("alerting", alUnresolvedApply, "alerting", alViewNotSynced, "alerting", alOperationStall, "alerting", alCertificateAlert, "alerting", alAdmissionAlert)), alViewReadAlert, "Renamed")),
		"no admission rule":          body("alerting", alUnresolvedApply, "alerting", alViewNotSynced, "alerting", alOperationStall, "alerting", alCertificateAlert),
		"no certificate rule":        body("alerting", alUnresolvedApply, "alerting", alViewNotSynced, "alerting", alOperationStall, "alerting", alAdmissionAlert),
		"no view rule":               body("alerting", alUnresolvedApply, "alerting", alOperationStall, "alerting", alCertificateAlert, "alerting", alAdmissionAlert),
		"no unresolved rule":         body("alerting", alViewNotSynced, "alerting", alOperationStall, "alerting", alCertificateAlert, "alerting", alAdmissionAlert),
		"no stalled rule":            body("alerting", alUnresolvedApply, "alerting", alViewNotSynced, "alerting", alCertificateAlert, "alerting", alAdmissionAlert),
		"the unresolved one records": body("recording", alUnresolvedApply, "alerting", alViewNotSynced, "alerting", alOperationStall, "alerting", alCertificateAlert, "alerting", alAdmissionAlert),
		"no groups":                  []byte(`{"status":"success","data":{"groups":[]}}`),
		"not JSON":                   []byte(`502 Bad Gateway`),
	} {
		if alRulesLoaded(answer) {
			t.Errorf("%s: loaded", name)
		}
	}
	split := []byte(`{"data":{"groups":[{"rules":[{"type":"alerting","name":"` + alUnresolvedApply +
		`"}]},{"rules":[{"type":"alerting","name":"` + alOperationStall + `"},{"type":"alerting","name":"` + alViewNotSynced + `"},{"type":"alerting","name":"` + alCertificateAlert + `"},{"type":"alerting","name":"` + alAdmissionAlert + `"},{"type":"alerting","name":"` + alViewReadAlert + `"}]}]}}`)
	if !alRulesLoaded(split) {
		t.Error("the rules in two groups were not recognized")
	}
}

func TestAlNoActiveAlerts(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"an empty vector": `{"status":"success","data":{"resultType":"vector","result":[]}}`,
		// jq's length of null is zero.
		"no result": `{"status":"success","data":{"resultType":"vector"}}`,
	} {
		if none, err := alNoActiveAlerts([]byte(body)); err != nil || !none {
			t.Errorf("%s: %t, %v", name, none, err)
		}
	}
	if none, err := alNoActiveAlerts([]byte(`{"status":"success","data":{"result":[{"metric":{"alertstate":"firing"}}]}}`)); err != nil || none {
		t.Errorf("an active alert read as none: %t, %v", none, err)
	}
	for name, body := range map[string]string{
		"an error":  `{"status":"error","errorType":"bad_data","error":"parse error"}`,
		"no status": `{"data":{"result":[]}}`,
		"not JSON":  `502 Bad Gateway`,
	} {
		if _, err := alNoActiveAlerts([]byte(body)); err == nil {
			t.Errorf("%s: read as an answer", name)
		}
	}
}

func TestAlMetricsService(t *testing.T) {
	t.Parallel()
	service := func(name string, ports ...string) corev1.Service {
		s := corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name}}
		for _, port := range ports {
			s.Spec.Ports = append(s.Spec.Ports, corev1.ServicePort{Name: port})
		}
		return s
	}
	if name, ok := alMetricsService([]corev1.Service{service("webhook", "https"), service("metrics-svc", "https", "metrics")}); !ok || name != "metrics-svc" {
		t.Fatalf("alMetricsService = %q, %t", name, ok)
	}
	for name, services := range map[string][]corev1.Service{
		"none":            {service("webhook", "https")},
		"no Services":     nil,
		"two":             {service("a", "metrics"), service("b", "metrics")},
		"a port named so": {service("a", "metrics-http")},
	} {
		if got, ok := alMetricsService(services); ok {
			t.Errorf("%s: chose %q", name, got)
		}
	}
}

func TestAlSelectorAndRegistryHost(t *testing.T) {
	t.Parallel()
	if got := alSelector(map[string]string{"app.kubernetes.io/name": "ptah", "app.kubernetes.io/component": "controller"}); got !=
		"app.kubernetes.io/component=controller,app.kubernetes.io/name=ptah" {
		t.Errorf("alSelector = %q", got)
	}
	if got := alSelector(nil); got != "" {
		t.Errorf("an empty selector read as %q", got)
	}
	for image, want := range map[string]string{
		"registry.invalid:5000/ptah-operator@sha256:1": "registry.invalid:5000",
		"registry.invalid/a/b/c:tag":                   "registry.invalid",
		"ptah-operator":                                "ptah-operator",
	} {
		if got := alRegistryHost(image); got != want {
			t.Errorf("alRegistryHost(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestAlAlertedCount(t *testing.T) {
	t.Parallel()
	if count, ok := alAlertedCount("3 migration resources carry an Apply nobody accounted for"); !ok || count != "3" {
		t.Fatalf("alAlertedCount = %q, %t", count, ok)
	}
	if count, ok := alAlertedCount("12 migration resources\nsecond line 4 x"); !ok || count != "12" {
		t.Errorf("a two-line summary read as %q, %t", count, ok)
	}
	for _, summary := range []string{
		"", "no count here", "3migration resources", " 3 migration resources", "3.0 migration resources", "3",
		"migration resources: 3 ",
	} {
		if count, ok := alAlertedCount(summary); ok {
			t.Errorf("%q read as %q", summary, count)
		}
	}
}

func TestAlUnresolvedMigrations(t *testing.T) {
	t.Parallel()
	migrations := []ptahv1alpha1.PtahMigration{{}, {}, {}}
	migrations[0].Status.UnresolvedRun = &ptahv1alpha1.UnresolvedMigrationRunStatus{}
	migrations[2].Status.UnresolvedRun = &ptahv1alpha1.UnresolvedMigrationRunStatus{}
	if got := alUnresolvedMigrations(migrations); got != 2 {
		t.Errorf("counted %d unresolved migrations, want 2", got)
	}
	if got := alUnresolvedMigrations(nil); got != 0 {
		t.Errorf("counted %d in nothing", got)
	}
}

func TestAlResolveClaimAndLeftFlight(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	held := func() *ptahv1alpha1.PtahSchema {
		schema := &ptahv1alpha1.PtahSchema{}
		schema.Status.Phase = ptahv1alpha1.PhaseResolving
		schema.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{
			Type: ptahv1alpha1.OperationResolve, StartedAt: metav1.NewTime(started),
		}
		return schema
	}
	if at, ok := alResolveClaim(held()); !ok || !at.Equal(started) {
		t.Fatalf("the claim read as %v, %t", at, ok)
	}
	if alLeftFlight(held()) {
		t.Fatal("a Resolve in flight read as having left it")
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahSchema){
		"no operation": func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation = nil },
		"a Verify":     func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.Type = ptahv1alpha1.OperationVerify },
		"no start":     func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.StartedAt = metav1.Time{} },
	} {
		schema := held()
		mutate(schema)
		if at, ok := alResolveClaim(schema); ok {
			t.Errorf("%s: claimed at %v", name, at)
		}
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahSchema){
		"no operation": func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation = nil },
		"a Verify":     func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation.Type = ptahv1alpha1.OperationVerify },
		// A failed attempt keeps its claim until the retry.
		"a failed Resolve": func(s *ptahv1alpha1.PtahSchema) { s.Status.Phase = ptahv1alpha1.PhaseFailed },
	} {
		schema := held()
		mutate(schema)
		if !alLeftFlight(schema) {
			t.Errorf("%s: still in flight", name)
		}
	}
}

func TestAlSecondsBetween(t *testing.T) {
	t.Parallel()
	at := func(value string) time.Time {
		t.Helper()
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	for _, test := range []struct {
		first, second string
		want          int64
	}{
		{"2026-09-28T10:00:00Z", "2026-09-28T10:01:00Z", 60},
		// The fraction is dropped from each instant, as the script's date read
		// them: the second is 10:01:00, not 59.2 seconds after the first.
		{"2026-09-28T10:00:00.9Z", "2026-09-28T10:01:00.1Z", 60},
		{"2026-09-28T10:00:00.1Z", "2026-09-28T10:01:00.9Z", 60},
		{"2026-09-28T10:01:00Z", "2026-09-28T10:00:30Z", -30},
		{"2026-09-28T12:00:00+02:00", "2026-09-28T10:00:05Z", 5},
	} {
		if got := alSecondsBetween(at(test.first), at(test.second)); got != test.want {
			t.Errorf("alSecondsBetween(%s, %s) = %d, want %d", test.first, test.second, got, test.want)
		}
	}
}

func TestAlReportLines(t *testing.T) {
	t.Parallel()
	alerts := alAlertLines([]byte(`{"data":{"alerts":[{"labels":{"alertname":"` + alOperationStall +
		`","family":"schema","operation":"Resolve"},"state":"firing"}]}}`))
	if want := []string{"  " + alOperationStall + ` firing {"family":"schema","operation":"Resolve"}`}; !reflect.DeepEqual(alerts, want) {
		t.Errorf("alAlertLines = %q", alerts)
	}
	targets := alTargetLines(alTargetsBody(t,
		alTarget{Labels: map[string]string{"pod": "manager-a"}, Health: "up"},
		alTarget{ScrapeURL: "http://10.0.0.4:8443/metrics", Health: "down", LastError: "connection refused"},
	))
	if want := []string{"  manager-a up ", "  http://10.0.0.4:8443/metrics down connection refused"}; !reflect.DeepEqual(targets, want) {
		t.Errorf("alTargetLines = %q", targets)
	}
	if alAlertLines([]byte("502")) != nil || alTargetLines([]byte("502")) != nil {
		t.Error("an answer that is not JSON produced report lines")
	}
}

// The runbook links the phase follows resolve to a heading on the page the
// chart's base names.
func TestAlRunbookAnchors(t *testing.T) {
	t.Parallel()
	page, err := os.ReadFile(filepath.Join("..", "..", "docs", "site", "src", "content", "docs", "use", "operations.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, anchor := range []string{"unresolved-gauges", "resource-state", "webhook-certificate-lifecycle"} {
		if !alRunbookAnchor(page, anchor) {
			t.Errorf("the operations page has no {#%s} heading", anchor)
		}
	}
	if alRunbookAnchor(page, "no-such-heading") {
		t.Error("an anchor the page does not carry was found")
	}
	if alRunbookAnchor([]byte("see #unresolved-gauges"), "unresolved-gauges") {
		t.Error("a link to the anchor was taken for the anchor")
	}
}
