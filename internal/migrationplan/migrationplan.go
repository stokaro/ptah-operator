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
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/workload"
)

const (
	// ContractVersion versions migration plan publication separately from the
	// Kubernetes API version. It is the contract of a plan computed from an
	// OCI artifact; SourceContractVersion is the one of a plan computed from a
	// ConfigMap, which binds its source in place of the artifact digest and
	// the verification policy.
	ContractVersion       int32 = fingerprint.CurrentPlanContractVersion
	SourceContractVersion int32 = fingerprint.SourcePlanContractVersion

	// LabelMigration names the PtahMigration a plan belongs to.
	LabelMigration = workload.LabelMigration

	// MaxMigrations is the longest sequence one plan may carry. It matches the
	// CRD's own bound, so a sequence the API would refuse is refused before it
	// is published rather than after.
	MaxMigrations = 256

	// MaxRepeatables is the most repeatable migrations one plan may carry, by
	// the same reasoning.
	MaxRepeatables = operatorv1alpha1.MaxPlannedRepeatables

	namePrefix = "ptah-mplan-"
)

var (
	sha256Pattern             = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	executionBindingIDPattern = regexp.MustCompile(`^v1-[0-9a-f]{32}$`)
	shadowIdentityPattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Binding is everything a migration plan is decided from. A changed input
// produces a different plan rather than a changed one.
//
// The manager that publishes the plan is not an input. Its image, its
// revision and the runner image built beside it are recorded on the plan and
// left out of here, so a manager release that changes only them finds the
// same fingerprint, the same plan name and the same approvals.
type Binding struct {
	MigrationUID       types.UID
	HistoryFingerprint string
	SequenceDigest     string
	ArtifactDigest     string
	// Source is the ConfigMap source a version 2 plan binds in place of the
	// artifact digest and the verification policy.
	Source               *fingerprint.PlanSource
	CoordinationDigest   string
	TargetIdentityDigest string
	// ShadowIdentityDigest is the shadow database a drift check replays
	// into, and empty for a plan whose policy asks for no check.
	ShadowIdentityDigest     string
	PolicyFingerprint        string
	VerificationPolicyUID    types.UID
	VerificationPolicyDigest string
	ExecutionBindingID       string
	ControllerStateVersion   int32
	PtahVersion              string
	ExecutorImage            string
	RunnerProtocolVersion    int32
}

// Fingerprint is the plan's identity. An approval names it, and everything the
// plan was decided from is inside it.
func (b Binding) Fingerprint() (string, error) {
	version := b.ContractVersion()
	for name, value := range map[string]string{
		"migration UID":          string(b.MigrationUID),
		"history fingerprint":    b.HistoryFingerprint,
		"sequence digest":        b.SequenceDigest,
		"coordination digest":    b.CoordinationDigest,
		"target identity digest": b.TargetIdentityDigest,
		"policy fingerprint":     b.PolicyFingerprint,
		"Ptah version":           b.PtahVersion,
		"executor image":         b.ExecutorImage,
	} {
		if strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("%s is required", name)
		}
	}
	if err := fingerprint.ValidatePlanSource(version, b.ArtifactDigest,
		string(b.VerificationPolicyUID), b.VerificationPolicyDigest, b.Source); err != nil {
		return "", err
	}
	if !executionBindingIDPattern.MatchString(b.ExecutionBindingID) {
		return "", errors.New("a valid execution binding ID is required")
	}
	if b.ControllerStateVersion < 1 {
		return "", errors.New("controller state version must be positive")
	}
	if b.RunnerProtocolVersion < 1 {
		return "", errors.New("runner protocol version must be positive")
	}
	if b.ShadowIdentityDigest != "" && !shadowIdentityPattern.MatchString(b.ShadowIdentityDigest) {
		return "", errors.New("the shadow identity digest is not a SHA-256 digest")
	}
	document := map[string]any{
		"contract_version":         version,
		"migration_uid":            string(b.MigrationUID),
		"history_fingerprint":      b.HistoryFingerprint,
		"sequence_digest":          b.SequenceDigest,
		"coordination_digest":      b.CoordinationDigest,
		"target_identity_digest":   b.TargetIdentityDigest,
		"policy_fingerprint":       b.PolicyFingerprint,
		"execution_binding_id":     b.ExecutionBindingID,
		"controller_state_version": b.ControllerStateVersion,
		"ptah_version":             b.PtahVersion,
		"executor_image":           b.ExecutorImage,
		"runner_protocol_version":  b.RunnerProtocolVersion,
	}
	// A version 1 document is exactly the one a manager without ConfigMap
	// sources hashed, so its fingerprint, its plan name and its approvals
	// carry over.
	if b.Source == nil {
		document["artifact_digest"] = b.ArtifactDigest
		document["verification_policy_uid"] = string(b.VerificationPolicyUID)
		document["verification_policy_digest"] = b.VerificationPolicyDigest
	} else {
		document["source"] = *b.Source
	}
	// Only a plan that binds a shadow names one, so a plan whose policy asks
	// for no drift check hashes the document it hashed before the check
	// existed, and keeps its name and its approvals.
	if b.ShadowIdentityDigest != "" {
		document["shadow_identity_digest"] = b.ShadowIdentityDigest
	}
	return fingerprint.DigestCanonicalJSON(document)
}

