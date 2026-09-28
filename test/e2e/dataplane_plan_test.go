package e2e

import (
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// The plan document the rebuild fixtures split in two. Its first statement
// quotes an identifier, so the way a plan document spells it differs from the
// SQL, and both forms are among the patterns the sealed-payload proof searches
// for.
const rebuildDocument = `{"format_version":1,"dialect":"postgres","from_fingerprint":"sha256:1111111111111111111111111111111111111111111111111111111111111111","to_fingerprint":"sha256:2222222222222222222222222222222222222222222222222222222222222222","destructive":false,"statements":[{"sql":"CREATE TABLE \"widgets\" (id integer PRIMARY KEY)","severity":"safe"},{"sql":"COMMIT","severity":"safe"}]}`

const rebuildSplitAt = 60

func rebuildFixture() (*ptahv1alpha1.PtahSchemaPlan, map[string]*ptahv1alpha1.PtahSchemaPlanChunk) {
	first, second := []byte(rebuildDocument[:rebuildSplitAt]), []byte(rebuildDocument[rebuildSplitAt:])
	plan := &ptahv1alpha1.PtahSchemaPlan{
		ObjectMeta: metav1.ObjectMeta{Name: "plan-2", UID: "plan-uid-2"},
		Spec: ptahv1alpha1.PtahSchemaPlanSpec{Chunks: []ptahv1alpha1.PlanChunkReference{
			{Name: "plan-2-000", Index: 0, Size: int32(len(first))},
			{Name: "plan-2-001", Index: 1, Size: int32(len(second))},
		}},
	}
	chunks := map[string]*ptahv1alpha1.PtahSchemaPlanChunk{
		"plan-2-000": {Spec: ptahv1alpha1.PtahSchemaPlanChunkSpec{Data: first}},
		"plan-2-001": {Spec: ptahv1alpha1.PtahSchemaPlanChunkSpec{Data: second}},
	}
	return plan, chunks
}

func chunkReader(chunks map[string]*ptahv1alpha1.PtahSchemaPlanChunk) func(string) (*ptahv1alpha1.PtahSchemaPlanChunk, error) {
	return func(name string) (*ptahv1alpha1.PtahSchemaPlanChunk, error) {
		chunk, found := chunks[name]
		if !found {
			return nil, errors.New("not found")
		}
		return chunk, nil
	}
}

func TestRebuiltPlanDocumentConcatenatesChunksInOrder(t *testing.T) {
	t.Parallel()
	plan, chunks := rebuildFixture()
	document, err := rebuiltPlanDocument(plan, chunkReader(chunks))
	if err != nil {
		t.Fatalf("rebuild refused two chunks in order: %v", err)
	}
	if string(document) != rebuildDocument {
		t.Fatalf("the rebuilt document is not the one the chunks were split from: %s", document)
	}
	if sha256Digest(document) != sha256Digest([]byte(rebuildDocument)) {
		t.Fatal("the rebuilt document does not hash to the fixture's digest")
	}
	for name, edit := range map[string]func(*ptahv1alpha1.PtahSchemaPlan, map[string]*ptahv1alpha1.PtahSchemaPlanChunk){
		"no chunks": func(p *ptahv1alpha1.PtahSchemaPlan, _ map[string]*ptahv1alpha1.PtahSchemaPlanChunk) {
			p.Spec.Chunks = nil
		},
		"chunks out of order": func(p *ptahv1alpha1.PtahSchemaPlan, _ map[string]*ptahv1alpha1.PtahSchemaPlanChunk) {
			p.Spec.Chunks[0], p.Spec.Chunks[1] = p.Spec.Chunks[1], p.Spec.Chunks[0]
		},
		"a chunk missing its data": func(_ *ptahv1alpha1.PtahSchemaPlan, c map[string]*ptahv1alpha1.PtahSchemaPlanChunk) {
			c["plan-2-001"] = &ptahv1alpha1.PtahSchemaPlanChunk{}
		},
		"a chunk shorter than its manifest": func(_ *ptahv1alpha1.PtahSchemaPlan, c map[string]*ptahv1alpha1.PtahSchemaPlanChunk) {
			c["plan-2-001"].Spec.Data = c["plan-2-001"].Spec.Data[:5]
		},
		"a chunk that cannot be read": func(_ *ptahv1alpha1.PtahSchemaPlan, c map[string]*ptahv1alpha1.PtahSchemaPlanChunk) {
			delete(c, "plan-2-000")
		},
	} {
		plan, chunks := rebuildFixture()
		edit(plan, chunks)
		if _, err := rebuiltPlanDocument(plan, chunkReader(chunks)); err == nil {
			t.Errorf("a plan with %s was rebuilt", name)
		}
	}
}

func projectionsOf(plan *ptahv1alpha1.PtahSchemaPlan, chunks map[string]*ptahv1alpha1.PtahSchemaPlanChunk) []corev1.ConfigMap {
	var projections []corev1.ConfigMap
	for _, reference := range plan.Spec.Chunks {
		projections = append(projections, corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: reference.Name, OwnerReferences: []metav1.OwnerReference{{
				APIVersion: ptahSchemaAPIVersion, Kind: "PtahSchemaPlan", Name: plan.Name, UID: plan.UID, Controller: ptr.To(true),
			}}},
			Immutable:  ptr.To(true),
			BinaryData: map[string][]byte{"chunk": slices.Clone(chunks[reference.Name].Spec.Data)},
		})
	}
	return projections
}

