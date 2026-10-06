//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/test/e2e/harness"
	"github.com/stokaro/ptah-operator/test/e2e/phases"
)

// TestAlerting proves infrastructure alert delivery and recovery. Operation
// incidents run in a separate cluster so manager and admission faults cannot
// contaminate their native histories.
func TestAlerting(t *testing.T) {
	runAlertingPhase(t, phases.Alerting)
}

// TestAlertingCertificates owns plan storage and certificate expiry incidents.
func TestAlertingCertificates(t *testing.T) {
	runAlertingPhase(t, phases.AlertingCertificates)
}

// TestAlertingOperations proves native operation incidents on both families.
func TestAlertingOperations(t *testing.T) {
	runAlertingPhase(t, phases.AlertingOperations)
}

func runAlertingPhase(t *testing.T, phase phases.Of[phases.AlertingInputs]) {
	run, inputs := harness.Begin(t, phase)
	a := newAlertingRun(t, run, inputs)
	a.scope = alScopeFor(phase.Name)
	bodies := map[string]func(){
		"native-producers":              a.nativeProducers,
		"monitoring-path":               a.monitoringPath,
		"unresolved-apply":              a.unresolvedApply,
		"ordinary-policy-waits":         a.negativeControls,
		"stalled-operation":             a.stalledOperation,
		"resource-overdue":              a.resourceOverdue,
		"lock-release-owed":             a.lockReleaseOwed,
		"operations-failing":            a.operationsFailing,
		"plan-store-large":              a.planStoreLarge,
		"unresolved-view-read-failures": a.viewReadFailures,
		"lost-scrape-target":            a.lostScrapeTarget,
		"certificate-expiry":            a.certificateExpiry,
		"lost-view":                     a.lostView,
		"upgrade-alerts":                a.upgradeAlerts,
	}
	for _, name := range phase.Scenarios {
		body, ok := bodies[name]
		if !ok {
			t.Fatalf("alerting scenario %s has no implementation", name)
		}
		if !run.Scenario(name, a.scenario(body)) {
			return
		}
	}
	run.Logf("e2e %s: PASS every declared native incident reached the receiver and recoverable faults cleared", phase.Name)
}

// alertingRun is what the alerting scenarios share. Each scenario runs as a
// subtest, and t is that subtest while it runs.
type alertingRun struct {
	t      *testing.T
	parent *testing.T
	ctx    context.Context
	in     phases.AlertingInputs
	scope  alPhaseScope

	cluster     *harness.Cluster
	workDir     string
	credentials registryCredentials

	// The installed manager: its Deployment, how many replicas it asks for,
	// the labels its Pods carry, the Service Prometheus discovers them behind,
	// and the registry its image came from.
	manager           string
	replicas          int32
	managerLabels     map[string]string
	metricsService    string
	apiServerTargets  []string
	apiMetricsObjects []client.Object
	registryHost      string
	// runbookBase is where the chart's runbook links point.
	runbookBase string

	// What the cleanup puts back however the phase ends.
	cordoned   []string
	gateOpened bool
}

// alPinnedImage is an image named by its digest.
var alPinnedImage = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

func newAlertingRun(t *testing.T, run *harness.Run, in phases.AlertingInputs) *alertingRun {
	t.Helper()
	a := &alertingRun{t: t, parent: t, ctx: run.Context(), in: in, scope: alScopeFor("alerting")}
	// Registered before anything is created, so a phase that fails part way
	// still releases the nodes and removes what it stood up.
	t.Cleanup(a.cleanup)
	for _, command := range []string{"kubectl", "helm"} {
		if _, err := exec.LookPath(command); err != nil {
			a.fatalf("required command is not installed: %s", command)
		}
	}
	if info, err := os.Stat(in.Kubeconfig); err != nil || !info.Mode().IsRegular() {
		a.fatalf("E2E_KUBECONFIG does not name a file")
	}
	if info, err := os.Stat(in.ChartPackage); err != nil || !info.Mode().IsRegular() {
		a.fatalf("E2E_CHART_PACKAGE does not name a file")
	}
	for _, image := range []string{in.ExecutorImage, in.FixtureImage, in.PrometheusImage, in.AlertmanagerImage} {
		if !alPinnedImage.MatchString(image) {
			a.fatalf("image %s is not pinned by digest", image)
		}
	}
	content, err := os.ReadFile(in.RegistryCredentialsFile)
	a.check(err, "read E2E_REGISTRY_CREDENTIALS_FILE")
	if a.credentials, err = parseRegistryCredentials(content); err != nil {
		a.fatalf("%v", err)
	}
	a.workDir, err = os.MkdirTemp("", "ptah-e2e-alerting.")
	a.check(err, "create the work directory")
	a.check(os.Chmod(a.workDir, 0o700), "make the work directory private")
	if a.cluster, err = harness.Connect(in.Kubeconfig); err != nil {
		a.fatalf("%v", err)
	}
	return a
}

