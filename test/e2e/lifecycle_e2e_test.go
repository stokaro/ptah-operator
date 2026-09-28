//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/controllerstate"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
	"github.com/stokaro/ptah-operator/test/e2e/phases"
)

// TestUpgrade is the upgrade phase: the CRD upgrade path of the release the
// driver installed, and the guards that keep one release per namespace.
func TestUpgrade(t *testing.T) {
	run, in := harness.Begin(t, phases.Upgrade)
	l := newLifecycleRun(t, run, "upgrade", lifecycleInputs{
		kubeconfig: in.Kubeconfig, debugLogs: in.DebugLogs, operatorNamespace: in.OperatorNamespace,
		proofNamespace: in.ProofNamespace, helmRelease: in.HelmRelease, chartPackage: in.ChartPackage,
		candidateValuesFile: in.CandidateValuesFile, controllerImage: in.ControllerImage,
		kubernetesVersion: in.KubernetesVersion,
	})
	for _, scenario := range []struct {
		name string
		body func()
	}{
		{"read-only-job-cleanup", l.readOnlyJobCleanupWithinTheRelease},
		{"crd-preflight-refusals", l.crdPreflightRefusals},
		{"drifted-crd-upgrade", l.driftedCRDUpgrade},
		{"controller-write-guard", l.proveControllerWriteGuard},
		{"runtime-recovery", l.proveRuntimeDeploymentRecovery},
		{"singleton-coordination", l.singletonCoordination},
	} {
		if !run.Scenario(scenario.name, l.scenario(scenario.body)) {
			return
		}
	}
	run.Logf("e2e crd: PASS phase=upgrade")
}

// TestUninstall is the uninstall phase: the synthetic next release, the
// rollback to this one, and the uninstalls and installs over retained CRDs.
func TestUninstall(t *testing.T) {
	run, in := harness.Begin(t, phases.Uninstall)
	l := newLifecycleRun(t, run, "uninstall", lifecycleInputs{
		kubeconfig: in.Kubeconfig, debugLogs: in.DebugLogs, operatorNamespace: in.OperatorNamespace,
		proofNamespace: in.ProofNamespace, helmRelease: in.HelmRelease, chartPackage: in.ChartPackage,
		candidateValuesFile: in.CandidateValuesFile, controllerImage: in.ControllerImage,
		kubernetesVersion: in.KubernetesVersion, nextChartPackage: in.NextChartPackage,
		nextValuesFile: in.NextValuesFile, nextControllerImage: in.NextControllerImage,
		registryCredentialsFile: in.RegistryCredentialsFile, dockerContext: in.DockerContext,
		externalPostgresContainerID: in.ExternalPostgresContainerID, externalPostgresIP: in.ExternalPostgresIP,
		externalPostgresCredentialsFile: in.ExternalPostgresCredentialsFile,
	})
	for _, scenario := range []struct {
		name string
		body func()
	}{
		{"next-release-upgrade", l.nextReleaseUpgrade},
		{"rollback", l.rollbackToTheCurrentRelease},
		{"uninstall", l.uninstallTheRolledBackRelease},
		{"reinstall-over-retained-crds", l.reinstallOverRetainedCRDs},
		{"exported-chart-install", l.installTheExportedChart},
	} {
		if !run.Scenario(scenario.name, l.scenario(scenario.body)) {
			return
		}
	}
	run.Logf("e2e crd: PASS phase=uninstall")
}

// lifecycleInputs is what either lifecycle phase was handed. The uninstall
// phase reads the fields the upgrade phase leaves empty.
type lifecycleInputs struct {
	kubeconfig, debugLogs, operatorNamespace, proofNamespace, helmRelease, chartPackage string
	candidateValuesFile, controllerImage, kubernetesVersion                             string
	nextChartPackage, nextValuesFile, nextControllerImage                               string
	registryCredentialsFile, dockerContext                                              string
	externalPostgresContainerID, externalPostgresIP, externalPostgresCredentialsFile    string
}

// lifecycleControllerIdentity is what capture_controller_service_account_identity
// recorded: the one controller Deployment of the release, running the image
// given, and the live ServiceAccount it runs as.
type lifecycleControllerIdentity struct {
	DeploymentName     string
	DeploymentUID      types.UID
	ServiceAccountName string
	ServiceAccountUID  types.UID
	ManagerImage       string
}

// lifecycleImpersonation is the manager's own identity, read from one of its
// Pods, which controller_kube sent requests as.
type lifecycleImpersonation struct {
	username string
	uid      string
	podName  string
	podUID   string
}

