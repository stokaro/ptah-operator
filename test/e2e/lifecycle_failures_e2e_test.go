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

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The refused upgrades, the late failure and the rollbacks: lines 447 to 786
// of hack/e2e-crd-upgrade.sh, in its order and with its messages.

// lifecycleFailures is the state the refused upgrades, the late failure and
// the rollbacks keep.
type lifecycleFailures struct {
	// blockerWebhook is LATE_FAILURE_BLOCKER_WEBHOOK, set while the blocker
	// may exist, so the cleanup removes it.
	blockerWebhook string
	// sharedNamespaceProbe is SHARED_NAMESPACE_PROBE, set while the foreign
	// CronJob may exist: left behind, it makes every later upgrade in the
	// release namespace fail the chart's shared-namespace check.
	sharedNamespaceProbe string
	// candidate is the late-failure candidate as it was when the late failure
	// was proved, which the retry has to be exactly.
	candidate lifecycleFailureCandidate
}

// printDebug writes what a refused Helm operation said to the phase's
// standard error when E2E_DEBUG_LOGS=1, as `cat file >&2` did.
func (l *lifecycleRun) printDebug(stderr []byte) {
	if l.debugLogs() {
		_, _ = os.Stderr.Write(stderr)
	}
}

// expectUpgradeFailureWithoutDeploymentChange is
// expect_upgrade_failure_without_deployment_change: the upgrade is refused by
// the reconcile hook of the revision it wrote, which the structured Helm
// status of that revision proves, and no runtime Deployment changed. The
// upgrade's standard error is kept once and printed only under
// E2E_DEBUG_LOGS; it is never read as evidence.
func (l *lifecycleRun) expectUpgradeFailureWithoutDeploymentChange(description string, extra ...string) {
	l.t.Helper()
	if l.upgradeValuesFile == "" {
		l.fatalf("upgrade values file is not configured")
	}
	if l.expectedReconcileHookName == "" {
		l.fatalf("rendered reconcile hook name is unavailable")
	}
	failedRevision := l.helmRevision() + 1
	before := l.deploymentEvidence()
	privileges := l.snapshotRuntimePrivileges()
	_, stderr, err := l.helm(append([]string{"upgrade", l.in.helmRelease, l.in.chartPackage,
		"--namespace", l.in.operatorNamespace, "--values", l.upgradeValuesFile, "--wait", "--timeout", "2m"}, extra...)...)
	l.failedUpgradeStderr = stderr
	if err == nil {
		l.fatalf("%s unexpectedly succeeded", description)
	}
	status, _, err := l.helm("status", l.in.helmRelease, "--namespace", l.in.operatorNamespace,
		"--revision", strconv.Itoa(failedRevision), "-o", "json")
	if err != nil {
		if l.debugLogs() {
			_, _ = fmt.Fprintln(os.Stderr, "e2e crd: E2E_DEBUG_LOGS=1: stderr of the refused upgrade follows")
			l.printDebug(l.failedUpgradeStderr)
		}
		l.fatalf("%s did not retain structured Helm evidence for failed revision %d", description, failedRevision)
	}
	if err := lifecycleFailedHookEvidence(status, failedRevision, l.expectedReconcileHookName); err != nil {
		l.logf("the failed revision's status: %s", lifecycleFailureStatusSummary(status))
		l.fatalf("%s lacks exact revision-bound failed reconcile evidence: %v", description, err)
	}
	if after := l.deploymentEvidence(); !bytes.Equal(before, after) {
		l.fatalf("%s mutated runtime Deployments", description)
	}
	l.assertRuntimePrivilegesUnchanged(privileges, description)
}