func (a *alertingRun) scenario(body func()) func(*testing.T) {
	return func(t *testing.T) {
		a.t = t
		defer func() { a.t = a.parent }()
		body()
	}
}

func (a *alertingRun) fatalf(format string, arguments ...any) {
	a.t.Helper()
	a.t.Fatalf("e2e alerting: "+format, arguments...)
}

func (a *alertingRun) logf(format string, arguments ...any) {
	a.t.Helper()
	a.t.Logf("e2e alerting: "+format, arguments...)
}

func (a *alertingRun) check(err error, format string, arguments ...any) {
	a.t.Helper()
	if err != nil {
		a.fatalf("%s: %v", fmt.Sprintf(format, arguments...), err)
	}
}

// sleep pauses between two readings, and ends the scenario when the phase's
// own bound ends first.
func (a *alertingRun) sleep(duration time.Duration) {
	a.t.Helper()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-a.ctx.Done():
		a.fatalf("the phase's bound ended while it waited: %v", a.ctx.Err())
	case <-timer.C:
	}
}

func (a *alertingRun) create(object client.Object) error {
	return a.cluster.Client.Create(a.ctx, object, client.FieldOwner(harness.FieldOwner))
}

func (a *alertingRun) mustCreate(object client.Object, what string) {
	a.t.Helper()
	a.check(a.create(object), "create %s", what)
}

// prometheus reads Prometheus's HTTP API through the API server's service
// proxy, so the phase needs no port of its own on the cluster.
func (a *alertingRun) prometheus(ctx context.Context, path string, parameters map[string]string) ([]byte, error) {
	return a.cluster.Clientset.CoreV1().Services(a.scope.monitoringNamespace).
		ProxyGet("http", "prometheus", "9090", path, parameters).DoRaw(ctx)
}

// noActiveAlerts asks Prometheus whether any series of the ALERTS selector
// given is active.
func (a *alertingRun) noActiveAlerts(selector string) bool {
	a.t.Helper()
	body, err := a.prometheus(a.ctx, "/api/v1/query", map[string]string{"query": selector})
	if err != nil {
		a.fatalf("Prometheus did not answer for its alerts: %v", err)
	}
	none, err := alNoActiveAlerts(body)
	if err != nil {
		a.fatalf("Prometheus did not answer for its alerts: %v", err)
	}
	return none
}

// deploymentLog is what kubectl logs deployment/<name> returns in the
// monitoring namespace: the log of the Deployment's Pod, which is the one Pod
// each monitoring Deployment runs.
func (a *alertingRun) deploymentLog(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	stdout, stderr, err := a.cluster.Kubectl(ctx, append([]string{"-n", a.scope.monitoringNamespace, "logs", "deployment/" + name}, arguments...)...)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(stderr)))
	}
	return stdout, nil
}

// deliveryCount is how many deliveries the receiver has logged so far. A
// wait that starts after an event takes deliveries from this position on, so
// one received before the event cannot stand in for it.
func (a *alertingRun) deliveryCount() int {
	a.t.Helper()
	count := -1
	err := harness.Wait(a.ctx, "a reading of the receiver's log", alTimeout, alDeliveryPoll,
		func(ctx context.Context) (bool, string, error) {
			log, err := a.deploymentLog(ctx, "alert-sink")
			if err != nil {
				return false, err.Error(), nil
			}
			deliveries, err := alDeliveries(log)
			if err != nil {
				return false, "", err
			}
			count = len(deliveries)
			return true, "", nil
		})
	a.check(err, "the receiver's log could not be read")
	return count
}

