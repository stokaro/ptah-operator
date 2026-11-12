package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/historyview"
	"github.com/stokaro/ptah-operator/internal/lifecyclejournal"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata/history from what kubectl ptah history prints now")

const historyGoldenDirectory = "testdata/history"

// historyCase is one resource the fake API server serves, the command line
// that reads its history, and what the history has to say about it.
type historyCase struct {
	name      string
	arguments []string
	objects   func(t *testing.T) []client.Object
	forbidden []string
	// resource is the object the history is of, among objects.
	resource func(objects []client.Object) client.Object
	// says are lines the text has to carry beside the golden file, each a
	// claim the case exists for, so a golden file rewritten by -update cannot
	// quietly stop making it.
	says []string
	// saysOf are more such claims, derived from the objects where they come
	// from a reading the e2e suite recaptures, so a recapture moves them with
	// the golden file rather than failing after -update.
	saysOf func(objects []client.Object) []string
}

func historyCases() []historyCase {
	return []historyCase{
		{
			// The journal the Ptah Console lab read: three plans applied, each
			// published a second time by the write after its Apply. The history
			// prints all fifteen entries as recorded, the repeats included: a
			// producer defect is the producer's to fix, not this reader's to hide.
			name:      "schema-applied-three-plans",
			arguments: []string{"history", "schema/storefront"},
			objects:   storefrontWithRetainedDecisions,
			resource:  named("PtahSchema", "storefront"),
			says: []string{
				"Journal:              15 entries, complete since 2026-10-24T18:16:13Z\n",
				"2026-10-24T18:16:55Z  PlanPublished\n" +
					"  Plan:               ptah-plan-a0bc2ff3689d678799b4e26e (no longer stored)\n" +
					"  Statements:         3\n" +
					"2026-10-24T18:17:03Z  ApplyFinished, Succeeded\n" +
					"  Started:            2026-10-24T18:16:55Z, 8s before it ended\n",
				"  Approval:           ptah-console-c66044cb26a71093020a, UID not recorded: a release before 0.2.10 " +
					"wrote this entry, so it is not joined to an approval that holds the name today\n",
				"  Plan:               ptah-plan-7bc832764f0f9c263509220f (stored)\n",
				"\nApprovals, 6 in all, oldest first:\n",
			},
		},
		{
			// The data plane's login-visibility schema, read by somebody who may
			// get it and list nothing else: the journal is all there, and each
			// missing enrichment is one line that says why.
			name:      "schema-lists-forbidden",
			arguments: []string{"history", "ptahschema/e2e-login-visibility-postgresql"},
			objects:   loginVisibilitySchema,
			forbidden: []string{"ptahschemaplans", "ptahschemaapprovals"},
			resource:  named("PtahSchema", "e2e-login-visibility-postgresql"),
			says: []string{
				"Whether each plan is still stored is not shown: listing ptahschemaplans is\nforbidden to you (RBAC):",
				"\nApprovals: not shown; listing ptahschemaapprovals is forbidden to you (RBAC):",
			},
			saysOf: func(objects []client.Object) []string {
				journal := journalOf(objects[0])
				return []string{
					journalLine(journal),
					// No mark: the list that would say whether the plan is stored was refused.
					"  Plan:               " + journal.Entries[0].Plan + "\n",
				}
			},
		},
		{
			// No release has captured a migration's run entries from a cluster
			// yet, so this one is made by the operator's own journal code from
			// the status writes a run makes: a plan, an approval, a run and the
			// reading after it, a second run that committed part of a migration,
			// three failed history reads folded into one entry, and the
			// acknowledgment that settled the partial run.
			name:      "migration-partial-run-acknowledged",
			arguments: []string{"history", "migration/orders"},
			objects:   ordersAfterAPartialRun,
			resource:  named("PtahMigration", "orders"),
			says: []string{
				"2026-11-10T09:12:00Z  RunFinished, Partial\n",
				"  Job:                ptah-m-apply-orders-2, UID job-2-uid\n",
				"  Stopped at:         version 5, statement 2 of 3, SQLSTATE 23505\n",
				"2026-11-10T09:15:00Z  OperationFailed, History, DatabaseUnreachable\n" +
					"  Attempts:           3 in a row, the last at 2026-11-10T09:25:00Z\n",
				"  Approval:           orders-approval-2, UID approval-2-uid (no longer stored)\n",
				"  Settled:            by a person's statement that the run is accounted for, not by a reading of the database\n",
				"\nRun acknowledgments, 1 in all, oldest first:\n",
				"revision table, which kubectl ptah migration orders summarizes",
			},
		},
		{
			// A migration the data plane refused to read: the operator keeps a
			// journal and it has recorded nothing.
			name:      "migration-empty-journal",
			arguments: []string{"history", "migration/e2e-reachability-migration"},
			objects:   refusedMigration,
			resource:  named("PtahMigration", "e2e-reachability-migration"),
			saysOf: func(objects []client.Object) []string {
				return []string{journalLine(journalOf(objects[0]))}
			},
			says: []string{
				"\nApprovals: none\n",
				"\nRun acknowledgments: none\n",
			},
		},
		{
			// A status a manager before 0.2.7 last wrote: no journal at all, and
			// the decisions still listed.
			name:      "schema-without-journal",
			arguments: []string{"history", "schema/catalog"},
			objects:   catalogWithoutJournal,
			resource:  named("PtahSchema", "catalog"),
			says: []string{
				"Journal:              none\n",
				"The status holds no journal.",
				"\nApprovals, 1 in all, oldest first:\n",
			},
		},
	}
}

