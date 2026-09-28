//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// lifecycleRunningApply is the state the Apply held running across the
// next-release upgrade keeps: the barrier process and the database it holds
// its lock in, the fixture's plan, the Job and Pod the manager dispatched, and
// the readings the later checks compare against.
type lifecycleRunningApply struct {
	// barrierDatabase is RUNNING_APPLY_BARRIER_DATABASE, and barrierActive
	// RUNNING_APPLY_BARRIER_ACTIVE: set while the barrier may hold its lock.
	barrierDatabase string
	barrierActive   bool
	// The barrier process: its cancel, a channel closed once it has exited,
	// what Wait returned then, and what it wrote on standard error.
	barrierCancel context.CancelFunc
	barrierDone   chan struct{}
	barrierErr    error
	barrierStderr *bytes.Buffer

	// scanner holds the fixture's database password and URL, so no fixture
	// log is printed with either in it.
	scanner credentialScanner

	bundle   predecessorApplyBundle
	planName string
	planUID  types.UID
	jobName  string
	jobUID   string
	podName  string
	podUID   types.UID

	// stagedGap is the schema as it stood with the Job UID removed, which the
	// successor's adoption is compared against, and jobBeforeCleanup the Job's
	// evidence at the upgrade boundary.
	stagedGap        map[string]any
	jobBeforeCleanup []byte
}

// predecessorApplySQL is running_apply_postgres_query: one statement in the
// barrier's database, through the external PostgreSQL container, as its
// administrator. The password stays in the container's environment.
func (l *lifecycleRun) predecessorApplySQL(ctx context.Context, query string) (string, error) {
	command := exec.CommandContext(ctx, "docker", "--context", l.in.dockerContext, "exec", //nolint:gosec // Arguments, not a shell.
		l.in.externalPostgresContainerID, "sh", "-ec",
		`PGPASSWORD="$POSTGRES_PASSWORD"; export PGPASSWORD; exec psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$2" -Atqc "$1"`,
		"sh", query, l.runningApply.barrierDatabase)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// predecessorApplyBarrierDatabaseName is resolve_running_apply_database.
func (l *lifecycleRun) predecessorApplyBarrierDatabaseName() string {
	l.t.Helper()
	if l.in.externalPostgresCredentialsFile == "" {
		l.fatalf("E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE is required for the running Apply barrier")
	}
	content, err := os.ReadFile(l.in.externalPostgresCredentialsFile)
	if err != nil {
		l.fatalf("external PostgreSQL credentials name no database for the running Apply barrier")
	}
	database, err := predecessorApplyBarrierDatabase(content)
	if err != nil {
		l.fatalf("%v", err)
	}
	return database
}

// startRunningApplyBarrier takes the advisory lock the Apply's statement will
// wait for, in a psql session that holds it until it is terminated.
func (l *lifecycleRun) startRunningApplyBarrier() {
	l.t.Helper()
	state := &l.runningApply
	if state.barrierActive {
		l.fatalf("running Apply database barrier is already active")
	}
	state.barrierDatabase = l.predecessorApplyBarrierDatabaseName()
	ctx, cancel := context.WithCancel(context.Background())
	command := exec.CommandContext(ctx, "docker", "--context", l.in.dockerContext, "exec", //nolint:gosec // Arguments, not a shell.
		l.in.externalPostgresContainerID, "sh", "-ec",
		`PGPASSWORD="$POSTGRES_PASSWORD"; export PGPASSWORD; PGAPPNAME="$1"; export PGAPPNAME; exec psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$3" -v ON_ERROR_STOP=1 -Atqc "SELECT pg_advisory_lock($2); SELECT pg_sleep(900)"`,
		"sh", predecessorApplyBarrierApplication, strconv.FormatInt(predecessorApplyBarrierKey, 10), state.barrierDatabase)
	state.barrierStderr = &bytes.Buffer{}
	command.Stdout, command.Stderr = &bytes.Buffer{}, state.barrierStderr
	// A child of the Docker client that keeps a pipe open cannot hold Wait.
	command.WaitDelay = 10 * time.Second
	if err := command.Start(); err != nil {
		cancel()
		l.fatalf("running Apply database barrier could not start: %v", err)
	}
	done := make(chan struct{})
	state.barrierCancel, state.barrierDone = cancel, done
	go func() {
		state.barrierErr = command.Wait()
		close(done)
	}()
	state.barrierActive = true

	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		held, err := l.predecessorApplySQL(l.ctx, predecessorApplyHeldQuery())
		if err != nil {
			l.fatalf("could not inspect the running Apply database barrier: %v", err)
		}
		if predecessorApplyValue(held) == "1" {
			return
		}
		select {
		case <-done:
			l.logf("the barrier said: %s", strings.TrimSpace(state.barrierStderr.String()))
			l.fatalf("running Apply database barrier exited before acquiring its lock")
		default:
		}
		l.sleep(time.Second)
	}
	l.fatalf("running Apply database barrier did not acquire its lock")
}

