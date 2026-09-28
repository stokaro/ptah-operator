//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The section of hack/e2e-crd-upgrade.sh from dispatch_read_only_job_fixture
// to wait_runtime_ready: the read-only Job a release dispatches and the next
// one retires, the runtime Deployments the proofs stop and restore, the
// identities read off them, and the init guard that holds a refused runtime
// before any main container starts.

// lifecycleRuntime is the section's own state.
type lifecycleRuntime struct {
	// readOnlyJobName and readOnlyJobUID are READ_ONLY_JOB_NAME and
	// READ_ONLY_JOB_UID: the Job the current fixture schema dispatched.
	readOnlyJobName string
	readOnlyJobUID  string
	// beforeCleanup is the read-only Job's cleanup evidence by schema,
	// recorded once the Job controller retired it.
	beforeCleanup map[string][]byte
	// controllerManager and rotatorManager are the server-side apply field
	// managers the snapshots are restored as.
	controllerManager string
	rotatorManager    string
}

// readOnlyJobDocumentKey is where the dispatched Job is kept: the file name
// the script wrote it to, $WORK_DIR/<schema>-read-only-job.json. The file is
// written there too.
func readOnlyJobDocumentKey(schema string) string { return schema + "-read-only-job.json" }

// readOnlyJobDocument is the Job the fixture schema dispatched, exactly as
// `kubectl get job -o json` returned it: the base every controller-object
// probe mutates and the running Apply fixture reads its manager-written shape
// from.
func (l *lifecycleRun) readOnlyJobDocument(schema string) []byte {
	l.t.Helper()
	document, ok := l.evidence[readOnlyJobDocumentKey(schema)]
	if !ok || len(document) == 0 {
		l.fatalf("no read-only Job was dispatched for %s", schema)
	}
	return document
}

// runtimeFixtureSchema reads a PtahSchema in the proof namespace as stored.
func (l *lifecycleRun) runtimeFixtureSchema(name string) (*unstructured.Unstructured, error) {
	schema := &unstructured.Unstructured{}
	schema.SetGroupVersionKind(lifecycleProofGroupVersion.WithKind("PtahSchema"))
	err := l.cluster.Client.Get(l.ctx, types.NamespacedName{Namespace: l.in.proofNamespace, Name: name}, schema)
	return schema, err
}

// runtimeFixtureJob reads a Job in the proof namespace as stored.
func (l *lifecycleRun) runtimeFixtureJob(name string) (*unstructured.Unstructured, error) {
	job := &unstructured.Unstructured{}
	job.SetAPIVersion("batch/v1")
	job.SetKind("Job")
	err := l.cluster.Client.Get(l.ctx, types.NamespacedName{Namespace: l.in.proofNamespace, Name: name}, job)
	return job, err
}