// The history command prints what the golden file holds, carries the claims
// each case is for, and prints nothing on stderr.
func TestHistoryPrintsTheRetainedJournalAndEveryDecision(t *testing.T) {
	t.Parallel()
	if *updateGolden {
		if err := os.MkdirAll(historyGoldenDirectory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range historyCases() {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			stdout := runHistory(t, test, "text")
			path := filepath.Join(historyGoldenDirectory, test.name+".txt")
			if *updateGolden {
				if err := os.WriteFile(path, []byte(stdout), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			golden, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read the golden history (run go test -update to write it): %v", err)
			}
			if stdout != string(golden) {
				t.Fatalf("kubectl ptah history printed a history %s does not hold:\n%s", path, stdout)
			}
			claims := test.says
			if test.saysOf != nil {
				claims = append(slices.Clone(claims), test.saysOf(test.objects(t))...)
			}
			for _, want := range claims {
				if !strings.Contains(stdout, want) {
					t.Fatalf("the history does not say %q:\n%s", want, stdout)
				}
			}
		})
	}
}

// A golden file that no case writes is a history nothing checks any longer.
func TestEveryHistoryGoldenFileHasACase(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join(historyGoldenDirectory, "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no golden history is stored, so the golden test compared nothing")
	}
	names := map[string]bool{}
	for _, test := range historyCases() {
		names[test.name+".txt"] = true
	}
	for _, file := range files {
		if !names[filepath.Base(file)] {
			t.Errorf("%s belongs to no history case", file)
		}
	}
}

// -o json carries every journal entry exactly as the status holds it, in the
// same order, the instant the journal is complete from, and every decision
// that names the resource by name and UID and none that names another.
func TestHistoryJSONCarriesEverythingTheStatusHolds(t *testing.T) {
	t.Parallel()
	for _, test := range historyCases() {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			objects := test.objects(t)
			resource := test.resource(objects)
			var view historyview.View
			decoder := json.NewDecoder(strings.NewReader(runHistory(t, test, "json")))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&view); err != nil {
				t.Fatalf("decode the history: %v", err)
			}

			if view.UID != string(resource.GetUID()) || view.Name != resource.GetName() ||
				!view.CreatedAt.Equal(ptr(resource.GetCreationTimestamp())) {
				t.Fatalf("the history is of %s %s created %s, want %s %s created %s",
					view.Name, view.UID, view.CreatedAt, resource.GetName(), resource.GetUID(), resource.GetCreationTimestamp())
			}
			journal := journalOf(resource)
			switch {
			case journal == nil && view.Journal != nil:
				t.Fatalf("the status holds no journal and the history carries one: %+v", view.Journal)
			case journal != nil && view.Journal == nil:
				t.Fatal("the status holds a journal and the history carries none")
			case journal != nil:
				if !view.Journal.CoveredSince.Equal(&journal.CoveredSince) {
					t.Fatalf("coveredSince = %s, the status holds %s", view.Journal.CoveredSince, journal.CoveredSince)
				}
				if view.Journal.Entries == nil {
					t.Fatal("entries is null; a journal with no entries carries an empty list")
				}
				carried := make([]operatorv1alpha1.JournalEntry, 0, len(view.Journal.Entries))
				for _, entry := range view.Journal.Entries {
					carried = append(carried, entry.JournalEntry)
				}
				stored := journal.Entries
				if stored == nil {
					stored = []operatorv1alpha1.JournalEntry{}
				}
				if !equality.Semantic.DeepEqual(carried, stored) {
					t.Fatalf("the history's entries differ from the status:\ncarried %+v\nstored  %+v", carried, stored)
				}
			}

			if !slices.Contains(test.forbidden, approvalsOf(resource)) {
				if view.Approvals == nil {
					t.Fatalf("the approvals could be listed and the history carries none: %+v", view.ApprovalsUnreadable)
				}
				// A script reads "listed, and there are none" as an empty list,
				// not as null, which reads as "not listed".
				if view.Approvals.Items == nil {
					t.Fatal("the approvals' items are null; a list that found none carries []")
				}
				if got, want := decisionUIDs(view.Approvals.Items), namingUIDs(objects, resource, approvalKind(resource)); !slices.Equal(got, want) {
					t.Fatalf("the history carries approvals %v, the cluster holds %v naming this %s", got, want, resource.GetUID())
				}
			}
			if _, migration := resource.(*operatorv1alpha1.PtahMigration); migration && view.Acknowledgments != nil {
				if view.Acknowledgments.Items == nil {
					t.Fatal("the acknowledgments' items are null; a list that found none carries []")
				}
				uids := make([]string, 0, len(view.Acknowledgments.Items))
				for _, acknowledgment := range view.Acknowledgments.Items {
					uids = append(uids, acknowledgment.UID)
				}
				if want := namingUIDs(objects, resource, "PtahMigrationRunAcknowledgment"); !slices.Equal(uids, want) {
					t.Fatalf("the history carries acknowledgments %v, the cluster holds %v", uids, want)
				}
			}
		})
	}
}

