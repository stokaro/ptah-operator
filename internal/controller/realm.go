package controller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
)

// realmCensus counts the resources that claim one coordination realm.
//
// It carries counts and never names, because a status written in one namespace
// should not enumerate another namespace's objects. The coordination key is in
// the reader's own spec, so the counts are enough to find the others.
type realmCensus struct {
	Schemas    int
	Migrations int
	// Undeclared is how many claimants have not set spec.target.sharedRealm.
	Undeclared int
}

func (c realmCensus) total() int {
	return c.Schemas + c.Migrations
}

// conflict reports a realm more than one resource claims while any claimant
// has left the declaration false.
//
// Serialization is not ownership: two resources that never run at the same
// time still undo each other's work by taking turns. So the second claimant is
// refused rather than queued, and sharing is a statement every claimant makes
// for itself.
func (c realmCensus) conflict() bool {
	return c.total() > 1 && c.Undeclared > 0
}

func (c realmCensus) message() string {
	return fmt.Sprintf(
		"This database realm is claimed by %d resources (%d PtahSchema, %d PtahMigration) "+
			"and %d of them have not set spec.target.sharedRealm. "+
			"Nothing runs against the database until every claimant declares the realm shared, "+
			"or until the others stop claiming it.",
		c.total(), c.Schemas, c.Migrations, c.Undeclared,
	)
}

// RealmDigestIndex names both families by the coordination realm their spec
// claims, so a census reads the members of one realm rather than every resource
// in the cluster.
//
// Without it each pass allocated and scanned both complete lists, and an idle
// sweep over N resources examined about N squared entries. Sharing one informer
// cache makes that cheap per entry and does not make it constant.
const RealmDigestIndex = "spec.target.realmDigest"

// RegisterRealmIndexes indexes both kinds for the realm census.
//
// Both are registered together, and from the manager rather than from either
// controller's own setup, because the census reads both kinds whichever
// controller asks for it. Registering each from its own controller would make
// a manager that runs one of them return "no index with name ..." for the
// other, and controller-runtime refuses a second registration of the same
// field, so they cannot each register both.
func RegisterRealmIndexes(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &operatorv1alpha1.PtahSchema{}, RealmDigestIndex,
		func(object client.Object) []string {
			schema, ok := object.(*operatorv1alpha1.PtahSchema)
			if !ok {
				return nil
			}
			return realmDigestIndexValue(schema.Spec.Target)
		}); err != nil {
		return fmt.Errorf("index schemas by coordination realm: %w", err)
	}
	if err := indexer.IndexField(ctx, &operatorv1alpha1.PtahMigration{}, RealmDigestIndex,
		func(object client.Object) []string {
			migration, ok := object.(*operatorv1alpha1.PtahMigration)
			if !ok {
				return nil
			}
			return realmDigestIndexValue(migration.Spec.Target)
		}); err != nil {
		return fmt.Errorf("index migrations by coordination realm: %w", err)
	}
	return nil
}

// realmDigestIndexValue is the realm one spec names, and nothing for a target
// whose digest cannot be derived -- the same answer claimsRealm gives such a
// target, so an unindexable resource is absent rather than counted everywhere.
//
// Dormancy is deliberately not part of this. Suspending or deleting a resource
// takes it out of the census, but it is the census that decides that: an index
// that dropped dormant resources would have to be rebuilt on a field the
// membership does not depend on, and the reading would then be spread across
// two places.
func realmDigestIndexValue(target operatorv1alpha1.DatabaseTargetSpec) []string {
	digest, err := fingerprint.DatabaseCoordinationDigest(string(target.Engine), target.CoordinationKey)
	if err != nil {
		return nil
	}
	return []string{digest}
}

