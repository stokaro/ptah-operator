//go:build e2e

package e2e

import (
	"context"
	"net/url"
	"time"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/crdupgrade"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
	"github.com/stokaro/ptah-operator/test/e2e/phases"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The four consumers have separate databases and never receive permission to
// apply. Collection histories cover their creation, both upgrades and recovery.
func (a *alertingRun) upgradeProbes(evidence string) ([]client.Object, func(), func(time.Time) time.Time, func()) {
	schema, migration := &ptahv1.PtahSchema{}, &ptahv1.PtahMigration{}
	a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: a.in.TestNamespace, Name: "e2e-reference-postgresql"}, schema), "read the native schema producer")
	a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: a.in.TestNamespace, Name: "e2e-migrations-postgresql"}, migration), "read the native migration producer")
	registry, err := url.Parse(migration.Spec.Artifact.OCIRef)
	a.check(err, "read the artifact registry")
	if registry.Scheme != "oci" || registry.Host == "" || migration.Status.ExecutionBinding == nil {
		a.fatalf("upgrade probes have no native producer")
	}
	watcher, err := client.NewWithWatch(a.cluster.Config, client.Options{Scheme: a.cluster.Scheme})
	a.check(err, "open pre-creation probe histories")
	schemas := newStoredStateRecorder[*ptahv1.PtahSchema](a.t, a.ctx, watcher, "upgrade-probe-schemas", a.in.TestNamespace, func() client.ObjectList { return &ptahv1.PtahSchemaList{} })
	migrations := newStoredStateRecorder[*ptahv1.PtahMigration](a.t, a.ctx, watcher, "upgrade-probe-migrations", a.in.TestNamespace, func() client.ObjectList { return &ptahv1.PtahMigrationList{} })
	jobs := newStoredStateRecorder[*batchv1.Job](a.t, a.ctx, watcher, "upgrade-probe-jobs", a.in.TestNamespace, func() client.ObjectList { return &batchv1.JobList{} })
	var probes []client.Object
	var auxiliaries []client.Object
	type database struct {
		run  *migrationRun
		name string
	}
	var databases []database
	originals := map[types.UID]client.Object{}
	checkHistory := func() {
		check := func(v client.Object) {
			if original := originals[v.GetUID()]; original != nil && !alUpgradeProbeSafe(v, original) {
				a.fatalf("upgrade probe %s changed or performed mutation work", v.GetName())
			}
		}
		for _, event := range schemas.snapshot() {
			check(event.Object)
		}
		for _, event := range migrations.snapshot() {
			check(event.Object)
		}
		for _, event := range jobs.snapshot() {
			j := event.Object
			owner := metav1.GetControllerOf(j)
			if owner != nil && originals[owner.UID] != nil && j.Labels[labelOperation] == "apply" {
				a.fatalf("upgrade dispatched an Apply for a Never probe")
			}
		}
	}
	check := func() {
		for _, r := range []recorder{schemas, migrations, jobs} {
			a.check(r.alive(), "retain every upgrade probe event")
		}
		checkHistory()
	}
	cleanup := func() {
		if a.t.Failed() {
			return
		}
		// A metadata barrier is safe after runtime recovery and closes each resource
		// history at an exact version without changing its spec or generation.
		for _, v := range probes {
			switch v := v.(type) {
			case *ptahv1.PtahSchema:
				storedStateWatchBarrier(a.t, a.ctx, a.cluster, schemas, v)
			case *ptahv1.PtahMigration:
				storedStateWatchBarrier(a.t, a.ctx, a.cluster, migrations, v)
			}
		}
		for _, r := range []recorder{schemas, migrations, jobs} {
			r.requestStop()
		}
		for _, r := range []recorder{schemas, migrations, jobs} {
			a.check(r.await(45*time.Second), "close upgrade probe history")
		}
		checkHistory()
		for uid, original := range originals {
			seen, readJob := false, false
			for _, e := range schemas.snapshot() {
				seen = seen || e.Object.UID == uid
			}
			for _, e := range migrations.snapshot() {
				seen = seen || e.Object.UID == uid
			}
			for _, e := range jobs.snapshot() {
				owner := metav1.GetControllerOf(e.Object)
				readJob = readJob || owner != nil && owner.UID == uid && e.Object.Labels[labelOperation] != "apply"
			}
			if !seen || !readJob {
				a.fatalf("probe %s has no nonempty resource and read-workload history", original.GetName())
			}
		}
		// Save the closed histories before removing their source objects. These
		// are the documents used to reject Apply and date post-hook recovery.
		a.retainUpgradeEvidence(evidence, "probe-originals.json", mustJSONBytes(originals))
		a.retainUpgradeEvidence(evidence, "probe-schemas.json", mustJSONBytes(schemas.snapshot()))
		a.retainUpgradeEvidence(evidence, "probe-migrations.json", mustJSONBytes(migrations.snapshot()))
		a.retainUpgradeEvidence(evidence, "probe-jobs.json", mustJSONBytes(jobs.snapshot()))
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		for _, v := range probes {
			a.check(storedStateDeleteExact(ctx, a.cluster, v), "finalize the owned upgrade probe")
		}
		for _, v := range auxiliaries {
			a.check(storedStateDeleteExact(ctx, a.cluster, v), "remove the owned upgrade fixture")
		}
		for _, db := range databases {
			db.run.dropDatabase(db.name)
		}
	}
	// Register before creating resources so partial failures retain their exact
	// state for diagnostics; the enclosing driver owns cluster teardown.
	a.t.Cleanup(func() {
		if a.t.Failed() {
			a.logf("upgrade probe fixtures retained for failed-cluster diagnostics")
		}
	})
	for _, engineName := range []string{"postgresql", "mysql"} {
		engine, err := migrationEngineFor(engineName)
		a.check(err, "select the upgrade probe engine")
		m := &migrationRun{t: a.t, parent: a.t, ctx: a.ctx, cluster: a.cluster, engine: engine, workDir: a.workDir, registryHost: registry.Host, repository: "e2e-alert-upgrade", in: phases.MigrationsInputs{TestNamespace: a.in.TestNamespace, ExecutorImage: migration.Status.ExecutionBinding.ExecutorImage}}
		credential := &corev1.Secret{}
		a.check(m.get(engine.sourceSecret, credential), "read the prepared engine credential")
		m.password = string(credential.Data["password"])
		if m.password == "" {
			a.fatalf("upgrade probe has no engine credential")
		}
		m.protect(m.password, a.credentials.Password)
		digest := m.publish("alerts-upgrade", m.fixtureDir(""), m.reference(""))
		for _, v := range []client.Object{&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: a.in.TestNamespace, Name: "e2e-push-migrations-" + engineName + "-alerts-upgrade"}}, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: a.in.TestNamespace, Name: "e2e-migrations-" + engineName + "-alerts-upgrade"}}} {
			a.check(a.cluster.Client.Get(a.ctx, client.ObjectKeyFromObject(v), v), "retain the migration publisher identity")
			auxiliaries = append(auxiliaries, v)
		}
		for _, template := range []client.Object{schema, migration} {
			family := alStalledReading(template).family
			name, db := "e2e-upgrade-"+family+"-"+engineName, "ptah_upgrade_"+family+"_"+engineName
			m.isolatedDatabase(db, name+"-db")
			databases = append(databases, database{m, db})
			secret := &corev1.Secret{}
			a.check(m.get(name+"-db", secret), "retain upgrade target identity")
			auxiliaries = append(auxiliaries, secret)
			object, err := alNegativeFixture(template, name, secret.Name, ptahv1.ApplyPolicyNever)
			a.check(err, "build the Never upgrade probe")
			switch v := object.(type) {
			case *ptahv1.PtahSchema:
				ref, publisher, source := a.publishUnresolvedSchema(m, name)
				auxiliaries = append(auxiliaries, publisher, source)
				v.Spec.Desired.OCIRef = ref
				v.Spec.Target.URLFrom.Key = "url"
				v.Spec.Target.Engine = ptahv1.DatabaseEngine(engine.kind)
				v.Spec.Target.CoordinationKey = "e2e/upgrade/" + family + "/" + engineName
				v.Spec.Execution.NodeSelector = nil
				v.Spec.Interval.Duration = 15 * time.Second
			case *ptahv1.PtahMigration:
				v.Spec.Artifact.OCIRef = m.reference("") + "@" + digest
				v.Spec.Target.URLFrom.Key = "url"
				v.Spec.Target.Engine = ptahv1.DatabaseEngine(engine.kind)
				v.Spec.Target.CoordinationKey = "e2e/upgrade/" + family + "/" + engineName
				v.Spec.Execution.NodeSelector = nil
				v.Spec.Interval.Duration = 15 * time.Second
			}
			a.check(a.create(object), "create the native upgrade probe")
			originals[object.GetUID()] = object.DeepCopyObject().(client.Object)
			probes = append(probes, object)
			a.check(harness.Wait(a.ctx, "a planned read-only upgrade probe", alTimeout, time.Second, func(context.Context) (bool, string, error) {
				check()
				current := object.DeepCopyObject().(client.Object)
				if err := a.cluster.Client.Get(a.ctx, client.ObjectKeyFromObject(current), current); err != nil {
					return false, "", err
				}
				_, ok := alUpgradeProbeProgress(current, originals[object.GetUID()], time.Time{})
				return ok, "waiting for fresh read-only policy boundary", nil
			}), "prepare %s", name)
		}
	}
	recovery := func(after time.Time) time.Time {
		check()
		latest := after
		for uid, original := range originals {
			var first time.Time
			accept := func(v client.Object) {
				if v.GetUID() != uid {
					return
				}
				at, ok := alUpgradeProbeProgress(v, original, after)
				if ok && (first.IsZero() || at.Before(first)) {
					first = at
				}
			}
			for _, e := range schemas.snapshot() {
				accept(e.Object)
			}
			for _, e := range migrations.snapshot() {
				accept(e.Object)
			}
			if first.IsZero() {
				a.fatalf("probe %s has no retained post-hook recovery", original.GetName())
			}
			if first.After(latest) {
				latest = first
			}
		}
		return latest
	}
	return probes, check, recovery, cleanup
}

