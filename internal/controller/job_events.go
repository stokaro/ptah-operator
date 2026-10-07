package controller

import (
	"reflect"

	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

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
