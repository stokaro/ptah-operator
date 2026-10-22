package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
)

const (
	gitCommitA    = "3f1c2b0a9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a"
	gitCommitB    = "9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e0d"
	gitContentOne = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	gitContentTwo = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

func gitSourceSpec() *operatorv1alpha1.GitSourceSpec {
	return &operatorv1alpha1.GitSourceSpec{
		URL: "https://git.example.com/acme/app.git", Ref: operatorv1alpha1.GitRef{Branch: "main"}, Path: "db/schema.hcl",
		AuthFrom: &operatorv1alpha1.GitAuthSource{Name: "git-token"},
	}
}

func gitResolveFrame(t *testing.T, operationID, commit, content string) []byte {
	t.Helper()
	frame, err := runner.MarshalFrame(runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationResolve,
		OperationID: operationID, ResolvedCommit: commit, ResolvedContentDigest: content,
	})
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

// gitResolvingSchema is a schema whose Git Resolve Job has finished, with the
// resolution before it, if any.
func gitResolvingSchema(t *testing.T, previous *operatorv1alpha1.GitSourceRevision) *operatorv1alpha1.PtahSchema {
	t.Helper()
	schema := schemaFixture()
	schema.Spec.Desired = operatorv1alpha1.SourceSpec{Git: gitSourceSpec()}
	schema.Finalizers = []string{activeOperationFinalizer}
	schema.Status.Phase = operatorv1alpha1.PhaseResolving
	schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{Git: previous}
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationResolve, ID: testDigest, InputFingerprint: testDigest,
		JobName: "resolve-job", JobUID: "job-uid", StartedAt: metav1.Now(), Attempt: 1,
	}
	bindActiveInput(t, schema)
	return schema
}

func reconcileGitResolve(t *testing.T, schema *operatorv1alpha1.PtahSchema, frame []byte) *operatorv1alpha1.PtahSchema {
	t.Helper()
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	reconciler, api := fakeReconciler(t, staticLogs{content: frame}, schema, job, pod)
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(schema)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKeyFromObject(schema), actual); err != nil {
		t.Fatal(err)
	}
	return actual
}

// A Git source has no verification policy: its resolution goes straight to
// observation, records what it answered, and claims no verification.
func TestGitResolveResultGoesStraightToObservation(t *testing.T) {
	t.Parallel()
	schema := gitResolvingSchema(t, nil)
	// The source was an OCI artifact before; its verification says nothing
	// about the commit.
	meta.SetStatusCondition(&schema.Status.Conditions, metav1.Condition{
		Type: operatorv1alpha1.ConditionArtifactVerified, Status: metav1.ConditionTrue,
		Reason: string(operatorv1alpha1.ReasonPolicySatisfied), Message: "the earlier OCI artifact passed its policy",
	})
	actual := reconcileGitResolve(t, schema, gitResolveFrame(t, testDigest, gitCommitA, gitContentOne))
	if actual.Status.ActiveOperation != nil || actual.Status.Phase != operatorv1alpha1.PhaseObserving {
		t.Fatalf("phase %s, operation %#v; want Observing and no claim", actual.Status.Phase, actual.Status.ActiveOperation)
	}
	git := actual.Status.Source.Git
	spec := gitSourceSpec()
	if git == nil || git.Commit != gitCommitA || git.ContentDigest != gitContentOne || git.URL != spec.URL ||
		git.Ref != spec.Ref || git.Path != spec.Path || actual.Status.Source.Digest != "" || actual.Status.Source.Verified {
		t.Fatalf("source status = %#v", actual.Status.Source)
	}
	resolved := meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionArtifactResolved)
	if resolved == nil || resolved.Status != metav1.ConditionTrue || resolved.Reason != string(operatorv1alpha1.ReasonCommitResolved) ||
		!strings.Contains(resolved.Message, gitCommitA) {
		t.Fatalf("ArtifactResolved = %#v", resolved)
	}
	if meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionArtifactVerified) != nil {
		t.Fatal("a Git source reports a verification it never had")
	}
	access := sourcebinding.SchemaAccess(actual.Spec.Desired, actual.Status.Source)
	if access == nil || access.Git == nil || access.Git.Commit != gitCommitA || access.Git.AuthFrom == nil {
		t.Fatalf("the next operation would read %#v", access)
	}
}

// A ref that moved to a commit holding the same selected files keeps the
// plan; other files retire it.
func TestGitResolveRetiresThePlanOnlyForOtherContent(t *testing.T) {
	t.Parallel()
	spec := gitSourceSpec()
	previous := &operatorv1alpha1.GitSourceRevision{
		URL: spec.URL, Ref: spec.Ref, Path: spec.Path, Commit: gitCommitA, ContentDigest: gitContentOne,
	}
	plan := &operatorv1alpha1.CurrentPlanStatus{Name: "orders-plan", UID: "plan-uid", Fingerprint: testDigest}
	for name, row := range map[string]struct {
		content string
		kept    bool
	}{
		"another commit with the same files": {content: gitContentOne, kept: true},
		"another commit with other files":    {content: gitContentTwo, kept: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			schema := gitResolvingSchema(t, previous.DeepCopy())
			schema.Status.Plan = plan.DeepCopy()
			actual := reconcileGitResolve(t, schema, gitResolveFrame(t, testDigest, gitCommitB, row.content))
			if actual.Status.Source.Git == nil || actual.Status.Source.Git.Commit != gitCommitB {
				t.Fatalf("source status = %#v, want the new commit", actual.Status.Source)
			}
			if kept := actual.Status.Plan != nil; kept != row.kept {
				t.Fatalf("plan kept = %v, want %v", kept, row.kept)
			}
		})
	}
}

