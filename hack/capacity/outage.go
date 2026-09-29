package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stokaro/ptah-operator/internal/runner"
	operationworkload "github.com/stokaro/ptah-operator/internal/workload"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// outage has an observed failure on both families as well as a timed hold.
// Creating a NetworkPolicy alone is not evidence that any operation failed.
func (s *scenarios) outage(ctx context.Context) (resultErr error) {
	if s.load.Outage.Duration == 0 {
		return nil
	}
	if s.in.registryIP == "" {
		return errors.New("an outage needs the registry address")
	}
	policy, err := s.clientset.NetworkingV1().NetworkPolicies(s.in.namespace).Create(ctx, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: outagePolicyName, Namespace: s.in.namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/managed-by": "ptah-operator"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{
				IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{s.in.registryIP + "/32"}},
			}}}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("cut the registry off: %w", err)
	}
	restore := func(restoreCtx context.Context) error {
		err := s.clientset.NetworkingV1().NetworkPolicies(s.in.namespace).Delete(restoreCtx, policy.Name,
			metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &policy.UID}})
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	restored := false
	defer func() {
		if !restored {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			resultErr = errors.Join(resultErr, restore(cleanupCtx))
		}
	}()
	start := time.Now().UTC()
	proof := map[string]string{}
	deadline := start.Add(s.load.Outage.Duration)
	for {
		pods, err := s.clientset.CoreV1().Pods(s.in.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/managed-by=ptah-operator,operator.ptah.run/operation=resolve",
		})
		if err != nil {
			return fmt.Errorf("read the registry failure evidence: %w", err)
		}
		for _, pod := range pods.Items {
			family, digest := s.failedResolve(pod, start)
			if family != "" {
				proof[family+"PodUID"], proof[family+"FrameDigest"] = string(pod.UID), digest
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(pollEvery, time.Until(deadline))):
		}
	}
	s.mark("registry outage", start, proof)
	if s.load.Schemas > 0 && proof["schemaPodUID"] == "" || s.load.Migrations > 0 && proof["migrationPodUID"] == "" {
		return errors.New("the registry outage produced no fresh Resolve failure for every loaded family")
	}
	recoveryStart := time.Now().UTC()
	if err := restore(ctx); err != nil {
		return fmt.Errorf("restore the registry: %w", err)
	}
	restored = true
	converged, err := s.waitConverged(ctx, recoveryStart, nil)
	s.mark("recovery", recoveryStart, map[string]string{"converged": converged})
	return err
}

// A successful Job only transports a runner result. Read that result's bounded
// termination summary, bound to the Pod's operation ID, without retaining logs.
func (s *scenarios) failedResolve(pod corev1.Pod, after time.Time) (family, digest string) {
	if pod.UID == "" || !pod.CreationTimestamp.After(after) || pod.Labels[operationworkload.LabelOperation] != "resolve" ||
		pod.Annotations[operationworkload.AnnotationOperationID] == "" {
		return "", ""
	}
	for i := range s.load.Schemas {
		if pod.Labels[operationworkload.LabelSchema] == s.schemaName(i) {
			family = "schema"
		}
	}
	for i := range s.load.Migrations {
		if pod.Labels[operationworkload.LabelMigration] == s.migrationName(i) {
			family = "migration"
		}
	}
	if family == "" {
		return "", ""
	}
	for _, container := range pod.Status.ContainerStatuses {
		end := container.State.Terminated
		if container.Name != "ptah" || end == nil || end.ExitCode != 0 || !end.FinishedAt.After(after) {
			continue
		}
		summary, err := runner.ParseSummaryFor(end.Message, runner.OperationResolve, pod.Annotations[operationworkload.AnnotationOperationID])
		if err == nil && summary.ErrorCode == "child_exit" && !summary.MutationStarted && !summary.Uncertain {
			return family, summary.FrameDigest
		}
	}
	return "", ""
}
