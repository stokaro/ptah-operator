package admissionpolicy_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/workload"
	"github.com/stokaro/ptah-operator/test/envtest/internal/policyenv"
)

const (
	cleanupTTLValueMessage = "a Job's cleanup TTL is 300 seconds, or 86400 seconds for the Job of an Apply that failed"
	cleanupTTLLongMessage  = "only an Apply Job keeps 86400 seconds"
)

// cleanupJobs are finished Jobs the rows send a cleanup TTL to, one of each kind
// the guard tells apart. Nothing here runs them, so the administrator stores each
// with the status the Job controller would have written.
type cleanupJobs struct {
	readOnly, migrationReadOnly, schemaApply, migrationApply, alreadyHarvested *batchv1.Job
}

var cleanupFixtures cleanupJobs

// asApplyJob relabels a built Resolve Job as the Apply of its resource. The
// guard reads the operation from the label and the Job's name from it, and holds
// an Apply to the annotations its builder writes, which is all this needs of one.
func asApplyJob(job *batchv1.Job, namePrefix, built, word string) *batchv1.Job {
	const operationLabel = "operator.ptah.run/operation"
	job.Name = namePrefix + "apply-" + word + "-" + strings.TrimPrefix(job.Name, namePrefix+built+"-")
	job.Labels[operationLabel] = "apply"
	job.Spec.Template.Labels[operationLabel] = "apply"
	for _, annotations := range []map[string]string{job.Annotations, job.Spec.Template.Annotations} {
		workload.MarkMutatingOperation(annotations)
		if namePrefix == "ptah-" {
			annotations[workload.AnnotationPlanFingerprint] = digest("e")
			annotations[workload.AnnotationPlanContentDigest] = digest("f")
		}
	}
	return job
}

// renamedJob gives a built Job a name of its own in the form the guard reads, so
// that the rows' Jobs do not share the one name the builder derives from the
// claim.
func renamedJob(job *batchv1.Job, prefix, word string) *batchv1.Job {
	job.Name = prefix + word + "-" + strings.TrimPrefix(job.Name, prefix)
	return job
}

// migrationHistoryJob is the migration family's history read, which the guard
// holds to the same containers an Apply carries: the fetch, and the database URL.
func migrationHistoryJob() (*batchv1.Job, error) {
	migration := tenantMigration.DeepCopy()
	operation := operatorv1alpha1.MigrationOperationStatus{
		Type:               operatorv1alpha1.MigrationOperationHistory,
		ID:                 digest("8"),
		InputFingerprint:   digest("a"),
		ExecutionBindingID: executionBindingID,
		Attempt:            1,
		StartedAt:          metav1.NewTime(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)),
		CoordinationDigest: digest("6"),
		AdmissionSnapshot:  &operatorv1alpha1.PodAdmissionSnapshot{Digest: digest("c"), TemplateDigest: digest("d")},
		Source: &operatorv1alpha1.SourceAccessBinding{
			ResolvedReference: "oci://registry.example/acme/ledger-migrations@" + digest("4"),
			Digest:            digest("4"),
		},
		Target: &operatorv1alpha1.DatabaseTargetBinding{
			Engine: operatorv1alpha1.DatabaseEnginePostgreSQL, URLFrom: migration.Spec.Target.URLFrom,
		},
	}
	name, err := workload.NameForMigration(migration, operation)
	if err != nil {
		return nil, err
	}
	operation.JobName = name
	return managerBuilder().BuildMigration(migration, operation, nil)
}

func setupTenantAndCleanupJobs(ctx context.Context) error {
	if err := setupTenant(ctx); err != nil {
		return err
	}
	return setupCleanupJobs(ctx)
}

func setupCleanupJobs(ctx context.Context) error {
	build := func(job *batchv1.Job, err error) (*batchv1.Job, error) { return job, err }
	readOnly, err := build(resolveJob())
	if err != nil {
		return err
	}
	harvested, err := build(resolveJob())
	if err != nil {
		return err
	}
	schemaApply, err := build(resolveJob())
	if err != nil {
		return err
	}
	migrationReadOnly, err := build(migrationResolveJob())
	if err != nil {
		return err
	}
	migrationApply, err := build(migrationHistoryJob())
	if err != nil {
		return err
	}
	cleanupFixtures = cleanupJobs{
		readOnly:          renamedJob(readOnly, "ptah-resolve-", "cleanup"),
		migrationReadOnly: renamedJob(migrationReadOnly, "ptah-m-resolve-", "cleanup"),
		schemaApply:       asApplyJob(schemaApply, "ptah-", "resolve", "cleanup"),
		migrationApply:    asApplyJob(migrationApply, "ptah-m-", "history", "cleanup"),
		alreadyHarvested:  asApplyJob(harvested, "ptah-", "resolve", "harvested"),
	}
	cleanupFixtures.alreadyHarvested.Spec.TTLSecondsAfterFinished = ptr.To(int32(300))
	start := metav1.NewTime(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	finish := metav1.NewTime(start.Add(time.Minute))
	for _, job := range []*batchv1.Job{
		cleanupFixtures.readOnly, cleanupFixtures.migrationReadOnly, cleanupFixtures.schemaApply,
		cleanupFixtures.migrationApply, cleanupFixtures.alreadyHarvested,
	} {
		if err := env.Admin.Create(ctx, job); err != nil {
			return fmt.Errorf("store Job %s: %w", job.Name, err)
		}
		job.Status = batchv1.JobStatus{
			StartTime: &start, CompletionTime: &finish, Succeeded: 1,
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: finish},
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: finish},
			},
		}
		if err := env.Admin.Status().Update(ctx, job); err != nil {
			return fmt.Errorf("finish Job %s: %w", job.Name, err)
		}
	}
	return nil
}

