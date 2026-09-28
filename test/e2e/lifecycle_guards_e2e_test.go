//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// The manager's write guards and the runtime singleton, ported from
// hack/e2e-crd-upgrade.sh: controller_write_evidence through
// prove_runtime_deployment_recovery, and assert_controller_downgrade_blocked
// through prove_controller_downgrade_guard.

// lifecycleGuards is the state the manager's write guards and the runtime
// singleton keep.
type lifecycleGuards struct {
	// owner is CONTROLLER_GUARD_OWNER: set while the ConfigMap the
	// owner-reference probe names exists, so the cleanup removes it.
	owner string
}

// guardSchema reads the proof schema as the API server stores it.
func (l *lifecycleRun) guardSchema() map[string]any {
	l.t.Helper()
	return l.proofObject("ptahschema", lifecycleProofSchema).Object
}

// guardWriteEvidence is controller_write_evidence.
func (l *lifecycleRun) guardWriteEvidence() []byte {
	l.t.Helper()
	evidence, err := lifecycleGuardWriteEvidence(l.guardSchema())
	l.check(err, "encode the controller write evidence of %s", lifecycleProofSchema)
	return evidence
}

// guardManifestFile writes a probe manifest to the private work directory for
// a kubectl call made as the manager, and returns its path and its removal.
func (l *lifecycleRun) guardManifestFile(manifest []byte) (string, func()) {
	l.t.Helper()
	file, err := os.CreateTemp(l.workDir, "guard-probe-*.json")
	l.check(err, "create a guard probe manifest")
	path := file.Name()
	_, writeErr := file.Write(manifest)
	closeErr := file.Close()
	l.check(errors.Join(writeErr, closeErr), "write %s", path)
	return path, func() { _ = os.Remove(path) }
}

// expectControllerWriteDenial is expect_controller_write_denial: a
// server-side dry run of the patch, as the manager's own Pod-bound identity,
// that the write guard has to refuse with its own words.
func (l *lifecycleRun) expectControllerWriteDenial(description, patchType, patchBody string) {
	l.t.Helper()
	stdout, stderr, err := l.controllerKubectl("-n", l.in.proofNamespace, "patch", "ptahschema", lifecycleProofSchema,
		"--type", patchType, "-p", patchBody, "--dry-run=server", "-o", "json")
	if err == nil {
		l.fatalf("controller identity was allowed to mutate %s", description)
	}
	if !lifecycleGuardSaid(lifecycleGuardWriteDenial, stderr, stdout) {
		// The refusal that did come back is the API server's answer to a
		// patch of this resource, which carries no credential, and it is the
		// only evidence of which admission step refused instead.
		l.fatalf("controller %s mutation failed without the exact write-guard denial: %s", description, lifecycleGuardHead(stderr))
	}
}

// expectControllerJobAPIAcceptance is expect_controller_job_api_acceptance:
// the administrator's server-side dry run of the probe succeeds, and the
// answer still carries the field, so a refusal as the manager is the policy's
// rather than a field the API server dropped.
func (l *lifecycleRun) expectControllerJobAPIAcceptance(probe lifecycleGuardFieldProbe, manifest []byte) {
	l.t.Helper()
	stdout, stderr, err := l.kubectlDocument(manifest, "create", "--dry-run=server", "-o", "json")
	if err != nil {
		_, _ = os.Stderr.Write(stderr)
		l.fatalf("Kubernetes %s did not accept the %s guard probe", l.kubernetesMajorMinor, probe.field)
	}
	var response map[string]any
	if json.Unmarshal(stdout, &response) != nil || !probe.retained(response) {
		l.fatalf("Kubernetes %s dropped the %s guard probe before admission", l.kubernetesMajorMinor, probe.field)
	}
}

// expectControllerJobVAPDenial is expect_controller_job_vap_denial: the
// manager's server-side dry run of the probe is refused by the
// controller-object policy.
func (l *lifecycleRun) expectControllerJobVAPDenial(description string, manifest []byte) {
	l.t.Helper()
	path, remove := l.guardManifestFile(manifest)
	defer remove()
	stdout, stderr, err := l.controllerKubectl("create", "--dry-run=server", "-o", "json", "-f", path)
	if err == nil {
		l.fatalf("controller identity was allowed to create a Job with %s", description)
	}
	if !lifecycleGuardSaid(lifecycleGuardJobVAPDenial, stdout, stderr) {
		_, _ = os.Stderr.Write(stderr)
		l.fatalf("controller %s probe failed without the exact controller-object VAP denial", description)
	}
}

