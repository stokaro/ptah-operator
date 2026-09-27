package crd_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// cause is one reason a refusal has to carry: the field the API server names,
// exactly, and a part of what it says about it.
type cause struct {
	field   string
	message string
}

// refusalMismatch says why err is not the refusal the row expected. It returns
// nil only for an Invalid refusal that names every wanted field with its
// message; an admission, any other kind of error, or a refusal for something
// else is a mismatch, and the returned error quotes what the API server said.
func refusalMismatch(err error, want ...cause) error {
	if len(want) == 0 {
		return errors.New("the row names no cause, so any refusal would satisfy it")
	}
	if err == nil {
		return errors.New("the API server admitted the object")
	}
	if !apierrors.IsInvalid(err) {
		return fmt.Errorf("the API server refused with reason %s rather than Invalid: %v", apierrors.ReasonForError(err), err)
	}
	var status apierrors.APIStatus
	if !errors.As(err, &status) || status.Status().Details == nil || len(status.Status().Details.Causes) == 0 {
		return fmt.Errorf("the refusal names no field: %v", err)
	}
	causes := status.Status().Details.Causes
	var missing []string
	for _, wanted := range want {
		found := false
		for _, actual := range causes {
			if actual.Field == wanted.field && strings.Contains(actual.Message, wanted.message) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, fmt.Sprintf("%s %q", wanted.field, wanted.message))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the refusal lacks %s; the API server said %s",
			strings.Join(missing, " and "), describeCauses(causes))
	}
	return nil
}

func describeCauses(causes []metav1.StatusCause) string {
	described := make([]string, 0, len(causes))
	for _, actual := range causes {
		described = append(described, fmt.Sprintf("[%s %s: %s]", actual.Type, actual.Field, actual.Message))
	}
	return strings.Join(described, " ")
}

// refusal is one object the schema must refuse: a mutation of a valid base, and
// the causes the API server has to name.
type refusal struct {
	name   string
	mutate func(t *testing.T, object *unstructured.Unstructured)
	want   []cause
}

// assertRefusals checks the base is admitted, then that every row is refused
// for the reasons it names. The base is the control: a row refused because the
// base itself is broken would pass for the wrong reason.
func assertRefusals(t *testing.T, base func() *unstructured.Unstructured, rows []refusal) {
	t.Helper()
	control := base()
	if err := dryRunCreate(control); err != nil {
		t.Fatalf("the base %s the rows mutate was refused, so no row below could prove anything: %v",
			control.GetKind(), err)
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			object := base()
			row.mutate(t, object)
			if mismatch := refusalMismatch(dryRunCreate(object), row.want...); mismatch != nil {
				t.Errorf("%s %s: %v", object.GetKind(), object.GetName(), mismatch)
			}
		})
	}
}

// The matcher is the filter every refusal row passes through, so it has to be
// shown to refuse: an admission, a refusal of another kind, and a refusal for a
// different field or message must each be reported, and only the exact cause
// accepted.
func TestTheRefusalMatcherRefusesWhatARowDidNotAskFor(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "matcher")
	object := schemaBase(namespace)()
	remove(object, "spec", "target", "engine")
	refused := dryRunCreate(object)
	engineRequired := cause{field: "spec.target.engine", message: "Required value"}
	if mismatch := refusalMismatch(refused, engineRequired); mismatch != nil {
		t.Fatalf("the matcher rejected the refusal it was built from: %v", mismatch)
	}

	tests := []struct {
		name string
		err  error
		want []cause
	}{
		{name: "an admission", err: nil, want: []cause{engineRequired}},
		{
			name: "a refusal that is not Invalid",
			err:  apierrors.NewForbidden(schema.GroupResource{Group: "operator.ptah.run", Resource: "ptahschemas"}, "x", errors.New("no")),
			want: []cause{engineRequired},
		},
		{name: "a refusal for another field", err: refused, want: []cause{{field: "spec.target.urlFrom", message: "Required value"}}},
		{name: "a parent of the refused field", err: refused, want: []cause{{field: "spec.target", message: "Required value"}}},
		{name: "the right field with another message", err: refused, want: []cause{{field: "spec.target.engine", message: "Unsupported value"}}},
		{name: "one cause present and one absent", err: refused, want: []cause{engineRequired, {field: "spec.desired", message: "Required value"}}},
		{name: "no cause at all", err: refused, want: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if refusalMismatch(test.err, test.want...) == nil {
				t.Fatalf("the matcher accepted %v as %v", test.err, test.want)
			}
		})
	}
}
