package controller

import (
	"reflect"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// Job watches normally drive progress. A read-only claim without a Lease only
// needs a bounded fallback when no event arrives; polling every five seconds
// repeats its live Job and intent reads while the same Pod is still running.
// Claims holding a Lease must keep their existing renewal cadence. Credential
// enrollment and result-read retries use their own shorter timers.
func activeJobPollInterval(holdsLock bool) time.Duration {
	if holdsLock {
		return maxLockContentionPoll
	}
	return 30 * time.Second
}

// Operation progress depends on active/terminal Pods and Job conditions, not
// the readiness count. The Job controller updates readiness separately, often
// immediately before another progress update. Avoid repeating reconciliation
// for that count alone; every other Job change still wakes its owner.
func operationJobEvents() predicate.Predicate {
	return predicate.Funcs{UpdateFunc: func(update event.UpdateEvent) bool {
		before, beforeOK := update.ObjectOld.(*batchv1.Job)
		after, afterOK := update.ObjectNew.(*batchv1.Job)
		if !beforeOK || !afterOK || before == nil || after == nil {
			return true
		}
		before, after = before.DeepCopy(), after.DeepCopy()
		before.ResourceVersion, after.ResourceVersion = "", ""
		before.ManagedFields, after.ManagedFields = nil, nil
		before.Status.Ready, after.Status.Ready = nil, nil
		return !reflect.DeepEqual(before, after)
	}}
}
