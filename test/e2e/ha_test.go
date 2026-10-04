package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// haCompleteReading is the reading the failover proof waits for: both
// counters present once, and the always-present gauges beside them. It is
// hack/e2e-ha-metrics-selftest.sh's accepted reading.
const haCompleteReading = `# HELP ptah_operator_reconciliations_total Total reconciliations.
# TYPE ptah_operator_reconciliations_total counter
ptah_operator_reconciliations_total{family="schema",result="success"} 4
# HELP ptah_operator_failures_total Total failures.
# TYPE ptah_operator_failures_total counter
ptah_operator_failures_total{category="operation",family="schema",stage="resolve"} 2
# HELP ptah_operator_unresolved_attempts Unaccounted mutations.
# TYPE ptah_operator_unresolved_attempts gauge
ptah_operator_unresolved_attempts{family="schema"} 0
ptah_operator_unresolved_attempts{family="migration"} 1
# HELP ptah_operator_unresolved_owed_seconds Owed for.
# TYPE ptah_operator_unresolved_owed_seconds gauge
ptah_operator_unresolved_owed_seconds{family="migration"} 61
# HELP ptah_operator_unresolved_view_synced View synchronized.
# TYPE ptah_operator_unresolved_view_synced gauge
ptah_operator_unresolved_view_synced 1
# HELP ptah_operator_unresolved_view_read_failures_total Failed reads.
# TYPE ptah_operator_unresolved_view_read_failures_total counter
ptah_operator_unresolved_view_read_failures_total 3
# HELP ptah_operator_resources Resources by phase.
# TYPE ptah_operator_resources gauge
ptah_operator_resources{family="schema",phase="InSync"} 1
# HELP ptah_operator_overdue_resources Overdue resources.
# TYPE ptah_operator_overdue_resources gauge
ptah_operator_overdue_resources{family="schema"} 0
# HELP ptah_operator_active_operations Operations in flight.
# TYPE ptah_operator_active_operations gauge
ptah_operator_active_operations{family="schema",operation="Observe"} 1
# HELP ptah_operator_active_operation_seconds Oldest operation age.
# TYPE ptah_operator_active_operation_seconds gauge
ptah_operator_active_operation_seconds{family="schema",operation="Observe"} 4
# HELP ptah_operator_pending_lock_releases Lock releases owed.
# TYPE ptah_operator_pending_lock_releases gauge
ptah_operator_pending_lock_releases{family="schema"} 0
# HELP ptah_operator_stored_plans Retained plans.
# TYPE ptah_operator_stored_plans gauge
ptah_operator_stored_plans{family="schema"} 2
# HELP ptah_operator_stored_plan_bytes Retained plan bytes.
# TYPE ptah_operator_stored_plan_bytes gauge
ptah_operator_stored_plan_bytes 4096
# HELP ptah_operator_webhook_certificate_expiry_timestamp_seconds Certificate expiry.
# TYPE ptah_operator_webhook_certificate_expiry_timestamp_seconds gauge
ptah_operator_webhook_certificate_expiry_timestamp_seconds 1.8e+09
# HELP ptah_operator_webhook_certificate_read_failures_total Failed certificate reads.
# TYPE ptah_operator_webhook_certificate_read_failures_total counter
ptah_operator_webhook_certificate_read_failures_total 0`

// haTwoCounters is the smallest complete reading: the two counters alone.
const haTwoCounters = `# HELP ptah_operator_reconciliations_total Total reconciliations.
# TYPE ptah_operator_reconciliations_total counter
ptah_operator_reconciliations_total{family="schema",result="success"} 4
# HELP ptah_operator_failures_total Total failures.
# TYPE ptah_operator_failures_total counter
ptah_operator_failures_total{category="operation",family="schema",stage="resolve"} 2`

func TestHAMetricNumber(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]float64{
		"0": 0, "4": 4, "1.5": 1.5, "7.": 7, ".5": 0.5, "3e+02": 300, "1.8e+09": 1.8e9, "2E3": 2000,
		// Underflow is a number, and it is zero, as awk read it.
		"1e-999": 0,
	} {
		if got, ok := haMetricNumber(value); !ok || got != want {
			t.Errorf("haMetricNumber(%q) = %v, %v; want %v", value, got, ok, want)
		}
	}
	for _, value := range []string{
		"", "-1", "+1", "NaN", "nan", "Inf", "+Inf", "1e999", "0x10", "1,0", "1.2.3", "e5", ".", "4\r", " 4",
	} {
		if got, ok := haMetricNumber(value); ok {
			t.Errorf("haMetricNumber(%q) read %v", value, got)
		}
	}
}

