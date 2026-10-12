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
	gets        map[client.ObjectKey]int
	fail        client.ObjectKey
	recordsOnly bool
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
	// The collector reads the marker once, and every other object's DELETE
	// reads it again. The marker's own admission uses the API-supplied object.
	// Every DELETE still independently reads current recovery pins.
	if got, want := reader.gets[client.ObjectKeyFromObject(f.marker)], len(f.c.deletes); got != want {
		t.Errorf("retirement reads: %d, want %d", got, want)
	}
	if got, want := reader.gets[client.ObjectKeyFromObject(f.f.Subject)], len(f.c.deletes)+1; got != want {
		t.Errorf("recovery pin reads: %d, want %d", got, want)
	}
}

func TestCollectorDeletesFromReadMetadataWithoutReloadingPayloads(t *testing.T) {
	f := fixture(t)
	f.retire(t)
	var intent api.PtahResultRecord
	for _, record := range remaining(t, f.c) {
		if record.Spec.Type == "intent" {
			intent = record
		}
	}
	if intent.UID == "" {
		t.Fatal("missing intent")
	}
	collector := f.collector(t)
	// Count only the collector's requests. Admission keeps its own live
	// reader and must still authorize every DELETE independently.
	reader := &policyReads{Reader: f.c, gets: map[client.ObjectKey]int{}, recordsOnly: true}
	collector.policy.Reader = reader
	collector.pending = []metav1.PartialObjectMetadata{
		{ObjectMeta: intent.ObjectMeta}, {ObjectMeta: f.marker.ObjectMeta},
	}
	if err := collector.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.c.deletes) != 5 || len(remaining(t, f.c)) != 0 {
		t.Fatalf("incomplete collection: deleted %v", f.c.deletes)
	}
	// The initial intent, its retirement, and the absent credential are the
	// only full reads. The member list already carries each child's UID/RV,
	// and the later retirement entry in this same page was just deleted.
	if len(reader.gets) != 3 {
		t.Fatalf("collector reloaded records already read or deleted: %v", reader.gets)
	}
	for key, count := range reader.gets {
		if count != 1 {
			t.Errorf("collector read %s %d times, want once", key, count)
		}
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
	if _, record := object.(*api.PtahResultRecord); record || !r.recordsOnly {
		r.gets[key]++
	}
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
	if err := f.collector(t).collect(t.Context(), f.marker, nil, f.f.Identity.Binding); err != nil {
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
	if err := f.collector(t).collect(t.Context(), f.marker, nil, f.f.Identity.Binding); !errors.Is(err, resultretention.ErrPinned) {
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

func TestDeleteAdmissionDoesNotReloadItsOwnImmutableObject(t *testing.T) {
	for _, role := range []string{"intent", "retired"} {
		t.Run(role, func(t *testing.T) {
			f := fixture(t)
			f.retire(t)
			var target api.PtahResultRecord
			for _, record := range remaining(t, f.c) {
				if record.Spec.Type == role {
					target = record
					continue
				}
				if role == "intent" && record.Spec.Type == "retired" {
					continue
				}
				if err := f.c.Client.Delete(t.Context(), &record); err != nil {
					t.Fatal(err)
				}
			}
			if target.UID == "" {
				t.Fatal("missing API admission object")
			}
			reader := &policyReads{Reader: f.c, gets: map[client.ObjectKey]int{}}
			f.p.Reader = reader
			if err := f.p.AuthorizeDelete(t.Context(), &target); err != nil {
				t.Fatal(err)
			}
			if got := reader.gets[client.ObjectKeyFromObject(&target)]; got != 0 {
				t.Fatalf("admission reloaded its API-supplied immutable %s %d times", role, got)
			}
			// Authority from other objects remains live and fail-closed.
			reader.fail = client.ObjectKeyFromObject(f.f.Subject)
			if err := f.p.AuthorizeDelete(t.Context(), &target); !errors.Is(err, errWrite) {
				t.Fatalf("%s skipped its current recovery-pin read: %v", role, err)
			}
		})
	}
}

// A retirement can disappear only after both roots have disappeared. Its
// source read must be the final absence check, not an earlier duplicate.
func TestRetirementDeletionReadsItsAbsentSourceOnce(t *testing.T) {
	for _, sourceType := range []string{"intent", "credential"} {
		t.Run(sourceType, func(t *testing.T) {
			f := fixture(t)
			if sourceType == "credential" {
				b := f.f.Identity.Binding
				var err error
				f.marker, err = resultretention.Record(b, resultretention.Source{
					Name: jobconfig.CredentialName(b.UID, b.OperationID, b.JobName),
					UID:  "original-credential", Type: "credential",
				}, f.p.Window)
				if err != nil {
					t.Fatal(err)
				}
			}
			f.retire(t)
			for _, record := range remaining(t, f.c) {
				if record.Spec.Type != "retired" {
					if err := f.c.Client.Delete(t.Context(), &record); err != nil {
						t.Fatal(err)
					}
				}
			}
			r, err := resultretention.Decode(f.marker)
			if err != nil {
				t.Fatal(err)
			}
			source := client.ObjectKey{Namespace: f.marker.Namespace, Name: r.Source.Name}
			reader := &policyReads{Reader: f.c, gets: map[client.ObjectKey]int{}}
			f.p.Reader = reader
			if err := f.p.AuthorizeDelete(t.Context(), f.marker); err != nil {
				t.Fatal(err)
			}
			if got := reader.gets[source]; got != 1 {
				t.Errorf("retirement read its absent %s source %d times; want one final absence check", sourceType, got)
			}
			reader.fail = source
			if err := f.p.AuthorizeDelete(t.Context(), f.marker); !errors.Is(err, errWrite) {
				t.Fatalf("unavailable source authorized deletion: %v", err)
			}
			reader.fail = client.ObjectKey{}
			// Even a foreign object restored under that name must keep the
			// retirement. No absence verdict survives another admission call.
			restored := &api.PtahResultRecord{ObjectMeta: metav1.ObjectMeta{
				Namespace: source.Namespace, Name: source.Name, UID: "restored-source",
			}}
			if err := f.c.Client.Create(t.Context(), restored); err != nil {
				t.Fatal(err)
			}
			if err := f.p.AuthorizeDelete(t.Context(), f.marker); err == nil {
				t.Fatal("restored source authorized retirement deletion")
			}
		})
	}
}
