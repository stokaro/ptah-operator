package e2e

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// lifecycleRuntimeDocument reads a document the way the API server's JSON
// decodes into an unstructured object.
func lifecycleRuntimeDocument(t *testing.T, document string) map[string]any {
	t.Helper()
	var parsed map[string]any
	if err := json.Unmarshal([]byte(document), &parsed); err != nil {
		t.Fatalf("parse %s: %v", document, err)
	}
	return parsed
}

// lifecycleRuntimeMutations runs a predicate over a document that satisfies
// it and over one mutation per clause that must not.
func lifecycleRuntimeMutations(t *testing.T, base string, accept func(map[string]any) bool,
	mutations map[string]func(map[string]any),
) {
	t.Helper()
	if !accept(lifecycleRuntimeDocument(t, base)) {
		t.Fatal("the reading the predicate is written for was refused")
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			document := lifecycleRuntimeDocument(t, base)
			mutate(document)
			if accept(document) {
				t.Fatal("the mistake passed")
			}
		})
	}
}

func lifecycleRuntimeField(document map[string]any, path ...string) map[string]any {
	current := document
	for _, key := range path {
		next, ok := current[key].(map[string]any)
		if !ok {
			next = map[string]any{}
			current[key] = next
		}
		current = next
	}
	return current
}

// The runtime guard fixtures hack/e2e-static.sh fed hack/e2e-crd-init-guard.jq:
// three Pods of release ptah-e2e, two controllers and one rotator.
func lifecycleRuntimeGuardPods(status func(index int) corev1.PodStatus) []corev1.Pod {
	pod := func(uid, component string, index int) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{UID: types.UID(uid), Labels: map[string]string{
				"app.kubernetes.io/instance": "ptah-e2e", "app.kubernetes.io/component": component,
			}},
			Status: status(index),
		}
	}
	return []corev1.Pod{
		pod("controller-a", "controller", 0), pod("controller-b", "controller", 1), pod("rotator", "certificate-rotation", 2),
	}
}

func lifecycleRuntimeFailedStatus() corev1.PodStatus {
	return corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "main", Started: new(false),
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}},
		}},
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name: lifecycleRuntimeVerifierContainer, RestartCount: 1,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}},
		}},
	}
}

func TestLifecycleRuntimeInitGuardFixtures(t *testing.T) {
	t.Parallel()
	// pending: Pods with no status yet. A transient empty main status is not
	// proof of a blocked runtime.
	pending := lifecycleRuntimeGuard(lifecycleRuntimeGuardPods(func(int) corev1.PodStatus { return corev1.PodStatus{} }),
		"ptah-e2e", "all", 3)
	if pending.ExplicitVerifierFailures {
		t.Fatal("transient Pending Pods satisfy the CRD runtime guard")
	}
	// failed: every verifier refused, and no main container started.
	failed := lifecycleRuntimeGuard(lifecycleRuntimeGuardPods(func(int) corev1.PodStatus { return lifecycleRuntimeFailedStatus() }),
		"ptah-e2e", "all", 3)
	if failed.PodCount != 3 || !failed.ExplicitVerifierFailures || !failed.MainContainersNeverStarted {
		t.Fatalf("explicit init failures do not satisfy the stable guard state: %+v", failed)
	}
	if failed.PodUIDs != "controller-a,controller-b,rotator" {
		t.Fatalf("PodUIDs = %q", failed.PodUIDs)
	}
	// running and terminated: the first Pod's main container started.
	for name, main := range map[string]corev1.ContainerStatus{
		"running": {Name: "main", Ready: true, Started: new(true),
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		"terminated": {Name: "main", RestartCount: 1, Started: new(false),
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			state := lifecycleRuntimeGuard(lifecycleRuntimeGuardPods(func(index int) corev1.PodStatus {
				status := lifecycleRuntimeFailedStatus()
				if index == 0 {
					status.ContainerStatuses[0] = main
				}
				return status
			}), "ptah-e2e", "all", 3)
			if state.MainContainersNeverStarted {
				t.Fatalf("runtime guard missed main-container start evidence in the %s fixture", name)
			}
		})
	}
}

