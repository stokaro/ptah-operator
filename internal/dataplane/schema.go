package dataplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
)

const (
	// SupportedSchemaPlanContract and SupportedSchemaApplyContract are the
	// versions of the documents `ptah schema plan --json` and `ptah schema
	// apply --json` print that this operator reads.
	//
	// Ptah raises a version when a field changes meaning or leaves, and when an
	// outcome gains a value. A document announcing another version is refused
	// rather than read for the fields this operator recognizes, because those
	// fields may already mean something else.
	//
	// Plan version 3 and apply version 4 are stokaro/ptah#5173's. Each carries
	// error as an object, the kind of a failure to open the target and Ptah's
	// message, where the versions before carried the message alone, and names
	// its command. Every outcome, the plan digest, the units an apply lists and
	// the refusal mean what they meant in plan version 2 and apply version 3.
	SupportedSchemaPlanContract  = 3
	SupportedSchemaApplyContract = 4

	// SchemaPlanCommand and SchemaApplyCommand are the commands the two
	// documents name. A document naming another is refused.
	SchemaPlanCommand  = "schema plan"
	SchemaApplyCommand = "schema apply"
)

// Outcomes a plan document reports.
const (
	SchemaPlanOutcomeChanges   = "changes"
	SchemaPlanOutcomeNoChanges = "no-changes"
	SchemaPlanOutcomeRefused   = "refused"
	SchemaPlanOutcomeFailed    = "failed"
)

// Outcomes an apply document reports. Refused and failed say that nothing
// reached the target; unknown says the statements were sent and the run
// returned an error.
const (
	SchemaApplyOutcomeApplied   = "applied"
	SchemaApplyOutcomeNoChanges = "no-changes"
	SchemaApplyOutcomeDryRun    = "dry-run"
	SchemaApplyOutcomeCanceled  = "canceled"
	SchemaApplyOutcomeRefused   = "refused"
	SchemaApplyOutcomeFailed    = "failed"
	SchemaApplyOutcomeUnknown   = "unknown"
)

// Refusal codes this operator acts on. Ptah may add codes without raising the
// contract version, since every refusal means nothing reached the target, so a
// code outside this list is read as a refusal of no particular kind rather than
// refused as a document.
const (
	SchemaRefusalStalePlan      = "stale-plan"
	SchemaRefusalProtectedTable = "protected-table"
	// SchemaRefusalLockTimeout is Ptah's RefusalLockTimeout: another session
	// held the schema apply lock longer than --lock-timeout allowed. It can
	// come from another schema's Apply, since MySQL's GET_LOCK name is
	// server-wide, so it says nothing about the plan.
	SchemaRefusalLockTimeout = "lock-timeout"
	// SchemaRefusalDeclarationRefused is Ptah v0.23.0's refusal of a desired
	// schema whose declarations contradict each other, such as a foreign key
	// that references an unknown table. Ptah v0.22.0 reported the same fault
	// as a failed plan, with the same error message.
	SchemaRefusalDeclarationRefused = "declaration-refused"
	// SchemaRefusalBackslashMode is Ptah v0.34.0's refusal of a plan whose
	// statements spell a string literal with a backslash in it, against a
	// MySQL or MariaDB session whose sql_mode holds NO_BACKSLASH_ESCAPES
	// (stokaro/ptah#6400). The plan was computed for one reading of a
	// backslash and the server would store the other. `schema apply` raises
	// it before its first statement, a dry run included. Ptah v0.33.1
	// reported the same fault as a failed apply with no code.
	SchemaRefusalBackslashMode = "backslash-mode"
	// SchemaRefusalGeneratedKind is Ptah v0.35.0's refusal of a plan that
	// would change a generated column's kind, STORED to VIRTUAL or back, which
	// no statement does in place (stokaro/ptah#6466). PostgreSQL 18 makes
	// GENERATED ALWAYS AS without STORED a VIRTUAL column, and Ptah reads the
	// same declaration in a schema file as STORED. Ptah v0.34.0 reported the
	// same fault as a failed plan with no code.
	SchemaRefusalGeneratedKind = "generated-kind"
)

