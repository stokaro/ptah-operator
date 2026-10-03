//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/crdupgrade"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

func (a *alertingRun) upgradeAlerts() {
	intent, hookAccount, chart, values, valuesPath := a.upgradeCandidate()
	evidence, err := os.MkdirTemp("", "ptah-e2e-upgrade-evidence.")
	a.check(err, "create private retained upgrade evidence")
	a.logf("retained private upgrade evidence: %s", evidence)
	a.check(os.WriteFile(filepath.Join(evidence, "candidate.tgz"), chart, 0600), "retain exact candidate bytes")
	a.check(os.WriteFile(filepath.Join(evidence, "values.yaml"), values, 0600), "retain exact values bytes")
	probes, checkProbes, recoveryBoundary, cleanup := a.upgradeProbes(evidence)
	defer func() {
		cleanup()
		if a.t.Failed() {
			return
		}
		hashes := map[string]string{}
		a.check(filepath.WalkDir(evidence, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(evidence, path)
			if err != nil {
				return err
			}
			hashes[filepath.ToSlash(relative)] = alUpgradeDigest(body)
			return nil
		}), "checksum the complete private upgrade evidence")
		a.retainUpgradeEvidence(evidence, "manifest.json", mustJSONBytes(hashes))
	}()
	for _, object := range probes {
		kind := "PtahSchema"
		if _, ok := object.(*ptahv1.PtahMigration); ok {
			kind = "PtahMigration"
		}
		intent.Probes = append(intent.Probes, alUpgradeProbe{Kind: kind, Namespace: object.GetNamespace(), Name: object.GetName(), UID: string(object.GetUID()), Generation: object.GetGeneration()})
	}
	for _, mode := range []string{"failed", "deadline"} {
		a.upgradeAlertCase(intent, hookAccount, chart, values, valuesPath, mode, probes, checkProbes, recoveryBoundary, evidence)
	}
}

func (a *alertingRun) upgradeCandidate() (alUpgradeIntent, string, []byte, []byte, string) {
	intent := alUpgradeIntent{Namespace: a.in.OperatorNamespace, Release: a.in.HelmRelease, Manager: a.manager, CRDDigests: map[string]string{}}
	chart, err := os.ReadFile(a.in.ChartPackage)
	a.check(err, "retain the exact upgrade chart")
	values, err := a.cluster.Helm(a.ctx, "-n", a.in.OperatorNamespace, "get", "values", a.in.HelmRelease, "-o", "yaml")
	a.check(err, "retain the actual installed upgrade values")
	valuesPath := filepath.Join(a.workDir, "upgrade-alert-values.yaml")
	a.check(os.WriteFile(valuesPath, values, 0600), "retain byte-identical retry values")
	intent.ChartDigest, intent.ValuesDigest = alUpgradeDigest(chart), alUpgradeDigest(values)
	render, err := a.cluster.Helm(a.ctx, "template", a.in.HelmRelease, a.in.ChartPackage, "--namespace", a.in.OperatorNamespace, "--values", valuesPath, "--show-only", "templates/crd-upgrade.yaml")
	a.check(err, "render the candidate hook")
	name, err := lifecycleReconcileHookName(render)
	a.check(err, "identify exactly one reconcile hook")
	var hook *batchv1.Job
	decode := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(render), 4096)
	for {
		var raw map[string]any
		err := decode.Decode(&raw)
		if err == io.EOF {
			break
		}
		a.check(err, "read candidate hook documents")
		if raw["kind"] != "Job" {
			continue
		}
		job := &batchv1.Job{}
		a.check(runtime.DefaultUnstructuredConverter.FromUnstructured(raw, job), "decode candidate hook")
		if job.Name == name {
			if hook != nil {
				a.fatalf("candidate repeated its hook")
			}
			hook = job
		}
	}
	if hook == nil || hook.Namespace != intent.Namespace || len(hook.Spec.Template.Spec.Containers) != 1 || !alPinnedImage.MatchString(hook.Spec.Template.Spec.Containers[0].Image) {
		a.fatalf("candidate hook has no exact runtime")
	}
	intent.HookJob, intent.Image, intent.HookArgs = hook.Name, hook.Spec.Template.Spec.Containers[0].Image, hook.Spec.Template.Spec.Containers[0].Args
	list := &appsv1.DeploymentList{}
	a.check(a.cluster.Client.List(a.ctx, list, client.InNamespace(intent.Namespace)), "read upgrade runtime identities")
	for _, d := range list.Items {
		if d.Labels["app.kubernetes.io/instance"] == intent.Release && d.Labels["app.kubernetes.io/component"] == "certificate-rotation" {
			if intent.Rotator != "" {
				a.fatalf("ambiguous rotator")
			}
			intent.Rotator = d.Name
		}
	}
	if intent.Rotator == "" {
		a.fatalf("no candidate rotator")
	}
	crds, err := a.cluster.Helm(a.ctx, "show", "crds", a.in.ChartPackage)
	a.check(err, "read the packaged candidate CRDs")
	decode = utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(crds), 4096)
	for {
		var crd apiextensionsv1.CustomResourceDefinition
		err := decode.Decode(&crd)
		if err == io.EOF {
			break
		}
		a.check(err, "decode candidate CRD")
		if crd.Name == "" {
			continue
		}
		digest, err := crdupgrade.ComputeSchemaDigest(&crd)
		a.check(err, "derive candidate schema digest")
		if crd.Annotations[crdupgrade.SchemaDigestAnnotation] != digest || intent.CRDDigests[crd.Name] != "" {
			a.fatalf("candidate CRD digest is absent, duplicated or incorrect")
		}
		intent.CRDDigests[crd.Name] = digest
	}
	if len(intent.CRDDigests) != len(crdupgrade.Names()) {
		a.fatalf("candidate does not contain the complete CRD inventory")
	}
	return intent, hook.Spec.Template.Spec.ServiceAccountName, chart, values, valuesPath
}