// lifecycleRun is what the lifecycle proofs share. The shell kept this in
// globals; each field here names the global it replaces where the name is not
// obvious. Each section of the port keeps its private state in its own field.
type lifecycleRun struct {
	t      *testing.T
	parent *testing.T
	ctx    context.Context
	run    *harness.Run
	phase  string
	in     lifecycleInputs

	cluster *harness.Cluster
	workDir string

	// The contract counters, read from the constants and the generated CRD
	// the candidate carries, never written down here.
	candidateCRDSchemaVersion   int64
	controllerStateVersion      int64
	newerControllerStateVersion int64
	planContractVersion         int32
	kubernetesMajorMinor        string

	// proofControllerImage is PROOF_CONTROLLER_IMAGE, the production image the
	// release values name, which the proof plan binds.
	proofControllerImage string
	// currentReleaseControllerImage is CURRENT_RELEASE_CONTROLLER_IMAGE, and
	// currentReleaseRevision the deployed revision the next release replaced,
	// which the rollback returns to.
	currentReleaseControllerImage string
	currentReleaseRevision        int
	// currentReleaseServiceAccount is current_service_account, the controller
	// ServiceAccount the current release's identity named, which no Pod may
	// run as once the late failure stops the runtime.
	currentReleaseServiceAccount string
	// upgradeValuesFile is UPGRADE_VALUES_FILE, the release's own values as
	// YAML, which every refused upgrade is attempted with.
	upgradeValuesFile string
	// expectedCRDUpgradeRender and expectedReconcileHookName are the rendered
	// CRD hook template and the one weight-0 reconcile Job in it.
	expectedCRDUpgradeRender  []byte
	expectedReconcileHookName string
	// failedUpgradeStderr is failed-upgrade.err: what the last refused Helm
	// upgrade wrote on standard error.
	failedUpgradeStderr []byte
	// lateRevision is late_revision, the Helm revision the late failure left.
	lateRevision int

	// readOnlyJobSchema is READ_ONLY_JOB_SCHEMA, the schema the read-only Job
	// fixture uses this time.
	readOnlyJobSchema string

	// The runtime Deployments and what stopping them snapshotted. The
	// controller snapshot is what the manager's status identity is read from
	// while no controller Deployment stands.
	controllerDeployment string
	rotatorDeployment    string
	controllerSnapshot   *unstructured.Unstructured
	rotatorSnapshot      *unstructured.Unstructured
	// The certificate Secrets the rotator names.
	certificateSecretName        string
	certificateStagingSecretName string
	// impersonation is CONTROLLER_IMPERSONATION_*, set while a guard proof
	// writes as the manager.
	impersonation *lifecycleImpersonation

	// evidence holds object, CRD and Deployment evidence by the key the
	// script's file name gave it.
	evidence map[string][]byte

	// Each section's own state.
	failures     lifecycleFailures
	runtime      lifecycleRuntime
	guards       lifecycleGuards
	runningApply lifecycleRunningApply
}

func newLifecycleRun(t *testing.T, run *harness.Run, phase string, in lifecycleInputs) *lifecycleRun {
	t.Helper()
	l := &lifecycleRun{t: t, parent: t, ctx: run.Context(), run: run, phase: phase, in: in, evidence: map[string][]byte{}}
	for _, command := range []string{"kubectl", "helm", "sh"} {
		if _, err := exec.LookPath(command); err != nil {
			l.fatalf("required command is not installed: %s", command)
		}
	}
	if info, err := os.Stat(in.kubeconfig); err != nil || !info.Mode().IsRegular() {
		l.fatalf("E2E_KUBECONFIG does not name a file")
	}
	if in.debugLogs != "0" && in.debugLogs != "1" {
		l.fatalf("E2E_DEBUG_LOGS must be 0 or 1")
	}
	if err := lifecycleProofNamespaceOK(in.proofNamespace); err != nil {
		l.fatalf("%v", err)
	}
	// The controller-state version the candidate writes, and the one past it:
	// this phase proves both sides of the release fence. The constant is the
	// number the candidate compiles, so the proof cannot agree with the chart
	// until the contract moves and then prove the opposite of what it says.
	l.controllerStateVersion = int64(controllerstate.CurrentVersion)
	l.newerControllerStateVersion = l.controllerStateVersion + 1
	l.planContractVersion = fingerprint.CurrentPlanContractVersion
	crd, err := os.ReadFile(filepath.Join(repositoryRoot, "config", "crd", "bases", "operator.ptah.run_ptahschemas.yaml"))
	l.check(err, "read the candidate CRD")
	if l.candidateCRDSchemaVersion, err = lifecycleCandidateCRDSchemaVersion(crd); err != nil {
		l.fatalf("%v", err)
	}
	if l.cluster, err = harness.Connect(in.kubeconfig); err != nil {
		l.fatalf("%v", err)
	}
	l.workDir, err = os.MkdirTemp("", "ptah-operator-e2e-crd.")
	l.check(err, "create the work directory")
	l.check(os.Chmod(l.workDir, 0o700), "make the work directory private")
	// Registered before anything is created, so a phase that fails part way
	// still removes what it made.
	t.Cleanup(l.cleanup)
	l.verifySupportedServerVersion()
	return l
}

