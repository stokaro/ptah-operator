package controller

import (
	"context"
	"fmt"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/coordination"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
)

// realmCensus counts the resources that claim one coordination realm and are
// allowed to.
//
// It carries counts and never names, because a status written in one namespace
// should not enumerate another namespace's objects. The realm is in the
// reader's own spec, so the counts are enough for whoever may see the others:
// the namespace itself for a coordination key, the administrator who wrote the
// PtahRealm for a realm.
type realmCensus struct {
	Schemas    int
	Migrations int
	// Undeclared is how many claimants have not set spec.target.sharedRealm.
	Undeclared int
}

func (c realmCensus) total() int {
	return c.Schemas + c.Migrations
}

// realmVerdict is what the census decides for the resource that took it.
type realmVerdict struct {
	// Namespace is the namespace of the resource that asked.
	Namespace string
	// Realm is the PtahRealm the resource names, and empty for a
	// coordination key, which the namespace owns and needs nobody to grant.
	Realm string
	// Authorized is false when that PtahRealm does not admit the resource: it
	// does not exist, it does not list the namespace, or it names another
	// engine. A refused resource is counted nowhere, so it blocks nobody.
	Authorized bool
	// Exclusive is true when the PtahRealm admits one claimant at a time.
	Exclusive bool
	// Census counts the admitted claimants, the asker included.
	Census realmCensus
}

// conflict reports a realm more than one admitted resource claims while the
// realm or any claimant has not agreed to share it.
//
// Serialization is not ownership: two resources that never run at the same
// time still undo each other's work by taking turns. So the second claimant is
// refused rather than queued, and sharing is a statement every claimant makes
// for itself -- and, for a PtahRealm, one the administrator makes as well.
func (v realmVerdict) conflict() bool {
	if !v.Authorized {
		return false
	}
	return v.Census.total() > 1 && (v.Exclusive || v.Census.Undeclared > 0)
}

// realmRefusal is the status a refused resource carries. Reason is the same on
// every condition the refusal writes; the messages differ by what each
// condition answers.
type realmRefusal struct {
	Reason operatorv1alpha1.ConditionReason
	// Message says why, for Ready and Blocked.
	Message string
	// Approval and Apply say what the refusal withholds.
	Approval string
	Apply    string
}

// refusal returns the refusal this verdict makes, if it makes one.
//
// Neither message names another namespace or another resource. A refused
// resource learns the realm it asked for, which is its own spec, and nothing
// about who else claims it; a contested one learns how many others there are.
func (v realmVerdict) refusal() (realmRefusal, bool) {
	c := v.Census
	switch {
	case !v.Authorized:
		return realmRefusal{
			Reason: operatorv1alpha1.ReasonRealmNotAuthorized,
			Message: fmt.Sprintf(
				"The PtahRealm %q does not admit this resource: it does not exist, "+
					"it does not list namespace %q, or it names another engine. "+
					"Nothing runs against the database until an administrator admits the namespace, "+
					"and this resource is not counted against the ones the realm does admit.",
				v.Realm, v.Namespace,
			),
			Approval: "No plan is approvable while the database realm does not admit this resource",
			Apply:    "No Apply operation is authorized while the database realm does not admit this resource",
		}, true
	case !v.conflict():
		return realmRefusal{}, false
	case v.Exclusive:
		return realmRefusal{
			Reason: operatorv1alpha1.ReasonRealmConflict,
			Message: fmt.Sprintf(
				"This database realm is claimed by %d resources (%d PtahSchema, %d PtahMigration), "+
					"and its PtahRealm %q admits one at a time. "+
					"Nothing runs against the database until the others stop claiming it.",
				c.total(), c.Schemas, c.Migrations, v.Realm,
			),
			Approval: "No plan is approvable while the database realm is contested",
			Apply:    "No Apply operation is authorized while the database realm is contested",
		}, true
	default:
		return realmRefusal{
			Reason: operatorv1alpha1.ReasonRealmConflict,
			Message: fmt.Sprintf(
				"This database realm is claimed by %d resources (%d PtahSchema, %d PtahMigration) "+
					"and %d of them have not set spec.target.sharedRealm. "+
					"Nothing runs against the database until every claimant declares the realm shared, "+
					"or until the others stop claiming it.",
				c.total(), c.Schemas, c.Migrations, c.Undeclared,
			),
			Approval: "No plan is approvable while the database realm is contested",
			Apply:    "No Apply operation is authorized while the database realm is contested",
		}, true
	}
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
			return realmDigestIndexValue(schema.Namespace, schema.Spec.Target)
		}); err != nil {
		return fmt.Errorf("index schemas by coordination realm: %w", err)
	}
	if err := indexer.IndexField(ctx, &operatorv1alpha1.PtahMigration{}, RealmDigestIndex,
		func(object client.Object) []string {
			migration, ok := object.(*operatorv1alpha1.PtahMigration)
			if !ok {
				return nil
			}
			return realmDigestIndexValue(migration.Namespace, migration.Spec.Target)
		}); err != nil {
		return fmt.Errorf("index migrations by coordination realm: %w", err)
	}
	return nil
}