// proveControllerObjectSupportedWindowGuard is
// prove_controller_object_supported_window_guard: a Job the manager could
// really create reaches the webhook's semantic boundary, the policy refuses
// the same Job stamped with another release's manager, and every field this
// Kubernetes minor adds that the manager must never set is kept by the API
// server and refused by the policy.
func (l *lifecycleRun) proveControllerObjectSupportedWindowGuard() {
	l.t.Helper()
	l.logf("proving controller Job guarded fields on Kubernetes %s", l.kubernetesMajorMinor)
	probes, known := lifecycleGuardFieldProbes(l.kubernetesMajorMinor)
	if !known {
		l.fatalf("Kubernetes %s has no declared guarded-field probe set; decide what it adds before running on it",
			l.kubernetesMajorMinor)
	}
	job := l.evidence[readOnlyJobDocumentKey(currentReadOnlyJobSchema)]
	if len(job) == 0 {
		l.fatalf("current-release read-only Job evidence is unavailable for the controller-object proof")
	}
	if !lifecycleImageIdentityOK(l.proofControllerImage) {
		l.fatalf("controller-object proof lacks an exact candidate controller image")
	}
	base, err := lifecycleGuardBaseManifest(job, l.proofControllerImage, strconv.FormatInt(l.controllerStateVersion, 10))
	l.check(err, "build the controller-object baseline")
	baseManifest := lifecycleGuardJSON(base)

	// This boundary is the webhook's answer, and the manager rolled out a
	// moment ago, so its Service can still hold an endpoint that refuses the
	// connection. Retry only while the API server reports it could not reach
	// the webhook at all: a refusal that arrives is the answer under test,
	// whatever it says.
	path, remove := l.guardManifestFile(baseManifest)
	defer remove()
	var stdout, stderr []byte
	for deadline := time.Now().Add(120 * time.Second); ; {
		stdout, stderr, err = l.controllerKubectl("create", "--dry-run=server", "-o", "json", "-f", path)
		if err == nil {
			l.fatalf("controller-object baseline bypassed the semantic Job write boundary")
		}
		if !lifecycleGuardWebhookUnreachable.Match(stderr) {
			break
		}
		if !time.Now().Before(deadline) {
			_, _ = os.Stderr.Write(stderr)
			l.fatalf("controller write webhook stayed unreachable for the baseline boundary")
		}
		l.sleep(2 * time.Second)
	}
	if lifecycleGuardSaid(lifecycleGuardJobVAPDenial, stdout, stderr) {
		l.fatalf("controller-object baseline does not satisfy the structural VAP contract")
	}
	if !lifecycleGuardSaid(lifecycleGuardJobSemanticBoundary, stdout, stderr) {
		_, _ = os.Stderr.Write(stderr)
		l.fatalf("controller-object baseline did not reach the semantic Job write boundary")
	}

	// The policy carries this release's manager image, so a Job stamped with
	// any other is refused before the webhook is asked: a manager left over
	// from another release cannot create one. The baseline above carries this
	// release's image and reached the webhook, so the refusal is the image.
	otherRelease := lifecycleGuardCopy(base).(map[string]any)
	l.check(lifecycleGuardStampOtherReleaseImage(otherRelease), "stamp the baseline with another release's manager image")
	l.expectControllerJobVAPDenial("another release's manager image", lifecycleGuardJSON(otherRelease))

	if len(probes) == 0 {
		l.logf("Kubernetes %s has no requested version-specific guarded-field probe", l.kubernetesMajorMinor)
	}
	for _, probe := range probes {
		manifest := lifecycleGuardCopy(base).(map[string]any)
		l.check(probe.mutate(manifest), "set %s on the baseline", probe.field)
		encoded := lifecycleGuardJSON(manifest)
		l.expectControllerJobAPIAcceptance(probe, encoded)
		l.expectControllerJobVAPDenial(probe.field, encoded)
	}
	l.logf("controller Job guarded-field proof passed on Kubernetes %s", l.kubernetesMajorMinor)
}

