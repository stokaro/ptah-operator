// Package migrationplan publishes and re-derives the immutable manifest of one
// planned migration sequence.
//
// The controller writes the plan and admission re-derives it from the same
// inputs, so the derivation lives here rather than in either of them: a plan
// nobody can reproduce is a plan nothing can check.
package migrationplan

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/controllerstate"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/workload"
)

const (
	// ContractVersion versions migration plan publication separately from the
	// Kubernetes API version.
	ContractVersion int32 = 1

	// LabelMigration names the PtahMigration a plan belongs to.
	LabelMigration = workload.LabelMigration

	// MaxMigrations is the longest sequence one plan may carry. It matches the
	// CRD's own bound, so a sequence the API would refuse is refused before it
	// is published rather than after.
	MaxMigrations = 256

	namePrefix = "ptah-mplan-"
)

var (
	sha256Pattern             = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	executionBindingIDPattern = regexp.MustCompile(`^v1-[0-9a-f]{32}$`)
	imageDigestPattern        = regexp.MustCompile(`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`)
)

// Binding is everything a migration plan is decided from. A changed input
// produces a different plan rather than a changed one.
type Binding struct {
	MigrationUID             types.UID
	HistoryFingerprint       string
	SequenceDigest           string
	ArtifactDigest           string
	CoordinationDigest       string
	TargetIdentityDigest     string
	PolicyFingerprint        string
	VerificationPolicyUID    types.UID
	VerificationPolicyDigest string
	ExecutionBindingID       string
	ControllerImage          string
	ControllerRevision       string
	ControllerStateVersion   int32
	PtahVersion              string
	ExecutorImage            string
	RunnerImage              string
	RunnerProtocolVersion    int32
}

// Fingerprint is the plan's identity. An approval names it, and everything the
// plan was decided from is inside it.
func (b Binding) Fingerprint() (string, error) {
	for name, value := range map[string]string{
		"migration UID":              string(b.MigrationUID),
		"history fingerprint":        b.HistoryFingerprint,
		"sequence digest":            b.SequenceDigest,
		"artifact digest":            b.ArtifactDigest,
		"coordination digest":        b.CoordinationDigest,
		"target identity digest":     b.TargetIdentityDigest,
		"policy fingerprint":         b.PolicyFingerprint,
		"verification policy UID":    string(b.VerificationPolicyUID),
		"verification policy digest": b.VerificationPolicyDigest,
		"Ptah version":               b.PtahVersion,
		"executor image":             b.ExecutorImage,
		"runner image":               b.RunnerImage,
	} {
		if strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("%s is required", name)
		}
	}
	if !executionBindingIDPattern.MatchString(b.ExecutionBindingID) {
		return "", errors.New("a valid execution binding ID is required")
	}
	if !imageDigestPattern.MatchString(b.ControllerImage) {
		return "", errors.New("controller image must be pinned by a lowercase SHA-256 digest")
	}
	if err := controllerstate.ValidateRevision(b.ControllerRevision); err != nil {
		return "", fmt.Errorf("invalid controller revision: %w", err)
	}
	if b.ControllerStateVersion < 1 {
		return "", errors.New("controller state version must be positive")
	}
	if b.RunnerProtocolVersion < 1 {
		return "", errors.New("runner protocol version must be positive")
	}
	return fingerprint.DigestCanonicalJSON(map[string]any{
		"contract_version":           ContractVersion,
		"migration_uid":              string(b.MigrationUID),
		"history_fingerprint":        b.HistoryFingerprint,
		"sequence_digest":            b.SequenceDigest,
		"artifact_digest":            b.ArtifactDigest,
		"coordination_digest":        b.CoordinationDigest,
		"target_identity_digest":     b.TargetIdentityDigest,
		"policy_fingerprint":         b.PolicyFingerprint,
		"verification_policy_uid":    string(b.VerificationPolicyUID),
		"verification_policy_digest": b.VerificationPolicyDigest,
		"execution_binding_id":       b.ExecutionBindingID,
		"controller_image":           b.ControllerImage,
		"controller_revision":        b.ControllerRevision,
		"controller_state_version":   b.ControllerStateVersion,
		"ptah_version":               b.PtahVersion,
		"executor_image":             b.ExecutorImage,
		"runner_image":               b.RunnerImage,
		"runner_protocol_version":    b.RunnerProtocolVersion,
	})
}

// Name is the plan's deterministic object name. Two publications of the same
// plan are the same object, so a controller that restarted mid-publication
// cannot leave a second copy of one decision.
func Name(planFingerprint string) (string, error) {
	if !sha256Pattern.MatchString(planFingerprint) {
		return "", errors.New("plan fingerprint is not a lowercase SHA-256 digest")
	}
	return namePrefix + planFingerprint[len("sha256:"):len("sha256:")+24], nil
}

