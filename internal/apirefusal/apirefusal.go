// Package apirefusal reads what the API server's answer to a failed write
// says about it.
//
// kubectl ptah approve, suspend, resume, refresh and set-source send a
// write as the person who runs them and tell that person what became of it.
// They ask the same two questions of a failed write, and answer them here, so
// a refusal reads the same whichever command met it.
package apirefusal

import (
	"errors"
	"net/http"
	"regexp"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Definite reports an answer that says the API server judged the write and
// stored nothing: a status in the 4xx range. A server error, a timeout or a
// lost connection is not definite, whatever it says, because the write may
// have reached storage before the answer was lost.
func Definite(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	code := status.Status().Code
	return code >= http.StatusBadRequest && code < http.StatusInternalServerError
}

// ByRBAC reports the authorizer's refusal of the request itself: RBAC does
// not let the identity that sent it use that verb on that resource.
//
// Admission refuses with 403 as well, in its own words. A webhook that passes
// on a refusal of its own read names another identity further in, but begins
// with "admission webhook", and a ValidatingAdmissionPolicy names itself
// where the authorizer names the user, so neither matches.
func ByRBAC(err error) bool {
	var status apierrors.APIStatus
	return apierrors.IsForbidden(err) && errors.As(err, &status) && rbacRefusal.MatchString(status.Status().Message)
}

// rbacRefusal is the authorizer's refusal as the API server words it: the
// resource, then the user written with %q, then the verb it may not use.
var rbacRefusal = regexp.MustCompile(`^\S+(?: "[^"]*")? is forbidden: User "(?:[^"\\]|\\.)*" cannot [a-z]+ resource "`)
