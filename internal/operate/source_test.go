package operate_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/operate"
)

var (
	digestOld   = "sha256:" + strings.Repeat("a", 64)
	digestNew   = "sha256:" + strings.Repeat("b", 64)
	digestOther = "sha256:" + strings.Repeat("c", 64)
	pinnedOld   = "oci://registry.example/shop/storefront@" + digestOld
	pinnedNew   = "oci://registry.example/shop/storefront@" + digestNew
	pinnedOther = "oci://registry.example/shop/storefront@" + digestOther
)

func TestParsePinnedReference(t *testing.T) {
	t.Parallel()

	// Each refused value is one edit away from an accepted one, so a row that
	// passes shows the parser refusing that edit and nothing more.
	rows := []struct {
		name, value string
		want        string // empty for an accepted value
		tag         string
		// notWant is what the refusal must not call the value.
		notWant string
	}{
		{name: "a digest", value: pinnedNew},
		{name: "a registry with a port", value: "oci://localhost:5000/shop/storefront@" + digestNew},
		{name: "a nested repository", value: "oci://registry.example/acme/shop/storefront@" + digestNew},
		{name: "a tag", value: "oci://registry.example/shop/storefront:1.5.0", want: "names tag 1.5.0, not a digest", tag: "1.5.0"},
		{name: "a tag on a registry with a port", value: "oci://localhost:5000/shop/storefront:1.5.0",
			want: "names tag 1.5.0, not a digest", tag: "1.5.0"},
		{name: "no selector", value: "oci://registry.example/shop/storefront", want: "which means tag latest", tag: "latest"},
		{name: "a tag beside the digest", value: "oci://registry.example/shop/storefront:1.5.0@" + digestNew,
			want: "names tag 1.5.0 beside its digest"},
		// The operator's parser reads repository oci, and drops the rest up
		// to the digest unread.
		{name: "another reference after the registry", value: "oci://registry.example/oci://other.example/shop/storefront@" + digestNew,
			want: "has oci://other.example/shop/storefront where its repository path belongs, and a repository path carries " +
				"no scheme: the operator would read repository oci from it and drop the rest unread", notWant: "tag"},
		{name: "another scheme after the registry", value: "oci://registry.example/https://other.example/shop/storefront@" + digestNew,
			want:    "has https://other.example/shop/storefront where its repository path belongs, and a repository path carries no scheme",
			notWant: "tag"},
		// The parser refuses it, calling what follows the first colon a tag.
		{name: "another reference after the registry, with no digest", value: "oci://registry.example/oci://other.example/shop/storefront:1.5.0",
			want:    "has oci://other.example/shop/storefront:1.5.0 where its repository path belongs, and a repository path carries no scheme",
			notWant: "tag \""},
		{name: "a path with a colon in it before the digest", value: "oci://registry.example/shop:storefront/v1@" + digestNew,
			want: "has shop:storefront/v1 where its repository path belongs, which is not a repository path, with a tag or " +
				"without: the operator would read repository shop from it and drop the rest unread", notWant: "names tag"},
		{name: "an empty tag beside the digest", value: "oci://registry.example/shop/storefront:@" + digestNew,
			want: "has shop/storefront: where its repository path belongs, which is not a repository path", notWant: "names tag"},
		{name: "two tags beside the digest", value: "oci://registry.example/shop/storefront:1.5:0@" + digestNew,
			want: "which is not a repository path", notWant: "names tag"},
		{name: "a tag longer than a tag can be beside the digest", value: "oci://registry.example/shop/storefront:" +
			strings.Repeat("v", 129) + "@" + digestNew, want: "which is not a repository path", notWant: "names tag"},
		{name: "the longest tag beside the digest", value: "oci://registry.example/shop/storefront:" +
			strings.Repeat("v", 128) + "@" + digestNew, want: "names tag " + strings.Repeat("v", 128) + " beside its digest"},
		{name: "another algorithm", value: "oci://registry.example/shop/storefront@sha512:" + strings.Repeat("b", 128),
			want: "names digest sha512:"},
		{name: "upper-case hex", value: "oci://registry.example/shop/storefront@sha256:" + strings.Repeat("B", 64),
			want: "is not an OCI reference the operator accepts"},
		{name: "a short digest", value: "oci://registry.example/shop/storefront@sha256:" + strings.Repeat("b", 63),
			want: "is not an OCI reference the operator accepts"},
		{name: "no scheme", value: "registry.example/shop/storefront@" + digestNew, want: "oci:// scheme"},
		{name: "a credential", value: "oci://user:secret@registry.example/shop/storefront@" + digestNew, want: "embedded credentials"},
		{name: "a query", value: pinnedNew + "?x=1", want: "is not an OCI reference the operator accepts"},
		{name: "space around it", value: " " + pinnedNew, want: "whitespace"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			pinned, err := operate.ParsePinnedReference(row.value)
			if row.want == "" {
				if err != nil || pinned.Reference != row.value || !strings.HasSuffix(row.value, "@"+pinned.Digest) {
					t.Fatalf("ParsePinnedReference(%q) = %+v, %v; want it accepted", row.value, pinned, err)
				}
				return
			}
			var notPinned *operate.NotPinnedError
			if !errors.As(err, &notPinned) || !strings.Contains(err.Error(), row.want) || notPinned.Tag != row.tag {
				t.Fatalf("ParsePinnedReference(%q) = %+v, %v; want a refusal naming %q with tag %q", row.value, pinned, err, row.want, row.tag)
			}
			if row.notWant != "" && strings.Contains(err.Error(), row.notWant) {
				t.Fatalf("ParsePinnedReference(%q) refused it as %q, which calls it %q", row.value, err, row.notWant)
			}
		})
	}
}

