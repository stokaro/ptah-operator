package admissionpolicy_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/internal/workload"
	"github.com/stokaro/ptah-operator/test/envtest/internal/policyenv"
)

// The guard on a deletion that would orphan an operation Job's Pods. The
// garbage collector would have to take the Job's owner reference off each
// Pod, which the Pod-intent webhook refuses, so the orphan finalizer would
// never come off. The guard refuses the deletion instead, for everyone: the
// propagationPolicy that asks for it, the legacy orphanDependents field, and a
// deletion that names no propagation, which batch/v1 orphans by default unless
// the Job carries the foregroundDeletion finalizer.
//
// The guard selects a Job by the identity the manager writes on it, so a Job
// the operator does not run is deleted as it always was, orphaning included.
//
// envtest runs no garbage collector and no Pod-intent webhook here, so these
// rows prove the verdict, which comes before either would act, and not what
// the deletion then does.
const (
	jobOrphanDeletionGuardPrefix = "ptah-operator-job-orphan-deletion-guard-"
	jobOrphanDeletionMessage     = "cannot orphan its Pods"
	jobDefaultOrphanMessage      = "names no propagation orphans"
)

// orphaningJobs are the Jobs the rows delete beside the cleanup fixtures'
// finished Apply Jobs: an operation Job in each state the registry reads, and
// Jobs the operator does not run, one of them a near miss on each label the
// guard reads.
type orphaningJobs struct {
	// carriesOrphan carries the orphan finalizer, as one a person put there
	// would; carriesForeground the foregroundDeletion one.
	carriesOrphan, carriesForeground *batchv1.Job
	// plain carries no label at all; otherManager the operation component
	// under another manager; otherComponent this operator with another
	// component.
	plain, otherManager, otherComponent *batchv1.Job
}

var orphaningFixtures orphaningJobs

// otherJob is a Job the operator does not run: suspended, so nothing would
// start it even where a Job controller ran.
func otherJob(name string, labels map[string]string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: tenantNamespace, Name: name, Labels: labels},
		Spec: batchv1.JobSpec{
			Suspend: ptr.To(true),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RestartPolicy:   corev1.RestartPolicyNever,
				SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
				Containers: []corev1.Container{{
					Name: "report", Image: "registry.example/report@" + digest("9"),
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: ptr.To(false),
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					},
				}},
			}},
		},
	}
}

func operationIdentity(component string) map[string]string {
	return map[string]string{workload.LabelManagedBy: "ptah-operator", workload.LabelComponent: component}
}

func setupOrphaningJobs(ctx context.Context) error {
	carriesOrphan, err := resolveJob()
	if err != nil {
		return err
	}
	carriesForeground, err := migrationResolveJob()
	if err != nil {
		return err
	}
	carriesOrphan = renamedJob(carriesOrphan, "ptah-resolve-", "orphaning")
	carriesOrphan.Finalizers = []string{metav1.FinalizerOrphanDependents}
	carriesForeground = renamedJob(carriesForeground, "ptah-m-resolve-", "foreground")
	carriesForeground.Finalizers = []string{metav1.FinalizerDeleteDependents}
	fixtures := orphaningJobs{
		carriesOrphan:     carriesOrphan,
		carriesForeground: carriesForeground,
		plain:             otherJob("nightly-report", nil),
		otherManager: otherJob("helm-migration-hook", map[string]string{
			workload.LabelManagedBy: "Helm", workload.LabelComponent: workload.ComponentMigrationOperation,
		}),
		otherComponent: otherJob("operator-crd-reconcile", map[string]string{
			workload.LabelManagedBy: "ptah-operator", workload.LabelComponent: "crd-manager",
		}),
	}
	for _, job := range []*batchv1.Job{
		fixtures.carriesOrphan, fixtures.carriesForeground, fixtures.plain, fixtures.otherManager, fixtures.otherComponent,
	} {
		if err := env.Admin.Create(ctx, job); err != nil {
			return fmt.Errorf("store Job %s: %w", job.Name, err)
		}
	}
	orphaningFixtures = fixtures
	return nil
}