// The refusals of an owner the server would refuse, which `schema apply`
// raises on PostgreSQL before its first statement since Ptah v0.30.1, a dry
// run included (stokaro/ptah#6196). Each names one statement in its owner
// object: the first the server would refuse.
const (
	// SchemaRefusalMissingOwnerRole names an owner role the server does not
	// hold and no statement before it creates.
	SchemaRefusalMissingOwnerRole = "missing-owner-role"
	// SchemaRefusalOwnerTransfer names an owner the connected role may not
	// give the object to: it lacks SET on the role on PostgreSQL 16 and
	// later, and membership in it before 16 and on CockroachDB.
	SchemaRefusalOwnerTransfer = "owner-transfer"
	// SchemaRefusalOwnerSchemaPrivilege names an owner that holds no CREATE
	// on the object's schema, which the server requires of a new owner.
	SchemaRefusalOwnerSchemaPrivilege = "owner-schema-privilege"
)

var (
	schemaPlanOutcomes = []string{
		SchemaPlanOutcomeChanges,
		SchemaPlanOutcomeNoChanges,
		SchemaPlanOutcomeRefused,
		SchemaPlanOutcomeFailed,
	}
	schemaApplyOutcomes = []string{
		SchemaApplyOutcomeApplied,
		SchemaApplyOutcomeNoChanges,
		SchemaApplyOutcomeDryRun,
		SchemaApplyOutcomeCanceled,
		SchemaApplyOutcomeRefused,
		SchemaApplyOutcomeFailed,
		SchemaApplyOutcomeUnknown,
	}
	// schemaRefusalCodePattern is the shape a refusal code may take. A code is
	// an identifier the runner may name in a message, so a value that is not
	// one is a malformed document rather than a new code. An option a refusal
	// names is held to it too, for the same reason.
	schemaRefusalCodePattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
	// generatedKindPattern is the shape of a generated column's kind, STORED
	// or VIRTUAL in Ptah v0.35.0: a word of Ptah's, so the runner names it as
	// written.
	generatedKindPattern = regexp.MustCompile(`^[A-Z]{1,16}$`)
)

// SchemaRefusal is why a plan or an apply refused. Only the fields its code
// describes are set.
type SchemaRefusal struct {
	Code string `json:"code"`
	// Tables are the fenced tables a protected-table refusal names.
	Tables []string `json:"tables,omitempty"`
	// Changed is what moved under a stale-plan refusal: "schema" or "rows".
	Changed             string `json:"changed,omitempty"`
	PlanFingerprint     string `json:"plan_fingerprint,omitempty"`
	DatabaseFingerprint string `json:"database_fingerprint,omitempty"`
	// ViewOptions are the views and security options a refusal names, one
	// entry per view and option, as view-security-option does since Ptah
	// v0.29.3 (stokaro/ptah#6105). They are kept as Ptah wrote them and read
	// by NamedViewOptions, so a list in a form this operator does not read
	// leaves the rest of the document readable.
	ViewOptions json.RawMessage `json:"view_options,omitempty"`
	// ViewDefiners are the MySQL or MariaDB views a view-definer refusal
	// names, each with the account it runs as and the account the plan's
	// read ran as, which a replacement would make its definer, since Ptah
	// v0.29.4 (stokaro/ptah#6109). They are read by NamedViewDefiners.
	ViewDefiners json.RawMessage `json:"view_definers,omitempty"`
	// Owner is the statement an owner refusal names, with the owner it gives
	// the object to, since Ptah v0.30.1 (stokaro/ptah#6196). It is read by
	// NamedOwner.
	Owner json.RawMessage `json:"owner,omitempty"`
	// GeneratedColumns are the generated columns a generated-kind refusal
	// names, each with its table and the kinds the database holds and the
	// schema declares, since Ptah v0.35.0 (stokaro/ptah#6466). They are read
	// by NamedGeneratedColumns.
	GeneratedColumns json.RawMessage `json:"generated_columns,omitempty"`

	// Since Ptah v0.29.4 a definer-privilege refusal of a schema apply also
	// lists the statements whose definer the connected account may not name,
	// as definer_privileges (stokaro/ptah#6124). The operator does not read
	// the list. A plan names no definer but the account its read ran as, and
	// the runner refuses an Apply under another login than the plan's
	// (target_binding_mismatch), so an Apply meets this refusal only where
	// the plan's login has come to match another MySQL account, as
	// 'ptah'@'%' and 'ptah'@'10.244.1.%' can from Pods on different nodes.
	// It reads by its code alone, and Ptah's sentence, which names the
	// privilege, goes to the Pod log.
}

