//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
)

// The two lifecycle phases in the script's order: run_upgrade_proof in six
// scenarios, then run_next_release_upgrade_proof and run_uninstall_proof in
// five.

const (
	crdSchemaVersionAnnotation       = "operator.ptah.run/crd-schema-version"
	crdSchemaDigestAnnotation        = "operator.ptah.run/crd-schema-digest"
	controllerStateVersionAnnotation = "operator.ptah.run/controller-state-version"
	lifecycleDigestCRD               = "ptahschemaplans.operator.ptah.run"
	currentReadOnlyJobSchema         = "read-only-job-current"
	successorReadOnlyJobSchema       = "read-only-job-successor"
)

// readOnlyJobCleanupWithinTheRelease opens the upgrade phase: the manager the
// driver installed is the candidate, and a read-only Job it dispatched is
// retired by the same release once the runtime is back.
func (l *lifecycleRun) readOnlyJobCleanupWithinTheRelease() {
	l.t.Helper()
	if info, err := os.Stat(l.in.candidateValuesFile); err != nil || !info.Mode().IsRegular() {
		l.fatalf("candidate values file is missing")
	}
	l.prepareExpectedHookNames(l.in.chartPackage, l.in.candidateValuesFile)
	l.runtimeDeploymentNames()
	deployment := &appsv1.Deployment{}
	l.check(l.cluster.Client.Get(l.ctx, types.NamespacedName{Namespace: l.in.operatorNamespace, Name: l.controllerDeployment}, deployment),
		"read the controller Deployment %s", l.controllerDeployment)
	installed := ""
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name == "manager" {
			installed = container.Image
		}
	}
	if installed != l.in.controllerImage {
		l.fatalf("installed current-release manager image is %s, expected %s", installed, l.in.controllerImage)
	}

	l.logf("proving read-only Job cleanup within the current release")
	l.mustKubectl("create", "namespace", l.in.proofNamespace)
	l.readOnlyJobSchema = currentReadOnlyJobSchema
	l.dispatchReadOnlyJobFixture()
	l.stopRuntimeDeployments()
	l.setPodWebhookFailurePolicy("Fail", "Ignore")
	l.stageReadOnlyJobCompletion()
	l.setPodWebhookFailurePolicy("Ignore", "Fail")
	l.startRuntimeDeployments()
	l.waitRuntimeReady()
	l.waitForReadOnlyJobCleanup()
	l.quiesceReadOnlyJobSchema()
}

