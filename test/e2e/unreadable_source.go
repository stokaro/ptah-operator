package e2e

import (
	"errors"
	"fmt"
	"regexp"
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

	// strayMigrationFile is a top-level SQL file a directory of Ptah's
	// ten-digit files does not name, and strayMigrationReason how Ptah opens
	// its refusal of the directory that holds it.
	strayMigrationFile   = "seed.sql"
	strayMigrationReason = "migration file the Ptah format does not read"
	// migrationFetchStep is the init container that pulls a migrations
	// artifact, with the registry credentials, and noMigrationFilesWords its
	// refusal of an artifact that holds no migration file, which stays in the
	// step's log.
	migrationFetchStep    = "fetch-migrations"
	noMigrationFilesWords = "migration artifact contains no migration files"
	// registryRefusalPassword is the wrong registry password the fetch row
	// gives the step, and registryRefusalDatabasePassword the wrong database
	// password that holds its History in retry until then. The database is
	// named without ptah_e2e, the registry user the row scans for.
	registryRefusalPassword         = "e2e-not-the-registry-password"
	registryRefusalDatabasePassword = "e2e-not-the-database-password"
	registryRefusalDatabase         = "e2e_registry_refusal"

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

	// invalidDomainSchema loads, and declares a timestamp domain whose own
	// default is 'now', which Ptah v0.34.0 refuses as a fault of the schema
	// (stokaro/ptah#6386). Drift and plan name the domain in the schema they
	// placed it in, "public.e2e_moment", and validate names it as the file
	// does. invalidDomainWords is how Ptah words the fault, which the capture
	// of the runner harness's relative-time-domain.sql holds to, and
	// invalidDomainReason the start of it for this file, with the name the
	// runner reports: validate's.
	invalidDomainSchema = "CREATE DOMAIN e2e_moment AS timestamp DEFAULT 'now';\n" +
		"CREATE TABLE e2e_invalid_events (id bigint PRIMARY KEY, created_at e2e_moment NOT NULL);\n"
	invalidDomainWords  = `: DEFAULT 'now' on a timestamp domain is read once, when the domain is created`
	invalidDomainReason = `domain "e2e_moment"` + invalidDomainWords
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
	return schemaRefused(schema, ptahv1alpha1.ReasonSchemaSourceUnreadable)
}

// schemaInvalidRefused is schemaSourceRefused for a desired schema Ptah read
// and refuses as a whole: the same stop, with Ready's reason SchemaInvalid.
// The read compared the schema with the database before it stopped, so
// DatabaseReachable has to say the read opened it.
func schemaInvalidRefused(schema *ptahv1alpha1.PtahSchema) (string, error) {
	message, err := schemaRefused(schema, ptahv1alpha1.ReasonSchemaInvalid)
	if err != nil {
		return "", err
	}
	reachable := meta.FindStatusCondition(schema.Status.Conditions, ptahv1alpha1.ConditionDatabaseReachable)
	if reachable == nil || reachable.Status != metav1.ConditionTrue || reachable.Reason != string(ptahv1alpha1.ReasonObserved) {
		return "", fmt.Errorf("DatabaseReachable %+v beside the refusal, which opened the database", reachable)
	}
	return message, nil
}