// sourced is a resource of family at rest on pinnedOld, with something in
// every source setting a change of reference must leave alone, and an
// operation of the given type in flight when one is named. Its Job has been
// created: the claim records its UID.
func sourced(family operate.Family, uid types.UID, operation string) client.Object {
	object := stored(family, uid, false, map[string]string{"owner": "shop"})
	source := operatorv1alpha1.SourceSpec{
		OCIRef:                 pinnedOld,
		VerificationPolicyFrom: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "ptah-verification-policy"}, Key: "policy.yaml"},
		RegistryAuthFrom:       &operatorv1alpha1.RegistryAuthSource{Name: "regcred", Mode: operatorv1alpha1.RegistryAuthDockerConfigJSON},
		Transport: operatorv1alpha1.OCITransportSpec{PlainHTTP: true,
			CAFrom: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "registry-ca"}, Key: "ca.crt"}},
	}
	switch typed := object.(type) {
	case *operatorv1alpha1.PtahSchema:
		typed.Spec.Desired = source
		typed.Status.Phase, typed.Status.ActiveOperation = operatorv1alpha1.PhaseInSync, nil
		typed.Status.Source = operatorv1alpha1.SchemaSourceStatus{RequestedReference: pinnedOld, ResolvedReference: pinnedOld,
			Digest: digestOld, Verified: true}
		if operation != "" {
			typed.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
				Type: operatorv1alpha1.OperationType(operation), ID: "attempt", JobName: "ptah-" + strings.ToLower(operation),
				JobUID: "uid-job"}
		}
	case *operatorv1alpha1.PtahMigration:
		typed.Spec.Artifact = source
		typed.Status.Phase, typed.Status.ActiveOperation = operatorv1alpha1.MigrationPhaseInSync, nil
		typed.Status.Artifact = &operatorv1alpha1.SourceAccessBinding{ResolvedReference: pinnedOld, Digest: digestOld}
		if operation != "" {
			typed.Status.ActiveOperation = &operatorv1alpha1.MigrationOperationStatus{
				Type: operatorv1alpha1.MigrationOperationType(operation), ID: "attempt", JobName: "ptah-" + strings.ToLower(operation),
				JobUID: "uid-job", Attempt: 1}
		}
	}
	return object
}

func referenceOf(t *testing.T, object client.Object) string {
	t.Helper()
	source, err := operate.SourceOf(object)
	if err != nil {
		t.Fatal(err)
	}
	return source.Reference
}

// withReference is the object with its OCI reference set to reference and
// the fields the API server moves on every write zeroed, for comparing what
// else changed.
func withReference(object client.Object, reference string) client.Object {
	copied := object.DeepCopyObject().(client.Object)
	copied.SetResourceVersion("")
	switch typed := copied.(type) {
	case *operatorv1alpha1.PtahSchema:
		typed.Spec.Desired.OCIRef = reference
	case *operatorv1alpha1.PtahMigration:
		typed.Spec.Artifact.OCIRef = reference
	}
	return copied
}

func mustPin(t *testing.T, value string) operate.PinnedReference {
	t.Helper()
	pinned, err := operate.ParsePinnedReference(value)
	if err != nil {
		t.Fatal(err)
	}
	return pinned
}

