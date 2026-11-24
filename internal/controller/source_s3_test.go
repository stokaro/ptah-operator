package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
)

const (
	s3VersionA = "3HL4kqtJlcpXroDTDmJ+rmSpXd3dIbrHY"
	s3VersionB = "Ga1Zo6Yx7bWqQf9cXJk0d2Vh5uTnPsRz"
)

func s3SourceSpec() *operatorv1alpha1.S3SourceSpec {
	return &operatorv1alpha1.S3SourceSpec{
		Endpoint: "https://objects.example.com:9000", Region: "us-east-1", Bucket: "app-releases",
		Key: "acme/app-1.4.0.zip", Digest: httpsDownloadA, Format: operatorv1alpha1.HTTPSFormatZip,
		Path:     "app-1.4.0/db/schema.hcl",
		AuthFrom: &operatorv1alpha1.S3AuthSource{Name: "objects-keys"},
	}
}

func s3Revision(spec *operatorv1alpha1.S3SourceSpec, version, digest, content string) *operatorv1alpha1.S3SourceRevision {
	return &operatorv1alpha1.S3SourceRevision{
		Endpoint: spec.Endpoint, Region: spec.Region, Bucket: spec.Bucket, Key: spec.Key, VersionID: version,
		Format: spec.Format, Path: spec.Path, Digest: digest, ContentDigest: content,
	}
}

func s3ResolveFrame(t *testing.T, operationID, download, version, content string, size int64) []byte {
	t.Helper()
	frame, err := runner.MarshalFrame(runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationResolve, OperationID: operationID,
		ResolvedDownloadDigest: download, ResolvedVersionID: version, ResolvedContentDigest: content, ResolvedContentBytes: size,
	})
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

// s3ResolvingSchema is a schema whose S3 Resolve Job has finished, with the
// resolution before it, if any.
func s3ResolvingSchema(t *testing.T, spec *operatorv1alpha1.S3SourceSpec, previous *operatorv1alpha1.S3SourceRevision) *operatorv1alpha1.PtahSchema {
	t.Helper()
	schema := gitResolvingSchema(t, nil)
	schema.Spec.Desired = operatorv1alpha1.SourceSpec{S3: spec}
	schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{S3: previous}
	bindActiveInput(t, schema)
	return schema
}

// An S3 source has no verification policy, as an HTTPS one has none: the
// pinned digest is the trust decision. The resolution goes straight to
// observation, records the version the endpoint served, and every later
// operation reads that version.
func TestS3ResolveResultGoesStraightToObservation(t *testing.T) {
	t.Parallel()
	schema := s3ResolvingSchema(t, s3SourceSpec(), nil)
	meta.SetStatusCondition(&schema.Status.Conditions, metav1.Condition{
		Type: operatorv1alpha1.ConditionArtifactVerified, Status: metav1.ConditionTrue,
		Reason: string(operatorv1alpha1.ReasonPolicySatisfied), Message: "the earlier OCI artifact passed its policy",
	})
	actual := reconcileGitResolve(t, schema, s3ResolveFrame(t, testDigest, httpsDownloadA, s3VersionA, gitContentOne, 0))
	if actual.Status.ActiveOperation != nil || actual.Status.Phase != operatorv1alpha1.PhaseObserving {
		t.Fatalf("phase %s, operation %#v; want Observing and no claim", actual.Status.Phase, actual.Status.ActiveOperation)
	}
	want := s3Revision(s3SourceSpec(), s3VersionA, httpsDownloadA, gitContentOne)
	if got := actual.Status.Source.S3; got == nil || *got != *want || actual.Status.Source.Digest != "" ||
		actual.Status.Source.HTTPS != nil || actual.Status.Source.Git != nil || actual.Status.Source.Verified {
		t.Fatalf("source status = %#v, want %#v alone", actual.Status.Source, want)
	}
	resolved := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionArtifactResolved)
	if resolved == nil || resolved.Status != metav1.ConditionTrue || resolved.Reason != string(operatorv1alpha1.ReasonDownloadVerified) ||
		!strings.Contains(resolved.Message, httpsDownloadA) || !strings.Contains(resolved.Message, gitContentOne) ||
		!strings.Contains(resolved.Message, "every operation reads version "+s3VersionA) {
		t.Fatalf("ArtifactResolved = %#v, want DownloadVerified naming both digests and the version", resolved)
	}
	if meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionArtifactVerified) != nil {
		t.Fatal("an S3 source reports a verification it never had")
	}
	access := sourcebinding.SchemaAccess(actual.Spec.Desired, actual.Status.Source)
	if access == nil || access.S3 == nil || access.S3.Digest != httpsDownloadA || access.S3.VersionID != s3VersionA ||
		access.S3.ContentDigest != gitContentOne || access.S3.AuthFrom == nil || access.S3.AuthFrom.Name != "objects-keys" {
		t.Fatalf("the next operation would read %#v", access)
	}
}