// proveControllerDirectWriteWebhook is prove_controller_direct_write_webhook.
// A plan's bytes are a PtahSchemaPlanChunk, and an Apply mounts them through
// a ConfigMap of the same shape. The manager creates both, so both go through
// the uncached webhook, and each is probed on its own: a webhook
// configuration that lost either resource would admit it here.
func (l *lifecycleRun) proveControllerDirectWriteWebhook() {
	l.t.Helper()
	for _, probe := range lifecycleGuardDirectWriteProbes(l.in.proofNamespace, lifecycleProofSchema) {
		l.expectControllerDirectWriteRefusal(probe.kind, lifecycleGuardJSON(probe.manifest))
	}
}

// expectControllerDirectWriteRefusal is expect_controller_direct_write_refusal.
//
// This runs immediately after the manager rollout, so the webhook may not be
// serving yet. Every webhook here is failurePolicy: Fail, so an unready one
// still rejects the create -- a refusal satisfied by the API server rather
// than by the boundary this proves, with a connection error instead of the
// semantic message. So it retries until the rejection is the semantic one.
// Acceptance is not retried: with failurePolicy: Fail nothing accepts the
// probe while the webhook is away, so an accepted create is the defect the
// probe exists to catch.
func (l *lifecycleRun) expectControllerDirectWriteRefusal(kind string, manifest []byte) {
	l.t.Helper()
	path, remove := l.guardManifestFile(manifest)
	defer remove()
	var stderr []byte
	for deadline := time.Now().Add(60 * time.Second); ; {
		var err error
		_, stderr, err = l.controllerKubectl("create", "--dry-run=server", "-f", path)
		if err == nil {
			l.fatalf("controller direct-write webhook accepted a structurally valid %s without a persisted plan", kind)
		}
		if lifecycleGuardSaid(lifecycleGuardDirectWriteRefusal, stderr) {
			return
		}
		if !time.Now().Before(deadline) {
			break
		}
		l.sleep(time.Second)
	}
	// What did reject the write is printed before giving up: swallowing it
	// once cost a whole run to work out what had refused.
	_, _ = os.Stderr.Write(stderr)
	l.fatalf("controller direct-write probe for a %s did not reach the uncached semantic webhook boundary", kind)
}