// waitForPredecessorApplyBarrierContention waits for the Apply to block on
// the barrier's lock inside the engine.
func (l *lifecycleRun) waitForPredecessorApplyBarrierContention() {
	l.t.Helper()
	for deadline := time.Now().Add(120 * time.Second); time.Now().Before(deadline); {
		waiting, err := l.predecessorApplySQL(l.ctx, predecessorApplyContentionQuery())
		if err != nil {
			l.fatalf("could not inspect running Apply barrier contention: %v", err)
		}
		if predecessorApplyValue(waiting) == "1" {
			return
		}
		l.sleep(time.Second)
	}
	l.fatalf("the Apply did not block on the controlled database barrier")
}

// assertPredecessorApplyBarrierContended holds that the Apply is still waiting
// on the barrier.
func (l *lifecycleRun) assertPredecessorApplyBarrierContended() {
	l.t.Helper()
	waiting, err := l.predecessorApplySQL(l.ctx, predecessorApplyContentionQuery())
	if err != nil {
		l.fatalf("could not recheck running Apply barrier contention: %v", err)
	}
	if predecessorApplyValue(waiting) != "1" {
		l.fatalf("the Apply left the controlled database barrier before it was released")
	}
}

// releaseRunningApplyBarrier terminates the barrier's session, which releases
// its lock, and requires the session to end with the failure that says it was
// terminated rather than with the success of running out its sleep.
func (l *lifecycleRun) releaseRunningApplyBarrier() {
	l.t.Helper()
	state := &l.runningApply
	if !state.barrierActive {
		l.fatalf("running Apply database barrier is not active")
	}
	released, err := l.predecessorApplySQL(l.ctx, predecessorApplyReleaseQuery())
	if err != nil {
		l.fatalf("could not release the running Apply database barrier: %v", err)
	}
	if predecessorApplyValue(released) != "t" {
		l.fatalf("running Apply database barrier release did not terminate exactly one holder")
	}
	state.barrierActive = false
	select {
	case <-state.barrierDone:
	case <-time.After(2 * time.Minute):
		l.fatalf("running Apply database barrier did not end within 2m of its release")
	}
	if state.barrierErr == nil {
		l.fatalf("running Apply database barrier exited successfully instead of being explicitly released")
	}
	state.barrierCancel()
	state.barrierCancel, state.barrierDone = nil, nil
}

// cleanupRunningApply is the barrier's half of the exit trap: whatever
// happened, a barrier still holding its lock is terminated, and the process is
// stopped and reaped. A failure to release is reported rather than failing the
// phase, as the trap did.
func (l *lifecycleRun) cleanupRunningApply() {
	state := &l.runningApply
	if state.barrierActive {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		if _, err := l.predecessorApplySQL(ctx, predecessorApplyReleaseQuery()); err != nil {
			l.t.Logf("e2e crd: could not release the running Apply database barrier")
		}
		cancel()
		state.barrierActive = false
	}
	if state.barrierCancel != nil {
		state.barrierCancel()
		<-state.barrierDone
		state.barrierCancel, state.barrierDone = nil, nil
	}
}

// predecessorApplyDocument reads one object in the proof namespace as the API
// server stores it, so a jq presence check keeps its meaning.
func (l *lifecycleRun) predecessorApplyDocument(apiVersion, kind, name string) (map[string]any, error) {
	object := &unstructured.Unstructured{}
	object.SetAPIVersion(apiVersion)
	object.SetKind(kind)
	if err := l.cluster.Client.Get(l.ctx, types.NamespacedName{Namespace: l.in.proofNamespace, Name: name}, object); err != nil {
		return nil, err
	}
	return object.Object, nil
}

func (l *lifecycleRun) predecessorApplySchemaDocument() (map[string]any, error) {
	return l.predecessorApplyDocument("operator.ptah.run/v1alpha1", "PtahSchema", predecessorApplySchema)
}

func (l *lifecycleRun) predecessorApplyJobDocument(name string) (map[string]any, error) {
	return l.predecessorApplyDocument("batch/v1", "Job", name)
}

// predecessorApplyPod reads one Pod in the proof namespace.
func (l *lifecycleRun) predecessorApplyPod(name string) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	err := l.cluster.Client.Get(l.ctx, types.NamespacedName{Namespace: l.in.proofNamespace, Name: name}, pod)
	return pod, err
}

// createInProofNamespace creates an object the fixture owns.
func (l *lifecycleRun) createForPredecessorApply(object client.Object, what string) {
	l.t.Helper()
	l.check(l.cluster.Client.Create(l.ctx, object, client.FieldOwner(harness.FieldOwner)), "create %s", what)
}