// Runtime conditions carry the event times. Re-reading healthy objects must
// never move the receiver deadline to the time this polling loop noticed them.
func (a *alertingRun) upgradeRuntimeBoundary(intent alUpgradeIntent, probes []client.Object, after time.Time, retain func(string, []byte)) time.Time {
	boundary := after
	for name, want := range intent.CRDDigests {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		a.check(a.cluster.Client.Get(a.ctx, client.ObjectKey{Name: name}, crd), "read recovered candidate CRD")
		retain("recovery-crd-"+name+".json", mustJSONBytes(crd))
		digest, err := crdupgrade.ComputeSchemaDigest(crd)
		a.check(err, "verify recovered schema bytes")
		established, names := false, false
		for _, c := range crd.Status.Conditions {
			established = established || c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue
			names = names || c.Type == apiextensionsv1.NamesAccepted && c.Status == apiextensionsv1.ConditionTrue
		}
		if digest != want || crd.Annotations[crdupgrade.SchemaDigestAnnotation] != want || !established || !names || crd.DeletionTimestamp != nil {
			a.fatalf("retry did not establish the exact candidate CRD")
		}
	}
	for _, name := range []string{intent.Manager, intent.Rotator} {
		d := &appsv1.Deployment{}
		a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: intent.Namespace, Name: name}, d), "read recovered runtime")
		retain("recovery-deployment-"+name+".json", mustJSONBytes(d))
		if d.Spec.Replicas == nil || *d.Spec.Replicas < 1 || d.Status.ObservedGeneration != d.Generation || d.Status.AvailableReplicas != *d.Spec.Replicas || d.Status.UpdatedReplicas != *d.Spec.Replicas || d.Status.Replicas != *d.Spec.Replicas || d.Spec.Selector == nil || len(d.Spec.Template.Spec.Containers) != 1 || d.Spec.Template.Spec.Containers[0].Image != intent.Image {
			a.fatalf("retry runtime is not the available candidate")
		}
		selector, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
		a.check(err, "read runtime selector")
		if selector.Empty() {
			a.fatalf("runtime selector is empty")
		}
		pods := &corev1.PodList{}
		a.check(a.cluster.Client.List(a.ctx, pods, client.InNamespace(intent.Namespace), client.MatchingLabelsSelector{Selector: selector}), "read all recovered runtime Pods")
		retain("recovery-pods-"+name+".json", mustJSONBytes(pods))
		if len(pods.Items) != int(*d.Spec.Replicas) {
			a.fatalf("retry runtime has incomplete Pod inventory")
		}
		for _, pod := range pods.Items {
			owner := metav1.GetControllerOf(&pod)
			if owner == nil || owner.Kind != "ReplicaSet" {
				a.fatalf("candidate Pod has no owning ReplicaSet")
			}
			rs := &appsv1.ReplicaSet{}
			a.check(a.cluster.Client.Get(a.ctx, types.NamespacedName{Namespace: intent.Namespace, Name: owner.Name}, rs), "read candidate Pod lineage")
			retain("recovery-lineage-"+pod.Name+".json", mustJSONBytes(rs))
			parent := metav1.GetControllerOf(rs)
			if parent == nil || parent.UID != d.UID || parent.Kind != "Deployment" || rs.UID != owner.UID || !harness.PodReady(&pod) || pod.DeletionTimestamp != nil || len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Image != intent.Image {
				a.fatalf("runtime Pod is not owned and ready on this candidate")
			}
			for _, c := range pod.Status.Conditions {
				if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
					if c.LastTransitionTime.IsZero() || !c.LastTransitionTime.After(after) {
						a.fatalf("runtime lacks a fresh native Ready transition")
					}
					if c.LastTransitionTime.After(boundary) {
						boundary = c.LastTransitionTime.Time
					}
				}
			}
		}
	}
	for _, probe := range probes {
		live := probe.DeepCopyObject().(client.Object)
		a.check(a.cluster.Client.Get(a.ctx, client.ObjectKeyFromObject(probe), live), "read original recovered probe")
		if !alUpgradeProbeSafe(live, probe) {
			a.fatalf("upgrade changed the read-only probe")
		}
		retain("recovery-probe-"+probe.GetName()+".json", mustJSONBytes(live))
		before := live.DeepCopyObject().(client.Object)
		annotations := live.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations["qualification.ptah.run/native-upgrade-admission"] = "verified"
		live.SetAnnotations(annotations)
		a.check(a.cluster.Client.Patch(a.ctx, live, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}), client.DryRunAll), "verify actual recovered admission")
		retain("recovery-admission-"+probe.GetName()+".json", mustJSONBytes(live))
	}
	return boundary
}
