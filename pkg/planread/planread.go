// Package planread reads a stored schema plan back the way the operator reads
// it, for programs outside this module.
//
// The SQL an operator applies lives in immutable PtahSchemaPlanChunk objects
// that a plan manifest binds by index, size and digest. Reading them by hand
// means reproducing that binding, and the binding is not a format: a chunk
// boundary falls wherever 512 KiB falls, which may be the middle of a SQL
// string, of a JSON escape, or of a UTF-8 character. Load resolves which plan
// a reader asked for, hands the reconstruction to the operator's own store,
// and returns nothing until every check the plan makes about itself has held.
//
// `kubectl ptah plan` reads through this package, so there is one reader.
// Every read goes through the client the caller passes in, which is how a
// program reads as the person using it: a reader without the grant on chunks
// gets the API server's Forbidden, unchanged.
//
// The package carries no compatibility promise before v1, like the API it
// reads. Pin the module version.
package planread

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/plancontract"
	"github.com/stokaro/ptah-operator/internal/planstore"
)

// Limits a reader can size its own buffers and messages by. They are the
// operator's, not a copy of them.
const (
	// ChunkBytes is the largest piece of a plan one chunk object holds.
	ChunkBytes = plancontract.ChunkBytes
	// MaxChunks is the most chunks one plan is stored in.
	MaxChunks = plancontract.MaxChunks
	// MaxPlanBytes is the largest complete plan document.
	MaxPlanBytes = plancontract.MaxExecutableBytes
)

// SupportedContract reports whether this build reads plans of a contract
// version: 1, a plan computed from an OCI artifact, and 2, a plan computed
// from a ConfigMap. A plan that records another one is refused with
// ErrUnsupported.
func SupportedContract(version int32) bool {
	return fingerprint.ValidatePlanContractVersion(version) == nil
}

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

// Errors a caller tells apart with errors.Is. An error that matches none of
// them is the API's, such as Forbidden on a chunk, and apierrors reads it.
var (
	// ErrSchemaNotFound is a schema that does not exist.
	ErrSchemaNotFound = errors.New("no such schema")
	// ErrNoPlan is a selection the schema has no plan for, or a plan that is
	// gone. Absence is not an empty plan.
	ErrNoPlan = errors.New("no such plan")
	// ErrUnsupported is a plan this build cannot read: another plan contract
	// or another document format.
	ErrUnsupported = errors.New("unsupported plan format")
	// ErrIntegrity is a stored plan that failed a check: a plan recreated
	// under the name the status holds, a plan of another schema, a storage
	// commit that did not finish, a chunk that is missing, replaced, owned by
	// something else, or does not match its digest.
	ErrIntegrity = errors.New("stored plan failed verification")
)

// Statement is one statement of a plan, as stored.
type Statement struct {
	SQL string
	// Severity is Ptah's classification of the statement.
	Severity string
	// Reason is why Ptah classified it so, when it said.
	Reason string
}

// RowSet is one declared row set a plan read: names and no values.
type RowSet struct {
	Schema  string
	Table   string
	Keys    []string
	Columns []string
}

// Chunk is one verified piece of a plan's storage.
type Chunk struct {
	Name   string
	Index  int32
	Size   int32
	Digest string
}

// Plan is one verified plan.
type Plan struct {
	Namespace string
	Schema    string
	SchemaUID types.UID
	Selection Selection

	PlanName      string
	PlanUID       types.UID
	Fingerprint   string
	ContentDigest string
	Dialect       string
	// Destructive is what the plan object records.
	Destructive bool
	// PrivilegeChanges are the kinds of authority the plan object records.
	// They are what the manager decided on: any of them made the plan wait
	// for an approval under apply: Always.
	PrivilegeChanges []string
	// UnrecordedPrivilegeChanges are kinds this build reads in the SQL and
	// the plan object does not record. The manager that published the plan did
	// not hold the plan for them, so they are kept apart from the recorded
	// ones rather than shown as a reason the plan waited. They appear only
	// where the reader and the manager are different builds.
	UnrecordedPrivilegeChanges []string

	Statements  []Statement
	ManagedRows []RowSet
	Chunks      []Chunk

	CreatedAt metav1.Time
	// CompletedAt is when the apply this plan belongs to finished. It is set
	// for an applied plan and empty for a current one, which has not run.
	CompletedAt *metav1.Time

	// Manifest is the plan object the checks held for. Its spec carries every
	// identifier an approval names.
	Manifest operatorv1alpha1.PtahSchemaPlan
	// Document is the bytes the store reconstructed and checked against the
	// plan's content digest: the plan as it was published.
	Document []byte
}

// SQL renders the statements as they were stored: one per entry, in order,
// each terminated once. Nothing is re-split on a semicolon, because a
// statement may carry one inside a string, a comment or a function body.
func (p Plan) SQL() string {
	file := dataplane.PlanFile{Statements: make([]dataplane.PlanStatement, 0, len(p.Statements))}
	for _, statement := range p.Statements {
		file.Statements = append(file.Statements, dataplane.PlanStatement{
			SQL: statement.SQL, Severity: statement.Severity, Reason: statement.Reason,
		})
	}
	return dataplane.StatementsSQL(file)
}