func (l *lifecycleRun) scenario(body func()) func(*testing.T) {
	return func(t *testing.T) {
		l.t = t
		defer func() { l.t = l.parent }()
		body()
	}
}

func (l *lifecycleRun) fatalf(format string, arguments ...any) {
	l.t.Helper()
	l.t.Fatalf("e2e crd: "+format, arguments...)
}

func (l *lifecycleRun) logf(format string, arguments ...any) {
	l.t.Helper()
	l.t.Logf("e2e crd: "+format, arguments...)
}

func (l *lifecycleRun) check(err error, format string, arguments ...any) {
	l.t.Helper()
	if err != nil {
		l.fatalf("%s: %v", fmt.Sprintf(format, arguments...), err)
	}
}

// sleep pauses between two readings, and ends the scenario when the phase's
// own bound ends first.
func (l *lifecycleRun) sleep(duration time.Duration) {
	l.t.Helper()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-l.ctx.Done():
		l.fatalf("the phase's bound ended while it waited: %v", l.ctx.Err())
	case <-timer.C:
	}
}

// debugLogs is E2E_DEBUG_LOGS=1, which prints the stderr of a refused Helm
// operation.
func (l *lifecycleRun) debugLogs() bool { return l.in.debugLogs == "1" }

// kubectl runs kubectl as the administrator the kubeconfig names, as kube
// did.
func (l *lifecycleRun) kubectl(arguments ...string) (stdout, stderr []byte, err error) {
	return l.cluster.Kubectl(l.ctx, arguments...)
}

