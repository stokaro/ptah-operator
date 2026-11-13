package crd_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/apirefusal"
	"github.com/stokaro/ptah-operator/internal/operate"
)

// The Apply whose proof the fixture owes, the target the Secret named when the
// read stopped, and a target a later read names.
var (
	settleApplyID     = "sha256:" + strings.Repeat("1", 64)
	settleMovedTarget = "sha256:" + strings.Repeat("b", 64)
	settleOtherTarget = "sha256:" + strings.Repeat("c", 64)
)

// kubectl ptah settle, through internal/operate, against the API server and as
// the identities the docs grant: a person with get and patch on ptahschemas,
// and a watcher with get alone. The API server authorizes each request,
// applies the JSON patch with its own library, checks the resourceVersion the
// patch carries, and validates the status the rows write as the operator would.
// No controller runs, so no row here proves that the operator takes the
// statement; internal/controller holds that.
func TestKubectlPtahSettleAgainstTheAPIServer(t *testing.T) {
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
	fixture := func(t *testing.T, prefix, target string) (operate.Ref, client.WithWatch, client.WithWatch) {
		t.Helper()
		ctx := context.Background()
		namespace := newNamespace(t, prefix)
		person, watcher := namespace+"-person", namespace+"-watcher"
		grant(ctx, t, namespace, person, "settle-get", []string{"ptahschemas"}, "get")
		grant(ctx, t, namespace, person, "settle-patch", []string{"ptahschemas"}, "patch")
		grant(ctx, t, namespace, watcher, "watch-get", []string{"ptahschemas"}, "get")
		allowed(ctx, t, namespace, person, "patch")
		allowed(ctx, t, namespace, watcher, "get")
		ref := storeStoppedSchema(t, admin, namespace, target)
		return ref, as(person), as(watcher)
	}
	annotationOf := func(t *testing.T, ref operate.Ref) (string, bool) {
		t.Helper()
		value, held := readBack(t, admin, ref).GetAnnotations()[operatorv1alpha1.SettledApplyAnnotation]
		return value, held
	}

	t.Run("the value the stop prints, into a resource with no annotations, and once", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		ref, person, _ := fixture(t, "settle-write", settleMovedTarget)
		before := readBack(t, admin, ref).(*operatorv1alpha1.PtahSchema)
		if len(before.Annotations) != 0 {
			t.Fatalf("the fixture carries annotations %v, so the row proves nothing about adding the map", before.Annotations)
		}

		result, err := operate.Settle(ctx, person, ref, operate.SettleOptions{Expect: settleApplyID + "/" + settleMovedTarget})

		if err != nil || !result.Changed || result.Held {
			t.Fatalf("settle = %+v, %v; want the annotation written", result, err)
		}
		after := readBack(t, admin, ref).(*operatorv1alpha1.PtahSchema)
		want := map[string]string{operatorv1alpha1.SettledApplyAnnotation: settleApplyID + "/" + settleMovedTarget}
		if !reflect.DeepEqual(after.Annotations, want) {
			t.Fatalf("annotations = %v, want %v", after.Annotations, want)
		}
		if !reflect.DeepEqual(before.Spec, after.Spec) || !reflect.DeepEqual(before.Labels, after.Labels) ||
			!reflect.DeepEqual(before.Status, after.Status) || before.Generation != after.Generation {
			t.Fatal("settle changed more than the annotation")
		}
		if managers := managersOf(after); !managers["kubectl-ptah"] {
			t.Fatalf("managedFields name %v; want the plugin's field manager among them", managers)
		}

		again, err := operate.Settle(ctx, person, ref, operate.SettleOptions{})
		if err != nil || again.Changed || readBack(t, admin, ref).GetResourceVersion() != after.ResourceVersion {
			t.Fatalf("settle again = %+v, %v; want nothing sent", again, err)
		}
	})

	t.Run("an expected value the stop no longer prints is refused", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		ref, person, _ := fixture(t, "settle-expect", settleOtherTarget)
		before := readBack(t, admin, ref).GetResourceVersion()

		_, err := operate.Settle(ctx, person, ref, operate.SettleOptions{Expect: settleApplyID + "/" + settleMovedTarget})

		var unexpected *operate.UnexpectedSettleValueError
		if !errors.As(err, &unexpected) || readBack(t, admin, ref).GetResourceVersion() != before {
			t.Fatalf("settle with another value expected = %v; want it refused with nothing stored", err)
		}
	})

	// Between the person's read and their patch something else writes. The
	// patch carries the resourceVersion of that read, so the API server refuses
	// it, and what the next read finds decides what happens.
	for _, row := range []struct {
		name    string
		between func(*testing.T, operate.Ref)
		check   func(*testing.T, error, operate.Ref)
		patches int
	}{
		{
			name: "the operator writes status, and the statement is written over it", patches: 2,
			between: func(t *testing.T, ref operate.Ref) {
				operatorStatus(t, admin, ref, func(s *status) { s.schema.Status.ObservedGeneration++ })
			},
			check: func(t *testing.T, err error, ref operate.Ref) {
				if value, _ := annotationOf(t, ref); err != nil || value != settleApplyID+"/"+settleMovedTarget {
					t.Fatalf("settle over a status write = %v, annotation %q", err, value)
				}
			},
		},
		{
			name: "another writer adds an annotation, and both are kept", patches: 2,
			between: func(t *testing.T, ref operate.Ref) {
				other := readBack(t, admin, ref)
				other.SetAnnotations(map[string]string{"shop.example/owner": "storefront-team"})
				if err := admin.Update(context.Background(), other); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, err error, ref operate.Ref) {
				annotations := readBack(t, admin, ref).GetAnnotations()
				if err != nil || annotations["shop.example/owner"] != "storefront-team" ||
					annotations[operatorv1alpha1.SettledApplyAnnotation] != settleApplyID+"/"+settleMovedTarget {
					t.Fatalf("settle over another writer's annotation = %v, annotations %v", err, annotations)
				}
			},
		},
		{
			name: "a read stops on another target, and nothing is written over it", patches: 1,
			between: func(t *testing.T, ref operate.Ref) {
				operatorStatus(t, admin, ref, func(s *status) {
					meta.FindStatusCondition(s.schema.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed).Message =
						settleStopMessage(ref, settleOtherTarget)
				})
			},
			check: func(t *testing.T, err error, ref operate.Ref) {
				var stale *operate.StaleSettleValueError
				if value, held := annotationOf(t, ref); !errors.As(err, &stale) || operate.Classify(err) != operate.Refused || held {
					t.Fatalf("settle over a moved stop = %v (%s), annotation %q", err, operate.Classify(err), value)
				}
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			ref, person, _ := fixture(t, "settle-race", settleMovedTarget)
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

			_, err := operate.Settle(ctx, racing, ref, operate.SettleOptions{})

			if patches != row.patches {
				t.Fatalf("settle sent %d patches, want %d: the row does not reach the API server's answer it is about", patches, row.patches)
			}
			row.check(t, err, ref)
		})
	}

	// The docs grant settle get and patch, and settle --wait get alone.
	t.Run("an identity that may only read waits, and is refused the statement", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		ref, person, watcher := fixture(t, "settle-rbac", settleMovedTarget)
		before := readBack(t, admin, ref).GetResourceVersion()

		_, err := operate.Settle(ctx, watcher, ref, operate.SettleOptions{})

		refusal := `cannot patch resource "ptahschemas" in API group "operator.ptah.run" in the namespace "` + ref.Namespace + `"`
		if operate.Classify(err) != operate.Forbidden || !apirefusal.ByRBAC(err) || !strings.Contains(err.Error(), refusal) {
			t.Fatalf("settle as the watcher: %v read as %s; want RBAC's refusal naming %q", err, operate.Classify(err), refusal)
		}
		if after := readBack(t, admin, ref).GetResourceVersion(); after != before {
			t.Fatalf("a refused settlement moved the resource from %s to %s", before, after)
		}
		if _, err := operate.Settle(ctx, as(ref.Namespace+"-stranger"), ref, operate.SettleOptions{}); operate.Classify(err) != operate.NotRead {
			t.Fatalf("settle as a stranger: %v read as %s; want a refused read", err, operate.Classify(err))
		}

		settled, err := operate.Settle(ctx, person, ref, operate.SettleOptions{})
		if err != nil {
			t.Fatal(err)
		}
		operator := &operatorWrites{t: t, admin: admin, ref: ref, steps: []func(client.Object){
			func(client.Object) {},
			tookTheStatementAt(time.Now().UTC()),
		}}

		result, err := operate.Wait(ctx, operator.reader(watcher), ref, operate.WaitOptions{
			Target: operate.TargetSettled, Timeout: 20 * time.Second, Interval: 10 * time.Millisecond,
			Since: settled.Object, Settlement: settled.Printed.Value,
		})

		if err != nil || result.Target != operate.TargetSettled || result.Reads != 2 {
			t.Fatalf("settle --wait as the watcher = %+v, %v; want the second read", result, err)
		}
	})
}

