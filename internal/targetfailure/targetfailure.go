// Package targetfailure writes what the operator says about an operation that
// could not open its target database: the kind of failure Ptah reported, what
// that kind means, and where the operation read its URL from: a Secret key or
// a SecretProviderClass object.
//
// Every word of the sentence is the operator's own. The kind comes from a
// closed vocabulary (dataplane.TargetFailureKind), and the names of the Secret
// and its key, or of the class and its object, are the resource's own data.
// Nothing Ptah or the database wrote is in it, so it may reach a condition, an
// Event and kubectl ptah.
package targetfailure

import (
	"fmt"

	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/urlsource"
)

// checks says, for each kind, where a person looks next.
var checks = map[dataplane.TargetFailureKind]string{
	dataplane.TargetDatabaseUnreachable: "Check the host and port in the URL that %s, " +
		"and that the database is running and accepts connections from this namespace",
	dataplane.TargetAuthenticationFailed: "Check the user and password in the URL that %s",
	dataplane.TargetDatabaseNotFound: "Check the database name in the URL that %s, " +
		"or create the database",
	dataplane.TargetPermissionDenied: "Check the user in the URL that %s, " +
		"or grant that user access to the database",
	dataplane.TargetTLSFailed: "Check the TLS settings in the URL that %s " +
		"against the certificate the server presents",
}

// Message is the sentence for kind, naming where the operation read url from.
// A kind outside the vocabulary has no sentence, and Message returns the empty
// string.
func Message(kind dataplane.TargetFailureKind, url urlsource.Source) string {
	check, known := checks[kind]
	if !known {
		return ""
	}
	return fmt.Sprintf("%s: %s. "+check, kind, kind.Description(), url.Holder())
}

// unseenObjectsCheck is where a person looks when the user reached the
// database and could not see every object in it.
const unseenObjectsCheck = "Grant the user in the URL that %s the privileges that show them, " +
	"which the MySQL authority section of the operator's supported databases page lists"

// UnseenObjectsMessage is the sentence for a permission_denied failure that
// names the kinds of object the user cannot see, from dataplane's closed
// vocabulary. Any other kind, and a list dataplane.ValidateUnseenObjects
// refuses, gets Message's sentence instead.
func UnseenObjectsMessage(kind dataplane.TargetFailureKind, objects []string, url urlsource.Source) string {
	if kind != dataplane.TargetPermissionDenied || dataplane.ValidateUnseenObjects(objects) != nil {
		return Message(kind, url)
	}
	return fmt.Sprintf("%s: %s. "+unseenObjectsCheck, kind, dataplane.UnseenObjectsDescription(objects), url.Holder())
}
