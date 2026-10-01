//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"net/url"
	"strings"
	"time"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type alNegativeFixtureRun struct {
	object   client.Object
	secret   *corev1.Secret
	database string
	initial  alNegativeState
}

func (a *alertingRun) negativeControls() {
	a.t.Helper()
	engine, err := migrationEngineFor("postgresql")
	a.check(err, "select the prepared PostgreSQL server")
	schema := &ptahv1.PtahSchema{}
	migration := &ptahv1.PtahMigration{}
	a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: a.in.TestNamespace, Name: "e2e-reference-postgresql"}, schema), "read the real reference schema producer")
	a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: a.in.TestNamespace, Name: "e2e-migrations-postgresql"}, migration), "read the real migration producer")
	watcher, err := client.NewWithWatch(a.cluster.Config, client.Options{Scheme: a.cluster.Scheme})
	a.check(err, "open negative-control watches")
	schemas := newStoredStateRecorder[*ptahv1.PtahSchema](a.t, a.ctx, watcher, "negative-schemas", a.in.TestNamespace, func() client.ObjectList { return &ptahv1.PtahSchemaList{} })
	migrations := newStoredStateRecorder[*ptahv1.PtahMigration](a.t, a.ctx, watcher, "negative-migrations", a.in.TestNamespace, func() client.ObjectList { return &ptahv1.PtahMigrationList{} })
	jobs := newStoredStateRecorder[*batchv1.Job](a.t, a.ctx, watcher, "negative-jobs", a.in.TestNamespace, func() client.ObjectList { return &batchv1.JobList{} })
	var fixtures []*alNegativeFixtureRun
	var protected [][]byte
	scan := func(raw []byte, label string) {
		for _, secret := range protected {
			if len(secret) > 0 && bytes.Contains(raw, secret) {
				a.fatalf("credential appeared in %s", label)
			}
		}
		if a.credentials.Password != "" && bytes.Contains(raw, []byte(a.credentials.Password)) {
			a.fatalf("registry credential appeared in %s", label)
		}
	}
	for _, template := range []client.Object{schema, migration} {
		family := alStalledReading(template).family
		var source *corev1.SecretKeySelector
		if family == "schema" {
			source = &schema.Spec.Target.URLFrom
		} else {
			source = &migration.Spec.Target.URLFrom
		}
		original := &corev1.Secret{}
		a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: template.GetNamespace(), Name: source.Name}, original), "read the prepared target credential")
		raw := original.Data[source.Key]
		connection, parseErr := url.Parse(string(raw))
		if parseErr != nil || connection.Scheme != "postgres" && connection.Scheme != "postgresql" || connection.Host == "" || connection.User == nil {
			a.fatalf("the prepared PostgreSQL credential has no valid URL")
		}
		password, _ := connection.User.Password()
		protected = append(protected, raw, []byte(password))
		for _, policy := range []ptahv1.ApplyPolicy{ptahv1.ApplyPolicyOnApproval, ptahv1.ApplyPolicyNever} {
			suffix := "approval"
			if policy == ptahv1.ApplyPolicyNever {
				suffix = "refusal"
			}
			name := "e2e-negative-" + family + "-" + suffix
			database := "ptah_negative_" + family + "_" + suffix
			// CREATE without IF NOT EXISTS refuses accidental reuse of a database.
			_, err := serverSQL(a.ctx, a.cluster, a.in.TestNamespace, engine, "CREATE DATABASE "+database+" OWNER "+migrationDatabaseUser)
			a.check(err, "create the isolated %s database", name)
			connectionCopy := *connection
			connectionCopy.Path = "/" + database
			connectionCopy.RawPath = ""
			credential := connectionCopy.String()
			protected = append(protected, []byte(credential))
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: a.in.TestNamespace, Name: name + "-db"}, Data: map[string][]byte{source.Key: []byte(credential)}}
			a.check(a.create(secret), "create the isolated %s target Secret", name)
			object, err := alNegativeFixture(template, name, secret.Name, policy)
			a.check(err, "build %s", name)
			a.check(a.create(object), "create %s", name)
			fixture := &alNegativeFixtureRun{object: object, secret: secret, database: database}
			fixtures = append(fixtures, fixture)
			a.check(harness.Wait(a.ctx, "the real "+name+" policy gate", alTimeout, time.Second, func(ctx context.Context) (bool, string, error) {
				err := a.cluster.Client.Get(ctx, client.ObjectKeyFromObject(object), object)
				if err != nil {
					return false, "", err
				}
				fixture.initial = alNegativeReading(object)
				return fixture.initial.gated(), "waiting for a published plan and its current policy decision", nil
			}), "observe %s at its policy gate", name)
		}
	}
	// These are development fixtures of this phase. Failed runs retain their
	// cluster for diagnosis; a successful row removes its exact objects and DBs.
	defer func() {
		if a.t.Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, f := range fixtures {
			uid := f.object.GetUID()
			if err := a.cluster.Client.Delete(ctx, f.object, client.Preconditions{UID: &uid}); err != nil {
				a.t.Errorf("remove negative-control resource: %v", err)
				continue
			}
			if err := harness.Wait(ctx, "negative-control resource deletion", time.Minute, time.Second, func(ctx context.Context) (bool, string, error) {
				copy := f.object.DeepCopyObject().(client.Object)
				err := a.cluster.Client.Get(ctx, client.ObjectKeyFromObject(copy), copy)
				if client.IgnoreNotFound(err) != nil {
					return false, "", err
				}
				return err != nil, "waiting for owned operation cleanup", nil
			}); err != nil {
				a.t.Errorf("finish negative-control resource deletion: %v", err)
				continue
			}
			if _, err := serverSQL(ctx, a.cluster, a.in.TestNamespace, engine, "DROP DATABASE "+f.database+" WITH (FORCE)"); err != nil {
				a.t.Errorf("drop negative-control database: %v", err)
			}
			uid = f.secret.UID
			if err := a.cluster.Client.Delete(ctx, f.secret, client.Preconditions{UID: &uid}); err != nil {
				a.t.Errorf("remove negative-control Secret: %v", err)
			}
		}
	}()
	a.waitForTargets()
	if !a.noActiveAlerts(`ALERTS{alertname!="PtahOperatorUnresolvedApply"}`) {
		a.fatalf("an unrelated incident is active before the negative-control window")
	}
	baselineLog, err := a.deploymentLog(a.ctx, "alert-sink")
	a.check(err, "read the original receiver journal")
	baselineDeliveries, err := alDeliveries(baselineLog)
	a.check(err, "decode the original receiver journal")
	baseline := map[string]alDelivery{}
	for _, d := range baselineDeliveries {
		if d.AlertName == alUnresolvedApply {
			baseline[d.Labels["family"]] = d
		}
	}
	if len(baseline) == 0 {
		a.fatalf("the preceding unresolved incident has no receiver evidence")
	}
	for _, d := range baseline {
		if d.Status != "firing" || d.Labels["operator_namespace"] != a.in.OperatorNamespace || d.Labels["operator_metrics_service"] != a.metricsService {
			a.fatalf("unresolved baseline does not name this installation's active incident")
		}
	}
	unresolved := a.negativeUnresolvedIdentity()
	lease, managers := a.managerSnapshot()
	monitor := a.negativeMonitorIdentity()
	for _, f := range fixtures {
		switch v := f.object.(type) {
		case *ptahv1.PtahSchema:
			storedStateWatchBarrier(a.t, a.ctx, a.cluster, schemas, v)
		case *ptahv1.PtahMigration:
			storedStateWatchBarrier(a.t, a.ctx, a.cluster, migrations, v)
		}
	}
	schemaStart, migrationStart := len(schemas.snapshot()), len(migrations.snapshot())
	a.logf("negative-control baseline receiver journal: %s", baselineLog)
	started := time.Now()
	end := started.Add(alNegativeWindow + alDetectionSlack)
	byUID := map[types.UID]*alNegativeFixtureRun{}
	lastRead := map[types.UID]time.Time{}
	for _, f := range fixtures {
		byUID[f.object.GetUID()] = f
	}
	checkFixture := func(f *alNegativeFixtureRun, object client.Object) {
		if !alNegativeReading(object).unchanged(f.initial) {
			a.fatalf("%s left its unchanged read-only policy control", object.GetName())
		}
	}
	checkReading := func(object client.Object) {
		if f := byUID[object.GetUID()]; f != nil {
			checkFixture(f, object)
		}
	}
	checkHistory := func() {
		for _, event := range schemas.snapshot()[schemaStart:] {
			checkReading(event.Object)
		}
		for _, event := range migrations.snapshot()[migrationStart:] {
			checkReading(event.Object)
		}
		for _, event := range jobs.snapshot() {
			job := event.Object
			owner := metav1.GetControllerOf(job)
			if owner != nil && byUID[owner.UID] != nil && job.Labels["operator.ptah.run/operation"] == "apply" {
				a.fatalf("a negative control dispatched an Apply")
			}
		}
	}
	for {
		for _, r := range []recorder{schemas, migrations, jobs} {
			a.check(r.alive(), "retain a complete negative-control watch")
		}
		checkHistory()
		for _, f := range fixtures {
			a.check(a.cluster.Client.Get(a.ctx, client.ObjectKeyFromObject(f.object), f.object), "read %s during its quiet window", f.object.GetName())
			checkFixture(f, f.object)
			reading := alNegativeReading(f.object)
			if !reading.readAt.Equal(lastRead[f.object.GetUID()]) {
				a.logf("negative-control native observation: family=%s resource=%s uid=%s generation=%d policy=%s observedAt=%s", reading.claim.family, reading.claim.name, reading.claim.uid, reading.claim.generation, reading.policy, reading.readAt)
				lastRead[f.object.GetUID()] = reading.readAt
			}

			if reading.readAt.IsZero() || time.Since(reading.readAt) > alNegativeFreshness {
				a.fatalf("%s stopped its normal observation cadence", f.object.GetName())
			}
		}
		currentLease, currentManagers := a.managerSnapshot()
		if !alSameManagers(currentLease, currentManagers, lease, managers) || !maps.Equal(monitor, a.negativeMonitorIdentity()) {
			a.fatalf("a manager or monitoring process changed during the quiet window")
		}
		body, err := a.prometheus(a.ctx, "/api/v1/targets", nil)
		a.check(err, "read every manager target during the quiet window")
		if !alTargetsReady(body, int(a.replicas)) {
			a.fatalf("a negative-control scrape target became unhealthy")
		}
		body, err = a.prometheus(a.ctx, "/api/v1/rules", nil)
		a.check(err, "read the complete rules during the quiet window")
		if !alRulesLoaded(body) {
			a.fatalf("a rule disappeared or stopped evaluating during the quiet window")
		}
		if !a.noActiveAlerts(`ALERTS{alertname!="PtahOperatorUnresolvedApply"}`) || !maps.Equal(unresolved, a.negativeUnresolvedIdentity()) {
			a.fatalf("a new incident appeared during ordinary policy waiting")
		}
		log, err := a.deploymentLog(a.ctx, "alert-sink")
		a.check(err, "retain the complete receiver journal")
		if !bytes.HasPrefix(log, baselineLog) {
			a.fatalf("the receiver journal lost its original prefix")
		}
		deliveries, err := alDeliveries(log)
		a.check(err, "decode the quiet-window receiver journal")
		for _, d := range deliveries[len(baselineDeliveries):] {
			if !alNegativeRepeat(d, baseline) {
				a.fatalf("ordinary policy waiting produced a new %s notification", d.AlertName)
			}
		}
		if !time.Now().Before(end) {
			break
		}
		a.sleep(min(alDeliveryPoll, time.Until(end)))
	}
	for _, f := range fixtures {
		if !alNegativeReading(f.object).readAt.After(started) {
			a.fatalf("%s has no fresh database observation inside the ten-minute window", f.object.GetName())
		}
		switch v := f.object.(type) {
		case *ptahv1.PtahSchema:
			storedStateWatchBarrier(a.t, a.ctx, a.cluster, schemas, v)
		case *ptahv1.PtahMigration:
			storedStateWatchBarrier(a.t, a.ctx, a.cluster, migrations, v)
		}
		count, err := databaseSQL(a.ctx, a.cluster, a.in.TestNamespace, engine, f.database, "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'")
		a.check(err, "read the isolated negative-control database")
		if strings.TrimSpace(count) != "0" {
			a.fatalf("%s changed its empty database without approval", f.object.GetName())
		}
	}
	// A retained unmanaged publisher supplies the Jobs collection's EOF barrier.
	existing := &batchv1.JobList{}
	a.check(a.cluster.Client.List(a.ctx, existing, client.InNamespace(a.in.TestNamespace)), "locate the Job watch sentinel")
	var sentinel *batchv1.Job
	for _, job := range existing.Items {
		if len(job.OwnerReferences) == 0 && job.Labels[labelManagedBy] != managedByOperator && job.Spec.TTLSecondsAfterFinished == nil && jobTerminal(&job) {
			sentinel = job.DeepCopy()
			break
		}
	}
	if sentinel == nil {
		a.fatalf("the negative-control Job watch has no retained publisher sentinel")
	}
	storedStateWatchBarrier(a.t, a.ctx, a.cluster, jobs, sentinel)
	closeRunnerWatches(a.t, []recorder{schemas, migrations, jobs}, scan)
	checkHistory()
	a.logf("PASS ordinary approval and Never policy controls: families=2 resources=%d started=%s ended=%s; normal refresh, no Apply, no new incident; pre-existing unresolved identities=%d", len(fixtures), started, time.Now(), len(unresolved))
}