// settleStopMessage is the stop internal/controller writes for ref when a read
// names target.
func settleStopMessage(ref operate.Ref, target string) string {
	return "the URL Secret now holds another target than the plan this Apply ran from: another login, host, port, " +
		"database or URL parameter, so no read can say what the Apply did; put the first URL back and the proof " +
		"finishes, or give the proof up, which reads the database under the new URL and, under apply: Always, applies " +
		"the next plan with no approval unless it is destructive or changes privileges: kubectl annotate ptahschema " +
		ref.Name + " -n " + ref.Namespace + " " + operatorv1alpha1.SettledApplyAnnotation + "=" + settleApplyID + "/" +
		target + " --overwrite"
}

// storeStoppedSchema creates the reference schema in namespace and writes the
// status of a proof whose read named target: Blocked, with the Apply's
// pendingObservation as the schema requires it and the stop that prints the
// command.
func storeStoppedSchema(t *testing.T, admin client.Client, namespace, target string) operate.Ref {
	t.Helper()
	base := schemaBase(namespace)()
	base.SetLabels(map[string]string{"app.kubernetes.io/part-of": "shop"})
	if err := api.Create(context.Background(), base); err != nil {
		t.Fatalf("store %s: %v", base.GetKind(), err)
	}
	ref := operate.Ref{Family: operate.FamilySchema, Namespace: namespace, Name: base.GetName()}
	operatorStatus(t, admin, ref, func(s *status) {
		schema := s.schema
		now := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
		schema.Status.ObservedGeneration, schema.Status.Phase = schema.Generation, operatorv1alpha1.PhaseBlocked
		schema.Status.PendingObservation = &operatorv1alpha1.PendingObservationStatus{
			Outcome: operatorv1alpha1.PendingObservationApplySucceeded, ApplyOperationID: settleApplyID,
			ApplyGeneration: schema.Generation, CoordinationDigest: coordinationDigest, LeaseDurationSeconds: 60,
			Target: operatorv1alpha1.DatabaseTargetBinding{Engine: "PostgreSQL", URLFrom: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "application-database"}, Key: "url"}},
			Source: operatorv1alpha1.SourceAccessBinding{
				ResolvedReference: "oci://ghcr.io/example/application-schema@" + artifactDigest, Digest: artifactDigest},
			Plan: operatorv1alpha1.CurrentPlanStatus{
				Name: "ptah-plan-aaa", UID: planUID, Fingerprint: planFingerprint, ContentDigest: contentDigest,
				ArtifactDigest: artifactDigest, CoordinationDigest: coordinationDigest, TargetIdentityDigest: targetIdentityDigest,
				ActualStateFingerprint: actualStateFingerprint, DesiredStateFingerprint: desiredStateFingerprint,
				PolicyFingerprint: policyFingerprint, ExecutionBindingID: executionBindingID, ControllerImage: controllerImage,
				ControllerRevision: controllerRevision, ControllerStateVersion: 2, PtahVersion: ptahVersion,
				ExecutorImage: executorImage, RunnerImage: runnerImage, RunnerProtocolVersion: 5, StatementCount: 1,
				CreatedAt: now,
			},
		}
		s.condition(operatorv1alpha1.ConditionReconciliationFailed, metav1.ConditionTrue,
			string(operatorv1alpha1.ReasonTargetIdentityChanged), settleStopMessage(ref, target))
		s.condition(operatorv1alpha1.ConditionReady, metav1.ConditionFalse, string(operatorv1alpha1.ReasonTargetIdentityChanged),
			"The proof of the last Apply needs a person: ReconciliationFailed says what to do")
		schema.Status.Journal = &operatorv1alpha1.LifecycleJournal{CoveredSince: now, Entries: []operatorv1alpha1.JournalEntry{{
			Type: operatorv1alpha1.JournalApplyFinished, At: now, Plan: "ptah-plan-aaa", Outcome: operatorv1alpha1.JournalSucceeded,
		}}}
	})
	if targetIdentityDigest == settleMovedTarget || targetIdentityDigest == settleOtherTarget {
		t.Fatal("the plan's target is one the read names, so the stop is not on a moved target")
	}
	return ref
}

