// Package resultretention protects operation evidence independently of Jobs,
// certificate validity, and the currently elected manager.
package resultretention

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var ErrPinned = errors.New("operation evidence is still retained by its resource")
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// CheckUnpinned uses a direct reader. Clearing ActiveOperation does not settle
// an uncertain Apply: its proof, last-run evidence, or restored unresolved copy
// can still retain the attempt. API failures never authorize deletion.
func CheckUnpinned(ctx context.Context, reader client.Reader, b resultstore.Binding) error {
	if reader == nil {
		return ErrPinned
	}
	if _, err := resultstore.Name(b); err != nil {
		return err
	}
	key := client.ObjectKey{Namespace: b.Namespace, Name: b.Name}
	switch b.Kind {
	case "PtahSchema":
		schema := &api.PtahSchema{}
		if err := reader.Get(ctx, key, schema); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if schema.UID != b.UID {
			return nil
		}
		if (schema.Status.ActiveOperation != nil && schema.Status.ActiveOperation.ID == b.OperationID) ||
			(schema.Status.PendingObservation != nil && schema.Status.PendingObservation.ApplyOperationID == b.OperationID) ||
			(schema.Status.PendingLockRelease != nil && schema.Status.PendingLockRelease.OperationID == b.OperationID) {
			return ErrPinned
		}
	case "PtahMigration":
		migration := &api.PtahMigration{}
		if err := reader.Get(ctx, key, migration); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if migration.UID != b.UID {
			return nil
		}
		if (migration.Status.ActiveOperation != nil && migration.Status.ActiveOperation.ID == b.OperationID) ||
			(migration.Status.UnresolvedRun != nil && migration.Status.UnresolvedRun.OperationID == b.OperationID) ||
			(migration.Status.ResolvedRun != nil && migration.Status.ResolvedRun.OperationID == b.OperationID) ||
			(migration.Status.PendingLockRelease != nil && migration.Status.PendingLockRelease.OperationID == b.OperationID) {
			return ErrPinned
		}
		if run := migration.Status.LastRun; run != nil && ((run.JobName == "" && b.Operation == "migration-apply") || (run.JobName == b.JobName && (run.JobUID == "" || run.JobUID == b.JobUID))) {
			return ErrPinned
		}
		if value, present := migration.Annotations[api.UnresolvedRunAnnotation]; present {
			// Keep malformed or newer-format copies intact. A restore may omit
			// status, and only the migration controller may settle this copy.
			var run api.UnresolvedMigrationRunStatus
			if json.Unmarshal([]byte(value), &run) != nil || !digestPattern.MatchString(run.OperationID) || run.PlanRef.Name == "" || run.PlanRef.UID == "" || run.RecordedAt.IsZero() || len(run.JobName) > 253 ||
				(run.TargetIdentityDigest != "" && !digestPattern.MatchString(run.TargetIdentityDigest)) ||
				(run.Outcome != api.MigrationRunOutcomePartial && run.Outcome != api.MigrationRunOutcomeUnknown) {
				return ErrPinned
			}
			canonical, err := json.Marshal(run)
			if err != nil || !bytes.Equal(canonical, []byte(value)) || run.OperationID == b.OperationID {
				return ErrPinned
			}
		}
	default:
		return ErrPinned
	}
	return nil
}