func TestHAValidateCustomOperatorMetrics(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		reading string
		want    haMetricVerdict
	}{
		// hack/e2e-ha-metrics-selftest.sh, each of its readings with the exit
		// status it expected: 0 complete, 1 waiting, 2 malformed.
		{"the complete post-failover reading", haCompleteReading, haMetricsComplete},
		// A gauge at zero is evidence, and the whole point of publishing it.
		{"a view that has not synchronized", haTwoCounters + `
# HELP ptah_operator_unresolved_view_synced View synchronized.
# TYPE ptah_operator_unresolved_view_synced gauge
ptah_operator_unresolved_view_synced 0`, haMetricsComplete},
		// Still waiting rather than broken: this is what makes the caller poll.
		{"a reading before the failure counter appeared", `# HELP ptah_operator_reconciliations_total Total reconciliations.
# TYPE ptah_operator_reconciliations_total counter
ptah_operator_reconciliations_total{family="schema",result="success"} 4`, haMetricsWaiting},
		// A family nobody decided to publish, the refusal the check exists for,
		// and each of its three lines refused on its own.
		{"an undeclared custom family", haCompleteReading + `
# HELP ptah_operator_mystery_total Something nobody decided to ship.
# TYPE ptah_operator_mystery_total counter
ptah_operator_mystery_total 1`, haMetricsMalformed},
		{"an undeclared family announced by HELP alone", haCompleteReading + `
# HELP ptah_operator_mystery_total Something nobody decided to ship.`, haMetricsMalformed},
		{"an undeclared family announced by TYPE alone", haCompleteReading + `
# TYPE ptah_operator_mystery_total counter`, haMetricsMalformed},
		{"an undeclared family present only as a sample", haCompleteReading + `
ptah_operator_mystery_total 1`, haMetricsMalformed},
		// A counter at zero is not the evidence this phase measures going up.
		{"a counter at zero", `# HELP ptah_operator_reconciliations_total Total reconciliations.
# TYPE ptah_operator_reconciliations_total counter
ptah_operator_reconciliations_total{family="schema",result="success"} 0`, haMetricsMalformed},
		{"an always-present family with the wrong type", haCompleteReading + `
# TYPE ptah_operator_unresolved_attempts counter`, haMetricsMalformed},
		{"a negative gauge", `# HELP ptah_operator_unresolved_attempts Unaccounted mutations.
# TYPE ptah_operator_unresolved_attempts gauge
ptah_operator_unresolved_attempts{family="schema"} -1`, haMetricsMalformed},
		// Two collectors answered, which the proof must not average over.
		{"a duplicated counter sample", `# HELP ptah_operator_reconciliations_total Total reconciliations.
# TYPE ptah_operator_reconciliations_total counter
ptah_operator_reconciliations_total{family="schema",result="success"} 4
ptah_operator_reconciliations_total{family="schema",result="success"} 5
# HELP ptah_operator_failures_total Total failures.
# TYPE ptah_operator_failures_total counter
ptah_operator_failures_total{category="operation",family="schema",stage="resolve"} 2`, haMetricsMalformed},

		// TestHACustomMetricValidatorRejectsOverflowingExponent in
		// hack/e2e_wiring_test.go: an exponent that overflows is not a count.
		{"an overflowing exponent", strings.Replace(haTwoCounters, "} 2", "} 1e999", 1), haMetricsMalformed},

		// One refusal per clause the script's awk carried.
		{"a HELP line with no text", haTwoCounters + "\n# HELP ptah_operator_resources", haMetricsMalformed},
		{"a TYPE line with a fifth field", haTwoCounters + "\n# TYPE ptah_operator_resources gauge extra", haMetricsMalformed},
		{"a reconciliation counter declared a gauge", strings.Replace(haTwoCounters,
			"# TYPE ptah_operator_reconciliations_total counter", "# TYPE ptah_operator_reconciliations_total gauge", 1), haMetricsMalformed},
		{"a failure counter declared a gauge", strings.Replace(haTwoCounters,
			"# TYPE ptah_operator_failures_total counter", "# TYPE ptah_operator_failures_total gauge", 1), haMetricsMalformed},
		{"a timestamped sample", strings.Replace(haTwoCounters, "} 4", "} 4 1700000000000", 1), haMetricsMalformed},
		{"an always-present gauge that is not a number", haTwoCounters + "\nptah_operator_stored_plan_bytes NaN", haMetricsMalformed},
		{"a counter sample for another family label", haTwoCounters +
			"\n" + `ptah_operator_reconciliations_total{family="migration",result="success"} 1`, haMetricsMalformed},
		{"a counter sample with its labels reordered", strings.Replace(haTwoCounters,
			`{category="operation",family="schema",stage="resolve"}`, `{family="schema",category="operation",stage="resolve"}`, 1), haMetricsMalformed},
		{"one of ours mentioned outside a HELP, TYPE or sample line", haTwoCounters + "\n# EOF ptah_operator_x", haMetricsMalformed},
		{"one of ours indented", haTwoCounters + "\n ptah_operator_stored_plan_bytes 1", haMetricsMalformed},
		{"a duplicated HELP", haTwoCounters + "\n# HELP ptah_operator_failures_total Again.", haMetricsMalformed},
		{"a duplicated TYPE", haTwoCounters + "\n# TYPE ptah_operator_reconciliations_total counter", haMetricsMalformed},
		{"a value ending in a carriage return", strings.Replace(haTwoCounters, "} 2", "} 2\r", 1), haMetricsMalformed},

		// Well formed, and not there yet: each counter needs its HELP, TYPE and
		// sample before the reading is complete.
		{"no failure counter HELP", strings.Replace(haTwoCounters, "# HELP ptah_operator_failures_total Total failures.\n", "", 1), haMetricsWaiting},
		{"no reconciliation counter TYPE", strings.Replace(haTwoCounters, "# TYPE ptah_operator_reconciliations_total counter\n", "", 1), haMetricsWaiting},
		{"no failure counter sample", strings.TrimSuffix(haTwoCounters,
			"\n"+`ptah_operator_failures_total{category="operation",family="schema",stage="resolve"} 2`), haMetricsWaiting},
		{"nothing of ours at all", "go_goroutines 7\n# HELP go_threads Threads.\n", haMetricsWaiting},

		// What awk ignored stays ignored, and awk's field separator is kept.
		{"other exporters' lines beside ours", "# HELP go_goroutines Goroutines.\ngo_goroutines 7\n" + haTwoCounters + "\n", haMetricsComplete},
		{"a tab between a sample and its value", strings.Replace(haTwoCounters, "} 4", "}\t4", 1), haMetricsComplete},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := haValidateCustomOperatorMetrics([]byte(test.reading)); got != test.want {
				t.Fatalf("verdict = %s, want %s", got, test.want)
			}
		})
	}
}

