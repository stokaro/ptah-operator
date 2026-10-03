package resultretention_test

import (
	"context"
	"errors"
	"testing"
	"time"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultauthority"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	"github.com/stokaro/ptah-operator/internal/resultretention"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func marker(t *testing.T, f *resulttest.Fixture, window time.Duration) *api.PtahResultRecord {
	t.Helper()
	b := f.Identity.Binding
	r, err := resultretention.Record(b, resultretention.Source{Type: "credential", Name: jobconfig.CredentialName(b.UID, b.OperationID, b.JobName), UID: "source-uid"}, window)
	if err != nil {
		t.Fatal(err)
	}
	r.UID = "retirement-uid"
	return r
}

func TestRetirementWindowStartsAtAPIRecordAndKeepsLongerPolicy(t *testing.T) {
	f := resulttest.New(t, "schema-observe")
	f.Subject.(*api.PtahSchema).Status.ActiveOperation = nil
	c := f.Client(t)
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name             string
		age, saved, live time.Duration
		missing, pinned  bool
		allow            bool
	}{
		{name: "new marker for old result", age: 0, saved: time.Hour, live: time.Hour},
		{name: "just before deadline", age: time.Hour - time.Nanosecond, saved: time.Hour, live: time.Hour},
		{name: "at deadline", age: time.Hour, saved: time.Hour, live: time.Hour, allow: true},
		{name: "longer saved window", age: time.Hour, saved: 2 * time.Hour, live: time.Hour},
		{name: "longer current window", age: time.Hour, saved: time.Hour, live: 2 * time.Hour},
		{name: "both elapsed", age: 2 * time.Hour, saved: 2 * time.Hour, live: time.Hour, allow: true},
		{name: "future marker", age: -time.Hour, saved: time.Hour, live: time.Hour},
		{name: "missing API time", missing: true, age: 2 * time.Hour, saved: time.Hour, live: time.Hour},
		{name: "proof still pending", pinned: true, age: 2 * time.Hour, saved: time.Hour, live: time.Hour},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := marker(t, f, test.saved)
			if !test.missing {
				r.CreationTimestamp = metav1.NewTime(now.Add(-test.age))
			}
			schema := f.Subject.(*api.PtahSchema)
			schema.Status.PendingObservation = nil
			if test.pinned {
				schema.Status.PendingObservation = &api.PendingObservationStatus{ApplyOperationID: f.Identity.Binding.OperationID}
			}
			if err := c.Update(t.Context(), schema); err != nil {
				t.Fatal(err)
			}
			if err := resultretention.Eligible(t.Context(), c, r, now, test.live); (err == nil) != test.allow {
				t.Fatalf("allow=%v: %v", test.allow, err)
			}
		})
	}
}

type failedReader struct{ client.Reader }

func (failedReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("API unavailable")
}

func TestRetirementFencesRestoredClaimsAndUnreadableRecords(t *testing.T) {
	for _, family := range []string{"schema-observe", "migration-history"} {
		f := resulttest.New(t, family)
		for _, corrupt := range []bool{false, true} {
			r := marker(t, f, time.Hour)
			if corrupt {
				r.Spec.Data = []byte("unreadable")
			}
			c := f.Client(t, r)
			if err := (resultauthority.Authorizer{Reader: c}).Check(t.Context(), f.Identity); !errors.Is(err, resultdelivery.ErrAuthority) {
				t.Fatalf("restored active claim accepted (corrupt=%v): %v", corrupt, err)
			}
		}
		if err := resultretention.CheckOpen(t.Context(), failedReader{}, f.Identity.Binding); err == nil {
			t.Fatal("unavailable retirement store granted authority")
		}
		if err := resultretention.CheckUnpinned(t.Context(), failedReader{}, f.Identity.Binding); err == nil {
			t.Fatal("unavailable resource store permitted cleanup")
		}
	}
}

type retiringReader struct {
	client.Reader
	retire func() error
}

func (r retiringReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := r.Reader.List(ctx, list, opts...); err != nil {
		return err
	}
	return r.retire()
}

func TestRetirementDuringAuthorityCheckIsObserved(t *testing.T) {
	f := resulttest.New(t, "schema-observe")
	c := f.Client(t)
	inserted := false
	reader := retiringReader{Reader: c, retire: func() error {
		inserted = true
		return c.Create(t.Context(), marker(t, f, time.Hour))
	}}
	if err := (resultauthority.Authorizer{Reader: reader}).Check(t.Context(), f.Identity); !errors.Is(err, resultdelivery.ErrAuthority) || !inserted {
		t.Fatalf("mid-check retirement was missed: inserted=%v error=%v", inserted, err)
	}
}

func TestRetirementRecordCannotMoveSourceBindingOrWindow(t *testing.T) {
	f := resulttest.New(t, "schema-observe")
	r := marker(t, f, time.Hour)
	for name, mutate := range map[string]func(*api.PtahResultRecord){
		"owner UID":  func(r *api.PtahResultRecord) { r.OwnerReferences[0].UID = "other" },
		"namespace":  func(r *api.PtahResultRecord) { r.Namespace = "other" },
		"name":       func(r *api.PtahResultRecord) { r.Name += "other" },
		"annotation": func(r *api.PtahResultRecord) { r.Annotations = map[string]string{"retired-at": "yesterday"} },
		"unknown data": func(r *api.PtahResultRecord) {
			r.Spec.Data = append(r.Spec.Data[:len(r.Spec.Data)-1], []byte(`,"override":true}`)...)
		},
		"finalizer": func(r *api.PtahResultRecord) { r.Finalizers = []string{"hold"} },
	} {
		t.Run(name, func(t *testing.T) {
			changed := r.DeepCopy()
			mutate(changed)
			if _, err := resultretention.Decode(changed); err == nil {
				t.Fatal("retirement metadata mutation accepted")
			}
		})
	}
}