// waitForDelivery waits for the first delivery at or after position from
// that match selects, and returns it with its position. A log that cannot be
// read is read again at the next poll, as the script's `|| true` did.
func (a *alertingRun) waitForDelivery(match alMatch, description string, timeout time.Duration, from int) (alDelivery, int) {
	return a.waitForDeliveryWithCheck(match, description, timeout, from, func() {})
}

func (a *alertingRun) waitForDeliveryWithCheck(match alMatch, description string, timeout time.Duration, from int, check func()) (alDelivery, int) {
	a.t.Helper()
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); {
		check()
		if log, err := a.deploymentLog(a.ctx, "alert-sink"); err == nil {
			deliveries, err := alDeliveries(log)
			if err != nil {
				a.fatalf("%v", err)
			}
			if delivery, index, ok := alFirstDelivery(deliveries, from, match); ok {
				return delivery, index
			}
		}
		a.sleep(alDeliveryPoll)
	}
	a.fatalf("the receiver never got %s", description)
	return alDelivery{}, -1
}

// operationsPage is the page the chart's runbook links point into.
func (a *alertingRun) operationsPage() []byte {
	a.t.Helper()
	page, err := os.ReadFile(filepath.Join(repositoryRoot, "docs", "site", "src", "content", "docs", "use", "operations.md"))
	a.check(err, "read the operations page")
	return page
}

// monitoringPath reads the installed release, renders the chart's rules the
// way this installation would, and stands up Prometheus, Alertmanager and a
// receiver: every manager replica a target, and the rules loaded.
func (a *alertingRun) monitoringPath() {
	a.t.Helper()
	a.readManager()
	a.requireAFreshCluster()
	rules := a.renderRules()
	a.standUp(rules)
	a.waitForTargets()
	a.waitForAPIServerTargets()
	a.check(harness.Wait(a.ctx, "every frozen chart rule to evaluate successfully", alDetectionSlack, alDeliveryPoll,
		func(ctx context.Context) (bool, string, error) {
			body, err := a.prometheus(ctx, "/api/v1/rules", nil)
			if err != nil {
				return false, "", err
			}
			return alRulesLoaded(body), "a frozen rule is missing, duplicated, unevaluated or failing", nil
		}), "verify the complete loaded alert rules")

	a.logf("Prometheus scrapes all %d manager replicas and loaded the chart rules", a.replicas)
}

// readManager reads the installed manager: its Deployment, its replicas, the
// labels its Pods carry, the Service in front of its metrics port, and the
// registry its image came from.
func (a *alertingRun) readManager() {
	a.t.Helper()
	deployments := &appsv1.DeploymentList{}
	a.check(a.cluster.Client.List(a.ctx, deployments, client.InNamespace(a.in.OperatorNamespace),
		client.MatchingLabels{"app.kubernetes.io/component": "controller"}), "list the controller Deployments")
	switch len(deployments.Items) {
	case 0:
		a.fatalf("installed controller Deployment is missing")
	case 1:
	default:
		// Every replica of one manager is removed below; a second controller
		// Deployment would keep a view running and the row would measure it.
		a.fatalf("the release runs %d controller Deployments, and this phase reads one", len(deployments.Items))
	}
	manager := deployments.Items[0]
	a.manager = manager.Name
	if manager.Spec.Replicas == nil || *manager.Spec.Replicas < 1 {
		a.fatalf("the controller Deployment asks for no replica")
	}
	a.replicas = *manager.Spec.Replicas
	if manager.Spec.Selector == nil || alSelector(manager.Spec.Selector.MatchLabels) == "" {
		a.fatalf("the controller Deployment has no label selector")
	}
	a.managerLabels = manager.Spec.Selector.MatchLabels
	services := &corev1.ServiceList{}
	a.check(a.cluster.Client.List(a.ctx, services, client.InNamespace(a.in.OperatorNamespace)), "list the release's Services")
	var ok bool
	if a.metricsService, ok = alMetricsService(services.Items); !ok {
		a.fatalf("the release has no single Service with a port named metrics for Prometheus to discover")
	}
	if len(manager.Spec.Template.Spec.Containers) == 0 {
		a.fatalf("the controller Deployment runs no container")
	}
	a.registryHost = alRegistryHost(manager.Spec.Template.Spec.Containers[0].Image)
}

