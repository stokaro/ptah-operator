package jobclaim_test

import (
	"encoding/json"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
)

const (
	epochInForce = "v1-11111111111111111111111111111111"
	epochRetired = "v1-22222222222222222222222222222222"
)

func digest(char byte) string { return "sha256:" + strings.Repeat(string(char), 64) }

func builder() workload.Builder {
	return workload.Builder{
		ExecutorImage:          "example.test/executor@" + digest('2'),
		RunnerImage:            "example.test/runner@" + digest('3'),
		PtahVersion:            "v0.3.0",
		ControllerImage:        "example.test/controller@" + digest('1'),
		ControllerRevision:     "test-revision",
		ControllerStateVersion: 1,
	}
}

func bindingAt(epoch string) *operatorv1alpha1.ExecutionBindingStatus {
	return &operatorv1alpha1.ExecutionBindingStatus{
		Epoch:                  epoch,
		ControllerStateVersion: 1,
		PtahVersion:            "v0.3.0",
		ExecutorImage:          "example.test/executor@" + digest('2'),
		RunnerProtocolVersion:  int32(runner.ProtocolVersion),
	}
}

// snapshotOf is the admission snapshot a dispatch persists for template.
func snapshotOf(t *testing.T, template *corev1.PodTemplateSpec) *operatorv1alpha1.PodAdmissionSnapshot {
	t.Helper()

	templateDigest, err := podintent.DigestTemplate(template)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &operatorv1alpha1.PodAdmissionSnapshot{
		Version:        podintent.SnapshotVersion,
		TemplateDigest: templateDigest,
		ServiceAccount: operatorv1alpha1.ServiceAccountAdmissionSnapshot{Object: operatorv1alpha1.AdmissionObjectBinding{
			Name: "ptah-orders", UID: "service-account-uid", ResourceVersion: "1",
		}},
	}
	snapshotDigest, err := fingerprint.DigestCanonicalJSON(*snapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Digest = snapshotDigest
	return snapshot
}

// stored is job as the API server keeps it: with a UID, the selector bound to
// it, and the template labels that repeat it.
func stored(job *batchv1.Job) *batchv1.Job {
	job = job.DeepCopy()
	job.UID = "job-uid"
	job.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{batchv1.ControllerUidLabel: string(job.UID)}}
	job.Spec.Template.Labels[batchv1.ControllerUidLabel] = string(job.UID)
	job.Spec.Template.Labels[batchv1.JobNameLabel] = job.Name
	return job
}

// A family is one kind of owner, with the Job its claim builds by the real
// builder -- the one the controller dispatches with and the webhook rebuilds
// with -- and a function that makes the same claim under another epoch.
type family struct {
	name  string
	claim jobclaim.Claim
	built *batchv1.Job
	// under is the claim and its Job as they were made under epoch, while
	// epoch was in force.
	under func(t *testing.T, epoch string) (jobclaim.Claim, *batchv1.Job)
}

func schemaFamily(t *testing.T) family {
	t.Helper()

	schema := &operatorv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "orders", UID: "schema-uid"},
		Spec: operatorv1alpha1.PtahSchemaSpec{
			Target: operatorv1alpha1.DatabaseTargetSpec{
				Engine: operatorv1alpha1.DatabaseEnginePostgreSQL,
				URLFrom: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "database"}, Key: "url",
				},
				CoordinationKey: "tenant-a/orders-primary",
			},
			Desired:   operatorv1alpha1.OCIArtifactSourceSpec{OCIRef: "oci://registry.test/acme/orders-schema:stable"},
			Execution: operatorv1alpha1.ExecutionSpec{ServiceAccountName: "ptah-orders"},
		},
		Status: operatorv1alpha1.PtahSchemaStatus{ExecutionBinding: bindingAt(epochInForce)},
	}
	under := func(t *testing.T, epoch string) (jobclaim.Claim, *batchv1.Job) {
		t.Helper()
		schema := schema.DeepCopy()
		schema.Status.ExecutionBinding = bindingAt(epoch)
		operation := &operatorv1alpha1.ActiveOperationStatus{
			Type: operatorv1alpha1.OperationResolve, ID: "operation-id", InputFingerprint: digest('8'),
			Attempt: 1, ExecutionBindingID: epoch, StartedAt: metav1.Now(),
		}
		name, err := workload.NameFor(schema, *operation)
		if err != nil {
			t.Fatal(err)
		}
		operation.JobName = name
		job, err := builder().Build(schema, *operation, nil)
		if err != nil {
			t.Fatal(err)
		}
		operation.AdmissionSnapshot = snapshotOf(t, &job.Spec.Template)
		if job, err = builder().Build(schema, *operation, nil); err != nil {
			t.Fatal(err)
		}
		claim := jobclaim.SchemaOperation(schema, operation)
		claim.Binding = schema.Status.ExecutionBinding
		return claim, job
	}
	claim, built := under(t, epochInForce)
	return family{name: "PtahSchema", claim: claim, built: built, under: under}
}