// ContractVersion is the plan contract this binding is published under.
func (b Binding) ContractVersion() int32 { return fingerprint.PlanContractVersionFor(b.Source) }

// SupportedContractVersion reports whether this manager publishes and reads
// migration plans of version.
func SupportedContractVersion(version int32) bool {
	return version == ContractVersion || version == SourceContractVersion
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
	// What the record says of each repeatable migration is part of the
	// premise: a file that changed, a record another run wrote, or an attempt
	// that failed moves the fingerprint and retires a plan made before it. A
	// history without them keeps the fingerprint it had before they existed,
	// so a plan published then is not retired by an upgrade.
	if len(report.Repeatables) > 0 {
		repeatables := make([]map[string]any, 0, len(report.Repeatables))
		for _, repeatable := range report.Repeatables {
			repeatables = append(repeatables, map[string]any{
				"identity":         repeatable.Identity,
				"checksum":         repeatable.Checksum,
				"applied_checksum": repeatable.AppliedChecksum,
				"orphaned":         repeatable.Orphaned,
				"state":            repeatable.State,
				"no_transaction":   repeatable.NoTransaction,
			})
		}
		document["repeatables"] = repeatables
	}
	return fingerprint.DigestCanonicalJSON(document)
}

// Sequence selects the migrations a plan executes, in the order the history
// document listed them, and the repeatable migrations that run after them, in
// the order Ptah lists them, which is the order it runs them. The order is the
// content: a plan that applies the same migrations in another order is a
// different plan.
//
// A history may have repeatable work and no pending version, or the reverse.
// Only one with neither has nothing to plan.
func Sequence(report dataplane.MigrationStatusReport) (
	[]operatorv1alpha1.PlannedMigration, []operatorv1alpha1.PlannedRepeatableMigration, error,
) {
	planned, err := versionedSequence(report)
	if err != nil {
		return nil, nil, err
	}
	repeatables, err := repeatableSequence(report)
	if err != nil {
		return nil, nil, err
	}
	if len(planned) == 0 && len(repeatables) == 0 {
		return nil, nil, errors.New("the history reports nothing pending")
	}
	return planned, repeatables, nil
}

// versionedSequence is the pending versioned migrations, as Sequence selects
// them, or none.
func versionedSequence(report dataplane.MigrationStatusReport) ([]operatorv1alpha1.PlannedMigration, error) {
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
		return nil, nil
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

// repeatableSequence is the repeatable migrations the next run executes: each
// file the database has no record of, and each whose content changed since it
// last applied, in the order the history lists them. A record whose attempt
// did not finish is never planned, since Ptah runs nothing while one exists,
// and the operator never asks it to retry one.
func repeatableSequence(report dataplane.MigrationStatusReport) ([]operatorv1alpha1.PlannedRepeatableMigration, error) {
	var planned []operatorv1alpha1.PlannedRepeatableMigration
	for _, repeatable := range report.PendingRepeatables() {
		if !dataplane.IsRepeatableIdentity(repeatable.Identity) || !dataplane.IsRepeatableChecksum(repeatable.Checksum) {
			return nil, errors.New("a pending repeatable migration carries no identity a file name can carry, or no h1 digest")
		}
		planned = append(planned, operatorv1alpha1.PlannedRepeatableMigration{
			Identity:        repeatable.Identity,
			Checksum:        repeatable.Checksum,
			AppliedChecksum: repeatable.AppliedChecksum,
		})
	}
	if len(planned) > MaxRepeatables {
		return nil, fmt.Errorf("%d repeatable migrations are pending, over the %d a plan may carry", len(planned), MaxRepeatables)
	}
	if len(planned) > 0 {
		if _, err := workload.EncodeRepeatableSequence(planned); err != nil {
			return nil, err
		}
	}
	return planned, nil
}

// SequenceDigest binds the plan's fingerprint to the exact sequence it carries:
// its versioned migrations and the repeatable migrations after them.
//
// The derivation itself lives in internal/workload, because the Apply Job has
// to carry the same digest and that package is below this one. One rule, two
// callers.
func SequenceDigest(
	planned []operatorv1alpha1.PlannedMigration,
	repeatables []operatorv1alpha1.PlannedRepeatableMigration,
) (string, error) {
	return workload.MigrationSequenceDigest(planned, repeatables)
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
