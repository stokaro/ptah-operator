package resultcleanup

import (
	"context"
	"errors"
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
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Collector scans metadata in bounded pages outside both reconciliation
// workers. It keeps no payload cache and deletes through the same policy the
// webhook independently applies. Only the elected manager scans.
type Collector struct {
	writer       client.Client
	policy       Policy
	query        int
	continuation string
	pending      []metav1.PartialObjectMetadata
	next         map[retryHintKey]time.Time
	operations   *prometheus.CounterVec
}

type retryHintKey struct {
	uid        types.UID
	credential client.ObjectKey
}

func New(writer client.Client, policy Policy, registry prometheus.Registerer) (*Collector, error) {
	if writer == nil || policy.Reader == nil || policy.Window < resultretention.MinimumWindow || policy.Window%time.Second != 0 {
		return nil, errors.New("invalid result cleanup configuration")
	}
	c := &Collector{writer: writer, policy: policy, next: map[retryHintKey]time.Time{}}
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
	if c.operations == nil {
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

// Step visits at most one 64-object metadata page, retaining the unfinished
// page across a deadline. Errors on one attempt do not starve later attempts.
// Start calls it serially; tests can advance a complete scan without sleeping.
func (c *Collector) Step(ctx context.Context) error {
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
		key := retryHintKey{uid: meta.UID}
		if meta.Labels["app.kubernetes.io/component"] == "result-credential" {
			key = retryHintKey{credential: client.ObjectKeyFromObject(&meta)}
		}
		if next := c.next[key]; c.policy.now().Before(next) {
			continue
		}
		next, err := c.process(ctx, meta)
		if err != nil && !retained(err) {
			failures = append(failures, err)
		}
		c.deferUntil(key, next)
	}
	return errors.Join(failures...)
}

func (c *Collector) deferUntil(key retryHintKey, next time.Time) {
	if len(c.next) >= 4096 {
		now := c.policy.now()
		for key, deadline := range c.next {
			if !now.Before(deadline) {
				delete(c.next, key)
			}
		}
	}
	// Never evict an unexpired hint to fit a new one. An uncached root still
	// passes the complete policy, and hints never authorize a deletion.
	if len(c.next) < 4096 {
		c.next[key] = next
	}
}

func (c *Collector) process(ctx context.Context, meta metav1.PartialObjectMetadata) (time.Time, error) {
	retry := c.policy.now().Add(30 * time.Second)
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
	if err := resultretention.CheckUnpinned(ctx, c.policy.Reader, b); err != nil {
		c.observe("retire", err)
		return retry, err
	}
	name, _ := resultretention.Name(b)
	marker := &api.PtahResultRecord{}
	err = c.policy.Reader.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: name}, marker)
	if apierrors.IsNotFound(err) && record.Spec.Type != "retired" {
		if !record.DeletionTimestamp.IsZero() {
			if _, err := c.policy.foregroundBinding(ctx, record); err != nil {
				return retry, err
			}
			err = c.collect(ctx, nil, b)
			c.observe("delete", err)
			return retry, err
		}
		marker, err = resultretention.Record(b, resultretention.Source{Name: record.Name, UID: record.UID, Type: record.Spec.Type}, c.policy.Window)
		if err == nil {
			err = c.writer.Create(ctx, marker)
			if err == nil || apierrors.IsAlreadyExists(err) {
				err = c.policy.Reader.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: name}, marker)
			}
		}
		c.observe("retire", err)
		if apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
			if err := c.policy.AuthorizeForegroundRetirement(ctx, record); err != nil {
				return retry, err
			}
			err = c.writer.Delete(ctx, record, &client.DeleteOptions{
				Preconditions:     &metav1.Preconditions{UID: &record.UID, ResourceVersion: &record.ResourceVersion},
				PropagationPolicy: ptr.To(metav1.DeletePropagationForeground),
			})
			c.observe("retire", err)
			return retry, err
		}
	}
	if err != nil {
		return retry, err
	}
	r, err := resultretention.Decode(marker)
	if err != nil || r.Binding != b {
		return retry, resultretention.ErrRecord
	}
	if err := resultretention.Eligible(ctx, c.policy.Reader, marker, c.policy.now(), c.policy.Window); err != nil {
		if errors.Is(err, resultretention.ErrWindow) {
			deadline := marker.CreationTimestamp.Add(max(c.policy.Window, time.Duration(r.RetentionSeconds)*time.Second))
			// The same retirement also retains this attempt's credential. Its
			// later metadata scan need not read the same window again. Keying
			// this deferral by name can only postpone a replacement's check;
			// deletion still verifies its actual UID, age and live pins.
			c.deferUntil(retryHintKey{credential: client.ObjectKey{Namespace: b.Namespace,
				Name: jobconfig.CredentialName(b.UID, b.OperationID, b.JobName)}}, deadline)
			return deadline, err
		}
		return retry, err
	}
	err = c.collect(ctx, marker, b)
	c.observe("delete", err)
	return retry, err
}

func (c *Collector) collect(ctx context.Context, marker *api.PtahResultRecord, b resultstore.Binding) error {
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
	// The member index also finds children left after an interrupted restore
	// omitted their intent. Admission checks each child's immutable binding.
	members, err := c.policy.members(ctx, b)
	if err != nil {
		return err
	}
	for _, meta := range members {
		if err := c.remove(ctx, client.ObjectKeyFromObject(&meta), meta.UID); err != nil {
			return err
		}
	}
	intentName, _ := resultstore.Name(b)
	for _, name := range []string{intentName, jobconfig.CredentialName(b.UID, b.OperationID, b.JobName)} {
		if err := c.remove(ctx, client.ObjectKey{Namespace: b.Namespace, Name: name}, ""); err != nil {
			return err
		}
	}
	if marker != nil {
		return c.remove(ctx, client.ObjectKeyFromObject(marker), marker.UID)
	}
	return nil
}

func (c *Collector) remove(ctx context.Context, key client.ObjectKey, uid types.UID) error {
	record := &api.PtahResultRecord{}
	if err := c.policy.Reader.Get(ctx, key, record); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if (uid != "" && record.UID != uid) || record.ResourceVersion == "" {
		return ErrRetained
	}
	if err := c.policy.AuthorizeDelete(ctx, record); err != nil {
		return err
	}
	if !record.DeletionTimestamp.IsZero() && len(record.Finalizers) == 1 && record.Finalizers[0] == metav1.FinalizerDeleteDependents {
		// The garbage collector finishes the foreground DELETE already in
		// progress. Admission applies the same policy to its finalizer removal.
		return nil
	}
	options := &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &record.UID, ResourceVersion: &record.ResourceVersion}, PropagationPolicy: ptr.To(metav1.DeletePropagationBackground)}
	if err := c.writer.Delete(ctx, record, options); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