// A bucket that keeps no versions serves none, and the condition says that a
// later write replaces what every operation reads.
func TestAnUnversionedS3ResolutionSaysAWriteReplacesIt(t *testing.T) {
	t.Parallel()
	actual := reconcileGitResolve(t, s3ResolvingSchema(t, s3SourceSpec(), nil),
		s3ResolveFrame(t, testDigest, httpsDownloadA, "", gitContentOne, 0))
	if got := actual.Status.Source.S3; got == nil || got.VersionID != "" {
		t.Fatalf("source status = %#v, want a resolution with no version", actual.Status.Source)
	}
	resolved := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionArtifactResolved)
	if resolved == nil || !strings.Contains(resolved.Message, "the bucket keeps no version of it, so a later write replaces what every operation reads") {
		t.Fatalf("ArtifactResolved = %#v, want the unversioned object named", resolved)
	}
}

// An object written again under a new digest, or read from another version,
// whose selected files did not change keeps the plan once the spec names the
// new digest; other files, another path, key, bucket or endpoint retire it.
func TestS3ResolveRetiresThePlanOnlyForOtherContent(t *testing.T) {
	t.Parallel()
	spec := s3SourceSpec()
	spec.Digest = httpsDownloadB
	plan := &operatorv1alpha1.CurrentPlanStatus{Name: "orders-plan", UID: "plan-uid", Fingerprint: testDigest}
	for name, row := range map[string]struct {
		content string
		edit    func(*operatorv1alpha1.S3SourceRevision)
		kept    bool
	}{
		"a new version with the same files": {content: gitContentOne, kept: true},
		"the same files in another region": {
			content: gitContentOne, kept: true,
			edit: func(r *operatorv1alpha1.S3SourceRevision) { r.Region = "eu-west-1" },
		},
		"a new version with other files": {content: gitContentTwo},
		"the same files under another path": {
			content: gitContentOne,
			edit:    func(r *operatorv1alpha1.S3SourceRevision) { r.Path = "app-1.3.0/db/schema.hcl" },
		},
		"the same files under another key": {
			content: gitContentOne,
			edit:    func(r *operatorv1alpha1.S3SourceRevision) { r.Key = "acme/app-1.3.0.zip" },
		},
		"the same files in another bucket": {
			content: gitContentOne,
			edit:    func(r *operatorv1alpha1.S3SourceRevision) { r.Bucket = "app-archive" },
		},
		"the same files from another endpoint": {
			content: gitContentOne,
			edit:    func(r *operatorv1alpha1.S3SourceRevision) { r.Endpoint = "https://objects.example.com" },
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			previous := s3Revision(spec, s3VersionA, httpsDownloadA, gitContentOne)
			if row.edit != nil {
				row.edit(previous)
			}
			schema := s3ResolvingSchema(t, spec.DeepCopy(), previous)
			schema.Status.Plan = plan.DeepCopy()
			actual := reconcileGitResolve(t, schema, s3ResolveFrame(t, testDigest, httpsDownloadB, s3VersionB, row.content, 0))
			if got := actual.Status.Source.S3; got == nil || got.Digest != httpsDownloadB || got.VersionID != s3VersionB || got.ContentDigest != row.content {
				t.Fatalf("source status = %#v, want the new version", actual.Status.Source)
			}
			if kept := actual.Status.Plan != nil; kept != row.kept {
				t.Fatalf("plan kept = %v, want %v", kept, row.kept)
			}
		})
	}
}

