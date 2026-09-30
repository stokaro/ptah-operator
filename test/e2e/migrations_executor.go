package e2e

import (
	"errors"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/equality"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/migrationplan"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
)

func executorVariantReference(original, output string) (string, error) {
	repository, oldDigest, ok := strings.Cut(original, "@")
	updated, marker := strings.CutPrefix(strings.TrimSpace(output), "Executor: ")
	nextRepository, nextDigest, next := strings.Cut(updated, "@")
	if !ok || !marker || !next || repository == "" || repository != nextRepository ||
		!sha256Pattern.MatchString(oldDigest) || !sha256Pattern.MatchString(nextDigest) || nextDigest == oldDigest {
		return "", errors.New("executor fixture did not return a different digest in the original repository")
	}
	return updated, nil
}

func changedMigrationExecutorDecision(before, current *ptahv1alpha1.PtahMigration, old, fresh *ptahv1alpha1.PtahMigrationPlan,
	original, replacement string,
) error {
	if before == nil || current == nil || old == nil || fresh == nil || before.UID == "" || before.UID != current.UID ||
		before.Name != current.Name || before.Namespace != current.Namespace || !equality.Semantic.DeepEqual(before.Spec, current.Spec) ||
		before.Status.ObservedGeneration != before.Generation || before.Status.UnresolvedRun != nil ||
		before.Status.Phase != ptahv1alpha1.MigrationPhaseAwaitingApproval || before.Status.ActiveOperation != nil || before.Status.Plan == nil ||
		before.Status.Plan.UID != old.UID || before.Status.Plan.Name != old.Name || old.UID == "" ||
		!changedMigrationApprovalRefused(current, old.UID, before.Generation, false) ||
		current.Status.Plan.UID != fresh.UID || current.Status.Plan.Name != fresh.Name {
		return errors.New("executor transition changed the migration or did not reach its fresh approval gate")
	}
	previous, next := before.Status.ExecutionBinding, current.Status.ExecutionBinding
	if previous == nil || next == nil || !executionEpoch.MatchString(previous.Epoch) || !executionEpoch.MatchString(next.Epoch) ||
		previous.Epoch == next.Epoch || previous.ExecutorImage != original || next.ExecutorImage != replacement ||
		original == replacement || !digestSuffix.MatchString(original) || !digestSuffix.MatchString(replacement) ||
		previous.PtahVersion != next.PtahVersion || previous.RunnerProtocolVersion != next.RunnerProtocolVersion || previous.ControllerStateVersion != next.ControllerStateVersion {
		return errors.New("migration executor transition did not rotate exactly the image-bound epoch")
	}
	if old.Spec.MigrationRef.Name != before.Name || old.Spec.MigrationRef.UID != before.UID ||
		old.Spec.ExecutionBindingID != previous.Epoch || fresh.Spec.ExecutionBindingID != next.Epoch ||
		old.Spec.ExecutorImage != original || fresh.Spec.ExecutorImage != replacement ||
		old.Spec.Fingerprint == "" || fresh.Spec.Fingerprint == "" || old.Spec.Fingerprint == fresh.Spec.Fingerprint ||
		old.Spec.HistoryFingerprint == "" || planVersionList(old) != "1 2 3" {
		return errors.New("migration plans do not bind the original and replacement execution identities")
	}
	// Apart from identity and publication time, the entire selected sequence
	// and every target, source, policy and execution contract must be unchanged.
	normalized := fresh.DeepCopy()
	normalized.Spec.ExecutorImage = old.Spec.ExecutorImage
	normalized.Spec.ExecutionBindingID = old.Spec.ExecutionBindingID
	normalized.Spec.Fingerprint = old.Spec.Fingerprint
	normalized.Spec.CreatedAt = old.Spec.CreatedAt
	if !equality.Semantic.DeepEqual(old.Spec, normalized.Spec) {
		return errors.New("executor transition changed another migration plan input")
	}
	return nil
}

func migrationHistoryMatchesExecutorPlan(result runner.Result, plan *ptahv1alpha1.PtahMigrationPlan) error {
	if plan == nil || result.Operation != runner.OperationMigrationHistory || result.OperationID == "" ||
		result.ProtocolVersion != int(plan.Spec.RunnerProtocolVersion) || result.ChildExitCode != 0 || result.Error != nil || result.Truncation != nil ||
		result.MigrationHistory == nil || result.TargetIdentityDigest != plan.Spec.TargetIdentityDigest || result.CoordinationDigest != plan.Spec.CoordinationDigest {
		return errors.New("executor control has no complete successful History result for the exact target")
	}
	fingerprint, err := migrationplan.HistoryFingerprint(*result.MigrationHistory)
	if err != nil || fingerprint != plan.Spec.HistoryFingerprint {
		return errors.New("executor History control did not produce the plan's history")
	}
	sequence, err := migrationplan.Sequence(*result.MigrationHistory)
	if err != nil || !equality.Semantic.DeepEqual(sequence, plan.Spec.Migrations) {
		return errors.New("executor History control did not produce the plan's exact sequence")
	}
	return nil
}

func migrationExecutorSQLControls(clients map[string]operationSQLClient, counts map[string]int, initial, current operationSQLClient) error {
	if initial.jobUID == current.jobUID || initial.podUID == current.podUID || initial.resourceUID == "" || initial.resourceUID != current.resourceUID {
		return errors.New("executor SQL controls need distinct Jobs and Pods for the same migration")
	}
	for _, required := range []operationSQLClient{initial, current} {
		if required.operation != "history" || required.jobUID == "" || required.podUID == "" {
			return errors.New("executor SQL control is not an identified History")
		}
		matches := 0
		for host, actor := range clients {
			if actor == required && counts[host] > 0 {
				matches++
			}
		}
		if matches != 1 {
			return errors.New("executor audit did not receive SQL from each exact History Job and Pod")
		}
	}
	return nil
}

// Read the admitted plan's inputs back from the actual Apply Job. Consuming an
// approval and reporting Applied cannot prove that this Job carried its work.
func migrationExecutorApplyInputs(job *batchv1.Job, plan *ptahv1alpha1.PtahMigrationPlan) error {
	if plan == nil || !jobUsesExecutor(job, plan.Spec.ExecutorImage) || job.Labels[labelOperation] != "apply" ||
		job.Annotations[annotationBindingID] != plan.Spec.ExecutionBindingID {
		return errors.New("fresh migration Apply has no exact executor-bound plan")
	}
	sequence, err := workload.EncodeMigrationSequence(plan.Spec.Migrations)
	if err != nil {
		return err
	}
	digest, err := workload.MigrationSequenceDigest(plan.Spec.Migrations)
	if err != nil {
		return err
	}
	for key, want := range map[string]string{
		runner.EnvExpectedTargetIdentityDigest:        plan.Spec.TargetIdentityDigest,
		runner.EnvExpectedCoordinationDigest:          plan.Spec.CoordinationDigest,
		runner.EnvExpectedMigrationHistoryFingerprint: plan.Spec.HistoryFingerprint,
		runner.EnvExpectedMigrationSequence:           sequence,
		runner.EnvExpectedMigrationSequenceDigest:     digest,
	} {
		count := 0
		for _, value := range job.Spec.Template.Spec.Containers[0].Env {
			if value.Name == key {
				count++
				if value.ValueFrom != nil || value.Value != want || want == "" {
					return errors.New("fresh migration Apply changed an approved plan input")
				}
			}
		}
		if count != 1 {
			return errors.New("fresh migration Apply omitted or duplicated an approved plan input")
		}
	}
	return nil
}
