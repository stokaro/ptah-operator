package resultcleanup

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultretention"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// One retained attempt can need both an intent-UID hint and a credential-name
// hint. The twenty-resource qualification fleet retains over 2,300 attempts
// inside its one-hour window. Keep both hints without evicting live deadlines;
// overflow still falls back to the complete policy, never an early deletion.
const maxRetryHints = 8192

// Start advances one step every five seconds. Expired results must not turn a
// metadata page into a burst of DELETEs and their admission reads while the
// controllers are dispatching current work. Count attempted writes, including
// refusals; an unfinished publication continues through the same live guards.
const maxDeletesPerStep = 10

var errDeleteBudget = errors.New("result cleanup deletion budget exhausted")

// A root whose retirement record a full ResourceQuota refuses, and whose
// foreground fallback cannot start either (its Job still exists, say), waits
// twice as long before each attempt, from thirty seconds to five minutes. Five
// minutes is a twelfth of the one-hour minimum window: the window starts at
// most that long after the fallback becomes possible.
const (
	quotaRetryFirst = 30 * time.Second
	quotaRetryMax   = 5 * time.Minute
)

// EventRecorder is the part of client-go's record.EventRecorder the collector
// uses.
type EventRecorder interface {
	Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...any)
}

// Collector scans metadata in bounded pages outside both reconciliation
// workers. It keeps no payload cache. The API's required admission guard
// authorizes each UID-bound DELETE with current reads; the collector does not
// repeat that entire verdict before sending the request. Only the leader scans.
type Collector struct {
	writer       client.Client
	policy       Policy
	recorder     EventRecorder
	query        int
	continuation string
	pending      []metav1.PartialObjectMetadata
	next         map[retryHintKey]time.Time
	// backoff is the delay a root last waited after a full ResourceQuota
	// refused its retirement and the foreground fallback could not start.
	// process takes a root's entry at the start of each pass, and only another
	// refused fallback stores it again. It shares the retry hints' bound and,
	// like them, authorizes nothing.
	backoff      map[retryHintKey]time.Duration
	operations   *prometheus.CounterVec
	deleteBudget *int // Set only during the serial Step call.
}

type retryHintKey struct {
	uid        types.UID
	credential client.ObjectKey
}

// hintKey names a credential root by name, which its replacement shares, and
// every other root by UID.
func hintKey(meta metav1.Object) retryHintKey {
	if meta.GetLabels()["app.kubernetes.io/component"] == "result-credential" {
		return retryHintKey{credential: client.ObjectKey{Namespace: meta.GetNamespace(), Name: meta.GetName()}}
	}
	return retryHintKey{uid: meta.GetUID()}
}

// New builds the collector. recorder receives the Warning Events that explain
// a retirement a ResourceQuota refused; a nil recorder records none.
func New(writer client.Client, policy Policy, registry prometheus.Registerer, recorder EventRecorder) (*Collector, error) {
	if writer == nil || policy.Reader == nil || policy.Window < resultretention.MinimumWindow || policy.Window%time.Second != 0 {
		return nil, errors.New("invalid result cleanup configuration")
	}
	c := &Collector{writer: writer, policy: policy, recorder: recorder, next: map[retryHintKey]time.Time{}, backoff: map[retryHintKey]time.Duration{}}
	if registry != nil {
		c.operations = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "ptah_operator_result_cleanup_operations_total", Help: "Result cleanup actions by bounded action and outcome."}, []string{"action", "outcome"})
		if err := registry.Register(c.operations); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (*Collector) NeedLeaderElection() bool { return true }

func (c *Collector) Start(ctx context.Context) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		stepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_ = c.Step(stepCtx) // Failed pages retry; no evidence is deleted on a failed check.
		cancel()
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (c *Collector) observe(action string, err error) {
	if c.operations == nil || errors.Is(err, errDeleteBudget) {
		return
	}
	outcome := "success"
	if err != nil {
		outcome = "error"
		if retained(err) {
			outcome = "retained"
		}
	}
	c.operations.WithLabelValues(action, outcome).Inc()
}

