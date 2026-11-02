// Package desiredspec decides which part of a PtahSchema's or a
// PtahMigration's spec is the desired state its plans come from: all of it but
// spec.suspend. Suspending and resuming change when the operator acts, not
// what it plans.
//
// Admission judges the recorded spec writer by it (internal/admission,
// specWriterValues), so pausing a resource does not make the person who paused
// it its author. kubectl ptah approve holds a decision to it, so a suspend and
// a resume between the review and the answer do not void the review, and any
// other edit of the spec does.
package desiredspec

import "encoding/json"

// WithoutSuspend is spec, decoded from JSON, without its top-level suspend
// key. Only the top-level key is set aside: a key named suspend anywhere else
// in the spec is desired state like any other. A value that is not a JSON
// object is returned as it is.
func WithoutSuspend(spec any) any {
	fields, ok := spec.(map[string]any)
	if !ok {
		return spec
	}
	kept := make(map[string]any, len(fields))
	for key, value := range fields {
		if key != "suspend" {
			kept[key] = value
		}
	}
	return kept
}

// Of is spec as canonical JSON without its top-level suspend key, so two specs
// that differ only in spec.suspend give the same string. spec is a typed spec
// or decoded JSON; a typed spec carries only the fields its type knows.
func Of(spec any) (string, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", err
	}
	// encoding/json writes the keys of a map in sorted order.
	canonical, err := json.Marshal(WithoutSuspend(decoded))
	if err != nil {
		return "", err
	}
	return string(canonical), nil
}
