package e2e

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultcleanup"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultretention"
	"github.com/stokaro/ptah-operator/internal/resultstore"
)

// The uninstall-first teardown's names. The namespace is the driver's; these
// are the objects the row creates in it.
const (
	// teardownSchema is the schema whose one operation leaves the records.
	teardownSchema = "teardown"
	// teardownPullSecret is the registry credential the operation pulls its
	// images with.
	teardownPullSecret = "e2e-teardown-registry-pull"
	// teardownDatabaseURLSecret and teardownVerificationPolicy are the
	// schema's fixtures.
	teardownDatabaseURLSecret  = "e2e-teardown-database-url"
	teardownVerificationPolicy = "e2e-teardown-verification-policy"
	// teardownDatabaseURL is a database the Resolve never reaches: it fails
	// at the unreachable artifact first.
	teardownDatabaseURL = "postgres://e2e:unused@database.invalid/e2e"
)

// teardownRecordCount is how many records one finished and retired operation
// attempt leaves: the credential that delivered its result, the result, and
// the record that retired them.
const teardownRecordCount = 3

// teardownSchemaDocument is a schema whose one operation is a Resolve against
// an artifact nobody serves. It reaches a Pod, delivers its failure as a
// result, and leaves the records the teardown has to remove, without a
// database or a published artifact.
func teardownSchemaDocument(namespace string) map[string]any {
	return map[string]any{
		"apiVersion": ptahv1alpha1.GroupVersion.String(), "kind": "PtahSchema",
		"metadata": map[string]any{"namespace": namespace, "name": teardownSchema},
		"spec": map[string]any{
			"target": map[string]any{
				"engine":          "PostgreSQL",
				"coordinationKey": "e2e/lifecycle/teardown",
				"urlFrom":         map[string]any{"name": teardownDatabaseURLSecret, "key": "url"},
			},
			"desired": map[string]any{
				"ociRef":                 "oci://127.0.0.1:1/e2e/teardown:unreachable",
				"verificationPolicyFrom": map[string]any{"name": teardownVerificationPolicy, "key": "policy.yaml"},
				"transport":              map[string]any{"plainHTTP": true},
			},
			"interval": "24h",
			"execution": map[string]any{
				"activeDeadlineSeconds": int64(120), "failureRetryInterval": "1h",
				"serviceAccountName": "default",
				"imagePullSecrets":   []any{map[string]any{"name": teardownPullSecret}},
			},
		},
	}
}

// teardownRecord is one record the teardown has to remove, as the API server
// stored it.
type teardownRecord struct {
	Name    string
	UID     string
	Type    string
	Created time.Time
}

// teardownAttempt is the retired operation attempt a namespace holds: its
// records, and the instant retention would first let any of them be deleted.
type teardownAttempt struct {
	Binding resultstore.Binding
	Records []teardownRecord
	// Retired is the server timestamp of the retirement record, which starts
	// the window.
	Retired time.Time
	// WindowEnds is the earliest instant the admission webhook would let any
	// of the records go: the retirement's own timestamp plus the window it
	// recorded, which Decode holds to at least the minimum every installation
	// keeps. The webhook holds each record to that and to its own timestamp
	// plus the window, and the retirement record is one of them, so none can
	// go sooner. It is what the records persisted, not a time the row chose.
	WindowEnds time.Time
}