func TestLifecycleRuntimeInitGuardClauses(t *testing.T) {
	t.Parallel()
	failed := func(int) corev1.PodStatus { return lifecycleRuntimeFailedStatus() }
	// The controller scope counts the two controllers and not the rotator.
	controller := lifecycleRuntimeGuard(lifecycleRuntimeGuardPods(failed), "ptah-e2e", "controller", 2)
	if controller.PodCount != 2 || !controller.ExplicitVerifierFailures || controller.PodUIDs != "controller-a,controller-b" {
		t.Fatalf("the controller scope read %+v", controller)
	}
	if lifecycleRuntimeGuard(lifecycleRuntimeGuardPods(failed), "ptah-e2e", "all", 2).ExplicitVerifierFailures {
		t.Fatal("three blocked Pods passed as the two the guard expected")
	}
	if state := lifecycleRuntimeGuard(lifecycleRuntimeGuardPods(failed), "another", "all", 3); state.PodCount != 0 || state.ExplicitVerifierFailures {
		t.Fatalf("Pods of another release were read: %+v", state)
	}
	// A verifier that failed in its current state counts as much as one that
	// failed before a restart.
	current := func(int) corev1.PodStatus {
		status := lifecycleRuntimeFailedStatus()
		status.InitContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2}}
		status.InitContainerStatuses[0].LastTerminationState = corev1.ContainerState{}
		return status
	}
	if !lifecycleRuntimeGuard(lifecycleRuntimeGuardPods(current), "ptah-e2e", "all", 3).ExplicitVerifierFailures {
		t.Fatal("a verifier terminated nonzero in its current state was not read as a failure")
	}
	for name, mutate := range map[string]func(*corev1.PodStatus){
		"verifier exited zero": func(status *corev1.PodStatus) {
			status.InitContainerStatuses[0].LastTerminationState.Terminated.ExitCode = 0
		},
		"another init container failed": func(status *corev1.PodStatus) {
			status.InitContainerStatuses[0].Name = "wait-for-crds"
		},
		"verifier never terminated": func(status *corev1.PodStatus) {
			status.InitContainerStatuses[0].LastTerminationState = corev1.ContainerState{}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			state := lifecycleRuntimeGuard(lifecycleRuntimeGuardPods(func(index int) corev1.PodStatus {
				status := lifecycleRuntimeFailedStatus()
				if index == 1 {
					mutate(&status)
				}
				return status
			}), "ptah-e2e", "all", 3)
			if state.ExplicitVerifierFailures {
				t.Fatal("a Pod without an explicit verifier failure passed as blocked")
			}
		})
	}
	for name, mutate := range map[string]func(*corev1.ContainerStatus){
		"restarted": func(status *corev1.ContainerStatus) { status.RestartCount = 1 },
		"started":   func(status *corev1.ContainerStatus) { status.Started = new(true) },
		"terminated": func(status *corev1.ContainerStatus) {
			status.State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}
		},
		"ran before": func(status *corev1.ContainerStatus) {
			status.LastTerminationState.Running = &corev1.ContainerStateRunning{}
		},
		"waiting for another cause": func(status *corev1.ContainerStatus) { status.State.Waiting.Reason = "ContainerCreating" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			state := lifecycleRuntimeGuard(lifecycleRuntimeGuardPods(func(index int) corev1.PodStatus {
				status := lifecycleRuntimeFailedStatus()
				if index == 2 {
					mutate(&status.ContainerStatuses[0])
				}
				return status
			}), "ptah-e2e", "all", 3)
			if state.MainContainersNeverStarted {
				t.Fatal("a main container that started passed as never started")
			}
		})
	}
	// The stability bounds the guard waits with.
	if lifecycleRuntimeBlockedStability != 10*time.Second || lifecycleRuntimeBlockedFailureTimeout != 150*time.Second {
		t.Fatal("the runtime guard bounds moved")
	}
}

const lifecycleRuntimeDispatchedJob = `{
  "metadata": {"uid": "job-uid", "labels": {"operator.ptah.run/schema": "read-only-job-current", "operator.ptah.run/operation": "resolve"}},
  "spec": {"backoffLimit": 0}
}`

func TestLifecycleRuntimeReadOnlyJobDispatched(t *testing.T) {
	t.Parallel()
	lifecycleRuntimeMutations(t, lifecycleRuntimeDispatchedJob, func(job map[string]any) bool {
		return lifecycleRuntimeReadOnlyJobDispatched(job, "read-only-job-current", "job-uid")
	}, map[string]func(map[string]any){
		"another UID": func(job map[string]any) { lifecycleRuntimeField(job, "metadata")["uid"] = "other" },
		"another schema": func(job map[string]any) {
			lifecycleRuntimeField(job, "metadata", "labels")["operator.ptah.run/schema"] = "x"
		},
		"another operation": func(job map[string]any) {
			lifecycleRuntimeField(job, "metadata", "labels")["operator.ptah.run/operation"] = "plan"
		},
		"no labels":          func(job map[string]any) { delete(lifecycleRuntimeField(job, "metadata"), "labels") },
		"cleanup TTL at all": func(job map[string]any) { lifecycleRuntimeField(job, "spec")["ttlSecondsAfterFinished"] = nil },
	})
}

