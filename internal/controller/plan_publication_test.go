package controller

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/planstore"
)

// refusePlanWrites answers every create of the given kind with err, the way
// the API server answers a write it will not store.
func refusePlanWrites(t *testing.T, reconciler *SchemaReconciler, api client.Client, chunk bool, err error) {
	t.Helper()
	watching, ok := api.(client.WithWatch)
	if !ok {
		t.Fatal("the fixture's client cannot be intercepted")
	}
	refusing := interceptor.NewClient(watching, interceptor.Funcs{
		Create: func(ctx context.Context, writer client.WithWatch, object client.Object, options ...client.CreateOption) error {
			_, isPlan := object.(*operatorv1alpha1.PtahSchemaPlan)
			_, isChunk := object.(*operatorv1alpha1.PtahSchemaPlanChunk)
			if (isPlan && !chunk) || (isChunk && chunk) {
				return err
			}
			return writer.Create(ctx, object, options...)
		},
	})
	reconciler.Plans = planstore.Store{Client: refusing, Reader: api}
}

// planResource is the resource a refused plan write named.
func planResource(chunk bool) k8sschema.GroupResource {
	if chunk {
		return operatorv1alpha1.GroupVersion.WithResource("ptahschemaplanchunks").GroupResource()
	}
	return operatorv1alpha1.GroupVersion.WithResource("ptahschemaplans").GroupResource()
}

// A plan the API server refuses to store is a Plan that failed with the
// refusal: the resource and an event name it, and the next attempt is a fresh
// Plan. Holding the claim instead met the refusal on every pass until the
// harvested Job's cleanup took the result, and the retry after that named a
// missing Job and nothing else (#711).
func TestARefusedPlanIsAFailedPlanThatNamesTheRefusal(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		chunk   bool
		refusal func(resource k8sschema.GroupResource) error
		says    string
	}{
		"an admission webhook": {
			refusal: func(resource k8sschema.GroupResource) error {
				return apierrors.NewForbidden(resource, "plan",
					errors.New(`admission webhook "plans.example" denied the request: not today`))
			},
			says: "not today",
		},
		"a webhook that set no reason": {
			// What the API server builds from a webhook's denial: the code
			// and the message, and no reason.
			refusal: func(k8sschema.GroupResource) error {
				return &apierrors.StatusError{ErrStatus: metav1.Status{
					Status: metav1.StatusFailure, Code: http.StatusForbidden,
					Message: `admission webhook "plans.example" denied the request: not today`,
				}}
			},
			says: "not today",
		},
		"an admission policy": {
			refusal: func(k8sschema.GroupResource) error {
				return apierrors.NewInvalid(operatorv1alpha1.GroupVersion.WithKind("PtahSchemaPlan").GroupKind(), "plan", field.ErrorList{
					field.Forbidden(field.NewPath("spec", "source"), "ValidatingAdmissionPolicy 'plan-guard' denied request"),
				})
			},
			says: "plan-guard",
		},
		"a quota": {
			refusal: func(resource k8sschema.GroupResource) error {
				return apierrors.NewForbidden(resource, "plan",
					errors.New("exceeded quota: plans, requested: count/ptahschemaplans.operator.ptah.run=1"))
			},
			says: "exceeded quota",
		},
		"a chunk too large to store": {
			chunk: true,
			refusal: func(k8sschema.GroupResource) error {
				return apierrors.NewRequestEntityTooLargeError("limit is 3145728")
			},
			says: "create plan chunk 0",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			reconciler, api, schema := planHarvestFixture(t, operatorv1alpha1.ApplyPolicyOnApproval, safetyPlanDocument(t, "observed-state"))
			refusePlanWrites(t, reconciler, api, test.chunk, test.refusal(planResource(test.chunk)))
			events := record.NewFakeRecorder(8)
			reconciler.Recorder = events
			claimed := schema.Status.ActiveOperation.DeepCopy()

			if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
				t.Fatalf("Reconcile() error = %v, want the refusal recorded and the Plan retried", err)
			}

			actual := safetyGetSchema(t, api, schema)
			failed := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
			if failed == nil || failed.Status != metav1.ConditionTrue ||
				failed.Reason != string(operatorv1alpha1.ReasonPlanPublicationRefused) ||
				!strings.Contains(failed.Message, "publish the plan: ") || !strings.Contains(failed.Message, test.says) {
				t.Fatalf("ReconciliationFailed = %#v, want PlanPublicationRefused naming %q", failed, test.says)
			}
			if actual.Status.Phase != operatorv1alpha1.PhaseFailed || actual.Status.Plan != nil {
				t.Fatalf("phase %s with plan %#v, want Failed and no plan", actual.Status.Phase, actual.Status.Plan)
			}
			operation := actual.Status.ActiveOperation
			if operation == nil || operation.Type != operatorv1alpha1.OperationPlan || operation.ID != claimed.ID ||
				operation.Attempt != claimed.Attempt+1 || operation.JobName == claimed.JobName || operation.JobUID != "" {
				t.Fatalf("active operation = %#v, want the Plan claimed again under a new Job", operation)
			}
			if event := findEvent(events, "Warning PlanPublicationRefused publish the plan: "); !strings.Contains(event, test.says) {
				t.Fatalf("PlanPublicationRefused event = %q, want one naming %q", event, test.says)
			}
		})
	}
}

// A server that did not answer has not refused anything. The claim holds and
// the error goes back to be retried, as before: retiring the Plan on a
// timeout would record a refusal nobody made.
func TestAPlanStoreThatDidNotAnswerKeepsTheClaim(t *testing.T) {
	t.Parallel()
	for name, failure := range map[string]error{
		"a conflict":            apierrors.NewConflict(planResource(false), "plan", errors.New("changed")),
		"a server timeout":      apierrors.NewServerTimeout(planResource(false), "create", 1),
		"an unavailable server": apierrors.NewServiceUnavailable("etcd is unavailable"),
		"an internal error":     apierrors.NewInternalError(errors.New("etcdserver: leader changed")),
		"throttling":            apierrors.NewTooManyRequests("slow down", 1),
		"a dropped connection":  errors.New("dial tcp 10.96.0.1:443: connect: connection refused"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			reconciler, api, schema := planHarvestFixture(t, operatorv1alpha1.ApplyPolicyOnApproval, safetyPlanDocument(t, "observed-state"))
			refusePlanWrites(t, reconciler, api, false, failure)
			events := record.NewFakeRecorder(8)
			reconciler.Recorder = events
			claimed := schema.Status.ActiveOperation.DeepCopy()

			if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err == nil {
				t.Fatal("Reconcile() returned no error, want it retried with the claim held")
			}

			actual := safetyGetSchema(t, api, schema)
			operation := actual.Status.ActiveOperation
			if operation == nil || operation.Attempt != claimed.Attempt || operation.JobName != claimed.JobName {
				t.Fatalf("active operation = %#v, want the claim it had", operation)
			}
			if failed := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed); failed != nil &&
				failed.Reason == string(operatorv1alpha1.ReasonPlanPublicationRefused) {
				t.Fatalf("ReconciliationFailed = %#v, a server that did not answer refused nothing", failed)
			}
			if event := findEvent(events, "Warning PlanPublicationRefused"); event != "" {
				t.Fatalf("event %q, a server that did not answer refused nothing", event)
			}
		})
	}
}