// requireAFreshCluster refuses a cluster an earlier run of this phase left
// behind: the two namespaces it stands up and removes, and a node already
// carrying the gate the held schema waits on.
func (a *alertingRun) requireAFreshCluster() {
	a.t.Helper()
	for _, name := range []string{a.scope.monitoringNamespace, a.scope.stalledNamespace} {
		err := a.cluster.Client.Get(a.ctx, types.NamespacedName{Name: name}, &corev1.Namespace{})
		switch {
		case err == nil:
			a.fatalf("namespace %s already exists; this phase stands it up and removes it", name)
		case !apierrors.IsNotFound(err):
			a.fatalf("namespace %s could not be looked up: %v", name, err)
		}
	}
	held := &corev1.NodeList{}
	a.check(a.cluster.Client.List(a.ctx, held, client.HasLabels{alGateLabel}), "list the nodes carrying %s", alGateLabel)
	if len(held.Items) != 0 {
		names := make([]string, 0, len(held.Items))
		for _, node := range held.Items {
			names = append(names, "node/"+node.Name)
		}
		a.fatalf("nodes already carry %s: %s", alGateLabel, strings.Join(names, " "))
	}
}

// renderRules renders the rules this installation would, from the release's
// own values with the monitoring switches this phase needs laid over them,
// and reads the runbook base the chart names.
func (a *alertingRun) renderRules() string {
	a.t.Helper()
	revisions := &corev1.SecretList{}
	a.check(a.cluster.Client.List(a.ctx, revisions, client.InNamespace(a.in.OperatorNamespace),
		client.MatchingLabels{"owner": "helm", "name": a.in.HelmRelease, "status": "deployed"}),
		"list the revisions of release %s", a.in.HelmRelease)
	if len(revisions.Items) == 0 {
		a.fatalf("release %s has no deployed revision to read values from", a.in.HelmRelease)
	}
	values, err := a.cluster.Helm(a.ctx, "-n", a.in.OperatorNamespace, "get", "values", a.in.HelmRelease, "-o", "yaml")
	if err != nil {
		a.fatalf("the values of release %s could not be read: %v", a.in.HelmRelease, err)
	}
	valuesFile := filepath.Join(a.workDir, "release-values.yaml")
	a.check(os.WriteFile(valuesFile, values, 0o600), "write the release values")
	rendered, err := a.cluster.Helm(a.ctx, "template", a.in.HelmRelease, a.in.ChartPackage,
		"--namespace", a.in.OperatorNamespace,
		"-f", valuesFile,
		"-f", filepath.Join(repositoryRoot, "support", "qualification", "0.2.0-monitoring.yaml"),
		"--show-only", "templates/prometheusrule.yaml")
	if err != nil {
		a.fatalf("the chart did not render its PrometheusRule: %v", err)
	}
	chartValues, err := a.cluster.Helm(a.ctx, "show", "values", a.in.ChartPackage)
	if err != nil {
		a.fatalf("the chart names no runbookBaseURL: %v", err)
	}
	if a.runbookBase, err = alRunbookBase(string(chartValues)); err != nil {
		a.fatalf("%v", err)
	}
	rules, err := alRuleFile(string(rendered))
	if err != nil {
		a.fatalf("%v", err)
	}
	return rules
}