// realmDigestIndexValue is the realm one spec names, and nothing for a target
// whose digest cannot be derived -- the same answer claimsRealm gives such a
// target, so an unindexable resource is absent rather than counted everywhere.
//
// The namespace is an input because a coordination key is scoped to it: the
// same key in two namespaces is two realms and two index values.
//
// Dormancy is deliberately not part of this. Suspending or deleting a resource
// takes it out of the census, but it is the census that decides that: an index
// that dropped dormant resources would have to be rebuilt on a field the
// membership does not depend on, and the reading would then be spread across
// two places. Authorization is not part of it either, for the same reason and
// one more: it lives in a PtahRealm, whose changes do not re-index anything.
func realmDigestIndexValue(namespace string, target operatorv1alpha1.DatabaseTargetSpec) []string {
	digest, err := coordination.Digest(namespace, target)
	if err != nil {
		return nil
	}
	return []string{digest}
}

// takeRealmCensus decides whether the resource in namespace whose target is
// given may claim its realm now, and counts every PtahSchema and PtahMigration
// that claims the same realm and is allowed to, the caller included.
//
// A coordination key names a realm inside one namespace, so every member of
// its census lives there and nobody has to grant the claim. A PtahRealm names
// a realm at cluster scope and grants it to the namespaces it lists: a
// resource elsewhere is refused, and it is left out of the count of everyone
// else, so a claimant nobody admitted can neither block the ones who were nor
// learn how many they are.
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
// The reader is the controller's cache, for the claimants and for the
// PtahRealm alike. A peer created moments ago may not be in it yet, which is
// the one window where both resources see themselves alone. Nothing runs
// concurrently in that window -- the database's own lock still serializes them
// -- and what this refusal exists to prevent is two managers undoing each other
// over many reconciliations, by which time the cache holds both. A grant an
// administrator withdrew moments ago is the same window from the other side,
// and it closes the same way: the next claim reads the withdrawal. An uncached
// read of three kinds on every pass would buy a millisecond of exactness for a
// request per reconciliation.
func takeRealmCensus(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	target operatorv1alpha1.DatabaseTargetSpec,
) (realmVerdict, error) {
	digest, err := coordination.Digest(namespace, target)
	if err != nil {
		return realmVerdict{}, fmt.Errorf("derive coordination realm: %w", err)
	}
	verdict := realmVerdict{Namespace: namespace, Realm: coordination.RealmName(target), Authorized: true}

	// admitted is the grant the census counts under. A coordination key admits
	// its own namespace, which is the only one its digest can come from.
	admitted := func(string) bool { return true }
	if verdict.Realm != "" {
		realm := &operatorv1alpha1.PtahRealm{}
		if err := reader.Get(ctx, client.ObjectKey{Name: verdict.Realm}, realm); err != nil {
			if !apierrors.IsNotFound(err) {
				return realmVerdict{}, fmt.Errorf("read the PtahRealm this resource names: %w", err)
			}
			verdict.Authorized = false
			return verdict, nil
		}
		if !realmAdmits(realm, namespace, target.Engine) {
			verdict.Authorized = false
			return verdict, nil
		}
		verdict.Exclusive = realm.Spec.Sharing != operatorv1alpha1.RealmSharingShared
		// Every member of this index bucket derived its digest from the same
		// realm name and the same engine, which realmAdmits just held to the
		// realm's own. So the namespace is what is left to decide.
		admitted = func(member string) bool { return slices.Contains(realm.Spec.Namespaces, member) }
	}

	members := client.MatchingFields{RealmDigestIndex: digest}
	schemas := &operatorv1alpha1.PtahSchemaList{}
	if err := reader.List(ctx, schemas, members); err != nil {
		return realmVerdict{}, fmt.Errorf("list schemas claiming the coordination realm: %w", err)
	}
	for index := range schemas.Items {
		schema := &schemas.Items[index]
		if !admitted(schema.Namespace) ||
			!claimsRealm(schema.DeletionTimestamp != nil || schema.Spec.Suspend, schema.Namespace, schema.Spec.Target, digest) {
			continue
		}
		verdict.Census.Schemas++
		if !schema.Spec.Target.SharedRealm {
			verdict.Census.Undeclared++
		}
	}

	migrations := &operatorv1alpha1.PtahMigrationList{}
	if err := reader.List(ctx, migrations, members); err != nil {
		return realmVerdict{}, fmt.Errorf("list migrations claiming the coordination realm: %w", err)
	}
	for index := range migrations.Items {
		migration := &migrations.Items[index]
		if !admitted(migration.Namespace) ||
			!claimsRealm(migration.DeletionTimestamp != nil || migration.Spec.Suspend, migration.Namespace, migration.Spec.Target, digest) {
			continue
		}
		verdict.Census.Migrations++
		if !migration.Spec.Target.SharedRealm {
			verdict.Census.Undeclared++
		}
	}
	return verdict, nil
}

