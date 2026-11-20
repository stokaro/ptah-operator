package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/test/e2e/gitrepo"
)

// capturedReason is the reason the pinned Ptah image gave for the file, from
// the output internal/dataplane/testdata/desired-source holds.
func capturedReason(t *testing.T, fixture string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "dataplane", "testdata", "desired-source", fixture))
	if err != nil {
		t.Fatal(err)
	}
	reason, found := dataplane.SchemaSourceProblem(data, "PostgreSQL")
	if !found {
		reason, found = dataplane.MigrationFilesProblem(data)
	}
	if !found {
		t.Fatalf("%s holds no reason", fixture)
	}
	return reason
}

func TestUnreadableSourceSaysHoldsTheClaim(t *testing.T) {
	t.Parallel()

	const code = runner.CodeDesiredSourceUnreadable + ": "
	inherits := code + capturedReason(t, "validate-sql.txt")
	migration := code + capturedReason(t, "scratch-directive.json")
	if err := unreadableSourceSays(inherits, unreadableSchemaFile, unreadableInheritsReason); err != nil {
		t.Errorf("the schema reason: %v", err)
	}
	if err := unreadableSourceSays("OperationRetried history attempt 1: "+inherits, unreadableSchemaFile, unreadableInheritsReason); err != nil {
		t.Errorf("the reason inside an Event's sentence: %v", err)
	}
	// The fixture's migration file is named by the directory the pinned image
	// was measured on, and the row's by what it delivers.
	if err := unreadableSourceSays(migration, "0000000001_init.up.sql", unreadableMigrationReason); err != nil {
		t.Errorf("the migration reason: %v", err)
	}

	for name, message := range map[string]string{
		"the exit status it replaced": "child_exit: ptah exited with code 2",
		"the Plan code it replaced":   "invalid_plan_output: plan command did not complete successfully",
		"the code with no reason":     code + "the file",
		"another file":                code + "error parsing schema file: read SQL schema file /source/other.sql:2:29: " + unreadableInheritsReason,
		// The reason Ptah v0.22.0 gave for the PARTITION OF file this row
		// delivered before.
		"another reason": code + "error parsing schema file: read SQL schema file /source/" + unreadableSchemaFile +
			": expected '(' after table name: expected Operator, got Identifier at position 180",
		"another code": "invalid_input: read SQL schema file " + unreadableSchemaFile + ":2:29: " + unreadableInheritsReason,
		"a kind":       "database_unreachable: " + unreadableSchemaFile + " " + unreadableInheritsReason,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := unreadableSourceSays(message, unreadableSchemaFile, unreadableInheritsReason); err == nil {
				t.Fatalf("unreadableSourceSays(%q) accepted it", message)
			}
		})
	}
}

// capturedFault is the fault the pinned Ptah image gave for a file, read the way
// the runner reads it: from validate's output, held to what drift said.
func capturedFault(t *testing.T, validated, failed string) string {
	t.Helper()
	read := func(fixture string) []byte {
		data, err := os.ReadFile(filepath.Join("..", "..", "internal", "dataplane", "testdata", "desired-source", fixture))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	failure, err := dataplane.DecodeDriftFailure(read(failed))
	if err != nil {
		t.Fatal(err)
	}
	fault, found := dataplane.SchemaStructuralProblem(read(validated), "PostgreSQL", failure.Message)
	if !found {
		t.Fatalf("%s holds no fault %s stopped on", validated, failed)
	}
	return fault
}

func TestInvalidSchemaSaysHoldsTheClaim(t *testing.T) {
	t.Parallel()

	const code = runner.CodeDesiredSchemaInvalid + ": "
	fault := capturedFault(t, "validate-foreign-key.txt", "drift-foreign-key.json")
	if !strings.HasPrefix(fault, invalidKeyWords) {
		t.Fatalf("the capture says %q, and the row waits for %q", fault, invalidKeyWords)
	}
	said := code + "schema.sql: " + fault
	if err := invalidSchemaSays(said, "schema.sql", invalidKeyWords); err != nil {
		t.Errorf("the fault: %v", err)
	}
	if err := invalidSchemaSays("OperationFailed: "+said, "schema.sql", invalidKeyWords); err != nil {
		t.Errorf("the fault inside an Event's sentence: %v", err)
	}
	for name, message := range map[string]string{
		"the exit status it replaced": "child_exit: ptah exited with code 2",
		"the Plan code it replaced":   "invalid_plan_output: plan command did not complete successfully",
		"the code with no fault":      code + "schema.sql: the file",
		"another file":                code + "other.sql: " + fault,
		"a file that does not load":   runner.CodeDesiredSourceUnreadable + ": schema.sql: " + fault,
		"another fault": code + `schema.sql: postgres requires referenced columns code on table "parent" to be declared unique: ` +
			`a primary key, UNIQUE constraint or unique index must key exactly those columns`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := invalidSchemaSays(message, "schema.sql", invalidKeyWords); err == nil {
				t.Fatalf("invalidSchemaSays(%q) accepted it", message)
			}
		})
	}
}