// A resource that is not there is exit 3, the status every read command
// gives for nothing to show, and a history of the wrong family is not read
// as the other one.
func TestHistoryOfAResourceThatIsNotThere(t *testing.T) {
	t.Parallel()
	server := newFakeAPIServer(t)
	kubeconfig := writeKubeconfig(t, server.URL)
	for _, arguments := range [][]string{
		{"history", "schema/orders"},
		{"history", "migration/storefront"},
	} {
		var stdout, stderr bytes.Buffer
		status := run(context.Background(), append(arguments, "-n", "application", "--kubeconfig", kubeconfig), &stdout, &stderr)
		if status != exitAbsent {
			t.Fatalf("run(%v) = %d, want %d\n%s", arguments, status, exitAbsent, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("run(%v) printed a history of nothing:\n%s", arguments, stdout.String())
		}
	}
}

func TestHistoryUsage(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		arguments []string
		says      string
	}{
		{[]string{"history"}, "name one resource, as schema/NAME or migration/NAME"},
		{[]string{"history", "storefront"}, `"storefront" does not name a resource`},
		{[]string{"history", "plan/storefront"}, `unknown resource family "plan"`},
		{[]string{"history", "schema/storefront", "-o", "yaml"}, `unknown output format "yaml"`},
		{[]string{"history", "schema/storefront", "--bogus"}, "unknown flag: --bogus"},
	} {
		var stdout, stderr bytes.Buffer
		if status := run(context.Background(), test.arguments, &stdout, &stderr); status != exitUsage {
			t.Fatalf("run(%v) = %d, want %d", test.arguments, status, exitUsage)
		}
		if !strings.Contains(stderr.String(), test.says) {
			t.Fatalf("run(%v) did not say %q:\n%s", test.arguments, test.says, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("run(%v) wrote to stdout while refusing the command line:\n%s", test.arguments, stdout.String())
		}
	}
}

func runHistory(t *testing.T, test historyCase, format string) string {
	t.Helper()
	server := newFakeAPIServerWith(t, test.objects(t), test.forbidden...)
	kubeconfig := writeKubeconfig(t, server.URL)
	arguments := append(append([]string(nil), test.arguments...), "-n", "application", "--kubeconfig", kubeconfig, "-o", format)
	var stdout, stderr bytes.Buffer
	if status := run(context.Background(), arguments, &stdout, &stderr); status != exitOK {
		t.Fatalf("run(%v) = %d, want %d\nstdout:\n%s\nstderr:\n%s", arguments, status, exitOK, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("run(%v) wrote to stderr:\n%s", arguments, stderr.String())
	}
	if refused := server.refused(); len(refused) != len(test.forbidden) {
		t.Fatalf("the server refused %v, want one refusal each of %v", refused, test.forbidden)
	}
	return stdout.String()
}

func named(kind, name string) func([]client.Object) client.Object {
	return func(objects []client.Object) client.Object {
		for _, object := range objects {
			if reflect.TypeOf(object).Elem().Name() == kind && object.GetName() == name {
				return object
			}
		}
		panic(fmt.Sprintf("no %s %s among the case's objects", kind, name))
	}
}

// journalLine is the header line a journal reads as.
func journalLine(journal *operatorv1alpha1.LifecycleJournal) string {
	count := "no entries"
	switch len(journal.Entries) {
	case 0:
	case 1:
		count = "1 entry"
	default:
		count = fmt.Sprintf("%d entries", len(journal.Entries))
	}
	return fmt.Sprintf("Journal:              %s, complete since %s\n", count, journal.CoveredSince.UTC().Format(time.RFC3339))
}

func journalOf(object client.Object) *operatorv1alpha1.LifecycleJournal {
	switch resource := object.(type) {
	case *operatorv1alpha1.PtahSchema:
		return resource.Status.Journal
	case *operatorv1alpha1.PtahMigration:
		return resource.Status.Journal
	}
	return nil
}

func approvalKind(object client.Object) string {
	if _, migration := object.(*operatorv1alpha1.PtahMigration); migration {
		return "PtahMigrationApproval"
	}
	return "PtahSchemaApproval"
}

func approvalsOf(object client.Object) string {
	if _, migration := object.(*operatorv1alpha1.PtahMigration); migration {
		return "ptahmigrationapprovals"
	}
	return "ptahschemaapprovals"
}

func decisionUIDs(approvals []historyview.Approval) []string {
	uids := make([]string, 0, len(approvals))
	for _, approval := range approvals {
		uids = append(uids, approval.UID)
	}
	return uids
}

// namingUIDs is the UIDs of every decision of kind among objects that names
// resource by name and UID, oldest first, worked out from the objects rather
// than from the view.
func namingUIDs(objects []client.Object, resource client.Object, kind string) []string {
	type decision struct {
		uid     string
		created metav1.Time
		name    string
	}
	var named []decision
	for _, object := range objects {
		var reference operatorv1alpha1.ImmutableObjectReference
		switch decisionObject := object.(type) {
		case *operatorv1alpha1.PtahSchemaApproval:
			reference = decisionObject.Spec.SchemaRef
		case *operatorv1alpha1.PtahMigrationApproval:
			reference = decisionObject.Spec.MigrationRef
		case *operatorv1alpha1.PtahMigrationRunAcknowledgment:
			reference = decisionObject.Spec.MigrationRef
		default:
			continue
		}
		if reflect.TypeOf(object).Elem().Name() != kind || reference.Name != resource.GetName() || reference.UID != resource.GetUID() {
			continue
		}
		named = append(named, decision{uid: string(object.GetUID()), created: object.GetCreationTimestamp(), name: object.GetName()})
	}
	slices.SortStableFunc(named, func(left, right decision) int {
		if order := left.created.Compare(right.created.Time); order != 0 {
			return order
		}
		return strings.Compare(left.name, right.name)
	})
	uids := make([]string, 0, len(named))
	for _, one := range named {
		uids = append(uids, one.uid)
	}
	return uids
}

func ptr[T any](value T) *T { return &value }

func at(value string) metav1.Time {
	instant, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return metav1.NewTime(instant)
}

func readReading(t *testing.T, name string, into any) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("..", "..", "testdata", "e2e", "readings", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, into); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
}

func schemaPlan(name string, owner client.Object) *operatorv1alpha1.PtahSchemaPlan {
	return &operatorv1alpha1.PtahSchemaPlan{ObjectMeta: metav1.ObjectMeta{
		Namespace: "application", Name: name, UID: types.UID(name + "-uid"),
		OwnerReferences: []metav1.OwnerReference{controlledBy("PtahSchema", owner)},
	}}
}

func controlledBy(kind string, owner client.Object) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: kind,
		Name: owner.GetName(), UID: owner.GetUID(), Controller: ptr(true), BlockOwnerDeletion: ptr(true),
	}
}