// waitForPredecessorApplyFixtureJob is wait_for_successful_fixture_job. A
// failed Job's log is printed, unless it carries the fixture's database
// credential.
func (l *lifecycleRun) waitForPredecessorApplyFixtureJob(name string) {
	l.t.Helper()
	for deadline := time.Now().Add(300 * time.Second); time.Now().Before(deadline); {
		if job, err := l.predecessorApplyJobDocument(name); err == nil {
			if predecessorApplyHasCondition(job, "Complete", "True") {
				return
			}
			if predecessorApplyHasCondition(job, "Failed", "True") {
				if stdout, _, err := l.kubectl("-n", l.in.proofNamespace, "logs", "job/"+name); err == nil {
					if l.runningApply.scanner.ready() && l.runningApply.scanner.leaks(stdout) {
						l.logf("fixture Job %s log withheld: it carries the fixture's database credential", name)
					} else {
						_, _ = os.Stderr.Write(stdout)
					}
				}
				l.fatalf("fixture Job %s failed", name)
			}
		}
		l.sleep(time.Second)
	}
	l.fatalf("fixture Job %s did not complete", name)
}

// prepareRunningApplyFixture builds everything the release under test needs
// to decide, on its own, to apply one long-running statement: a database it
// can reach, an immutable verification policy, a suspended PtahSchema, and a
// plan whose every binding the manager re-derives. The plan's state
// fingerprints come from a real `ptah schema plan` against that database
// rather than from literals, because the Apply refuses a plan recorded against
// a state it does not observe.
func (l *lifecycleRun) prepareRunningApplyFixture() {
	l.t.Helper()
	state := &l.runningApply
	for _, required := range []struct{ value, name string }{
		{l.in.registryCredentialsFile, "E2E_REGISTRY_CREDENTIALS_FILE"},
		{l.in.externalPostgresCredentialsFile, "E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE"},
		{l.in.externalPostgresIP, "E2E_EXTERNAL_POSTGRES_IP"},
		{l.in.dockerContext, "E2E_DOCKER_CONTEXT"},
		{l.in.externalPostgresContainerID, "E2E_EXTERNAL_POSTGRES_CONTAINER_ID"},
	} {
		if required.value == "" {
			l.fatalf("%s is required for the running Apply proof", required.name)
		}
	}
	l.requireMode0600RegularFile(l.in.registryCredentialsFile, "E2E_REGISTRY_CREDENTIALS_FILE")
	l.requireMode0600RegularFile(l.in.externalPostgresCredentialsFile, "E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE")
	if !ipv4Address.MatchString(l.in.externalPostgresIP) {
		l.fatalf("E2E_EXTERNAL_POSTGRES_IP must be an IPv4 address")
	}
	if err := predecessorApplyDockerTarget(l.in.dockerContext, l.in.externalPostgresContainerID); err != nil {
		l.fatalf("%v", err)
	}
	identity, err := l.predecessorApplyDockerInspect("{{.Id}}")
	if err != nil {
		l.fatalf("could not inspect the external PostgreSQL barrier container: %v", err)
	}
	if identity != l.in.externalPostgresContainerID {
		l.fatalf("external PostgreSQL barrier container identity changed")
	}
	// Inspecting a container says it exists, not that it is serving, and the
	// two failures look identical from inside the cluster: a Pod dialing the
	// barrier Service reports `connection refused` whether the backend is down
	// or the Service has no programmed endpoint yet. Separating them here costs
	// one call and names the first one before any Pod can blame the second.
	if running, err := l.predecessorApplyDockerInspect("{{.State.Running}}"); err != nil || running != "true" {
		l.fatalf("the external PostgreSQL barrier container is not running")
	}

	// The executor the Apply will run is the one the live release configured,
	// read from the controller it dispatched with rather than from a value file
	// the proof could get wrong.
	l.runtimeDeploymentNames()
	controller := &appsv1.Deployment{}
	l.check(l.cluster.Client.Get(l.ctx, types.NamespacedName{Namespace: l.in.operatorNamespace, Name: l.controllerDeployment}, controller),
		"could not read the live executor image")
	executor, err := predecessorApplyExecutorImage(controller)
	if err != nil {
		l.fatalf("could not read the live executor image: %v", err)
	}
	if !predecessorApplyPinnedImage.MatchString(executor) {
		l.fatalf("the live executor image is not digest-pinned")
	}

	registryContent, err := os.ReadFile(l.in.registryCredentialsFile)
	l.check(err, "read E2E_REGISTRY_CREDENTIALS_FILE")
	registry, err := parseRegistryCredentials(registryContent)
	if err != nil {
		l.fatalf("%v", err)
	}
	databaseContent, err := os.ReadFile(l.in.externalPostgresCredentialsFile)
	l.check(err, "read E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE")
	database, err := predecessorApplyDatabaseCredentials(databaseContent)
	if err != nil {
		l.fatalf("%v", err)
	}
	url := predecessorApplyDatabaseURL(database, predecessorApplyDatabase+":5432")
	if state.scanner, err = newCredentialScanner(registry.Password, database.Password, url); err != nil {
		l.fatalf("%v", err)
	}
	immutable := true
	l.createForPredecessorApply(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: l.in.proofNamespace, Name: predecessorApplyPullSecret},
		Immutable:  &immutable,
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(dockerConfigJSON(predecessorApplyRegistry(executor), registry.Username, registry.Password)),
		},
	}, "Secret "+predecessorApplyPullSecret)
	l.createForPredecessorApply(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: l.in.proofNamespace, Name: predecessorApplyDatabase},
		Immutable:  &immutable,
		StringData: map[string]string{"url": url},
	}, "Secret "+predecessorApplyDatabase)
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: l.in.proofNamespace, Name: predecessorApplyDatabase},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{
			Name: "postgresql", Port: 5432, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(5432),
		}}},
	}
	l.createForPredecessorApply(service, "Service "+predecessorApplyDatabase)
	serviceController, blockDeletion, ready := true, false, true
	port, protocol, portName := int32(5432), corev1.ProtocolTCP, "postgresql"
	l.createForPredecessorApply(&discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: l.in.proofNamespace, Name: predecessorApplyDatabase + "-docker",
			Labels: map[string]string{
				discoveryv1.LabelServiceName: predecessorApplyDatabase,
				discoveryv1.LabelManagedBy:   "ptah-operator-e2e",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1", Kind: "Service", Name: predecessorApplyDatabase, UID: service.UID,
				Controller: &serviceController, BlockOwnerDeletion: &blockDeletion,
			}},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses: []string{l.in.externalPostgresIP}, Conditions: discoveryv1.EndpointConditions{Ready: &ready},
		}},
		Ports: []discoveryv1.EndpointPort{{Name: &portName, Port: &port, Protocol: &protocol}},
	}, "EndpointSlice "+predecessorApplyDatabase+"-docker")

	policy := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: l.in.proofNamespace, Name: predecessorApplyPolicy},
		Data:       map[string]string{"policy.yaml": predecessorApplyPolicyContent},
	}
	l.createForPredecessorApply(policy, "ConfigMap "+predecessorApplyPolicy)
	l.check(l.cluster.Client.Patch(l.ctx, policy, client.RawPatch(types.MergePatchType, []byte(`{"immutable":true}`)),
		client.FieldOwner(harness.FieldOwner)), "make ConfigMap %s immutable", predecessorApplyPolicy)

	schemaDocument := mustJSON(map[string]any{
		"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahSchema",
		"metadata": map[string]any{"name": predecessorApplySchema},
		"spec": map[string]any{
			"suspend":  true,
			"interval": "24h",
			"target": map[string]any{
				"engine": "PostgreSQL", "coordinationKey": predecessorApplySchema,
				"urlFrom": map[string]any{"name": predecessorApplyDatabase, "key": "url"},
			},
			"desired": map[string]any{
				"ociRef":                 "oci://example.invalid/schema@" + predecessorApplyArtifactDigest,
				"verificationPolicyFrom": map[string]any{"name": predecessorApplyPolicy, "key": "policy.yaml"},
			},
			"policy": map[string]any{
				"apply": "Always", "allowDestructive": false, "driftSeverity": "all",
				"lockTimeout": "30s", "transactionMode": "file",
			},
			"execution": map[string]any{
				"activeDeadlineSeconds": 600, "failureRetryInterval": "30s", "connectTimeout": "10s",
				"serviceAccountName": "default",
				"imagePullSecrets":   []any{map[string]any{"name": predecessorApplyPullSecret}},
			},
		},
	})
	if _, stderr, err := l.kubectlDocument(schemaDocument, "-n", l.in.proofNamespace, "apply"); err != nil {
		l.fatalf("apply PtahSchema %s: %v: %s", predecessorApplySchema, err, strings.TrimSpace(string(stderr)))
	}
	l.waitForSuspended(predecessorApplySchema)

	source, err := os.ReadFile(filepath.Join(repositoryRoot, "testdata", "e2e", "postgresql-v1.sql"))
	l.check(err, "read the running Apply plan source")
	l.createForPredecessorApply(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: l.in.proofNamespace, Name: predecessorApplyPlanSource},
		Data:       map[string]string{"schema.sql": string(source)},
	}, "ConfigMap "+predecessorApplyPlanSource)
	// The Job dials the barrier Service seconds after the Service and its
	// EndpointSlice were created, and a ClusterIP whose endpoint kube-proxy has
	// not programmed yet is rejected rather than dropped, so the executor sees
	// `connection refused` on its first packet and its connect timeout never
	// applies. Measured on run 35282131046, `Kubernetes 1.35 lifecycle`: the
	// diagnostics dump has the Service at AGE 5s and the Pod already in Error
	// inside those same five seconds.
	//
	// backoffLimit gives the dial a second and third chance in a fresh Pod,
	// which is the only probe of that path the fixture has. It hides nothing:
	// a barrier that is genuinely down fails every attempt with the same
	// message, and a barrier container that is not running is refused above,
	// by name, before any Pod runs.
	l.createForPredecessorApply(&unstructured.Unstructured{Object: predecessorApplyPlanSourceJob(l.in.proofNamespace, executor)},
		"Job "+predecessorApplyPlanSource)
	l.waitForPredecessorApplyFixtureJob(predecessorApplyPlanSource)
	// The plan comes from the Pod that succeeded, by name. A retried Job has
	// more than one Pod, and a failed attempt's output would be read as the
	// plan and fail the shape check below with a reason that is about neither.
	succeeded := &corev1.PodList{}
	l.check(l.cluster.Client.List(l.ctx, succeeded, client.InNamespace(l.in.proofNamespace),
		client.MatchingLabels{"app.kubernetes.io/component": predecessorApplyPlanSource},
		client.MatchingFields{"status.phase": string(corev1.PodSucceeded)}), "list the running Apply plan Pods")
	if len(succeeded.Items) == 0 {
		l.fatalf("the running Apply plan Job completed without a succeeded Pod")
	}
	native, err := l.cluster.ContainerLog(l.ctx, l.in.proofNamespace, succeeded.Items[0].Name, "planner")
	l.check(err, "read the native plan from %s", succeeded.Items[0].Name)
	planData, err := predecessorApplyPlan(native, predecessorApplySchema)
	if err != nil {
		l.fatalf("could not derive an exact long-running Apply plan: %v", err)
	}

	schema := &ptahv1alpha1.PtahSchema{}
	l.check(l.cluster.Client.Get(l.ctx, types.NamespacedName{Namespace: l.in.proofNamespace, Name: predecessorApplySchema}, schema),
		"read PtahSchema %s", predecessorApplySchema)
	secret := &corev1.Secret{}
	l.check(l.cluster.Client.Get(l.ctx, types.NamespacedName{Namespace: l.in.proofNamespace, Name: predecessorApplyDatabase}, secret),
		"read Secret %s", predecessorApplyDatabase)
	// The exact URL the Apply Job resolves. The runner derives the target
	// identity from it and refuses a plan recorded against a different one, so
	// the fixture binds this value rather than a placeholder.
	databaseURL := string(secret.Data["url"])
	if !strings.HasPrefix(databaseURL, "postgres://") {
		l.fatalf("running Apply database secret does not carry a postgres URL")
	}
	// The plan records the manager that publishes it. The status binding holds
	// only what the plan binds, so the manager's identity is read from a Job it
	// dispatched: the read-only fixture this proof dispatches first.
	manager, err := predecessorApplyManagerOf(l.predecessorApplyManagerJob())
	if err != nil {
		l.fatalf("%v", err)
	}
	state.bundle, err = predecessorApplyFixture(schema, manager, planData, string(policy.UID),
		[]byte(predecessorApplyPolicyContent), databaseURL, time.Now().UTC())
	if err != nil {
		l.fatalf("could not build the running Apply fixture: %v", err)
	}
	if !predecessorApplyPlanCarriesContract(&state.bundle.Plan, l.planContractVersion) {
		l.fatalf("the generated Apply plan does not carry the current manager contract")
	}
	plan := state.bundle.Plan.DeepCopy()
	l.createForPredecessorApply(plan, "PtahSchemaPlan "+plan.Name)
	state.planName, state.planUID = plan.Name, plan.UID
	chunkName := plan.Spec.Chunks[0].Name
	chunkController, chunkBlock := true, true
	chunk := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahSchemaPlanChunk",
		"metadata": map[string]any{
			"namespace": l.in.proofNamespace, "name": chunkName,
			"labels": map[string]any{"operator.ptah.run/plan": plan.Name, "operator.ptah.run/schema": predecessorApplySchema},
			"ownerReferences": []any{map[string]any{
				"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahSchemaPlan",
				"name": plan.Name, "uid": string(plan.UID), "controller": chunkController, "blockOwnerDeletion": chunkBlock,
			}},
		},
		"spec": map[string]any{"data": base64.StdEncoding.EncodeToString(planData)},
	}}
	l.createForPredecessorApply(chunk, "PtahSchemaPlanChunk "+chunkName)
	readyAt := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	status, err := json.Marshal(map[string]any{"status": map[string]any{
		"observedGeneration": plan.Generation,
		"publishedChunks":    []any{map[string]any{"name": chunkName, "uid": string(chunk.GetUID()), "index": 0}},
		"conditions": []any{map[string]any{
			"type": "Ready", "status": "True", "reason": "Published", "message": "Verified 1 immutable plan chunks",
			"observedGeneration": plan.Generation, "lastTransitionTime": readyAt,
		}},
	}})
	l.check(err, "encode the plan status")
	published := &ptahv1alpha1.PtahSchemaPlan{}
	published.Namespace, published.Name = l.in.proofNamespace, plan.Name
	l.check(l.asManagerStatus().Status().Patch(l.ctx, published, client.RawPatch(types.MergePatchType, status)),
		"publish PtahSchemaPlan %s as the manager", plan.Name)
}