func TestLifecycleRuntimeSchemaQuiesced(t *testing.T) {
	t.Parallel()
	lifecycleRuntimeMutations(t, `{"spec": {"suspend": true}, "status": {"phase": "Suspended", "activeOperation": null}}`,
		lifecycleRuntimeSchemaQuiesced, map[string]func(map[string]any){
			"not suspended":  func(schema map[string]any) { lifecycleRuntimeField(schema, "spec")["suspend"] = false },
			"suspend absent": func(schema map[string]any) { delete(lifecycleRuntimeField(schema, "spec"), "suspend") },
			"another phase":  func(schema map[string]any) { lifecycleRuntimeField(schema, "status")["phase"] = "Resolving" },
			"operation in hand": func(schema map[string]any) {
				lifecycleRuntimeField(schema, "status")["activeOperation"] = map[string]any{}
			},
			"suspend as a string": func(schema map[string]any) { lifecycleRuntimeField(schema, "spec")["suspend"] = "true" },
		})
}

func TestLifecycleRuntimePodWebhook(t *testing.T) {
	t.Parallel()
	configuration := lifecycleRuntimeDocument(t, `{"webhooks": [
	  {"name": "vschema.operator.ptah.run", "failurePolicy": "Fail"},
	  {"name": "vpodintent.operator.ptah.run", "failurePolicy": "Fail"}
	]}`)
	index, err := lifecycleRuntimePodWebhookIndex(configuration)
	if err != nil || index != 1 {
		t.Fatalf("index = %d, %v", index, err)
	}
	if name, policy := lifecycleRuntimeWebhookAt(configuration, index); name != lifecycleRuntimePodWebhook || policy != "Fail" {
		t.Fatalf("webhook at %d = %s %s", index, name, policy)
	}
	if name, policy := lifecycleRuntimeWebhookAt(configuration, 5); name != "" || policy != "" {
		t.Fatal("an index past the list read a webhook")
	}
	for name, document := range map[string]string{
		"none":      `{"webhooks": [{"name": "vschema.operator.ptah.run"}]}`,
		"two":       `{"webhooks": [{"name": "vpodintent.operator.ptah.run"}, {"name": "vpodintent.operator.ptah.run"}]}`,
		"no list":   `{}`,
		"null list": `{"webhooks": null}`,
	} {
		if _, err := lifecycleRuntimePodWebhookIndex(lifecycleRuntimeDocument(t, document)); err == nil {
			t.Errorf("%s: the Pod intent webhook was found", name)
		}
	}
	for _, transition := range [][2]string{{"Fail", "Ignore"}, {"Ignore", "Fail"}} {
		if err := lifecycleRuntimeFailurePolicyTransition(transition[0], transition[1]); err != nil {
			t.Errorf("%v refused: %v", transition, err)
		}
	}
	for _, transition := range [][2]string{{"Fail", "Fail"}, {"Ignore", "Ignore"}, {"fail", "Ignore"}, {"", "Fail"}} {
		if err := lifecycleRuntimeFailurePolicyTransition(transition[0], transition[1]); err == nil {
			t.Errorf("%v accepted", transition)
		}
	}
	want := `[{"op":"test","path":"/webhooks/1/name","value":"vpodintent.operator.ptah.run"},` +
		`{"op":"test","path":"/webhooks/1/failurePolicy","value":"Fail"},` +
		`{"op":"replace","path":"/webhooks/1/failurePolicy","value":"Ignore"}]`
	if got := string(lifecycleRuntimeFailurePolicyPatch(1, "Fail", "Ignore")); got != want {
		t.Fatalf("patch = %s", got)
	}
}

