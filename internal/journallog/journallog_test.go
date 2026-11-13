package journallog_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/journallog"
)

var queuedAt = time.Date(2026, 11, 13, 12, 0, 0, 123456789, time.UTC)

func clock() time.Time { return queuedAt }

var schema = &metav1.ObjectMeta{Namespace: "team-a", Name: "orders", UID: types.UID("schema-uid")}

func entry(plan string, offset time.Duration) operatorv1alpha1.JournalEntry {
	return operatorv1alpha1.JournalEntry{
		Type: operatorv1alpha1.JournalPlanPublished, At: metav1.NewTime(queuedAt.Truncate(time.Second).Add(offset)), Plan: plan, Statements: 2,
	}
}

// syncBuffer is an output the test reads while the writer goroutine writes.
type syncBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *syncBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *syncBuffer) lines(t *testing.T) []string {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var lines []string
	scanner := bufio.NewScanner(bytes.NewReader(b.buffer.Bytes()))
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if text := b.buffer.String(); text != "" && !strings.HasSuffix(text, "\n") {
		t.Fatalf("the output ends inside a line: %q", text)
	}
	return lines
}

// blockedOutput takes the first take lines and then no line until release is
// closed, and says when a write is waiting on it.
type blockedOutput struct {
	syncBuffer
	take    int
	waiting chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockedOutput() *blockedOutput {
	return &blockedOutput{waiting: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockedOutput) Write(data []byte) (int, error) {
	if b.take > 0 {
		b.take--
		return b.syncBuffer.Write(data)
	}
	b.once.Do(func() { close(b.waiting) })
	<-b.release
	return b.syncBuffer.Write(data)
}

func dropped(t *testing.T, registry *prometheus.Registry) float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "ptah_operator_journal_lines_dropped_total" {
			return family.GetMetric()[0].GetCounter().GetValue()
		}
	}
	t.Fatal("the drop counter is not registered")
	return 0
}

func closeLog(t *testing.T, log *journallog.Log) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := log.Close(ctx); err != nil {
		t.Fatalf("Close() = %v", err)
	}
}

// Each entry is one line holding the resource's identity and the entry as
// status stores it, under the keys the audit page names and no others.
func TestEachEntryIsOneLineWithTheResourceIdentity(t *testing.T) {
	t.Parallel()
	out := &syncBuffer{}
	log := journallog.New(out, journallog.Options{Clock: clock})
	approved := operatorv1alpha1.JournalEntry{
		Type: operatorv1alpha1.JournalApprovalAccepted, At: metav1.NewTime(queuedAt.Truncate(time.Second)),
		Plan: "plan-a", Approval: "approve-a", ApprovalUID: "approval-uid", Approver: "alice@example.test",
	}
	entries := []operatorv1alpha1.JournalEntry{entry("plan-a", -time.Minute), approved}
	log.Entries("PtahSchema", schema, entries)
	closeLog(t, log)

	lines := out.lines(t)
	if len(lines) != len(entries) {
		t.Fatalf("wrote %d lines for %d entries: %q", len(lines), len(entries), lines)
	}
	for index, text := range lines {
		var keys map[string]json.RawMessage
		if err := json.Unmarshal([]byte(text), &keys); err != nil {
			t.Fatalf("line %d is not JSON: %v: %q", index, err, text)
		}
		if got, want := slices.Sorted(mapKeys(keys)), []string{"entry", "kind", "logger", "name", "namespace", "time", "uid"}; !slices.Equal(got, want) {
			t.Fatalf("line %d holds keys %q, want %q", index, got, want)
		}
		var line journallog.Line
		if err := json.Unmarshal([]byte(text), &line); err != nil {
			t.Fatal(err)
		}
		want := journallog.Line{
			Logger: journallog.LoggerName, Time: queuedAt, Kind: "PtahSchema",
			Namespace: "team-a", Name: "orders", UID: "schema-uid", Entry: entries[index],
		}
		line.Entry.At = metav1.NewTime(line.Entry.At.UTC())
		if !reflect.DeepEqual(line, want) {
			t.Fatalf("line %d = %#v, want %#v", index, line, want)
		}
		stored, err := json.Marshal(entries[index])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(keys["entry"], stored) {
			t.Fatalf("line %d holds entry %s, want it as status stores it: %s", index, keys["entry"], stored)
		}
	}
	if !strings.Contains(lines[0], `"logger":"journal"`) {
		t.Fatalf("the line does not name its logger as the audit page says: %s", lines[0])
	}
}

func mapKeys(keys map[string]json.RawMessage) func(func(string) bool) {
	return func(yield func(string) bool) {
		for key := range keys {
			if !yield(key) {
				return
			}
		}
	}
}