func migrationFamily(t *testing.T) family {
	t.Helper()

	migration := &operatorv1alpha1.PtahMigration{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "orders", UID: "migration-uid"},
		Spec: operatorv1alpha1.PtahMigrationSpec{
			Target: operatorv1alpha1.DatabaseTargetSpec{
				Engine:  operatorv1alpha1.DatabaseEnginePostgreSQL,
				URLFrom: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "database"}, Key: "url"},
			},
			Artifact: operatorv1alpha1.OCIArtifactSourceSpec{
				OCIRef: "oci://registry.test/acme/orders-migrations:stable",
				VerificationPolicyFrom: corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "verification"}, Key: "policy.yaml",
				},
			},
			Execution: operatorv1alpha1.ExecutionSpec{ServiceAccountName: "ptah-orders"},
		},
		Status: operatorv1alpha1.PtahMigrationStatus{ExecutionBinding: bindingAt(epochInForce)},
	}
	under := func(t *testing.T, epoch string) (jobclaim.Claim, *batchv1.Job) {
		t.Helper()
		migration := migration.DeepCopy()
		migration.Status.ExecutionBinding = bindingAt(epoch)
		operation := &operatorv1alpha1.MigrationOperationStatus{
			Type: operatorv1alpha1.MigrationOperationHistory, ID: digest('8'), InputFingerprint: digest('a'),
			ExecutionBindingID: epoch, Attempt: 1, StartedAt: metav1.Now(),
			Source: &operatorv1alpha1.OCIArtifactAccessBinding{
				ResolvedReference: "oci://registry.test/acme/orders-migrations@" + digest('4'),
				Digest:            digest('4'),
			},
			Target: &operatorv1alpha1.DatabaseTargetBinding{
				Engine:  operatorv1alpha1.DatabaseEnginePostgreSQL,
				URLFrom: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "database"}, Key: "url"},
			},
			CoordinationDigest: digest('6'),
		}
		name, err := workload.NameForMigration(migration, *operation)
		if err != nil {
			t.Fatal(err)
		}
		operation.JobName = name
		job, err := builder().BuildMigration(migration, *operation, nil)
		if err != nil {
			t.Fatal(err)
		}
		operation.AdmissionSnapshot = snapshotOf(t, &job.Spec.Template)
		if job, err = builder().BuildMigration(migration, *operation, nil); err != nil {
			t.Fatal(err)
		}
		claim := jobclaim.MigrationOperation(migration, operation)
		claim.Binding = migration.Status.ExecutionBinding
		return claim, job
	}
	claim, built := under(t, epochInForce)
	return family{name: "PtahMigration", claim: claim, built: built, under: under}
}

func families(t *testing.T) []family {
	t.Helper()
	return []family{schemaFamily(t), migrationFamily(t)}
}

// A mode is one of the three ways a caller holds a Job to its claim: to the
// Job the claim rebuilds, as the API server stored it or as the manager
// submitted it, or to the envelope the claim fixes when nothing can be
// rebuilt.
type mode struct {
	name   string
	adjust func(claim jobclaim.Claim, built *batchv1.Job) jobclaim.Claim
}