func TestInvalidSchemaEventFindsTheEventOfTheResource(t *testing.T) {
	t.Parallel()

	const owner = types.UID("schema-uid")
	said := runner.CodeDesiredSchemaInvalid + ": schema.sql: " + capturedFault(t, "validate-foreign-key.txt", "drift-foreign-key.json")
	event := func(uid types.UID, kind, reason, message string) corev1.Event {
		return corev1.Event{InvolvedObject: corev1.ObjectReference{UID: uid}, Type: kind, Reason: reason, Message: message}
	}
	check := func(events ...corev1.Event) error {
		return invalidSchemaEvent(events, owner, "OperationFailed", "schema.sql", invalidKeyWords)
	}
	if err := check(event(owner, corev1.EventTypeWarning, "OperationFailed", "child_exit: ptah exited with code 2"),
		event(owner, corev1.EventTypeWarning, "OperationFailed", said)); err != nil {
		t.Errorf("the Event among others: %v", err)
	}
	for name, events := range map[string][]corev1.Event{
		"no Events":                      nil,
		"another resource's Event":       {event("other-uid", corev1.EventTypeWarning, "OperationFailed", said)},
		"another reason":                 {event(owner, corev1.EventTypeWarning, "OperationRetried", said)},
		"a Normal Event":                 {event(owner, corev1.EventTypeNormal, "OperationFailed", said)},
		"an Event that does not say why": {event(owner, corev1.EventTypeWarning, "OperationFailed", "child_exit: ptah exited with code 2")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := check(events...); err == nil {
				t.Fatal("it accepted the list")
			}
		})
	}
}

func TestUnreadableSourceEventFindsTheEventOfTheResource(t *testing.T) {
	t.Parallel()

	const (
		owner  = types.UID("schema-uid")
		reason = string(ptahv1alpha1.ReasonSchemaSourceUnreadable)
	)
	said := runner.CodeDesiredSourceUnreadable + ": " + capturedReason(t, "validate-sql.txt")
	event := func(uid types.UID, kind, reason, message string) corev1.Event {
		return corev1.Event{InvolvedObject: corev1.ObjectReference{UID: uid}, Type: kind, Reason: reason, Message: message}
	}
	check := func(events ...corev1.Event) error {
		return unreadableSourceEvent(events, owner, reason, unreadableSchemaFile, unreadableInheritsReason)
	}

	if err := check(event(owner, corev1.EventTypeWarning, reason, "child_exit: ptah exited with code 2"),
		event(owner, corev1.EventTypeWarning, reason, said)); err != nil {
		t.Errorf("the Event among others: %v", err)
	}
	for name, test := range map[string]struct {
		events []corev1.Event
		want   string
	}{
		"no Events": {nil, "no Warning " + reason},
		"another resource's Event": {
			[]corev1.Event{event("other-uid", corev1.EventTypeWarning, reason, said)}, "no Warning " + reason,
		},
		// The Event each failed attempt wrote before a refusal stopped the read.
		"the reason it replaced": {
			[]corev1.Event{event(owner, corev1.EventTypeWarning, "OperationFailed", said)}, "no Warning " + reason,
		},
		"a Normal Event": {
			[]corev1.Event{event(owner, corev1.EventTypeNormal, reason, said)}, "no Warning " + reason,
		},
		"an Event that does not say why": {
			[]corev1.Event{event(owner, corev1.EventTypeWarning, reason, "child_exit: ptah exited with code 2")},
			"1 Warning " + reason + " Events, none says why",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := check(test.events...)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unreadableSourceEvent() = %v, want %q", err, test.want)
			}
		})
	}
}

