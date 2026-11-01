package crd_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/apirefusal"
	"github.com/stokaro/ptah-operator/internal/operate"
)

var (
	envtestOldDigest = "sha256:" + strings.Repeat("a", 64)
	envtestNewDigest = "sha256:" + strings.Repeat("b", 64)
	envtestOldRef    = "oci://ghcr.io/example/application@" + envtestOldDigest
	envtestNewRef    = "oci://ghcr.io/example/application@" + envtestNewDigest
	envtestOtherRef  = "oci://ghcr.io/example/application@sha256:" + strings.Repeat("c", 64)
)

// kubectl ptah set-source, through internal/operate, against the API server
// and as the identities the docs grant: a person with get and patch on both
// families, and a watcher with get alone. The API server authorizes each
// request, applies the patch with its own JSON patch library and checks the
// resourceVersion the patch carries against what it stores. No controller
// runs, so where a row waits, it writes the status the operator would,
// between the wait's reads.
func TestKubectlPtahSetSourceAgainstTheAPIServer(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(plane.Config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	as := func(user string) client.WithWatch {
		t.Helper()
		c, err := client.NewWithWatch(plane.Impersonate(user), client.Options{Scheme: scheme})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	fixture := func(t *testing.T, prefix string) (string, client.WithWatch, client.WithWatch) {
		t.Helper()
		ctx := context.Background()
		namespace := newNamespace(t, prefix)
		person, watcher := namespace+"-person", namespace+"-watcher"
		families := []string{"ptahschemas", "ptahmigrations"}
		grant(ctx, t, namespace, person, "source-get", families, "get")
		grant(ctx, t, namespace, person, "source-patch", families, "patch")
		grant(ctx, t, namespace, watcher, "watch-get", families, "get")
		allowed(ctx, t, namespace, person, "patch")
		allowed(ctx, t, namespace, watcher, "get")
		return namespace, as(person), as(watcher)
	}

	t.Run("the reference changes and nothing else, and once", func(t *testing.T) {
		t.Parallel()
		for _, family := range []operate.Family{operate.FamilySchema, operate.FamilyMigration} {
			t.Run(string(family), func(t *testing.T) {
				ctx := context.Background()
				namespace, person, _ := fixture(t, "source-set")
				ref := storeOnArtifact(t, family, namespace)
				before := readUnstructured(t, family, ref)

				result, err := operate.SetSource(ctx, person, ref, pinnedEnvtest(t, envtestNewRef))
				if err != nil || !result.Changed || result.Generation != before.GetGeneration()+1 || result.Previous != envtestOldRef {
					t.Fatalf("set-source = %+v, %v; want a change to generation %d", result, err, before.GetGeneration()+1)
				}
				after := readUnstructured(t, family, ref)
				field := sourceField(family)
				if got, _, _ := unstructured.NestedString(after.Object, "spec", field, "ociRef"); got != envtestNewRef {
					t.Fatalf("spec.%s.ociRef = %q, want %q", field, got, envtestNewRef)
				}
				// Everything but the one field, as the API server stored it:
				// the rest of the source, the policy, the target, metadata.
				for _, object := range []*unstructured.Unstructured{before, after} {
					unstructured.RemoveNestedField(object.Object, "spec", field, "ociRef")
				}
				if !reflect.DeepEqual(before.Object["spec"], after.Object["spec"]) ||
					!reflect.DeepEqual(before.GetLabels(), after.GetLabels()) ||
					!reflect.DeepEqual(before.GetAnnotations(), after.GetAnnotations()) {
					t.Fatalf("the change reached more than the reference:\nbefore %v\nafter  %v", before.Object, after.Object)
				}
				if registry, _, _ := unstructured.NestedString(after.Object, "spec", field, "registryAuthFrom", "name"); registry != "regcred" {
					t.Fatalf("the fixture's registryAuthFrom is %q, so the row proves nothing about keeping it", registry)
				}

				again, err := operate.SetSource(ctx, person, ref, pinnedEnvtest(t, envtestNewRef))
				if err != nil || again.Changed || readUnstructured(t, family, ref).GetResourceVersion() != after.GetResourceVersion() {
					t.Fatalf("set-source again = %+v, %v; want nothing sent", again, err)
				}
			})
		}
	})

	// The schema admits a tag and a reference with a credential in it; the
	// plugin is what refuses them, before it reaches the cluster.
	t.Run("a reference the API server would store is refused before anything is sent", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		namespace, _, _ := fixture(t, "source-unpinned")
		ref := storeOnArtifact(t, operate.FamilySchema, namespace)
		for _, value := range []string{
			"oci://ghcr.io/example/application:1.5.0",
			"oci://ghcr.io/example/application:1.5.0@" + envtestNewDigest,
			"oci://user:secret@ghcr.io/example/application@" + envtestNewDigest,
		} {
			stored := readUnstructured(t, operate.FamilySchema, ref)
			if err := unstructured.SetNestedField(stored.Object, value, "spec", "desired", "ociRef"); err != nil {
				t.Fatal(err)
			}
			if err := api.Update(ctx, stored, client.DryRunAll); err != nil {
				t.Fatalf("the API server refuses %q itself (%v), so the row proves nothing about the plugin", value, err)
			}
			var notPinned *operate.NotPinnedError
			if _, err := operate.ParsePinnedReference(value); !errors.As(err, &notPinned) {
				t.Fatalf("ParsePinnedReference(%q) = %v, want a refusal", value, err)
			}
		}
	})

	t.Run("a ConfigMap source is refused", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		namespace, person, _ := fixture(t, "source-configmap")
		created := schemaBase(namespace)()
		created.Object["spec"].(map[string]any)["desired"] = map[string]any{
			"configMap": map[string]any{"name": "application-schema",
				"items": []any{map[string]any{"key": "schema.sql", "path": "schema.sql"}}},
		}
		if err := api.Create(ctx, created); err != nil {
			t.Fatal(err)
		}
		ref := operate.Ref{Family: operate.FamilySchema, Namespace: namespace, Name: created.GetName()}
		before := readUnstructured(t, operate.FamilySchema, ref)

		_, err := operate.SetSource(ctx, person, ref, pinnedEnvtest(t, envtestNewRef))

		var refused *operate.SourceKindError
		if !errors.As(err, &refused) || refused.Kind != operate.SourceConfigMap ||
			readUnstructured(t, operate.FamilySchema, ref).GetResourceVersion() != before.GetResourceVersion() {
			t.Fatalf("set-source on a ConfigMap source = %v; want it refused with nothing stored", err)
		}
	})

	t.Run("RBAC refuses the change to an identity that may only read, in the words the other verbs read", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		namespace, _, watcher := fixture(t, "source-forbidden")
		ref := storeOnArtifact(t, operate.FamilySchema, namespace)
		before := readUnstructured(t, operate.FamilySchema, ref)

		_, err := operate.SetSource(ctx, watcher, ref, pinnedEnvtest(t, envtestNewRef))

		refusal := `cannot patch resource "ptahschemas" in API group "operator.ptah.run" in the namespace "` + namespace + `"`
		if operate.Classify(err) != operate.Forbidden || !apirefusal.ByRBAC(err) || !strings.Contains(err.Error(), refusal) {
			t.Fatalf("set-source as the watcher: %v read as %s; want RBAC's refusal naming %q", err, operate.Classify(err), refusal)
		}
		if after := readUnstructured(t, operate.FamilySchema, ref); after.GetResourceVersion() != before.GetResourceVersion() {
			t.Fatalf("a refused change moved the resource from %s to %s", before.GetResourceVersion(), after.GetResourceVersion())
		}
		_, err = operate.SetSource(ctx, as(namespace+"-stranger"), ref, pinnedEnvtest(t, envtestNewRef))
		if operate.Classify(err) != operate.NotRead {
			t.Fatalf("set-source as a stranger: %v read as %s; want a refused read", err, operate.Classify(err))
		}
	})

	// Between the person's read and their patch something else writes. The
	// patch carries the resourceVersion of the read, so the API server
	// refuses it, and what the next read finds decides what happens.
	for _, row := range []struct {
		name    string
		family  operate.Family
		between func(*testing.T, operate.Ref)
		check   func(*testing.T, error, operate.Ref)
	}{
		{
			name: "the operator claims an Apply", family: operate.FamilyMigration,
			between: func(t *testing.T, ref operate.Ref) {
				operatorStatus(t, admin, ref, func(s *status) {
					s.phase("Applying", 1)
					s.operation("Apply", time.Now().UTC())
					s.migration.Status.ActiveOperation.ExecutionBindingID = "v1-" + strings.Repeat("0", 32)
					// The claim records that its dispatch began: its Job may
					// exist.
					s.migration.Status.ActiveOperation.DispatchStarted = true
				})
			},
			check: func(t *testing.T, err error, ref operate.Ref) {
				var running *operate.ApplyRunningError
				if !errors.As(err, &running) || referenceIn(t, admin, ref) != envtestOldRef {
					t.Fatalf("set-source over a claimed Apply = %v, reference %s; want it refused", err, referenceIn(t, admin, ref))
				}
			},
		},
		{
			name: "another writer sets another reference, which is kept", family: operate.FamilySchema,
			between: func(t *testing.T, ref operate.Ref) {
				other := readBack(t, admin, ref).(*operatorv1alpha1.PtahSchema)
				other.Spec.Desired.OCIRef = envtestOtherRef
				if err := admin.Update(context.Background(), other); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, err error, ref operate.Ref) {
				var changed *operate.SourceChangedError
				if !errors.As(err, &changed) || changed.Now.Reference != envtestOtherRef || referenceIn(t, admin, ref) != envtestOtherRef {
					t.Fatalf("set-source over another writer = %v, reference %s; want theirs kept", err, referenceIn(t, admin, ref))
				}
			},
		},
		{
			name: "the operator writes status, and the change is made over it", family: operate.FamilySchema,
			between: func(t *testing.T, ref operate.Ref) {
				operatorStatus(t, admin, ref, func(s *status) { s.phase("Resolving", 1) })
			},
			check: func(t *testing.T, err error, ref operate.Ref) {
				stored := readBack(t, admin, ref).(*operatorv1alpha1.PtahSchema)
				if err != nil || stored.Spec.Desired.OCIRef != envtestNewRef || stored.Status.Phase != operatorv1alpha1.PhaseResolving {
					t.Fatalf("set-source over a status write = %v; stored %s in phase %s", err, stored.Spec.Desired.OCIRef, stored.Status.Phase)
				}
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			namespace, person, _ := fixture(t, "source-race")
			ref := storeOnArtifact(t, row.family, namespace)
			patches := 0
			racing := interceptor.NewClient(person, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					patches++
					if patches == 1 {
						row.between(t, ref)
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
			})

			_, err := operate.SetSource(ctx, racing, ref, pinnedEnvtest(t, envtestNewRef))

			if patches == 0 {
				t.Fatal("no patch was sent, so the row proves nothing about the API server's answer")
			}
			row.check(t, err, ref)
		})
	}

	// After the change the operator first answers the generation before it,
	// then the new generation from a fresh reading with the previous
	// artifact's state still in status, and then works from the new
	// reference. Only the last is what the change asked for.
	t.Run("a wait does not finish on the previous artifact", func(t *testing.T) {
		t.Parallel()
		for _, family := range []operate.Family{operate.FamilySchema, operate.FamilyMigration} {
			t.Run(string(family), func(t *testing.T) {
				ctx := context.Background()
				namespace, person, watcher := fixture(t, "source-wait")
				ref := storeOnArtifact(t, family, namespace)
				lastRead := time.Now().UTC().Add(-time.Hour)
				operatorStatus(t, admin, ref, func(s *status) {
					s.phase("InSync", 1)
					historyAt(s, lastRead)
					resolvedTo(s, envtestOldDigest)
				})
				changed, err := operate.SetSource(ctx, person, ref, pinnedEnvtest(t, envtestNewRef))
				if err != nil {
					t.Fatal(err)
				}
				target, phase := operate.TargetInSync, "InSync"
				if family == operate.FamilyMigration {
					target, phase = operate.TargetApproval, "AwaitingApproval"
				}
				fresh := time.Now().UTC().Add(time.Minute)
				answer := func(generation int64, at time.Time, digest string) func(client.Object) {
					return func(object client.Object) {
						setStatus(object, func(s *status) {
							s.phase(phase, generation)
							historyAt(s, at)
							s.noOperation()
							resolvedTo(s, digest)
							if family == operate.FamilySchema {
								s.inSync(generation)
								return
							}
							s.condition(operatorv1alpha1.ConditionApprovalRequired, metav1.ConditionTrue, "AwaitingApproval", "The plan needs an approval")
							s.migration.Status.Plan = &operatorv1alpha1.ImmutableObjectReference{
								Name: "ptah-mplan-" + digest[7:10], UID: types.UID("uid-" + digest[7:10])}
						})
					}
				}
				var previous client.Object
				operator := &operatorWrites{t: t, admin: admin, ref: ref, steps: []func(client.Object){
					answer(changed.Generation-1, lastRead, envtestOldDigest),
					func(object client.Object) {
						answer(changed.Generation, fresh, envtestOldDigest)(object)
						previous = object
					},
					answer(changed.Generation, fresh.Add(time.Minute), envtestNewDigest),
				}}

				result, err := operate.Wait(ctx, operator.reader(watcher), ref, operate.WaitOptions{
					Target: target, Timeout: 20 * time.Second, Interval: 10 * time.Millisecond,
					Since: changed.Object, Generation: changed.Generation, Source: envtestNewRef,
				})

				if err != nil || result.Reads != 3 {
					t.Fatalf("wait for %s = %+v, %v; want the third read", target, result, err)
				}
				// The document the wait went past is one the generation and a
				// fresh reading alone accept.
				if !reachedWithoutSource(t, previous, target, changed.Generation, lastRead) {
					t.Fatal("the previous artifact's document fails the generation and freshness filter, so the row proves nothing about the source")
				}
			})
		}
	})
}

