package controller

import (
	"context"
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// These are controller lookup measurements, not cluster throughput. There is
// no API server, no informer feeding the store and no reconcile around the
// call; what they separate is a lookup whose cost follows the realm from one
// whose cost follows the cluster.
//
// Two shapes, because the index changes them differently. Distinct realms is
// what the scan punished hardest: every resource examined to find the one that
// matches. A realm every resource shares is the case an index cannot make
// smaller, and it is measured so a later change cannot trade one for the other
// without the numbers saying so.

func realmBenchObjects(count int, shared bool) []client.Object {
	objects := make([]client.Object, 0, count)
	for index := range count {
		key := "team/shared"
		if !shared {
			key = fmt.Sprintf("team/realm-%d", index)
		}
		objects = append(objects, realmSchemaFixture(fmt.Sprintf("schema-%d", index), key, shared))
	}
	return objects
}

func realmBenchKey(shared bool) string {
	if shared {
		return "team/shared"
	}
	return "team/realm-0"
}

// The store a cached client reads through. controller-runtime's MatchingFields
// resolves to ByIndex on exactly this, so this is the mechanism the census now
// uses in a manager, measured without one.
func realmBenchIndexer(objects []client.Object) toolscache.Indexer {
	indexer := toolscache.NewIndexer(toolscache.MetaNamespaceKeyFunc, toolscache.Indexers{
		RealmDigestIndex: func(stored any) ([]string, error) {
			schema, ok := stored.(*operatorv1alpha1.PtahSchema)
			if !ok {
				return nil, nil
			}
			return realmDigestIndexValue(schema.Spec.Target), nil
		},
	})
	for _, object := range objects {
		if err := indexer.Add(object); err != nil {
			panic(err)
		}
	}
	return indexer
}

func benchmarkRealmMembership(b *testing.B, count int, shared bool, indexed bool) {
	b.Helper()

	objects := realmBenchObjects(count, shared)
	indexer := realmBenchIndexer(objects)
	digest := realmBenchDigest(b, shared)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		var members int
		if indexed {
			found, err := indexer.ByIndex(RealmDigestIndex, digest)
			if err != nil {
				b.Fatal(err)
			}
			members = len(found)
		} else {
			// What the census did before: every resource of the kind examined
			// for one realm's membership.
			for _, stored := range indexer.List() {
				schema, ok := stored.(*operatorv1alpha1.PtahSchema)
				if !ok {
					continue
				}
				if claimsRealm(false, schema.Spec.Target, digest) {
					members++
				}
			}
		}
		if members == 0 {
			b.Fatal("no member was found, so this measures the wrong thing")
		}
	}
}

func realmBenchDigest(b *testing.B, shared bool) string {
	b.Helper()

	values := realmDigestIndexValue(operatorv1alpha1.DatabaseTargetSpec{
		Engine:          operatorv1alpha1.DatabaseEnginePostgreSQL,
		CoordinationKey: realmBenchKey(shared),
	})
	if len(values) != 1 {
		b.Fatalf("the benchmark realm has no digest: %v", values)
	}
	return values[0]
}

func BenchmarkRealmMembership(b *testing.B) {
	for _, count := range []int{100, 500, 1000} {
		for _, shape := range []struct {
			name   string
			shared bool
		}{{name: "distinct-realms", shared: false}, {name: "one-shared-realm", shared: true}} {
			b.Run(fmt.Sprintf("%s/%d/indexed", shape.name, count), func(b *testing.B) {
				benchmarkRealmMembership(b, count, shape.shared, true)
			})
			b.Run(fmt.Sprintf("%s/%d/scanned", shape.name, count), func(b *testing.B) {
				benchmarkRealmMembership(b, count, shape.shared, false)
			})
		}
	}
}

// And the census itself, end to end. The fake client materializes the whole
// list before it applies a field selector, so this does not show the saving a
// manager's cache gives -- it bounds the census's own work around the lookup,
// and it is here so a change that made that work quadratic would be visible.
func BenchmarkRealmCensus(b *testing.B) {
	scheme := runtime.NewScheme()
	if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
		b.Fatal(err)
	}
	for _, count := range []int{100, 1000} {
		b.Run(fmt.Sprintf("distinct-realms/%d", count), func(b *testing.B) {
			api := withRealmIndexes(fake.NewClientBuilder().WithScheme(scheme)).
				WithObjects(realmBenchObjects(count, false)...).Build()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				census, err := takeRealmCensus(context.Background(), api,
					operatorv1alpha1.DatabaseEnginePostgreSQL, realmBenchKey(false))
				if err != nil {
					b.Fatal(err)
				}
				if census.total() == 0 {
					b.Fatal("the census found no claimant, so this measures the wrong thing")
				}
			}
		})
	}
}