// TestTheUnreadableGitCommitLeavesTheCapturedOnes holds the repository the Git row
// serves to the history the captures name: one commit more, on top of the
// rewritten main, with the file at the path the row selects.
func TestTheUnreadableGitCommitLeavesTheCapturedOnes(t *testing.T) {
	t.Parallel()

	spec := gitUnreadableSpec()
	built, err := gitrepo.Build(spec)
	if err != nil {
		t.Fatal(err)
	}
	rewritten, err := gitrepo.Build(gitRewrittenSpec())
	if err != nil {
		t.Fatal(err)
	}
	captured := len(gitRewrittenSpec().Commits)
	if len(spec.Commits) != captured+1 {
		t.Fatalf("the spec has %d commits, want the %d of the rewritten history and one more", len(spec.Commits), captured)
	}
	for index := range captured {
		if built.Commit(index) != rewritten.Commit(index) {
			t.Errorf("commit %d moved from %s to %s", index, rewritten.Commit(index), built.Commit(index))
		}
	}
	if spec.Branches["main"] != captured {
		t.Errorf("main names commit %d, want the added one", spec.Branches["main"])
	}
	if spec.Commits[captured].Files[gitUnreadablePath] != unreadableInheritsSchema {
		t.Errorf("the added commit does not hold the file at %s", gitUnreadablePath)
	}
	migrationFile := gitUnreadableMigrationPath + "/" + unreadableMigrationFile
	if spec.Commits[captured].Files[migrationFile] != unreadableMigrationFiles[0].body {
		t.Errorf("the added commit does not hold the file at %s", migrationFile)
	}
	for index := range captured {
		for _, added := range []string{gitUnreadablePath, migrationFile} {
			if _, present := spec.Commits[index].Files[added]; present {
				t.Errorf("commit %d holds the file %s the row adds", index, added)
			}
		}
	}
}

// TestTheRepairedGitCommitRepairsOnlyTheSchemaFile holds the commit the row pushes
// to what the row claims of it: the schema file changed, and the migration row
// that runs after it still finds the file it cannot load.
func TestTheRepairedGitCommitRepairsOnlyTheSchemaFile(t *testing.T) {
	t.Parallel()
	unreadable, repaired := gitUnreadableSpec(), gitRepairedSpec()
	if len(repaired.Commits) != len(unreadable.Commits)+1 {
		t.Fatalf("the repair has %d commits, want one more than %d", len(repaired.Commits), len(unreadable.Commits))
	}
	if repaired.Branches["main"] != len(repaired.Commits)-1 {
		t.Fatalf("main names commit %d, not the repair", repaired.Branches["main"])
	}
	files := repaired.Commits[len(repaired.Commits)-1].Files
	if body := files[gitUnreadablePath]; body == "" || body == unreadableInheritsSchema {
		t.Fatalf("the repair left %q at %s", body, gitUnreadablePath)
	}
	migrationFile := gitUnreadableMigrationPath + "/" + unreadableMigrationFile
	if files[migrationFile] != unreadableMigrationFiles[0].body {
		t.Fatalf("the repair changed %s, which the next row needs unreadable", migrationFile)
	}
}

