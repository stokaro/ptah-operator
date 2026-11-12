package statusview_test

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stokaro/ptah-operator/internal/statusview"
)

// A read request is answered only when the controller recorded the value the
// annotation holds now; an older answer beside a newer request is a read
// still owed.
func TestReadRequestSaysWhetherTheReadWasAnswered(t *testing.T) {
	t.Parallel()

	for _, test := range []struct{ requested, answered, want string }{
		{"", "", ""},
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