// proveControllerWriteGuard is prove_controller_write_guard: as the manager's
// own Pod-bound identity, every desired-state mutation of a schema is refused,
// a status write through the main resource changes nothing, and the one write
// it is allowed -- its own active-operation finalizer -- lands and nothing
// else changes.
func (l *lifecycleRun) proveControllerWriteGuard() {
	l.t.Helper()
	l.logf("proving the controller desired-state write boundary")
	l.captureControllerImpersonationIdentity()

	before := l.guardWriteEvidence()
	l.proveControllerDirectWriteWebhook()
	l.proveControllerObjectSupportedWindowGuard()
	l.stopControllerDeployment()

	suspendPatch, err := lifecycleGuardSuspendPatch(l.guardSchema())
	if err != nil {
		l.fatalf("%v", err)
	}
	l.expectControllerWriteDenial("spec", "merge", string(suspendPatch))
	l.expectControllerWriteDenial("labels", "merge",
		`{"metadata":{"labels":{"operator.ptah.run/controller-write-probe":"forbidden"}}}`)
	l.expectControllerWriteDenial("annotations", "merge",
		`{"metadata":{"annotations":{"operator.ptah.run/controller-write-probe":"forbidden"}}}`)

	l.guards.owner = lifecycleGuardOwnerConfigMap
	owner := &corev1.ConfigMap{}
	owner.Namespace, owner.Name = l.in.proofNamespace, l.guards.owner
	l.check(l.cluster.Client.Create(l.ctx, owner, client.FieldOwner(harness.FieldOwner)),
		"create ConfigMap %s/%s", l.in.proofNamespace, l.guards.owner)
	l.check(l.cluster.Client.Get(l.ctx, types.NamespacedName{Namespace: l.in.proofNamespace, Name: l.guards.owner}, owner),
		"read ConfigMap %s/%s", l.in.proofNamespace, l.guards.owner)
	l.expectControllerWriteDenial("ownerReferences", "merge", string(lifecycleGuardOwnerPatch(l.guards.owner, string(owner.UID))))
	l.expectControllerWriteDenial("a foreign finalizer", "merge",
		`{"metadata":{"finalizers":["operator.ptah.run/foreign-operation"]}}`)

	statusBefore, err := lifecycleGuardStatusEvidence(l.guardSchema())
	l.check(err, "encode the status of %s", lifecycleProofSchema)
	stdout, stderr, err := l.controllerKubectl("-n", l.in.proofNamespace, "patch", "ptahschema", lifecycleProofSchema,
		"--type", "merge", "-p", `{"status":{"phase":"ControllerWriteProbe"}}`, "--dry-run=server", "-o", "json")
	if err == nil {
		var response map[string]any
		if err := json.Unmarshal(stdout, &response); err != nil {
			l.fatalf("the answer to the controller status dry run is not a JSON document: %v", err)
		}
		statusResponse, err := lifecycleGuardStatusEvidence(response)
		l.check(err, "encode the status in the dry-run answer")
		if !bytes.Equal(statusBefore, statusResponse) {
			l.fatalf("the main PtahSchema endpoint accepted a controller status mutation")
		}
	} else if !lifecycleGuardSaid(lifecycleGuardWriteDenial, stderr) {
		l.fatalf("controller main-resource status mutation failed without a safe API or write-guard refusal")
	}

	finalizers, err := lifecycleGuardFinalizers(l.guardSchema())
	l.check(err, "read the finalizers of %s", lifecycleProofSchema)
	if lifecycleGuardHasActiveOperation(finalizers) {
		l.fatalf("proof PtahSchema already has the active-operation finalizer")
	}
	if _, stderr, err := l.controllerKubectl("-n", l.in.proofNamespace, "patch", "ptahschema", lifecycleProofSchema,
		"--type", "merge", "-p", string(lifecycleGuardFinalizersPatch(finalizers, true))); err != nil {
		l.fatalf("controller identity could not add its active-operation finalizer: %v: %s", err, strings.TrimSpace(string(stderr)))
	}
	if !lifecycleGuardAddedExactly(l.guardSchema(), finalizers) {
		l.fatalf("controller identity did not add exactly its active-operation finalizer")
	}
	if _, stderr, err := l.controllerKubectl("-n", l.in.proofNamespace, "patch", "ptahschema", lifecycleProofSchema,
		"--type", "merge", "-p", string(lifecycleGuardFinalizersPatch(finalizers, false))); err != nil {
		l.fatalf("controller identity could not remove its active-operation finalizer: %v: %s", err, strings.TrimSpace(string(stderr)))
	}

	l.mustKubectl("-n", l.in.proofNamespace, "delete", "configmap", l.guards.owner, "--wait=true")
	l.guards.owner = ""
	if after := l.guardWriteEvidence(); !bytes.Equal(before, after) {
		l.logf("controller write evidence before: %s", before)
		l.logf("controller write evidence after:  %s", after)
		l.fatalf("controller write proof changed anything except the temporary active-operation finalizer")
	}

	l.startControllerDeployment()
	l.waitRuntimeReady()
	l.clearControllerImpersonationIdentity()
	l.logf("controller desired-state and direct-write boundaries passed")
}

// proveRuntimeDeploymentRecovery is prove_runtime_deployment_recovery. An
// active release whose two runtime Deployments were both deleted -- a GitOps
// prune, a namespace-wide delete that spared the release's other objects --
// can only be restored by an upgrade, so the upgrade has to run in that state
// (stokaro/ptah-operator#10). The reconcile hook finds nothing to stop, and
// Helm creates both Deployments again.
func (l *lifecycleRun) proveRuntimeDeploymentRecovery() {
	l.t.Helper()
	l.logf("proving an active release survives losing both runtime Deployments")
	l.stopRuntimeDeployments()
	for _, name := range []string{l.controllerDeployment, l.rotatorDeployment} {
		err := l.cluster.Client.Get(l.ctx, types.NamespacedName{Namespace: l.in.operatorNamespace, Name: name}, &appsv1.Deployment{})
		switch {
		case err == nil:
			l.fatalf("%s survived the delete this proof depends on", name)
		case !apierrors.IsNotFound(err):
			l.fatalf("could not read Deployment %s to confirm the delete this proof depends on: %v", name, err)
		}
	}
	if l.upgradeValuesFile == "" {
		l.fatalf("upgrade values file is not configured")
	}
	if _, stderr, err := l.helm("upgrade", l.in.helmRelease, l.in.chartPackage, "--namespace", l.in.operatorNamespace,
		"--values", l.upgradeValuesFile, "--wait", "--timeout", "5m"); err != nil {
		if l.debugLogs() {
			_, _ = os.Stderr.Write(stderr)
		}
		l.fatalf("the upgrade that restores both deleted runtime Deployments was refused")
	}
	l.waitRuntimeReady()
	l.logf("both runtime Deployments were restored by the upgrade")
}

