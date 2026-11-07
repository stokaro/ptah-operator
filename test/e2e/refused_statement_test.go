package e2e

import (
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

const refusedLine = `ptah-runner: ptah reported: apply schema changes: failed to execute SQL statement: SQL execution failed: ` +
	`ERROR: could not create unique index "[in the statement]" (SQLSTATE 23505)` + "\n"

func TestFailedApplyLogSaysHoldsTheClaim(t *testing.T) {
	t.Parallel()

	const frame = "PTAH_RUNNER_RESULT_V1 10 abc\n{}\nPTAH_RUNNER_RESULT_END_V1\n"
	if err := failedApplyLogSays([]byte(refusedLine + "ptah-runner: ptah sent the statements\n" + frame)); err != nil {
		t.Errorf("the line the runner writes: %v", err)
	}
	for name, log := range map[string]string{
		"an empty log":      "",
		"no reported error": "ptah-runner: ptah sent the statements and returned an error\n",
		"two runs":          refusedLine + refusedLine,
		"another answer":    "ptah-runner: ptah reported: ERROR: permission denied\n",
		"the statement on its line": strings.TrimSuffix(refusedLine, "\n") +
			` SQL: CREATE UNIQUE INDEX e2e_refused_one_per_customer ON orders (customer_id)` + "\n",
		"the statement on the next line":  refusedLine + "SQL: CREATE UNIQUE INDEX e2e_refused_one_per_customer ON orders (customer_id)\n",
		"the answer without the prefix":   `ERROR: could not create unique index "[in the statement]"` + "\n",
		"the name the statement declares": "ptah-runner: ptah reported: ERROR: could not create unique index \"e2e_refused_one_per_customer\" (SQLSTATE 23505)\n",
		"the name beside the marker":      strings.TrimSuffix(refusedLine, "\n") + " e2e_refused_one_per_customer\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := failedApplyLogSays([]byte(log)); err == nil {
				t.Fatalf("failedApplyLogSays(%q) accepted it", log)
			}
		})
	}
}

func TestFailedApplyPointerSaysNamesTheJobAndTheTime(t *testing.T) {
	t.Parallel()

	const good = "Apply outcome is uncertain. What Ptah reported is in the Pod log for 24 hours after the Job finishes: " +
		"kubectl logs -n team-a job/ptah-apply-orders-0123 -c ptah"
	if err := failedApplyPointerSays(good, "team-a", "ptah-apply-orders-0123"); err != nil {
		t.Errorf("the pointer: %v", err)
	}
	for name, test := range map[string]struct{ message, namespace, job string }{
		"no pointer":       {"Apply outcome is uncertain", "team-a", "ptah-apply-orders-0123"},
		"another Job":      {good, "team-a", "ptah-apply-orders-9999"},
		"another place":    {good, "team-b", "ptah-apply-orders-0123"},
		"the short window": {strings.ReplaceAll(good, "24 hours", "5 minutes"), "team-a", "ptah-apply-orders-0123"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := failedApplyPointerSays(test.message, test.namespace, test.job); err == nil {
				t.Fatalf("failedApplyPointerSays(%q) accepted it", test.message)
			}
		})
	}
}

func TestStatusKeepsTheAnswerOutRefusesWhatItIsFor(t *testing.T) {
	t.Parallel()

	if err := statusKeepsTheAnswerOut("the status", `{"message":"What Ptah reported is in the Pod log"}`); err != nil {
		t.Errorf("a status with the pointer: %v", err)
	}
	for name, document := range map[string]string{
		"an empty reading": "",
		"the answer":       refusedLine,
		"the statement":    `{"message":"CREATE UNIQUE INDEX i ON t (c)"}`,
		"a paraphrase":     `{"message":"the unique index could not be built"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := statusKeepsTheAnswerOut("the status", document); err == nil {
				t.Fatalf("statusKeepsTheAnswerOut(%q) accepted it", document)
			}
		})
	}
}

func TestFailedApplyJobRetainedReadsTheTTL(t *testing.T) {
	t.Parallel()

	job := func(ttl *int32) *batchv1.Job {
		return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "apply"}, Spec: batchv1.JobSpec{TTLSecondsAfterFinished: ttl}}
	}
	if err := failedApplyJobRetained(job(ptr.To(int32(86400)))); err != nil {
		t.Errorf("the long TTL: %v", err)
	}
	for name, ttl := range map[string]*int32{"none": nil, "the short TTL": ptr.To(int32(300)), "another": ptr.To(int32(86399))} {
		if err := failedApplyJobRetained(job(ttl)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestFailedApplyLogReportsWantsOneLine(t *testing.T) {
	t.Parallel()

	if err := failedApplyLogReports([]byte("ptah-runner: started\n" + reportedErrorPrefix + "a refusal\n")); err != nil {
		t.Errorf("one line: %v", err)
	}
	for name, log := range map[string]string{
		"an empty log":                  "",
		"no such line":                  "ptah-runner: started\n",
		"two lines":                     reportedErrorPrefix + "one\n" + reportedErrorPrefix + "two\n",
		"quoted, not a line of its own": "the log said " + reportedErrorPrefix + "one\n",
	} {
		if err := failedApplyLogReports([]byte(log)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestFinishedJobKeepsShortTTLReadsTheTTL(t *testing.T) {
	t.Parallel()

	job := func(ttl *int32) *batchv1.Job {
		return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "apply"}, Spec: batchv1.JobSpec{TTLSecondsAfterFinished: ttl}}
	}
	if err := finishedJobKeepsShortTTL(job(ptr.To(int32(300)))); err != nil {
		t.Errorf("the short TTL: %v", err)
	}
	for name, ttl := range map[string]*int32{"none": nil, "the long TTL": ptr.To(int32(86400)), "another": ptr.To(int32(301))} {
		if err := finishedJobKeepsShortTTL(job(ttl)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestCleanupScheduledReadsEitherTTL(t *testing.T) {
	t.Parallel()

	for ttl, want := range map[int64]bool{300: true, 86400: true, 0: false, 301: false, 86399: false} {
		if got := cleanupScheduled(ttl); got != want {
			t.Errorf("cleanupScheduled(%d) = %v, want %v", ttl, got, want)
		}
	}
}