// mustKubectl runs kubectl and ends the scenario when it fails, as a bare
// kube call did under set -e, with what it said.
func (l *lifecycleRun) mustKubectl(arguments ...string) []byte {
	l.t.Helper()
	stdout, stderr, err := l.kubectl(arguments...)
	if err != nil {
		l.fatalf("kubectl %s: %v: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(stderr)))
	}
	return stdout
}

// kubectlDocument sends one manifest to kubectl -f, as the script's here
// documents did, so the field manager and the apply semantics are kubectl's
// own. The manifest is written to the private work directory first.
func (l *lifecycleRun) kubectlDocument(manifest []byte, arguments ...string) (stdout, stderr []byte, err error) {
	l.t.Helper()
	file, err := os.CreateTemp(l.workDir, "manifest-*.json")
	l.check(err, "create a manifest file")
	path := file.Name()
	_, writeErr := file.Write(manifest)
	closeErr := file.Close()
	l.check(errors.Join(writeErr, closeErr), "write %s", path)
	defer func() { _ = os.Remove(path) }()
	return l.kubectl(append(arguments, "-f", path)...)
}

// helm runs helm against the cluster and returns what it wrote on each stream,
// for a call whose refusal is the proof.
func (l *lifecycleRun) helm(arguments ...string) (stdout, stderr []byte, err error) {
	var out, errOut bytes.Buffer
	command := exec.CommandContext(l.ctx, "helm", append([]string{"--kubeconfig", l.in.kubeconfig}, arguments...)...) //nolint:gosec // Arguments, not a shell.
	command.Stdout, command.Stderr = &out, &errOut
	err = command.Run()
	return out.Bytes(), errOut.Bytes(), err
}

// mustHelm runs helm and ends the scenario when it fails. Helm's standard
// error goes to the phase's as it is written, so the refusal is in the log
// above the failure that reports it.
func (l *lifecycleRun) mustHelm(failure string, arguments ...string) []byte {
	l.t.Helper()
	stdout, err := l.cluster.Helm(l.ctx, arguments...)
	if err != nil {
		if failure == "" {
			failure = "helm " + strings.Join(arguments, " ") + " failed"
		}
		l.fatalf("%s: %v", failure, err)
	}
	return stdout
}

// helmRevision is the release's current revision, and deployedRevision the
// same only when that revision is deployed, as the script's jq selections
// read them.
func (l *lifecycleRun) helmRevision() int {
	l.t.Helper()
	revision, _ := l.helmStatus()
	return revision
}

func (l *lifecycleRun) deployedRevision() int {
	l.t.Helper()
	revision, status := l.helmStatus()
	if status != "deployed" {
		l.fatalf("release %s is %s, not deployed", l.in.helmRelease, cmpOrNone(status))
	}
	return revision
}

func (l *lifecycleRun) helmStatus() (int, string) {
	l.t.Helper()
	output := l.mustHelm("", "status", l.in.helmRelease, "--namespace", l.in.operatorNamespace, "-o", "json")
	var status struct {
		Version json.Number `json:"version"`
		Info    struct {
			Status string `json:"status"`
		} `json:"info"`
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.UseNumber()
	if err := decoder.Decode(&status); err != nil {
		l.fatalf("read the status of release %s: %v", l.in.helmRelease, err)
	}
	revision, err := strconv.Atoi(status.Version.String())
	if err != nil || revision < 1 {
		l.fatalf("release %s reports no revision of at least 1", l.in.helmRelease)
	}
	return revision, status.Info.Status
}

// asController is controller_kube: a client that writes as the manager's own
// Pod-bound identity, which the chart's guards single out.
func (l *lifecycleRun) asController() client.Client {
	l.t.Helper()
	identity := l.controllerImpersonationConfig()
	impersonated, err := l.cluster.As(identity)
	l.check(err, "build a client that writes as the manager")
	return impersonated
}

func (l *lifecycleRun) controllerImpersonationConfig() rest.ImpersonationConfig {
	l.t.Helper()
	identity := l.impersonation
	switch {
	case identity == nil || identity.username == "":
		l.fatalf("controller impersonation username is missing")
	case identity.uid == "":
		l.fatalf("controller impersonation UID is missing")
	case identity.podName == "":
		l.fatalf("controller impersonation Pod name is missing")
	case identity.podUID == "":
		l.fatalf("controller impersonation Pod UID is missing")
	}
	return rest.ImpersonationConfig{
		UserName: identity.username, UID: identity.uid,
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + l.in.operatorNamespace, "system:authenticated"},
		Extra: map[string][]string{
			"authentication.kubernetes.io/pod-name": {identity.podName},
			"authentication.kubernetes.io/pod-uid":  {identity.podUID},
		},
	}
}

// controllerKubectl is controller_kube for a call kubectl has to make: the
// same identity, as flags.
func (l *lifecycleRun) controllerKubectl(arguments ...string) (stdout, stderr []byte, err error) {
	l.t.Helper()
	identity := l.controllerImpersonationConfig()
	flags := []string{"--as", identity.UserName, "--as-uid", identity.UID}
	for _, group := range identity.Groups {
		flags = append(flags, "--as-group", group)
	}
	for _, key := range []string{"authentication.kubernetes.io/pod-name", "authentication.kubernetes.io/pod-uid"} {
		flags = append(flags, "--as-user-extra", key+"="+identity.Extra[key][0])
	}
	return l.kubectl(append(flags, arguments...)...)
}

// managerStatusAccount is the ServiceAccount manager_status_kube wrote as:
// the one controller Deployment's, or the snapshot's while none stands.
func (l *lifecycleRun) managerStatusAccount() string {
	l.t.Helper()
	deployments := &unstructured.UnstructuredList{}
	deployments.SetAPIVersion("apps/v1")
	deployments.SetKind("DeploymentList")
	l.check(l.cluster.Client.List(l.ctx, deployments, client.InNamespace(l.in.operatorNamespace),
		client.MatchingLabels{"app.kubernetes.io/component": "controller"}),
		"could not list the controller Deployment the manager writes status as")
	var account string
	switch len(deployments.Items) {
	case 1:
		account, _, _ = unstructured.NestedString(deployments.Items[0].Object, "spec", "template", "spec", "serviceAccountName")
	case 0:
		if l.controllerSnapshot == nil {
			l.fatalf("no controller Deployment stands and none was snapshotted to write status as")
		}
		account, _, _ = unstructured.NestedString(l.controllerSnapshot.Object, "spec", "template", "spec", "serviceAccountName")
	default:
		l.fatalf("more than one controller Deployment could name the ServiceAccount to write status as")
	}
	if account == "" {
		l.fatalf("the manager Deployment names no ServiceAccount to write status as")
	}
	return "system:serviceaccount:" + l.in.operatorNamespace + ":" + account
}

// asManagerStatus is manager_status_kube: a client that writes as the
// manager's ServiceAccount, the only identity the chart's status guard lets
// write status.
func (l *lifecycleRun) asManagerStatus() client.Client {
	l.t.Helper()
	impersonated, err := l.cluster.As(rest.ImpersonationConfig{UserName: l.managerStatusAccount()})
	l.check(err, "build a client that writes status as the manager")
	return impersonated
}

// managerStatusKubectl is manager_status_kube for a call kubectl has to make.
func (l *lifecycleRun) managerStatusKubectl(arguments ...string) (stdout, stderr []byte, err error) {
	l.t.Helper()
	return l.kubectl(append([]string{"--as", l.managerStatusAccount()}, arguments...)...)
}

// requireMode0600RegularFile is require_mode_0600_regular_file.
func (l *lifecycleRun) requireMode0600RegularFile(path, description string) {
	l.t.Helper()
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		l.fatalf("%s must name a regular non-symlink file", description)
	}
	if info.Mode().Perm() != 0o600 {
		l.fatalf("%s must have mode 0600", description)
	}
}