// predecessorApplyDockerInspect reads one field of the external PostgreSQL
// container.
func (l *lifecycleRun) predecessorApplyDockerInspect(format string) (string, error) {
	command := exec.CommandContext(l.ctx, "docker", "--context", l.in.dockerContext, "container", "inspect", //nolint:gosec // Arguments, not a shell.
		"--format", format, l.in.externalPostgresContainerID)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return predecessorApplyValue(stdout.String()), nil
}

// predecessorApplyManagerJob is a Job the live manager dispatched: the
// read-only fixture's, as its dispatch read it back, which the proof
// dispatches first.
func (l *lifecycleRun) predecessorApplyManagerJob() *batchv1.Job {
	l.t.Helper()
	if l.readOnlyJobSchema == "" || len(l.evidence[readOnlyJobDocumentKey(l.readOnlyJobSchema)]) == 0 {
		l.fatalf("the running Apply fixture needs a Job the live manager dispatched; the read-only Job fixture runs first")
	}
	job := &batchv1.Job{}
	if err := json.Unmarshal(l.readOnlyJobDocument(l.readOnlyJobSchema), job); err != nil {
		l.fatalf("decode the read-only Job the live manager dispatched: %v", err)
	}
	return job
}

// predecessorApplyPlanSourceJob is the Job that runs `ptah schema plan
// --dry-run` against the fixture's database, for the state fingerprints the
// plan carries.
func predecessorApplyPlanSourceJob(namespace, executor string) map[string]any {
	return map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"namespace": namespace, "name": predecessorApplyPlanSource},
		"spec": map[string]any{
			"backoffLimit": int64(3), "activeDeadlineSeconds": int64(180), "ttlSecondsAfterFinished": int64(300),
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app.kubernetes.io/component": predecessorApplyPlanSource}},
				"spec": map[string]any{
					"restartPolicy": "Never", "automountServiceAccountToken": false,
					"imagePullSecrets": []any{map[string]any{"name": predecessorApplyPullSecret}},
					"securityContext": map[string]any{
						"runAsNonRoot": true, "runAsUser": int64(65532), "runAsGroup": int64(65532), "fsGroup": int64(65532),
						"seccompProfile": map[string]any{"type": "RuntimeDefault"},
					},
					"containers": []any{map[string]any{
						"name": "planner", "image": executor, "imagePullPolicy": "IfNotPresent",
						"command": []any{"/usr/local/bin/ptah"}, "args": []any{"schema", "plan", "--dry-run"},
						"env": []any{
							map[string]any{"name": "HOME", "value": "/work"},
							map[string]any{"name": "TMPDIR", "value": "/work"},
							map[string]any{"name": "PTAH_SCHEMA_FILE", "value": "/schema/schema.sql"},
							map[string]any{"name": "PTAH_CONNECT_TIMEOUT", "value": "10s"},
							map[string]any{"name": "PTAH_LOCK_TIMEOUT", "value": "30s"},
							map[string]any{"name": "PTAH_DB_URL", "valueFrom": map[string]any{
								"secretKeyRef": map[string]any{"name": predecessorApplyDatabase, "key": "url"},
							}},
						},
						"securityContext": map[string]any{
							"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
							"capabilities": map[string]any{"drop": []any{"ALL"}},
						},
						"volumeMounts": []any{
							map[string]any{"name": "schema", "mountPath": "/schema", "readOnly": true},
							map[string]any{"name": "work", "mountPath": "/work"},
						},
					}},
					"volumes": []any{
						map[string]any{"name": "schema", "configMap": map[string]any{"name": predecessorApplyPlanSource}},
						map[string]any{"name": "work", "emptyDir": map[string]any{"sizeLimit": "64Mi"}},
					},
				},
			},
		},
	}
}