// refusalReading is the schema the reconciler stores when an Observe could not
// read the desired schema. TestTheRefusalReadingIsWhatTheReconcilerStores in
// internal/controller holds the file to what the reconciler writes, so the
// predicates below are tested against a state the controller produces.
func refusalReading(t *testing.T) *ptahv1alpha1.PtahSchema {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "controller", "testdata", "schema-source-unreadable.json"))
	if err != nil {
		t.Fatal(err)
	}
	schema := &ptahv1alpha1.PtahSchema{}
	if err := json.Unmarshal(data, schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

// schemaSourceRefused takes the schema the reconciler stores for a file Ptah
// cannot read, and refuses the retry it replaced, the reads of the cycle that
// leads up to it, and a refusal something stands beside.
func TestASchemaSourceRefusalIsBlockedWithNoClaim(t *testing.T) {
	t.Parallel()
	message, err := schemaSourceRefused(refusalReading(t))
	if err != nil {
		t.Fatalf("schemaSourceRefused() refused the reconciler's reading: %v", err)
	}
	if err := unreadableSourceSays(message, "relative-time.sql", "DEFAULT 'now'"); err != nil {
		t.Fatalf("the reading's message: %v", err)
	}
	setCondition := func(schema *ptahv1alpha1.PtahSchema, condition metav1.Condition) {
		meta.SetStatusCondition(&schema.Status.Conditions, condition)
	}
	for name, edit := range map[string]func(*ptahv1alpha1.PtahSchema){
		"the retry it replaced": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Phase = ptahv1alpha1.PhaseFailed
			s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationObserve, ID: "read", Attempt: 2}
			setCondition(s, metav1.Condition{Type: ptahv1alpha1.ConditionReconciliationFailed, Status: metav1.ConditionTrue,
				Reason: string(ptahv1alpha1.ReasonOperationFailed), Message: message})
			setCondition(s, metav1.Condition{Type: ptahv1alpha1.ConditionReady, Status: metav1.ConditionFalse,
				Reason: string(ptahv1alpha1.ReasonOperationFailed), Message: "Reconciliation did not complete"})
		},
		"the read a request started": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Phase = ptahv1alpha1.PhaseObserving
			s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationObserve, ID: "next"}
		},
		"a claim beside": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{ID: "next"}
		},
		"a plan beside":  func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan = &ptahv1alpha1.CurrentPlanStatus{Name: "plan"} },
		"another phase":  func(s *ptahv1alpha1.PtahSchema) { s.Status.Phase = ptahv1alpha1.PhaseInSync },
		"another reason": func(s *ptahv1alpha1.PtahSchema) { readyReason(s, ptahv1alpha1.ReasonPolicyChanged) },
		"Ready true": func(s *ptahv1alpha1.PtahSchema) {
			meta.FindStatusCondition(s.Status.Conditions, ptahv1alpha1.ConditionReady).Status = metav1.ConditionTrue
		},
		"no Ready": func(s *ptahv1alpha1.PtahSchema) {
			meta.RemoveStatusCondition(&s.Status.Conditions, ptahv1alpha1.ConditionReady)
		},
		"a failure beside": func(s *ptahv1alpha1.PtahSchema) {
			meta.FindStatusCondition(s.Status.Conditions, ptahv1alpha1.ConditionReconciliationFailed).Status = metav1.ConditionTrue
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			schema := refusalReading(t)
			edit(schema)
			if said, err := schemaSourceRefused(schema); err == nil {
				t.Fatalf("accepted it: %q", said)
			}
		})
	}
}

func readyReason(schema *ptahv1alpha1.PtahSchema, reason ptahv1alpha1.ConditionReason) {
	meta.FindStatusCondition(schema.Status.Conditions, ptahv1alpha1.ConditionReady).Reason = string(reason)
}

// A refusal that stands is the one the hold began with: a later attempt, or a
// read under way, is a read the operator ran, which is what the hold rules out.
func TestASchemaRefusalStandsOnlyOnItsOwnAttempt(t *testing.T) {
	t.Parallel()
	refused := refusalReading(t)
	attempt := refused.Status.LastAttemptTime.DeepCopy()
	if err := schemaRefusalStands(refused, attempt); err != nil {
		t.Fatalf("refused the refusal on its own attempt: %v", err)
	}
	later := metav1.NewTime(attempt.Add(15 * time.Second))
	for name, edit := range map[string]func(*ptahv1alpha1.PtahSchema){
		"a later refusal": func(s *ptahv1alpha1.PtahSchema) { s.Status.LastAttemptTime = &later },
		"a retry under way": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{ID: "retry", Attempt: 2}
		},
		"no attempt": func(s *ptahv1alpha1.PtahSchema) { s.Status.LastAttemptTime = nil },
		"the refusal gone": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Phase = ptahv1alpha1.PhaseFailed
			readyReason(s, ptahv1alpha1.ReasonOperationFailed)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			schema := refusalReading(t)
			edit(schema)
			if err := schemaRefusalStands(schema, attempt); err == nil {
				t.Fatal("accepted it")
			}
		})
	}
	if err := schemaRefusalStands(refused, nil); err == nil {
		t.Error("accepted a hold that began with no attempt")
	}
}