// fileSHA256 is file_sha256.
func (l *lifecycleRun) fileSHA256(path string) string {
	l.t.Helper()
	content, err := os.ReadFile(path) //nolint:gosec // A path the phase chose.
	l.check(err, "read %s to fingerprint it", path)
	return strings.TrimPrefix(sha256Digest(content), "sha256:")
}

// verifySupportedServerVersion holds the cluster to the exact version the
// driver named, and that version to the support catalog.
func (l *lifecycleRun) verifySupportedServerVersion() {
	l.t.Helper()
	majorMinor, err := lifecycleKubernetesMajorMinor(l.in.kubernetesVersion)
	if err != nil {
		l.fatalf("%v", err)
	}
	l.kubernetesMajorMinor = majorMinor
	resolver := exec.CommandContext(l.ctx, "sh", filepath.Join(repositoryRoot, "hack", "e2e-kubernetes-support-image.sh"), //nolint:gosec // Arguments, not a shell.
		filepath.Join(repositoryRoot, "support", "kubernetes.json"), l.in.kubernetesVersion)
	resolver.Stderr = os.Stderr
	if err := resolver.Run(); err != nil {
		l.fatalf("Kubernetes %s is not an exact member of support/kubernetes.json", l.in.kubernetesVersion)
	}
	server, err := l.cluster.Clientset.Discovery().ServerVersion()
	l.check(err, "read the server version")
	if !lifecycleServerVersionMatches(server.GitVersion, l.in.kubernetesVersion) {
		l.fatalf("cluster reports %s, expected v%s for the guarded-field proof", server.GitVersion, l.in.kubernetesVersion)
	}
}

// productionControllerImageFromValues is production_controller_image_from_values
// over a values file in JSON.
func (l *lifecycleRun) productionControllerImageFromValues(path string) string {
	l.t.Helper()
	values, err := os.ReadFile(path) //nolint:gosec // A path the driver or the phase chose.
	l.check(err, "read %s", path)
	image, err := lifecycleProductionControllerImage(values)
	if err != nil {
		l.fatalf("%v", err)
	}
	return image
}

// releaseValues writes the release's own values to the work directory in the
// format given and returns the path.
func (l *lifecycleRun) releaseValues(name, format string) string {
	l.t.Helper()
	values := l.mustHelm("", "get", "values", l.in.helmRelease, "-n", l.in.operatorNamespace, "-o", format)
	path := filepath.Join(l.workDir, name)
	l.check(os.WriteFile(path, values, 0o600), "write %s", path)
	return path
}

var lifecycleProofGroupVersion = schema.GroupVersion{Group: ptahv1alpha1.GroupVersion.Group, Version: ptahv1alpha1.GroupVersion.Version}

// proofObject reads one proof object as the API server stores it.
func (l *lifecycleRun) proofObject(resource, name string) *unstructured.Unstructured {
	l.t.Helper()
	kind := ""
	for _, proof := range lifecycleProofResources {
		if proof.resource == resource {
			kind = proof.kind
		}
	}
	if kind == "" {
		l.fatalf("%s is not a proof resource", resource)
	}
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(lifecycleProofGroupVersion.WithKind(kind))
	l.check(l.cluster.Client.Get(l.ctx, types.NamespacedName{Namespace: l.in.proofNamespace, Name: name}, object),
		"read %s/%s", resource, name)
	return object
}

// objectEvidence is object_evidence, kept under the key given.
func (l *lifecycleRun) objectEvidence(resource, name, key string) {
	l.t.Helper()
	evidence, err := lifecycleObjectEvidence(l.proofObject(resource, name).Object)
	l.check(err, "encode the evidence of %s/%s", resource, name)
	l.evidence[key] = evidence
}

// assertObjectUnchanged is assert_object_unchanged against the evidence kept
// under the key.
func (l *lifecycleRun) assertObjectUnchanged(resource, name, key string) {
	l.t.Helper()
	before, ok := l.evidence[key]
	if !ok {
		l.fatalf("no evidence of %s/%s was kept as %s", resource, name, key)
	}
	after, err := lifecycleObjectEvidence(l.proofObject(resource, name).Object)
	l.check(err, "encode the evidence of %s/%s", resource, name)
	if !bytes.Equal(before, after) {
		l.logf("%s/%s before: %s", resource, name, before)
		l.logf("%s/%s after:  %s", resource, name, after)
		l.fatalf("%s/%s UID, spec, or status changed during CRD management", resource, name)
	}
}

// proofEvidence and assertProofUnchanged walk the three proof objects, as the
// script's loops over ptahschema, ptahschemaplan and ptahschemaapproval did.
func (l *lifecycleRun) proofEvidence(suffix string) {
	l.t.Helper()
	for _, proof := range lifecycleProofResources {
		l.objectEvidence(proof.resource, lifecycleProofSchema, proof.resource+suffix)
	}
}

