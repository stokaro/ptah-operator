package controller

import (
	"context"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/sourcecontent"
	"github.com/stokaro/ptah-operator/internal/sourcesnapshot"
)

// ownedSnapshot is the snapshot of one file a ConfigMap source left for
// owner, and the binding that names it.
func ownedSnapshot(t *testing.T, owner sourcesnapshot.Owner, content string) (*corev1.ConfigMap, operatorv1alpha1.SourceSnapshotBinding) {
	t.Helper()
	configMap, digest, err := sourcesnapshot.Build("team-a", owner, []sourcecontent.File{{Path: "schema.sql", Data: []byte(content)}})
	if err != nil {
		t.Fatal(err)
	}
	configMap.UID = types.UID("uid-" + configMap.Name)
	return configMap, operatorv1alpha1.SourceSnapshotBinding{
		Name: configMap.Name, UID: configMap.UID, ContentDigest: digest, SchemaFile: "schema.sql",
	}
}

func snapshotNamesIn(t *testing.T, api client.Client) []string {
	t.Helper()
	var names []string
	for _, snapshot := range snapshots(t, api) {
		names = append(names, snapshot.Name)
	}
	slices.Sort(names)
	return names
}

func ociResolveFrame(t *testing.T, operationID, repository string) []byte {
	t.Helper()
	frame, err := runner.MarshalFrame(runner.Result{
		ProtocolVersion: runner.ProtocolVersion, Operation: runner.OperationResolve, OperationID: operationID,
		ResolvedDigest: testDigest, ResolvedReference: "oci://registry.example/team/" + repository + "@" + testDigest,
		ResolvedMediaType: "application/vnd.oci.image.manifest.v1+json", ResolvedSize: 321,
	})
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

// A schema whose source moved from a ConfigMap to Git or OCI deletes the
// snapshot the ConfigMap left once the resolution that replaces it is stored.
// A snapshot under the same label that another schema owns stays, and so
// does the source ConfigMap.
func TestResolveOfAnotherSourceRetiresTheSchemaSnapshot(t *testing.T) {
	t.Parallel()
	for name, row := range map[string]struct {
		desired operatorv1alpha1.SourceSpec
		frame   []byte
		stored  func(operatorv1alpha1.SchemaSourceStatus) bool
	}{
		"Git": {
			desired: operatorv1alpha1.SourceSpec{Git: gitSourceSpec()},
			frame:   gitResolveFrame(t, testDigest, gitCommitA, gitContentOne),
			stored: func(source operatorv1alpha1.SchemaSourceStatus) bool {
				return source.Git != nil && source.Git.Commit == gitCommitA
			},
		},
		"OCI": {
			desired: schemaFixture().Spec.Desired,
			frame:   ociResolveFrame(t, testDigest, "schema"),
			stored: func(source operatorv1alpha1.SchemaSourceStatus) bool {
				return source.Digest == testDigest
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			schema := gitResolvingSchema(t, nil)
			schema.Spec.Desired = row.desired
			left, binding := ownedSnapshot(t, sourcesnapshot.Owner{Kind: "PtahSchema", Name: schema.Name, UID: schema.UID}, "CREATE TABLE orders (id int);\n")
			// A schema of the same name deleted and created again labels its
			// snapshots the same way; only the owner reference tells them apart.
			earlier, _ := ownedSnapshot(t, sourcesnapshot.Owner{Kind: "PtahSchema", Name: schema.Name, UID: "earlier-schema-uid"}, "CREATE TABLE old (id int);\n")
			schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
				ConfigMap: &operatorv1alpha1.ConfigMapSourceRevision{Name: "app-schema", UID: "configmap-uid", ResourceVersion: "3"},
				Snapshot:  &binding,
			}
			bindActiveInput(t, schema)
			job, pod := terminalWorkload(schema, batchv1.JobComplete)
			reconciler, api := fakeReconciler(t, staticLogs{content: row.frame}, schema, job, pod, left, earlier, sourceConfigMap("CREATE TABLE orders (id int);\n"))

			actual := reconcileSchema(t, reconciler, api, schema)
			if actual.Status.ActiveOperation != nil || !row.stored(actual.Status.Source) || actual.Status.Source.Snapshot != nil {
				t.Fatalf("the resolution was not stored: operation %#v, source %#v", actual.Status.ActiveOperation, actual.Status.Source)
			}
			if remaining := snapshotNamesIn(t, api); !slices.Equal(remaining, []string{earlier.Name}) {
				t.Fatalf("snapshots after the resolution = %v, want only the earlier schema's %s", remaining, earlier.Name)
			}
			source := &corev1.ConfigMap{}
			if err := api.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "app-schema"}, source); err != nil {
				t.Fatalf("the source ConfigMap went with the snapshot: %v", err)
			}
		})
	}
}