// A resolution that does not answer the object the spec pins is retried,
// never recorded: an OCI descriptor, a Git commit, another object, or
// another version than the one the spec names.
func TestS3ResolveRefusesAResultThatAnswersSomethingElse(t *testing.T) {
	t.Parallel()
	ociFrame, err := runner.MarshalFrame(runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationResolve, OperationID: testDigest,
		ResolvedDigest: testDigest, ResolvedReference: "oci://registry.example/team/schema@" + testDigest,
		ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json", ResolvedSize: 321,
	})
	if err != nil {
		t.Fatal(err)
	}
	named := s3SourceSpec()
	named.VersionID = s3VersionA
	for name, row := range map[string]struct {
		spec     *operatorv1alpha1.S3SourceSpec
		frame    []byte
		recorded bool
	}{
		"an OCI descriptor":    {spec: s3SourceSpec(), frame: ociFrame},
		"a Git commit":         {spec: s3SourceSpec(), frame: gitResolveFrame(t, testDigest, gitCommitA, gitContentOne)},
		"another object":       {spec: s3SourceSpec(), frame: s3ResolveFrame(t, testDigest, httpsDownloadB, s3VersionA, gitContentOne, 0)},
		"another version":      {spec: named, frame: s3ResolveFrame(t, testDigest, httpsDownloadA, s3VersionB, gitContentOne, 0)},
		"no version":           {spec: named, frame: s3ResolveFrame(t, testDigest, httpsDownloadA, "", gitContentOne, 0)},
		"the pinned object":    {spec: s3SourceSpec(), frame: s3ResolveFrame(t, testDigest, httpsDownloadA, s3VersionA, gitContentOne, 0), recorded: true},
		"the version it names": {spec: named, frame: s3ResolveFrame(t, testDigest, httpsDownloadA, s3VersionA, gitContentOne, 0), recorded: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			actual := reconcileGitResolve(t, s3ResolvingSchema(t, row.spec, nil), row.frame)
			recorded := actual.Status.Source.S3 != nil
			// The pinned object and the named version are the controls: the
			// same fixture records them, so the refusals are about the answer.
			if recorded != row.recorded || recorded != (actual.Status.Phase == operatorv1alpha1.PhaseObserving) {
				t.Fatalf("recorded %v, phase %s; want recorded %v: source %#v", recorded, actual.Status.Phase, row.recorded, actual.Status.Source)
			}
			if actual.Status.Source.Git != nil || actual.Status.Source.HTTPS != nil || actual.Status.Source.Digest != "" {
				t.Fatalf("an S3 source recorded another kind: %#v", actual.Status.Source)
			}
		})
	}
}

