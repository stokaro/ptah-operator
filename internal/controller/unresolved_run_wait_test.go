package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/operate"
)

// storedDocuments answers the nth Get of the resource with the nth document the
// reconciler stored, and every later Get with the last: the resource as each poll
// of a wait finds it.
type storedDocuments struct {
	documents []*operatorv1alpha1.PtahMigration
	reads     int
}

func (s *storedDocuments) Get(_ context.Context, _ client.ObjectKey, object client.Object, _ ...client.GetOption) error {
	document := s.documents[min(s.reads, len(s.documents)-1)]
	s.reads++
	migration, ok := object.(*operatorv1alpha1.PtahMigration)
	if !ok {
		return errors.New("storedDocuments holds migrations only")
	}
	document.DeepCopyInto(migration)
	return nil
}

func (s *storedDocuments) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("storedDocuments lists nothing")
}

// readAfterTheRun takes the history read that follows the Apply's write: its
// claim takes Progressing to True, as claimMigration writes for every claim
// (readMigrationHistory starts at the claim and does not write it), and the
// harvest records report. taken is the read request the operator took before it
// claimed the read, or empty.
func readAfterTheRun(
	t *testing.T,
	applied *operatorv1alpha1.PtahMigration,
	report dataplane.MigrationStatusReport,
	taken string,
) *operatorv1alpha1.PtahMigration {
	t.Helper()

	claimed := applied.DeepCopy()
	if taken != "" {
		claimed.Annotations = map[string]string{operatorv1alpha1.ReadRequestAnnotation: taken}
		claimed.Status.LastReadRequest = operatorv1alpha1.ReadRequestToken(taken)
	}
	setMigrationCondition(claimed, operatorv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue,
		operatorv1alpha1.ReasonOperationInProgress, "History operation is in progress")
	read, _ := readMigrationHistory(t, claimed, report)
	return read
}