func TestSetSourceSendsOneGuardedFieldChange(t *testing.T) {
	t.Parallel()

	for _, family := range families {
		t.Run(string(family), func(t *testing.T) {
			t.Parallel()

			rec := &recorder{}
			c := rec.client(t, sourced(family, uidA, ""))
			before := read(t, c, family)

			result, err := operate.SetSource(context.Background(), c, refOf(family), mustPin(t, pinnedNew))
			if err != nil {
				t.Fatal(err)
			}
			sent := rec.sent()
			if len(sent) != 1 || sent[0].patchType != types.JSONPatchType || sent[0].manager != operate.FieldManager {
				t.Fatalf("sent %+v", sent)
			}
			path := "/spec/desired/ociRef"
			if family == operate.FamilyMigration {
				path = "/spec/artifact/ociRef"
			}
			want := []map[string]any{
				{"op": "test", "path": "/metadata/uid", "value": string(uidA)},
				{"op": "replace", "path": "/metadata/resourceVersion", "value": before.GetResourceVersion()},
				{"op": "replace", "path": path, "value": pinnedNew},
			}
			if got := decodeOperations(t, sent[0].body); !reflect.DeepEqual(got, want) {
				t.Fatalf("patch = %v, want %v", got, want)
			}
			after := read(t, c, family)
			if referenceOf(t, after) != pinnedNew {
				t.Fatalf("the reference is %s, want %s", referenceOf(t, after), pinnedNew)
			}
			if !reflect.DeepEqual(withReference(before, pinnedNew), withReference(after, pinnedNew)) {
				t.Fatalf("the change reached more than the reference:\nbefore %+v\nafter  %+v", before, after)
			}
			if !result.Changed || result.Reference != pinnedNew || result.Previous != pinnedOld || result.UID != uidA ||
				result.Generation != after.GetGeneration() || result.ObservedGeneration != 6 || result.Phase != "InSync" ||
				result.Running != "" || result.Source.Digest != digestOld || result.Source.Spec.RegistryAuthFrom.Name != "regcred" ||
				result.Object == nil {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestSetSourceSendsNothingWhenTheSpecAlreadyAsksForIt(t *testing.T) {
	t.Parallel()

	for _, family := range families {
		t.Run(string(family), func(t *testing.T) {
			t.Parallel()

			rec := &recorder{}
			c := rec.client(t, sourced(family, uidA, ""))
			result, err := operate.SetSource(context.Background(), c, refOf(family), mustPin(t, pinnedOld))
			if err != nil || result.Changed || result.Reference != pinnedOld || result.Previous != pinnedOld ||
				result.Generation != 7 || len(rec.sent()) != 0 {
				t.Fatalf("result = %+v, %v after %d patches; want no change and no patch", result, err, len(rec.sent()))
			}
		})
	}
}

func TestSetSourceRefusesAConfigMapGitOrHTTPSSource(t *testing.T) {
	t.Parallel()

	for _, family := range families {
		for _, kind := range []operate.SourceKind{operate.SourceConfigMap, operate.SourceGit, operate.SourceHTTPS, operate.SourceOther} {
			t.Run(string(family)+"/"+string(kind), func(t *testing.T) {
				t.Parallel()

				object := sourced(family, uidA, "")
				source := operatorv1alpha1.SourceSpec{}
				// Other is a spec that names none of the four, as one a newer
				// API's kind of source would read here: there is no ociRef to
				// replace.
				switch kind {
				case operate.SourceConfigMap:
					source.ConfigMap = &operatorv1alpha1.ConfigMapSourceSpec{Name: "schema",
						Items: []operatorv1alpha1.ConfigMapSourceItem{{Key: "schema.sql", Path: "schema.sql"}}}
				case operate.SourceGit:
					source.Git = &operatorv1alpha1.GitSourceSpec{URL: "https://git.example/shop/schema",
						Ref: operatorv1alpha1.GitRef{Branch: "main"}, Path: "schema.sql"}
				case operate.SourceHTTPS:
					// The kind is named, so the refusal says "the HTTPS
					// source" rather than reading it as a kind it does not
					// know.
					source.HTTPS = &operatorv1alpha1.HTTPSSourceSpec{URL: "https://artifacts.example/shop/schema.sql",
						Digest: "sha256:" + strings.Repeat("a", 64), Format: operatorv1alpha1.HTTPSFormatFile, Path: "schema.sql"}
				}
				field := "spec.desired"
				switch typed := object.(type) {
				case *operatorv1alpha1.PtahSchema:
					typed.Spec.Desired = source
				case *operatorv1alpha1.PtahMigration:
					typed.Spec.Artifact, field = source, "spec.artifact"
				}
				rec := &recorder{}
				c := rec.client(t, object)

				_, err := operate.SetSource(context.Background(), c, refOf(family), mustPin(t, pinnedNew))

				var refused *operate.SourceKindError
				if !errors.As(err, &refused) || refused.Kind != kind || refused.Field != field || len(rec.sent()) != 0 {
					t.Fatalf("err = %v after %d patches; want a %s source refused before anything is sent", err, len(rec.sent()), kind)
				}
			})
		}
	}
}

// An Apply whose Job may exist is refused. An Apply claim with no Job yet is
// not, since the change drops it before any SQL runs, and neither is any
// other operation; the result names what is in flight.
func TestSetSourceRefusesWhileAnApplyRuns(t *testing.T) {
	t.Parallel()

	for _, family := range families {
		readOnly := "Observe"
		if family == operate.FamilyMigration {
			readOnly = "History"
		}
		for _, row := range []struct {
			name, operation string
			// noJob clears the claim's Job UID: its Job was not created.
			noJob, refused bool
		}{
			{name: "an Apply with its Job", operation: "Apply", refused: true},
			{name: "an Apply claim with no Job", operation: "Apply", noJob: true},
			{name: "a read", operation: readOnly},
		} {
			t.Run(string(family)+"/"+row.name, func(t *testing.T) {
				t.Parallel()

				object := sourced(family, uidA, row.operation)
				if row.noJob {
					switch typed := object.(type) {
					case *operatorv1alpha1.PtahSchema:
						typed.Status.ActiveOperation.JobUID = ""
					case *operatorv1alpha1.PtahMigration:
						typed.Status.ActiveOperation.JobUID = ""
					}
				}
				rec := &recorder{}
				c := rec.client(t, object)

				result, err := operate.SetSource(context.Background(), c, refOf(family), mustPin(t, pinnedNew))

				var running *operate.ApplyRunningError
				if row.refused {
					if !errors.As(err, &running) || running.Job != "ptah-apply" || len(rec.sent()) != 0 ||
						referenceOf(t, read(t, c, family)) != pinnedOld {
						t.Fatalf("err = %v after %d patches; want the Apply named and nothing sent", err, len(rec.sent()))
					}
					return
				}
				if err != nil || !result.Changed || result.Running != row.operation || referenceOf(t, read(t, c, family)) != pinnedNew {
					t.Fatalf("result = %+v, %v; want the change made and %s named", result, err, row.operation)
				}
			})
		}
	}
}

// The patch carries the resourceVersion it read, so a write in between makes
// the API server refuse it, and the fresh read decides what follows.
func TestSetSourceDecidesAgainAfterAConflict(t *testing.T) {
	t.Parallel()

	rows := []struct {
		name string
		// between is a write that lands between SetSource's read and its
		// first patch.
		between     func(context.Context, client.WithWatch, operate.Family) error
		want        func(operate.SourceResult, error) bool
		wantRef     string
		wantPatches int
	}{
		{
			name: "the operator writes status, which the change is retried over",
			between: func(ctx context.Context, c client.WithWatch, family operate.Family) error {
				return writeStatus(ctx, c, family, func(object client.Object) { setPhase(object, "Resolving") })
			},
			want: func(result operate.SourceResult, err error) bool { return err == nil && result.Changed }, wantRef: pinnedNew, wantPatches: 2,
		},
		{
			name: "the operator claims an Apply",
			between: func(ctx context.Context, c client.WithWatch, family operate.Family) error {
				return writeStatus(ctx, c, family, func(object client.Object) {
					claimed := sourced(family, uidA, "Apply")
					setStatusOf(object, claimed)
				})
			},
			want: func(_ operate.SourceResult, err error) bool {
				var running *operate.ApplyRunningError
				return errors.As(err, &running)
			},
			wantRef: pinnedOld, wantPatches: 1,
		},
		{
			name: "another writer sets another reference, which is kept",
			between: func(ctx context.Context, c client.WithWatch, family operate.Family) error {
				return writeReference(ctx, c, family, pinnedOther)
			},
			want: func(_ operate.SourceResult, err error) bool {
				var changed *operate.SourceChangedError
				return errors.As(err, &changed) && changed.Was == pinnedOld && changed.Now.Reference == pinnedOther &&
					operate.Classify(err) == operate.Refused
			},
			wantRef: pinnedOther, wantPatches: 1,
		},
		{
			name: "another writer sets the same reference",
			between: func(ctx context.Context, c client.WithWatch, family operate.Family) error {
				return writeReference(ctx, c, family, pinnedNew)
			},
			// What the person asked for holds: the change is reached, by
			// another writer's hand, and nothing this one sent was stored.
			want: func(result operate.SourceResult, err error) bool {
				return err == nil && !result.Changed && result.SetByAnother && result.Previous == pinnedOld &&
					result.Reference == pinnedNew
			},
			wantRef: pinnedNew, wantPatches: 1,
		},
		{
			name: "the resource is replaced",
			between: func(ctx context.Context, c client.WithWatch, family operate.Family) error {
				if err := c.Delete(ctx, sourced(family, uidA, "")); err != nil {
					return err
				}
				return c.Create(ctx, sourced(family, uidB, ""))
			},
			want: func(_ operate.SourceResult, err error) bool { return errors.Is(err, operate.ErrReplaced) }, wantRef: pinnedOld, wantPatches: 1,
		},
	}
	for _, row := range rows {
		for _, family := range families {
			t.Run(string(family)+"/"+row.name, func(t *testing.T) {
				t.Parallel()

				rec := &recorder{}
				rec.before = func(ctx context.Context, c client.WithWatch, count int) error {
					if count == 1 {
						return row.between(ctx, c, family)
					}
					return nil
				}
				c := rec.client(t, sourced(family, uidA, ""))

				result, err := operate.SetSource(context.Background(), c, refOf(family), mustPin(t, pinnedNew))

				if !row.want(result, err) || len(rec.sent()) != row.wantPatches {
					t.Fatalf("err = %v after %d patches; want %d patches", err, len(rec.sent()), row.wantPatches)
				}
				if got := referenceOf(t, read(t, c, family)); got != row.wantRef {
					t.Fatalf("the reference is %s, want %s", got, row.wantRef)
				}
			})
		}
	}

	// A resource whose status moves on every attempt is given up on with the
	// API server's refusal: nothing was stored.
	t.Run("a conflict on every attempt", func(t *testing.T) {
		t.Parallel()

		rec := &recorder{}
		phases := []string{"Resolving", "Verifying", "Observing"}
		rec.before = func(ctx context.Context, c client.WithWatch, count int) error {
			return writeStatus(ctx, c, operate.FamilySchema, func(object client.Object) { setPhase(object, phases[count-1]) })
		}
		c := rec.client(t, sourced(operate.FamilySchema, uidA, ""))

		_, err := operate.SetSource(context.Background(), c, refOf(operate.FamilySchema), mustPin(t, pinnedNew))

		if operate.Classify(err) != operate.Refused || len(rec.sent()) != 3 || referenceOf(t, read(t, c, operate.FamilySchema)) != pinnedOld {
			t.Fatalf("err = %v (%s) after %d patches; want the conflict after three", err, operate.Classify(err), len(rec.sent()))
		}
	})
}

// A conflict is read twice: once to classify it, once to decide again. A
// resource replaced between those two reads is not the one first read
// either, and the second read refuses it rather than deciding afresh for an
// object nobody looked at.
func TestSetSourceRefusesAResourceReplacedWhileAConflictIsRead(t *testing.T) {
	t.Parallel()

	for _, family := range families {
		t.Run(string(family), func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			gets, patches := 0, 0
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).
				WithStatusSubresource(&operatorv1alpha1.PtahSchema{}, &operatorv1alpha1.PtahMigration{}).
				WithObjects(sourced(family, uidA, "")).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						mu.Lock()
						gets++
						third := gets == 3
						mu.Unlock()
						if third {
							if err := replace(ctx, c, sourced(family, uidB, "")); err != nil {
								return err
							}
						}
						return c.Get(ctx, key, obj, opts...)
					},
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						mu.Lock()
						patches++
						mu.Unlock()
						if err := writeStatus(ctx, c, family, func(object client.Object) { setPhase(object, "Resolving") }); err != nil {
							return err
						}
						return c.Patch(ctx, obj, patch, opts...)
					},
				}).Build()

			_, err := operate.SetSource(context.Background(), c, refOf(family), mustPin(t, pinnedNew))

			replacement := read(t, c, family)
			if !errors.Is(err, operate.ErrReplaced) || patches != 1 || gets < 3 ||
				replacement.GetUID() != uidB || referenceOf(t, replacement) != pinnedOld {
				t.Fatalf("err = %v after %d patches and %d reads; the replacement %s asks for %s",
					err, patches, gets, replacement.GetUID(), referenceOf(t, replacement))
			}
		})
	}
}