func TestProofReadsTheCurrentSourceComparesTheS3Content(t *testing.T) {
	t.Parallel()
	spec := s3SourceSpec()
	binding := sourcebinding.S3Access(*spec, *s3Revision(spec, s3VersionA, httpsDownloadA, gitContentOne))
	status := operatorv1alpha1.SchemaSourceStatus{S3: s3Revision(spec, s3VersionB, httpsDownloadB, gitContentOne)}
	if !proofReadsTheCurrentSource(status, operatorv1alpha1.SourceAccessBinding{S3: binding}) {
		t.Fatal("a proof of the same files in another version was refused")
	}
	for name, edit := range map[string]func(*operatorv1alpha1.SchemaSourceStatus){
		"other files":      func(s *operatorv1alpha1.SchemaSourceStatus) { s.S3.ContentDigest = gitContentTwo },
		"another path":     func(s *operatorv1alpha1.SchemaSourceStatus) { s.S3.Path = "db/other" },
		"another key":      func(s *operatorv1alpha1.SchemaSourceStatus) { s.S3.Key = "acme/other.zip" },
		"another bucket":   func(s *operatorv1alpha1.SchemaSourceStatus) { s.S3.Bucket = "app-archive" },
		"another endpoint": func(s *operatorv1alpha1.SchemaSourceStatus) { s.S3.Endpoint = "https://objects.example.com" },
		"no S3 resolution": func(s *operatorv1alpha1.SchemaSourceStatus) { s.S3 = nil },
		"an HTTPS resolution of the same files": func(s *operatorv1alpha1.SchemaSourceStatus) {
			s.HTTPS = &operatorv1alpha1.HTTPSSourceRevision{
				URL: spec.Endpoint + "/" + spec.Bucket + "/" + spec.Key, Format: spec.Format, Path: spec.Path,
				Digest: httpsDownloadA, ContentDigest: gitContentOne,
			}
			s.S3 = nil
		},
	} {
		moved := *status.DeepCopy()
		edit(&moved)
		if proofReadsTheCurrentSource(moved, operatorv1alpha1.SourceAccessBinding{S3: binding}) {
			t.Errorf("a proof was admitted against %s", name)
		}
	}
}

// A bucket the policy does not name gets no Resolve Job: the key pair is
// never used there. A policy that names the endpoint and bucket lets the
// same schema claim its Resolve, and one that names only the endpoint as an
// HTTPS host does not.
func TestASourcePolicyRefusesAnS3BucketBeforeAnyJob(t *testing.T) {
	t.Parallel()
	for name, row := range map[string]struct {
		policy  string
		refused bool
	}{
		"another bucket on the endpoint": {policy: "allowedS3Buckets:\n  - objects.example.com:9000/app-private\n", refused: true},
		"the endpoint as an HTTPS host":  {policy: "allowedHTTPSHosts:\n  - objects.example.com:9000\nallowedS3Buckets: []\n", refused: true},
		"its endpoint and bucket":        {policy: "allowedS3Buckets:\n  - objects.example.com:9000/app-releases\n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			schema := schemaFixture()
			schema.Spec.Desired = operatorv1alpha1.SourceSpec{S3: s3SourceSpec()}
			withSourcePolicy(&schema.Spec.Desired, "source-policy")
			reconciler, api := fakeReconciler(t, staticLogs{}, schema, sourcePolicyConfigMap("source-policy", "policy-uid", row.policy))
			actual := reconcileSchema(t, reconciler, api, schema)
			jobs := &batchv1.JobList{}
			if err := api.List(context.Background(), jobs); err != nil {
				t.Fatal(err)
			}
			if !row.refused {
				if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve {
					t.Fatalf("an allowed bucket claimed %#v, want a Resolve", actual.Status.ActiveOperation)
				}
				return
			}
			requireRefused(t, actual, "allowedS3Buckets does not name objects.example.com:9000/app-releases")
			if actual.Status.ActiveOperation != nil || len(jobs.Items) != 0 {
				t.Fatalf("a refused bucket was claimed: %#v, %d Jobs", actual.Status.ActiveOperation, len(jobs.Items))
			}
			resolved := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionArtifactResolved)
			if resolved == nil || !strings.Contains(resolved.Message, "The S3 source") {
				t.Fatalf("ArtifactResolved = %#v, want the S3 source named", resolved)
			}
		})
	}
}

