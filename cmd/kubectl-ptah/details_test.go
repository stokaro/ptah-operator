package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/migrationview"
	"github.com/stokaro/ptah-operator/internal/schemaview"
)

const detailsGoldenDirectory = "testdata/details"

// detailsCase is one resource read with --details, from a status the e2e
// suite captured.
type detailsCase struct {
	name    string
	reading string
	// document names the resource in a reading that holds one per engine.
	document string
	// schema is true for a PtahSchema, false for a PtahMigration.
	schema  bool
	suspend bool
	// says are lines the text has to carry beside the golden file, each a
	// claim the case exists for, compared as words since a lead is wrapped.
	says []string
}

func detailsCases() []detailsCase {
	return []detailsCase{
		{
			// A read claimed and running: its Job confirmed by UID.
			name: "schema-operation-claimed", reading: "configmap-recovered-schema.json", schema: true,
			says: []string{"Active operation:", "is confirmed: the claim records the Job's UID", "Pending proof: none"},
		},
		{
			// An Apply nobody can account for, OutcomeUnknown, and the reading
			// after it owed.
			name: "schema-apply-owes-proof", reading: "alert-schema-recovery-wait.json", schema: true,
			says: []string{
				"Pending proof: the reading that proves an Apply is owed", "Nobody can say how the Apply ended.",
				"Apply pod UIDs:",
			},
		},
		{
			// The same, suspended: suspension does not end the proof.
			name: "schema-suspended-owes-proof", reading: "alert-schema-recovery-wait.json", schema: true, suspend: true,
			says: []string{"Suspension does not end it: nothing else changes the database until it settles."},
		},
		{
			// A known failure: the source refused, and the claim that read it.
			// An ordinary Observe marks nothing before its create, so its Job
			// with no UID may exist.
			name: "schema-source-refused", reading: "configmap-refused-schema.json", schema: true,
			says: []string{"Active operation:", "is named, and no UID is recorded: whether it was created is not confirmed yet."},
		},
		{
			// A run nobody can account for, its claim retained.
			name: "migration-unresolved-claim", reading: "uncertain-migration-retained-claim.json", document: "postgresql",
			says: []string{"Unresolved run: Unknown", "A run nobody has accounted for is the unresolved run"},
		},
		{
			// A dirty revision table, its rows printed one by one.
			name: "migration-dirty-revisions", reading: "uncertain-migration-history-dirty.json", document: "postgresql",
			says: []string{"History read:", "Revisions:", "Dirty: true"},
		},
		{
			// A run that committed part of a migration.
			name: "migration-partial-run", reading: "partial-run-left-a-dirty-revision.json",
			says: []string{"Last run: Partial"},
		},
		{
			// An idle migration in sync, with its last run and no claim.
			name: "migration-in-sync", reading: "isolated-node-durable-migration.json",
			says: []string{"Active operation: none", "Last run:"},
		},
	}
}

