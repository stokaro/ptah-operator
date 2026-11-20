//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// resourceBlocked holds a schema that Ptah's own verification refuses at
// Blocked while its interval repeats the read that refuses it. The phase
// leaves Blocked on every such read; status.blockedSince does not move, so
// the alert dated by it fires once and stays firing across a refresh pass.
// A spec edit that removes the cause keeps the date through the pass it
// starts, which ends at AwaitingApproval and clears it, and the alert
// resolves.
func (a *alertingRun) resourceBlocked() {
	a.t.Helper()
	name := "e2e-alerting-blocked" + a.scope.suffix
	policyName := name + "-policy"
	database := "ptah_alerting_blocked" + strings.ReplaceAll(a.scope.suffix, "-", "_")
	query := fmt.Sprintf(`ALERTS{alertname=%q,family="schema"}`, alBlockedAlert)
	a.waitForTargets()
	all := &ptahv1.PtahSchemaList{}
	a.check(a.cluster.Client.List(a.ctx, all), "inventory every schema before the block")
	// The row's claim is the schema family's alert, which a blocked migration
	// does not fire.
	if leftovers := alBlockedLeftovers(all.Items, nil); len(leftovers) != 0 {
		a.fatalf("schemas already blocked would fire the family's alert before this row's block: %s", strings.Join(leftovers, ", "))
	}
	if !a.noActiveAlerts(query) {
		a.fatalf("%s is already active for the schema family before the block", alBlockedAlert)
	}

	template := &ptahv1.PtahSchema{}
	a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: a.in.TestNamespace, Name: a.scope.schemaProducer}, template), "read the native schema producer")
	if template.Spec.Desired.VerificationPolicyFrom == nil {
		a.fatalf("the native schema producer names no verification policy")
	}
	accepting := template.Spec.Desired.VerificationPolicyFrom.Name
	engine, err := migrationEngineFor("postgresql")
	a.check(err, "select the prepared PostgreSQL server")
	source := template.Spec.Target.URLFrom
	original := &corev1.Secret{}
	a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: template.Namespace, Name: source.Name}, original), "read the producer's target credential")
	raw := original.Data[source.Key]
	connection, err := url.Parse(string(raw))
	if err != nil || connection.Scheme != "postgres" && connection.Scheme != "postgresql" || connection.Host == "" || connection.User == nil {
		a.fatalf("the producer's PostgreSQL credential has no valid URL")
	}
	password, _ := connection.User.Password()
	connection.Path, connection.RawPath = "/"+database, ""
	credential := connection.String()
	protected := [][]byte{raw, []byte(password), []byte(credential)}
	scan := func(body []byte, label string) {
		for _, secret := range protected {
			if len(secret) > 0 && bytes.Contains(body, secret) {
				a.fatalf("credential appeared in %s", label)
			}
		}
		if a.credentials.Password != "" && bytes.Contains(body, []byte(a.credentials.Password)) {
			a.fatalf("registry credential appeared in %s", label)
		}
	}

	// CREATE without IF NOT EXISTS refuses accidental reuse of a database.
	_, err = serverSQL(a.ctx, a.cluster, a.in.TestNamespace, engine, "CREATE DATABASE "+database+" OWNER "+migrationDatabaseUser)
	a.check(err, "create the blocked row's database")
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: a.in.TestNamespace, Name: name + "-db"}, Data: map[string][]byte{source.Key: []byte(credential)}}
	a.check(a.create(secret), "create the blocked row's target Secret")
	refusing, err := os.ReadFile(filepath.Join(repositoryRoot, "testdata", "e2e", otherTypePolicyFile))
	a.check(err, "read the verification policy Ptah refuses by")
	policy := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: a.in.TestNamespace, Name: policyName},
		Immutable:  ptr.To(true),
		Data:       map[string]string{template.Spec.Desired.VerificationPolicyFrom.Key: string(refusing)},
	}
	a.check(a.create(policy), "create the refusing verification policy")
	object, err := alBlockedFixture(template, name, secret.Name, policyName)
	a.check(err, "build the blocked schema")

	watcher, err := newRecorderClient(a.cluster.Config, a.cluster.Scheme)
	a.check(err, "open the blocked schema's watch")
	schemas := newStoredStateRecorder[*ptahv1.PtahSchema](a.t, a.ctx, watcher, "blocked-schemas", a.in.TestNamespace, func() client.ObjectList { return &ptahv1.PtahSchemaList{} })
	history := func() []*ptahv1.PtahSchema {
		var states []*ptahv1.PtahSchema
		for _, event := range schemas.snapshot() {
			states = append(states, event.Object)
		}
		return states
	}
	// A failed row keeps its fixtures for diagnosis; a passing row removes
	// exactly what it created.
	defer func() {
		if a.t.Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		uid := object.UID
		if err := a.cluster.Client.Delete(ctx, object, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
			a.t.Errorf("remove the blocked schema: %v", err)
			return
		}
		if err := harness.Wait(ctx, "the blocked schema's deletion", time.Minute, time.Second, func(ctx context.Context) (bool, string, error) {
			err := a.cluster.Client.Get(ctx, client.ObjectKeyFromObject(object), &ptahv1.PtahSchema{})
			return apierrors.IsNotFound(err), "waiting for owned operation cleanup", client.IgnoreNotFound(err)
		}); err != nil {
			a.t.Errorf("finish the blocked schema's deletion: %v", err)
			return
		}
		if _, err := serverSQL(ctx, a.cluster, a.in.TestNamespace, engine, "DROP DATABASE "+database+" WITH (FORCE)"); err != nil {
			a.t.Errorf("drop the blocked row's database: %v", err)
		}
		for _, owned := range []client.Object{secret, policy} {
			uid := owned.GetUID()
			if err := a.cluster.Client.Delete(ctx, owned, client.Preconditions{UID: &uid}); err != nil {
				a.t.Errorf("remove %s: %v", owned.GetName(), err)
			}
		}
	}()

	from := a.deliveryCount()
	a.check(a.create(object), "create the blocked schema")
	var walk alBlockedWalk
	walkHistory := func(after time.Time) func(context.Context) (bool, string, error) {
		return func(context.Context) (bool, string, error) {
			if err := schemas.alive(); err != nil {
				return false, "", err
			}
			var err error
			walk, err = alBlockedHistory(history(), object.UID, after)
			return true, "", err
		}
	}
	a.check(harness.Wait(a.ctx, "the refused schema's status.blockedSince", alTimeout, time.Second, func(ctx context.Context) (bool, string, error) {
		// The first polls can run before the watch has delivered the
		// created schema. Every later wait starts after a dated state.
		if _, _, err := walkHistory(time.Time{})(ctx); errors.Is(err, errAlBlockedUnseen) {
			return false, "waiting for the watch to record the created schema", nil
		} else if err != nil {
			return false, "", err
		}
		return !walk.since.IsZero(), "waiting for Ptah's refusal to stop the schema at Blocked and date it", nil
	}), "date the refused schema's block")
	since := walk.since
	if since.Before(object.CreationTimestamp.Time) || since.After(time.Now().Add(refusalClockSkew)) {
		a.fatalf("%s dated its block at %s, outside its own life (created %s)", name, since, object.CreationTimestamp)
	}

	labels := map[string]string{"family": "schema", "operator_namespace": a.in.OperatorNamespace, "operator_metrics_service": a.metricsService}
	keep := func() {
		if _, _, err := walkHistory(time.Time{})(a.ctx); err != nil {
			a.fatalf("%s: %v", name, err)
		}
		if !walk.settled.IsZero() {
			a.fatalf("%s cleared its block before the row removed the cause", name)
		}
	}
	firing, index := a.waitForDeliveryWithCheck(alMatch{status: "firing", alertName: alBlockedAlert, labels: labels}, "the blocked schema notification",
		time.Until(since.Add(alBlockedAfter+alDetectionSlack)), from, keep)
	if !alBlockedDelivered(firing, since) || firing.Labels["severity"] != "warning" ||
		firing.Annotations["runbook_url"] != a.runbookBase+"#blocked-resources" || !alRunbookAnchor(a.operationsPage(), "blocked-resources") {
		a.fatalf("the blocked alert missed its threshold after status.blockedSince, its severity or its runbook: %s", firing.raw)
	}
	for _, label := range []string{"operation", "phase", "resource", "reason", "pod", "job", "plan"} {
		if _, present := firing.Labels[label]; present {
			a.fatalf("the blocked alert acquired label %s", label)
		}
	}

	// The read the interval repeats, claimed after the alert fired: the
	// phase goes to Resolving and back to Blocked, and the date stays.
	a.check(harness.Wait(a.ctx, "a refresh pass of the blocked schema after the alert fired", alTimeout, time.Second, func(ctx context.Context) (bool, string, error) {
		if _, _, err := walkHistory(firing.StartsAt)(ctx); err != nil {
			return false, "", err
		}
		if !walk.settled.IsZero() {
			return false, "", fmt.Errorf("%s cleared its block before the row removed the cause", name)
		}
		return walk.refresh.blockedAgain != "", "waiting for a Resolve claimed after the alert fired to end at Blocked again", nil
	}), "observe a refresh pass under the firing alert")
	refresh := walk.refresh
	passEnded := time.Now()
	a.sleep(2 * alScrapeInterval)
	queriedAt := time.Now().UTC()
	snapshot, body := a.historySnapshot(a.ctx, queriedAt, alScrapeJob, `,family="schema"`, alBlockedMetric)
	samples, err := alBlockedAges(snapshot[alBlockedMetric], since, refresh.started, passEnded, queriedAt)
	if err != nil {
		a.logf("blocked-age history: %s", body)
	}
	a.check(err, "hold the blocked age to status.blockedSince across the refresh pass")

	// Remove the cause with a spec edit: the producer's own policy accepts
	// the artifact, so the pass after the edit reads the database, plans,
	// and stops at AwaitingApproval.
	current := &ptahv1.PtahSchema{}
	a.check(a.cluster.Client.Get(a.ctx, client.ObjectKeyFromObject(object), current), "read the blocked schema before its edit")
	if current.UID != object.UID {
		a.fatalf("%s was replaced during its block", name)
	}
	patch := fmt.Appendf(nil, `{"spec":{"desired":{"verificationPolicyFrom":{"name":%q}}}}`, accepting)
	a.check(a.cluster.Client.Patch(a.ctx, current, client.RawPatch(types.MergePatchType, patch)), "point the blocked schema at the policy that accepts its artifact")
	a.check(harness.Wait(a.ctx, "the pass after the edit to clear status.blockedSince", alTimeout, time.Second, func(ctx context.Context) (bool, string, error) {
		if _, _, err := walkHistory(firing.StartsAt)(ctx); err != nil {
			return false, "", err
		}
		return !walk.settled.IsZero(), "waiting for the edited schema to stop at AwaitingApproval", nil
	}), "clear the block by removing its cause")
	if walk.settledGeneration <= walk.generation {
		a.fatalf("%s cleared its block at generation %d, not after the edit of generation %d", name, walk.settledGeneration, walk.generation)
	}
	resolved, _ := a.waitForDeliveryWithCheck(alMatch{status: "resolved", alertName: alBlockedAlert, labels: labels}, "the blocked schema resolution",
		max(time.Until(walk.settled.Add(alStateReadGapHold+alDetectionSlack)), alDeliveryPoll), index+1, func() {})
	if !alBlockedCleared(firing, resolved, refresh.started, walk.settled) || !a.noActiveAlerts(query) {
		a.fatalf("the blocked alert did not span the refresh pass and clear within its bound: firing %s, resolved %s", firing.raw, resolved.raw)
	}
	a.check(a.cluster.Client.Get(a.ctx, client.ObjectKeyFromObject(object), current), "read the cleared schema")
	storedStateWatchBarrier(a.t, a.ctx, a.cluster, schemas, current)
	closeRunnerWatches(a.t, []recorder{schemas}, scan)
	final, err := alBlockedHistory(history(), object.UID, firing.StartsAt)
	a.check(err, "hold the complete history of the blocked schema")
	if !final.since.Equal(since) || final.settled.IsZero() || final.refresh.id != refresh.id {
		a.fatalf("the complete history of %s does not read as the watched one", name)
	}
	a.logf("PASS schema blocked: resourceUID=%s blockedSince=%s firingStartsAt=%s refreshClaim=%s refreshStarted=%s ageSamples=%d settled=%s resolvedReceived=%s",
		object.UID, since.Format(time.RFC3339), firing.StartsAt.Format(time.RFC3339Nano), refresh.id, refresh.started.Format(time.RFC3339), samples,
		walk.settled.Format(time.RFC3339), resolved.ReceivedAt.Format(time.RFC3339Nano))
}
