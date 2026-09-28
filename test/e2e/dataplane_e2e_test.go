//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/test/e2e/harness"
	"github.com/stokaro/ptah-operator/test/e2e/phases"
)

// TestDataPlane is the data-plane suite's second phase, and the migration
// suites' preparation. It stands up a registry endpoint, the databases, an
// authenticated HTTPS registry proxy and the admission fixtures in the test
// namespace, and stops there when the driver asks for preparation. In full it
// takes both engines through their lifecycles against real databases, holds
// the operator to its refusals, runs the restart and fault injection, and
// proves the four-eyes and Pod-metadata rows.
func TestDataPlane(t *testing.T) {
	run, inputs := harness.Begin(t, phases.DataPlane)
	d := newDataPlane(t, run, inputs)

	if !run.Scenario("databases-and-fixtures", d.scenario(d.databasesAndFixtures)) {
		return
	}
	// The preparation boundary. A suite that only needs this namespace stops
	// here: what follows is this phase's own acceptance, which its own suite
	// runs.
	if inputs.Mode == "prepare" {
		run.Logf("e2e data plane: PASS prerequisites only: registry endpoint, isolated databases, TLS proxy, and admission fixtures")
		run.Prepared()
		return
	}
	for _, scenario := range []struct {
		name string
		body func()
	}{
		{"postgresql-lifecycle", d.postgresqlLifecycle},
		{"external-postgresql-lifecycle", d.externalPostgresqlLifecycle},
		{"mysql-lifecycle", d.mysqlLifecycle},
		{"mysql-dsn-refusal", d.mysqlDSNRefusalScenario},
		{"faults", d.faults},
		{"closing-audits", d.closingAudits},
		{"four-eyes-distinct-approver", d.fourEyesDistinctApprover},
		{"pod-metadata-admission", d.podMetadataAdmission},
	} {
		if !run.Scenario(scenario.name, d.scenario(scenario.body)) {
			return
		}
	}
	run.Logf("e2e data plane: PASS PostgreSQL, external PostgreSQL, MySQL, OCI, restart, and fault lifecycle")
}

// dataPlane is what the scenarios share. Each scenario runs as a subtest, and
// t is that subtest while it runs, so a failure anywhere below ends the
// scenario it happened in.
type dataPlane struct {
	t       *testing.T
	parent  *testing.T
	ctx     context.Context
	cluster *harness.Cluster
	in      phases.DataPlaneInputs

	runnerProtocol int64
	controller     controllerIdentity
	// controllerName is the manager's Deployment, which shares its name with
	// the ClusterRole whose status verb the phase pauses.
	controllerName           string
	controllerServiceAccount string

	registry         registryCredentials
	external         externalPostgresCredentials
	credentials      fixtureCredentials
	scanner          credentialScanner
	registryHost     string
	tlsProxy         tlsProxyAddress
	tlsProxyIdentity tlsProxyPod

	workDir string
	// The ledgers the fault phase shares: every Job the phase has seen, and
	// the Jobs whose credential audit it completed, broadly and fully.
	observed     *jobLedger
	audited      *uidLedger
	fullyAudited *uidLedger
	evidence     map[string]*jobEvidence
	// resultAssert is the command the fault phase reads results with.
	resultAssert string

	rbac                     rbacPause
	ephemeralTested          bool
	fourEyesSwitchOn         bool
	podMetadataPolicyCreated bool

	captured capturedJob
	plan     currentPlan
	// planDocument is the last plan document assert_plan rebuilt from chunks.
	planDocument     []byte
	periodicNoop     checkpoint
	blockedGate      checkpoint
	mysqlDestructive mysqlDestructiveEvidence
}

// tlsProxyPod is the one proxy Pod whose request counter a row reads.
type tlsProxyPod struct {
	name, uid, podIP, containerID string
}

// capturedJob is the last result Job a row read: its identity, and its one
// Pod's.
type capturedJob struct {
	jobName, jobUID, operationID     string
	podName, podUID, podGenerateName string
	evidence                         *jobEvidence
}

// currentPlan is the plan a schema's status named when the row read it.
type currentPlan struct {
	name, uid, fingerprint string
}

