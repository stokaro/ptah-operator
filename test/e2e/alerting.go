package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// The alerting phase stands the path from a manager's metrics to a person up
// with plain Deployments -- no Prometheus Operator, so the rules are taken from
// the chart's PrometheusRule and loaded as a file -- and asserts at the
// receiver, test/e2e/alertsink, what Alertmanager actually delivered.
const (
	alMonitoringNamespace = "ptah-e2e-monitoring"
	alStalledNamespace    = "ptah-e2e-alerting-stalled"
	alStalledSchema       = "held-resolve"
	alGateLabel           = "operator.ptah.run/e2e-alerting-gate"
	alPullSecret          = "e2e-alerting-registry-pull"
	alDiscoveryRole       = "e2e-alerting-discovery"
	alPolicyConfigMap     = "e2e-alerting-verification-policy"
	alDatabaseURLSecret   = "e2e-alerting-database-url"
	// alScrapeJob is the job Prometheus scrapes the manager replicas as.
	alScrapeJob = "ptah-operator"
)

// The numbers the rules are rendered with. They are this phase's, chosen to
// keep it short, and they are not advice: the chart has no default for either
// because the right value depends on the cluster.
const (
	alViewUnsyncedFor = 60 * time.Second
	alStalledAfter    = 60 * time.Second
)

// How often Prometheus scrapes and evaluates, and how long Alertmanager waits
// before it sends a new group. An alert whose condition holds reaches the
// receiver after its threshold plus, at most, one scrape, one evaluation, the
// group wait and one delivery. alDetectionSlack is that sum with room for a
// slow API server, and it is the detection target this phase declares: a
// delivery later than threshold plus slack fails the phase.
const (
	alScrapeInterval = 5 * time.Second
	alScrapeTimeout  = 4 * time.Second
	alGroupWait      = 5 * time.Second
	alDetectionSlack = 45 * time.Second
	alTimeout        = 300 * time.Second
	// alDeliveryPoll is how often the receiver's log is read, and alClaimPoll
	// how often the held schema is.
	alDeliveryPoll = 3 * time.Second
	alClaimPoll    = 2 * time.Second
)

// The alerts the phase needs from the rendered rules.
const (
	alUnresolvedApply = "PtahOperatorUnresolvedApply"
	alViewNotSynced   = "PtahOperatorUnresolvedViewNotSynced"
	alOperationStall  = "PtahOperatorOperationStalled"
)

// alRuleFile is the rule file a Prometheus without the Operator reads, which
// is the spec of the PrometheusRule one level up. The template is this
// repository's, so its shape is known: everything under `spec:` is the groups,
// indented two spaces more than a rule file indents them. The rendering is
// kept as the chart wrote it rather than parsed and written again, so the
// rules Prometheus loads are the chart's text.
func alRuleFile(rendered string) (string, error) {
	var lines []string
	found := false
	for line := range strings.SplitSeq(rendered, "\n") {
		if found {
			lines = append(lines, strings.TrimPrefix(line, "  "))
		}
		if line == "spec:" {
			found = true
		}
	}
	if len(lines) == 0 || lines[0] != "groups:" {
		return "", errors.New("the rendered PrometheusRule has no spec.groups to load")
	}
	rules := strings.Join(lines, "\n")
	for _, alert := range []string{alUnresolvedApply, alViewNotSynced, alOperationStall, alCertificateAlert, alAdmissionAlert, alViewReadAlert} {
		if !slices.ContainsFunc(lines, func(line string) bool { return strings.HasSuffix(line, "alert: "+alert) }) {
			return "", fmt.Errorf("the rendered rules have no %s", alert)
		}
	}
	return rules, nil
}

// alRunbookValue is the chart's runbookBaseURL line in `helm show values`.
var alRunbookValue = regexp.MustCompile(`^ *runbookBaseURL: *"(.*)"$`)

