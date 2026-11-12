package e2e

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

// printedMessage is the pointer as the manager writes it, in a condition, an Event and
// status.lastRun.message alike.
const printedMessage = "Apply outcome is uncertain. What Ptah reported is in the Pod log for 24 hours after the Job finishes: " +
	"kubectl logs -n team-a job/ptah-apply-orders-0123 -c ptah"

func TestPrintedLogCommandReadsTheMessageAndNothingElse(t *testing.T) {
	t.Parallel()

	got, err := printedLogCommand(printedMessage)
	if err != nil {
		t.Fatalf("the pointer: %v", err)
	}
	want := []string{"logs", "-n", "team-a", "job/ptah-apply-orders-0123", "-c", "ptah"}
	if !slices.Equal(got, want) {
		t.Fatalf("printedLogCommand() = %q, want %q", got, want)
	}
	for name, message := range map[string]string{
		"no command":        "Apply outcome is uncertain",
		"an empty message":  "",
		"cut short":         strings.TrimSuffix(printedMessage, " -c ptah"),
		"cut inside a word": strings.TrimSuffix(printedMessage, "tah"),
		"text after it":     printedMessage + ".",
		"a sentence after":  printedMessage + ". Then delete the Job.",
		"two commands":      "kubectl logs -n team-a job/first -c ptah or " + printedMessage,
		"another verb":      strings.ReplaceAll(printedMessage, "kubectl logs", "kubectl delete"),
		"a second command":  printedMessage + "; kubectl delete -n team-a job/ptah-apply-orders-0123",
		"a shell behind it": printedMessage + " | sh",
		"another container": strings.ReplaceAll(printedMessage, "-c ptah", "-c fetch"),
		"no container":      strings.ReplaceAll(printedMessage, " -c ptah", ""),
		"a Pod, not a Job":  strings.ReplaceAll(printedMessage, "job/", "pod/"),
		"no namespace":      strings.ReplaceAll(printedMessage, "-n team-a ", ""),
		"another flag":      strings.ReplaceAll(printedMessage, "logs -n", "logs --previous -n"),
		"no Job name":       strings.ReplaceAll(printedMessage, "job/ptah-apply-orders-0123", "job/"),
		"not lower case":    strings.ReplaceAll(printedMessage, "team-a", "Team-A"),
		"glued to a word":   strings.ReplaceAll(printedMessage, " kubectl", "xkubectl"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got, err := printedLogCommand(message); err == nil {
				t.Fatalf("printedLogCommand(%q) accepted it: %q", message, got)
			}
		})
	}
}

func TestReadPrintedLogRunsWhatTheMessagePrints(t *testing.T) {
	t.Parallel()

	var ran [][]string
	kubectl := func(arguments ...string) ([]byte, []byte, error) {
		ran = append(ran, arguments)
		return []byte(refusedLine), []byte("kubectl wrote this to standard error"), nil
	}
	log, err := readPrintedLog(printedMessage, kubectl)
	if err != nil {
		t.Fatalf("readPrintedLog() error = %v", err)
	}
	if string(log) != refusedLine {
		t.Errorf("readPrintedLog() = %q, want standard output and nothing else", log)
	}
	want := []string{"logs", "-n", "team-a", "job/ptah-apply-orders-0123", "-c", "ptah"}
	if len(ran) != 1 || !slices.Equal(ran[0], want) {
		t.Errorf("ran %q, want the one command the message prints", ran)
	}

	// A message that prints something else runs nothing at all.
	ran = nil
	if _, err := readPrintedLog(strings.ReplaceAll(printedMessage, "kubectl logs", "kubectl delete"), kubectl); err == nil {
		t.Error("another verb was accepted")
	}
	if len(ran) != 0 {
		t.Errorf("a refused message ran %q", ran)
	}

	_, err = readPrintedLog(printedMessage, func(...string) ([]byte, []byte, error) {
		return nil, []byte("Error from server (NotFound): jobs.batch not found"), errors.New("exit status 1")
	})
	if err == nil || !strings.Contains(err.Error(), "NotFound") || !strings.Contains(err.Error(), "logs -n team-a") {
		t.Errorf("a command that failed reported %v, want the command and its standard error", err)
	}
}

// outsiderRefusal is what kubectl wrote on kind when a printed command ran as the
// diagnostic reader under a Role that had lost its batch jobs rule, with the outsider's
// name in place of the reader's. Getting the Job is the command's first request, and
// the outsider is refused it the same way.
const outsiderRefusal = `Error from server (Forbidden): jobs.batch "ptah-apply-e2e-configmap-refused-schema-491536d3b7fdc341" ` +
	`is forbidden: User "e2e-diagnostic-outsider" cannot get resource "jobs" in API group "batch" ` +
	`in the namespace "ptah-test-a-1-36-4-local-52ec5dd439-34811-41ccc3bc0f"`