func retained(err error) bool {
	return errors.Is(err, ErrRetained) || errors.Is(err, resultretention.ErrPinned) || errors.Is(err, resultretention.ErrWindow)
}

// Step visits at most one 64-object metadata page and attempts at most ten
// DELETEs, retaining the unfinished page across a deadline or write budget.
// Errors on one attempt do not starve later attempts.
// Start calls it serially; tests can advance a complete scan without sleeping.
func (c *Collector) Step(ctx context.Context) error {
	remaining := maxDeletesPerStep
	c.deleteBudget = &remaining
	defer func() { c.deleteBudget = nil }()
	if len(c.pending) == 0 {
		queries := []labels.Selector{
			selector(resultstore.LabelRecord, selection.In, "intent", "retired"),
			selector("app.kubernetes.io/component", selection.Equals, "result-credential"),
		}
		list := metadataList()
		err := c.policy.Reader.List(ctx, list, &client.ListOptions{LabelSelector: queries[c.query], Limit: 64, Continue: c.continuation})
		c.observe("scan", err)
		if err != nil {
			if apierrors.IsResourceExpired(err) {
				c.continuation = ""
			}
			return err
		}
		c.pending = list.Items
		c.continuation = list.Continue
		if c.continuation == "" {
			c.query = (c.query + 1) % len(queries)
		}
	}
	var failures []error
	for len(c.pending) != 0 {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if remaining == 0 {
			return errors.Join(failures...)
		}
		meta := c.pending[0]
		c.pending = c.pending[1:]
		// The server timestamp already proves that a young retirement cannot
		// expire. Do not spend API reads or a retry hint on it. This only delays
		// collection: the complete record, longer recorded window and live pins
		// are checked again after this earliest possible deadline.
		if meta.Labels[resultstore.LabelRecord] == "retired" && !meta.CreationTimestamp.IsZero() &&
			c.policy.now().Before(meta.CreationTimestamp.Add(c.policy.Window)) {
			continue
		}
		key := hintKey(&meta)
		if next := c.next[key]; c.policy.now().Before(next) {
			continue
		}
		next, err := c.process(ctx, meta)
		if errors.Is(err, errDeleteBudget) {
			// The page cursor has already advanced. Keep this exact root at
			// the front instead of waiting for another scan or a retry hint.
			// Its next pass re-reads current identities, pins, and members.
			c.pending = append([]metav1.PartialObjectMetadata{meta}, c.pending...)
			return errors.Join(failures...)
		}
		if err != nil && !retained(err) {
			failures = append(failures, err)
		}
		c.deferUntil(key, next)
	}
	return errors.Join(failures...)
}

func (c *Collector) deferUntil(key retryHintKey, next time.Time) {
	if len(c.next) >= maxRetryHints {
		now := c.policy.now()
		for key, deadline := range c.next {
			if !now.Before(deadline) {
				delete(c.next, key)
			}
		}
	}
	// Never evict an unexpired hint to fit a new one. An uncached root still
	// passes the complete policy, and hints never authorize a deletion.
	if len(c.next) < maxRetryHints {
		c.next[key] = next
	}
}