func (a *alertingRun) upgradeAlertCase(intent alUpgradeIntent, hookAccount string, chart, values []byte, valuesPath, mode string, probes []client.Object, checkProbes func(), recoveryBoundary func(time.Time) time.Time, evidence string) {
	directory := filepath.Join(evidence, mode)
	a.check(os.Mkdir(directory, 0700), "create unique fault evidence directory")
	hashes := map[string]string{}
	retain := func(name string, body []byte) {
		a.retainUpgradeEvidence(directory, name, body)
		hashes[name] = alUpgradeDigest(body)
	}
	retain("intent.json", mustJSONBytes(intent))
	kubeconfig := []byte("apiVersion: v1\nkind: Config\ncurrent-context: observer\nclusters:\n- name: cluster\n  cluster:\n    server: https://kubernetes.default.svc\n    certificate-authority: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt\nusers:\n- name: observer\n  user:\n    tokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token\ncontexts:\n- name: observer\n  context:\n    cluster: cluster\n    user: observer\n")
	objects, err := alUpgradeObserverObjects(intent, a.in.FixtureImage, chart, values, kubeconfig)
	a.check(err, "build the independent observer")
	for _, object := range objects {
		a.check(a.create(object), "create %T %s", object, object.GetName())
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for i := len(objects) - 1; i >= 0; i-- {
			v := objects[i]
			uid := v.GetUID()
			if err := a.cluster.Client.Delete(ctx, v, client.Preconditions{UID: &uid}, client.PropagationPolicy(metav1.DeletePropagationForeground)); client.IgnoreNotFound(err) != nil {
				a.t.Errorf("remove upgrade observer %T: %v", v, err)
			} else if err := storedStateDeleteExact(ctx, a.cluster, v); err != nil {
				a.t.Errorf("wait for upgrade observer removal: %v", err)
			}
		}
	}()
	a.check(a.cluster.WaitForRollout(a.ctx, alMonitoringNamespace, alUpgradeObserver, alTimeout), "start the independent observer")
	pods := &corev1.PodList{}
	a.check(a.cluster.Client.List(a.ctx, pods, client.InNamespace(alMonitoringNamespace), client.MatchingLabels{"app": alUpgradeObserver}), "read observer Pod")
	if len(pods.Items) != 1 || !harness.PodReady(&pods.Items[0]) {
		a.fatalf("observer has no unique ready Pod")
	}
	observer := pods.Items[0].DeepCopy()
	monitor := a.negativeMonitorIdentity()
	check := func() {
		checkProbes()
		current := &corev1.Pod{}
		a.check(a.cluster.Client.Get(a.ctx, client.ObjectKeyFromObject(observer), current), "read the original observer")
		if current.UID != observer.UID || current.DeletionTimestamp != nil || current.Status.Phase != corev1.PodRunning || !noRestarts(current) || !maps.Equal(monitor, a.negativeMonitorIdentity()) {
			a.fatalf("upgrade changed an independent observation process")
		}
	}
	var original *alUpgradeState
	read := func() alUpgradeState {
		check()
		body, _, err := a.cluster.Kubectl(a.ctx, "-n", alMonitoringNamespace, "exec", observer.Name, "-c", alUpgradeObserver, "--", "/e2e-upgrade-observer", "inspect", "--state", "/data/state.json")
		a.check(err, "inspect retained upgrade state")
		s, err := alUpgradeReadState(body, intent, original)
		a.check(err, "validate immutable upgrade transaction")
		return s
	}
	initial := read()
	original = &initial
	retain("initial-state.json", mustJSONBytes(initial))
	if len(initial.Attempts) != 0 || initial.FailedAt != nil || initial.RecoveredAt != nil || initial.StartedAt.Before(observer.CreationTimestamp.Time) || initial.Deadline.Before(time.Now().Add(14*time.Minute)) {
		a.fatalf("observer did not start a fresh pre-Helm transaction")
	}
	restoreRules := a.loadUpgradeMonitoring()
	for name, path := range map[string]string{"effective-config.json": "/api/v1/status/config", "effective-rules.json": "/api/v1/rules"} {
		body, err := a.prometheus(a.ctx, path, nil)
		a.check(err, "retain actual monitoring configuration")
		retain(name, body)
	}
	defer restoreRules()
	a.check(harness.Wait(a.ctx, "a healthy external observer scrape", time.Minute, time.Second, func(context.Context) (bool, string, error) {
		check()
		return a.upgradeGauge("up", 1) && a.upgradeGauge("ptah_operator_upgrade_observer_ready", 1) && a.upgradeGauge("ptah_operator_upgrade_pending", 1) && a.upgradeGauge("ptah_operator_upgrade_deadline_seconds", float64(initial.Deadline.UnixNano())/1e9), "waiting for native observer scrape", nil
	}), "scrape the observer before fault injection")
	watcher, err := client.NewWithWatch(a.cluster.Config, client.Options{Scheme: a.cluster.Scheme})
	a.check(err, "open independent upgrade hook history")
	jobs := newStoredStateRecorder[*batchv1.Job](a.t, a.ctx, watcher, "upgrade-"+mode+"-hooks", intent.Namespace, func() client.ObjectList { return &batchv1.JobList{} })
	checkHistory := func() { check(); a.check(jobs.alive(), "retain every transient hook event") }
	policy, binding := alUpgradeFaultPolicy(intent, hookAccount, mode)
	a.check(a.create(policy), "create the upgrade fault policy")
	a.check(a.create(binding), "bind the upgrade fault policy")
	removeFault := func() {
		for _, object := range []client.Object{binding, policy} {
			uid := object.GetUID()
			a.check(client.IgnoreNotFound(a.cluster.Client.Delete(a.ctx, object, client.Preconditions{UID: &uid})), "remove the exact upgrade fault")
		}
	}
	defer removeFault()
	a.confirmUpgradeFault(policy.Name, mode, intent, hookAccount)
	from := a.deliveryCount()
	attempt := 0
	helm := func() ([]byte, error) {
		attempt++
		gotChart, err := os.ReadFile(a.in.ChartPackage)
		a.check(err, "read retry chart")
		gotValues, err := os.ReadFile(valuesPath)
		a.check(err, "read retry values")
		if alUpgradeDigest(gotChart) != intent.ChartDigest || alUpgradeDigest(gotValues) != intent.ValuesDigest {
			a.fatalf("retry changed the declared candidate")
		}
		ctx, cancel := context.WithTimeout(a.ctx, 8*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "helm", "--kubeconfig", a.cluster.Kubeconfig, "upgrade", intent.Release, a.in.ChartPackage, "--namespace", intent.Namespace, "--values", valuesPath, "--force-conflicts", "--wait", "--timeout", "7m")
		a.logf("upgrade command: argv=%s chart=%s values=%s", mustJSONBytes(cmd.Args), intent.ChartDigest, intent.ValuesDigest)
		output, err := cmd.CombinedOutput()
		retain(fmt.Sprintf("helm-attempt-%d.log", attempt), output)
		retain(fmt.Sprintf("helm-attempt-%d-argv.json", attempt), mustJSONBytes(cmd.Args))
		return output, err
	}
	output, err := helm()
	if err == nil {
		a.fatalf("the %s upgrade fault was not reached", mode)
	}
	if !bytes.Contains(output, []byte(policy.Name)) {
		a.fatalf("failed upgrade did not name the injected policy; output digest=%s", alUpgradeDigest(output))
	}
	a.logf("upgrade %s failed against policy %s; command output digest=%s", mode, policy.Name, alUpgradeDigest(output))
	var fault alUpgradeState
	a.check(harness.Wait(a.ctx, "the original hook's retained terminal state", time.Minute, time.Second, func(context.Context) (bool, string, error) {
		checkHistory()
		fault = read()
		_, err := alUpgradeIncident(fault, jobs.snapshot(), mode)
		return err == nil, "waiting for the independent terminal hook event", nil
	}), "retain the actual upgrade fault")
	retain("fault-state.json", mustJSONBytes(fault))
	trigger, err := alUpgradeIncident(fault, jobs.snapshot(), mode)
	a.check(err, "date the external incident from its original event")
	if mode == "deadline" {
		a.requireUpgradeRuntimesStopped(intent)
	}
	labels := map[string]string{"operator_namespace": intent.Namespace, "release": intent.Release}
	firing, index := a.waitForDeliveryWithCheck(alMatch{status: "firing", alertName: alUpgradeAlert, labels: labels}, "the external upgrade notification", max(time.Second, time.Until(trigger.Add(alDetectionSlack))), from, func() {
		checkHistory()
		s := read()
		if s.RecoveredAt != nil || len(s.Attempts) != 1 {
			a.fatalf("upgrade recovered or retried before the notification")
		}
		if mode == "deadline" {
			a.requireUpgradeRuntimesStopped(intent)
		}
	})
	if firing.StartsAt.Before(trigger) || firing.ReceivedAt.Before(firing.StartsAt) || firing.ReceivedAt.After(trigger.Add(alDetectionSlack)) || firing.Labels["severity"] != "critical" || firing.Annotations["runbook_url"] != "https://operator.ptah.run/use/operations/#retry-upgrade" {
		a.fatalf("upgrade notification missed its underlying event or frozen delivery bound")
	}
	if !a.upgradeGauge("ptah_operator_upgrade_pending", 1) || mode == "failed" && !a.upgradeGauge("ptah_operator_upgrade_failed", 1) {
		a.fatalf("upgrade notification lacks the actual observer signal")
	}
	retain("firing.json", mustJSONBytes(firing))
	removeFault()
	output, err = helm()
	a.check(err, "retry identical chart, image and values; output digest=%s", alUpgradeDigest(output))
	var recovered alUpgradeState
	a.check(harness.Wait(a.ctx, "verified admission and workload recovery", 5*time.Minute, time.Second, func(context.Context) (bool, string, error) {
		checkHistory()
		recovered = read()
		return recovered.RecoveredAt != nil, "waiting for the observer's complete recovery proof", nil
	}), "restore the declared candidate")
	if len(recovered.Attempts) != 2 || recovered.Attempts[1].CompletedAt == nil || recovered.RecoveryJobUID != recovered.Attempts[1].UID || recovered.Attempts[0].UID != fault.Attempts[0].UID || recovered.Attempts[1].UID == fault.Attempts[0].UID {
		a.fatalf("recovery has no distinct successful retry of the retained incident")
	}
	// Fresh stored progress and the original runtime Pod transitions date the
	// healthy installation independently of the observer's polling timestamp.
	healthyAt := recoveryBoundary(*recovered.Attempts[1].CompletedAt)
	runtimeAt := a.upgradeRuntimeBoundary(intent, probes, *recovered.Attempts[1].CompletedAt, retain)
	if runtimeAt.After(healthyAt) {
		healthyAt = runtimeAt
	}
	resolved, _ := a.waitForDeliveryWithCheck(alMatch{status: "resolved", alertName: alUpgradeAlert, labels: labels}, "the external upgrade resolution", alDetectionSlack, index+1, checkHistory)
	if !resolved.StartsAt.Equal(firing.StartsAt) || resolved.EndsAt.Before(healthyAt) || resolved.ReceivedAt.After(healthyAt.Add(alDetectionSlack)) || resolved.ReceivedAt.Before(resolved.EndsAt) || !a.upgradeGauge("ptah_operator_upgrade_pending", 0) {
		a.fatalf("upgrade resolution missed verified recovery or its original bound")
	}
	jobs.requestStop()
	a.check(jobs.await(45*time.Second), "close the complete upgrade hook history")
	_, err = alUpgradeIncident(fault, jobs.snapshot(), mode)
	a.check(err, "revalidate the closed original hook history")
	a.check(alUpgradeRetryEvidence(recovered, jobs.snapshot()), "bind retry recovery to its closed native Job history")
	retain("recovered-state.json", mustJSONBytes(recovered))
	retain("resolved.json", mustJSONBytes(resolved))
	retain("hook-history.json", mustJSONBytes(jobs.snapshot()))
	retain("manifest.json", mustJSONBytes(hashes))
	a.logf("PASS external upgrade %s: observerPod=%s initialHook=%s retryHook=%s trigger=%s firing=%s healthy=%s resolved=%s state=%s", mode, observer.UID, fault.Attempts[0].UID, recovered.RecoveryJobUID, trigger, firing.ReceivedAt, healthyAt, resolved.ReceivedAt, mustJSONBytes(recovered))
}

