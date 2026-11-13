package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
)

const (
	httpsDownloadA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	httpsDownloadB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func httpsSourceSpec() *operatorv1alpha1.HTTPSSourceSpec {
	return &operatorv1alpha1.HTTPSSourceSpec{
		URL: "https://artifacts.example.com/acme/app-1.4.0.zip", Digest: httpsDownloadA,
		Format: operatorv1alpha1.HTTPSFormatZip, Path: "app-1.4.0/db/schema.hcl",
		AuthFrom: &operatorv1alpha1.HTTPSAuthSource{Name: "artifacts-token"},
	}
}

func httpsResolveFrame(t *testing.T, operationID, download, content string, size int64) []byte {
	t.Helper()
	frame, err := runner.MarshalFrame(runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationResolve, OperationID: operationID,
		ResolvedDownloadDigest: download, ResolvedContentDigest: content, ResolvedContentBytes: size,
	})
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

// httpsResolvingSchema is a schema whose HTTPS Resolve Job has finished, with
// the resolution before it, if any.
func httpsResolvingSchema(t *testing.T, spec *operatorv1alpha1.HTTPSSourceSpec, previous *operatorv1alpha1.HTTPSSourceRevision) *operatorv1alpha1.PtahSchema {
	t.Helper()
	schema := gitResolvingSchema(t, nil)
	schema.Spec.Desired = operatorv1alpha1.SourceSpec{HTTPS: spec}
	schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{HTTPS: previous}
	bindActiveInput(t, schema)
	return schema
}

// An HTTPS source has no verification policy: the pinned digest is the trust
// decision, and the Resolve Job held the download to it. The resolution goes
// straight to observation and names both digests.
func TestHTTPSResolveResultGoesStraightToObservation(t *testing.T) {
	t.Parallel()
	schema := httpsResolvingSchema(t, httpsSourceSpec(), nil)
	meta.SetStatusCondition(&schema.Status.Conditions, metav1.Condition{
		Type: operatorv1alpha1.ConditionArtifactVerified, Status: metav1.ConditionTrue,
		Reason: string(operatorv1alpha1.ReasonPolicySatisfied), Message: "the earlier OCI artifact passed its policy",
	})
	actual := reconcileGitResolve(t, schema, httpsResolveFrame(t, testDigest, httpsDownloadA, gitContentOne, 0))
	if actual.Status.ActiveOperation != nil || actual.Status.Phase != operatorv1alpha1.PhaseObserving {
		t.Fatalf("phase %s, operation %#v; want Observing and no claim", actual.Status.Phase, actual.Status.ActiveOperation)
	}
	spec := httpsSourceSpec()
	want := operatorv1alpha1.HTTPSSourceRevision{
		URL: spec.URL, Format: spec.Format, Path: spec.Path, Digest: httpsDownloadA, ContentDigest: gitContentOne,
	}
	if got := actual.Status.Source.HTTPS; got == nil || *got != want || actual.Status.Source.Digest != "" ||
		actual.Status.Source.Git != nil || actual.Status.Source.Verified {
		t.Fatalf("source status = %#v, want %#v alone", actual.Status.Source, want)
	}
	resolved := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionArtifactResolved)
	if resolved == nil || resolved.Status != metav1.ConditionTrue || resolved.Reason != string(operatorv1alpha1.ReasonDownloadVerified) ||
		!strings.Contains(resolved.Message, httpsDownloadA) || !strings.Contains(resolved.Message, gitContentOne) {
		t.Fatalf("ArtifactResolved = %#v, want DownloadVerified naming both digests", resolved)
	}
	if meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionArtifactVerified) != nil {
		t.Fatal("an HTTPS source reports a verification it never had")
	}
	access := sourcebinding.SchemaAccess(actual.Spec.Desired, actual.Status.Source)
	if access == nil || access.HTTPS == nil || access.HTTPS.Digest != httpsDownloadA ||
		access.HTTPS.ContentDigest != gitContentOne || access.HTTPS.AuthFrom == nil {
		t.Fatalf("the next operation would read %#v", access)
	}
}