func (c *Collector) process(ctx context.Context, meta metav1.PartialObjectMetadata) (time.Time, error) {
	retry := c.policy.now().Add(30 * time.Second)
	// A backoff lasts one refusal. Every outcome of this pass but another
	// refused fallback forgets it, and that one stores it again, doubled.
	key := hintKey(&meta)
	waited := c.backoff[key]
	delete(c.backoff, key)
	record := &api.PtahResultRecord{}
	if err := c.policy.Reader.Get(ctx, client.ObjectKeyFromObject(&meta), record); err != nil {
		if apierrors.IsNotFound(err) {
			return retry, nil
		}
		c.observe("retire", err)
		return retry, err
	}
	if record.UID != meta.UID {
		return retry, ErrRetained
	}
	b, err := RootBinding(record)
	if err != nil {
		c.observe("retire", err)
		return retry, err
	}
	name, _ := resultretention.Name(b)
	// A listed retirement was just read directly and UID-checked above.
	marker := record
	if record.Spec.Type != "retired" {
		marker = &api.PtahResultRecord{}
		err = c.policy.Reader.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: name}, marker)
	}
	if apierrors.IsNotFound(err) && record.Spec.Type != "retired" {
		if !record.DeletionTimestamp.IsZero() {
			if _, err := c.policy.foregroundBinding(ctx, record); err != nil {
				// The API server's deletionTimestamp fixes when this window
				// ends, and no later reading can bring that closer. Wait for it
				// rather than reading the root again every thirty seconds.
				if ends, isTime := resultretention.WindowEnd(err); isTime {
					return ends, err
				}
				return retry, err
			}
			err = c.collect(ctx, nil, record, b)
			c.observe("delete", err)
			return retry, err
		}
		if err := resultretention.CheckUnpinned(ctx, c.policy.Reader, b); err != nil {
			c.observe("retire", err)
			return retry, err
		}
		marker, err = resultretention.Record(b, resultretention.Source{Name: record.Name, UID: record.UID, Type: record.Spec.Type}, c.policy.Window)
		if err == nil {
			err = c.writer.Create(ctx, marker)
			// A successful CREATE returns the stored object, including its
			// server UID and retention timestamp. A concurrent writer instead
			// requires a fresh read of the winning immutable record.
			if apierrors.IsAlreadyExists(err) {
				err = c.policy.Reader.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: name}, marker)
			}
		}
		c.observe("retire", err)
		if quota, full := exceededQuota(err); full {
			return c.retireInForeground(ctx, key, waited, record, b, &quota)
		}
		if apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
			return c.retireInForeground(ctx, key, waited, record, b, nil)
		}
	}
	if err != nil {
		return retry, err
	}
	r, err := resultretention.Decode(marker)
	if err != nil || r.Binding != b || marker.UID == "" || marker.CreationTimestamp.IsZero() {
		return retry, resultretention.ErrRecord
	}
	deadline := marker.CreationTimestamp.Add(max(c.policy.Window, time.Duration(r.RetentionSeconds)*time.Second))
	if c.policy.now().Before(deadline) {
		// A persisted future deadline already requires retention. Checking
		// current pins cannot permit collection sooner. Eligible and DELETE
		// admission recheck those pins when the window has actually elapsed.
		// The same deadline defers the credential scan. Its name-based hint
		// can only postpone a replacement's check, never authorize deletion.
		c.deferUntil(retryHintKey{credential: client.ObjectKey{Namespace: b.Namespace,
			Name: jobconfig.CredentialName(b.UID, b.OperationID, b.JobName)}}, deadline)
		return deadline, resultretention.ErrWindow
	}
	if err := resultretention.Eligible(ctx, c.policy.Reader, marker, c.policy.now(), c.policy.Window); err != nil {
		c.observe("retire", err)
		return retry, err
	}
	err = c.collect(ctx, marker, record, b)
	c.observe("delete", err)
	return retry, err
}