// A conflict whose read again fails still answered the change: the API
// server refused it and stored nothing, so it reads as refused, and the
// message names both. A resource found gone on that read is absent.
func TestSetSourceReportsTheConflictWhenTheReadAfterItFails(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name    string
		readErr error
		want    operate.Answer
	}{
		{"the API server does not answer the read", apierrors.NewServiceUnavailable("etcd is not ready"), operate.Refused},
		{"the resource is gone", apierrors.NewNotFound(resource, name), operate.Absent},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			gets, patches := 0, 0
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).
				WithStatusSubresource(&operatorv1alpha1.PtahSchema{}, &operatorv1alpha1.PtahMigration{}).
				WithObjects(sourced(operate.FamilySchema, uidA, "")).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						mu.Lock()
						gets++
						// The first read decides, the second classifies the
						// conflict, the third would decide again.
						third := gets == 3
						mu.Unlock()
						if third {
							return row.readErr
						}
						return c.Get(ctx, key, obj, opts...)
					},
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						mu.Lock()
						patches++
						mu.Unlock()
						if err := writeStatus(ctx, c, operate.FamilySchema, func(object client.Object) { setPhase(object, "Resolving") }); err != nil {
							return err
						}
						return c.Patch(ctx, obj, patch, opts...)
					},
				}).Build()

			_, err := operate.SetSource(context.Background(), c, refOf(operate.FamilySchema), mustPin(t, pinnedNew))

			if operate.Classify(err) != row.want || patches != 1 || gets != 3 ||
				!strings.Contains(err.Error(), "Operation cannot be fulfilled") ||
				!strings.Contains(err.Error(), "reading it again after that conflict failed") {
				t.Fatalf("err = %v read as %s after %d patches and %d reads; want %s naming the conflict and the read",
					err, operate.Classify(err), patches, gets, row.want)
			}
			if got := referenceOf(t, read(t, c, operate.FamilySchema)); got != pinnedOld {
				t.Fatalf("the reference is %s, want %s: a refused change stored nothing", got, pinnedOld)
			}
		})
	}
}

