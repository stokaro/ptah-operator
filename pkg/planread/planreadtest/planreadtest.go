// Package planreadtest holds stored plans in an in-memory API server, for
// tests of programs that read plans.
//
// A plan here is published through the operator's own store, so it is chunked,
// bound and committed the way the controller does it. Cases lists the stored
// states a plan reader has to answer, each with the answer planread gives; a
// program that reads plans through its own API runs the same cases and has to
// give the same answers.
//
// The objects carry the UIDs this package assigned and the bindings between
// them depend on those UIDs, so they belong in a client that keeps them, such
// as the one NewClient builds. A real API server assigns its own.
package planreadtest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/pkg/planread"
)

// Namespace is where every lab stores its schemas.
const Namespace = "application"

// Lab is a namespace on paper: one schema and the plans stored for it.
type Lab struct {
	schema *operatorv1alpha1.PtahSchema
	client client.Client
}

// NewLab stores one PostgreSQL schema with no plan yet.
func NewLab(t testing.TB, name string) *Lab {
	t.Helper()
	schema := newSchema(name)
	return &Lab{schema: schema, client: NewClient(t, schema)}
}

// AddSchema stores another schema beside this lab's, in the same API server.
func (l *Lab) AddSchema(t testing.TB, name string) *Lab {
	t.Helper()
	schema := newSchema(name)
	if err := l.client.Create(context.Background(), schema); err != nil {
		t.Fatalf("store the schema %s: %v", name, err)
	}
	return &Lab{schema: schema, client: l.client}
}

// Client is the lab's API server, for reads and for changes a test makes.
func (l *Lab) Client() client.Client { return l.client }

// Schema is the lab's schema as last read or written.
func (l *Lab) Schema() *operatorv1alpha1.PtahSchema { return l.schema.DeepCopy() }

// Publish stores one plan of the given statements and returns it with its
// exact document. The marker tells two plans of the same lab apart.
func (l *Lab) Publish(t testing.TB, statements []string, marker string) (*operatorv1alpha1.PtahSchemaPlan, []byte) {
	t.Helper()
	document := Document(t, statements)
	return l.PublishDocument(t, document, marker), document
}

// PublishDocument stores exact bytes as a plan, for cases about what the
// document says rather than the statements in it. Adjust edits the manifest
// before it is bound.
func (l *Lab) PublishDocument(
	t testing.TB,
	document []byte,
	marker string,
	adjust ...func(*operatorv1alpha1.PtahSchemaPlanSpec),
) *operatorv1alpha1.PtahSchemaPlan {
	t.Helper()
	coordination, err := fingerprint.DatabaseCoordinationDigest("PostgreSQL", l.schema.Namespace, "prod/application/"+l.schema.Name)
	if err != nil {
		t.Fatal(err)
	}
	spec := operatorv1alpha1.PtahSchemaPlanSpec{
		ContractVersion:          fingerprint.CurrentPlanContractVersion,
		SchemaRef:                operatorv1alpha1.ImmutableObjectReference{Name: l.schema.Name, UID: l.schema.UID},
		ArtifactDigest:           "sha256:artifact-" + marker,
		CoordinationDigest:       coordination,
		TargetIdentityDigest:     "sha256:target",
		ActualStateFingerprint:   "sha256:actual-" + marker,
		DesiredStateFingerprint:  "sha256:desired-" + marker,
		PolicyFingerprint:        "sha256:policy",
		VerificationPolicyUID:    "verification-policy-uid",
		VerificationPolicyDigest: "sha256:verification",
		ExecutionBindingID:       "v1-33333333333333333333333333333333",
		ControllerImage:          "example.invalid/manager@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		ControllerRevision:       "controller-test-revision",
		ControllerStateVersion:   1,
		PtahVersion:              "v0.3.0",
		ExecutorImage:            "example.invalid/ptah@sha256:executor",
		RunnerImage:              "example.invalid/operator@sha256:runner",
		RunnerProtocolVersion:    int32(runner.ProtocolVersion),
		Dialect:                  "postgresql",
		StatementCount:           1,
	}
	for _, change := range adjust {
		change(&spec)
	}
	spec.ContentDigest = fingerprint.DigestBytes(document)
	spec.Fingerprint, err = planstore.Binding(l.schema.UID, spec).Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	desired, chunks, err := planstore.Prepare(l.schema, spec, document)
	if err != nil {
		t.Fatalf("prepare the plan: %v", err)
	}
	published, err := planstore.Store{Client: l.client, Reader: l.client}.Publish(context.Background(), desired, chunks)
	if err != nil {
		t.Fatalf("publish the plan: %v", err)
	}
	return published
}

