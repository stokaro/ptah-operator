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
	originals, err := s.registryOutagePolicies(ctx)
	if err != nil {
		return err
	}
	var policies []*networkingv1.NetworkPolicy
	restore := func(restoreCtx context.Context) error {
		var problems []error
		for _, original := range originals {
			problems = append(problems, s.restoreRegistryPolicy(restoreCtx, original))
		}
		for _, policy := range policies {
			if policy.UID == "" {
				problems = append(problems, fmt.Errorf("cannot safely remove outage policy %s/%s without UID", policy.Namespace, policy.Name))
				continue
			}
			err := s.clientset.NetworkingV1().NetworkPolicies(policy.Namespace).Delete(restoreCtx, policy.Name,
				metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &policy.UID}})
			if err != nil && !apierrors.IsNotFound(err) {
				problems = append(problems, err)
			}
		}
		return errors.Join(problems...)
	}
	restored := false
	defer func() {
		if !restored {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			resultErr = errors.Join(resultErr, restore(cleanupCtx))
		}
	}()
	for _, original := range originals {
		disabled := original.DeepCopy()
		disabled.Spec.Egress = nil
		if _, err := s.clientset.NetworkingV1().NetworkPolicies(original.Namespace).Update(ctx, disabled, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("withdraw registry policy %s/%s: %w", original.Namespace, original.Name, err)
		}
	}
	if len(originals) == 0 {
		for _, namespace := range workloadNamespaces(s.in.namespace, s.in.namespaces) {
			policy, err := s.clientset.NetworkingV1().NetworkPolicies(namespace).Create(ctx, &networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: outagePolicyName, Namespace: namespace},
				Spec: networkingv1.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/managed-by": "ptah-operator"}},
					PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
					Egress: []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{
						IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{s.in.registryIP + "/32"}},
					}}}},
				},
			}, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("cut the registry off in %s: %w", namespace, err)
			}
			policies = append(policies, policy)
			if policy.UID == "" {
				return fmt.Errorf("created outage policy in %s has no UID", namespace)
			}
		}
	}
	start := time.Now().UTC()
	proof := map[string]string{}
	deadline := start.Add(s.load.Outage.Duration)
	for {
		for _, namespace := range workloadNamespaces(s.in.namespace, s.in.namespaces) {
			pods, err := s.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: "app.kubernetes.io/managed-by=ptah-operator,operator.ptah.run/operation in (resolve,verify)",
			})
			if err != nil {
				return fmt.Errorf("read the registry failure evidence: %w", err)
			}
			for _, pod := range pods.Items {
				family, digest := s.failedRegistryRead(pod, start)
				if family != "" {
					proof[namespace+"/"+family+"PodUID"], proof[namespace+"/"+family+"FrameDigest"] = string(pod.UID), digest
					proof[namespace+"/"+family+"Operation"] = pod.Labels[operationworkload.LabelOperation]
				}
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
	for _, family := range []struct {
		name  string
		count int
	}{{"schema", s.load.Schemas}, {"migration", s.load.Migrations}} {
		for i := range family.count {
			namespace := s.in.namespaceFor(i)
			if proof[namespace+"/"+family.name+"PodUID"] == "" {
				return fmt.Errorf("the registry outage produced no fresh registry-read failure for %s in %s", family.name, namespace)
			}
		}
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
// A refresh can fail at Verify after Resolve succeeded. Both read the registry,
// so either stage's fresh child failure can prove the outage.
// Verify reports invalid_verification_output when registry diagnostics replace
// its JSON report. A policy refusal is not evidence of an unavailable registry.
// Database operations and transport failures supply no such evidence.
func (s *scenarios) failedRegistryRead(pod corev1.Pod, after time.Time) (family, digest string) {
	operation := runner.Operation(pod.Labels[operationworkload.LabelOperation])
	if operation != runner.OperationResolve && operation != runner.OperationVerify {
		return "", ""
	}
	if pod.UID == "" || !pod.CreationTimestamp.After(after) ||
		pod.Annotations[operationworkload.AnnotationOperationID] == "" {
		return "", ""
	}
	for i := range s.load.Schemas {
		if pod.Namespace == s.in.namespaceFor(i) && pod.Labels[operationworkload.LabelSchema] == s.schemaName(i) {
			family = "schema"
		}
	}
	for i := range s.load.Migrations {
		if pod.Namespace == s.in.namespaceFor(i) && pod.Labels[operationworkload.LabelMigration] == s.migrationName(i) {
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
		summary, err := runner.ParseSummaryFor(end.Message, operation, pod.Annotations[operationworkload.AnnotationOperationID])
		registryFailure := summary.ErrorCode == "child_exit" ||
			(operation == runner.OperationVerify && summary.ErrorCode == "invalid_verification_output")
		if err == nil && registryFailure && !summary.MutationStarted && !summary.Uncertain {
			return family, summary.FrameDigest
		}
	}
	return "", ""
}