func (l *lifecycleRun) assertProofUnchanged(suffix string) {
	l.t.Helper()
	for _, proof := range lifecycleProofResources {
		l.assertObjectUnchanged(proof.resource, lifecycleProofSchema, proof.resource+suffix)
	}
}

// crd reads a CRD as the API server stores it.
func (l *lifecycleRun) crd(name string) (*unstructured.Unstructured, error) {
	crd := &unstructured.Unstructured{}
	crd.SetAPIVersion("apiextensions.k8s.io/v1")
	crd.SetKind("CustomResourceDefinition")
	err := l.cluster.Client.Get(l.ctx, types.NamespacedName{Name: name}, crd)
	return crd, err
}

// crdEvidence is crd_evidence, kept under the key given.
func (l *lifecycleRun) crdEvidence(name, key string) {
	l.t.Helper()
	crd, err := l.crd(name)
	l.check(err, "read CRD %s", name)
	evidence, err := lifecycleCRDEvidence(crd.Object)
	l.check(err, "encode the evidence of CRD %s", name)
	l.evidence[key] = evidence
}

// assertCRDUnchanged is assert_crd_unchanged against the evidence kept under
// the key.
func (l *lifecycleRun) assertCRDUnchanged(name, key string) {
	l.t.Helper()
	before, ok := l.evidence[key]
	if !ok {
		l.fatalf("no evidence of CRD %s was kept as %s", name, key)
	}
	crd, err := l.crd(name)
	l.check(err, "read CRD %s", name)
	after, err := lifecycleCRDEvidence(crd.Object)
	l.check(err, "encode the evidence of CRD %s", name)
	if !bytes.Equal(before, after) {
		l.fatalf("%s identity, annotations, spec, or resourceVersion changed despite failed CRD preflight", name)
	}
}

// crdsEvidence and assertCRDsUnchanged walk the three managed CRDs.
func (l *lifecycleRun) crdsEvidence(suffix string) {
	l.t.Helper()
	for _, name := range lifecycleManagedCRDs {
		l.crdEvidence(name, name+suffix)
	}
}

func (l *lifecycleRun) assertCRDsUnchanged(suffix string) {
	l.t.Helper()
	for _, name := range lifecycleManagedCRDs {
		l.assertCRDUnchanged(name, name+suffix)
	}
}

// crdDescription is the top-level schema description of a CRD's first
// version, which the drift proofs rewrite.
func (l *lifecycleRun) crdDescription(name string) string {
	l.t.Helper()
	crd, err := l.crd(name)
	l.check(err, "read CRD %s", name)
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	if len(versions) == 0 {
		return ""
	}
	first, _ := versions[0].(map[string]any)
	description, _, _ := unstructured.NestedString(first, "schema", "openAPIV3Schema", "description")
	return description
}

// driftCRD writes a description onto a CRD's schema as kubectl, which then
// owns the field, as the script's drift did.
func (l *lifecycleRun) driftCRD(name, description string) {
	l.t.Helper()
	patch := `[{"op":"add","path":"/spec/versions/0/schema/openAPIV3Schema/description","value":` +
		strconv.Quote(description) + `}]`
	l.mustKubectl("patch", "crd", name, "--type=json", "-p="+patch)
}

// annotateCRD sets an annotation on a CRD as kubectl annotate did, or removes
// it when the value is empty.
func (l *lifecycleRun) annotateCRD(name, key, value string) {
	l.t.Helper()
	if value == "" {
		l.mustKubectl("annotate", "crd", name, key+"-")
		return
	}
	l.mustKubectl("annotate", "crd", name, key+"="+value, "--overwrite")
}

// crdAnnotation is one annotation of a CRD.
func (l *lifecycleRun) crdAnnotation(name, key string) string {
	l.t.Helper()
	crd, err := l.crd(name)
	l.check(err, "read CRD %s", name)
	return crd.GetAnnotations()[key]
}

// assertCRDsRetained is the script's `kube get crd` over the three managed
// CRDs after an uninstall.
func (l *lifecycleRun) assertCRDsRetained() {
	l.t.Helper()
	for _, name := range lifecycleManagedCRDs {
		if _, err := l.crd(name); err != nil {
			l.fatalf("CRD %s was not retained: %v", name, err)
		}
	}
}

// deploymentEvidence is deployment_evidence over the release namespace.
func (l *lifecycleRun) deploymentEvidence() []byte {
	l.t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion("apps/v1")
	list.SetKind("DeploymentList")
	l.check(l.cluster.Client.List(l.ctx, list, client.InNamespace(l.in.operatorNamespace)), "list the runtime Deployments")
	items := make([]map[string]any, 0, len(list.Items))
	for index := range list.Items {
		items = append(items, list.Items[index].Object)
	}
	evidence, err := lifecycleDeploymentEvidence(items)
	l.check(err, "encode the Deployment evidence")
	return evidence
}