// A resolution that does not answer the Git source is retried, never
// recorded.
func TestGitResolveRefusesAResultThatAnswersSomethingElse(t *testing.T) {
	t.Parallel()
	ociFrame, err := runner.MarshalFrame(runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationResolve, OperationID: testDigest,
		ResolvedDigest: testDigest, ResolvedReference: "oci://registry.example/team/schema@" + testDigest,
		ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json", ResolvedSize: 321,
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, row := range map[string]struct {
		frame []byte
		edit  func(*operatorv1alpha1.PtahSchema)
	}{
		"an OCI descriptor": {frame: ociFrame},
		"another commit than the ref names": {
			frame: gitResolveFrame(t, testDigest, gitCommitB, gitContentOne),
			edit: func(schema *operatorv1alpha1.PtahSchema) {
				schema.Spec.Desired.Git.Ref = operatorv1alpha1.GitRef{Commit: gitCommitA}
				bindActiveInput(t, schema)
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			schema := gitResolvingSchema(t, nil)
			if row.edit != nil {
				row.edit(schema)
			}
			actual := reconcileGitResolve(t, schema, row.frame)
			if actual.Status.Source.Git != nil || actual.Status.Source.Digest != "" || actual.Status.Phase == operatorv1alpha1.PhaseObserving {
				t.Fatalf("a refused resolution was recorded: phase %s, source %#v", actual.Status.Phase, actual.Status.Source)
			}
		})
	}
}

func TestProofReadsTheCurrentSourceComparesTheContent(t *testing.T) {
	t.Parallel()
	binding := &operatorv1alpha1.GitSourceBinding{URL: "https://git.example.com/acme/app.git", Path: "db", Commit: gitCommitA, ContentDigest: gitContentOne}
	status := operatorv1alpha1.SchemaSourceStatus{Git: &operatorv1alpha1.GitSourceRevision{
		URL: binding.URL, Path: binding.Path, Commit: gitCommitB, ContentDigest: gitContentOne,
	}}
	if !proofReadsTheCurrentSource(status, operatorv1alpha1.SourceAccessBinding{Git: binding}) {
		t.Fatal("a proof of the same files at another commit was refused")
	}
	for name, edit := range map[string]func(*operatorv1alpha1.SchemaSourceStatus){
		"other files":        func(s *operatorv1alpha1.SchemaSourceStatus) { s.Git.ContentDigest = gitContentTwo },
		"another path":       func(s *operatorv1alpha1.SchemaSourceStatus) { s.Git.Path = "db/other" },
		"another repository": func(s *operatorv1alpha1.SchemaSourceStatus) { s.Git.URL = "https://git.example.com/acme/other.git" },
		"no Git resolution":  func(s *operatorv1alpha1.SchemaSourceStatus) { s.Git = nil },
	} {
		moved := *status.DeepCopy()
		edit(&moved)
		if proofReadsTheCurrentSource(moved, operatorv1alpha1.SourceAccessBinding{Git: binding}) {
			t.Errorf("a proof was admitted against %s", name)
		}
	}
	if proofReadsTheCurrentSource(status, operatorv1alpha1.SourceAccessBinding{}) {
		t.Error("a proof that reads no source was admitted")
	}
}

// A migration directory in Git is resolved by a Job and read by the history
// claim that follows, with no Verify step.
func TestMigrationGitResolveResultGoesStraightToTheHistoryRead(t *testing.T) {
	t.Parallel()
	migration := migrationFixture()
	spec := gitSourceSpec()
	spec.Path = "db/migrations"
	migration.Spec.Artifact = operatorv1alpha1.SourceSpec{Git: spec}
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
	frame := gitResolveFrame(t, operation.ID, gitCommitA, gitContentOne)
	reconciler, api := fakeMigrationReconciler(t, staticLogs{content: frame}, migration, job, pod)

	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	actual := readMigration(t, api, migration)
	if actual.Status.ActiveOperation != nil || actual.Status.Phase != operatorv1alpha1.MigrationPhaseReading {
		t.Fatalf("phase %s, operation %#v; want Reading and no claim", actual.Status.Phase, actual.Status.ActiveOperation)
	}
	git := actual.Status.Artifact.Git
	if git == nil || git.Commit != gitCommitA || git.ContentDigest != gitContentOne || git.Path != "db/migrations" ||
		actual.Status.Artifact.Digest != "" || actual.Status.ConfigMap != nil {
		t.Fatalf("artifact = %#v", actual.Status.Artifact)
	}
	if meta.FindStatusCondition(actual.Status.Conditions, operatorv1alpha1.ConditionMigrationArtifactVerified) != nil {
		t.Fatal("a Git source reports a verification it never had")
	}
	if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	history := readMigration(t, api, migration).Status.ActiveOperation
	if history == nil || history.Type != operatorv1alpha1.MigrationOperationHistory || history.Source == nil ||
		history.Source.Git == nil || history.Source.Git.Commit != gitCommitA || history.Source.Git.AuthFrom == nil {
		t.Fatalf("history claim = %#v, want the resolved commit", history)
	}
}
