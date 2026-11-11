package resultcleanup

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultcredentials"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultretention"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var recordResource = api.GroupVersion.WithResource("ptahresultrecords").GroupResource()

// quotaExceededReason is the reason the collector writes as a literal, so the
// gate on the Event reasons page can read it from the call.
const quotaExceededReason = "ResultRetirementQuotaExceeded"

// quotaRefused is the API server's refusal of a CREATE by a full ResourceQuota:
// admission.NewForbidden around the message the quota admission plugin writes.
func quotaRefused(quota, name string) error {
	return apierrors.NewForbidden(recordResource, name, fmt.Errorf("exceeded quota: %s, requested: %s, used: %s, limited: %s", quota,
		"count/ptahresultrecords.operator.ptah.run=1", "count/ptahresultrecords.operator.ptah.run=4", "count/ptahresultrecords.operator.ptah.run=4"))
}

// refuseRetirement refuses every retirement record with refusal, and nothing
// else: a quota that filled up after the attempt published.
func refuseRetirement(refusal func(name string) error) func(client.Object) error {
	return func(o client.Object) error {
		if o.(*api.PtahResultRecord).Spec.Type != "retired" {
			return nil
		}
		return refusal(o.GetName())
	}
}

type recordedEvent struct {
	object                     runtime.Object
	eventType, reason, message string
}

type eventLog struct{ events []recordedEvent }

func (l *eventLog) Eventf(object runtime.Object, eventType, reason, messageFmt string, args ...any) {
	l.events = append(l.events, recordedEvent{object, eventType, reason, fmt.Sprintf(messageFmt, args...)})
}

func (f cleanupFixture) collectorWith(t *testing.T, recorder EventRecorder) *Collector {
	t.Helper()
	f.c.admission = &f.p
	c, err := New(f.c, f.p, nil, recorder)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f cleanupFixture) intent(t *testing.T) *api.PtahResultRecord {
	t.Helper()
	name, _ := resultstore.Name(f.f.Identity.Binding)
	intent := &api.PtahResultRecord{}
	if err := f.c.Get(t.Context(), client.ObjectKey{Namespace: f.f.Identity.Binding.Namespace, Name: name}, intent); err != nil {
		t.Fatal(err)
	}
	return intent
}

// requireQuotaEvent holds an Event to the resource that ran the operation, the
// quota by name and its usage, and the outcome the collector reached.
func requireQuotaEvent(t *testing.T, f cleanupFixture, e recordedEvent, outcome string) {
	t.Helper()
	b := f.f.Identity.Binding
	ref, ok := e.object.(*corev1.ObjectReference)
	if !ok || ref.APIVersion != api.GroupVersion.String() || ref.Kind != b.Kind || ref.Namespace != b.Namespace || ref.Name != b.Name || ref.UID != b.UID {
		t.Fatalf("the Event is about %#v, want the %s %s/%s with UID %s", e.object, b.Kind, b.Namespace, b.Name, b.UID)
	}
	if e.eventType != corev1.EventTypeWarning || e.reason != quotaExceededReason {
		t.Fatalf("the Event is a %s %s, want a %s %s", e.eventType, e.reason, corev1.EventTypeWarning, quotaExceededReason)
	}
	for _, want := range []string{"ResourceQuota results refused", "used: count/ptahresultrecords.operator.ptah.run=4", outcome} {
		if !strings.Contains(e.message, want) {
			t.Fatalf("the Event says %q, which does not say %q", e.message, want)
		}
	}
}