// What an S3 resolution selected is held to the size rule before it is
// recorded.
func TestASourcePolicyRefusesAnS3ResolutionThatHoldsTooMuch(t *testing.T) {
	t.Parallel()
	for name, row := range map[string]struct {
		bytes    int64
		recorded bool
	}{
		"within the bound": {bytes: 100, recorded: true},
		"past the bound":   {bytes: 101},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			schema := s3ResolvingSchema(t, s3SourceSpec(), nil)
			withSourcePolicy(&schema.Spec.Desired, "source-policy")
			bindActiveInput(t, schema)
			frame := s3ResolveFrame(t, testDigest, httpsDownloadA, s3VersionA, gitContentOne, row.bytes)
			job, pod := terminalWorkload(schema, batchv1.JobComplete)
			reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod,
				sourcePolicyConfigMap("source-policy", "policy-uid", "maxContentBytes: 100\n"))
			actual := reconcileSchema(t, reconciler, api, schema)
			if recorded := actual.Status.Source.S3 != nil; recorded != row.recorded {
				t.Fatalf("recorded %v, want %v: status %#v", recorded, row.recorded, actual.Status)
			}
			if !row.recorded {
				failed := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed)
				if failed == nil || !strings.Contains(failed.Message, "more than maxContentBytes 100") {
					t.Fatalf("failure %#v; want the size rule named", failed)
				}
				return
			}
			if actual.Status.Source.SourcePolicy == nil || actual.Status.Source.SourcePolicy.UID != "policy-uid" {
				t.Fatalf("the resolution was recorded without its policy: %#v", actual.Status.Source)
			}
		})
	}
}

// s3ResolvingMigration is a migration whose S3 Resolve Job has finished with
// frame, and the reconciler that will read it.
func s3ResolvingMigration(t *testing.T, policy string, frame func(operationID string) []byte) (*MigrationReconciler, *operatorv1alpha1.PtahMigration, func() *operatorv1alpha1.PtahMigration) {
	t.Helper()
	migration := migrationFixture()
	spec := s3SourceSpec()
	spec.Key, spec.Format, spec.Path = "acme/orders-1.4.0.tar.gz", operatorv1alpha1.HTTPSFormatTarGzip, "orders-1.4.0/db/migrations"
	migration.Spec.Artifact = operatorv1alpha1.SourceSpec{S3: spec}
	objects := []client.Object{}
	if policy != "" {
		withSourcePolicy(&migration.Spec.Artifact, "source-policy")
		objects = append(objects, sourcePolicyConfigMap("source-policy", "policy-uid", policy))
	}
	// The source was a ConfigMap before.
	migration.Status.ConfigMap = &operatorv1alpha1.ConfigMapSourceRevision{Name: "orders-migrations", UID: "configmap-uid", ResourceVersion: "3"}
	meta.SetStatusCondition(&migration.Status.Conditions, metav1.Condition{
		Type: operatorv1alpha1.ConditionMigrationArtifactVerified, Status: metav1.ConditionTrue,
		Reason: string(operatorv1alpha1.ReasonPolicySatisfied), Message: "an earlier OCI artifact passed its policy",
	})
	migration.Status.ExecutionBinding = migrationExecutionBinding()
	migration.Status.Phase = operatorv1alpha1.MigrationPhaseResolving
	migration.Finalizers = []string{migrationOperationFinalizer}
	operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationResolve)
	job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{content: frame(operation.ID)}, append([]client.Object{migration, job, pod}, objects...)...)
	return reconciler, migration, func() *operatorv1alpha1.PtahMigration { return readMigration(t, api, migration) }
}

