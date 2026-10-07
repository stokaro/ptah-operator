package main

import (
	"context"
	"fmt"
	"time"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
)

func waitCapacityPoll(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(pollEvery):
		return nil
	}
}

func approvalGateReady(item *unstructured.Unstructured) bool {
	var migration operatorv1alpha1.PtahMigration
	if runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, &migration) != nil ||
		migration.UID == "" || migration.Generation < 1 || migration.Status.ObservedGeneration != migration.Generation ||
		migration.Spec.Policy.Apply != operatorv1alpha1.ApplyPolicyOnApproval || migration.Status.Plan == nil ||
		migration.Status.Plan.Name == "" || migration.Status.Plan.UID == "" || migration.Status.ActiveOperation != nil {
		return false
	}
	for _, condition := range migration.Status.Conditions {
		if condition.Type == "ApprovalRequired" && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == migration.Generation {
			return true
		}
	}
	return false
}

// Watch from the pre-approval reading so even a fast Apply leaves its original
// approval and plan binding available for verification after the claim retires.
func (s *scenarios) approveAndWait(ctx context.Context, start time.Time) (map[string]string, error) {
	outcome := map[string]string{}
	ctx, cancel := context.WithDeadline(ctx, start.Add(restartReadyBudget+s.load.Settle.Duration))
	defer cancel()
	admissionCtx, admissionCancel := context.WithDeadline(ctx, start.Add(restartReadyBudget))
	defer admissionCancel()
	resource := s.dynamic.Resource(migrationResource).Namespace(s.in.namespace)
	var migration *unstructured.Unstructured
	for {
		if err := admissionCtx.Err(); err != nil {
			return outcome, err
		}
		var err error
		migration, err = resource.Get(admissionCtx, approvalResource, metav1.GetOptions{})
		if err != nil {
			return outcome, fmt.Errorf("read approval workload: %w", err)
		}
		if approvalGateReady(migration) {
			break
		}
		// Periodic refresh can temporarily hold an operation while the
		// approval remains required. Wait for that refresh to settle.
		if err := waitCapacityPoll(admissionCtx); err != nil {
			return outcome, fmt.Errorf("approval gate did not settle: %w", err)
		}
	}
	approval, err := s.approvalFor(admissionCtx, migration)
	if err != nil {
		return outcome, err
	}
	// A missing resourceVersion cannot establish an uninterrupted history.
	if migration.GetResourceVersion() == "" {
		return outcome, fmt.Errorf("approval workload has no resourceVersion")
	}
	stream, err := resource.Watch(ctx, metav1.ListOptions{FieldSelector: "metadata.name=" + approvalResource, ResourceVersion: migration.GetResourceVersion()})
	if err != nil {
		return outcome, fmt.Errorf("watch approval workload: %w", err)
	}
	defer stream.Stop()
	for {
		created, err := s.approvalWriter().Resource(approvalGVR).Namespace(s.in.namespace).Create(admissionCtx, approval, metav1.CreateOptions{})
		if err == nil {
			if created.GetUID() == "" || created.GetCreationTimestamp().Time.IsZero() || created.GetCreationTimestamp().Time.Before(start.Truncate(time.Second)) {
				return outcome, fmt.Errorf("admitted approval lacks a fresh API identity")
			}
			approval = created
			outcome["approvalUID"] = string(created.GetUID())
			outcome["approvalAdmitted"] = time.Since(start).Round(time.Second).String()
			break
		}
		// An existing object is not evidence that this attempt was admitted.
		// A lost response is inconclusive too; never assign it a made-up time.
		if apierrors.IsAlreadyExists(err) {
			return outcome, fmt.Errorf("approval admission is ambiguous: %w", err)
		}
		if waitErr := waitCapacityPoll(admissionCtx); waitErr != nil {
			return outcome, fmt.Errorf("approval was not admitted: %w (last create: %v)", waitErr, err)
		}
	}
	return s.waitApprovedDispatch(ctx, start, migration, approval, stream, outcome)
}

func (s *scenarios) approvalFor(ctx context.Context, migration *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if migration.GetNamespace() == "" || !approvalGateReady(migration) {
		return nil, fmt.Errorf("approval workload has no current approval gate")
	}
	planName, _, _ := unstructured.NestedString(migration.Object, "status", "plan", "name")
	planUID, _, _ := unstructured.NestedString(migration.Object, "status", "plan", "uid")
	plan, err := s.dynamic.Resource(migrationPlanResource).Namespace(migration.GetNamespace()).Get(ctx, planName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	ownerName, _, _ := unstructured.NestedString(plan.Object, "spec", "migrationRef", "name")
	ownerUID, _, _ := unstructured.NestedString(plan.Object, "spec", "migrationRef", "uid")
	fingerprint, _, _ := unstructured.NestedString(plan.Object, "spec", "fingerprint")
	if string(plan.GetUID()) != planUID || ownerName != migration.GetName() || ownerUID != string(migration.GetUID()) || fingerprint == "" {
		return nil, fmt.Errorf("approval plan does not bind the original migration and plan UIDs")
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahMigrationApproval",
		"metadata": map[string]any{"name": approvalResource, "namespace": migration.GetNamespace()},
		"spec": map[string]any{
			"migrationRef":    map[string]any{"name": migration.GetName(), "uid": string(migration.GetUID())},
			"planRef":         map[string]any{"name": planName, "uid": planUID},
			"planFingerprint": fingerprint,
		},
	}}, nil
}