// A ResourceQuota on result records that is full refuses the record that would
// retire an attempt, and nothing could be collected without one, so the
// namespace stayed full for good (#928). The collector starts the window with a
// foreground deletion instead, and says why on the resource.
func TestAFullQuotaRetiresThroughForegroundDeletion(t *testing.T) {
	f := fixture(t)
	published := f.c.creates
	f.c.refuseCreate = refuseRetirement(func(name string) error { return quotaRefused("results", name) })
	events := &eventLog{}
	if err := f.collectorWith(t, events).Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	intent := f.intent(t)
	if intent.DeletionTimestamp.IsZero() || !slices.Equal(intent.Finalizers, []string{metav1.FinalizerDeleteDependents}) {
		t.Fatalf("the intent was not retired by a foreground deletion: deletion=%v finalizers=%v", intent.DeletionTimestamp, intent.Finalizers)
	}
	if !slices.Equal(f.c.foreground, []string{"intent"}) || len(f.c.deletes) != 0 || f.c.creates-published != 1 {
		t.Fatalf("sent %d CREATEs, foreground deletions of %v and deletions of %v; want one refused CREATE and the intent's foreground deletion alone", f.c.creates-published, f.c.foreground, f.c.deletes)
	}
	if len(events.events) != 1 {
		t.Fatalf("recorded %d Events, want one", len(events.events))
	}
	requireQuotaEvent(t, f, events.events[0], "started from a foreground deletion")

	// Another leader, with no memory of this one, sends nothing more: the API
	// server's deletion timestamp is the clock now, and the window is open.
	if err := f.collectorWith(t, events).Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.c.creates-published != 1 || len(f.c.foreground) != 1 || len(f.c.deletes) != 0 || len(events.events) != 1 {
		t.Fatalf("the next pass sent %d CREATEs, %d foreground deletions and %v, and recorded %d Events", f.c.creates-published, len(f.c.foreground), f.c.deletes, len(events.events))
	}
}

// Only a quota's refusal is a reason to give up on the retirement record. A
// webhook's denial, a quota the API server has not counted yet and a lost
// response leave the root as it was, to be retired the ordinary way.
func TestOnlyAQuotaRefusalStartsForegroundRetirement(t *testing.T) {
	for _, row := range []struct {
		name    string
		refusal func(name string) error
		quota   bool
	}{
		{name: "full quota", refusal: func(name string) error { return quotaRefused("results", name) }, quota: true},
		{name: "webhook denial that quotes a quota", refusal: func(string) error {
			return &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: 403, Reason: metav1.StatusReasonForbidden,
				Message: `admission webhook "policy.example.com" denied the request: exceeded quota: results, requested: count/ptahresultrecords.operator.ptah.run=1`}}
		}},
		{name: "quota not yet counted", refusal: func(name string) error {
			return apierrors.NewForbidden(recordResource, name, errors.New("status unknown for quota: results, resources: count/ptahresultrecords.operator.ptah.run"))
		}},
		{name: "lost response", refusal: func(string) error { return errWrite }},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := fixture(t)
			f.c.refuseCreate = refuseRetirement(row.refusal)
			events := &eventLog{}
			err := f.collectorWith(t, events).Step(t.Context())
			if row.quota {
				if err != nil || !slices.Equal(f.c.foreground, []string{"intent"}) || len(events.events) != 1 {
					t.Fatalf("a full quota: err=%v foreground=%v events=%d", err, f.c.foreground, len(events.events))
				}
				return
			}
			if err == nil || len(f.c.foreground) != 0 || len(events.events) != 0 || !f.intent(t).DeletionTimestamp.IsZero() {
				t.Fatalf("err=%v foreground=%v events=%d; want the refusal returned and the intent left as it was", err, f.c.foreground, len(events.events))
			}
		})
	}
}

