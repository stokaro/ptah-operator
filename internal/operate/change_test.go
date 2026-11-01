package operate_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/operate"
)

// stored is a resource with something in every part a change must leave
// alone: labels, other annotations, the rest of the spec, and status.
func stored(family operate.Family, uid types.UID, suspend bool, annotations map[string]string) client.Object {
	meta := metav1.ObjectMeta{
		Namespace: namespace, Name: name, UID: uid, Generation: 7,
		Labels:      map[string]string{"app.kubernetes.io/part-of": "shop"},
		Annotations: annotations,
	}
	target := operatorv1alpha1.DatabaseTargetSpec{Engine: operatorv1alpha1.DatabaseEnginePostgreSQL}
	target.URLFrom.Name, target.URLFrom.Key = "storefront-db", "url"
	source := operatorv1alpha1.SourceSpec{OCIRef: "registry.example/shop/schema@sha256:" + strings.Repeat("a", 64)}
	interval := metav1.Duration{Duration: 10 * time.Minute}
	if family == operate.FamilyMigration {
		return &operatorv1alpha1.PtahMigration{
			ObjectMeta: meta,
			Spec: operatorv1alpha1.PtahMigrationSpec{
				Target: target, Artifact: source, Interval: interval, Suspend: suspend,
				Policy: operatorv1alpha1.MigrationPolicy{Apply: operatorv1alpha1.ApplyPolicyOnApproval},
			},
			Status: operatorv1alpha1.PtahMigrationStatus{ObservedGeneration: 6, Phase: "Applying",
				ActiveOperation: &operatorv1alpha1.MigrationOperationStatus{Type: "Apply", ID: "attempt", JobName: "job", Attempt: 1}},
		}
	}
	return &operatorv1alpha1.PtahSchema{
		ObjectMeta: meta,
		Spec: operatorv1alpha1.PtahSchemaSpec{
			Target: target, Desired: source, Interval: interval, Suspend: suspend,
			Policy: operatorv1alpha1.ReconciliationPolicy{Apply: operatorv1alpha1.ApplyPolicyOnApproval},
		},
		Status: operatorv1alpha1.PtahSchemaStatus{ObservedGeneration: 6, Phase: "Applying",
			ActiveOperation: &operatorv1alpha1.ActiveOperationStatus{Type: "Apply", ID: "attempt", JobName: "job"}},
	}
}

// recorder keeps every patch sent and lets a row act on the API server
// before one is applied.
type recorder struct {
	mu      sync.Mutex
	patches []sentPatch
	before  func(context.Context, client.WithWatch, int) error
}

type sentPatch struct {
	patchType types.PatchType
	body      []byte
	manager   string
}

func (r *recorder) client(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithStatusSubresource(&operatorv1alpha1.PtahSchema{}, &operatorv1alpha1.PtahMigration{}).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				body, err := patch.Data(obj)
				if err != nil {
					return err
				}
				options := &client.PatchOptions{}
				options.ApplyOptions(opts)
				r.mu.Lock()
				r.patches = append(r.patches, sentPatch{patchType: patch.Type(), body: body, manager: options.FieldManager})
				count := len(r.patches)
				r.mu.Unlock()
				if r.before != nil {
					if err := r.before(ctx, c, count); err != nil {
						return err
					}
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).Build()
}

func (r *recorder) sent() []sentPatch {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sentPatch(nil), r.patches...)
}

func decodeOperations(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var operations []map[string]any
	if err := json.Unmarshal(body, &operations); err != nil {
		t.Fatalf("patch %s: %v", body, err)
	}
	return operations
}

func read(t *testing.T, c client.Reader, family operate.Family) client.Object {
	t.Helper()
	object := stored(family, "", false, nil)
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, object); err != nil {
		t.Fatal(err)
	}
	return object
}

// withoutSuspend is the object with spec.suspend cleared and the fields the
// API server moves on every write zeroed, for comparing what else changed.
func withoutSuspend(object client.Object) client.Object {
	copied := object.DeepCopyObject().(client.Object)
	copied.SetResourceVersion("")
	switch typed := copied.(type) {
	case *operatorv1alpha1.PtahSchema:
		typed.Spec.Suspend = false
	case *operatorv1alpha1.PtahMigration:
		typed.Spec.Suspend = false
	}
	return copied
}