func schemaApproval(name, uid string, schema client.Object, created string, plan string) *operatorv1alpha1.PtahSchemaApproval {
	return &operatorv1alpha1.PtahSchemaApproval{
		ObjectMeta: metav1.ObjectMeta{Namespace: "application", Name: name, UID: types.UID(uid), CreationTimestamp: at(created)},
		Spec: operatorv1alpha1.PtahSchemaApprovalSpec{
			SchemaRef:       operatorv1alpha1.ImmutableObjectReference{Name: schema.GetName(), UID: schema.GetUID()},
			PlanRef:         operatorv1alpha1.ImmutableObjectReference{Name: plan, UID: types.UID(plan + "-uid")},
			PlanFingerprint: "sha256:" + strings.Repeat("f", 64),
			Approver:        operatorv1alpha1.ApprovalIdentity{Username: "e2e-approver"},
			ApprovedAt:      at(created),
		},
	}
}

// storefrontWithRetainedDecisions is the schema in
// testdata/e2e/readings/journal-plan-published-again.json, served in
// namespace application with the creation time the reading does not keep. It
// holds two plans of its four and has six approvals, more than a view lists;
// beside them are a plan and an approval of the same names that a schema of
// another UID owns, which this one's history must not count.
func storefrontWithRetainedDecisions(t *testing.T) []client.Object {
	schema := &operatorv1alpha1.PtahSchema{}
	readReading(t, "journal-plan-published-again.json", schema)
	schema.Namespace = "application"
	schema.CreationTimestamp = at("2026-10-24T18:16:10Z")
	predecessor := &operatorv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Name: "storefront", UID: "predecessor-uid"}}
	return []client.Object{
		schema,
		schemaPlan("ptah-plan-7bc832764f0f9c263509220f", schema),
		schemaPlan("ptah-plan-23dbe08e2f050a0db2d0f320", schema),
		schemaPlan("ptah-plan-a0bc2ff3689d678799b4e26e", predecessor),
		schemaApproval("storefront-first", "approval-1-uid", schema, "2026-10-24T18:16:58Z", "ptah-plan-a0bc2ff3689d678799b4e26e"),
		schemaApproval("ptah-console-035360308fa9fe290101", "approval-2-uid", schema, "2026-10-24T18:28:50Z", "ptah-plan-745692fb6a6c6b38e0d4d54b"),
		schemaApproval("storefront-third-a", "approval-3-uid", schema, "2026-10-24T18:30:20Z", "ptah-plan-23dbe08e2f050a0db2d0f320"),
		schemaApproval("storefront-third-b", "approval-4-uid", schema, "2026-10-24T18:30:40Z", "ptah-plan-23dbe08e2f050a0db2d0f320"),
		schemaApproval("storefront-third-c", "approval-5-uid", schema, "2026-10-24T18:31:00Z", "ptah-plan-23dbe08e2f050a0db2d0f320"),
		schemaApproval("ptah-console-c66044cb26a71093020a", "approval-6-uid", schema, "2026-10-24T18:31:20Z", "ptah-plan-7bc832764f0f9c263509220f"),
		schemaApproval("storefront-predecessor", "approval-0-uid", predecessor, "2026-10-24T18:00:00Z", "ptah-plan-a0bc2ff3689d678799b4e26e"),
	}
}