// startRunningApplyFixture hands the manager the status that makes the Apply
// its own decision, and waits until the Apply Pod is actually running. The
// controller is stopped while the status is written so that nothing
// reconciles a half-written state, which is the same fence the read-only
// fixture uses.
func (l *lifecycleRun) startRunningApplyFixture() {
	l.t.Helper()
	state := &l.runningApply
	if state.planName == "" {
		l.fatalf("running Apply plan name is missing")
	}
	if state.planUID == "" {
		l.fatalf("running Apply plan UID is missing")
	}
	l.stopControllerDeployment()
	l.mustKubectl("-n", l.in.proofNamespace, "patch", "ptahschema", predecessorApplySchema, "--type=merge",
		`-p={"spec":{"suspend":false}}`)
	schema := &ptahv1alpha1.PtahSchema{}
	l.check(l.cluster.Client.Get(l.ctx, types.NamespacedName{Namespace: l.in.proofNamespace, Name: predecessorApplySchema}, schema),
		"read PtahSchema %s", predecessorApplySchema)
	schema.Status = predecessorApplyReadyStatus(state.bundle.SchemaStatus, schema.Generation, state.planUID)
	l.check(l.asManagerStatus().Status().Update(l.ctx, schema), "hand %s its ready status as the manager", predecessorApplySchema)
	l.startControllerDeployment()
	l.check(l.cluster.WaitForRollout(l.ctx, l.in.operatorNamespace, l.controllerDeployment, 3*time.Minute),
		"the controller Deployment %s did not roll out", l.controllerDeployment)

	var matchedJob map[string]any
	for deadline := time.Now().Add(300 * time.Second); time.Now().Before(deadline); {
		live, err := l.predecessorApplySchemaDocument()
		l.check(err, "read PtahSchema %s", predecessorApplySchema)
		if predecessorApplyTerminalFailure(live) {
			l.emitPredecessorApplyDiagnostic()
			l.fatalf("the Apply reached a terminal failure before its Pod was observed running")
		}
		name, committed := predecessorApplyDispatchedJob(live)
		state.jobName = name
		if name != "" && committed != "" {
			if job, err := l.predecessorApplyJobDocument(name); err == nil {
				state.jobUID, _ = predecessorApplyAt(job, "metadata", "uid").(string)
				if committed == state.jobUID {
					pods := &corev1.PodList{}
					if l.cluster.Client.List(l.ctx, pods, client.InNamespace(l.in.proofNamespace)) == nil {
						if pod, found := predecessorApplyRunningPod(pods.Items, types.UID(state.jobUID)); found {
							state.podName, state.podUID = pod.Name, pod.UID
							matchedJob = job
							break
						}
					}
				}
			}
		}
		l.sleep(time.Second)
	}
	if state.podUID == "" {
		l.emitPredecessorApplyDiagnostic()
		l.fatalf("the Apply Job did not reach a running Pod")
	}
	if !predecessorApplyJobCarriesIdentity(matchedJob, predecessorApplySchema) {
		l.fatalf("the running Apply Job does not carry its dispatched operation identity")
	}
	l.waitForPredecessorApplyBarrierContention()
}

