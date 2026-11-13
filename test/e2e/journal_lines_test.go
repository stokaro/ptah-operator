package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/journallog"
)

// journalLinesReading is the schema journal read from a cluster, with the
// failure the second resource folded three times. The schema's journal holds
// the same entry more than once, which is what makes a count of lines per
// entry the claim rather than a set of them.
func journalLinesReading(t *testing.T) (schema, failing journalAudited) {
	t.Helper()
	body, err := os.ReadFile("../../testdata/e2e/readings/journal-plan-published-again.json")
	if err != nil {
		t.Fatal(err)
	}
	var stored ptahv1alpha1.PtahSchema
	if err := json.Unmarshal(body, &stored); err != nil {
		t.Fatal(err)
	}
	// The reading was taken without metadata.creationTimestamp; the journal
	// began covering when the resource first wrote status.
	stored.CreationTimestamp = stored.Status.Journal.CoveredSince
	schema = journalAudited{Kind: "PtahSchema", Object: stored.ObjectMeta, Journal: stored.Status.Journal,
		Declared: []declaredJournalEntries{{Type: ptahv1alpha1.JournalApplyFinished, Outcome: ptahv1alpha1.JournalSucceeded, Count: 3}}}

	body, err = os.ReadFile("../../testdata/e2e/readings/configmap-recovered-schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var recovered ptahv1alpha1.PtahSchema
	if err := json.Unmarshal(body, &recovered); err != nil {
		t.Fatal(err)
	}
	recovered.ObjectMeta = metav1.ObjectMeta{Namespace: stored.Namespace, Name: "configmap-recovered", UID: "c0ffee00-0000-4000-8000-000000000001",
		CreationTimestamp: recovered.Status.Journal.CoveredSince}
	failing = journalAudited{Kind: "PtahSchema", Object: recovered.ObjectMeta, Journal: recovered.Status.Journal}
	return schema, failing
}

// foldedThrice is the journal's one failure as each of its three attempts
// left it: alone, then counted twice, then three times.
func foldedThrice(t *testing.T, journal *ptahv1alpha1.LifecycleJournal) []ptahv1alpha1.JournalEntry {
	t.Helper()
	if len(journal.Entries) != 1 || journal.Entries[0].Type != ptahv1alpha1.JournalOperationFailed {
		t.Fatalf("the failing reading holds %#v, want one failure", journal.Entries)
	}
	first := journal.Entries[0]
	second, third := *first.DeepCopy(), *first.DeepCopy()
	second.Count, second.LastAt = 2, &metav1.Time{Time: first.At.Add(10 * time.Second)}
	third.Count, third.LastAt = 3, &metav1.Time{Time: first.At.Add(20 * time.Second)}
	journal.Entries[0] = third
	return []ptahv1alpha1.JournalEntry{first, second, third}
}

// managerLogOf is a manager container's log as the kubelet serves it with
// timestamps: a diagnostic line first, then the journal lines the manager's
// own writer encodes for each batch, each after a diagnostic line.
func managerLogOf(t *testing.T, start time.Time, batches ...journalBatch) []byte {
	t.Helper()
	var encoded bytes.Buffer
	log := journallog.New(&encoded, journallog.Options{Clock: func() time.Time { return start }})
	for _, batch := range batches {
		log.Entries(batch.kind, &batch.object, batch.entries)
	}
	if err := log.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	at := start
	record := func(text string) {
		at = at.Add(time.Millisecond)
		fmt.Fprintf(&out, "%s %s\n", at.Format(time.RFC3339Nano), text)
	}
	record(`{"level":"info","ts":"2026-10-24T18:16:00Z","logger":"setup","msg":"starting manager"}`)
	for _, line := range strings.SplitAfter(encoded.String(), "\n") {
		if line == "" {
			continue
		}
		record(`{"level":"info","ts":"2026-10-24T18:16:00Z","logger":"controller","msg":"reconciled"}`)
		record(strings.TrimSuffix(line, "\n"))
	}
	return out.Bytes()
}

type journalBatch struct {
	kind    string
	object  metav1.ObjectMeta
	entries []ptahv1alpha1.JournalEntry
}

func readLogs(t *testing.T, raw map[string][]byte) []managerJournalLog {
	t.Helper()
	var logs []managerJournalLog
	for _, pod := range []string{"manager-a", "manager-b"} {
		if raw[pod] == nil {
			continue
		}
		log, err := readManagerJournalLog(pod, raw[pod])
		if err != nil {
			t.Fatal(err)
		}
		logs = append(logs, log)
	}
	return logs
}

func TestJournalLinesHoldOneLinePerStoredEntry(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 10, 24, 18, 0, 0, 0, time.UTC)
	baseline := func(t *testing.T) (resources []journalAudited, schemaLines []ptahv1alpha1.JournalEntry, attempts []ptahv1alpha1.JournalEntry) {
		schema, failing := journalLinesReading(t)
		attempts = foldedThrice(t, failing.Journal)
		return []journalAudited{schema, failing}, schema.Journal.Entries, attempts
	}
	// A later resource that reused the schema's name is not the schema.
	predecessor := func(schema journalAudited) journalBatch {
		object := schema.Object
		object.UID = "0ld00000-0000-4000-8000-000000000000"
		return journalBatch{"PtahSchema", object, schema.Journal.Entries[:1]}
	}

	for _, test := range []struct {
		name string
		// edit changes what the manager logged, or what the journals store.
		edit func(resources []journalAudited, schemaLines, attempts []ptahv1alpha1.JournalEntry) (
			[]journalAudited, map[string][]journalBatch, time.Time)
		refusal string
	}{
		{
			name: "the lines the manager writes, split over two Pods",
			edit: func(resources []journalAudited, schemaLines, attempts []ptahv1alpha1.JournalEntry) ([]journalAudited, map[string][]journalBatch, time.Time) {
				return resources, map[string][]journalBatch{
					"manager-a": {predecessor(resources[0]), {"PtahSchema", resources[0].Object, schemaLines[:6]}},
					"manager-b": {{"PtahSchema", resources[0].Object, schemaLines[6:]}, {"PtahSchema", resources[1].Object, attempts}},
				}, started
			},
		},
		{
			name: "a line missing",
			edit: func(resources []journalAudited, schemaLines, attempts []ptahv1alpha1.JournalEntry) ([]journalAudited, map[string][]journalBatch, time.Time) {
				return resources, map[string][]journalBatch{"manager-a": {
					{"PtahSchema", resources[0].Object, schemaLines[:len(schemaLines)-1]}, {"PtahSchema", resources[1].Object, attempts},
				}}, started
			},
			refusal: "0 lines for PtahSchema",
		},
		{
			// The reading stores this publication twice; three lines is one too many.
			name: "a line written twice",
			edit: func(resources []journalAudited, schemaLines, attempts []ptahv1alpha1.JournalEntry) ([]journalAudited, map[string][]journalBatch, time.Time) {
				return resources, map[string][]journalBatch{"manager-a": {
					{"PtahSchema", resources[0].Object, schemaLines}, {"PtahSchema", resources[0].Object, schemaLines[:1]},
					{"PtahSchema", resources[1].Object, attempts},
				}}, started
			},
			refusal: "3 lines for PtahSchema",
		},
		{
			name: "a line written by both Pods",
			edit: func(resources []journalAudited, schemaLines, attempts []ptahv1alpha1.JournalEntry) ([]journalAudited, map[string][]journalBatch, time.Time) {
				return resources, map[string][]journalBatch{
					"manager-a": {{"PtahSchema", resources[0].Object, schemaLines}, {"PtahSchema", resources[1].Object, attempts}},
					"manager-b": {{"PtahSchema", resources[0].Object, schemaLines[5:6]}},
				}, started
			},
			refusal: "2 lines for PtahSchema",
		},
		{
			name: "a line whose approver differs from the stored one",
			edit: func(resources []journalAudited, schemaLines, attempts []ptahv1alpha1.JournalEntry) ([]journalAudited, map[string][]journalBatch, time.Time) {
				changed := append([]ptahv1alpha1.JournalEntry(nil), schemaLines...)
				changed[5].Approver = "mallory"
				return resources, map[string][]journalBatch{"manager-a": {
					{"PtahSchema", resources[0].Object, changed}, {"PtahSchema", resources[1].Object, attempts},
				}}, started
			},
			refusal: "holds an entry the journal does not store",
		},
		{
			name: "a line under another resource's UID",
			edit: func(resources []journalAudited, schemaLines, attempts []ptahv1alpha1.JournalEntry) ([]journalAudited, map[string][]journalBatch, time.Time) {
				return resources, map[string][]journalBatch{"manager-a": {
					{"PtahSchema", resources[0].Object, schemaLines[1:]}, predecessor(resources[0]),
					{"PtahSchema", resources[1].Object, attempts},
				}}, started
			},
			// The reading stores this publication twice, and one line is left.
			refusal: "1 lines for PtahSchema",
		},
		{
			name: "a line naming the UID under another name",
			edit: func(resources []journalAudited, schemaLines, attempts []ptahv1alpha1.JournalEntry) ([]journalAudited, map[string][]journalBatch, time.Time) {
				renamed := resources[0].Object
				renamed.Name = "storefront-copy"
				return resources, map[string][]journalBatch{"manager-a": {
					{"PtahSchema", resources[0].Object, schemaLines}, {"PtahSchema", renamed, schemaLines[:1]},
					{"PtahSchema", resources[1].Object, attempts},
				}}, started
			},
			refusal: "storefront-copy",
		},
		{
			name: "a line for an entry no write stored",
			edit: func(resources []journalAudited, schemaLines, attempts []ptahv1alpha1.JournalEntry) ([]journalAudited, map[string][]journalBatch, time.Time) {
				invented := *schemaLines[0].DeepCopy()
				invented.Plan = "ptah-plan-nobody-published"
				return resources, map[string][]journalBatch{"manager-a": {
					{"PtahSchema", resources[0].Object, append(append([]ptahv1alpha1.JournalEntry(nil), schemaLines...), invented)},
					{"PtahSchema", resources[1].Object, attempts},
				}}, started
			},
			refusal: "ptah-plan-nobody-published",
		},
		{
			name: "the failure's final count never logged",
			edit: func(resources []journalAudited, schemaLines, attempts []ptahv1alpha1.JournalEntry) ([]journalAudited, map[string][]journalBatch, time.Time) {
				return resources, map[string][]journalBatch{"manager-a": {
					{"PtahSchema", resources[0].Object, schemaLines}, {"PtahSchema", resources[1].Object, attempts[:2]},
				}}, started
			},
			refusal: "0 lines for PtahSchema",
		},
		{
			name: "an earlier count logged twice",
			edit: func(resources []journalAudited, schemaLines, attempts []ptahv1alpha1.JournalEntry) ([]journalAudited, map[string][]journalBatch, time.Time) {
				return resources, map[string][]journalBatch{"manager-a": {
					{"PtahSchema", resources[0].Object, schemaLines}, {"PtahSchema", resources[1].Object, append(attempts, attempts[1])},
				}}, started
			},
			refusal: "give count 2",
		},
		{
			name: "an earlier count never logged",
			edit: func(resources []journalAudited, schemaLines, attempts []ptahv1alpha1.JournalEntry) ([]journalAudited, map[string][]journalBatch, time.Time) {
				return resources, map[string][]journalBatch{"manager-a": {
					{"PtahSchema", resources[0].Object, schemaLines}, {"PtahSchema", resources[1].Object, []ptahv1alpha1.JournalEntry{attempts[0], attempts[2]}},
				}}, started
			},
			refusal: "gives count 2",
		},
		{
			name: "only the final count logged",
			edit: func(resources []journalAudited, schemaLines, attempts []ptahv1alpha1.JournalEntry) ([]journalAudited, map[string][]journalBatch, time.Time) {
				return resources, map[string][]journalBatch{"manager-a": {
					{"PtahSchema", resources[0].Object, schemaLines}, {"PtahSchema", resources[1].Object, attempts[2:]},
				}}, started
			},
			refusal: "gives count 1",
		},
		{
			name: "a count past the stored one",
			edit: func(resources []journalAudited, schemaLines, attempts []ptahv1alpha1.JournalEntry) ([]journalAudited, map[string][]journalBatch, time.Time) {
				fourth := *attempts[2].DeepCopy()
				fourth.Count, fourth.LastAt = 4, &metav1.Time{Time: attempts[2].LastAt.Add(10 * time.Second)}
				return resources, map[string][]journalBatch{"manager-a": {
					{"PtahSchema", resources[0].Object, schemaLines}, {"PtahSchema", resources[1].Object, append(attempts, fourth)},
				}}, started
			},
			refusal: "holds an entry the journal does not store",
		},
		{
			name: "a journal with no entry",
			edit: func(resources []journalAudited, schemaLines, attempts []ptahv1alpha1.JournalEntry) ([]journalAudited, map[string][]journalBatch, time.Time) {
				resources[1].Journal = &ptahv1alpha1.LifecycleJournal{CoveredSince: resources[1].Journal.CoveredSince}
				return resources, map[string][]journalBatch{"manager-a": {{"PtahSchema", resources[0].Object, schemaLines}}}, started
			},
			refusal: "stores no journal entry",
		},
		{
			name: "fewer Applies than the phase made",
			edit: func(resources []journalAudited, schemaLines, attempts []ptahv1alpha1.JournalEntry) ([]journalAudited, map[string][]journalBatch, time.Time) {
				resources[0].Declared[0].Count = 4
				return resources, map[string][]journalBatch{"manager-a": {
					{"PtahSchema", resources[0].Object, schemaLines}, {"PtahSchema", resources[1].Object, attempts},
				}}, started
			},
			refusal: "stores 3 ApplyFinished entries",
		},
		{
			name: "a journal at its bound",
			edit: func(resources []journalAudited, schemaLines, attempts []ptahv1alpha1.JournalEntry) ([]journalAudited, map[string][]journalBatch, time.Time) {
				full := resources[0].Journal.DeepCopy()
				for len(full.Entries) < 64 {
					full.Entries = append(full.Entries, schemaLines[0])
				}
				resources[0].Journal = full
				return resources, map[string][]journalBatch{"manager-a": {
					{"PtahSchema", resources[0].Object, full.Entries}, {"PtahSchema", resources[1].Object, attempts},
				}}, started
			},
			refusal: "the bound",
		},
		{
			name: "a log that starts after the resource was created",
			edit: func(resources []journalAudited, schemaLines, attempts []ptahv1alpha1.JournalEntry) ([]journalAudited, map[string][]journalBatch, time.Time) {
				return resources, map[string][]journalBatch{"manager-a": {
					{"PtahSchema", resources[0].Object, schemaLines}, {"PtahSchema", resources[1].Object, attempts},
				}}, resources[0].Object.CreationTimestamp.Add(time.Second)
			},
			refusal: "rotated or the container restarted",
		},
		{
			name: "no manager log",
			edit: func(resources []journalAudited, schemaLines, attempts []ptahv1alpha1.JournalEntry) ([]journalAudited, map[string][]journalBatch, time.Time) {
				return resources, nil, started
			},
			refusal: "no manager log",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			resources, batches, start := test.edit(baseline(t))
			raw := map[string][]byte{}
			for pod, written := range batches {
				raw[pod] = managerLogOf(t, start, written...)
			}
			err := journalLinesHold(resources, readLogs(t, raw))
			switch {
			case test.refusal == "" && err != nil:
				t.Fatalf("journalLinesHold() = %v, want the lines admitted", err)
			case test.refusal != "" && (err == nil || !strings.Contains(err.Error(), test.refusal)):
				t.Fatalf("journalLinesHold() = %v, want a refusal naming %q", err, test.refusal)
			}
		})
	}
}