// dispatchReadOnlyJobFixture: the controller dispatches a read-only Job for
// an unsuspended schema whose execution nodeSelector nothing satisfies, so
// the Job stays pending with a committed UID and never reads the database URL
// or the desired reference. Its manifest is the base every controller-object
// probe mutates, and its staged terminal state is what the cleanup proofs
// hand across a release.
func (l *lifecycleRun) dispatchReadOnlyJobFixture() {
	l.t.Helper()
	schema := l.readOnlyJobSchema
	if schema == "" {
		l.fatalf("read-only Job fixture schema name is unset")
	}
	document := mustJSON(map[string]any{
		"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahSchema",
		"metadata": map[string]any{"name": schema},
		"spec": map[string]any{
			"target": map[string]any{
				"engine": "PostgreSQL", "coordinationKey": schema,
				"urlFrom": map[string]any{"name": "unused-database-url", "key": "url"},
			},
			"desired": map[string]any{
				"ociRef":                 "oci://example.invalid/schema:v1",
				"verificationPolicyFrom": map[string]any{"name": "unused-verification-policy", "key": "policy.yaml"},
			},
			"execution": map[string]any{
				"serviceAccountName": "default",
				"nodeSelector":       map[string]any{lifecycleRuntimeBlockedNodeSelectorKey: "blocked"},
			},
		},
	})
	if _, stderr, err := l.kubectlDocument(document, "-n", l.in.proofNamespace, "apply"); err != nil {
		l.fatalf("apply PtahSchema %s: %v: %s", schema, err, strings.TrimSpace(string(stderr)))
	}
	for deadline := time.Now().Add(120 * time.Second); time.Now().Before(deadline); {
		// The script read the name and the UID with two requests; one reading
		// holds both, so they cannot come from two versions of the claim.
		stored, err := l.runtimeFixtureSchema(schema)
		name, uid := "", ""
		if err == nil {
			name = lifecycleRuntimeString(stored.Object, "status", "activeOperation", "jobName")
			uid = lifecycleRuntimeString(stored.Object, "status", "activeOperation", "jobUID")
		}
		if name != "" && uid != "" {
			// The document is read through kubectl, as the script wrote it:
			// the probes that mutate it expect kubectl's shape, with no
			// managed fields.
			if job, _, err := l.kubectl("-n", l.in.proofNamespace, "get", "job", name, "-o", "json"); err == nil {
				var parsed map[string]any
				if json.Unmarshal(job, &parsed) != nil || !lifecycleRuntimeReadOnlyJobDispatched(parsed, schema, uid) {
					l.fatalf("read-only Job does not match the dispatched operation identity")
				}
				l.runtime.readOnlyJobName, l.runtime.readOnlyJobUID = name, uid
				key := readOnlyJobDocumentKey(schema)
				l.evidence[key] = job
				l.check(os.WriteFile(filepath.Join(l.workDir, key), job, 0o600), "write %s", key)
				return
			}
		}
		l.sleep(time.Second)
	}
	l.fatalf("controller did not dispatch a read-only Job with a committed UID for %s", schema)
}

// quiesceReadOnlyJobSchema suspends the fixture schema and holds it to a
// suspended schema with no operation.
func (l *lifecycleRun) quiesceReadOnlyJobSchema() {
	l.t.Helper()
	schema := l.readOnlyJobSchema
	l.mustKubectl("-n", l.in.proofNamespace, "patch", "ptahschema", schema, "--type=merge", "-p", `{"spec":{"suspend":true}}`)
	l.waitForSuspended(schema)
	stored, err := l.runtimeFixtureSchema(schema)
	if err != nil || !lifecycleRuntimeSchemaQuiesced(stored.Object) {
		l.fatalf("read-only Job schema %s did not quiesce exactly", schema)
	}
}

// runtimeAdmissionConfiguration reads the operator's validating webhook
// configuration as stored.
func (l *lifecycleRun) runtimeAdmissionConfiguration() map[string]any {
	l.t.Helper()
	configuration := &unstructured.Unstructured{}
	configuration.SetAPIVersion("admissionregistration.k8s.io/v1")
	configuration.SetKind("ValidatingWebhookConfiguration")
	l.check(l.cluster.Client.Get(l.ctx, types.NamespacedName{Name: lifecycleRuntimeAdmissionConfiguration}, configuration),
		"read validatingwebhookconfiguration %s", lifecycleRuntimeAdmissionConfiguration)
	return configuration.Object
}

// setPodWebhookFailurePolicy: the operator's Pod webhook fails closed, and
// the controller that serves it is stopped while a terminal Job is staged.
// The Job controller cannot retire the pending Pod through a webhook nobody
// answers, so the outage is bridged by an exact, verified failurePolicy
// transition and restored right after.
func (l *lifecycleRun) setPodWebhookFailurePolicy(expected, desired string) {
	l.t.Helper()
	if err := lifecycleRuntimeFailurePolicyTransition(expected, desired); err != nil {
		l.fatalf("%v", err)
	}
	configuration := l.runtimeAdmissionConfiguration()
	index, err := lifecycleRuntimePodWebhookIndex(configuration)
	if err != nil {
		l.fatalf("%v", err)
	}
	if _, policy := lifecycleRuntimeWebhookAt(configuration, index); policy != expected {
		l.fatalf("Pod webhook failurePolicy is %s, expected %s", policy, expected)
	}
	l.mustKubectl("patch", "validatingwebhookconfiguration", lifecycleRuntimeAdmissionConfiguration,
		"--type=json", "-p", string(lifecycleRuntimeFailurePolicyPatch(index, expected, desired)))
	name, policy := lifecycleRuntimeWebhookAt(l.runtimeAdmissionConfiguration(), index)
	if name != lifecycleRuntimePodWebhook || policy != desired {
		l.fatalf("Pod webhook failurePolicy transition was not persisted")
	}
}

