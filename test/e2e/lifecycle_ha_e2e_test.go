//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
	"github.com/stokaro/ptah-operator/test/e2e/phases"
)

// TestHA is the ha phase: one namespaced Lease with exact RBAC, a leader
// failover that keeps the Lease and moves its holder to a ready replica, and
// an operation the new leader admits, fails and counts in its metrics.
func TestHA(t *testing.T) {
	run, inputs := harness.Begin(t, phases.HA)
	h := newHARun(t, run, inputs)
	for _, scenario := range []struct {
		name string
		body func()
	}{
		{"lease-authorization", h.leaseAuthorization},
		{"leader-failover", h.leaderFailover},
		{"operation-after-failover", h.operationAfterFailover},
	} {
		if !run.Scenario(scenario.name, h.scenario(scenario.body)) {
			return
		}
	}
	run.Logf("e2e HA: PASS one Lease, exact RBAC, Pod failover, admitted operation, and custom metrics")
}

// haRun is what the high-availability proofs share. Each scenario runs as a
// subtest, and t is that subtest while it runs.
type haRun struct {
	t      *testing.T
	parent *testing.T
	ctx    context.Context
	in     phases.HAInputs

	cluster *harness.Cluster
	// manager is the controller Deployment, which also names its Lease Role
	// and its ClusterRole; serviceAccount is the account the manager runs as.
	manager, serviceAccount string
	// registryHost is the registry the manager image comes from, which serves
	// the operation images only to an authenticated puller.
	registryHost string
	credentials  registryCredentials

	// What the failover leaves for the operation that follows it.
	leaseUID      types.UID
	secondHolder  string
	failureBefore string
}

func newHARun(t *testing.T, run *harness.Run, in phases.HAInputs) *haRun {
	t.Helper()
	h := &haRun{t: t, parent: t, ctx: run.Context(), in: in}
	if info, err := os.Stat(in.Kubeconfig); err != nil || !info.Mode().IsRegular() {
		h.fatalf("E2E_KUBECONFIG does not name a file")
	}
	if err := haNamespacesDistinct(in.OperatorNamespace, in.HATestNamespace, in.ForeignNamespace, in.ProofNamespace); err != nil {
		h.fatalf("%v", err)
	}
	cluster, err := harness.Connect(in.Kubeconfig)
	if err != nil {
		h.fatalf("%v", err)
	}
	h.cluster = cluster
	// The operation images live in the run's own registry, which serves them
	// only to an authenticated puller, so the operation namespace needs the
	// credentials the release was installed with. They go into its pull
	// Secret and nowhere else.
	content, err := os.ReadFile(in.RegistryCredentialsFile)
	if err != nil {
		h.fatalf("E2E_REGISTRY_CREDENTIALS_FILE could not be read")
	}
	if h.credentials, err = parseRegistryCredentials(content); err != nil {
		h.fatalf("%v", err)
	}

	// The chart derives the controller's object names from the release and its
	// own naming rules, and gives the controller a ServiceAccount that carries
	// the controller-state version, so neither can be spelled from the release
	// name. Both are read from the installed controller Deployment.
	deployments := &appsv1.DeploymentList{}
	if err := h.cluster.Client.List(h.ctx, deployments, client.InNamespace(in.OperatorNamespace),
		client.MatchingLabels{"app.kubernetes.io/component": "controller"}); err != nil || len(deployments.Items) == 0 {
		h.fatalf("installed controller Deployment is missing")
	}
	deployment := deployments.Items[0]
	h.manager = deployment.Name
	h.serviceAccount = deployment.Spec.Template.Spec.ServiceAccountName
	if h.serviceAccount == "" {
		h.fatalf("installed controller Deployment %s has no ServiceAccount", h.manager)
	}
	image := ""
	if containers := deployment.Spec.Template.Spec.Containers; len(containers) > 0 {
		image = containers[0].Image
	}
	host, found := haRegistryHost(image)
	if !found {
		h.fatalf("manager image %s names no registry to authenticate against", image)
	}
	h.registryHost = host
	// Registered before anything is created, so a failure anywhere after this
	// still removes the operation namespace.
	t.Cleanup(h.cleanup)
	return h
}

func (h *haRun) scenario(body func()) func(*testing.T) {
	return func(t *testing.T) {
		h.t = t
		defer func() { h.t = h.parent }()
		body()
	}
}