func TestSetSuspendSendsOneTestedFieldChange(t *testing.T) {
	t.Parallel()

	for _, family := range families {
		for _, suspend := range []bool{true, false} {
			t.Run(string(family)+"/"+map[bool]string{true: "suspend", false: "resume"}[suspend], func(t *testing.T) {
				t.Parallel()

				rec := &recorder{}
				c := rec.client(t, stored(family, uidA, !suspend, map[string]string{"team": "shop"}))
				before := read(t, c, family)

				result, err := operate.SetSuspend(context.Background(), c, refOf(family), suspend)
				if err != nil {
					t.Fatal(err)
				}
				sent := rec.sent()
				if len(sent) != 1 || sent[0].patchType != types.JSONPatchType || sent[0].manager != operate.FieldManager {
					t.Fatalf("sent %+v", sent)
				}
				want := []map[string]any{
					{"op": "test", "path": "/metadata/uid", "value": string(uidA)},
					{"op": "add", "path": "/spec/suspend", "value": suspend},
				}
				if got := decodeOperations(t, sent[0].body); !reflect.DeepEqual(got, want) {
					t.Fatalf("patch = %v, want %v", got, want)
				}
				after := read(t, c, family)
				// The generation is the one the answer carried. The fake API
				// server does not move it; test/envtest/crd proves a real one
				// does.
				if !result.Changed || result.Suspend != suspend || result.UID != uidA || result.Generation != after.GetGeneration() ||
					result.ObservedGeneration != 6 || result.Phase != "Applying" || result.Running != "Apply" || result.Object == nil {
					t.Fatalf("result = %+v", result)
				}
				if got, _ := operateSuspend(after); got != suspend {
					t.Fatalf("spec.suspend = %v, want %v", got, suspend)
				}
				if !reflect.DeepEqual(withoutSuspend(before), withoutSuspend(after)) {
					t.Fatalf("the change reached more than spec.suspend:\nbefore %+v\nafter  %+v", before, after)
				}
			})
		}
	}
}

func operateSuspend(object client.Object) (bool, bool) {
	switch typed := object.(type) {
	case *operatorv1alpha1.PtahSchema:
		return typed.Spec.Suspend, true
	case *operatorv1alpha1.PtahMigration:
		return typed.Spec.Suspend, true
	}
	return false, false
}

func TestSetSuspendSendsNothingWhenTheSpecAlreadySaysSo(t *testing.T) {
	t.Parallel()

	for _, family := range families {
		for _, suspend := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/suspend=%t", family, suspend), func(t *testing.T) {
				t.Parallel()

				rec := &recorder{}
				c := rec.client(t, stored(family, uidA, suspend, nil))
				result, err := operate.SetSuspend(context.Background(), c, refOf(family), suspend)
				if err != nil {
					t.Fatal(err)
				}
				if result.Changed || result.Suspend != suspend || result.Generation != 7 || result.ObservedGeneration != 6 || len(rec.sent()) != 0 {
					t.Fatalf("result = %+v after %d patches, want no change and no patch", result, len(rec.sent()))
				}
			})
		}
	}
}

func TestSetSuspendRefusesAResourceReplacedSinceItWasRead(t *testing.T) {
	t.Parallel()

	for _, family := range families {
		t.Run(string(family), func(t *testing.T) {
			t.Parallel()

			rec := &recorder{}
			// Between the read and the patch the resource is deleted and
			// created again under the same name.
			rec.before = func(ctx context.Context, c client.WithWatch, _ int) error {
				if err := c.Delete(ctx, stored(family, uidA, false, nil)); err != nil {
					return err
				}
				return c.Create(ctx, stored(family, uidB, false, nil))
			}
			c := rec.client(t, stored(family, uidA, false, nil))
			_, err := operate.SetSuspend(context.Background(), c, refOf(family), true)
			if !errors.Is(err, operate.ErrReplaced) {
				t.Fatalf("err = %v, want ErrReplaced", err)
			}
			replacement := read(t, c, family)
			if suspend, _ := operateSuspend(replacement); suspend || replacement.GetUID() != uidB {
				t.Fatalf("the replacement was changed: uid %s, suspend %v", replacement.GetUID(), suspend)
			}
		})
	}
}

