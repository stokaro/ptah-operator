package crdupgrade

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/stokaro/ptah-operator/internal/controllerstate"
)

func TestCandidatesAreCompleteAndDeterministic(t *testing.T) {
	candidates, err := Candidates()
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		got = append(got, candidate.Name)
		if candidate.Annotations[SchemaVersionAnnotation] != "1" {
			t.Fatalf("candidate %s schema version = %q, want 1", candidate.Name, candidate.Annotations[SchemaVersionAnnotation])
		}
		computedDigest, digestErr := ComputeSchemaDigest(candidate)
		if digestErr != nil {
			t.Fatalf("candidate %s digest: %v", candidate.Name, digestErr)
		}
		if candidate.Annotations[SchemaDigestAnnotation] != computedDigest {
			t.Fatalf("candidate %s schema digest = %q, want %q", candidate.Name, candidate.Annotations[SchemaDigestAnnotation], computedDigest)
		}
	}
	want := Names()
	if !sort.StringsAreSorted(got) {
		t.Fatalf("candidate names are not sorted: %v", got)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate names = %v, want %v", got, want)
	}
}

func TestReconcilePreservesIdentityMetadataAndStatus(t *testing.T) {
	candidates := mustCandidates(t)
	objects := readyObjects(candidates)
	target := objects[PtahSchemaCRDName]
	target.UID = types.UID("stable-crd-uid")
	target.ResourceVersion = "41"
	target.Labels = map[string]string{"owner.example.test": "preserved"}
	target.Annotations["owner.example.test/annotation"] = "preserved"
	target.Status.StoredVersions = []string{"v1alpha1"}
	target.Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "outdated schema"

	client := &memoryClient{objects: objects}
	manager := &Manager{Client: client, PollInterval: time.Millisecond}
	if err := manager.reconcile(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	updated := client.objects[PtahSchemaCRDName]
	if updated.UID != types.UID("stable-crd-uid") {
		t.Fatalf("UID = %q, want stable-crd-uid", updated.UID)
	}
	if updated.Labels["owner.example.test"] != "preserved" {
		t.Fatalf("metadata labels were not preserved: %v", updated.Labels)
	}
	if updated.Annotations["owner.example.test/annotation"] != "preserved" {
		t.Fatalf("metadata annotations were not preserved: %v", updated.Annotations)
	}
	if updated.Annotations[SchemaDigestAnnotation] != candidateByName(candidates, PtahSchemaCRDName).Annotations[SchemaDigestAnnotation] {
		t.Fatalf("updated schema digest = %q", updated.Annotations[SchemaDigestAnnotation])
	}
	if !reflect.DeepEqual(updated.Status.StoredVersions, []string{"v1alpha1"}) {
		t.Fatalf("stored versions = %v", updated.Status.StoredVersions)
	}
	matches, err := sameSpec(updated, candidateByName(candidates, PtahSchemaCRDName))
	if err != nil {
		t.Fatal(err)
	}
	if !matches {
		t.Fatal("updated CRD does not match the candidate")
	}
	if client.dryRunUpdates != 1 || client.realUpdates != 1 {
		t.Fatalf("updates dry-run=%d real=%d, want 1 and 1", client.dryRunUpdates, client.realUpdates)
	}
}

func TestReconcileRefusesMissingCRDWithoutCreating(t *testing.T) {
	candidates := mustCandidates(t)
	objects := readyObjects(candidates)
	delete(objects, PtahSchemaApprovalCRDName)
	client := &memoryClient{objects: objects}
	manager := &Manager{Client: client, PollInterval: time.Millisecond}
	err := manager.reconcile(context.Background(), nil)
	if err == nil || !contains(err.Error(), "missing; refusing to recreate") {
		t.Fatalf("Reconcile error = %v, want explicit missing-CRD refusal", err)
	}
	if client.dryRunUpdates != 0 || client.realUpdates != 0 {
		t.Fatalf("missing preflight performed updates: dry-run=%d real=%d", client.dryRunUpdates, client.realUpdates)
	}
}

func TestReconcilePreflightsEveryChangeBeforeMutation(t *testing.T) {
	candidates := mustCandidates(t)
	objects := readyObjects(candidates)
	for _, object := range objects {
		object.Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "outdated schema"
	}
	client := &memoryClient{
		objects:       objects,
		dryRunFailure: map[string]error{PtahSchemaPlanCRDName: errors.New("policy denied update")},
	}
	manager := &Manager{Client: client, PollInterval: time.Millisecond}
	err := manager.reconcile(context.Background(), nil)
	if err == nil || !contains(err.Error(), "dry-run CRD update") {
		t.Fatalf("Reconcile error = %v, want dry-run failure", err)
	}
	if client.realUpdates != 0 {
		t.Fatalf("preflight failure allowed %d real updates", client.realUpdates)
	}
	for _, object := range client.objects {
		if object.Spec.Versions[0].Schema.OpenAPIV3Schema.Description != "outdated schema" {
			t.Fatalf("preflight failure changed %s", object.Name)
		}
	}
}

func TestReconcileStatePreflightLeavesEveryCRDUnchangedOnFutureState(t *testing.T) {
	candidates := mustCandidates(t)
	objects := readyObjects(candidates)
	before := make(map[string]*apiextensionsv1.CustomResourceDefinition, len(objects))
	for name, object := range objects {
		object.Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "outdated schema"
		object.ResourceVersion = "future-state-proof-" + name
		before[name] = object.DeepCopy()
	}
	client := &memoryClient{objects: objects}
	manager := &Manager{Client: client, PollInterval: time.Millisecond}
	schemas := &schemaListClient{pages: []*unstructured.UnstructuredList{{
		Items: []unstructured.Unstructured{
			schemaWithControllerStateAt("tenant-a", "schema-a", int64(2), "status", "pendingObservation", "plan", "controllerStateVersion"),
		},
	}}}
	err := manager.ReconcileWithStatePreflight(context.Background(), storedStateClientsWithSchemas(schemas), 1)
	if err == nil || !contains(err.Error(), "controller downgrade refused") {
		t.Fatalf("ReconcileWithStatePreflight error = %v, want downgrade refusal", err)
	}
	if client.dryRunUpdates != 0 || client.realUpdates != 0 {
		t.Fatalf("state preflight performed CRD updates: dry-run=%d real=%d", client.dryRunUpdates, client.realUpdates)
	}
	for name, want := range before {
		if got := client.objects[name]; !reflect.DeepEqual(got, want) {
			t.Fatalf("CRD %s changed during controller-state preflight", name)
		}
	}
}

func TestReconcileStatePreflightRequiresEveryDurableClientBeforeCRDWork(t *testing.T) {
	candidates := mustCandidates(t)
	client := &memoryClient{objects: readyObjects(candidates)}
	manager := &Manager{Client: client, PollInterval: time.Millisecond}
	state := StoredControllerStateClients{Schemas: &schemaListClient{}}

	err := manager.ReconcileWithStatePreflight(context.Background(), state, 1)
	if err == nil || !contains(err.Error(), "PtahSchemaPlan client is required") {
		t.Fatalf("ReconcileWithStatePreflight error = %v, want missing-plan-client refusal", err)
	}
	if client.dryRunUpdates != 0 || client.realUpdates != 0 {
		t.Fatalf("missing durable-state client performed CRD updates: dry-run=%d real=%d", client.dryRunUpdates, client.realUpdates)
	}
}

func TestReconcileRepeatsStatePreflightAfterDryRuns(t *testing.T) {
	candidates := mustCandidates(t)
	objects := readyObjects(candidates)
	objects[PtahSchemaApprovalCRDName].Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "outdated schema"
	client := &memoryClient{objects: objects}
	manager := &Manager{Client: client, PollInterval: time.Millisecond}
	schemas := &schemaListClient{pages: []*unstructured.UnstructuredList{
		{
			Items: []unstructured.Unstructured{
				schemaWithControllerStateAt("tenant-a", "schema-a", int64(1), "status", "executionBinding", "controllerStateVersion"),
			},
		},
		{
			Items: []unstructured.Unstructured{
				schemaWithControllerStateAt("tenant-a", "schema-a", int64(2), "status", "pendingObservation", "plan", "controllerStateVersion"),
			},
		},
	}}
	err := manager.ReconcileWithStatePreflight(context.Background(), storedStateClientsWithSchemas(schemas), 1)
	if err == nil || !contains(err.Error(), "repeat stored controller-state preflight") ||
		!contains(err.Error(), "controller downgrade refused") {
		t.Fatalf("ReconcileWithStatePreflight error = %v, want repeated downgrade refusal", err)
	}
	if client.dryRunUpdates != 1 || client.realUpdates != 0 {
		t.Fatalf("repeated state preflight updates dry-run=%d real=%d, want 1 and 0", client.dryRunUpdates, client.realUpdates)
	}
	if len(schemas.options) != 2 {
		t.Fatalf("stored-state list calls = %d, want 2", len(schemas.options))
	}
}

func TestReconcileRepeatsStatePreflightAfterReleaseCutover(t *testing.T) {
	candidates := mustCandidates(t)
	objects := readyObjects(candidates)
	objects[PtahSchemaApprovalCRDName].Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "outdated schema"
	client := &memoryClient{objects: objects}
	manager := &Manager{Client: client, PollInterval: time.Millisecond}
	schemas := &schemaListClient{pages: []*unstructured.UnstructuredList{
		{
			Items: []unstructured.Unstructured{
				schemaWithControllerStateAt("tenant-a", "schema-a", int64(1), "status", "executionBinding", "controllerStateVersion"),
			},
		},
		{
			Items: []unstructured.Unstructured{
				schemaWithControllerStateAt("tenant-a", "schema-a", int64(1), "status", "executionBinding", "controllerStateVersion"),
			},
		},
		{
			Items: []unstructured.Unstructured{
				schemaWithControllerStateAt("tenant-a", "schema-a", int64(1), "status", "pendingObservation", "plan", "controllerStateVersion"),
			},
		},
	}}
	prepareCalls := 0
	state := storedStateClientsWithSchemas(schemas)
	state.Approvals = &schemaListClient{pages: []*unstructured.UnstructuredList{
		{Items: []unstructured.Unstructured{schemaWithControllerStateAt("tenant-a", "approval-a", int64(1), "spec", "controllerStateVersion")}},
		{Items: []unstructured.Unstructured{schemaWithControllerStateAt("tenant-a", "approval-a", int64(1), "spec", "controllerStateVersion")}},
		{Items: []unstructured.Unstructured{schemaWithControllerStateAt("tenant-a", "approval-a", int64(2), "spec", "controllerStateVersion")}},
	}}
	err := manager.ReconcileWithStatePreflightAndPrepare(
		context.Background(),
		state,
		1,
		func(context.Context) error {
			prepareCalls++
			return nil
		},
	)
	if err == nil || !contains(err.Error(), "final stored controller-state preflight after release cutover") ||
		!contains(err.Error(), "controller downgrade refused") || !contains(err.Error(), "PtahSchemaApproval tenant-a/approval-a") {
		t.Fatalf("ReconcileWithStatePreflightAndPrepare error = %v, want post-cutover downgrade refusal", err)
	}
	if prepareCalls != 1 {
		t.Fatalf("prepare calls = %d, want 1", prepareCalls)
	}
	if client.dryRunUpdates != 1 || client.realUpdates != 0 {
		t.Fatalf("post-cutover state preflight updates dry-run=%d real=%d, want 1 and 0", client.dryRunUpdates, client.realUpdates)
	}
	if len(schemas.options) != 3 {
		t.Fatalf("stored-state list calls = %d, want 3", len(schemas.options))
	}
	for name, client := range map[string]ControllerStateListClient{
		"PtahSchemaPlan":     state.Plans,
		"PtahSchemaApproval": state.Approvals,
	} {
		if calls := len(client.(*schemaListClient).options); calls != 3 {
			t.Fatalf("%s stored-state list calls = %d, want 3", name, calls)
		}
	}
}

func TestReconcileRefusesToRemoveStoredVersion(t *testing.T) {
	candidates := mustCandidates(t)
	objects := readyObjects(candidates)
	objects[PtahSchemaPlanCRDName].Status.StoredVersions = []string{"v1alpha1", "v1beta1"}
	client := &memoryClient{objects: objects}
	manager := &Manager{Client: client, PollInterval: time.Millisecond}
	err := manager.reconcile(context.Background(), nil)
	if err == nil || !contains(err.Error(), `stored version "v1beta1" is absent`) {
		t.Fatalf("Reconcile error = %v, want stored-version refusal", err)
	}
	if client.realUpdates != 0 {
		t.Fatalf("stored-version refusal allowed %d updates", client.realUpdates)
	}
}

func TestReconcileRefusesNewerSchemaVersionBeforeAnyUpdate(t *testing.T) {
	candidates := mustCandidates(t)
	objects := readyObjects(candidates)
	for _, object := range objects {
		object.Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "outdated schema"
	}
	objects[PtahSchemaCRDName].Annotations[SchemaVersionAnnotation] = "2"
	before := make(map[string]*apiextensionsv1.CustomResourceDefinition, len(objects))
	for name, object := range objects {
		before[name] = object.DeepCopy()
	}
	client := &memoryClient{objects: objects}
	manager := &Manager{Client: client, PollInterval: time.Millisecond}
	err := manager.reconcile(context.Background(), nil)
	if err == nil || !contains(err.Error(), "schema rollback refused") {
		t.Fatalf("Reconcile error = %v, want newer schema-version refusal", err)
	}
	if client.dryRunUpdates != 0 || client.realUpdates != 0 {
		t.Fatalf("newer schema marker allowed updates: dry-run=%d real=%d", client.dryRunUpdates, client.realUpdates)
	}
	for name, want := range before {
		if got := client.objects[name]; !reflect.DeepEqual(got, want) {
			t.Fatalf("CRD %s changed after a partial newer-marker preflight", name)
		}
	}
}

func TestReconcileRefusesSameVersionDigestCollisionBeforeAnyUpdate(t *testing.T) {
	candidates := mustCandidates(t)
	objects := readyObjects(candidates)
	for _, object := range objects {
		object.Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "outdated schema"
	}
	objects[PtahSchemaCRDName].Annotations[SchemaDigestAnnotation] =
		"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	before := make(map[string]*apiextensionsv1.CustomResourceDefinition, len(objects))
	for name, object := range objects {
		before[name] = object.DeepCopy()
	}
	client := &memoryClient{objects: objects}
	manager := &Manager{Client: client, PollInterval: time.Millisecond}
	err := manager.reconcile(context.Background(), nil)
	if err == nil || !contains(err.Error(), "schema identity collision refused") {
		t.Fatalf("reconcile error = %v, want same-version digest collision refusal", err)
	}
	if client.dryRunUpdates != 0 || client.realUpdates != 0 {
		t.Fatalf("digest collision allowed updates: dry-run=%d real=%d", client.dryRunUpdates, client.realUpdates)
	}
	for name, want := range before {
		if got := client.objects[name]; !reflect.DeepEqual(got, want) {
			t.Fatalf("CRD %s changed after same-version digest collision", name)
		}
	}
}

func TestReconcileRejectsMalformedSchemaDigests(t *testing.T) {
	for _, digest := range []string{
		"",
		"sha256:abc",
		"sha256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"sha512:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	} {
		t.Run(digest, func(t *testing.T) {
			candidates := mustCandidates(t)
			objects := readyObjects(candidates)
			objects[PtahSchemaPlanCRDName].Annotations[SchemaDigestAnnotation] = digest
			client := &memoryClient{objects: objects}
			manager := &Manager{Client: client, PollInterval: time.Millisecond}
			err := manager.reconcile(context.Background(), nil)
			if err == nil || !contains(err.Error(), "is not a lowercase SHA-256 digest") {
				t.Fatalf("reconcile error = %v, want malformed schema-digest refusal", err)
			}
			if client.dryRunUpdates != 0 || client.realUpdates != 0 {
				t.Fatalf("malformed schema digest %q allowed updates: dry-run=%d real=%d", digest, client.dryRunUpdates, client.realUpdates)
			}
		})
	}
}

func TestCompatibleAllowsOlderVersionWithValidDigest(t *testing.T) {
	candidates := mustCandidates(t)
	existing := candidateByName(candidates, PtahSchemaCRDName).DeepCopy()
	candidate := existing.DeepCopy()
	candidate.Annotations[SchemaVersionAnnotation] = "2"
	existing.Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "older or drifted schema"
	if err := compatible(existing, candidate, false, false); err != nil {
		t.Fatalf("older schema identity was not upgrade-compatible: %v", err)
	}
}

func TestValidateCandidateIdentityRejectsStaleDigest(t *testing.T) {
	candidate := candidateByName(mustCandidates(t), PtahSchemaCRDName).DeepCopy()
	candidate.Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "changed without regeneration"
	err := validateCandidateIdentity(candidate)
	if err == nil || !contains(err.Error(), "does not match normalized spec digest") {
		t.Fatalf("validateCandidateIdentity error = %v, want stale digest refusal", err)
	}
}

func TestReconcileRejectsMalformedSchemaVersions(t *testing.T) {
	for _, version := range []string{"0", "01", "-1", "+1", " 1", "one"} {
		t.Run(version, func(t *testing.T) {
			candidates := mustCandidates(t)
			objects := readyObjects(candidates)
			objects[PtahSchemaPlanCRDName].Annotations[SchemaVersionAnnotation] = version
			client := &memoryClient{objects: objects}
			manager := &Manager{Client: client, PollInterval: time.Millisecond}
			err := manager.reconcile(context.Background(), nil)
			if err == nil || !contains(err.Error(), "not a positive exact decimal version") {
				t.Fatalf("Reconcile error = %v, want malformed schema-version refusal", err)
			}
			if client.dryRunUpdates != 0 || client.realUpdates != 0 {
				t.Fatalf("malformed schema marker %q allowed updates: dry-run=%d real=%d", version, client.dryRunUpdates, client.realUpdates)
			}
		})
	}
}

func TestReconcileAdoptsIncompleteIdentityOnlyForExactCandidateSchema(t *testing.T) {
	candidates := mustCandidates(t)
	objects := readyObjects(candidates)
	target := objects[PtahSchemaApprovalCRDName]
	delete(target.Annotations, SchemaVersionAnnotation)
	delete(target.Annotations, SchemaDigestAnnotation)
	target.Annotations["owner.example.test/preserved"] = "true"
	client := &memoryClient{objects: objects}
	manager := &Manager{Client: client, PollInterval: time.Millisecond}
	if err := manager.reconcile(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	updated := client.objects[PtahSchemaApprovalCRDName]
	if updated.Annotations[SchemaVersionAnnotation] != "1" {
		t.Fatalf("adopted schema version = %q, want 1", updated.Annotations[SchemaVersionAnnotation])
	}
	if updated.Annotations[SchemaDigestAnnotation] != candidateByName(candidates, PtahSchemaApprovalCRDName).Annotations[SchemaDigestAnnotation] {
		t.Fatalf("adopted schema digest = %q", updated.Annotations[SchemaDigestAnnotation])
	}
	if updated.Annotations["owner.example.test/preserved"] != "true" {
		t.Fatalf("legacy adoption changed foreign annotations: %v", updated.Annotations)
	}
	if client.dryRunUpdates != 1 || client.realUpdates != 1 {
		t.Fatalf("legacy adoption updates dry-run=%d real=%d, want 1 and 1", client.dryRunUpdates, client.realUpdates)
	}
}

func TestReconcileAdoptsMissingDigestOnlyForExactCandidateSchema(t *testing.T) {
	candidates := mustCandidates(t)
	objects := readyObjects(candidates)
	target := objects[PtahSchemaPlanCRDName]
	delete(target.Annotations, SchemaDigestAnnotation)
	client := &memoryClient{objects: objects}
	manager := &Manager{Client: client, PollInterval: time.Millisecond}
	if err := manager.reconcile(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	updated := client.objects[PtahSchemaPlanCRDName]
	if updated.Annotations[SchemaDigestAnnotation] != candidateByName(candidates, PtahSchemaPlanCRDName).Annotations[SchemaDigestAnnotation] {
		t.Fatalf("adopted schema digest = %q", updated.Annotations[SchemaDigestAnnotation])
	}
	if client.dryRunUpdates != 1 || client.realUpdates != 1 {
		t.Fatalf("digest adoption updates dry-run=%d real=%d, want 1 and 1", client.dryRunUpdates, client.realUpdates)
	}
}

func TestReconcileRefusesMissingDigestWithSchemaDrift(t *testing.T) {
	candidates := mustCandidates(t)
	objects := readyObjects(candidates)
	target := objects[PtahSchemaPlanCRDName]
	delete(target.Annotations, SchemaDigestAnnotation)
	target.Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "unidentified schema"
	client := &memoryClient{objects: objects}
	manager := &Manager{Client: client, PollInterval: time.Millisecond}
	err := manager.reconcile(context.Background(), nil)
	if err == nil || !contains(err.Error(), "unknown legacy schema mutation without offline migration") {
		t.Fatalf("reconcile error = %v, want missing-digest drift refusal", err)
	}
	if client.dryRunUpdates != 0 || client.realUpdates != 0 {
		t.Fatalf("missing digest with drift allowed updates: dry-run=%d real=%d", client.dryRunUpdates, client.realUpdates)
	}
}

func TestReconcileRefusesIncompleteSchemaIdentityWithDriftBeforeAnyUpdate(t *testing.T) {
	candidates := mustCandidates(t)
	objects := readyObjects(candidates)
	for _, object := range objects {
		object.Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "outdated schema"
	}
	delete(objects[PtahSchemaCRDName].Annotations, SchemaVersionAnnotation)
	delete(objects[PtahSchemaCRDName].Annotations, SchemaDigestAnnotation)
	before := make(map[string]*apiextensionsv1.CustomResourceDefinition, len(objects))
	for name, object := range objects {
		before[name] = object.DeepCopy()
	}
	client := &memoryClient{objects: objects}
	manager := &Manager{Client: client, PollInterval: time.Millisecond}
	err := manager.reconcile(context.Background(), nil)
	if err == nil || !contains(err.Error(), "unknown legacy schema mutation without offline migration") {
		t.Fatalf("Reconcile error = %v, want unmarked drift refusal", err)
	}
	if client.dryRunUpdates != 0 || client.realUpdates != 0 {
		t.Fatalf("unmarked drift allowed updates: dry-run=%d real=%d", client.dryRunUpdates, client.realUpdates)
	}
	for name, want := range before {
		if got := client.objects[name]; !reflect.DeepEqual(got, want) {
			t.Fatalf("CRD %s changed after unmarked drift refusal", name)
		}
	}
}

func TestReconcileRefusesConcurrentIncompleteSchemaIdentityDuringUpdate(t *testing.T) {
	candidates := mustCandidates(t)
	objects := readyObjects(candidates)
	target := objects[PtahSchemaApprovalCRDName]
	target.Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "initial drift"
	client := &memoryClient{objects: objects}
	mutateImmediatelyBeforeUpdate := func() error {
		current := client.objects[PtahSchemaApprovalCRDName]
		delete(current.Annotations, SchemaDigestAnnotation)
		current.Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "concurrent unknown schema"
		return nil
	}
	manager := &Manager{Client: client, PollInterval: time.Millisecond}
	err := manager.reconcile(context.Background(), mutateImmediatelyBeforeUpdate)
	if err == nil || !contains(err.Error(), "unknown legacy schema mutation without offline migration") {
		t.Fatalf("Reconcile error = %v, want concurrent unmarked drift refusal", err)
	}
	if client.dryRunUpdates != 1 || client.realUpdates != 0 {
		t.Fatalf("concurrent unmarked drift updates dry-run=%d real=%d, want 1 and 0", client.dryRunUpdates, client.realUpdates)
	}
	current := client.objects[PtahSchemaApprovalCRDName]
	if _, marked := current.Annotations[SchemaDigestAnnotation]; marked {
		t.Fatal("concurrent missing schema digest was silently restored")
	}
	if current.Spec.Versions[0].Schema.OpenAPIV3Schema.Description != "concurrent unknown schema" {
		t.Fatal("concurrent unknown schema was overwritten")
	}
}

func TestReconcileRefusesConcurrentSameVersionDigestCollisionDuringUpdate(t *testing.T) {
	candidates := mustCandidates(t)
	objects := readyObjects(candidates)
	target := objects[PtahSchemaApprovalCRDName]
	target.Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "initial drift"
	client := &memoryClient{objects: objects}
	mutateImmediatelyBeforeUpdate := func() error {
		current := client.objects[PtahSchemaApprovalCRDName]
		current.Annotations[SchemaDigestAnnotation] =
			"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		return nil
	}
	manager := &Manager{Client: client, PollInterval: time.Millisecond}
	err := manager.reconcile(context.Background(), mutateImmediatelyBeforeUpdate)
	if err == nil || !contains(err.Error(), "schema identity collision refused") {
		t.Fatalf("reconcile error = %v, want concurrent digest collision refusal", err)
	}
	if client.dryRunUpdates != 1 || client.realUpdates != 0 {
		t.Fatalf("concurrent digest collision updates dry-run=%d real=%d, want 1 and 0", client.dryRunUpdates, client.realUpdates)
	}
	current := client.objects[PtahSchemaApprovalCRDName]
	if current.Annotations[SchemaDigestAnnotation] !=
		"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatal("concurrent digest collision was overwritten")
	}
}

func TestReconcileRechecksEveryIdentityAfterDryRunsBeforeAnyRealUpdate(t *testing.T) {
	candidates := mustCandidates(t)
	objects := readyObjects(candidates)
	objects[PtahSchemaApprovalCRDName].Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "outdated schema"
	client := &memoryClient{objects: objects}
	client.afterDryRun = func() {
		client.objects[PtahSchemaCRDName].Annotations[SchemaVersionAnnotation] = "2"
	}
	manager := &Manager{Client: client, PollInterval: time.Millisecond}
	err := manager.reconcile(context.Background(), nil)
	if err == nil || !contains(err.Error(), "changed incompatibly after dry-run") ||
		!contains(err.Error(), "schema rollback refused") {
		t.Fatalf("reconcile error = %v, want post-dry-run identity refusal", err)
	}
	if client.dryRunUpdates != 1 || client.realUpdates != 0 {
		t.Fatalf("post-dry-run identity change updates dry-run=%d real=%d, want 1 and 0", client.dryRunUpdates, client.realUpdates)
	}
}

func TestReconcileAdoptsOnlyTheCompleteKnownPredecessorSet(t *testing.T) {
	candidates := mustCandidates(t)
	objects, digests := fakePredecessorObjects(t, candidates)
	replacePredecessorDigests(t, digests)
	client := &memoryClient{objects: objects}
	manager := &Manager{Client: client, PollInterval: time.Millisecond}
	if err := manager.reconcile(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if client.dryRunUpdates != len(candidates) || client.realUpdates != len(candidates) {
		t.Fatalf("predecessor updates dry-run=%d real=%d, want %d each", client.dryRunUpdates, client.realUpdates, len(candidates))
	}
	for _, candidate := range candidates {
		updated := client.objects[candidate.Name]
		matches, err := sameSpec(updated, candidate)
		if err != nil || !matches {
			t.Fatalf("adopted CRD %s spec match=%t error=%v", candidate.Name, matches, err)
		}
		identityMatches, err := sameSchemaIdentity(updated, candidate)
		if err != nil || !identityMatches {
			t.Fatalf("adopted CRD %s identity match=%t error=%v", candidate.Name, identityMatches, err)
		}
	}
}

func TestPreflightWithStateRunsEveryDryRunWithoutPersistingCRDs(t *testing.T) {
	candidates := mustCandidates(t)
	objects, digests := fakePredecessorObjects(t, candidates)
	replacePredecessorDigests(t, digests)
	before := make(map[string]*apiextensionsv1.CustomResourceDefinition, len(objects))
	for name, object := range objects {
		before[name] = object.DeepCopy()
	}
	client := &memoryClient{objects: objects}
	manager := &Manager{Client: client, PollInterval: time.Millisecond}
	if err := manager.PreflightWithState(context.Background(), emptyStoredStateClients(), 1); err != nil {
		t.Fatal(err)
	}
	if client.dryRunUpdates != len(candidates) || client.realUpdates != 0 {
		t.Fatalf("preflight updates dry-run=%d real=%d, want %d and 0", client.dryRunUpdates, client.realUpdates, len(candidates))
	}
	for name, want := range before {
		if got := client.objects[name]; !reflect.DeepEqual(got, want) {
			t.Fatalf("preflight changed CRD %s", name)
		}
	}
}

func TestReconcileResumesPartialKnownPredecessorTransition(t *testing.T) {
	candidates := mustCandidates(t)
	objects, digests := fakePredecessorObjects(t, candidates)
	replacePredecessorDigests(t, digests)
	alreadyAdopted := candidates[0].DeepCopy()
	alreadyAdopted.Status = *objects[alreadyAdopted.Name].Status.DeepCopy()
	objects[alreadyAdopted.Name] = alreadyAdopted
	client := &memoryClient{objects: objects}
	manager := &Manager{Client: client, PollInterval: time.Millisecond}
	if err := manager.reconcile(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if client.dryRunUpdates != len(candidates)-1 || client.realUpdates != len(candidates)-1 {
		t.Fatalf("resumed updates dry-run=%d real=%d, want %d each", client.dryRunUpdates, client.realUpdates, len(candidates)-1)
	}
}

func TestReconcileRejectsUnknownOrMixedLegacySetBeforeAnyUpdate(t *testing.T) {
	candidates := mustCandidates(t)
	objects, digests := fakePredecessorObjects(t, candidates)
	replacePredecessorDigests(t, digests)
	unknown := objects[PtahSchemaPlanCRDName]
	unknown.Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "unknown legacy schema"
	before := make(map[string]*apiextensionsv1.CustomResourceDefinition, len(objects))
	for name, object := range objects {
		before[name] = object.DeepCopy()
	}
	client := &memoryClient{objects: objects}
	manager := &Manager{Client: client, PollInterval: time.Millisecond}
	err := manager.reconcile(context.Background(), nil)
	if err == nil || !contains(err.Error(), "unknown legacy schema mutation") {
		t.Fatalf("reconcile error = %v, want unknown legacy refusal", err)
	}
	if client.dryRunUpdates != 0 || client.realUpdates != 0 {
		t.Fatalf("unknown legacy set updates dry-run=%d real=%d", client.dryRunUpdates, client.realUpdates)
	}
	for name, want := range before {
		if !reflect.DeepEqual(client.objects[name], want) {
			t.Fatalf("unknown legacy refusal changed CRD %s", name)
		}
	}
}

func TestReconcileRefusesNewerControllerStateMarkerBeforeAnyUpdate(t *testing.T) {
	candidates := mustCandidates(t)
	objects := readyObjects(candidates)
	objects[PtahSchemaPlanCRDName].Annotations[ControllerStateVersionAnnotation] = "2"
	client := &memoryClient{objects: objects}
	manager := &Manager{Client: client, PollInterval: time.Millisecond}
	err := manager.reconcile(context.Background(), nil)
	if err == nil || !contains(err.Error(), "controller-state rollback refused") {
		t.Fatalf("reconcile error = %v, want controller-state rollback refusal", err)
	}
	if client.dryRunUpdates != 0 || client.realUpdates != 0 {
		t.Fatalf("newer controller-state marker updates dry-run=%d real=%d", client.dryRunUpdates, client.realUpdates)
	}
}

func TestReconcileRequiresSupportedVersionToMatchCompiledContract(t *testing.T) {
	candidates := mustCandidates(t)
	client := &memoryClient{objects: readyObjects(candidates)}
	manager := &Manager{Client: client, PollInterval: time.Millisecond}
	err := manager.ReconcileWithStatePreflight(context.Background(), emptyStoredStateClients(), int64(controllerstate.CurrentVersion)+1)
	if err == nil || !contains(err.Error(), "does not match compiled version") {
		t.Fatalf("ReconcileWithStatePreflight error = %v, want compiled-version mismatch", err)
	}
	if client.dryRunUpdates != 0 || client.realUpdates != 0 {
		t.Fatal("compiled-version mismatch performed CRD updates")
	}
}

func TestValidateCandidateIdentityRejectsControllerStateMarkerMismatch(t *testing.T) {
	candidate := candidateByName(mustCandidates(t), PtahSchemaCRDName).DeepCopy()
	candidate.Annotations[ControllerStateVersionAnnotation] = "2"
	err := validateCandidateIdentity(candidate)
	if err == nil || !contains(err.Error(), "does not match compiled controller-state version") {
		t.Fatalf("validateCandidateIdentity error = %v, want controller-state mismatch", err)
	}
}

func TestVerifyFailsOnSchemaDriftAndRejectedNames(t *testing.T) {
	candidates := mustCandidates(t)
	objects := readyObjects(candidates)
	objects[PtahSchemaCRDName].Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "drift"
	manager := &Manager{Client: &memoryClient{objects: objects}, PollInterval: time.Millisecond}
	if err := manager.Verify(context.Background()); err == nil || !contains(err.Error(), "does not match") {
		t.Fatalf("Verify drift error = %v", err)
	}

	objects = readyObjects(candidates)
	objects[PtahSchemaCRDName].Annotations[SchemaDigestAnnotation] =
		"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	manager.Client = &memoryClient{objects: objects}
	if err := manager.Verify(context.Background()); err == nil || !contains(err.Error(), "schema identity") {
		t.Fatalf("Verify digest error = %v", err)
	}

	objects = readyObjects(candidates)
	objects[PtahSchemaCRDName].Status.Conditions = []apiextensionsv1.CustomResourceDefinitionCondition{
		{Type: apiextensionsv1.NamesAccepted, Status: apiextensionsv1.ConditionFalse, Message: "name conflict"},
	}
	manager.Client = &memoryClient{objects: objects}
	if err := manager.Verify(context.Background()); err == nil || !contains(err.Error(), "names were rejected") {
		t.Fatalf("Verify rejected-name error = %v", err)
	}
}

func mustCandidates(t *testing.T) []*apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	candidates, err := Candidates()
	if err != nil {
		t.Fatal(err)
	}
	return candidates
}

func readyObjects(candidates []*apiextensionsv1.CustomResourceDefinition) map[string]*apiextensionsv1.CustomResourceDefinition {
	objects := make(map[string]*apiextensionsv1.CustomResourceDefinition, len(candidates))
	for i, candidate := range candidates {
		object := candidate.DeepCopy()
		object.UID = types.UID(fmt.Sprintf("uid-%d", i))
		object.ResourceVersion = "1"
		object.Status.StoredVersions = []string{"v1alpha1"}
		object.Status.Conditions = []apiextensionsv1.CustomResourceDefinitionCondition{
			{Type: apiextensionsv1.NamesAccepted, Status: apiextensionsv1.ConditionTrue},
			{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue},
		}
		objects[object.Name] = object
	}
	return objects
}

func fakePredecessorObjects(
	t *testing.T,
	candidates []*apiextensionsv1.CustomResourceDefinition,
) (map[string]*apiextensionsv1.CustomResourceDefinition, map[string]string) {
	t.Helper()
	objects := readyObjects(candidates)
	digests := make(map[string]string, len(objects))
	for name, object := range objects {
		delete(object.Annotations, SchemaVersionAnnotation)
		delete(object.Annotations, SchemaDigestAnnotation)
		delete(object.Annotations, ControllerStateVersionAnnotation)
		object.Spec.Versions[0].Schema.OpenAPIV3Schema.Description = "known predecessor " + name
		digest, err := ComputeSchemaDigest(object)
		if err != nil {
			t.Fatalf("compute fake predecessor digest for %s: %v", name, err)
		}
		digests[name] = digest
	}
	return objects, digests
}

func replacePredecessorDigests(t *testing.T, replacement map[string]string) {
	t.Helper()
	original := predecessorSchemaDigests
	predecessorSchemaDigests = replacement
	t.Cleanup(func() { predecessorSchemaDigests = original })
}

func candidateByName(candidates []*apiextensionsv1.CustomResourceDefinition, name string) *apiextensionsv1.CustomResourceDefinition {
	for _, candidate := range candidates {
		if candidate.Name == name {
			return candidate
		}
	}
	panic("candidate not found: " + name)
}

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}

type memoryClient struct {
	objects       map[string]*apiextensionsv1.CustomResourceDefinition
	dryRunFailure map[string]error
	dryRunUpdates int
	realUpdates   int
	afterDryRun   func()
}

func (c *memoryClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*apiextensionsv1.CustomResourceDefinition, error) {
	object, found := c.objects[name]
	if !found {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: apiextensionsv1.GroupName, Resource: "customresourcedefinitions"}, name)
	}
	return object.DeepCopy(), nil
}

func (c *memoryClient) Update(_ context.Context, object *apiextensionsv1.CustomResourceDefinition, options metav1.UpdateOptions) (*apiextensionsv1.CustomResourceDefinition, error) {
	if len(options.DryRun) != 0 {
		c.dryRunUpdates++
		if err := c.dryRunFailure[object.Name]; err != nil {
			return nil, err
		}
		result := object.DeepCopy()
		if c.afterDryRun != nil {
			afterDryRun := c.afterDryRun
			c.afterDryRun = nil
			afterDryRun()
		}
		return result, nil
	}
	c.realUpdates++
	current, found := c.objects[object.Name]
	if !found {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: apiextensionsv1.GroupName, Resource: "customresourcedefinitions"}, object.Name)
	}
	updated := object.DeepCopy()
	updated.Status = *current.Status.DeepCopy()
	updated.ResourceVersion = fmt.Sprintf("%d", c.realUpdates+1)
	c.objects[object.Name] = updated
	return updated.DeepCopy(), nil
}