// Transient reports whether a refusal says the database moved or was busy
// rather than anything about the plan: a stale plan, or a lock timeout, which
// Ptah v0.34.0 names RefusalLockTimeout ("lock-timeout") in
// internal/atlasschema/report.go. The same read may pass the next time. Every
// other code, one this operator does not know included, is not transient: the
// same plan against the same server meets it again, so a Plan that ends with
// it stops rather than being tried again. A transient code Ptah adds later
// stops too, and waits for the interval or a read request.
func (r SchemaRefusal) Transient() bool {
	return r.Code == SchemaRefusalStalePlan || r.Code == SchemaRefusalLockTimeout
}

// SchemaRefusalOwner is the statement an owner refusal names. Statement is the
// plan's SQL, Kind the object's kind in Ptah's words and Role the owner the
// statement gives it to. Connected and Privilege are set for owner-transfer:
// the role the Apply connected as and the privilege on Role it lacks, SET or
// MEMBER. Schema is set for owner-schema-privilege: the schema Role holds no
// CREATE on. Holds says Role owns the object now and the plan gives it back
// to it, as a rebuild does.
type SchemaRefusalOwner struct {
	Statement string `json:"statement"`
	Kind      string `json:"kind"`
	Role      string `json:"role"`
	Connected string `json:"connected,omitempty"`
	Privilege string `json:"privilege,omitempty"`
	Schema    string `json:"schema,omitempty"`
	Holds     bool   `json:"holds,omitempty"`
}

// schemaRefusalPrivilegePattern is the shape of the privilege an owner-transfer
// refusal names: a word of Ptah's, such as SET or MEMBER.
var schemaRefusalPrivilegePattern = regexp.MustCompile(`^[A-Z][A-Z_]{0,31}$`)

// NamedOwner reads the statement an owner refusal names. It reports false when
// the refusal names none, and when it names one in a form this operator does
// not read: anything but an object with a statement, a kind and a role, and
// with what the code names beside them, a connected role and a privilege for
// owner-transfer and a schema for owner-schema-privilege.
//
// Every value but the privilege is returned as written. A statement comes from
// the plan and a role or a schema from the database, so a caller that shows
// one cleans it first.
func (r SchemaRefusal) NamedOwner() (SchemaRefusalOwner, bool) {
	if len(r.Owner) == 0 {
		return SchemaRefusalOwner{}, false
	}
	var owner SchemaRefusalOwner
	if err := json.Unmarshal(r.Owner, &owner); err != nil ||
		owner.Statement == "" || owner.Kind == "" || owner.Role == "" {
		return SchemaRefusalOwner{}, false
	}
	switch r.Code {
	case SchemaRefusalMissingOwnerRole:
	case SchemaRefusalOwnerTransfer:
		if owner.Connected == "" || !schemaRefusalPrivilegePattern.MatchString(owner.Privilege) {
			return SchemaRefusalOwner{}, false
		}
	case SchemaRefusalOwnerSchemaPrivilege:
		if owner.Schema == "" {
			return SchemaRefusalOwner{}, false
		}
	default:
		return SchemaRefusalOwner{}, false
	}
	return owner, true
}

// SchemaRefusalViewDefiner is one view a view-definer refusal names. View is
// spelled as the schema spells it. Definer is the account the view runs as and
// Account the one the plan's read ran as, each spelled user@host as the
// catalog and CURRENT_USER() spell it, and each empty where the read did not
// record it: Ptah refuses then too, since it cannot tell whether the definer
// would change.
type SchemaRefusalViewDefiner struct {
	View    string `json:"view"`
	Definer string `json:"definer"`
	Account string `json:"account"`
}