func TestChangesReportTheAPIServersRefusal(t *testing.T) {
	t.Parallel()

	for _, family := range families {
		t.Run(string(family)+"/not found", func(t *testing.T) {
			t.Parallel()

			rec := &recorder{}
			c := rec.client(t)
			if _, err := operate.SetSuspend(context.Background(), c, refOf(family), true); !apierrors.IsNotFound(err) ||
				!strings.Contains(err.Error(), "storefront in namespace demo") {
				t.Fatalf("suspend: err = %v", err)
			}
			if _, err := operate.Refresh(context.Background(), c, refOf(family), clockStart); !apierrors.IsNotFound(err) {
				t.Fatalf("refresh: err = %v", err)
			}
		})
		t.Run(string(family)+"/forbidden", func(t *testing.T) {
			t.Parallel()

			rec := &recorder{}
			rec.before = func(context.Context, client.WithWatch, int) error {
				return apierrors.NewForbidden(resource, name, errors.New(`User "dev" cannot patch resource "ptahschemas"`))
			}
			c := rec.client(t, stored(family, uidA, false, nil))
			for operation, call := range map[string]func() error{
				"suspend": func() error { _, err := operate.SetSuspend(context.Background(), c, refOf(family), true); return err },
				"refresh": func() error {
					_, err := operate.Refresh(context.Background(), c, refOf(family), clockStart)
					return err
				},
			} {
				err := call()
				if !apierrors.IsForbidden(err) || errors.Is(err, operate.ErrReplaced) ||
					!strings.Contains(err.Error(), `User "dev" cannot patch resource "ptahschemas"`) {
					t.Fatalf("%s: err = %v", operation, err)
				}
			}
		})
	}
}

func TestRefreshWritesADistinctRequestAndNothingElse(t *testing.T) {
	t.Parallel()

	key := operatorv1alpha1.ReadRequestAnnotation
	for _, family := range families {
		t.Run(string(family), func(t *testing.T) {
			t.Parallel()

			rec := &recorder{}
			c := rec.client(t, stored(family, uidA, false, map[string]string{"team": "shop"}))
			before := read(t, c, family)
			now := time.Date(2026, 11, 1, 12, 30, 45, 123456789, time.FixedZone("CET", 3600))

			first, err := operate.Refresh(context.Background(), c, refOf(family), now)
			if err != nil {
				t.Fatal(err)
			}
			if first.Request != "2026-11-01T11:30:45.123456789Z" || first.Previous != "" || first.UID != uidA {
				t.Fatalf("first = %+v", first)
			}
			if _, err := time.Parse(time.RFC3339, first.Request); err != nil {
				t.Fatalf("the controller would not read %q as an instant: %v", first.Request, err)
			}
			want := []map[string]any{
				{"op": "test", "path": "/metadata/uid", "value": string(uidA)},
				{"op": "add", "path": "/metadata/annotations/operator.ptah.run~1read-requested-at", "value": first.Request},
			}
			if got := decodeOperations(t, rec.sent()[0].body); !reflect.DeepEqual(got, want) || rec.sent()[0].manager != operate.FieldManager {
				t.Fatalf("patch = %v by %q, want %v", got, rec.sent()[0].manager, want)
			}
			after := read(t, c, family)
			if after.GetAnnotations()[key] != first.Request || after.GetAnnotations()["team"] != "shop" {
				t.Fatalf("annotations = %v", after.GetAnnotations())
			}
			unchanged := after.DeepCopyObject().(client.Object)
			annotations := unchanged.GetAnnotations()
			delete(annotations, key)
			unchanged.SetAnnotations(annotations)
			unchanged.SetResourceVersion(before.GetResourceVersion())
			if !reflect.DeepEqual(before, unchanged) {
				t.Fatalf("refresh changed more than the annotation:\nbefore %+v\nafter  %+v", before, after)
			}

			// The same instant again is a second request, not the first one
			// repeated.
			second, err := operate.Refresh(context.Background(), c, refOf(family), now)
			if err != nil {
				t.Fatal(err)
			}
			firstAt, _ := time.Parse(time.RFC3339, first.Request)
			secondAt, _ := time.Parse(time.RFC3339, second.Request)
			if second.Request == first.Request || !secondAt.After(firstAt) || second.Previous != first.Request {
				t.Fatalf("second = %+v after %q", second, first.Request)
			}
		})
	}
}