// expectUpgradeRenderFailureWithoutDeploymentChange is
// expect_upgrade_render_failure_without_deployment_change: the chart refuses
// to render, so Helm writes no revision and no runtime Deployment changes.
// What Helm wrote on standard error stays in l.failedUpgradeStderr for the
// caller to hold to the refusal it expects.
func (l *lifecycleRun) expectUpgradeRenderFailureWithoutDeploymentChange(description string, extra ...string) {
	l.t.Helper()
	if l.upgradeValuesFile == "" {
		l.fatalf("upgrade values file is not configured")
	}
	beforeRevision := l.helmRevision()
	before := l.deploymentEvidence()
	privileges := l.snapshotRuntimePrivileges()
	_, stderr, err := l.helm(append([]string{"upgrade", l.in.helmRelease, l.in.chartPackage,
		"--namespace", l.in.operatorNamespace, "--values", l.upgradeValuesFile, "--wait", "--timeout", "2m"}, extra...)...)
	l.failedUpgradeStderr = stderr
	if err == nil {
		l.fatalf("%s unexpectedly succeeded", description)
	}
	if afterRevision := l.helmRevision(); afterRevision != beforeRevision {
		l.fatalf("%s created Helm revision %d before template validation, expected %d", description, afterRevision, beforeRevision)
	}
	if after := l.deploymentEvidence(); !bytes.Equal(before, after) {
		l.fatalf("%s mutated runtime Deployments", description)
	}
	l.assertRuntimePrivilegesUnchanged(privileges, description)
}

// proveSharedReleaseNamespaceRefusal: the release namespace is part of the
// operator's trusted computing base, so the chart refuses to install into one
// that runs somebody else's workloads. The foreign workload is a suspended
// CronJob: a workload controller that starts no Pod and that no admission
// policy of this chart matches, so the refusal can only be the chart's. The
// same upgrade with the override is then rendered as a server-side dry run,
// which reads the same cluster and runs no hook.
func (l *lifecycleRun) proveSharedReleaseNamespaceRefusal() {
	l.t.Helper()
	l.logf("proving the chart refuses a release namespace that runs foreign workloads")
	l.failures.sharedNamespaceProbe = lifecycleFailureSharedProbe
	if _, stderr, err := l.kubectlDocument(mustJSON(lifecycleFailureSharedNamespaceProbe()),
		"-n", l.in.operatorNamespace, "create", "--request-timeout=15s"); err != nil {
		l.fatalf("create CronJob %s: %v: %s", lifecycleFailureSharedProbe, err, strings.TrimSpace(string(stderr)))
	}
	l.expectUpgradeRenderFailureWithoutDeploymentChange("shared release namespace upgrade")
	refusal := lifecycleFailureSharedNamespaceRefusal(l.in.operatorNamespace, l.in.helmRelease, l.failures.sharedNamespaceProbe)
	if !bytes.Contains(l.failedUpgradeStderr, []byte(refusal)) {
		l.fatalf("an upgrade into a release namespace running a foreign CronJob failed without the shared-namespace refusal naming it")
	}
	if _, stderr, err := l.helm("upgrade", l.in.helmRelease, l.in.chartPackage, "--namespace", l.in.operatorNamespace,
		"--values", l.upgradeValuesFile, "--set", "releaseNamespace.allowSharedNamespace=true", "--dry-run=server"); err != nil {
		_, _ = os.Stderr.Write(stderr)
		l.fatalf("releaseNamespace.allowSharedNamespace=true did not admit the upgrade over a foreign CronJob")
	}
	l.mustKubectl("-n", l.in.operatorNamespace, "delete", "cronjob", l.failures.sharedNamespaceProbe,
		"--wait=true", "--timeout=60s", "--request-timeout=15s")
	l.failures.sharedNamespaceProbe = ""
}

// createLateFailureBlocker stands in for whatever fails after the hook
// stopped the runtime and before Helm applied the new Deployments: the one
// partial upgrade that leaves no runtime running, since the hook scaled the
// predecessor to zero and nothing replaced it. It is a webhook with no
// backend that matches only a write of this release's two Deployments
// carrying the candidate image, so the hook's scale-down passes it and Helm's
// apply of the candidate is refused.
func (l *lifecycleRun) createLateFailureBlocker() {
	l.t.Helper()
	l.failures.blockerWebhook = lifecycleFailureBlockerWebhook
	l.runtimeDeploymentNames()
	blocker := lifecycleFailureBlocker(l.in.operatorNamespace, l.controllerDeployment, l.rotatorDeployment, l.in.nextControllerImage)
	if _, stderr, err := l.kubectlDocument(mustJSON(blocker), "apply"); err != nil {
		l.fatalf("apply ValidatingWebhookConfiguration %s: %v: %s", lifecycleFailureBlockerWebhook, err, strings.TrimSpace(string(stderr)))
	}
}

