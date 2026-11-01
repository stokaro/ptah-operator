package operate_test

import (
	"context"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/operate"
)

const (
	namespace = "demo"
	name      = "storefront"
	uidA      = types.UID("11111111-1111-1111-1111-111111111111")
	uidB      = types.UID("22222222-2222-2222-2222-222222222222")
)

// t0 is when the fixtures' operator observed things. It is deliberately far
// from the wait's clock (clockStart): a wait that compared its own clock with
// the operator's timestamps would decide differently, and the rows would
// show it.
var (
	t0         = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	clockStart = time.Date(2031, 3, 4, 5, 6, 7, 0, time.UTC)
)

func at(seconds int) time.Time { return t0.Add(time.Duration(seconds) * time.Second) }

var families = []operate.Family{operate.FamilySchema, operate.FamilyMigration}

func refOf(family operate.Family) operate.Ref {
	return operate.Ref{Family: family, Namespace: namespace, Name: name}
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

// doc is one document of either family, written in the terms a wait reads.
// object turns it into the family's typed resource.
type doc struct {
	uid                types.UID
	generation         int64
	observedGeneration int64
	suspend            bool
	// deleting is a resource deletion has begun on: it carries a finalizer,
	// and the fake API server sets deletionTimestamp when it is deleted.
	deleting   bool
	phase      string
	observedAt time.Time
	operation  string
	proof      bool
	annotation string
	lastRead   string
	conditions []metav1.Condition
	plan       *planDoc
	noHistory  bool
	applied    *appliedDoc
	lastRun    *runDoc
	unresolved *unresolvedDoc
	journal    []operatorv1alpha1.JournalEntry
}

type planDoc struct {
	name        string
	uid         types.UID
	fingerprint string
	approved    bool
}

type appliedDoc struct {
	plan        string
	uid         types.UID
	fingerprint string
	completedAt time.Time
}

type runDoc struct {
	outcome    operatorv1alpha1.MigrationRunOutcome
	job        string
	jobUID     types.UID
	startedAt  time.Time
	finishedAt time.Time
	message    string
}

type unresolvedDoc struct {
	operationID string
	outcome     operatorv1alpha1.MigrationRunOutcome
	job         string
	plan        string
}

func (d doc) object(family operate.Family) client.Object {
	uid := d.uid
	if uid == "" {
		uid = uidA
	}
	generation := d.generation
	if generation == 0 {
		generation = 3
	}
	meta := metav1.ObjectMeta{Namespace: namespace, Name: name, UID: uid, Generation: generation}
	if d.deleting {
		meta.Finalizers = []string{"operator.ptah.run/test"}
	}
	if d.annotation != "" {
		meta.Annotations = map[string]string{operatorv1alpha1.ReadRequestAnnotation: d.annotation}
	}
	observedGeneration := d.observedGeneration
	if observedGeneration == 0 {
		observedGeneration = generation
	}
	var journal *operatorv1alpha1.LifecycleJournal
	if len(d.journal) > 0 {
		journal = &operatorv1alpha1.LifecycleJournal{CoveredSince: metav1.NewTime(at(-86400)), Entries: d.journal}
	}
	if family == operate.FamilySchema {
		schema := &operatorv1alpha1.PtahSchema{ObjectMeta: meta}
		schema.Spec.Suspend = d.suspend
		status := &schema.Status
		status.ObservedGeneration = observedGeneration
		status.Phase = operatorv1alpha1.ReconciliationPhase(d.phase)
		status.LastReadRequest = d.lastRead
		status.Conditions = d.conditions
		status.Journal = journal
		if !d.observedAt.IsZero() {
			observed := metav1.NewTime(d.observedAt)
			status.Target.LastObservedAt = &observed
		}
		if d.operation != "" {
			status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
				Type: operatorv1alpha1.OperationType(d.operation), ID: "attempt", JobName: "job", StartedAt: metav1.NewTime(at(-5)),
			}
		}
		if d.proof {
			status.PendingObservation = &operatorv1alpha1.PendingObservationStatus{
				Outcome: operatorv1alpha1.PendingObservationApplySucceeded, ApplyOperationID: "apply",
			}
		}
		if p := d.plan; p != nil {
			status.Plan = &operatorv1alpha1.CurrentPlanStatus{Name: p.name, UID: p.uid, Fingerprint: p.fingerprint}
			if p.approved {
				status.Plan.Approval = &operatorv1alpha1.ConsumedApprovalStatus{
					Name: "approval", UID: "approval-uid", ApprovedAt: metav1.NewTime(at(-60)),
				}
			}
		}
		if a := d.applied; a != nil {
			status.Applied = &operatorv1alpha1.AppliedStatus{
				PlanRef:         operatorv1alpha1.ImmutableObjectReference{Name: a.plan, UID: a.uid},
				PlanFingerprint: a.fingerprint, CompletedAt: metav1.NewTime(a.completedAt),
			}
		}
		return schema
	}
	migration := &operatorv1alpha1.PtahMigration{ObjectMeta: meta}
	migration.Spec.Suspend = d.suspend
	status := &migration.Status
	status.ObservedGeneration = observedGeneration
	status.Phase = operatorv1alpha1.MigrationPhase(d.phase)
	status.LastReadRequest = d.lastRead
	status.Conditions = d.conditions
	status.Journal = journal
	if !d.observedAt.IsZero() && !d.noHistory {
		status.History = &operatorv1alpha1.MigrationHistoryStatus{ObservedAt: metav1.NewTime(d.observedAt), ContractVersion: 1}
	}
	if d.operation != "" {
		status.ActiveOperation = &operatorv1alpha1.MigrationOperationStatus{
			Type: operatorv1alpha1.MigrationOperationType(d.operation), ID: "attempt", JobName: "job",
			StartedAt: metav1.NewTime(at(-5)), Attempt: 1,
		}
	}
	if p := d.plan; p != nil {
		status.Plan = &operatorv1alpha1.ImmutableObjectReference{Name: p.name, UID: p.uid}
	}
	if r := d.lastRun; r != nil {
		status.LastRun = &operatorv1alpha1.MigrationRunStatus{
			Outcome: r.outcome, JobName: r.job, JobUID: r.jobUID, StartedAt: metav1.NewTime(r.startedAt), Message: r.message,
		}
		if !r.finishedAt.IsZero() {
			finished := metav1.NewTime(r.finishedAt)
			status.LastRun.FinishedAt = &finished
		}
	}
	if u := d.unresolved; u != nil {
		status.UnresolvedRun = &operatorv1alpha1.UnresolvedMigrationRunStatus{
			Outcome: u.outcome, OperationID: u.operationID, JobName: u.job,
			PlanRef: operatorv1alpha1.ImmutableObjectReference{Name: u.plan}, RecordedAt: metav1.NewTime(at(100)),
		}
	}
	return migration
}