// prepareExpectedHookNames renders the CRD hook the chart given would run and
// records its one weight-0 reconcile Job.
func (l *lifecycleRun) prepareExpectedHookNames(chart, values string) {
	l.t.Helper()
	l.expectedCRDUpgradeRender = l.mustHelm("", "template", l.in.helmRelease, chart,
		"--namespace", l.in.operatorNamespace, "--values", values, "--show-only", "templates/crd-upgrade.yaml")
	name, err := lifecycleReconcileHookName(l.expectedCRDUpgradeRender)
	if err != nil {
		l.fatalf("%v", err)
	}
	l.expectedReconcileHookName = name
}

// renderedHookJobName is rendered_hook_job_name over the render
// prepareExpectedHookNames recorded, one name per line.
func (l *lifecycleRun) renderedHookJobName(component, weight string) string {
	return strings.Join(lifecycleRenderedHookJobNames(l.expectedCRDUpgradeRender, component, weight), "\n")
}

// createProofObjects creates the schema, plan and approval whose preservation
// the phase proves, with the manager stopped and the admission webhooks gone
// so the plan and approval can carry what only the manager would write.
func (l *lifecycleRun) createProofObjects() {
	l.t.Helper()
	namespace := &unstructured.Unstructured{}
	namespace.SetAPIVersion("v1")
	namespace.SetKind("Namespace")
	l.check(l.cluster.Client.Get(l.ctx, types.NamespacedName{Name: l.in.proofNamespace}, namespace),
		"read namespace %s", l.in.proofNamespace)
	schemaDocument := mustJSON(map[string]any{
		"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahSchema",
		"metadata": map[string]any{"name": lifecycleProofSchema},
		"spec": map[string]any{
			"suspend": true,
			"target": map[string]any{
				"engine": "PostgreSQL", "coordinationKey": "crd-upgrade-proof",
				"urlFrom": map[string]any{"name": "unused-database-url", "key": "url"},
			},
			"desired": map[string]any{
				"ociRef":                 "oci://example.invalid/schema:v1",
				"verificationPolicyFrom": map[string]any{"name": "unused-verification-policy", "key": "policy.yaml"},
			},
		},
	})
	if _, stderr, err := l.kubectlDocument(schemaDocument, "-n", l.in.proofNamespace, "apply"); err != nil {
		l.fatalf("apply PtahSchema %s: %v: %s", lifecycleProofSchema, err, strings.TrimSpace(string(stderr)))
	}
	l.waitForSuspended(lifecycleProofSchema)

	// Only the controller reconciles PtahSchemas, and only its rollout is what
	// the drifted-CRD proof watches, so only the controller is stopped. Losing
	// both is its own recovery path, proven separately by
	// proveRuntimeDeploymentRecovery.
	l.stopControllerDeployment()

	l.mustKubectl("delete", "mutatingwebhookconfiguration", "ptah-operator-admission")
	l.mustKubectl("delete", "validatingwebhookconfiguration", "ptah-operator-admission")
	schemaUID := l.proofObject("ptahschema", lifecycleProofSchema).GetUID()

	planDocument := mustJSON(map[string]any{
		"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahSchemaPlan",
		"metadata": map[string]any{"name": lifecycleProofPlan},
		"spec": map[string]any{
			"contractVersion":          l.planContractVersion,
			"schemaRef":                map[string]any{"name": lifecycleProofSchema, "uid": string(schemaUID)},
			"fingerprint":              "plan-fingerprint",
			"contentDigest":            "sha256:content",
			"size":                     1,
			"artifactDigest":           "sha256:artifact",
			"coordinationDigest":       "sha256:coordination",
			"targetIdentityDigest":     "sha256:target",
			"actualStateFingerprint":   "actual",
			"desiredStateFingerprint":  "desired",
			"policyFingerprint":        "policy",
			"verificationPolicyUID":    "verification-policy-uid",
			"verificationPolicyDigest": "sha256:verification-policy",
			"executionBindingID":       "v1-00000000000000000000000000000000",
			"controllerImage":          l.proofControllerImage,
			"controllerRevision":       "e2e-crd-upgrade",
			"controllerStateVersion":   l.controllerStateVersion,
			"ptahVersion":              "e2e",
			"executorImage":            "e2e.invalid/executor@sha256:0000000000000000000000000000000000000000000000000000000000000000",
			"runnerImage":              "e2e.invalid/runner@sha256:1111111111111111111111111111111111111111111111111111111111111111",
			"runnerProtocolVersion":    runner.ProtocolVersion,
			"dialect":                  "postgresql",
			"destructive":              false,
			"statementCount":           1,
			"chunks": []any{
				map[string]any{"name": "proof-chunk", "index": 0, "digest": "sha256:chunk", "size": 1},
			},
		},
	})
	if _, stderr, err := l.kubectlDocument(planDocument, "-n", l.in.proofNamespace, "create"); err != nil {
		l.fatalf("create PtahSchemaPlan %s: %v: %s", lifecycleProofPlan, err, strings.TrimSpace(string(stderr)))
	}
	planUID := l.proofObject("ptahschemaplan", lifecycleProofPlan).GetUID()
	l.patchProofStatus("ptahschemaplan", lifecycleProofPlan, "Ready")

	approvalDocument := mustJSON(map[string]any{
		"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahSchemaApproval",
		"metadata": map[string]any{"name": lifecycleProofApproval},
		"spec": map[string]any{
			"schemaRef":          map[string]any{"name": lifecycleProofSchema, "uid": string(schemaUID)},
			"planRef":            map[string]any{"name": lifecycleProofPlan, "uid": string(planUID)},
			"planFingerprint":    "plan-fingerprint",
			"approver":           map[string]any{"username": "crd-upgrade-proof"},
			"approvedAt":         "2026-01-01T00:00:00Z",
			"mutationRequestUID": "crd-upgrade-proof",
		},
	})
	if _, stderr, err := l.kubectlDocument(approvalDocument, "-n", l.in.proofNamespace, "create"); err != nil {
		l.fatalf("create PtahSchemaApproval %s: %v: %s", lifecycleProofApproval, err, strings.TrimSpace(string(stderr)))
	}
	l.patchProofStatus("ptahschemaapproval", lifecycleProofApproval, "Accepted")
}

