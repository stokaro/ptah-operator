// Package operate changes and waits on a PtahSchema or a PtahMigration the
// way kubectl ptah suspend, resume, refresh and wait do.
//
// Every change is a JSON patch that first tests metadata.uid, so a resource
// deleted and created again under the same name between the read and the
// write is refused rather than changed. A change touches one field and
// nothing else, and is sent under the field manager kubectl-ptah.
//
// Suspension prevents new Jobs and never cancels SQL that has started. An
// Apply already running is observed to its end; any proof still owed after
// an Apply waits for resume; a read under way, a migration's read of its
// history after a run included, is dropped. SuspendResult.Effect says which
// applies. A read request asks for a fresh read of the database; it starts no
// mutating operation and authorizes nothing.
//
// A wait reads the resource until it reaches a target, and never counts state
// that was already there when the wait began: each target compares what it
// reads with a baseline document, using the timestamps the operator wrote
// rather than the clock of the process that waits.
package operate

import (
	"errors"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// FieldManager is the field manager every change is sent under.
const FieldManager = "kubectl-ptah"

// ErrReplaced reports that the resource first read is not the one under its
// name now: it was deleted and created again. A change that returns it
// changed nothing, and a wait that returns it stopped reading.
var ErrReplaced = errors.New("the resource was deleted and created again under the same name")

// Family is the resource family a command acts on.
type Family string

const (
	// FamilySchema is a PtahSchema.
	FamilySchema Family = "schema"
	// FamilyMigration is a PtahMigration.
	FamilyMigration Family = "migration"
)

// ParseFamily reads the family a command line names: schema or migration,
// or the kind's resource name, plural or short name as kubectl spells them.
func ParseFamily(value string) (Family, error) {
	switch strings.ToLower(value) {
	case "schema", "ptahschema", "ptahschemas", "ptahs":
		return FamilySchema, nil
	case "migration", "ptahmigration", "ptahmigrations", "ptahm":
		return FamilyMigration, nil
	default:
		return "", fmt.Errorf("unknown resource family %q: use schema or migration", value)
	}
}

// Ref names one resource.
type Ref struct {
	Family    Family
	Namespace string
	Name      string
}

// ParseRef reads a family/name argument, such as schema/storefront, in
// namespace.
func ParseRef(value, namespace string) (Ref, error) {
	family, name, found := strings.Cut(value, "/")
	if !found || name == "" || strings.Contains(name, "/") {
		return Ref{}, fmt.Errorf("%q does not name a resource: write schema/NAME or migration/NAME", value)
	}
	parsed, err := ParseFamily(family)
	if err != nil {
		return Ref{}, err
	}
	if namespace == "" {
		return Ref{}, errors.New("a namespace is required")
	}
	return Ref{Family: parsed, Namespace: namespace, Name: name}, nil
}

// Kind is the Kubernetes kind the family stands for.
func (r Ref) Kind() string {
	if r.Family == FamilyMigration {
		return "PtahMigration"
	}
	return "PtahSchema"
}

// String is the resource as kubectl names it: ptahschema/storefront in
// namespace demo.
func (r Ref) String() string {
	return fmt.Sprintf("%s/%s in namespace %s", strings.ToLower(r.Kind()), r.Name, r.Namespace)
}

func (r Ref) key() client.ObjectKey {
	return client.ObjectKey{Namespace: r.Namespace, Name: r.Name}
}

func (r Ref) newObject() (client.Object, error) {
	switch r.Family {
	case FamilySchema:
		return &operatorv1alpha1.PtahSchema{}, nil
	case FamilyMigration:
		return &operatorv1alpha1.PtahMigration{}, nil
	default:
		return nil, fmt.Errorf("unknown resource family %q", r.Family)
	}
}

// failed puts the resource's name on an API error. The error stays
// inspectable: apierrors.IsNotFound and apierrors.IsForbidden still see it,
// and the message is the API server's own.
func (r Ref) failed(verb string, err error) error {
	return fmt.Errorf("%s %s: %w", verb, r, err)
}
