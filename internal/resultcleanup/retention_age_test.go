package resultcleanup

import (
	"errors"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/internal/resultretention"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestYoungRecordDeletionRefusesWithoutAPIReads(t *testing.T) {
	f := fixture(t)
	f.retire(t)
	reader := &policyReads{Reader: f.c, gets: map[client.ObjectKey]int{}}
	f.p.Reader = reader
	records := remaining(t, f.c)
	if len(records) != 5 {
		t.Fatalf("got %d records, want the complete retained publication", len(records))
	}
	for _, record := range records {
		t.Run(record.Name, func(t *testing.T) {
			deadline := record.CreationTimestamp.Add(f.p.Window)
			for _, before := range []time.Duration{30 * time.Minute, time.Nanosecond} {
				f.c.now = deadline.Add(-before)
				clear(reader.gets)
				if err := f.p.AuthorizeDelete(t.Context(), &record); !errors.Is(err, resultretention.ErrWindow) {
					t.Fatalf("young %s was not retained: %v", record.Spec.Type, err)
				}
				if len(reader.gets) != 0 {
					t.Errorf("a %s already within its own retention window made API reads: %v", record.Spec.Type, reader.gets)
				}
			}
			// Reaching the earliest deadline is not permission to delete. The
			// existing live authority checks must resume and fail closed.
			f.c.now = deadline
			reader.fail = client.ObjectKeyFromObject(f.marker)
			if err := f.p.AuthorizeDelete(t.Context(), &record); !errors.Is(err, errWrite) {
				t.Errorf("expired %s skipped the unavailable current retirement: %v", record.Spec.Type, err)
			}
			reader.fail = client.ObjectKey{}
		})
	}
}

func TestRecordAgeDoesNotShortenLongerRetirement(t *testing.T) {
	f := fixture(t)
	r, err := resultretention.Decode(f.marker)
	if err != nil {
		t.Fatal(err)
	}
	f.marker, err = resultretention.Record(r.Binding, r.Source, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f.retire(t)
	f.c.now = f.marker.CreationTimestamp.Add(90 * time.Minute)
	reader := &policyReads{Reader: f.c, gets: map[client.ObjectKey]int{}}
	f.p.Reader = reader
	examined := 0
	for _, record := range remaining(t, f.c) {
		clear(reader.gets)
		if err := f.p.AuthorizeDelete(t.Context(), &record); !errors.Is(err, resultretention.ErrWindow) {
			t.Fatalf("%s lost its longer persisted window: %v", record.Spec.Type, err)
		}
		if len(reader.gets) == 0 {
			t.Errorf("%s did not check its longer persisted retirement", record.Spec.Type)
		}
		examined++
	}
	if examined != 5 {
		t.Fatalf("examined %d records, want the complete retained publication", examined)
	}
}
