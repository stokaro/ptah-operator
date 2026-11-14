package statusview

import (
	"fmt"
	"strings"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// MigrationLog says what a run's writes to schema_migrations_log came to: how
// many entries the database accepted, and, when it refused any, that the
// database's own record of the run is incomplete and which entries it lacks.
// The history and the status print the same words for the same record.
func MigrationLog(log operatorv1alpha1.MigrationLogStatus) string {
	if log.Lost == 0 {
		return fmt.Sprintf("%d %s written, none refused", log.Written, entries(log.Written))
	}
	lost := make([]string, 0, len(log.Losses))
	for _, loss := range log.Losses {
		entry := fmt.Sprintf("version %d %s", loss.Version, Clean(string(loss.State)))
		var answer []string
		if loss.SQLState != "" {
			answer = append(answer, "SQLSTATE "+Clean(loss.SQLState))
		}
		if loss.ErrorNumber != 0 {
			answer = append(answer, fmt.Sprintf("error %d", loss.ErrorNumber))
		}
		if len(answer) > 0 {
			entry += " (" + strings.Join(answer, ", ") + ")"
		}
		lost = append(lost, entry)
	}
	if more := int(log.Lost) - len(log.Losses); more > 0 {
		lost = append(lost, fmt.Sprintf("%d more", more))
	}
	text := fmt.Sprintf("%d of %d %s refused, so the database's record of the run is incomplete",
		log.Lost, log.Written+log.Lost, entries(log.Written+log.Lost))
	if len(lost) > 0 {
		text += ": " + strings.Join(lost, ", ")
	}
	return text
}

func entries(count int32) string {
	if count == 1 {
		return "entry"
	}
	return "entries"
}
