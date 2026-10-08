package resultcleanup

import (
	"context"
	"errors"
	"testing"
	"time"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultretention"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type policyReads struct {
	client.Reader
	gets map[client.ObjectKey]int
	fail client.ObjectKey
}

func TestCollectorReusesItsRetirementRead(t *testing.T) {
	f := fixture(t)
	f.retire(t)
	reader := &policyReads{Reader: f.c, gets: map[client.ObjectKey]int{}}
	f.p.Reader = reader
	collector := f.collector(t)
	if _, err := collector.process(t.Context(), metav1.PartialObjectMetadata{ObjectMeta: f.marker.ObjectMeta}); err != nil {
		t.Fatal(err)
	}
	if len(f.c.deletes) != 5 || len(remaining(t, f.c)) != 0 {
		t.Fatalf("incomplete collection: deleted %v", f.c.deletes)
	}
	// Each DELETE independently reads the marker and current recovery pins.
	// The collector needs one pin preflight, one marker read to process the
	// listed record, and one marker read for its UID/RV-bound final DELETE.
	if got, want := reader.gets[client.ObjectKeyFromObject(f.marker)], len(f.c.deletes)+2; got != want {
		t.Errorf("retirement reads: %d, want %d", got, want)
	}
	if got, want := reader.gets[client.ObjectKeyFromObject(f.f.Subject)], len(f.c.deletes)+1; got != want {
		t.Errorf("recovery pin reads: %d, want %d", got, want)
	}
}

func TestCollectorRetainsNewRetirementWithoutAnotherPinRead(t *testing.T) {
	f := fixture(t)
	reader := &policyReads{Reader: f.c, gets: map[client.ObjectKey]int{}}
	f.p.Reader = reader
	collector := f.collector(t)
	if err := collector.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.c.deletes) != 0 || len(remaining(t, f.c)) != 5 {
		t.Fatal("new retirement did not retain all evidence")
	}
	if got := reader.gets[client.ObjectKeyFromObject(f.f.Subject)]; got != 1 {
		t.Fatalf("collector made %d pin reads, want one before creating retirement", got)
	}
	// A fresh collector must recheck a restored pin once the window expires.
	// Knowing the deadline never grants permission to remove evidence.
	subject := f.f.Subject.(*api.PtahSchema)
	subject.Status.PendingObservation = &api.PendingObservationStatus{ApplyOperationID: f.f.Identity.Binding.OperationID}
	if err := f.c.Update(t.Context(), subject); err != nil {
		t.Fatal(err)
	}
	f.c.now = f.c.now.Add(time.Hour)
	clear(reader.gets)
	if err := f.collector(t).Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if reader.gets[client.ObjectKeyFromObject(subject)] == 0 || len(f.c.deletes) != 0 || len(remaining(t, f.c)) != 5 {
		t.Fatal("expired retirement skipped its restored recovery pin")
	}
}

func (r *policyReads) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	r.gets[key]++
	if key == r.fail {
		return errWrite
	}
	return r.Reader.Get(ctx, key, object, opts...)
}

func TestCollectionDoesNotDuplicateDeleteAdmissionReads(t *testing.T) {
	f := fixture(t)
	f.retire(t)
	reader := &policyReads{Reader: f.c, gets: map[client.ObjectKey]int{}}
	f.p.Reader = reader
	if err := f.collector(t).collect(t.Context(), f.marker, f.f.Identity.Binding); err != nil {
		t.Fatal(err)
	}
	if len(f.c.deletes) != 5 || len(remaining(t, f.c)) != 0 {
		t.Fatalf("incomplete collection: deleted %v", f.c.deletes)
	}
	job := client.ObjectKeyFromObject(f.f.Job)
	if got := reader.gets[job]; got != len(f.c.deletes)+1 {
		t.Fatalf("%d deletes made %d original-Job absence reads; want one collection preflight and one per admitted DELETE", len(f.c.deletes), got)
	}
}

func TestDeleteAdmissionSeesPinRestoredAfterCollectorRead(t *testing.T) {
	f := fixture(t)
	f.retire(t)
	f.c.beforeDelete = func(client.Object) {
		f.c.beforeDelete = nil
		subject := f.f.Subject.(*api.PtahSchema)
		subject.Status.PendingObservation = &api.PendingObservationStatus{ApplyOperationID: f.f.Identity.Binding.OperationID}
		if err := f.c.Update(t.Context(), subject); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.collector(t).collect(t.Context(), f.marker, f.f.Identity.Binding); !errors.Is(err, resultretention.ErrPinned) {
		t.Fatalf("the API accepted deletion after a recovery pin was restored: %v", err)
	}
	if len(f.c.deletes) != 0 || len(remaining(t, f.c)) != 5 {
		t.Fatal("the restored pin did not preserve every result record")
	}
}

func TestDeletionReadsSharedSourceAndOwnerOnce(t *testing.T) {
	for _, role := range []string{"complete", "chunk"} {
		t.Run(role, func(t *testing.T) {
			f := fixture(t)
			f.retire(t)
			var target api.PtahResultRecord
			for _, record := range remaining(t, f.c) {
				if role == "chunk" && record.Spec.Type == "complete" {
					if err := f.c.Client.Delete(t.Context(), &record); err != nil {
						t.Fatal(err)
					}
				}
				if record.Spec.Type == role {
					target = record
				}
			}
			if target.UID == "" {
				t.Fatal("missing published member")
			}
			owner := client.ObjectKey{Namespace: target.Namespace, Name: target.OwnerReferences[0].Name}
			reader := &policyReads{Reader: f.c, gets: map[client.ObjectKey]int{}}
			f.p.Reader = reader
			if err := f.p.AuthorizeDelete(t.Context(), &target); err != nil {
				t.Fatal(err)
			}
			if got := reader.gets[owner]; got != 1 {
				t.Fatalf("one deletion read the same source/owner %d times; want one current API read", got)
			}
			reader.fail = owner
			if err := f.p.AuthorizeDelete(t.Context(), &target); !errors.Is(err, errWrite) {
				t.Fatalf("unavailable current owner did not retain evidence: %v", err)
			}
		})
	}
}

func TestDeletionStillReadsOwnerWhenRetirementHasAnotherSource(t *testing.T) {
	f := fixture(t)
	b := f.f.Identity.Binding
	credentialName := jobconfig.CredentialName(b.UID, b.OperationID, b.JobName)
	var err error
	f.marker, err = resultretention.Record(b,
		resultretention.Source{Name: credentialName, UID: "original-credential", Type: "credential"}, f.p.Window)
	if err != nil {
		t.Fatal(err)
	}
	f.retire(t)
	var target api.PtahResultRecord
	for _, record := range remaining(t, f.c) {
		if record.Spec.Type == "complete" {
			target = record
		}
	}
	if target.UID == "" {
		t.Fatal("missing completion")
	}
	owner := client.ObjectKey{Namespace: target.Namespace, Name: target.OwnerReferences[0].Name}
	reader := &policyReads{Reader: f.c, gets: map[client.ObjectKey]int{}, fail: owner}
	f.p.Reader = reader
	if err := f.p.AuthorizeDelete(t.Context(), &target); !errors.Is(err, errWrite) {
		t.Fatalf("missing separate source bypassed the live owner read: %v", err)
	}
	if reader.gets[owner] != 1 || reader.gets[client.ObjectKey{Namespace: target.Namespace, Name: credentialName}] != 1 {
		t.Fatalf("expected current source and owner reads: %v", reader.gets)
	}
}
