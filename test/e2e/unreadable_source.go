package e2e

import (
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// What the rows deliver that Ptah cannot read, and how Ptah words each reason.
const (
	// unreadableInheritsSchema is a file the parser refuses: it reads no
	// INHERITS. The PARTITION OF file stokaro/ptah-operator#788 reported was
	// this row's file until Ptah v0.23.0 learned to read it. It is the runner
	// harness's desired-source/schema.sql, whose capture gives the reason.
	unreadableInheritsSchema = `CREATE TABLE p (id bigint NOT NULL PRIMARY KEY);
CREATE TABLE c (extra text) INHERITS (p);
`
	unreadableSchemaFile     = "schema.sql"
	unreadableInheritsReason = "unsupported table option: INHERITS"

	unreadableMigrationFile   = "0000000001_create_events.up.sql"
	unreadableMigrationReason = `invalid +ptah no_transaction value "sometimes"`

	// schemaFetchStep is the init container that pulls a schema artifact. It
	// holds the registry credentials, writes the layer unparsed, and so passes
	// a schema the executor cannot parse on to the operation container.
	schemaFetchStep = "fetch-schema"
	// unreadableArtifactFile is where the fetch leaves the layer in the
	// operation Pod, and unreadableArtifactWords how Ptah opens its refusal of
	// the layer hack/unparsableschemafixture publishes: the runner's check reads
	// the file at that path, so the reason names it there.
	unreadableArtifactFile  = "/source/schema.hcl"
	unreadableArtifactWords = "error parsing schema file: parse HCL schema: " + unreadableArtifactFile + ":"

	// invalidKeySchema loads, and declares a foreign key into a table it does
	// not hold. invalidKeyWords is how Ptah opens the fault, which the capture
	// holds to, and invalidKeyReason the whole of it for this file.
	invalidKeySchema = "CREATE TABLE e2e_invalid_orders (id bigint REFERENCES e2e_no_such_table (id));\n"
	invalidKeyWords  = `invalid foreign key: field "id" references unknown table `
	invalidKeyReason = invalidKeyWords + `"e2e_no_such_table"`
)

// unreadableMigrationFiles is a migration directory the loader refuses: the
// directive's value is neither true nor false.
var unreadableMigrationFiles = []struct{ key, path, body string }{
	{"0001-up", unreadableMigrationFile, "-- +ptah no_transaction=sometimes\nCREATE TABLE e2e_unreadable_events (id bigint);\n"},
	{"0001-down", "0000000001_create_events.down.sql", "DROP TABLE e2e_unreadable_events;\n"},
}

// schemaReadRetried is a schema whose read failed and runs again under the same
// claim. previous is a claim the read has to have left behind, or empty for
// none: it tells the read a request started from the one it was asked to end.
func schemaReadRetried(schema *ptahv1alpha1.PtahSchema, previous string) bool {
	operation := schema.Status.ActiveOperation
	return operation != nil && operation.Attempt > 1 && operation.ID != previous &&
		meta.IsStatusConditionTrue(schema.Status.Conditions, ptahv1alpha1.ConditionReconciliationFailed)
}

// migrationReadRetried is schemaReadRetried for a migration, which reports a
// failed attempt on Progressing.
func migrationReadRetried(migration *ptahv1alpha1.PtahMigration, previous string) bool {
	operation := migration.Status.ActiveOperation
	progressing := meta.FindStatusCondition(migration.Status.Conditions, ptahv1alpha1.ConditionMigrationProgressing)
	return operation != nil && operation.Attempt > 1 && operation.ID != previous &&
		progressing != nil && progressing.Reason == string(ptahv1alpha1.ReasonOperationFailed)
}

// readRequestTakenWhileFailing is a request the operator answered beside a failure
// it reports: the file is still the one it cannot read, so the read the request
// started either has not ended or failed too. The row repairs the file only after
// this, so nothing else could have let the request through.
func readRequestTakenWhileFailing(schema *ptahv1alpha1.PtahSchema, request string) bool {
	return request != "" && schema.Status.LastReadRequest == request &&
		meta.IsStatusConditionTrue(schema.Status.Conditions, ptahv1alpha1.ConditionReconciliationFailed)
}

// schemaReadTheRepair says what is missing from a schema that read a repaired
// source by itself: its spec is the one the failure was reported under, the
// failure is gone, and a plan is published from a source other than the one
// that failed. The generation is what separates this from a repair a spec edit
// carried, which the operator always read.
func schemaReadTheRepair(schema *ptahv1alpha1.PtahSchema, generation int64, failedSource string) error {
	failed := meta.FindStatusCondition(schema.Status.Conditions, ptahv1alpha1.ConditionReconciliationFailed)
	switch {
	case schema.Generation != generation:
		return fmt.Errorf("the spec moved from generation %d to %d, so an edit carried the repair", generation, schema.Generation)
	case failed == nil || failed.Status != metav1.ConditionFalse:
		return fmt.Errorf("ReconciliationFailed is %+v", failed)
	case schema.Status.Plan == nil:
		return fmt.Errorf("no plan yet, phase %s", schema.Status.Phase)
	case schemaSourceIdentity(schema) == "" || schemaSourceIdentity(schema) == failedSource:
		return fmt.Errorf("the plan comes from source %q, the one that failed", schemaSourceIdentity(schema))
	}
	return nil
}

// schemaSourceIdentity names what a schema's source resolved to: a snapshot's
// content, a commit or a digest.
func schemaSourceIdentity(schema *ptahv1alpha1.PtahSchema) string {
	source := schema.Status.Source
	switch {
	case source.Snapshot != nil:
		return source.Snapshot.ContentDigest
	case source.Git != nil:
		return source.Git.Commit
	}
	return source.Digest
}

// migrationReadTheRepair is schemaReadTheRepair for a migration, whose snapshot
// is the only source the row repairs.
func migrationReadTheRepair(migration *ptahv1alpha1.PtahMigration, generation int64, failedSource string) error {
	progressing := meta.FindStatusCondition(migration.Status.Conditions, ptahv1alpha1.ConditionMigrationProgressing)
	artifact := migration.Status.Artifact
	switch {
	case migration.Generation != generation:
		return fmt.Errorf("the spec moved from generation %d to %d, so an edit carried the repair", generation, migration.Generation)
	case progressing != nil && progressing.Reason == string(ptahv1alpha1.ReasonOperationFailed):
		return fmt.Errorf("Progressing is %+v", progressing)
	case migration.Status.Plan == nil:
		return fmt.Errorf("no plan yet, phase %s", migration.Status.Phase)
	case artifact == nil || artifact.Snapshot == nil || artifact.Snapshot.ContentDigest == failedSource:
		return fmt.Errorf("the plan comes from artifact %+v, the one that failed", artifact)
	}
	return nil
}

// unreadableSourceSays holds a condition's or an Event's message to the claim:
// the result code that means the source could not be read, the file by name,
// and Ptah's reason. Reading the code alone would pass on a message with no
// reason in it, and the exit status it replaced names neither.
func unreadableSourceSays(message, file, reason string) error {
	switch {
	case !strings.Contains(message, runner.CodeDesiredSourceUnreadable+": "):
		return fmt.Errorf("the message does not carry %s: %q", runner.CodeDesiredSourceUnreadable, message)
	case !strings.Contains(message, file):
		return fmt.Errorf("the message does not name file %s: %q", file, message)
	case !strings.Contains(message, reason):
		return fmt.Errorf("the message does not say %q: %q", reason, message)
	}
	return nil
}

// invalidSchemaSays holds a condition's or an Event's message to the claim: the
// result code that means Ptah read the file and refuses it, the file by name, and
// the fault. It refuses a file Ptah could not read, whose code says something
// else, and the exit status a fault read as before.
func invalidSchemaSays(message, file, reason string) error {
	switch {
	case !strings.Contains(message, runner.CodeDesiredSchemaInvalid+": "):
		return fmt.Errorf("the message does not carry %s: %q", runner.CodeDesiredSchemaInvalid, message)
	case !strings.Contains(message, file):
		return fmt.Errorf("the message does not name file %s: %q", file, message)
	case !strings.Contains(message, reason):
		return fmt.Errorf("the message does not say %q: %q", reason, message)
	}
	return nil
}

// fetchPassedTheLayerOn holds a terminal Pod of a schema artifact's run to
// where the run went: the fetch step ended at zero and the ptah container ran
// to its end, so the reason in status is the words of a container that holds
// no registry credential. A fetch that parses the layer stops the Pod at the
// fetch step instead, and leaves the ptah container waiting.
func fetchPassedTheLayerOn(pod *corev1.Pod) error {
	var fetch, operation *corev1.ContainerStatus
	for index := range pod.Status.InitContainerStatuses {
		if status := &pod.Status.InitContainerStatuses[index]; status.Name == schemaFetchStep {
			fetch = status
		}
	}
	for index := range pod.Status.ContainerStatuses {
		if status := &pod.Status.ContainerStatuses[index]; status.Name == "ptah" {
			operation = status
		}
	}
	switch {
	case fetch == nil:
		return fmt.Errorf("Pod %s has no %s step", pod.Name, schemaFetchStep)
	case fetch.State.Terminated == nil:
		return fmt.Errorf("the %s step of Pod %s has not ended: %+v", schemaFetchStep, pod.Name, fetch.State)
	case fetch.State.Terminated.ExitCode != 0:
		return fmt.Errorf("the %s step of Pod %s exited %d, so the run never reached the ptah container",
			schemaFetchStep, pod.Name, fetch.State.Terminated.ExitCode)
	case operation == nil || operation.State.Terminated == nil:
		return fmt.Errorf("the ptah container of Pod %s has not run to its end: %+v", pod.Name, operation)
	}
	return nil
}

// unreadableSourceEvent finds the Warning Event a failure wrote on one resource
// with the reason in it.
func unreadableSourceEvent(events []corev1.Event, owner types.UID, reason, file, want string) error {
	return warningEventSays(events, owner, reason, func(message string) error {
		return unreadableSourceSays(message, file, want)
	})
}

// invalidSchemaEvent finds the Warning Event a failure wrote on one resource
// with the fault in it.
func invalidSchemaEvent(events []corev1.Event, owner types.UID, reason, file, want string) error {
	return warningEventSays(events, owner, reason, func(message string) error {
		return invalidSchemaSays(message, file, want)
	})
}

// warningEventSays reports what it examined, so a list with no Event of that
// resource and that reason reads differently from one that says something else.
func warningEventSays(events []corev1.Event, owner types.UID, reason string, says func(string) error) error {
	examined := 0
	var last error
	for index := range events {
		event := &events[index]
		if event.InvolvedObject.UID != owner || event.Reason != reason || event.Type != corev1.EventTypeWarning {
			continue
		}
		examined++
		if last = says(event.Message); last == nil {
			return nil
		}
	}
	if examined == 0 {
		return errors.New("no Warning " + reason + " Event of the resource yet")
	}
	return fmt.Errorf("%d Warning %s Events, none says why: %w", examined, reason, last)
}