func (h *haRun) fatalf(format string, arguments ...any) {
	h.t.Helper()
	h.t.Fatalf("e2e HA: "+format, arguments...)
}

func (h *haRun) logf(format string, arguments ...any) {
	h.t.Helper()
	h.t.Logf("e2e HA: "+format, arguments...)
}

// sleep pauses between two readings, and ends the scenario when the phase's
// own bound ends first.
func (h *haRun) sleep(duration time.Duration) {
	h.t.Helper()
	if err := haPause(h.ctx, duration); err != nil {
		h.fatalf("the phase's bound ended while it waited: %v", err)
	}
}

// haPause waits for the duration unless ctx ends first.
func haPause(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// diagnose writes one kubectl reading to the phase's standard error, as the
// script did before a failure it was about to report. It is diagnostics, so
// its own failure is written down rather than reported.
func (h *haRun) diagnose(arguments ...string) {
	stdout, stderr, err := h.cluster.Kubectl(h.ctx, arguments...)
	_, _ = os.Stderr.Write(stdout)
	_, _ = os.Stderr.Write(stderr)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "e2e HA: kubectl %s: %v\n", strings.Join(arguments, " "), err)
	}
}

// cleanup removes the operation namespace whatever happened, without waiting,
// as the script's exit trap did. A failure to remove it is not the phase's
// verdict: the trap ignored it too.
func (h *haRun) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	namespace := &corev1.Namespace{}
	namespace.Name = h.in.HATestNamespace
	_ = client.IgnoreNotFound(h.cluster.Client.Delete(ctx, namespace))
}

// leaseAuthorization proves the manager's Lease grant is namespaced and
// exact: get, create and update in its own namespace, nothing in a foreign
// one, no other verb anywhere, a Role with that one rule, and a ClusterRole
// that grants no Lease at all.
func (h *haRun) leaseAuthorization() {
	h.t.Helper()
	h.logf("verifying namespace-scoped Lease authorization")
	for _, verb := range []string{"get", "create", "update"} {
		h.assertCanI(true, h.in.OperatorNamespace, verb)
		h.assertCanI(false, h.in.ForeignNamespace, verb)
	}
	for _, verb := range []string{"list", "watch", "patch", "delete", "deletecollection"} {
		h.assertCanI(false, h.in.OperatorNamespace, verb)
		h.assertCanI(false, h.in.ForeignNamespace, verb)
	}

	// Compared as the stored document, so a rule that gained a key the typed
	// PolicyRule drops when empty is not the same rule.
	role := &unstructured.Unstructured{}
	role.SetGroupVersionKind(schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "Role"})
	if err := h.cluster.Client.Get(h.ctx, types.NamespacedName{Namespace: h.in.OperatorNamespace, Name: h.manager}, role); err != nil ||
		!haRoleRulesExact(role.Object["rules"]) {
		h.fatalf("manager Lease Role is not exact")
	}
	clusterRole := &unstructured.Unstructured{}
	clusterRole.SetGroupVersionKind(schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"})
	if err := h.cluster.Client.Get(h.ctx, types.NamespacedName{Name: h.manager}, clusterRole); err != nil {
		h.fatalf("manager ClusterRole grants cluster-wide Lease access")
	}
	rules, _, err := unstructured.NestedSlice(clusterRole.Object, "rules")
	if err != nil || haClusterRoleGrantsLeases(rules) {
		h.fatalf("manager ClusterRole grants cluster-wide Lease access")
	}
}

// assertCanI asks what `kubectl auth can-i <verb> leases.coordination.k8s.io
// --as=system:serviceaccount:<ns>:<account>` asked: a review sent as the
// manager's account alone, whose groups the API server derives.
func (h *haRun) assertCanI(want bool, namespace, verb string) {
	h.t.Helper()
	user := "system:serviceaccount:" + h.in.OperatorNamespace + ":" + h.serviceAccount
	allowed, err := h.cluster.CanI(h.ctx, user, authorizationv1.ResourceAttributes{
		Namespace: namespace, Verb: verb, Group: "coordination.k8s.io", Resource: "leases",
	})
	if err != nil {
		h.fatalf("could not ask whether %s/%s can %s a Lease in %s: %v",
			h.in.OperatorNamespace, h.serviceAccount, verb, namespace, err)
	}
	if allowed != want {
		h.fatalf("%s/%s can-i %s Lease in %s = %s, want %s",
			h.in.OperatorNamespace, h.serviceAccount, verb, namespace, haYesNo(allowed), haYesNo(want))
	}
}

