package harness

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stokaro/ptah-operator/test/e2e/phases"
)

// The row hack/e2e-timing.sh prints for the same stage, with its printf
// format filled in by hand. A Go phase's scenarios land in the ledger a shell
// phase's do, and hack/e2etiming reads both, so the bytes have to agree.
func TestLedgerWritesTheShellRow(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "timings.jsonl")
	if err := os.WriteFile(path, []byte(`{"kind":"phase","name":"assert","outcome":"pass","start":"2026-09-28T09:59:00Z","end":"2026-09-28T10:00:00Z","seconds":60}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var warnings bytes.Buffer
	ledger := NewLedger(path, &warnings)
	start := time.Date(2026, 9, 28, 10, 0, 0, 900_000_000, time.FixedZone("CEST", 2*60*60))
	ledger.Row("scenario", "live-helm-lookup", "pass", start, start.Add(95*time.Second+200*time.Millisecond))
	ledger.Row("scenario", "a name/with \"quotes\" and ü", "fail", start, start)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"kind":"phase","name":"assert","outcome":"pass","start":"2026-09-28T09:59:00Z","end":"2026-09-28T10:00:00Z","seconds":60}` + "\n" +
		`{"kind":"scenario","name":"live-helm-lookup","outcome":"pass","start":"2026-09-28T08:00:00Z","end":"2026-09-28T08:01:36Z","seconds":96}` + "\n" +
		`{"kind":"scenario","name":"a name/with -quotes- and --","outcome":"fail","start":"2026-09-28T08:00:00Z","end":"2026-09-28T08:00:00Z","seconds":0}` + "\n"
	if string(got) != want {
		t.Fatalf("ledger =\n%s\nwant\n%s", got, want)
	}
	if warnings.Len() != 0 {
		t.Fatalf("a ledger that could be written reported %q", warnings.String())
	}
}

// A ledger that cannot be written costs the durations and nothing else: the
// row is dropped, the failure is said once, and nothing panics or fails.
func TestLedgerThatCannotBeWrittenIsReportedOnce(t *testing.T) {
	t.Parallel()
	var warnings bytes.Buffer
	ledger := NewLedger(filepath.Join(t.TempDir(), "missing", "timings.jsonl"), &warnings)
	now := time.Now()
	ledger.Row("scenario", "one", "pass", now, now)
	ledger.Row("scenario", "two", "pass", now, now)
	if count := strings.Count(warnings.String(), "cannot be appended to"); count != 1 {
		t.Fatalf("reported the unwritable ledger %d times: %q", count, warnings.String())
	}

	var silent bytes.Buffer
	NewLedger("", &silent).Row("scenario", "one", "pass", now, now)
	if silent.Len() != 0 {
		t.Fatalf("a run with no ledger reported %q", silent.String())
	}
}

func TestWaitReturnsOnceTheStateArrives(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)}
	readings := 0
	err := wait(context.Background(), "the thing", time.Minute, 10*time.Second,
		func(context.Context) (bool, string, error) {
			readings++
			return readings == 3, "reading", nil
		}, clock.read, clock.pause)
	if err != nil {
		t.Fatalf("wait() error = %v", err)
	}
	if readings != 3 {
		t.Fatalf("wait() read %d times, want 3", readings)
	}
}

// The failure names what was awaited, the bound and the last reading, and the
// last reading is taken at the deadline rather than an interval short of it.
func TestWaitFailsNamingWhatItSaw(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)}
	start := clock.now
	var last time.Time
	readings := 0
	err := wait(context.Background(), "the webhook Service's endpoints", 25*time.Second, 10*time.Second,
		func(context.Context) (bool, string, error) {
			readings++
			last = clock.now
			if readings == 2 {
				return false, "", nil
			}
			return false, "one ready address", nil
		}, clock.read, clock.pause)
	if !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("wait() error = %v, want a timeout", err)
	}
	for _, part := range []string{"the webhook Service's endpoints", "25s", "one ready address"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("wait() error %q does not name %q", err, part)
		}
	}
	if got := last.Sub(start); got != 25*time.Second {
		t.Errorf("the last reading was taken %s after the start, want at the 25s deadline", got)
	}
	if readings != 4 {
		t.Errorf("wait() read %d times, want 4 (0s, 10s, 20s, 25s)", readings)
	}
}

func TestWaitStopsAtAnErrorWithoutWaiting(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)}
	refusal := errors.New("forbidden")
	readings := 0
	err := wait(context.Background(), "the staging Secret", time.Minute, time.Second,
		func(context.Context) (bool, string, error) {
			readings++
			return false, "", refusal
		}, clock.read, clock.pause)
	if !errors.Is(err, refusal) || readings != 1 {
		t.Fatalf("wait() = %v after %d readings, want the refusal after one", err, readings)
	}
}