// deleteLateFailureBlocker removes the blocker and waits for it to be gone.
func (l *lifecycleRun) deleteLateFailureBlocker() {
	l.t.Helper()
	if l.failures.blockerWebhook == "" {
		return
	}
	l.mustKubectl("delete", "validatingwebhookconfiguration", l.failures.blockerWebhook, "--wait=true")
	l.failures.blockerWebhook = ""
}

// lateFailureCandidateNow reads the candidate's chart and values checksums
// and its image as they are now.
func (l *lifecycleRun) lateFailureCandidateNow() (lifecycleFailureCandidate, error) {
	return lifecycleFailureReadCandidate(l.in.nextChartPackage, l.in.nextValuesFile, l.in.nextControllerImage)
}

// assertLateFailureCandidateUnchanged: the retry is the candidate that failed
// late, byte for byte, and names the same image.
func (l *lifecycleRun) assertLateFailureCandidateUnchanged() {
	l.t.Helper()
	now, err := l.lateFailureCandidateNow()
	if err != nil {
		l.fatalf("%v", err)
	}
	if !lifecycleFailureCandidateUnchanged(l.failures.candidate, now) {
		l.fatalf("late-failure recovery changed the candidate chart, values, or image")
	}
}

// proveLateFailureRecovery proves the boundary a late upgrade failure leaves
// behind: the reconcile hook of the failed revision ran and succeeded, the
// blocker refused Helm's apply of the candidate, and both runtime Deployments
// are stopped on the predecessor's template with no runtime Pod left. It
// records the failed revision in l.lateRevision.
func (l *lifecycleRun) proveLateFailureRecovery(currentImage string) {
	l.t.Helper()
	l.logf("proving the boundary a late upgrade failure leaves behind")
	candidate, err := l.lateFailureCandidateNow()
	if err != nil {
		l.fatalf("%v", err)
	}
	l.failures.candidate = candidate
	if l.expectedReconcileHookName == "" {
		l.fatalf("rendered reconcile hook name is unavailable")
	}
	l.runtimeDeploymentNames()
	// The account the predecessor's controller runs as, which no Pod may run
	// as once the late failure stopped the runtime: the one the identity
	// captured before the running Apply named.
	controllerAccount := l.currentReleaseServiceAccount
	if controllerAccount == "" {
		l.fatalf("the current release's controller ServiceAccount was not captured before the late failure")
	}
	l.lateRevision = l.helmRevision() + 1
	privileges := l.snapshotRuntimePrivileges()
	l.createLateFailureBlocker()
	// Helm 4 applies server-side, and a conflict is raised for a field whose
	// value this apply changes while another manager owns it. The hook stops
	// the runtime by scaling both Deployments to zero, so it owns
	// .spec.replicas with the value this apply has to raise again. The force
	// is confined to what the stop moved.
	_, stderr, err := l.helm("upgrade", l.in.helmRelease, l.in.nextChartPackage, "--namespace", l.in.operatorNamespace,
		"--values", l.in.nextValuesFile, "--force-conflicts", "--wait", "--timeout", "7m")
	if err == nil {
		l.fatalf("upgrade with a late-failure blocker unexpectedly succeeded")
	}
	if !bytes.Contains(stderr, []byte(lifecycleFailureBlockerName)) {
		l.printDebug(stderr)
		l.fatalf("the late upgrade failed without the blocker's refusal of the candidate Deployments")
	}
	status, _, err := l.helm("status", l.in.helmRelease, "--namespace", l.in.operatorNamespace,
		"--revision", strconv.Itoa(l.lateRevision), "-o", "json")
	if err != nil {
		l.fatalf("the late failure did not retain structured Helm evidence for revision %d", l.lateRevision)
	}
	if err := lifecycleLateFailureEvidence(status, l.lateRevision, l.expectedReconcileHookName); err != nil {
		l.logf("the late revision's status: %s", lifecycleFailureStatusSummary(status))
		l.fatalf("the late failure did not come after a reconcile hook that succeeded: %v", err)
	}
	for _, name := range []string{l.controllerDeployment, l.rotatorDeployment} {
		deployment := &appsv1.Deployment{}
		if err := l.cluster.Client.Get(l.ctx, types.NamespacedName{Namespace: l.in.operatorNamespace, Name: name}, deployment); err != nil ||
			!lifecycleFailureDeploymentOn(deployment, currentImage, true) {
			l.fatalf("the late failure did not leave %s stopped on the predecessor's template", name)
		}
	}
	pods := &corev1.PodList{}
	if err := l.cluster.Client.List(l.ctx, pods, client.InNamespace(l.in.operatorNamespace)); err != nil ||
		!lifecycleFailureRuntimePodsGone(pods.Items, controllerAccount, l.rotatorDeployment) {
		l.fatalf("the late failure left a runtime Pod after the runtime stop")
	}
	l.assertRuntimePrivilegesUnchanged(privileges, "late upgrade failure")
	l.logf("the late failure left the runtime stopped on the predecessor template")
}

