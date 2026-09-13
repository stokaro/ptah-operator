package planview_test

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
)

// lab is a namespace on paper: a schema, the plans stored for it, and a reader
// that records what a command asked the API server for.
type lab struct {
	schema *operatorv1alpha1.PtahSchema
	client client.Client
	reads  *recorder
}

// newLab stores one schema with no plan yet.
func newLab(t *testing.T, name string) *lab {
	t.Helper()
	schema := &operatorv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: "application", Name: name, UID: types.UID("uid-" + name)},
		Spec: operatorv1alpha1.PtahSchemaSpec{
			Target: operatorv1alpha1.DatabaseTargetSpec{Engine: "PostgreSQL"},
		},
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	api := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&operatorv1alpha1.PtahSchemaPlan{}, &operatorv1alpha1.PtahSchema{}).
		WithObjects(schema).Build()
	return &lab{schema: schema, client: &uidAssigningClient{Client: api}, reads: &recorder{}}
}

// reader returns what the viewer reads through, recording every call.
func (l *lab) reader() *recorder {
	l.reads.Reader = l.client
	return l.reads
}

// store publishes one plan for the schema and returns it with its exact bytes.
//
// It goes through the operator's own Prepare and Publish, so what the tests
// read back was chunked, bound and committed the way the controller does it.
func (l *lab) store(t *testing.T, statements []string, marker string) (*operatorv1alpha1.PtahSchemaPlan, []byte) {
	t.Helper()
	return l.storeDocument(t, planDocument(t, statements), marker), planDocument(t, statements)
}

// storeDocument publishes exact bytes, for the cases that are about what the
// document says rather than about the statements in it.
func (l *lab) storeDocument(t *testing.T, document []byte, marker string) *operatorv1alpha1.PtahSchemaPlan {
	t.Helper()
	coordination, err := fingerprint.DatabaseCoordinationDigest("PostgreSQL", "prod/application/"+l.schema.Name)
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
		Destructive:              false,
	}
	spec.ContentDigest = fingerprint.DigestBytes(document)
	spec.Fingerprint, err = binding(l.schema, spec).Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	desired, chunks, err := planstore.Prepare(l.schema, spec, document)
	if err != nil {
		t.Fatalf("prepare the plan: %v", err)
	}
	store := planstore.Store{Client: l.client, Reader: l.client}
	published, err := store.Publish(context.Background(), desired, chunks)
	if err != nil {
		t.Fatalf("publish the plan: %v", err)
	}
	return published
}

// futureFormat is a plan document from a build that is not this one.
func futureFormat(t *testing.T) []byte {
	t.Helper()
	document := planDocument(t, []string{"CREATE TABLE customers (id BIGINT PRIMARY KEY)"})
	var decoded map[string]any
	if err := json.Unmarshal(document, &decoded); err != nil {
		t.Fatal(err)
	}
	decoded["format_version"] = dataplane.PlanFormatVersion + 1
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// current points the schema's status at a stored plan.
func (l *lab) current(t *testing.T, plan *operatorv1alpha1.PtahSchemaPlan) {
	t.Helper()
	l.update(t, func(schema *operatorv1alpha1.PtahSchema) {
		schema.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{
			Name: plan.Name, UID: plan.UID, Fingerprint: plan.Spec.Fingerprint,
			ContentDigest: plan.Spec.ContentDigest,
		}
	})
}

// applied records a confirmed apply of a stored plan, with the reference a
// current operator writes.
func (l *lab) applied(t *testing.T, plan *operatorv1alpha1.PtahSchemaPlan) {
	t.Helper()
	l.update(t, func(schema *operatorv1alpha1.PtahSchema) {
		schema.Status.Applied = &operatorv1alpha1.AppliedStatus{
			PlanRef:         &operatorv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID},
			PlanFingerprint: plan.Spec.Fingerprint,
			CompletedAt:     metav1.Now(),
		}
	})
}