// Include the operation identity, not merely the number of old incidents.
func (a *alertingRun) negativeUnresolvedIdentity() map[types.UID]string {
	schemas := &ptahv1.PtahSchemaList{}
	migrations := &ptahv1.PtahMigrationList{}
	a.check(a.cluster.Client.List(a.ctx, schemas), "read schema unresolved identities")
	a.check(a.cluster.Client.List(a.ctx, migrations), "read migration unresolved identities")
	result := map[types.UID]string{}
	for _, v := range schemas.Items {
		if v.Status.PendingObservation != nil {
			result[v.UID] = "schema:" + v.Status.PendingObservation.ApplyOperationID
		}
	}
	for _, v := range migrations.Items {
		if v.Status.UnresolvedRun != nil {
			result[v.UID] = "migration:" + v.Status.UnresolvedRun.OperationID
		}
	}
	return result
}

func (a *alertingRun) negativeMonitorIdentity() map[string]string {
	pods := &corev1.PodList{}
	a.check(a.cluster.Client.List(a.ctx, pods, client.InNamespace(alMonitoringNamespace)), "read monitoring process identities")
	result := map[string]string{}
	for _, pod := range pods.Items {
		name := pod.Labels["app"]
		if name != "prometheus" && name != "alertmanager" && name != "alert-sink" {
			continue
		}
		if result[name] != "" || pod.UID == "" || pod.DeletionTimestamp != nil || !harness.PodReady(&pod) || len(pod.Status.ContainerStatuses) != 1 || pod.Status.ContainerStatuses[0].State.Running == nil {
			a.fatalf("a monitoring process is missing, duplicated or unhealthy")
		}
		s := pod.Status.ContainerStatuses[0]
		result[name] = fmt.Sprintf("%s/%d/%s", pod.UID, s.RestartCount, s.State.Running.StartedAt.Time)
	}
	if len(result) != 3 {
		a.fatalf("the quiet window requires Prometheus, Alertmanager and the receiver")
	}
	return result
}