// SetCurrent points the schema's status at a stored plan.
func (l *Lab) SetCurrent(t testing.TB, plan *operatorv1alpha1.PtahSchemaPlan) {
	t.Helper()
	l.updateSchema(t, func(schema *operatorv1alpha1.PtahSchema) {
		schema.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{
			Name: plan.Name, UID: plan.UID, Fingerprint: plan.Spec.Fingerprint,
			ContentDigest: plan.Spec.ContentDigest,
		}
	})
}

// SetApplied records a confirmed apply of a stored plan, with the reference
// the operator writes.
func (l *Lab) SetApplied(t testing.TB, plan *operatorv1alpha1.PtahSchemaPlan) {
	t.Helper()
	l.updateSchema(t, func(schema *operatorv1alpha1.PtahSchema) {
		schema.Status.Applied = &operatorv1alpha1.AppliedStatus{
			PlanRef:         operatorv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID},
			PlanFingerprint: plan.Spec.Fingerprint,
			CompletedAt:     metav1.Now(),
		}
	})
}

// Uncommit takes back the condition that says the chunks were all written,
// which is the state a publication interrupted halfway leaves behind.
func (l *Lab) Uncommit(t testing.TB, plan *operatorv1alpha1.PtahSchemaPlan) {
	t.Helper()
	stored := l.plan(t, plan.Name)
	meta.RemoveStatusCondition(&stored.Status.Conditions, operatorv1alpha1.ConditionPlanStorageReady)
	if err := l.client.Status().Update(context.Background(), stored); err != nil {
		t.Fatalf("update the plan status: %v", err)
	}
}

// Recreate deletes a plan and stores the same manifest under the same name
// as a new object, which is what a delete and a recreate leave behind.
func (l *Lab) Recreate(t testing.TB, plan *operatorv1alpha1.PtahSchemaPlan) {
	t.Helper()
	replaced := l.plan(t, plan.Name)
	if err := l.client.Delete(context.Background(), replaced); err != nil {
		t.Fatalf("delete the plan: %v", err)
	}
	replaced.ResourceVersion = ""
	replaced.UID = types.UID("uid-recreated-" + plan.Name)
	if err := l.client.Create(context.Background(), replaced); err != nil {
		t.Fatalf("recreate the plan: %v", err)
	}
}

// DeletePlan removes a stored plan. Its chunks stay, as they would until the
// garbage collector reaches them.
func (l *Lab) DeletePlan(t testing.TB, plan *operatorv1alpha1.PtahSchemaPlan) {
	t.Helper()
	if err := l.client.Delete(context.Background(), l.plan(t, plan.Name)); err != nil {
		t.Fatalf("delete the plan: %v", err)
	}
}

// EditChunk rewrites the first chunk of a plan in place. Chunks are immutable
// in a cluster; this is how a test makes one that no longer matches.
func (l *Lab) EditChunk(t testing.TB, plan *operatorv1alpha1.PtahSchemaPlan, edit func(*operatorv1alpha1.PtahSchemaPlanChunk)) {
	t.Helper()
	chunk := l.chunk(t, plan)
	edit(chunk)
	if err := l.client.Update(context.Background(), chunk); err != nil {
		t.Fatalf("rewrite the chunk: %v", err)
	}
}

// DeleteChunk removes the first chunk of a plan.
func (l *Lab) DeleteChunk(t testing.TB, plan *operatorv1alpha1.PtahSchemaPlan) {
	t.Helper()
	if err := l.client.Delete(context.Background(), l.chunk(t, plan)); err != nil {
		t.Fatalf("delete the chunk: %v", err)
	}
}

// EditManifest rewrites a stored plan's spec in place.
func (l *Lab) EditManifest(t testing.TB, plan *operatorv1alpha1.PtahSchemaPlan, edit func(*operatorv1alpha1.PtahSchemaPlanSpec)) {
	t.Helper()
	stored := l.plan(t, plan.Name)
	edit(&stored.Spec)
	if err := l.client.Update(context.Background(), stored); err != nil {
		t.Fatalf("rewrite the plan: %v", err)
	}
}

// Objects returns every schema, plan and chunk the lab's API server holds,
// ready for NewClient.
func (l *Lab) Objects(t testing.TB) []client.Object {
	t.Helper()
	ctx := context.Background()
	var objects []client.Object
	schemas := &operatorv1alpha1.PtahSchemaList{}
	plans := &operatorv1alpha1.PtahSchemaPlanList{}
	chunks := &operatorv1alpha1.PtahSchemaPlanChunkList{}
	for _, list := range []client.ObjectList{schemas, plans, chunks} {
		if err := l.client.List(ctx, list, client.InNamespace(Namespace)); err != nil {
			t.Fatalf("list the lab's objects: %v", err)
		}
	}
	for i := range schemas.Items {
		objects = append(objects, &schemas.Items[i])
	}
	for i := range plans.Items {
		objects = append(objects, &plans.Items[i])
	}
	for i := range chunks.Items {
		objects = append(objects, &chunks.Items[i])
	}
	for _, object := range objects {
		object.SetResourceVersion("")
	}
	return objects
}

