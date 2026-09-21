package managercache

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// What the watch needs survives, and what it does not need is gone.
//
// The map function behind the ConfigMap watch looks up the resources bound to a
// policy by the ConfigMap's namespace and name, and wakes them. Nothing about
// the decision it makes reads a byte of the object's contents.
func TestStripConfigMapPayloadKeepsOnlyWhatNamesIt(t *testing.T) {
	t.Parallel()

	immutable := true
	// A plan chunk, which is the largest thing this watch would otherwise hold.
	chunk := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: "orders-plan-chunk-0", UID: types.UID("chunk-uid"),
			ResourceVersion: "42",
			Labels:          map[string]string{"operator.ptah.run/plan": "orders"},
			ManagedFields:   []metav1.ManagedFieldsEntry{{Manager: "ptah-operator", Operation: metav1.ManagedFieldsOperationApply}},
		},
		Immutable:  &immutable,
		Data:       map[string]string{"notes": strings.Repeat("a", 1<<10)},
		BinaryData: map[string][]byte{"chunk-0": []byte(strings.Repeat("CREATE TABLE orders ();", 1<<10))},
	}

	stripped, err := StripConfigMapPayload(chunk)
	if err != nil {
		t.Fatalf("StripConfigMapPayload() error = %v", err)
	}
	cached, ok := stripped.(*corev1.ConfigMap)
	if !ok {
		t.Fatalf("the cache was handed a %T instead of a ConfigMap", stripped)
	}
	if cached.Data != nil || cached.BinaryData != nil {
		t.Fatalf("the cache keeps the payload: %d data keys, %d binary keys",
			len(cached.Data), len(cached.BinaryData))
	}
	if cached.ManagedFields != nil {
		t.Fatalf("the cache keeps managed fields describing a payload it no longer has: %#v", cached.ManagedFields)
	}
	if cached.Namespace != "team-a" || cached.Name != "orders-plan-chunk-0" {
		t.Fatalf("the cache lost the name the watch wakes resources by: %s/%s", cached.Namespace, cached.Name)
	}
	if cached.UID != "chunk-uid" || cached.ResourceVersion != "42" {
		t.Fatalf("the cache lost the object's identity: uid=%q resourceVersion=%q", cached.UID, cached.ResourceVersion)
	}
	if cached.Labels["operator.ptah.run/plan"] != "orders" {
		t.Fatalf("the cache lost the labels a predicate may select on: %#v", cached.Labels)
	}
}

// A transform sees everything the informer stores, not only the kind it was
// written for, and the tombstone of a deleted object is one of those things.
// Refusing what it does not recognize would drop a delete, which is how a
// verification policy that was removed goes on waking nothing.
func TestStripConfigMapPayloadLeavesEverythingElseAlone(t *testing.T) {
	t.Parallel()

	for _, object := range []any{
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "database"},
			Data:       map[string][]byte{"url": []byte("postgres://user:pass@host/db")},
		},
		"a tombstone the informer wrapped in something this does not know",
		nil,
	} {
		passed, err := StripConfigMapPayload(object)
		if err != nil {
			t.Fatalf("StripConfigMapPayload(%T) error = %v", object, err)
		}
		if passed == nil && object != nil {
			t.Fatalf("StripConfigMapPayload(%T) dropped the object", object)
		}
	}

	secret := &corev1.Secret{Data: map[string][]byte{"url": []byte("postgres://user:pass@host/db")}}
	if _, err := StripConfigMapPayload(secret); err != nil {
		t.Fatal(err)
	}
	if len(secret.Data) != 1 {
		t.Fatal("a Secret was emptied by a transform written for ConfigMaps")
	}
}

// The manager has to be the caller, or none of the above is true of anything.
func TestTheManagerCacheStripsConfigMaps(t *testing.T) {
	t.Parallel()

	byObject := Options().ByObject
	if len(byObject) == 0 {
		t.Fatal("the cache names no object to transform")
	}
	var transform func(any) (any, error)
	for object, options := range byObject {
		if _, ok := object.(*corev1.ConfigMap); ok {
			transform = options.Transform
		}
	}
	if transform == nil {
		t.Fatal("the cache stores ConfigMaps exactly as they arrive")
	}
	stripped, err := transform(&corev1.ConfigMap{BinaryData: map[string][]byte{"chunk-0": []byte("CREATE TABLE orders ();")}})
	if err != nil {
		t.Fatal(err)
	}
	if cached := stripped.(*corev1.ConfigMap); cached.BinaryData != nil {
		t.Fatal("the cache's own transform keeps the payload")
	}
}

// And a read must not be served from what the transform emptied.
func TestConfigMapReadsDoNotUseTheCache(t *testing.T) {
	t.Parallel()

	options := ClientOptions()
	if options.Cache == nil {
		t.Fatal("the client caches every read, including the ConfigMaps the transform emptied")
	}
	for _, object := range options.Cache.DisableFor {
		if _, ok := object.(*corev1.ConfigMap); ok {
			return
		}
	}
	t.Fatalf("ConfigMap reads are still served from the cache: %#v", options.Cache.DisableFor)
}