// tookTheStatementAt is the status write that gives the proof up, at at.
func tookTheStatementAt(at time.Time) func(client.Object) {
	return func(object client.Object) {
		setStatus(object, func(s *status) {
			schema := s.schema
			schema.Status.PendingObservation, schema.Status.Phase = nil, operatorv1alpha1.PhasePending
			message := "A person settled the proof of the last Apply; no read established what that Apply did"
			s.condition(operatorv1alpha1.ConditionReconciliationFailed, metav1.ConditionFalse, string(operatorv1alpha1.ReasonProofSettled), message)
			s.condition(operatorv1alpha1.ConditionReady, metav1.ConditionFalse, string(operatorv1alpha1.ReasonProofSettled), message)
			schema.Status.Journal.Entries = append(schema.Status.Journal.Entries, operatorv1alpha1.JournalEntry{
				Type: operatorv1alpha1.JournalVerified, At: metav1.NewTime(at.Truncate(time.Second).Add(time.Second)),
				Plan: "ptah-plan-aaa", Outcome: operatorv1alpha1.JournalUnknown,
			})
		})
	}
}

func managersOf(object client.Object) map[string]bool {
	managers := map[string]bool{}
	for _, entry := range object.GetManagedFields() {
		managers[entry.Manager] = true
	}
	return managers
}
