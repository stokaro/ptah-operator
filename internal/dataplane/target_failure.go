package dataplane

import (
	"bytes"
	"fmt"
)

// TargetFailureKind names why a Ptah command could not open its connection to
// the target database. It is the closed vocabulary Ptah writes in the kind of
// a document's error object (stokaro/ptah#5173), and the only part of that
// object the operator reads.
//
// The message beside it is the sentence Ptah wrote for a person, and it can
// quote what the database answered: a role, a host, a database name, a row.
// Nothing here returns it, and the runner and the controller describe a kind in
// sentences of their own.
type TargetFailureKind string

// The vocabulary. Ptah classifies only the failure to open the target
// connection, from the driver's typed errors, and leaves every other failure
// without a kind.
const (
	// TargetDatabaseUnreachable is a server that never answered: the host did
	// not resolve, the address refused the connection, or the connection
	// timed out before the server completed it.
	TargetDatabaseUnreachable TargetFailureKind = "database_unreachable"
	// TargetAuthenticationFailed is a server that did not accept the
	// connection's credentials.
	TargetAuthenticationFailed TargetFailureKind = "authentication_failed"
	// TargetDatabaseNotFound is a server that has no database by the name
	// the connection gives.
	TargetDatabaseNotFound TargetFailureKind = "database_not_found"
	// TargetPermissionDenied is a server that accepted the credentials and
	// refused the role access to the database.
	TargetPermissionDenied TargetFailureKind = "permission_denied"
	// TargetTLSFailed is a TLS handshake that did not complete.
	TargetTLSFailed TargetFailureKind = "tls_failed"
)

// targetFailureDescriptions is what the runner says of each kind in its
// result frame. Each is a fixed sentence: it states what Ptah classified and
// repeats nothing Ptah or the database wrote.
var targetFailureDescriptions = map[TargetFailureKind]string{
	TargetDatabaseUnreachable:  "the target database did not answer: its host did not resolve, refused the connection, or did not respond in time",
	TargetAuthenticationFailed: "the target database refused the credentials",
	TargetDatabaseNotFound:     "the target database server has no database by the name the URL gives",
	TargetPermissionDenied:     "the target database server accepted the credentials and refused the user access to the database",
	TargetTLSFailed:            "the TLS handshake with the target database failed",
}

// TargetFailureKinds returns the vocabulary in a fixed order.
func TargetFailureKinds() []TargetFailureKind {
	return []TargetFailureKind{
		TargetDatabaseUnreachable,
		TargetAuthenticationFailed,
		TargetDatabaseNotFound,
		TargetPermissionDenied,
		TargetTLSFailed,
	}
}

// ParseTargetFailureKind returns the kind a document named, when it is one of
// the vocabulary. Anything else is no kind: Ptah may add a kind without
// raising a contract version, and a kind this operator has no sentence for
// is read as the failure having none, never repeated.
func ParseTargetFailureKind(kind string) (TargetFailureKind, bool) {
	parsed := TargetFailureKind(kind)
	if _, known := targetFailureDescriptions[parsed]; !known {
		return "", false
	}
	return parsed, true
}

// Description is the runner's sentence for the kind, and empty for a value
// outside the vocabulary.
func (k TargetFailureKind) Description() string { return targetFailureDescriptions[k] }

// CommandError is the error object a Ptah document carries for a run that
// failed.
type CommandError struct {
	// Kind is from the vocabulary above, or a kind a later Ptah added, or
	// absent when the failure has none.
	Kind string `json:"kind,omitempty"`
	// Message is Ptah's sentence for a person. It can quote what the database
	// said, so nothing but a protected-table refusal's tables is copied from
	// it. SchemaStructuralProblem compares it with words from a run that had no
	// database, and copies none of it.
	Message string `json:"message"`
}

// TargetFailure is the kind of the error, when it is one of the vocabulary. A
// nil error has none.
func (e *CommandError) TargetFailure() (TargetFailureKind, bool) {
	if e == nil {
		return "", false
	}
	return ParseTargetFailureKind(e.Kind)
}

// commandFailure is the document a command prints on standard output in place
// of its report when it fails before it has one: the contract version, the
// command, and the error.
type commandFailure struct {
	ContractVersion int           `json:"contract_version"`
	Command         string        `json:"command"`
	Error           *CommandError `json:"error"`
}

// decodeCommandFailure reads the failure document a command prints in place
// of its report, held to the command's contract version and name.
func decodeCommandFailure(data []byte, contractVersion int, command string) (*CommandError, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("%s printed no failure document", command)
	}
	var failure commandFailure
	if err := decodeJSON(data, &failure, false); err != nil {
		return nil, fmt.Errorf("decode %s failure document: %w", command, err)
	}
	if failure.ContractVersion != contractVersion {
		return nil, fmt.Errorf("%s failure document contract version %d is not the %d this operator reads",
			command, failure.ContractVersion, contractVersion)
	}
	if failure.Command != command {
		return nil, fmt.Errorf("a %s failure document names another command", command)
	}
	if failure.Error == nil {
		return nil, fmt.Errorf("%s failure document carries no error", command)
	}
	return failure.Error, nil
}

// DecodeDriftFailure reads the document `schema drift --format json` prints
// on standard output when it fails: its contract version, its command and the
// error, in place of the report.
func DecodeDriftFailure(data []byte) (*CommandError, error) {
	return decodeCommandFailure(data, SupportedDriftContract, DriftCommand)
}

// DecodeMigrationStatusFailure reads the document `migrations status --json`
// prints on standard output when it cannot read the history.
func DecodeMigrationStatusFailure(data []byte) (*CommandError, error) {
	return decodeCommandFailure(data, SupportedMigrationStatusContract, MigrationStatusCommand)
}