func TestLifecycleRuntimeReadOnlyJobOpen(t *testing.T) {
	t.Parallel()
	lifecycleRuntimeMutations(t, `{"metadata": {"uid": "job-uid"}, "status": {"conditions": [
	  {"type": "Failed", "status": "False"}, {"type": "Suspended", "status": "True"}
	]}}`, func(job map[string]any) bool { return lifecycleRuntimeReadOnlyJobOpen(job, "job-uid") },
		map[string]func(map[string]any){
			"another UID": func(job map[string]any) { lifecycleRuntimeField(job, "metadata")["uid"] = "x" },
			"completed": func(job map[string]any) {
				lifecycleRuntimeField(job, "status")["conditions"] = []any{map[string]any{"type": "Complete", "status": "True"}}
			},
			"failed": func(job map[string]any) {
				lifecycleRuntimeField(job, "status")["conditions"] = []any{map[string]any{"type": "Failed", "status": "True"}}
			},
			"failure target": func(job map[string]any) {
				lifecycleRuntimeField(job, "status")["conditions"] = []any{map[string]any{"type": "FailureTarget", "status": "True"}}
			},
			"completion time": func(job map[string]any) { lifecycleRuntimeField(job, "status")["completionTime"] = nil },
		})
	// No status at all fails the check: jq's has on null is an error, which
	// failed the script's filter. The API server always writes a status, so
	// this reading is one the harness should never be handed.
	if lifecycleRuntimeReadOnlyJobOpen(lifecycleRuntimeDocument(t, `{"metadata": {"uid": "job-uid"}}`), "job-uid") {
		t.Fatal("a Job with no status passed as open, where the script's jq failed")
	}
	// An empty status, which is what the API server writes for a Job no
	// controller has touched, is open.
	if !lifecycleRuntimeReadOnlyJobOpen(lifecycleRuntimeDocument(t, `{"metadata": {"uid": "job-uid"}, "status": {}}`), "job-uid") {
		t.Fatal("a Job with an empty status was read as terminal")
	}
}

func TestLifecycleRuntimeFailureTargetPatch(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 28, 20, 1, 2, 999, time.FixedZone("x", 3600))
	want := `{"status":{"conditions":[{"lastProbeTime":"2026-09-28T19:01:02Z","lastTransitionTime":"2026-09-28T19:01:02Z",` +
		`"message":"terminal read-only Job retained across quiescence","reason":"ReadOnlyJobProof","status":"True","type":"FailureTarget"}]}}`
	if got := string(lifecycleRuntimeFailureTargetPatch(at)); got != want {
		t.Fatalf("patch = %s", got)
	}
}

const lifecycleRuntimeRetiredJob = `{
  "metadata": {"uid": "job-uid"},
  "spec": {"backoffLimit": 0},
  "status": {
    "startTime": "2026-01-01T00:00:00Z", "active": 0, "ready": 0, "terminating": 0,
    "uncountedTerminatedPods": {},
    "conditions": [
      {"type": "FailureTarget", "status": "True", "reason": "ReadOnlyJobProof", "message": "terminal read-only Job retained across quiescence"},
      {"type": "Failed", "status": "True", "reason": "ReadOnlyJobProof", "message": "terminal read-only Job retained across quiescence"}
    ]
  }
}`

func TestLifecycleRuntimeReadOnlyJobRetired(t *testing.T) {
	t.Parallel()
	condition := func(job map[string]any, index int) map[string]any {
		conditions := lifecycleRuntimeField(job, "status")["conditions"].([]any)
		return conditions[index].(map[string]any)
	}
	lifecycleRuntimeMutations(t, lifecycleRuntimeRetiredJob, func(job map[string]any) bool {
		return lifecycleRuntimeReadOnlyJobRetired(job, "job-uid")
	}, map[string]func(map[string]any){
		"another UID":       func(job map[string]any) { lifecycleRuntimeField(job, "metadata")["uid"] = "x" },
		"never started":     func(job map[string]any) { delete(lifecycleRuntimeField(job, "status"), "startTime") },
		"still active":      func(job map[string]any) { lifecycleRuntimeField(job, "status")["active"] = 1 },
		"still ready":       func(job map[string]any) { lifecycleRuntimeField(job, "status")["ready"] = 1 },
		"still terminating": func(job map[string]any) { lifecycleRuntimeField(job, "status")["terminating"] = 1 },
		"uncounted success": func(job map[string]any) {
			lifecycleRuntimeField(job, "status", "uncountedTerminatedPods")["succeeded"] = []any{"p"}
		},
		"uncounted failure": func(job map[string]any) {
			lifecycleRuntimeField(job, "status", "uncountedTerminatedPods")["failed"] = []any{"p"}
		},
		"completion time": func(job map[string]any) {
			lifecycleRuntimeField(job, "status")["completionTime"] = "2026-01-01T00:01:00Z"
		},
		"target of another": func(job map[string]any) { condition(job, 0)["reason"] = "BackoffLimitExceeded" },
		"target not true":   func(job map[string]any) { condition(job, 0)["status"] = "False" },
		"failed elsewhere":  func(job map[string]any) { condition(job, 1)["message"] = "another message" },
		"never failed":      func(job map[string]any) { condition(job, 1)["type"] = "Complete" },
		"cleanup TTL":       func(job map[string]any) { lifecycleRuntimeField(job, "spec")["ttlSecondsAfterFinished"] = 300 },
		"no conditions":     func(job map[string]any) { delete(lifecycleRuntimeField(job, "status"), "conditions") },
	})
	// Absent counts read as zero, as `// 0` does.
	job := lifecycleRuntimeDocument(t, lifecycleRuntimeRetiredJob)
	status := lifecycleRuntimeField(job, "status")
	for _, key := range []string{"active", "ready", "terminating", "uncountedTerminatedPods"} {
		delete(status, key)
	}
	if !lifecycleRuntimeReadOnlyJobRetired(job, "job-uid") {
		t.Fatal("absent counts were not read as zero")
	}
	// The API server's integers decode as int64 into an unstructured object.
	status["active"] = int64(0)
	if !lifecycleRuntimeReadOnlyJobRetired(job, "job-uid") {
		t.Fatal("an int64 zero was not read as zero")
	}
	status["active"] = int64(1)
	if lifecycleRuntimeReadOnlyJobRetired(job, "job-uid") {
		t.Fatal("an int64 one was read as zero")
	}
}

