// Command capacity measures what the operator costs under a declared workload.
//
// It runs against a lab cluster the harness prepared -- hack/capacity.sh brings
// one up and publishes the artifacts -- creates the workload the file names,
// and walks it through the conditions every installation meets: a cold start,
// a steady state, a restart of every manager at once, a batch of changes, and
// a registry nobody can reach. Throughout, it samples the cluster: operation
// Jobs from creation to their end, Pods by phase, how stale each resource's
// last reading is against its own timestamps, the manager's memory, CPU, queue
// and client throttling, the API server's admission latency, and the plans and
// chunks left behind.
//
// Nothing in the report is typed beside the measurement. Every figure is a
// reading, and the report carries the workload and the environment it was
// taken under, because a figure without them says nothing about another
// installation.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "capacity:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		kubeconfig   = flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "kubeconfig of the lab cluster")
		workloadPath = flag.String("workload", "support/capacity/workload.json", "the workload to run")
		outDir       = flag.String("out", "", "directory to write report.json and summary.md into")
		metricsPort  = flag.Int("metrics-port", 8080, "the manager's metrics port")
		schemaV1     = flag.String("schema-v1", "", "the first schema artifact, as an oci:// reference by digest")
		schemaV2     = flag.String("schema-v2", "", "the schema artifact the change batch moves to")
		migrationV1  = flag.String("migration-v1", "", "the first migration artifact")
		migrationV2  = flag.String("migration-v2", "", "the migration artifact the change batch moves to")
		in           inputs
	)
	flag.StringVar(&in.namespace, "namespace", "", "the namespace the workload runs in")
	flag.StringVar(&in.operatorNamespace, "operator-namespace", "", "the namespace the manager runs in")
	flag.StringVar(&in.managerSelector, "manager-selector", "app.kubernetes.io/component=controller", "label selector for the manager Pods")
	flag.StringVar(&in.registrySecret, "registry-secret", "demo-registry", "Secret holding the registry credentials")
	flag.StringVar(&in.schemaPolicy, "schema-policy", "demo-verification-policy", "verification policy ConfigMap for schemas")
	flag.StringVar(&in.migrationPolicy, "migration-policy", "demo-migration-verification-policy", "verification policy ConfigMap for migrations")
	flag.StringVar(&in.databaseSecret, "database-secret", "capacity-db-%d", "Secret name pattern, one database per resource")
	flag.StringVar(&in.registryIP, "registry-ip", "", "the registry's address, for the outage")
	flag.Parse()

	in.schemaRefs = [2]string{*schemaV1, *schemaV2}
	in.migrationRefs = [2]string{*migrationV1, *migrationV2}
	load, err := loadWorkload(*workloadPath)
	if err != nil {
		return err
	}
	if err := requireInputs(in, load, *outDir); err != nil {
		return err
	}
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		return fmt.Errorf("read the kubeconfig: %w", err)
	}
	// The tool reads the cluster more often than a controller would, and a
	// throttled reader would report its own queueing as the operator's.
	config.QPS, config.Burst = 50, 100
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	environment, err := describeEnvironment(ctx, clientset, in)
	if err != nil {
		return err
	}
	watch := &sampler{
		clientset: clientset, dynamic: dynamicClient,
		namespace: in.namespace, operatorNamespace: in.operatorNamespace,
		selector: capacityLabel + "=" + load.Name, managerSelector: in.managerSelector,
		metricsPort: *metricsPort, every: load.SampleEvery.Duration,
		jobs: map[string]*jobRecord{},
	}
	sampling, stopSampling := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { watch.run(sampling); close(done) }()

	steps := &scenarios{in: in, load: load, clientset: clientset, dynamic: dynamicClient}
	scenarioErr := runScenarios(ctx, steps)
	stopSampling()
	<-done

	samples, jobs := watch.snapshot()
	out := report{Workload: load, Environment: environment, Samples: samples, Jobs: jobs}
	for _, w := range steps.windows {
		out.Scenarios = append(out.Scenarios, cost(w, samples, jobs))
	}
	if err := writeReport(*outDir, out); err != nil {
		return errors.Join(scenarioErr, err)
	}
	return scenarioErr
}

// runScenarios walks the workload through each condition in order. A scenario
// that fails ends the run, and the report still carries every window measured
// up to it: a workload that did not converge is itself a finding.
func runScenarios(ctx context.Context, steps *scenarios) error {
	for _, step := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"cold start", steps.create},
		{"steady state", steps.steady},
		{"approval gate", steps.prepareApproval},
		{"restart burst", steps.restart},
		{"change batch", steps.change},
		{"registry outage", steps.outage},
	} {
		slog.Info("scenario", "name", step.name)
		if err := step.run(ctx); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
	}
	return nil
}

func requireInputs(in inputs, load workload, outDir string) error {
	var missing []string
	for name, value := range map[string]string{
		"-namespace": in.namespace, "-operator-namespace": in.operatorNamespace, "-out": outDir,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if load.Schemas > 0 && (in.schemaRefs[0] == "" || load.ChangeBatch > 0 && in.schemaRefs[1] == "") {
		missing = append(missing, "-schema-v1/-schema-v2")
	}
	if in.migrationRefs[0] == "" || load.ChangeBatch > 0 && in.migrationRefs[1] == "" {
		missing = append(missing, "-migration-v1/-migration-v2")
	}
	if load.Outage.Duration > 0 && in.registryIP == "" {
		missing = append(missing, "-registry-ip")
	}
	for _, reference := range append(in.schemaRefs[:], in.migrationRefs[:]...) {
		if reference != "" && !strings.Contains(reference, "@sha256:") {
			return fmt.Errorf("%s is not pinned by digest", reference)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("missing %s", strings.Join(missing, ", "))
	}
	return nil
}

// describeEnvironment records what the figures were measured on: the cluster,
// how much it could give, and the manager that ran.
func describeEnvironment(ctx context.Context, clientset kubernetes.Interface, in inputs) (map[string]any, error) {
	out := map[string]any{"measuredAt": time.Now().UTC().Format(time.RFC3339)}
	version, err := clientset.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("read the server version: %w", err)
	}
	out["kubernetes"] = version.GitVersion
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	cpu, memory := resource.Quantity{}, resource.Quantity{}
	for _, node := range nodes.Items {
		cpu.Add(node.Status.Allocatable[corev1.ResourceCPU])
		memory.Add(node.Status.Allocatable[corev1.ResourceMemory])
	}
	out["nodes"] = len(nodes.Items)
	out["allocatableCPU"] = cpu.String()
	out["allocatableMemory"] = memory.String()
	pods, err := clientset.CoreV1().Pods(in.operatorNamespace).List(ctx, metav1.ListOptions{LabelSelector: in.managerSelector})
	if err != nil {
		return nil, err
	}
	out["managerReplicas"] = len(pods.Items)
	if len(pods.Items) > 0 {
		container := pods.Items[0].Spec.Containers[0]
		out["managerImage"] = container.Image
		out["managerMemoryLimit"] = container.Resources.Limits.Memory().String()
		out["managerCPURequest"] = container.Resources.Requests.Cpu().String()
	}
	return out, nil
}

func writeReport(dir string, r report) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	content, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), append(content, '\n'), 0o600); err != nil {
		return err
	}
	summary, err := os.Create(filepath.Join(dir, "summary.md")) //nolint:gosec // The directory is the caller's own argument.
	if err != nil {
		return err
	}
	defer summary.Close()
	return writeSummary(summary, r)
}