// An archive published again under a new digest whose selected files did not
// change keeps the plan once the spec names the new digest; other files,
// another path or another URL retire it.
func TestHTTPSResolveRetiresThePlanOnlyForOtherContent(t *testing.T) {
	t.Parallel()
	spec := httpsSourceSpec()
	spec.Digest = httpsDownloadB
	plan := &operatorv1alpha1.CurrentPlanStatus{Name: "orders-plan", UID: "plan-uid", Fingerprint: testDigest}
	for name, row := range map[string]struct {
		content string
		edit    func(*operatorv1alpha1.HTTPSSourceRevision)
		kept    bool
	}{
		"a new download with the same files": {content: gitContentOne, kept: true},
		"a new download with other files":    {content: gitContentTwo},
		"the same files under another path": {
			content: gitContentOne,
			edit:    func(r *operatorv1alpha1.HTTPSSourceRevision) { r.Path = "app-1.3.0/db/schema.hcl" },
		},
		"the same files from another URL": {
			content: gitContentOne,
			edit: func(r *operatorv1alpha1.HTTPSSourceRevision) {
				r.URL = "https://artifacts.example.com/acme/app-1.3.0.zip"
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			previous := &operatorv1alpha1.HTTPSSourceRevision{
				URL: spec.URL, Format: spec.Format, Path: spec.Path, Digest: httpsDownloadA, ContentDigest: gitContentOne,
			}
			if row.edit != nil {
				row.edit(previous)
			}
			schema := httpsResolvingSchema(t, spec.DeepCopy(), previous)
			schema.Status.Plan = plan.DeepCopy()
			actual := reconcileGitResolve(t, schema, httpsResolveFrame(t, testDigest, httpsDownloadB, row.content, 0))
			if got := actual.Status.Source.HTTPS; got == nil || got.Digest != httpsDownloadB || got.ContentDigest != row.content {
				t.Fatalf("source status = %#v, want the new download", actual.Status.Source)
			}
			if kept := actual.Status.Plan != nil; kept != row.kept {
				t.Fatalf("plan kept = %v, want %v", kept, row.kept)
			}
		})
	}
}

// A resolution that does not answer the download the spec pins is retried,
// never recorded: an OCI descriptor, a Git commit, or another download.
func TestHTTPSResolveRefusesAResultThatAnswersSomethingElse(t *testing.T) {
	t.Parallel()
	ociFrame, err := runner.MarshalFrame(runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationResolve, OperationID: testDigest,
		ResolvedDigest: testDigest, ResolvedReference: "oci://registry.example/team/schema@" + testDigest,
		ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json", ResolvedSize: 321,
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, frame := range map[string][]byte{
		"an OCI descriptor":   ociFrame,
		"a Git commit":        gitResolveFrame(t, testDigest, gitCommitA, gitContentOne),
		"another download":    httpsResolveFrame(t, testDigest, httpsDownloadB, gitContentOne, 0),
		"the pinned download": httpsResolveFrame(t, testDigest, httpsDownloadA, gitContentOne, 0),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			actual := reconcileGitResolve(t, httpsResolvingSchema(t, httpsSourceSpec(), nil), frame)
			recorded := actual.Status.Source.HTTPS != nil
			// The pinned download is the control: the same fixture records
			// it, so the refusals above are about the answer.
			if want := name == "the pinned download"; recorded != want || (recorded != (actual.Status.Phase == operatorv1alpha1.PhaseObserving)) {
				t.Fatalf("recorded %v, phase %s; want recorded %v: source %#v", recorded, actual.Status.Phase, want, actual.Status.Source)
			}
			if actual.Status.Source.Git != nil || actual.Status.Source.Digest != "" {
				t.Fatalf("an HTTPS source recorded another kind: %#v", actual.Status.Source)
			}
		})
	}
}

func TestProofReadsTheCurrentSourceComparesTheHTTPSContent(t *testing.T) {
	t.Parallel()
	binding := &operatorv1alpha1.HTTPSSourceBinding{
		URL: "https://artifacts.example.com/acme/app.zip", Digest: httpsDownloadA, Format: operatorv1alpha1.HTTPSFormatZip,
		Path: "db", ContentDigest: gitContentOne,
	}
	status := operatorv1alpha1.SchemaSourceStatus{HTTPS: &operatorv1alpha1.HTTPSSourceRevision{
		URL: binding.URL, Format: binding.Format, Path: binding.Path, Digest: httpsDownloadB, ContentDigest: gitContentOne,
	}}
	if !proofReadsTheCurrentSource(status, operatorv1alpha1.SourceAccessBinding{HTTPS: binding}) {
		t.Fatal("a proof of the same files in another download was refused")
	}
	for name, edit := range map[string]func(*operatorv1alpha1.SchemaSourceStatus){
		"other files":  func(s *operatorv1alpha1.SchemaSourceStatus) { s.HTTPS.ContentDigest = gitContentTwo },
		"another path": func(s *operatorv1alpha1.SchemaSourceStatus) { s.HTTPS.Path = "db/other" },
		"another URL": func(s *operatorv1alpha1.SchemaSourceStatus) {
			s.HTTPS.URL = "https://artifacts.example.com/acme/other.zip"
		},
		"no HTTPS resolution": func(s *operatorv1alpha1.SchemaSourceStatus) { s.HTTPS = nil },
		"a Git resolution of the same files": func(s *operatorv1alpha1.SchemaSourceStatus) {
			s.Git = &operatorv1alpha1.GitSourceRevision{URL: binding.URL, Path: binding.Path, Commit: gitCommitA, ContentDigest: gitContentOne}
			s.HTTPS = nil
		},
	} {
		moved := *status.DeepCopy()
		edit(&moved)
		if proofReadsTheCurrentSource(moved, operatorv1alpha1.SourceAccessBinding{HTTPS: binding}) {
			t.Errorf("a proof was admitted against %s", name)
		}
	}
}