// retireInForeground starts the window with a foreground deletion of the root
// when the API refuses the record that would retire it: a terminating
// namespace refuses every CREATE, and a full ResourceQuota on result records
// refuses this one. The deletionTimestamp the API server stamps is then the
// retirement clock. Admission permits only the start of this deletion and holds
// its finalizer until AuthorizeDelete permits destroying the bytes, so an
// interrupted fallback leaves either the root as it was, which the next pass
// retires again, or a deleting root, which the next pass collects once its
// window ends. quota is nil for a terminating namespace. waited is the backoff
// the root's previous refusal set, which process has already taken from the
// table.
func (c *Collector) retireInForeground(ctx context.Context, key retryHintKey, waited time.Duration, record *api.PtahResultRecord, b resultstore.Binding, quota *quotaRefusal) (time.Time, error) {
	retry := c.policy.now().Add(30 * time.Second)
	err := c.policy.AuthorizeForegroundRetirement(ctx, record)
	if err == nil {
		// Step checks its budget before each root, and nothing on the way here
		// deletes, so this DELETE always has budget and never answers
		// errDeleteBudget. TestAFullQuotaStartsOneBudgetOfFallbacksPerStep
		// fails if a root starts spending more than this one DELETE.
		err = c.deleteRecord(ctx, record, &client.DeleteOptions{
			Preconditions:     &metav1.Preconditions{UID: &record.UID, ResourceVersion: &record.ResourceVersion},
			PropagationPolicy: ptr.To(metav1.DeletePropagationForeground),
		})
	}
	c.observe("retire", err)
	if quota == nil {
		return retry, err
	}
	c.warnQuota(b, record.Name, *quota, err == nil)
	if err == nil {
		return retry, nil
	}
	delay := min(max(2*waited, quotaRetryFirst), quotaRetryMax)
	// The table shares the retry hints' bound. A root that held an entry has
	// room for it again: process removed it at the start of this pass.
	if len(c.backoff) < maxRetryHints {
		c.backoff[key] = delay
	}
	return c.policy.now().Add(delay), err
}

// quotaRefusal is a ResourceQuota's refusal of a CREATE.
type quotaRefusal struct {
	// name is the ResourceQuota's.
	name string
	// usage is the API server's account of it: requested, used and limited.
	usage string
}

// exceededQuota reads which ResourceQuota refused a CREATE. The quota admission
// plugin says so only in the message of its 403, as admission.NewForbidden
// wraps "exceeded quota: <name>, requested: ..., used: ..., limited: ..." in
// k8s.io/apiserver/pkg/admission/plugin/resourcequota. The API server does not
// wrap a webhook's denial this way: it reports `admission webhook "<name>"
// denied the request: ...`. A webhook whose own message quotes these words
// could still match, and that would only start a foreground deletion that
// admission grants or refuses on its own terms, under an Event that names the
// wrong quota.
func exceededQuota(err error) (quotaRefusal, bool) {
	var status apierrors.APIStatus
	if !apierrors.IsForbidden(err) || !errors.As(err, &status) {
		return quotaRefusal{}, false
	}
	_, refusal, found := strings.Cut(status.Status().Message, " is forbidden: exceeded quota: ")
	if !found {
		return quotaRefusal{}, false
	}
	name, usage, found := strings.Cut(refusal, ", ")
	if !found || len(validation.IsDNS1123Subdomain(name)) != 0 {
		return quotaRefusal{}, false
	}
	return quotaRefusal{name: name, usage: usage}, true
}

// warnQuota tells the resource that ran the operation why its result could not
// be retired the ordinary way. The same full quota also refuses the results its
// next operations publish. The message names the quota and quotes the API
// server's account of it, which holds no credential or payload.
func (c *Collector) warnQuota(b resultstore.Binding, root string, quota quotaRefusal, started bool) {
	if c.recorder == nil {
		return
	}
	subject := &corev1.ObjectReference{APIVersion: api.GroupVersion.String(), Kind: b.Kind, Namespace: b.Namespace, Name: b.Name, UID: b.UID}
	outcome := "so its retention window started from a foreground deletion of the record instead"
	if !started {
		outcome = "and the collector could not start its retention window from a foreground deletion either, so it retries with backoff"
	}
	c.recorder.Eventf(subject, corev1.EventTypeWarning, "ResultRetirementQuotaExceeded",
		"ResourceQuota %s refused the record that retires result record %s (%s), %s. Leave room in the quota for one retirement record per operation.",
		quota.name, root, quota.usage, outcome)
}