func TestPlanProjectedExactlyHoldsEachProjectionToItsChunk(t *testing.T) {
	t.Parallel()
	plan, chunks := rebuildFixture()
	if err := planProjectedExactly(plan, projectionsOf(plan, chunks), chunkReader(chunks)); err != nil {
		t.Fatalf("an exact projection was refused: %v", err)
	}
	for name, edit := range map[string]func([]corev1.ConfigMap) []corev1.ConfigMap{
		"missing a chunk":      func(p []corev1.ConfigMap) []corev1.ConfigMap { return p[:1] },
		"carrying other bytes": func(p []corev1.ConfigMap) []corev1.ConfigMap { p[1].BinaryData["chunk"] = []byte("other"); return p },
		"mutable":              func(p []corev1.ConfigMap) []corev1.ConfigMap { p[0].Immutable = nil; return p },
		"another plan's":       func(p []corev1.ConfigMap) []corev1.ConfigMap { p[0].OwnerReferences[0].UID = "plan-uid-9"; return p },
		"with text data too": func(p []corev1.ConfigMap) []corev1.ConfigMap {
			p[0].Data = map[string]string{"sql": "CREATE TABLE"}
			return p
		},
		"named for no chunk": func(p []corev1.ConfigMap) []corev1.ConfigMap { p[1].Name = "plan-2-009"; return p },
	} {
		projections := edit(projectionsOf(plan, chunks))
		if err := planProjectedExactly(plan, projections, chunkReader(chunks)); err == nil {
			t.Errorf("a projection %s was accepted", name)
		}
	}
}

func TestSealedPayloadLeakSearchesBothSpellingsAndTheKey(t *testing.T) {
	t.Parallel()
	document, err := parsePlanDocument([]byte(rebuildDocument))
	if err != nil {
		t.Fatal(err)
	}
	patterns := planTextPatterns(document)
	want := []string{`CREATE TABLE "widgets" (id integer PRIMA`, `CREATE TABLE \"widgets\" (id integer PRIMA`, `"format_version"`}
	if !slices.Equal(patterns, want) {
		t.Fatalf("patterns = %q, want the statement in both spellings and the document key", patterns)
	}
	sealed := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("sha256:ab", 20)))
	if err := sealedPayloadLeak(sealed, []byte(rebuildDocument)); err != nil {
		t.Fatalf("a sealed payload was refused: %v", err)
	}
	for name, stdout := range map[string]string{
		"an empty stdout":                    "",
		"a plaintext stdout":                 rebuildDocument,
		"a stdout carrying statement SQL":    sealed + `CREATE TABLE "widgets" (id integer PRIMARY KEY)`,
		"a stdout carrying a JSON statement": sealed + `CREATE TABLE \"widgets\" (id integer PRIMARY KEY)`,
		"a stdout carrying the document key": sealed + `{"format_version":1}`,
	} {
		if err := sealedPayloadLeak(stdout, []byte(rebuildDocument)); err == nil {
			t.Errorf("%s was read as sealed", name)
		}
	}
	if err := sealedPayloadLeak(sealed, []byte(`{"format_version":1,"statements":[]}`)); err == nil {
		t.Error("a document without statements was checked against")
	}
}

func TestResolvedReferenceReplacesTheTagWithTheDigest(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("a", 64)
	for reference, want := range map[string]string{
		"oci://registry.ns.svc.cluster.local:5000/schemas/postgresql:stable": "oci://registry.ns.svc.cluster.local:5000/schemas/postgresql@" + digest,
		"oci://registry:5000/schemas/postgresql@" + digest:                   "oci://registry:5000/schemas/postgresql@" + digest,
	} {
		if got := resolvedReference(reference, digest); got != want {
			t.Errorf("resolvedReference(%s) = %s, want %s", reference, got, want)
		}
	}
}

func TestHoldsStringFindsAScalarAnywhere(t *testing.T) {
	t.Parallel()
	document, err := asJSON(map[string]any{"a": []any{map[string]any{"b": "e2e/postgresql/app"}}, "c": 1})
	if err != nil {
		t.Fatal(err)
	}
	if !holdsString(document, "e2e/postgresql/app") {
		t.Fatal("a nested scalar was not found")
	}
	if holdsString(document, "e2e/postgresql") || holdsString(document, "1") {
		t.Fatal("a prefix or a number read as the scalar")
	}
}

