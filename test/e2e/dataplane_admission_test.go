package e2e

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// refusalRewriteSource is the Job the shell phase's self-check and
// hack/e2e-static.sh rewrote: one container whose URL was a literal, whose
// operation ID came from the Pod's UID, and a variable the rewrite must leave
// alone.
const refusalRewriteSource = `{
  "spec": {
    "backoffLimit": 7,
    "template": {
      "metadata": {"labels": {"preserved": "source-only"}},
      "spec": {
        "restartPolicy": "Never",
        "containers": [{
          "name": "ptah",
          "image": "fixture.invalid/ptah@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
          "args": ["observe"],
          "env": [
            {"name": "PTAH_DB_URL", "value": "mysql://original.invalid"},
            {"name": "PTAH_OPERATION_ID", "valueFrom": {"fieldRef": {"fieldPath": "metadata.uid"}}},
            {"name": "PRESERVED", "value": "exact"}
          ]
        }]
      }
    }
  }
}`

// rewrittenRefusalContainers is the one container every rewrite of the source
// must produce: the URL from the unsafe Secret, the operation ID a literal,
// and nothing else moved.
const rewrittenRefusalContainers = `[{
  "name": "ptah",
  "image": "fixture.invalid/ptah@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "args": ["observe"],
  "env": [
    {"name": "PTAH_DB_URL", "valueFrom": {"secretKeyRef": {"name": "test-secret", "key": "url"}}},
    {"name": "PTAH_OPERATION_ID", "value": "test-operation-id"},
    {"name": "PRESERVED", "value": "exact"}
  ]
}]`

func decodeDocument(t *testing.T, content string) map[string]any {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal([]byte(content), &document); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return document
}

func decodeValue(t *testing.T, content string) any {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(content), &value); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return value
}

func rewriteTestJob(t *testing.T, source map[string]any) map[string]any {
	t.Helper()
	rewritten, err := rewriteMySQLRefusalJob(source, "test-namespace", "test-name", "test-schema", "observe",
		"test-operation-id", "test-secret")
	if err != nil {
		t.Fatalf("rewriteMySQLRefusalJob() = %v", err)
	}
	return rewritten
}

func field(t *testing.T, document map[string]any, path ...string) (any, bool) {
	t.Helper()
	value, found, err := unstructured.NestedFieldNoCopy(document, path...)
	if err != nil {
		t.Fatalf("read %s: %v", strings.Join(path, "."), err)
	}
	return value, found
}

// The rewrite keeps an absent initContainers absent, and everything in the
// Pod template but the two variables it exists to change exactly as the
// source had it (hack/e2e-static.sh and the shell phase's own self-check).
func TestRewriteMySQLRefusalJobWithoutInitContainers(t *testing.T) {
	t.Parallel()
	source := decodeDocument(t, refusalRewriteSource)
	pristine := runtime.DeepCopyJSON(source)
	rewritten := rewriteTestJob(t, source)
	if !reflect.DeepEqual(source, pristine) {
		t.Fatal("the rewrite changed the source it was given")
	}
	if _, found := field(t, rewritten, "spec", "template", "spec", "initContainers"); found {
		t.Error("the rewrite added initContainers the source did not have")
	}
	if value, _ := field(t, rewritten, "spec", "backoffLimit"); value != float64(7) {
		t.Errorf("backoffLimit = %v, want 7", value)
	}
	if value, _ := field(t, rewritten, "spec", "template", "spec", "restartPolicy"); value != "Never" {
		t.Errorf("restartPolicy = %v, want Never", value)
	}
	containers, _ := field(t, rewritten, "spec", "template", "spec", "containers")
	if want := decodeValue(t, rewrittenRefusalContainers); !reflect.DeepEqual(containers, want) {
		t.Errorf("containers = %v, want %v", containers, want)
	}
	wantMetadata := map[string]any{
		"namespace": "test-namespace", "name": "test-name",
		"labels": map[string]any{
			"app.kubernetes.io/component": "e2e-invalid-dsn",
			"operator.ptah.run/schema":    "test-schema", "operator.ptah.run/operation": "observe",
		},
		"annotations": map[string]any{"operator.ptah.run/operation-id": "test-operation-id"},
	}
	if !reflect.DeepEqual(rewritten["metadata"], wantMetadata) || rewritten["apiVersion"] != "batch/v1" ||
		rewritten["kind"] != "Job" {
		t.Errorf("the rewritten Job is %v %v %v", rewritten["apiVersion"], rewritten["kind"], rewritten["metadata"])
	}
	// The source's template labels are the operator's selectors; the copy
	// carries its own and nothing of the source's.
	templateMetadata, _ := field(t, rewritten, "spec", "template", "metadata")
	if want := map[string]any{"labels": wantMetadata["labels"], "annotations": wantMetadata["annotations"]}; !reflect.DeepEqual(templateMetadata, want) {
		t.Errorf("template metadata = %v, want %v", templateMetadata, want)
	}
}

