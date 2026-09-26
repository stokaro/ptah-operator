// Package planview reads a stored plan back the way the operator reads it.
//
// The SQL an operator applies lives in immutable ConfigMap chunks that a plan
// manifest binds by index, key, size and digest. Reading them by hand means
// reproducing that binding, and the binding is not a format: a chunk boundary
// falls wherever 512 KiB falls, which may be the middle of a SQL string, of a
// JSON escape, or of a UTF-8 character. This package resolves which plan a
// reader asked for, hands the reconstruction to the operator's own store, and
// returns bytes that have already been checked against everything the plan
// says about itself.
package planview

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/planstore"
)

// Selection is which stored plan a reader asked for.
//
// They are two different questions and neither answers the other: the current
// plan is what the operator would run next, and the applied plan is what the
// last confirmed apply ran. A reader asking for one is never given the other,
// and neither is "the newest plan object in the namespace".
type Selection string

const (
	// Current is the plan the schema's status names now.
	Current Selection = "current"
	// Applied is the plan the last confirmed apply ran.
	Applied Selection = "applied"
)

// Errors a caller distinguishes. Absence is not an empty plan: a reader who
// asked for SQL that is not there is told so, with a status to match.
var (
	ErrSchemaNotFound = errors.New("no such schema")
	ErrNoPlan         = errors.New("no such plan")
	ErrUnsupported    = errors.New("unsupported plan format")
)

// View is one verified plan.
//
// Document holds the bytes the store reconstructed and checked against the
// plan's content digest. They are the plan as it was published, and printing
// them is how a reader gets a document that still matches that digest.
type View struct {
	Namespace   string
	Schema      string
	Selection   Selection
	PlanName    string
	PlanUID     types.UID
	Fingerprint string

	ContentDigest  string
	Dialect        string
	Destructive    bool
	StatementCount int
	CreatedAt      metav1.Time
	// CompletedAt is when the apply this plan belongs to finished. It is set
	// for an applied plan and empty for a current one, which has not run.
	CompletedAt *metav1.Time

	Document []byte
	Plan     dataplane.PlanFile
}

// Load resolves the selected plan, reconstructs it and verifies it.
//
// Nothing reaches the caller before every check has held. A plan whose last
// chunk is corrupt produces an error and no document, rather than an error
// after half the SQL has already been handed over.
func Load(
	ctx context.Context,
	reader client.Reader,
	namespace, name string,
	selection Selection,
) (View, error) {
	schema := &operatorv1alpha1.PtahSchema{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, schema); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return View{}, fmt.Errorf("%w: %s/%s", ErrSchemaNotFound, namespace, name)
		}
		return View{}, fmt.Errorf("read schema %s/%s: %w", namespace, name, err)
	}

	plan, completedAt, err := resolve(ctx, reader, schema, selection)
	if err != nil {
		return View{}, err
	}

	document, err := planstore.Store{Reader: reader}.Load(ctx, plan)
	if err != nil {
		return View{}, fmt.Errorf("read the stored plan %s: %w", plan.Name, err)
	}
	// Against the dialect the manifest recorded, not against the engine the
	// schema targets now. A plan that was applied is a plan that was applied,
	// and a target changed since then does not make it unreadable.
	decoded, err := dataplane.DecodePlan(document, plan.Spec.Dialect)
	if err != nil {
		return View{}, fmt.Errorf("%w: %s: %w", ErrUnsupported, plan.Name, err)
	}

	return View{
		Namespace:      schema.Namespace,
		Schema:         schema.Name,
		Selection:      selection,
		PlanName:       plan.Name,
		PlanUID:        plan.UID,
		Fingerprint:    plan.Spec.Fingerprint,
		ContentDigest:  plan.Spec.ContentDigest,
		Dialect:        plan.Spec.Dialect,
		Destructive:    plan.Spec.Destructive,
		StatementCount: len(decoded.Statements),
		CreatedAt:      plan.CreationTimestamp,
		CompletedAt:    completedAt,
		Document:       document,
		Plan:           decoded,
	}, nil
}

// resolve returns the plan object the selection names, and when its apply
// finished if it has one.
func resolve(
	ctx context.Context,
	reader client.Reader,
	schema *operatorv1alpha1.PtahSchema,
	selection Selection,
) (*operatorv1alpha1.PtahSchemaPlan, *metav1.Time, error) {
	switch selection {
	case Current:
		current := schema.Status.Plan
		if current == nil || current.Name == "" {
			return nil, nil, fmt.Errorf("%w: %s/%s has no current plan", ErrNoPlan, schema.Namespace, schema.Name)
		}
		plan, err := get(ctx, reader, schema, current.Name, current.UID, current.Fingerprint)
		return plan, nil, err
	case Applied:
		applied := schema.Status.Applied
		if applied == nil || applied.PlanFingerprint == "" {
			return nil, nil, fmt.Errorf(
				"%w: %s/%s records no confirmed apply", ErrNoPlan, schema.Namespace, schema.Name)
		}
		completedAt := applied.CompletedAt
		plan, err := get(ctx, reader, schema, applied.PlanRef.Name, applied.PlanRef.UID, applied.PlanFingerprint)
		return plan, &completedAt, err
	default:
		return nil, nil, fmt.Errorf("unknown selection %q", selection)
	}
}

// get reads the named plan and refuses one that is not the plan the status
// meant: a name is reused when an object is recreated, and a UID is not.
func get(
	ctx context.Context,
	reader client.Reader,
	schema *operatorv1alpha1.PtahSchema,
	name string,
	uid types.UID,
	fingerprint string,
) (*operatorv1alpha1.PtahSchemaPlan, error) {
	if name == "" {
		return nil, fmt.Errorf("%w: the record names no stored plan", ErrNoPlan)
	}
	if uid == "" {
		return nil, fmt.Errorf(
			"the record names the stored plan %s without a UID, so nothing says it is still the same object", name)
	}
	plan := &operatorv1alpha1.PtahSchemaPlan{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: schema.Namespace, Name: name}, plan); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, fmt.Errorf("%w: the stored plan %s is gone", ErrNoPlan, name)
		}
		return nil, fmt.Errorf("read the stored plan %s: %w", name, err)
	}
	if err := bound(plan, schema, uid, fingerprint); err != nil {
		return nil, err
	}
	return plan, nil
}

// bound reports whether a plan object is the one a status meant.
func bound(
	plan *operatorv1alpha1.PtahSchemaPlan,
	schema *operatorv1alpha1.PtahSchema,
	uid types.UID,
	fingerprint string,
) error {
	if plan.UID != uid {
		return fmt.Errorf(
			"the stored plan %s has UID %q and the record names %q, so it is a different object under the same name",
			plan.Name, plan.UID, uid)
	}
	if fingerprint != "" && plan.Spec.Fingerprint != fingerprint {
		return fmt.Errorf(
			"the stored plan %s carries the fingerprint %s and the record names %s",
			plan.Name, plan.Spec.Fingerprint, fingerprint)
	}
	if plan.Spec.SchemaRef.Name != schema.Name || plan.Spec.SchemaRef.UID != schema.UID {
		return fmt.Errorf(
			"the stored plan %s belongs to %s/%s, not to this schema",
			plan.Name, plan.Namespace, plan.Spec.SchemaRef.Name)
	}
	return nil
}