// pinnedEnvtest parses a reference the rows know to be pinned.
func pinnedEnvtest(t *testing.T, value string) operate.PinnedReference {
	t.Helper()
	pinned, err := operate.ParsePinnedReference(value)
	if err != nil {
		t.Fatal(err)
	}
	return pinned
}

func sourceField(family operate.Family) string {
	if family == operate.FamilyMigration {
		return "artifact"
	}
	return "desired"
}

// storeOnArtifact creates the reference example of family pinned to
// envtestOldRef, with a registry credential, a custom CA, labels and an
// annotation a change of source must leave as they are.
func storeOnArtifact(t *testing.T, family operate.Family, namespace string) operate.Ref {
	t.Helper()
	base := schemaBase(namespace)()
	if family == operate.FamilyMigration {
		base = migrationBase(namespace)()
	}
	field := sourceField(family)
	set(t, base, envtestOldRef, "spec", field, "ociRef")
	set(t, base, map[string]any{"name": "regcred", "mode": "DockerConfigJSON"}, "spec", field, "registryAuthFrom")
	set(t, base, map[string]any{"caFrom": map[string]any{"name": "registry-ca", "key": "ca.crt"}}, "spec", field, "transport")
	set(t, base, "Always", "spec", "policy", "apply")
	base.SetLabels(map[string]string{"app.kubernetes.io/part-of": "shop"})
	base.SetAnnotations(map[string]string{"shop.example/owner": "storefront-team"})
	if err := api.Create(context.Background(), base); err != nil {
		t.Fatalf("store %s: %v", base.GetKind(), err)
	}
	return operate.Ref{Family: family, Namespace: namespace, Name: base.GetName()}
}