func approvedClaim(item, original, approval *unstructured.Unstructured) (*operatorv1alpha1.PtahMigration, error) {
	if item.GetUID() != original.GetUID() || item.GetName() != original.GetName() || item.GetNamespace() != original.GetNamespace() || item.GetGeneration() != original.GetGeneration() {
		return nil, fmt.Errorf("approval workload identity or generation changed")
	}
	var migration operatorv1alpha1.PtahMigration
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, &migration); err != nil {
		return nil, err
	}
	op := migration.Status.ActiveOperation
	if op == nil || op.Type != operatorv1alpha1.MigrationOperationApply {
		return nil, nil
	}
	planName, _, _ := unstructured.NestedString(approval.Object, "spec", "planRef", "name")
	planUID, _, _ := unstructured.NestedString(approval.Object, "spec", "planRef", "uid")
	if op.ApprovalRef == nil || op.ApprovalRef.Name != approval.GetName() || op.ApprovalRef.UID != approval.GetUID() ||
		op.PlanRef == nil || op.PlanRef.Name != planName || string(op.PlanRef.UID) != planUID ||
		op.StartedAt.Time.Before(approval.GetCreationTimestamp().Time) {
		return nil, fmt.Errorf("Apply claim does not bind the admitted approval and plan")
	}
	// The claim precedes dispatch. Wait for the status that recorded the Job
	// UID and its resolved admission snapshot before matching the envelope.
	if op.JobUID == "" || op.AdmissionSnapshot == nil {
		return nil, nil
	}
	return &migration, nil
}

func (s *scenarios) waitApprovedDispatch(ctx context.Context, start time.Time, original, approval *unstructured.Unstructured, stream watch.Interface, outcome map[string]string) (map[string]string, error) {
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	var claimed *operatorv1alpha1.PtahMigration
	for {
		if claimed != nil {
			op := claimed.Status.ActiveOperation
			job, err := s.clientset.BatchV1().Jobs(s.in.namespace).Get(ctx, op.JobName, metav1.GetOptions{})
			if err == nil {
				if err := jobclaim.Match(job, jobclaim.MigrationOperation(claimed, op)); err != nil {
					return outcome, fmt.Errorf("approved Apply Job: %w", err)
				}
				if job.CreationTimestamp.IsZero() || job.CreationTimestamp.Before(&op.StartedAt) {
					return outcome, fmt.Errorf("approved Apply Job predates its claim")
				}
				outcome["approvalDispatched"] = job.CreationTimestamp.Sub(start).Round(time.Second).String()
				outcome["approvalJobUID"] = string(job.UID)
				outcome["approvalOperationID"] = op.ID
				outcome["approvalMigrationUID"] = string(claimed.UID)
				outcome["approvalPlanUID"] = string(op.PlanRef.UID)
				return outcome, s.waitApprovedRecovery(ctx, original, job.CreationTimestamp.Time, outcome, start)
			}
			if !apierrors.IsNotFound(err) {
				return outcome, fmt.Errorf("read approved Apply Job: %w", err)
			}
		}
		select {
		case <-ctx.Done():
			return outcome, fmt.Errorf("approved Apply was not proven dispatched: %w", ctx.Err())
		case <-ticker.C:
		case event, ok := <-stream.ResultChan():
			if !ok || event.Type != watch.Added && event.Type != watch.Modified {
				return outcome, fmt.Errorf("approval workload watch ended or lost its resource: %s", event.Type)
			}
			item, ok := event.Object.(*unstructured.Unstructured)
			if !ok {
				return outcome, fmt.Errorf("approval workload watch returned an unexpected object")
			}
			candidate, err := approvedClaim(item, original, approval)
			if err != nil {
				return outcome, err
			}
			if candidate != nil {
				claimed = candidate
			}
		}
	}
}

func (s *scenarios) waitApprovedRecovery(ctx context.Context, original *unstructured.Unstructured, after time.Time, outcome map[string]string, start time.Time) error {
	for {
		item, err := s.dynamic.Resource(migrationResource).Namespace(s.in.namespace).Get(ctx, original.GetName(), metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read approved workload recovery: %w", err)
		}
		if item.GetUID() != original.GetUID() || item.GetGeneration() != original.GetGeneration() {
			return fmt.Errorf("approved workload identity or generation changed during recovery")
		}
		reading, err := readCycle("migration", "GET", item, time.Now().UTC())
		if err != nil {
			return err
		}
		if cycleReady("migration", reading) && !reading.CompletedAt.Before(after) {
			outcome["approvalConverged"] = time.Since(start).Round(time.Second).String()
			return nil
		}
		if err := waitCapacityPoll(ctx); err != nil {
			return fmt.Errorf("approved workload did not converge: %w", err)
		}
	}
}
