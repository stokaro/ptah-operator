package e2e

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultretention"
)

// teardownRecordsReading is the retired attempt the manager left on a kind
// lab: the credential, the result and the retirement record of one failed
// Resolve, read after its schema was deleted.
func teardownRecordsReading(t *testing.T) []ptahv1alpha1.PtahResultRecord {
	t.Helper()
	body, err := os.ReadFile("../../testdata/e2e/readings/teardown-retired-attempt.json")
	if err != nil {
		t.Fatal(err)
	}
	var list ptahv1alpha1.PtahResultRecordList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != teardownRecordCount {
		t.Fatalf("the reading holds %d records, not one retired attempt's %d", len(list.Items), teardownRecordCount)
	}
	return list.Items
}

// teardownReadingRecord returns the reading's record of the given type.
func teardownReadingRecord(t *testing.T, records []ptahv1alpha1.PtahResultRecord, kind string) *ptahv1alpha1.PtahResultRecord {
	t.Helper()
	for index := range records {
		if records[index].Spec.Type == kind {
			return &records[index]
		}
	}
	t.Fatalf("the reading holds no %s record", kind)
	return nil
}

func TestTeardownRetiredAttemptReadsTheWindowTheRetirementPersisted(t *testing.T) {
	t.Parallel()
	records := teardownRecordsReading(t)
	marker := teardownReadingRecord(t, records, "retired")
	attempt, err := teardownRetiredAttempt(records)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempt.Records) != teardownRecordCount {
		t.Fatalf("read %d records, want %d", len(attempt.Records), teardownRecordCount)
	}
	if !attempt.Retired.Equal(marker.CreationTimestamp.Time) {
		t.Fatalf("Retired = %s, want the retirement record's timestamp %s", attempt.Retired, marker.CreationTimestamp.Time)
	}
	if want := marker.CreationTimestamp.Add(time.Hour); !attempt.WindowEnds.Equal(want) {
		t.Fatalf("WindowEnds = %s, want an hour after the retirement, %s", attempt.WindowEnds, want)
	}
	retirement, err := resultretention.Decode(marker)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Binding != retirement.Binding {
		t.Fatalf("Binding = %+v, want the retirement's %+v", attempt.Binding, retirement.Binding)
	}
}