func readUnstructured(t *testing.T, family operate.Family, ref operate.Ref) *unstructured.Unstructured {
	t.Helper()
	object := &unstructured.Unstructured{}
	object.SetAPIVersion(apiVersion)
	object.SetKind(ref.Kind())
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, object); err != nil {
		t.Fatalf("read %s: %v", ref, err)
	}
	return object
}

func referenceIn(t *testing.T, reader client.Reader, ref operate.Ref) string {
	t.Helper()
	source, err := operate.SourceOf(readBack(t, reader, ref))
	if err != nil {
		t.Fatal(err)
	}
	return source.Reference
}

// historyAt records a reading of the database at at, with the identities a
// migration's history carries.
func historyAt(s *status, at time.Time) {
	s.observedAt(at)
	if s.migration != nil {
		history := s.migration.Status.History
		history.Fingerprint = "sha256:" + strings.Repeat("d", 64)
		history.TargetIdentityDigest = "sha256:" + strings.Repeat("e", 64)
	}
}

// resolvedTo records digest as the artifact the operator resolved.
func resolvedTo(s *status, digest string) {
	reference := "oci://ghcr.io/example/application@" + digest
	if s.schema != nil {
		s.schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
			RequestedReference: reference, ResolvedReference: reference, Digest: digest, Verified: true}
		return
	}
	s.migration.Status.Artifact = &operatorv1alpha1.SourceAccessBinding{ResolvedReference: reference, Digest: digest}
}

// reachedWithoutSource is the filter a wait for a change of source must not
// stop at: the target's phase at the generation the change asked for, from a
// reading taken after the last one before it.
func reachedWithoutSource(t *testing.T, object client.Object, target operate.Target, generation int64, lastRead time.Time) bool {
	t.Helper()
	switch typed := object.(type) {
	case *operatorv1alpha1.PtahSchema:
		return target == operate.TargetInSync && typed.Status.Phase == operatorv1alpha1.PhaseInSync &&
			typed.Status.ObservedGeneration == generation && typed.Status.Target.LastObservedAt.After(lastRead) &&
			meta.IsStatusConditionTrue(typed.Status.Conditions, operatorv1alpha1.ConditionInSync)
	case *operatorv1alpha1.PtahMigration:
		return target == operate.TargetApproval && typed.Status.Phase == operatorv1alpha1.MigrationPhaseAwaitingApproval &&
			typed.Status.ObservedGeneration == generation && typed.Status.History.ObservedAt.After(lastRead) &&
			typed.Status.Plan != nil && meta.IsStatusConditionTrue(typed.Status.Conditions, operatorv1alpha1.ConditionApprovalRequired)
	}
	t.Fatalf("no resource in %T", object)
	return false
}
