// Package resultstore_test verifies publication against a real API server.
// No receiver, family controller, or Kubernetes garbage collector runs here.
package resultstore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	recordapi "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"github.com/stokaro/ptah-operator/test/envtest/internal/harness"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

var (
	plane  = harness.New(&envtest.Environment{CRDDirectoryPaths: []string{harness.CRDDirectory()}})
	scheme = runtime.NewScheme()
	api    client.Client
)

func TestMain(m *testing.M) {
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := recordapi.AddToScheme(scheme); err != nil {
		panic(err)
	}
	plane.Environment.Scheme = scheme
	os.Exit(plane.Main(m, func() error {
		var err error
		api, err = client.New(plane.Config, client.Options{Scheme: scheme})
		return err
	}))
}

func newBinding(t *testing.T) resultstore.Binding {
	t.Helper()
	plane.Require(t)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "results-"}}
	if err := api.Create(t.Context(), ns); err != nil {
		t.Fatal(err)
	}
	return resultstore.Binding{Namespace: ns.Name, Kind: "PtahSchema", Name: "schema", UID: "schema-uid",
		Generation: 1, ExecutionBindingID: "v1-" + strings.Repeat("a", 32), InputFingerprint: "sha256:" + strings.Repeat("b", 64),
		Operation: "plan", OperationID: "plan-1", JobName: "plan-job", JobUID: "job-uid", PodName: "plan-pod", PodUID: "pod-uid"}
}

func digest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestMaximumPayloadAndRecordImmutability(t *testing.T) {
	b := newBinding(t)
	// Exercise every storage chunk at the complete payload bound. Protocol
	// validity is the receiver's separate obligation; this is arbitrary binary.
	payload := bytes.Repeat([]byte{0, '\n', '\\', '<', 0xff}, int(resultstore.MaxPayloadBytes)/5+1)[:resultstore.MaxPayloadBytes]
	s := resultstore.Store{Client: api, Reader: api}
	receipt, err := s.Publish(t.Context(), b, payload, digest(payload))
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.New(plane.Config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	got, readReceipt, err := (resultstore.Store{Reader: second}).Load(t.Context(), b)
	if err != nil || !bytes.Equal(got, payload) || readReceipt != receipt {
		t.Fatalf("fresh API client lost result: %v", err)
	}
	list := &recordapi.PtahResultRecordList{}
	if err := api.List(t.Context(), list, client.InNamespace(b.Namespace)); err != nil {
		t.Fatal(err)
	}
	wantChunks := int((resultstore.MaxPayloadBytes + resultstore.ChunkBytes - 1) / resultstore.ChunkBytes)
	if len(list.Items) != wantChunks+2 {
		t.Fatalf("stored %d records, want %d", len(list.Items), wantChunks+2)
	}
	for _, original := range list.Items {
		obj := original.DeepCopy()
		obj.Spec.Data[0] ^= 1
		err := api.Update(t.Context(), obj)
		if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "immutable") {
			t.Fatalf("Record %s permitted payload mutation: %v", obj.Name, err)
		}
	}
	if _, err := s.Publish(t.Context(), b, append(bytes.Clone(payload), 'x'), "sha256:"+strings.Repeat("0", 64)); !errors.Is(err, resultstore.ErrInvalid) {
		t.Fatalf("oversize accepted: %v", err)
	}
}

type failWrite struct {
	client.Client
	at, calls int
	after     bool
}

var errWrite = errors.New("injected API write failure")

func (c *failWrite) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.calls++
	if c.calls == c.at && !c.after {
		return errWrite
	}
	err := c.Client.Create(ctx, obj, opts...)
	if err == nil && c.calls == c.at {
		return errWrite
	}
	return err
}

func TestRealPublicationCrashBoundaries(t *testing.T) {
	plane.Require(t)
	payload := bytes.Repeat([]byte("p"), resultstore.ChunkBytes+1)
	for at := 1; at <= 4; at++ {
		for _, after := range []bool{false, true} {
			t.Run(fmt.Sprintf("write-%d/after-%t", at, after), func(t *testing.T) {
				b := newBinding(t)
				broken := resultstore.Store{Client: &failWrite{Client: api, at: at, after: after}, Reader: api}
				if receipt, err := broken.Publish(t.Context(), b, payload, digest(payload)); !errors.Is(err, errWrite) || receipt != (resultstore.Receipt{}) {
					t.Fatalf("failed write acknowledged: %v", err)
				}
				s := resultstore.Store{Client: api, Reader: api}
				_, _, err := s.Load(t.Context(), b)
				if at == 4 && after {
					if err != nil {
						t.Fatalf("completed write unreadable: %v", err)
					}
				} else if !errors.Is(err, resultstore.ErrIncomplete) {
					t.Fatalf("partial write readable: %v", err)
				}
				receipt, err := s.Publish(t.Context(), b, payload, digest(payload))
				if err != nil {
					t.Fatal(err)
				}
				retry, err := s.Publish(t.Context(), b, payload, digest(payload))
				if err != nil || retry != receipt {
					t.Fatalf("redelivery changed receipt: %v", err)
				}
			})
		}
	}
}