// --details prints what the golden file holds and the claims each case is
// for, and -o json carries the status the reading holds, unchanged.
func TestDetailsPrintTheWholeStatus(t *testing.T) {
	t.Parallel()
	if *updateGolden {
		if err := os.MkdirAll(detailsGoldenDirectory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range detailsCases() {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			object := detailsObject(t, test)
			stdout := runDetails(t, test, object, "text")
			path := filepath.Join(detailsGoldenDirectory, test.name+".txt")
			if *updateGolden {
				if err := os.WriteFile(path, []byte(stdout), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			golden, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read the golden details (run go test -update to write it): %v", err)
			}
			if stdout != string(golden) {
				t.Fatalf("--details printed what %s does not hold:\n%s", path, stdout)
			}
			flat := strings.Join(strings.Fields(stdout), " ")
			for _, want := range test.says {
				if !strings.Contains(flat, strings.Join(strings.Fields(want), " ")) {
					t.Fatalf("--details does not say %q:\n%s", want, stdout)
				}
			}

			decoder := json.NewDecoder(strings.NewReader(runDetails(t, test, object, "json")))
			decoder.DisallowUnknownFields()
			switch stored := object.(type) {
			case *operatorv1alpha1.PtahSchema:
				var view schemaview.View
				if err := decoder.Decode(&view); err != nil {
					t.Fatal(err)
				}
				if view.Details == nil || !equality.Semantic.DeepEqual(view.Details.Status, stored.Status) {
					t.Fatalf("details.status differs from the reading's status")
				}
			case *operatorv1alpha1.PtahMigration:
				var view migrationview.View
				if err := decoder.Decode(&view); err != nil {
					t.Fatal(err)
				}
				if view.Details == nil || !equality.Semantic.DeepEqual(view.Details.Status, stored.Status) {
					t.Fatalf("details.status differs from the reading's status")
				}
			}
		})
	}
}

func TestEveryDetailsGoldenFileHasACase(t *testing.T) {
	t.Parallel()
	named := map[string]bool{}
	for _, test := range detailsCases() {
		named[test.name+".txt"] = true
	}
	entries, err := os.ReadDir(detailsGoldenDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(named) {
		t.Errorf("%s holds %d files and there are %d cases", detailsGoldenDirectory, len(entries), len(named))
	}
	for _, entry := range entries {
		if !named[entry.Name()] {
			t.Errorf("%s has no case", filepath.Join(detailsGoldenDirectory, entry.Name()))
		}
	}
}

// detailsObject is the reading's status under a name, UID and generation
// the reading does not keep.
func detailsObject(t *testing.T, test detailsCase) client.Object {
	t.Helper()
	meta := func(name string, observed int64) metav1.ObjectMeta {
		return metav1.ObjectMeta{Namespace: "application", Name: name, UID: "details-uid", Generation: max(observed, 1)}
	}
	if test.schema {
		schema := &operatorv1alpha1.PtahSchema{}
		readDetailsReading(t, test, schema)
		if equality.Semantic.DeepEqual(schema.Status, operatorv1alpha1.PtahSchemaStatus{}) {
			t.Fatalf("%s holds no PtahSchema status", test.reading)
		}
		schema.ObjectMeta = meta("storefront", schema.Status.ObservedGeneration)
		schema.Spec.Suspend = test.suspend
		return schema
	}
	migration := &operatorv1alpha1.PtahMigration{}
	readDetailsReading(t, test, migration)
	if equality.Semantic.DeepEqual(migration.Status, operatorv1alpha1.PtahMigrationStatus{}) {
		t.Fatalf("%s holds no PtahMigration status", test.reading)
	}
	migration.ObjectMeta = meta("orders", migration.Status.ObservedGeneration)
	migration.Spec.Suspend = test.suspend
	return migration
}

// readDetailsReading decodes one resource from a reading. A field the API
// has since dropped is ignored, as the API server prunes it from a stored
// object; a reading of another shape decodes to an empty status, which
// detailsObject refuses, since a case over it would compare nothing with
// nothing.
func readDetailsReading(t *testing.T, test detailsCase, into client.Object) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("..", "..", "testdata", "e2e", "readings", test.reading))
	if err != nil {
		t.Fatal(err)
	}
	if test.document != "" {
		var documents map[string]json.RawMessage
		if err := json.Unmarshal(content, &documents); err != nil {
			t.Fatalf("decode %s: %v", test.reading, err)
		}
		var ok bool
		if content, ok = documents[test.document]; !ok {
			t.Fatalf("%s holds no %q document", test.reading, test.document)
		}
	}
	if err := json.Unmarshal(content, into); err != nil {
		t.Fatalf("decode %s: %v", test.reading, err)
	}
}

func runDetails(t *testing.T, test detailsCase, object client.Object, format string) string {
	t.Helper()
	server := newFakeAPIServerWith(t, []client.Object{object})
	kubeconfig := writeKubeconfig(t, server.URL)
	command := "migration"
	if test.schema {
		command = "schema"
	}
	arguments := []string{command, object.GetName(), "--details", "-n", "application", "--kubeconfig", kubeconfig, "-o", format}
	var stdout, stderr bytes.Buffer
	if status := run(context.Background(), arguments, &stdout, &stderr); status != exitOK {
		t.Fatalf("run(%v) = %d, want %d\nstdout:\n%s\nstderr:\n%s", arguments, status, exitOK, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("run(%v) wrote to stderr:\n%s", arguments, stderr.String())
	}
	return stdout.String()
}