func TestExceededQuotaReadsTheAPIServersRefusal(t *testing.T) {
	refusal := quotaRefused("results", "ptah-result-a-retired")
	for _, row := range []struct {
		name  string
		err   error
		quota quotaRefusal
		found bool
	}{
		{name: "refusal", err: refusal, found: true, quota: quotaRefusal{name: "results",
			usage: "requested: count/ptahresultrecords.operator.ptah.run=1, used: count/ptahresultrecords.operator.ptah.run=4, limited: count/ptahresultrecords.operator.ptah.run=4"}},
		{name: "wrapped refusal", err: fmt.Errorf("retire: %w", refusal), found: true, quota: quotaRefusal{name: "results",
			usage: "requested: count/ptahresultrecords.operator.ptah.run=1, used: count/ptahresultrecords.operator.ptah.run=4, limited: count/ptahresultrecords.operator.ptah.run=4"}},
		{name: "not a 403", err: apierrors.NewBadRequest(refusal.Error())},
		{name: "no quota name", err: apierrors.NewForbidden(recordResource, "a", errors.New("exceeded quota: Results!, requested: x"))},
		{name: "nil", err: nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			quota, found := exceededQuota(row.err)
			if found != row.found || quota != row.quota {
				t.Fatalf("read %+v, %v from %v; want %+v, %v", quota, found, row.err, row.quota, row.found)
			}
		})
	}
}

// While the original Job exists, admission refuses to start the window from a
// foreground deletion, and the quota keeps refusing the retirement record.
// Each refusal doubles the wait before the next attempt, up to five minutes,
// rather than sending both requests every thirty seconds until the Job goes.
func TestAQuotaRefusalBacksOffWhileForegroundRetirementCannotStart(t *testing.T) {
	f := fixture(t)
	published := f.c.creates
	f.f.Job.ResourceVersion = ""
	if err := f.c.Client.Create(t.Context(), f.f.Job); err != nil {
		t.Fatal(err)
	}
	f.c.refuseCreate = refuseRetirement(func(name string) error { return quotaRefused("results", name) })
	events := &eventLog{}
	c := f.collectorWith(t, events)
	intent := f.intent(t)
	root := metav1.PartialObjectMetadata{ObjectMeta: intent.ObjectMeta}
	key := retryHintKey{uid: intent.UID}
	waits := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for attempt, want := range waits {
		c.pending = []metav1.PartialObjectMetadata{root}
		if err := c.Step(t.Context()); err != nil {
			t.Fatal(err)
		}
		if f.c.creates-published != attempt+1 {
			t.Fatalf("attempt %d sent %d retirement CREATEs in all", attempt+1, f.c.creates-published)
		}
		if got := c.next[key].Sub(f.c.now); got != want {
			t.Fatalf("after refusal %d the root waits %v, want %v", attempt+1, got, want)
		}
		f.c.now = c.next[key].Add(-time.Second)
		c.pending = []metav1.PartialObjectMetadata{root}
		if err := c.Step(t.Context()); err != nil {
			t.Fatal(err)
		}
		if f.c.creates-published != attempt+1 {
			t.Fatalf("a pass a second before the wait ended sent a retirement CREATE")
		}
		f.c.now = c.next[key]
	}
	if len(f.c.foreground) != 0 || len(events.events) != len(waits) {
		t.Fatalf("foreground deletions of %v and %d Events while the Job exists, want none and one Event per refusal", f.c.foreground, len(events.events))
	}
	for _, e := range events.events {
		requireQuotaEvent(t, f, e, "retries with backoff")
	}

	// The Job goes. The next attempt starts the window, and the wait resets.
	if err := f.c.Client.Delete(t.Context(), f.f.Job); err != nil {
		t.Fatal(err)
	}
	c.pending = []metav1.PartialObjectMetadata{root}
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.c.foreground, []string{"intent"}) {
		t.Fatalf("foreground deletions of %v once the Job was gone, want the intent's", f.c.foreground)
	}
	if delay, found := c.backoff[key]; found {
		t.Fatalf("the root keeps a %v backoff after its window started", delay)
	}
	requireQuotaEvent(t, f, events.events[len(events.events)-1], "started from a foreground deletion")
}