// emitPredecessorApplyDiagnostic is emit_running_apply_diagnostic: what the
// schema decided and what its Jobs report, written to the work directory and
// printed.
func (l *lifecycleRun) emitPredecessorApplyDiagnostic() {
	var lines [][]byte
	if schema, err := l.predecessorApplySchemaDocument(); err == nil {
		if line, err := predecessorApplySchemaDiagnostic(schema); err == nil {
			lines = append(lines, line)
		}
	}
	jobs := &unstructured.UnstructuredList{}
	jobs.SetAPIVersion("batch/v1")
	jobs.SetKind("JobList")
	if l.cluster.Client.List(l.ctx, jobs, client.InNamespace(l.in.proofNamespace),
		client.MatchingLabels{"operator.ptah.run/schema": predecessorApplySchema}) == nil {
		documents := make([]map[string]any, 0, len(jobs.Items))
		for index := range jobs.Items {
			documents = append(documents, jobs.Items[index].Object)
		}
		if line, err := predecessorApplyJobsDiagnostic(documents); err == nil {
			lines = append(lines, line)
		}
	}
	content := append(bytes.Join(lines, []byte("\n")), '\n')
	path := filepath.Join(l.workDir, "running-apply-diagnostic.json")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		l.t.Logf("e2e crd: could not write the running Apply diagnostic: %v", err)
	}
	l.t.Logf("e2e crd: running Apply diagnostic written to %s", path)
	for _, line := range lines {
		l.t.Logf("e2e crd:   %s", line)
	}
}

