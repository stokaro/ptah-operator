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

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "capacity:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		kubeconfig      = flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "kubeconfig of the lab cluster")
		checkpointPath  = flag.String("checkpoint-probe", "", "Python database verifier for soak checkpoints")
		checkpointState = flag.String("checkpoint-state", "", "owned capacity database state for soak checkpoints")
		catalogPath     = flag.String("inputs", "", "populated per-slot input catalog prepared by the capacity harness")
		workloadPath    = flag.String("workload", "support/capacity/workload.json", "the workload to run")
		outDir          = flag.String("out", "", "directory to write report.json and summary.md into")
		hostPath        = flag.String("host-info", "", "JSON capacity reading from the Docker daemon hosting the lab")
		apiCount        = flag.Int("expected-api-servers", 3, "required number of independently sampled control-plane API servers")
		managerCount    = flag.Int("expected-managers", 2, "required number of independently sampled manager processes")
		metricsPort     = flag.Int("metrics-port", 8080, "the manager's metrics port")
		schemaV1        = flag.String("schema-v1", "", "the first schema artifact, as an oci:// reference by digest")
		schemaV2        = flag.String("schema-v2", "", "the schema artifact the change batch moves to")
		migrationV1     = flag.String("migration-v1", "", "the first migration artifact")
		migrationV2     = flag.String("migration-v2", "", "the migration artifact the change batch moves to")
		in              inputs
	)
	flag.StringVar(&in.namespace, "namespace", "", "comma-separated workload namespaces; the first also holds the restart approval fixture")
	flag.StringVar(&in.operatorNamespace, "operator-namespace", "", "the namespace the manager runs in")
	flag.StringVar(&in.managerSelector, "manager-selector", "app.kubernetes.io/component=controller", "label selector for the manager Pods")
	flag.StringVar(&in.registrySecret, "registry-secret", "demo-registry", "Secret holding the registry credentials")
	flag.StringVar(&in.schemaPolicy, "schema-policy", "demo-verification-policy", "verification policy ConfigMap for schemas")
	flag.StringVar(&in.migrationPolicy, "migration-policy", "demo-migration-verification-policy", "verification policy ConfigMap for migrations")
	flag.StringVar(&in.databaseSecret, "database-secret", "capacity-db-%d", "Secret name pattern, one database per resource")
	flag.StringVar(&in.registryIP, "registry-ip", "", "the registry's address, for the outage")
	flag.Parse()
	if *apiCount < 1 {
		return errors.New("expected-api-servers must be positive")
	}
	if *managerCount < 1 {
		return errors.New("expected-managers must be positive")
	}

	in.schemaRefs = [2]string{*schemaV1, *schemaV2}
	in.migrationRefs = [2]string{*migrationV1, *migrationV2}
	var err error
	in.namespaces, err = parseNamespaces(in.namespace)
	if err != nil {
		return err
	}
	in.namespace = in.namespaces[0]
	load, err := loadWorkload(*workloadPath)
	if err != nil {
		return err
	}
	var catalogDigest string
	if *catalogPath != "" {
		in.catalog, catalogDigest, err = readInputCatalog(*catalogPath, load, os.Getenv("E2E_PTAH_REVISION"))
		if err != nil {
			return err
		}
	}
	if err := requireInputs(in, load, *outDir); err != nil {
		return err
	}
	var checkpoint func(context.Context, int, string) (databaseCheckpoint, error)
	if load.Soak != nil {
		checkpoint, err = checkpointProbe(*checkpointPath, *checkpointState, filepath.Dir(*catalogPath), in.catalog)
		if err != nil {
			return err
		}
	}
	host, err := readHostCapacity(*hostPath)
	if err != nil {
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

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return err
	}
	if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	inputReader, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	environment, err := describeEnvironment(ctx, clientset, in)
	if err != nil {
		return err
	}
	if in.catalog != nil {
		environment["inputCatalogSHA256"] = catalogDigest
		environment["inputCatalog"] = in.catalog
	}
	environment["expectedAPIServers"] = *apiCount
	recordHostCapacity(environment, host)
	environment["expectedManagers"] = *managerCount
	watch := &sampler{
		expectedAPIServers: *apiCount,
		expectedManagers:   *managerCount,
		scrapeAPI: func(ctx context.Context, pod corev1.Pod) (scrape, error) {
			return scrapeAPIPod(ctx, config, clientset, pod)
		},
		clientset: clientset, dynamic: dynamicClient,
		namespace: in.namespace, namespaces: in.namespaces, operatorNamespace: in.operatorNamespace,
		selector: capacityLabel + "=" + load.Name, managerSelector: in.managerSelector,
		metricsPort: *metricsPort, every: load.SampleEvery.Duration,
		jobs: map[string]*jobRecord{},
	}
	workCtx, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	var recorders []*cycleRecorder
	for _, namespace := range in.namespaces {
		for _, family := range []struct {
			name   string
			client dynamic.ResourceInterface
		}{
			{"schema", dynamicClient.Resource(schemaResource).Namespace(namespace)},
			{"migration", dynamicClient.Resource(migrationResource).Namespace(namespace)},
		} {
			recorder := newCycleRecorder(family.client, family.name, namespace, capacityLabel+"="+load.Name)
			recorders = append(recorders, recorder)
			go recorder.run(workCtx)
			go func() {
				<-recorder.done
				if recorder.snapshot().Error != "" {
					cancelWork()
				}
			}()
		}
	}
	var setupErr error
	for _, recorder := range recorders {
		select {
		case err := <-recorder.ready:
			setupErr = errors.Join(setupErr, err)
		case <-workCtx.Done():
			setupErr = errors.Join(setupErr, workCtx.Err())
		}
	}
	// Work may finish or fail while a sample is in flight. Keep collection
	// under the parent cancellation signal and stop it through its finish
	// channel, so normal scenario completion cannot truncate that sample.
	sampling, stopSampling := context.WithCancel(ctx)
	defer stopSampling()
	finishSampling := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- watch.run(sampling, finishSampling) }()

	steps := &scenarios{sampleSnapshot: func() []sample { samples, _ := watch.snapshot(); return samples }, checkpoint: checkpoint, evidenceDir: *outDir, restartJobs: func() []jobRecord {
		_, jobs := watch.snapshot()
		return jobs
	}, inputReader: inputReader, in: in, load: load, clientset: clientset, dynamic: dynamicClient, recorders: recorders}
	scenarioErr := setupErr
	if scenarioErr == nil {
		scenarioErr = runScenarios(workCtx, steps)
	}
	cancelWork()
	for _, recorder := range recorders {
		<-recorder.done
	}
	cycleProof := collectCycleEvidence(recorders)
	if load.Soak != nil && steps.soakWindow != nil {
		scenarioErr = errors.Join(scenarioErr, steps.validateSoakCycles(cycleProof), steps.validateRetentionPlateau())
	}
	environment["databaseCheckpoints"] = steps.databaseCheckpoints
	environment["churn"] = steps.churnProofs
	environment["retention"] = steps.retentionProofs
	for _, history := range cycleProof.Histories {
		if history.Error != "" {
			scenarioErr = errors.Join(scenarioErr, fmt.Errorf("%s cycles: %s", history.Family, history.Error))
		}
	}
	close(finishSampling)
	if err := <-done; err != nil {
		scenarioErr = errors.Join(scenarioErr, fmt.Errorf("final capacity collection: %w", err))
	}

	if in.catalog != nil {
		environment["inputPlans"] = steps.inputPlans
	}
	samples, jobs := watch.snapshot()
	out := report{Cycles: cycleProof, FormatVersion: 3, Workload: load, Environment: environment, Samples: samples, Jobs: jobs}
	for _, w := range steps.windows {
		reading := cost(w, samples, jobs)
		reading.RefreshCycles, reading.CycleProblems = cyclesInWindow(w, cycleProof)
		if len(reading.CycleProblems) > 0 {
			reading.Incomplete[sourceCycles] = len(reading.CycleProblems)
		}
		out.Scenarios = append(out.Scenarios, reading)
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
	if steps.load.Soak != nil {
		for _, step := range []struct {
			name string
			run  func(context.Context) error
		}{
			{"cold start", steps.create}, {"soak", steps.soak}, {"approval gate", steps.prepareApproval}, {"restart burst", steps.restart}, {"registry outage", steps.outage},
		} {
			slog.Info("scenario", "name", step.name)
			if err := step.run(ctx); err != nil {
				return fmt.Errorf("%s: %w", step.name, err)
			}
		}
		return nil
	}
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
	if load.Soak != nil && in.catalog == nil {
		missing = append(missing, "-inputs for soak")
	}
	for name, value := range map[string]string{
		"-namespace": in.namespace, "-operator-namespace": in.operatorNamespace, "-out": outDir,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if in.catalog == nil && load.Schemas > 0 && (in.schemaRefs[0] == "" || load.ChangeBatch > 0 && in.schemaRefs[1] == "") {
		missing = append(missing, "-schema-v1/-schema-v2")
	}
	if in.catalog == nil && (in.migrationRefs[0] == "" || load.ChangeBatch > 0 && in.migrationRefs[1] == "") {
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
	out := map[string]any{"measuredAt": time.Now().UTC().Format(time.RFC3339), "workloadNamespaces": workloadNamespaces(in.namespace, in.namespaces)}
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