func TestHAResultCleanupMetrics(t *testing.T) {
	t.Parallel()
	// These lines came from the leader that failed the durable-delivery HA
	// run. Cleanup counters are background evidence, not a replacement for
	// the operation failure and reconciliation counters this phase requires.
	raw, err := os.ReadFile("../../testdata/e2e/readings/ha-result-cleanup-metrics.prom")
	if err != nil {
		t.Fatal(err)
	}
	cleanup := string(raw)
	for _, test := range []struct {
		name, reading string
		want          haMetricVerdict
	}{
		{"installed cleanup samples", haTwoCounters + "\n" + cleanup, haMetricsComplete},
		{"cleanup alone", cleanup, haMetricsWaiting},
		{"wrong cleanup type", haTwoCounters + "\n" + strings.Replace(cleanup, " counter\n", " gauge\n", 1), haMetricsMalformed},
		{"invalid cleanup value", haTwoCounters + "\n" + strings.Replace(cleanup, "} 2\n", "} NaN\n", 1), haMetricsMalformed},
		{"unknown cleanup family", haTwoCounters + "\n" + strings.ReplaceAll(cleanup, "result_cleanup_operations_total", "result_unknown_total"), haMetricsMalformed},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := haValidateCustomOperatorMetrics([]byte(test.reading)); got != test.want {
				t.Fatalf("verdict = %s, want %s", got, test.want)
			}
		})
	}
}