// A null initContainers stays null, rather than becoming a list or
// disappearing.
func TestRewriteMySQLRefusalJobWithNullInitContainers(t *testing.T) {
	t.Parallel()
	source := decodeDocument(t, refusalRewriteSource)
	if err := unstructured.SetNestedField(source, nil, "spec", "template", "spec", "initContainers"); err != nil {
		t.Fatal(err)
	}
	rewritten := rewriteTestJob(t, source)
	value, found := field(t, rewritten, "spec", "template", "spec", "initContainers")
	if !found || value != nil {
		t.Errorf("initContainers = %v (present %t), want a null that stays null", value, found)
	}
	if value, _ := field(t, rewritten, "spec", "backoffLimit"); value != float64(7) {
		t.Errorf("backoffLimit = %v, want 7", value)
	}
	containers, _ := field(t, rewritten, "spec", "template", "spec", "containers")
	if want := decodeValue(t, rewrittenRefusalContainers); !reflect.DeepEqual(containers, want) {
		t.Errorf("containers = %v, want %v", containers, want)
	}
}

// Helper containers with no environment list, absent or null, keep what they
// had, and one with a list is rewritten like the main container.
func TestRewriteMySQLRefusalJobKeepsHelpersWithoutEnvironment(t *testing.T) {
	t.Parallel()
	source := decodeDocument(t, refusalRewriteSource)
	helpers := `[
	  {"name": "without-env", "image": "fixture.invalid/helper:one"},
	  {"name": "null-env", "image": "fixture.invalid/helper:two", "env": null}
	]`
	if err := unstructured.SetNestedField(source, decodeValue(t, helpers), "spec", "template", "spec", "initContainers"); err != nil {
		t.Fatal(err)
	}
	rewritten := rewriteTestJob(t, source)
	initContainers, _ := field(t, rewritten, "spec", "template", "spec", "initContainers")
	if want := decodeValue(t, helpers); !reflect.DeepEqual(initContainers, want) {
		t.Errorf("initContainers = %v, want %v", initContainers, want)
	}
	containers, _ := field(t, rewritten, "spec", "template", "spec", "containers")
	if want := decodeValue(t, rewrittenRefusalContainers); !reflect.DeepEqual(containers, want) {
		t.Errorf("containers = %v, want %v", containers, want)
	}

	withEnv := decodeDocument(t, refusalRewriteSource)
	if err := unstructured.SetNestedField(withEnv, decodeValue(t, `[{"name": "install-runner", "env": [
	  {"name": "PTAH_DB_URL", "value": "mysql://original.invalid"}
	]}]`), "spec", "template", "spec", "initContainers"); err != nil {
		t.Fatal(err)
	}
	initContainers, _ = field(t, rewriteTestJob(t, withEnv), "spec", "template", "spec", "initContainers")
	want := decodeValue(t, `[{"name": "install-runner", "env": [
	  {"name": "PTAH_DB_URL", "valueFrom": {"secretKeyRef": {"name": "test-secret", "key": "url"}}}
	]}]`)
	if !reflect.DeepEqual(initContainers, want) {
		t.Errorf("an init container with environment = %v, want %v", initContainers, want)
	}
}