// crdPreflightRefusals: the CRD hook refuses a missing CRD, a newer schema
// version, a newer durable controller-state marker, an incomplete schema
// identity and a digest collision, each without changing a CRD or a runtime
// Deployment.
func (l *lifecycleRun) crdPreflightRefusals() {
	l.t.Helper()
	l.upgradeValuesFile = l.releaseValues("release-values.yaml", "yaml")
	l.proofControllerImage = l.productionControllerImageFromValues(l.releaseValues("release-values.json", "json"))

	l.logf("proving a missing CRD aborts Helm upgrade without recreation")
	l.mustKubectl("delete", "crd", "ptahschemaapprovals.operator.ptah.run")
	l.expectUpgradeFailureWithoutDeploymentChange("upgrade with a missing CRD")
	if _, err := l.crd("ptahschemaapprovals.operator.ptah.run"); err == nil {
		l.fatalf("CRD hook recreated a missing CRD")
	}
	l.mustKubectl("create", "-f", filepath.Join(repositoryRoot, "config", "crd", "bases", "operator.ptah.run_ptahschemaapprovals.yaml"))
	l.mustKubectl("wait", "--for=condition=Established", "crd/ptahschemaapprovals.operator.ptah.run", "--timeout=60s")

	l.logf("proving a newer CRD schema version blocks rollback")
	future := strconv.FormatInt(l.candidateCRDSchemaVersion+1, 10)
	l.annotateCRD(lifecycleDigestCRD, crdSchemaVersionAnnotation, future)
	l.crdsEvidence("-before-schema-rollback")
	l.expectUpgradeFailureWithoutDeploymentChange("upgrade with a newer CRD schema version")
	l.assertCRDsUnchanged("-before-schema-rollback")
	l.annotateCRD(lifecycleDigestCRD, crdSchemaVersionAnnotation, strconv.FormatInt(l.candidateCRDSchemaVersion, 10))

	l.logf("proving a newer durable controller-state marker blocks rollback")
	l.annotateCRD(lifecycleDigestCRD, controllerStateVersionAnnotation, strconv.FormatInt(l.newerControllerStateVersion, 10))
	l.crdsEvidence("-before-state-rollback")
	l.expectUpgradeFailureWithoutDeploymentChange("upgrade with a newer durable controller-state marker")
	l.assertCRDsUnchanged("-before-state-rollback")
	l.annotateCRD(lifecycleDigestCRD, controllerStateVersionAnnotation, strconv.FormatInt(l.controllerStateVersion, 10))

	l.logf("proving an incomplete schema identity and a digest collision are refused")
	candidateDigest := l.crdAnnotation(lifecycleDigestCRD, crdSchemaDigestAnnotation)
	if !sha256Pattern.MatchString(candidateDigest) {
		l.fatalf("%s does not carry a valid candidate schema digest", lifecycleDigestCRD)
	}
	// The live spec still matches the candidate exactly; only the digest is
	// missing. The operator upgrades only from a release that carries the
	// whole identity tuple, so an exact schema without it is refused too.
	l.annotateCRD(lifecycleDigestCRD, crdSchemaDigestAnnotation, "")
	l.crdsEvidence("-before-missing-digest")
	// The refusal happens inside the reconcile hook's container, so Helm
	// reports only that the hook Job failed. What this proof pins is the
	// refusal itself, and the shared helper pins that the reconcile hook of the
	// expected revision is what refused while every CRD and Deployment stayed
	// unchanged.
	l.expectUpgradeFailureWithoutDeploymentChange("upgrade with a missing schema digest")
	l.assertCRDsUnchanged("-before-missing-digest")
	l.annotateCRD(lifecycleDigestCRD, crdSchemaDigestAnnotation, candidateDigest)
	l.upgradeTheRelease(l.in.chartPackage, l.upgradeValuesFile)

	collision := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if collision == candidateDigest {
		collision = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	}
	l.annotateCRD(lifecycleDigestCRD, crdSchemaDigestAnnotation, collision)
	l.crdsEvidence("-before-digest-collision")
	l.expectUpgradeFailureWithoutDeploymentChange("upgrade with a same-version schema digest collision")
	l.assertCRDsUnchanged("-before-digest-collision")
	l.annotateCRD(lifecycleDigestCRD, crdSchemaDigestAnnotation, candidateDigest)
}

// driftedCRDUpgrade: live objects survive every CRD change, future controller
// state refuses the upgrade without a rewrite, and drifted CRDs converge
// before the manager rolls out.
func (l *lifecycleRun) driftedCRDUpgrade() {
	l.t.Helper()
	l.logf("creating live-object preservation evidence")
	l.createProofObjects()
	l.proofEvidence("-before")

	for _, name := range lifecycleManagedCRDs {
		l.driftCRD(name, "outdated e2e schema")
		l.crdEvidence(name, name+"-before-future-state")
	}
	l.patchExecutionBindingStateVersion(l.newerControllerStateVersion)
	l.expectUpgradeFailureWithoutDeploymentChange("upgrade against future controller state")
	l.assertCRDsUnchanged("-before-future-state")
	stored := l.proofObject("ptahschema", lifecycleProofSchema)
	storedVersion, found, err := unstructuredInt64(stored.Object, "status", "executionBinding", "controllerStateVersion")
	if err != nil || !found || storedVersion != l.newerControllerStateVersion {
		l.fatalf("failed CRD preflight rewrote future controller state")
	}
	l.patchExecutionBindingStateVersion(l.controllerStateVersion)

	l.logf("upgrading drifted CRDs before the manager rollout")
	l.upgradeTheRelease(l.in.chartPackage, l.upgradeValuesFile)
	for _, name := range lifecycleManagedCRDs {
		if l.crdDescription(name) == "outdated e2e schema" {
			l.fatalf("%s retained the outdated schema", name)
		}
	}
	l.assertProofUnchanged("-before")
}