// The cases of TestHAResolveOperationFailureCounterParser in
// hack/e2e_wiring_test.go, and the samples it did not name.
func TestHAResolveFailureCounter(t *testing.T) {
	t.Parallel()
	const sample = `ptah_operator_failures_total{category="operation",family="schema",stage="resolve"}`
	for _, test := range []struct {
		name, metrics, want string
		wantError           bool
	}{
		{name: "absent", metrics: "# unrelated\n", want: "0"},
		{name: "zero", metrics: sample + " 0\n", want: "0"},
		{name: "positive exponent", metrics: sample + " 3e+02\n", want: "3e+02"},
		{name: "overflowing exponent", metrics: sample + " 1e999\n", wantError: true},
		{name: "duplicate", metrics: sample + " 1\n" + sample + " 2\n", wantError: true},
		{name: "negative", metrics: sample + " -1\n", wantError: true},
		{name: "non-finite", metrics: sample + " NaN\n", wantError: true},
		{name: "timestamped", metrics: sample + " 1 123\n", wantError: true},
		{name: "among the whole reading", metrics: haCompleteReading, want: "2"},
		// The sample is matched as Prometheus spells it; another spelling is
		// not the counter at all, so it reads as not yet published.
		{name: "labels reordered", metrics: `ptah_operator_failures_total{family="schema",category="operation",stage="resolve"} 5` + "\n", want: "0"},
		{name: "another stage", metrics: `ptah_operator_failures_total{category="operation",family="schema",stage="plan"} 5` + "\n", want: "0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := haResolveFailureCounter([]byte(test.metrics))
			if test.wantError {
				if err == nil || !errors.Is(err, errHAFailureCounter) {
					t.Fatalf("parser accepted invalid metrics and returned %q, %v", got, err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("parser = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestHACounterIncreased(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		baseline, current string
		want              bool
	}{
		{"3", "4", true},
		// Not an increase: the proof the script held against a >= comparison.
		{"3", "3", false},
		{"4", "3", false},
		{"3e+02", "301", true},
		{"300", "3e+02", false},
		{"0", "0.5", true},
	} {
		got, err := haCounterIncreased(test.baseline, test.current)
		if err != nil || got != test.want {
			t.Errorf("haCounterIncreased(%q, %q) = %v, %v; want %v", test.baseline, test.current, got, err, test.want)
		}
	}
	for _, pair := range [][2]string{{"", "1"}, {"1", "NaN"}, {"-1", "2"}} {
		if _, err := haCounterIncreased(pair[0], pair[1]); !errors.Is(err, errHAFailureCounter) {
			t.Errorf("haCounterIncreased(%q, %q) compared an unreadable counter: %v", pair[0], pair[1], err)
		}
	}
}

// haClock is a clock the wait advances by pausing, so a test measures the
// loop and not the machine.
type haClock struct{ now time.Time }

func (c *haClock) read() time.Time { return c.now }

func (c *haClock) pause(_ context.Context, duration time.Duration) error {
	c.now = c.now.Add(duration)
	return nil
}

func haCounterReading(value string) []byte {
	return []byte(strings.Replace(haTwoCounters, "} 2", "} "+value, 1))
}

func TestHAAwaitIncreasedFailureCounter(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	await := func(scrapes [][]byte, errs []error, baseline string) (int, error) {
		clock := &haClock{now: start}
		scrape := func(context.Context) ([]byte, error) {
			index := 0
			for index < len(scrapes)-1 && clock.now.Sub(start) > time.Duration(index)*time.Second {
				index++
			}
			if index < len(errs) && errs[index] != nil {
				return nil, errs[index]
			}
			return scrapes[index], nil
		}
		return haAwaitIncreasedFailureCounter(context.Background(), scrape, baseline, haMetricsTimeout, haPoll,
			clock.read, clock.pause)
	}

	// TestHACustomMetricsPollsUntilResolveFailureCounterIncreases in
	// hack/e2e_wiring_test.go: the first scrape still reads the baseline, the
	// second the increase, and the wait ends on the second.
	if scrapes, err := await([][]byte{haCounterReading("3"), haCounterReading("4")}, nil, "3"); err != nil || scrapes != 2 {
		t.Fatalf("the wait for an increase ended after %d scrapes with %v, want 2 and nil", scrapes, err)
	}
	// A failed scrape and a reading still waiting are polled through.
	waiting := []byte(`# HELP ptah_operator_reconciliations_total Total reconciliations.`)
	if scrapes, err := await([][]byte{nil, waiting, haCounterReading("4")},
		[]error{errors.New("proxy error"), nil, nil}, "3"); err != nil || scrapes != 3 {
		t.Fatalf("the wait ended after %d scrapes with %v, want 3 and nil", scrapes, err)
	}
	// A malformed reading ends the wait at once: no later scrape can fix it.
	malformed := []byte(haTwoCounters + "\nptah_operator_mystery_total 1")
	if scrapes, err := await([][]byte{malformed, haCounterReading("4")}, nil, "3"); !errors.Is(err, errHAMetricsMalformed) || scrapes != 1 {
		t.Fatalf("a malformed reading ended the wait after %d scrapes with %v", scrapes, err)
	}
	// An unreadable baseline cannot be compared with.
	if _, err := await([][]byte{haCounterReading("4")}, nil, "bogus"); !errors.Is(err, errHAFailureCounter) {
		t.Fatalf("an unreadable baseline was compared with: %v", err)
	}
	// A counter that never goes up times out, naming the missing increase.
	if scrapes, err := await([][]byte{haCounterReading("3")}, nil, "3"); !errors.Is(err, errHAMetricsTimeout) || scrapes != 30 {
		t.Fatalf("a counter that never moved ended the wait after %d scrapes with %v, want 30 and a timeout", scrapes, err)
	}
	// A context that ends ends the wait with its own error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := haAwaitIncreasedFailureCounter(ctx, func(context.Context) ([]byte, error) { return haCounterReading("3"), nil },
		"3", haMetricsTimeout, haPoll, time.Now, haTestPause)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("an ended context did not end the wait: %v", err)
	}
}

func haTestPause(ctx context.Context, _ time.Duration) error { return ctx.Err() }

func TestHAReportsActiveLeader(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`leader_election_master_status{name="ptah-operator"} 1`,
		"leader_election_master_status 1.0",
		"leader_election_master_status\t1.000",
		"# HELP leader_election_master_status Leader.\n# TYPE leader_election_master_status gauge\n" +
			`leader_election_master_status{name="ptah-operator"} 1` + "\ngo_goroutines 7\n",
	} {
		if !haReportsActiveLeader([]byte(body)) {
			t.Errorf("an active leader reading was refused: %q", body)
		}
	}
	for _, body := range []string{
		"",
		`leader_election_master_status{name="ptah-operator"} 0`,
		"leader_election_master_status 1.5",
		"leader_election_master_status 10",
		"leader_election_master_status_total 1",
		" leader_election_master_status 1",
		"leader_election_master_status 1 1700000000",
		`leader_election_master_status{name="a}"} 1`,
	} {
		if haReportsActiveLeader([]byte(body)) {
			t.Errorf("a reading that is not an active leader passed: %q", body)
		}
	}
}

func TestHALeaderPodName(t *testing.T) {
	t.Parallel()
	for holder, want := range map[string]string{
		"ptah-e2e-ptah-operator-7d9f8c5b4d-x2k9p_0b5c9a8e-1d2f-4c3b-9a8e-7f6d5c4b3a21": "ptah-e2e-ptah-operator-7d9f8c5b4d-x2k9p",
		"a_b_c": "a_b",
		"plain": "plain",
		"pod_":  "pod",
		"":      "",
		"_id":   "",
	} {
		if got := haLeaderPodName(holder); got != want {
			t.Errorf("haLeaderPodName(%q) = %q, want %q", holder, got, want)
		}
	}
}

func TestHALeaseReadings(t *testing.T) {
	t.Parallel()
	absent := &coordinationv1.Lease{}
	if haLeaseHolder(absent) != "" || haLeaseTransitions(absent) != 0 {
		t.Fatal("a Lease naming no holder and no transitions was read as naming some")
	}
	held := &coordinationv1.Lease{Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr.To("pod_id"), LeaseTransitions: ptr.To[int32](3)}}
	if haLeaseHolder(held) != "pod_id" || haLeaseTransitions(held) != 3 {
		t.Fatal("the Lease's holder and transitions were misread")
	}
	if !haLeaderMoved("b_1", "a_1") || !haLeaderMoved("a_1", "") {
		t.Fatal("a new holder was not recognized")
	}
	if haLeaderMoved("", "") || haLeaderMoved("a_1", "a_1") {
		t.Fatal("no holder, or the holder the failover started from, passed as a new one")
	}
	leases := []coordinationv1.Lease{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "ptah-system", Name: leaderLeaseName}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "kube-scheduler"}},
	}
	if got := haCountLeasesNamed(leases, leaderLeaseName); got != 1 {
		t.Fatalf("counted %d Leases, want 1", got)
	}
	leases = append(leases, coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: leaderLeaseName}})
	if got := haCountLeasesNamed(leases, leaderLeaseName); got != 2 {
		t.Fatalf("counted %d Leases, want the second one too", got)
	}
}

func haReadyManagerPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			"app.kubernetes.io/instance": "ptah-e2e", "app.kubernetes.io/component": "controller",
		}},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		}},
	}
}

func TestHAHolderIsReadyManagerPod(t *testing.T) {
	t.Parallel()
	if !haHolderIsReadyManagerPod(haReadyManagerPod(), "ptah-e2e") {
		t.Fatal("a ready manager replica was not recognized")
	}
	for _, test := range []struct {
		name   string
		mutate func(*corev1.Pod)
	}{
		{"being deleted", func(p *corev1.Pod) { p.DeletionTimestamp = &metav1.Time{Time: time.Now()} }},
		{"another release", func(p *corev1.Pod) { p.Labels["app.kubernetes.io/instance"] = "other" }},
		{"no release label", func(p *corev1.Pod) { delete(p.Labels, "app.kubernetes.io/instance") }},
		{"not the controller", func(p *corev1.Pod) { p.Labels["app.kubernetes.io/component"] = "certificate-rotation" }},
		{"not ready", func(p *corev1.Pod) { p.Status.Conditions[1].Status = corev1.ConditionFalse }},
		{"no conditions", func(p *corev1.Pod) { p.Status.Conditions = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			pod := haReadyManagerPod()
			test.mutate(pod)
			if haHolderIsReadyManagerPod(pod, "ptah-e2e") {
				t.Fatal("the Pod passed as a ready manager replica")
			}
		})
	}
}

