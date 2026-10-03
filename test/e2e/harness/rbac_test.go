package harness

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type delayedPermissionWriter struct {
	client.Client
	calls int
	write func(context.Context, client.Object, ...client.CreateOption) error
}

func (w *delayedPermissionWriter) Create(ctx context.Context, object client.Object, opts ...client.CreateOption) error {
	w.calls++
	return w.write(ctx, object, opts...)
}

func forbiddenCreate() error {
	return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "proof", errors.New("the new group has no permission yet"))
}

func TestCreateAfterRoleBindingKeepsTheRequest(t *testing.T) {
	object := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "work", Name: "proof"}, Data: map[string]string{"value": "unchanged"}}
	original := object.DeepCopy()
	writer := &delayedPermissionWriter{}
	writer.write = func(_ context.Context, got client.Object, opts ...client.CreateOption) error {
		if got != object || !reflect.DeepEqual(got, original) {
			t.Fatal("retry changed the requested object")
		}
		options := &client.CreateOptions{}
		for _, opt := range opts {
			opt.ApplyToCreate(options)
		}
		if options.FieldManager != FieldOwner || options.FieldValidation != "Strict" {
			t.Fatal("retry lost the admission options")
		}
		if writer.calls == 1 {
			return forbiddenCreate()
		}
		got.SetUID("created-once")
		return nil
	}
	if err := CreateAfterRoleBinding(t.Context(), writer, object, client.FieldOwner(FieldOwner), client.FieldValidation("Strict")); err != nil {
		t.Fatal(err)
	}
	if writer.calls != 2 || object.UID != "created-once" {
		t.Fatal("the successful request was repeated or not returned")
	}
}

func TestCreateAfterRoleBindingDoesNotRetryAmbiguousWrites(t *testing.T) {
	for _, cause := range []error{
		errors.New("response lost after persistence"),
		context.DeadlineExceeded,
		apierrors.NewInternalError(errors.New("API failure")),
		apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, "proof"),
		apierrors.NewBadRequest("invalid acknowledgment"),
	} {
		t.Run(cause.Error(), func(t *testing.T) {
			writer := &delayedPermissionWriter{write: func(context.Context, client.Object, ...client.CreateOption) error { return cause }}
			if err := CreateAfterRoleBinding(t.Context(), writer, &corev1.ConfigMap{}); !errors.Is(err, cause) || writer.calls != 1 {
				t.Fatalf("error was retried or lost: calls=%d error=%v", writer.calls, err)
			}
		})
	}
}

func TestCreateAfterRoleBindingDoesNotHideMissingPermission(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	writer := &delayedPermissionWriter{write: func(context.Context, client.Object, ...client.CreateOption) error { return forbiddenCreate() }}
	err := CreateAfterRoleBinding(ctx, writer, &corev1.ConfigMap{})
	if !errors.Is(err, context.DeadlineExceeded) || writer.calls != 1 {
		t.Fatalf("permanent refusal did not respect the bound: calls=%d error=%v", writer.calls, err)
	}
}