// singletonCoordination: a second release in the namespace is refused by the
// ownership of what the first installed, the coordination and leader-election
// settings are immutable, the release namespace is the operator's alone, and
// the runtime and downgrade guards hold.
func (l *lifecycleRun) singletonCoordination() {
	l.t.Helper()
	l.logf("proving immutable singleton coordination")
	second := l.in.helmRelease + "-second"
	if _, stderr, err := l.helm("install", second, l.in.chartPackage, "--namespace", l.in.operatorNamespace,
		"--values", l.upgradeValuesFile, "--wait", "--timeout", "2m"); err == nil {
		l.fatalf("a second operator release was installed")
	} else if !bytes.Contains(stderr, []byte("is not owned by Helm release")) || !bytes.Contains(stderr, []byte(l.in.helmRelease)) {
		// The chart refuses a second release in a namespace another release
		// owns, and the controller Deployment's provenance is the first thing
		// it reads, so that refusal is what a second install sees. It names the
		// release that owns the object, which is what makes the refusal a
		// coordination proof rather than any rendering error.
		l.fatalf("second release failed without the ownership refusal naming the installed release")
	}
	if _, _, err := l.helm("status", second, "-n", l.in.operatorNamespace); err == nil {
		l.fatalf("failed second release was recorded")
	}

	l.expectUpgradeRenderFailureWithoutDeploymentChange("coordination namespace mutation",
		"--set-string", "coordination.namespace=forbidden-coordination")
	if !bytes.Contains(l.failedUpgradeStderr, []byte("operator.ptah.run/coordination-namespace")) {
		l.fatalf("coordination mutation failed without the immutable annotation guard")
	}
	l.expectUpgradeRenderFailureWithoutDeploymentChange("leader-election mutation",
		"--set", "replicaCount=1", "--set", "leaderElection=false")
	if !bytes.Contains(l.failedUpgradeStderr, []byte("operator.ptah.run/leader-election")) {
		l.fatalf("leader-election mutation failed without the immutable annotation guard")
	}
	l.proveSharedReleaseNamespaceRefusal()

	l.proveRuntimeSingletonGuard()
	l.proveControllerDowngradeGuard()
	l.upgradeTheRelease(l.in.chartPackage, l.upgradeValuesFile)
	l.logf("upgrade and singleton proofs passed")
}

// upgradeTheRelease is a Helm upgrade that has to succeed.
func (l *lifecycleRun) upgradeTheRelease(chart, values string) {
	l.t.Helper()
	l.mustHelm("", "upgrade", l.in.helmRelease, chart, "--namespace", l.in.operatorNamespace,
		"--values", values, "--wait", "--timeout", "5m")
}