// A migration directory in an S3 archive is resolved by a Job and read by
// the history claim that follows, with no Verify step, from the version the
// resolution recorded.
func TestMigrationS3ResolveResultGoesStraightToTheHistoryRead(t *testing.T) {
	t.Parallel()
	reconciler, migration, read := s3ResolvingMigration(t, "", func(id string) []byte {
		return s3ResolveFrame(t, id, httpsDownloadA, s3VersionA, gitContentOne, 0)
	})
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := read()
	if actual.Status.ActiveOperation != nil || actual.Status.Phase != operatorv1alpha1.MigrationPhaseReading {
		t.Fatalf("phase %s, operation %#v; want Reading and no claim", actual.Status.Phase, actual.Status.ActiveOperation)
	}
	object := actual.Status.Artifact.S3
	if object == nil || object.Digest != httpsDownloadA || object.VersionID != s3VersionA || object.ContentDigest != gitContentOne ||
		object.Format != operatorv1alpha1.HTTPSFormatTarGzip || object.Path != "orders-1.4.0/db/migrations" ||
		actual.Status.Artifact.Digest != "" || actual.Status.Artifact.HTTPS != nil || actual.Status.Artifact.Git != nil ||
		actual.Status.ConfigMap != nil {
		t.Fatalf("artifact = %#v", actual.Status.Artifact)
	}
	if meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationArtifactVerified) != nil {
		t.Fatal("an S3 source reports a verification it never had")
	}
	progressing := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	if progressing == nil || progressing.Reason != string(operatorv1alpha1.ReasonDownloadVerified) ||
		!strings.Contains(progressing.Message, httpsDownloadA) || !strings.Contains(progressing.Message, gitContentOne) ||
		!strings.Contains(progressing.Message, s3VersionA) {
		t.Fatalf("Progressing = %#v, want DownloadVerified naming both digests and the version", progressing)
	}
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	history := read().Status.ActiveOperation
	if history == nil || history.Type != operatorv1alpha1.MigrationOperationHistory || history.Source == nil ||
		history.Source.S3 == nil || history.Source.S3.Digest != httpsDownloadA || history.Source.S3.VersionID != s3VersionA ||
		history.Source.S3.AuthFrom == nil || history.Source.Digest != "" {
		t.Fatalf("history claim = %#v, want the resolved version with its credential selector", history)
	}
}

// A migration's S3 resolution is held to its source policy before it is
// recorded, as a schema's is.
func TestAMigrationS3ResolutionIsHeldToItsSourcePolicy(t *testing.T) {
	t.Parallel()
	for name, row := range map[string]struct {
		bytes    int64
		recorded bool
	}{
		"within the bound": {bytes: 100, recorded: true},
		"past the bound":   {bytes: 101},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reconciler, migration, read := s3ResolvingMigration(t, "maxContentBytes: 100\n", func(id string) []byte {
				return s3ResolveFrame(t, id, httpsDownloadA, s3VersionA, gitContentOne, row.bytes)
			})
			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := read()
			recorded := actual.Status.Artifact != nil && actual.Status.Artifact.S3 != nil
			if recorded != row.recorded {
				t.Fatalf("recorded %v, want %v: status %#v", recorded, row.recorded, actual.Status)
			}
			if recorded && (actual.Status.SourcePolicy == nil || actual.Status.SourcePolicy.UID != "policy-uid") {
				t.Fatalf("the resolution was recorded without its policy: %#v", actual.Status.SourcePolicy)
			}
		})
	}
}

// A migration's bucket the policy does not name gets no Resolve Job.
func TestAMigrationSourcePolicyRefusesAnS3BucketBeforeAnyJob(t *testing.T) {
	t.Parallel()
	for name, row := range map[string]struct {
		allowed string
		refused bool
	}{
		"another bucket": {allowed: "objects.example.com:9000/app-private", refused: true},
		"its bucket":     {allowed: "objects.example.com:9000/app-releases"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			migration := migrationFixture()
			migration.Spec.Artifact = operatorv1alpha1.SourceSpec{S3: s3SourceSpec()}
			migration.Spec.Artifact.S3.Path = "db/migrations"
			withSourcePolicy(&migration.Spec.Artifact, "source-policy")
			reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration,
				sourcePolicyConfigMap("source-policy", "policy-uid", "allowedS3Buckets:\n  - "+row.allowed+"\n"))
			for range 2 {
				if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
					t.Fatalf("Reconcile() error = %v", err)
				}
			}
			actual := readMigration(t, api, migration)
			jobs := &batchv1.JobList{}
			if err := api.List(context.Background(), jobs); err != nil {
				t.Fatal(err)
			}
			if !row.refused {
				if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Type != operatorv1alpha1.MigrationOperationResolve {
					t.Fatalf("an allowed bucket claimed %#v, want a Resolve", actual.Status.ActiveOperation)
				}
				return
			}
			ready := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationReady)
			if actual.Status.Phase != operatorv1alpha1.MigrationPhaseFailed || actual.Status.ActiveOperation != nil || len(jobs.Items) != 0 ||
				ready == nil || !strings.Contains(ready.Message, "allowedS3Buckets does not name objects.example.com:9000/app-releases") {
				t.Fatalf("phase %s, claim %#v, %d Jobs, Ready %#v; want Failed naming the rule before any Job",
					actual.Status.Phase, actual.Status.ActiveOperation, len(jobs.Items), ready)
			}
		})
	}
}

