package dataplane_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// TestParseTargetFailureKindReadsOnlyTheVocabulary holds the parser to the five
// kinds and refuses everything else a child could write in their place. An
// unknown kind is not an error: it is no kind, and nothing repeats it.
func TestParseTargetFailureKindReadsOnlyTheVocabulary(t *testing.T) {
	t.Parallel()

	for _, kind := range dataplane.TargetFailureKinds() {
		parsed, known := dataplane.ParseTargetFailureKind(string(kind))
		if !known || parsed != kind {
			t.Errorf("ParseTargetFailureKind(%q) = %q, %v", kind, parsed, known)
		}
		if strings.TrimSpace(kind.Description()) == "" {
			t.Errorf("%s has no description", kind)
		}
	}
	for _, refused := range []string{
		"",
		"Database_Unreachable",
		" database_unreachable",
		"database_unreachable ",
		"database_unreachable\n",
		"database-unreachable",
		"connection_refused",
		"lock_timeout",
		"database_unreachable: password authentication failed for user \"app\"",
		"authentication_failed\x00",
	} {
		if parsed, known := dataplane.ParseTargetFailureKind(refused); known || parsed != "" {
			t.Errorf("ParseTargetFailureKind(%q) = %q, %v, want no kind", refused, parsed, known)
		}
		if description := dataplane.TargetFailureKind(refused).Description(); description != "" {
			t.Errorf("Description() of %q = %q, want none", refused, description)
		}
	}
	if kinds := dataplane.TargetFailureKinds(); len(kinds) != 5 {
		t.Fatalf("the vocabulary has %d kinds, want the five stokaro/ptah#5173 declares", len(kinds))
	}
}

// targetFailureCases is what each directory under testdata/target-failures
// was captured against, and the kind Ptah named for it. The directories are
// the documents a build of stokaro/ptah 71c5b28a (#5173) printed on standard
// output against PostgreSQL 17 and MySQL 8.4, stored as printed: a host that
// does not resolve, a port nothing listens on, a wrong password, a role the
// server does not know, a database it does not hold, a role refused CONNECT
// (PostgreSQL) or a user with no privilege on the database (MySQL), and a
// certificate that does not verify.
var targetFailureCases = map[string]dataplane.TargetFailureKind{
	"pg-unresolvable": dataplane.TargetDatabaseUnreachable,
	"pg-refused":      dataplane.TargetDatabaseUnreachable,
	"pg-password":     dataplane.TargetAuthenticationFailed,
	"pg-role":         dataplane.TargetAuthenticationFailed,
	"pg-database":     dataplane.TargetDatabaseNotFound,
	"pg-connect":      dataplane.TargetPermissionDenied,
	"pg-tls":          dataplane.TargetTLSFailed,
	"my-unresolvable": dataplane.TargetDatabaseUnreachable,
	"my-refused":      dataplane.TargetDatabaseUnreachable,
	"my-password":     dataplane.TargetAuthenticationFailed,
	"my-database":     dataplane.TargetDatabaseNotFound,
	"my-connect":      dataplane.TargetPermissionDenied,
	"my-tls":          dataplane.TargetTLSFailed,
}

// targetFailureCommands are the commands the runner runs against a target,
// by the name of the file each one's document was stored under.
var targetFailureCommands = []string{"drift", "plan", "apply-dry-run", "apply", "status", "up"}