// Each file is new and private. A repeated write must not silently replace
// evidence from an earlier observation or attempt.
func (a *alertingRun) retainUpgradeEvidence(directory, name string, body []byte) {
	f, err := os.OpenFile(filepath.Join(directory, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	a.check(err, "create new upgrade evidence %s", name)
	defer f.Close()
	_, err = f.Write(body)
	a.check(err, "retain upgrade evidence")
	a.check(f.Sync(), "sync upgrade evidence")
	a.check(f.Close(), "close upgrade evidence")
}

func (a *alertingRun) requireUpgradeRuntimesStopped(intent alUpgradeIntent) {
	for _, name := range []string{intent.Manager, intent.Rotator} {
		d := &appsv1.Deployment{}
		a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: intent.Namespace, Name: name}, d), "read stopped runtime")
		if d.Spec.Replicas == nil || *d.Spec.Replicas != 0 || d.Status.Replicas != 0 || d.Status.ReadyReplicas != 0 {
			a.fatalf("interrupted upgrade did not keep %s stopped", name)
		}
	}
}

func (a *alertingRun) confirmUpgradeFault(name, mode string, intent alUpgradeIntent, hookAccount string) {
	// Require the intended API refusal before running the expensive hook.
	ctx, cancel := context.WithTimeout(a.ctx, time.Minute)
	defer cancel()
	a.check(harness.Wait(ctx, "the upgrade policy to enforce its own refusal", time.Minute, time.Second, func(context.Context) (bool, string, error) {
		if mode == "deadline" {
			d := &appsv1.Deployment{}
			if err := a.cluster.Client.Get(ctx, types.NamespacedName{Namespace: intent.Namespace, Name: intent.Manager}, d); err != nil {
				return false, "", err
			}
			err := a.cluster.Client.Update(ctx, d, client.DryRunAll)
			return err != nil && strings.Contains(err.Error(), name), "waiting for Deployment refusal", nil
		}
		current := &admissionv1.ValidatingAdmissionPolicy{}
		if err := a.cluster.Client.Get(ctx, client.ObjectKey{Name: name}, current); err != nil {
			return false, "", err
		}
		if current.Status.ObservedGeneration != current.Generation {
			return false, "waiting for policy observation", nil
		}
		if current.Status.TypeChecking != nil && len(current.Status.TypeChecking.ExpressionWarnings) != 0 {
			return false, "", fmt.Errorf("upgrade fault policy has expression warnings")
		}
		return true, "", nil
	}), "prove the exact upgrade fault is active")
}