// stageReadOnlyJobCompletion stages a FailureTarget on the pending read-only
// Job and waits for the Job controller to retire it, then records what the
// successor's cleanup may not change.
func (l *lifecycleRun) stageReadOnlyJobCompletion() {
	l.t.Helper()
	name, uid := l.runtime.readOnlyJobName, l.runtime.readOnlyJobUID
	if name == "" {
		l.fatalf("read-only Job name is missing")
	}
	if uid == "" {
		l.fatalf("read-only Job UID is missing")
	}
	job, err := l.runtimeFixtureJob(name)
	if err != nil || !lifecycleRuntimeReadOnlyJobOpen(job.Object, uid) {
		l.fatalf("read-only Job was already terminal before FailureTarget staging")
	}
	l.mustKubectl("-n", l.in.proofNamespace, "patch", "job", name, "--subresource=status",
		"--type=merge", "-p", string(lifecycleRuntimeFailureTargetPatch(time.Now())))

	var retired *unstructured.Unstructured
	for deadline := time.Now().Add(120 * time.Second); time.Now().Before(deadline); {
		if reading, err := l.runtimeFixtureJob(name); err == nil && lifecycleRuntimeReadOnlyJobRetired(reading.Object, uid) {
			retired = reading
			break
		}
		l.sleep(time.Second)
	}
	if retired == nil {
		l.runtimeReportReadOnlyJob(name)
		l.fatalf("Job controller did not retire the read-only Job after FailureTarget staging")
	}
	// The evidence is taken from the reading that matched rather than a
	// second read: the runtime is stopped here, and nothing else writes the
	// Job, but a reading that satisfied the wait cannot race.
	evidence, err := lifecycleRuntimeJobCleanupEvidence(retired.Object)
	l.check(err, "encode the cleanup evidence of Job %s", name)
	if l.runtime.beforeCleanup == nil {
		l.runtime.beforeCleanup = map[string][]byte{}
	}
	l.runtime.beforeCleanup[l.readOnlyJobSchema] = evidence
}

// runtimeReportReadOnlyJob prints the Job and its Pods when the Job controller did
// not retire it.
func (l *lifecycleRun) runtimeReportReadOnlyJob(name string) {
	l.t.Helper()
	if job, err := l.runtimeFixtureJob(name); err == nil {
		status, _ := json.Marshal(map[string]any{"name": job.GetName(), "uid": job.GetUID(), "status": job.Object["status"]})
		l.logf("%s", status)
	}
	pods := &corev1.PodList{}
	if l.cluster.Client.List(l.ctx, pods, client.InNamespace(l.in.proofNamespace),
		client.MatchingLabels{"batch.kubernetes.io/job-name": name}) == nil {
		summary := make([]map[string]any, 0, len(pods.Items))
		for _, pod := range pods.Items {
			summary = append(summary, map[string]any{
				"name": pod.Name, "uid": pod.UID, "phase": pod.Status.Phase, "deletionTimestamp": pod.DeletionTimestamp,
			})
		}
		encoded, _ := json.Marshal(summary)
		l.logf("%s", encoded)
	}
}