func modes() []mode {
	return []mode{
		{name: "rebuilt, read back", adjust: func(claim jobclaim.Claim, built *batchv1.Job) jobclaim.Claim {
			claim.Built, claim.Stored = built, true
			return claim
		}},
		{name: "rebuilt, submitted", adjust: func(claim jobclaim.Claim, built *batchv1.Job) jobclaim.Claim {
			claim.Built, claim.Stored = built, false
			return claim
		}},
		{name: "envelope", adjust: func(claim jobclaim.Claim, _ *batchv1.Job) jobclaim.Claim {
			claim.Built = nil
			return claim
		}},
	}
}

// Every mode holds a Job to the same account of its claim. The control in
// each cell is the exact Job, which has to be accepted for a refusal below it
// to mean anything; every row then changes one thing a Job could differ in.
func TestAJobIsItsClaimsOnlyWhenEveryPartMatches(t *testing.T) {
	t.Parallel()

	rows := []struct {
		name   string
		differ func(*batchv1.Job)
	}{
		{name: "another name", differ: func(job *batchv1.Job) { job.Name += "-2" }},
		{name: "another namespace", differ: func(job *batchv1.Job) { job.Namespace = "tenant-b" }},
		{name: "another UID than the claim recorded", differ: func(job *batchv1.Job) { job.UID = "another-job-uid" }},
		{name: "no UID", differ: func(job *batchv1.Job) { job.UID = "" }},
		{name: "another owner", differ: func(job *batchv1.Job) { job.OwnerReferences[0].UID = "another-owner-uid" }},
		{name: "an owner that is not the controller", differ: func(job *batchv1.Job) {
			job.OwnerReferences[0].Controller = nil
		}},
		{name: "a second owner", differ: func(job *batchv1.Job) {
			job.OwnerReferences = append(job.OwnerReferences, job.OwnerReferences[0])
		}},
		{name: "another operation ID", differ: func(job *batchv1.Job) {
			job.Annotations[workload.AnnotationOperationID] = "another-operation"
			job.Spec.Template.Annotations[workload.AnnotationOperationID] = "another-operation"
		}},
		{name: "a reserved label the claim does not fix", differ: func(job *batchv1.Job) {
			job.Labels["ptah.run/extra"] = "x"
			job.Spec.Template.Labels["ptah.run/extra"] = "x"
		}},
		{name: "an undeclared label on the Job and its Pods", differ: func(job *batchv1.Job) {
			job.Labels["team"] = "payments"
			job.Spec.Template.Labels["team"] = "payments"
		}},
		{name: "Pod annotations that are not the Job's", differ: func(job *batchv1.Job) {
			job.Spec.Template.Annotations["note"] = "only on the Pods"
		}},
		{name: "another executor", differ: func(job *batchv1.Job) {
			job.Spec.Template.Spec.Containers[len(job.Spec.Template.Spec.Containers)-1].Image =
				"example.test/other@" + digest('9')
		}},
		{name: "a selector bound to another Job", differ: func(job *batchv1.Job) {
			job.Spec.Selector.MatchLabels[batchv1.ControllerUidLabel] = "another-job-uid"
		}},
		{name: "a Pod label claiming another Job's identity", differ: func(job *batchv1.Job) {
			job.Spec.Template.Labels[batchv1.ControllerUidLabel] = "another-job-uid"
		}},
		{name: "another execution epoch on the Job", differ: func(job *batchv1.Job) {
			job.Annotations[workload.AnnotationExecutionBindingID] = epochRetired
			job.Spec.Template.Annotations[workload.AnnotationExecutionBindingID] = epochRetired
		}},
	}

	for _, family := range families(t) {
		for _, mode := range modes() {
			claim := family.claim
			claim.JobUID = "job-uid"
			claim = mode.adjust(claim, family.built)
			exact := stored(family.built)
			if err := jobclaim.Match(exact, claim); err != nil {
				t.Fatalf("%s, %s: the exact Job was refused, so nothing below proves anything: %v", family.name, mode.name, err)
			}
			for _, row := range rows {
				t.Run(family.name+"/"+mode.name+"/"+row.name, func(t *testing.T) {
					t.Parallel()

					job := exact.DeepCopy()
					row.differ(job)
					if err := jobclaim.Match(job, claim); err == nil {
						t.Fatalf("a Job with %s was accepted as its claim's", row.name)
					}
				})
			}
		}
	}
}