// The same for a migration. A migration that moves to OCI also drops the
// ConfigMap revision and the source policy the earlier source recorded: an
// OCI source reads neither.
func TestResolveOfAnotherSourceRetiresTheMigrationSnapshot(t *testing.T) {
	t.Parallel()
	gitSpec := gitSourceSpec()
	gitSpec.Path = "db/migrations"
	for name, row := range map[string]struct {
		artifact operatorv1alpha1.SourceSpec
		frame    func(operationID string) []byte
		stored   func(*operatorv1alpha1.SourceAccessBinding) bool
	}{
		"Git": {
			artifact: operatorv1alpha1.SourceSpec{Git: gitSpec},
			frame:    func(id string) []byte { return gitResolveFrame(t, id, gitCommitA, gitContentOne) },
			stored: func(artifact *operatorv1alpha1.SourceAccessBinding) bool {
				return artifact != nil && artifact.Git != nil && artifact.Git.Commit == gitCommitA
			},
		},
		"OCI": {
			artifact: migrationFixture().Spec.Artifact,
			frame:    func(id string) []byte { return ociResolveFrame(t, id, "migrations") },
			stored: func(artifact *operatorv1alpha1.SourceAccessBinding) bool {
				return artifact != nil && artifact.Digest == testDigest
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			migration := migrationFixture()
			migration.Spec.Artifact = row.artifact
			left, binding := ownedSnapshot(t, sourcesnapshot.Owner{Kind: "PtahMigration", Name: migration.Name, UID: migration.UID}, "CREATE TABLE orders (id int);\n")
			earlier, _ := ownedSnapshot(t, sourcesnapshot.Owner{Kind: "PtahMigration", Name: migration.Name, UID: "earlier-migration-uid"}, "CREATE TABLE old (id int);\n")
			migration.Status.Artifact = &operatorv1alpha1.SourceAccessBinding{Snapshot: &binding}
			migration.Status.ConfigMap = &operatorv1alpha1.ConfigMapSourceRevision{Name: "orders-migrations", UID: "configmap-uid", ResourceVersion: "3"}
			migration.Status.SourcePolicy = &operatorv1alpha1.SourcePolicyBinding{UID: "policy-uid", Digest: testDigest}
			migration.Status.ExecutionBinding = migrationExecutionBinding()
			migration.Status.Phase = operatorv1alpha1.MigrationPhaseResolving
			migration.Finalizers = []string{migrationOperationFinalizer}
			operation := migrationClaim(t, migration, operatorv1alpha1.MigrationOperationResolve)
			job, pod := terminalMigrationWorkload(migration, batchv1.JobComplete)
			reconciler, api := fakeMigrationReconciler(t, staticLogs{content: row.frame(operation.ID)}, migration, job, pod, left, earlier)

			if _, err := reconciler.Reconcile(context.Background(), migrationRequest(migration)); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			actual := readMigration(t, api, migration)
			if actual.Status.ActiveOperation != nil || !row.stored(actual.Status.Artifact) || actual.Status.Artifact.Snapshot != nil {
				t.Fatalf("the resolution was not stored: operation %#v, artifact %#v", actual.Status.ActiveOperation, actual.Status.Artifact)
			}
			if actual.Status.ConfigMap != nil || actual.Status.SourcePolicy != nil {
				t.Fatalf("the earlier source is still recorded: configMap %#v, source policy %#v", actual.Status.ConfigMap, actual.Status.SourcePolicy)
			}
			if remaining := snapshotNamesIn(t, api); !slices.Equal(remaining, []string{earlier.Name}) {
				t.Fatalf("snapshots after the resolution = %v, want only the earlier migration's %s", remaining, earlier.Name)
			}
		})
	}
}

