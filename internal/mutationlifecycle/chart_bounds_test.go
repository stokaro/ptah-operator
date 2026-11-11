package mutationlifecycle_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/yaml"

	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
)

// dispatchStepsFloor is the least the critical section has to leave besides
// the Job-write webhook's wait. Four requests share it with that wait: the
// mark, the create's own handling and storage, the read-back and the record.
// A section equal to the webhook timeout, or longer by a moment, gives them
// nothing, and the row refuses it.
const dispatchStepsFloor = 5 * time.Second

// leaseReleaseFloor is the least the Pod's grace period has to leave after the
// manager's graceful shutdown: the leader Lease is released after every
// runnable has stopped, and the kubelet's kill must not race it. 0.2.2 left
// ten seconds, and the release landed.
const leaseReleaseFloor = 10 * time.Second

// A Job-write webhook slower than the dispatch critical section made the
// dispatcher's own create time out on the client side. The create read as
// unknown, and the claim was retired with its approval spent, with one manager
// and no race, while the API server was about to answer (#973). The section
// has to outlast every timeout the chart lets an install give that webhook.
func TestTheCriticalSectionOutlastsJobWriteAdmission(t *testing.T) {
	t.Parallel()

	// The template has to give the Job-write webhook the value read below, or
	// what the row holds is not what the API server is told.
	if got := controllerWriteWebhookTimeout(t); got != "{{ .Values.webhook.controllerWriteTimeoutSeconds }}" {
		t.Fatalf("the vcontrollerwrite webhook's timeoutSeconds is %q, not webhook.controllerWriteTimeoutSeconds", got)
	}

	accepted := map[string]int64{
		"the values schema's bound": schemaControllerWriteTimeoutBound(t),
		"the values default":        defaultControllerWriteTimeout(t),
	}
	for source, seconds := range accepted {
		admission := time.Duration(seconds) * time.Second
		if slack := mutationlifecycle.DispatchCriticalSection - admission; slack < dispatchStepsFloor {
			t.Errorf("DispatchCriticalSection %s leaves %s after %s of Job-write admission (%s), want at least %s",
				mutationlifecycle.DispatchCriticalSection, slack, admission, source, dispatchStepsFloor)
		}
	}
}

// slowWebhookTrace is a create whose Job-write admission takes as long as the
// chart lets the webhook take, after which the API server answers.
type slowWebhookTrace struct {
	familyTrace
	admission time.Duration
	answer    error
}

func (f *slowWebhookTrace) Create(ctx context.Context, job *batchv1.Job) error {
	select {
	case <-time.After(f.admission):
	case <-ctx.Done():
		f.calls = append(f.calls, "create abandoned")
		return ctx.Err()
	}
	if f.answer != nil {
		f.calls = append(f.calls, "create")
		return f.answer
	}
	return f.familyTrace.Create(ctx, job)
}

// The driver, not only the constant: a dispatch whose Job-write webhook takes
// the longest timeout the chart accepts still gets the API server's answer,
// and settles the claim by it. Admitted, the Job is recorded; refused because
// the webhook did not answer in time, the mark is taken back and the approval
// kept. Before #973, the client gave up at 15 seconds and the claim was
// retired as unaccounted either way.
func TestAJobWriteWebhookAsSlowAsTheChartAllowsIsAnswered(t *testing.T) {
	t.Parallel()

	admission := time.Duration(schemaControllerWriteTimeoutBound(t)) * time.Second
	for _, row := range []struct {
		name   string
		answer error
		want   []string
	}{
		{
			name: "admitted",
			want: []string{"authorize", "lease", "stage", "build", "consume", "mark", "create", "confirm", "record"},
		},
		{
			name:   "refused when the webhook timed out",
			answer: webhookTimedOut(),
			want:   []string{"authorize", "lease", "stage", "build", "consume", "mark", "create", "withdraw"},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				family := &slowWebhookTrace{familyTrace: familyTrace{claim: mutatingClaim()}, admission: admission, answer: row.answer}
				_, err := (mutationlifecycle.Driver{}).Dispatch(context.Background(), family)
				if !slices.Equal(family.calls, row.want) {
					t.Fatalf("after %s of admission the family was asked %v, want %v (Dispatch error %v)",
						admission, family.calls, row.want, err)
				}
			})
		})
	}
}