// A request refused again is one the schema answered, refused by an attempt
// after the refusal the request was written against.
func TestARequestRefusedAgainNeedsALaterRefusal(t *testing.T) {
	t.Parallel()
	const request = "2026-08-30T12:05:00Z"
	attempt := refusalReading(t).Status.LastAttemptTime.DeepCopy()
	later := metav1.NewTime(attempt.Add(40 * time.Second))
	answered := func() *ptahv1alpha1.PtahSchema {
		schema := refusalReading(t)
		schema.Status.LastReadRequest = request
		schema.Status.LastAttemptTime = later.DeepCopy()
		return schema
	}
	if err := readRequestRefusedAgain(answered(), request, attempt); err != nil {
		t.Fatalf("refused a request refused in its turn: %v", err)
	}
	for name, edit := range map[string]func(*ptahv1alpha1.PtahSchema){
		"not answered":      func(s *ptahv1alpha1.PtahSchema) { s.Status.LastReadRequest = "" },
		"another request":   func(s *ptahv1alpha1.PtahSchema) { s.Status.LastReadRequest = "2026-08-30T11:00:00Z" },
		"the first refusal": func(s *ptahv1alpha1.PtahSchema) { s.Status.LastAttemptTime = attempt.DeepCopy() },
		"no attempt":        func(s *ptahv1alpha1.PtahSchema) { s.Status.LastAttemptTime = nil },
		"the read under way": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Phase = ptahv1alpha1.PhaseObserving
			s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{ID: "next"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			schema := answered()
			edit(schema)
			if err := readRequestRefusedAgain(schema, request, attempt); err == nil {
				t.Fatal("accepted it")
			}
		})
	}
	unanswered := refusalReading(t)
	unanswered.Status.LastAttemptTime = later.DeepCopy()
	if err := readRequestRefusedAgain(unanswered, "", attempt); err == nil {
		t.Error("an empty request matched an empty answer")
	}
}

// migrationDirectoryRefused takes a migration that stopped on its directory, and
// refuses the retry it replaced, another reason, and a stop a claim stands
// beside.
func TestAMigrationDirectoryRefusalIsBlockedWithNoClaim(t *testing.T) {
	t.Parallel()
	const said = "desired_source_unreadable: migration file the Ptah format does not read: seed.sql"
	condition := func(conditionType string, status metav1.ConditionStatus, reason ptahv1alpha1.ConditionReason) metav1.Condition {
		return metav1.Condition{Type: conditionType, Status: status, Reason: string(reason), Message: said}
	}
	refused := func() *ptahv1alpha1.PtahMigration {
		return &ptahv1alpha1.PtahMigration{Status: ptahv1alpha1.PtahMigrationStatus{
			Phase: ptahv1alpha1.MigrationPhaseBlocked,
			Conditions: []metav1.Condition{condition(ptahv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue,
				ptahv1alpha1.ReasonMigrationDirectoryUnreadable)},
		}}
	}
	if message, err := migrationDirectoryRefused(refused()); err != nil || message != said {
		t.Fatalf("migrationDirectoryRefused() = %q, %v", message, err)
	}
	for name, edit := range map[string]func(*ptahv1alpha1.PtahMigration){
		"the retry it replaced": func(m *ptahv1alpha1.PtahMigration) {
			m.Status.Phase = ptahv1alpha1.MigrationPhaseReading
			m.Status.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{ID: "second", Attempt: 2}
			m.Status.Conditions = []metav1.Condition{condition(ptahv1alpha1.ConditionMigrationProgressing, metav1.ConditionTrue,
				ptahv1alpha1.ReasonOperationFailed)}
		},
		"another reason": func(m *ptahv1alpha1.PtahMigration) {
			m.Status.Conditions[0].Reason = string(ptahv1alpha1.ReasonRepeatableMigrationsUnsupported)
		},
		"Blocked false": func(m *ptahv1alpha1.PtahMigration) { m.Status.Conditions[0].Status = metav1.ConditionFalse },
		"another phase": func(m *ptahv1alpha1.PtahMigration) { m.Status.Phase = ptahv1alpha1.MigrationPhaseReading },
		"a claim beside": func(m *ptahv1alpha1.PtahMigration) {
			m.Status.ActiveOperation = &ptahv1alpha1.MigrationOperationStatus{ID: "next"}
		},
		"no condition": func(m *ptahv1alpha1.PtahMigration) { m.Status.Conditions = nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			migration := refused()
			edit(migration)
			if message, err := migrationDirectoryRefused(migration); err == nil {
				t.Fatalf("accepted it: %q", message)
			}
		})
	}
}

// The stray-file row waits for the file and the words the pinned image gave
// for the same directory, and refuses the reason of another file.
func TestUnreadableSourceSaysHoldsTheStrayFile(t *testing.T) {
	t.Parallel()
	said := runner.CodeDesiredSourceUnreadable + ": " + capturedReason(t, "scratch-unread.json")
	if err := unreadableSourceSays(said, strayMigrationFile, strayMigrationReason); err != nil {
		t.Fatalf("the capture: %v", err)
	}
	directive := runner.CodeDesiredSourceUnreadable + ": " + capturedReason(t, "scratch-directive.json")
	if err := unreadableSourceSays(directive, strayMigrationFile, strayMigrationReason); err == nil {
		t.Fatalf("unreadableSourceSays(%q) accepted the reason of another file", directive)
	}
}

