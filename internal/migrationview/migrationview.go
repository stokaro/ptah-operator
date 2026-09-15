// Package migrationview reads what the operator stored about one migration.
//
// It is the read side of the migration path: what the database's own history
// said when it was last read, which sequence the operator would run next, and
// what the last run did. It connects to no database and reads no Secret.
package migrationview

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// ErrMigrationNotFound is returned when the named PtahMigration is not there.
// It is separate from a read failure: "there is nothing by that name" is an
// answer.
var ErrMigrationNotFound = errors.New("no PtahMigration by that name")

// View is everything this command prints, resolved before anything is written.
type View struct {
	Namespace  string          `json:"namespace"`
	Migration  string          `json:"migration"`
	Phase      string          `json:"phase,omitempty"`
	Suspended  bool            `json:"suspended,omitempty"`
	Artifact   *ArtifactView   `json:"artifact,omitempty"`
	History    *HistoryView    `json:"history,omitempty"`
	Plan       *PlanView       `json:"plan,omitempty"`
	LastRun    *RunView        `json:"lastRun,omitempty"`
	Conditions []ConditionView `json:"conditions,omitempty"`
}

// ArtifactView is the resolved, credential-free source binding.
type ArtifactView struct {
	Reference string `json:"reference"`
	Digest    string `json:"digest"`
}

// HistoryView is what the revision table said when it was last read.
type HistoryView struct {
	ObservedAt        metav1.Time `json:"observedAt"`
	CurrentVersion    int64       `json:"currentVersion"`
	CheckpointVersion int64       `json:"checkpointVersion,omitempty"`
	AppliedCount      int32       `json:"appliedCount"`
	PendingCount      int32       `json:"pendingCount"`
	Dirty             bool        `json:"dirty,omitempty"`
	ModifiedVersions  []int64     `json:"modifiedVersions,omitempty"`
	Fingerprint       string      `json:"fingerprint,omitempty"`
}

// PlanView is the sequence the operator would run next. A migration plan holds
// versions and checksums rather than SQL: the statements live in the artifact,
// and printing them here would mean fetching it.
type PlanView struct {
	Name           string                 `json:"name"`
	Fingerprint    string                 `json:"fingerprint"`
	CurrentVersion int64                  `json:"currentVersion"`
	CreatedAt      metav1.Time            `json:"createdAt"`
	Migrations     []PlannedMigrationView `json:"migrations"`
}

// PlannedMigrationView is one migration of the sequence.
type PlannedMigrationView struct {
	Version         int64  `json:"version"`
	VersionKey      string `json:"versionKey,omitempty"`
	Description     string `json:"description,omitempty"`
	Checksum        string `json:"checksum"`
	Checkpoint      bool   `json:"checkpoint,omitempty"`
	TransactionMode string `json:"transactionMode,omitempty"`
}

// RunView is what the database accounted for after the last execution.
type RunView struct {
	Outcome         string       `json:"outcome"`
	StartedAt       metav1.Time  `json:"startedAt"`
	FinishedAt      *metav1.Time `json:"finishedAt,omitempty"`
	AppliedVersions []int64      `json:"appliedVersions,omitempty"`
	Message         string       `json:"message,omitempty"`
}

// ConditionView is one published condition, reduced to what a reader acts on.
type ConditionView struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// Load reads one migration and the plan it names.
//
// A plan the migration no longer names is not shown: the status reference is
// what makes a stored plan the current one, and a plan read by name alone
// could be one the operator already discarded.
func Load(
	ctx context.Context,
	reader client.Reader,
	namespace, name string,
) (View, error) {
	if reader == nil {
		return View{}, errors.New("a cluster reader is required")
	}
	if namespace == "" || name == "" {
		return View{}, errors.New("a namespace and a migration name are required")
	}
	migration := &operatorv1alpha1.PtahMigration{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, migration); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return View{}, fmt.Errorf("%w: %s/%s", ErrMigrationNotFound, namespace, name)
		}
		return View{}, fmt.Errorf("read the migration: %w", err)
	}

	view := View{
		Namespace: migration.Namespace,
		Migration: migration.Name,
		Phase:     string(migration.Status.Phase),
		Suspended: migration.Spec.Suspend,
	}
	if artifact := migration.Status.Artifact; artifact != nil {
		view.Artifact = &ArtifactView{Reference: artifact.ResolvedReference, Digest: artifact.Digest}
	}
	if history := migration.Status.History; history != nil {
		view.History = &HistoryView{
			ObservedAt:        history.ObservedAt,
			CurrentVersion:    history.CurrentVersion,
			CheckpointVersion: history.CheckpointVersion,
			AppliedCount:      history.AppliedCount,
			PendingCount:      history.PendingCount,
			Dirty:             history.Dirty,
			ModifiedVersions:  append([]int64(nil), history.ModifiedVersions...),
			Fingerprint:       history.Fingerprint,
		}
	}
	if run := migration.Status.LastRun; run != nil {
		view.LastRun = &RunView{
			Outcome:         string(run.Outcome),
			StartedAt:       run.StartedAt,
			FinishedAt:      run.FinishedAt,
			AppliedVersions: append([]int64(nil), run.AppliedVersions...),
			Message:         run.Message,
		}
	}
	for _, condition := range migration.Status.Conditions {
		view.Conditions = append(view.Conditions, ConditionView{
			Type: condition.Type, Status: string(condition.Status),
			Reason: condition.Reason, Message: condition.Message,
		})
	}
	if migration.Status.Plan == nil {
		return view, nil
	}
	plan := &operatorv1alpha1.PtahMigrationPlan{}
	key := client.ObjectKey{Namespace: migration.Namespace, Name: migration.Status.Plan.Name}
	if err := reader.Get(ctx, key, plan); err != nil {
		if client.IgnoreNotFound(err) == nil {
			// The reference outlived the object. Say so by omission rather than
			// by failing: everything else read here is still true.
			return view, nil
		}
		return View{}, fmt.Errorf("read the migration plan: %w", err)
	}
	if plan.UID != migration.Status.Plan.UID {
		return view, nil
	}
	view.Plan = &PlanView{
		Name:           plan.Name,
		Fingerprint:    plan.Spec.Fingerprint,
		CurrentVersion: plan.Spec.CurrentVersion,
		CreatedAt:      plan.Spec.CreatedAt,
	}
	for _, planned := range plan.Spec.Migrations {
		view.Plan.Migrations = append(view.Plan.Migrations, PlannedMigrationView{
			Version:         planned.Version,
			VersionKey:      planned.VersionKey,
			Description:     planned.Description,
			Checksum:        planned.Checksum,
			Checkpoint:      planned.Checkpoint,
			TransactionMode: planned.TransactionMode,
		})
	}
	return view, nil
}
