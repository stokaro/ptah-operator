package resultretention_test

import (
	"encoding/json"
	"strings"
	"testing"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	"github.com/stokaro/ptah-operator/internal/resultretention"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRecoveryPinsSurviveOperationRetirement(t *testing.T) {
	for _, family := range []string{"schema-apply-admitted-scheduling", "migration-apply-admitted-scheduling"} {
		states := []string{"active", "retired", "other operation", "pending lock release", "replacement owner"}
		if family == "schema-apply-admitted-scheduling" {
			states = append(states, "pending proof")
		} else {
			states = append(states, "unresolved status", "resolved run", "restored unresolved copy", "malformed copy", "unknown copy fields", "other unresolved copy", "last run", "unidentified last run", "replacement last Job")
		}
		for _, state := range states {
			t.Run(family+"/"+state, func(t *testing.T) {
				f := resulttest.New(t, family)
				b := f.Identity.Binding
				pinned := false
				switch subject := f.Subject.(type) {
				case *api.PtahSchema:
					subject.Status.ActiveOperation = nil
					switch state {
					case "active":
						subject.Status.ActiveOperation = &api.ActiveOperationStatus{ID: b.OperationID}
						pinned = true
					case "other operation":
						subject.Status.ActiveOperation = &api.ActiveOperationStatus{ID: "other"}
					case "pending proof":
						subject.Status.PendingObservation = &api.PendingObservationStatus{ApplyOperationID: b.OperationID}
						pinned = true
					case "pending lock release":
						subject.Status.PendingLockRelease = &api.TargetLockReleaseStatus{OperationID: b.OperationID}
						pinned = true
					case "replacement owner":
						subject.UID = "replacement"
						subject.Status.ActiveOperation = &api.ActiveOperationStatus{ID: b.OperationID}
					}
				case *api.PtahMigration:
					subject.Status.ActiveOperation = nil
					run := &api.UnresolvedMigrationRunStatus{OperationID: b.OperationID, Outcome: api.MigrationRunOutcomeUnknown, PlanRef: api.ImmutableObjectReference{Name: "plan", UID: "plan-uid"}, RecordedAt: metav1.Now()}
					switch state {
					case "active":
						subject.Status.ActiveOperation = &api.MigrationOperationStatus{ID: b.OperationID}
						pinned = true
					case "other operation":
						subject.Status.ActiveOperation = &api.MigrationOperationStatus{ID: "other"}
					case "unresolved status":
						subject.Status.UnresolvedRun = run
						pinned = true
					case "resolved run":
						subject.Status.ResolvedRun = &api.ResolvedMigrationRunStatus{OperationID: b.OperationID}
						pinned = true
					case "pending lock release":
						subject.Status.PendingLockRelease = &api.TargetLockReleaseStatus{OperationID: b.OperationID}
						pinned = true
					case "restored unresolved copy", "malformed copy", "unknown copy fields", "other unresolved copy":
						pinned = state != "other unresolved copy"
						if !pinned {
							run.OperationID = "sha256:" + strings.Repeat("7", 64)
						}
						encoded, err := json.Marshal(run)
						if err != nil {
							t.Fatal(err)
						}
						value := string(encoded)
						if state == "malformed copy" {
							value = "{}"
						} else if state == "unknown copy fields" {
							value = value[:len(value)-1] + `,"future":"pin"}`
						}
						subject.Annotations = map[string]string{api.UnresolvedRunAnnotation: value}
					case "last run", "unidentified last run", "replacement last Job":
						subject.Status.LastRun = &api.MigrationRunStatus{JobName: b.JobName, JobUID: b.JobUID}
						pinned = state != "replacement last Job"
						if state == "unidentified last run" {
							subject.Status.LastRun.JobName = ""
						}
						if !pinned {
							subject.Status.LastRun.JobUID = "another-job"
						}
					case "replacement owner":
						subject.UID = "replacement"
						subject.Status.ActiveOperation = &api.MigrationOperationStatus{ID: b.OperationID}
					}
				}
				if err := resultretention.CheckUnpinned(t.Context(), f.Client(t), b); (err != nil) != pinned {
					t.Fatalf("pinned=%v: %v", pinned, err)
				}
			})
		}
	}
}
