// Package targetfailure writes what the operator says about an operation that
// could not open its target database: the kind of failure Ptah reported, what
// that kind means, and the Secret key the operation read its URL from.
//
// Every word of the sentence is the operator's own. The kind comes from a
// closed vocabulary (dataplane.TargetFailureKind), and the Secret's name and
// key are the resource's own data. Nothing Ptah or the database wrote is in
// it, so it may reach a condition, an Event and kubectl ptah.
package targetfailure

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// checks says, for each kind, where a person looks next.
var checks = map[dataplane.TargetFailureKind]string{
	dataplane.TargetDatabaseUnreachable: "Check the host and port in the URL that Secret %q holds under key %q, " +
		"and that the database is running and accepts connections from this namespace",
	dataplane.TargetAuthenticationFailed: "Check the user and password in the URL that Secret %q holds under key %q",
	dataplane.TargetDatabaseNotFound: "Check the database name in the URL that Secret %q holds under key %q, " +
		"or create the database",
	dataplane.TargetPermissionDenied: "Check the user in the URL that Secret %q holds under key %q, " +
		"or grant that user access to the database",
	dataplane.TargetTLSFailed: "Check the TLS settings in the URL that Secret %q holds under key %q " +
		"against the certificate the server presents",
}

// Message is the sentence for kind, naming the Secret key url. A kind outside
// the vocabulary has no sentence, and Message returns the empty string.
func Message(kind dataplane.TargetFailureKind, url corev1.SecretKeySelector) string {
	check, known := checks[kind]
	if !known {
		return ""
	}
	return fmt.Sprintf("%s: %s. "+check, kind, kind.Description(), url.Name, url.Key)
}

// unseenObjectsCheck is where a person looks when the user reached the
// database and could not see every object in it.
const unseenObjectsCheck = "Grant the user in the URL that Secret %q holds under key %q the privileges that show them, " +
	"which the MySQL authority section of the operator's supported databases page lists"

// UnseenObjectsMessage is the sentence for a permission_denied failure that
// names the kinds of object the user cannot see, from dataplane's closed
// vocabulary. Any other kind, and a list dataplane.ValidateUnseenObjects
// refuses, gets Message's sentence instead.
func UnseenObjectsMessage(kind dataplane.TargetFailureKind, objects []string, url corev1.SecretKeySelector) string {
	if kind != dataplane.TargetPermissionDenied || dataplane.ValidateUnseenObjects(objects) != nil {
		return Message(kind, url)
	}
	return fmt.Sprintf("%s: %s. "+unseenObjectsCheck, kind, dataplane.UnseenObjectsDescription(objects), url.Name, url.Key)
}