func TestPrintedLogRefusedNamesTheUser(t *testing.T) {
	t.Parallel()

	refused := func(stderr string) error {
		_, err := readPrintedLog(printedMessage, func(...string) ([]byte, []byte, error) {
			return nil, []byte(stderr), errors.New("exit status 1")
		})
		return err
	}
	if err := printedLogRefused(refused(outsiderRefusal), "e2e-diagnostic-outsider"); err != nil {
		t.Fatalf("refused the API server's refusal of the outsider: %v", err)
	}
	for name, test := range map[string]struct {
		err  error
		user string
	}{
		"the command ran":     {nil, "e2e-diagnostic-outsider"},
		"no such Job":         {refused(`Error from server (NotFound): jobs.batch "ptah-apply-orders-0123" not found`), "e2e-diagnostic-outsider"},
		"another user":        {refused(outsiderRefusal), "e2e-diagnostic-reader"},
		"a shorter user name": {refused(outsiderRefusal), "e2e-diagnostic"},
		"no refusal":          {refused(`error: timed out waiting for the condition`), "e2e-diagnostic-outsider"},
		"a refusal unnamed":   {refused(`Error from server (Forbidden): jobs.batch "ptah-apply-orders-0123" is forbidden`), "e2e-diagnostic-outsider"},
		"the name, no answer": {refused(`User "e2e-diagnostic-outsider" cannot get resource "jobs"`), "e2e-diagnostic-outsider"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := printedLogRefused(test.err, test.user); err == nil {
				t.Fatalf("printedLogRefused(%v, %q) accepted it", test.err, test.user)
			}
		})
	}
}

func TestJobFinishedAtReadsWhatTheTTLControllerReads(t *testing.T) {
	t.Parallel()

	finished := metav1.NewTime(time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC))
	job := func(conditions ...batchv1.JobCondition) *batchv1.Job {
		return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "apply"}, Status: batchv1.JobStatus{Conditions: conditions}}
	}
	earlier := metav1.NewTime(finished.Add(-time.Minute))
	for name, test := range map[string]*batchv1.Job{
		"complete": job(batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: finished}),
		"failed":   job(batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: finished}),
		"complete, after its success criteria": job(
			batchv1.JobCondition{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: earlier},
			batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: finished}),
		"failed, after its failure target": job(
			batchv1.JobCondition{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, LastTransitionTime: earlier},
			batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: finished}),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := jobFinishedAt(test)
			if err != nil || !got.Equal(finished.Time) {
				t.Fatalf("jobFinishedAt() = %v, %v, want %v", got, err, finished)
			}
		})
	}
	for name, test := range map[string]*batchv1.Job{
		"no conditions":        job(),
		"complete, not true":   job(batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionFalse, LastTransitionTime: finished}),
		"failed, unknown":      job(batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionUnknown, LastTransitionTime: finished}),
		"success criteria met": job(batchv1.JobCondition{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: finished}),
		"failure target":       job(batchv1.JobCondition{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, LastTransitionTime: finished}),
		"suspended":            job(batchv1.JobCondition{Type: batchv1.JobSuspended, Status: corev1.ConditionTrue, LastTransitionTime: finished}),
		"no time":              job(batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got, err := jobFinishedAt(test); err == nil {
				t.Fatalf("jobFinishedAt() accepted it: %v", got)
			}
		})
	}
}

func TestFailedApplyJobOutlivedShortTTLMeasuresFromTheJob(t *testing.T) {
	t.Parallel()

	finished := metav1.NewTime(time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC))
	deadline := finished.Add(5 * time.Minute)
	job := func() *batchv1.Job {
		return &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "apply", UID: types.UID("the-run")},
			Spec:       batchv1.JobSpec{TTLSecondsAfterFinished: ptr.To(int32(86400))},
			Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: finished},
			}},
		}
	}
	kept := job()
	for name, readAt := range map[string]time.Time{"at the deadline": deadline, "after it": deadline.Add(time.Hour)} {
		if err := failedApplyJobOutlivedShortTTL(kept, job(), readAt); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, test := range map[string]struct {
		mutate func(*batchv1.Job)
		readAt time.Time
	}{
		"read a second early": {readAt: deadline.Add(-time.Second)},
		"read as it finished": {readAt: finished.Time},
		"another Job":         {mutate: func(job *batchv1.Job) { job.UID = "a-later-run" }, readAt: deadline},
		"the short TTL":       {mutate: func(job *batchv1.Job) { job.Spec.TTLSecondsAfterFinished = ptr.To(int32(300)) }, readAt: deadline},
		"no TTL":              {mutate: func(job *batchv1.Job) { job.Spec.TTLSecondsAfterFinished = nil }, readAt: deadline},
		"being deleted":       {mutate: func(job *batchv1.Job) { job.DeletionTimestamp = &finished }, readAt: deadline},
		"not finished":        {mutate: func(job *batchv1.Job) { job.Status.Conditions = nil }, readAt: deadline},
		"finished later than the row took it to": {
			mutate: func(job *batchv1.Job) {
				job.Status.Conditions[0].LastTransitionTime = metav1.NewTime(finished.Add(time.Minute))
			},
			readAt: deadline,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			read := job()
			if test.mutate != nil {
				test.mutate(read)
			}
			if err := failedApplyJobOutlivedShortTTL(kept, read, test.readAt); err == nil {
				t.Fatal("failedApplyJobOutlivedShortTTL() accepted it")
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
