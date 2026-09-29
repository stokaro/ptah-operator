package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/internal/runner"
	operationworkload "github.com/stokaro/ptah-operator/internal/workload"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func resolvePod(t *testing.T, family string, at time.Time, mutate func(*runner.Result)) corev1.Pod {
	t.Helper()
	result := runner.Result{Operation: runner.OperationResolve, OperationID: "resolve-id", ChildExitCode: 1,
		Error: &runner.ResultError{Code: "child_exit", Message: "Ptah exited with code 1"}}
	if mutate != nil {
		mutate(&result)
	}
	encoded, err := runner.EncodeResult(result)
	if err != nil || encoded.SummaryErr != nil {
		t.Fatalf("encode the result: %v / %v", err, encoded.SummaryErr)
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "resolve", Namespace: "work", UID: "pod-uid",
			CreationTimestamp: metav1.NewTime(at),
			Labels: map[string]string{operationworkload.LabelOperation: "resolve",
				"operator.ptah.run/" + family: "capacity-" + family + "-000"},
			Annotations: map[string]string{operationworkload.AnnotationOperationID: "resolve-id"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "ptah",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				FinishedAt: metav1.NewTime(at.Add(time.Second)), Message: string(encoded.Summary), ExitCode: 0,
			}}}}},
	}
}

func TestUnobservedOutageFailsAndRemovesOnlyItsPolicy(t *testing.T) {
	t.Parallel()
	for _, readError := range []bool{false, true} {
		clientset := fake.NewClientset()
		clientset.PrependReactor("create", "networkpolicies", func(action clienttesting.Action) (bool, runtime.Object, error) {
			action.(clienttesting.CreateAction).GetObject().(*networkingv1.NetworkPolicy).UID = "policy-uid"
			return false, nil, nil
		})
		if readError {
			clientset.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("injected API failure")
			})
		}
		s := &scenarios{clientset: clientset, in: inputs{namespace: "work", registryIP: "192.0.2.1"},
			load: workload{Schemas: 1, Migrations: 1, Outage: duration{time.Nanosecond}}}
		want := "no fresh Resolve failure"
		if readError {
			want = "injected API failure"
		}
		if err := s.outage(context.Background()); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("an unobserved outage must fail at %q: %v", want, err)
		}
		_, err := clientset.NetworkingV1().NetworkPolicies("work").Get(context.Background(), outagePolicyName, metav1.GetOptions{})
		if !apierrors.IsNotFound(err) {
			t.Fatal("a failed measurement left the registry disconnected")
		}
		deletions := 0
		for _, action := range clientset.Actions() {
			if action.GetVerb() != "delete" {
				continue
			}
			deletions++
			options := action.(clienttesting.DeleteAction).GetDeleteOptions()
			if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != "policy-uid" {
				t.Fatal("cleanup could delete a replacement policy")
			}
		}
		if deletions != 1 {
			t.Fatalf("examined %d cleanup writes, want one", deletions)
		}
	}
}

func TestRegistryFailureNeedsAFreshBoundRunnerFailure(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	s := &scenarios{load: workload{Schemas: 1, Migrations: 1}}
	for _, family := range []string{"schema", "migration"} {
		good := resolvePod(t, family, start.Add(time.Second), nil)
		if got, digest := s.failedResolve(good, start); got != family || !strings.HasPrefix(digest, "sha256:") {
			t.Fatalf("the actual runner failure was refused: %s %s", got, digest)
		}
		for name, mutate := range map[string]func(*corev1.Pod){
			"old Pod":              func(p *corev1.Pod) { p.CreationTimestamp = metav1.NewTime(start.Add(-time.Second)) },
			"no UID":               func(p *corev1.Pod) { p.UID = "" },
			"unrelated resource":   func(p *corev1.Pod) { p.Labels["operator.ptah.run/"+family] = "unrelated" },
			"another operation":    func(p *corev1.Pod) { p.Labels[operationworkload.LabelOperation] = "verify" },
			"missing operation ID": func(p *corev1.Pod) { p.Annotations = nil },
			"wrong operation ID":   func(p *corev1.Pod) { p.Annotations[operationworkload.AnnotationOperationID] = "other-id" },
			"transport failure":    func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.ExitCode = 1 },
			"no summary":           func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.Message = "" },
			"old termination":      func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.FinishedAt = metav1.NewTime(start) },
			"wrong container":      func(p *corev1.Pod) { p.Status.ContainerStatuses[0].Name = "init" },
			"still running":        func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated = nil },
		} {
			t.Run(family+"/"+name, func(t *testing.T) {
				pod := good.DeepCopy()
				mutate(pod)
				if got, _ := s.failedResolve(*pod, start); got != "" {
					t.Fatal("accepted a Pod that does not prove the outage")
				}
			})
		}
		for _, code := range []string{"", "dispatch_deadline_expired"} {
			pod := resolvePod(t, family, start.Add(time.Second), func(r *runner.Result) {
				if code == "" {
					r.ChildExitCode, r.Error = 0, nil
					r.ResolvedDigest = "sha256:" + strings.Repeat("1", 64)
					r.ResolvedReference = "oci://registry/schema@" + r.ResolvedDigest
					r.ResolvedMediaType = "application/vnd.oci.image.manifest.v1+json"
				} else {
					r.Error.Code = code
				}
			})
			if got, _ := s.failedResolve(pod, start); got != "" {
				t.Fatalf("accepted the wrong result code %q", code)
			}
		}
	}
}