// mysqlDestructiveEvidence is what the MySQL lifecycle leaves for the closing
// audit to hold: the destructive plan it refused, and the checkpoint no Apply
// may follow.
type mysqlDestructiveEvidence struct {
	schema, plan, planUID, digest string
	applyCheckpoint               checkpoint
	retained                      bool
}

func (d *dataPlane) scenario(body func()) func(*testing.T) {
	return func(t *testing.T) {
		d.t = t
		defer func() { d.t = d.parent }()
		body()
	}
}

func (d *dataPlane) fatalf(format string, arguments ...any) {
	d.t.Helper()
	d.t.Fatalf("e2e data plane: "+format, arguments...)
}

func (d *dataPlane) logf(format string, arguments ...any) {
	d.t.Helper()
	d.t.Logf("e2e data plane: "+format, arguments...)
}

// check ends the scenario when err is not nil, saying what failed and why.
func (d *dataPlane) check(err error, format string, arguments ...any) {
	d.t.Helper()
	if err != nil {
		d.fatalf("%s: %v", fmt.Sprintf(format, arguments...), err)
	}
}

// sleep pauses between two readings, and ends the scenario when the phase's
// own bound ends first.
func (d *dataPlane) sleep(duration time.Duration) {
	d.t.Helper()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-d.ctx.Done():
		d.fatalf("the phase's bound ended while it waited: %v", d.ctx.Err())
	case <-timer.C:
	}
}

func newDataPlane(t *testing.T, run *harness.Run, in phases.DataPlaneInputs) *dataPlane {
	t.Helper()
	d := &dataPlane{t: t, parent: t, ctx: run.Context(), in: in, evidence: map[string]*jobEvidence{}}
	// Registered before the phase creates its work directory or touches the
	// cluster, so a setup that fails part way still removes what it made.
	t.Cleanup(d.cleanup)
	for _, command := range []string{"docker", "kubectl", "helm", "go"} {
		if _, err := exec.LookPath(command); err != nil {
			d.fatalf("required command is not installed: %s", command)
		}
	}
	if err := dataPlaneInputsOK(in); err != nil {
		d.fatalf("%v", err)
	}
	if info, err := os.Stat(in.Kubeconfig); err != nil || !info.Mode().IsRegular() {
		d.fatalf("E2E_KUBECONFIG does not name a file")
	}
	d.requirePrivateFile(in.RegistryCredentialsFile, "E2E_REGISTRY_CREDENTIALS_FILE")
	d.requirePrivateFile(in.ExternalPostgresCredentialsFile, "E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE")
	for _, path := range []string{in.TLSProxyCAFile, in.TLSProxyCertFile, in.TLSProxyKeyFile} {
		d.requirePrivateFile(path, "TLS proxy input file")
	}
	d.requireSSHDockerContext()
	if info, err := os.Stat(in.ChartPackage); err != nil || !info.Mode().IsRegular() {
		d.fatalf("E2E_CHART_PACKAGE does not name a chart package")
	}

	content, err := os.ReadFile(in.RegistryCredentialsFile)
	d.check(err, "read E2E_REGISTRY_CREDENTIALS_FILE")
	if d.registry, err = parseRegistryCredentials(content); err != nil {
		d.fatalf("%v", err)
	}
	content, err = os.ReadFile(in.ExternalPostgresCredentialsFile)
	d.check(err, "read E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE")
	if d.external, err = parseExternalPostgresCredentials(content, in.ExternalPostgresService, in.TestNamespace); err != nil {
		d.fatalf("%v", err)
	}
	catalog, err := os.ReadFile(filepath.Join(repositoryRoot, "support", "ptah.json"))
	d.check(err, "read support/ptah.json")
	if d.runnerProtocol, err = edgeRunnerProtocolVersion(catalog); err != nil {
		d.fatalf("%v", err)
	}
	d.controller = controllerIdentity{
		image: in.ControllerImage, revision: in.ControllerRevision, stateVersion: in.ControllerStateVersion,
	}

	if d.cluster, err = harness.Connect(in.Kubeconfig); err != nil {
		d.fatalf("%v", err)
	}
	d.resolveController()

	d.workDir, err = os.MkdirTemp("", "ptah-operator-data-e2e.")
	d.check(err, "create the work directory")
	d.check(os.Chmod(d.workDir, 0o700), "make the work directory private")
	if d.observed, err = newJobLedger(filepath.Join(d.workDir, "observed-jobs.jsonl")); err != nil {
		d.fatalf("create the observed Job ledger: %v", err)
	}
	if d.audited, err = newUIDLedger(filepath.Join(d.workDir, "audited-jobs.txt")); err != nil {
		d.fatalf("create the audited Job ledger: %v", err)
	}
	if d.fullyAudited, err = newUIDLedger(filepath.Join(d.workDir, "fully-audited-jobs.txt")); err != nil {
		d.fatalf("create the fully audited Job ledger: %v", err)
	}
	d.buildResultAssert()

	d.credentials = deriveFixtureCredentials(in.TestNamespace)
	d.registryHost = in.RegistryService + "." + in.TestNamespace + ".svc.cluster.local:5000"
	caBytes, err := os.ReadFile(in.TLSProxyCAFile)
	d.check(err, "read the TLS proxy CA")
	caSum := sha256.Sum256(caBytes)
	authority := in.TLSProxyService + "." + in.TestNamespace + ".svc.cluster.local:5443"
	d.tlsProxy = tlsProxyAddress{
		authority: authority,
		reference: "oci://" + authority + "/schemas/postgresql:stable",
		caSHA256:  "sha256:" + hex.EncodeToString(caSum[:]),
	}
	if err := tlsProxyGrantsDistinct(d.tlsProxy); err != nil {
		d.fatalf("%v", err)
	}
	if d.scanner, err = newCredentialScanner(
		d.registry.Password,
		d.credentials.pgPassword, d.credentials.pgURL, d.credentials.customCAPGURL,
		d.credentials.fourEyesPGURL, d.credentials.podMetadataPGURL,
		d.credentials.mysqlPassword, d.credentials.mysqlRootPassword, d.credentials.mysqlURL,
		d.external.Password, d.external.URL,
	); err != nil {
		d.fatalf("%v", err)
	}
	return d
}

