package e2e

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

const (
	// otherTypePolicyFile is a policy that permits an artifact type no fixture
	// publishes, so Ptah itself refuses every artifact it is shown.
	otherTypePolicyFile = "verification-policy-other-type.yaml"
	// otherTypeRequirement is the requirement that policy leaves unmet.
	otherTypeRequirement = "artifact_types"
	// refusalFrameMessage is the whole of what the runner says of a refusal.
	// Ptah's own sentence about the artifact stops at the requirement codes.
	refusalFrameMessage = "artifact does not satisfy the verification policy"
	// refusalJobLag is how long after a claim its Job may be created. The claim
	// fixes the next read and the Job dates it, so the bound gives that much up.
	refusalJobLag = 20 * time.Second
)

// nativelyRefusedSchema is a schema Ptah's own verification stopped: blocked,
// with nothing in flight, and the policy's requirement named where a person
// reads it. Ready carries the same reason, so the refusal does not read as an
// operation that failed.
func nativelyRefusedSchema(schema *ptahv1alpha1.PtahSchema) error {
	status := schema.Status
	switch {
	case !blockedQuiet(schema):
		return fmt.Errorf("it is %s, with an operation or a proof possibly in flight", status.Phase)
	case status.Plan != nil:
		return errors.New("it published a plan")
	case status.Source.Verified:
		return errors.New("its source reads as verified")
	}
	return refusalConditions(status.Conditions, ptahv1alpha1.ConditionArtifactVerified, ptahv1alpha1.ConditionReady)
}

// nativelyRefusedMigration is the same for a migration, which reaches this
// before it reads any history of the database.
func nativelyRefusedMigration(migration *ptahv1alpha1.PtahMigration) error {
	status := migration.Status
	switch {
	case status.Phase != ptahv1alpha1.MigrationPhaseBlocked || status.ActiveOperation != nil:
		return fmt.Errorf("it is %s, with claim %+v", status.Phase, status.ActiveOperation)
	case status.Plan != nil:
		return errors.New("it published a plan")
	case status.History != nil:
		return errors.New("it read the database")
	}
	return refusalConditions(status.Conditions, ptahv1alpha1.ConditionMigrationArtifactVerified, ptahv1alpha1.ConditionMigrationReady)
}

func refusalConditions(conditions []metav1.Condition, verifiedType, readyType string) error {
	verified := meta.FindStatusCondition(conditions, verifiedType)
	if verified == nil || verified.Status != metav1.ConditionFalse || verified.Reason != string(ptahv1alpha1.ReasonPolicyRefused) ||
		!strings.Contains(verified.Message, otherTypeRequirement) {
		return fmt.Errorf("%s is %+v", verifiedType, verified)
	}
	ready := meta.FindStatusCondition(conditions, readyType)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != string(ptahv1alpha1.ReasonPolicyRefused) {
		return fmt.Errorf("%s is %+v", readyType, ready)
	}
	return nil
}

// refusedOnce is a refusal that read its artifact once and waits for its
// interval to read it again: one Resolve, one Verify, nothing of the database,
// and the next read an interval after the Resolve that started the cycle, however
// long the failure interval is. A refusal retried as a failure passes neither.
func refusedOnce(jobs []batchv1.Job, next *metav1.Time, interval time.Duration) error {
	if next == nil {
		return errors.New("the refusal scheduled no next read")
	}
	var resolves, verifies int
	var resolved time.Time
	for index := range jobs {
		job := &jobs[index]
		switch operation := job.Labels[labelOperation]; operation {
		case "resolve":
			resolves++
			resolved = job.CreationTimestamp.Time
		case "verify":
			verifies++
		default:
			return fmt.Errorf("the refusal ran %s Job %s", operation, job.Name)
		}
	}
	if resolves != 1 || verifies != 1 {
		return fmt.Errorf("the refusal ran %d Resolve and %d Verify Jobs, want one of each", resolves, verifies)
	}
	if earliest := resolved.Add(interval - refusalJobLag); next.Time.Before(earliest) {
		return fmt.Errorf("the next read is at %s, before %s, an interval of %s after the Resolve",
			next.Format(time.RFC3339), earliest.Format(time.RFC3339), interval)
	}
	return nil
}

// recoveredFromRefusal is a migration past its refusal, reading the spec it was
// edited to: its artifact verified, a plan waiting for a person, and no condition
// left naming what refused it.
func recoveredFromRefusal(migration *ptahv1alpha1.PtahMigration) error {
	status := migration.Status
	switch {
	case status.ObservedGeneration != migration.Generation:
		return fmt.Errorf("it read generation %d of %d", status.ObservedGeneration, migration.Generation)
	case status.Phase != ptahv1alpha1.MigrationPhaseAwaitingApproval || status.ActiveOperation != nil:
		return fmt.Errorf("it is %s, with claim %+v", status.Phase, status.ActiveOperation)
	case status.Plan == nil:
		return errors.New("it published no plan")
	}
	for _, want := range []struct {
		conditionType string
		status        metav1.ConditionStatus
	}{
		{ptahv1alpha1.ConditionMigrationArtifactVerified, metav1.ConditionTrue},
		{ptahv1alpha1.ConditionMigrationBlocked, metav1.ConditionFalse},
		{ptahv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionTrue},
	} {
		if !conditionStatus(status.Conditions, want.conditionType, want.status) {
			return fmt.Errorf("%s is not %s: %+v", want.conditionType, want.status, status.Conditions)
		}
	}
	for _, condition := range status.Conditions {
		if condition.Reason == string(ptahv1alpha1.ReasonPolicyRefused) {
			return fmt.Errorf("%s still names the refusal: %s", condition.Type, condition.Message)
		}
	}
	return nil
}

// nativeRefusalResult is the Verify Ptah refused: its exit status, the policy by
// digest, the one requirement it found unmet, the refusal code, and the runner's
// fixed message, so no word of what Ptah wrote about the artifact.
func nativeRefusalResult(result runner.Result, digest, policyDigest string) error {
	switch {
	case result.ChildExitCode != 2:
		return fmt.Errorf("the executor exited %d, not 2", result.ChildExitCode)
	case result.ResolvedDigest != digest || result.VerificationPolicyDigest != policyDigest:
		return errors.New("the Verify is not the policy's reading of the digest")
	case !slices.Equal(result.VerificationRequirements, []string{otherTypeRequirement}):
		return fmt.Errorf("the Verify names %v", result.VerificationRequirements)
	case result.Error == nil || result.Error.Code != "verification_refused" || result.Error.Message != refusalFrameMessage:
		return fmt.Errorf("the Verify ended with %+v", result.Error)
	case result.Stdout != "" || result.ObservedArtifactType != "" || result.ResolvedReference != "":
		return errors.New("the frame carries what the executor read")
	case result.MutationStarted || result.Uncertain || result.Truncation != nil:
		return errors.New("the Verify touched something or lost output")
	}
	return nil
}

// nativeRefusalEvent finds the Warning Event that names the unmet requirement.
func nativeRefusalEvent(events []corev1.Event, owner types.UID) error {
	return warningEventSays(events, owner, "ArtifactVerificationRefused", func(message string) error {
		if !strings.Contains(message, otherTypeRequirement) {
			return fmt.Errorf("the Event says %q, which does not name %s", message, otherTypeRequirement)
		}
		return nil
	})
}
