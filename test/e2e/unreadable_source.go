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

// schemaSourceRefused returns what a schema says when it stopped on a desired
// file Ptah could not read: Ready False with SchemaSourceUnreadable, in phase
// Blocked, with no claim, no plan and no failure standing. Another read of the
// same file gives the same answer, so the operator reads it again at the
// interval, when the source changes or on a read request, and never at the
// failure retry.
func schemaSourceRefused(schema *ptahv1alpha1.PtahSchema) (string, error) {
	ready := meta.FindStatusCondition(schema.Status.Conditions, ptahv1alpha1.ConditionReady)
	switch {
	case ready == nil || ready.Status != metav1.ConditionFalse ||
		ready.Reason != string(ptahv1alpha1.ReasonSchemaSourceUnreadable):
		return "", fmt.Errorf("phase %s, Ready %+v", schema.Status.Phase, ready)
	case schema.Status.Phase != ptahv1alpha1.PhaseBlocked:
		return "", fmt.Errorf("phase %s beside Ready %+v", schema.Status.Phase, ready)
	case schema.Status.ActiveOperation != nil:
		return "", fmt.Errorf("claim %+v stands beside the refusal", schema.Status.ActiveOperation)
	case schema.Status.Plan != nil:
		return "", fmt.Errorf("plan %s stands beside the refusal", schema.Status.Plan.Name)
	case meta.IsStatusConditionTrue(schema.Status.Conditions, ptahv1alpha1.ConditionReconciliationFailed):
		return "", fmt.Errorf("the refusal is reported as a failure: %+v",
			meta.FindStatusCondition(schema.Status.Conditions, ptahv1alpha1.ConditionReconciliationFailed))
	}
	return ready.Message, nil
}

// schemaRefusalStands holds a refused schema to the attempt that refused it. A
// retry starts another read under a claim, and a read that ends stamps another
// attempt, so a hold past the failure retry that reads this throughout shows
// that no read ran.
func schemaRefusalStands(schema *ptahv1alpha1.PtahSchema, attempt *metav1.Time) error {
	if _, err := schemaSourceRefused(schema); err != nil {
		return err
	}
	last := schema.Status.LastAttemptTime
	if attempt == nil || last == nil || !last.Equal(attempt) {
		return fmt.Errorf("the last attempt is %v, not the refused one at %v", last, attempt)
	}
	return nil
}

// readRequestRefusedAgain is a refused schema that answered request, and whose
// read the request started was refused in its turn: by an attempt later than
// the refusal at attempt. The file is still the one Ptah cannot read, and the
// row repairs it only after this, so nothing else could have let the request
// through.
func readRequestRefusedAgain(schema *ptahv1alpha1.PtahSchema, request string, attempt *metav1.Time) error {
	if _, err := schemaSourceRefused(schema); err != nil {
		return err
	}
	last := schema.Status.LastAttemptTime
	switch {
	case request == "" || schema.Status.LastReadRequest != request:
		return fmt.Errorf("lastReadRequest %q, want %q", schema.Status.LastReadRequest, request)
	case attempt == nil || last == nil || !last.After(attempt.Time):
		return fmt.Errorf("the last attempt %v is not after the refusal at %v", last, attempt)
	}
	return nil
}

// migrationDirectoryRefused returns what a migration says when it stopped on a
// directory Ptah could not read: Blocked with MigrationDirectoryUnreadable, in
// phase Blocked, with no claim standing. Another read of the same files gives
// the same answer, so the operator does not retry it, as it does not retry a
// schema's read of a file Ptah cannot read (schemaSourceRefused).
func migrationDirectoryRefused(migration *ptahv1alpha1.PtahMigration) (string, error) {
	blocked := meta.FindStatusCondition(migration.Status.Conditions, ptahv1alpha1.ConditionMigrationBlocked)
	switch {
	case blocked == nil || blocked.Status != metav1.ConditionTrue ||
		blocked.Reason != string(ptahv1alpha1.ReasonMigrationDirectoryUnreadable):
		return "", fmt.Errorf("phase %s, Blocked %+v", migration.Status.Phase, blocked)
	case migration.Status.Phase != ptahv1alpha1.MigrationPhaseBlocked:
		return "", fmt.Errorf("phase %s beside Blocked %+v", migration.Status.Phase, blocked)
	case migration.Status.ActiveOperation != nil:
		return "", fmt.Errorf("claim %+v stands beside the refusal", migration.Status.ActiveOperation)
	}
	return blocked.Message, nil
}

// schemaReadTheRepair says what is missing from a schema that read a repaired
// source by itself: its spec is the one the refusal was reported under, no
// failure stands, and a plan is published from a source other than the one
// Ptah could not read. The generation is what separates this from a repair a
// spec edit carried, which the operator always read.
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