// requirePrivateFile refuses a credential input that is not a regular file,
// that is a symlink, or that anyone but its owner can read.
func (d *dataPlane) requirePrivateFile(path, description string) {
	d.t.Helper()
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		d.fatalf("%s must name a regular non-symlink file", description)
	}
	if info.Mode().Perm() != 0o600 {
		d.fatalf("%s must have mode 0600", description)
	}
}

// requireSSHDockerContext holds the Docker context to a remote daemon reached
// over SSH: the registry and the external PostgreSQL run there, and a local
// daemon would be another machine's containers.
func (d *dataPlane) requireSSHDockerContext() {
	d.t.Helper()
	if _, err := d.docker("context", "inspect", d.in.DockerContext); err != nil {
		d.fatalf("E2E_DOCKER_CONTEXT cannot be inspected: %v", err)
	}
	endpoint, err := d.docker("context", "inspect", "--format", `{{ (index .Endpoints "docker").Host }}`, d.in.DockerContext)
	if err != nil || !strings.HasPrefix(strings.TrimSpace(endpoint), "ssh://") {
		d.fatalf("E2E_DOCKER_CONTEXT must use an SSH endpoint")
	}
}

// docker runs the Docker CLI against the phase's own context and returns its
// standard output.
func (d *dataPlane) docker(arguments ...string) (string, error) {
	command := exec.CommandContext(d.ctx, "docker", append([]string{"--context", d.in.DockerContext}, arguments...)...) //nolint:gosec // Arguments, not a shell.
	output, err := command.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return string(output), fmt.Errorf("docker %s: %w: %s", arguments[0], err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return string(output), fmt.Errorf("docker %s: %w", arguments[0], err)
	}
	return string(output), nil
}

// resolveController reads the names the chart derived: the manager's
// Deployment, which the ClusterRole the phase pauses shares its name with,
// and the ServiceAccount it runs as, which carries the controller-state
// version. It holds the manager to the image the driver built.
func (d *dataPlane) resolveController() {
	d.t.Helper()
	deployments := &appsv1.DeploymentList{}
	if err := d.cluster.Client.List(d.ctx, deployments, client.InNamespace(d.in.OperatorNamespace),
		client.MatchingLabels{"app.kubernetes.io/component": "controller"}); err != nil || len(deployments.Items) == 0 {
		d.fatalf("installed controller Deployment is missing: %v", err)
	}
	deployment := &deployments.Items[0]
	d.controllerName = deployment.Name
	d.controllerServiceAccount = deployment.Spec.Template.Spec.ServiceAccountName
	if d.controllerServiceAccount == "" {
		d.fatalf("installed controller Deployment %s has no ServiceAccount", d.controllerName)
	}
	image, err := controllerImageArgument(deployment)
	if err != nil {
		d.fatalf("%v", err)
	}
	if image != d.in.ControllerImage {
		d.fatalf("manager controller image argument does not match E2E_CONTROLLER_IMAGE")
	}
}