func TestTeardownRetiredAttemptReadsALongerRecordedWindow(t *testing.T) {
	t.Parallel()
	records := teardownRecordsReading(t)
	marker := teardownReadingRecord(t, records, "retired")
	retirement, err := resultretention.Decode(marker)
	if err != nil {
		t.Fatal(err)
	}
	longer, err := resultretention.Record(retirement.Binding, retirement.Source, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	longer.UID, longer.CreationTimestamp, longer.ResourceVersion = marker.UID, marker.CreationTimestamp, marker.ResourceVersion
	*marker = *longer
	attempt, err := teardownRetiredAttempt(records)
	if err != nil {
		t.Fatal(err)
	}
	if want := marker.CreationTimestamp.Add(2 * time.Hour); !attempt.WindowEnds.Equal(want) {
		t.Fatalf("WindowEnds = %s, want the two hours the retirement recorded, %s", attempt.WindowEnds, want)
	}
}

func TestTeardownRetiredAttemptRefusesWhatIsNotOneRetiredAttempt(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(t *testing.T, records []ptahv1alpha1.PtahResultRecord) []ptahv1alpha1.PtahResultRecord
		want   string
	}{
		{
			name:   "no records",
			change: func(*testing.T, []ptahv1alpha1.PtahResultRecord) []ptahv1alpha1.PtahResultRecord { return nil },
			want:   "holds no result records",
		},
		{
			name: "not retired yet",
			change: func(t *testing.T, records []ptahv1alpha1.PtahResultRecord) []ptahv1alpha1.PtahResultRecord {
				return without(records, teardownReadingRecord(t, records, "retired").Name)
			},
			want: "holds 0 retirement records",
		},
		{
			name: "a second retirement",
			change: func(t *testing.T, records []ptahv1alpha1.PtahResultRecord) []ptahv1alpha1.PtahResultRecord {
				extra := *teardownReadingRecord(t, records, "retired").DeepCopy()
				extra.Name += "-copy"
				return append(records, extra)
			},
			want: "holds 2 retirement records",
		},
		{
			name: "the credential missing",
			change: func(t *testing.T, records []ptahv1alpha1.PtahResultRecord) []ptahv1alpha1.PtahResultRecord {
				return without(records, teardownReadingRecord(t, records, "credential").Name)
			},
			want: "holds 2 result records",
		},
		{
			name: "a record of another attempt",
			change: func(t *testing.T, records []ptahv1alpha1.PtahResultRecord) []ptahv1alpha1.PtahResultRecord {
				extra := *teardownReadingRecord(t, records, "credential").DeepCopy()
				extra.Name = "ptah-result-key-00000000000000000000000000000000"
				return append(records, extra)
			},
			want: "holds 4 result records",
		},
		{
			name: "the credential replaced by another attempt's",
			change: func(t *testing.T, records []ptahv1alpha1.PtahResultRecord) []ptahv1alpha1.PtahResultRecord {
				teardownReadingRecord(t, records, "credential").Name = "ptah-result-key-00000000000000000000000000000000"
				return records
			},
			want: "is not one of the retired attempt's records",
		},
		{
			name: "the result recorded as a credential",
			change: func(t *testing.T, records []ptahv1alpha1.PtahResultRecord) []ptahv1alpha1.PtahResultRecord {
				teardownReadingRecord(t, records, "intent").Spec.Type = "credential"
				return records
			},
			want: "is not one of the retired attempt's records",
		},
		{
			name: "a result bound to another operation",
			change: func(t *testing.T, records []ptahv1alpha1.PtahResultRecord) []ptahv1alpha1.PtahResultRecord {
				intent := teardownReadingRecord(t, records, "intent")
				intent.Spec.Data = replaceOnce(t, intent.Spec.Data, `"operationID":"sha256:`, `"operationID":"sha256:0`)
				return records
			},
			want: "is not bound to the attempt its retirement names",
		},
		{
			// A credential's name derives from the resource UID, the operation
			// and the Job alone, so this one keeps its name and decodes.
			name: "a credential bound to another generation",
			change: func(t *testing.T, records []ptahv1alpha1.PtahResultRecord) []ptahv1alpha1.PtahResultRecord {
				credential := teardownReadingRecord(t, records, "credential")
				credential.Spec.Data = replaceOnce(t, credential.Spec.Data, `"generation":1,`, `"generation":2,`)
				return records
			},
			want: "is not bound to the attempt its retirement names",
		},
		{
			name: "a retirement that does not decode",
			change: func(t *testing.T, records []ptahv1alpha1.PtahResultRecord) []ptahv1alpha1.PtahResultRecord {
				teardownReadingRecord(t, records, "retired").Spec.Data = []byte(`{"version":2}`)
				return records
			},
			want: "does not decode",
		},
		{
			name: "a record with no server timestamp",
			change: func(t *testing.T, records []ptahv1alpha1.PtahResultRecord) []ptahv1alpha1.PtahResultRecord {
				teardownReadingRecord(t, records, "credential").CreationTimestamp = metav1.Time{}
				return records
			},
			want: "has no UID or creation timestamp",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			records := test.change(t, teardownRecordsReading(t))
			_, err := teardownRetiredAttempt(records)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("teardownRetiredAttempt() error = %v, want one containing %q", err, test.want)
			}
		})
	}
}

// replaceOnce replaces the one occurrence of old in data, and fails a row whose
// change would otherwise leave the reading as it was.
func replaceOnce(t *testing.T, data []byte, old, replacement string) []byte {
	t.Helper()
	if count := strings.Count(string(data), old); count != 1 {
		t.Fatalf("the reading holds %q %d times, want once", old, count)
	}
	return []byte(strings.Replace(string(data), old, replacement, 1))
}

// without returns records less the one with the given name.
func without(records []ptahv1alpha1.PtahResultRecord, name string) []ptahv1alpha1.PtahResultRecord {
	var kept []ptahv1alpha1.PtahResultRecord
	for _, record := range records {
		if record.Name != name {
			kept = append(kept, record)
		}
	}
	return kept
}

func TestTeardownNamespaceWindowIsWhatTheRecordsHaveLeft(t *testing.T) {
	t.Parallel()
	ends := time.Date(2026, 11, 12, 21, 30, 0, 0, time.UTC)
	attempt := teardownAttempt{WindowEnds: ends}
	deleted := metav1.NewTime(ends.Add(-55 * time.Minute))
	remaining, err := teardownNamespaceWindow(&deleted, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if remaining != 55*time.Minute {
		t.Fatalf("remaining = %s, want the 55 minutes between the deletion and the window's end", remaining)
	}
	for _, test := range []struct {
		name    string
		deleted *metav1.Time
		attempt teardownAttempt
		want    string
	}{
		{"no deletion timestamp", nil, attempt, "no deletion timestamp"},
		{"no window", &deleted, teardownAttempt{}, "persisted no window"},
		{"deleted as the window ends", &metav1.Time{Time: ends}, attempt, "had already ended"},
		{"deleted after the window", &metav1.Time{Time: ends.Add(time.Second)}, attempt, "had already ended"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := teardownNamespaceWindow(test.deleted, test.attempt); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("teardownNamespaceWindow() error = %v, want one containing %q", err, test.want)
			}
		})
	}
}