// NamedViewDefiners reads the views a refusal names with their definers, in
// the order Ptah lists them. It reports false when the refusal names none, and
// when it names them in a form this operator does not read: anything but a
// list of objects, each with a view and with a definer and an account that are
// strings. A definer or an account may be empty, where the read did not record
// it.
//
// Every value is returned as written. A view comes from a schema or a database
// and an account from the database, so a caller that shows one cleans it
// first.
func (r SchemaRefusal) NamedViewDefiners() ([]SchemaRefusalViewDefiner, bool) {
	named, ok := decodeRefusalList[SchemaRefusalViewDefiner](r.ViewDefiners)
	if !ok {
		return nil, false
	}
	for _, each := range named {
		if each.View == "" {
			return nil, false
		}
	}
	return named, true
}

// decodeRefusalList reads a list a refusal carries, and reports false for an
// absent or empty list and for one that is not a list of such objects.
func decodeRefusalList[Entry any](listed json.RawMessage) ([]Entry, bool) {
	if len(listed) == 0 {
		return nil, false
	}
	var named []Entry
	if err := json.Unmarshal(listed, &named); err != nil || len(named) == 0 {
		return nil, false
	}
	return named, true
}

// SchemaRefusalGeneratedColumn is one generated column a generated-kind
// refusal names. Table is the column's table as the comparison names it, with
// its schema on PostgreSQL, and Column is the column as the schema spells it.
// HeldKind is the kind the database holds and DeclaredKind the kind the
// schema declares.
type SchemaRefusalGeneratedColumn struct {
	Table        string `json:"table"`
	Column       string `json:"column"`
	HeldKind     string `json:"held_kind"`
	DeclaredKind string `json:"declared_kind"`
}

// NamedGeneratedColumns reads the generated columns a refusal names, in the
// order Ptah lists them. It reports false when the refusal names none, and
// when it names them in a form this operator does not read: anything but a
// list of objects, each with a table and a column, and with two kinds that are
// words of Ptah's.
//
// The table and the column are returned as written. They come from a schema
// or a database, so a caller that shows one cleans it first.
func (r SchemaRefusal) NamedGeneratedColumns() ([]SchemaRefusalGeneratedColumn, bool) {
	named, ok := decodeRefusalList[SchemaRefusalGeneratedColumn](r.GeneratedColumns)
	if !ok {
		return nil, false
	}
	for _, each := range named {
		if each.Table == "" || each.Column == "" ||
			!generatedKindPattern.MatchString(each.HeldKind) || !generatedKindPattern.MatchString(each.DeclaredKind) {
			return nil, false
		}
	}
	return named, true
}

// SchemaRefusalViewOption is one view and one of its options that a refusal
// names. View is spelled as the schema spells it, so it can hold any text an
// identifier can; Option is the option's name, or sql_security for a MySQL or
// MariaDB view's SQL SECURITY mode.
type SchemaRefusalViewOption struct {
	View   string `json:"view"`
	Option string `json:"option"`
}

// NamedViewOptions reads the views and options the refusal names, in the order
// Ptah lists them. It reports false when the refusal names none, and when it
// names them in a form this operator does not read: anything but a list of
// objects, each with a view and an option that is an identifier. A caller then
// names the refusal by its code alone, as it did before Ptah named the views.
//
// The view is returned as written. It comes from a schema or a database, so a
// caller that shows it cleans it first.
func (r SchemaRefusal) NamedViewOptions() ([]SchemaRefusalViewOption, bool) {
	named, ok := decodeRefusalList[SchemaRefusalViewOption](r.ViewOptions)
	if !ok {
		return nil, false
	}
	for _, each := range named {
		if each.View == "" || !schemaRefusalCodePattern.MatchString(each.Option) {
			return nil, false
		}
	}
	return named, true
}

// SchemaPlanReport is Ptah's `schema plan --json` document.
//
// The plan document it embeds is not declared, so it is not read: the file
// --output wrote is the plan, and PlanDigest is what binds the report to it.
type SchemaPlanReport struct {
	ContractVersion int            `json:"contract_version"`
	Command         string         `json:"command"`
	Outcome         string         `json:"outcome"`
	PlanDigest      string         `json:"plan_digest,omitempty"`
	PlanPath        string         `json:"plan_path,omitempty"`
	Refusal         *SchemaRefusal `json:"refusal,omitempty"`
	// Error is why the run failed or refused. Its message is the sentence
	// Ptah printed on standard error and may quote a statement, so it is for a
	// fence refusal's tables and nothing else; its kind names a target that
	// could not be opened.
	Error *CommandError `json:"error,omitempty"`
	// Undecided names the declared objects the plan's comparison could not
	// decide, and so planned nothing for. A report that plans no change and
	// names one has not shown the database matches.
	Undecided Undecided `json:"undecided,omitzero"`
}