func haYesNo(allowed bool) string {
	if allowed {
		return "yes"
	}
	return "no"
}

// leaderFailover proves the leader Lease moves to a ready replica when the
// leader's Pod goes, keeps its identity, counts the transition, and that the
// new leader reports itself as such. It ends with the Resolve failure counter
// the operation after it has to raise, read once every earlier source of that
// counter is quiet.
func (h *haRun) leaderFailover() {
	h.t.Helper()
	if err := h.cluster.WaitForRollout(h.ctx, h.in.OperatorNamespace, h.manager, haRolloutTimeout); err != nil {
		h.fatalf("%v", err)
	}
	deployment := &appsv1.Deployment{}
	if err := h.cluster.Client.Get(h.ctx, types.NamespacedName{Namespace: h.in.OperatorNamespace, Name: h.manager}, deployment); err != nil {
		h.fatalf("manager Deployment %s could not be read: %v", h.manager, err)
	}
	if ready := deployment.Status.ReadyReplicas; ready != 2 {
		h.fatalf("manager Deployment has %d ready replicas, want two", ready)
	}

	initialHolder := h.waitForLeader("")
	initialPod := haLeaderPodName(initialHolder)
	lease := h.lease()
	h.leaseUID = lease.UID
	initialTransitions := haLeaseTransitions(lease)
	h.assertLeaseIdentity(h.leaseUID)
	h.assertActiveLeaderMetric(initialHolder)

	h.logf("deleting leader Pod %s/%s", h.in.OperatorNamespace, initialPod)
	h.deletePodAndWait(initialPod, haPodDeleteBound)
	h.secondHolder = h.waitForLeader(initialHolder)
	h.assertLeaseIdentity(h.leaseUID)
	if secondTransitions := haLeaseTransitions(h.lease()); secondTransitions <= initialTransitions {
		h.fatalf("leader Pod failover did not increment leaseTransitions")
	}
	h.assertActiveLeaderMetric(h.secondHolder)
	if err := h.cluster.WaitForRollout(h.ctx, h.in.OperatorNamespace, h.manager, haRolloutTimeout); err != nil {
		h.fatalf("%v", err)
	}
	h.assertPriorResolveMetricSourcesQuiesced()
	h.failureBefore = h.readResolveFailureCounter(h.secondHolder)
}

// lease reads the manager's leader Lease.
func (h *haRun) lease() *coordinationv1.Lease {
	h.t.Helper()
	lease := &coordinationv1.Lease{}
	if err := h.cluster.Client.Get(h.ctx, types.NamespacedName{Namespace: h.in.OperatorNamespace, Name: leaderLeaseName}, lease); err != nil {
		h.fatalf("leader Lease %s/%s could not be read: %v", h.in.OperatorNamespace, leaderLeaseName, err)
	}
	return lease
}

// waitForLeader waits for the Lease to name a holder other than previous
// whose Pod is a ready manager replica of this release, and returns it.
func (h *haRun) waitForLeader(previous string) string {
	h.t.Helper()
	for deadline := time.Now().Add(haLeaderTimeout); time.Now().Before(deadline); {
		lease := &coordinationv1.Lease{}
		if err := h.cluster.Client.Get(h.ctx, types.NamespacedName{Namespace: h.in.OperatorNamespace, Name: leaderLeaseName}, lease); err == nil {
			holder := haLeaseHolder(lease)
			if haLeaderMoved(holder, previous) && h.holderIsReadyManagerPod(holder) {
				return holder
			}
		}
		h.sleep(haPoll)
	}
	h.diagnose("-n", h.in.OperatorNamespace, "get", "lease", leaderLeaseName, "-o", "yaml")
	h.diagnose("-n", h.in.OperatorNamespace, "get", "pods", "-l", "app.kubernetes.io/component=controller", "-o", "wide")
	h.fatalf("manager leader Lease did not move to a ready replica")
	return ""
}

// holderIsReadyManagerPod reads the Pod a holder identity names and holds it
// to a ready manager replica of this release.
func (h *haRun) holderIsReadyManagerPod(holder string) bool {
	name := haLeaderPodName(holder)
	if name == "" {
		return false
	}
	pod := &corev1.Pod{}
	if err := h.cluster.Client.Get(h.ctx, types.NamespacedName{Namespace: h.in.OperatorNamespace, Name: name}, pod); err != nil {
		return false
	}
	return haHolderIsReadyManagerPod(pod, h.in.HelmRelease)
}