// takeRealmCensus counts every PtahSchema and PtahMigration whose spec names
// the realm the given engine and coordination key name, the caller included.
//
// The digest comes from each claimant's own spec rather than from its status:
// a resource the controller has not reconciled yet has no status target, and a
// realm it will claim on its first pass is one this census has to see now.
//
// A resource that runs nothing claims nothing. Deleting one leaves the realm,
// and so does suspending it: suspension is how a resource steps aside without
// being deleted, and a resource that cannot dispatch cannot take a turn at the
// database. Resuming it puts it back in the census, and the conflict is refused
// then -- before any Job, because the census runs before the first claim.
//
// The reader is the controller's cache. A peer created moments ago may not be
// in it yet, which is the one window where both resources see themselves alone.
// Nothing runs concurrently in that window -- the database's own lock still
// serializes them -- and what this refusal exists to prevent is two managers
// undoing each other over many reconciliations, by which time the cache holds
// both. An uncached list of two kinds on every pass would buy a millisecond of
// exactness for a request per reconciliation.
func takeRealmCensus(
	ctx context.Context,
	reader client.Reader,
	engine operatorv1alpha1.DatabaseEngine,
	coordinationKey string,
) (realmCensus, error) {
	digest, err := fingerprint.DatabaseCoordinationDigest(string(engine), coordinationKey)
	if err != nil {
		return realmCensus{}, fmt.Errorf("derive coordination realm: %w", err)
	}

	var census realmCensus
	members := client.MatchingFields{RealmDigestIndex: digest}
	schemas := &operatorv1alpha1.PtahSchemaList{}
	if err := reader.List(ctx, schemas, members); err != nil {
		return realmCensus{}, fmt.Errorf("list schemas claiming the coordination realm: %w", err)
	}
	for index := range schemas.Items {
		schema := &schemas.Items[index]
		if !claimsRealm(schema.DeletionTimestamp != nil || schema.Spec.Suspend, schema.Spec.Target, digest) {
			continue
		}
		census.Schemas++
		if !schema.Spec.Target.SharedRealm {
			census.Undeclared++
		}
	}

	migrations := &operatorv1alpha1.PtahMigrationList{}
	if err := reader.List(ctx, migrations, members); err != nil {
		return realmCensus{}, fmt.Errorf("list migrations claiming the coordination realm: %w", err)
	}
	for index := range migrations.Items {
		migration := &migrations.Items[index]
		if !claimsRealm(migration.DeletionTimestamp != nil || migration.Spec.Suspend, migration.Spec.Target, digest) {
			continue
		}
		census.Migrations++
		if !migration.Spec.Target.SharedRealm {
			census.Undeclared++
		}
	}
	return census, nil
}

// claimsRealm reports whether one resource claims the realm the digest names.
// A dormant resource -- deleting, or suspended -- claims nothing.
//
// A target whose own digest cannot be derived claims nothing: the key it holds
// is one the API validation refuses, so no admitted resource can reach the same
// database through it.
func claimsRealm(dormant bool, target operatorv1alpha1.DatabaseTargetSpec, digest string) bool {
	if dormant {
		return false
	}
	candidate, err := fingerprint.DatabaseCoordinationDigest(string(target.Engine), target.CoordinationKey)
	if err != nil {
		return false
	}
	return candidate == digest
}

// realmRecheckCeiling bounds how long a contested realm waits for another look.
//
// What ends a conflict is another resource's spec change, and that resource's
// events do not reach this one. A refusal that waited out this resource's own
// interval would keep a ten-minute resource blocked for ten minutes after the
// declaration that resolved it, and an hourly one for an hour. Two cached
// lists are cheap enough to repeat every minute for as long as the refusal
// stands, so the deadline is the shorter of the two.
const realmRecheckCeiling = time.Minute

// realmBlockDeadline keeps the deadline a standing refusal already carries.
//
// A controller watches its own resource, so a status write it makes wakes it
// again. A refusal that stamped a new next-reconciliation time on every pass
// would differ from the stored status every time, patch every time, and wake
// itself every time: a hot loop that never sleeps and never says why. Reusing
// a deadline that has not passed makes the second pass a no-op write, which is
// no write at all, and the ceiling keeps the interval between two writes at a
// minute rather than at nothing.
func realmBlockDeadline(current *metav1.Time, now time.Time, interval time.Duration) metav1.Time {
	if current != nil && current.After(now) {
		return *current
	}
	if interval <= 0 || interval > realmRecheckCeiling {
		interval = realmRecheckCeiling
	}
	return metav1.NewTime(now.Add(interval))
}
