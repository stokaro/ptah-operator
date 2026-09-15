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

// takeRealmCensus counts every PtahSchema and PtahMigration whose spec names
// the realm the given engine and coordination key name, the caller included.
//
// The digest comes from each claimant's own spec rather than from its status:
// a resource the controller has not reconciled yet has no status target, and a
// realm it will claim on its first pass is one this census has to see now.
//
// A resource being deleted is not counted. Deleting is how a resource leaves a
// realm; suspending is not, because suspension is a pause and the resource
// still means to manage the database when it ends.
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
	schemas := &operatorv1alpha1.PtahSchemaList{}
	if err := reader.List(ctx, schemas); err != nil {
		return realmCensus{}, fmt.Errorf("list schemas claiming the coordination realm: %w", err)
	}
	for index := range schemas.Items {
		schema := &schemas.Items[index]
		if !claimsRealm(schema.DeletionTimestamp != nil, schema.Spec.Target, digest) {
			continue
		}
		census.Schemas++
		if !schema.Spec.Target.SharedRealm {
			census.Undeclared++
		}
	}

	migrations := &operatorv1alpha1.PtahMigrationList{}
	if err := reader.List(ctx, migrations); err != nil {
		return realmCensus{}, fmt.Errorf("list migrations claiming the coordination realm: %w", err)
	}
	for index := range migrations.Items {
		migration := &migrations.Items[index]
		if !claimsRealm(migration.DeletionTimestamp != nil, migration.Spec.Target, digest) {
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
//
// A target whose own digest cannot be derived claims nothing: the key it holds
// is one the API validation refuses, so no admitted resource can reach the same
// database through it.
func claimsRealm(deleting bool, target operatorv1alpha1.DatabaseTargetSpec, digest string) bool {
	if deleting {
		return false
	}
	candidate, err := fingerprint.DatabaseCoordinationDigest(string(target.Engine), target.CoordinationKey)
	if err != nil {
		return false
	}
	return candidate == digest
}

// realmRecheckInterval is the floor between two verdicts about a contested
// realm. The resource's own interval decides the cadence; the floor only keeps
// an object whose interval is absent from turning a refusal into a hot loop.
const realmRecheckInterval = time.Minute

func realmRecheckDeadline(now time.Time, interval time.Duration) metav1.Time {
	if interval < realmRecheckInterval {
		interval = realmRecheckInterval
	}
	return metav1.NewTime(now.Add(interval))
}
