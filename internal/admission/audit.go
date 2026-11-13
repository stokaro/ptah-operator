package admission

import (
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// The keys of the audit annotations a stamping webhook returns beside its
// patch. The API server records each on the request's audit event, at the
// Metadata level and above, under the answering webhook entry's name, a slash
// and the key: mapproval.operator.ptah.run/mutationRequestUID for a schema
// approval. k8s.io/apiserver's mutating dispatcher builds that key and records
// it only if it is a qualified name, whose part after the slash is at most 63
// characters.
//
// Nothing else joins a stamp to its audit event. The stamped
// mutationRequestUID is the UID of the AdmissionReview this webhook answered,
// which the API server makes for each webhook call and writes nowhere in the
// event, whose auditID is its own; at Metadata the event carries no body.
const (
	auditAnnotationMutationRequestUID = "mutationRequestUID"
	auditAnnotationUsername           = "username"
)

// withStampAnnotations names, in a mutating answer's audit annotations, the
// mutationRequestUID and the username its patch writes into the object. Only
// the path that returns the patch calls it: a refusal stores nothing it
// stamped, and names nothing.
func withStampAnnotations(response cradmission.Response, mutationRequestUID, username string) cradmission.Response {
	response.AuditAnnotations = map[string]string{
		auditAnnotationMutationRequestUID: mutationRequestUID,
		auditAnnotationUsername:           username,
	}
	return response
}
