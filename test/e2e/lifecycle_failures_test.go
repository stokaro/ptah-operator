package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	celgo "github.com/google/cel-go/cel"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

// lifecycleFailureValidHookStatus is the valid fixture of
// hack/failed-hook-evidence-selftest.sh: revision 7 failed in the reconcile
// hook Job at weight 0 (null), after the ServiceAccount of the same hook ran,
// and before the later hook at weight 5 ran.
const lifecycleFailureValidHookStatus = `{
  "version": 7,
  "info": {"status": "failed"},
  "hooks": [
    {
      "name": "ptah-crd-reconcile",
      "kind": "ServiceAccount",
      "weight": -110,
      "events": ["pre-install", "pre-upgrade", "pre-rollback"],
      "last_run": {
        "phase": "Succeeded",
        "started_at": "2026-01-01T00:00:00Z",
        "completed_at": "2026-01-01T00:00:01Z"
      }
    },
    {
      "name": "ptah-crd-reconcile",
      "kind": "Job",
      "weight": null,
      "events": ["pre-install", "pre-upgrade", "pre-rollback"],
      "last_run": {
        "phase": "Failed",
        "started_at": "2026-01-01T00:00:02Z",
        "completed_at": "2026-01-01T00:00:03Z"
      }
    },
    {
      "name": "later-hook",
      "kind": "Job",
      "weight": 5,
      "events": ["pre-upgrade"],
      "last_run": {"phase": ""}
    }
  ]
}`