// stagePredecessorApplyJobUIDGapWhileRunning is the UID-adoption boundary: a
// Job the manager created but whose UID it had not yet recorded is still that
// manager's work. The successor has to adopt it by name and then by UID,
// before any Pod discovery.
func (l *lifecycleRun) stagePredecessorApplyJobUIDGapWhileRunning() {
	l.t.Helper()
	state := &l.runningApply
	switch {
	case state.jobName == "":
		l.fatalf("running Apply Job name is missing")
	case state.jobUID == "":
		l.fatalf("running Apply Job UID is missing")
	case state.podUID == "":
		l.fatalf("running Apply Pod UID is missing")
	}
	job, err := l.predecessorApplyJobDocument(state.jobName)
	l.check(err, "read Job %s", state.jobName)
	if !predecessorApplyJobRunning(job, state.jobUID) {
		l.fatalf("the Apply Job is not running at the upgrade boundary")
	}
	if pod, err := l.predecessorApplyPod(state.podName); err != nil || !predecessorApplyPodIn(pod, state.podUID, corev1.PodRunning) {
		l.fatalf("the Apply Pod is not running at the upgrade boundary")
	}
	state.jobBeforeCleanup, err = predecessorApplyJobEvidence(job)
	l.check(err, "encode the evidence of Job %s", state.jobName)
	schema := &ptahv1alpha1.PtahSchema{}
	schema.Namespace, schema.Name = l.in.proofNamespace, predecessorApplySchema
	l.check(l.asManagerStatus().Status().Patch(l.ctx, schema,
		client.RawPatch(types.JSONPatchType, []byte(`[{"op":"remove","path":"/status/activeOperation/jobUID"}]`))),
		"remove the recorded Job UID of %s as the manager", predecessorApplySchema)
	state.stagedGap, err = l.predecessorApplySchemaDocument()
	l.check(err, "read PtahSchema %s", predecessorApplySchema)
	// One message for five conditions cost a whole lifecycle to diagnose: the
	// phase stops at its first failure, and the log said only that the gap
	// was not retained. Each condition names itself, and the schema's own
	// status is printed beside it.
	for _, check := range predecessorApplyStagedGapChecks {
		if !check.holds(state.stagedGap, state.jobName) {
			l.emitPredecessorApplyDiagnostic()
			l.fatalf("the Apply fixture did not retain the running late-create UID gap: %s", check.reason)
		}
	}
}