// retrySameCandidate: the retry is the identical candidate. Its hook finds the
// Deployments still on the predecessor's template, stops them again, which
// changes nothing, and Helm applies the candidate over them.
func (l *lifecycleRun) retrySameCandidate() {
	l.t.Helper()
	privileges := l.snapshotRuntimePrivileges()
	if _, stderr, err := l.helm("upgrade", l.in.helmRelease, l.in.nextChartPackage, "--namespace", l.in.operatorNamespace,
		"--values", l.in.nextValuesFile, "--force-conflicts", "--wait", "--timeout", "7m"); err != nil {
		l.printDebug(stderr)
		l.fatalf("the same-candidate retry did not complete the upgrade")
	}
	l.assertRuntimePrivilegesUnchanged(privileges, "same-candidate upgrade retry")
}

// proofSchemaStatus is the proof schema's status as the API server stores it,
// in the canonical form jq -S wrote, for a byte comparison.
func (l *lifecycleRun) proofSchemaStatus() []byte {
	l.t.Helper()
	encoded, err := json.Marshal(l.proofObject("ptahschema", lifecycleProofSchema).Object["status"])
	l.check(err, "encode the status of %s", lifecycleProofSchema)
	return encoded
}

// proveRollbackRefusedOverFutureState: a rollback runs the CRD hook of the
// release it rolls back to. That release has to read what is stored, and when
// it cannot, its hook refuses before Helm replaces the running Pods with ones
// whose verifier would refuse to start.
func (l *lifecycleRun) proveRollbackRefusedOverFutureState(revision int) {
	l.t.Helper()
	l.logf("proving a rollback the stored state has outgrown is refused before any Pod changes")
	history := l.mustHelm("", "history", l.in.helmRelease, "--namespace", l.in.operatorNamespace, "--max", "1", "-o", "json")
	historyBefore, err := lifecycleFailureHistoryRevision(history)
	if err != nil {
		l.fatalf("read the newest revision of release %s: %v", l.in.helmRelease, err)
	}
	stored, found, err := unstructuredInt64(l.proofObject("ptahschema", lifecycleProofSchema).Object,
		"status", "executionBinding", "controllerStateVersion")
	if err != nil || !found || stored != l.controllerStateVersion {
		shown := ""
		if found && err == nil {
			shown = strconv.FormatInt(stored, 10)
		}
		l.fatalf("proof PtahSchema controller state version is %s, expected %d", shown, l.controllerStateVersion)
	}
	l.patchExecutionBindingStateVersion(l.newerControllerStateVersion)
	futureState := l.proofSchemaStatus()
	before := l.deploymentEvidence()
	privileges := l.snapshotRuntimePrivileges()
	_, stderr, err := l.helm("rollback", l.in.helmRelease, strconv.Itoa(revision), "--namespace", l.in.operatorNamespace,
		"--force-conflicts", "--wait", "--timeout", "3m")
	if err == nil {
		l.fatalf("a rollback over stored state newer than the release it rolls back to was admitted")
	}
	// Helm writes the rollback revision before it runs the pre-rollback hook,
	// so a revision past the last one is what separates a refusal in the hook
	// from one before it.
	refused := l.mustHelm("", "history", l.in.helmRelease, "--namespace", l.in.operatorNamespace, "--max", "1", "-o", "json")
	if err := lifecycleFailureRefusedRollbackHistory(refused, historyBefore); err != nil {
		l.logf("the history after the refused rollback: %s", strings.TrimSpace(string(refused)))
		l.printDebug(stderr)
		l.fatalf("the refused rollback did not reach its pre-rollback hook: %v", err)
	}
	if after := l.deploymentEvidence(); !bytes.Equal(before, after) {
		l.fatalf("the refused rollback changed a runtime Deployment")
	}
	l.assertRuntimePrivilegesUnchanged(privileges, "refused downgrade")
	if after := l.proofSchemaStatus(); !bytes.Equal(futureState, after) {
		l.fatalf("the refused rollback rewrote the future PtahSchema state")
	}
	l.patchExecutionBindingStateVersion(l.controllerStateVersion)
	l.waitRuntimeReady()
	l.logf("the rollback was refused before any Pod changed")
}

