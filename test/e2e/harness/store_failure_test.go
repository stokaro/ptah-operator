package harness

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	apiwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// The first entry is the error master's schema-faults job failed on.
func storeFailures() []error {
	return []error{
		apierrors.NewInternalError(errors.New("etcdserver: request timed out, possibly due to previous leader failure")),
		apierrors.NewInternalError(errors.New("etcdserver: request timed out")),
		apierrors.NewInternalError(errors.New("etcdserver: leader changed")),
	}
}

func TestIsStoreFailure(t *testing.T) {
	t.Parallel()
	for _, err := range storeFailures() {
		if !isStoreFailure(err) {
			t.Errorf("%v was not read as a store failure", err)
		}
	}
	jobs := schema.GroupResource{Group: "batch", Resource: "jobs"}
	for name, err := range map[string]error{
		"no error":                    nil,
		"the context ending":          context.DeadlineExceeded,
		"a plain error naming etcd":   errors.New("etcdserver: request timed out"),
		"another internal error":      apierrors.NewInternalError(errors.New("admission webhook failed")),
		"a timeout that is not etcd":  apierrors.NewServerTimeout(jobs, "patch", 1),
		"a conflict":                  apierrors.NewConflict(jobs, "j", errors.New("etcdserver: request timed out")),
		"a refusal":                   apierrors.NewForbidden(jobs, "j", errors.New("etcdserver: request timed out")),
		"an object that is not there": apierrors.NewNotFound(jobs, "j"),
	} {
		if isStoreFailure(err) {
			t.Errorf("%s was read as a store failure", name)
		}
	}
}

// failingClient answers each Patch with the next error, then lets it through,
// and counts how many times it was asked.
func failingClient(t *testing.T, failures ...error) (client.Client, *int) {
	t.Helper()
	object := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "n", Name: "c"}}
	attempts := 0
	direct := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(object).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, target client.Object, patch client.Patch, options ...client.PatchOption) error {
			attempts++
			if attempts <= len(failures) {
				return failures[attempts-1]
			}
			return c.Patch(ctx, target, patch, options...)
		},
	}).Build()
	return withStoreFailureRetries(direct, apiwait.Backoff{Steps: 4, Duration: time.Millisecond}), &attempts
}

func patchAnnotation(c client.Client) (*corev1.ConfigMap, error) {
	object := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "n", Name: "c"}}
	err := c.Patch(context.Background(), object, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"k":"v"}}}`)))
	return object, err
}

func TestAPatchSurvivesAStoreFailure(t *testing.T) {
	t.Parallel()
	c, attempts := failingClient(t, storeFailures()[0], storeFailures()[1])
	object, err := patchAnnotation(c)
	if err != nil {
		t.Fatalf("the patch failed: %v", err)
	}
	if *attempts != 3 {
		t.Errorf("it took %d attempts, want 3", *attempts)
	}
	if object.Annotations["k"] != "v" || object.ResourceVersion == "" {
		t.Errorf("the object is not what the API server returned: %+v", object.ObjectMeta)
	}
}

func TestAPatchStopsAtAnyOtherError(t *testing.T) {
	t.Parallel()
	refusal := apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "c", errors.New("denied"))
	c, attempts := failingClient(t, refusal)
	if _, err := patchAnnotation(c); !apierrors.IsForbidden(err) {
		t.Errorf("the patch returned %v, want the refusal", err)
	}
	if *attempts != 1 {
		t.Errorf("a refusal was retried: %d attempts", *attempts)
	}
}

func TestAPatchGivesUpWhenTheStoreStaysDown(t *testing.T) {
	t.Parallel()
	failure := storeFailures()[0]
	c, attempts := failingClient(t, failure, failure, failure, failure, failure, failure)
	if _, err := patchAnnotation(c); !isStoreFailure(err) {
		t.Errorf("the patch returned %v, want the store failure", err)
	}
	if *attempts != 4 {
		t.Errorf("it took %d attempts, want the backoff's 4", *attempts)
	}
}

func TestOtherWritesAreNotRetried(t *testing.T) {
	t.Parallel()
	attempts := 0
	direct := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			attempts++
			return storeFailures()[0]
		},
	}).Build()
	c := withStoreFailureRetries(direct, apiwait.Backoff{Steps: 4, Duration: time.Millisecond})
	if err := c.Create(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "n", Name: "c"}}); !isStoreFailure(err) {
		t.Errorf("the create returned %v", err)
	}
	if attempts != 1 {
		t.Errorf("a create was retried: %d attempts", attempts)
	}
}