// An Apply under apply Always that ends Unknown rests in Blocked beside the
// reading from before the run until the operator reads the database again, and
// that reading settles the run when nothing is pending there (Resolution
// HistoryRead). A wait does not stop on the Blocked before the reading: a plain
// wait for in-sync reaches InSync, and refresh --wait ends with the read. Where
// the reading finds work pending, the run stays unaccounted for, and both stop
// with exit status 8. Every document is the one the reconciler stored.
//
// The reading after the run is stamped by the reconciler's clock, which reads the
// second the record carries, and the rows date the reading from before the run an
// hour before the record and an hour after it, as a manager whose clock was ahead
// would. A wait that ordered the reading against status.unresolvedRun.recordedAt
// would not count the reading that followed the record, and in the second row it
// would count the one that came before it.
func TestAWaitOnAnUnknownRunStopsOnlyOnceAReadingFollowsIt(t *testing.T) {
	t.Parallel()

	for _, dated := range []struct {
		name string
		by   time.Duration
	}{
		{name: "the reading before the run dated before the record", by: -time.Hour},
		{name: "the reading before the run dated after the record by a clock ahead", by: time.Hour},
	} {
		applied := unresolvedMigrationRun(t, operatorv1alpha1.ApplyPolicyAlways, operatorv1alpha1.MigrationRunOutcomeUnknown)
		recordedAt := applied.Status.UnresolvedRun.RecordedAt
		applied.Status.History.ObservedAt = metav1.NewTime(recordedAt.Add(dated.by))
		// The Apply's write is the run's end, an hour before the read that
		// follows it; a second apart, the two would carry one stamp.
		progressing := meta.FindStatusCondition(applied.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
		if applied.Status.Phase != operatorv1alpha1.MigrationPhaseBlocked || progressing == nil ||
			progressing.Status != metav1.ConditionFalse || progressing.Reason != string(operatorv1alpha1.ReasonApplyOutcomeUnknown) {
			t.Fatalf("the Apply left phase %s and Progressing %#v, want Blocked and False with ApplyOutcomeUnknown",
				applied.Status.Phase, progressing)
		}
		progressing.LastTransitionTime = metav1.NewTime(progressing.LastTransitionTime.Add(-time.Hour))
		ref := operate.Ref{Family: operate.FamilyMigration, Namespace: applied.Namespace, Name: applied.Name}

		for _, row := range []struct {
			name    string
			report  dataplane.MigrationStatusReport
			settles bool
		}{
			{name: "nothing pending", report: settledMigrationHistory(), settles: true},
			{name: "work pending", report: pendingMigrationHistory()},
		} {
			t.Run(dated.name+"/"+row.name+"/wait for in-sync", func(t *testing.T) {
				t.Parallel()

				read := readAfterTheRun(t, applied, row.report, "")
				assertTheReadingFollowedTheRun(t, applied, read, row.settles)
				documents := &storedDocuments{documents: []*operatorv1alpha1.PtahMigration{applied, applied, read}}

				_, err := waitOn(documents, ref, operate.WaitOptions{Target: operate.TargetInSync})

				if row.settles {
					if err != nil || documents.reads != 3 {
						t.Fatalf("wait = %v after %d reads, want InSync at read 3", err, documents.reads)
					}
					return
				}
				blocked := meta.FindStatusCondition(read.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked)
				assertBlockedStop(t, err, operatorv1alpha1.ReasonApplyOutcomeUnknown, blocked.Message, false)
				if documents.reads != 4 {
					t.Fatalf("the wait stopped at read %d, want read 4: the second poll of the reading after the run", documents.reads)
				}
			})
			t.Run(dated.name+"/"+row.name+"/refresh --wait", func(t *testing.T) {
				t.Parallel()

				requested := applied.DeepCopy()
				requested.Annotations = map[string]string{operatorv1alpha1.ReadRequestAnnotation: requestedAt}
				read := readAfterTheRun(t, applied, row.report, requestedAt)
				assertTheReadingFollowedTheRun(t, applied, read, row.settles)
				documents := &storedDocuments{documents: []*operatorv1alpha1.PtahMigration{requested, read}}

				_, err := waitOn(documents, ref, readWaitOptions(requested))

				if row.settles {
					if err != nil || documents.reads != 2 {
						t.Fatalf("wait = %v after %d reads, want the read done at read 2", err, documents.reads)
					}
					return
				}
				blocked := meta.FindStatusCondition(read.Status.Conditions, operatorv1alpha1.ConditionMigrationBlocked)
				assertBlockedStop(t, err, operatorv1alpha1.ReasonApplyOutcomeUnknown, blocked.Message, true)
			})
		}
	}
}

// assertTheReadingFollowedTheRun holds the reading the reconciler stored after the
// run to what the row is about: a reading other than the one beside the record,
// which removed the record when settles and left it standing in Blocked otherwise.
func assertTheReadingFollowedTheRun(t *testing.T, applied, read *operatorv1alpha1.PtahMigration, settles bool) {
	t.Helper()
	if read.Status.History == nil || read.Status.History.ObservedAt.Equal(&applied.Status.History.ObservedAt) {
		t.Fatalf("the read recorded %#v, want a reading other than the one at %v", read.Status.History, applied.Status.History.ObservedAt)
	}
	if settles {
		if read.Status.Phase != operatorv1alpha1.MigrationPhaseInSync || read.Status.UnresolvedRun != nil ||
			read.Status.ResolvedRun == nil || read.Status.ResolvedRun.Resolution != operatorv1alpha1.MigrationRunResolvedByHistoryRead {
			t.Fatalf("phase %s, unresolved %#v, resolved %#v; want InSync with the run resolved by the reading",
				read.Status.Phase, read.Status.UnresolvedRun, read.Status.ResolvedRun)
		}
		return
	}
	assertMigrationBlockedFor(t, read, operatorv1alpha1.ReasonApplyOutcomeUnknown)
	if read.Status.UnresolvedRun == nil || read.Status.UnresolvedRun.OperationID != applied.Status.UnresolvedRun.OperationID {
		t.Fatalf("the reading left record %#v, want the run's record %q standing", read.Status.UnresolvedRun,
			applied.Status.UnresolvedRun.OperationID)
	}
}