func TestRefreshAddsTheAnnotationsMapOnlyWhereThereIsNone(t *testing.T) {
	t.Parallel()

	key := operatorv1alpha1.ReadRequestAnnotation
	for _, family := range families {
		t.Run(string(family)+"/none", func(t *testing.T) {
			t.Parallel()

			rec := &recorder{}
			c := rec.client(t, stored(family, uidA, false, nil))
			result, err := operate.Refresh(context.Background(), c, refOf(family), clockStart)
			if err != nil {
				t.Fatal(err)
			}
			want := []map[string]any{
				{"op": "test", "path": "/metadata/uid", "value": string(uidA)},
				{"op": "test", "path": "/metadata/annotations", "value": nil},
				{"op": "add", "path": "/metadata/annotations", "value": map[string]any{key: result.Request}},
			}
			if got := decodeOperations(t, rec.sent()[0].body); !reflect.DeepEqual(got, want) {
				t.Fatalf("patch = %v, want %v", got, want)
			}
			if got := read(t, c, family).GetAnnotations(); !reflect.DeepEqual(got, map[string]string{key: result.Request}) {
				t.Fatalf("annotations = %v", got)
			}
		})
		t.Run(string(family)+"/added by another writer meanwhile", func(t *testing.T) {
			t.Parallel()

			rec := &recorder{}
			// Another writer adds an annotation after the read: the patch that
			// assumed none is refused, and the next one adds only the key.
			rec.before = func(ctx context.Context, c client.WithWatch, count int) error {
				if count != 1 {
					return nil
				}
				current := stored(family, "", false, nil)
				if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, current); err != nil {
					return err
				}
				current.SetAnnotations(map[string]string{"owner": "team-b"})
				return c.Update(ctx, current)
			}
			c := rec.client(t, stored(family, uidA, false, nil))
			result, err := operate.Refresh(context.Background(), c, refOf(family), clockStart)
			if err != nil {
				t.Fatal(err)
			}
			if sent := rec.sent(); len(sent) != 2 {
				t.Fatalf("sent %d patches, want 2", len(sent))
			}
			want := map[string]string{"owner": "team-b", key: result.Request}
			if got := read(t, c, family).GetAnnotations(); !reflect.DeepEqual(got, want) {
				t.Fatalf("annotations = %v, want %v", got, want)
			}
		})
	}
}

func TestRefreshBuildsThePatchAgainWhenTheAnnotationsVanish(t *testing.T) {
	t.Parallel()

	key := operatorv1alpha1.ReadRequestAnnotation
	for _, family := range families {
		t.Run(string(family), func(t *testing.T) {
			t.Parallel()

			rec := &recorder{}
			// Another writer removes every annotation after the read: the
			// patch that adds a key to the map meets no map, and the next one
			// adds the map.
			rec.before = func(ctx context.Context, c client.WithWatch, count int) error {
				if count != 1 {
					return nil
				}
				current := stored(family, "", false, nil)
				if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, current); err != nil {
					return err
				}
				current.SetAnnotations(nil)
				return c.Update(ctx, current)
			}
			c := rec.client(t, stored(family, uidA, false, map[string]string{"team": "shop"}))
			result, err := operate.Refresh(context.Background(), c, refOf(family), clockStart)
			if err != nil {
				t.Fatal(err)
			}
			sent := rec.sent()
			if len(sent) != 2 {
				t.Fatalf("sent %d patches, want 2", len(sent))
			}
			if got := decodeOperations(t, sent[1].body); len(got) != 3 || got[1]["path"] != "/metadata/annotations" || got[1]["op"] != "test" {
				t.Fatalf("second patch = %v, want the one that adds the map", got)
			}
			if got := read(t, c, family).GetAnnotations(); !reflect.DeepEqual(got, map[string]string{key: result.Request}) {
				t.Fatalf("annotations = %v", got)
			}
		})
	}
}