// The epoch is what ties a Job to the claim that dispatched it, and a claim
// to the binding in force. A Job dispatched under a retired epoch is refused
// by a claim made under the current one, in every mode, and so is the Job of
// a claim that is itself from a retired epoch, wherever the caller names the
// binding in force. That second refusal is the one the Job-create webhook
// relied on its builder for before it held Jobs to their claim here.
func TestAJobUnderAnotherEpochIsNotItsClaims(t *testing.T) {
	t.Parallel()

	for _, family := range families(t) {
		retiredClaim, retiredBuild := family.under(t, epochRetired)
		retiredJob := stored(retiredBuild)
		// The epoch is part of the name a claim derives, so a Job of another
		// epoch normally stands under another name as well. This one stands
		// under the name the current claim reserved, which leaves the epoch
		// the only thing that tells the two apart.
		squatter := retiredJob.DeepCopy()
		squatter.Name = family.claim.JobName
		squatter.Spec.Template.Labels[batchv1.JobNameLabel] = squatter.Name
		for _, mode := range modes() {
			t.Run(family.name+"/"+mode.name, func(t *testing.T) {
				t.Parallel()

				claim := mode.adjust(family.claim, family.built)
				if err := jobclaim.Match(stored(family.built), claim); err != nil {
					t.Fatalf("the Job built under the claim's epoch was refused, so nothing below proves anything: %v", err)
				}

				// The claim is current; the Job under its name was dispatched
				// under the retired epoch.
				err := jobclaim.Match(squatter, claim)
				if err == nil || !strings.Contains(err.Error(), "another execution epoch") {
					t.Fatalf("a Job dispatched under another epoch than its claim = %v, want an epoch refusal", err)
				}

				// The claim and its Job agree, and both belong to an epoch the
				// binding in force has replaced. The builder would refuse to
				// rebuild this claim; the rebuild is supplied anyway, so the
				// refusal is shown to be the matcher's own.
				stale := mode.adjust(retiredClaim, retiredBuild)
				stale.Binding = bindingAt(epochInForce)
				err = jobclaim.Match(retiredJob, stale)
				if err == nil || !strings.Contains(err.Error(), "no longer in force") {
					t.Fatalf("the Job of a claim from a retired epoch = %v, want a refusal naming the epoch in force", err)
				}

				// A caller judging a retired claim's Job names no binding and
				// holds the epoch to its retirement record instead; the Job
				// is then the claim's.
				stale.Binding = nil
				if err := jobclaim.Match(retiredJob, stale); err != nil {
					t.Fatalf("a retired claim's own Job, judged without the binding in force, was refused: %v", err)
				}
			})
		}
	}
}

// A Job read back may carry the cleanup TTL the controller sets once it has
// harvested it; a Job being created may not, since nothing has harvested it.
func TestOnlyAStoredJobMayCarryTheCleanupTTL(t *testing.T) {
	t.Parallel()

	for _, family := range families(t) {
		t.Run(family.name, func(t *testing.T) {
			t.Parallel()

			for _, row := range []struct {
				name   string
				stored bool
				ttl    int32
				accept bool
			}{
				{name: "read back with the cleanup TTL", stored: true, ttl: jobclaim.CleanupTTLSeconds, accept: true},
				{name: "read back with another TTL", stored: true, ttl: jobclaim.CleanupTTLSeconds + 1},
				{name: "submitted with the cleanup TTL", stored: false, ttl: jobclaim.CleanupTTLSeconds},
			} {
				claim := family.claim
				claim.Built, claim.Stored = family.built, row.stored
				job := stored(family.built)
				job.Spec.TTLSecondsAfterFinished = &row.ttl
				if err := jobclaim.Match(job, claim); (err == nil) != row.accept {
					t.Fatalf("%s: Match() = %v, want accepted %t", row.name, err, row.accept)
				}
			}
		})
	}
}

