package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	ptah "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/runner"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

const faultDeliveryRefusal = "ptah-runner: durable result delivery failed: result receiver returned HTTP 403"

// Suspension changes the resource generation while SQL holds the database
// lock. The old generation cannot publish its result. Require that exact
// delivery refusal after a completed mutation, not an arbitrary failed Job.
// The controller must still preserve Unknown, and the later database readback
// and continuous Job history must establish recovery without another Apply.
func validateFaultApplyResult(original *ptah.PtahMigration, job *batchv1.Job, pod *corev1.Pod, diagnostic string) error {
	if original == nil || original.Status.ActiveOperation == nil || original.Status.History == nil || job == nil || pod == nil {
		return fmt.Errorf("retention fault lacks its original Apply evidence")
	}
	op := original.Status.ActiveOperation
	claim := jobclaim.MigrationOperation(original, op)
	if err := jobclaim.Match(job, claim); err != nil {
		return err
	}
	if err := podintent.ValidateStoredPod(pod, job, claim.Snapshot); err != nil {
		return err
	}
	_, failed, done := jobEnd(job)
	if !done || !failed || job.Status.Active != 0 || pod.Status.Phase != corev1.PodFailed || len(pod.Status.ContainerStatuses) != 1 {
		return fmt.Errorf("retention fault Apply has no terminal delivery refusal")
	}
	if len(pod.Status.InitContainerStatuses) != len(pod.Spec.InitContainers) {
		return fmt.Errorf("retention fault Apply lacks completed initialization")
	}
	for _, status := range pod.Status.InitContainerStatuses {
		if status.RestartCount != 0 || status.State.Terminated == nil || status.State.Terminated.ExitCode != 0 {
			return fmt.Errorf("retention fault Apply failed during initialization")
		}
	}
	status := pod.Status.ContainerStatuses[0]
	ended := status.State.Terminated
	if status.Name != "ptah" || status.RestartCount != 0 || ended == nil || ended.ExitCode != 2 || ended.Reason != "Error" {
		return fmt.Errorf("retention fault Apply did not exit on result delivery")
	}
	summary, err := runner.ParseSummaryFor(ended.Message, runner.OperationMigrationApply, op.ID)
	if err != nil {
		return err
	}
	version := original.Status.History.CurrentVersion + 1
	if !summary.MutationStarted || summary.Uncertain || summary.ErrorCode != "" || summary.CoordinationDigest != op.CoordinationDigest || summary.TargetIdentityDigest != original.Status.History.TargetIdentityDigest || summary.Migration == nil || summary.Migration.Outcome != dataplane.MigrationOutcomeApplied || summary.Migration.AppliedCount != 1 || summary.Migration.FirstApplied != version || summary.Migration.LastApplied != version {
		return fmt.Errorf("retention fault Apply did not complete its one pending migration")
	}
	for _, line := range strings.Split(diagnostic, "\n") {
		if line == faultDeliveryRefusal {
			return nil
		}
	}
	return fmt.Errorf("retention fault Apply lacks the expected delivery authority refusal")
}

func (s *scenarios) recordFaultApplyResult(ctx context.Context, proof *retentionFaultProof) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	original := proof.DispatchedMigration
	op := original.Status.ActiveOperation
	for {
		job, err := s.clientset.BatchV1().Jobs(original.Namespace).Get(ctx, op.JobName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := jobclaim.Match(job, jobclaim.MigrationOperation(original, op)); err != nil {
			return err
		}
		proof.ApplyJob = job
		if _, _, done := jobEnd(job); done {
			break
		}
		if err := waitCapacityPoll(ctx); err != nil {
			return fmt.Errorf("retention fault Apply did not terminate: %w", err)
		}
	}
	pods, err := s.clientset.CoreV1().Pods(original.Namespace).List(ctx, metav1.ListOptions{LabelSelector: batchv1.ControllerUidLabel + "=" + string(op.JobUID)})
	if err != nil {
		return err
	}
	if len(pods.Items) != 1 {
		return fmt.Errorf("retention fault Apply has %d Pods, need one", len(pods.Items))
	}
	proof.ApplyPod = &pods.Items[0]
	diagnostic, err := s.clientset.CoreV1().Pods(original.Namespace).GetLogs(proof.ApplyPod.Name, &corev1.PodLogOptions{Container: "ptah", LimitBytes: ptr.To(int64(64 << 10))}).DoRaw(ctx)
	if err != nil {
		return err
	}
	archive, err := writeRetentionEvidence(s.evidenceDir, "retention-fault-apply-result.json", map[string]any{"job": proof.ApplyJob, "pod": proof.ApplyPod, "diagnostic": string(diagnostic)})
	if err != nil {
		return err
	}
	proof.Archives = append(proof.Archives, archive)
	return validateFaultApplyResult(original, proof.ApplyJob, proof.ApplyPod, string(diagnostic))
}