// An HTTPS host the policy does not allow gets no Resolve Job: the credential
// is never sent there. A policy that names the host lets the same schema
// claim its Resolve.
func TestASourcePolicyRefusesAnHTTPSHostBeforeAnyJob(t *testing.T) {
	t.Parallel()
	for name, row := range map[string]struct {
		allowed string
		refused bool
	}{
		"another host": {allowed: "artifacts.internal.example", refused: true},
		"its host":     {allowed: "artifacts.example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			schema := schemaFixture()
			schema.Spec.Desired = operatorv1alpha1.SourceSpec{HTTPS: httpsSourceSpec()}
			withSourcePolicy(&schema.Spec.Desired, "source-policy")
			reconciler, api := fakeReconciler(t, staticLogs{}, schema,
				sourcePolicyConfigMap("source-policy", "policy-uid", "allowedHTTPSHosts:\n  - "+row.allowed+"\n"))
			actual := reconcileSchema(t, reconciler, api, schema)
			jobs := &batchv1.JobList{}
			if err := api.List(context.Background(), jobs); err != nil {
				t.Fatal(err)
			}
			if !row.refused {
				if actual.Status.ActiveOperation == nil || actual.Status.ActiveOperation.Type != operatorv1alpha1.OperationResolve {
					t.Fatalf("an allowed host claimed %#v, want a Resolve", actual.Status.ActiveOperation)
				}
				return
			}
			requireRefused(t, actual, "allowedHTTPSHosts does not name artifacts.example.com")
			if actual.Status.ActiveOperation != nil || len(jobs.Items) != 0 {
				t.Fatalf("a refused host was claimed: %#v, %d Jobs", actual.Status.ActiveOperation, len(jobs.Items))
			}
			resolved := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionArtifactResolved)
			if resolved == nil || !strings.Contains(resolved.Message, "The HTTPS source") {
				t.Fatalf("ArtifactResolved = %#v, want the HTTPS source named", resolved)
			}
		})
	}
}

