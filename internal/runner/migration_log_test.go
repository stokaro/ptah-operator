package runner

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// refusedActor is a value a database refused for the log's actor column, as
// Ptah's message about the refusal quotes it.
const refusedActor = "oidc:a-name-the-column-cannot-hold"

// migrationRunWithLostEntries is an applied run of version 3 whose two log
// entries the database refused, as Ptah v0.26.0 reports them, message included.
func migrationRunWithLostEntries() string {
	loss := func(state string) string {
		return `{"operation":"up","version":3,"version_key":"3","state":"` + state + `","sqlstate":"22001","error_number":1406,` +
			`"message":"Error 1406 (22001): Data too long for column 'actor' at row 1: ` + refusedActor + `"}`
	}
	return `{"contract_version":2,"command":"migrations up","direction":"up","outcome":"applied","planned":[3],"applied":[3],` +
		`"migration_log":{"written":0,"lost":[` + loss("started") + `,` + loss("applied") + `]},` +
		`"status":{"contract_version":2,"current_version":3,"total_migrations":3,"has_pending_changes":false}}`
}

// runLostEntries runs one migration Apply whose child wrote document.
func runLostEntries(t *testing.T, document string) Result {
	t.Helper()
	executor := &scriptedExecutor{t: t, responses: []scriptedResponse{{stdout: document}}}
	return Run(context.Background(), Config{
		Operation:   OperationMigrationApply,
		Environment: withRunnerProtocol(migrationApplyEnvironment(t, "migration-log-lost")),
		Executor:    executor,
	})
}

// The frame carries which entries the log lacks and the code the database
// answered with, and none of Ptah's sentence, which quotes the value refused.
// The outcome stays the one the revision table gave.
func TestMigrationRunCarriesLostLogEntriesWithoutTheirMessage(t *testing.T) {
	t.Parallel()

	result := runLostEntries(t, migrationRunWithLostEntries())
	if result.Error != nil || result.MigrationRun == nil {
		t.Fatalf("Run() = %#v, want the applied run the child reported", result)
	}
	if result.MigrationRun.Outcome != dataplane.MigrationOutcomeApplied || result.Uncertain {
		t.Fatalf("Run() outcome %q uncertain %t, want applied and certain", result.MigrationRun.Outcome, result.Uncertain)
	}
	frame, err := MarshalFrame(result)
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	if bytes.Contains(frame, []byte(refusedActor)) || bytes.Contains(frame, []byte("Data too long")) {
		t.Fatalf("the frame carries the database's message: %s", frame)
	}
	parsed, err := ParseResultFor(frame, OperationMigrationApply, result.OperationID)
	if err != nil {
		t.Fatalf("ParseResultFor() error = %v", err)
	}
	loss := func(state string) dataplane.MigrationLogLoss {
		return dataplane.MigrationLogLoss{Operation: "up", Version: 3, VersionKey: "3", State: state, SQLState: "22001", ErrorNumber: 1406}
	}
	want := &dataplane.MigrationLogReport{Lost: []dataplane.MigrationLogLoss{loss("started"), loss("applied")}}
	if parsed.MigrationRun == nil || !reflect.DeepEqual(parsed.MigrationRun.MigrationLog, want) {
		t.Fatalf("the manager parsed migration run %#v, want log %+v", parsed.MigrationRun, want)
	}
}

// A log report the run cannot have written stops the document from being read
// at all, as any other part of the run that contradicts it does.
func TestMigrationRunRefusesALogItsRunCannotHaveWritten(t *testing.T) {
	t.Parallel()

	document := `{"contract_version":2,"command":"migrations up","direction":"up","outcome":"applied","planned":[3],"applied":[3],` +
		`"migration_log":{"written":1,"lost":[{"operation":"up","version":4,"version_key":"4","state":"started","message":"m"}]},` +
		`"status":{"contract_version":2,"current_version":3,"total_migrations":3,"has_pending_changes":false}}`
	result := runLostEntries(t, document)
	if result.MigrationRun != nil || result.Error == nil || result.Error.Code != "invalid_migration_run_output" {
		t.Fatalf("Run() = %#v, want invalid_migration_run_output and no report", result)
	}
}

// A log report is held to the run it came in and carries no message, on the
// side that writes the frame and on the side that reads it.
func TestFrameRefusesAMigrationLogItCannotCarry(t *testing.T) {
	t.Parallel()

	base := runLostEntries(t, migrationRunWithLostEntries())
	if base.MigrationRun == nil || base.MigrationRun.MigrationLog.LostCount() != 2 {
		t.Fatalf("the base result carries no lost entries: %#v", base.MigrationRun)
	}
	// The base, as it is, crosses: each refusal below is the mutation alone.
	if _, err := MarshalFrame(base); err != nil {
		t.Fatalf("MarshalFrame(base) error = %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*dataplane.MigrationLogReport)
	}{
		{name: "a message on the second entry", mutate: func(log *dataplane.MigrationLogReport) { log.Lost[1].Message = refusedActor }},
		{name: "a message on the first entry", mutate: func(log *dataplane.MigrationLogReport) { log.Lost[0].Message = "x" }},
		{name: "a version the run did not select", mutate: func(log *dataplane.MigrationLogReport) { log.Lost[0].Version = 4 }},
		{name: "a SQLSTATE that quotes a value", mutate: func(log *dataplane.MigrationLogReport) { log.Lost[0].SQLState = refusedActor }},
		{name: "an error number past two bytes", mutate: func(log *dataplane.MigrationLogReport) { log.Lost[0].ErrorNumber = 65536 }},
		{name: "a state no up run writes", mutate: func(log *dataplane.MigrationLogReport) { log.Lost[0].State = "rolled_back" }},
		{name: "more entries than the run writes", mutate: func(log *dataplane.MigrationLogReport) { log.Written = 1 }},
		{name: "a negative count written", mutate: func(log *dataplane.MigrationLogReport) { log.Written = -1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			candidate := base
			run := *base.MigrationRun
			log := *run.MigrationLog
			log.Lost = append([]dataplane.MigrationLogLoss(nil), run.MigrationLog.Lost...)
			run.MigrationLog = &log
			candidate.MigrationRun = &run
			test.mutate(&log)
			if _, err := MarshalFrame(candidate); !errors.Is(err, ErrMalformedFrame) {
				t.Fatalf("MarshalFrame() error = %v, want ErrMalformedFrame", err)
			}
			frame := handcraftedIntegrityValidFrame(t, candidate)
			if _, err := ParseResultFor(frame, candidate.Operation, candidate.OperationID); !errors.Is(err, ErrMalformedFrame) {
				t.Fatalf("ParseResultFor() error = %v, want ErrMalformedFrame", err)
			}
		})
	}
}
