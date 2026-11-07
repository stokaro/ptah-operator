package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/targetfailure"
)

// TestARunAccountFromPtahV023Documents writes the message and the status
// failure for run documents Ptah printed at f47b2bfa3, the commit v0.23.0 is
// to be tagged on (see testdata/ptah-f47b2bfa3 at the repository root). Each
// report is decoded and then cut the way the runner cuts it before the frame,
// with Ptah's sentence and the revision row's text gone;
// TestMigrationRunCarriesPtahV023CommitFailure and
// TestMigrationRunCarriesPtahV023UnknownMySQLRun in internal/runner hold the
// runner to that.
func TestARunAccountFromPtahV023Documents(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		file string
		want string
		// failure is whether status.lastRun.failure is written. A failed
		// commit has no statement position for it to hold.
		failure bool
	}{
		{
			file: "migrations-up-commit-failed.json",
			want: "Migration 1 (Create) failed at its commit with SQLSTATE 23503 and committed nothing; " +
				"0 migrations before it are recorded applied",
		},
		{
			file: "migrations-up-commit-failed-second.json",
			want: "Migration 2 (Orphan) failed at its commit with SQLSTATE 23503 and committed nothing; " +
				"1 migration before it is recorded applied",
		},
		{
			file: "migrations-up-commit-failed-tx-all.json",
			want: "Migration 1 (Create) failed at its commit with SQLSTATE 23503 and committed nothing; " +
				"0 migrations before it are recorded applied",
		},
		{
			file: "migrations-up-commit-failed-tx-all-second.json",
			want: "Migration 2 (Orphan) failed at its commit with SQLSTATE 23503 and committed nothing; " +
				"0 migrations before it are recorded applied",
		},
		{
			file: "migrations-up-unknown-mysql-ddl.json",
			want: "Migration 1 (Change) stopped at statement 1 of 1 with SQLSTATE 23000 (error 1062), " +
				"and whether it committed is not recorded",
			failure: true,
		},
	} {
		t.Run(row.file, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "ptah-f47b2bfa3", row.file))
			if err != nil {
				t.Fatal(err)
			}
			report, err := dataplane.DecodeMigrationRun(data)
			if err != nil {
				t.Fatalf("DecodeMigrationRun() error = %v", err)
			}
			report.Error = nil
			if report.Status != nil && report.Status.DirtyRevision != nil {
				report.Status.DirtyRevision.Error = ""
			}
			if got := migrationRunMessage(report); got != row.want {
				t.Errorf("migrationRunMessage() = %q\nwant %q", got, row.want)
			}
			failure := migrationRunFailure(report.Failure)
			if (failure != nil) != row.failure {
				t.Fatalf("migrationRunFailure() = %+v, want written %t", failure, row.failure)
			}
			if failure != nil && (failure.Statement < 1 || failure.Statement > failure.Statements) {
				t.Fatalf("migrationRunFailure() = %+v, which the schema's statement bounds refuse", failure)
			}
		})
	}
}

// A permission_denied result that names the kinds of object a MySQL user
// cannot see reads, in status and in DatabaseReachable, as the operator's
// sentence about them; the runner's message is not read. Without the kinds it
// reads as the plain permission_denied.
func TestUnseenObjectsReadAsTheOperatorsSentence(t *testing.T) {
	t.Parallel()

	url := corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "accounts-db"}, Key: "url"}
	named := resultFailure(&runner.ResultError{
		Code: "permission_denied", Message: "RUNNER-TEXT",
		UnseenObjects: []string{dataplane.UnseenTriggers, dataplane.UnseenRoutines},
	}, url)
	want := targetfailure.UnseenObjectsMessage(dataplane.TargetPermissionDenied,
		[]string{dataplane.UnseenTriggers, dataplane.UnseenRoutines}, url)
	if named.Error() != want || !strings.Contains(want, "cannot see every trigger and routine") {
		t.Fatalf("resultFailure() = %q, want %q", named.Error(), want)
	}
	reason, message, known := databaseUnreached(named)
	if !known || reason != operatorv1alpha1.ReasonPermissionDenied || message != want {
		t.Fatalf("databaseUnreached() = %s, %q, %t; want %s and the sentence", reason, message, known, operatorv1alpha1.ReasonPermissionDenied)
	}
	plain := resultFailure(&runner.ResultError{Code: "permission_denied", Message: "RUNNER-TEXT"}, url)
	if got := plain.Error(); got != targetfailure.Message(dataplane.TargetPermissionDenied, url) {
		t.Fatalf("resultFailure() without unseen objects = %q, want the plain sentence", got)
	}
}