// schemaRefused is a schema stopped on its desired schema with reason: Ready
// False with that reason, in phase Blocked, with no claim, no plan and no
// failure standing. It returns Ready's message.
func schemaRefused(schema *ptahv1alpha1.PtahSchema, reason ptahv1alpha1.ConditionReason) (string, error) {
	ready := meta.FindStatusCondition(schema.Status.Conditions, ptahv1alpha1.ConditionReady)
	switch {
	case ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != string(reason):
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

// schemaRefusalStands holds a schema refused as refused says to the attempt
// that refused it. A retry starts another read under a claim, and a read that
// ends stamps another attempt, so a hold past the failure retry that reads
// this throughout shows that no read ran.
func schemaRefusalStands(schema *ptahv1alpha1.PtahSchema, refused func(*ptahv1alpha1.PtahSchema) (string, error), attempt *metav1.Time) error {
	if _, err := refused(schema); err != nil {
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

// editRefusedAgain is a schema refused as invalid whose source edit, which
// generation does not count, started a read that Ptah refused in its turn: by
// an attempt later than the refusal at attempt, from a source other than
// failedSource. The edit is the only thing that moves the source, so nothing
// else could have started that read.
func editRefusedAgain(schema *ptahv1alpha1.PtahSchema, generation int64, failedSource string, attempt *metav1.Time) (string, error) {
	message, err := schemaInvalidRefused(schema)
	if err != nil {
		return "", err
	}
	last := schema.Status.LastAttemptTime
	switch {
	case schema.Generation != generation:
		return "", fmt.Errorf("the spec moved from generation %d to %d, so an edit of the spec started the read", generation, schema.Generation)
	case schemaSourceIdentity(schema) == "" || schemaSourceIdentity(schema) == failedSource:
		return "", fmt.Errorf("the refusal comes from source %q, the one refused before", schemaSourceIdentity(schema))
	case attempt == nil || last == nil || !last.After(attempt.Time):
		return "", fmt.Errorf("the last attempt %v is not after the refusal at %v", last, attempt)
	}
	return message, nil
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

// fetchRefusalSentence is the pattern of the sentence the operator writes for
// a fetch step that failed: the step and its exit code.
func fetchRefusalSentence(step string) string {
	return `the ` + regexp.QuoteMeta(step) + ` step exited [1-9][0-9]*, so the run never started`
}

// fetchRefusalSaysOnly holds a migration whose fetch step failed to a
// Progressing condition that names the step and its exit code and nothing
// else: the step holds the registry credential, so what it printed stays in
// its log. A message that goes on to carry the step's words, names another
// step, or says the source's access was refused, is refused.
func fetchRefusalSaysOnly(status ptahv1alpha1.PtahMigrationStatus, step string) error {
	progressing := meta.FindStatusCondition(status.Conditions, ptahv1alpha1.ConditionMigrationProgressing)
	if progressing == nil {
		return fmt.Errorf("phase %s with no %s condition", status.Phase, ptahv1alpha1.ConditionMigrationProgressing)
	}
	pattern := regexp.MustCompile(`^` + fetchRefusalSentence(step) + `$`)
	if !pattern.MatchString(progressing.Message) {
		return fmt.Errorf("%s says %q, want the %s step and its exit code alone", progressing.Type, progressing.Message, step)
	}
	return nil
}

// operationRetriedReason is the reason of the Warning Event a migration writes
// each time it retries an operation that failed.
const operationRetriedReason = "OperationRetried"

// fetchRefusalEvent finds the OperationRetried Warning Event one resource
// wrote when the fetch step of an operation failed: "<operation> attempt <n>: "
// and the condition's sentence, and nothing more. An Event that goes on to
// carry the step's words, or names another step or operation, is refused.
func fetchRefusalEvent(events []corev1.Event, owner types.UID, operation, step string) error {
	pattern := regexp.MustCompile(`^` + regexp.QuoteMeta(operation) + ` attempt [1-9][0-9]*: ` + fetchRefusalSentence(step) + `$`)
	return warningEventSays(events, owner, operationRetriedReason, func(message string) error {
		if !pattern.MatchString(message) {
			return fmt.Errorf("the Event says %q, want the %s step of the %s and its exit code alone", message, step, operation)
		}
		return nil
	})
}

// failedFetchLog is the log of the step of a Pod whose step failed, where the
// words the operator withholds from status stay. It refuses a Pod list in
// which no Pod's step failed, since a row that scans for words the step never
// printed would pass however the operator carried them.
func failedFetchLog(pods []corev1.Pod, step string, read func(pod, container string) ([]byte, error)) ([]byte, error) {
	for index := range pods {
		for _, status := range pods[index].Status.InitContainerStatuses {
			if status.Name != step || status.State.Terminated == nil || status.State.Terminated.ExitCode == 0 {
				continue
			}
			return read(pods[index].Name, step)
		}
	}
	return nil, fmt.Errorf("none of %d Pods has a %s step that failed", len(pods), step)
}

// registryRefusalWords are the parts of Ptah's answer to a registry that
// refused the credential that the fetch step's log has to carry: the status
// code, the registry's word for it, and the URL the step asked, which names
// the registry and the repository. A log without them did not fail on the
// credential. The URL is returned as the step printed it. The log itself is
// not quoted: the step held the credential.
func registryRefusalWords(log []byte) ([]string, error) {
	match := regexp.MustCompile(`GET "([^"\s]+/v2/[^"\s]+)": response status code 401: unauthorized`).FindSubmatch(log)
	if match == nil {
		return nil, fmt.Errorf("the step's log of %d bytes carries no 401 for a registry request", len(log))
	}
	return []string{"response status code 401", "unauthorized", string(match[1])}, nil
}

// saysNoneOf refuses a document that carries one of the words. It refuses to
// pass on no document, on an empty document and on no word, since each of
// those would hold whatever the operator wrote.
func saysNoneOf(documents map[string][]byte, words []string) error {
	if len(documents) == 0 || len(words) == 0 {
		return fmt.Errorf("%d documents and %d words, so nothing was scanned", len(documents), len(words))
	}
	for name, document := range documents {
		if len(document) == 0 {
			return fmt.Errorf("%s is empty, so nothing was scanned", name)
		}
		for _, word := range words {
			if word == "" {
				return fmt.Errorf("an empty word is found in every document")
			}
			if strings.Contains(string(document), word) {
				return fmt.Errorf("%s carries %q", name, word)
			}
		}
	}
	return nil
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