// The copy selects its own Pods: the source's selector and manual selector
// go, and a source the rewrite cannot read is refused rather than copied.
func TestRewriteMySQLRefusalJobDropsSelectorsAndRefusesMalformedSources(t *testing.T) {
	t.Parallel()
	source := decodeDocument(t, refusalRewriteSource)
	if err := unstructured.SetNestedField(source, map[string]any{"matchLabels": map[string]any{"a": "b"}}, "spec", "selector"); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(source, true, "spec", "manualSelector"); err != nil {
		t.Fatal(err)
	}
	rewritten := rewriteTestJob(t, source)
	for _, name := range []string{"selector", "manualSelector"} {
		if _, found := field(t, rewritten, "spec", name); found {
			t.Errorf("the rewrite kept spec.%s", name)
		}
	}
	for _, test := range []struct {
		name   string
		source string
	}{
		{"no spec", `{}`},
		{"no template", `{"spec": {}}`},
		{"no Pod spec", `{"spec": {"template": {}}}`},
		{"no container list", `{"spec": {"template": {"spec": {"containers": null}}}}`},
		{"a container that is not an object", `{"spec": {"template": {"spec": {"containers": ["ptah"]}}}}`},
		{"a variable that is not an object", `{"spec": {"template": {"spec": {"containers": [{"env": ["PTAH_DB_URL"]}]}}}}`},
		{"initContainers that is not a list", `{"spec": {"template": {"spec": {"containers": [], "initContainers": {}}}}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := rewriteMySQLRefusalJob(decodeDocument(t, test.source), "n", "j", "s", "observe", "id", "secret"); err == nil {
				t.Fatal("the rewrite accepted a source it cannot read")
			}
		})
	}
}

func TestUnsafeMySQLDSN(t *testing.T) {
	t.Parallel()
	got := unsafeMySQLDSN("mysql://u:p@tcp(host:3306)/db")
	want := "mysql://u:p@tcp(host:3306)/db?multiStatements=true&sql_mode=%27%27%3BDROP%20TABLE%20e2e_widgets"
	if got != want {
		t.Fatalf("unsafeMySQLDSN() = %q, want %q", got, want)
	}
}

func completedJobDocument(name, created string, complete bool) unstructured.Unstructured {
	status := "False"
	if complete {
		status = "True"
	}
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name, "creationTimestamp": created},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Complete", "status": status},
		}},
	}}
}

func TestLatestCompletedJob(t *testing.T) {
	t.Parallel()
	jobs := []unstructured.Unstructured{
		completedJobDocument("early", "2026-09-01T00:00:00Z", true),
		completedJobDocument("running-latest", "2026-09-01T00:05:00Z", false),
		completedJobDocument("tie-first", "2026-09-01T00:02:00Z", true),
		completedJobDocument("tie-second", "2026-09-01T00:02:00Z", true),
		{Object: map[string]any{"metadata": map[string]any{"name": "no-status", "creationTimestamp": "2026-09-01T00:09:00Z"}}},
	}
	latest, err := latestCompletedJob(jobs)
	if err != nil {
		t.Fatal(err)
	}
	// A Job still running is not a source, however late it was created, and
	// two created in one second keep the order the list gave them.
	if latest.GetName() != "tie-second" {
		t.Fatalf("latestCompletedJob() = %s, want tie-second", latest.GetName())
	}
	if _, err := latestCompletedJob(jobs[1:2]); err == nil {
		t.Fatal("a list with no completed Job produced a source")
	}
}

func invalidTargetResult() runner.Result {
	return runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationObserve, OperationID: "e2e-mysql-dsn-observe-operation",
		ChildExitCode: -1, Error: &runner.ResultError{Code: "invalid_target", Message: "refused"},
	}
}

func TestInvalidTargetRefusal(t *testing.T) {
	t.Parallel()
	protocol := int64(runner.ProtocolVersion)
	if err := invalidTargetRefusal(invalidTargetResult(), protocol, "observe", "e2e-mysql-dsn-observe-operation"); err != nil {
		t.Fatalf("a refusal of the target was refused: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*runner.Result)
	}{
		{"another protocol", func(r *runner.Result) { r.ProtocolVersion++ }},
		{"another operation", func(r *runner.Result) { r.Operation = runner.OperationPlan }},
		{"another operation ID", func(r *runner.Result) { r.OperationID = "other" }},
		{"no error", func(r *runner.Result) { r.Error = nil }},
		{"another error", func(r *runner.Result) { r.Error.Code = "invalid_oci_access" }},
		{"output", func(r *runner.Result) { r.Stdout = "{}" }},
		{"a plan digest", func(r *runner.Result) { r.PlanContentDigest = "sha256:" + strings.Repeat("a", 64) }},
		{"a plan outcome", func(r *runner.Result) { r.PlanOutcome = runner.PlanOutcomeNoChanges }},
		{"a mutation", func(r *runner.Result) { r.MutationStarted = true }},
		{"an uncertain outcome", func(r *runner.Result) { r.Uncertain = true }},
		{"truncated output", func(r *runner.Result) { r.Truncation = &runner.TruncationMetadata{Stdout: true} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result := invalidTargetResult()
			result.Error = &runner.ResultError{Code: result.Error.Code}
			test.mutate(&result)
			if err := invalidTargetRefusal(result, protocol, "observe", "e2e-mysql-dsn-observe-operation"); err == nil {
				t.Fatal("the mistake passed as a refusal of the target")
			}
		})
	}
}

func TestDisclosesSessionPayload(t *testing.T) {
	t.Parallel()
	if disclosesSessionPayload([]byte("PTAH_RUNNER_RESULT_V1 12 abc\n{\"error\":\"invalid_target\"}\n")) {
		t.Fatal("a transport that repeats nothing was read as disclosing the payload")
	}
	for _, transport := range []string{
		"target: DROP TABLE e2e_widgets\n",
		"target: drop\ttable e2e_widgets\n",
		"sql_mode=%27%27%3B\n",
		"calls side_effecting_function()\n",
	} {
		if !disclosesSessionPayload([]byte(transport)) {
			t.Errorf("%q was not read as disclosing the payload", transport)
		}
	}
	// grep read the transport a line at a time, so a statement split across
	// two lines was not a match there either.
	if disclosesSessionPayload([]byte("DROP\nTABLE\n")) {
		t.Error("a match across two lines counted")
	}
}

func preChildResult() runner.Result {
	return runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationResolve, OperationID: "resolve",
		ChildExitCode: -1, Error: &runner.ResultError{Code: "invalid_oci_access", Message: "refused"},
	}
}

func TestPreChildAccessRefusal(t *testing.T) {
	t.Parallel()
	if err := preChildAccessRefusal(preChildResult()); err != nil {
		t.Fatalf("a pre-child refusal was refused: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*runner.Result)
	}{
		{"the child started", func(r *runner.Result) { r.ChildExitCode = 1 }},
		{"output", func(r *runner.Result) { r.Stdout = "x" }},
		{"no error", func(r *runner.Result) { r.Error = nil }},
		{"another error", func(r *runner.Result) { r.Error.Code = "invalid_target" }},
		{"a resolved digest", func(r *runner.Result) { r.ResolvedDigest = "sha256:" + strings.Repeat("a", 64) }},
		{"a resolved reference", func(r *runner.Result) { r.ResolvedReference = "oci://registry/schema@sha256:a" }},
		{"a mutation", func(r *runner.Result) { r.MutationStarted = true }},
		{"an uncertain outcome", func(r *runner.Result) { r.Uncertain = true }},
		{"truncated output", func(r *runner.Result) { r.Truncation = &runner.TruncationMetadata{Stdout: true} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result := preChildResult()
			result.Error = &runner.ResultError{Code: result.Error.Code}
			test.mutate(&result)
			if err := preChildAccessRefusal(result); err == nil {
				t.Fatal("the mistake passed as a pre-child refusal")
			}
		})
	}
}

func failedAccessSchema() *ptahv1alpha1.PtahSchema {
	next := metav1.NewTime(time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC))
	return &ptahv1alpha1.PtahSchema{Status: ptahv1alpha1.PtahSchemaStatus{
		Phase: ptahv1alpha1.PhaseFailed, NextReconciliationTime: &next,
		Conditions: []metav1.Condition{{
			Type: "ReconciliationFailed", Status: metav1.ConditionTrue, Reason: "OperationFailed",
			Message: "invalid_oci_access: the registry grant names another CA",
		}},
	}}
}

func TestFailedBeforeResolveChild(t *testing.T) {
	t.Parallel()
	if !failedBeforeResolveChild(failedAccessSchema()) {
		t.Fatal("a schema failed for invalid_oci_access was not recognized")
	}
	for _, test := range []struct {
		name   string
		mutate func(*ptahv1alpha1.PtahSchema)
	}{
		{"not failed", func(s *ptahv1alpha1.PtahSchema) { s.Status.Phase = ptahv1alpha1.PhaseResolving }},
		{"no retry deadline", func(s *ptahv1alpha1.PtahSchema) { s.Status.NextReconciliationTime = nil }},
		{"another code", func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions[0].Message = "invalid_target: refused" }},
		{"the code inside the message", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Conditions[0].Message = "failed: invalid_oci_access: refused"
		}},
		{"another reason", func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions[0].Reason = "ConfigurationError" }},
		{"not failing", func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions[0].Status = metav1.ConditionFalse }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			schema := failedAccessSchema()
			test.mutate(schema)
			if failedBeforeResolveChild(schema) {
				t.Fatal("the mistake passed as a pre-child failure")
			}
		})
	}
}

func TestFailedForCurrentSpec(t *testing.T) {
	t.Parallel()
	failed := failedAccessSchema()
	failed.Generation, failed.Status.ObservedGeneration = 2, 2
	if !failedForCurrentSpec(failed) {
		t.Fatal("a schema that failed for the spec it holds was not recognized")
	}
	for _, test := range []struct {
		name   string
		mutate func(*ptahv1alpha1.PtahSchema)
	}{
		// The reading that failed CI: suspended, and not yet observed.
		{"failed for the spec before the edit", func(s *ptahv1alpha1.PtahSchema) { s.Generation = 3 }},
		{"never observed", func(s *ptahv1alpha1.PtahSchema) { s.Status.ObservedGeneration = 0 }},
		{"not failed", func(s *ptahv1alpha1.PtahSchema) { s.Status.Phase = ptahv1alpha1.PhaseSuspended }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			schema := failed.DeepCopy()
			test.mutate(schema)
			if failedForCurrentSpec(schema) {
				t.Fatal("the reading passed as a failure of the current spec")
			}
		})
	}
}

func TestTLSProxyCounter(t *testing.T) {
	t.Parallel()
	for body, want := range map[string]int64{"0": 0, "17\n": 17, "12345\n\n": 12345} {
		if got, err := tlsProxyCounter([]byte(body)); err != nil || got != want {
			t.Errorf("tlsProxyCounter(%q) = %d, %v; want %d", body, got, err, want)
		}
	}
	for _, body := range []string{"", "\n", "007", "-1", "12 ", " 12", "12\nfoo", "twelve", "1.5"} {
		if _, err := tlsProxyCounter([]byte(body)); err == nil {
			t.Errorf("tlsProxyCounter(%q) read a count", body)
		}
	}
}

func proxyPod(name, uid string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, PodIP: "10.244.1.7",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: tlsProxyContainer, Ready: true, ContainerID: "containerd://abc",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
}

func TestTLSProxyPodIdentity(t *testing.T) {
	t.Parallel()
	identity, err := tlsProxyPodIdentity([]corev1.Pod{proxyPod("proxy-a", "uid-a")})
	want := proxyPodIdentity{name: "proxy-a", uid: "uid-a", podIP: "10.244.1.7", containerID: "containerd://abc"}
	if err != nil || identity != want {
		t.Fatalf("tlsProxyPodIdentity() = %+v, %v; want %+v", identity, err, want)
	}
	deleting := proxyPod("proxy-old", "uid-old")
	deleting.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	if identity, err := tlsProxyPodIdentity([]corev1.Pod{deleting, proxyPod("proxy-a", "uid-a")}); err != nil || identity != want {
		t.Errorf("a Pod being deleted was counted: %+v, %v", identity, err)
	}
	for _, test := range []struct {
		name   string
		mutate func([]corev1.Pod) []corev1.Pod
	}{
		{"no Pod", func([]corev1.Pod) []corev1.Pod { return nil }},
		{"two Pods", func(pods []corev1.Pod) []corev1.Pod { return append(pods, proxyPod("proxy-b", "uid-b")) }},
		{"pending", func(pods []corev1.Pod) []corev1.Pod { pods[0].Status.Phase = corev1.PodPending; return pods }},
		{"no address", func(pods []corev1.Pod) []corev1.Pod { pods[0].Status.PodIP = ""; return pods }},
		{"not ready", func(pods []corev1.Pod) []corev1.Pod {
			pods[0].Status.Conditions[0].Status = corev1.ConditionFalse
			return pods
		}},
		{"container restarted", func(pods []corev1.Pod) []corev1.Pod {
			pods[0].Status.ContainerStatuses[0].RestartCount = 1
			return pods
		}},
		{"container not ready", func(pods []corev1.Pod) []corev1.Pod {
			pods[0].Status.ContainerStatuses[0].Ready = false
			return pods
		}},
		{"no container ID", func(pods []corev1.Pod) []corev1.Pod {
			pods[0].Status.ContainerStatuses[0].ContainerID = ""
			return pods
		}},
		{"container not running", func(pods []corev1.Pod) []corev1.Pod {
			pods[0].Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}
			return pods
		}},
		{"another container", func(pods []corev1.Pod) []corev1.Pod {
			pods[0].Status.ContainerStatuses[0].Name = "sidecar"
			return pods
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := tlsProxyPodIdentity(test.mutate([]corev1.Pod{proxyPod("proxy-a", "uid-a")})); err == nil {
				t.Fatal("the mistake passed as the proxy's identity")
			}
		})
	}
}

func TestTLSProxyIdentityStable(t *testing.T) {
	t.Parallel()
	captured := proxyPodIdentity{name: "proxy-a", uid: "uid-a", podIP: "10.244.1.7", containerID: "containerd://abc"}
	if !tlsProxyIdentityStable([]corev1.Pod{proxyPod("proxy-a", "uid-a")}, captured) {
		t.Fatal("the captured proxy was not recognized")
	}
	for _, test := range []struct {
		name   string
		mutate func([]corev1.Pod) []corev1.Pod
	}{
		{"replaced under the same name", func(pods []corev1.Pod) []corev1.Pod { pods[0].UID = "uid-b"; return pods }},
		{"another Pod", func(pods []corev1.Pod) []corev1.Pod { pods[0].Name = "proxy-b"; return pods }},
		{"moved address", func(pods []corev1.Pod) []corev1.Pod { pods[0].Status.PodIP = "10.244.1.8"; return pods }},
		{"another container", func(pods []corev1.Pod) []corev1.Pod {
			pods[0].Status.ContainerStatuses[0].ContainerID = "containerd://def"
			return pods
		}},
		{"restarted", func(pods []corev1.Pod) []corev1.Pod {
			pods[0].Status.ContainerStatuses[0].RestartCount = 1
			return pods
		}},
		{"not ready", func(pods []corev1.Pod) []corev1.Pod {
			pods[0].Status.Conditions[0].Status = corev1.ConditionFalse
			return pods
		}},
		{"a second Pod", func(pods []corev1.Pod) []corev1.Pod { return append(pods, proxyPod("proxy-b", "uid-b")) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if tlsProxyIdentityStable(test.mutate([]corev1.Pod{proxyPod("proxy-a", "uid-a")}), captured) {
				t.Fatal("the change passed as the captured proxy")
			}
		})
	}
}

func TestReferenceAtDigest(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("b", 64)
	for reference, want := range map[string]string{
		"oci://proxy.ns.svc.cluster.local:5443/schemas/postgresql:stable": "oci://proxy.ns.svc.cluster.local:5443/schemas/postgresql@" + digest,
		"oci://registry/schemas/postgresql:v1":                            "oci://registry/schemas/postgresql@" + digest,
	} {
		if got := referenceAtDigest(reference, digest); got != want {
			t.Errorf("referenceAtDigest(%q) = %q, want %q", reference, got, want)
		}
	}
}

func convergedSchema(digest string) *ptahv1alpha1.PtahSchema {
	return &ptahv1alpha1.PtahSchema{Status: ptahv1alpha1.PtahSchemaStatus{
		Phase: ptahv1alpha1.PhaseInSync, Source: ptahv1alpha1.SchemaSourceStatus{Digest: digest},
		Applied:    &ptahv1alpha1.AppliedStatus{ArtifactDigest: digest},
		Conditions: []metav1.Condition{{Type: "InSync", Status: metav1.ConditionTrue, Reason: "ScopedConverged"}},
	}}
}

func TestConvergedOn(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("c", 64)
	if !convergedOn(convergedSchema(digest), digest) {
		t.Fatal("a converged schema was not recognized")
	}
	for _, test := range []struct {
		name   string
		mutate func(*ptahv1alpha1.PtahSchema)
	}{
		{"not InSync", func(s *ptahv1alpha1.PtahSchema) { s.Status.Phase = ptahv1alpha1.PhaseApplying }},
		{"another source", func(s *ptahv1alpha1.PtahSchema) { s.Status.Source.Digest = "sha256:other" }},
		{"nothing applied", func(s *ptahv1alpha1.PtahSchema) { s.Status.Applied = nil }},
		{"another artifact applied", func(s *ptahv1alpha1.PtahSchema) { s.Status.Applied.ArtifactDigest = "sha256:other" }},
		{"an observation pending", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.PendingObservation = &ptahv1alpha1.PendingObservationStatus{}
		}},
		{"an operation running", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{}
		}},
		{"converged for another reason", func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions[0].Reason = "Converged" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			schema := convergedSchema(digest)
			test.mutate(schema)
			if convergedOn(schema, digest) {
				t.Fatal("the mistake passed as convergence")
			}
		})
	}
}

func TestAwaitingApprovalOf(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("d", 64)
	awaiting := func() *ptahv1alpha1.PtahSchema {
		return &ptahv1alpha1.PtahSchema{Status: ptahv1alpha1.PtahSchemaStatus{
			Phase: ptahv1alpha1.PhaseAwaitingApproval, Source: ptahv1alpha1.SchemaSourceStatus{Digest: digest},
			Plan: &ptahv1alpha1.CurrentPlanStatus{Name: "plan"},
		}}
	}
	if !awaitingApprovalOf(awaiting(), digest) {
		t.Fatal("a schema awaiting approval was not recognized")
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahSchema){
		"another phase":  func(s *ptahv1alpha1.PtahSchema) { s.Status.Phase = ptahv1alpha1.PhaseBlocked },
		"no plan":        func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan = nil },
		"another source": func(s *ptahv1alpha1.PtahSchema) { s.Status.Source.Digest = "sha256:other" },
	} {
		schema := awaiting()
		mutate(schema)
		if awaitingApprovalOf(schema, digest) {
			t.Errorf("%s passed as awaiting approval", name)
		}
	}
}

func TestUnsettledContainerLines(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 450)
	pods := []corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Name: "manager"},
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name:                 "verify-candidate-runtime",
				State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: long}},
			}},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "manager", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
				{Name: "waiting", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}},
			},
		},
	}}
	want := []string{
		"manager/verify-candidate-runtime: waiting=CrashLoopBackOff lastExit=1 message=" + strings.Repeat("x", 400),
		"manager/waiting: waiting=PodInitializing lastExit=- message=-",
	}
	if got := unsettledContainerLines(pods); !reflect.DeepEqual(got, want) {
		t.Fatalf("unsettledContainerLines() = %q, want %q", got, want)
	}
}

func TestCutLines(t *testing.T) {
	t.Parallel()
	got := cutLines([]byte("abcdef\nab\n"), 3)
	if string(got) != "abc\nab\n" {
		t.Fatalf("cutLines() = %q", got)
	}
}

// The policy's expressions are the ones the shell built with jq's tojson: the
// schemas it matches by their subject label, and the one annotation value it
// admits.
func TestPodMetadataPolicyDocuments(t *testing.T) {
	t.Parallel()
	documents := podMetadataPolicyDocuments(podMetadataPolicyName, "ptah-e2e", "e2e-pod-metadata-postgresql",
		"e2e-pod-metadata-refused-postgresql", podMetadataAnnotation)
	if len(documents) != 2 || documents[0]["kind"] != "ValidatingAdmissionPolicy" ||
		documents[1]["kind"] != "ValidatingAdmissionPolicyBinding" {
		t.Fatalf("the documents are not the policy and its binding: %v", documents)
	}
	condition, _ := field(t, documents[0], "spec", "matchConditions")
	wantCondition := `has(object.metadata.labels) && "operator.ptah.run/schema" in object.metadata.labels && ` +
		`object.metadata.labels["operator.ptah.run/schema"] in ["e2e-pod-metadata-postgresql", "e2e-pod-metadata-refused-postgresql"]`
	if got := condition.([]any)[0].(map[string]any)["expression"]; got != wantCondition {
		t.Errorf("match condition = %s, want %s", got, wantCondition)
	}
	validations, _ := field(t, documents[0], "spec", "validations")
	wantValidation := `has(object.metadata.annotations) && "sidecar.istio.io/inject" in object.metadata.annotations && ` +
		`object.metadata.annotations["sidecar.istio.io/inject"] == "false"`
	validation := validations.([]any)[0].(map[string]any)
	if validation["expression"] != wantValidation || validation["message"] != podMetadataRefusal {
		t.Errorf("validation = %v, want %s", validation, wantValidation)
	}
	if policy, _ := field(t, documents[0], "spec", "failurePolicy"); policy != "Fail" {
		t.Errorf("failurePolicy = %v, want Fail", policy)
	}
	selector, _ := field(t, documents[1], "spec", "matchResources", "namespaceSelector", "matchLabels")
	if !reflect.DeepEqual(selector, map[string]any{"kubernetes.io/metadata.name": "ptah-e2e"}) {
		t.Errorf("the binding reaches %v, not the test namespace alone", selector)
	}
	if name, _ := field(t, documents[1], "spec", "policyName"); name != podMetadataPolicyName {
		t.Errorf("the binding names policy %v", name)
	}
	if got := celString(`a"b\c`); got != `"a\"b\\c"` {
		t.Errorf("celString() = %s", got)
	}
}

// The probe carries what the policy selects and not what it admits, so its
// refusal can only be the policy's.
func TestPodMetadataProbePod(t *testing.T) {
	t.Parallel()
	probe := podMetadataProbePod("ptah-e2e", "refused", "executor@sha256:a")
	labels, _ := field(t, probe, "metadata", "labels")
	if !reflect.DeepEqual(labels, map[string]any{"operator.ptah.run/schema": "refused"}) {
		t.Errorf("probe labels = %v", labels)
	}
	if _, found := field(t, probe, "metadata", "annotations"); found {
		t.Error("the probe carries annotations, so the policy might admit it")
	}
}

func refusedReport(job string) *ptahv1alpha1.PtahSchema {
	return &ptahv1alpha1.PtahSchema{Status: ptahv1alpha1.PtahSchemaStatus{
		ActiveOperation: &ptahv1alpha1.ActiveOperationStatus{JobName: job},
		Conditions: []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionFalse, Reason: "PodAdmissionRefused",
				Message: "Job " + job + ": ValidatingAdmissionPolicy 'e2e-pod-metadata-mesh-opt-out' denied request: " + podMetadataRefusal},
			{Type: "Applying", Status: metav1.ConditionFalse, Reason: "Waiting"},
		},
	}}
}

func TestPodAdmissionRefusalReported(t *testing.T) {
	t.Parallel()
	if !podAdmissionRefusalReported(refusedReport("job-a")) {
		t.Fatal("a reported refusal was not recognized")
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahSchema){
		"no operation":   func(s *ptahv1alpha1.PtahSchema) { s.Status.ActiveOperation = nil },
		"another reason": func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions[0].Reason = "OperationFailed" },
		"ready":          func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions[0].Status = metav1.ConditionTrue },
		"another refusal": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Conditions[0].Message = "ValidatingAdmissionPolicy denied request"
		},
		"another admission": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Conditions[0].Message = "admission webhook denied: " + podMetadataRefusal
		},
	} {
		schema := refusedReport("job-a")
		mutate(schema)
		if podAdmissionRefusalReported(schema) {
			t.Errorf("%s passed as a reported refusal", name)
		}
	}
}

func TestReadyConditionNamesJob(t *testing.T) {
	t.Parallel()
	if !readyConditionNamesJob(refusedReport("job-a"), "job-a") {
		t.Fatal("the Ready condition naming the Job was not recognized")
	}
	if readyConditionNamesJob(refusedReport("job-a"), "job-b") {
		t.Error("a Ready condition naming another Job passed")
	}
	twice := refusedReport("job-a")
	twice.Status.Conditions = append(twice.Status.Conditions, twice.Status.Conditions[0])
	if readyConditionNamesJob(twice, "job-a") {
		t.Error("two Ready conditions passed as one")
	}
}

func TestRefusedJobIdle(t *testing.T) {
	t.Parallel()
	idle := batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
		{Type: batchv1.JobFailed, Status: corev1.ConditionFalse},
	}}}
	if !refusedJobIdle(&idle) {
		t.Fatal("a Job with no Pod and no verdict was not recognized")
	}
	for name, mutate := range map[string]func(*batchv1.Job){
		"a Pod running":   func(j *batchv1.Job) { j.Status.Active = 1 },
		"a Pod succeeded": func(j *batchv1.Job) { j.Status.Succeeded = 1 },
		"a Pod failed":    func(j *batchv1.Job) { j.Status.Failed = 1 },
		"a verdict":       func(j *batchv1.Job) { j.Status.Conditions[0].Status = corev1.ConditionTrue },
	} {
		job := idle.DeepCopy()
		mutate(job)
		if refusedJobIdle(job) {
			t.Errorf("%s passed as idle", name)
		}
	}
}

func TestFailedCreateNamesPolicy(t *testing.T) {
	t.Parallel()
	event := corev1.Event{
		InvolvedObject: corev1.ObjectReference{Name: "job-a"}, Reason: "FailedCreate",
		Message: `Error creating: pods "job-a-x" is forbidden: ValidatingAdmissionPolicy 'e2e-pod-metadata-mesh-opt-out' denied request`,
	}
	if !failedCreateNamesPolicy([]corev1.Event{event}, "job-a", podMetadataPolicyName) {
		t.Fatal("the FailedCreate naming the policy was not recognized")
	}
	for name, mutate := range map[string]func(*corev1.Event){
		"another Job":    func(e *corev1.Event) { e.InvolvedObject.Name = "job-b" },
		"another reason": func(e *corev1.Event) { e.Reason = "SuccessfulCreate" },
		"another policy": func(e *corev1.Event) { e.Message = "forbidden: ValidatingAdmissionPolicy 'other' denied request" },
	} {
		changed := event
		mutate(&changed)
		if failedCreateNamesPolicy([]corev1.Event{changed}, "job-a", podMetadataPolicyName) {
			t.Errorf("%s passed", name)
		}
	}
	if failedCreateNamesPolicy(nil, "job-a", podMetadataPolicyName) {
		t.Error("no Event passed")
	}
}

func declaredJob(name, operation string) batchv1.Job {
	labels := map[string]string{
		"app.kubernetes.io/managed-by": "ptah-operator", "app.kubernetes.io/component": "schema-operation",
		"operator.ptah.run/schema": "schema", "operator.ptah.run/operation": operation,
		"operator.ptah.run/operation-id": "0123456789abcdef", podMetadataLabel: "platform",
	}
	annotations := map[string]string{podMetadataAnnotation: "false"}
	job := batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, Annotations: annotations}}
	job.Spec.Template.Labels = map[string]string{podMetadataLabel: "platform"}
	job.Spec.Template.Annotations = map[string]string{podMetadataAnnotation: "false"}
	return job
}

func TestDeclaredMetadataOnJobs(t *testing.T) {
	t.Parallel()
	jobs := func() []batchv1.Job {
		return []batchv1.Job{declaredJob("resolve", "resolve"), declaredJob("plan", "plan"), declaredJob("apply", "apply")}
	}
	if err := declaredMetadataOnJobs(jobs(), podMetadataLabel, podMetadataAnnotation); err != nil {
		t.Fatalf("Jobs carrying the declaration were refused: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func([]batchv1.Job) []batchv1.Job
	}{
		{"no label on a Job", func(j []batchv1.Job) []batchv1.Job { delete(j[0].Labels, podMetadataLabel); return j }},
		{"no label on a template", func(j []batchv1.Job) []batchv1.Job {
			j[1].Spec.Template.Labels[podMetadataLabel] = "other"
			return j
		}},
		{"no annotation on a Job", func(j []batchv1.Job) []batchv1.Job { j[0].Annotations = nil; return j }},
		{"annotation on a template changed", func(j []batchv1.Job) []batchv1.Job {
			j[2].Spec.Template.Annotations[podMetadataAnnotation] = "true"
			return j
		}},
		{"an operator label too many", func(j []batchv1.Job) []batchv1.Job {
			j[0].Labels["operator.ptah.run/extra"] = "x"
			return j
		}},
		{"an operator label missing", func(j []batchv1.Job) []batchv1.Job {
			delete(j[0].Labels, "app.kubernetes.io/managed-by")
			return j
		}},
		{"no Apply", func(j []batchv1.Job) []batchv1.Job { return j[:2] }},
		{"two Applies", func(j []batchv1.Job) []batchv1.Job { return append(j, declaredJob("apply-2", "apply")) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := declaredMetadataOnJobs(test.mutate(jobs()), podMetadataLabel, podMetadataAnnotation); err == nil {
				t.Fatal("the mistake passed as the declaration")
			}
		})
	}
}

func TestDeclaredAnnotationOnPods(t *testing.T) {
	t.Parallel()
	admitted := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{podMetadataAnnotation: "false"}}}
	if !declaredAnnotationOnPods([]corev1.Pod{admitted, admitted}, podMetadataAnnotation) {
		t.Fatal("Pods carrying the annotation were refused")
	}
	for name, pod := range map[string]corev1.Pod{
		"no annotation":    {},
		"another value":    {ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{podMetadataAnnotation: "true"}}},
		"another spelling": {ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{podMetadataAnnotation: "False"}}},
	} {
		if declaredAnnotationOnPods([]corev1.Pod{admitted, pod}, podMetadataAnnotation) {
			t.Errorf("a Pod with %s passed", name)
		}
	}
}