// A quota that frees up admits the retirement record on the next attempt, and
// the root drops the wait it had built up, so the bounded backoff table holds
// only roots that are still refused.
func TestARetirementTheQuotaLaterAdmitsDropsItsBackoff(t *testing.T) {
	f := fixture(t)
	published := f.c.creates
	f.f.Job.ResourceVersion = ""
	if err := f.c.Client.Create(t.Context(), f.f.Job); err != nil {
		t.Fatal(err)
	}
	f.c.refuseCreate = refuseRetirement(func(name string) error { return quotaRefused("results", name) })
	c := f.collectorWith(t, &eventLog{})
	intent := f.intent(t)
	root := metav1.PartialObjectMetadata{ObjectMeta: intent.ObjectMeta}
	key := retryHintKey{uid: intent.UID}
	c.pending = []metav1.PartialObjectMetadata{root}
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if delay := c.backoff[key]; delay != quotaRetryFirst {
		t.Fatalf("the refused root waits %v, want %v", delay, quotaRetryFirst)
	}

	f.c.refuseCreate = nil
	f.c.now = c.next[key]
	c.pending = []metav1.PartialObjectMetadata{root}
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	retired := 0
	for _, record := range remaining(t, f.c) {
		if record.Spec.Type == "retired" {
			retired++
		}
	}
	if f.c.creates-published != 2 || retired != 1 || len(f.c.foreground) != 0 {
		t.Fatalf("sent %d retirement CREATEs, stored %d retirement records and started foreground deletions of %v, want 2, 1 and none",
			f.c.creates-published, retired, f.c.foreground)
	}
	if delay, found := c.backoff[key]; found {
		t.Fatalf("the root keeps a %v backoff after its retirement record was stored", delay)
	}
}

// The API server applied the foreground deletion and its response was lost.
// The next pass reads the deletion it stamped, sends nothing while the window
// runs, and collects the payload records at the instant the window ends. The
// garbage collector, which a fake client does not run, finishes the root.
func TestAnInterruptedForegroundRetirementFinishesOnTheNextPass(t *testing.T) {
	f := fixture(t)
	published := f.c.creates
	f.c.refuseCreate = refuseRetirement(func(name string) error { return quotaRefused("results", name) })
	f.c.loseForeground = true
	c := f.collectorWith(t, nil)
	if err := c.Step(t.Context()); !errors.Is(err, errWrite) {
		t.Fatalf("the lost response: %v", err)
	}
	f.c.loseForeground = false
	intent := f.intent(t)
	if intent.DeletionTimestamp.IsZero() {
		t.Fatal("the fixture did not apply the foreground deletion it lost the response to")
	}
	root := metav1.PartialObjectMetadata{ObjectMeta: intent.ObjectMeta}
	key := retryHintKey{uid: intent.UID}
	if _, found := c.backoff[key]; !found {
		t.Fatal("the lost response left no backoff, so this cannot show the next pass drops it")
	}
	f.c.now = c.next[key]
	c.pending = []metav1.PartialObjectMetadata{root}
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if delay, found := c.backoff[key]; found {
		t.Fatalf("the deleting root keeps a %v backoff it can never use again", delay)
	}
	ends := intent.DeletionTimestamp.Add(f.p.Window)
	if f.c.creates-published != 1 || len(f.c.foreground) != 1 || len(f.c.deletes) != 0 {
		t.Fatalf("the next pass sent %d CREATEs, %d foreground deletions and deletions of %v inside the window", f.c.creates-published, len(f.c.foreground), f.c.deletes)
	}
	if !c.next[key].Equal(ends) {
		t.Fatalf("the root waits until %v, want %v, the end of the window its deletionTimestamp started", c.next[key], ends)
	}
	f.c.now = ends
	c.pending = []metav1.PartialObjectMetadata{root}
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.c.deletes, []string{"complete", "chunk", "chunk"}) {
		t.Fatalf("deleted %v at the end of the window, want the completion and then each chunk", f.c.deletes)
	}
	left := remaining(t, f.c)
	if len(left) != 1 || left[0].Spec.Type != "intent" || left[0].DeletionTimestamp.IsZero() {
		t.Fatalf("%d records remain, want the deleting intent alone for the garbage collector", len(left))
	}
}

