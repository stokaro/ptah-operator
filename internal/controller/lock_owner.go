package controller

import (
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
)

// The two families owe a database realm back through the same obligation and
// keep it in a field of their own. These adapters are the whole of the
// difference: everything the obligation decides lives in
// internal/mutationlifecycle, so a rule changed there changes for both.

type schemaLockOwner struct{ schema *operatorv1alpha1.PtahSchema }

func (o schemaLockOwner) UID() types.UID { return o.schema.UID }

func (o schemaLockOwner) OwedLockRelease() *operatorv1alpha1.TargetLockReleaseStatus {
	return o.schema.Status.PendingLockRelease
}

func (o schemaLockOwner) SetOwedLockRelease(release *operatorv1alpha1.TargetLockReleaseStatus) {
	o.schema.Status.PendingLockRelease = release
}

type migrationLockOwner struct {
	migration *operatorv1alpha1.PtahMigration
}

func (o migrationLockOwner) UID() types.UID { return o.migration.UID }

func (o migrationLockOwner) OwedLockRelease() *operatorv1alpha1.TargetLockReleaseStatus {
	return o.migration.Status.PendingLockRelease
}

func (o migrationLockOwner) SetOwedLockRelease(release *operatorv1alpha1.TargetLockReleaseStatus) {
	o.migration.Status.PendingLockRelease = release
}

var (
	_ mutationlifecycle.LockOwner = schemaLockOwner{}
	_ mutationlifecycle.LockOwner = migrationLockOwner{}
)