// stageReadOnlyJobUIDGap removes the committed Job UID from the fixture
// schema's claim, as the manager, so the successor meets a claim that names
// its Job but has not recorded which one.
func (l *lifecycleRun) stageReadOnlyJobUIDGap() {
	l.t.Helper()
	name, uid := l.runtime.readOnlyJobName, l.runtime.readOnlyJobUID
	if name == "" {
		l.fatalf("read-only Job name is missing")
	}
	if uid == "" {
		l.fatalf("read-only Job UID is missing")
	}
	if _, stderr, err := l.managerStatusKubectl("-n", l.in.proofNamespace, "patch", "ptahschema", l.readOnlyJobSchema,
		"--subresource=status", "--type=json", `-p=[{"op":"remove","path":"/status/activeOperation/jobUID"}]`); err != nil {
		l.fatalf("remove the committed Job UID of %s as the manager: %v: %s",
			l.readOnlyJobSchema, err, strings.TrimSpace(string(stderr)))
	}
	stored, err := l.runtimeFixtureSchema(l.readOnlyJobSchema)
	if err != nil || !lifecycleRuntimeUIDGapStaged(stored.Object, name) {
		l.fatalf("read-only fixture did not retain the exact Job name with an empty committed UID")
	}
	job, err := l.runtimeFixtureJob(name)
	if err != nil || !lifecycleRuntimeLateJobFailed(job.Object, uid) {
		l.fatalf("late-created read-only Job identity changed while staging the UID gap")
	}
}

// waitForReadOnlyJobCleanup waits for the candidate manager to schedule the
// retired Job's cleanup, and holds the Job to what it was apart from the TTL.
func (l *lifecycleRun) waitForReadOnlyJobCleanup() {
	l.t.Helper()
	before, ok := l.runtime.beforeCleanup[l.readOnlyJobSchema]
	if !ok {
		l.fatalf("no cleanup evidence of the read-only Job of %s was recorded", l.readOnlyJobSchema)
	}
	for deadline := time.Now().Add(120 * time.Second); time.Now().Before(deadline); {
		if job, err := l.runtimeFixtureJob(l.runtime.readOnlyJobName); err == nil && lifecycleRuntimeCleanupScheduled(job.Object) {
			after, err := lifecycleRuntimeJobCleanupEvidence(job.Object)
			l.check(err, "encode the cleanup evidence of Job %s", l.runtime.readOnlyJobName)
			if !bytes.Equal(before, after) {
				l.logf("read-only Job before cleanup: %s", before)
				l.logf("read-only Job after cleanup:  %s", after)
				l.fatalf("successor cleanup changed the read-only Job outside ttlSecondsAfterFinished")
			}
			return
		}
		l.sleep(time.Second)
	}
	l.fatalf("candidate manager did not schedule cleanup for the quiesced read-only Job")
}

// waitForSuspended waits for a schema in the proof namespace to report
// Suspended; the proof schema when none is named.
func (l *lifecycleRun) waitForSuspended(schema string) {
	l.t.Helper()
	if schema == "" {
		schema = lifecycleProofSchema
	}
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); {
		if stored, err := l.runtimeFixtureSchema(schema); err == nil &&
			lifecycleRuntimeString(stored.Object, "status", "phase") == "Suspended" {
			return
		}
		l.sleep(time.Second)
	}
	l.fatalf("PtahSchema %s did not become Suspended", schema)
}

// runtimeFirstDeployment names the first Deployment of a component in the release
// namespace, as kubectl's .items[0] did.
func (l *lifecycleRun) runtimeFirstDeployment(component string) string {
	l.t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion("apps/v1")
	list.SetKind("DeploymentList")
	l.check(l.cluster.Client.List(l.ctx, list, client.InNamespace(l.in.operatorNamespace),
		client.MatchingLabels{"app.kubernetes.io/component": component}), "list the %s Deployments", component)
	if len(list.Items) == 0 {
		return ""
	}
	return list.Items[0].GetName()
}

// runtimeDeploymentNames sets the controller and certificate-rotation
// Deployment names.
func (l *lifecycleRun) runtimeDeploymentNames() {
	l.t.Helper()
	l.controllerDeployment = l.runtimeFirstDeployment("controller")
	l.rotatorDeployment = l.runtimeFirstDeployment("certificate-rotation")
	if l.controllerDeployment == "" {
		l.fatalf("controller Deployment is missing")
	}
	if l.rotatorDeployment == "" {
		l.fatalf("certificate-rotation Deployment is missing")
	}
}