// The backoff table is bounded like the retry hints. Once it is full, a newly
// refused root waits the ordinary thirty seconds and takes no entry, so its
// wait does not grow either.
func TestAFullBackoffTableTakesNoNewRoot(t *testing.T) {
	f := fixture(t)
	f.f.Job.ResourceVersion = ""
	if err := f.c.Client.Create(t.Context(), f.f.Job); err != nil {
		t.Fatal(err)
	}
	f.c.refuseCreate = refuseRetirement(func(name string) error { return quotaRefused("results", name) })
	c := f.collectorWith(t, &eventLog{})
	for i := range maxRetryHints {
		c.backoff[retryHintKey{uid: types.UID(fmt.Sprintf("refused-%d", i))}] = quotaRetryMax
	}
	intent := f.intent(t)
	key := hintKey(intent)
	for attempt := 1; attempt <= 2; attempt++ {
		c.pending = []metav1.PartialObjectMetadata{{ObjectMeta: intent.ObjectMeta}}
		if err := c.Step(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := c.next[key].Sub(f.c.now); got != quotaRetryFirst {
			t.Fatalf("refusal %d beside a full table: the root waits %v, want %v", attempt, got, quotaRetryFirst)
		}
		if delay, found := c.backoff[key]; found || len(c.backoff) != maxRetryHints {
			t.Fatalf("refusal %d beside a full table: the root stored %v (stored=%v) and the table holds %d entries, want no entry and %d",
				attempt, delay, found, len(c.backoff), maxRetryHints)
		}
		f.c.now = c.next[key]
	}
	if f.c.foreground != nil {
		t.Fatalf("foreground deletions of %v while the Job exists", f.c.foreground)
	}
}

// A full quota can refuse every root on a page, and each fallback is one DELETE
// against the step's budget. A step starts at most maxDeletesPerStep of them.
// The next root waits at the front of the page with no Event and no backoff,
// because nothing refused it yet, and the next step starts it.
func TestAFullQuotaStartsOneBudgetOfFallbacksPerStep(t *testing.T) {
	f := fixture(t)
	b := f.f.Identity.Binding
	store := resultstore.Store{Client: f.c, Reader: f.c}
	for i := range maxDeletesPerStep {
		attempt := b
		attempt.OperationID = fmt.Sprintf("%s-%d", b.OperationID, i)
		attempt.JobName = fmt.Sprintf("%s-%d", b.JobName, i)
		attempt.JobUID = types.UID(fmt.Sprintf("job-%d", i))
		payload := fmt.Appendf(nil, "outcome %d", i)
		if _, err := store.Publish(t.Context(), attempt, payload, fmt.Sprintf("sha256:%x", sha256.Sum256(payload))); err != nil {
			t.Fatal(err)
		}
	}
	f.c.refuseCreate = refuseRetirement(func(name string) error { return quotaRefused("results", name) })
	events := &eventLog{}
	c := f.collectorWith(t, events)
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.c.foreground) != maxDeletesPerStep || len(events.events) != maxDeletesPerStep {
		t.Fatalf("one step started %d foreground retirements and recorded %d Events, want %d of each",
			len(f.c.foreground), len(events.events), maxDeletesPerStep)
	}
	for _, e := range events.events {
		requireQuotaEvent(t, f, e, "started from a foreground deletion")
	}
	var waiting []api.PtahResultRecord
	for _, record := range remaining(t, f.c) {
		if record.Spec.Type == "intent" && record.DeletionTimestamp.IsZero() {
			waiting = append(waiting, record)
		}
	}
	if len(waiting) != 1 {
		t.Fatalf("%d intents are not deleting after the first step, want the one the budget left", len(waiting))
	}
	if len(c.pending) == 0 || c.pending[0].UID != waiting[0].UID {
		t.Fatalf("the intent the budget left is not at the front of the page: %d pending", len(c.pending))
	}
	if len(c.backoff) != 0 {
		t.Fatalf("the backoff table holds %d entries after fallbacks that all started", len(c.backoff))
	}

	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.c.foreground) != maxDeletesPerStep+1 || len(events.events) != maxDeletesPerStep+1 {
		t.Fatalf("the next step brought the foreground retirements to %d and the Events to %d, want %d of each",
			len(f.c.foreground), len(events.events), maxDeletesPerStep+1)
	}
	requireQuotaEvent(t, f, events.events[maxDeletesPerStep], "started from a foreground deletion")
	left := &api.PtahResultRecord{}
	if err := f.c.Get(t.Context(), client.ObjectKeyFromObject(&waiting[0]), left); err != nil || left.DeletionTimestamp.IsZero() {
		t.Fatalf("the intent the budget left is not deleting after the next step: %v", err)
	}
}