// buildResultAssert builds the command the fault phase reads results with,
// from this snapshot, with a build cache of the phase's own.
func (d *dataPlane) buildResultAssert() {
	d.t.Helper()
	d.resultAssert = filepath.Join(d.workDir, "e2e-resultassert")
	cache := filepath.Join(d.workDir, "go-cache")
	d.check(os.MkdirAll(cache, 0o700), "create the result parser's build cache")
	command := exec.CommandContext(d.ctx, "go", "build", "-trimpath", "-o", d.resultAssert, "./resultassert") //nolint:gosec // Arguments, not a shell.
	command.Env = append(os.Environ(), "GOCACHE="+cache)
	command.Stdout, command.Stderr = os.Stderr, os.Stderr
	d.check(command.Run(), "build the result parser the fault phase reads results with")
}

// cleanup runs on every exit. A failure leaves credential-safe diagnostics
// behind it, and every cluster-wide change a row made is undone: the paused
// status verb, the four-eyes switch and the Pod-metadata policy.
func (d *dataPlane) cleanup() {
	t := d.parent
	d.t = t
	// The phase's own context has ended when the phase ran out of time, which
	// is when putting the cluster back matters most, so the cleanup gets a
	// bound of its own.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	d.ctx = ctx
	if t.Failed() && d.cluster != nil {
		d.collectDiagnostics()
	}
	if d.rbac.paused {
		if err := d.resumeStatusWrites(); err != nil {
			t.Errorf("e2e data plane: could not restore controller status-write RBAC: %v", err)
		}
	}
	if d.fourEyesSwitchOn {
		if err := d.setRequireDistinctApprover(false); err != nil {
			t.Errorf("e2e data plane: could not turn approvals.requireDistinctApprover back off: %v", err)
		}
	}
	if d.podMetadataPolicyCreated {
		if err := d.removePodMetadataPolicy(); err != nil {
			t.Errorf("e2e data plane: could not remove the Pod-metadata admission policy: %v", err)
		}
	}
	if d.workDir != "" {
		if !strings.HasPrefix(filepath.Base(d.workDir), "ptah-operator-data-e2e.") {
			t.Errorf("e2e data plane: refusing to remove unexpected work directory %s", d.workDir)
			return
		}
		if err := os.RemoveAll(d.workDir); err != nil {
			t.Errorf("e2e data plane: remove the work directory: %v", err)
		}
	}
}

// collectDiagnostics prints what a failure left in the namespace without
// printing a credential: a projection of the schemas, their Events, Jobs and
// Leases that carries no free text, scanned before it is printed, and the
// printer columns of the resources the phase made. Raw Events and logs are
// withheld, because the failure may be a credential that escaped into them.
func (d *dataPlane) collectDiagnostics() {
	stderr := os.Stderr
	_, _ = fmt.Fprintln(stderr, "e2e data plane: collecting failure diagnostics")
	d.emitCleanupProjection()
	for _, kinds := range []string{"ptahschemas,ptahschemaplans,ptahschemaapprovals", "jobs,pods"} {
		stdout, errOut, _ := d.cluster.Kubectl(context.Background(), "-n", d.in.TestNamespace, "get", kinds, "-o", "wide")
		_, _ = stderr.Write(stdout)
		_, _ = stderr.Write(errOut)
	}
	_, _ = fmt.Fprintln(stderr, "e2e data plane: raw events and logs are suppressed to protect credential-isolation failures")
}

// jsonBytes is the JSON of an object the phase scans or compares. An object
// that does not marshal is a harness defect, so it ends the scenario.
func (d *dataPlane) jsonBytes(value any) []byte {
	d.t.Helper()
	content, err := json.Marshal(value)
	d.check(err, "encode %T", value)
	return content
}
