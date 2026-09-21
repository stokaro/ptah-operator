// Package managercache says what the manager keeps in memory from the
// resources it watches.
//
// A watch is a cache, and a cache holds whole objects. The schema controller
// watches ConfigMaps cluster-wide so a changed verification policy wakes the
// resources bound to it, and the only thing that wakes them is the ConfigMap's
// namespace and name. Everything else the watch would hold is cost: unrelated
// application configuration, and this operator's own plan chunks, which carry
// up to eight mebibytes of SQL each.
package managercache

import (
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Options is the cache the manager runs with.
func Options() cache.Options {
	return cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&corev1.ConfigMap{}: {Transform: StripConfigMapPayload},
		},
	}
}

// ClientOptions keeps ConfigMap reads off that cache.
//
// The transform is what bounds the memory; this is what keeps the bound
// honest. Every ConfigMap this operator reads -- a verification policy, a plan
// chunk -- is read through the API reader today, because each one is a
// decision that a cache may not be current enough to make. Routing the cached
// client's reads to the API server as well means a read added later cannot
// quietly start seeing objects the transform emptied, and cannot quietly build
// a second informer that holds what the transform dropped.
func ClientOptions() client.Options {
	return client.Options{
		Cache: &client.CacheOptions{
			DisableFor: []client.Object{&corev1.ConfigMap{}},
		},
	}
}

// StripConfigMapPayload drops what a ConfigMap carries and keeps what names it.
//
// It is the cache's transform, so it runs on every object the watch delivers
// before it is stored, and on nothing the API reader returns. Managed fields go
// with the payload: they describe the fields that are no longer there, and on a
// plan chunk they are proportional to what was dropped.
//
// Anything that is not a ConfigMap is returned untouched. A transform sees the
// deletion tombstones the informer emits as well as live objects, and turning
// one of those into an error would drop a delete event, which is how a policy
// that was removed goes on waking nothing.
func StripConfigMapPayload(object any) (any, error) {
	configMap, ok := object.(*corev1.ConfigMap)
	if !ok {
		return object, nil
	}
	configMap.Data = nil
	configMap.BinaryData = nil
	configMap.ManagedFields = nil
	return configMap, nil
}