func mustJSONBytes(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func (a *alertingRun) upgradeGauge(metric string, want float64) bool {
	selector := fmt.Sprintf(`%s{job="ptah-upgrade-observer"}`, metric)
	query := fmt.Sprintf(`%s and (timestamp(%s) > time() - 15) and (timestamp(%s) <= time())`, selector, selector, selector)
	body, err := a.prometheus(a.ctx, "/api/v1/query", map[string]string{"query": query})
	a.check(err, "read native external upgrade metric")
	return alUpgradeGauge(body, want, time.Now())
}

func (a *alertingRun) loadUpgradeMonitoring() func() {
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: alMonitoringNamespace, Name: "prometheus"}
	a.check(a.cluster.Client.Get(a.ctx, key, cm), "read installed monitoring configuration")
	original := maps.Clone(cm.Data)
	rules, err := os.ReadFile(filepath.Join(repositoryRoot, "hack", "upgradealert", "rules.yaml"))
	a.check(err, "read actual external upgrade rules")
	var config map[string]any
	a.check(yaml.Unmarshal([]byte(cm.Data["prometheus.yml"]), &config), "decode installed scrape configuration")
	config["rule_files"] = append(config["rule_files"].([]any), "/etc/prometheus/upgrade-rules.yaml")
	config["scrape_configs"] = append(config["scrape_configs"].([]any), map[string]any{"job_name": "ptah-upgrade-observer", "static_configs": []any{map[string]any{"targets": []any{alUpgradeObserver + "." + alMonitoringNamespace + ".svc:9812"}}}})
	encoded, err := yaml.Marshal(config)
	a.check(err, "encode external observer scrape")
	desired := maps.Clone(cm.Data)
	desired["prometheus.yml"] = string(encoded)
	desired["upgrade-rules.yaml"] = string(rules)
	load := func(data map[string]string, enabled bool) {
		a.check(a.cluster.Client.Get(a.ctx, key, cm), "read monitoring configuration before reload")
		cm.Data = data
		a.check(a.cluster.Client.Update(a.ctx, cm), "update external observation configuration")
		a.check(harness.Wait(a.ctx, "the exact external rule configuration to load", alTimeout, time.Second, func(context.Context) (bool, string, error) {
			_, err := a.cluster.Clientset.CoreV1().RESTClient().Post().Namespace(alMonitoringNamespace).Resource("services").Name("http:prometheus:9090").SubResource("proxy").Suffix("-/reload").DoRaw(a.ctx)
			if err != nil {
				return false, "", err
			}
			body, err := a.prometheus(a.ctx, "/api/v1/status/config", nil)
			if err != nil {
				return false, "", err
			}
			var response struct {
				Data struct {
					YAML string `json:"yaml"`
				} `json:"data"`
			}
			if err := json.Unmarshal(body, &response); err != nil {
				return false, "", err
			}
			found := strings.Contains(response.Data.YAML, "ptah-upgrade-observer")
			return found == enabled && (!enabled || a.upgradeRulesLoaded(rules)), "waiting for the projected monitoring configuration and evaluated rules", nil
		}), "load the external upgrade rule and target")
	}
	load(desired, true)
	return func() { load(original, false) }
}

