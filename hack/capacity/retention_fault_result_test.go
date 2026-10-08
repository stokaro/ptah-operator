package main

import (
	"errors"
	"strings"
	"testing"

	ptah "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/resultauthority"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	"github.com/stokaro/ptah-operator/internal/runner"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestRetentionFaultRequiresCompletedSQLAndRefusedDelivery(t *testing.T) {
	for _, mode := range []string{"valid", "unfinished", "successful Job", "wrong Job UID", "wrong Pod owner", "running Pod", "initialization failed", "missing initialization", "OOM", "wrong exit", "restart", "missing summary", "wrong operation", "wrong migration", "SQL failure", "wrong target", "wrong realm", "missing refusal", "unavailable receiver"} {
		t.Run(mode, func(t *testing.T) {
			f := resulttest.New(t, "migration-apply-admitted-scheduling")
			original := f.Subject.(*ptah.PtahMigration)
			op := original.Status.ActiveOperation
			op.CoordinationDigest = "sha256:" + strings.Repeat("a", 64)
			original.Status.History = &ptah.MigrationHistoryStatus{CurrentVersion: 10, TargetIdentityDigest: "sha256:" + strings.Repeat("b", 64)}
			result := runner.Result{Operation: runner.OperationMigrationApply, OperationID: op.ID, MutationStarted: true,
				CoordinationDigest: op.CoordinationDigest, TargetIdentityDigest: original.Status.History.TargetIdentityDigest,
				MigrationRun: &dataplane.MigrationRunReport{ContractVersion: 1, Direction: "up", Outcome: dataplane.MigrationOutcomeApplied, Planned: []int64{11}, Applied: []int64{11}}}
			if mode == "wrong operation" {
				result.OperationID = "another-operation"
			}
			if mode == "wrong migration" {
				result.MigrationRun.Planned, result.MigrationRun.Applied = []int64{12}, []int64{12}
			}
			if mode == "SQL failure" {
				result.Error = &runner.ResultError{Code: "execution_error", Message: "SQL failed"}
				result.ChildExitCode = 1
				result.MigrationRun.Outcome = dataplane.MigrationOutcomeFailed
				result.MigrationRun.Applied = nil
			}
			if mode == "wrong target" {
				result.TargetIdentityDigest = "sha256:" + strings.Repeat("c", 64)
			}
			if mode == "wrong realm" {
				result.CoordinationDigest = "sha256:" + strings.Repeat("c", 64)
			}
			summary, err := runner.EncodeSummary(result)
			if err != nil {
				t.Fatal(err)
			}
			f.Job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
			f.Pod.Status.Phase = corev1.PodFailed
			for _, container := range f.Pod.Spec.InitContainers {
				f.Pod.Status.InitContainerStatuses = append(f.Pod.Status.InitContainerStatuses, corev1.ContainerStatus{Name: container.Name, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}})
			}
			ended := &corev1.ContainerStateTerminated{ExitCode: 2, Reason: "Error", Message: string(summary)}
			f.Pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "ptah", State: corev1.ContainerState{Terminated: ended}}}
			diagnostic := faultDeliveryRefusal + "\n"
			switch mode {
			case "unfinished":
				f.Job.Status.Conditions = nil
			case "successful Job":
				f.Job.Status.Conditions[0].Type = batchv1.JobComplete
			case "wrong Job UID":
				f.Job.UID = "replacement"
			case "wrong Pod owner":
				f.Pod.OwnerReferences[0].UID = "replacement"
			case "running Pod":
				f.Pod.Status.Phase = corev1.PodRunning
			case "initialization failed":
				f.Pod.Status.InitContainerStatuses[0].State.Terminated.ExitCode = 1
			case "missing initialization":
				f.Pod.Status.InitContainerStatuses = nil
			case "OOM":
				ended.Reason = "OOMKilled"
			case "wrong exit":
				ended.ExitCode = 1
			case "restart":
				f.Pod.Status.ContainerStatuses[0].RestartCount = 1
			case "missing summary":
				ended.Message = ""
			case "missing refusal":
				diagnostic = ""
			case "unavailable receiver":
				diagnostic = strings.ReplaceAll(diagnostic, "403", "503")
			}
			if err := validateFaultApplyResult(original, f.Job, f.Pod, diagnostic); (err == nil) != (mode == "valid") {
				t.Fatalf("accepted=%v: %v", err == nil, err)
			}
		})
	}
}

func TestRetentionFaultSuspensionRevokesDelivery(t *testing.T) {
	f := resulttest.New(t, "migration-apply-admitted-scheduling")
	if err := (resultauthority.Authorizer{Reader: f.Client(t)}).Check(t.Context(), f.Identity); err != nil {
		t.Fatal(err)
	}
	migration := f.Subject.(*ptah.PtahMigration)
	migration.Spec.Suspend = true
	migration.Generation++
	if err := (resultauthority.Authorizer{Reader: f.Client(t)}).Check(t.Context(), f.Identity); !errors.Is(err, resultdelivery.ErrAuthority) {
		t.Fatalf("suspension did not revoke the previous generation's delivery authority: %v", err)
	}
}