// runtimeOperatorObject reads one object in the release namespace as stored.
func (l *lifecycleRun) runtimeOperatorObject(apiVersion, kind, name string) (*unstructured.Unstructured, error) {
	object := &unstructured.Unstructured{}
	object.SetAPIVersion(apiVersion)
	object.SetKind(kind)
	err := l.cluster.Client.Get(l.ctx, types.NamespacedName{Namespace: l.in.operatorNamespace, Name: name}, object)
	return object, err
}

// captureCertificateSecretNames records the serving and staging certificate
// Secrets the rotator is told about, which an uninstall has to remove though
// they carry no release label.
func (l *lifecycleRun) captureCertificateSecretNames() {
	l.t.Helper()
	l.runtimeDeploymentNames()
	// One reading carries both names, where the script read the Deployment
	// once for each.
	rotator, err := l.runtimeOperatorObject("apps/v1", "Deployment", l.rotatorDeployment)
	l.check(err, "read the certificate-rotation Deployment %s", l.rotatorDeployment)
	serving, err := lifecycleRuntimeRotatorArgument(rotator.Object, "--secret-name=")
	if err != nil {
		l.fatalf("could not capture the exact generated certificate Secret identity: %v", err)
	}
	staging, err := lifecycleRuntimeRotatorArgument(rotator.Object, "--staging-secret-name=")
	if err != nil {
		l.fatalf("could not capture the exact certificate staging Secret identity: %v", err)
	}
	if serving == staging {
		l.fatalf("generated and staging certificate Secret identities must differ")
	}
	l.certificateSecretName, l.certificateStagingSecretName = serving, staging
}

// captureControllerImpersonationIdentity reads the manager's identity off its
// first running Pod: the ServiceAccount with its UID, and the Pod the
// credential is bound to.
func (l *lifecycleRun) captureControllerImpersonationIdentity() {
	l.t.Helper()
	l.runtimeDeploymentNames()
	pods := &corev1.PodList{}
	l.check(l.cluster.Client.List(l.ctx, pods, client.InNamespace(l.in.operatorNamespace), client.MatchingLabels{
		"app.kubernetes.io/instance": l.in.helmRelease, "app.kubernetes.io/component": "controller",
	}), "list the controller Pods")
	pod, err := lifecycleRuntimeImpersonationPod(pods.Items)
	if err != nil {
		l.fatalf("%v", err)
	}
	if pod.Spec.ServiceAccountName == "" {
		l.fatalf("the running controller Pod %s names no ServiceAccount", pod.Name)
	}
	account := &corev1.ServiceAccount{}
	l.check(l.cluster.Client.Get(l.ctx, types.NamespacedName{Namespace: l.in.operatorNamespace, Name: pod.Spec.ServiceAccountName}, account),
		"read ServiceAccount %s", pod.Spec.ServiceAccountName)
	if account.UID == "" {
		l.fatalf("controller ServiceAccount UID is empty")
	}
	l.impersonation = &lifecycleImpersonation{
		username: "system:serviceaccount:" + l.in.operatorNamespace + ":" + pod.Spec.ServiceAccountName,
		uid:      string(account.UID), podName: pod.Name, podUID: string(pod.UID),
	}
}

// clearControllerImpersonationIdentity forgets the manager's identity.
func (l *lifecycleRun) clearControllerImpersonationIdentity() {
	l.impersonation = nil
}