// Undecided is what a comparison could not decide, as Ptah's drift and plan
// documents list it under "undecided" (stokaro/ptah#3850): the declared
// objects of a kind the read of the database did not describe, so nothing
// checked whether they exist. Ptah counts them as the drift finding
// DriftFindingUndecided.
type Undecided struct {
	// Common are objects of a kind every engine shares, such as a view.
	Common []UndecidedObject `json:"common,omitempty"`
	// Features are objects a feature owner compares, such as a PostgreSQL
	// publication.
	Features []UndecidedFeature `json:"features,omitempty"`
}

// UndecidedObject is one common object the read did not describe. Reason is
// a token of Ptah's coverage vocabulary, such as not-inspected, and may be
// empty.
type UndecidedObject struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Reason string `json:"reason,omitempty"`
}

// UndecidedFeature is one object a feature owner could not compare. Reason is
// Ptah's sentence for why, and Label the owner's name for the kind, such as
// "PostgreSQL publication", where it states one.
type UndecidedFeature struct {
	Kind    string           `json:"kind"`
	Subject UndecidedSubject `json:"subject"`
	Reason  string           `json:"reason"`
	Label   string           `json:"label,omitempty"`
}

// UndecidedSubject is the part of an object identity a name is spelled from.
// A subject with none of them stands for every object of its kind.
type UndecidedSubject struct {
	Catalog IdentityPart `json:"catalog,omitzero"`
	Schema  IdentityPart `json:"schema,omitzero"`
	Parent  IdentityPart `json:"parent,omitzero"`
	Name    IdentityPart `json:"name,omitzero"`
}

// IdentityPart is one component of an object identity, as it was spelled.
type IdentityPart struct {
	Source string `json:"source"`
}

// Empty reports whether the comparison decided every declared object.
func (u Undecided) Empty() bool { return len(u.Common) == 0 && len(u.Features) == 0 }

// Count is how many declared objects the comparison could not decide, which
// is the count Ptah gives the undecided finding.
func (u Undecided) Count() int { return len(u.Common) + len(u.Features) }

// validate refuses an entry that does not say what it is: a common object with
// no kind, or a feature object with no kind or no reason.
func (u Undecided) validate() error {
	for _, object := range u.Common {
		if strings.TrimSpace(object.Kind) == "" {
			return errors.New("names an undecided object of no kind")
		}
	}
	for _, feature := range u.Features {
		if strings.TrimSpace(feature.Kind) == "" || strings.TrimSpace(feature.Reason) == "" {
			return errors.New("names an undecided object without its kind or its reason")
		}
	}
	return nil
}

// SchemaApplyReport is Ptah's `schema apply --json` document.
type SchemaApplyReport struct {
	ContractVersion int               `json:"contract_version"`
	Command         string            `json:"command"`
	Outcome         string            `json:"outcome"`
	PlanName        string            `json:"plan_name,omitempty"`
	PlanDigest      string            `json:"plan_digest,omitempty"`
	Units           []SchemaApplyUnit `json:"units,omitempty"`
	Refusal         *SchemaRefusal    `json:"refusal,omitempty"`
	// Error is why the run failed, refused or ended unknown. Only its kind is
	// read.
	Error *CommandError `json:"error,omitempty"`
	// Failure is where a run that sent a statement stopped, and the code the
	// database answered with. It carries no text, so it is what may leave the
	// Pod. A build before stokaro/ptah#5562 writes none.
	Failure *SchemaApplyFailure `json:"failure,omitempty"`
	// ApplyLog is what the run's writes to ptah_schema_apply_log came to, read
	// for the run reference the entries carry. Its lost entries are not read:
	// a lost entry changes nothing about the outcome.
	ApplyLog *RunLogReference `json:"apply_log,omitempty"`
}