func haDecodedJSON(t *testing.T, document string) any {
	t.Helper()
	var decoded any
	if err := json.Unmarshal([]byte(document), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestHARoleRulesExact(t *testing.T) {
	t.Parallel()
	exact := `[{"apiGroups":["coordination.k8s.io"],"resources":["leases"],"verbs":["get","create","update"]}]`
	if !haRoleRulesExact(haDecodedJSON(t, exact)) {
		t.Fatal("the one rule the Role may hold was refused")
	}
	for name, rules := range map[string]string{
		"verbs reordered":         `[{"apiGroups":["coordination.k8s.io"],"resources":["leases"],"verbs":["create","get","update"]}]`,
		"a verb more":             `[{"apiGroups":["coordination.k8s.io"],"resources":["leases"],"verbs":["get","create","update","patch"]}]`,
		"a verb less":             `[{"apiGroups":["coordination.k8s.io"],"resources":["leases"],"verbs":["get","update"]}]`,
		"an empty resourceNames":  `[{"apiGroups":["coordination.k8s.io"],"resources":["leases"],"verbs":["get","create","update"],"resourceNames":[]}]`,
		"another group":           `[{"apiGroups":["coordination.k8s.io",""],"resources":["leases"],"verbs":["get","create","update"]}]`,
		"another resource":        `[{"apiGroups":["coordination.k8s.io"],"resources":["leases","events"],"verbs":["get","create","update"]}]`,
		"a second rule":           `[{"apiGroups":["coordination.k8s.io"],"resources":["leases"],"verbs":["get","create","update"]},{"apiGroups":[""],"resources":["events"],"verbs":["create"]}]`,
		"no rules":                `[]`,
		"rules absent (null)":     `null`,
		"the rule outside a list": `{"apiGroups":["coordination.k8s.io"],"resources":["leases"],"verbs":["get","create","update"]}`,
	} {
		if haRoleRulesExact(haDecodedJSON(t, rules)) {
			t.Errorf("%s passed as the exact Role", name)
		}
	}
}

func TestHAClusterRoleGrantsLeases(t *testing.T) {
	t.Parallel()
	rules := func(document string) []any {
		decoded, _ := haDecodedJSON(t, document).([]any)
		return decoded
	}
	for name, document := range map[string]string{
		"the coordination group and leases": `[{"apiGroups":["","coordination.k8s.io"],"resources":["pods","leases"],"verbs":["get"]}]`,
		"every group":                       `[{"apiGroups":["*"],"resources":["leases"],"verbs":["get"]}]`,
		"every resource":                    `[{"apiGroups":["coordination.k8s.io"],"resources":["*"],"verbs":["get"]}]`,
		"a later rule":                      `[{"apiGroups":[""],"resources":["pods"],"verbs":["get"]},{"apiGroups":["coordination.k8s.io"],"resources":["leases"],"verbs":["list"]}]`,
	} {
		if !haClusterRoleGrantsLeases(rules(document)) {
			t.Errorf("a ClusterRole granting Leases through %s was not caught", name)
		}
	}
	for name, document := range map[string]string{
		"no rules":                  `[]`,
		"the group alone":           `[{"apiGroups":["coordination.k8s.io"],"resources":["events"],"verbs":["get"]}]`,
		"leases in another group":   `[{"apiGroups":["example.com"],"resources":["leases"],"verbs":["get"]}]`,
		"no apiGroups":              `[{"resources":["leases"],"verbs":["get"]}]`,
		"no resources":              `[{"apiGroups":["coordination.k8s.io"],"verbs":["get"]}]`,
		"a Lease subresource alone": `[{"apiGroups":["coordination.k8s.io"],"resources":["leases/status"],"verbs":["get"]}]`,
		"a rule that is not a map":  `["leases"]`,
	} {
		if haClusterRoleGrantsLeases(rules(document)) {
			t.Errorf("a ClusterRole with %s was read as granting Leases", name)
		}
	}
	if haClusterRoleGrantsLeases(nil) {
		t.Fatal("a ClusterRole with no rules was read as granting Leases")
	}
}

func TestHASchemaQuiesced(t *testing.T) {
	t.Parallel()
	quiet := func() *ptahv1alpha1.PtahSchema {
		schema := &ptahv1alpha1.PtahSchema{}
		schema.Spec.Suspend = true
		schema.Status.Phase = ptahv1alpha1.PhaseSuspended
		return schema
	}
	if !haSchemaQuiesced(quiet()) {
		t.Fatal("a suspended schema with nothing in flight was refused")
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahSchema){
		"not suspended":          func(s *ptahv1alpha1.PtahSchema) { s.Spec.Suspend = false },
		"not yet reporting it":   func(s *ptahv1alpha1.PtahSchema) { s.Status.Phase = ptahv1alpha1.PhaseResolving },
		"no phase at all":        func(s *ptahv1alpha1.PtahSchema) { s.Status.Phase = "" },
		"an operation in flight": func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{} },
		"a Resolve still claimed": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationResolve}
		},
		"suspended but still busy": func(s *ptahv1alpha1.PtahSchema) { s.Status.Phase = ptahv1alpha1.PhaseApplying },
	} {
		schema := quiet()
		mutate(schema)
		if haSchemaQuiesced(schema) {
			t.Errorf("a schema %s passed as quiet", name)
		}
	}
}

func TestHAResolveJobTerminal(t *testing.T) {
	t.Parallel()
	job := func(active int32, conditions ...batchv1.JobCondition) *batchv1.Job {
		return &batchv1.Job{Status: batchv1.JobStatus{Active: active, Conditions: conditions}}
	}
	for name, candidate := range map[string]*batchv1.Job{
		"complete": job(0, batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}),
		"failed": job(0, batchv1.JobCondition{Type: batchv1.JobSuspended, Status: corev1.ConditionFalse},
			batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}),
	} {
		if !haResolveJobTerminal(candidate) {
			t.Errorf("a %s Resolve Job was read as able to move", name)
		}
	}
	for name, candidate := range map[string]*batchv1.Job{
		"with an active Pod":       job(1, batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}),
		"with no conditions":       job(0),
		"with a false Complete":    job(0, batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionFalse}),
		"with only another type":   job(0, batchv1.JobCondition{Type: batchv1.JobSuspended, Status: corev1.ConditionTrue}),
		"with an unknown Complete": job(0, batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionUnknown}),
	} {
		if haResolveJobTerminal(candidate) {
			t.Errorf("a Resolve Job %s passed as terminal", name)
		}
	}
}