// captureControllerServiceAccountIdentity holds the release to one controller
// Deployment running the image given, and to the live ServiceAccount it runs
// as, and returns both identities.
func (l *lifecycleRun) captureControllerServiceAccountIdentity(expectedImage string) lifecycleControllerIdentity {
	l.t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion("apps/v1")
	list.SetKind("DeploymentList")
	l.check(l.cluster.Client.List(l.ctx, list, client.InNamespace(l.in.operatorNamespace), client.MatchingLabels{
		"app.kubernetes.io/instance": l.in.helmRelease, "app.kubernetes.io/component": "controller",
	}), "list the controller Deployments")
	deployments := make([]map[string]any, 0, len(list.Items))
	for index := range list.Items {
		deployments = append(deployments, list.Items[index].Object)
	}
	deployment, err := lifecycleRuntimeControllerDeployment(deployments, l.in.helmRelease, expectedImage)
	if err != nil {
		l.fatalf("controller Deployment does not have the exact runtime identity for image %s: %v", expectedImage, err)
	}
	account, err := l.runtimeOperatorObject("v1", "ServiceAccount", deployment.serviceAccount)
	uid := ""
	if err == nil {
		uid, err = lifecycleRuntimeLiveServiceAccount(account.Object, deployment.serviceAccount, l.in.operatorNamespace, l.in.helmRelease)
	}
	if err != nil {
		l.fatalf("controller ServiceAccount does not have the exact live identity for image %s: %v", expectedImage, err)
	}
	return lifecycleControllerIdentity{
		DeploymentName: deployment.name, DeploymentUID: types.UID(deployment.uid),
		ServiceAccountName: deployment.serviceAccount, ServiceAccountUID: types.UID(uid), ManagerImage: expectedImage,
	}
}

// runtimeCountLabeled counts the objects of a kind labeled for the release, in the
// release namespace or cluster-wide.
func (l *lifecycleRun) runtimeCountLabeled(inventory lifecycleRuntimeReleaseInventory, namespace string) int {
	l.t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion(inventory.apiVersion)
	list.SetKind(inventory.kind)
	options := []client.ListOption{client.MatchingLabels{"app.kubernetes.io/instance": l.in.helmRelease}}
	if namespace != "" {
		options = append(options, client.InNamespace(namespace))
	}
	l.check(l.cluster.Client.List(l.ctx, list, options...), "list the labeled %s objects", inventory.resource)
	return len(list.Items)
}

// assertReleaseRuntimeRemoved holds an uninstall to zero residue: nothing
// labeled for the release in the cluster or its namespace, no admission
// configuration, and neither certificate Secret the rotator named.
func (l *lifecycleRun) assertReleaseRuntimeRemoved() {
	l.t.Helper()
	if l.certificateSecretName == "" {
		l.fatalf("generated certificate Secret identity was not captured before uninstall")
	}
	if l.certificateStagingSecretName == "" {
		l.fatalf("certificate staging Secret identity was not captured before uninstall")
	}
	// helm uninstall --wait waits for the objects Helm deletes. The
	// ReplicaSets and Pods behind the Deployments go through garbage
	// collection and the Pods' termination grace afterwards, so give them that
	// time before the inventory below asserts that nothing labeled is left.
	replicaSets, pods := lifecycleRuntimeInventoryNamed("replicaset"), lifecycleRuntimeInventoryNamed("pod")
	for deadline := time.Now().Add(180 * time.Second); ; {
		remaining := l.runtimeCountLabeled(replicaSets, l.in.operatorNamespace) + l.runtimeCountLabeled(pods, l.in.operatorNamespace)
		if remaining == 0 {
			break
		}
		if !time.Now().Before(deadline) {
			l.fatalf("%d labeled ReplicaSet or Pod objects outlived uninstall by 180s", remaining)
		}
		l.sleep(2 * time.Second)
	}
	for _, kind := range []string{"MutatingWebhookConfiguration", "ValidatingWebhookConfiguration"} {
		configuration := &unstructured.Unstructured{}
		configuration.SetAPIVersion("admissionregistration.k8s.io/v1")
		configuration.SetKind(kind)
		err := l.cluster.Client.Get(l.ctx, types.NamespacedName{Name: lifecycleRuntimeAdmissionConfiguration}, configuration)
		if err == nil {
			l.fatalf("%s/%s survived uninstall", strings.ToLower(kind), lifecycleRuntimeAdmissionConfiguration)
		}
		if !apierrors.IsNotFound(err) {
			l.fatalf("read %s/%s: %v", strings.ToLower(kind), lifecycleRuntimeAdmissionConfiguration, err)
		}
	}
	for _, inventory := range lifecycleRuntimeClusterInventory {
		if remaining := l.runtimeCountLabeled(inventory, ""); remaining != 0 {
			l.fatalf("%d labeled %s objects survived uninstall", remaining, inventory.resource)
		}
	}
	for _, inventory := range lifecycleRuntimeNamespacedInventory {
		if remaining := l.runtimeCountLabeled(inventory, l.in.operatorNamespace); remaining != 0 {
			l.fatalf("%d labeled %s objects survived uninstall", remaining, inventory.resource)
		}
	}
	for _, secret := range []struct{ name, description string }{
		{l.certificateSecretName, "generated certificate"},
		{l.certificateStagingSecretName, "certificate staging"},
	} {
		_, err := l.runtimeOperatorObject("v1", "Secret", secret.name)
		if err == nil {
			l.fatalf("unlabeled %s Secret/%s survived uninstall", secret.description, secret.name)
		}
		if !apierrors.IsNotFound(err) {
			l.fatalf("read Secret/%s: %v", secret.name, err)
		}
	}
	l.certificateSecretName, l.certificateStagingSecretName = "", ""
}