// NewClient is an in-memory API server that holds the given objects with the
// UIDs they carry.
func NewClient(t testing.TB, objects ...client.Object) client.Client {
	t.Helper()
	api := fake.NewClientBuilder().WithScheme(Scheme(t)).
		WithStatusSubresource(&operatorv1alpha1.PtahSchemaPlan{}, &operatorv1alpha1.PtahSchema{}).
		WithObjects(objects...).Build()
	return &uidAssigningClient{Client: api}
}

// Scheme knows the core kinds and the operator's.
func Scheme(t testing.TB) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

// Document renders a plan document the way an executor writes one, every
// statement classified safe.
func Document(t testing.TB, statements []string) []byte {
	t.Helper()
	file := dataplane.PlanFile{
		FormatVersion:   dataplane.PlanFormatVersion,
		Name:            "plan",
		Dialect:         "postgresql",
		FromFingerprint: "sha256:from",
		ToFingerprint:   "sha256:to",
	}
	for _, sql := range statements {
		file.Statements = append(file.Statements, dataplane.PlanStatement{SQL: sql, Severity: "safe"})
	}
	document, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

// FutureDocument is a plan document from a build that is not this one.
func FutureDocument(t testing.TB) []byte {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(Document(t, []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"}), &decoded); err != nil {
		t.Fatal(err)
	}
	decoded["format_version"] = dataplane.PlanFormatVersion + 1
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// SpanningStatements are long enough to cross a chunk boundary, and made of
// what a boundary must not be allowed to cut: a quote, a backslash escape and
// a multi-byte character all sit inside the SQL.
func SpanningStatements() []string {
	filler := strings.Repeat(`x'\"й`, planread.ChunkBytes/8)
	return []string{
		"CREATE TABLE customers (id BIGINT PRIMARY KEY)",
		"COMMENT ON TABLE customers IS '" + filler + "'",
		"CREATE INDEX customers_email ON customers (email)",
	}
}

func newSchema(name string) *operatorv1alpha1.PtahSchema {
	return &operatorv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: Namespace, Name: name, UID: types.UID("uid-" + name)},
		Spec: operatorv1alpha1.PtahSchemaSpec{
			Target: operatorv1alpha1.DatabaseTargetSpec{Engine: "PostgreSQL"},
		},
	}
}

func (l *Lab) updateSchema(t testing.TB, change func(*operatorv1alpha1.PtahSchema)) {
	t.Helper()
	schema := &operatorv1alpha1.PtahSchema{}
	key := types.NamespacedName{Namespace: l.schema.Namespace, Name: l.schema.Name}
	if err := l.client.Get(context.Background(), key, schema); err != nil {
		t.Fatalf("read the schema: %v", err)
	}
	change(schema)
	if err := l.client.Status().Update(context.Background(), schema); err != nil {
		t.Fatalf("update the schema status: %v", err)
	}
	l.schema = schema
}

func (l *Lab) plan(t testing.TB, name string) *operatorv1alpha1.PtahSchemaPlan {
	t.Helper()
	plan := &operatorv1alpha1.PtahSchemaPlan{}
	if err := l.client.Get(context.Background(), types.NamespacedName{Namespace: Namespace, Name: name}, plan); err != nil {
		t.Fatalf("read the plan %s: %v", name, err)
	}
	return plan
}

func (l *Lab) chunk(t testing.TB, plan *operatorv1alpha1.PtahSchemaPlan) *operatorv1alpha1.PtahSchemaPlanChunk {
	t.Helper()
	chunk := &operatorv1alpha1.PtahSchemaPlanChunk{}
	key := types.NamespacedName{Namespace: Namespace, Name: plan.Spec.Chunks[0].Name}
	if err := l.client.Get(context.Background(), key, chunk); err != nil {
		t.Fatalf("read the chunk: %v", err)
	}
	return chunk
}

// uidAssigningClient stands in for the API server's UID assignment, which the
// plan store's bindings are written against.
type uidAssigningClient struct {
	client.Client
	next atomic.Int64
}

func (c *uidAssigningClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if object.GetUID() == "" {
		object.SetUID(types.UID(fmt.Sprintf("uid-%s-%d", object.GetName(), c.next.Add(1))))
	}
	return c.Client.Create(ctx, object, options...)
}