// fetchRefusalSaysOnly takes the step and its exit code, and refuses a message
// that carries what the step printed, names another step, or reports a refused
// access.
func TestFetchRefusalSaysOnlyTheStepAndItsExitCode(t *testing.T) {
	t.Parallel()
	status := func(message string) ptahv1alpha1.PtahMigrationStatus {
		return ptahv1alpha1.PtahMigrationStatus{Phase: ptahv1alpha1.MigrationPhaseReading, Conditions: []metav1.Condition{{
			Type: ptahv1alpha1.ConditionMigrationProgressing, Status: metav1.ConditionTrue,
			Reason: string(ptahv1alpha1.ReasonOperationFailed), Message: message,
		}}}
	}
	const said = "the fetch-migrations step exited 2, so the run never started"
	if err := fetchRefusalSaysOnly(status(said), migrationFetchStep); err != nil {
		t.Fatalf("fetchRefusalSaysOnly(%q) = %v", said, err)
	}
	for name, message := range map[string]string{
		"the step's words":    said + ": error: " + noMigrationFilesWords,
		"the words alone":     "error: " + noMigrationFilesWords,
		"another step":        "the fetch-schema step exited 2, so the run never started",
		"a refused access":    "the fetch-migrations step refused the source's access, so the run never started: access_denied: the grant names another host",
		"an exit of zero":     "the fetch-migrations step exited 0, so the run never started",
		"a sentence before":   "History failed: " + said,
		"the Job's own words": "the History Job failed",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := fetchRefusalSaysOnly(status(message), migrationFetchStep); err == nil {
				t.Fatalf("fetchRefusalSaysOnly(%q) accepted it", message)
			}
		})
	}
	if err := fetchRefusalSaysOnly(ptahv1alpha1.PtahMigrationStatus{}, migrationFetchStep); err == nil {
		t.Fatal("fetchRefusalSaysOnly accepted a status with no Progressing condition")
	}
}

// A repair the row counts is one the source carried: a plan from another source,
// with the failure gone, under the spec the failure was reported on. The last
// rows are the two a suspension or a spec edit would pass.
func TestARepairIsReadFromTheSourceAlone(t *testing.T) {
	t.Parallel()
	const failedSource = "sha256:failed"
	base := func() *ptahv1alpha1.PtahSchema {
		return &ptahv1alpha1.PtahSchema{
			ObjectMeta: metav1.ObjectMeta{Generation: 3},
			Status: ptahv1alpha1.PtahSchemaStatus{
				Plan:       &ptahv1alpha1.CurrentPlanStatus{Name: "plan"},
				Source:     ptahv1alpha1.SchemaSourceStatus{Digest: "sha256:repaired"},
				Conditions: []metav1.Condition{{Type: ptahv1alpha1.ConditionReconciliationFailed, Status: metav1.ConditionFalse}},
			},
		}
	}
	if err := schemaReadTheRepair(base(), 3, failedSource); err != nil {
		t.Fatalf("refused a repaired read: %v", err)
	}
	for name, edit := range map[string]func(*ptahv1alpha1.PtahSchema){
		"still failing": func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions[0].Status = metav1.ConditionTrue },
		"no condition":  func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions = nil },
		"no plan":       func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan = nil },
		"same source":   func(s *ptahv1alpha1.PtahSchema) { s.Status.Source.Digest = failedSource },
		"no source":     func(s *ptahv1alpha1.PtahSchema) { s.Status.Source = ptahv1alpha1.SchemaSourceStatus{} },
		"a spec edit":   func(s *ptahv1alpha1.PtahSchema) { s.Generation = 4 },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			schema := base()
			edit(schema)
			if err := schemaReadTheRepair(schema, 3, failedSource); err == nil {
				t.Fatal("accepted a reading that is no repair")
			}
		})
	}
	for name, source := range map[string]ptahv1alpha1.SchemaSourceStatus{
		"a snapshot": {Snapshot: &ptahv1alpha1.SourceSnapshotBinding{ContentDigest: failedSource}},
		"a commit":   {Git: &ptahv1alpha1.GitSourceRevision{Commit: failedSource}},
	} {
		schema := base()
		schema.Status.Source = source
		if err := schemaReadTheRepair(schema, 3, failedSource); err == nil {
			t.Errorf("%s: accepted the source that failed", name)
		}
	}
}

