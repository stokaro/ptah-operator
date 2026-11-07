package e2e

import (
	"errors"
	"fmt"
	"slices"
	"strings"

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

// nativeRefusalResult is the Verify Ptah refused: its exit status, the policy by
// digest, the one requirement it found unmet, the refusal code, and no word of
// what Ptah wrote about the artifact.
func nativeRefusalResult(result runner.Result, digest, policyDigest string) error {
	switch {
	case result.ChildExitCode != 2:
		return fmt.Errorf("the executor exited %d, not 2", result.ChildExitCode)
	case result.ResolvedDigest != digest || result.VerificationPolicyDigest != policyDigest:
		return errors.New("the Verify is not the policy's reading of the digest")
	case !slices.Equal(result.VerificationRequirements, []string{otherTypeRequirement}):
		return fmt.Errorf("the Verify names %v", result.VerificationRequirements)
	case result.Error == nil || result.Error.Code != "verification_refused":
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