// alRunbookBase reads the runbook base the chart's values name. The chart
// names it once; a second line would leave the phase comparing links against
// two bases at once, so it is refused rather than joined.
func alRunbookBase(values string) (string, error) {
	var bases []string
	for line := range strings.SplitSeq(values, "\n") {
		if match := alRunbookValue.FindStringSubmatch(line); match != nil {
			bases = append(bases, match[1])
		}
	}
	if len(bases) != 1 || bases[0] == "" {
		return "", errors.New("the chart names no runbookBaseURL")
	}
	return bases[0], nil
}

// alPrometheusConfig is prometheus.yml. Prometheus discovers the manager Pods
// behind the metrics Service one by one, as the chart's ServiceMonitor would:
// a single scrape of the Service address would land on whichever replica
// answered.
func alPrometheusConfig(monitoringNamespace, operatorNamespace, metricsService string, apiServers ...string) string {
	seconds := int(alScrapeInterval / time.Second)
	return fmt.Sprintf(`global:
  scrape_interval: %[1]ds
  scrape_timeout: 4s
  evaluation_interval: %[1]ds
  external_labels:
    operator_namespace: "%[3]s"
    operator_metrics_service: "%[4]s"
rule_files:
  - /etc/prometheus/rules.yaml
alerting:
  alertmanagers:
    - static_configs:
        - targets: ["alertmanager.%[2]s.svc:9093"]
scrape_configs:
%[6]s  - job_name: %[5]s
    kubernetes_sd_configs:
      - role: endpointslice
        namespaces:
          names: ["%[3]s"]
    relabel_configs:
      - source_labels: [__meta_kubernetes_service_name]
        regex: %[4]s
        action: keep
      - source_labels: [__meta_kubernetes_endpointslice_port_name]
        regex: metrics
        action: keep
      - source_labels: [__meta_kubernetes_pod_name]
        target_label: pod
`, seconds, monitoringNamespace, operatorNamespace, metricsService, alScrapeJob, alAPIServerScrapeConfig(apiServers))
}

const alMissingMetricsPath = "/e2e-missing-metrics"

// Change only the selected Pod's scrape path. Its address and public labels
// stay the same, so this is a failed target, not a removed discovery result.
func alScrapeFaultConfig(config, pod string) string {
	return config + fmt.Sprintf(`      - source_labels: [__meta_kubernetes_pod_name]
        regex: %q
        action: replace
        target_label: __metrics_path__
        replacement: %s
`, regexp.QuoteMeta(pod), alMissingMetricsPath)
}