// standUp creates the monitoring namespace and everything in it: the pull
// credential, Prometheus's identity and its discovery grant in the release
// namespace, both configurations, and the three workloads.
func (a *alertingRun) standUp(rules string) {
	a.t.Helper()
	a.logf("standing up Prometheus, Alertmanager and a receiver in %s", a.scope.monitoringNamespace)
	namespace := &corev1.Namespace{}
	namespace.Name = a.scope.monitoringNamespace
	a.mustCreate(namespace, "namespace "+a.scope.monitoringNamespace)
	a.mustCreate(alPullSecretFor(a.scope.monitoringNamespace, a.registryHost, a.credentials.Username, a.credentials.Password),
		"the monitoring pull Secret")
	account := &corev1.ServiceAccount{}
	account.Namespace, account.Name = a.scope.monitoringNamespace, "prometheus"
	a.mustCreate(account, "the prometheus ServiceAccount")
	a.apiServerMetrics()
	role, binding := alDiscoveryRBAC(a.in.OperatorNamespace, a.scope.monitoringNamespace)
	a.mustCreate(role, "the discovery Role")
	a.mustCreate(binding, "the discovery RoleBinding")
	a.mustCreate(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: a.scope.monitoringNamespace, Name: "prometheus"},
		Data: map[string]string{
			"prometheus.yml": alPrometheusConfig(a.scope.monitoringNamespace, a.in.OperatorNamespace, a.metricsService, a.apiServerTargets...),
			"rules.yaml":     rules,
		},
	}, "the prometheus ConfigMap")
	a.mustCreate(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: a.scope.monitoringNamespace, Name: "alertmanager"},
		Data:       map[string]string{"alertmanager.yml": alAlertmanagerConfig(a.scope.monitoringNamespace)},
	}, "the alertmanager ConfigMap")
	for _, workload := range []alWorkload{
		// The receiver comes from the isolated fixture image, whose entrypoint
		// is the OCI publisher, so it names its own command.
		{name: "alert-sink", image: a.in.FixtureImage, port: 8080, args: []string{"-listen", ":8080"},
			serviceAccount: "default", command: []string{"/e2e-alert-sink"}},
		{name: "alertmanager", image: a.in.AlertmanagerImage, port: 9093,
			args:           []string{"--config.file=/etc/alertmanager/alertmanager.yml", "--storage.path=/data", "--cluster.listen-address="},
			serviceAccount: "default", configMap: "alertmanager"},
		{name: "prometheus", image: a.in.PrometheusImage, port: 9090,
			args:           []string{"--config.file=/etc/prometheus/prometheus.yml", "--storage.tsdb.path=/data", "--web.enable-lifecycle", "--no-config.auto-reload"},
			serviceAccount: "prometheus", configMap: "prometheus"},
	} {
		workload.namespace, workload.pullSecret = a.scope.monitoringNamespace, alPullSecret
		deployment, service := workload.objects()
		a.mustCreate(deployment, "Deployment "+workload.name)
		a.mustCreate(service, "Service "+workload.name)
	}
	for _, name := range []string{"alert-sink", "alertmanager", "prometheus"} {
		if err := a.cluster.WaitForRollout(a.ctx, a.scope.monitoringNamespace, name, alTimeout); err != nil {
			a.fatalf("%s did not become ready: %v", name, err)
		}
	}
}

// waitForTargets waits for every manager replica to be a target, and every
// target up.
func (a *alertingRun) waitForTargets() {
	a.t.Helper()
	for deadline := time.Now().Add(alTimeout); ; {
		if body, err := a.prometheus(a.ctx, "/api/v1/targets", nil); err == nil && alTargetsReady(body, int(a.replicas)) {
			return
		}
		if !time.Now().Before(deadline) {
			// The cleanup reports what the monitoring path held.
			a.fatalf("Prometheus did not reach all %d manager replicas", a.replicas)
		}
		a.sleep(alDeliveryPoll)
	}
}

