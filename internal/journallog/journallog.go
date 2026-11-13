// Package journallog writes each journal entry a status write stored as one
// JSON line, on an output of the manager's own. A pipeline that collects the
// manager's output then keeps the journal after the resource, its namespace or
// the cluster is gone.
//
// A line is a copy of the record, not the record. It is written only after the
// API server stored the entry, and it never holds up the write: lines wait in a
// bounded queue for one goroutine that writes them, and a line that finds the
// queue full, or whose write fails, is dropped and counted in
// ptah_operator_journal_lines_dropped_total. So a line can be missing, and
// nothing in a line proves it came from the manager: whoever controls the
// manager's output can drop lines or write their own.
package journallog

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// LoggerName is what the logger field of every line holds, which tells these
// lines from the manager's other output when a pipeline merges the streams.
const LoggerName = "journal"

// DefaultCapacity is how many lines may wait for the writer. A status write
// adds a handful of entries, so the queue fills only while the output stops
// taking lines.
const DefaultCapacity = 1024

// Line is one line as it is written. Its keys are a contract with whoever reads
// the lines, and the audit page lists them.
type Line struct {
	// Logger is LoggerName.
	Logger string `json:"logger"`
	// Time is when the manager queued the line, right after the write that
	// stored the entry returned. Entry.At is when the event happened.
	Time time.Time `json:"time"`
	// Kind, Namespace, Name and UID are the resource whose journal holds the
	// entry. The UID tells the resource from a later one with the same name.
	Kind      string    `json:"kind"`
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	UID       types.UID `json:"uid"`
	// Entry is the entry as the API server stored it in status.journal.
	Entry operatorv1alpha1.JournalEntry `json:"entry"`
}

// Options configure a Log. The zero value is a Log with DefaultCapacity, the
// wall clock and a drop counter registered nowhere.
type Options struct {
	// Capacity is how many lines may wait for the writer.
	Capacity int
	// Registerer receives the drop counter when it is set.
	Registerer prometheus.Registerer
	// Clock dates the lines. Tests set it.
	Clock func() time.Time
}

// Log writes journal lines to one output. It is safe for concurrent use.
type Log struct {
	out     io.Writer
	lines   chan []byte
	dropped prometheus.Counter
	clock   func() time.Time

	mu      sync.RWMutex
	closed  bool
	stopped chan struct{}
}

// New starts the goroutine that writes to out and returns the Log that feeds
// it. Nothing else may write to out: a line is written with one call, and a
// second writer could split it.
func New(out io.Writer, options Options) *Log {
	if options.Capacity <= 0 {
		options.Capacity = DefaultCapacity
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	log := &Log{
		out:   out,
		lines: make(chan []byte, options.Capacity),
		dropped: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "ptah_operator",
			Name:      "journal_lines_dropped_total",
			Help:      "Journal lines the manager did not write: the queue was full, the write failed, or the manager was stopping.",
		}),
		clock:   options.Clock,
		stopped: make(chan struct{}),
	}
	if options.Registerer != nil {
		options.Registerer.MustRegister(log.dropped)
	}
	go log.write()
	return log
}

// Entries queues one line for each entry of the resource object, in order. It
// never blocks: a line that does not fit the queue is dropped and counted. The
// caller passes the entries as the API server returned them from the write that
// stored them, and only after that write succeeded.
func (l *Log) Entries(kind string, object metav1.Object, entries []operatorv1alpha1.JournalEntry) {
	if l == nil || len(entries) == 0 {
		return
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	queued := l.clock().UTC()
	for _, entry := range entries {
		if l.closed {
			l.dropped.Inc()
			continue
		}
		line, err := encode(Line{
			Logger: LoggerName, Time: queued, Kind: kind,
			Namespace: object.GetNamespace(), Name: object.GetName(), UID: object.GetUID(), Entry: entry,
		})
		if err != nil {
			l.dropped.Inc()
			continue
		}
		select {
		case l.lines <- line:
		default:
			l.dropped.Inc()
		}
	}
}

// Close stops taking lines and waits until the queued ones are written or ctx
// ends, whichever comes first. A line queued after Close is dropped and
// counted. The manager calls it once its controllers have stopped, so the
// last status writes still reach the output.
func (l *Log) Close(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		close(l.lines)
	}
	l.mu.Unlock()
	select {
	case <-l.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *Log) write() {
	defer close(l.stopped)
	for line := range l.lines {
		if _, err := l.out.Write(line); err != nil {
			l.dropped.Inc()
		}
	}
}

// encode is line as one line of JSON, ending in a newline. HTML characters in
// a username stay as they are, as status holds them.
func encode(line Line) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(line); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