// deletePodAndWait deletes a Pod and waits for that Pod, by UID, to be gone,
// as kubectl delete --wait does.
func (h *haRun) deletePodAndWait(name string, bound time.Duration) {
	h.t.Helper()
	pod := &corev1.Pod{}
	key := types.NamespacedName{Namespace: h.in.OperatorNamespace, Name: name}
	if err := h.cluster.Client.Get(h.ctx, key, pod); err != nil {
		h.fatalf("leader Pod %s/%s could not be read: %v", h.in.OperatorNamespace, name, err)
	}
	uid := pod.UID
	if err := h.cluster.Client.Delete(h.ctx, pod); client.IgnoreNotFound(err) != nil {
		h.fatalf("leader Pod %s/%s could not be deleted: %v", h.in.OperatorNamespace, name, err)
	}
	for deadline := time.Now().Add(bound); time.Now().Before(deadline); {
		current := &corev1.Pod{}
		err := h.cluster.Client.Get(h.ctx, key, current)
		if apierrors.IsNotFound(err) || (err == nil && current.UID != uid) {
			return
		}
		h.sleep(haPoll)
	}
	h.fatalf("leader Pod %s/%s was not removed within %s", h.in.OperatorNamespace, name, bound)
}

// metrics reads what a manager replica serves on its metrics port, through
// the API server's Pod proxy.
func (h *haRun) metrics(ctx context.Context, pod string) ([]byte, error) {
	return h.cluster.Raw(ctx, "/api/v1/namespaces/"+h.in.OperatorNamespace+"/pods/"+pod+":8080/proxy/metrics")
}

// assertActiveLeaderMetric holds the holder's replica to reporting itself as
// the leader.
func (h *haRun) assertActiveLeaderMetric(holder string) {
	h.t.Helper()
	pod := haLeaderPodName(holder)
	body, err := h.metrics(h.ctx, pod)
	if err != nil {
		h.fatalf("%s/%s metrics could not be read: %v", h.in.OperatorNamespace, pod, err)
	}
	if !haReportsActiveLeader(body) {
		h.fatalf("%s/%s does not report active leader status", h.in.OperatorNamespace, pod)
	}
}

// assertLeaseIdentity holds the leader Lease to the one the failover started
// with, and to being the only one of its name in the cluster.
func (h *haRun) assertLeaseIdentity(expected types.UID) {
	h.t.Helper()
	if actual := h.lease().UID; actual != expected {
		h.fatalf("leader Lease was recreated during failover: %s != %s", actual, expected)
	}
	leases := &coordinationv1.LeaseList{}
	if err := h.cluster.Client.List(h.ctx, leases); err != nil {
		h.fatalf("the cluster's Leases could not be listed: %v", err)
	}
	if count := haCountLeasesNamed(leases.Items, leaderLeaseName); count != 1 {
		h.fatalf("cluster contains %d manager leader Leases named %s, want one", count, leaderLeaseName)
	}
}

// assertPriorResolveMetricSourcesQuiesced proves nothing but this phase's
// operation can move the Resolve failure counter. The upgrade phase leaves the
// schemas it drove behind, and they are what produced the Resolve metrics this
// phase measures a delta against. The quiescence check says nothing when there
// are none, so the sources have to exist before they are required to be
// quiet.
func (h *haRun) assertPriorResolveMetricSourcesQuiesced() {
	h.t.Helper()
	proof := &ptahv1alpha1.PtahSchemaList{}
	if err := h.cluster.Client.List(h.ctx, proof, client.InNamespace(h.in.ProofNamespace)); err != nil || len(proof.Items) < 1 {
		h.fatalf("the upgrade phase left no Resolve metric source to quiesce")
	}
	schemas := &ptahv1alpha1.PtahSchemaList{}
	if err := h.cluster.Client.List(h.ctx, schemas); err != nil {
		h.fatalf("an earlier unsuspended PtahSchema can contaminate the HA metric delta")
	}
	for index := range schemas.Items {
		if !haSchemaQuiesced(&schemas.Items[index]) {
			h.fatalf("an earlier unsuspended PtahSchema can contaminate the HA metric delta")
		}
	}
	jobs := &batchv1.JobList{}
	if err := h.cluster.Client.List(h.ctx, jobs, client.MatchingLabels{labelOperation: "resolve"}); err != nil {
		h.fatalf("an earlier nonterminal Resolve Job can contaminate the HA metric delta")
	}
	for index := range jobs.Items {
		if !haResolveJobTerminal(&jobs.Items[index]) {
			h.fatalf("an earlier nonterminal Resolve Job can contaminate the HA metric delta")
		}
	}
}

