//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

func (a *alertingRun) resourceOverdue() {
	for _, family := range []string{"schema", "migration"} {
		a.overdueFamily(family)
	}
}

func (a *alertingRun) overdueFamily(family string) {
	a.t.Helper()
	name := "overdue-" + family
	read := func(ctx context.Context, name string) (client.Object, error) {
		var object client.Object = &ptahv1.PtahSchema{}
		if family == "migration" {
			object = &ptahv1.PtahMigration{}
		}
		err := a.cluster.Client.Get(ctx, types.NamespacedName{Namespace: alStalledNamespace, Name: name}, object)
		return object, err
	}
	query := fmt.Sprintf(`ALERTS{alertname=%q,family=%q}`, alOverdueAlert, family)
	a.waitForTargets()
	if !a.noActiveAlerts(query) {
		a.fatalf("a %s was already overdue before the fault", family)
	}
	lease, managers := a.managerSnapshot()
	checkManagers := func() {
		currentLease, pods := a.managerSnapshot()
		if !alSameManagers(currentLease, pods, lease, managers) {
			a.fatalf("manager identity or leadership changed during the overdue fault")
		}
		body, err := a.prometheus(a.ctx, "/api/v1/targets", nil)
		a.check(err, "read the manager scrape targets")
		if !alTargetsReady(body, int(a.replicas)) {
			a.fatalf("a manager scrape failed during the overdue fault")
		}
	}
	realm := alOverdueRealm(family)
	if err := a.cluster.Client.Get(a.ctx, client.ObjectKeyFromObject(realm), &ptahv1.PtahRealm{}); !apierrors.IsNotFound(err) {
		a.fatalf("the overdue fixture's realm must be absent before its refusal")
	}
	a.check(a.cluster.Client.Create(a.ctx, &unstructured.Unstructured{Object: alOverdueResource(family)}, client.FieldOwner(harness.FieldOwner), client.FieldValidation("Strict")), "create the overdue %s fixture", family)
	var before alOverdueState
	a.check(harness.Wait(a.ctx, "a persisted eligible realm-recheck deadline", alTimeout, time.Second, func(ctx context.Context) (bool, string, error) {
		object, err := read(ctx, name)
		if err != nil {
			return false, "", err
		}
		before = alOverdueReading(object)
		return before.scheduled() && before.next.After(time.Now().Add(30*time.Second)), "waiting for the original realm refusal's next reconciliation deadline", nil
	}), "record the eligible %s deadline", family)
	watcher, err := client.NewWithWatch(a.cluster.Config, client.Options{Scheme: a.cluster.Scheme})
	a.check(err, "open the overdue resource watch")
	var history func() []client.Object
	var finish func(client.Object)
	scan := func(raw []byte, label string) {
		if a.credentials.Password != "" && bytes.Contains(raw, []byte(a.credentials.Password)) {
			a.fatalf("registry credential appeared in %s", label)
		}
	}
	if family == "schema" {
		r := newStoredStateRecorder[*ptahv1.PtahSchema](a.t, a.ctx, watcher, "overdue-schemas", alStalledNamespace, func() client.ObjectList { return &ptahv1.PtahSchemaList{} })
		history = func() []client.Object {
			var objects []client.Object
			for _, event := range r.snapshot() {
				objects = append(objects, event.Object)
			}
			return objects
		}
		finish = func(object client.Object) {
			storedStateWatchBarrier(a.t, a.ctx, a.cluster, r, object.(*ptahv1.PtahSchema))
			closeRunnerWatches(a.t, []recorder{r}, scan)
		}
	} else {
		r := newStoredStateRecorder[*ptahv1.PtahMigration](a.t, a.ctx, watcher, "overdue-migrations", alStalledNamespace, func() client.ObjectList { return &ptahv1.PtahMigrationList{} })
		history = func() []client.Object {
			var objects []client.Object
			for _, event := range r.snapshot() {
				objects = append(objects, event.Object)
			}
			return objects
		}
		finish = func(object client.Object) {
			storedStateWatchBarrier(a.t, a.ctx, a.cluster, r, object.(*ptahv1.PtahMigration))
			closeRunnerWatches(a.t, []recorder{r}, scan)
		}
	}
	probe := func(ctx context.Context, objectName string) error {
		object, err := read(ctx, objectName)
		if err != nil {
			return err
		}
		return a.cluster.Client.Patch(ctx, object, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"operator.ptah.run/e2e-overdue-probe":"true"}}}`)), client.DryRunAll)
	}
	a.check(probe(a.ctx, name), "admit the exact resource write before its fault")
	policy, binding := alOverduePolicy(before)
	var created []client.Object
	remove := func(ctx context.Context) error {
		for len(created) > 0 {
			object := created[len(created)-1]
			uid := object.GetUID()
			if err := a.cluster.Client.Delete(ctx, object, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			created = created[:len(created)-1]
		}
		return nil
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := remove(ctx); err != nil {
			a.t.Errorf("remove the exact overdue fault: %v", err)
		}
	}()
	from := a.deliveryCount()
	for _, object := range []client.Object{policy, binding} {
		a.check(a.create(object), "install the exact %s write fault", family)
		created = append(created, object)
	}
	a.check(harness.Wait(a.ctx, "the exact resource-specific admission refusal", 20*time.Second, time.Second, func(ctx context.Context) (bool, string, error) {
		err := probe(ctx, name)
		if err != nil && !alOverdueDenied(err, policy.Name) {
			return false, "", err
		}
		return alOverdueDenied(err, policy.Name), "the write fault has not propagated", nil
	}), "observe the overdue admission fault")
	a.check(probe(a.ctx, alStalledSchema), "keep the same family's other resource writable during the fault")
	if !time.Now().Before(before.next) {
		a.fatalf("the write fault did not become effective before the persisted deadline")
	}
	checkHeld := func() {
		checkManagers()
		object, err := read(a.ctx, name)
		a.check(err, "read the held %s deadline", family)
		if alOverdueReading(object) != before {
			a.fatalf("the held %s changed identity, eligibility, claim or deadline", family)
		}
		if !alOverdueDenied(probe(a.ctx, name), policy.Name) {
			a.fatalf("the exact overdue write fault stopped holding")
		}
	}
	checkHeld()
	labels := map[string]string{"family": family, "operator_namespace": a.in.OperatorNamespace, "operator_metrics_service": a.metricsService}
	firing, index := a.waitForDeliveryWithCheck(alMatch{status: "firing", alertName: alOverdueAlert, labels: labels}, "the overdue "+family+" notification", time.Until(before.next.Add(alOverdueAfter+alDetectionSlack)), from, checkHeld)
	if !alOverdueDelivered(firing, before.next) || firing.Labels["severity"] != "warning" ||
		firing.Annotations["runbook_url"] != a.runbookBase+"#resource-state" || !alRunbookAnchor(a.operationsPage(), "resource-state") {
		a.fatalf("the %s overdue alert missed its native deadline, severity or runbook", family)
	}
	for _, label := range []string{"operation", "resource", "pod", "job", "plan", "execution"} {
		if _, present := firing.Labels[label]; present {
			a.fatalf("the overdue alert acquired label %s", label)
		}
	}
	// Grant the resource's isolated realm while writes remain refused. Only
	// restoring its own write permission can let the missed reconciliation run.
	a.check(a.create(realm), "grant the overdue fixture's isolated realm")
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		uid := realm.UID
		if err := a.cluster.Client.Delete(ctx, realm, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
			a.t.Errorf("remove the exact overdue realm grant: %v", err)
		}
	}()
	checkHeld()
	a.check(remove(a.ctx), "remove the overdue write fault")
	a.check(harness.Wait(a.ctx, "the restored write permission", 20*time.Second, time.Second, func(ctx context.Context) (bool, string, error) {
		err := probe(ctx, name)
		if err != nil && !alOverdueDenied(err, policy.Name) {
			return false, "", err
		}
		return err == nil, "the removed write fault is still propagating", nil
	}), "admit the original resource write after fault removal")
	var recovered alStalledClaim
	a.check(harness.Wait(a.ctx, "the watched new Resolve claim after the missed deadline", alTimeout, time.Second, func(context.Context) (bool, string, error) {
		checkManagers()
		var found bool
		recovered, found = alOverdueRecovery(history(), before)
		return found, "waiting for a new persisted Resolve claim with no overdue deadline", nil
	}), "observe native %s reconciliation recovery", family)
	// This shared reader verifies the original resource's exact new Job and
	// one owned Pod. It does not require a transient phase to remain visible.
	a.check(harness.Wait(a.ctx, "the recovered operation's real workload", alTimeout, time.Second, func(context.Context) (bool, string, error) {
		_, _, found := a.heldWorkload(recovered, "")
		return found, "the new Resolve has no Pod yet", nil
	}), "verify actual %s progress after restoring writes", family)
	resolved, _ := a.waitForDeliveryWithCheck(alMatch{status: "resolved", alertName: alOverdueAlert, labels: labels}, "the overdue "+family+" resolution", max(time.Until(recovered.started.Add(alDetectionSlack)), alDeliveryPoll), index+1, checkManagers)
	if !alOverdueCleared(firing, resolved, recovered.started) || !a.noActiveAlerts(query) {
		a.fatalf("the overdue %s incident did not clear within its native recovery bound", family)
	}
	object, err := read(a.ctx, name)
	a.check(err, "read the recovered %s", family)
	a.check(a.cluster.Client.Patch(a.ctx, object, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"suspend":true}}`))), "suspend the recovered overdue fixture")
	finish(object)
	a.logf("PASS %s overdue: resourceUID=%s persistedDeadline=%s firingReceived=%s recoveryClaim=%s recoveredAt=%s resolvedReceived=%s", family, before.resource.uid, before.next, firing.ReceivedAt, recovered.id, recovered.started, resolved.ReceivedAt)
}
