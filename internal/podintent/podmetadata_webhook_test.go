package podintent_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// What spec.execution.podMetadata declares travels on the Job template, and
// the webhook holds a Pod to its template: a Pod carrying exactly the declared
// metadata is admitted, one carrying more is refused as before, and one a
// mutating admission stripped of a declared key is refused too, because the
// template digest the snapshot pinned still names it.

func declaredFixture(t *testing.T, subjectKind string) (*corev1.Pod, func(*corev1.Pod) bool) {
	t.Helper()
	jobName := "apply-job"
	if subjectKind == "PtahMigration" {
		jobName = "ptah-m-history-app"
	}
	handler, pod := subjectFixtureDeclaring(t, jobName, subjectKind,
		map[string]string{"acme.example/team": "platform"},
		map[string]string{"sidecar.istio.io/inject": "false"})
	return pod, func(candidate *corev1.Pod) bool {
		return handler.Handle(context.Background(), podRequest(t, candidate)).Allowed
	}
}

func TestValidationHandlerAdmitsExactlyTheDeclaredPodMetadata(t *testing.T) {
	t.Parallel()

	for _, subjectKind := range []string{"PtahSchema", "PtahMigration"} {
		t.Run(subjectKind, func(t *testing.T) {
			t.Parallel()
			pod, admitted := declaredFixture(t, subjectKind)
			if pod.Labels["acme.example/team"] != "platform" || pod.Annotations["sidecar.istio.io/inject"] != "false" {
				t.Fatalf("the fixture's Pod carries no declared metadata: %v %v", pod.Labels, pod.Annotations)
			}
			if !admitted(pod) {
				t.Fatal("Handle() refused a Pod carrying exactly the declared metadata")
			}

			rows := map[string]func(*corev1.Pod){
				"a declared label stripped":      func(pod *corev1.Pod) { delete(pod.Labels, "acme.example/team") },
				"a declared annotation stripped": func(pod *corev1.Pod) { delete(pod.Annotations, "sidecar.istio.io/inject") },
				"a declared label rewritten":     func(pod *corev1.Pod) { pod.Labels["acme.example/team"] = "someone-else" },
				"a declared annotation rewritten": func(pod *corev1.Pod) {
					pod.Annotations["sidecar.istio.io/inject"] = "true"
				},
				"a label beyond the declaration": func(pod *corev1.Pod) { pod.Labels["acme.example/injected"] = "true" },
				"an annotation beyond the declaration": func(pod *corev1.Pod) {
					pod.Annotations["sidecar.istio.io/status"] = "injected"
				},
			}
			for name, mutate := range rows {
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					candidate := pod.DeepCopy()
					mutate(candidate)
					if admitted(candidate) {
						t.Fatalf("Handle() admitted a Pod with %s", name)
					}
				})
			}
		})
	}
}