func jobOrphanDeletionRows(t *testing.T, c *catalog) {
	guard := policy(t, jobOrphanDeletionGuardPrefix)
	user := policyenv.User()
	administrator := policyenv.Identity{Username: "envtest-cluster-administrator", Groups: []string{"system:masters"}}
	namespaceController := policyenv.ServiceAccount("kube-system", "namespace-controller")
	garbageCollector := policyenv.ServiceAccount("kube-system", "generic-garbage-collector")
	ttlController := policyenv.ServiceAccount("kube-system", "ttl-after-finished-controller")
	orphan := client.PropagationPolicy(metav1.DeletePropagationOrphan)
	background := client.PropagationPolicy(metav1.DeletePropagationBackground)
	foreground := client.PropagationPolicy(metav1.DeletePropagationForeground)
	schemaApply, migrationApply := cleanupFixtures.schemaApply, cleanupFixtures.migrationApply
	fixtures := orphaningFixtures

	const (
		migrationOrphanRow       = "ordinary user deletes a migration Apply Job with propagationPolicy Orphan"
		schemaOrphanRow          = "ordinary user deletes a schema Apply Job with propagationPolicy Orphan"
		migrationLegacyOrphanRow = "ordinary user deletes a migration Apply Job with orphanDependents"
		administratorOrphanRow   = "cluster administrator deletes a schema Apply Job with propagationPolicy Orphan"
		migrationDefaultRow      = "ordinary user deletes a migration Apply Job naming no propagation"
		schemaDefaultRow         = "ordinary user deletes a schema Apply Job naming no propagation"
		carriesOrphanDefaultRow  = "ordinary user deletes an operation Job that carries the orphan finalizer, naming no propagation"

		migrationBackgroundRow    = "ordinary user deletes a migration Apply Job with propagationPolicy Background"
		schemaBackgroundRow       = "ordinary user deletes a schema Apply Job with propagationPolicy Background"
		migrationForegroundRow    = "ordinary user deletes a migration Apply Job with propagationPolicy Foreground"
		schemaLegacyCascadeRow    = "ordinary user deletes a schema Apply Job with orphanDependents false"
		carriesOrphanClearedRow   = "ordinary user deletes an operation Job that carries the orphan finalizer with propagationPolicy Background"
		carriesForegroundRow      = "ordinary user deletes an operation Job that carries the foreground finalizer, naming no propagation"
		namespaceJobsRow          = "namespace controller deletes every Job in a namespace"
		garbageCollectorRow       = "garbage collector deletes an operation Job with propagationPolicy Background"
		ttlControllerRow          = "TTL controller deletes a finished operation Job with propagationPolicy Foreground"
		plainOrphanRow            = "ordinary user deletes a Job the operator does not run with propagationPolicy Orphan"
		plainDefaultRow           = "ordinary user deletes a Job the operator does not run, naming no propagation"
		otherManagerOrphanRow     = "ordinary user deletes a Job another manager labels as an operation with propagationPolicy Orphan"
		otherComponentOrphanRow   = "ordinary user deletes a Job of another operator component with propagationPolicy Orphan"
		propagationPolicyAlone    = `has(request.options.propagationPolicy) && request.options.propagationPolicy == "Orphan"`
		orphanDependentsAlone     = `has(request.options.orphanDependents) && request.options.orphanDependents`
		unrelatedOperationLiteral = `"another-operation"`
	)

	// Every way of asking for orphaned Pods is refused, the cluster
	// administrator's included, and so is a deletion that leaves the choice to
	// batch/v1's default, on a Job of either family.
	for _, row := range []struct {
		name    string
		as      policyenv.Identity
		object  client.Object
		options []client.DeleteOption
		message string
	}{
		{migrationOrphanRow, user, migrationApply, []client.DeleteOption{orphan}, jobOrphanDeletionMessage},
		{schemaOrphanRow, user, schemaApply, []client.DeleteOption{orphan}, jobOrphanDeletionMessage},
		{migrationLegacyOrphanRow, user, migrationApply, []client.DeleteOption{orphanDependents(true)}, jobOrphanDeletionMessage},
		{administratorOrphanRow, administrator, schemaApply, []client.DeleteOption{orphan}, jobOrphanDeletionMessage},
		{migrationDefaultRow, user, migrationApply, nil, jobDefaultOrphanMessage},
		{schemaDefaultRow, user, schemaApply, nil, jobDefaultOrphanMessage},
		{carriesOrphanDefaultRow, user, fixtures.carriesOrphan, nil, jobDefaultOrphanMessage},
	} {
		c.row(policyenv.Row{
			Name: row.name, Deny: []string{guard}, Message: row.message,
			Do: as(row.as, deleteStored(row.object, row.options...)),
		})
	}

	// Every other deletion of an operation Job goes through: the default
	// propagation kubectl sends and the foreground one, the legacy field set
	// false, a named propagation on a Job that carries the orphan finalizer,
	// which takes it off, a Job whose foreground finalizer decides for it, and
	// the controllers that delete Jobs, each naming a propagation.
	c.row(policyenv.Row{Name: migrationBackgroundRow, Do: as(user, deleteStored(migrationApply, background))})
	c.row(policyenv.Row{Name: schemaBackgroundRow, Do: as(user, deleteStored(schemaApply, background))})
	c.row(policyenv.Row{Name: migrationForegroundRow, Do: as(user, deleteStored(migrationApply, foreground))})
	c.row(policyenv.Row{Name: schemaLegacyCascadeRow, Do: as(user, deleteStored(schemaApply, orphanDependents(false)))})
	c.row(policyenv.Row{Name: carriesOrphanClearedRow, Do: as(user, deleteStored(fixtures.carriesOrphan, background))})
	c.row(policyenv.Row{Name: carriesForegroundRow, Do: as(user, deleteStored(fixtures.carriesForeground))})
	c.row(policyenv.Row{Name: namespaceJobsRow, Do: as(namespaceController, deleteNamespaceCollection(&batchv1.Job{}))})
	c.row(policyenv.Row{Name: garbageCollectorRow, Do: as(garbageCollector, deleteStored(schemaApply, background))})
	c.row(policyenv.Row{Name: ttlControllerRow, Do: func(ctx context.Context, env *policyenv.Env) error {
		uid := migrationApply.UID
		return as(ttlController, deleteStored(migrationApply, foreground, client.Preconditions{UID: &uid}))(ctx, env)
	}})

	// A Job the operator does not run is not the guard's to judge, however it
	// is deleted, and neither is one that carries only part of the identity.
	c.row(policyenv.Row{Name: plainOrphanRow, Do: as(user, deleteStored(fixtures.plain, orphan))})
	c.row(policyenv.Row{Name: plainDefaultRow, Do: as(user, deleteStored(fixtures.plain))})
	c.row(policyenv.Row{Name: otherManagerOrphanRow, Do: as(user, deleteStored(fixtures.otherManager, orphan))})
	c.row(policyenv.Row{Name: otherComponentOrphanRow, Do: as(user, deleteStored(fixtures.otherComponent, orphan))})

	refused := []string{
		migrationOrphanRow, schemaOrphanRow, migrationLegacyOrphanRow, administratorOrphanRow,
		migrationDefaultRow, schemaDefaultRow, carriesOrphanDefaultRow,
	}
	namedPropagation := []string{
		migrationBackgroundRow, schemaBackgroundRow, migrationForegroundRow, schemaLegacyCascadeRow,
		carriesOrphanClearedRow, namespaceJobsRow, garbageCollectorRow, ttlControllerRow,
	}
	admitted := append(slices.Clone(namedPropagation), carriesForegroundRow)
	other := []string{plainOrphanRow, plainDefaultRow, otherManagerOrphanRow, otherComponentOrphanRow}

	c.mutation(policyenv.Mutation{
		Name: "job orphan-deletion guard binding dropped", Policies: []string{guard},
		Apply: policyenv.DropBinding(guard), Breaks: refused,
	})
	// Without its condition the guard judges every Job, and refuses the
	// operator's neighbors what it refuses the operation Jobs.
	c.mutation(policyenv.Mutation{
		Name: "job orphan-deletion guard judges every Job", Policies: []string{guard},
		Apply: policyenv.WidenMatch(guard), Breaks: other,
	})
	c.mutation(policyenv.Mutation{
		Name: "job orphan-deletion guard refuses what it matches", Policies: []string{guard},
		Apply: policyenv.RefuseEverything(guard), Breaks: admitted,
	})
	// Each way of asking is read on its own: a guard that reads only one of the
	// two fields admits what the other one asks for.
	c.mutation(policyenv.Mutation{
		Name: "job orphan-deletion guard reads no orphanDependents", Policies: []string{guard},
		Apply:  policyenv.SetVariables(guard, map[string]string{"orphanRequested": propagationPolicyAlone}),
		Breaks: []string{migrationLegacyOrphanRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "job orphan-deletion guard reads no propagationPolicy", Policies: []string{guard},
		Apply:  policyenv.SetVariables(guard, map[string]string{"orphanRequested": orphanDependentsAlone}),
		Breaks: []string{migrationOrphanRow, schemaOrphanRow, administratorOrphanRow}, Admits: true,
	})
	// The default is what refuses a deletion that names nothing: read as named
	// everywhere, the guard admits those, and read as named nowhere it refuses
	// every named propagation that should have decided.
	c.mutation(policyenv.Mutation{
		Name: "job orphan-deletion guard reads every deletion as naming a propagation", Policies: []string{guard},
		Apply:  policyenv.SetVariables(guard, map[string]string{"propagationNamed": "true"}),
		Breaks: []string{migrationDefaultRow, schemaDefaultRow, carriesOrphanDefaultRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "job orphan-deletion guard reads no named propagation", Policies: []string{guard},
		Apply:  policyenv.SetVariables(guard, map[string]string{"propagationNamed": "false"}),
		Breaks: namedPropagation,
	})
	c.mutation(policyenv.Mutation{
		Name: "job orphan-deletion guard reads no foreground finalizer", Policies: []string{guard},
		Apply:  policyenv.SetVariables(guard, map[string]string{"foregroundFinalizer": "false"}),
		Breaks: []string{carriesForegroundRow},
	})
	// Each literal of the identity selects its Jobs: a condition that reads
	// another manager, or another value for a family's component, stops
	// judging that family.
	c.mutation(policyenv.Mutation{
		Name: "job orphan-deletion guard reads another manager", Policies: []string{guard},
		Apply:  policyenv.RewriteMatchConditions(guard, `== "ptah-operator"`, `== "another-manager"`),
		Breaks: refused, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "job orphan-deletion guard reads another migration component", Policies: []string{guard},
		Apply:  policyenv.RewriteMatchConditions(guard, `"`+workload.ComponentMigrationOperation+`"`, unrelatedOperationLiteral),
		Breaks: []string{migrationOrphanRow, migrationLegacyOrphanRow, migrationDefaultRow}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "job orphan-deletion guard reads another schema component", Policies: []string{guard},
		Apply:  policyenv.RewriteMatchConditions(guard, `"`+workload.ComponentSchemaOperation+`"`, unrelatedOperationLiteral),
		Breaks: []string{schemaOrphanRow, administratorOrphanRow, schemaDefaultRow, carriesOrphanDefaultRow}, Admits: true,
	})
}

// TestJobOrphanDeletionGuardOnStoredJobs sends the deletions the rows send as
// dry runs for real, and reads what each one left: a refused one leaves the Job
// as it was stored, and a named propagation releases a Job an orphaning
// deletion left Terminating before the guard was installed. It also measures
// the premise the guard's second rule rests on: batch/v1 orphans by default,
// so a deletion that names no propagation puts the orphan finalizer on a Job.
func TestJobOrphanDeletionGuardOnStoredJobs(t *testing.T) {
	plane.Require(t)
	ctx := context.Background()
	guard := policy(t, jobOrphanDeletionGuardPrefix)
	user := policyenv.User()
	compiled := policyenv.Row{
		Name: "the guard refuses an orphaning deletion", Deny: []string{guard}, Message: jobOrphanDeletionMessage,
		Do: as(user, deleteStored(cleanupFixtures.schemaApply, client.PropagationPolicy(metav1.DeletePropagationOrphan))),
	}
	if err := env.WaitFor(ctx, []policyenv.Row{compiled}, time.Minute); err != nil {
		t.Fatalf("the API server did not compile the guard: %v", err)
	}
	deleteAs := func(identity policyenv.Identity, job *batchv1.Job, options ...client.DeleteOption) func(context.Context, *policyenv.Env) error {
		return as(identity, func(ctx context.Context, api client.Client) error {
			return api.Delete(ctx, job.DeepCopy(), options...)
		})
	}

	t.Run("a refused deletion leaves the Job as it was", func(t *testing.T) {
		job := otherJob("premise-refused", operationIdentity(workload.ComponentMigrationOperation))
		if err := env.Admin.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := env.Admin.Delete(context.Background(), job.DeepCopy(),
				client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
				t.Errorf("remove Job %s: %v", job.Name, err)
			}
		})
		for _, row := range []policyenv.Row{
			{Name: "Orphan", Message: jobOrphanDeletionMessage, Do: deleteAs(user, job, client.PropagationPolicy(metav1.DeletePropagationOrphan))},
			{Name: "orphanDependents", Message: jobOrphanDeletionMessage, Do: deleteAs(user, job, orphanDependents(true))},
			{Name: "no propagation", Message: jobDefaultOrphanMessage, Do: deleteAs(user, job)},
		} {
			row.Deny = []string{guard}
			if err := env.Check(ctx, row); err != nil {
				t.Fatal(err)
			}
		}
		after, err := stored(ctx, job)
		if err != nil {
			t.Fatal(err)
		}
		if after.ResourceVersion != job.ResourceVersion || after.DeletionTimestamp != nil || len(after.Finalizers) != 0 {
			t.Fatalf("a refused deletion changed Job %s: resourceVersion %s, was %s; deletionTimestamp %v; finalizers %v",
				job.Name, after.ResourceVersion, job.ResourceVersion, after.DeletionTimestamp, after.Finalizers)
		}
	})

	t.Run("a named propagation releases a Job an orphaning deletion left Terminating", func(t *testing.T) {
		// Stored without the identity, so the guard does not judge the
		// deletion that orphans it: that is the state a deletion before the
		// guard existed left.
		job := otherJob("premise-orphaned", nil)
		if err := env.Admin.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := env.Admin.Delete(context.Background(), job.DeepCopy(),
				client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
				t.Errorf("remove Job %s: %v", job.Name, err)
			}
		})
		if err := env.Admin.Delete(ctx, job.DeepCopy()); err != nil {
			t.Fatalf("delete Job %s naming no propagation: %v", job.Name, err)
		}
		orphaned, err := stored(ctx, job)
		if err != nil {
			t.Fatalf("a deletion that names no propagation removed Job %s at once, so batch/v1 does not orphan by default: %v", job.Name, err)
		}
		if orphaned.DeletionTimestamp == nil || !slices.Equal(orphaned.Finalizers, []string{metav1.FinalizerOrphanDependents}) {
			t.Fatalf("a deletion that names no propagation left Job %s with deletionTimestamp %v and finalizers %v, want the orphan finalizer alone",
				job.Name, orphaned.DeletionTimestamp, orphaned.Finalizers)
		}

		labeled := orphaned.DeepCopy()
		if labeled.Labels == nil {
			labeled.Labels = map[string]string{}
		}
		for key, value := range operationIdentity(workload.ComponentSchemaOperation) {
			labeled.Labels[key] = value
		}
		if err := env.Admin.Patch(ctx, labeled, client.MergeFrom(orphaned)); err != nil {
			t.Fatalf("give the orphaned Job %s the operation identity: %v", job.Name, err)
		}
		if err := env.Check(ctx, policyenv.Row{
			Name: "no propagation on an orphaned operation Job", Deny: []string{guard}, Message: jobDefaultOrphanMessage,
			Do: deleteAs(user, labeled),
		}); err != nil {
			t.Fatal(err)
		}
		if err := env.Check(ctx, policyenv.Row{
			Name: "Background on an orphaned operation Job", Do: deleteAs(user, labeled, client.PropagationPolicy(metav1.DeletePropagationBackground)),
		}); err != nil {
			t.Fatal(err)
		}
		if left, err := stored(ctx, job); !apierrors.IsNotFound(err) {
			t.Fatalf("Job %s is still stored after a Background deletion took its orphan finalizer off: deletionTimestamp %v, finalizers %v, error %v",
				job.Name, left.DeletionTimestamp, left.Finalizers, err)
		}
	})
}
