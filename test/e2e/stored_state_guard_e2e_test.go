//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

func storedStateCheck(t *testing.T, err error, action string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
}

func newStoredStateRecorder[T client.Object](t *testing.T, ctx context.Context, watcher client.WithWatch,
	name, namespace string, newList func() client.ObjectList,
) *watchRecorder[T] {
	t.Helper()
	list := newList()
	storedStateCheck(t, watcher.List(ctx, list, client.InNamespace(namespace)), "read the "+name+" collection boundary")
	if list.GetResourceVersion() == "" {
		t.Fatal("the stored-state watch list has no resourceVersion")
	}
	watchCtx, cancel := context.WithCancel(context.Background())
	r := &watchRecorder[T]{name: name, namespace: namespace, newList: newList,
		watcher: watcher, ctx: watchCtx, cancel: cancel, quiet: true, done: make(chan struct{})}
	go r.run(list.GetResourceVersion())
	t.Cleanup(r.abort)
	return r
}

func storedStateWatchBarrier[T client.Object](t *testing.T, ctx context.Context, cluster *harness.Cluster,
	r *watchRecorder[T], object T,
) string {
	t.Helper()
	storedStateCheck(t, cluster.Client.Get(ctx, client.ObjectKeyFromObject(object), object), "read the "+r.name+" sentinel")
	before := object.DeepCopyObject().(client.Object)
	annotations := maps.Clone(object.GetAnnotations())
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[annotationWatchBarrier] = string(uuid.NewUUID())
	object.SetAnnotations(annotations)
	storedStateCheck(t, cluster.Client.Patch(ctx, object, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})), "write the "+r.name+" sentinel")
	version, uid := object.GetResourceVersion(), object.GetUID()
	if version == "" || uid == "" {
		t.Fatal("the stored-state watch sentinel has no exact API identity")
	}
	storedStateCheck(t, harness.Wait(ctx, "the "+r.name+" exact watch barrier", 30*time.Second, time.Second,
		func(context.Context) (bool, string, error) {
			if err := r.alive(); err != nil {
				return false, "watch failed", err
			}
			seen := slices.ContainsFunc(r.snapshot(), func(event watchEvent[T]) bool {
				return event.Object.GetUID() == uid && event.Object.GetResourceVersion() == version
			})
			return seen, "waiting for exact sentinel UID and resourceVersion", nil
		}), "close the "+r.name+" API boundary")
	return version
}

func storedStateDeleteExact(ctx context.Context, cluster *harness.Cluster, object client.Object) error {
	if object == nil || object.GetUID() == "" {
		return nil
	}
	uid := object.GetUID()
	if err := client.IgnoreNotFound(cluster.Client.Delete(ctx, object, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})); err != nil {
		return err
	}
	return harness.Wait(ctx, "the exact stored-state control to be removed", 30*time.Second, time.Second,
		func(ctx context.Context) (bool, string, error) {
			live := object.DeepCopyObject().(client.Object)
			err := cluster.Client.Get(ctx, client.ObjectKeyFromObject(object), live)
			return apierrors.IsNotFound(err), "waiting for NotFound", client.IgnoreNotFound(err)
		})
}

type storedStateManagerLog struct {
	pod    corev1.Pod
	prefix []byte
}

func storedStateManagerLogs(ctx context.Context, cluster *harness.Cluster, user string, scan func([]byte, string)) ([]storedStateManagerLog, error) {
	if !storedStateManagerIdentity(user) {
		return nil, fmt.Errorf("the stored-state control has no installed manager identity")
	}
	parts := strings.Split(user, ":")
	pods := &corev1.PodList{}
	if err := cluster.Client.List(ctx, pods, client.InNamespace(parts[2]), client.MatchingLabels{"app.kubernetes.io/component": "controller"}); err != nil {
		return nil, err
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no installed manager Pod was available to witness the runtime guard")
	}
	var logs []storedStateManagerLog
	for _, pod := range pods.Items {
		if pod.UID == "" || pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil || pod.Spec.ServiceAccountName != parts[3] {
			return nil, fmt.Errorf("the manager log witness is not the exact running installed identity")
		}
		prefix, err := cluster.ContainerLog(ctx, pod.Namespace, pod.Name, "manager")
		if err != nil {
			return nil, err
		}
		scan(prefix, "stored-state manager log prefix")
		logs = append(logs, storedStateManagerLog{pod: pod, prefix: prefix})
	}
	return logs, nil
}