// createHeldNamespace prepares both families' held Resolve fixtures.
func (a *alertingRun) createHeldNamespace() {
	a.t.Helper()
	namespace := &corev1.Namespace{}
	namespace.Name = a.scope.stalledNamespace
	a.mustCreate(namespace, "namespace "+a.scope.stalledNamespace)
	policy, err := os.ReadFile(filepath.Join(repositoryRoot, "testdata", "e2e", "verification-policy.yaml"))
	a.check(err, "read the verification policy fixture")
	a.mustCreate(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: a.scope.stalledNamespace, Name: alPolicyConfigMap},
		Data:       map[string]string{"policy.yaml": string(policy)},
	}, "the verification policy")
	a.mustCreate(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: a.scope.stalledNamespace, Name: alDatabaseURLSecret},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"url": []byte("postgres://e2e:unused@database.invalid/e2e")},
	}, "the database URL Secret")
	pull := &corev1.Secret{}
	a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: a.scope.monitoringNamespace, Name: alPullSecret}, pull),
		"read the monitoring pull Secret")
	a.mustCreate(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: a.scope.stalledNamespace, Name: pull.Name},
		Type:       pull.Type, Data: pull.Data,
	}, "the held schema's pull Secret")
}

// removeManagers deletes every manager Pod and waits for each one deleted to
// be gone, as kubectl delete --wait does: a replacement the ReplicaSet makes
// under the same selector is a different Pod, and it stays Pending.
func (a *alertingRun) removeManagers() error {
	pods := &corev1.PodList{}
	if err := a.cluster.Client.List(a.ctx, pods, client.InNamespace(a.in.OperatorNamespace),
		client.MatchingLabels(a.managerLabels)); err != nil {
		return err
	}
	deleted := map[types.UID]string{}
	for index := range pods.Items {
		pod := &pods.Items[index]
		if err := a.cluster.Client.Delete(a.ctx, pod, client.Preconditions{UID: &pod.UID}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete pod %s: %w", pod.Name, err)
		}
		deleted[pod.UID] = pod.Name
	}
	return harness.Wait(a.ctx, "the deleted manager Pods to be gone", alTimeout, time.Second,
		func(ctx context.Context) (bool, string, error) {
			var remaining []string
			for uid, name := range deleted {
				pod := &corev1.Pod{}
				err := a.cluster.Client.Get(ctx, types.NamespacedName{Namespace: a.in.OperatorNamespace, Name: name}, pod)
				switch {
				case apierrors.IsNotFound(err):
				case err != nil:
					return false, fmt.Sprintf("pod %s could not be read: %v", name, err), nil
				case pod.UID == uid:
					remaining = append(remaining, name)
				}
			}
			slices.Sort(remaining)
			return len(remaining) == 0, "still present: " + strings.Join(remaining, ", "), nil
		})
}

// setGate labels every node with the gate, or removes it from every node when
// value is empty.
func (a *alertingRun) setGate(ctx context.Context, value string) error {
	return a.setSchedulingGate(ctx, alGateLabel, value)
}

func (a *alertingRun) setSchedulingGate(ctx context.Context, label, value string) error {
	nodes := &corev1.NodeList{}
	if err := a.cluster.Client.List(ctx, nodes); err != nil {
		return err
	}
	for index := range nodes.Items {
		name := nodes.Items[index].Name
		err := a.updateNode(ctx, name, func(node *corev1.Node) bool {
			if value == "" {
				if _, found := node.Labels[label]; !found {
					return false
				}
				delete(node.Labels, label)
				return true
			}
			if node.Labels == nil {
				node.Labels = map[string]string{}
			}
			node.Labels[label] = value
			return true
		})
		if err != nil {
			return fmt.Errorf("node %s: %w", name, err)
		}
	}
	return nil
}

// setUnschedulable cordons or uncordons one node, as kubectl cordon does.
func (a *alertingRun) setUnschedulable(ctx context.Context, name string, unschedulable bool) error {
	return a.updateNode(ctx, name, func(node *corev1.Node) bool {
		if node.Spec.Unschedulable == unschedulable {
			return false
		}
		node.Spec.Unschedulable = unschedulable
		return true
	})
}

// updateNode applies change to the node as it is now, and writes it back
// only when change made one, rereading on a conflict.
func (a *alertingRun) updateNode(ctx context.Context, name string, change func(*corev1.Node) bool) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node := &corev1.Node{}
		if err := a.cluster.Client.Get(ctx, types.NamespacedName{Name: name}, node); err != nil {
			return err
		}
		if !change(node) {
			return nil
		}
		return a.cluster.Client.Update(ctx, node, client.FieldOwner(harness.FieldOwner))
	})
}