// readResolveFailureCounter reads the Resolve failure counter the holder's
// replica serves before the operation that has to raise it.
func (h *haRun) readResolveFailureCounter(holder string) string {
	h.t.Helper()
	pod := haLeaderPodName(holder)
	body, err := h.metrics(h.ctx, pod)
	if err != nil {
		h.fatalf("%s/%s metrics endpoint was unavailable before the HA operation", h.in.OperatorNamespace, pod)
	}
	value, err := haResolveFailureCounter(body)
	if err != nil {
		h.fatalf("%s/%s exposed an invalid Resolve operation failure counter", h.in.OperatorNamespace, pod)
	}
	return value
}

// operationAfterFailover proves the new leader reconciles a real operation:
// its Job's Pod is admitted, the Resolve fails the way a Resolve against an
// artifact nobody serves has to, the failure counter goes up with exactly the
// families the manager publishes, and the Lease is still the one it was.
// Removing the schema and its Job leaves no operation Pod behind.
func (h *haRun) operationAfterFailover() {
	h.t.Helper()
	h.logf("creating a real operation after leader failover")
	h.waitForSchemaCRDEstablished()
	namespace := &corev1.Namespace{}
	namespace.Name = h.in.HATestNamespace
	h.create(namespace)
	h.waitForDefaultServiceAccount()
	policy, err := os.ReadFile(filepath.Join(repositoryRoot, "testdata", "e2e", "verification-policy.yaml"))
	if err != nil {
		h.fatalf("the verification policy fixture could not be read: %v", err)
	}
	h.create(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: h.in.HATestNamespace, Name: haVerificationPolicy},
		Data:       map[string]string{"policy.yaml": string(policy)},
	})
	h.create(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: h.in.HATestNamespace, Name: haDatabaseURLSecret},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"url": []byte(haDatabaseURL)},
	})
	h.create(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: h.in.HATestNamespace, Name: haPullSecret},
		Type:       corev1.SecretTypeDockerConfigJson,
		StringData: map[string]string{
			corev1.DockerConfigJsonKey: dockerConfigJSON(h.registryHost, h.credentials.Username, h.credentials.Password),
		},
	})
	created := &unstructured.Unstructured{Object: haSchemaDocument(h.in.HATestNamespace)}
	if err := h.cluster.Client.Create(h.ctx, created, client.FieldOwner(harness.FieldOwner),
		client.FieldValidation("Strict")); err != nil {
		h.fatalf("PtahSchema %s/%s could not be created: %v", h.in.HATestNamespace, haSchema, err)
	}
	schemaUID := created.GetUID()

	operationJob := h.waitForAdmittedOperationPod(schemaUID)
	if operationJob == "" {
		h.fatalf("failover operation Job name is empty")
	}
	h.waitForFailedResolveLifecycle(schemaUID)
	h.assertActiveLeaderMetric(h.secondHolder)
	h.assertCustomOperatorMetrics(h.secondHolder, h.failureBefore)
	h.assertLeaseIdentity(h.leaseUID)

	schemaObject := &ptahv1alpha1.PtahSchema{}
	schemaObject.Namespace, schemaObject.Name = h.in.HATestNamespace, haSchema
	if err := h.cluster.Client.Delete(h.ctx, schemaObject); client.IgnoreNotFound(err) != nil {
		h.fatalf("PtahSchema %s/%s could not be deleted: %v", h.in.HATestNamespace, haSchema, err)
	}
	// The operator removes a finished operation Job on its own, so the Job may
	// already be gone by now; what this proves either way is that no operation
	// Pod outlives it.
	h.deleteJobInBackground(operationJob)
	remaining := h.remainingOperationPods(operationJob)
	for deadline := time.Now().Add(60 * time.Second); len(remaining) > 0 && time.Now().Before(deadline); {
		h.sleep(haPoll)
		remaining = h.remainingOperationPods(operationJob)
	}
	if len(remaining) > 0 {
		h.fatalf("background Job deletion left orphan operation Pods: %s", strings.Join(remaining, " "))
	}
	h.waitForGone(schemaObject, types.NamespacedName{Namespace: h.in.HATestNamespace, Name: haSchema}, 60*time.Second)
	if err := h.cluster.Client.Delete(h.ctx, namespace); client.IgnoreNotFound(err) != nil {
		h.fatalf("namespace %s could not be deleted: %v", h.in.HATestNamespace, err)
	}
	// Durable receipts and their credentials outlive the workload through the
	// recovery window. Namespace deletion is asynchronous for that reason; the
	// schema, Job and Pods above must still be gone before this phase passes.
	// The retention and namespace-cleanup proofs measure their later collection.
	h.logf("operation workload removed; namespace deletion requested with result retention intact")
}