func writeStatus(ctx context.Context, c client.WithWatch, family operate.Family, change func(client.Object)) error {
	object := sourced(family, "", "")
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, object); err != nil {
		return err
	}
	change(object)
	return c.Status().Update(ctx, object)
}

func writeReference(ctx context.Context, c client.WithWatch, family operate.Family, reference string) error {
	object := sourced(family, "", "")
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, object); err != nil {
		return err
	}
	switch typed := object.(type) {
	case *operatorv1alpha1.PtahSchema:
		typed.Spec.Desired.OCIRef = reference
	case *operatorv1alpha1.PtahMigration:
		typed.Spec.Artifact.OCIRef = reference
	}
	return c.Update(ctx, object)
}

func setPhase(object client.Object, phase string) {
	switch typed := object.(type) {
	case *operatorv1alpha1.PtahSchema:
		typed.Status.Phase = operatorv1alpha1.ReconciliationPhase(phase)
	case *operatorv1alpha1.PtahMigration:
		typed.Status.Phase = operatorv1alpha1.MigrationPhase(phase)
	}
}

func setStatusOf(object, from client.Object) {
	switch typed := object.(type) {
	case *operatorv1alpha1.PtahSchema:
		typed.Status = from.(*operatorv1alpha1.PtahSchema).Status
	case *operatorv1alpha1.PtahMigration:
		typed.Status = from.(*operatorv1alpha1.PtahMigration).Status
	}
}