// A username is compared as a string with the one on the decision, so it is
// written as status holds it rather than with HTML characters escaped.
func TestAUsernameIsWrittenAsStatusHoldsIt(t *testing.T) {
	t.Parallel()
	out := &syncBuffer{}
	log := journallog.New(out, journallog.Options{Clock: clock})
	approved := entry("plan-a", 0)
	approved.Type, approved.Approver = operatorv1alpha1.JournalApprovalAccepted, "oidc:<alice>&co"
	log.Entries("PtahSchema", schema, []operatorv1alpha1.JournalEntry{approved})
	closeLog(t, log)
	if lines := out.lines(t); len(lines) != 1 || !strings.Contains(lines[0], `"approver":"oidc:<alice>&co"`) {
		t.Fatalf("lines = %q, want the approver as status holds it", lines)
	}
}

// The longest entry the API admits still fits one record of containerd's
// container log, which splits a line past 16 KiB: every bounded string at its
// bound, in the characters JSON spells longest. A runtime that reads its
// output in smaller pieces records a longer line as partial records, which
// kubectl logs joins again and a collector that tails the files must too.
func TestTheLongestEntryFitsOneLogRecord(t *testing.T) {
	t.Parallel()
	const runtimeRecord = 16 * 1024
	name := strings.Repeat("n", 253)
	uid := types.UID(strings.Repeat("u", 128))
	username := strings.Repeat("\x01", 512)
	longest := operatorv1alpha1.JournalEntry{
		Type: operatorv1alpha1.JournalOperationFailed, At: metav1.NewTime(queuedAt), StartedAt: ptr(metav1.NewTime(queuedAt)),
		LastAt: ptr(metav1.NewTime(queuedAt)), Count: 1 << 30, OperationID: "sha256:" + strings.Repeat("0", 64),
		Plan: name, PlanUID: uid, PlanFingerprint: strings.Repeat("f", 128), Statements: 1 << 30, Destructive: true,
		ApprovalRequired: true, Replaced: name, Authorization: operatorv1alpha1.MigrationRunAuthorizationKind("Approval"),
		Approval: name, ApprovalUID: uid, Approver: username, Outcome: operatorv1alpha1.JournalDriftRemains,
		Operation: "History", Reason: "R" + strings.Repeat("r", 127), JobName: name, JobUID: uid, AppliedVersions: 1 << 30,
		Failure: &operatorv1alpha1.MigrationRunFailureStatus{
			Version: 1 << 62, Statement: 1 << 30, Statements: 1 << 30, SQLState: "40001", ErrorNumber: 65535,
		},
		Acknowledgment: name, AcknowledgedBy: username,
	}
	out := &syncBuffer{}
	log := journallog.New(out, journallog.Options{Clock: clock})
	log.Entries("PtahMigration", &metav1.ObjectMeta{Namespace: strings.Repeat("s", 63), Name: name, UID: uid},
		[]operatorv1alpha1.JournalEntry{longest})
	closeLog(t, log)
	lines := out.lines(t)
	if len(lines) != 1 {
		t.Fatalf("wrote %d lines, want one", len(lines))
	}
	if size := len(lines[0]) + 1; size > runtimeRecord {
		t.Fatalf("the longest line is %d bytes, more than the %d containerd writes as one record", size, runtimeRecord)
	}
}

func ptr[T any](value T) *T { return &value }

// An output that takes nothing holds no caller: the lines that fit wait, the
// rest are dropped and counted, and the queued ones reach the output once it
// takes them again.
func TestAStoppedOutputNeverHoldsTheCaller(t *testing.T) {
	t.Parallel()
	const capacity = 3
	out := newBlockedOutput()
	registry := prometheus.NewRegistry()
	log := journallog.New(out, journallog.Options{Capacity: capacity, Registerer: registry, Clock: clock})

	// The first line is taken by the writer, which then waits on the output.
	log.Entries("PtahSchema", schema, []operatorv1alpha1.JournalEntry{entry("plan-0", 0)})
	select {
	case <-out.waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer never took the first line")
	}

	var entries []operatorv1alpha1.JournalEntry
	for index := range capacity + 2 {
		entries = append(entries, entry("plan-"+string(rune('a'+index)), time.Duration(index)*time.Second))
	}
	returned := make(chan struct{})
	go func() {
		log.Entries("PtahSchema", schema, entries)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("queuing lines waited on an output that takes none")
	}
	if got := dropped(t, registry); got != 2 {
		t.Fatalf("dropped %v lines, want the 2 past the queue's %d", got, capacity)
	}

	close(out.release)
	closeLog(t, log)
	var plans []string
	for _, text := range out.lines(t) {
		var line journallog.Line
		if err := json.Unmarshal([]byte(text), &line); err != nil {
			t.Fatal(err)
		}
		plans = append(plans, line.Entry.Plan)
	}
	if want := []string{"plan-0", "plan-a", "plan-b", "plan-c"}; !slices.Equal(plans, want) {
		t.Fatalf("wrote plans %q, want %q in order", plans, want)
	}
}

// failingOutput fails the writes it is told to.
type failingOutput struct {
	syncBuffer
	mu    sync.Mutex
	fails int
}

func (f *failingOutput) Write(data []byte) (int, error) {
	f.mu.Lock()
	failing := f.fails > 0
	f.fails--
	f.mu.Unlock()
	if failing {
		return 0, errors.New("broken pipe")
	}
	return f.syncBuffer.Write(data)
}