// Load resolves the selected plan, reconstructs it and verifies it.
//
// Nothing reaches the caller before every check has held. A plan whose last
// chunk is corrupt produces an error and an empty Plan, rather than an error
// after half the SQL has been handed over.
func Load(
	ctx context.Context,
	reader client.Reader,
	namespace, name string,
	selection Selection,
) (Plan, error) {
	schema := &operatorv1alpha1.PtahSchema{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, schema); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return Plan{}, fmt.Errorf("%w: %s/%s", ErrSchemaNotFound, namespace, name)
		}
		return Plan{}, fmt.Errorf("read schema %s/%s: %w", namespace, name, err)
	}

	plan, completedAt, err := resolve(ctx, reader, schema, selection)
	if err != nil {
		return Plan{}, err
	}
	if !SupportedContract(plan.Spec.ContractVersion) {
		return Plan{}, fmt.Errorf("%w: %s records plan contract %d, which this build does not read",
			ErrUnsupported, plan.Name, plan.Spec.ContractVersion)
	}

	document, err := planstore.Store{Reader: reader}.Load(ctx, plan)
	if err != nil {
		err = fmt.Errorf("read the stored plan %s: %w", plan.Name, err)
		if errors.Is(err, planstore.ErrVerification) {
			return Plan{}, integrity(err)
		}
		return Plan{}, err
	}
	// Against the dialect the manifest recorded, not against the engine the
	// schema targets now. A plan that was applied is a plan that was applied,
	// and a target changed since then does not make it unreadable.
	decoded, err := dataplane.DecodePlan(document, plan.Spec.Dialect)
	if err != nil {
		return Plan{}, fmt.Errorf("%w: %s: %w", ErrUnsupported, plan.Name, err)
	}

	return Plan{
		Namespace:                  schema.Namespace,
		Schema:                     schema.Name,
		SchemaUID:                  schema.UID,
		Selection:                  selection,
		PlanName:                   plan.Name,
		PlanUID:                    plan.UID,
		Fingerprint:                plan.Spec.Fingerprint,
		ContentDigest:              plan.Spec.ContentDigest,
		Dialect:                    plan.Spec.Dialect,
		Destructive:                plan.Spec.Destructive,
		PrivilegeChanges:           recorded(plan),
		UnrecordedPrivilegeChanges: unrecorded(plan, decoded),
		Statements:                 statements(decoded),
		ManagedRows:                rowSets(decoded),
		Chunks:                     chunks(plan),
		CreatedAt:                  plan.CreationTimestamp,
		CompletedAt:                completedAt,
		Manifest:                   *plan.DeepCopy(),
		Document:                   document,
	}, nil
}

// recorded is what the plan records, shown as recorded, a kind this build
// does not know included.
func recorded(plan *operatorv1alpha1.PtahSchemaPlan) []string {
	kinds := make([]string, 0, len(plan.Spec.PrivilegeChanges))
	for _, kind := range plan.Spec.PrivilegeChanges {
		kinds = append(kinds, string(kind))
	}
	return kinds
}

// unrecorded is what this reading of the SQL finds and the record lacks. The
// two agree when the reader and the manager are the same build.
func unrecorded(plan *operatorv1alpha1.PtahSchemaPlan, decoded dataplane.PlanFile) []string {
	known := make(map[string]bool, len(plan.Spec.PrivilegeChanges))
	for _, kind := range plan.Spec.PrivilegeChanges {
		known[string(kind)] = true
	}
	var kinds []string
	for _, kind := range decoded.PrivilegeChanges {
		if !known[kind] {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

func statements(decoded dataplane.PlanFile) []Statement {
	out := make([]Statement, 0, len(decoded.Statements))
	for _, statement := range decoded.Statements {
		out = append(out, Statement{SQL: statement.SQL, Severity: statement.Severity, Reason: statement.Reason})
	}
	return out
}

func rowSets(decoded dataplane.PlanFile) []RowSet {
	if len(decoded.ManagedRows) == 0 {
		return nil
	}
	out := make([]RowSet, 0, len(decoded.ManagedRows))
	for _, rows := range decoded.ManagedRows {
		out = append(out, RowSet{
			Schema:  rows.Schema,
			Table:   rows.Table,
			Keys:    append([]string(nil), rows.Keys...),
			Columns: append([]string(nil), rows.Columns...),
		})
	}
	return out
}

func chunks(plan *operatorv1alpha1.PtahSchemaPlan) []Chunk {
	out := make([]Chunk, 0, len(plan.Spec.Chunks))
	for _, ref := range plan.Spec.Chunks {
		out = append(out, Chunk{Name: ref.Name, Index: ref.Index, Size: ref.Size, Digest: ref.Digest})
	}
	return out
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
		return nil, integrity(fmt.Errorf(
			"the record names the stored plan %s without a UID, so nothing says it is still the same object", name))
	}
	plan := &operatorv1alpha1.PtahSchemaPlan{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: schema.Namespace, Name: name}, plan); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, fmt.Errorf("%w: the stored plan %s is gone", ErrNoPlan, name)
		}
		return nil, fmt.Errorf("read the stored plan %s: %w", name, err)
	}
	if err := bound(plan, schema, uid, fingerprint); err != nil {
		return nil, integrity(err)
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

// integrityError carries ErrIntegrity without changing the message it wraps.
type integrityError struct{ err error }

func (e integrityError) Error() string { return e.err.Error() }

func (e integrityError) Unwrap() error { return e.err }

func (e integrityError) Is(target error) bool { return target == ErrIntegrity }

func integrity(err error) error { return integrityError{err: err} }