func TestLifecycleRuntimeJobCleanupEvidence(t *testing.T) {
	t.Parallel()
	base := `{
	  "metadata": {"uid": "u", "name": "n", "namespace": "ns", "labels": {"a": "b"}, "annotations": null,
	    "ownerReferences": [{"uid": "o"}], "resourceVersion": "1", "managedFields": []},
	  "spec": {"backoffLimit": 0, "template": {}},
	  "status": {"active": 1}
	}`
	before, err := lifecycleRuntimeJobCleanupEvidence(lifecycleRuntimeDocument(t, base))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"annotations":null,"finalizers":[],"labels":{"a":"b"},"name":"n","namespace":"ns",` +
		`"ownerReferences":[{"uid":"o"}],"spec":{"backoffLimit":0,"template":{}},"uid":"u"}`
	if string(before) != want {
		t.Fatalf("evidence = %s", before)
	}
	cleaned := lifecycleRuntimeDocument(t, base)
	lifecycleRuntimeField(cleaned, "spec")["ttlSecondsAfterFinished"] = 300
	lifecycleRuntimeField(cleaned, "metadata")["resourceVersion"] = "2"
	lifecycleRuntimeField(cleaned, "status")["active"] = 0
	if after, _ := lifecycleRuntimeJobCleanupEvidence(cleaned); string(after) != string(before) {
		t.Fatalf("the cleanup TTL, the resourceVersion or the status changed the evidence: %s", after)
	}
	for name, mutate := range map[string]func(map[string]any){
		"spec":        func(job map[string]any) { lifecycleRuntimeField(job, "spec")["backoffLimit"] = 1 },
		"label":       func(job map[string]any) { lifecycleRuntimeField(job, "metadata", "labels")["a"] = "c" },
		"annotation":  func(job map[string]any) { lifecycleRuntimeField(job, "metadata")["annotations"] = map[string]any{} },
		"finalizer":   func(job map[string]any) { lifecycleRuntimeField(job, "metadata")["finalizers"] = []any{"f"} },
		"owner":       func(job map[string]any) { lifecycleRuntimeField(job, "metadata")["ownerReferences"] = []any{} },
		"another UID": func(job map[string]any) { lifecycleRuntimeField(job, "metadata")["uid"] = "v" },
	} {
		document := lifecycleRuntimeDocument(t, base)
		mutate(document)
		if after, _ := lifecycleRuntimeJobCleanupEvidence(document); string(after) == string(before) {
			t.Errorf("a changed %s left the evidence unchanged", name)
		}
	}
}

func TestLifecycleRuntimeCleanupScheduled(t *testing.T) {
	t.Parallel()
	if !lifecycleRuntimeCleanupScheduled(lifecycleRuntimeDocument(t, `{"spec": {"ttlSecondsAfterFinished": 300}}`)) {
		t.Fatal("the successor's TTL was not recognized")
	}
	for _, document := range []string{`{"spec": {"ttlSecondsAfterFinished": 600}}`, `{"spec": {}}`, `{}`, `{"spec": {"ttlSecondsAfterFinished": "300"}}`} {
		if lifecycleRuntimeCleanupScheduled(lifecycleRuntimeDocument(t, document)) {
			t.Errorf("%s passed as the successor's cleanup", document)
		}
	}
}

func TestLifecycleRuntimeUIDGapStaged(t *testing.T) {
	t.Parallel()
	lifecycleRuntimeMutations(t, `{"status": {"activeOperation": {"type": "Resolve", "jobName": "job-a"}}}`,
		func(schema map[string]any) bool { return lifecycleRuntimeUIDGapStaged(schema, "job-a") },
		map[string]func(map[string]any){
			"another Job": func(schema map[string]any) {
				lifecycleRuntimeField(schema, "status", "activeOperation")["jobName"] = "job-b"
			},
			"another operation": func(schema map[string]any) {
				lifecycleRuntimeField(schema, "status", "activeOperation")["type"] = "Plan"
			},
			"UID still committed": func(schema map[string]any) {
				lifecycleRuntimeField(schema, "status", "activeOperation")["jobUID"] = "u"
			},
			"UID empty but kept": func(schema map[string]any) { lifecycleRuntimeField(schema, "status", "activeOperation")["jobUID"] = "" },
			"no operation":       func(schema map[string]any) { delete(lifecycleRuntimeField(schema, "status"), "activeOperation") },
		})
}

func TestLifecycleRuntimeLateJobFailed(t *testing.T) {
	t.Parallel()
	lifecycleRuntimeMutations(t, `{"metadata": {"uid": "job-uid"}, "spec": {}, "status": {"conditions": [{"type": "Failed", "status": "True"}]}}`,
		func(job map[string]any) bool { return lifecycleRuntimeLateJobFailed(job, "job-uid") },
		map[string]func(map[string]any){
			"another UID":   func(job map[string]any) { lifecycleRuntimeField(job, "metadata")["uid"] = "x" },
			"not failed":    func(job map[string]any) { lifecycleRuntimeField(job, "status")["conditions"] = []any{} },
			"no conditions": func(job map[string]any) { delete(lifecycleRuntimeField(job, "status"), "conditions") },
			"cleanup TTL":   func(job map[string]any) { lifecycleRuntimeField(job, "spec")["ttlSecondsAfterFinished"] = 300 },
		})
}

func TestLifecycleRuntimeRotatorArgument(t *testing.T) {
	t.Parallel()
	deployment := lifecycleRuntimeDocument(t, `{"spec": {"template": {"spec": {"containers": [
	  {"name": "certificate-rotator", "args": ["--secret-name=serving", "--staging-secret-name=staging", "--other"]},
	  {"name": "sidecar", "args": ["--secret-name=elsewhere"]}
	]}}}}`)
	for prefix, want := range map[string]string{"--secret-name=": "serving", "--staging-secret-name=": "staging"} {
		if got, err := lifecycleRuntimeRotatorArgument(deployment, prefix); err != nil || got != want {
			t.Errorf("%s = %q, %v", prefix, got, err)
		}
	}
	for name, document := range map[string]string{
		"none":  `{"spec": {"template": {"spec": {"containers": [{"name": "certificate-rotator"}]}}}}`,
		"empty": `{"spec": {"template": {"spec": {"containers": [{"name": "certificate-rotator", "args": ["--secret-name="]}]}}}}`,
		"twice": `{"spec": {"template": {"spec": {"containers": [{"name": "certificate-rotator", "args": ["--secret-name=a", "--secret-name=b"]}]}}}}`,
		"two rotators": `{"spec": {"template": {"spec": {"containers": [{"name": "certificate-rotator", "args": ["--secret-name=a"]},` +
			`{"name": "certificate-rotator", "args": ["--secret-name=b"]}]}}}}`,
		"another container": `{"spec": {"template": {"spec": {"containers": [{"name": "sidecar", "args": ["--secret-name=a"]}]}}}}`,
	} {
		if _, err := lifecycleRuntimeRotatorArgument(lifecycleRuntimeDocument(t, document), "--secret-name="); err == nil {
			t.Errorf("%s: a serving Secret identity was read", name)
		}
	}
}

func TestLifecycleRuntimeImpersonationPod(t *testing.T) {
	t.Parallel()
	pod := func(name string, phase corev1.PodPhase) corev1.Pod {
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.PodStatus{Phase: phase}}
	}
	got, err := lifecycleRuntimeImpersonationPod([]corev1.Pod{
		pod("manager-c", corev1.PodRunning), pod("manager-a", corev1.PodPending), pod("manager-b", corev1.PodRunning),
	})
	if err != nil || got.Name != "manager-b" {
		t.Fatalf("the first running Pod by name = %s, %v", got.Name, err)
	}
	if _, err := lifecycleRuntimeImpersonationPod([]corev1.Pod{pod("manager-a", corev1.PodPending)}); err == nil ||
		!strings.Contains(err.Error(), "no running controller Pod") {
		t.Fatalf("no running Pod read as %v", err)
	}
}

const lifecycleRuntimeControllerDeploymentDocument = `{
  "metadata": {"name": "ptah-e2e-ptah-operator", "uid": "deployment-uid",
    "labels": {"app.kubernetes.io/instance": "ptah-e2e", "app.kubernetes.io/component": "controller"}},
  "spec": {"template": {"spec": {"serviceAccountName": "ptah-e2e-ptah-operator", "containers": [
    {"name": "manager", "image": "registry/manager@sha256:aa"}, {"name": "sidecar", "image": "registry/manager@sha256:aa"}
  ]}}}
}`

func TestLifecycleRuntimeControllerDeployment(t *testing.T) {
	t.Parallel()
	identity, err := lifecycleRuntimeControllerDeployment(
		[]map[string]any{lifecycleRuntimeDocument(t, lifecycleRuntimeControllerDeploymentDocument)}, "ptah-e2e", "registry/manager@sha256:aa")
	if err != nil || identity != (lifecycleRuntimeDeployment{name: "ptah-e2e-ptah-operator", uid: "deployment-uid", serviceAccount: "ptah-e2e-ptah-operator"}) {
		t.Fatalf("identity = %+v, %v", identity, err)
	}
	accept := func(document map[string]any) bool {
		_, err := lifecycleRuntimeControllerDeployment([]map[string]any{document}, "ptah-e2e", "registry/manager@sha256:aa")
		return err == nil
	}
	containers := func(document map[string]any) []any {
		return lifecycleRuntimeField(document, "spec", "template", "spec")["containers"].([]any)
	}
	lifecycleRuntimeMutations(t, lifecycleRuntimeControllerDeploymentDocument, accept, map[string]func(map[string]any){
		"another release": func(d map[string]any) {
			lifecycleRuntimeField(d, "metadata", "labels")["app.kubernetes.io/instance"] = "x"
		},
		"another component": func(d map[string]any) {
			lifecycleRuntimeField(d, "metadata", "labels")["app.kubernetes.io/component"] = "x"
		},
		"no UID": func(d map[string]any) { delete(lifecycleRuntimeField(d, "metadata"), "uid") },
		"no ServiceAccount": func(d map[string]any) {
			lifecycleRuntimeField(d, "spec", "template", "spec")["serviceAccountName"] = ""
		},
		"another image":   func(d map[string]any) { containers(d)[0].(map[string]any)["image"] = "registry/manager@sha256:bb" },
		"two managers":    func(d map[string]any) { containers(d)[1].(map[string]any)["name"] = "manager" },
		"manager renamed": func(d map[string]any) { containers(d)[0].(map[string]any)["name"] = "controller" },
	})
	two := []map[string]any{
		lifecycleRuntimeDocument(t, lifecycleRuntimeControllerDeploymentDocument),
		lifecycleRuntimeDocument(t, lifecycleRuntimeControllerDeploymentDocument),
	}
	if _, err := lifecycleRuntimeControllerDeployment(two, "ptah-e2e", "registry/manager@sha256:aa"); err == nil {
		t.Fatal("two controller Deployments passed as one")
	}
	if _, err := lifecycleRuntimeControllerDeployment(nil, "ptah-e2e", "registry/manager@sha256:aa"); err == nil {
		t.Fatal("no controller Deployment passed as one")
	}
}

func TestLifecycleRuntimeLiveServiceAccount(t *testing.T) {
	t.Parallel()
	base := `{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": {"name": "sa", "namespace": "ns", "uid": "sa-uid",
	  "labels": {"app.kubernetes.io/instance": "ptah-e2e"}}}`
	if uid, err := lifecycleRuntimeLiveServiceAccount(lifecycleRuntimeDocument(t, base), "sa", "ns", "ptah-e2e"); err != nil || uid != "sa-uid" {
		t.Fatalf("uid = %q, %v", uid, err)
	}
	lifecycleRuntimeMutations(t, base, func(account map[string]any) bool {
		_, err := lifecycleRuntimeLiveServiceAccount(account, "sa", "ns", "ptah-e2e")
		return err == nil
	}, map[string]func(map[string]any){
		"another version":   func(a map[string]any) { a["apiVersion"] = "v2" },
		"another kind":      func(a map[string]any) { a["kind"] = "Secret" },
		"another name":      func(a map[string]any) { lifecycleRuntimeField(a, "metadata")["name"] = "x" },
		"another namespace": func(a map[string]any) { lifecycleRuntimeField(a, "metadata")["namespace"] = "x" },
		"another release": func(a map[string]any) {
			lifecycleRuntimeField(a, "metadata", "labels")["app.kubernetes.io/instance"] = "x"
		},
		"no UID": func(a map[string]any) { lifecycleRuntimeField(a, "metadata")["uid"] = "" },
		"being deleted": func(a map[string]any) {
			lifecycleRuntimeField(a, "metadata")["deletionTimestamp"] = "2026-01-01T00:00:00Z"
		},
	})
}

func TestLifecycleRuntimeSnapshot(t *testing.T) {
	t.Parallel()
	live := lifecycleRuntimeDocument(t, `{
	  "apiVersion": "apps/v1", "kind": "Deployment",
	  "metadata": {"name": "d", "namespace": "ns", "uid": "u", "resourceVersion": "7", "generation": 3,
	    "creationTimestamp": "2026-01-01T00:00:00Z",
	    "annotations": {"deployment.kubernetes.io/revision": "2", "kept": "yes"},
	    "managedFields": [
	      {"manager": "helm", "operation": "Apply"},
	      {"manager": "kube-controller-manager", "operation": "Update"},
	      {"manager": "helm", "operation": "Apply", "subresource": "status"}
	    ]},
	  "spec": {"replicas": 2},
	  "status": {"replicas": 2}
	}`)
	live["spec"].(map[string]any)["replicas"] = int64(2)
	snapshot, manager, err := lifecycleRuntimeSnapshot(live)
	if err != nil || manager != "helm" {
		t.Fatalf("manager = %q, %v", manager, err)
	}
	encoded, _ := json.Marshal(snapshot)
	want := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"annotations":{"kept":"yes"},"name":"d","namespace":"ns"},"spec":{"replicas":2}}`
	if string(encoded) != want {
		t.Fatalf("snapshot = %s", encoded)
	}
	if _, isInt := snapshot["spec"].(map[string]any)["replicas"].(int64); !isInt {
		t.Fatal("the snapshot turned an integer into another type")
	}
	if _, found := live["status"]; !found {
		t.Fatal("the snapshot changed the live document")
	}
	for name, fields := range map[string]string{
		"no apply manager":   `[{"manager": "kubectl-create", "operation": "Update"}]`,
		"two apply managers": `[{"manager": "helm", "operation": "Apply"}, {"manager": "kubectl", "operation": "Apply"}]`,
		"no managed fields":  `null`,
	} {
		document := lifecycleRuntimeDocument(t, `{"metadata": {"managedFields": `+fields+`}}`)
		if _, _, err := lifecycleRuntimeSnapshot(document); err == nil {
			t.Errorf("%s: a manager to restore as was chosen", name)
		}
	}
}