// A stopping manager finishes a dispatch it began, so its graceful shutdown is
// DispatchStopBound, and the Pod's grace period has to cover that shutdown and
// the Lease release after it.
func TestTheManagerPodOutlastsADispatchItStops(t *testing.T) {
	t.Parallel()

	if mutationlifecycle.DispatchStopBound <= mutationlifecycle.DispatchCriticalSection {
		t.Fatalf("DispatchStopBound %s does not outlast DispatchCriticalSection %s, so a stop cuts the write after a create",
			mutationlifecycle.DispatchStopBound, mutationlifecycle.DispatchCriticalSection)
	}
	grace := managerTerminationGrace(t)
	if slack := grace - mutationlifecycle.DispatchStopBound; slack < leaseReleaseFloor {
		t.Fatalf("the manager Pod's grace period %s leaves %s after a %s shutdown, want at least %s for the Lease release",
			grace, slack, mutationlifecycle.DispatchStopBound, leaseReleaseFloor)
	}
}

// chartFile reads a file of the chart this repository ships.
func chartFile(t *testing.T, name string) []byte {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	path := filepath.Join(filepath.Dir(filename), "..", "..", "charts", "ptah-operator", filepath.FromSlash(name))
	data, err := os.ReadFile(path) //nolint:gosec // The path is the repository's own chart.
	if err != nil {
		t.Fatalf("read the chart's %s: %v", name, err)
	}
	return data
}

// schemaControllerWriteTimeoutBound is the largest
// webhook.controllerWriteTimeoutSeconds the values schema admits.
func schemaControllerWriteTimeoutBound(t *testing.T) int64 {
	t.Helper()
	var schema struct {
		Properties struct {
			Webhook struct {
				Properties map[string]struct {
					Const   *int64 `json:"const"`
					Maximum *int64 `json:"maximum"`
				} `json:"properties"`
			} `json:"webhook"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(chartFile(t, "values.schema.json"), &schema); err != nil {
		t.Fatalf("decode the values schema: %v", err)
	}
	field, ok := schema.Properties.Webhook.Properties["controllerWriteTimeoutSeconds"]
	switch {
	case !ok:
		t.Fatal("the values schema does not declare webhook.controllerWriteTimeoutSeconds")
	case field.Const != nil:
		return *field.Const
	case field.Maximum != nil:
		return *field.Maximum
	}
	t.Fatal("the values schema sets no upper bound on webhook.controllerWriteTimeoutSeconds, so no critical section can be held above it")
	return 0
}

// defaultControllerWriteTimeout is the webhook.controllerWriteTimeoutSeconds
// values.yaml gives an install that sets none.
func defaultControllerWriteTimeout(t *testing.T) int64 {
	t.Helper()
	var values struct {
		Webhook struct {
			ControllerWriteTimeoutSeconds *int64 `json:"controllerWriteTimeoutSeconds"`
		} `json:"webhook"`
	}
	if err := yaml.Unmarshal(chartFile(t, "values.yaml"), &values); err != nil {
		t.Fatalf("decode the chart values: %v", err)
	}
	if values.Webhook.ControllerWriteTimeoutSeconds == nil {
		t.Fatal("values.yaml sets no webhook.controllerWriteTimeoutSeconds")
	}
	return *values.Webhook.ControllerWriteTimeoutSeconds
}

// controllerWriteWebhookTimeout is the text the webhook template gives the
// vcontrollerwrite webhook's timeoutSeconds.
func controllerWriteWebhookTimeout(t *testing.T) string {
	t.Helper()
	var entry []string
	inside := false
	for line := range strings.SplitSeq(string(chartFile(t, "templates/webhook.yaml")), "\n") {
		if strings.HasPrefix(line, "  - name: ") || !strings.HasPrefix(line, " ") {
			inside = line == "  - name: vcontrollerwrite.operator.ptah.run"
		}
		if inside {
			entry = append(entry, line)
		}
	}
	if len(entry) == 0 {
		t.Fatal("the webhook template has no vcontrollerwrite.operator.ptah.run entry")
	}
	var timeouts []string
	for _, line := range entry {
		if value, ok := strings.CutPrefix(line, "    timeoutSeconds: "); ok {
			timeouts = append(timeouts, value)
		}
	}
	if len(timeouts) != 1 {
		t.Fatalf("the vcontrollerwrite webhook entry sets timeoutSeconds %d times, want once", len(timeouts))
	}
	return timeouts[0]
}

var podGraceLine = regexp.MustCompile(`(?m)^      terminationGracePeriodSeconds: ([0-9]+)$`)

// managerTerminationGrace is the grace period the chart gives the manager Pod.
func managerTerminationGrace(t *testing.T) time.Duration {
	t.Helper()
	matches := podGraceLine.FindAllSubmatch(chartFile(t, "templates/deployment.yaml"), -1)
	if len(matches) != 1 {
		t.Fatalf("the manager Deployment template sets terminationGracePeriodSeconds %d times, want once", len(matches))
	}
	seconds, err := strconv.Atoi(string(matches[0][1]))
	if err != nil {
		t.Fatalf("parse the manager Pod's terminationGracePeriodSeconds: %v", err)
	}
	return time.Duration(seconds) * time.Second
}