// loginVisibilitySchema is the status in
// testdata/e2e/readings/postgresql-login-visibility-drift.json, under the
// name, namespace, UID and creation time the reading does not keep.
func loginVisibilitySchema(t *testing.T) []client.Object {
	schema := &operatorv1alpha1.PtahSchema{}
	readReading(t, "postgresql-login-visibility-drift.json", schema)
	schema.ObjectMeta = metav1.ObjectMeta{
		Namespace: "application", Name: "e2e-login-visibility-postgresql", UID: "login-visibility-uid",
		CreationTimestamp: createdBefore(schema.Status.Journal), Generation: 1,
	}
	return []client.Object{schema}
}

// createdBefore dates a resource whose reading kept no creation time a few
// seconds before its journal began, which the first status write starts.
func createdBefore(journal *operatorv1alpha1.LifecycleJournal) metav1.Time {
	return metav1.NewTime(journal.CoveredSince.Add(-4 * time.Second))
}

// refusedMigration is the status in
// testdata/e2e/readings/configmap-refused-migration.json, under the name the
// row gave it and a UID and creation time the reading does not keep.
func refusedMigration(t *testing.T) []client.Object {
	migration := &operatorv1alpha1.PtahMigration{}
	readReading(t, "configmap-refused-migration.json", migration)
	migration.ObjectMeta = metav1.ObjectMeta{
		Namespace: "application", Name: "e2e-reachability-migration", UID: "reachability-uid",
		CreationTimestamp: createdBefore(migration.Status.Journal), Generation: 1,
	}
	return []client.Object{migration}
}

