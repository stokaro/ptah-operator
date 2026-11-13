//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// The teardown's bounds on its own preparation. The bound on the namespace
// itself is not here: it is what the records persisted.
const (
	teardownOperationTimeout = 3 * time.Minute
	teardownWorkloadTimeout  = 2 * time.Minute
	// teardownRetireTimeout covers the collector's five-second steps over
	// both of its scans, and the thirty seconds it defers a root it has just
	// read, several times over.
	teardownRetireTimeout = 3 * time.Minute
	teardownPoll          = time.Second
)

// uninstallFirstTeardown runs the teardown the operations page gives a lab or
// CI cluster that stays, as its three commands: delete the resources, uninstall
// the release, delete the namespace. The namespace holds the records of one
// finished operation, retired and inside their retention window, which the
// admission webhook refuses to delete for an hour while the release is
// installed. With the release gone the namespace has to go long before that
// window ends, leaving none of its records or Secrets, and nothing it reports
// may name a refused deletion or a finalizer left on its content.
//
// The scenario installs the candidate again first, since the phase leaves no
// release installed before it, and holds the uninstall to everything the
// phase's other uninstalls are held to.
func (l *lifecycleRun) uninstallFirstTeardown() {
	l.t.Helper()
	namespace := l.in.teardownNamespace
	l.logf("installing the candidate release for the uninstall-first teardown")
	l.mustHelm("the install before the uninstall-first teardown failed; Helm's own error is above",
		"install", l.in.helmRelease, l.in.chartPackage, "--namespace", l.in.operatorNamespace,
		"--values", l.in.candidateValuesFile, "--wait", "--timeout", "5m")
	l.waitRuntimeReady()
	l.captureControllerServiceAccountIdentity(l.in.controllerImage)
	// The new manager may refresh what the proof objects report, so the
	// uninstall below is held to the state they are in now.
	l.proofEvidence("-before")

	schemaUID := l.startTeardownOperation(namespace)
	l.waitTeardownResolveFailed(namespace, schemaUID)
	l.logf("teardown: kubectl delete ptahschemas,ptahmigrations --all -n %s --wait=true", namespace)
	l.mustKubectl("delete", "ptahschemas,ptahmigrations", "--all", "-n", namespace, "--wait=true", "--timeout=2m")
	l.waitTeardownWorkloadGone(namespace)
	attempt := l.waitTeardownAttemptRetired(namespace)
	for _, record := range attempt.Records {
		l.logf("teardown: record %s type=%s uid=%s created=%s", record.Name, record.Type, record.UID, record.Created.Format(time.RFC3339))
	}
	l.logf("teardown: retired at %s; retention lets the first record go at %s",
		attempt.Retired.Format(time.RFC3339), attempt.WindowEnds.Format(time.RFC3339))

	l.logf("teardown: helm uninstall %s -n %s --wait --timeout 5m", l.in.helmRelease, l.in.operatorNamespace)
	l.uninstallAndAssertRetained("the uninstall before the namespace teardown failed; Helm's own error is above")
	l.logf("teardown: kubectl delete namespace %s --wait=true", namespace)
	l.deleteTeardownNamespace(namespace, attempt)
	l.assertTeardownLeftNothing(namespace, attempt)
	l.logf("PASS uninstall-first teardown: namespace %s and its %d retained records gone inside their window", namespace, len(attempt.Records))
}

// startTeardownOperation creates the namespace, the fixtures the operation
// reads and the schema, and returns the schema's UID.
func (l *lifecycleRun) startTeardownOperation(namespace string) types.UID {
	l.t.Helper()
	l.createTeardownObject(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})
	l.check(harness.Wait(l.ctx, "the default ServiceAccount of namespace "+namespace, teardownWorkloadTimeout, teardownPoll,
		func(ctx context.Context) (bool, string, error) {
			err := l.cluster.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "default"}, &corev1.ServiceAccount{})
			if err != nil {
				return false, err.Error(), nil
			}
			return true, "", nil
		}), "wait for the operation's ServiceAccount")

	policy, err := os.ReadFile(filepath.Join(repositoryRoot, "testdata", "e2e", "verification-policy.yaml"))
	l.check(err, "read the verification policy fixture")
	registryContent, err := os.ReadFile(l.in.registryCredentialsFile)
	l.check(err, "read E2E_REGISTRY_CREDENTIALS_FILE")
	registry, err := parseRegistryCredentials(registryContent)
	if err != nil {
		l.fatalf("%v", err)
	}
	// The operation pulls the executor from the registry the release names it
	// in, which serves it only to an authenticated puller.
	l.runtimeDeploymentNames()
	controller := &appsv1.Deployment{}
	l.check(l.cluster.Client.Get(l.ctx, types.NamespacedName{Namespace: l.in.operatorNamespace, Name: l.controllerDeployment}, controller),
		"read the controller Deployment for its executor image")
	executor, err := predecessorApplyExecutorImage(controller)
	if err != nil {
		l.fatalf("could not read the live executor image: %v", err)
	}
	l.createTeardownObject(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: teardownVerificationPolicy},
		Data:       map[string]string{"policy.yaml": string(policy)},
	})
	l.createTeardownObject(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: teardownDatabaseURLSecret},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"url": []byte(teardownDatabaseURL)},
	})
	l.createTeardownObject(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: teardownPullSecret},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(dockerConfigJSON(predecessorApplyRegistry(executor), registry.Username, registry.Password)),
		},
	})
	schema := &unstructured.Unstructured{Object: teardownSchemaDocument(namespace)}
	if err := l.cluster.Client.Create(l.ctx, schema, client.FieldOwner(harness.FieldOwner), client.FieldValidation("Strict")); err != nil {
		l.fatalf("PtahSchema %s/%s could not be created: %v", namespace, teardownSchema, err)
	}
	return schema.GetUID()
}