// patchProofStatus writes the status a proof object carries, as the manager.
func (l *lifecycleRun) patchProofStatus(resource, name, condition string) {
	l.t.Helper()
	patch := `{"status":{"observedGeneration":1,"conditions":[{"type":"` + condition +
		`","status":"True","reason":"UpgradeProof","message":"proof status","lastTransitionTime":"2026-01-01T00:00:00Z"}]}}`
	if _, stderr, err := l.managerStatusKubectl("-n", l.in.proofNamespace, "patch", resource, name,
		"--subresource=status", "--type=merge", "-p", patch); err != nil {
		l.fatalf("write the status of %s/%s as the manager: %v: %s", resource, name, err, strings.TrimSpace(string(stderr)))
	}
}

// patchExecutionBindingStateVersion rewrites the proof schema's recorded
// controller-state version, as the manager.
func (l *lifecycleRun) patchExecutionBindingStateVersion(version int64) {
	l.t.Helper()
	patch := `[{"op":"replace","path":"/status/executionBinding/controllerStateVersion","value":` +
		strconv.FormatInt(version, 10) + `}]`
	if _, stderr, err := l.managerStatusKubectl("-n", l.in.proofNamespace, "patch", "ptahschema", lifecycleProofSchema,
		"--subresource=status", "--type=json", "-p="+patch); err != nil {
		l.fatalf("rewrite the controller-state version of %s as the manager: %v: %s",
			lifecycleProofSchema, err, strings.TrimSpace(string(stderr)))
	}
}

func mustJSON(document any) []byte {
	encoded, err := json.Marshal(document)
	if err != nil {
		panic(err)
	}
	return encoded
}

// cleanup is the script's EXIT trap: whatever happened, stop the barrier,
// remove the late-failure blocker, the shared-namespace probe and the guard
// owner, and remove the work directory, unless E2E_KEEP_ON_FAILURE=1 keeps a
// failed phase's objects to read.
func (l *lifecycleRun) cleanup() {
	t := l.parent
	l.t = t
	retain := t.Failed() && os.Getenv("E2E_KEEP_ON_FAILURE") == "1"
	// A barrier left holding an advisory lock keeps the Apply it blocks alive
	// past the phase, and the next phase meets a database nobody can change.
	l.cleanupRunningApply()
	for _, remove := range []func(bool) error{l.cleanupFailures, l.cleanupGuards} {
		if err := remove(retain); err != nil {
			t.Errorf("e2e crd: %v", err)
		}
	}
	if retain {
		t.Logf("e2e crd: E2E_KEEP_ON_FAILURE=1: retaining work directory %s and the proof objects in namespaces %s and %s",
			l.workDir, l.in.operatorNamespace, l.in.proofNamespace)
		return
	}
	if l.workDir != "" {
		if err := os.RemoveAll(l.workDir); err != nil {
			t.Errorf("e2e crd: remove the work directory %s: %v", l.workDir, err)
		}
	}
}

// deleteIgnoringAbsence deletes an object and treats one already gone as
// deleted, as --ignore-not-found did.
func (l *lifecycleRun) deleteIgnoringAbsence(ctx context.Context, object client.Object) error {
	if err := l.cluster.Client.Delete(ctx, object); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
