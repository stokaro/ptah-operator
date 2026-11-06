package resultcleanup

import (
	"errors"
	"testing"
	"time"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultretention"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// requireWindowEnd holds err to a window refusal that names exactly ends. The
// rows set the clock somewhere else, so an instant computed from the clock
// instead of a persisted timestamp cannot pass.
func requireWindowEnd(t *testing.T, err error, ends, now time.Time) {
	t.Helper()
	if ends.Equal(now) {
		t.Fatalf("the row dates its window end at the clock under test (%s), so it proves nothing", now)
	}
	got, ok := resultretention.WindowEnd(err)
	if !ok {
		t.Fatalf("refusal names no instant, want %s: %v", ends, err)
	}
	if !got.Equal(ends) {
		t.Fatalf("refusal names %s, want %s", got, ends)
	}
	if !errors.Is(err, resultretention.ErrWindow) {
		t.Fatalf("%v does not match ErrWindow", err)
	}
}

func recordOfType(t *testing.T, c client.Client, kind string) *api.PtahResultRecord {
	t.Helper()
	records := remaining(t, c)
	for i := range records {
		if records[i].Spec.Type == kind {
			return &records[i]
		}
	}
	t.Fatalf("no %s record among %d", kind, len(records))
	return nil
}

// foregroundRetired stores the publication the way a terminating namespace
// leaves it: no retirement marker, and an intent the API server began deleting
// in the foreground after the given time. It returns that intent and a member.
func foregroundRetired(t *testing.T, f cleanupFixture, after time.Duration) (root, member *api.PtahResultRecord) {
	t.Helper()
	objects := []client.Object{f.f.Subject, f.f.Pod}
	found := false
	for _, record := range remaining(t, f.c) {
		if record.Spec.Type == "intent" {
			found = true
			record.DeletionTimestamp = &metav1.Time{Time: record.CreationTimestamp.Add(after)}
			record.Finalizers = []string{metav1.FinalizerDeleteDependents}
		}
		objects = append(objects, record.DeepCopy())
	}
	if !found {
		t.Fatal("the publication has no intent")
	}
	f.c.Client = fake.NewClientBuilder().WithScheme(f.c.Scheme()).WithObjects(objects...).Build()
	return recordOfType(t, f.c, "intent"), recordOfType(t, f.c, "complete")
}

// A record's own creation time is only the first window it has to clear. Each
// row reaches a different check, and the instant has to be the one that check
// measured: naming the record's own window after it passed would send a person
// back at a time that is already over.
func TestWindowEndIsTheWindowThatRefused(t *testing.T) {
	t.Run("a young record names its own window", func(t *testing.T) {
		f := fixture(t)
		record := recordOfType(t, f.c, "intent")
		ends := record.CreationTimestamp.Add(time.Hour)
		f.c.now = record.CreationTimestamp.Add(20 * time.Minute)
		requireWindowEnd(t, f.p.AuthorizeDelete(t.Context(), record), ends, f.c.now)
	})
	t.Run("a retirement younger than the record names the retirement's window", func(t *testing.T) {
		f := fixture(t)
		f.c.now = f.c.now.Add(10 * time.Minute)
		if err := f.c.Create(t.Context(), f.marker); err != nil {
			t.Fatal(err)
		}
		record := recordOfType(t, f.c, "intent")
		ends := f.marker.CreationTimestamp.Add(time.Hour)
		f.c.now = record.CreationTimestamp.Add(65 * time.Minute)
		if !record.CreationTimestamp.Add(time.Hour).Before(f.c.now) {
			t.Fatal("the record's own window has to be over for this row to reach the retirement")
		}
		requireWindowEnd(t, f.p.AuthorizeDelete(t.Context(), record), ends, f.c.now)
	})
	t.Run("a longer persisted window names that window", func(t *testing.T) {
		f := fixture(t)
		r, err := resultretention.Decode(f.marker)
		if err != nil {
			t.Fatal(err)
		}
		f.marker, err = resultretention.Record(r.Binding, r.Source, 2*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.c.Create(t.Context(), f.marker); err != nil {
			t.Fatal(err)
		}
		ends := f.marker.CreationTimestamp.Add(2 * time.Hour)
		f.c.now = f.marker.CreationTimestamp.Add(90 * time.Minute)
		requireWindowEnd(t, f.p.AuthorizeDelete(t.Context(), recordOfType(t, f.c, "intent")), ends, f.c.now)
	})
	t.Run("a record written after its retirement names its own longer window", func(t *testing.T) {
		f := fixture(t)
		r, err := resultretention.Decode(f.marker)
		if err != nil {
			t.Fatal(err)
		}
		f.marker, err = resultretention.Record(r.Binding, r.Source, 2*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.c.Create(t.Context(), f.marker); err != nil {
			t.Fatal(err)
		}
		late := recordOfType(t, f.c, "complete")
		late.CreationTimestamp = metav1.NewTime(f.marker.CreationTimestamp.Add(90 * time.Minute))
		ends := late.CreationTimestamp.Add(2 * time.Hour)
		f.c.now = late.CreationTimestamp.Add(70 * time.Minute)
		if f.c.now.Before(f.marker.CreationTimestamp.Add(2 * time.Hour)) {
			t.Fatal("the retirement's window has to be over for this row to reach the late record's")
		}
		requireWindowEnd(t, f.p.AuthorizeDelete(t.Context(), late), ends, f.c.now)
	})
	t.Run("a foreground retirement names the window its deletion started", func(t *testing.T) {
		f := fixture(t)
		root, member := foregroundRetired(t, f, 30*time.Minute)
		ends := root.DeletionTimestamp.Add(time.Hour)
		f.c.now = root.CreationTimestamp.Add(70 * time.Minute)
		requireWindowEnd(t, f.p.AuthorizeDelete(t.Context(), root), ends, f.c.now)
		requireWindowEnd(t, f.p.AuthorizeDelete(t.Context(), member), ends, f.c.now)
	})
	// AuthorizeDelete refuses a record this young at its first check, so only
	// the collector's direct call reaches this comparison.
	t.Run("a young foreground record read by the collector names its own window", func(t *testing.T) {
		f := fixture(t)
		root, _ := foregroundRetired(t, f, 0)
		young := root.DeepCopy()
		young.CreationTimestamp = metav1.NewTime(root.DeletionTimestamp.Add(10 * time.Minute))
		ends := young.CreationTimestamp.Add(time.Hour)
		f.c.now = root.DeletionTimestamp.Add(65 * time.Minute)
		_, err := f.p.foregroundBinding(t.Context(), young)
		requireWindowEnd(t, err, ends, f.c.now)
	})
}

// The refusals nothing but a change of state lifts. Each row is past every
// window it could have been measured against, so a refusal here is about
// something else, and an instant would be a claim the policy cannot back.
func TestRefusalsThatAreNotAboutTimeNameNoInstant(t *testing.T) {
	retired := func(t *testing.T) cleanupFixture {
		t.Helper()
		f := fixture(t)
		f.retire(t) // Two hours past both the records and the marker.
		return f
	}
	for _, row := range []struct {
		name string
		// arrange returns the record to authorize.
		arrange func(*testing.T, cleanupFixture) *api.PtahResultRecord
		want    error
	}{
		{
			name: "the original Job can still authenticate",
			arrange: func(t *testing.T, f cleanupFixture) *api.PtahResultRecord {
				job := f.f.Job.DeepCopy()
				job.ResourceVersion = ""
				if err := f.c.Client.Create(t.Context(), job); err != nil {
					t.Fatal(err)
				}
				return recordOfType(t, f.c, "complete")
			},
			want: ErrRetained,
		},
		{
			name: "the resource still pins the attempt",
			arrange: func(t *testing.T, f cleanupFixture) *api.PtahResultRecord {
				schema := f.f.Subject.(*api.PtahSchema)
				schema.Status.ActiveOperation = &api.ActiveOperationStatus{ID: f.f.Identity.Binding.OperationID}
				if err := f.c.Update(t.Context(), schema); err != nil {
					t.Fatal(err)
				}
				return recordOfType(t, f.c, "complete")
			},
			want: resultretention.ErrPinned,
		},
		{
			name: "members have to go before their intent",
			arrange: func(t *testing.T, f cleanupFixture) *api.PtahResultRecord {
				return recordOfType(t, f.c, "intent")
			},
			want: ErrRetained,
		},
		{
			name: "the record cannot be bound",
			arrange: func(t *testing.T, f cleanupFixture) *api.PtahResultRecord {
				record := recordOfType(t, f.c, "complete")
				record.OwnerReferences = nil
				return record
			},
			want: ErrRetained,
		},
		{
			name: "nothing has started the retirement clock",
			arrange: func(t *testing.T, f cleanupFixture) *api.PtahResultRecord {
				if err := f.c.Client.Delete(t.Context(), f.marker); err != nil {
					t.Fatal(err)
				}
				return recordOfType(t, f.c, "complete")
			},
			want: ErrRetained,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := retired(t)
			record := row.arrange(t, f)
			err := f.p.AuthorizeDelete(t.Context(), record)
			if !errors.Is(err, row.want) {
				t.Fatalf("refusal = %v, want %v", err, row.want)
			}
			if ends, ok := resultretention.WindowEnd(err); ok {
				t.Fatalf("a refusal about %v named %s", row.want, ends)
			}
		})
	}
	t.Run("a failed read", func(t *testing.T) {
		f := retired(t)
		f.p.Reader = &policyReads{Reader: f.c, gets: map[client.ObjectKey]int{}, fail: client.ObjectKeyFromObject(f.marker)}
		err := f.p.AuthorizeDelete(t.Context(), recordOfType(t, f.c, "complete"))
		if !errors.Is(err, errWrite) {
			t.Fatalf("refusal = %v, want the read's failure", err)
		}
		if ends, ok := resultretention.WindowEnd(err); ok {
			t.Fatalf("a failed read named %s", ends)
		}
	})
	t.Run("a record with no persisted creation time", func(t *testing.T) {
		f := retired(t)
		record := recordOfType(t, f.c, "complete")
		record.CreationTimestamp = metav1.Time{}
		err := f.p.AuthorizeDelete(t.Context(), record)
		if !errors.Is(err, resultretention.ErrWindow) {
			t.Fatalf("refusal = %v, want ErrWindow", err)
		}
		if ends, ok := resultretention.WindowEnd(err); ok {
			t.Fatalf("a window with no start named %s", ends)
		}
	})
}