// Introduce unsupported state only after approval and startup. The current
// manager must enter its runtime guard, leave the whole state untouched and
// create no workload. The caller audits SQL before this restores supported
// state and withdraws the old approval for an explicit fresh-approval control.
func holdUnsupportedStoredState(t *testing.T, ctx context.Context, cluster *harness.Cluster,
	barrier *controllerStatusBarrier, initial client.Object, databaseService string,
	admit func() client.Object, auditRefusal func(client.Object), scan func([]byte, string),
) {
	t.Helper()
	supported, err := storedStateVersion(initial)
	storedStateCheck(t, err, "read the supported controller-state version")
	storedStateCheck(t, storedStateReady(initial, supported), "settle the stored-state control before dispatch")
	future := supported + 1
	if future <= supported {
		t.Fatal("the stored-state control has no representable future version")
	}
	kind, resource, familyLabel := storedStateFamily(initial)
	if barrier.resource != resource || barrier.namespace != initial.GetNamespace() || !storedStateManagerIdentity(barrier.user) {
		t.Fatal("the stored-state barrier does not name its exact installed resource identity")
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := barrier.resume(cleanupCtx); err != nil {
			t.Errorf("restore stored-state manager status authorization: %v", err)
		}
	})
	storedStateCheck(t, barrier.pause(ctx), "hold status writes before admitting the stored-state approval")
	parts := strings.Split(barrier.user, ":")
	managerGroups := []string{"system:serviceaccounts", "system:serviceaccounts:" + parts[2], "system:authenticated"}
	attributes := authorizationv1.ResourceAttributes{Namespace: initial.GetNamespace(), Name: initial.GetName(),
		Verb: "patch", Group: "operator.ptah.run", Resource: resource, Subresource: "status"}
	authorize := func(groups []string, attrs authorizationv1.ResourceAttributes) (bool, error) {
		review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{User: barrier.user, Groups: groups, ResourceAttributes: &attrs}}
		err := cluster.Client.Create(ctx, review)
		return review.Status.Allowed, err
	}
	allowed, err := authorize(managerGroups, attributes)
	storedStateCheck(t, err, "probe the actual manager's complete group membership before approval")
	if allowed {
		t.Fatal("the real manager retained status PATCH permission while the approval boundary was held")
	}
	approval := admit()
	if approval == nil || approval.GetUID() == "" {
		t.Fatal("the stored-state control has no exact admitted approval")
	}
	storedStateCheck(t, storedStateApprovalUnconsumed(approval, approval), "retain the exact unconsumed approval before injection")
	// Withdrawal precedes any restoration, including failure cleanup.
	var injected bool
	var writer client.Client
	live := initial.DeepCopyObject().(client.Object)
	restore := func(cleanupCtx context.Context) error {
		if err := storedStateDeleteExact(cleanupCtx, cluster, approval); err != nil {
			return err
		}
		if !injected {
			return nil
		}
		if err := cluster.Client.Get(cleanupCtx, client.ObjectKeyFromObject(initial), live); err != nil {
			return err
		}
		version, err := storedStateVersion(live)
		if err != nil || version == supported {
			return err
		}
		if err := storedStateChangedOnlyByVersion(initial, live, future); err != nil {
			return err
		}
		patch, err := storedStatePatch(live, future, supported)
		if err != nil {
			return err
		}
		return writer.Status().Patch(cleanupCtx, live, client.RawPatch(types.JSONPatchType, patch))
	}
	// Registered before scope cleanup below; explicitly restore first on the
	// successful path. Failure cleanup registered after scope setup uses it too.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := storedStateDeleteExact(cleanupCtx, cluster, approval); err != nil {
			t.Errorf("withdraw the stored-state approval: %v", err)
		}
	})
	storedStateCheck(t, cluster.Client.Get(ctx, client.ObjectKeyFromObject(initial), live), "read the held supported-state control")
	storedStateCheck(t, storedStateChangedOnlyByVersion(initial, live, supported), "retain the original approval boundary")
	initial = live.DeepCopyObject().(client.Object)
	watcher, err := client.NewWithWatch(cluster.Config, client.Options{Scheme: cluster.Scheme})
	storedStateCheck(t, err, "open direct stored-state watches")
	jobs := newStoredStateRecorder[*batchv1.Job](t, ctx, watcher, "stored-state-jobs", initial.GetNamespace(), func() client.ObjectList { return &batchv1.JobList{} })
	pods := newStoredStateRecorder[*corev1.Pod](t, ctx, watcher, "stored-state-pods", initial.GetNamespace(), func() client.ObjectList { return &corev1.PodList{} })
	var recorders = []recorder{jobs, pods}
	var closeResource func() string
	var assertHistory func(string, string) error
	switch original := initial.(type) {
	case *ptahv1alpha1.PtahSchema:
		r := newStoredStateRecorder[*ptahv1alpha1.PtahSchema](t, ctx, watcher, "stored-state-schema", initial.GetNamespace(), func() client.ObjectList { return &ptahv1alpha1.PtahSchemaList{} })
		recorders = append(recorders, r)
		closeResource = func() string { return storedStateWatchBarrier(t, ctx, cluster, r, original.DeepCopy()) }
		assertHistory = func(start, end string) error {
			return storedStateHeldHistory(r.snapshot(), original, start, end, future)
		}
	case *ptahv1alpha1.PtahMigration:
		r := newStoredStateRecorder[*ptahv1alpha1.PtahMigration](t, ctx, watcher, "stored-state-migration", initial.GetNamespace(), func() client.ObjectList { return &ptahv1alpha1.PtahMigrationList{} })
		recorders = append(recorders, r)
		closeResource = func() string { return storedStateWatchBarrier(t, ctx, cluster, r, original.DeepCopy()) }
		assertHistory = func(start, end string) error {
			return storedStateHeldHistory(r.snapshot(), original, start, end, future)
		}
	}
	name := "e2e-state-writer-" + string(uuid.NewUUID())[:12]
	group := "e2e.ptah.run/stored-state/" + string(uuid.NewUUID())
	role, binding, err := storedStateWriterObjects(initial.GetNamespace(), name, resource, initial.GetName(), group)
	storedStateCheck(t, err, "scope the stored-state injection permission")
	cleanupScope := func(cleanupCtx context.Context) error {
		if err := storedStateDeleteExact(cleanupCtx, cluster, binding); err != nil {
			return err
		}
		return storedStateDeleteExact(cleanupCtx, cluster, role)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := cleanupScope(cleanupCtx); err != nil {
			t.Errorf("remove the exact stored-state writer permission: %v", err)
		}
	})
	storedStateCheck(t, cluster.Client.Create(ctx, role), "create the name-scoped status injection Role")
	storedStateCheck(t, cluster.Client.Create(ctx, binding), "bind status injection only to the private test group")
	writerGroups := append(slices.Clone(managerGroups), group)
	storedStateCheck(t, harness.Wait(ctx, "the exact private-group injection permission", 30*time.Second, time.Second,
		func(context.Context) (bool, string, error) {
			allowed, err := authorize(writerGroups, attributes)
			return allowed, "waiting for scoped status PATCH authorization", err
		}), "admit the scoped writer identity")
	allowed, err = authorize(managerGroups, attributes)
	storedStateCheck(t, err, "probe the actual manager's held status authorization")
	if allowed {
		t.Fatal("the temporary writer also granted the actual manager a status write")
	}
	other := attributes
	other.Name += "-other"
	allowed, err = authorize(writerGroups, other)
	storedStateCheck(t, err, "probe the scoped writer on another resource name")
	if allowed {
		t.Fatal("the injection group acquired permission outside its exact resource")
	}
	writer, err = cluster.As(rest.ImpersonationConfig{UserName: barrier.user, Groups: writerGroups})
	storedStateCheck(t, err, "retain installed status admission while impersonating the manager")
	ordinary, err := cluster.As(rest.ImpersonationConfig{UserName: name, Groups: []string{group}})
	storedStateCheck(t, err, "build the ordinary writer denial control")
	patch, err := storedStatePatch(live, supported, future)
	storedStateCheck(t, err, "bind the injection to the held API version")
	err = ordinary.Status().Patch(ctx, live.DeepCopyObject().(client.Object), client.RawPatch(types.JSONPatchType, patch), &client.SubResourcePatchOptions{PatchOptions: client.PatchOptions{DryRun: []string{metav1.DryRunAll}}})
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "Ptah status is written only by the operator's manager") {
		t.Fatal("installed status admission did not refuse the otherwise authorized ordinary writer")
	}
	managerLogs, err := storedStateManagerLogs(ctx, cluster, barrier.user, scan)
	storedStateCheck(t, err, "capture the installed managers before unsupported state arrives")
	storedStateCheck(t, writer.Status().Patch(ctx, live, client.RawPatch(types.JSONPatchType, patch)), "introduce exactly one unsupported controller-state field")
	injected = true
	start := live.GetResourceVersion()
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := restore(cleanupCtx); err != nil {
			t.Errorf("withdraw approval and restore only the injected state field: %v", err)
		}
	})
	storedStateCheck(t, storedStateChangedOnlyByVersion(initial, live, future), "retain every field except the controlled future version")
	storedStateCheck(t, barrier.resume(ctx), "let the installed manager enter its runtime state guard")
	ids := map[string]bool{}
	storedStateCheck(t, harness.Wait(ctx, "repeated runtime controller-state refusals", 90*time.Second, time.Second,
		func(ctx context.Context) (bool, string, error) {
			for _, r := range recorders {
				if err := r.alive(); err != nil {
					return false, "watch failed", err
				}
			}
			if err := cluster.Client.Get(ctx, client.ObjectKeyFromObject(initial), live); err != nil {
				return false, "read held state", err
			}
			if err := storedStateChangedOnlyByVersion(initial, live, future); err != nil {
				return false, "unsupported state changed", err
			}
			currentApproval := approval.DeepCopyObject().(client.Object)
			if err := cluster.Client.Get(ctx, client.ObjectKeyFromObject(approval), currentApproval); err != nil {
				return false, "read old approval", err
			}
			if err := storedStateApprovalUnconsumed(approval, currentApproval); err != nil {
				return false, "old approval changed or consumed", err
			}
			for _, witness := range managerLogs {
				pod := &corev1.Pod{}
				if err := cluster.Client.Get(ctx, client.ObjectKeyFromObject(&witness.pod), pod); err != nil {
					return false, "read manager log identity", err
				}
				if pod.UID != witness.pod.UID || pod.DeletionTimestamp != nil {
					return false, "manager replaced", fmt.Errorf("the state guard witness lost its manager Pod")
				}
				logs, err := cluster.ContainerLog(ctx, pod.Namespace, pod.Name, "manager")
				if err != nil {
					return false, "read manager runtime refusal", err
				}
				scan(logs, "complete stored-state manager log")
				if !bytes.HasPrefix(logs, witness.prefix) {
					return false, "manager journal replaced", fmt.Errorf("the runtime guard lost its original log prefix")
				}
				found, err := storedStateGuardReconciliations(logs[len(witness.prefix):], initial.GetNamespace(), initial.GetName(), kind, future, supported)
				if err != nil {
					return false, "parse exact runtime refusals", err
				}
				maps.Copy(ids, found)
			}
			return len(ids) >= 2, fmt.Sprintf("exact refused reconciliation IDs=%d", len(ids)), nil
		}), "observe actual runtime refusal and unchanged complete state")
	jobList := &batchv1.JobList{}
	storedStateCheck(t, cluster.Client.List(ctx, jobList, client.InNamespace(initial.GetNamespace())), "find an unmanaged completed Job watch sentinel")
	var sentinel *batchv1.Job
	for _, job := range jobList.Items {
		if job.Labels[labelManagedBy] != managedByOperator && len(job.OwnerReferences) == 0 && job.UID != "" &&
			job.Spec.TTLSecondsAfterFinished == nil && jobTerminal(&job) {
			sentinel = job.DeepCopy()
			break
		}
	}
	if sentinel == nil {
		t.Fatal("the stored-state Job collection has no unmanaged completed sentinel")
	}
	storedStateWatchBarrier(t, ctx, cluster, jobs, sentinel)
	databases := &corev1.PodList{}
	storedStateCheck(t, cluster.Client.List(ctx, databases, client.InNamespace(initial.GetNamespace()),
		client.MatchingLabels{"app.kubernetes.io/name": databaseService}, client.MatchingFields{"status.phase": string(corev1.PodRunning)}), "read the database Pod watch sentinel")
	database, err := migrationExecutorPodBarrierSource(databases.Items, initial.GetNamespace(), databaseService)
	storedStateCheck(t, err, "bind the unmanaged database Pod sentinel")
	storedStateWatchBarrier(t, ctx, cluster, pods, database)
	end := closeResource()
	for _, r := range recorders {
		r.requestStop()
	}
	deadline := time.Now().Add(35 * time.Second)
	for _, r := range recorders {
		storedStateCheck(t, r.await(time.Until(deadline)), "close the "+r.stem()+" history at natural EOF")
		history, count, err := r.history()
		storedStateCheck(t, err, "encode the "+r.stem()+" complete history")
		if count == 0 {
			t.Fatal("the stored-state collection watch recorded nothing")
		}
		scan(history, "stored-state closed collection history")
	}
	storedStateCheck(t, assertHistory(start, end), "preserve the complete unsupported-state resource window")
	if !storedStateCreatedNoWork(jobs.snapshot(), pods.snapshot(), initial, familyLabel) {
		t.Fatal("the unsupported-state manager created a workload in the complete Job/Pod history")
	}
	auditRefusal(initial)
	storedStateCheck(t, restore(ctx), "withdraw the old approval before restoring supported state")
	storedStateCheck(t, cleanupScope(ctx), "remove both exact temporary RBAC objects")
	storedStateCheck(t, harness.Wait(ctx, "the temporary group's status permission to be removed", 30*time.Second, time.Second,
		func(ctx context.Context) (bool, string, error) {
			review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{User: name, Groups: []string{group}, ResourceAttributes: &attributes}}
			err := cluster.Client.Create(ctx, review)
			return !review.Status.Allowed, "waiting for the removed group to lose its exact status PATCH", err
		}), "remove the temporary writer's effective permission")
	t.Logf("controller-state runtime refusal: kind=%s namespace=%s name=%s supported=%d refused=%d reconciliationIDs=%d; no new workload; original state retained; temporary Role/RoleBinding removed", kind, initial.GetNamespace(), initial.GetName(), supported, future, len(ids))
}