// A refresh of a resolved S3 source says it reads the object again, and
// keeps what was resolved as historical evidence.
func TestAnS3RefreshNamesTheObject(t *testing.T) {
	t.Parallel()
	schema := schemaFixture()
	schema.Spec.Desired = operatorv1alpha1.SourceSpec{S3: s3SourceSpec()}
	schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{S3: s3Revision(schema.Spec.Desired.S3, s3VersionA, httpsDownloadA, gitContentOne)}
	markSourceRefreshPending(schema)
	resolved := meta.FindStatusCondition(schema.Status.Conditions, operatorv1alpha1.ConditionArtifactResolved)
	if resolved == nil || resolved.Reason != string(operatorv1alpha1.ReasonRefreshing) || !strings.Contains(resolved.Message, "Reading the S3 object again") {
		t.Fatalf("ArtifactResolved = %#v, want a refresh of the S3 source", resolved)
	}
	markSourceRefreshFailed(schema)
	resolved = meta.FindStatusCondition(schema.Status.Conditions, operatorv1alpha1.ConditionArtifactResolved)
	if resolved == nil || resolved.Reason != string(operatorv1alpha1.ReasonRefreshFailed) || !strings.Contains(resolved.Message, "S3 object could not be read again") {
		t.Fatalf("ArtifactResolved = %#v, want the failed S3 refresh with the retained content", resolved)
	}
}

// A plan computed from an S3 object is published for approval without a
// verification policy, as an HTTPS one is, and binds the object's revision.
func TestAnS3PlanIsPublishedWithoutAVerificationPolicy(t *testing.T) {
	t.Parallel()
	schema := schemaFixture()
	schema.Spec.Desired = operatorv1alpha1.SourceSpec{S3: s3SourceSpec()}
	schema.Spec.Policy.Apply = operatorv1alpha1.ApplyPolicyOnApproval
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhasePlanning
	schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{S3: s3Revision(schema.Spec.Desired.S3, s3VersionA, httpsDownloadA, gitContentOne)}
	schema.Status.Target = operatorv1alpha1.TargetStatus{
		CoordinationDigest: testCoordinationDigest, IdentityDigest: testDigest, DriftReportDigest: safetyOtherDigest,
	}
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationPlan, ID: "s3-plan-operation", JobName: "s3-plan-job", JobUID: "job-uid",
		StartedAt: metav1.Now(), Attempt: 1, PlanSealPublicKeyDigest: planSealPublicKeyDigest(testSchemaSealKey.PublicKey()),
	}
	bindActiveInput(t, schema)
	planDocument := safetyPlanDocument(t, "observed-state")
	frame := safetyRunnerFrame(t, runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationPlan, OperationID: schema.Status.ActiveOperation.ID,
		Stdout:             safetySealPlan(t, schema.Status.ActiveOperation, planDocument),
		CoordinationDigest: schema.Status.Target.CoordinationDigest, TargetIdentityDigest: schema.Status.Target.IdentityDigest,
		PlanContentDigest: fingerprint.DigestBytes(planDocument), PlanOutcome: runner.PlanOutcomeChanges,
	})
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod)
	reconciler.Plans = planstore.Store{Client: api, Reader: api}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}
	for pass := range 4 {
		if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	actual := safetyGetSchema(t, api, schema)
	verified := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionArtifactVerified)
	if actual.Status.Phase != operatorv1alpha1.PhaseAwaitingApproval || actual.Status.Plan == nil || verified != nil {
		t.Fatalf("phase %s, plan %#v, ArtifactVerified %#v; want a plan awaiting approval and no verification",
			actual.Status.Phase, actual.Status.Plan, verified)
	}
	plan := &operatorv1alpha1.PtahSchemaPlan{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: actual.Namespace, Name: actual.Status.Plan.Name}, plan); err != nil {
		t.Fatal(err)
	}
	object := schema.Spec.Desired.S3
	if source := plan.Spec.Source; source == nil || source.Kind != operatorv1alpha1.SourceKindS3 ||
		source.ContentDigest != gitContentOne ||
		source.Revision != sourcebinding.S3Revision(object.Endpoint, object.Bucket, object.Key, object.Path) {
		t.Fatalf("the plan binds %#v, want the S3 object's content", plan.Spec.Source)
	}
}