// nextReleaseUpgrade is the first half of run_next_release_upgrade_proof:
// managed lifecycle coverage advances an already active release. The current
// release is the predecessor, and the synthetic next release is the candidate
// that replaces it, recovering from a late failure while an Apply the
// predecessor started keeps running.
func (l *lifecycleRun) nextReleaseUpgrade() {
	l.t.Helper()
	l.requireRegularFile(l.in.chartPackage, "E2E_CHART_PACKAGE must name the regular non-symlink current-release chart package")
	l.requireRegularFile(l.in.candidateValuesFile, "E2E_CANDIDATE_VALUES_FILE must name the regular non-symlink current-release values file")
	if !lifecycleImageIdentityOK(l.in.controllerImage) {
		l.fatalf("E2E_CONTROLLER_IMAGE must be an exact repository-and-digest identity")
	}
	if l.productionControllerImageFromValues(l.in.candidateValuesFile) != l.in.controllerImage {
		l.fatalf("current-release values do not bind the supplied candidate controller image")
	}
	l.proofEvidence("-before")

	l.requireRegularFile(l.in.nextChartPackage, "E2E_NEXT_CHART_PACKAGE must name a regular synthetic next-release chart package")
	l.requireRegularFile(l.in.nextValuesFile, "E2E_NEXT_VALUES_FILE must name a regular synthetic next-release values file")
	if !lifecycleImageIdentityOK(l.in.nextControllerImage) {
		l.fatalf("E2E_NEXT_CONTROLLER_IMAGE must be an exact repository-and-digest identity")
	}
	if l.productionControllerImageFromValues(l.in.nextValuesFile) != l.in.nextControllerImage {
		l.fatalf("synthetic next-release values do not bind the supplied controller image")
	}
	l.currentReleaseControllerImage = l.productionControllerImageFromValues(l.releaseValues("current-release-values.json", "json"))
	if l.currentReleaseControllerImage == l.in.nextControllerImage {
		l.fatalf("current and synthetic next-release controller images must be distinct")
	}

	current := l.captureControllerServiceAccountIdentity(l.currentReleaseControllerImage)
	l.currentReleaseServiceAccount = current.ServiceAccountName
	l.currentReleaseRevision = l.deployedRevision()

	l.prepareExpectedHookNames(l.in.nextChartPackage, l.in.nextValuesFile)
	// The late failure leaves the runtime stopped; stage the handoff while no
	// runtime can consume or clean up the Job. Do not delete or resurrect
	// Deployments across that recovery boundary.
	l.logf("dispatching a read-only Job the successor must retire")
	l.readOnlyJobSchema = successorReadOnlyJobSchema
	// A running Apply is the work an upgrade may not interrupt. The barrier
	// holds the statement inside the engine, so the Apply is genuinely running
	// across the upgrade rather than finished before it
	// (stokaro/ptah-operator#7).
	l.logf("holding one Apply open across the next-release upgrade")
	l.dispatchReadOnlyJobFixture()
	l.startRunningApplyBarrier()
	l.prepareRunningApplyFixture()
	l.startRunningApplyFixture()
	l.stagePredecessorApplyJobUIDGapWhileRunning()
	l.proveLateFailureRecovery(l.currentReleaseControllerImage)
	l.setPodWebhookFailurePolicy("Fail", "Ignore")
	l.stageReadOnlyJobCompletion()
	l.setPodWebhookFailurePolicy("Ignore", "Fail")
	l.stageReadOnlyJobUIDGap()
	l.assertLateFailureCandidateUnchanged()
	l.deleteLateFailureBlocker()
	if l.helmRevision() != l.lateRevision {
		l.fatalf("late-failure recovery did not resume the exact failed Helm revision")
	}
	l.logf("retrying the upgrade to the same synthetic next release")
	l.retrySameCandidate()
	l.waitRuntimeReady()
	l.waitForReadOnlyJobCleanup()
	l.quiesceReadOnlyJobSchema()
	l.assertPredecessorApplyRemainsExclusiveWhileRunning()
	l.releaseRunningApplyBarrier()
	l.waitForPredecessorApplyJobTerminal()
	l.waitForPredecessorApplyJobCleanup()
	if l.deployedRevision() != l.lateRevision+1 {
		l.fatalf("same-candidate recovery did not create exactly one retry Helm revision")
	}

	next := l.captureControllerServiceAccountIdentity(l.in.nextControllerImage)
	if next.DeploymentName != current.DeploymentName {
		l.fatalf("synthetic next-release upgrade changed the controller Deployment identity")
	}
	// Every release runs the controller as the same ServiceAccount, and Helm
	// keeps that object across the upgrade: the name and the UID both hold.
	if next.ServiceAccountName != current.ServiceAccountName {
		l.fatalf("synthetic next-release upgrade moved the controller from ServiceAccount %s to %s",
			current.ServiceAccountName, next.ServiceAccountName)
	}
	if next.ServiceAccountUID != current.ServiceAccountUID {
		l.fatalf("synthetic next-release upgrade replaced controller ServiceAccount %s (UID %s became %s)",
			current.ServiceAccountName, current.ServiceAccountUID, next.ServiceAccountUID)
	}
	// The synthetic next release differs from this one in its manager image
	// alone: the executor, the Ptah version, the runner protocol and the
	// controller-state version are the same. None of that is in the execution
	// binding, so the schema keeps its epoch and the plan and approval under
	// it stay exactly as they were -- a patch release retires nothing.
	l.assertProofUnchanged("-before")
	l.logf("same-candidate late-failure recovery passed")
}

// rollbackToTheCurrentRelease is the second half of
// run_next_release_upgrade_proof. A rollback to the release this one replaced
// runs that release's CRD hook first. Refused, it leaves the running release
// alone and the rollback revision pending; the rollback that follows is both
// the way out of that and the proof that a rollback the stored state allows
// goes through.
func (l *lifecycleRun) rollbackToTheCurrentRelease() {
	l.t.Helper()
	l.proveRollbackRefusedOverFutureState(l.currentReleaseRevision)
	l.proveRollback(l.currentReleaseRevision, l.currentReleaseControllerImage)
	l.assertProofUnchanged("-before")
	// Everything after this, including the uninstall, is held to the state
	// the rollback leaves behind.
	l.proofEvidence("-before")
	l.logf("synthetic next-release upgrade kept the controller identity, and the rollback to the current release went through its hook")
}