// SchemaApplyFailure is the unit or the commit a schema apply stopped at, as
// Ptah's apply document names it (docs/exit_codes.md, "Where a Run Stopped").
type SchemaApplyFailure struct {
	// Statement is the position of the failed unit among the units the
	// document lists, counting from one. It is zero when no unit is to blame:
	// the commit after them failed, or the run confirmed every unit and still
	// did not succeed.
	Statement int `json:"statement"`
	// Statements is how many units the document lists.
	Statements int `json:"statements"`
	// Commit is true when every unit ran and the commit that ends them failed.
	Commit bool `json:"commit,omitempty"`
	// SQLState is empty when the server gave no answer, and ErrorNumber is
	// zero on every protocol but MySQL's.
	SQLState    string `json:"sqlstate,omitempty"`
	ErrorNumber int    `json:"error_number,omitempty"`
}

// Validate holds a failure to the forms Ptah's contract gives it. Nothing here
// quotes the SQLSTATE back, because a value that failed its form could be
// anything.
func (f SchemaApplyFailure) Validate() error {
	switch {
	case f.Statements < 1 || f.Statements > maxSchemaApplyStatements:
		return fmt.Errorf("schema apply reports statement %d of %d failed", f.Statement, f.Statements)
	case f.Commit && f.Statement != 0:
		// The commit runs after every statement, so a document that blames
		// one of them as well says two things about the same failure.
		return fmt.Errorf("schema apply reports statement %d of %d and the commit after them failed", f.Statement, f.Statements)
	case f.Statement < 0 || f.Statement > f.Statements:
		return fmt.Errorf("schema apply reports statement %d of %d failed", f.Statement, f.Statements)
	case f.SQLState != "" && !sqlStatePattern.MatchString(f.SQLState):
		return fmt.Errorf("schema apply reports a SQLSTATE that is not five digits or capital letters")
	case f.ErrorNumber < 0 || f.ErrorNumber > math.MaxUint16:
		return fmt.Errorf("schema apply reports error number %d, which the two bytes MySQL sends cannot hold", f.ErrorNumber)
	}
	return nil
}

// Failed reports whether the failure names the statement the run stopped at.
// A failed commit, and a run that stopped with every unit confirmed, name
// none: Validate holds both to statement zero.
func (f SchemaApplyFailure) Failed() bool {
	return f.Statement >= 1
}

// maxSchemaApplyStatements bounds the unit count a failure may report, so the
// position and the count fit the int32 the status keeps them in.
const maxSchemaApplyStatements = math.MaxInt32

// SchemaApplyUnit is one planned operation an apply document lists: its SQL,
// or a summary of a native provider operation that has none.
type SchemaApplyUnit struct {
	SQL    string          `json:"sql,omitempty"`
	Native json.RawMessage `json:"native,omitempty"`
}

// ErrNativeApplyUnit is a listed operation that is not SQL. This operator
// plans and reviews SQL statements, so it cannot account for one.
var ErrNativeApplyUnit = errors.New("schema apply lists an operation that is not SQL")

// SQLStatements is the SQL of each listed unit, in order. A unit that is a
// native operation, or that has no SQL, is refused rather than skipped.
func (r SchemaApplyReport) SQLStatements() ([]string, error) {
	statements := make([]string, 0, len(r.Units))
	for _, unit := range r.Units {
		if len(unit.Native) != 0 || unit.SQL == "" {
			return nil, ErrNativeApplyUnit
		}
		statements = append(statements, unit.SQL)
	}
	return statements, nil
}