// createTeardownObject creates one object of the teardown's own.
func (l *lifecycleRun) createTeardownObject(object client.Object) {
	l.t.Helper()
	if err := l.cluster.Client.Create(l.ctx, object, client.FieldOwner(harness.FieldOwner)); err != nil {
		l.fatalf("%T %s could not be created: %v", object, client.ObjectKeyFromObject(object), err)
	}
}

// waitTeardownResolveFailed waits for the schema to report its Resolve's typed
// failure: the operation ran in a Pod, delivered its result and ended.
func (l *lifecycleRun) waitTeardownResolveFailed(namespace string, schemaUID types.UID) {
	l.t.Helper()
	err := harness.Wait(l.ctx, fmt.Sprintf("PtahSchema %s/%s to record its failed Resolve", namespace, teardownSchema),
		teardownOperationTimeout, teardownPoll, func(ctx context.Context) (bool, string, error) {
			schema := &ptahv1alpha1.PtahSchema{}
			if err := l.cluster.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: teardownSchema}, schema); err != nil {
				return false, err.Error(), nil
			}
			return haFailedResolveLifecycle(schema, schemaUID), fmt.Sprintf("phase=%s observedGeneration=%d generation=%d",
				schema.Status.Phase, schema.Status.ObservedGeneration, schema.Generation), nil
		})
	if err != nil {
		l.diagnoseTeardown(namespace)
		l.fatalf("%v", err)
	}
}

// waitTeardownWorkloadGone waits for the operation's Job and Pods to go with
// the schema that owns them, so that the namespace holds no object Kubernetes
// finalizes itself when it is deleted.
func (l *lifecycleRun) waitTeardownWorkloadGone(namespace string) {
	l.t.Helper()
	err := harness.Wait(l.ctx, "the operation's Jobs and Pods in namespace "+namespace+" to go with their schema",
		teardownWorkloadTimeout, teardownPoll, func(ctx context.Context) (bool, string, error) {
			jobs, pods := &batchv1.JobList{}, &corev1.PodList{}
			if err := l.cluster.Client.List(ctx, jobs, client.InNamespace(namespace)); err != nil {
				return false, err.Error(), nil
			}
			if err := l.cluster.Client.List(ctx, pods, client.InNamespace(namespace)); err != nil {
				return false, err.Error(), nil
			}
			return len(jobs.Items) == 0 && len(pods.Items) == 0, fmt.Sprintf("%d Jobs and %d Pods remain", len(jobs.Items), len(pods.Items)), nil
		})
	if err != nil {
		l.diagnoseTeardown(namespace)
		l.fatalf("%v", err)
	}
}

// waitTeardownAttemptRetired waits for the collector to retire the deleted
// schema's attempt, and returns what its records persisted.
func (l *lifecycleRun) waitTeardownAttemptRetired(namespace string) teardownAttempt {
	l.t.Helper()
	var attempt teardownAttempt
	err := harness.Wait(l.ctx, "the collector to retire the attempt in namespace "+namespace,
		teardownRetireTimeout, teardownPoll, func(ctx context.Context) (bool, string, error) {
			records := &ptahv1alpha1.PtahResultRecordList{}
			if err := l.cluster.Client.List(ctx, records, client.InNamespace(namespace)); err != nil {
				return false, err.Error(), nil
			}
			read, err := teardownRetiredAttempt(records.Items)
			if err != nil {
				return false, err.Error(), nil
			}
			attempt = read
			return true, "", nil
		})
	if err != nil {
		l.diagnoseTeardown(namespace)
		l.fatalf("%v", err)
	}
	return attempt
}

