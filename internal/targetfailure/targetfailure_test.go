package targetfailure_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/targetfailure"
)

// TestEachKindHasItsSentence holds each kind to the sentence a condition, an
// Event and kubectl ptah show for it: the kind, what it means, and the Secret
// key the operation read its URL from. A kind outside the vocabulary has none.
func TestEachKindHasItsSentence(t *testing.T) {
	t.Parallel()

	url := corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "demo-database-broken"}, Key: "url"}
	want := map[dataplane.TargetFailureKind]string{
		dataplane.TargetDatabaseUnreachable: "database_unreachable: the target database did not answer: its host did not " +
			"resolve, refused the connection, or did not respond in time. Check the host and port in the URL that " +
			`Secret "demo-database-broken" holds under key "url", and that the database is running and accepts ` +
			"connections from this namespace",
		dataplane.TargetAuthenticationFailed: "authentication_failed: the target database refused the credentials. " +
			`Check the user and password in the URL that Secret "demo-database-broken" holds under key "url"`,
		dataplane.TargetDatabaseNotFound: "database_not_found: the target database server has no database by the name " +
			`the URL gives. Check the database name in the URL that Secret "demo-database-broken" holds under key "url", ` +
			"or create the database",
		dataplane.TargetPermissionDenied: "permission_denied: the target database server accepted the credentials and " +
			`refused the user access to the database. Check the user in the URL that Secret "demo-database-broken" holds ` +
			`under key "url", or grant that user access to the database`,
		dataplane.TargetTLSFailed: "tls_failed: the TLS handshake with the target database failed. Check the TLS settings " +
			`in the URL that Secret "demo-database-broken" holds under key "url" against the certificate the server presents`,
	}
	if len(want) != len(dataplane.TargetFailureKinds()) {
		t.Fatalf("the table names %d kinds, the vocabulary %d", len(want), len(dataplane.TargetFailureKinds()))
	}
	for _, kind := range dataplane.TargetFailureKinds() {
		if message := targetfailure.Message(kind, url); message != want[kind] {
			t.Errorf("Message(%s) =\n%q\nwant\n%q", kind, message, want[kind])
		}
	}
	for _, unknown := range []dataplane.TargetFailureKind{"", "lock_timeout", "DATABASE_UNREACHABLE"} {
		if message := targetfailure.Message(unknown, url); message != "" {
			t.Errorf("Message(%q) = %q, want no sentence", unknown, message)
		}
	}
}

// A permission_denied that names what the user cannot see says so, and names
// no database, object or privilege; anything else reads as Message does.
func TestUnseenObjectsHaveTheirSentence(t *testing.T) {
	t.Parallel()

	url := corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "demo-database"}, Key: "url"}
	got := targetfailure.UnseenObjectsMessage(dataplane.TargetPermissionDenied,
		[]string{dataplane.UnseenTriggers, dataplane.UnseenRoutines}, url)
	want := "permission_denied: the target database server accepted the credentials, and the user cannot see every " +
		`trigger and routine in the database. Grant the user in the URL that Secret "demo-database" holds under key "url" ` +
		"the privileges that show them, which the MySQL authority section of the operator's supported databases page lists"
	if got != want {
		t.Fatalf("UnseenObjectsMessage() =\n%q\nwant\n%q", got, want)
	}
	for _, row := range []struct {
		name    string
		kind    dataplane.TargetFailureKind
		objects []string
	}{
		{name: "another kind", kind: dataplane.TargetAuthenticationFailed, objects: []string{dataplane.UnseenTriggers}},
		{name: "no objects", kind: dataplane.TargetPermissionDenied},
		{name: "an object outside the vocabulary", kind: dataplane.TargetPermissionDenied, objects: []string{"events"}},
		{name: "objects out of order", kind: dataplane.TargetPermissionDenied,
			objects: []string{dataplane.UnseenRoutines, dataplane.UnseenTriggers}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if got, want := targetfailure.UnseenObjectsMessage(row.kind, row.objects, url), targetfailure.Message(row.kind, url); got != want {
				t.Fatalf("UnseenObjectsMessage() = %q, want Message's %q", got, want)
			}
		})
	}
}