func TestAMigrationRepairIsReadFromTheSourceAlone(t *testing.T) {
	t.Parallel()
	const failedSource = "sha256:failed"
	base := func() *ptahv1alpha1.PtahMigration {
		return &ptahv1alpha1.PtahMigration{
			ObjectMeta: metav1.ObjectMeta{Generation: 3},
			Status: ptahv1alpha1.PtahMigrationStatus{
				Plan: &ptahv1alpha1.ImmutableObjectReference{Name: "plan"},
				Artifact: &ptahv1alpha1.SourceAccessBinding{
					Snapshot: &ptahv1alpha1.SourceSnapshotBinding{ContentDigest: "sha256:repaired"},
				},
			},
		}
	}
	if err := migrationReadTheRepair(base(), 3, failedSource); err != nil {
		t.Fatalf("refused a repaired read: %v", err)
	}
	for name, edit := range map[string]func(*ptahv1alpha1.PtahMigration){
		"still failing": func(m *ptahv1alpha1.PtahMigration) {
			m.Status.Conditions = []metav1.Condition{{
				Type: ptahv1alpha1.ConditionMigrationProgressing, Reason: string(ptahv1alpha1.ReasonOperationFailed),
			}}
		},
		"no plan":     func(m *ptahv1alpha1.PtahMigration) { m.Status.Plan = nil },
		"no artifact": func(m *ptahv1alpha1.PtahMigration) { m.Status.Artifact = nil },
		"same source": func(m *ptahv1alpha1.PtahMigration) { m.Status.Artifact.Snapshot.ContentDigest = failedSource },
		"a spec edit": func(m *ptahv1alpha1.PtahMigration) { m.Generation = 4 },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			migration := base()
			edit(migration)
			if err := migrationReadTheRepair(migration, 3, failedSource); err == nil {
				t.Fatal("accepted a reading that is no repair")
			}
		})
	}
}