// assertControllerDowngradeBlocked is assert_controller_downgrade_blocked: the
// manager's Pods fail their preflight, and the certificate rotator, which a
// controller-only downgrade does not concern, stays ready.
func (l *lifecycleRun) assertControllerDowngradeBlocked() {
	l.t.Helper()
	l.assertExplicitRuntimeGuard("future stored controller state", "controller", 2)
	rotator := &appsv1.Deployment{}
	err := l.cluster.Client.Get(l.ctx, types.NamespacedName{Namespace: l.in.operatorNamespace, Name: l.rotatorDeployment}, rotator)
	if err != nil || rotator.Status.AvailableReplicas != 1 {
		l.fatalf("controller-only downgrade preflight prevented the certificate rotator from remaining ready")
	}
}

// admissionConfiguration reads one of the admission singleton's
// configurations as the API server stores it.
func (l *lifecycleRun) admissionConfiguration(kind string) *unstructured.Unstructured {
	l.t.Helper()
	configuration := &unstructured.Unstructured{}
	configuration.SetAPIVersion("admissionregistration.k8s.io/v1")
	configuration.SetKind(kind)
	l.check(l.cluster.Client.Get(l.ctx, types.NamespacedName{Name: lifecycleGuardAdmissionSingleton}, configuration),
		"read %s %s", kind, lifecycleGuardAdmissionSingleton)
	return configuration
}

// restartRelease deletes every Pod of the release without waiting, so the
// runtime starts again against what the proof just put back.
func (l *lifecycleRun) restartRelease(selector string) {
	l.t.Helper()
	l.mustKubectl("-n", l.in.operatorNamespace, "delete", "pod", "-l", selector, "--wait=false")
	l.waitRuntimeReady()
}