func condition(conditionType string, status metav1.ConditionStatus, reason, message string, generation int64) metav1.Condition {
	return metav1.Condition{
		Type: conditionType, Status: status, Reason: reason, Message: message,
		ObservedGeneration: generation, LastTransitionTime: metav1.NewTime(at(-3600)),
	}
}

// timeline serves docs one per read: the first read gets the first, and
// every read after the last gets the last again. Each document is written to
// the fake API server before it is read back, so it reaches the wait through
// the same encoding as a real answer, which keeps timestamps to whole
// seconds.
type timeline struct {
	mu     sync.Mutex
	family operate.Family
	docs   []doc
	reads  int
	errs   map[int]error
}

func (tl *timeline) reader(t *testing.T) client.Reader {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithStatusSubresource(&operatorv1alpha1.PtahSchema{}, &operatorv1alpha1.PtahMigration{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				tl.mu.Lock()
				index := tl.reads
				tl.reads++
				err := tl.errs[index]
				next := tl.docs[min(index, len(tl.docs)-1)]
				tl.mu.Unlock()
				if err != nil {
					return err
				}
				if err := replace(ctx, c, next.object(tl.family)); err != nil {
					return err
				}
				if next.deleting {
					// Create ignores a deletionTimestamp; a delete sets it on
					// an object that carries a finalizer.
					if err := c.Delete(ctx, next.object(tl.family)); err != nil {
						return err
					}
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
}

// replace stores object in place of what is there, finalizer or not.
func replace(ctx context.Context, c client.WithWatch, object client.Object) error {
	stored := object.DeepCopyObject().(client.Object)
	if err := c.Get(ctx, client.ObjectKeyFromObject(object), stored); err == nil && len(stored.GetFinalizers()) > 0 {
		stored.SetFinalizers(nil)
		if err := c.Update(ctx, stored); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	if err := c.Delete(ctx, object.DeepCopyObject().(client.Object)); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return c.Create(ctx, object)
}

func (tl *timeline) readCount() int {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return tl.reads
}

// stepClock is a clock whose sleeps return at once, having moved the time
// on by the sleep.
type stepClock struct {
	mu  sync.Mutex
	now time.Time
}

func newStepClock() *stepClock { return &stepClock{now: clockStart} }

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stepClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	fired := make(chan time.Time, 1)
	fired <- c.now
	return fired
}