// snapshotRuntimeDeployment records a runtime Deployment and the field
// manager that owns it. Helm 4 applies server-side, so restoring the snapshot
// with kubectl's own manager would leave the object owned by "kubectl-create"
// and the next helm upgrade would fail with a field-manager conflict over the
// fields Helm expects to own, instead of upgrading. The restore therefore
// applies as the manager the live object had, and a Deployment with no
// server-side apply manager, or more than one, is refused rather than guessed.
func (l *lifecycleRun) snapshotRuntimeDeployment(name string) (*unstructured.Unstructured, string) {
	l.t.Helper()
	// The client reads managed fields, as kubectl --show-managed-fields did.
	live, err := l.runtimeOperatorObject("apps/v1", "Deployment", name)
	l.check(err, "read runtime Deployment %s", name)
	snapshot, manager, err := lifecycleRuntimeSnapshot(live.Object)
	if err != nil {
		l.fatalf("runtime Deployment %s has %v", name, err)
	}
	return &unstructured.Unstructured{Object: snapshot}, manager
}

// restoreRuntimeDeployment recreates a snapshot as its recorded owner.
func (l *lifecycleRun) restoreRuntimeDeployment(snapshot *unstructured.Unstructured, manager, description string) {
	l.t.Helper()
	if snapshot == nil {
		l.fatalf("runtime Deployment snapshot %s is missing", description)
	}
	if manager == "" {
		l.fatalf("runtime Deployment snapshot %s has no recorded field manager", description)
	}
	document, err := json.Marshal(snapshot.Object)
	l.check(err, "encode the %s snapshot", description)
	if _, stderr, err := l.kubectlDocument(document, "apply", "--server-side", "--field-manager="+manager); err != nil {
		l.fatalf("restore runtime Deployment %s as %s: %v: %s", snapshot.GetName(), manager, err, strings.TrimSpace(string(stderr)))
	}
}

// runtimeDeleteDeployments deletes Deployments with foreground cascading and
// waits for them, then for every Pod of each component to be gone.
func (l *lifecycleRun) runtimeDeleteDeployments(deployments, components []string) {
	l.t.Helper()
	l.mustKubectl(append(append([]string{"-n", l.in.operatorNamespace, "delete", "deployment"}, deployments...),
		"--cascade=foreground", "--wait=true", "--timeout=2m")...)
	for _, component := range components {
		l.mustKubectl("-n", l.in.operatorNamespace, "wait", "pod", "-l", "app.kubernetes.io/component="+component,
			"--for=delete", "--timeout=2m")
	}
}

// stopRuntimeDeployments snapshots and removes the controller and the
// certificate rotator.
func (l *lifecycleRun) stopRuntimeDeployments() {
	l.t.Helper()
	l.runtimeDeploymentNames()
	l.controllerSnapshot, l.runtime.controllerManager = l.snapshotRuntimeDeployment(l.controllerDeployment)
	l.rotatorSnapshot, l.runtime.rotatorManager = l.snapshotRuntimeDeployment(l.rotatorDeployment)
	l.runtimeDeleteDeployments([]string{l.controllerDeployment, l.rotatorDeployment}, []string{"controller", "certificate-rotation"})
}

