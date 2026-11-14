package statusview_test

import (
	"strings"
	"testing"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/statusview"
)

func TestMigrationLogSaysWhatTheRecordLacks(t *testing.T) {
	t.Parallel()

	tooLong := operatorv1alpha1.MigrationLogLossStatus{
		Version: 3, State: operatorv1alpha1.MigrationLogEntryStarted, SQLState: "22001", ErrorNumber: 1406,
	}
	sixteen := make([]operatorv1alpha1.MigrationLogLossStatus, 16)
	for index := range sixteen {
		sixteen[index] = tooLong
	}
	for _, test := range []struct {
		name string
		log  operatorv1alpha1.MigrationLogStatus
		want string
	}{
		{
			name: "every entry written",
			log:  operatorv1alpha1.MigrationLogStatus{Written: 4},
			want: "4 entries written, none refused",
		},
		{
			name: "nothing written",
			log:  operatorv1alpha1.MigrationLogStatus{},
			want: "0 entries written, none refused",
		},
		{
			name: "an entry refused, and one with no answer",
			log: operatorv1alpha1.MigrationLogStatus{Written: 1, Lost: 2, Losses: []operatorv1alpha1.MigrationLogLossStatus{
				tooLong, {Version: 3, State: operatorv1alpha1.MigrationLogEntryApplied},
			}},
			want: "2 of 3 entries refused, so the database's record of the run is incomplete: " +
				"version 3 Started (SQLSTATE 22001, error 1406), version 3 Applied",
		},
		{
			name: "an answer with no MySQL number",
			log: operatorv1alpha1.MigrationLogStatus{Lost: 1, Losses: []operatorv1alpha1.MigrationLogLossStatus{
				{Version: 9, State: operatorv1alpha1.MigrationLogEntryFailed, SQLState: "22P02"},
			}},
			want: "1 of 1 entry refused, so the database's record of the run is incomplete: version 9 Failed (SQLSTATE 22P02)",
		},
		{
			// Status keeps sixteen and counts the rest.
			name: "more refused than status keeps",
			log:  operatorv1alpha1.MigrationLogStatus{Lost: 18, Losses: sixteen},
			want: "18 of 18 entries refused, so the database's record of the run is incomplete: " +
				strings.Repeat("version 3 Started (SQLSTATE 22001, error 1406), ", 16) + "2 more",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := statusview.MigrationLog(test.log); got != test.want {
				t.Fatalf("MigrationLog() =\n%s\nwant\n%s", got, test.want)
			}
		})
	}
}