// reportState prints what the monitoring path held when the phase failed:
// the receiver's log, the alerts Prometheus had, its targets, and
// Alertmanager's log. A failure here otherwise reads as a timeout with
// nothing behind it. Every read is best effort.
func (a *alertingRun) reportState(ctx context.Context) {
	w := os.Stderr
	indent := func(log []byte) {
		for line := range strings.SplitSeq(strings.TrimRight(string(log), "\n"), "\n") {
			if line != "" {
				_, _ = fmt.Fprintf(w, "  %s\n", line)
			}
		}
	}
	_, _ = fmt.Fprintln(w, "e2e alerting: receiver log:")
	if log, err := a.deploymentLog(ctx, "alert-sink", "--tail=60"); err == nil {
		indent(log)
	}
	_, _ = fmt.Fprintln(w, "e2e alerting: Prometheus alerts:")
	if body, err := a.prometheus(ctx, "/api/v1/alerts", nil); err == nil {
		for _, line := range alAlertLines(body) {
			_, _ = fmt.Fprintln(w, line)
		}
	}
	_, _ = fmt.Fprintln(w, "e2e alerting: Prometheus targets:")
	if body, err := a.prometheus(ctx, "/api/v1/targets", nil); err == nil {
		for _, line := range alTargetLines(body) {
			_, _ = fmt.Fprintln(w, line)
		}
	}
	_, _ = fmt.Fprintln(w, "e2e alerting: Alertmanager log:")
	if log, err := a.deploymentLog(ctx, "alertmanager", "--tail=30"); err == nil {
		indent(log)
	}
}

// cleanup puts the cluster back however the phase ended. Nodes the lost-view
// row cordoned are released whatever happened, because every later step of
// any run on this cluster schedules Pods.
func (a *alertingRun) cleanup() {
	t := a.parent
	a.t = t
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if a.cluster != nil {
		if t.Failed() {
			a.reportState(ctx)
		}
		for _, name := range a.cordoned {
			if err := a.setUnschedulable(ctx, name, false); err != nil {
				t.Errorf("e2e alerting: node %s could not be uncordoned: %v", name, err)
			}
		}
		a.cordoned = nil
		if a.gateOpened {
			if err := a.setGate(ctx, ""); err != nil {
				t.Errorf("e2e alerting: the gate could not be removed: %v", err)
			}
			a.gateOpened = false
		}
		for i := len(a.apiMetricsObjects) - 1; i >= 0; i-- {
			object := a.apiMetricsObjects[i]
			uid := object.GetUID()
			if err := a.cluster.Client.Delete(ctx, object, client.Preconditions{UID: &uid}); client.IgnoreNotFound(err) != nil {
				t.Errorf("e2e alerting: remove the API server metrics grant: %v", err)
			}
		}
		// Neither namespace is waited for: nothing after this phase reads them.
		for _, name := range []string{a.scope.stalledNamespace, a.scope.monitoringNamespace} {
			namespace := &corev1.Namespace{}
			namespace.Name = name
			if err := a.cluster.Client.Delete(ctx, namespace); err != nil && !apierrors.IsNotFound(err) {
				t.Errorf("e2e alerting: namespace %s could not be removed: %v", name, err)
			}
		}
		role, binding := alDiscoveryRBAC(a.in.OperatorNamespace, a.scope.monitoringNamespace)
		for _, object := range []client.Object{binding, role} {
			if err := a.cluster.Client.Delete(ctx, object); err != nil && !apierrors.IsNotFound(err) {
				t.Errorf("e2e alerting: %T %s could not be removed: %v", object, alDiscoveryRole, err)
			}
		}
	}
	if a.workDir != "" {
		if err := os.RemoveAll(a.workDir); err != nil {
			t.Errorf("e2e alerting: the work directory could not be removed: %v", err)
		}
	}
}