// uninstallTheRolledBackRelease: the uninstall removes the runtime and keeps
// the CRDs and every live object.
func (l *lifecycleRun) uninstallTheRolledBackRelease() {
	l.t.Helper()
	l.uninstallAndAssertRetained("the uninstall of the rolled-back release failed; Helm's own error is above")
}

// reinstallOverRetainedCRDs: an install over retained CRDs another manager
// drifted converges them, and its uninstall again leaves the CRDs and live
// objects.
func (l *lifecycleRun) reinstallOverRetainedCRDs() {
	l.t.Helper()
	l.logf("reinstalling over retained and drifted CRDs")
	l.driftCRD("ptahschemas.operator.ptah.run", "retained reinstall drift")
	// The drift above is written by kubectl, which owns the field it added.
	// Helm 4 applies the chart's CRDs server-side on install and refuses to
	// change a field another manager owns, so an install over a retained CRD
	// somebody edited needs the force. It is confined to the CRDs this chart
	// ships.
	//
	// That apply also happens before any hook runs, so what this step proves
	// is that the install converges a retained CRD, not that the hook does.
	// The hook's own CRD convergence is proved on the upgrade above, where
	// Helm leaves the CRDs alone and the same drift needs no force.
	l.mustHelm("", "install", l.in.helmRelease, l.in.nextChartPackage, "--namespace", l.in.operatorNamespace,
		"--values", l.in.nextValuesFile, "--force-conflicts", "--wait", "--timeout", "5m")
	if l.crdDescription("ptahschemas.operator.ptah.run") == "retained reinstall drift" {
		l.fatalf("the reinstall did not reconcile a retained CRD another manager drifted")
	}
	l.assertProofUnchanged("-before")
	l.captureControllerServiceAccountIdentity(l.in.nextControllerImage)
	l.uninstallAndAssertRetained("the uninstall of the release reinstalled over retained CRDs failed; Helm's own error is above")
}

// installTheExportedChart: the exact exported current-release chart bytes
// recover from quota refusal under restricted Pod Security over retained
// drifted CRDs, then uninstall with no residue.
func (l *lifecycleRun) installTheExportedChart() {
	l.t.Helper()
	l.logf("fresh-installing the exact exported current-release chart bytes")
	l.driftCRD("ptahschemas.operator.ptah.run", "exact released-chart install drift")
	// The drift above is written by kubectl, which owns the field it added,
	// and Helm 4 refuses to change a field another manager owns. This install
	// carries the force for the same reason the one before it does, and proves
	// the same thing: that the install converges a retained CRD.
	l.installWithQuotaAndPodSecurity()
	l.waitRuntimeReady()
	if l.crdDescription("ptahschemas.operator.ptah.run") == "exact released-chart install drift" {
		l.fatalf("the exact released-chart install did not reconcile a retained CRD another manager drifted")
	}
	l.captureControllerServiceAccountIdentity(l.in.controllerImage)
	// This install puts a different manager in place and changes nothing the
	// execution binding holds, so the schema, its plan and its approval stay
	// exactly as they were, as they do across the next-release upgrade.
	// Everything after this, including this release's own uninstall, is held
	// to the state it leaves behind.
	l.proofEvidence("-before")
	l.uninstallAndAssertRetained("the uninstall of the exported current-release chart failed; Helm's own error is above")
	l.logf("exact exported current-release chart passed fresh install and zero-residue uninstall")
	l.logf("uninstall retained CRDs and live objects")
}

// uninstallAndAssertRetained uninstalls the release and proves the runtime is
// gone while the CRDs and the proof objects stay exactly as they were.
func (l *lifecycleRun) uninstallAndAssertRetained(failure string) {
	l.t.Helper()
	l.captureCertificateSecretNames()
	privileges := l.captureReleasePrivileges()
	l.mustHelm(failure, "uninstall", l.in.helmRelease, "-n", l.in.operatorNamespace, "--wait", "--timeout", "5m")
	l.assertReleaseRuntimeRemoved()
	l.assertReleasePrivilegesRemoved(privileges)
	l.assertCRDsRetained()
	l.assertProofUnchanged("-before")
}

// requireRegularFile refuses a path that is not a regular non-symlink file.
func (l *lifecycleRun) requireRegularFile(path, failure string) {
	l.t.Helper()
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		l.fatalf("%s", failure)
	}
}