// A submitted Job's metadata is the manager's whole request, so anything in
// it beyond what the API server assigns has to be what the claim builds. A
// stored Job's metadata beyond its labels, annotations and owner is written
// by others after it was created -- a finalizer foreground deletion adds --
// and says nothing about what the Job runs.
func TestASubmittedJobCarriesNoMetadataItsClaimDidNotBuild(t *testing.T) {
	t.Parallel()

	for _, family := range families(t) {
		job := stored(family.built)
		job.Finalizers = []string{"foregroundDeletion"}

		submitted := family.claim
		submitted.Built = family.built
		if err := jobclaim.Match(job, submitted); err == nil || !strings.Contains(err.Error(), "metadata") {
			t.Fatalf("%s: a submitted Job with a finalizer = %v, want a metadata refusal", family.name, err)
		}
		read := submitted
		read.Stored = true
		if err := jobclaim.Match(job, read); err != nil {
			t.Fatalf("%s: a stored Job with a finalizer was refused: %v", family.name, err)
		}
	}
}

// The spec is compared by meaning. A Job read back through the API carries
// its quantities in canonical form, which is not the form the builder wrote,
// and is still the same Job; a different size, or an empty map where the
// build had none, is not.
func TestASpecIsComparedByWhatItMeans(t *testing.T) {
	t.Parallel()

	for _, family := range families(t) {
		claim := family.claim
		claim.Built, claim.Stored = family.built, true

		payload, err := json.Marshal(stored(family.built))
		if err != nil {
			t.Fatal(err)
		}
		readBack := &batchv1.Job{}
		if err := json.Unmarshal(payload, readBack); err != nil {
			t.Fatal(err)
		}
		if err := jobclaim.Match(readBack, claim); err != nil {
			t.Fatalf("%s: the Job read back through JSON was refused: %v", family.name, err)
		}

		resized := readBack.DeepCopy()
		changed := false
		for _, volume := range resized.Spec.Template.Spec.Volumes {
			if volume.EmptyDir != nil && volume.EmptyDir.SizeLimit != nil {
				size := resource.MustParse("3Mi")
				volume.EmptyDir.SizeLimit = &size
				changed = true
				break
			}
		}
		if !changed {
			t.Fatalf("%s: the builder wrote no sized volume, so the size row proves nothing", family.name)
		}
		if err := jobclaim.Match(resized, claim); err == nil || !strings.Contains(err.Error(), "spec") {
			t.Fatalf("%s: a Job with a resized volume = %v, want a spec refusal", family.name, err)
		}

		emptied := readBack.DeepCopy()
		emptied.Spec.Template.Spec.NodeSelector = map[string]string{}
		if family.built.Spec.Template.Spec.NodeSelector != nil {
			t.Fatalf("%s: the builder wrote a node selector, so the empty-map row proves nothing", family.name)
		}
		if err := jobclaim.Match(emptied, claim); err == nil || !strings.Contains(err.Error(), "spec") {
			t.Fatalf("%s: an empty node selector = %v, want a spec refusal", family.name, err)
		}
	}
}

// A claim that cannot say what its Job is accepts none.
func TestAnIncompleteClaimAcceptsNoJob(t *testing.T) {
	t.Parallel()

	for _, family := range families(t) {
		job := stored(family.built)
		if err := jobclaim.Match(job, family.claim); err != nil {
			t.Fatalf("%s: the complete claim refused its Job, so nothing below proves anything: %v", family.name, err)
		}
		for _, row := range []struct {
			name   string
			change func(*jobclaim.Claim)
		}{
			{name: "no admission snapshot", change: func(claim *jobclaim.Claim) { claim.Snapshot = nil }},
			{name: "a malformed epoch", change: func(claim *jobclaim.Claim) { claim.Epoch = "epoch-1" }},
			{name: "an owner kind that dispatches no Jobs", change: func(claim *jobclaim.Claim) { claim.Owner.Kind = "Deployment" }},
			{name: "no operation ID", change: func(claim *jobclaim.Claim) { claim.ID = "" }},
			{name: "no Job name", change: func(claim *jobclaim.Claim) { claim.JobName = "" }},
		} {
			claim := family.claim
			row.change(&claim)
			if err := jobclaim.Match(job, claim); err == nil {
				t.Fatalf("%s: a claim with %s accepted a Job", family.name, row.name)
			}
		}
	}
}