const haTestSchemaUID = types.UID("8d8b1c6e-9f0a-4b7c-8d2e-1f3a5b7c9d0e")

func haSchemaJob() batchv1.Job {
	return batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "ptah-resolve-leader-failover-abc", UID: "job-uid",
		Annotations: map[string]string{haAdmissionSnapshotAnnotation: "sha256:" + strings.Repeat("a", 64)},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "operator.ptah.run/v1alpha1", Kind: "PtahSchema", Name: haSchema, UID: haTestSchemaUID,
			Controller: ptr.To(true),
		}},
	}}
}

func TestHASchemaJobs(t *testing.T) {
	t.Parallel()
	if owned := haSchemaJobs([]batchv1.Job{haSchemaJob()}, haTestSchemaUID); len(owned) != 1 {
		t.Fatalf("the schema's Job was not found: %d", len(owned))
	}
	for name, mutate := range map[string]func(*metav1.OwnerReference){
		"not the controller":      func(r *metav1.OwnerReference) { r.Controller = ptr.To(false) },
		"controller unset":        func(r *metav1.OwnerReference) { r.Controller = nil },
		"another API version":     func(r *metav1.OwnerReference) { r.APIVersion = "operator.ptah.run/v1beta1" },
		"another kind":            func(r *metav1.OwnerReference) { r.Kind = "PtahMigration" },
		"another name":            func(r *metav1.OwnerReference) { r.Name = "other" },
		"another schema's UID":    func(r *metav1.OwnerReference) { r.UID = "other-uid" },
		"an owner with no UID at": func(r *metav1.OwnerReference) { r.UID = "" },
	} {
		job := haSchemaJob()
		mutate(&job.OwnerReferences[0])
		if owned := haSchemaJobs([]batchv1.Job{job}, haTestSchemaUID); len(owned) != 0 {
			t.Errorf("a Job whose owner has %s was counted as the schema's", name)
		}
	}
	if owned := haSchemaJobs([]batchv1.Job{haSchemaJob()}, ""); len(owned) != 0 {
		t.Fatal("an empty schema UID matched a Job")
	}
	if owned := haSchemaJobs([]batchv1.Job{haSchemaJob(), haSchemaJob()}, haTestSchemaUID); len(owned) != 2 {
		t.Fatalf("two Jobs of the schema counted as %d; the second one is what the phase refuses", len(owned))
	}
}

func haAdmittedOperationPod(job *batchv1.Job) corev1.Pod {
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:        job.Name + "-x",
		Annotations: map[string]string{haAdmissionSnapshotAnnotation: job.Annotations[haAdmissionSnapshotAnnotation]},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: ptr.To(true),
		}},
	}}
}

func TestHAAdmittedPod(t *testing.T) {
	t.Parallel()
	job := haSchemaJob()
	if !haAdmittedPod(&job, []corev1.Pod{haAdmittedOperationPod(&job)}) {
		t.Fatal("the admitted Pod was not recognized")
	}
	for name, mutate := range map[string]func(*batchv1.Job, *corev1.Pod){
		"the Job names no snapshot": func(j *batchv1.Job, _ *corev1.Pod) { delete(j.Annotations, haAdmissionSnapshotAnnotation) },
		"the Job's snapshot is not SHA256": func(j *batchv1.Job, p *corev1.Pod) {
			j.Annotations[haAdmissionSnapshotAnnotation] = "sha256:abc"
			p.Annotations[haAdmissionSnapshotAnnotation] = "sha256:abc"
		},
		"the Pod carries another snapshot": func(_ *batchv1.Job, p *corev1.Pod) {
			p.Annotations[haAdmissionSnapshotAnnotation] = "sha256:" + strings.Repeat("b", 64)
		},
		"the Pod carries none":             func(_ *batchv1.Job, p *corev1.Pod) { delete(p.Annotations, haAdmissionSnapshotAnnotation) },
		"the Pod is being deleted":         func(_ *batchv1.Job, p *corev1.Pod) { p.DeletionTimestamp = &metav1.Time{Time: time.Now()} },
		"the Pod belongs to another Job":   func(_ *batchv1.Job, p *corev1.Pod) { p.OwnerReferences[0].UID = "other-job" },
		"the Job does not control the Pod": func(_ *batchv1.Job, p *corev1.Pod) { p.OwnerReferences[0].Controller = nil },
	} {
		candidate := haSchemaJob()
		pod := haAdmittedOperationPod(&candidate)
		mutate(&candidate, &pod)
		if haAdmittedPod(&candidate, []corev1.Pod{pod}) {
			t.Errorf("an admission where %s passed", name)
		}
	}
	if haAdmittedPod(&job, nil) {
		t.Fatal("a Job with no Pod passed as admitted")
	}
	if haAdmittedPod(&job, []corev1.Pod{haAdmittedOperationPod(&job), haAdmittedOperationPod(&job)}) {
		t.Fatal("two admitted Pods passed as exactly one")
	}
}