// assertPredecessorApplyRemainsExclusiveWhileRunning: the next release differs
// from the one that dispatched the Apply in its manager image alone, which the
// execution binding does not hold. So the successor keeps the epoch and adopts
// the running Apply as its own: the claim stays, the Job UID the harness
// removed is recorded again, and nothing is retired into a pending observation
// while the Apply is still inside the engine.
func (l *lifecycleRun) assertPredecessorApplyRemainsExclusiveWhileRunning() {
	l.t.Helper()
	state := &l.runningApply
	adopted := false
	for deadline := time.Now().Add(180 * time.Second); time.Now().Before(deadline); {
		if schema, err := l.predecessorApplySchemaDocument(); err == nil &&
			predecessorApplyExclusive(schema, state.stagedGap, state.jobName, state.jobUID) {
			adopted = true
			break
		}
		l.sleep(time.Second)
	}
	if !adopted {
		l.emitPredecessorApplyDiagnostic()
		l.fatalf("the successor did not adopt the running Apply under the epoch it was dispatched in")
	}
	if job, err := l.predecessorApplyJobDocument(state.jobName); err != nil || !predecessorApplyJobRunning(job, state.jobUID) {
		l.fatalf("the successor replaced, completed, or cleaned the running Apply Job")
	}
	if pod, err := l.predecessorApplyPod(state.podName); err != nil || !predecessorApplyPodIn(pod, state.podUID, corev1.PodRunning) {
		l.fatalf("the upgrade did not retain the running Apply Pod UID")
	}
	jobs := &batchv1.JobList{}
	if err := l.cluster.Client.List(l.ctx, jobs, client.InNamespace(l.in.proofNamespace),
		client.MatchingLabels{"operator.ptah.run/schema": predecessorApplySchema}); err != nil ||
		len(jobs.Items) != 1 || jobs.Items[0].Name != state.jobName || string(jobs.Items[0].UID) != state.jobUID {
		l.fatalf("the successor launched new work over the running Apply")
	}
	// The Apply is still inside the engine, waiting on the barrier: exclusivity
	// that held because the Apply had already finished would prove nothing.
	l.assertPredecessorApplyBarrierContended()
}

// waitForPredecessorApplyJobTerminal waits for the released Apply to finish,
// and holds its Job and Pod to the UIDs the successor adopted.
func (l *lifecycleRun) waitForPredecessorApplyJobTerminal() {
	l.t.Helper()
	state := &l.runningApply
	var last map[string]any
	for deadline := time.Now().Add(300 * time.Second); time.Now().Before(deadline); {
		job, err := l.predecessorApplyJobDocument(state.jobName)
		last = job
		if err == nil && predecessorApplyJobFinished(job) {
			break
		}
		l.sleep(time.Second)
	}
	if last == nil || !predecessorApplyJobTerminal(last, state.jobUID) {
		l.fatalf("the Apply Job did not finish after the successor adopted it")
	}
	if pod, err := l.predecessorApplyPod(state.podName); err != nil ||
		!predecessorApplyPodIn(pod, state.podUID, corev1.PodSucceeded, corev1.PodFailed) {
		l.fatalf("the Apply Pod is not terminal after the successor adopted it")
	}
}

// waitForPredecessorApplyJobCleanup: the adopted Apply is accounted for
// through its own result, under the epoch it was dispatched in. The successor
// read the frame, scheduled the Job's cleanup and recorded what the run said
// -- applied, or unknown when the frame says so -- for the read-only
// observation that follows. Either account names this Job; a fence would have
// moved the epoch instead. It also names the manager that dispatched the Job,
// read from the Job's own Pod template: the predecessor, not the successor
// that harvested it, because the Job is collected five minutes later and the
// record is what is left.
func (l *lifecycleRun) waitForPredecessorApplyJobCleanup() {
	l.t.Helper()
	state := &l.runningApply
	for deadline := time.Now().Add(240 * time.Second); time.Now().Before(deadline); {
		job, err := l.predecessorApplyJobDocument(state.jobName)
		if err == nil && predecessorApplyTTL(job) == 300 {
			if schema, err := l.predecessorApplySchemaDocument(); err == nil {
				dispatcher := predecessorApplyDispatcher(job)
				if dispatcher == "" || dispatcher == l.in.nextControllerImage {
					l.fatalf("the adopted Apply Job does not record the predecessor that dispatched it")
				}
				if predecessorApplyAccounted(schema, state.stagedGap, state.jobName, state.jobUID, string(state.planUID), dispatcher) {
					after, err := predecessorApplyJobEvidence(job)
					l.check(err, "encode the evidence of Job %s", state.jobName)
					if !bytes.Equal(state.jobBeforeCleanup, after) {
						l.fatalf("the successor changed the adopted Apply Job outside ttlSecondsAfterFinished")
					}
					return
				}
			}
		}
		l.sleep(time.Second)
	}
	l.emitPredecessorApplyDiagnostic()
	l.fatalf("the successor did not account for the adopted Apply Job through its own result")
}
