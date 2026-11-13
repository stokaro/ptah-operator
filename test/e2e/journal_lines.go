package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/journallog"
	"github.com/stokaro/ptah-operator/internal/lifecyclejournal"
)

// managerJournalLog is one manager container's log, read with the kubelet's
// timestamps.
type managerJournalLog struct {
	Pod string
	// First is the kubelet's timestamp on the first line. The log holds
	// nothing the container wrote before it: a rotated file or an earlier
	// container is gone.
	First time.Time
	// Records counts every line of the log, journal lines or not.
	Records int
	Lines   []journallog.Line
}

// readManagerJournalLog parses a manager container's log as the kubelet
// serves it with timestamps: each line is an RFC 3339 timestamp, a space, and
// what the container wrote. A line whose logger is journallog.LoggerName has
// to be a journal line and nothing more; every other line is the manager's
// diagnostic log, merged in by the kubelet, and is only counted.
func readManagerJournalLog(pod string, raw []byte) (managerJournalLog, error) {
	log := managerJournalLog{Pod: pod}
	for number, record := range bytes.Split(raw, []byte("\n")) {
		if len(record) == 0 {
			continue
		}
		stamp, text, found := bytes.Cut(record, []byte(" "))
		at, err := time.Parse(time.RFC3339Nano, string(stamp))
		if !found || err != nil {
			return log, fmt.Errorf("line %d of the log of %s carries no kubelet timestamp: %q", number+1, pod, record)
		}
		if log.Records == 0 {
			log.First = at
		}
		log.Records++
		var probe struct {
			Logger string `json:"logger"`
		}
		if json.Unmarshal(text, &probe) != nil || probe.Logger != journallog.LoggerName {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(text))
		decoder.DisallowUnknownFields()
		var line journallog.Line
		if err := decoder.Decode(&line); err != nil {
			return log, fmt.Errorf("line %d of the log of %s names the %s logger and is not a journal line: %v: %s",
				number+1, pod, journallog.LoggerName, err, text)
		}
		log.Lines = append(log.Lines, line)
	}
	if log.Records == 0 {
		return log, fmt.Errorf("the log of %s is empty", pod)
	}
	return log, nil
}

// journalAudited is a resource whose journal the manager's lines are held to.
type journalAudited struct {
	Kind    string
	Object  metav1.ObjectMeta
	Journal *ptahv1alpha1.LifecycleJournal
	// Declared is how many entries of each type and outcome the phase's own
	// actions put in the journal, so the row knows it read the entries it is
	// about rather than a journal that happened to be short.
	Declared []declaredJournalEntries
}

type declaredJournalEntries struct {
	Type    ptahv1alpha1.JournalEntryType
	Outcome ptahv1alpha1.JournalOutcome
	Count   int
}

// journalLogsCover is nil when every log reaches back past the creation of
// every resource. A log that starts later lost lines to rotation or to a
// restarted container, and no reading of it can tell those from lines the
// manager never wrote.
func journalLogsCover(logs []managerJournalLog, resources []journalAudited) error {
	if len(logs) == 0 {
		return errors.New("no manager log was read")
	}
	for _, resource := range resources {
		created := resource.Object.CreationTimestamp
		if created.IsZero() {
			return fmt.Errorf("%s %s/%s carries no creation time", resource.Kind, resource.Object.Namespace, resource.Object.Name)
		}
		for _, log := range logs {
			if log.First.After(created.Time) {
				return fmt.Errorf("the log of %s starts at %s, after %s %s/%s was created at %s: it rotated or the container restarted, "+
					"so lines for the resource may be gone", log.Pod, log.First.Format(time.RFC3339Nano), resource.Kind,
					resource.Object.Namespace, resource.Object.Name, created.Format(time.RFC3339))
			}
		}
	}
	return nil
}