// The check the Plan harvest makes holds an S3 source to the object the
// spec names now: a status resolved from another object or version is
// refused.
func TestTheSourceCheckHoldsAnS3SourceToTheSpec(t *testing.T) {
	t.Parallel()
	for name, row := range map[string]struct {
		edit    func(*operatorv1alpha1.S3SourceSpec)
		refused bool
	}{
		"the object it was resolved from":  {edit: func(*operatorv1alpha1.S3SourceSpec) {}},
		"the version it was resolved from": {edit: func(s *operatorv1alpha1.S3SourceSpec) { s.VersionID = s3VersionA }},
		"another object":                   {edit: func(s *operatorv1alpha1.S3SourceSpec) { s.Digest = httpsDownloadB }, refused: true},
		"another version":                  {edit: func(s *operatorv1alpha1.S3SourceSpec) { s.VersionID = s3VersionB }, refused: true},
		"another region":                   {edit: func(s *operatorv1alpha1.S3SourceSpec) { s.Region = "eu-west-1" }, refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			schema := schemaFixture()
			schema.Spec.Desired = operatorv1alpha1.SourceSpec{S3: s3SourceSpec()}
			schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{S3: s3Revision(schema.Spec.Desired.S3, s3VersionA, httpsDownloadA, gitContentOne)}
			row.edit(schema.Spec.Desired.S3)
			reconciler, _ := fakeReconciler(t, staticLogs{}, schema)
			err := reconciler.verifiedSourcePolicyError(context.Background(), schema)
			if row.refused != (err != nil) || (err != nil && !strings.Contains(err.Error(), "no longer the S3 object")) {
				t.Fatalf("verifiedSourcePolicyError() = %v, want refused %v", err, row.refused)
			}
		})
	}
}

// The fetch step's refusal record reaches the failure as its code and
// sentence; a record under another kind's code from the S3 step says
// nothing more than the step's exit status.
func TestTheS3FetchStepSaysWhyItsAccessWasRefused(t *testing.T) {
	t.Parallel()
	refusal := `the S3 credential is granted for the bucket "orders-private", not the source's`
	for _, row := range []struct {
		name   string
		status corev1.ContainerStatus
		want   string
	}{
		{name: "an S3 record", status: accessStepStatus(t, "fetch-s3-source", runner.CodeInvalidS3Access, refusal),
			want: "the fetch-s3-source step refused the source's access, so the run never started: invalid_s3_access: " + refusal},
		{name: "an HTTPS record from the S3 step", status: accessStepStatus(t, "fetch-s3-source", runner.CodeInvalidHTTPSAccess, "FRAME-TEXT"),
			want: "the fetch-s3-source step exited 2, so the run never started"},
		{name: "an S3 record from the HTTPS step", status: accessStepStatus(t, "fetch-https-source", runner.CodeInvalidS3Access, "FRAME-TEXT"),
			want: "the fetch-https-source step exited 2, so the run never started"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			pod := &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{
				{Name: "install-runner", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
				row.status,
			}}}
			if got := initBoundaryOf([]*corev1.Pod{pod}); got != row.want {
				t.Fatalf("initBoundaryOf() = %q, want %q", got, row.want)
			}
		})
	}
}