// targetFailureKindOf decodes one captured document with the decoder the
// runner uses for its command, and returns the kind it reads.
func targetFailureKindOf(t *testing.T, command string, document []byte) (dataplane.TargetFailureKind, bool) {
	t.Helper()
	switch command {
	case "drift":
		// A failure document carries none of a report's fields, so at exit 0
		// it would read as a converged database with no drift.
		for _, exit := range []int{0, 1, 2} {
			if _, err := dataplane.DecodeDrift(document, exit); err == nil {
				t.Fatalf("DecodeDrift() read a failure document as a report at exit %d", exit)
			}
		}
		failure, err := dataplane.DecodeDriftFailure(document)
		if err != nil {
			t.Fatalf("DecodeDriftFailure() error = %v", err)
		}
		return failure.TargetFailure()
	case "plan":
		report, err := dataplane.DecodeSchemaPlan(document)
		if err != nil || report.Outcome != dataplane.SchemaPlanOutcomeFailed {
			t.Fatalf("DecodeSchemaPlan() = outcome %q, %v", report.Outcome, err)
		}
		return report.Error.TargetFailure()
	case "apply-dry-run", "apply":
		report, err := dataplane.DecodeSchemaApply(document)
		if err != nil || report.Outcome != dataplane.SchemaApplyOutcomeFailed {
			t.Fatalf("DecodeSchemaApply() = outcome %q, %v", report.Outcome, err)
		}
		return report.Error.TargetFailure()
	case "status":
		if report, err := dataplane.DecodeMigrationStatus(document); err == nil {
			t.Fatalf("DecodeMigrationStatus() read a failure document as the history %+v", report)
		}
		failure, err := dataplane.DecodeMigrationStatusFailure(document)
		if err != nil {
			t.Fatalf("DecodeMigrationStatusFailure() error = %v", err)
		}
		return failure.TargetFailure()
	case "up":
		report, err := dataplane.DecodeMigrationRun(document)
		if err != nil || !report.StoppedBeforeSelecting() {
			t.Fatalf("DecodeMigrationRun() = %+v, %v, want a run that stopped before selecting", report, err)
		}
		return report.Error.TargetFailure()
	default:
		t.Fatalf("no decoder for %s", command)
		return "", false
	}
}

// TestCapturedTargetFailuresNameTheirKind reads every document Ptah printed
// for a target it could not open, through the decoder the runner uses for
// that command, and holds each to the kind of the fault it was captured
// against. It counts what it read, so a directory that went missing fails it
// rather than passing on nothing.
func TestCapturedTargetFailuresNameTheirKind(t *testing.T) {
	t.Parallel()

	read := 0
	seen := map[dataplane.TargetFailureKind]bool{}
	for name, want := range targetFailureCases {
		for _, command := range targetFailureCommands {
			document, err := os.ReadFile(filepath.Join("testdata", "target-failures", name, command+".json"))
			if err != nil {
				t.Fatal(err)
			}
			kind, known := targetFailureKindOf(t, command, document)
			if !known || kind != want {
				t.Errorf("%s %s: kind %q, %v, want %q", name, command, kind, known, want)
			}
			seen[kind] = true
			read++
		}
	}
	entries, err := filepath.Glob(filepath.Join("testdata", "target-failures", "*", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if want := len(targetFailureCases) * len(targetFailureCommands); read != want || len(entries) != want {
		t.Fatalf("read %d documents of %d stored, want %d", read, len(entries), want)
	}
	for _, kind := range dataplane.TargetFailureKinds() {
		if !seen[kind] {
			t.Errorf("no captured document names %s", kind)
		}
	}
}

// TestFailureDocumentsAreReadOnlyWhenTheyAreWhatTheySay holds the failure
// document decoders to the contract version and command they read, and to an
// error object being present, with one accepted document as the control.
func TestFailureDocumentsAreReadOnlyWhenTheyAreWhatTheySay(t *testing.T) {
	t.Parallel()

	const (
		drift  = `{"contract_version":1,"command":"schema drift","error":{"kind":"tls_failed","message":"x509"}}`
		status = `{"contract_version":2,"command":"migrations status","error":{"kind":"tls_failed","message":"x509"}}`
	)
	decoders := map[string]struct {
		decode  func([]byte) (*dataplane.CommandError, error)
		control string
	}{
		"drift":  {dataplane.DecodeDriftFailure, drift},
		"status": {dataplane.DecodeMigrationStatusFailure, status},
	}
	for name, decoder := range decoders {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			failure, err := decoder.decode([]byte(decoder.control))
			if kind, known := failure.TargetFailure(); err != nil || !known || kind != dataplane.TargetTLSFailed {
				t.Fatalf("decode(control) = %+v, %v", failure, err)
			}
			other := drift
			if name == "drift" {
				other = status
			}
			for row, document := range map[string]string{
				"the other command's document": other,
				"another contract version":     strings.Replace(decoder.control, `"contract_version":`, `"contract_version":9`, 1),
				"no command":                   strings.Replace(decoder.control, `"command":`, `"commandx":`, 1),
				"no error":                     strings.Replace(decoder.control, `"error":`, `"errorx":`, 1),
				"a null error":                 decoder.control[:strings.Index(decoder.control, `"error":`)] + `"error":null}`,
				"an error that is a string":    decoder.control[:strings.Index(decoder.control, `"error":`)] + `"error":"x509"}`,
				"nothing at all":               "",
				"a human line":                 "error: error connecting to database\n",
				"a second document":            decoder.control + decoder.control,
			} {
				if failure, err := decoder.decode([]byte(document)); err == nil {
					t.Errorf("decode(%s) = %+v, want it refused", row, failure)
				}
			}
		})
	}
}

