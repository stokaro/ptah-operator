//go:build e2e

package e2e

import (
	"encoding/json"
	"errors"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// applyRemoval records what the deletion of a running Apply's Job left in the
// API server: the Pods of the Apply's namespace, the Leases of the realm
// Lease's namespace and the migration, each watched from before the deletion.
type applyRemoval struct {
	m          *migrationRun
	what       string
	subject    applyRemovalSubject
	pods       *watchRecorder[*corev1.Pod]
	leases     *watchRecorder[*coordinationv1.Lease]
	migrations *watchRecorder[*ptahv1alpha1.PtahMigration]
}

// watchApplyRemoval reads what a removal is decided against and starts the
// watches that record it. The caller deletes the Job after it returns. claim
// is the migration's claim as it stands, and the Job it names has to own
// exactly one running Pod.
func (m *migrationRun) watchApplyRemoval(migration *ptahv1alpha1.PtahMigration, claim *ptahv1alpha1.MigrationOperationStatus,
	what string,
) *applyRemoval {
	m.t.Helper()
	apiServers := &corev1.PodList{}
	m.check(m.cluster.Client.List(m.ctx, apiServers, client.InNamespace("kube-system"),
		client.MatchingLabels{"component": "kube-apiserver"}), "read the kube-apiserver Pods")
	m.check(singleEtcdForEveryResource(apiServers.Items), "order the writes of a Pod, a Lease and a PtahMigration by resourceVersion")
	if migration == nil || migration.UID == "" || claim == nil || claim.JobUID == "" {
		m.fatalf("%s has no migration and claim to be read against", what)
	}
	job := &batchv1.Job{}
	m.check(m.get(claim.JobName, job), "read %s before removing it", what)
	if job.UID != claim.JobUID {
		m.fatalf("%s under the name %s has UID %s, not the claim's %s", what, claim.JobName, job.UID, claim.JobUID)
	}
	pods := &corev1.PodList{}
	m.check(m.list(pods), "read the Pods before removing %s", what)
	owned := ownedPods(pods.Items, job.UID)
	if len(owned) != 1 || owned[0].Status.Phase != corev1.PodRunning || owned[0].DeletionTimestamp != nil {
		m.fatalf("%s does not own exactly one running Pod before its removal: it owns %d", what, len(owned))
	}
	leases := &coordinationv1.LeaseList{}
	m.check(m.cluster.Client.List(m.ctx, leases), "list the realm Leases")
	leaseNamespace, leaseName, holder, err := leaseAtEpoch(leases.Items, claim.LeaseEpoch)
	if err != nil || holder == "" {
		m.fatalf("no single realm Lease with a holder carries the epoch the %s Apply claimed under: %v", m.engine.name, err)
	}

	watcher, err := client.NewWithWatch(m.cluster.Config, client.Options{Scheme: m.cluster.Scheme})
	m.check(err, "build a client to watch the removal of %s", what)
	r := &applyRemoval{m: m, what: what, subject: applyRemovalSubject{
		PodUID: owned[0].UID, JobUID: job.UID,
		Lease:  types.NamespacedName{Namespace: leaseNamespace, Name: leaseName},
		Holder: holder, Epoch: claim.LeaseEpoch,
		MigrationUID: migration.UID, Claim: claim.DeepCopy(),
	}}
	r.pods = newStoredStateRecorder[*corev1.Pod](m.t, m.ctx, watcher, "Apply removal Pod", m.in.TestNamespace,
		func() client.ObjectList { return &corev1.PodList{} })
	r.leases = newStoredStateRecorder[*coordinationv1.Lease](m.t, m.ctx, watcher, "Apply removal Lease", leaseNamespace,
		func() client.ObjectList { return &coordinationv1.LeaseList{} })
	r.migrations = newStoredStateRecorder[*ptahv1alpha1.PtahMigration](m.t, m.ctx, watcher, "Apply removal PtahMigration",
		migration.Namespace, func() client.ObjectList { return &ptahv1alpha1.PtahMigrationList{} })
	// The histories are read against the claim, so it has to be the one the
	// migration holds once its watch has begun.
	current := &ptahv1alpha1.PtahMigration{}
	m.check(m.cluster.Client.Get(m.ctx, client.ObjectKeyFromObject(migration), current), "read %s once its removal is watched", migration.Name)
	if current.UID != migration.UID || current.Status.PendingLockRelease != nil ||
		!equality.Semantic.DeepEqual(current.Status.ActiveOperation, claim) {
		m.fatalf("%s no longer holds the claim on %s it was read with", migration.Name, what)
	}
	return r
}

// settle waits until the histories decide which branch the removal took, and
// fails where the run took neither: a release before any stored state said
// the executor stopped, another Pod under the Job, or no decision while a
// hold could still be read. It returns the verdict and stops the watches.
func (r *applyRemoval) settle(term time.Time) applyRemovalVerdict {
	m := r.m
	m.t.Helper()
	deadline := term.Add(-applyLeaseTermMargin)
	for {
		for _, watched := range []recorder{r.pods, r.leases, r.migrations} {
			m.check(watched.alive(), "keep the %s watch of %s", watched.stem(), r.what)
		}
		histories := applyRemovalHistories{Pods: r.pods.snapshot(), Leases: r.leases.snapshot(), Migrations: r.migrations.snapshot()}
		verdict, err := readApplyRemoval(histories, r.subject)
		switch {
		case err == nil:
			for _, watched := range []recorder{r.pods, r.leases, r.migrations} {
				watched.abort()
			}
			reading, err := json.Marshal(applyRemovalReading{Subject: r.subject, Verdict: verdict, Histories: histories.trimmed(r.subject)})
			m.check(err, "encode the reading of the removal of %s", r.what)
			m.scan(reading, "the reading of the removal of "+r.what)
			m.logf("the removal of %s measured the branch %s", r.what, verdict)
			m.logf("Apply removal reading: %s", reading)
			return verdict
		case !errors.Is(err, errApplyRemovalPending):
			m.fatalf("the removal of %s took neither branch the manager's contract allows: %v", r.what, err)
		case !time.Now().Before(deadline):
			m.fatalf("the removal of %s was not decided by %s, %s short of the claim's Lease term: %v",
				r.what, deadline.UTC().Format(time.RFC3339), applyLeaseTermMargin, err)
		}
		m.sleep(time.Second)
	}
}