func TestRefreshRefusesAResourceReplacedSinceItWasRead(t *testing.T) {
	t.Parallel()

	for _, family := range families {
		t.Run(string(family), func(t *testing.T) {
			t.Parallel()

			rec := &recorder{}
			rec.before = func(ctx context.Context, c client.WithWatch, _ int) error {
				if err := c.Delete(ctx, stored(family, uidA, false, nil)); err != nil {
					return err
				}
				return c.Create(ctx, stored(family, uidB, false, map[string]string{"team": "shop"}))
			}
			c := rec.client(t, stored(family, uidA, false, map[string]string{"team": "shop"}))
			_, err := operate.Refresh(context.Background(), c, refOf(family), clockStart)
			if !errors.Is(err, operate.ErrReplaced) {
				t.Fatalf("err = %v, want ErrReplaced", err)
			}
			if got := read(t, c, family).GetAnnotations(); !reflect.DeepEqual(got, map[string]string{"team": "shop"}) {
				t.Fatalf("the replacement was changed: %v", got)
			}
		})
	}
}

func TestNextReadRequestMovesPastWhatIsThere(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	rows := []struct {
		name              string
		current, answered string
		want              string
	}{
		{name: "nothing there", want: "2026-11-01T12:00:00Z"},
		{name: "an earlier request", current: "2026-11-01T11:59:59Z", answered: "2026-11-01T11:59:59Z", want: "2026-11-01T12:00:00Z"},
		{name: "the same instant", current: "2026-11-01T12:00:00Z", want: "2026-11-01T12:00:00.000000001Z"},
		{
			name: "status answered this instant and the annotation is gone", answered: "2026-11-01T12:00:00Z",
			want: "2026-11-01T12:00:00.000000001Z",
		},
		{name: "a later request in another zone", current: "2026-11-01T13:00:00.5+01:00", want: "2026-11-01T12:00:00.500000001Z"},
		{name: "a value that is not an instant", current: "soon", answered: "sha256:" + strings.Repeat("b", 64), want: "2026-11-01T12:00:00Z"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			got := operate.NextReadRequest(now, row.current, row.answered)
			if got != row.want || got == row.current || got == row.answered {
				t.Fatalf("NextReadRequest = %q, want %q", got, row.want)
			}
		})
	}
}

func TestParseRef(t *testing.T) {
	t.Parallel()

	for value, want := range map[string]operate.Family{
		"schema/storefront": operate.FamilySchema, "ptahschema/storefront": operate.FamilySchema,
		"PtahSchemas/storefront": operate.FamilySchema, "ptahs/storefront": operate.FamilySchema,
		"migration/storefront": operate.FamilyMigration, "ptahmigration/storefront": operate.FamilyMigration,
		"ptahmigrations/storefront": operate.FamilyMigration, "ptahm/storefront": operate.FamilyMigration,
	} {
		ref, err := operate.ParseRef(value, namespace)
		if err != nil || ref != (operate.Ref{Family: want, Namespace: namespace, Name: name}) {
			t.Errorf("ParseRef(%q) = %+v, %v", value, ref, err)
		}
	}
	for _, value := range []string{"storefront", "schema/", "/storefront", "plan/storefront", "schema/a/b", "ptahschemaapproval/storefront"} {
		if ref, err := operate.ParseRef(value, namespace); err == nil {
			t.Errorf("ParseRef(%q) = %+v, want an error", value, ref)
		}
	}
	if _, err := operate.ParseRef("schema/storefront", ""); err == nil {
		t.Error("ParseRef with no namespace succeeded")
	}
}