// catalogWithoutJournal is a schema whose status a manager before 0.2.7 last
// wrote: what it applied, and no journal.
func catalogWithoutJournal(*testing.T) []client.Object {
	schema := &operatorv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "application", Name: "catalog", UID: "catalog-uid",
			CreationTimestamp: at("2026-09-01T08:00:00Z"), Generation: 2,
		},
		Status: operatorv1alpha1.PtahSchemaStatus{
			ObservedGeneration: 2,
			Phase:              operatorv1alpha1.PhaseInSync,
			Applied: &operatorv1alpha1.AppliedStatus{
				PlanRef:         operatorv1alpha1.ImmutableObjectReference{Name: "ptah-plan-catalog-2", UID: "ptah-plan-catalog-2-uid"},
				PlanFingerprint: "sha256:" + strings.Repeat("c", 64),
				CompletedAt:     at("2026-09-02T10:00:00Z"),
			},
		},
	}
	approval := schemaApproval("catalog-v2", "catalog-approval-uid", schema, "2026-09-02T09:58:00Z", "ptah-plan-catalog-2")
	approval.Status.Conditions = []metav1.Condition{{
		Type: "Consumed", Status: metav1.ConditionTrue, Reason: "Applied", Message: "The approved plan was applied",
		LastTransitionTime: at("2026-09-02T10:00:00Z"),
	}}
	return []client.Object{schema, approval}
}