// DecodeSchemaPlan reads one plan document and refuses anything it cannot
// account for: another contract version, an outcome it does not know, a
// second JSON value, and an outcome whose fields contradict it.
//
// Since Ptah v0.30.0 a no-changes report may name a saved plan: one of notes
// alone, which say what differs and why Ptah did not plan it
// (stokaro/ptah#6165). It names the plan's digest and path together, as a
// plan with changes does, and DecodeNotePlan reads the plan.
//
// Fields it does not declare are ignored, because Ptah adds fields without
// raising the version.
func DecodeSchemaPlan(data []byte) (SchemaPlanReport, error) {
	var report SchemaPlanReport
	if err := decodeJSON(data, &report, false); err != nil {
		return SchemaPlanReport{}, fmt.Errorf("decode schema plan report: %w", err)
	}
	if report.ContractVersion != SupportedSchemaPlanContract {
		return SchemaPlanReport{}, fmt.Errorf(
			"schema plan contract version %d is not the %d this operator reads",
			report.ContractVersion, SupportedSchemaPlanContract,
		)
	}
	if report.Command != SchemaPlanCommand {
		return SchemaPlanReport{}, fmt.Errorf("schema plan report names another command than %s", SchemaPlanCommand)
	}
	if !slices.Contains(schemaPlanOutcomes, report.Outcome) {
		return SchemaPlanReport{}, fmt.Errorf("schema plan reports outcome %q, which this operator does not know", report.Outcome)
	}
	if err := validateSchemaRefusal(report.Outcome == SchemaPlanOutcomeRefused, report.Refusal); err != nil {
		return SchemaPlanReport{}, fmt.Errorf("schema plan %w", err)
	}
	if err := report.Undecided.validate(); err != nil {
		return SchemaPlanReport{}, fmt.Errorf("schema plan report %w", err)
	}
	switch report.Outcome {
	case SchemaPlanOutcomeChanges:
		if !validDigest(report.PlanDigest) || report.PlanPath == "" {
			return SchemaPlanReport{}, fmt.Errorf("schema plan reports changes without the digest and path of the plan it saved")
		}
	case SchemaPlanOutcomeNoChanges:
		if (report.PlanDigest != "" || report.PlanPath != "") && (!validDigest(report.PlanDigest) || report.PlanPath == "") {
			return SchemaPlanReport{}, fmt.Errorf("schema plan reports no changes and names a saved plan without its digest and path")
		}
	default:
		if report.PlanDigest != "" || report.PlanPath != "" {
			return SchemaPlanReport{}, fmt.Errorf("schema plan reports %s and names a saved plan", report.Outcome)
		}
	}
	return report, nil
}

// DecodeSchemaApply reads one apply document under the rules DecodeSchemaPlan
// states.
func DecodeSchemaApply(data []byte) (SchemaApplyReport, error) {
	var report SchemaApplyReport
	if err := decodeJSON(data, &report, false); err != nil {
		return SchemaApplyReport{}, fmt.Errorf("decode schema apply report: %w", err)
	}
	if report.ContractVersion != SupportedSchemaApplyContract {
		return SchemaApplyReport{}, fmt.Errorf(
			"schema apply contract version %d is not the %d this operator reads",
			report.ContractVersion, SupportedSchemaApplyContract,
		)
	}
	if report.Command != SchemaApplyCommand {
		return SchemaApplyReport{}, fmt.Errorf("schema apply report names another command than %s", SchemaApplyCommand)
	}
	if !slices.Contains(schemaApplyOutcomes, report.Outcome) {
		return SchemaApplyReport{}, fmt.Errorf("schema apply reports outcome %q, which this operator does not know", report.Outcome)
	}
	if err := validateSchemaRefusal(report.Outcome == SchemaApplyOutcomeRefused, report.Refusal); err != nil {
		return SchemaApplyReport{}, fmt.Errorf("schema apply %w", err)
	}
	if report.PlanDigest != "" && !validDigest(report.PlanDigest) {
		return SchemaApplyReport{}, fmt.Errorf("schema apply names a plan digest that is not an exact SHA-256 digest")
	}
	if failure := report.Failure; failure != nil {
		// Only a run that sent a statement and returned an error says where it
		// stopped: unknown, or failed after a rollback the database confirmed.
		if report.Outcome != SchemaApplyOutcomeUnknown && report.Outcome != SchemaApplyOutcomeFailed {
			return SchemaApplyReport{}, fmt.Errorf("schema apply reports outcome %q and a failed statement", report.Outcome)
		}
		if err := failure.Validate(); err != nil {
			return SchemaApplyReport{}, err
		}
	}
	return report, nil
}

// validateSchemaRefusal holds a refusal to its outcome: present exactly when
// the outcome is refused, and carrying a code.
func validateSchemaRefusal(refused bool, refusal *SchemaRefusal) error {
	switch {
	case refused && refusal == nil:
		return fmt.Errorf("reports a refusal without its reason")
	case !refused && refusal != nil:
		return fmt.Errorf("carries a refusal on an outcome that is not refused")
	case refused && !schemaRefusalCodePattern.MatchString(refusal.Code):
		return fmt.Errorf("reports a refusal whose code is not an identifier")
	default:
		return nil
	}
}