func (c *Collector) collect(ctx context.Context, marker, root *api.PtahResultRecord, b resultstore.Binding) error {
	if marker == nil {
		// A credential can outlive its original intent. Never use its attempt
		// index to collect a restored intent carrying a different full binding.
		name, _ := resultstore.Name(b)
		intent := &api.PtahResultRecord{}
		if err := c.policy.Reader.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: name}, intent); err == nil {
			binding, err := RootBinding(intent)
			if err != nil || binding != b {
				return ErrRetained
			}
		} else if !apierrors.IsNotFound(err) {
			return err
		}
	}
	// Avoid sending deletions while the original Job still fences this attempt.
	// Admission repeats this check at each write, along with current recovery
	// pins, source identity, record age, and dependency order.
	if err := c.policy.requireJobAbsent(ctx, b); err != nil {
		return err
	}
	// The member index also finds children left after an interrupted restore
	// omitted their intent. Admission checks each child's immutable binding.
	members, err := c.policy.members(ctx, b)
	if err != nil {
		return err
	}
	for _, meta := range members {
		if err := c.removeMetadata(ctx, meta.ObjectMeta); err != nil {
			return err
		}
	}
	intentName, _ := resultstore.Name(b)
	for _, name := range []string{intentName, jobconfig.CredentialName(b.UID, b.OperationID, b.JobName)} {
		var err error
		if root != nil && root.Namespace == b.Namespace && root.Name == name {
			err = c.removeMetadata(ctx, root.ObjectMeta)
		} else {
			err = c.remove(ctx, client.ObjectKey{Namespace: b.Namespace, Name: name})
		}
		if err != nil {
			return err
		}
	}
	if marker != nil {
		return c.removeMetadata(ctx, marker.ObjectMeta)
	}
	return nil
}

func (c *Collector) remove(ctx context.Context, key client.ObjectKey) error {
	record := &api.PtahResultRecord{}
	if err := c.policy.Reader.Get(ctx, key, record); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return c.removeMetadata(ctx, record.ObjectMeta)
}

// The scan or root read already supplied the version to delete. Fetching its
// payload again adds no authority: admission checks the API server's current
// object, and both preconditions retain anything replaced or updated since
// this reading. A conflict retries through a fresh scan.
func (c *Collector) removeMetadata(ctx context.Context, meta metav1.ObjectMeta) error {
	if meta.UID == "" || meta.ResourceVersion == "" {
		return ErrRetained
	}
	if !meta.DeletionTimestamp.IsZero() && len(meta.Finalizers) == 1 && meta.Finalizers[0] == metav1.FinalizerDeleteDependents {
		// The garbage collector finishes the foreground DELETE already in
		// progress. Admission applies the same policy to its finalizer removal.
		return nil
	}
	// The API admission guard must authorize this exact object. A denied or
	// unavailable webhook leaves it intact; no collector decision bypasses it.
	record := &api.PtahResultRecord{ObjectMeta: meta}
	options := &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &record.UID, ResourceVersion: &record.ResourceVersion}, PropagationPolicy: ptr.To(metav1.DeletePropagationBackground)}
	if err := c.deleteRecord(ctx, record, options); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	// collect also removes the attempt's other root, which can still hold the
	// backoff of a refusal its sibling's retirement record has since answered.
	delete(c.backoff, hintKey(&meta))
	// The same metadata page may still contain this root's retirement entry.
	// Deferring that exact UID avoids a redundant NotFound read. These bounded
	// hints only postpone scans; they cannot authorize a replacement's DELETE.
	if role := meta.Labels[resultstore.LabelRecord]; role == "intent" || role == "retired" {
		c.deferUntil(retryHintKey{uid: meta.UID}, c.policy.now().Add(30*time.Second))
	}
	if meta.Labels["app.kubernetes.io/component"] == "result-credential" {
		c.deferUntil(retryHintKey{credential: client.ObjectKeyFromObject(record)}, c.policy.now().Add(30*time.Second))
	}
	return nil
}

func (c *Collector) deleteRecord(ctx context.Context, record *api.PtahResultRecord, options ...client.DeleteOption) error {
	if c.deleteBudget != nil {
		if *c.deleteBudget == 0 {
			return errDeleteBudget
		}
		*c.deleteBudget -= 1
	}
	return c.writer.Delete(ctx, record, options...)
}