// TestAnUnknownKindIsReadAsNone holds a document whose kind is not one of the
// vocabulary to no kind, on every document that carries one, where a reader
// that trusted the field would repeat whatever the child wrote there.
func TestAnUnknownKindIsReadAsNone(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"lock_timeout", "database_unreachable: host db.internal", "DATABASE_UNREACHABLE"} {
		quoted := `"kind":"` + kind + `","message":"m"`
		documents := map[string]func() (*dataplane.CommandError, error){
			"drift": func() (*dataplane.CommandError, error) {
				return dataplane.DecodeDriftFailure([]byte(`{"contract_version":1,"command":"schema drift","error":{` + quoted + `}}`))
			},
			"status": func() (*dataplane.CommandError, error) {
				return dataplane.DecodeMigrationStatusFailure([]byte(`{"contract_version":2,"command":"migrations status","error":{` + quoted + `}}`))
			},
			"plan": func() (*dataplane.CommandError, error) {
				report, err := dataplane.DecodeSchemaPlan([]byte(`{"contract_version":3,"command":"schema plan","outcome":"failed","error":{` + quoted + `}}`))
				return report.Error, err
			},
			"apply": func() (*dataplane.CommandError, error) {
				report, err := dataplane.DecodeSchemaApply([]byte(`{"contract_version":4,"command":"schema apply","outcome":"failed","error":{` + quoted + `}}`))
				return report.Error, err
			},
			"up": func() (*dataplane.CommandError, error) {
				report, err := dataplane.DecodeMigrationRun([]byte(`{"contract_version":2,"command":"migrations up","direction":"up","outcome":"failed","error":{` + quoted + `}}`))
				return report.Error, err
			},
		}
		for name, decode := range documents {
			failure, err := decode()
			if err != nil || failure == nil || failure.Kind != kind {
				t.Fatalf("%s with kind %q: %+v, %v", name, kind, failure, err)
			}
			if parsed, known := failure.TargetFailure(); known || parsed != "" {
				t.Errorf("%s: kind %q read as %q", name, kind, parsed)
			}
		}
	}
	var none *dataplane.CommandError
	if kind, known := none.TargetFailure(); known || kind != "" {
		t.Fatalf("a report with no error names %q", kind)
	}
}

// TestTheVocabularyIsPtahs holds the kinds to the names stokaro/ptah#5173
// declares, in its order, so a renamed constant is caught here rather than on
// a cluster where no kind would ever match.
func TestTheVocabularyIsPtahs(t *testing.T) {
	t.Parallel()

	want := []string{"database_unreachable", "authentication_failed", "database_not_found", "permission_denied", "tls_failed"}
	got := make([]string, 0, len(want))
	for _, kind := range dataplane.TargetFailureKinds() {
		got = append(got, string(kind))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("TargetFailureKinds() = %q, want %q", got, want)
	}
}

// TestDecodeDriftReadsOnlyItsOwnContract holds a drift report to the contract
// version and command it reads, against a converged report it accepts. The
// report Ptah v0.17.0 printed had neither field.
func TestDecodeDriftReadsOnlyItsOwnContract(t *testing.T) {
	t.Parallel()

	const converged = `{"contract_version":1,"command":"schema drift","drift":false,"failed":false,` +
		`"failure_threshold":"all","highest_severity":"","dialect":"postgres","diff":{}}`
	if _, err := dataplane.DecodeDrift([]byte(converged), 0); err != nil {
		t.Fatalf("DecodeDrift() refused a converged report: %v", err)
	}
	for name, document := range map[string]string{
		"the report Ptah v0.17.0 printed": strings.Replace(converged, `"contract_version":1,"command":"schema drift",`, ``, 1),
		"no contract version":             strings.Replace(converged, `"contract_version":1,`, ``, 1),
		"a newer contract version":        strings.Replace(converged, `"contract_version":1`, `"contract_version":2`, 1),
		"no command":                      strings.Replace(converged, `"command":"schema drift",`, ``, 1),
		"another command's document":      strings.Replace(converged, `"schema drift"`, `"migrations status"`, 1),
	} {
		if report, err := dataplane.DecodeDrift([]byte(document), 0); err == nil {
			t.Errorf("DecodeDrift() accepted %s as %+v", name, report)
		}
	}
}