// teardownRetiredAttempt reads the one retired attempt out of every record in
// the namespace. It refuses an empty list, a namespace whose attempt has not
// been retired yet, and anything but exactly the attempt's credential, result
// and retirement record, each bound to the one operation the retirement
// names.
func teardownRetiredAttempt(records []ptahv1alpha1.PtahResultRecord) (teardownAttempt, error) {
	if len(records) == 0 {
		return teardownAttempt{}, errors.New("the namespace holds no result records")
	}
	var markers []*ptahv1alpha1.PtahResultRecord
	for index := range records {
		if records[index].Spec.Type == "retired" {
			markers = append(markers, &records[index])
		}
	}
	if len(markers) != 1 {
		return teardownAttempt{}, fmt.Errorf("the namespace holds %d retirement records among %d result records; want 1", len(markers), len(records))
	}
	marker := markers[0]
	retirement, err := resultretention.Decode(marker)
	if err != nil {
		return teardownAttempt{}, fmt.Errorf("retirement record %s does not decode: %w", marker.Name, err)
	}
	if marker.CreationTimestamp.IsZero() {
		return teardownAttempt{}, fmt.Errorf("retirement record %s has no creation timestamp", marker.Name)
	}
	intent, err := resultstore.Name(retirement.Binding)
	if err != nil {
		return teardownAttempt{}, fmt.Errorf("retirement record %s names no attempt: %w", marker.Name, err)
	}
	b := retirement.Binding
	want := map[string]string{
		intent: "intent",
		jobconfig.CredentialName(b.UID, b.OperationID, b.JobName): "credential",
		marker.Name: "retired",
	}
	if len(records) != teardownRecordCount {
		return teardownAttempt{}, fmt.Errorf("the namespace holds %d result records; one retired attempt leaves %d", len(records), teardownRecordCount)
	}
	window := time.Duration(retirement.RetentionSeconds) * time.Second
	attempt := teardownAttempt{
		Binding: b, Retired: marker.CreationTimestamp.UTC(), WindowEnds: marker.CreationTimestamp.Add(window).UTC(),
	}
	for index := range records {
		record := &records[index]
		kind, expected := want[record.Name]
		if !expected || record.Spec.Type != kind {
			return teardownAttempt{}, fmt.Errorf("result record %s of type %q is not one of the retired attempt's records", record.Name, record.Spec.Type)
		}
		delete(want, record.Name)
		bound, err := resultcleanup.RootBinding(record)
		if err != nil || bound != b {
			return teardownAttempt{}, fmt.Errorf("result record %s is not bound to the attempt its retirement names", record.Name)
		}
		if record.UID == "" || record.CreationTimestamp.IsZero() {
			return teardownAttempt{}, fmt.Errorf("result record %s has no UID or creation timestamp", record.Name)
		}
		attempt.Records = append(attempt.Records, teardownRecord{
			Name: record.Name, UID: string(record.UID), Type: record.Spec.Type, Created: record.CreationTimestamp.UTC(),
		})
	}
	slices.SortFunc(attempt.Records, func(a, b teardownRecord) int { return strings.Compare(a.Name, b.Name) })
	return attempt, nil
}

// teardownNamespaceWindow is how long after its deletion the namespace has to
// be gone for the uninstall, rather than the end of the window, to be what
// removed its records. It refuses a deletion at or after the window's end,
// whose outcome says nothing about the uninstall.
func teardownNamespaceWindow(deleted *metav1.Time, attempt teardownAttempt) (time.Duration, error) {
	if deleted == nil || deleted.IsZero() {
		return 0, errors.New("the namespace carries no deletion timestamp")
	}
	if attempt.WindowEnds.IsZero() {
		return 0, errors.New("the records persisted no window")
	}
	if !deleted.Before(&metav1.Time{Time: attempt.WindowEnds}) {
		return 0, fmt.Errorf("the namespace was deleted at %s, when its records' window had already ended at %s",
			deleted.UTC().Format(time.RFC3339), attempt.WindowEnds.Format(time.RFC3339))
	}
	return attempt.WindowEnds.Sub(deleted.Time), nil
}

// teardownNamespaceGoneInTime holds a reading that found the namespace gone to
// the records' window. read is when that reading returned, and it has to be
// before the window's end: from that instant retention lets the records go, so
// a namespace found gone then says nothing about the uninstall. The wait that
// polls for it runs one last reading at its deadline and accepts it before it
// looks at the time, so the deadline alone does not hold this.
func teardownNamespaceGoneInTime(read time.Time, attempt teardownAttempt) error {
	if !read.Before(attempt.WindowEnds) {
		return fmt.Errorf("the namespace was found gone at %s, when its records' window had already ended at %s",
			read.UTC().Format(time.RFC3339Nano), attempt.WindowEnds.Format(time.RFC3339))
	}
	return nil
}

// teardownNamespaceRefusal reports a terminating namespace whose deletion
// something refused, in the namespace controller's own words: content an
// admission check or the API refused to delete, or content that carries a
// finalizer. Neither lifts while the records' window runs, so the row fails on
// the first reading that holds one rather than at the end of the window. The
// row's namespace holds no Pod and no object Kubernetes finalizes itself by the
// time it is deleted, so a finalizer here is one nothing is left to remove.
func teardownNamespaceRefusal(namespace *corev1.Namespace) (string, bool) {
	for _, condition := range namespace.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		switch condition.Type {
		case corev1.NamespaceDeletionContentFailure, corev1.NamespaceFinalizersRemaining:
			return fmt.Sprintf("%s: %s", condition.Type, condition.Message), true
		}
	}
	return "", false
}

// teardownNamespaceObserved summarizes a terminating namespace for a failure
// message: its phase, when its deletion began, and every condition that holds.
func teardownNamespaceObserved(namespace *corev1.Namespace) string {
	parts := []string{"phase=" + string(namespace.Status.Phase)}
	if namespace.DeletionTimestamp != nil {
		parts = append(parts, "deletionTimestamp="+namespace.DeletionTimestamp.UTC().Format(time.RFC3339))
	}
	for _, condition := range namespace.Status.Conditions {
		if condition.Status == corev1.ConditionTrue {
			parts = append(parts, fmt.Sprintf("%s=%q", condition.Type, condition.Message))
		}
	}
	return strings.Join(parts, " ")
}