func lifecycleFailureFixture(t *testing.T, document string) map[string]any {
	t.Helper()
	var fixture map[string]any
	if err := json.Unmarshal([]byte(document), &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func lifecycleFailureHook(fixture map[string]any, index int) map[string]any {
	return fixture["hooks"].([]any)[index].(map[string]any)
}

func lifecycleFailureEncode(t *testing.T, fixture map[string]any) []byte {
	t.Helper()
	encoded, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// Every row of hack/failed-hook-evidence-selftest.sh, and the clauses
// hack/verify-kubernetes-support.go pinned the filter to, held against the Go
// predicate that replaces the filter.
func TestLifecycleFailedHookEvidence(t *testing.T) {
	t.Parallel()
	const revision, name = 7, "ptah-crd-reconcile"
	if err := lifecycleFailedHookEvidence([]byte(lifecycleFailureValidHookStatus), revision, name); err != nil {
		t.Fatalf("the valid fixture was refused: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
		accept bool
	}{
		// The self-test's refusals, one per line of it.
		{name: "wrong-revision", mutate: func(f map[string]any) { f["version"] = 8 }},
		{name: "not-failed", mutate: func(f map[string]any) { f["info"].(map[string]any)["status"] = "deployed" }},
		{name: "wrong-name", mutate: func(f map[string]any) { lifecycleFailureHook(f, 1)["name"] = "other-reconcile" }},
		{name: "wrong-kind", mutate: func(f map[string]any) { lifecycleFailureHook(f, 1)["kind"] = "Pod" }},
		{name: "wrong-weight", mutate: func(f map[string]any) { lifecycleFailureHook(f, 1)["weight"] = -60 }},
		{name: "wrong-event", mutate: func(f map[string]any) { lifecycleFailureHook(f, 1)["events"] = []any{"post-upgrade"} }},
		{name: "never-started", mutate: func(f map[string]any) {
			lifecycleFailureHook(f, 1)["last_run"].(map[string]any)["started_at"] = ""
		}},
		{name: "two-failures", mutate: func(f map[string]any) {
			lifecycleFailureHook(f, 0)["last_run"].(map[string]any)["phase"] = "Failed"
		}},
		{name: "no-failure", mutate: func(f map[string]any) {
			lifecycleFailureHook(f, 1)["last_run"].(map[string]any)["phase"] = "Succeeded"
		}},
		{name: "later-hook-ran", mutate: func(f map[string]any) {
			lifecycleFailureHook(f, 2)["last_run"] = lifecycleFailureHook(f, 0)["last_run"]
		}},
		{name: "malformed-later-weight", mutate: func(f map[string]any) { lifecycleFailureHook(f, 2)["weight"] = "not-a-weight" }},
		// The clauses the rows above leave to the filter's own text.
		{name: "never completed", mutate: func(f map[string]any) {
			delete(lifecycleFailureHook(f, 1)["last_run"].(map[string]any), "completed_at")
		}},
		{name: "no hooks at all", mutate: func(f map[string]any) { delete(f, "hooks") }},
		{name: "the failed hook's weight as a string that is not zero", mutate: func(f map[string]any) {
			lifecycleFailureHook(f, 1)["weight"] = "-60"
		}},
		// jq's rules the predicate keeps: a weight written as a string is read
		// as the number it spells, and a hook that is not pre-upgrade is never
		// weighed, so what its weight says cannot refuse the evidence.
		{name: "the failed hook's weight written as the string 0", accept: true, mutate: func(f map[string]any) {
			lifecycleFailureHook(f, 1)["weight"] = "0"
		}},
		{name: "a later hook outside pre-upgrade with a malformed weight", accept: true, mutate: func(f map[string]any) {
			hook := lifecycleFailureHook(f, 2)
			hook["events"] = []any{"post-upgrade"}
			hook["weight"] = "not-a-weight"
		}},
		{name: "a pre-upgrade hook weighted before the refusal ran", accept: true, mutate: func(f map[string]any) {
			lifecycleFailureHook(f, 2)["weight"] = -5
			lifecycleFailureHook(f, 2)["last_run"] = lifecycleFailureHook(f, 0)["last_run"]
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := lifecycleFailureFixture(t, lifecycleFailureValidHookStatus)
			test.mutate(fixture)
			err := lifecycleFailedHookEvidence(lifecycleFailureEncode(t, fixture), revision, name)
			if accepted := err == nil; accepted != test.accept {
				t.Fatalf("accepted = %t, want %t: %v", accepted, test.accept, err)
			}
		})
	}
	if lifecycleFailedHookEvidence([]byte("not json"), revision, name) == nil {
		t.Fatal("a status that is not JSON was accepted")
	}
}

// The rows of TestLateFailureRevisionClassification, which held the filter
// prove_late_failure_recovery carried.
func TestLifecycleLateFailureEvidence(t *testing.T) {
	t.Parallel()
	const (
		expectedRevision      = 4
		expectedReconcileName = "ptah-operator-crd-v1-0123456789ab"
	)
	hook := func(name, kind string, weight any, phase string) map[string]any {
		return map[string]any{
			"name": name, "kind": kind, "weight": weight,
			"events": []any{"pre-install", "pre-upgrade", "pre-rollback"},
			"last_run": map[string]any{
				"phase": phase, "started_at": "2026-09-04T12:00:00Z", "completed_at": "2026-09-04T12:00:01Z",
			},
		}
	}
	fixture := func() map[string]any {
		return map[string]any{
			"version": expectedRevision,
			"info":    map[string]any{"status": "failed"},
			"hooks": []any{
				hook(expectedReconcileName, "ServiceAccount", -110, "Succeeded"),
				hook(expectedReconcileName, "Job", nil, "Succeeded"),
			},
		}
	}
	reconcile := func(status map[string]any) map[string]any { return status["hooks"].([]any)[1].(map[string]any) }
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
		want   bool
	}{
		{name: "the reconcile hook succeeded and the release failed after it", mutate: func(map[string]any) {}, want: true},
		{name: "the reconcile hook failed", mutate: func(status map[string]any) {
			reconcile(status)["last_run"].(map[string]any)["phase"] = "Failed"
		}},
		{name: "another hook failed", mutate: func(status map[string]any) {
			status["hooks"] = append(status["hooks"].([]any), hook("other-hook", "Job", 5, "Failed"))
		}},
		{name: "the reconcile hook never ran", mutate: func(status map[string]any) {
			reconcile(status)["last_run"].(map[string]any)["phase"] = ""
		}},
		{name: "another release's reconcile hook", mutate: func(status map[string]any) {
			reconcile(status)["name"] = "other-reconcile"
		}},
		{name: "two reconcile hooks", mutate: func(status map[string]any) {
			status["hooks"] = append(status["hooks"].([]any), hook(expectedReconcileName, "Job", nil, "Succeeded"))
		}},
		{name: "the reconcile hook is not a pre-upgrade hook", mutate: func(status map[string]any) {
			reconcile(status)["events"] = []any{"pre-install"}
		}},
		{name: "another revision", mutate: func(status map[string]any) { status["version"] = expectedRevision + 1 }},
		{name: "the release did not fail", mutate: func(status map[string]any) {
			status["info"].(map[string]any)["status"] = "deployed"
		}},
		// jq's `if type == "array" then . else [] end` reads hooks that are
		// not a list as none, which leaves no reconcile hook to have run.
		{name: "hooks that are not a list", mutate: func(status map[string]any) { status["hooks"] = "none" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			status := fixture()
			test.mutate(status)
			encoded, err := json.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}
			err = lifecycleLateFailureEvidence(encoded, expectedRevision, expectedReconcileName)
			if got := err == nil; got != test.want {
				t.Fatalf("late failure revision classification = %t, want %t: %v", got, test.want, err)
			}
		})
	}
}

// The rows of TestLateFailureRetryRejectsChangedCandidate: the retry is the
// candidate that failed late, and a checksum that cannot be taken refuses it.
func TestLifecycleFailureCandidateUnchanged(t *testing.T) {
	t.Parallel()
	for _, mutation := range []string{"none", "chart", "values", "image", "chart checksum failure", "values checksum failure"} {
		t.Run(mutation, func(t *testing.T) {
			t.Parallel()
			directory := t.TempDir()
			chartPath := filepath.Join(directory, "candidate.tgz")
			valuesPath := filepath.Join(directory, "candidate-values.json")
			for path, contents := range map[string]string{chartPath: "immutable chart fixture", valuesPath: `{"image":"candidate"}`} {
				if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := lifecycleFailureReadCandidate(chartPath, valuesPath, "candidate-image")
			if err != nil {
				t.Fatal(err)
			}
			image := "candidate-image"
			switch mutation {
			case "chart":
				err = os.WriteFile(chartPath, []byte("replacement chart"), 0o600)
			case "values":
				err = os.WriteFile(valuesPath, []byte(`{"image":"replacement"}`), 0o600)
			case "image":
				image = "replacement-image"
			case "chart checksum failure":
				err = os.Remove(chartPath)
			case "values checksum failure":
				err = os.Remove(valuesPath)
			}
			if err != nil {
				t.Fatal(err)
			}
			after, readErr := lifecycleFailureReadCandidate(chartPath, valuesPath, image)
			accepted := readErr == nil && lifecycleFailureCandidateUnchanged(before, after)
			if want := mutation == "none"; accepted != want {
				t.Fatalf("candidate retry accepted = %t, want %t (%v)", accepted, want, readErr)
			}
			if strings.HasSuffix(mutation, "checksum failure") && !strings.Contains(readErr.Error(), "could not checksum the late-failure candidate") {
				t.Fatalf("the checksum failure said %q", readErr)
			}
		})
	}
}

// The rows of TestLateFailureBlockerMatchesOnlyTheCandidateDeployments. The
// blocker stands in for whatever fails after the hook stopped the runtime, so
// it has to let the hook's own scale-down through and refuse only Helm's write
// of the candidate. Both match conditions must hold for the webhook to be
// called, as the API server evaluates them, and they are read from the
// document the phase applies.
func TestLifecycleFailureBlockerMatchesOnlyTheCandidateDeployments(t *testing.T) {
	t.Parallel()
	const (
		candidate   = "registry.invalid/operator@sha256:candidate"
		predecessor = "registry.invalid/operator@sha256:predecessor"
	)
	blocker := lifecycleFailureBlocker("ptah-system", "ptah-operator", "ptah-operator-cert-rotator", candidate)
	webhook := blocker["webhooks"].([]any)[0].(map[string]any)
	if webhook["name"] != lifecycleFailureBlockerName || webhook["failurePolicy"] != "Fail" {
		t.Fatalf("the blocker webhook is %v with failure policy %v", webhook["name"], webhook["failurePolicy"])
	}
	environment, err := celgo.NewEnv(
		celgo.Variable("request", celgo.DynType),
		celgo.Variable("object", celgo.DynType),
	)
	if err != nil {
		t.Fatal(err)
	}
	conditions := webhook["matchConditions"].([]any)
	if len(conditions) != 2 {
		t.Fatalf("the blocker carries %d match conditions, want 2", len(conditions))
	}
	var programs []celgo.Program
	for _, condition := range conditions {
		expression := condition.(map[string]any)["expression"].(string)
		ast, issues := environment.Compile(expression)
		if issues != nil && issues.Err() != nil {
			t.Fatalf("compile %q: %v", expression, issues.Err())
		}
		program, err := environment.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		programs = append(programs, program)
	}
	deployment := func(images ...string) map[string]any {
		containers := make([]any, 0, len(images))
		for _, image := range images {
			containers = append(containers, map[string]any{"image": image})
		}
		return map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": containers}}}}
	}
	request := func(namespace, name string) map[string]any {
		return map[string]any{"namespace": namespace, "name": name}
	}
	for _, test := range []struct {
		name    string
		request map[string]any
		object  any
		want    bool
	}{
		{name: "Helm writes the candidate controller", request: request("ptah-system", "ptah-operator"), object: deployment(candidate), want: true},
		{name: "Helm writes the candidate rotator", request: request("ptah-system", "ptah-operator-cert-rotator"), object: deployment(candidate), want: true},
		{name: "the hook scales the predecessor to zero", request: request("ptah-system", "ptah-operator"), object: deployment(predecessor)},
		{name: "another Deployment carries the candidate", request: request("ptah-system", "other"), object: deployment(candidate)},
		{name: "the controller name in another namespace", request: request("other", "ptah-operator"), object: deployment(candidate)},
		{name: "a request without an object", request: request("ptah-system", "ptah-operator"), object: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			matched := true
			for _, program := range programs {
				result, _, err := program.Eval(map[string]any{"request": test.request, "object": test.object})
				if err != nil {
					t.Fatalf("evaluate the blocker: %v", err)
				}
				value, ok := result.Value().(bool)
				if !ok {
					t.Fatalf("the blocker's condition returned %T(%v), want bool", result.Value(), result.Value())
				}
				matched = matched && value
			}
			if matched != test.want {
				t.Fatalf("the blocker matched = %t, want %t", matched, test.want)
			}
		})
	}
}

func TestLifecycleFailureHistory(t *testing.T) {
	t.Parallel()
	if revision, err := lifecycleFailureHistoryRevision([]byte(`[{"revision":3,"status":"deployed"}]`)); err != nil || revision != 3 {
		t.Fatalf("history revision = %d, %v; want 3", revision, err)
	}
	for _, history := range []string{`[]`, `[{"revision":0}]`, `[{"revision":"3"}]`, `{}`, `[{}]`, `not json`} {
		if _, err := lifecycleFailureHistoryRevision([]byte(history)); err == nil {
			t.Errorf("history %s read as a revision", history)
		}
	}
	for _, test := range []struct {
		history string
		accept  bool
	}{
		{`[{"revision":6,"status":"pending-rollback"}]`, true},
		{`[{"revision":6,"status":"failed"}]`, true},
		// jq's `.status != "deployed"` holds for a status that is absent.
		{`[{"revision":6}]`, true},
		{`[{"revision":6,"status":"deployed"}]`, false},
		{`[{"revision":6,"status":"superseded"}]`, false},
		// A refusal before the hook writes no revision.
		{`[{"revision":5,"status":"deployed"}]`, false},
		{`[{"revision":5,"status":"failed"}]`, false},
		{`[{"revision":7,"status":"failed"}]`, false},
		{`[]`, false},
	} {
		if err := lifecycleFailureRefusedRollbackHistory([]byte(test.history), 5); (err == nil) != test.accept {
			t.Errorf("refused rollback history %s accepted = %t, want %t: %v", test.history, err == nil, test.accept, err)
		}
	}
}

func TestLifecycleFailureDeploymentOn(t *testing.T) {
	t.Parallel()
	const image, other = "registry.invalid/operator@sha256:current", "registry.invalid/operator@sha256:other"
	deployment := func(replicas *int32, images ...string) *appsv1.Deployment {
		d := &appsv1.Deployment{}
		d.Spec.Replicas = replicas
		for _, containerImage := range images {
			d.Spec.Template.Spec.Containers = append(d.Spec.Template.Spec.Containers, corev1.Container{Image: containerImage})
		}
		return d
	}
	for _, test := range []struct {
		name       string
		deployment *appsv1.Deployment
		stopped    bool
		want       bool
	}{
		{"stopped on the predecessor", deployment(ptr.To[int32](0), image, image), true, true},
		{"still running after the late failure", deployment(ptr.To[int32](1), image), true, false},
		{"stopped on another template", deployment(ptr.To[int32](0), image, other), true, false},
		{"no replica count", deployment(nil, image), true, false},
		{"back after the rollback", deployment(ptr.To[int32](2), image), false, true},
		{"still stopped after the rollback", deployment(ptr.To[int32](0), image), false, false},
		{"back on another image", deployment(ptr.To[int32](1), other), false, false},
	} {
		if got := lifecycleFailureDeploymentOn(test.deployment, image, test.stopped); got != test.want {
			t.Errorf("%s: %t, want %t", test.name, got, test.want)
		}
	}
}

func TestLifecycleFailureRuntimePodsGone(t *testing.T) {
	t.Parallel()
	pod := func(account string) corev1.Pod {
		p := corev1.Pod{}
		p.Spec.ServiceAccountName = account
		return p
	}
	if !lifecycleFailureRuntimePodsGone([]corev1.Pod{pod("registry"), pod("")}, "ptah-operator", "ptah-operator-cert-rotator") {
		t.Fatal("Pods of other accounts were read as a runtime Pod")
	}
	for _, account := range []string{"ptah-operator", "ptah-operator-cert-rotator"} {
		if lifecycleFailureRuntimePodsGone([]corev1.Pod{pod("registry"), pod(account)}, "ptah-operator", "ptah-operator-cert-rotator") {
			t.Errorf("a Pod running as %s was missed", account)
		}
	}
}

func TestLifecycleFailureSharedNamespaceProbe(t *testing.T) {
	t.Parallel()
	probe := lifecycleFailureSharedNamespaceProbe()
	spec := probe["spec"].(map[string]any)
	if probe["kind"] != "CronJob" || probe["metadata"].(map[string]any)["name"] != lifecycleFailureSharedProbe || spec["suspend"] != true {
		t.Fatalf("the probe is not the suspended CronJob %s: %v", lifecycleFailureSharedProbe, probe)
	}
	want := "release namespace ptah-e2e runs workloads without app.kubernetes.io/instance=ptah-e2e (CronJob/" +
		lifecycleFailureSharedProbe + ")"
	if got := lifecycleFailureSharedNamespaceRefusal("ptah-e2e", "ptah-e2e", lifecycleFailureSharedProbe); got != want {
		t.Fatalf("refusal = %q, want %q", got, want)
	}
}

func TestLifecycleFailureStatusSummary(t *testing.T) {
	t.Parallel()
	summary := lifecycleFailureStatusSummary([]byte(lifecycleFailureValidHookStatus))
	var decoded struct {
		Version json.Number      `json:"version"`
		Status  string           `json:"status"`
		Hooks   []map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(summary), &decoded); err != nil {
		t.Fatalf("summary %s: %v", summary, err)
	}
	if decoded.Version != "7" || decoded.Status != "failed" || len(decoded.Hooks) != 3 || decoded.Hooks[1]["name"] != "ptah-crd-reconcile" {
		t.Fatalf("summary %s", summary)
	}
	if got := lifecycleFailureStatusSummary([]byte("not json")); got != "not json" {
		t.Fatalf("a document that is not JSON summarized as %q", got)
	}
}