// appliedBeforeTheReference records the same apply the way an operator wrote it
// before AppliedStatus carried a plan reference.
func (l *lab) appliedBeforeTheReference(t *testing.T, plan *operatorv1alpha1.PtahSchemaPlan) {
	t.Helper()
	l.update(t, func(schema *operatorv1alpha1.PtahSchema) {
		schema.Status.Applied = &operatorv1alpha1.AppliedStatus{
			PlanFingerprint: plan.Spec.Fingerprint,
			CompletedAt:     metav1.Now(),
		}
	})
}

func (l *lab) update(t *testing.T, change func(*operatorv1alpha1.PtahSchema)) {
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

// uncommit takes back the condition that says the chunks were all written,
// which is the state a publication interrupted halfway leaves behind.
func (l *lab) uncommit(t *testing.T, plan *operatorv1alpha1.PtahSchemaPlan) {
	t.Helper()
	stored := &operatorv1alpha1.PtahSchemaPlan{}
	key := types.NamespacedName{Namespace: plan.Namespace, Name: plan.Name}
	if err := l.client.Get(context.Background(), key, stored); err != nil {
		t.Fatalf("read the plan: %v", err)
	}
	meta.RemoveStatusCondition(&stored.Status.Conditions, operatorv1alpha1.ConditionPlanStorageReady)
	if err := l.client.Status().Update(context.Background(), stored); err != nil {
		t.Fatalf("update the plan status: %v", err)
	}
}

// planDocument renders a plan file the way an executor writes one.
func planDocument(t *testing.T, statements []string) []byte {
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

func binding(schema *operatorv1alpha1.PtahSchema, spec operatorv1alpha1.PtahSchemaPlanSpec) fingerprint.PlanBinding {
	return fingerprint.PlanBinding{
		ContractVersion:          spec.ContractVersion,
		SchemaUID:                string(schema.UID),
		PlanContentDigest:        spec.ContentDigest,
		ArtifactDigest:           spec.ArtifactDigest,
		CoordinationDigest:       spec.CoordinationDigest,
		TargetIdentityDigest:     spec.TargetIdentityDigest,
		ActualStateFingerprint:   spec.ActualStateFingerprint,
		DesiredStateFingerprint:  spec.DesiredStateFingerprint,
		PolicyFingerprint:        spec.PolicyFingerprint,
		VerificationPolicyUID:    string(spec.VerificationPolicyUID),
		VerificationPolicyDigest: spec.VerificationPolicyDigest,
		ExecutionBindingID:       spec.ExecutionBindingID,
		ControllerImage:          spec.ControllerImage,
		ControllerRevision:       spec.ControllerRevision,
		ControllerStateVersion:   spec.ControllerStateVersion,
		PtahVersion:              spec.PtahVersion,
		ExecutorImage:            spec.ExecutorImage,
		RunnerImage:              spec.RunnerImage,
		RunnerProtocolVersion:    spec.RunnerProtocolVersion,
	}
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

// recorder is a read-only client that remembers what was asked of it.
//
// It is how the tests say what the viewer is allowed to read: a client that
// reached for a Secret, or for anything outside the plan's own objects, would
// be asking a reader for access the command must not need.
type recorder struct {
	client.Reader
	kinds []string
}

func (r *recorder) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	r.kinds = append(r.kinds, fmt.Sprintf("get %T", object))
	return r.Reader.Get(ctx, key, object, options...)
}

func (r *recorder) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	r.kinds = append(r.kinds, fmt.Sprintf("list %T", list))
	return r.Reader.List(ctx, list, options...)
}

// touched returns the distinct types the viewer asked for, in order.
func (r *recorder) touched() []string {
	var seen []string
	for _, kind := range r.kinds {
		if len(seen) > 0 && seen[len(seen)-1] == kind {
			continue
		}
		if !contains(seen, kind) {
			seen = append(seen, kind)
		}
	}
	return seen
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// sqlOf reads the statements back out of rendered SQL.
func sqlOf(rendered string) []string {
	var statements []string
	for _, line := range strings.Split(strings.TrimSpace(rendered), "\n") {
		if strings.TrimSpace(line) != "" {
			statements = append(statements, strings.TrimSuffix(strings.TrimSpace(line), ";"))
		}
	}
	return statements
}