// TestTheUnreadableFilesAreTheOnesThePinnedImageRefused keeps the row's input
// tied to what was measured: the delivered schema is the file the capture read,
// so the reason the row waits for is the reason that file gives.
func TestTheUnreadableFilesAreTheOnesThePinnedImageRefused(t *testing.T) {
	t.Parallel()

	if !strings.Contains(capturedReason(t, "validate-sql.txt"), unreadableInheritsReason) {
		t.Errorf("the capture does not say %q", unreadableInheritsReason)
	}
	if !strings.Contains(capturedReason(t, "scratch-directive.json"), unreadableMigrationReason) {
		t.Errorf("the capture does not say %q", unreadableMigrationReason)
	}
	measured, err := os.ReadFile(filepath.Join("..", "runnerharness", "testdata", "sources", "desired-source", unreadableSchemaFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(measured) != unreadableInheritsSchema {
		t.Errorf("the row delivers %q, and the capture read %q", unreadableInheritsSchema, measured)
	}
	up := unreadableMigrationFiles[0]
	if up.path != unreadableMigrationFile || !strings.Contains(up.body, "no_transaction=sometimes") {
		t.Errorf("the row's up migration is %+v", up)
	}
}

// capturedFrameError is the error a runner harness row's result frame holds:
// what the runner said in the operation container, for the same fixture a row
// delivers to the cluster.
func capturedFrameError(t *testing.T, row string) runner.ResultError {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "dataplane", "testdata", "runner-harness", "transcript.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Name   string `json:"name"`
		Result struct {
			Error *runner.ResultError `json:"error"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	for _, captured := range rows {
		if captured.Name != row {
			continue
		}
		if captured.Result.Error == nil {
			t.Fatalf("the harness row %s holds no error", row)
		}
		return *captured.Result.Error
	}
	t.Fatalf("no harness row %s among %d: run `make runner-harness-write`", row, len(rows))
	return runner.ResultError{}
}

// TestTheUnparsableArtifactIsReadInTheOperationContainer holds the OCI row's
// checks to what the runner said of the artifact hack/unparsableschemafixture
// publishes, and refuses what the cluster said while the fetch parsed it: the
// step by name and nothing more.
func TestTheUnparsableArtifactIsReadInTheOperationContainer(t *testing.T) {
	t.Parallel()

	const owner = types.UID("schema-uid")
	stopped := "the " + schemaFetchStep + " step exited 2, so the run never started"
	for _, row := range []string{"boundary-unparsable-oci/observe", "boundary-unparsable-oci/plan"} {
		failure := capturedFrameError(t, row)
		if failure.Code != runner.CodeDesiredSourceUnreadable {
			t.Fatalf("%s: the runner said %s, and the row waits for %s", row, failure.Code, runner.CodeDesiredSourceUnreadable)
		}
		said := failure.Code + ": " + failure.Message
		if err := unreadableSourceSays(said, unreadableArtifactFile, unreadableArtifactWords); err != nil {
			t.Errorf("%s: %v", row, err)
		}
		reason := string(ptahv1alpha1.ReasonSchemaSourceUnreadable)
		event := corev1.Event{
			InvolvedObject: corev1.ObjectReference{UID: owner}, Type: corev1.EventTypeWarning, Reason: reason, Message: said,
		}
		if err := unreadableSourceEvent([]corev1.Event{event}, owner, reason, unreadableArtifactFile, unreadableArtifactWords); err != nil {
			t.Errorf("%s, the Event: %v", row, err)
		}
	}
	for name, message := range map[string]string{
		"the fetch step that stopped the run": stopped,
		"the step and the code":               runner.CodeDesiredSourceUnreadable + ": " + stopped,
		// What `ptah schema pull` printed in the fetch step's log on Ptah v0.22.0.
		"the fetch step's words": runner.CodeDesiredSourceUnreadable + ": parse schema artifact: parse HCL schema: schema.hcl:3,31-32: " +
			"Invalid single-argument block definition",
		"another file": runner.CodeDesiredSourceUnreadable + ": error parsing schema file: parse HCL schema: /source/schema/schema.hcl:3,31-32: " +
			"Invalid single-argument block definition",
		"a SQL file": runner.CodeDesiredSourceUnreadable + ": error parsing schema file: read SQL schema file " + unreadableArtifactFile +
			":2:29: " + unreadableInheritsReason,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := unreadableSourceSays(message, unreadableArtifactFile, unreadableArtifactWords); err == nil {
				t.Fatalf("unreadableSourceSays(%q) accepted it", message)
			}
		})
	}
}

// TestFetchPassedTheLayerOnRefusesAFetchThatStopped accepts a Pod whose fetch
// left the layer to the ptah container, and refuses the Pod a parsing fetch
// left behind: kind printed it as Init:Error, the step terminated at exit 2.
func TestFetchPassedTheLayerOnRefusesAFetchThatStopped(t *testing.T) {
	t.Parallel()
	terminated := func(code int32) corev1.ContainerState {
		return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code}}
	}
	waiting := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	passed := func() *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "observe-pod"},
			Status: corev1.PodStatus{
				Phase: corev1.PodSucceeded,
				InitContainerStatuses: []corev1.ContainerStatus{
					{Name: "install-runner", State: terminated(0)},
					{Name: "validate-source-authority", State: terminated(0)},
					{Name: schemaFetchStep, State: terminated(0)},
				},
				ContainerStatuses: []corev1.ContainerStatus{{Name: "ptah", State: terminated(0)}},
			},
		}
	}
	if err := fetchPassedTheLayerOn(passed()); err != nil {
		t.Fatalf("fetchPassedTheLayerOn() = %v", err)
	}
	for name, test := range map[string]struct {
		edit func(*corev1.Pod)
		want string
	}{
		"stopped at the fetch step": {func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodFailed
			p.Status.InitContainerStatuses[2].State = terminated(2)
			p.Status.ContainerStatuses[0].State = waiting
		}, "exited 2"},
		"with no fetch step": {func(p *corev1.Pod) {
			p.Status.InitContainerStatuses = p.Status.InitContainerStatuses[:2]
		}, "has no " + schemaFetchStep},
		"still fetching": {func(p *corev1.Pod) {
			p.Status.InitContainerStatuses[2].State = running
			p.Status.ContainerStatuses[0].State = waiting
		}, "has not ended"},
		"whose ptah container never started": {func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State = waiting
		}, "has not run to its end"},
		"whose ptah container is running": {func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State = running
		}, "has not run to its end"},
		"with no ptah container": {func(p *corev1.Pod) {
			p.Status.ContainerStatuses = nil
		}, "has not run to its end"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pod := passed()
			test.edit(pod)
			err := fetchPassedTheLayerOn(pod)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("fetchPassedTheLayerOn() = %v, want %q", err, test.want)
			}
		})
	}
}