// served answers each read with the next object, as a timeline does with
// documents, and the last one again after that.
type served struct {
	mu      sync.Mutex
	objects []client.Object
	reads   int
}

func (s *served) reader(t *testing.T) client.Reader {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithStatusSubresource(&operatorv1alpha1.PtahSchema{}, &operatorv1alpha1.PtahMigration{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				s.mu.Lock()
				next := s.objects[min(s.reads, len(s.objects)-1)]
				s.reads++
				s.mu.Unlock()
				if err := replace(ctx, c, next.DeepCopyObject().(client.Object)); err != nil {
					return err
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
}

// pointedAt is a document of family whose spec asks for reference, whose
// status records digest as resolved, and, for a schema, a plan computed from
// planDigest when one is named.
func pointedAt(d doc, family operate.Family, reference, digest, planDigest string) client.Object {
	object := d.object(family)
	switch typed := object.(type) {
	case *operatorv1alpha1.PtahSchema:
		typed.Spec.Desired = operatorv1alpha1.SourceSpec{OCIRef: reference}
		typed.Status.Source = operatorv1alpha1.SchemaSourceStatus{RequestedReference: reference,
			ResolvedReference: "oci://registry.example/shop/storefront@" + digest, Digest: digest, Verified: true}
		if typed.Status.Plan != nil {
			typed.Status.Plan.ArtifactDigest = planDigest
		}
	case *operatorv1alpha1.PtahMigration:
		typed.Spec.Artifact = operatorv1alpha1.SourceSpec{OCIRef: reference}
		typed.Status.Artifact = &operatorv1alpha1.SourceAccessBinding{
			ResolvedReference: "oci://registry.example/shop/storefront@" + digest, Digest: digest}
	}
	return object
}

// The documents a wait for a change of source reads. The change made
// generation 4. Another writer then pointed the resource at pinnedOther,
// which the operator worked from at generation 5, and set pinnedNew back at
// generation 6: until the operator acts on 6, status carries the other
// artifact at a generation past the change and from a reading after it, which
// the generation and a fresh reading alone accept.

// awaiting is a resource of family on pinnedNew at generation 6, waiting for
// an approval of a plan computed from planned, with resolved the artifact
// the operator resolved, as status records them at observedGeneration.
func awaiting(family operate.Family, observedGeneration int64, observedAt int, resolved, planned string) client.Object {
	d := doc{generation: 6, observedGeneration: observedGeneration, phase: "AwaitingApproval", observedAt: at(observedAt),
		conditions: approvalConditions(metav1.ConditionTrue), plan: &planDoc{name: "plan-" + planned[7:10], uid: "plan-uid"}}
	if family == operate.FamilyMigration {
		resolved = planned
	}
	return pointedAt(d, family, pinnedNew, resolved, planned)
}

// resting is a resource of family on pinnedNew at generation 6, in sync from
// digest as status records it at observedGeneration.
func resting(family operate.Family, observedGeneration int64, observedAt int, digest string) client.Object {
	d := doc{generation: 6, observedGeneration: observedGeneration, phase: "InSync", observedAt: at(observedAt),
		conditions: inSyncConditions(family, observedGeneration)}
	return pointedAt(d, family, pinnedNew, digest, "")
}

// changedTo is the answer to the change: generation 4, which the operator
// had not observed, on the artifact before it.
func changedTo(family operate.Family) client.Object {
	return pointedAt(doc{generation: 4, observedGeneration: 3, phase: "InSync", observedAt: at(0),
		conditions: inSyncConditions(family, 3)}, family, pinnedNew, digestOld, "")
}

// A wait for the source SetSource wrote counts a document only once the
// operator worked from that reference. Each row waits again without the
// source, and that wait stops on the other writer's artifact.
func TestWaitForASourceNeedsTheOperatorToWorkFromIt(t *testing.T) {
	t.Parallel()

	rows := []struct {
		name   string
		target operate.Target
		// documents is what the operator writes after the change, in the
		// order the wait reads it; the last one is its work from pinnedNew.
		documents func(operate.Family) []client.Object
		// wantWithout is the read a wait without the source stops at.
		wantWithout int
	}{
		{
			// A schema's plan is held to the digest it was computed from,
			// which the middle document gives the other artifact beside a
			// resolution of the new one: the claim is about the plan a person
			// would approve.
			name: "approval: a plan from another writer's artifact", target: operate.TargetApproval,
			documents: func(f operate.Family) []client.Object {
				return []client.Object{
					awaiting(f, 3, 0, digestOld, digestOld),
					awaiting(f, 5, 30, digestNew, digestOther),
					awaiting(f, 6, 60, digestNew, digestNew),
				}
			},
			wantWithout: 2,
		},
		{
			name: "in-sync: a resolution of another writer's artifact", target: operate.TargetInSync,
			documents: func(f operate.Family) []client.Object {
				return []client.Object{
					resting(f, 3, 0, digestOld),
					resting(f, 5, 30, digestOther),
					resting(f, 6, 60, digestNew),
				}
			},
			wantWithout: 2,
		},
	}
	for _, row := range rows {
		for _, family := range families {
			for _, withSource := range []bool{true, false} {
				t.Run(string(family)+"/"+row.name+map[bool]string{true: "", false: "/without the source"}[withSource], func(t *testing.T) {
					t.Parallel()

					tl := &served{objects: row.documents(family)}
					options := operate.WaitOptions{
						Target: row.target, Timeout: timeout, Interval: interval, Clock: newStepClock(),
						Since: changedTo(family), Generation: 4,
					}
					want := row.wantWithout
					if withSource {
						options.Source, want = pinnedNew, 3
					}

					result, err := operate.Wait(context.Background(), tl.reader(t), refOf(family), options)

					if err != nil || result.Reads != want {
						t.Fatalf("wait = %+v, %v; want the target at read %d", result, err, want)
					}
				})
			}
		}
	}
}

// Without --for, set-source waits for whichever the operator reaches from the
// new reference, and the result names which.
func TestWaitForAPlannedSourceEndsOnWhicheverTheOperatorReaches(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name      string
		documents func(operate.Family) []client.Object
		want      operate.Target
	}{
		{"a plan waits for an approval", func(f operate.Family) []client.Object {
			return []client.Object{awaiting(f, 5, 30, digestNew, digestOther), awaiting(f, 6, 60, digestNew, digestNew)}
		}, operate.TargetApproval},
		{"nothing to change", func(f operate.Family) []client.Object {
			return []client.Object{resting(f, 5, 30, digestOther), resting(f, 6, 60, digestNew)}
		}, operate.TargetInSync},
	} {
		for _, family := range families {
			t.Run(string(family)+"/"+row.name, func(t *testing.T) {
				t.Parallel()

				tl := &served{objects: row.documents(family)}
				result, err := operate.Wait(context.Background(), tl.reader(t), refOf(family), operate.WaitOptions{
					Target: operate.TargetPlanned, Timeout: timeout, Interval: interval, Clock: newStepClock(),
					Since: changedTo(family), Generation: 4, Source: pinnedNew,
				})

				if err != nil || result.Reads != 2 || result.Target != row.want {
					t.Fatalf("wait = %+v, %v; want %s at read 2", result, err, row.want)
				}
			})
		}
	}
}

// A wait for an approval on a resource that rests in sync from the new
// reference will not see a plan from it: the operator found nothing to do.
// In sync from another writer's artifact says nothing about this one.
func TestWaitForAnApprovalStopsWhenTheNewArtifactNeedsNoPlan(t *testing.T) {
	t.Parallel()

	for _, family := range families {
		t.Run(string(family), func(t *testing.T) {
			t.Parallel()

			tl := &served{objects: []client.Object{resting(family, 5, 30, digestOther), resting(family, 6, 60, digestNew)}}
			_, err := operate.Wait(context.Background(), tl.reader(t), refOf(family), operate.WaitOptions{
				Target: operate.TargetApproval, Timeout: timeout, Interval: interval, Clock: newStepClock(),
				Since: changedTo(family), Generation: 4, Source: pinnedNew,
			})

			var stop *operate.StopError
			if !errors.As(err, &stop) || stop.Reason != operate.StopNothingPlanned || tl.reads != 2 ||
				!strings.Contains(stop.Message, "rests in sync, and no plan waits") {
				t.Fatalf("err = %v after %d reads, want a NothingPlanned stop at read 2", err, tl.reads)
			}
		})
	}
}

func TestWaitForASourceStopsWhenTheSpecNamesAnother(t *testing.T) {
	t.Parallel()

	for _, family := range families {
		t.Run(string(family), func(t *testing.T) {
			t.Parallel()

			at3 := doc{generation: 4, observedGeneration: 3, phase: "InSync", observedAt: at(0), conditions: inSyncConditions(family, 3)}
			tl := &served{objects: []client.Object{
				pointedAt(at3, family, pinnedNew, digestOld, ""),
				pointedAt(at3, family, pinnedOther, digestOld, ""),
			}}
			_, err := operate.Wait(context.Background(), tl.reader(t), refOf(family), operate.WaitOptions{
				Target: operate.TargetInSync, Timeout: timeout, Interval: interval, Clock: newStepClock(),
				Generation: 4, Source: pinnedNew,
			})

			var stop *operate.StopError
			if !errors.As(err, &stop) || stop.Reason != operate.StopSourceChanged || !strings.Contains(stop.Message, pinnedOther) {
				t.Fatalf("err = %v, want a stop naming %s", err, pinnedOther)
			}
		})
	}
}

func TestWaitRefusesASourceItCannotHoldTheTargetTo(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name    string
		options operate.WaitOptions
		want    string
	}{
		{"a read", operate.WaitOptions{Target: operate.TargetRead, ReadRequest: request, Source: pinnedNew}, "narrows only a wait for in-sync or approval"},
		{"an Apply", operate.WaitOptions{Target: operate.TargetApplied, Source: pinnedNew}, "narrows only a wait for in-sync or approval"},
		{"suspension", operate.WaitOptions{Target: operate.TargetSuspended, Source: pinnedNew}, "narrows only a wait for in-sync or approval"},
		{"a tag", operate.WaitOptions{Target: operate.TargetInSync, Source: "oci://registry.example/shop/storefront:1.5.0"}, "not a digest"},
		// Only the end of a change of source is waited on as either.
		{"approval or in-sync with no source", operate.WaitOptions{Target: operate.TargetPlanned}, "unknown wait target"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			tl := &timeline{family: operate.FamilySchema, docs: []doc{{phase: "InSync"}}}
			row.options.Timeout, row.options.Clock = timeout, newStepClock()
			_, err := operate.Wait(context.Background(), tl.reader(t), refOf(operate.FamilySchema), row.options)
			if err == nil || !strings.Contains(err.Error(), row.want) || tl.readCount() != 0 {
				t.Fatalf("err = %v after %d reads, want %q before any read", err, tl.readCount(), row.want)
			}
		})
	}
}