// teardownNamespaceReading is a terminating namespace the namespace controller
// reported on a kind lab with the operator installed: the admission webhook
// refused to delete a result record inside its window.
func teardownNamespaceReading(t *testing.T) *corev1.Namespace {
	t.Helper()
	body, err := os.ReadFile("../../testdata/e2e/readings/teardown-namespace-refused.json")
	if err != nil {
		t.Fatal(err)
	}
	namespace := &corev1.Namespace{}
	if err := json.Unmarshal(body, namespace); err != nil {
		t.Fatal(err)
	}
	return namespace
}

// setNamespaceCondition sets one condition's status and message, as the
// namespace controller writes it.
func setNamespaceCondition(namespace *corev1.Namespace, kind corev1.NamespaceConditionType, status corev1.ConditionStatus, message string) {
	for index := range namespace.Status.Conditions {
		if namespace.Status.Conditions[index].Type == kind {
			namespace.Status.Conditions[index].Status, namespace.Status.Conditions[index].Message = status, message
			return
		}
	}
	namespace.Status.Conditions = append(namespace.Status.Conditions, corev1.NamespaceCondition{Type: kind, Status: status, Message: message})
}

func TestTeardownNamespaceRefusalNamesWhatTheNamespaceControllerReports(t *testing.T) {
	t.Parallel()
	refused := teardownNamespaceReading(t)
	message, isRefusal := teardownNamespaceRefusal(refused)
	if !isRefusal || !strings.Contains(message, "NamespaceDeletionContentFailure") ||
		!strings.Contains(message, "result publication retention has not authorized deletion") {
		t.Fatalf("teardownNamespaceRefusal(lab reading) = %q, %v; want the webhook's refusal", message, isRefusal)
	}

	finalized := teardownNamespaceReading(t)
	setNamespaceCondition(finalized, corev1.NamespaceDeletionContentFailure, corev1.ConditionFalse, "All content successfully deleted")
	setNamespaceCondition(finalized, corev1.NamespaceFinalizersRemaining, corev1.ConditionTrue,
		"Some content in the namespace has finalizers remaining: operator.ptah.run/result-retention in 1 resource instances")
	if message, isRefusal := teardownNamespaceRefusal(finalized); !isRefusal || !strings.Contains(message, "operator.ptah.run/result-retention") {
		t.Fatalf("teardownNamespaceRefusal(finalizer remaining) = %q, %v; want the finalizer named", message, isRefusal)
	}

	for _, test := range []struct {
		name   string
		change func(*corev1.Namespace)
	}{
		{"content still being deleted", func(namespace *corev1.Namespace) {
			namespace.Status.Conditions = nil
			setNamespaceCondition(namespace, corev1.NamespaceDeletionContentFailure, corev1.ConditionFalse, "All content successfully deleted")
			setNamespaceCondition(namespace, corev1.NamespaceContentRemaining, corev1.ConditionTrue,
				"Some resources are remaining: ptahresultrecords.operator.ptah.run has 3 resource instances")
			setNamespaceCondition(namespace, corev1.NamespaceFinalizersRemaining, corev1.ConditionFalse, "All content-preserving finalizers satisfied")
		}},
		{"discovery that failed", func(namespace *corev1.Namespace) {
			namespace.Status.Conditions = nil
			setNamespaceCondition(namespace, corev1.NamespaceDeletionDiscoveryFailure, corev1.ConditionTrue, "Discovery failed for some groups")
		}},
		{"no conditions yet", func(namespace *corev1.Namespace) { namespace.Status.Conditions = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			namespace := teardownNamespaceReading(t)
			test.change(namespace)
			if message, isRefusal := teardownNamespaceRefusal(namespace); isRefusal {
				t.Fatalf("teardownNamespaceRefusal() = %q, true; a deletion still in progress is not a refusal", message)
			}
		})
	}
}

func TestTeardownNamespaceObservedNamesEveryConditionThatHolds(t *testing.T) {
	t.Parallel()
	observed := teardownNamespaceObserved(teardownNamespaceReading(t))
	for _, want := range []string{"phase=Terminating", "deletionTimestamp=", "NamespaceDeletionContentFailure="} {
		if !strings.Contains(observed, want) {
			t.Fatalf("teardownNamespaceObserved() = %q, want it to contain %q", observed, want)
		}
	}
}