// HistoryFingerprint is the exact reading of the revision table a plan was
// computed against. It covers every migration the report accounted for, so a
// history that moved -- by anyone, for any reason -- produces a different
// fingerprint and invalidates the plan.
func HistoryFingerprint(report dataplane.MigrationStatusReport) (string, error) {
	if report.ContractVersion != dataplane.SupportedMigrationStatusContract {
		return "", fmt.Errorf("unsupported migration status contract version %d", report.ContractVersion)
	}
	records := make([]map[string]any, 0, len(report.Migrations))
	for _, record := range report.Migrations {
		records = append(records, map[string]any{
			"version":          record.Version,
			"version_key":      record.VersionKey,
			"checksum":         record.Checksum,
			"applied_checksum": record.AppliedChecksum,
			"checkpoint":       record.Checkpoint,
			"state":            record.State,
		})
	}
	document := map[string]any{
		"contract_version":   report.ContractVersion,
		"current_version":    report.CurrentVersion,
		"checkpoint_version": report.CheckpointVersion,
		"total_migrations":   report.TotalMigrations,
		"migrations":         records,
		"dirty":              report.DirtyRevision != nil,
	}
	if report.DirtyRevision != nil {
		document["dirty_revision"] = map[string]any{
			"version": report.DirtyRevision.Version,
			"applied": report.DirtyRevision.Applied,
			"total":   report.DirtyRevision.Total,
		}
	}
	return fingerprint.DigestCanonicalJSON(document)
}

// Sequence selects the migrations a plan executes, in the order the history
// document listed them. The order is the content: a plan that applies the same
// migrations in another order is a different plan.
func Sequence(report dataplane.MigrationStatusReport) ([]operatorv1alpha1.PlannedMigration, error) {
	planned := make([]operatorv1alpha1.PlannedMigration, 0, len(report.Migrations))
	for _, record := range report.Migrations {
		if record.State != dataplane.MigrationStatePending && record.State != dataplane.MigrationStateOutOfOrder {
			continue
		}
		if record.Version < 1 {
			return nil, fmt.Errorf("migration version %d is not positive", record.Version)
		}
		if strings.TrimSpace(record.Checksum) == "" {
			return nil, fmt.Errorf("migration %d carries no checksum", record.Version)
		}
		transactionMode := record.TransactionMode
		if transactionMode != "" && transactionMode != "file" && transactionMode != "none" {
			return nil, fmt.Errorf("migration %d declares an unknown transaction mode %q", record.Version, transactionMode)
		}
		planned = append(planned, operatorv1alpha1.PlannedMigration{
			Version:         record.Version,
			VersionKey:      record.VersionKey,
			Description:     record.Description,
			Checksum:        record.Checksum,
			Checkpoint:      record.Checkpoint,
			TransactionMode: transactionMode,
		})
	}
	if len(planned) == 0 {
		return nil, errors.New("the history reports nothing pending")
	}
	if len(planned) > MaxMigrations {
		return nil, fmt.Errorf("the pending sequence is %d migrations, over the %d a plan may carry", len(planned), MaxMigrations)
	}
	// The Apply Job carries the sequence to the runner, which hands it to
	// Ptah. A sequence it cannot carry is refused here, before anyone is asked
	// to approve a plan that no Job could execute.
	if _, err := workload.EncodeMigrationSequence(planned); err != nil {
		return nil, err
	}
	return planned, nil
}

// SequenceDigest binds the plan's fingerprint to the exact sequence it carries.
//
// The derivation itself lives in internal/workload, because the Apply Job has
// to carry the same digest and that package is below this one. One rule, two
// callers.
func SequenceDigest(planned []operatorv1alpha1.PlannedMigration) (string, error) {
	return workload.MigrationSequenceDigest(planned)
}

// Desired builds the object the controller publishes. Its metadata is exactly
// what admission expects, because admission builds the same object.
func Desired(
	migration *operatorv1alpha1.PtahMigration,
	spec operatorv1alpha1.PtahMigrationPlanSpec,
) (*operatorv1alpha1.PtahMigrationPlan, error) {
	if migration == nil || migration.Name == "" || migration.UID == "" {
		return nil, errors.New("migration name and UID are required")
	}
	name, err := Name(spec.Fingerprint)
	if err != nil {
		return nil, err
	}
	controller := true
	blockDeletion := true
	return &operatorv1alpha1.PtahMigrationPlan{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: migration.Namespace,
			Name:      name,
			Labels:    map[string]string{LabelMigration: migration.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         operatorv1alpha1.GroupVersion.String(),
				Kind:               "PtahMigration",
				Name:               migration.Name,
				UID:                migration.UID,
				Controller:         &controller,
				BlockOwnerDeletion: &blockDeletion,
			}},
		},
		Spec: spec,
	}, nil
}