func TestWaitStopsWhenTheContextEnds(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Wait(ctx, "the rotator Pod", time.Hour, time.Hour, func(context.Context) (bool, string, error) {
		return false, "terminating", nil
	})
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "terminating") {
		t.Fatalf("Wait() = %v, want the cancellation with the last reading", err)
	}
}

// The four checks `kubectl rollout status deployment` makes, each shown to
// hold a rollout back, and the one state it accepts.
func TestDeploymentRolledOutFollowsKubectl(t *testing.T) {
	t.Parallel()
	complete := func() *appsv1.Deployment {
		replicas := int32(2)
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "manager", Generation: 4},
			Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
			Status: appsv1.DeploymentStatus{
				ObservedGeneration: 4, Replicas: 2, UpdatedReplicas: 2, AvailableReplicas: 2,
				Conditions: []appsv1.DeploymentCondition{{
					Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "NewReplicaSetAvailable",
				}},
			},
		}
	}
	for _, test := range []struct {
		name    string
		edit    func(*appsv1.Deployment)
		done    bool
		wantErr bool
		saying  string
	}{
		{name: "rolled out", done: true, saying: "successfully rolled out"},
		{name: "spec not yet observed", edit: func(d *appsv1.Deployment) { d.Generation = 5 }, saying: "not yet observed"},
		{name: "past its progress deadline", edit: func(d *appsv1.Deployment) {
			d.Status.Conditions[0].Reason = progressDeadlineExceeded
		}, wantErr: true},
		{name: "replicas not yet updated", edit: func(d *appsv1.Deployment) { d.Status.UpdatedReplicas = 1 }, saying: "1 out of 2 new replicas"},
		{name: "old replicas terminating", edit: func(d *appsv1.Deployment) { d.Status.Replicas = 3 }, saying: "1 old replicas"},
		{name: "updated replicas not available", edit: func(d *appsv1.Deployment) { d.Status.AvailableReplicas = 1 }, saying: "1 of 2 updated"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			deployment := complete()
			if test.edit != nil {
				test.edit(deployment)
			}
			done, observed, err := DeploymentRolledOut(deployment)
			if (err != nil) != test.wantErr || done != test.done {
				t.Fatalf("DeploymentRolledOut() = %v, %q, %v", done, observed, err)
			}
			if !strings.Contains(observed, test.saying) {
				t.Fatalf("DeploymentRolledOut() said %q, want %q", observed, test.saying)
			}
		})
	}
}

func TestPodReadyReadsTheReadyCondition(t *testing.T) {
	t.Parallel()
	pod := func(conditions ...corev1.PodCondition) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{Conditions: conditions}}
	}
	if !PodReady(pod(corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue})) {
		t.Error("a Ready Pod was read as not ready")
	}
	for name, candidate := range map[string]*corev1.Pod{
		"not ready":       pod(corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse}),
		"containers only": pod(corev1.PodCondition{Type: corev1.ContainersReady, Status: corev1.ConditionTrue}),
		"no conditions":   pod(),
	} {
		if PodReady(candidate) {
			t.Errorf("%s: read as ready", name)
		}
	}
}

// The record is the driver's evidence that the phase passed, so it exists
// after a completed phase and after nothing else.
func TestConcludeRecordsOnlyACompletedPhase(t *testing.T) {
	t.Parallel()
	phase := phases.CertRotation.Phase
	directory := t.TempDir()
	for _, test := range []struct {
		name      string
		code      int
		completed bool
		wantCode  int
		saying    string
	}{
		{name: "completed", completed: true},
		{name: "failed", code: 1, wantCode: 1},
		{name: "failed after completing", code: 1, completed: true, wantCode: 1},
		{name: "passed without completing", wantCode: 1, saying: "TestCertRotation did not run to its end"},
	} {
		record := filepath.Join(directory, strings.ReplaceAll(test.name, " ", "-"))
		code, message := conclude(phase, test.code, test.completed, record)
		if code != test.wantCode || !strings.Contains(message, test.saying) {
			t.Errorf("%s: conclude() = %d, %q", test.name, code, message)
		}
		written, err := os.ReadFile(record)
		switch {
		case test.wantCode == 0 && (err != nil || string(written) != "cert-rotation"):
			t.Errorf("%s: the record holds %q, %v; want the phase's name", test.name, written, err)
		case test.wantCode != 0 && err == nil:
			t.Errorf("%s: a phase that did not pass left a record", test.name)
		}
	}
	unwritable := filepath.Join(directory, "missing", "record")
	if code, message := conclude(phase, 0, true, unwritable); code == 0 || !strings.Contains(message, "could not be recorded") {
		t.Errorf("an unwritable record = %d, %q", code, message)
	}
	if code, message := conclude(phase, 0, true, ""); code != 0 || message != "" {
		t.Errorf("a run asked for no record = %d, %q", code, message)
	}
}