// Compare the effective rules through the Prometheus image's own parser.
// Formatting differences cannot hide a changed expression or hold duration.
func (a *alertingRun) upgradeRulesLoaded(source []byte) bool {
	var expected struct {
		Groups []struct {
			Rules []struct {
				Alert       string            `json:"alert"`
				Expr        string            `json:"expr"`
				For         string            `json:"for"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"rules"`
		} `json:"groups"`
	}
	a.check(yaml.Unmarshal(source, &expected), "read expected external rules")
	body, err := a.prometheus(a.ctx, "/api/v1/rules", nil)
	a.check(err, "read effective upgrade rules")
	if !alRulesLoaded(body) {
		return false
	}
	var loaded struct {
		Data struct {
			Groups []struct {
				Rules []struct {
					Name        string            `json:"name"`
					Type        string            `json:"type"`
					Query       string            `json:"query"`
					Duration    float64           `json:"duration"`
					Labels      map[string]string `json:"labels"`
					Annotations map[string]string `json:"annotations"`
				} `json:"rules"`
			} `json:"groups"`
		} `json:"data"`
	}
	a.check(json.Unmarshal(body, &loaded), "decode effective upgrade rules")
	canonical := func(query string) string {
		out, _, err := a.cluster.Kubectl(a.ctx, "-n", alMonitoringNamespace, "exec", "deployment/prometheus", "--", "/bin/promtool", "--experimental", "promql", "format", query)
		a.check(err, "parse rule with the monitored Prometheus version")
		return string(out)
	}
	count := 0
	for _, group := range expected.Groups {
		for _, want := range group.Rules {
			count++
			found := 0
			hold := time.Duration(0)
			if want.For != "" {
				hold, err = time.ParseDuration(want.For)
				a.check(err, "parse the external rule hold")
			}
			for _, group := range loaded.Data.Groups {
				for _, got := range group.Rules {
					if got.Name != want.Alert {
						continue
					}
					found++
					if got.Type != "alerting" || got.Duration != hold.Seconds() || !maps.Equal(got.Labels, want.Labels) || !maps.Equal(got.Annotations, want.Annotations) || canonical(got.Query) != canonical(want.Expr) {
						a.fatalf("effective external rule %s differs from the source", want.Alert)
					}
				}
			}
			if found != 1 {
				return false
			}
		}
	}
	if count != 2 {
		a.fatalf("external rule inventory changed without a proof")
	}
	return true
}
