package controller

import (
	"testing"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/operate"
)

// readInSync takes the migration through one read that ends in sync.
func (h *migrationLifecycle) readInSync() {
	h.t.Helper()
	h.claimed(operatorv1alpha1.MigrationOperationResolve)
	h.answer(resolvedTo(testDigest))
	h.claimed(operatorv1alpha1.MigrationOperationVerify)
	h.answer(acceptedBy(testDigest))
	h.claimed(operatorv1alpha1.MigrationOperationHistory)
	h.answer(historyAnswer(h, inSyncReport()))
	h.phase(operatorv1alpha1.MigrationPhaseInSync)
}

// A read request on a migration in sync. A failover hands the resource to a
// manager whose clock is a minute behind, so the read the request starts records a
// reading dated before the one it follows. The wait refresh --wait makes is done at
// that reading: it is the one recorded since the request, whatever its date.
func TestAWaitForAReadTakesAReadingAnotherManagerDatedEarlier(t *testing.T) {
	t.Parallel()
	harness := newMigrationLifecycle(t, migrationFixture())
	clock := movableMigrationClock(harness.reconciler, harness.api)
	harness.readInSync()
	before := harness.migration().Status.History.ObservedAt
	requested := harness.requestRead()

	failOverBehind(clock)
	harness.readInSync()
	read := harness.migration()
	if read.Status.LastReadRequest != requestedAt || read.Status.History == nil || !read.Status.History.ObservedAt.Before(&before) {
		t.Fatalf("lastReadRequest %q, history %#v; want the request answered by a reading dated before %s",
			read.Status.LastReadRequest, read.Status.History, before)
	}

	result, err := waitOn(harness.api, migrationRef(read), readWaitOptions(requested))
	if err != nil {
		t.Fatalf("wait = %v, want the read the request started to end it", err)
	}
	if result.Target != operate.TargetRead || result.Reads != 1 {
		t.Fatalf("result = %+v, want the read done at the first read", result)
	}
}
