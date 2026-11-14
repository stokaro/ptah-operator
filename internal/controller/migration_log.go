package controller

import (
	"fmt"
	"math"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// maxMigrationLogLosses is how many lost entries status keeps. The API holds
// the list to it, and the count beside it says how many there were in all.
const maxMigrationLogLosses = 16

// migrationLogStatus is what a run reported of its writes to
// schema_migrations_log, in the terms status keeps. It is nil when the report
// carries none, and when an entry names a state status has no word for, which
// neither the runner nor the frame admits: a record nobody established stays
// absent rather than guessed.
func migrationLogStatus(report *dataplane.MigrationLogReport) *operatorv1alpha1.MigrationLogStatus {
	if report == nil {
		return nil
	}
	log := &operatorv1alpha1.MigrationLogStatus{
		Written: boundedCount(report.Written),
		Lost:    boundedCount(len(report.Lost)),
	}
	for _, loss := range report.Lost[:min(len(report.Lost), maxMigrationLogLosses)] {
		state, known := migrationLogEntryState(loss.State)
		if !known {
			return nil
		}
		log.Losses = append(log.Losses, operatorv1alpha1.MigrationLogLossStatus{
			Version:     loss.Version,
			State:       state,
			SQLState:    loss.SQLState,
			ErrorNumber: int32(loss.ErrorNumber), // The runner admits at most 65535.
		})
	}
	return log
}

func migrationLogEntryState(state string) (operatorv1alpha1.MigrationLogEntryState, bool) {
	switch state {
	case dataplane.MigrationLogStarted:
		return operatorv1alpha1.MigrationLogEntryStarted, true
	case dataplane.MigrationLogApplied:
		return operatorv1alpha1.MigrationLogEntryApplied, true
	case dataplane.MigrationLogFailed:
		return operatorv1alpha1.MigrationLogEntryFailed, true
	default:
		return "", false
	}
}

// boundedCount is a count as status holds it. A run writes two entries for
// each migration it selected, so no real count comes near the bound.
func boundedCount(count int) int32 {
	return int32(min(max(count, 0), math.MaxInt32))
}

// setMigrationRunLogged says on RunLogged what the run's writes to
// schema_migrations_log came to. A run that reported nothing, or wrote and lost
// nothing, says nothing about the record, so the condition keeps what the last
// run that did say left there: a record one run left incomplete stays reported
// until a later run writes its own in full.
func setMigrationRunLogged(migration *operatorv1alpha1.PtahMigration, log *operatorv1alpha1.MigrationLogStatus, operationID string) {
	if log == nil || log.Written == 0 && log.Lost == 0 {
		return
	}
	run := "the run"
	if operationID != "" {
		run += " of operation " + operationID
	}
	if log.Lost == 0 {
		setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationRunLogged, metav1.ConditionTrue,
			operatorv1alpha1.ReasonLogWritten,
			fmt.Sprintf("The database accepted all %d entries %s wrote to schema_migrations_log", log.Written, run))
		return
	}
	setMigrationCondition(migration, operatorv1alpha1.ConditionMigrationRunLogged, metav1.ConditionFalse,
		operatorv1alpha1.ReasonLogEntriesLost,
		fmt.Sprintf("The database refused %d of the %d entries %s wrote to schema_migrations_log%s, "+
			"so its own record of the run is incomplete. The run's outcome is unchanged; "+
			"status.lastRun.migrationLog and the run's journal entry name the entries",
			log.Lost, log.Written+log.Lost, run, migrationLogAnswers(log.Losses)))
}

// migrationLogAnswers names the codes the database refused entries with, each
// once, in the order they first came. The runner admitted only a
// five-character SQLSTATE and a number, so neither can carry the value refused.
func migrationLogAnswers(losses []operatorv1alpha1.MigrationLogLossStatus) string {
	var answers []string
	for _, loss := range losses {
		var answer string
		switch {
		case loss.SQLState != "" && loss.ErrorNumber > 0:
			answer = fmt.Sprintf("SQLSTATE %s (error %d)", loss.SQLState, loss.ErrorNumber)
		case loss.SQLState != "":
			answer = "SQLSTATE " + loss.SQLState
		case loss.ErrorNumber > 0:
			answer = fmt.Sprintf("error %d", loss.ErrorNumber)
		default:
			answer = "no code"
		}
		if !slices.Contains(answers, answer) {
			answers = append(answers, answer)
		}
	}
	if len(answers) == 0 {
		return ""
	}
	return " with " + strings.Join(answers, ", ")
}