// journalLinesHold is nil when the manager's logs hold exactly one line for
// each entry the journal of each resource stores, equal to it field by field,
// and no other line for that resource. A failure folded into one entry has
// exactly one line for each count it reached: a write derives at most one
// failure, so every count below the stored one was stored, and logged, by a
// write of its own. The journals must be read twice
// around the logs and found the same, so a line still on its way is not read
// as a missing one.
func journalLinesHold(resources []journalAudited, logs []managerJournalLog) error {
	if err := journalLogsCover(logs, resources); err != nil {
		return err
	}
	if len(resources) == 0 {
		return errors.New("no resource was named to hold the lines to")
	}
	var problems []string
	for _, resource := range resources {
		problems = append(problems, journalLinesOf(resource, logs)...)
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func journalLinesOf(resource journalAudited, logs []managerJournalLog) []string {
	who := fmt.Sprintf("%s %s/%s UID %s", resource.Kind, resource.Object.Namespace, resource.Object.Name, resource.Object.UID)
	journal := resource.Journal
	if journal == nil || len(journal.Entries) == 0 {
		return []string{who + " stores no journal entry"}
	}
	// Below the count bound nothing was ever dropped: once the bound is
	// reached the journal stays at it. A line for a dropped entry would match
	// nothing, and the row could not tell it from one nobody stored.
	if len(journal.Entries) >= lifecyclejournal.MaxEntries {
		return []string{fmt.Sprintf("%s stores %d entries, the bound: it may have dropped entries, and their lines match nothing", who, len(journal.Entries))}
	}
	var problems []string
	for _, declared := range resource.Declared {
		got := 0
		for _, entry := range journal.Entries {
			if entry.Type == declared.Type && entry.Outcome == declared.Outcome {
				got++
			}
		}
		if got != declared.Count {
			problems = append(problems, fmt.Sprintf("%s stores %d %s entries with outcome %q, and the phase made %d", who, got, declared.Type, declared.Outcome, declared.Count))
		}
	}

	var mine []journallog.Line
	for _, log := range logs {
		for _, line := range log.Lines {
			if line.UID != resource.Object.UID {
				continue
			}
			if line.Kind != resource.Kind || line.Namespace != resource.Object.Namespace || line.Name != resource.Object.Name {
				problems = append(problems, fmt.Sprintf("a line on the log of %s names UID %s as %s %s/%s, not as %s", log.Pod, line.UID, line.Kind, line.Namespace, line.Name, who))
				continue
			}
			mine = append(mine, line)
		}
	}

	stored := map[string]int{}
	folded := map[string]int32{}
	for _, entry := range journal.Entries {
		stored[journalEntryKey(entry)]++
		if entry.Count > 1 {
			folded[journalAttemptKey(entry)] = max(folded[journalAttemptKey(entry)], entry.Count)
		}
	}
	logged := map[string]int{}
	earlier := map[string]int{}
	for _, line := range mine {
		key := journalEntryKey(line.Entry)
		if stored[key] > 0 {
			logged[key]++
			continue
		}
		count := max(line.Entry.Count, 1)
		if reached, ok := folded[journalAttemptKey(line.Entry)]; !ok || count >= reached {
			problems = append(problems, fmt.Sprintf("a line for %s holds an entry the journal does not store: %s", who, key))
			continue
		}
		counted := fmt.Sprintf("%s#%d", journalAttemptKey(line.Entry), count)
		if earlier[counted]++; earlier[counted] == 2 {
			problems = append(problems, fmt.Sprintf("two lines for %s give count %d of the failure %s", who, count, journalAttemptKey(line.Entry)))
		}
	}
	for _, attempt := range slices.Sorted(maps.Keys(folded)) {
		for count := int32(1); count < folded[attempt]; count++ {
			if earlier[fmt.Sprintf("%s#%d", attempt, count)] == 0 {
				problems = append(problems, fmt.Sprintf("no line for %s gives count %d of the failure %s", who, count, attempt))
			}
		}
	}
	keys := make([]string, 0, len(stored))
	for key := range stored {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if logged[key] != stored[key] {
			problems = append(problems, fmt.Sprintf("%d lines for %s hold %s, which the journal stores %d times", logged[key], who, key, stored[key]))
		}
	}
	return problems
}

// journalEntryKey is entry as JSON, which is how a line and status both hold
// it.
func journalEntryKey(entry ptahv1alpha1.JournalEntry) string {
	encoded, err := json.Marshal(entry)
	if err != nil {
		return fmt.Sprintf("unencodable entry %#v: %v", entry, err)
	}
	return string(encoded)
}

// journalAttemptKey is entry without what a fold moves: the count and the
// time of the newest attempt.
func journalAttemptKey(entry ptahv1alpha1.JournalEntry) string {
	entry.Count, entry.LastAt = 0, nil
	return journalEntryKey(entry)
}

// sameJournals reports whether two readings of the resources hold the same
// journals.
func sameJournals(first, second []journalAudited) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		left, _ := json.Marshal(first[index].Journal)
		right, _ := json.Marshal(second[index].Journal)
		if first[index].Object.UID != second[index].Object.UID || !bytes.Equal(left, right) {
			return false
		}
	}
	return true
}