// stopControllerDeployment snapshots and removes the controller alone.
func (l *lifecycleRun) stopControllerDeployment() {
	l.t.Helper()
	l.runtimeDeploymentNames()
	l.controllerSnapshot, l.runtime.controllerManager = l.snapshotRuntimeDeployment(l.controllerDeployment)
	l.runtimeDeleteDeployments([]string{l.controllerDeployment}, []string{"controller"})
}

// startRuntimeDeployments restores both snapshots.
func (l *lifecycleRun) startRuntimeDeployments() {
	l.t.Helper()
	l.restoreRuntimeDeployment(l.controllerSnapshot, l.runtime.controllerManager, "controller-deployment.json")
	l.restoreRuntimeDeployment(l.rotatorSnapshot, l.runtime.rotatorManager, "certificate-deployment.json")
}

// startControllerDeployment restores the controller snapshot.
func (l *lifecycleRun) startControllerDeployment() {
	l.t.Helper()
	l.restoreRuntimeDeployment(l.controllerSnapshot, l.runtime.controllerManager, "controller-deployment.json")
}

// assertExplicitRuntimeGuard waits for the runtime in scope to be blocked by
// its verifier: the expected Pods each fail the verify-candidate-runtime init
// container, the same Pods for BLOCKED_STABILITY_SECONDS by the phase's own
// readings, and no main container ever starts in the meantime.
func (l *lifecycleRun) assertExplicitRuntimeGuard(description, scope string, expectedPods int) {
	l.t.Helper()
	var blockedSince time.Time
	blockedUIDs := ""
	for deadline := time.Now().Add(lifecycleRuntimeBlockedFailureTimeout); time.Now().Before(deadline); {
		pods := &corev1.PodList{}
		l.check(l.cluster.Client.List(l.ctx, pods, client.InNamespace(l.in.operatorNamespace)), "list the release namespace Pods")
		read := time.Now()
		state := lifecycleRuntimeGuard(pods.Items, l.in.helmRelease, scope, expectedPods)
		if !state.MainContainersNeverStarted {
			l.fatalf("%s allowed a manager or certificate-rotator main container to start", description)
		}
		switch {
		case !state.ExplicitVerifierFailures:
			blockedSince, blockedUIDs = time.Time{}, ""
		case state.PodUIDs != blockedUIDs:
			blockedSince, blockedUIDs = read, state.PodUIDs
		case read.Sub(blockedSince) >= lifecycleRuntimeBlockedStability:
			return
		}
		l.sleep(time.Second)
	}
	l.fatalf("%s did not produce stable explicit init-container failures on %d Pods", description, expectedPods)
}

// assertRuntimeBlocked is the guard over both runtime components.
func (l *lifecycleRun) assertRuntimeBlocked(description string) {
	l.t.Helper()
	l.assertExplicitRuntimeGuard(description, "all", 3)
}

// waitRuntimeReady waits for both runtime Deployments to roll out, and prints
// the release's init containers when one does not.
func (l *lifecycleRun) waitRuntimeReady() {
	l.t.Helper()
	l.runtimeDeploymentNames()
	for _, deployment := range []string{l.controllerDeployment, l.rotatorDeployment} {
		if err := l.cluster.WaitForRollout(l.ctx, l.in.operatorNamespace, deployment, 3*time.Minute); err != nil {
			pods := &corev1.PodList{}
			if l.cluster.Client.List(l.ctx, pods, client.InNamespace(l.in.operatorNamespace),
				client.MatchingLabels{"app.kubernetes.io/instance": l.in.helmRelease}) == nil {
				for _, summary := range lifecycleRuntimeInitStatusSummary(pods.Items) {
					encoded, _ := json.Marshal(summary)
					_, _ = fmt.Fprintf(os.Stderr, "%s\n", encoded)
				}
			}
			l.fatalf("runtime Deployment %s/%s did not become ready within 3m: %v", l.in.operatorNamespace, deployment, err)
		}
	}
}
