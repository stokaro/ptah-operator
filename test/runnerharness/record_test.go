package runnerharness_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/test/runnerharness"
)

func TestRunRecordRoundTrips(t *testing.T) {
	t.Parallel()

	want := runnerharness.RunRecord{
		Prefix:  "ptah-runner-harness-1a2b3c4d",
		Expires: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC),
		Pulled:  []string{"registry.invalid/image@sha256:" + strings.Repeat("a", 64)},
	}
	label, err := want.Label()
	if err != nil {
		t.Fatal(err)
	}
	got, err := runnerharness.ParseRunRecord(label)
	if err != nil {
		t.Fatal(err)
	}
	if got.Prefix != want.Prefix || !got.Expires.Equal(want.Expires) || len(got.Pulled) != 1 || got.Pulled[0] != want.Pulled[0] {
		t.Fatalf("read %+v back, want %+v", got, want)
	}
}

// A sweep removes everything under the prefix a record names, so a record that
// does not say exactly whose objects these are removes nothing.
func TestParseRunRecordRefusesARecordASweepCouldNotTrust(t *testing.T) {
	t.Parallel()

	const expires = `"expires":"2026-11-07T12:00:00Z"`
	for name, label := range map[string]string{
		"not a record":              "1",
		"empty":                     "",
		"no prefix":                 `{` + expires + `}`,
		"an empty prefix":           `{"prefix":"",` + expires + `}`,
		"a prefix matching any":     `{"prefix":".*",` + expires + `}`,
		"a prefix with an anchor":   `{"prefix":"a$|b",` + expires + `}`,
		"a prefix with a separator": `{"prefix":"a/b",` + expires + `}`,
		"no expiry":                 `{"prefix":"ptah-runner-harness-1"}`,
		"an expiry that is no time": `{"prefix":"ptah-runner-harness-1","expires":"tomorrow"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if record, err := runnerharness.ParseRunRecord(label); err == nil {
				t.Fatalf("read %q as %+v, want a refusal", label, record)
			}
		})
	}
}

func TestRunRecordExpiresOnlyAfterItsTime(t *testing.T) {
	t.Parallel()

	expires := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	record := runnerharness.RunRecord{Prefix: "ptah-runner-harness-1", Expires: expires}
	for _, row := range []struct {
		now  time.Time
		want bool
	}{
		{expires.Add(-time.Second), false},
		{expires, false},
		{expires.Add(time.Second), true},
	} {
		if got := record.Expired(row.now); got != row.want {
			t.Errorf("Expired(%s) = %t, want %t", row.now, got, row.want)
		}
	}
}
