package statusview_test

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stokaro/ptah-operator/internal/statusview"
)

func TestLabelSpellsAnAPINameAsWords(t *testing.T) {
	t.Parallel()

	for name, want := range map[string]string{
		"jobUID":                        "Job UID",
		"applyPodUIDs":                  "Apply pod UIDs",
		"dispatchNotAfter":              "Dispatch not after",
		"executionBindingID":            "Execution binding ID",
		"targetIdentityDigest":          "Target identity digest",
		"sqlState":                      "SQL state",
		"id":                            "ID",
		"terminationGracePeriodSeconds": "Termination grace period seconds",
		"observationProtectedTables":    "Observation protected tables",
	} {
		if got := statusview.Label(name); got != want {
			t.Errorf("Label(%q) = %q, want %q", name, got, want)
		}
	}
}

type nested struct {
	Name string `json:"name"`
	UID  string `json:"uid,omitempty"`
}

type record struct {
	Required  string                       `json:"required"`
	Optional  string                       `json:"optional,omitempty"`
	Count     int32                        `json:"count"`
	Unset     int32                        `json:"unset,omitempty"`
	Zero      *int64                       `json:"zero,omitempty"`
	Flag      bool                         `json:"flag"`
	Off       bool                         `json:"off,omitempty"`
	At        metav1.Time                  `json:"at"`
	Never     *metav1.Time                 `json:"never,omitempty"`
	Timeout   metav1.Duration              `json:"timeout,omitempty"`
	Versions  []int64                      `json:"versions,omitempty"`
	Labels    map[string]string            `json:"labels,omitempty"`
	Limits    map[string]resource.Quantity `json:"limits,omitempty"`
	Ref       nested                       `json:"ref"`
	Empty     *nested                      `json:"empty,omitempty"`
	Items     []nested                     `json:"items,omitempty"`
	Ignored   string                       `json:"-"`
	unwritten string
}

func TestWriteFieldsPrintsWhatTheRecordHolds(t *testing.T) {
	t.Parallel()

	zero := int64(0)
	value := record{
		Required: "kept\x1b[2J", Count: 0, Zero: &zero, Flag: false,
		At:       metav1.NewTime(time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)),
		Timeout:  metav1.Duration{Duration: 90 * time.Second},
		Versions: []int64{3, 4},
		Labels:   map[string]string{"b": "2", "a": "1"},
		Limits:   map[string]resource.Quantity{"memory": resource.MustParse("64Mi")},
		Ref:      nested{Name: "plan-a", UID: "uid-a"},
		Items:    []nested{{Name: "first", UID: "uid-1"}, {Name: "second"}},
		Ignored:  "never printed", unwritten: "never printed",
	}
	var text strings.Builder
	statusview.WriteFields(&text, "  ", value)
	want := `  Required: kept\x1b[2J
  Count: 0
  Zero: 0
  Flag: false
  At: 2026-03-01T09:00:00Z
  Timeout: 1m30s
  Versions: 3, 4
  Labels: a=1, b=2
  Limits: memory=64Mi
  Ref:
    Name: plan-a
    UID: uid-a
  Items:
    - Name: first
      UID: uid-1
    - Name: second
`
	if text.String() != want {
		t.Fatalf("WriteFields printed:\n%s\nwant:\n%s", text.String(), want)
	}
}

func TestWriteScalarsLeavesOutTheRecordsItHolds(t *testing.T) {
	t.Parallel()

	var text strings.Builder
	statusview.WriteScalars(&text, "", record{
		Required: "kept", Ref: nested{Name: "plan-a"}, Empty: &nested{Name: "plan-b"}, Versions: []int64{3},
	})
	if got := text.String(); got != "Required: kept\nCount: 0\nFlag: false\n" {
		t.Fatalf("WriteScalars printed:\n%s", got)
	}
}