func TestConcurrentConflictsUseOneDurableReceipt(t *testing.T) {
	b := newBinding(t)
	s := resultstore.Store{Client: api, Reader: api}
	var wg sync.WaitGroup
	errs := make([]error, 8)
	receipts := make([]resultstore.Receipt, 8)
	for i := range errs {
		wg.Go(func() {
			payload := []byte(fmt.Sprintf("result-%d", i%2))
			receipts[i], errs[i] = s.Publish(t.Context(), b, payload, digest(payload))
		})
	}
	wg.Wait()
	got, receipt, err := s.Load(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	accepted, conflicts := 0, 0
	for i, err := range errs {
		if err == nil {
			accepted++
			if receipts[i] != receipt || string(got) != fmt.Sprintf("result-%d", i%2) {
				t.Fatal("conflicting delivery acknowledged")
			}
		} else if errors.Is(err, resultstore.ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if accepted != 4 || conflicts != 4 {
		t.Fatalf("accepted=%d conflicts=%d", accepted, conflicts)
	}
}

func TestResultNeedsNeitherJobNorPodToLoad(t *testing.T) {
	b := newBinding(t)
	podSpec := corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "runner", Image: "fixture.invalid/runner"}}}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: b.Namespace, Name: b.JobName}, Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: podSpec}}}
	if err := api.Create(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: b.Namespace, Name: b.PodName}, Spec: podSpec}
	if err := api.Create(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	b.JobUID, b.PodUID = job.UID, pod.UID
	s := resultstore.Store{Client: api, Reader: api}
	payload := []byte("durable result")
	receipt, err := s.Publish(t.Context(), b, payload, digest(payload))
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range []client.Object{job, pod} {
		uid := obj.GetUID()
		// Delete each object directly. envtest has no garbage collector to
		// finish a Job's default orphan propagation finalizer.
		if err := api.Delete(t.Context(), obj, client.GracePeriodSeconds(0),
			client.PropagationPolicy(metav1.DeletePropagationBackground), client.Preconditions{UID: &uid}); err != nil {
			t.Fatal(err)
		}
		if err := api.Get(t.Context(), client.ObjectKeyFromObject(obj), obj); !apierrors.IsNotFound(err) {
			t.Fatalf("object still exists: %v", err)
		}
	}
	fresh, err := client.New(plane.Config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	got, loaded, err := (resultstore.Store{Reader: fresh}).Load(t.Context(), b)
	if err != nil || !bytes.Equal(got, payload) || loaded != receipt {
		t.Fatalf("deleted producer lost result: %v", err)
	}
}

func TestReplacementChunkUIDIsNotAccepted(t *testing.T) {
	b := newBinding(t)
	s := resultstore.Store{Client: api, Reader: api}
	payload := []byte("same bytes")
	if _, err := s.Publish(t.Context(), b, payload, digest(payload)); err != nil {
		t.Fatal(err)
	}
	name, _ := resultstore.Name(b)
	part := &recordapi.PtahResultRecord{}
	if err := api.Get(t.Context(), types.NamespacedName{Namespace: b.Namespace, Name: name + "-000"}, part); err != nil {
		t.Fatal(err)
	}
	oldUID := part.UID
	if err := api.Delete(t.Context(), part, client.Preconditions{UID: &oldUID}); err != nil {
		t.Fatal(err)
	}
	part.UID = ""
	part.ResourceVersion = ""
	part.CreationTimestamp = metav1.Time{}
	part.ManagedFields = nil
	if err := api.Create(t.Context(), part); err != nil {
		t.Fatal(err)
	}
	if part.UID == oldUID {
		t.Fatal("test did not replace chunk identity")
	}
	if got, receipt, err := s.Load(t.Context(), b); !errors.Is(err, resultstore.ErrConflict) || len(got) != 0 || receipt != (resultstore.Receipt{}) {
		t.Fatalf("replacement accepted: %v", err)
	}
}

// The result API must not require the permission that reads database Secrets.
// This test uses a real RBAC identity with only get/create on result records,
// not the administrator reader used by the persistence fault cases above.
func TestPublicationWithNoSecretReadPermission(t *testing.T) {
	b := newBinding(t)
	ctx := t.Context()
	const username = "result-record-writer"
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: b.Namespace, Name: "result-writer"}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{recordapi.GroupVersion.Group}, Resources: []string{"ptahresultrecords"}, Verbs: []string{"get", "create"}}}}
	if err := api.Create(ctx, role); err != nil {
		t.Fatal(err)
	}
	grant := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: b.Namespace, Name: role.Name}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name}, Subjects: []rbacv1.Subject{{Kind: "User", APIGroup: rbacv1.GroupName, Name: username}}}
	if err := api.Create(ctx, grant); err != nil {
		t.Fatal(err)
	}
	databaseSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: b.Namespace, Name: "database"}, Data: map[string][]byte{"password": []byte("test-only")}}
	if err := api.Create(ctx, databaseSecret); err != nil {
		t.Fatal(err)
	}
	restricted, err := client.New(plane.Impersonate(username), client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := restricted.Get(ctx, client.ObjectKeyFromObject(databaseSecret), &corev1.Secret{}); !apierrors.IsForbidden(err) {
		t.Fatalf("database Secret read was not forbidden: %v", err)
	}
	payload := bytes.Repeat([]byte("durable result"), resultstore.ChunkBytes/13+1)
	store := resultstore.Store{Client: restricted, Reader: restricted}
	receipt, err := store.Publish(ctx, b, payload, digest(payload))
	if err != nil {
		t.Fatal(err)
	}
	loaded, readReceipt, err := store.Load(ctx, b)
	if err != nil || !bytes.Equal(loaded, payload) || readReceipt != receipt {
		t.Fatalf("restricted result read failed: %v", err)
	}
	if err := restricted.Get(ctx, client.ObjectKeyFromObject(databaseSecret), &corev1.Secret{}); !apierrors.IsForbidden(err) {
		t.Fatalf("publication granted Secret access: %v", err)
	}
}