// deleteTeardownNamespace deletes the namespace and waits for it to be gone.
// The bound is the records' own: the namespace has to be gone before the
// earliest instant retention would have let any of them go, counted from the
// deletion timestamp the API server wrote, so only the uninstall can be what
// removed them. A reading in which the namespace controller reports a refused
// deletion or content held by a finalizer ends the wait at once.
func (l *lifecycleRun) deleteTeardownNamespace(namespace string, attempt teardownAttempt) {
	l.t.Helper()
	requested := time.Now()
	deleted := &corev1.Namespace{}
	// The DELETE answers with the namespace as it now stands, carrying the
	// deletion timestamp the API server wrote, so no later read can miss it.
	l.check(l.cluster.Clientset.CoreV1().RESTClient().Delete().Resource("namespaces").Name(namespace).
		Body(&metav1.DeleteOptions{}).Do(l.ctx).Into(deleted), "delete namespace %s", namespace)
	remaining, err := teardownNamespaceWindow(deleted.DeletionTimestamp, attempt)
	if err != nil {
		l.fatalf("namespace %s: %v", namespace, err)
	}
	l.logf("teardown: namespace %s deleted at %s; its records' window ends %s later",
		namespace, deleted.DeletionTimestamp.UTC().Format(time.RFC3339), remaining.Round(time.Second))
	what := fmt.Sprintf("namespace %s, deleted at %s, to be gone before its records' window ends at %s",
		namespace, deleted.DeletionTimestamp.UTC().Format(time.RFC3339), attempt.WindowEnds.Format(time.RFC3339))
	// The window's end is the API server's time, read on the local clock:
	// kind shares it, or keeps within seconds of it on a remote Docker host.
	err = harness.Wait(l.ctx, what, time.Until(attempt.WindowEnds), teardownPoll,
		func(ctx context.Context) (bool, string, error) {
			current := &corev1.Namespace{}
			err := l.cluster.Client.Get(ctx, types.NamespacedName{Name: namespace}, current)
			read := time.Now()
			if apierrors.IsNotFound(err) {
				if late := teardownNamespaceGoneInTime(read, attempt); late != nil {
					return false, "", late
				}
				return true, "", nil
			}
			if err != nil {
				return false, err.Error(), nil
			}
			if current.UID != deleted.UID {
				return false, "", fmt.Errorf("namespace %s was replaced: UID %s, deleted %s", namespace, current.UID, deleted.UID)
			}
			if refusal, refused := teardownNamespaceRefusal(current); refused {
				return false, "", fmt.Errorf("the namespace controller reports %s", refusal)
			}
			return false, teardownNamespaceObserved(current), nil
		})
	if err != nil {
		l.diagnoseTeardown(namespace)
		l.fatalf("%v", err)
	}
	l.logf("teardown: namespace %s gone %s after its deletion was requested, %s before its records' window ended",
		namespace, time.Since(requested).Round(time.Second), time.Until(attempt.WindowEnds).Round(time.Second))
}

// assertTeardownLeftNothing reads each record the attempt left, and each
// Secret the teardown created, by name: none of them may outlive the
// namespace.
func (l *lifecycleRun) assertTeardownLeftNothing(namespace string, attempt teardownAttempt) {
	l.t.Helper()
	if len(attempt.Records) != teardownRecordCount {
		l.fatalf("the teardown examined %d records; one retired attempt leaves %d", len(attempt.Records), teardownRecordCount)
	}
	examined := 0
	for _, record := range attempt.Records {
		err := l.cluster.Client.Get(l.ctx, types.NamespacedName{Namespace: namespace, Name: record.Name}, &ptahv1alpha1.PtahResultRecord{})
		if !apierrors.IsNotFound(err) {
			l.fatalf("result record %s/%s outlived its namespace: %v", namespace, record.Name, err)
		}
		examined++
	}
	for _, name := range []string{teardownDatabaseURLSecret, teardownPullSecret} {
		err := l.cluster.Client.Get(l.ctx, types.NamespacedName{Namespace: namespace, Name: name}, &corev1.Secret{})
		if !apierrors.IsNotFound(err) {
			l.fatalf("Secret %s/%s outlived its namespace: %v", namespace, name, err)
		}
		examined++
	}
	records := &ptahv1alpha1.PtahResultRecordList{}
	l.check(l.cluster.Client.List(l.ctx, records, client.InNamespace(namespace)), "list result records in namespace %s", namespace)
	if len(records.Items) != 0 {
		l.fatalf("%d result records remain in deleted namespace %s", len(records.Items), namespace)
	}
	l.logf("teardown: %d records and Secrets read by name, none left", examined)
}

// diagnoseTeardown writes what the namespace and its records say to the
// phase's standard error before a failure, with every finalizer and deletion
// timestamp a record carries.
func (l *lifecycleRun) diagnoseTeardown(namespace string) {
	for _, arguments := range [][]string{
		{"get", "namespace", namespace, "-o", "yaml"},
		{"-n", namespace, "get", "ptahschemas,jobs,pods", "-o", "wide"},
		{"-n", namespace, "get", "ptahresultrecords", "-o",
			"custom-columns=NAME:.metadata.name,TYPE:.spec.type,CREATED:.metadata.creationTimestamp,DELETING:.metadata.deletionTimestamp,FINALIZERS:.metadata.finalizers"},
		{"-n", namespace, "get", "events", "--sort-by=.metadata.creationTimestamp"},
	} {
		stdout, stderr, err := l.kubectl(arguments...)
		_, _ = os.Stderr.Write(stdout)
		_, _ = os.Stderr.Write(stderr)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "e2e crd: kubectl %s: %v\n", strings.Join(arguments, " "), err)
		}
	}
}