// A failed write loses that line, counts it, and takes the next one.
func TestAFailedWriteIsCountedAndTheNextLineWritten(t *testing.T) {
	t.Parallel()
	out := &failingOutput{fails: 1}
	registry := prometheus.NewRegistry()
	log := journallog.New(out, journallog.Options{Registerer: registry, Clock: clock})
	log.Entries("PtahSchema", schema, []operatorv1alpha1.JournalEntry{entry("plan-a", 0), entry("plan-b", time.Second)})
	closeLog(t, log)
	if got := dropped(t, registry); got != 1 {
		t.Fatalf("dropped %v lines, want the one whose write failed", got)
	}
	if lines := out.lines(t); len(lines) != 1 || !strings.Contains(lines[0], `"plan":"plan-b"`) {
		t.Fatalf("lines = %q, want plan-b's alone", lines)
	}
}

// Close gives up on an output that takes nothing, so a stopping manager
// exits, and says how many lines it did not write: the one the writer holds
// and the ones still queued.
func TestCloseGivesUpOnAStoppedOutput(t *testing.T) {
	t.Parallel()
	out := newBlockedOutput()
	defer close(out.release)
	log := journallog.New(out, journallog.Options{Clock: clock})
	log.Entries("PtahSchema", schema, []operatorv1alpha1.JournalEntry{entry("plan-a", 0)})
	<-out.waiting
	log.Entries("PtahSchema", schema, []operatorv1alpha1.JournalEntry{entry("plan-b", time.Second), entry("plan-c", 2*time.Second)})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := log.Close(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close() = %v, want the deadline", err)
	}
	if !strings.HasPrefix(err.Error(), "3 journal lines not written") {
		t.Fatalf("Close() = %q, want it to count the 3 lines left", err)
	}
}

// A Close that gives up counts only the lines the output never took, not the
// ones it wrote before it stopped.
func TestCloseCountsOnlyTheLinesLeft(t *testing.T) {
	t.Parallel()
	out := newBlockedOutput()
	out.take = 1
	defer close(out.release)
	log := journallog.New(out, journallog.Options{Clock: clock})
	log.Entries("PtahSchema", schema, []operatorv1alpha1.JournalEntry{
		entry("plan-a", 0), entry("plan-b", time.Second), entry("plan-c", 2*time.Second),
	})
	<-out.waiting
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := log.Close(ctx)
	if err == nil || !strings.HasPrefix(err.Error(), "2 journal lines not written") {
		t.Fatalf("Close() = %v, want it to count the 2 lines after the one written", err)
	}
	if lines := out.lines(t); len(lines) != 1 {
		t.Fatalf("wrote %q, want plan-a's line alone", lines)
	}
}

// A line after Close is dropped and counted, and nothing panics.
func TestALineAfterCloseIsCounted(t *testing.T) {
	t.Parallel()
	out := &syncBuffer{}
	registry := prometheus.NewRegistry()
	log := journallog.New(out, journallog.Options{Registerer: registry, Clock: clock})
	closeLog(t, log)
	closeLog(t, log)
	log.Entries("PtahSchema", schema, []operatorv1alpha1.JournalEntry{entry("plan-a", 0)})
	if got := dropped(t, registry); got != 1 {
		t.Fatalf("dropped %v lines, want the one queued after Close", got)
	}
	if lines := out.lines(t); len(lines) != 0 {
		t.Fatalf("wrote %q after Close", lines)
	}
}

func TestANilLogWritesNothing(t *testing.T) {
	t.Parallel()
	var log *journallog.Log
	log.Entries("PtahSchema", schema, []operatorv1alpha1.JournalEntry{entry("plan-a", 0)})
	if err := log.Close(context.Background()); err != nil {
		t.Fatalf("Close() = %v", err)
	}
}

// Both controllers write status at once. Every line is written whole, and
// every entry is either written or counted.
func TestConcurrentWritersLoseNoLineUncounted(t *testing.T) {
	t.Parallel()
	const writers, each = 8, 50
	out := &syncBuffer{}
	registry := prometheus.NewRegistry()
	log := journallog.New(out, journallog.Options{Capacity: 16, Registerer: registry, Clock: clock})
	var group sync.WaitGroup
	for writer := range writers {
		group.Go(func() {
			for index := range each {
				log.Entries("PtahMigration", schema, []operatorv1alpha1.JournalEntry{entry("plan", time.Duration(writer*each+index)*time.Second)})
			}
		})
	}
	group.Wait()
	closeLog(t, log)
	lines := out.lines(t)
	for _, text := range lines {
		if !json.Valid([]byte(text)) {
			t.Fatalf("a line was not written whole: %q", text)
		}
	}
	if lost := dropped(t, registry); float64(len(lines))+lost != writers*each {
		t.Fatalf("wrote %d lines and dropped %v, want %d in all", len(lines), lost, writers*each)
	}
}