// proveRollback: a rollback to a release that reads what is stored goes
// through. Its hook stops the runtime the image no longer matches, and Helm
// brings up the release rolled back to. A refused rollback leaves Helm's
// rollback revision pending, and this is also the way out of it.
func (l *lifecycleRun) proveRollback(revision int, image string) {
	l.t.Helper()
	l.logf("rolling back to revision %d", revision)
	if _, stderr, err := l.helm("rollback", l.in.helmRelease, strconv.Itoa(revision), "--namespace", l.in.operatorNamespace,
		"--force-conflicts", "--wait", "--timeout", "5m"); err != nil {
		l.printDebug(stderr)
		l.fatalf("the rollback to revision %d was refused", revision)
	}
	l.waitRuntimeReady()
	l.runtimeDeploymentNames()
	for _, name := range []string{l.controllerDeployment, l.rotatorDeployment} {
		deployment := &appsv1.Deployment{}
		if err := l.cluster.Client.Get(l.ctx, types.NamespacedName{Namespace: l.in.operatorNamespace, Name: name}, deployment); err != nil ||
			!lifecycleFailureDeploymentOn(deployment, image, false) {
			l.fatalf("the rollback did not bring %s back on %s", name, image)
		}
	}
	if _, status := l.helmStatus(); status != "deployed" {
		l.fatalf("the rollback to revision %d did not end deployed", revision)
	}
}

// cleanupFailures is this section's part of the script's EXIT trap: unless a
// failed phase keeps its objects, the blocker and the foreign CronJob go
// whatever happened. The deletions run on a context of their own, because the
// phase's may already have ended.
func (l *lifecycleRun) cleanupFailures(retain bool) error {
	if retain {
		if l.failures.blockerWebhook != "" {
			l.t.Logf("e2e crd: retaining validating webhook configuration %s", l.failures.blockerWebhook)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var failures []error
	if l.failures.blockerWebhook != "" {
		blocker := &admissionregistrationv1.ValidatingWebhookConfiguration{}
		blocker.Name = l.failures.blockerWebhook
		if err := l.deleteIgnoringAbsence(ctx, blocker); err != nil {
			failures = append(failures, fmt.Errorf("remove the late-failure blocker %s: %w", blocker.Name, err))
		}
	}
	if l.failures.sharedNamespaceProbe != "" {
		probe := &batchv1.CronJob{}
		probe.Namespace, probe.Name = l.in.operatorNamespace, l.failures.sharedNamespaceProbe
		if err := l.deleteIgnoringAbsence(ctx, probe); err != nil {
			failures = append(failures, fmt.Errorf("remove the shared-namespace probe %s: %w", probe.Name, err))
		}
	}
	return errors.Join(failures...)
}