// ordersAfterAPartialRun is a migration whose journal the operator's own
// journal code wrote, write by write, from the status each step of its life
// left: lifecyclejournal.MigrationEntries and Record, which the controller
// calls in every status write. Its first plan was pruned, its second is
// stored; its second approval was deleted after the run that consumed it.
func ordersAfterAPartialRun(t *testing.T) []client.Object {
	migration := &operatorv1alpha1.PtahMigration{ObjectMeta: metav1.ObjectMeta{
		Namespace: "application", Name: "orders", UID: "orders-uid",
		CreationTimestamp: at("2026-11-10T08:59:55Z"), Generation: 1,
	}}
	plan1 := operatorv1alpha1.ImmutableObjectReference{Name: "ptah-mplan-orders-1", UID: "plan-1-uid"}
	plan2 := operatorv1alpha1.ImmutableObjectReference{Name: "ptah-mplan-orders-2", UID: "plan-2-uid"}
	approval1 := operatorv1alpha1.ImmutableObjectReference{Name: "orders-approval-1", UID: "approval-1-uid"}
	approval2 := operatorv1alpha1.ImmutableObjectReference{Name: "orders-approval-2", UID: "approval-2-uid"}
	operation1 := "sha256:" + strings.Repeat("1", 64)
	operation2 := "sha256:" + strings.Repeat("2", 64)
	fingerprint1 := "sha256:" + strings.Repeat("a", 64)
	fingerprint2 := "sha256:" + strings.Repeat("b", 64)
	alice := &operatorv1alpha1.ApprovalIdentity{Username: "alice@example.com"}
	bob := &operatorv1alpha1.ApprovalIdentity{Username: "bob@example.com"}
	carol := &operatorv1alpha1.ApprovalIdentity{Username: "carol@example.com"}

	apply := func(operation string, plan, approval operatorv1alpha1.ImmutableObjectReference, fingerprint string,
		approver *operatorv1alpha1.ApprovalIdentity, started string,
	) *operatorv1alpha1.MigrationOperationStatus {
		return &operatorv1alpha1.MigrationOperationStatus{
			Type: operatorv1alpha1.MigrationOperationApply, ID: operation, Attempt: 1, StartedAt: at(started),
			JobName: "ptah-m-apply-orders", PlanRef: ptr(plan), PlanFingerprint: fingerprint,
			ApprovalRef: ptr(approval), ApprovedBy: approver, ApprovedAt: ptr(at(started)),
		}
	}
	run := func(outcome operatorv1alpha1.MigrationRunOutcome, operation, job string, plan, approval operatorv1alpha1.ImmutableObjectReference,
		fingerprint string, approver *operatorv1alpha1.ApprovalIdentity, started, finished string, applied ...int64,
	) *operatorv1alpha1.MigrationRunStatus {
		return &operatorv1alpha1.MigrationRunStatus{
			Outcome: outcome, OperationID: operation, JobName: job, JobUID: types.UID("job-" + job[len(job)-1:] + "-uid"),
			StartedAt: at(started), FinishedAt: ptr(at(finished)), AppliedVersions: applied,
			PlanRef: ptr(plan), PlanFingerprint: fingerprint,
			Authorization: &operatorv1alpha1.MigrationRunAuthorizationStatus{
				Kind: operatorv1alpha1.MigrationRunAuthorizedByApproval, ApprovalRef: ptr(approval), Approver: approver,
				ApprovedAt: ptr(at(started)),
			},
		}
	}
	failedRead := func(attempt int32) func(*operatorv1alpha1.PtahMigrationStatus) {
		return func(status *operatorv1alpha1.PtahMigrationStatus) {
			status.Phase = operatorv1alpha1.MigrationPhaseFailed
			status.Conditions = []metav1.Condition{{
				Type: operatorv1alpha1.ConditionMigrationReady, Status: metav1.ConditionFalse, Reason: "DatabaseUnreachable",
			}}
			status.ActiveOperation = &operatorv1alpha1.MigrationOperationStatus{
				Type: operatorv1alpha1.MigrationOperationHistory, ID: "sha256:" + strings.Repeat("3", 64), Attempt: attempt,
			}
		}
	}

	status := operatorv1alpha1.PtahMigrationStatus{}
	for _, step := range []struct {
		at     string
		change func(*operatorv1alpha1.PtahMigrationStatus)
	}{
		{"2026-11-10T09:00:00Z", func(status *operatorv1alpha1.PtahMigrationStatus) {
			status.Phase = operatorv1alpha1.MigrationPhaseReading
		}},
		{"2026-11-10T09:01:00Z", func(status *operatorv1alpha1.PtahMigrationStatus) {
			status.Phase, status.Plan = operatorv1alpha1.MigrationPhaseAwaitingApproval, ptr(plan1)
		}},
		{"2026-11-10T09:03:00Z", func(status *operatorv1alpha1.PtahMigrationStatus) {
			status.Phase = operatorv1alpha1.MigrationPhaseApplying
			status.ActiveOperation = apply(operation1, plan1, approval1, fingerprint1, alice, "2026-11-10T09:03:00Z")
		}},
		{"2026-11-10T09:04:00Z", func(status *operatorv1alpha1.PtahMigrationStatus) {
			status.Phase, status.ActiveOperation, status.Plan = operatorv1alpha1.MigrationPhaseVerifyingHistory, nil, nil
			status.LastRun = run(operatorv1alpha1.MigrationRunOutcomeApplied, operation1, "ptah-m-apply-orders-1", plan1, approval1,
				fingerprint1, alice, "2026-11-10T09:03:00Z", "2026-11-10T09:04:00Z", 3, 4)
		}},
		{"2026-11-10T09:05:00Z", func(status *operatorv1alpha1.PtahMigrationStatus) {
			status.Phase = operatorv1alpha1.MigrationPhaseAwaitingApproval
			status.LastRun.Verification = &operatorv1alpha1.MigrationRunVerificationStatus{
				ObservedAt: at("2026-11-10T09:05:00Z"), CurrentVersion: 4, Result: operatorv1alpha1.MigrationRunVerifiedPending,
				HistoryFingerprint: "sha256:" + strings.Repeat("4", 64),
			}
			status.Plan = ptr(plan2)
		}},
		{"2026-11-10T09:11:00Z", func(status *operatorv1alpha1.PtahMigrationStatus) {
			status.Phase = operatorv1alpha1.MigrationPhaseApplying
			status.ActiveOperation = apply(operation2, plan2, approval2, fingerprint2, bob, "2026-11-10T09:11:00Z")
		}},
		{"2026-11-10T09:12:00Z", func(status *operatorv1alpha1.PtahMigrationStatus) {
			status.Phase, status.ActiveOperation, status.Plan = operatorv1alpha1.MigrationPhaseBlocked, nil, nil
			status.LastRun = run(operatorv1alpha1.MigrationRunOutcomePartial, operation2, "ptah-m-apply-orders-2", plan2, approval2,
				fingerprint2, bob, "2026-11-10T09:11:00Z", "2026-11-10T09:12:00Z", 5)
			status.LastRun.Failure = &operatorv1alpha1.MigrationRunFailureStatus{Version: 5, Statement: 2, Statements: 3, SQLState: "23505"}
			status.UnresolvedRun = &operatorv1alpha1.UnresolvedMigrationRunStatus{
				Outcome: operatorv1alpha1.MigrationRunOutcomePartial, OperationID: operation2,
				JobName: status.LastRun.JobName, JobUID: status.LastRun.JobUID, PlanRef: plan2, RecordedAt: at("2026-11-10T09:12:00Z"),
			}
		}},
		{"2026-11-10T09:15:00Z", failedRead(1)},
		{"2026-11-10T09:20:00Z", failedRead(2)},
		{"2026-11-10T09:25:00Z", failedRead(3)},
		{"2026-11-10T09:40:00Z", func(status *operatorv1alpha1.PtahMigrationStatus) {
			status.Phase, status.ActiveOperation, status.Conditions = operatorv1alpha1.MigrationPhaseInSync, nil, nil
			status.ResolvedRun = &operatorv1alpha1.ResolvedMigrationRunStatus{
				OperationID: operation2, Outcome: operatorv1alpha1.MigrationRunOutcomePartial,
				Resolution:        operatorv1alpha1.MigrationRunResolvedByAcknowledgment,
				AcknowledgmentRef: &operatorv1alpha1.ImmutableObjectReference{Name: "orders-run-2-accounted", UID: "acknowledgment-uid"},
				AcknowledgedBy:    carol, ResolvedAt: at("2026-11-10T09:40:00Z"),
			}
			status.UnresolvedRun = nil
		}},
	} {
		before := *status.DeepCopy()
		step.change(&status)
		now := at(step.at).Time
		status.Journal = lifecyclejournal.Record(before.Journal,
			lifecyclejournal.MigrationEntries(&before, &status, before.Journal, now), now)
	}
	migration.Status = status

	return []client.Object{
		migration,
		&operatorv1alpha1.PtahMigrationPlan{ObjectMeta: metav1.ObjectMeta{
			Namespace: "application", Name: plan2.Name, UID: plan2.UID,
			OwnerReferences: []metav1.OwnerReference{controlledBy("PtahMigration", migration)},
		}},
		&operatorv1alpha1.PtahMigrationApproval{
			ObjectMeta: metav1.ObjectMeta{Namespace: "application", Name: approval1.Name, UID: approval1.UID, CreationTimestamp: at("2026-11-10T09:02:30Z")},
			Spec: operatorv1alpha1.PtahMigrationApprovalSpec{
				MigrationRef: operatorv1alpha1.ImmutableObjectReference{Name: "orders", UID: migration.UID},
				PlanRef:      plan1, PlanFingerprint: fingerprint1, Approver: *alice, ApprovedAt: at("2026-11-10T09:02:30Z"),
			},
			Status: operatorv1alpha1.PtahMigrationApprovalStatus{Conditions: []metav1.Condition{{
				Type: "Consumed", Status: metav1.ConditionTrue, Reason: "Dispatched", Message: "The approved plan was dispatched",
				LastTransitionTime: at("2026-11-10T09:03:00Z"),
			}}},
		},
		&operatorv1alpha1.PtahMigrationRunAcknowledgment{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "application", Name: "orders-run-2-accounted", UID: "acknowledgment-uid",
				CreationTimestamp: at("2026-11-10T09:39:50Z"),
			},
			Spec: operatorv1alpha1.PtahMigrationRunAcknowledgmentSpec{
				MigrationRef: operatorv1alpha1.ImmutableObjectReference{Name: "orders", UID: migration.UID},
				OperationID:  operation2, AcknowledgedBy: *carol, AcknowledgedAt: at("2026-11-10T09:39:50Z"),
			},
			Status: operatorv1alpha1.PtahMigrationRunAcknowledgmentStatus{Conditions: []metav1.Condition{{
				Type: "Consumed", Status: metav1.ConditionTrue, Reason: "Acknowledged", Message: "The unresolved run was settled",
				LastTransitionTime: at("2026-11-10T09:40:00Z"),
			}}},
		},
		// A run acknowledgment of the same operation for an earlier
		// migration of the name: not this one's.
		&operatorv1alpha1.PtahMigrationRunAcknowledgment{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "application", Name: "orders-run-1-accounted", UID: "predecessor-acknowledgment-uid",
				CreationTimestamp: at("2026-11-01T09:00:00Z"),
			},
			Spec: operatorv1alpha1.PtahMigrationRunAcknowledgmentSpec{
				MigrationRef: operatorv1alpha1.ImmutableObjectReference{Name: "orders", UID: "predecessor-uid"},
				OperationID:  operation2, AcknowledgedBy: *carol, AcknowledgedAt: at("2026-11-01T09:00:00Z"),
			},
		},
	}
}