func TestLifecycleRuntimeInitStatusSummary(t *testing.T) {
	t.Parallel()
	pods := make([]corev1.Pod, 12)
	for index := range pods {
		pods[index].Name = "pod-" + string(rune('a'+index))
	}
	for range 9 {
		pods[0].Status.InitContainerStatuses = append(pods[0].Status.InitContainerStatuses, corev1.ContainerStatus{Name: "init"})
	}
	pods[1].Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name:  lifecycleRuntimeVerifierContainer,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}},
	}}
	summary := lifecycleRuntimeInitStatusSummary(pods)
	if len(summary) != 10 || len(summary[0]["init"].([]map[string]any)) != 8 {
		t.Fatalf("the summary was not bounded to ten Pods of eight init containers: %d", len(summary))
	}
	encoded, _ := json.Marshal(summary[1])
	want := `{"init":[{"exitCode":1,"name":"verify-candidate-runtime","terminatedReason":"Error","waitingReason":null}],"pod":"pod-b"}`
	if string(encoded) != want {
		t.Fatalf("summary = %s", encoded)
	}
	encoded, _ = json.Marshal(summary[2])
	if string(encoded) != `{"init":[],"pod":"pod-c"}` {
		t.Fatalf("a Pod with no init containers = %s", encoded)
	}
}