// create creates one object the operation needs, as kubectl create did.
func (h *haRun) create(object client.Object) {
	h.t.Helper()
	if err := h.cluster.Client.Create(h.ctx, object, client.FieldOwner(harness.FieldOwner)); err != nil {
		h.fatalf("%T %s could not be created: %v", object, client.ObjectKeyFromObject(object), err)
	}
}

// waitForSchemaCRDEstablished is kubectl wait --for=condition=Established on
// the PtahSchema CRD, bounded at 60 seconds.
func (h *haRun) waitForSchemaCRDEstablished() {
	h.t.Helper()
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := h.cluster.Client.Get(h.ctx, types.NamespacedName{Name: haSchemaCRD}, crd); err == nil {
			for _, condition := range crd.Status.Conditions {
				if condition.Type == apiextensionsv1.Established && condition.Status == apiextensionsv1.ConditionTrue {
					return
				}
			}
		}
		h.sleep(haPoll)
	}
	h.fatalf("CRD %s was not Established within 60s", haSchemaCRD)
}

// waitForDefaultServiceAccount waits for the namespace's default account,
// which the operation's Pod runs as.
func (h *haRun) waitForDefaultServiceAccount() {
	h.t.Helper()
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); {
		account := &corev1.ServiceAccount{}
		if h.cluster.Client.Get(h.ctx, types.NamespacedName{Namespace: h.in.HATestNamespace, Name: "default"}, account) == nil {
			return
		}
		h.sleep(haPoll)
	}
	h.fatalf("%s/default ServiceAccount was not created", h.in.HATestNamespace)
}

// waitForAdmittedOperationPod waits for the failover schema to own exactly
// one operation Job whose Pod was admitted under the Job's snapshot digest,
// and returns the Job's name. A second Job is a failure at once: the new
// leader reconciled the schema twice.
func (h *haRun) waitForAdmittedOperationPod(schemaUID types.UID) string {
	h.t.Helper()
	for deadline := time.Now().Add(haWorkloadTimeout); time.Now().Before(deadline); {
		jobs := &batchv1.JobList{}
		// A read that fails is one more poll: the wait still has to see the
		// admitted Pod to end.
		if err := h.cluster.Client.List(h.ctx, jobs, client.InNamespace(h.in.HATestNamespace),
			client.MatchingLabels{labelSchema: haSchema}); err == nil {
			owned := haSchemaJobs(jobs.Items, schemaUID)
			if len(owned) > 1 {
				h.fatalf("failover reconciliation created %d operation Jobs", len(owned))
			}
			if len(owned) == 1 {
				job := owned[0]
				pods := &corev1.PodList{}
				if err := h.cluster.Client.List(h.ctx, pods, client.InNamespace(h.in.HATestNamespace),
					client.MatchingLabels{"batch.kubernetes.io/job-name": job.Name}); err == nil && haAdmittedPod(&job, pods.Items) {
					return job.Name
				}
			}
		}
		h.sleep(haPoll)
	}
	h.diagnose("-n", h.in.HATestNamespace, "get", "ptahschema", haSchema, "-o", "yaml")
	h.diagnose("-n", h.in.HATestNamespace, "get", "jobs,pods", "-o", "wide")
	h.diagnose("-n", h.in.HATestNamespace, "get", "events", "--sort-by=.metadata.creationTimestamp")
	h.fatalf("new leader did not reconcile an operation into an admitted Job Pod")
	return ""
}