// A snapshot that could not be deleted does not undo the resolution: the
// commit is recorded, the failure is an event, and the snapshot waits for
// the next resolution.
func TestASnapshotThatCouldNotBeDeletedLeavesTheResolutionStored(t *testing.T) {
	t.Parallel()
	schema := gitResolvingSchema(t, nil)
	left, binding := ownedSnapshot(t, sourcesnapshot.Owner{Kind: "PtahSchema", Name: schema.Name, UID: schema.UID}, "CREATE TABLE orders (id int);\n")
	schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
		ConfigMap: &operatorv1alpha1.ConfigMapSourceRevision{Name: "app-schema", UID: "configmap-uid", ResourceVersion: "3"},
		Snapshot:  &binding,
	}
	bindActiveInput(t, schema)
	job, pod := terminalWorkload(schema, batchv1.JobComplete)
	reconciler, api := fakeReconciler(t, staticLogs{content: gitResolveFrame(t, testDigest, gitCommitA, gitContentOne)}, schema, job, pod, left)
	reconciler.Client = interceptor.NewClient(api.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, writer client.WithWatch, object client.Object, options ...client.DeleteOption) error {
			if _, ok := object.(*corev1.ConfigMap); ok {
				return apierrors.NewServiceUnavailable("the API server is away")
			}
			return writer.Delete(ctx, object, options...)
		},
	})
	events := record.NewFakeRecorder(8)
	reconciler.Recorder = events

	actual := reconcileSchema(t, reconciler, api, schema)
	if actual.Status.Source.Git == nil || actual.Status.Source.Git.Commit != gitCommitA || actual.Status.Phase != operatorv1alpha1.PhaseObserving {
		t.Fatalf("phase %s, source %#v; want the commit recorded and Observing", actual.Status.Phase, actual.Status.Source)
	}
	if remaining := snapshotNamesIn(t, api); !slices.Equal(remaining, []string{left.Name}) {
		t.Fatalf("snapshots = %v, want the one the refused delete left", remaining)
	}
	event := findEvent(events, "Warning SourceSnapshotRetirementFailed")
	if !strings.Contains(event, left.Name) || !strings.Contains(event, "the API server is away") {
		t.Fatalf("event = %q, want the snapshot and the refusal named", event)
	}
}

// No resolution runs beside an operation or a post-Apply proof today: the
// reconciler settles both before it resolves. Should one ever do, a snapshot
// either of them reads is kept.
func TestSnapshotRetirementKeepsWhatAnOperationOrAProofReads(t *testing.T) {
	t.Parallel()
	schema := schemaFixture()
	owner := sourcesnapshot.Owner{Kind: "PtahSchema", Name: schema.Name, UID: schema.UID}
	proof, proofBinding := ownedSnapshot(t, owner, "CREATE TABLE proof (id int);\n")
	observed, observedBinding := ownedSnapshot(t, owner, "CREATE TABLE observed (id int);\n")
	unread, _ := ownedSnapshot(t, owner, "CREATE TABLE unread (id int);\n")
	schema.Status.PendingObservation = &operatorv1alpha1.PendingObservationStatus{
		Source: operatorv1alpha1.SourceAccessBinding{Snapshot: &proofBinding},
	}
	schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
		Type: operatorv1alpha1.OperationObserve, Source: &operatorv1alpha1.SourceAccessBinding{Snapshot: &observedBinding},
	}
	reconciler, api := fakeReconciler(t, staticLogs{}, proof, observed, unread)

	reconciler.retireSchemaSnapshots(context.Background(), schema)
	want := []string{proof.Name, observed.Name}
	slices.Sort(want)
	if remaining := snapshotNamesIn(t, api); !slices.Equal(remaining, want) {
		t.Fatalf("snapshots = %v, want %v", remaining, want)
	}
}