func boundSchema() *ptahv1alpha1.PtahSchema {
	digest := "sha256:" + strings.Repeat("a", 64)
	reference := "oci://registry:5000/schemas/postgresql:stable"
	return &ptahv1alpha1.PtahSchema{Status: ptahv1alpha1.PtahSchemaStatus{
		Source: ptahv1alpha1.SchemaSourceStatus{
			RequestedReference: reference, Digest: digest, ResolvedReference: resolvedReference(reference, digest),
			Verified: true, ArtifactType: schemaArtifactType, VerificationPolicyDigest: "sha256:" + strings.Repeat("b", 64),
		},
		Target: ptahv1alpha1.TargetStatus{
			CoordinationDigest: "sha256:" + strings.Repeat("c", 64), IdentityDigest: "sha256:" + strings.Repeat("d", 64),
			DriftReportDigest: "sha256:" + strings.Repeat("e", 64),
		},
		ExecutionBinding: &ptahv1alpha1.ExecutionBindingStatus{Epoch: "v1-" + strings.Repeat("0", 32), ControllerStateVersion: 3},
		Plan: &ptahv1alpha1.CurrentPlanStatus{
			ExecutionBindingID: "v1-" + strings.Repeat("0", 32), ControllerImage: "manager", ControllerRevision: "abc",
			ControllerStateVersion: 3, ContentDigest: "sha256:" + strings.Repeat("f", 64),
			CoordinationDigest: "sha256:" + strings.Repeat("c", 64), TargetIdentityDigest: "sha256:" + strings.Repeat("d", 64),
		},
	}}
}

func TestPlanStatusBoundHoldsThePlanToThisManager(t *testing.T) {
	t.Parallel()
	identity := controllerIdentity{image: "manager", revision: "abc", stateVersion: "3"}
	reference, digest := "oci://registry:5000/schemas/postgresql:stable", "sha256:"+strings.Repeat("a", 64)
	if err := planStatusBound(boundSchema(), reference, digest, identity, 3); err != nil {
		t.Fatalf("a bound schema was refused: %v", err)
	}
	for name, edit := range map[string]func(*ptahv1alpha1.PtahSchema){
		"unverified":            func(s *ptahv1alpha1.PtahSchema) { s.Status.Source.Verified = false },
		"resolved by tag":       func(s *ptahv1alpha1.PtahSchema) { s.Status.Source.ResolvedReference = reference },
		"another artifact":      func(s *ptahv1alpha1.PtahSchema) { s.Status.Source.ArtifactType = "application/json" },
		"no drift report":       func(s *ptahv1alpha1.PtahSchema) { s.Status.Target.DriftReportDigest = "" },
		"no binding":            func(s *ptahv1alpha1.PtahSchema) { s.Status.ExecutionBinding = nil },
		"another state":         func(s *ptahv1alpha1.PtahSchema) { s.Status.ExecutionBinding.ControllerStateVersion = 2 },
		"plan of another epoch": func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan.ExecutionBindingID = "v1-" + strings.Repeat("1", 32) },
		"plan of another image": func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan.ControllerImage = "other" },
		"no plan":               func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan = nil },
	} {
		schema := boundSchema()
		edit(schema)
		if err := planStatusBound(schema, reference, digest, identity, 3); err == nil {
			t.Errorf("a schema %s was accepted", name)
		}
	}
}