// waitForFailedResolveLifecycle waits for the schema to report the Resolve's
// typed failure for its own generation.
func (h *haRun) waitForFailedResolveLifecycle(schemaUID types.UID) {
	h.t.Helper()
	for deadline := time.Now().Add(haWorkloadTimeout); time.Now().Before(deadline); {
		current := &ptahv1alpha1.PtahSchema{}
		if err := h.cluster.Client.Get(h.ctx, types.NamespacedName{Namespace: h.in.HATestNamespace, Name: haSchema}, current); err == nil &&
			haFailedResolveLifecycle(current, schemaUID) {
			return
		}
		h.sleep(haPoll)
	}
	h.diagnose("-n", h.in.HATestNamespace, "get", "ptahschema", haSchema, "-o", "yaml")
	h.diagnose("-n", h.in.HATestNamespace, "get", "jobs,pods", "-o", "wide")
	h.fatalf("post-failover Resolve did not reach its typed failed lifecycle state")
}

// assertCustomOperatorMetrics waits for the holder's replica to serve the
// exact post-failure families with a Resolve failure counter above the one
// read before the operation.
func (h *haRun) assertCustomOperatorMetrics(holder, baseline string) {
	h.t.Helper()
	pod := haLeaderPodName(holder)
	_, err := haAwaitIncreasedFailureCounter(h.ctx,
		func(ctx context.Context) ([]byte, error) { return h.metrics(ctx, pod) },
		baseline, haMetricsTimeout, haPoll, time.Now, haPause)
	switch {
	case err == nil:
	case errors.Is(err, errHAMetricsMalformed):
		h.fatalf("%s/%s exposes malformed, duplicate, or unexpected custom metric evidence", h.in.OperatorNamespace, pod)
	case errors.Is(err, errHAFailureCounter):
		h.fatalf("%s/%s exposed an invalid Resolve operation failure counter", h.in.OperatorNamespace, pod)
	case errors.Is(err, errHAMetricsTimeout):
		h.fatalf("%s/%s did not expose an increased Resolve operation failure counter with the exact post-failure custom metric families",
			h.in.OperatorNamespace, pod)
	default:
		h.fatalf("the phase's bound ended while it waited: %v", err)
	}
}

// deleteJobInBackground deletes the operation Job with background propagation
// and waits up to 30 seconds for that Job, by UID, to be gone, as kubectl
// delete --cascade=background --wait=true --ignore-not-found did.
func (h *haRun) deleteJobInBackground(name string) {
	h.t.Helper()
	key := types.NamespacedName{Namespace: h.in.HATestNamespace, Name: name}
	job := &batchv1.Job{}
	if err := h.cluster.Client.Get(h.ctx, key, job); err != nil {
		if apierrors.IsNotFound(err) {
			return
		}
		h.fatalf("Job %s could not be read: %v", key, err)
	}
	uid := job.UID
	if err := h.cluster.Client.Delete(h.ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
		h.fatalf("Job %s could not be deleted: %v", key, err)
	}
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		current := &batchv1.Job{}
		err := h.cluster.Client.Get(h.ctx, key, current)
		if apierrors.IsNotFound(err) || (err == nil && current.UID != uid) {
			return
		}
		h.sleep(haPoll)
	}
	h.fatalf("Job %s was not removed within 30s", key)
}

// remainingOperationPods is every Pod the operation Job's name still labels,
// as kubectl get -o name prints them.
func (h *haRun) remainingOperationPods(job string) []string {
	h.t.Helper()
	pods := &corev1.PodList{}
	if err := h.cluster.Client.List(h.ctx, pods, client.InNamespace(h.in.HATestNamespace),
		client.MatchingLabels{"batch.kubernetes.io/job-name": job}); err != nil {
		h.fatalf("the operation Pods of Job %s could not be listed: %v", job, err)
	}
	names := make([]string, 0, len(pods.Items))
	for _, pod := range pods.Items {
		names = append(names, "pod/"+pod.Name)
	}
	return names
}

// waitForGone waits for an object to be removed, as kubectl wait --for=delete
// and kubectl delete --wait did.
func (h *haRun) waitForGone(object client.Object, key types.NamespacedName, bound time.Duration) {
	h.t.Helper()
	for deadline := time.Now().Add(bound); time.Now().Before(deadline); {
		if apierrors.IsNotFound(h.cluster.Client.Get(h.ctx, key, object)) {
			return
		}
		h.sleep(haPoll)
	}
	h.fatalf("%T %s was not removed within %s", object, key, bound)
}