// What an HTTPS resolution selected is held to the size rule before it is
// recorded.
func TestASourcePolicyRefusesAnHTTPSResolutionThatHoldsTooMuch(t *testing.T) {
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
			schema := httpsResolvingSchema(t, httpsSourceSpec(), nil)
			withSourcePolicy(&schema.Spec.Desired, "source-policy")
			bindActiveInput(t, schema)
			frame := httpsResolveFrame(t, testDigest, httpsDownloadA, gitContentOne, row.bytes)
			job, pod := terminalWorkload(schema, batchv1.JobComplete)
			reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod,
				sourcePolicyConfigMap("source-policy", "policy-uid", "maxContentBytes: 100\n"))
			actual := reconcileSchema(t, reconciler, api, schema)
			if recorded := actual.Status.Source.HTTPS != nil; recorded != row.recorded {
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

// httpsResolvingMigration is a migration whose HTTPS Resolve Job has finished
// with frame, and the reconciler that will read it.
func httpsResolvingMigration(t *testing.T, policy string, frame func(operationID string) []byte) (*MigrationReconciler, *operatorv1alpha1.PtahMigration, func() *operatorv1alpha1.PtahMigration) {
	t.Helper()
	migration := migrationFixture()
	spec := httpsSourceSpec()
	spec.URL, spec.Format, spec.Path = "https://artifacts.example.com/acme/orders-1.4.0.tar.gz", operatorv1alpha1.HTTPSFormatTarGzip, "orders-1.4.0/db/migrations"
	migration.Spec.Artifact = operatorv1alpha1.SourceSpec{HTTPS: spec}
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

// A migration directory in an HTTPS archive is resolved by a Job and read by
// the history claim that follows, with no Verify step.
func TestMigrationHTTPSResolveResultGoesStraightToTheHistoryRead(t *testing.T) {
	t.Parallel()
	reconciler, migration, read := httpsResolvingMigration(t, "", func(id string) []byte {
		return httpsResolveFrame(t, id, httpsDownloadA, gitContentOne, 0)
	})
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := read()
	if actual.Status.ActiveOperation != nil || actual.Status.Phase != operatorv1alpha1.MigrationPhaseReading {
		t.Fatalf("phase %s, operation %#v; want Reading and no claim", actual.Status.Phase, actual.Status.ActiveOperation)
	}
	download := actual.Status.Artifact.HTTPS
	if download == nil || download.Digest != httpsDownloadA || download.ContentDigest != gitContentOne ||
		download.Format != operatorv1alpha1.HTTPSFormatTarGzip || download.Path != "orders-1.4.0/db/migrations" ||
		actual.Status.Artifact.Digest != "" || actual.Status.Artifact.Git != nil || actual.Status.ConfigMap != nil {
		t.Fatalf("artifact = %#v", actual.Status.Artifact)
	}
	if meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationArtifactVerified) != nil {
		t.Fatal("an HTTPS source reports a verification it never had")
	}
	progressing := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationProgressing)
	if progressing == nil || progressing.Reason != string(operatorv1alpha1.ReasonDownloadVerified) ||
		!strings.Contains(progressing.Message, httpsDownloadA) || !strings.Contains(progressing.Message, gitContentOne) {
		t.Fatalf("Progressing = %#v, want DownloadVerified naming both digests", progressing)
	}
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	history := read().Status.ActiveOperation
	if history == nil || history.Type != operatorv1alpha1.MigrationOperationHistory || history.Source == nil ||
		history.Source.HTTPS == nil || history.Source.HTTPS.Digest != httpsDownloadA || history.Source.HTTPS.AuthFrom == nil ||
		history.Source.Digest != "" {
		t.Fatalf("history claim = %#v, want the resolved download with its credential selector", history)
	}
}

// A migration's HTTPS resolution is held to its source policy before it is
// recorded, as a schema's is.
func TestAMigrationHTTPSResolutionIsHeldToItsSourcePolicy(t *testing.T) {
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
			reconciler, migration, read := httpsResolvingMigration(t, "maxContentBytes: 100\n", func(id string) []byte {
				return httpsResolveFrame(t, id, httpsDownloadA, gitContentOne, row.bytes)
			})
			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := read()
			recorded := actual.Status.Artifact != nil && actual.Status.Artifact.HTTPS != nil
			if recorded != row.recorded {
				t.Fatalf("recorded %v, want %v: status %#v", recorded, row.recorded, actual.Status)
			}
			if recorded && (actual.Status.SourcePolicy == nil || actual.Status.SourcePolicy.UID != "policy-uid") {
				t.Fatalf("the resolution was recorded without its policy: %#v", actual.Status.SourcePolicy)
			}
		})
	}
}

// A migration's HTTPS host the policy does not allow gets no Resolve Job.
func TestAMigrationSourcePolicyRefusesAnHTTPSHostBeforeAnyJob(t *testing.T) {
	t.Parallel()
	for name, row := range map[string]struct {
		allowed string
		refused bool
	}{
		"another host": {allowed: "artifacts.internal.example", refused: true},
		"its host":     {allowed: "artifacts.example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			migration := migrationFixture()
			migration.Spec.Artifact = operatorv1alpha1.SourceSpec{HTTPS: httpsSourceSpec()}
			migration.Spec.Artifact.HTTPS.Format, migration.Spec.Artifact.HTTPS.Path = operatorv1alpha1.HTTPSFormatZip, "db/migrations"
			withSourcePolicy(&migration.Spec.Artifact, "source-policy")
			reconciler, api := fakeMigrationReconciler(t, staticLogs{}, migration,
				sourcePolicyConfigMap("source-policy", "policy-uid", "allowedHTTPSHosts:\n  - "+row.allowed+"\n"))
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
					t.Fatalf("an allowed host claimed %#v, want a Resolve", actual.Status.ActiveOperation)
				}
				return
			}
			ready := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationReady)
			if actual.Status.Phase != operatorv1alpha1.MigrationPhaseFailed || actual.Status.ActiveOperation != nil || len(jobs.Items) != 0 ||
				ready == nil || !strings.Contains(ready.Message, "allowedHTTPSHosts does not name artifacts.example.com") {
				t.Fatalf("phase %s, claim %#v, %d Jobs, Ready %#v; want Failed naming the rule before any Job",
					actual.Status.Phase, actual.Status.ActiveOperation, len(jobs.Items), ready)
			}
		})
	}
}