// Read the running configuration, which may lag the ConfigMap volume. An
// empty pod asks for the fault to be absent after restoration.
func alScrapeFaultLoaded(body []byte, pod string) bool {
	var response struct {
		Status string `json:"status"`
		Data   struct {
			YAML string `json:"yaml"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &response) != nil || response.Status != "success" {
		return false
	}
	var config struct {
		Scrapes []struct {
			Job     string `json:"job_name"`
			Relabel []struct {
				SourceLabels               []string `json:"source_labels"`
				Regex, Action, Replacement string
				TargetLabel                string `json:"target_label"`
			} `json:"relabel_configs"`
		} `json:"scrape_configs"`
	}
	if yaml.Unmarshal([]byte(response.Data.YAML), &config) != nil {
		return false
	}
	jobs, faults, matched := 0, 0, false
	for _, scrape := range config.Scrapes {
		if scrape.Job != alScrapeJob {
			continue
		}
		jobs++
		for _, rule := range scrape.Relabel {
			if rule.TargetLabel != "__metrics_path__" {
				continue
			}
			faults++
			matched = rule.Regex == regexp.QuoteMeta(pod) && rule.Action == "replace" &&
				rule.Replacement == alMissingMetricsPath &&
				slices.Equal(rule.SourceLabels, []string{"__meta_kubernetes_pod_name"})
		}
	}
	return jobs == 1 && ((pod == "" && faults == 0) || (pod != "" && faults == 1 && matched))
}

// alAlertmanagerConfig is alertmanager.yml: every alert to the receiver,
// grouped the way the rules label them, resolutions included.
func alAlertmanagerConfig(monitoringNamespace string) string {
	return fmt.Sprintf(`route:
  receiver: sink
  group_by: [operator_namespace, operator_metrics_service, alertname, family, operation]
  group_wait: %ds
  group_interval: 10s
  repeat_interval: 1h
receivers:
  - name: sink
    webhook_configs:
      - url: http://alert-sink.%s.svc:8080/alerts
        send_resolved: true
`, int(alGroupWait/time.Second), monitoringNamespace)
}

// alWorkload is one monitoring Deployment and the Service in front of it. The
// Pods run as the image's non-root user with nothing but an emptyDir to write
// to. configMap, when named, is mounted at /etc/<configMap>; command, when
// given, replaces the image's entrypoint. Only a Pod that runs as a
// ServiceAccount of its own gets a token.
type alWorkload struct {
	namespace, name, image string
	port                   int32
	args, command          []string
	serviceAccount         string
	configMap              string
	pullSecret             string
}

func (w alWorkload) objects() (*appsv1.Deployment, *corev1.Service) {
	labels := map[string]string{"app": w.name}
	mounts := []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
	volumes := []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	if w.configMap != "" {
		mounts = append(mounts, corev1.VolumeMount{Name: "config", MountPath: "/etc/" + w.configMap})
		volumes = append(volumes, corev1.Volume{Name: "config", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: w.configMap}},
		}})
	}
	container := corev1.Container{
		Name: w.name, Image: w.image, Args: w.args, Command: w.command,
		Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: w.port}},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(w.port)}},
			PeriodSeconds: 2,
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true),
			Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: mounts,
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: w.namespace, Name: w.name},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName:           w.serviceAccount,
					AutomountServiceAccountToken: ptr.To(w.serviceAccount != "default"),
					ImagePullSecrets:             []corev1.LocalObjectReference{{Name: w.pullSecret}},
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To[int64](65534), RunAsGroup: ptr.To[int64](65534),
						FSGroup:        ptr.To[int64](65534),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{container},
					Volumes:    volumes,
				},
			},
		},
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: w.namespace, Name: w.name},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports:    []corev1.ServicePort{{Name: "http", Port: w.port, TargetPort: intstr.FromInt32(w.port)}},
		},
	}
	return deployment, service
}

// alPullSecretFor is the credential the monitoring Pods pull their images
// with, the same shape the data plane's pull Secret has.
func alPullSecretFor(namespace, registry, username, password string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: alPullSecret},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(dockerConfigJSON(registry, username, password))},
	}
}

// alDiscoveryRBAC lets Prometheus find the manager Pods behind the metrics
// Service in the release namespace, and nothing more: read access to Pods,
// Services and EndpointSlices there.
func alDiscoveryRBAC(operatorNamespace, monitoringNamespace string) (*rbacv1.Role, *rbacv1.RoleBinding) {
	read := []string{"get", "list", "watch"}
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Namespace: operatorNamespace, Name: alDiscoveryRole},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"pods", "services"}, Verbs: read},
			{APIGroups: []string{"discovery.k8s.io"}, Resources: []string{"endpointslices"}, Verbs: read},
		},
	}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: operatorNamespace, Name: alDiscoveryRole},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: alDiscoveryRole},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Namespace: monitoringNamespace, Name: "prometheus"}},
	}
	return role, binding
}

// alHeldSchema is a schema whose operation Pods may run only on a node
// carrying the gate label: while no node does, its first Resolve is claimed,
// its Pod never schedules, and the operation stays in flight. It is the
// document the script created, so the API server judges the same fields.
func alHeldSchema(namespace string) map[string]any {
	return map[string]any{
		"apiVersion": ptahv1alpha1.GroupVersion.String(), "kind": "PtahSchema",
		"metadata": map[string]any{"namespace": namespace, "name": alStalledSchema},
		"spec": map[string]any{
			"target": map[string]any{
				"engine":          "PostgreSQL",
				"coordinationKey": "e2e/alerting/held-resolve",
				"urlFrom":         map[string]any{"name": alDatabaseURLSecret, "key": "url"},
			},
			"desired": map[string]any{
				"ociRef":                 "oci://127.0.0.1:1/e2e/held-resolve:unreachable",
				"verificationPolicyFrom": map[string]any{"name": alPolicyConfigMap, "key": "policy.yaml"},
				"transport":              map[string]any{"plainHTTP": true},
			},
			"interval": "24h",
			"execution": map[string]any{
				"activeDeadlineSeconds": int64(600), "failureRetryInterval": "1h",
				"serviceAccountName": "default",
				"imagePullSecrets":   []any{map[string]any{"name": alPullSecret}},
				"nodeSelector":       map[string]any{alGateLabel: "open"},
			},
		},
	}
}

// alDelivery is one line of the receiver's log: an alert as Alertmanager
// delivered it. It is test/e2e/alertsink's own record, read back.
type alDelivery struct {
	Receiver    string            `json:"receiver"`
	Status      string            `json:"status"`
	AlertName   string            `json:"alertname"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
	EndsAt      time.Time         `json:"endsAt"`
	ReceivedAt  time.Time         `json:"receivedAt"`
	// raw is the line as the receiver wrote it, for a failure to quote.
	raw string
}

// alDeliveries reads every delivery the receiver logged, in the order it
// logged them. The receiver writes its own diagnostics to standard error as
// plain text, so only the lines that are objects are deliveries. A line that
// is an object and does not read as a delivery is refused: the receiver
// writes only deliveries it understood, so such a line says the log is not
// the receiver's.
func alDeliveries(log []byte) ([]alDelivery, error) {
	var deliveries []alDelivery
	scanner := bufio.NewScanner(bytes.NewReader(log))
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var delivery alDelivery
		if err := json.Unmarshal([]byte(line), &delivery); err != nil {
			return nil, fmt.Errorf("the receiver logged a line that is not a delivery: %q: %w", line, err)
		}
		delivery.raw = line
		deliveries = append(deliveries, delivery)
	}
	return deliveries, scanner.Err()
}

// alMatch selects a delivery by its status, its alert and the labels given.
// A label the delivery does not carry is not equal to any value, as jq's null
// is not.
type alMatch struct {
	status, alertName string
	labels            map[string]string
}

func (m alMatch) matches(delivery alDelivery) bool {
	if delivery.Status != m.status || delivery.AlertName != m.alertName {
		return false
	}
	for key, value := range m.labels {
		if got, ok := delivery.Labels[key]; !ok || got != value {
			return false
		}
	}
	return true
}

// alFirstDelivery is the first delivery at or after position from that the
// match selects, and its position. A delivery logged before from was received
// before the event the caller measures, so it cannot be that event's.
func alFirstDelivery(deliveries []alDelivery, from int, match alMatch) (alDelivery, int, bool) {
	for index := max(from, 0); index < len(deliveries); index++ {
		if match.matches(deliveries[index]) {
			return deliveries[index], index, true
		}
	}
	return alDelivery{}, -1, false
}

// alTargets is the part of Prometheus's /api/v1/targets the phase reads.
type alTargets struct {
	Status string `json:"status"`
	Data   struct {
		ActiveTargets []alTarget `json:"activeTargets"`
	} `json:"data"`
}

type alTarget struct {
	Labels     map[string]string `json:"labels"`
	ScrapeURL  string            `json:"scrapeUrl"`
	Health     string            `json:"health"`
	LastError  string            `json:"lastError"`
	LastScrape time.Time         `json:"lastScrape"`
}

// The selected manager must have failed an actual scrape after the fault was
// loaded, while each other manager remains a distinct, healthy target.
func alOneTargetLost(body []byte, replicas int, pod string, loaded time.Time) bool {
	var targets alTargets
	if replicas < 2 || pod == "" || loaded.IsZero() || json.Unmarshal(body, &targets) != nil || targets.Status != "success" {
		return false
	}
	seen, lost := make(map[string]bool), false
	for _, target := range targets.Data.ActiveTargets {
		if target.Labels["job"] != alScrapeJob {
			continue
		}
		name := target.Labels["pod"]
		if name == "" || seen[name] {
			return false
		}
		seen[name] = true
		if name == pod {
			scrapeURL, err := url.Parse(target.ScrapeURL)
			if err != nil || scrapeURL.Path != alMissingMetricsPath || target.Health != "down" ||
				target.LastError == "" || target.LastScrape.Before(loaded) {
				return false
			}
			lost = true
		} else if target.Health != "up" {
			return false
		}
	}
	return lost && len(seen) == replicas
}

// Lease transitions detect a leader that moved away and back between polls;
// UIDs and per-container restart counts detect replacement or process restart.
func alSameManagers(lease *coordinationv1.Lease, pods []corev1.Pod, before *coordinationv1.Lease, original []corev1.Pod) bool {
	if lease.UID == "" || lease.UID != before.UID || haLeaseHolder(lease) == "" ||
		haLeaseHolder(lease) != haLeaseHolder(before) || haLeaseTransitions(lease) != haLeaseTransitions(before) ||
		len(pods) < 2 || len(pods) != len(original) {
		return false
	}
	previous := make(map[string]corev1.Pod, len(original))
	for _, pod := range original {
		previous[pod.Name] = pod
	}
	if len(previous) != len(original) {
		return false
	}
	leader := false
	for _, pod := range pods {
		old, ok := previous[pod.Name]
		if !ok || pod.UID == "" || pod.UID != old.UID || pod.DeletionTimestamp != nil ||
			pod.Status.Phase != corev1.PodRunning || !harness.PodReady(&pod) ||
			len(pod.Status.ContainerStatuses) == 0 || len(pod.Status.ContainerStatuses) != len(pod.Spec.Containers) {
			return false
		}
		delete(previous, pod.Name)
		currentRestarts, oldRestarts := make(map[string]int32), make(map[string]int32)
		for _, status := range pod.Status.ContainerStatuses {
			if !status.Ready || status.State.Running == nil {
				return false
			}
			currentRestarts[status.Name] = status.RestartCount
		}
		for _, status := range old.Status.ContainerStatuses {
			oldRestarts[status.Name] = status.RestartCount
		}
		if len(currentRestarts) != len(pod.Status.ContainerStatuses) || !maps.Equal(currentRestarts, oldRestarts) {
			return false
		}
		leader = leader || pod.Name == haLeaderPodName(haLeaseHolder(lease))
	}
	return leader && len(previous) == 0
}

// alTargetsReady is every manager replica a target, and every target up. A
// Prometheus that found one replica of two would pass everything after it on
// the leader's numbers and say nothing about the other.
func alTargetsReady(body []byte, replicas int) bool {
	var targets alTargets
	if err := json.Unmarshal(body, &targets); err != nil {
		return false
	}
	count, up := 0, true
	for _, target := range targets.Data.ActiveTargets {
		if target.Labels["job"] != alScrapeJob {
			continue
		}
		count++
		up = up && target.Health == "up"
	}
	return count == replicas && up
}

// alRulesLoaded is Prometheus answering with the chart's alerting rules.
func alRulesLoaded(body []byte) bool {
	var rules struct {
		Data struct {
			Groups []struct {
				Rules []struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"rules"`
			} `json:"groups"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &rules); err != nil {
		return false
	}
	var alerting []string
	for _, group := range rules.Data.Groups {
		for _, rule := range group.Rules {
			if rule.Type == "alerting" {
				alerting = append(alerting, rule.Name)
			}
		}
	}
	return slices.Contains(alerting, alUnresolvedApply) && slices.Contains(alerting, alViewNotSynced) &&
		slices.Contains(alerting, alOperationStall) && slices.Contains(alerting, alCertificateAlert) && slices.Contains(alerting, alAdmissionAlert) && slices.Contains(alerting, alViewReadAlert)
}

// alNoActiveAlerts reads an instant query for ALERTS: true when Prometheus
// answered and no series matched. An answer that is not a success is an
// error, so an empty result cannot stand in for one Prometheus never gave.
func alNoActiveAlerts(body []byte) (bool, error) {
	var answer struct {
		Status string `json:"status"`
		Data   struct {
			Result []json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return false, fmt.Errorf("the answer is not JSON: %w", err)
	}
	if answer.Status != "success" {
		return false, fmt.Errorf("the query answered %q", answer.Status)
	}
	return len(answer.Data.Result) == 0, nil
}

// alMetricsService is the release's one Service with a port named metrics,
// which Prometheus discovers the manager replicas through.
func alMetricsService(services []corev1.Service) (string, bool) {
	var names []string
	for _, service := range services {
		if slices.ContainsFunc(service.Spec.Ports, func(port corev1.ServicePort) bool { return port.Name == "metrics" }) {
			names = append(names, service.Name)
		}
	}
	if len(names) != 1 {
		return "", false
	}
	return names[0], true
}

// alSelector is the Deployment's matchLabels as a kubectl label selector,
// keys in order.
func alSelector(matchLabels map[string]string) string {
	keys := make([]string, 0, len(matchLabels))
	for key := range matchLabels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, key+"="+matchLabels[key])
	}
	return strings.Join(pairs, ",")
}

// alRegistryHost is the registry a manager image was pulled from: everything
// before its first slash, as ${image%%/*} took it.
func alRegistryHost(image string) string {
	host, _, _ := strings.Cut(image, "/")
	return host
}

// alSummaryCount is the count the unresolved alert's summary leads with.
var alSummaryCount = regexp.MustCompile(`^([0-9]+) `)

// alAlertedCount reads the count off the unresolved alert's summary, as
// written: the drill-down it is compared with is a count of resources, so a
// leading zero or a fraction would be a different answer rather than the
// same one spelled differently.
func alAlertedCount(summary string) (string, bool) {
	first, _, _ := strings.Cut(summary, "\n")
	match := alSummaryCount.FindStringSubmatch(first)
	if match == nil {
		return "", false
	}
	return match[1], true
}

// alUnresolvedMigrations counts the migrations carrying a run nobody
// accounted for, which is what the runbook's drill-down lists.
func alUnresolvedMigrations(migrations []ptahv1alpha1.PtahMigration) int {
	count := 0
	for index := range migrations {
		if migrations[index].Status.UnresolvedRun != nil {
			count++
		}
	}
	return count
}

// alSecondsBetween is the second instant minus the first in whole seconds,
// each taken to the second it falls in, as `date +%s` read the script's
// instants with their fractions dropped.
func alSecondsBetween(first, second time.Time) int64 {
	return second.Unix() - first.Unix()
}

// alAlertLines is report_state's reading of /api/v1/alerts: one line per
// alert with its name, its state and its other labels.
func alAlertLines(body []byte) []string {
	var alerts struct {
		Data struct {
			Alerts []struct {
				Labels map[string]string `json:"labels"`
				State  string            `json:"state"`
			} `json:"alerts"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &alerts) != nil {
		return nil
	}
	lines := make([]string, 0, len(alerts.Data.Alerts))
	for _, alert := range alerts.Data.Alerts {
		rest := map[string]string{}
		for key, value := range alert.Labels {
			if key != "alertname" {
				rest[key] = value
			}
		}
		encoded, err := json.Marshal(rest)
		if err != nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("  %s %s %s", alert.Labels["alertname"], alert.State, encoded))
	}
	return lines
}

// alTargetLines is report_state's reading of /api/v1/targets: each target by
// its Pod, or by its address where it names none, its health and its last
// error.
func alTargetLines(body []byte) []string {
	var targets alTargets
	if json.Unmarshal(body, &targets) != nil {
		return nil
	}
	lines := make([]string, 0, len(targets.Data.ActiveTargets))
	for _, target := range targets.Data.ActiveTargets {
		name := target.Labels["pod"]
		if name == "" {
			name = target.ScrapeURL
		}
		lines = append(lines, fmt.Sprintf("  %s %s %s", name, target.Health, target.LastError))
	}
	return lines
}

// alRunbookAnchor says whether a page carries the heading anchor a runbook
// link names, written as the page writes its anchors: {#name}.
func alRunbookAnchor(page []byte, anchor string) bool {
	return bytes.Contains(page, []byte("{#"+anchor+"}"))
}
