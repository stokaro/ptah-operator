package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/lifecyclejournal"
)

// JournalLog takes the journal entries a status write stored, so they reach an
// output beside status. internal/journallog writes them as JSON lines. It must
// not block, since a status write calls it and an Apply waits on the status
// write.
type JournalLog interface {
	Entries(kind string, object metav1.Object, entries []operatorv1alpha1.JournalEntry)
}

// logStoredEntries hands log the entries a status write stored. stored is the
// object the API server returned from that write, and written is how many of
// its journal's newest entries the write wrote, as lifecyclejournal.Record
// counted them. It runs only after the write succeeded: a write that
// conflicted or failed stored nothing, and the pass that follows derives its
// entries again from what is stored. The entries come from stored rather than
// from what the write sent, so a line says what the API server kept.
func logStoredEntries(log JournalLog, kind string, stored metav1.Object, journal *operatorv1alpha1.LifecycleJournal, written int) {
	if log == nil {
		return
	}
	if entries := lifecyclejournal.Newest(journal, written); len(entries) > 0 {
		log.Entries(kind, stored, entries)
	}
}