// Classify reads every way a change can fail, from the error the change
// returned. A read refused before anything was sent is not a refused change,
// whatever the API server said, and a 403 that admission wrote is not RBAC's.
func TestClassifyReadsWhatBecameOfTheChange(t *testing.T) {
	t.Parallel()

	const rbac = `ptahschemas.ptah.run "storefront" is forbidden: User "dev" cannot patch resource "ptahschemas" in API group "ptah.run" in the namespace "demo"`
	forbidden := func(message string) error {
		return &apierrors.StatusError{ErrStatus: metav1.Status{
			Status: metav1.StatusFailure, Code: 403, Reason: metav1.StatusReasonForbidden, Message: message,
		}}
	}
	gone := apierrors.NewNotFound(resource, name)
	for _, test := range []struct {
		name string
		// read answers the change's read; patch answers its patch.
		read, patch error
		// replace deletes the resource and creates another under its name
		// before the patch arrives.
		replace bool
		want    operate.Answer
	}{
		{name: "RBAC refuses the patch", patch: forbidden(rbac), want: operate.Forbidden},
		{name: "admission refuses the patch", patch: forbidden(`admission webhook "v.example" denied the request: no`), want: operate.Refused},
		{name: "the API schema refuses the patch", patch: apierrors.NewInvalid(
			operatorv1alpha1.GroupVersion.WithKind("PtahSchema").GroupKind(), name, nil), want: operate.Refused},
		{name: "the API server is too busy", patch: apierrors.NewTooManyRequests("busy", 1), want: operate.Refused},
		{name: "the patch has no answer in time", patch: context.DeadlineExceeded, want: operate.Unknown},
		{name: "the API server fails the patch", patch: apierrors.NewInternalError(errors.New("etcd")), want: operate.Unknown},
		{name: "the resource is gone when the patch arrives", patch: gone, want: operate.Absent},
		{name: "the resource is replaced before the patch", replace: true, want: operate.Replaced},
		// Refused reading, nothing sent: the 403 is RBAC's, and still not a
		// refused change.
		{name: "RBAC refuses the read", read: forbidden(strings.Replace(rbac, "cannot patch", "cannot get", 1)), want: operate.NotRead},
		{name: "the read has no answer", read: context.DeadlineExceeded, want: operate.NotRead},
		{name: "the resource is not there", read: gone, want: operate.Absent},
	} {
		for _, family := range families {
			t.Run(string(family)+"/"+test.name, func(t *testing.T) {
				t.Parallel()

				changes := map[string]func(client.Client) error{
					"suspend": func(c client.Client) error {
						_, err := operate.SetSuspend(context.Background(), c, refOf(family), true)
						return err
					},
					"refresh": func(c client.Client) error {
						_, err := operate.Refresh(context.Background(), c, refOf(family), clockStart)
						return err
					},
				}
				for change, call := range changes {
					rec := &recorder{}
					rec.before = func(ctx context.Context, c client.WithWatch, _ int) error {
						if test.replace {
							if err := c.Delete(ctx, stored(family, uidA, false, nil)); err != nil {
								return err
							}
							return c.Create(ctx, stored(family, uidB, false, nil))
						}
						if test.patch == gone {
							if err := c.Delete(ctx, stored(family, uidA, false, nil)); err != nil {
								return err
							}
						}
						return test.patch
					}
					c := interceptor.NewClient(rec.client(t, stored(family, uidA, false, nil)), interceptor.Funcs{
						Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
							if test.read != nil {
								return test.read
							}
							return c.Get(ctx, key, obj, opts...)
						},
					})

					err := call(c)

					if got := operate.Classify(err); got != test.want {
						t.Fatalf("%s: Classify(%v) = %s, want %s", change, err, got, test.want)
					}
					if sent := len(rec.sent()); (test.read != nil) != (sent == 0) {
						t.Fatalf("%s sent %d patches after a read that answered %v", change, sent, test.read)
					}
				}
			})
		}
	}
}