// proveRuntimeSingletonGuard is prove_runtime_singleton_guard: the runtime
// refuses to start with the admission singleton incomplete, owned by another
// release, or drifted in behavior, and starts again once it is whole.
func (l *lifecycleRun) proveRuntimeSingletonGuard() {
	l.t.Helper()
	release := "app.kubernetes.io/instance=" + l.in.helmRelease

	l.logf("proving runtime rejection of an incomplete singleton")
	snapshot, err := lifecycleGuardAdmissionSnapshot(l.admissionConfiguration("ValidatingWebhookConfiguration").Object)
	l.check(err, "snapshot the validating webhook configuration")
	l.stopRuntimeDeployments()
	l.mustKubectl("delete", "validatingwebhookconfiguration", lifecycleGuardAdmissionSingleton)
	l.startRuntimeDeployments()
	l.assertRuntimeBlocked("incomplete admission singleton")
	if _, stderr, err := l.kubectlDocument(snapshot, "create"); err != nil {
		l.fatalf("restore validatingwebhookconfiguration %s: %v: %s", lifecycleGuardAdmissionSingleton, err,
			strings.TrimSpace(string(stderr)))
	}
	l.restartRelease(release)

	// The runtime refuses to serve an admission singleton another release
	// owns. Nothing stops an administrator writing that annotation, so the
	// refusal is the runtime's: its Pods do not start until the singleton
	// names this release again.
	l.logf("proving the runtime refuses an admission singleton another release owns")
	l.stopRuntimeDeployments()
	l.mustKubectl("annotate", "mutatingwebhookconfiguration", lifecycleGuardAdmissionSingleton,
		"operator.ptah.run/release-name=foreign-release", "--overwrite")
	l.startRuntimeDeployments()
	l.assertRuntimeBlocked("foreign admission singleton owner")
	l.mustKubectl("annotate", "mutatingwebhookconfiguration", lifecycleGuardAdmissionSingleton,
		"operator.ptah.run/release-name="+l.in.helmRelease, "--overwrite")
	l.restartRelease(release)

	l.logf("proving runtime rejection of drifted admission behavior")
	firstName, service := lifecycleGuardFirstWebhook(l.admissionConfiguration("ValidatingWebhookConfiguration").Object)
	if service == "" {
		l.fatalf("admission webhook Service name is empty")
	}
	if firstName != lifecycleGuardApprovalWebhook {
		l.fatalf("approval validating webhook is not in its rendered position")
	}
	l.stopRuntimeDeployments()
	l.mustKubectl("patch", "mutatingwebhookconfiguration", lifecycleGuardAdmissionSingleton, "--type=json",
		`-p=[{"op":"replace","path":"/webhooks/0/failurePolicy","value":"Ignore"}]`)
	l.mustKubectl("patch", "validatingwebhookconfiguration", lifecycleGuardAdmissionSingleton, "--type=json",
		`-p=[{"op":"replace","path":"/webhooks/0/clientConfig/service/name","value":"foreign-service"}]`)
	l.startRuntimeDeployments()
	l.assertRuntimeBlocked("drifted admission behavior")
	l.mustKubectl("patch", "mutatingwebhookconfiguration", lifecycleGuardAdmissionSingleton, "--type=json",
		`-p=[{"op":"replace","path":"/webhooks/0/failurePolicy","value":"Fail"}]`)
	l.mustKubectl("patch", "validatingwebhookconfiguration", lifecycleGuardAdmissionSingleton, "--type=json",
		"-p="+string(lifecycleGuardJSON([]any{map[string]any{
			"op": "replace", "path": "/webhooks/0/clientConfig/service/name", "value": service,
		}})))
	l.restartRelease(release)
}

// proveControllerDowngradeGuard is prove_controller_downgrade_guard: with a
// schema recording a controller-state version newer than the candidate's,
// the candidate manager refuses to start and rewrites nothing, and starts
// again once the recorded version is its own.
func (l *lifecycleRun) proveControllerDowngradeGuard() {
	l.t.Helper()
	l.logf("proving controller downgrade preflight")
	l.stopControllerDeployment()
	stored, found, err := unstructuredInt64(l.guardSchema(), "status", "executionBinding", "controllerStateVersion")
	if err != nil || !found || stored != l.controllerStateVersion {
		recorded := ""
		if found && err == nil {
			recorded = strconv.FormatInt(stored, 10)
		}
		l.fatalf("proof PtahSchema controller state version is %s, expected %d", recorded, l.controllerStateVersion)
	}
	l.patchExecutionBindingStateVersion(l.newerControllerStateVersion)
	future, err := lifecycleGuardStatusExactly(l.guardSchema())
	l.check(err, "encode the future controller state of %s", lifecycleProofSchema)
	l.startControllerDeployment()
	l.assertControllerDowngradeBlocked()
	after, err := lifecycleGuardStatusExactly(l.guardSchema())
	l.check(err, "encode the controller state of %s", lifecycleProofSchema)
	if !bytes.Equal(future, after) {
		l.fatalf("blocked candidate manager rewrote future PtahSchema state")
	}
	l.patchExecutionBindingStateVersion(l.controllerStateVersion)
	l.restartRelease("app.kubernetes.io/component=controller")
}

// cleanupGuards is the script's EXIT trap for this section: unless the phase
// keeps its objects, remove the ConfigMap the owner-reference probe named.
func (l *lifecycleRun) cleanupGuards(retain bool) error {
	if retain || l.guards.owner == "" || l.cluster == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	owner := &corev1.ConfigMap{}
	owner.Namespace, owner.Name = l.in.proofNamespace, l.guards.owner
	if err := l.deleteIgnoringAbsence(ctx, owner); err != nil {
		return fmt.Errorf("remove the controller guard owner ConfigMap %s/%s: %w", l.in.proofNamespace, l.guards.owner, err)
	}
	l.guards.owner = ""
	return nil
}
