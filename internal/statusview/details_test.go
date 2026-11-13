package statusview_test

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/statusview"
)

// A read request is answered only when the controller recorded the value the
// annotation holds now; an older answer beside a newer request is a read
// still owed.
func TestReadRequestSaysWhetherTheReadWasAnswered(t *testing.T) {
	t.Parallel()

	// The status records a value longer than MaxReadRequestLength as its
	// digest, so a 65-byte request is answered by its token and not by
	// itself.
	long := "2026-03-02T00:00:00Z" + strings.Repeat("x", operatorv1alpha1.MaxReadRequestLength+1-20)
	token := operatorv1alpha1.ReadRequestToken(long)
	if len(long) != operatorv1alpha1.MaxReadRequestLength+1 || token == long {
		t.Fatalf("the long request is %d bytes and its token %q", len(long), token)
	}
	for _, test := range []struct{ requested, answered, want string }{
		{"", "", ""},
		{long, token, long + ", answered"},
		{long, long, long + ", not answered yet; the last one answered was " + long},
		{"2026-03-02T00:00:00Z", "2026-03-02T00:00:00Z", "2026-03-02T00:00:00Z, answered"},
		{"2026-03-02T00:00:00Z", "", "2026-03-02T00:00:00Z, not answered yet"},
		{"2026-03-02T00:00:00Z", "2026-03-01T00:00:00Z",
			"2026-03-02T00:00:00Z, not answered yet; the last one answered was 2026-03-01T00:00:00Z"},
		{"", "2026-03-01T00:00:00Z", "none now; the last one answered was 2026-03-01T00:00:00Z"},
	} {
		if got := statusview.ReadRequest(test.requested, test.answered); got != test.want {
			t.Errorf("ReadRequest(%q, %q) = %q, want %q", test.requested, test.answered, got, test.want)
		}
	}
	if got := statusview.ReadRequested("", ""); got != "" {
		t.Errorf("ReadRequested with nothing asked = %q, want no line", got)
	}
}

func TestStatusForAndClaimedSayWhatTheStatusRecords(t *testing.T) {
	t.Parallel()

	if got := statusview.StatusFor(0, 3); got != "no generation yet; the spec is at generation 3" {
		t.Errorf("StatusFor(0, 3) = %q", got)
	}
	if got := statusview.StatusFor(2, 3); got != "generation 2; the spec is at generation 3" {
		t.Errorf("StatusFor(2, 3) = %q", got)
	}
	if got := statusview.Claimed("Apply", 2, metav1.Time{}); got != "Apply, attempt 2" {
		t.Errorf("Claimed with no start = %q", got)
	}
	at := metav1.NewTime(time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC))
	if got := statusview.Claimed("Apply", 2, at); got != "Apply, attempt 2, claimed 2026-03-01T09:00:00Z" {
		t.Errorf("Claimed = %q", got)
	}
}

// A claim that marks its dispatch before the create is known not to have
// created a Job it has no mark for; a claim that marks nothing is not.
func TestJobLeadSaysWhatTheClaimKnows(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name                   string
		uid                    string
		dispatchStarted, marks bool
		want                   string
	}{
		{"confirmed", "uid-1", true, true, "Its Job job-a is confirmed: the claim records the Job's UID uid-1"},
		{"confirmed without a mark", "uid-1", false, false, "Its Job job-a is confirmed"},
		{"create attempted", "", true, true, "is reserved and its one create was attempted, but no UID is recorded"},
		{"marked claim, no mark", "", false, true, "Its Job job-a is reserved by name and not created yet: no UID is recorded."},
		{"claim that marks nothing", "", false, false,
			"Its Job job-a is named, and no UID is recorded: whether it was created is not confirmed yet."},
	} {
		got := statusview.JobLead("job-a", types.UID(test.uid), test.dispatchStarted, test.marks)
		if !strings.Contains(got, test.want) {
			t.Errorf("%s: JobLead = %q, want %q", test.name, got, test.want)
		}
		if !test.marks && test.uid == "" && strings.Contains(got, "not created yet") {
			t.Errorf("%s: JobLead = %q, says not created yet of a claim that marks nothing", test.name, got)
		}
	}
}