func TestScenariosRunInTheDeclaredOrder(t *testing.T) {
	t.Parallel()
	run := &Run{phase: phases.Phase{Name: "example", Scenarios: []string{"first", "second", "third"}}}
	if err := run.advance("second"); err == nil || !strings.Contains(err.Error(), "where it declares first") {
		t.Fatalf("advance(second) before first = %v", err)
	}
	for _, name := range []string{"first", "second"} {
		if err := run.advance(name); err != nil {
			t.Fatalf("advance(%s) = %v", name, err)
		}
	}
	if got := run.unrun(); len(got) != 1 || got[0] != "third" {
		t.Fatalf("unrun() = %v, want [third]", got)
	}
	if err := run.advance("first"); err == nil {
		t.Fatal("a scenario was run twice")
	}
	if err := run.advance("third"); err != nil {
		t.Fatalf("advance(third) = %v", err)
	}
	if got := run.unrun(); len(got) != 0 {
		t.Fatalf("unrun() = %v after every scenario ran", got)
	}
	if err := run.advance("fourth"); err == nil || !strings.Contains(err.Error(), "after the last") {
		t.Fatalf("advance past the end = %v", err)
	}
}

// Preparation stops a phase at its declared boundary and nowhere else, and a
// scenario past the boundary is refused once the run stopped there.
func TestPreparedStopsAtTheDeclaredBoundary(t *testing.T) {
	t.Parallel()
	declared := phases.Phase{Name: "example", Scenarios: []string{"fixtures", "acceptance"}, Preparation: 1}

	early := &Run{phase: declared}
	if err := early.prepare(); err == nil || !strings.Contains(err.Error(), "after 0 scenarios") {
		t.Fatalf("prepare() before the boundary = %v", err)
	}
	if early.stoppedPrepared() {
		t.Fatal("a run refused at the boundary counts as prepared")
	}

	late := &Run{phase: declared}
	for _, name := range declared.Scenarios {
		if err := late.advance(name); err != nil {
			t.Fatal(err)
		}
	}
	if err := late.prepare(); err == nil || !strings.Contains(err.Error(), "after 2 scenarios") {
		t.Fatalf("prepare() past the boundary = %v", err)
	}

	none := &Run{phase: phases.Phase{Name: "example", Scenarios: []string{"only"}}}
	if err := none.advance("only"); err != nil {
		t.Fatal(err)
	}
	if err := none.prepare(); err == nil || !strings.Contains(err.Error(), "no preparation mode") {
		t.Fatalf("prepare() for a phase without one = %v", err)
	}

	run := &Run{phase: declared}
	if err := run.advance("fixtures"); err != nil {
		t.Fatal(err)
	}
	if err := run.prepare(); err != nil {
		t.Fatalf("prepare() at the boundary = %v", err)
	}
	if !run.stoppedPrepared() {
		t.Fatal("a run stopped at its boundary does not count as prepared")
	}
	if err := run.advance("acceptance"); err == nil || !strings.Contains(err.Error(), "preparation boundary") {
		t.Fatalf("advance past a prepared stop = %v", err)
	}
}

// A scenario that passes is a subtest that passes, and the ledger holds its
// row with the outcome the subtest had.
func TestScenarioRecordsItsRow(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "timings.jsonl")
	run := &Run{
		t:      t,
		phase:  phases.Phase{Name: "example", Scenarios: []string{"only"}},
		ledger: NewLedger(path, os.Stderr),
	}
	ran := false
	if !run.Scenario("only", func(*testing.T) { ran = true }) || !ran {
		t.Fatal("the scenario did not run and pass")
	}
	row, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(row), `{"kind":"scenario","name":"only","outcome":"pass","start":"`) {
		t.Fatalf("ledger row = %s", row)
	}
	if missing := run.unrun(); len(missing) != 0 {
		t.Fatalf("unrun() = %v", missing)
	}
}

type fakeClock struct {
	now time.Time
}

func (c *fakeClock) read() time.Time { return c.now }

func (c *fakeClock) pause(_ context.Context, duration time.Duration) error {
	c.now = c.now.Add(duration)
	return nil
}