// The row reads every line the journal logger wrote, and a line that names
// that logger and is not a journal line is a fault, not a line to skip.
func TestReadManagerJournalLogRefusesAForeignJournalLine(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 24, 18, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	for _, test := range []struct {
		name, log, refusal string
		lines              int
	}{
		{name: "diagnostic lines only", log: at + ` {"level":"info","logger":"setup","msg":"starting manager"}` + "\n" + at + " plain text\n"},
		{name: "a journal line with a key the contract lacks",
			log:     at + ` {"logger":"journal","time":"2026-10-24T18:00:00Z","kind":"PtahSchema","namespace":"a","name":"b","uid":"c","entry":{"type":"Verified","at":"2026-10-24T18:00:00Z"},"actor":"x"}` + "\n",
			refusal: `unknown field "actor"`},
		{name: "a line with no kubelet timestamp", log: `{"logger":"journal"}` + "\n", refusal: "no kubelet timestamp"},
		{name: "an empty log", log: "", refusal: "is empty"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			log, err := readManagerJournalLog("manager-a", []byte(test.log))
			switch {
			case test.refusal == "" && (err != nil || log.Records != 2 || len(log.Lines) != 0):
				t.Fatalf("readManagerJournalLog() = %#v, %v, want two diagnostic records and no journal line", log, err)
			case test.refusal != "" && (err == nil || !strings.Contains(err.Error(), test.refusal)):
				t.Fatalf("readManagerJournalLog() = %v, want a refusal naming %q", err, test.refusal)
			}
		})
	}
}

func TestSameJournalsTellsAMovedJournal(t *testing.T) {
	t.Parallel()
	schema, failing := journalLinesReading(t)
	first := []journalAudited{schema, failing}
	moved := []journalAudited{schema, failing}
	moved[1].Journal = failing.Journal.DeepCopy()
	moved[1].Journal.Entries[0].Count = 2
	if !sameJournals(first, []journalAudited{schema, failing}) {
		t.Fatal("sameJournals refused two readings of the same journals")
	}
	if sameJournals(first, moved) {
		t.Fatal("sameJournals admitted a journal whose failure was counted again")
	}
	if sameJournals(first, first[:1]) {
		t.Fatal("sameJournals admitted a reading that lost a resource")
	}
}