func TestResultBindingsReadTheSchemasEvidence(t *testing.T) {
	t.Parallel()
	schema := boundSchema()
	target := schema.Status.Target
	observe := runner.Result{
		ChildExitCode: 1, CoordinationDigest: target.CoordinationDigest, TargetIdentityDigest: target.IdentityDigest,
		DriftReportDigest: target.DriftReportDigest, ObservedDrift: true, DriftFindingCount: 2,
		HighestDriftSeverity: "warning", ObservedDialect: "postgresql",
	}
	if err := observedDriftBound(observe, target, "postgres"); err != nil {
		t.Fatalf("a bound Observe result was refused: %v", err)
	}
	for name, edit := range map[string]func(*runner.Result){
		"an error":           func(r *runner.Result) { r.Error = &runner.ResultError{Code: "x"} },
		"output":             func(r *runner.Result) { r.Stdout = "{}" },
		"exit 2":             func(r *runner.Result) { r.ChildExitCode = 2 },
		"no drift":           func(r *runner.Result) { r.ObservedDrift = false },
		"no finding":         func(r *runner.Result) { r.DriftFindingCount = 0 },
		"unknown severity":   func(r *runner.Result) { r.HighestDriftSeverity = "fatal" },
		"another report":     func(r *runner.Result) { r.DriftReportDigest = "sha256:" + strings.Repeat("9", 64) },
		"MySQL for Postgres": func(r *runner.Result) { r.ObservedDialect = "mysql" },
	} {
		result := observe
		edit(&result)
		if err := observedDriftBound(result, target, "postgres"); err == nil {
			t.Errorf("an Observe result with %s was accepted", name)
		}
	}
	plan := runner.Result{
		PlanOutcome: runner.PlanOutcomeChanges, Stdout: "sealed", PlanContentDigest: schema.Status.Plan.ContentDigest,
		CoordinationDigest: schema.Status.Plan.CoordinationDigest, TargetIdentityDigest: schema.Status.Plan.TargetIdentityDigest,
	}
	if err := changedPlanBound(plan, schema.Status.Plan); err != nil {
		t.Fatalf("a bound Plan result was refused: %v", err)
	}
	if err := changedPlanBound(plan, nil); err == nil {
		t.Error("a Plan result was bound to no plan")
	}
	stale := plan
	stale.PlanContentDigest = "sha256:" + strings.Repeat("8", 64)
	if err := changedPlanBound(stale, schema.Status.Plan); err == nil {
		t.Error("a Plan result of another plan was accepted")
	}
	noChanges := plan
	noChanges.PlanOutcome = runner.PlanOutcomeNoChanges
	if err := changedPlanBound(noChanges, schema.Status.Plan); err == nil {
		t.Error("a NoChanges Plan result was read as a change")
	}
}

func TestConvergedResultsNeedANoOpCycle(t *testing.T) {
	t.Parallel()
	schema := boundSchema()
	schema.Status.Plan = nil
	schema.Status.Conditions = []metav1.Condition{{Type: "InSync", Status: metav1.ConditionTrue, Reason: "ScopedConverged"}}
	target := schema.Status.Target
	observe := runner.Result{
		CoordinationDigest: target.CoordinationDigest, TargetIdentityDigest: target.IdentityDigest,
		DriftReportDigest: target.DriftReportDigest,
	}
	plan := runner.Result{
		PlanOutcome: runner.PlanOutcomeNoChanges, CoordinationDigest: target.CoordinationDigest,
		TargetIdentityDigest: target.IdentityDigest,
	}
	if err := convergedResults(schema, observe, plan); err != nil {
		t.Fatalf("a converged cycle was refused: %v", err)
	}
	drifted := observe
	drifted.ObservedDrift = true
	if err := convergedResults(schema, drifted, plan); err == nil {
		t.Error("an Observe that found drift was read as converged")
	}
	changed := plan
	changed.PlanOutcome = runner.PlanOutcomeChanges
	if err := convergedResults(schema, observe, changed); err == nil {
		t.Error("a Plan with changes was read as converged")
	}
	pending := schema.DeepCopy()
	pending.Status.PendingLockRelease = &ptahv1alpha1.TargetLockReleaseStatus{}
	if err := convergedResults(pending, observe, plan); err == nil {
		t.Error("a schema still releasing a lock was read as converged")
	}
	stale := schema.DeepCopy()
	stale.Status.Conditions = nil
	if err := convergedResults(stale, observe, plan); err == nil {
		t.Error("a schema without the InSync condition was read as converged")
	}
}

func TestApprovalConsumedKeepsTheWholeHistory(t *testing.T) {
	t.Parallel()
	approval := &ptahv1alpha1.PtahSchemaApproval{
		ObjectMeta: metav1.ObjectMeta{Generation: 1},
		Spec:       ptahv1alpha1.PtahSchemaApprovalSpec{PlanRef: ptahv1alpha1.ImmutableObjectReference{UID: "plan-uid"}},
		Status: ptahv1alpha1.PtahSchemaApprovalStatus{ObservedGeneration: 1, Conditions: []metav1.Condition{
			{Type: "Accepted", Status: metav1.ConditionFalse, Reason: "PlanNoLongerCurrent"},
			{Type: "Consumed", Status: metav1.ConditionTrue, Reason: "DispatchCommitted"},
			{Type: "Stale", Status: metav1.ConditionTrue, Reason: "PlanNoLongerCurrent"},
		}},
	}
	if !approvalConsumed(approval, "plan-uid") {
		t.Fatal("a consumed approval was refused")
	}
	if approvalConsumed(approval, "other-plan") {
		t.Error("an approval of another plan was read as consumed")
	}
	for index := range approval.Status.Conditions {
		trimmed := approval.DeepCopy()
		trimmed.Status.Conditions = slices.Delete(trimmed.Status.Conditions, index, index+1)
		if approvalConsumed(trimmed, "plan-uid") {
			t.Errorf("an approval without %s was read as consumed", approval.Status.Conditions[index].Type)
		}
	}
}