// An attempt's result and its credential share one retirement record. Both
// back off while the Job exists. Once the quota has room, the result's pass
// stores the record and defers the credential to the record's deadline, so the
// credential does not come back to its refusal. The collection that removes
// both roots at that deadline drops the credential's backoff with it.
func TestCollectingAnAttemptDropsItsSiblingsBackoff(t *testing.T) {
	f := fixture(t)
	f.f.Job.ResourceVersion = ""
	if err := f.c.Client.Create(t.Context(), f.f.Job); err != nil {
		t.Fatal(err)
	}
	b := f.f.Identity.Binding
	// Ensure persists the credential before it checks live authority, which
	// this consumed operation no longer has, so only the record matters here.
	_, _ = (resultcredentials.PodBindings{Writer: f.c, Reader: f.c}).Ensure(t.Context(), f.f.Identity)
	credential := &api.PtahResultRecord{}
	if err := f.c.Get(t.Context(), client.ObjectKey{Namespace: b.Namespace, Name: jobconfig.CredentialName(b.UID, b.OperationID, b.JobName)}, credential); err != nil {
		t.Fatal(err)
	}
	f.c.refuseCreate = refuseRetirement(func(name string) error { return quotaRefused("results", name) })
	c := f.collectorWith(t, &eventLog{})
	intent := f.intent(t)
	intentKey, credentialKey := hintKey(intent), hintKey(credential)
	c.pending = []metav1.PartialObjectMetadata{{ObjectMeta: intent.ObjectMeta}, {ObjectMeta: credential.ObjectMeta}}
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, key := range []retryHintKey{intentKey, credentialKey} {
		if _, found := c.backoff[key]; !found {
			t.Fatalf("root %+v holds no backoff after its refused fallback", key)
		}
	}

	f.c.refuseCreate = nil
	f.c.now = c.next[intentKey]
	c.pending = []metav1.PartialObjectMetadata{{ObjectMeta: intent.ObjectMeta}}
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	name, _ := resultretention.Name(b)
	marker := &api.PtahResultRecord{}
	if err := f.c.Get(t.Context(), client.ObjectKey{Namespace: b.Namespace, Name: name}, marker); err != nil {
		t.Fatal(err)
	}
	deadline := marker.CreationTimestamp.Add(f.p.Window)
	if !c.next[credentialKey].Equal(deadline) {
		t.Fatalf("the credential is deferred until %v, want the retirement deadline %v", c.next[credentialKey], deadline)
	}
	if _, found := c.backoff[credentialKey]; !found {
		t.Fatal("the credential lost its backoff before the collection, so this cannot show the collection drops it")
	}

	if err := f.c.Client.Delete(t.Context(), f.f.Job); err != nil {
		t.Fatal(err)
	}
	f.c.now = deadline
	c.pending = []metav1.PartialObjectMetadata{{ObjectMeta: intent.ObjectMeta}}
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Get(t.Context(), client.ObjectKeyFromObject(credential), &api.PtahResultRecord{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the credential was not collected with its attempt: %v; %d records remain", err, len(remaining(t, f.c)))
	}
	if delay, found := c.backoff[credentialKey]; found {
		t.Fatalf("the collected credential keeps a %v backoff", delay)
	}
}