func haFailedResolveSchema() *ptahv1alpha1.PtahSchema {
	schema := &ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{UID: haTestSchemaUID, Generation: 1}}
	schema.Status.ObservedGeneration = 1
	schema.Status.Phase = ptahv1alpha1.PhaseFailed
	schema.Status.NextReconciliationTime = &metav1.Time{Time: time.Date(2026, 9, 28, 13, 0, 0, 0, time.UTC)}
	schema.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationResolve}
	schema.Status.Conditions = []metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionFalse, Reason: "OperationFailed"},
		{Type: "ReconciliationFailed", Status: metav1.ConditionTrue, Reason: "OperationFailed"},
	}
	return schema
}

func TestHAFailedResolveLifecycle(t *testing.T) {
	t.Parallel()
	if !haFailedResolveLifecycle(haFailedResolveSchema(), haTestSchemaUID) {
		t.Fatal("the typed Resolve failure was not recognized")
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahSchema){
		"another schema":               func(s *ptahv1alpha1.PtahSchema) { s.UID = "other" },
		"an older generation observed": func(s *ptahv1alpha1.PtahSchema) { s.Generation = 2 },
		"not failed":                   func(s *ptahv1alpha1.PtahSchema) { s.Status.Phase = ptahv1alpha1.PhaseResolving },
		"no retry scheduled":           func(s *ptahv1alpha1.PtahSchema) { s.Status.NextReconciliationTime = nil },
		"no operation claimed":         func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation = nil },
		"another operation claimed": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{Type: ptahv1alpha1.OperationVerify}
		},
		"the failure not true":     func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions[1].Status = metav1.ConditionFalse },
		"another failure reason":   func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions[1].Reason = "ConfigurationError" },
		"the reason on Ready only": func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions = s.Status.Conditions[:1] },
		"no conditions":            func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions = nil },
	} {
		schema := haFailedResolveSchema()
		mutate(schema)
		if haFailedResolveLifecycle(schema, haTestSchemaUID) {
			t.Errorf("a schema with %s passed as the typed Resolve failure", name)
		}
	}
	if haFailedResolveLifecycle(haFailedResolveSchema(), "") {
		t.Fatal("an empty schema UID matched")
	}
}

func TestHARegistryHost(t *testing.T) {
	t.Parallel()
	host, ok := haRegistryHost("registry.ptah-e2e.svc:5000/ptah-operator@sha256:" + strings.Repeat("a", 64))
	if !ok || host != "registry.ptah-e2e.svc:5000" {
		t.Fatalf("haRegistryHost = %q, %v", host, ok)
	}
	for _, image := range []string{"ptah-operator:latest", "", "/ptah-operator"} {
		if host, ok := haRegistryHost(image); ok {
			t.Errorf("haRegistryHost(%q) named registry %q", image, host)
		}
	}
}

func TestHANamespacesDistinct(t *testing.T) {
	t.Parallel()
	if err := haNamespacesDistinct("ptah-system", "ptah-ha", "ptah-foreign", "ptah-proof"); err != nil {
		t.Fatalf("distinct namespaces were refused: %v", err)
	}
	for _, test := range []struct{ operator, operation, foreign, proof, want string }{
		{"ops", "ops", "foreign", "proof", "HA workload namespace must differ from the operator namespace"},
		{"ops", "ha", "ops", "proof", "foreign namespace must differ from the coordination namespace"},
		{"ops", "ha", "foreign", "ops", "upgrade proof namespace must differ from the coordination namespace"},
		{"ops", "ha", "foreign", "ha", "upgrade proof namespace must differ from the HA workload namespace"},
	} {
		err := haNamespacesDistinct(test.operator, test.operation, test.foreign, test.proof)
		if err == nil || err.Error() != test.want {
			t.Errorf("haNamespacesDistinct(%q, %q, %q, %q) = %v, want %q",
				test.operator, test.operation, test.foreign, test.proof, err, test.want)
		}
	}
}

func TestHASchemaDocument(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(haSchemaDocument("ptah-ha"))
	if err != nil {
		t.Fatal(err)
	}
	schema := &ptahv1alpha1.PtahSchema{}
	if err := strictUnmarshal(encoded, schema); err != nil {
		t.Fatalf("the schema document does not decode as a PtahSchema: %v", err)
	}
	spec := schema.Spec
	switch {
	case schema.Namespace != "ptah-ha" || schema.Name != haSchema:
		t.Fatalf("the schema is %s/%s", schema.Namespace, schema.Name)
	case spec.Target.Engine != "PostgreSQL" || spec.Target.CoordinationKey != "e2e/ha/leader-failover":
		t.Fatalf("the target is %+v", spec.Target)
	case spec.Desired.OCIRef != "oci://127.0.0.1:1/e2e/leader-failover:unreachable":
		t.Fatalf("the artifact is %q", spec.Desired.OCIRef)
	case spec.Execution.ServiceAccountName != "default" ||
		len(spec.Execution.ImagePullSecrets) != 1 || spec.Execution.ImagePullSecrets[0].Name != haPullSecret:
		t.Fatalf("the execution is %+v", spec.Execution)
	}
}