// realmAdmits reports whether realm grants a claim from namespace for engine.
//
// The engines are compared as families, the way the digest compares them, so
// "postgres" in a resource and "PostgreSQL" in the realm are one engine. An
// engine the operator does not support admits nothing.
func realmAdmits(realm *operatorv1alpha1.PtahRealm, namespace string, engine operatorv1alpha1.DatabaseEngine) bool {
	if !slices.Contains(realm.Spec.Namespaces, namespace) {
		return false
	}
	granted, err := fingerprint.CanonicalDatabaseEngine(string(realm.Spec.Engine))
	if err != nil {
		return false
	}
	claimed, err := fingerprint.CanonicalDatabaseEngine(string(engine))
	return err == nil && claimed == granted
}

// claimsRealm reports whether one resource claims the realm the digest names.
// A dormant resource -- deleting, or suspended -- claims nothing.
//
// A target whose own digest cannot be derived claims nothing: the key it holds
// is one the API validation refuses, so no admitted resource can reach the same
// database through it.
func claimsRealm(dormant bool, namespace string, target operatorv1alpha1.DatabaseTargetSpec, digest string) bool {
	if dormant {
		return false
	}
	candidate, err := coordination.Digest(namespace, target)
	if err != nil {
		return false
	}
	return candidate == digest
}

// realmClaimantRequests wakes the resources of one kind that name realm.
//
// A grant changing is another object's event, like a peer's declaration, and
// the bounded re-check below would reach every claimant within a minute
// anyway. The watch is what makes that immediate, and what makes the manager
// sync its PtahRealm cache before any reconcile reads one: a census that
// started the informer on its first read would wait for that sync inside a
// reconcile, with nothing bounding the wait.
//
// A claimant that names the realm with another engine derived another digest
// and is not found here. It is refused whatever the realm says, and the
// re-check is enough for it.
func realmClaimantRequests(
	ctx context.Context,
	reader client.Reader,
	newList func() client.ObjectList,
	object client.Object,
) []reconcile.Request {
	realm, ok := object.(*operatorv1alpha1.PtahRealm)
	if !ok {
		return nil
	}
	digest, err := fingerprint.DatabaseRealmDigest(string(realm.Spec.Engine), realm.Name)
	if err != nil {
		return nil
	}
	list := newList()
	if err := reader.List(ctx, list, client.MatchingFields{RealmDigestIndex: digest}); err != nil {
		return nil
	}
	var requests []reconcile.Request
	_ = meta.EachListItem(list, func(item runtime.Object) error {
		if claimant, ok := item.(client.Object); ok {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(claimant)})
		}
		return nil
	})
	return requests
}

// realmRecheckCeiling bounds how long a refused realm claim waits for another
// look.
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