// cleanupRows sends the manager's one write to a finished Job. The guard names
// which TTL is allowed: a value that is neither of the two, and the long one on a
// Job that is not an Apply, are refused by what they get wrong. It cannot refuse
// the long TTL on an Apply that succeeded, because a failed Apply usually leaves
// a Complete Job as well. The webhook reads that from the Pod's summary, and
// test/envtest/webhook holds it there.
func cleanupRows(t *testing.T, c *catalog) {
	jobGuard := policy(t, "ptah-operator-job-write-guard-")
	manager := env.Manager()
	setTTL := func(job func() *batchv1.Job, seconds int32) func(context.Context, *policyenv.Env) error {
		return func(ctx context.Context, env *policyenv.Env) error {
			return as(manager, patchStored(job(), func(stored *batchv1.Job) {
				stored.Spec.TTLSecondsAfterFinished = ptr.To(seconds)
			}))(ctx, env)
		}
	}
	readOnly := func() *batchv1.Job { return cleanupFixtures.readOnly }
	migrationReadOnly := func() *batchv1.Job { return cleanupFixtures.migrationReadOnly }
	schemaApply := func() *batchv1.Job { return cleanupFixtures.schemaApply }
	migrationApply := func() *batchv1.Job { return cleanupFixtures.migrationApply }
	harvested := func() *batchv1.Job { return cleanupFixtures.alreadyHarvested }

	const (
		readOnlyShort      = "manager gives a finished read-only Job the short cleanup TTL"
		schemaApplyShort   = "manager gives a finished schema Apply Job the short cleanup TTL"
		schemaApplyLong    = "manager keeps a finished schema Apply Job for 24 hours"
		migrationApplyLong = "manager keeps a finished migration Apply Job for 24 hours"
		readOnlyLong       = "manager keeps a finished schema read-only Job for 24 hours"
		migrationLong      = "manager keeps a finished migration read-only Job for 24 hours"
		neitherValue       = "manager gives a finished Apply Job a cleanup TTL that is neither value"
		overTheLongOne     = "manager gives a finished Apply Job a cleanup TTL past the long one"
		alreadyHarvested   = "manager moves a Job a previous release harvested to the long cleanup TTL"
	)
	// No policy judges a person deleting a Job, which is what the security page tells
	// someone to do once they have read a failed Apply's log.
	c.row(policyenv.Row{Name: "a person deletes a finished Apply Job", Do: func(ctx context.Context, env *policyenv.Env) error {
		return as(policyenv.User(), dryRunDelete(schemaApply()))(ctx, env)
	}})
	c.row(policyenv.Row{Name: readOnlyShort, Do: setTTL(readOnly, 300)})
	c.row(policyenv.Row{Name: schemaApplyShort, Do: setTTL(schemaApply, 300)})
	c.row(policyenv.Row{Name: schemaApplyLong, Do: setTTL(schemaApply, 86400)})
	c.row(policyenv.Row{Name: migrationApplyLong, Do: setTTL(migrationApply, 86400)})
	c.row(policyenv.Row{Name: readOnlyLong, Deny: []string{jobGuard}, Message: cleanupTTLLongMessage, Do: setTTL(readOnly, 86400)})
	c.row(policyenv.Row{Name: migrationLong, Deny: []string{jobGuard}, Message: cleanupTTLLongMessage, Do: setTTL(migrationReadOnly, 86400)})
	c.row(policyenv.Row{Name: neitherValue, Deny: []string{jobGuard}, Message: cleanupTTLValueMessage, Do: setTTL(schemaApply, 600)})
	c.row(policyenv.Row{Name: overTheLongOne, Deny: []string{jobGuard}, Message: cleanupTTLValueMessage, Do: setTTL(schemaApply, 86401)})
	c.row(policyenv.Row{
		Name: alreadyHarvested, Deny: []string{jobGuard}, Message: "rejected an unsafe workload shape",
		Do: setTTL(harvested, 86400),
	})

	c.mutation(policyenv.Mutation{
		Name: "Job write guard admits any cleanup TTL", Policies: []string{jobGuard},
		Apply:  policyenv.DropValidations(jobGuard, "a Job's cleanup TTL is"),
		Breaks: []string{neitherValue, overTheLongOne}, Admits: true,
	})
	c.mutation(policyenv.Mutation{
		Name: "Job write guard lets a read-only Job keep the long cleanup TTL", Policies: []string{jobGuard},
		Apply:  policyenv.DropValidations(jobGuard, cleanupTTLLongMessage),
		Breaks: []string{readOnlyLong, migrationLong}, Admits: true,
	})
}

// TestARefusedCleanupTTLIsOneTheManagerFallsBackFrom reads the error a real API
// server returns when the guard refuses the long TTL. The manager falls back to
// the short one on that class of error and on no other, which is what keeps a
// failed Apply settling while the previous release's guard is still installed.
func TestARefusedCleanupTTLIsOneTheManagerFallsBackFrom(t *testing.T) {
	plane.Require(t)
	ctx := context.Background()
	if err := env.WaitFor(ctx, buildCatalog(t).rows, time.Minute); err != nil {
		t.Fatalf("the rows did not settle after the install: %v", err)
	}
	api, err := env.As(env.Manager())
	if err != nil {
		t.Fatal(err)
	}
	refused := patchStored(cleanupFixtures.readOnly, func(job *batchv1.Job) {
		job.Spec.TTLSecondsAfterFinished = ptr.To(int32(86400))
	})(ctx, api)
	if !apierrors.IsInvalid(refused) && !apierrors.IsForbidden(refused) {
		t.Fatalf("the guard's refusal is %T %v, which the manager would not fall back from", refused, refused)
	}
}
