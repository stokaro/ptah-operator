package crdupgrade

// These tests intentionally use package internals. The sealed inventory must
// exercise the same private constructors and semantic projection as the
// production retirement state machine.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func TestPredecessorRetirementSealCurrentBuildsExactCanonicalInventory(t *testing.T) {
	t.Parallel()

	fixture := newPredecessorRetirementFixture(t)
	if err := fixture.retirement.SealCurrent(context.Background()); err != nil {
		t.Fatalf("SealCurrent() error = %v", err)
	}
	marker := fixture.configMaps.objects[fixture.currentMarkerName]
	guard := NewAdmissionConvergenceGuard(fixture.rollout)
	inventory, err := guard.verifySealedMarker(marker)
	if err != nil {
		t.Fatalf("verifySealedMarker() error = %v", err)
	}
	if marker.Immutable == nil || !*marker.Immutable {
		t.Fatal("sealed marker is mutable")
	}
	if len(inventory.Entries) != 25 {
		t.Fatalf("sealed inventory entry count = %d, want 25", len(inventory.Entries))
	}
	for index := 0; index < 24; index += 2 {
		if inventory.Entries[index].Kind != "ValidatingAdmissionPolicy" ||
			inventory.Entries[index+1].Kind != "ValidatingAdmissionPolicyBinding" ||
			inventory.Entries[index].Name != inventory.Entries[index+1].Name {
			t.Fatalf("sealed pair %d = %#v / %#v", index/2, inventory.Entries[index], inventory.Entries[index+1])
		}
	}
	if got := inventory.Entries[24]; got.Kind != "ConfigMap" || got.Name != fixture.probeName {
		t.Fatalf("sealed hook probe = %#v", got)
	}
	raw := marker.Data[PredecessorRetirementInventoryDataKey]
	encoded, err := encodePredecessorRetirementInventory(inventory)
	if err != nil {
		t.Fatal(err)
	}
	if raw != encoded {
		t.Fatalf("sealed inventory is not canonical: got %q want %q", raw, encoded)
	}

	updates := fixture.configMaps.updates
	if updates != 1 {
		t.Fatalf("marker updates = %d, want 1", updates)
	}
	if err := fixture.retirement.SealCurrent(context.Background()); err != nil {
		t.Fatalf("idempotent SealCurrent() error = %v", err)
	}
	if fixture.configMaps.updates != updates {
		t.Fatalf("idempotent SealCurrent() updates = %d, want %d", fixture.configMaps.updates, updates)
	}
}

func TestPredecessorRetirementRejectsSemanticDriftAfterSeal(t *testing.T) {
	t.Parallel()

	fixture := newPredecessorRetirementFixture(t)
	if err := fixture.retirement.SealCurrent(context.Background()); err != nil {
		t.Fatal(err)
	}
	name := fixture.pairNames[0]
	fixture.policies.objects[name].Annotations["foreign.example/change"] = "true"
	if err := fixture.retirement.VerifyCurrentSealed(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "inventory differs from exact live objects") {
		t.Fatalf("VerifyCurrentSealed() error = %v, want semantic drift", err)
	}
}

func TestPredecessorSemanticDigestExcludesServerBookkeepingAndStatus(t *testing.T) {
	t.Parallel()

	fixture := newPredecessorRetirementFixture(t)
	policy := fixture.policies.objects[fixture.pairNames[0]].DeepCopy()
	first, err := predecessorPolicyDigest(policy)
	if err != nil {
		t.Fatal(err)
	}
	policy.ResourceVersion = "999"
	policy.Generation = 44
	policy.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "server"}}
	policy.Status.ObservedGeneration = 77
	second, err := predecessorPolicyDigest(policy)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("server bookkeeping changed semantic digest: %s != %s", first, second)
	}
	policy.Spec.Validations[0].Expression += " && true"
	third, err := predecessorPolicyDigest(policy)
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("policy spec mutation did not change semantic digest")
	}
}

func TestPredecessorRetirementDeletesBindingsThenPoliciesThenMarkers(t *testing.T) {
	t.Parallel()

	fixture := newSealedPredecessorRetirementFixture(t)
	barrierCalls := 0
	err := fixture.retirement.Retire(context.Background(), func(_ context.Context, target PredecessorRetirementBarrierTarget) error {
		barrierCalls++
		fixture.recorder.events = append(fixture.recorder.events, "Barrier")
		if err := target.VerifyMarker(fixture.configMaps.objects[target.MarkerName()]); err != nil {
			return err
		}
		probes := target.Probes()
		if len(probes) != 12 {
			return fmt.Errorf("probe count = %d, want 12", len(probes))
		}
		for index, name := range fixture.pairNames {
			if probes[index].PolicyName != name || probes[index].BindingName != name ||
				probes[index].FieldManager == "" || probes[index].Message == "" {
				return fmt.Errorf("probe %d = %#v, want %s", index, probes[index], name)
			}
			if fixture.bindings.objects[name] != nil {
				return fmt.Errorf("binding %s remained before barrier", name)
			}
			if fixture.policies.objects[name] == nil {
				return fmt.Errorf("policy %s was deleted before barrier", name)
			}
		}
		if fixture.configMaps.objects[fixture.probeName] == nil {
			return errors.New("hook probe was deleted before barrier")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Retire() error = %v", err)
	}
	if barrierCalls != 1 {
		t.Fatalf("barrier calls = %d, want 1", barrierCalls)
	}
	if len(fixture.policies.objects) != 0 || len(fixture.bindings.objects) != 0 || len(fixture.configMaps.objects) != 0 {
		t.Fatalf("retirement left residue: policies=%d bindings=%d ConfigMaps=%d", len(fixture.policies.objects), len(fixture.bindings.objects), len(fixture.configMaps.objects))
	}
	if got := fixture.recorder.kinds(); !reflect.DeepEqual(got, []string{
		"ValidatingAdmissionPolicyBinding", "ValidatingAdmissionPolicyBinding", "ValidatingAdmissionPolicyBinding", "ValidatingAdmissionPolicyBinding",
		"ValidatingAdmissionPolicyBinding", "ValidatingAdmissionPolicyBinding", "ValidatingAdmissionPolicyBinding", "ValidatingAdmissionPolicyBinding",
		"ValidatingAdmissionPolicyBinding", "ValidatingAdmissionPolicyBinding", "ValidatingAdmissionPolicyBinding", "ValidatingAdmissionPolicyBinding",
		"Barrier",
		"ValidatingAdmissionPolicy", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicy",
		"ValidatingAdmissionPolicy", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicy",
		"ValidatingAdmissionPolicy", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicy",
		"ConfigMap", "ConfigMap",
	}) {
		t.Fatalf("retirement order = %v", got)
	}
	for _, event := range fixture.recorder.events {
		if event == "Barrier" {
			continue
		}
		if !strings.Contains(event, "uid=") || !strings.Contains(event, "rv=") {
			t.Fatalf("delete omitted preconditions: %s", event)
		}
	}
}

func TestPredecessorRetirementResumesOnlyContiguousPhases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		deletedBindings  int
		deletedPolicies  int
		deleteProbe      bool
		wantBarrierCalls int
	}{
		{name: "fresh", wantBarrierCalls: 1},
		{name: "partial bindings", deletedBindings: 5, wantBarrierCalls: 1},
		{name: "all bindings", deletedBindings: 12, wantBarrierCalls: 1},
		{name: "partial policies", deletedBindings: 12, deletedPolicies: 7, wantBarrierCalls: 1},
		{name: "all pairs", deletedBindings: 12, deletedPolicies: 12},
		{name: "marker only", deletedBindings: 12, deletedPolicies: 12, deleteProbe: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newSealedPredecessorRetirementFixture(t)
			for _, name := range fixture.pairNames[:test.deletedBindings] {
				delete(fixture.bindings.objects, name)
			}
			for _, name := range fixture.pairNames[:test.deletedPolicies] {
				delete(fixture.policies.objects, name)
			}
			if test.deleteProbe {
				delete(fixture.configMaps.objects, fixture.probeName)
			}
			calls := 0
			err := fixture.retirement.Retire(context.Background(), func(context.Context, PredecessorRetirementBarrierTarget) error {
				calls++
				return nil
			})
			if err != nil {
				t.Fatalf("Retire() error = %v", err)
			}
			if calls != test.wantBarrierCalls {
				t.Fatalf("barrier calls = %d, want %d", calls, test.wantBarrierCalls)
			}
			if len(fixture.policies.objects) != 0 || len(fixture.bindings.objects) != 0 || len(fixture.configMaps.objects) != 0 {
				t.Fatal("resumed retirement left residue")
			}
		})
	}
}

func TestPredecessorRetirementRejectsSparseAndForeignStates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*predecessorRetirementFixture)
		wantErr string
	}{
		{
			name: "binding hole",
			mutate: func(f *predecessorRetirementFixture) {
				delete(f.bindings.objects, f.pairNames[1])
			},
			wantErr: "binding inventory is sparse",
		},
		{
			name: "policy before bindings",
			mutate: func(f *predecessorRetirementFixture) {
				delete(f.policies.objects, f.pairNames[0])
			},
			wantErr: "absent before all bindings",
		},
		{
			name: "policy hole",
			mutate: func(f *predecessorRetirementFixture) {
				for _, name := range f.pairNames {
					delete(f.bindings.objects, name)
				}
				delete(f.policies.objects, f.pairNames[1])
			},
			wantErr: "policy inventory is sparse",
		},
		{
			name: "probe too early",
			mutate: func(f *predecessorRetirementFixture) {
				delete(f.configMaps.objects, f.probeName)
			},
			wantErr: "hook probe is absent",
		},
		{
			name: "UID replacement",
			mutate: func(f *predecessorRetirementFixture) {
				f.policies.objects[f.pairNames[0]].UID = "replacement"
			},
			wantErr: "foreign or incomplete live identity",
		},
		{
			name: "foreign finalizer",
			mutate: func(f *predecessorRetirementFixture) {
				f.policies.objects[f.pairNames[0]].Finalizers = []string{"foreign.example/hold"}
			},
			wantErr: "foreign or incomplete live identity",
		},
		{
			name: "semantic drift",
			mutate: func(f *predecessorRetirementFixture) {
				f.bindings.objects[f.pairNames[0]].Spec.ValidationActions = nil
			},
			wantErr: "sealed semantic digest",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newSealedPredecessorRetirementFixture(t)
			test.mutate(fixture)
			err := fixture.retirement.Preflight(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Preflight() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestPredecessorRetirementRejectsUnsealedActiveMarker(t *testing.T) {
	t.Parallel()

	fixture := newPredecessorRetirementFixture(t)
	fixture.advanceToNextRelease(t)
	err := fixture.retirement.Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sealed marker") {
		t.Fatalf("Preflight() error = %v, want sealed marker rejection", err)
	}
}

func TestPredecessorRetirementBarrierFailurePreservesPoliciesAndMarkers(t *testing.T) {
	t.Parallel()

	fixture := newSealedPredecessorRetirementFixture(t)
	want := errors.New("endpoint not converged")
	err := fixture.retirement.Retire(context.Background(), func(context.Context, PredecessorRetirementBarrierTarget) error {
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("Retire() error = %v, want %v", err, want)
	}
	if len(fixture.bindings.objects) != 0 {
		t.Fatalf("bindings remaining after barrier boundary = %d", len(fixture.bindings.objects))
	}
	if len(fixture.policies.objects) != 12 || fixture.configMaps.objects[fixture.probeName] == nil ||
		fixture.configMaps.objects[fixture.previousMarkerName] == nil {
		t.Fatal("barrier failure removed a policy or marker")
	}
}

func TestPredecessorRetirementAbsentMarkerIsCompletedState(t *testing.T) {
	t.Parallel()

	fixture := newSealedPredecessorRetirementFixture(t)
	delete(fixture.configMaps.objects, fixture.previousMarkerName)
	calls := 0
	if err := fixture.retirement.Retire(context.Background(), func(context.Context, PredecessorRetirementBarrierTarget) error {
		calls++
		return nil
	}); err != nil {
		t.Fatalf("Retire() error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("barrier calls = %d, want 0", calls)
	}
}

func TestPredecessorRetirementInventoryRejectsNoncanonicalAndUnknownJSON(t *testing.T) {
	t.Parallel()

	fixture := newPredecessorRetirementFixture(t)
	if err := fixture.retirement.SealCurrent(context.Background()); err != nil {
		t.Fatal(err)
	}
	marker := fixture.configMaps.objects[fixture.currentMarkerName]
	raw := marker.Data[PredecessorRetirementInventoryDataKey]
	expected := predecessorRetirementExpectedEntries(
		fixture.rollout.ReleaseNamespace,
		fixture.rollout.ReleaseName,
		fixture.rollout.ReleaseSequence,
		fixture.rollout.ManagerImage,
	)
	if _, err := decodePredecessorRetirementInventory(" "+raw, expected); err == nil {
		t.Fatal("padded inventory was accepted")
	}
	withUnknown := strings.Replace(raw, `{"version":"1"`, `{"unknown":true,"version":"1"`, 1)
	if _, err := decodePredecessorRetirementInventory(withUnknown, expected); err == nil {
		t.Fatal("unknown inventory field was accepted")
	}
}

type predecessorRetirementFixture struct {
	rollout            *RolloutGuard
	retirement         *PredecessorRetirement
	policies           *predecessorRetirementPolicyClient
	bindings           *predecessorRetirementBindingClient
	configMaps         *predecessorRetirementConfigMapClient
	recorder           *predecessorRetirementRecorder
	pairNames          []string
	probeName          string
	currentMarkerName  string
	previousMarkerName string
}

func newPredecessorRetirementFixture(t *testing.T) *predecessorRetirementFixture {
	t.Helper()

	rollout, _, _, _ := readyRolloutGuard()
	recorder := &predecessorRetirementRecorder{}
	policies := &predecessorRetirementPolicyClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{}, recorder: recorder}
	bindings := &predecessorRetirementBindingClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding{}, recorder: recorder}
	configMaps := &predecessorRetirementConfigMapClient{objects: map[string]*corev1.ConfigMap{}, recorder: recorder}
	rollout.Policies = policies
	rollout.Bindings = bindings

	blueprints, err := predecessorRetirementPairBlueprints(rollout)
	if err != nil {
		t.Fatal(err)
	}
	pairNames := make([]string, 0, len(blueprints))
	for index, blueprint := range blueprints {
		policy := readyPolicy(blueprint.policy)
		setPredecessorRetirementIdentity(policy, "policy", index)
		policies.objects[blueprint.name] = policy
		binding := blueprint.binding.DeepCopy()
		setPredecessorRetirementIdentity(binding, "binding", index)
		bindings.objects[blueprint.name] = binding
		pairNames = append(pairNames, blueprint.name)
	}
	probe := predecessorHookProbeObject(rollout)
	setPredecessorRetirementIdentity(probe, "probe", 0)
	configMaps.objects[probe.Name] = probe
	marker := NewAdmissionConvergenceGuard(rollout).unsealedMarker()
	setPredecessorRetirementIdentity(marker, "marker", 0)
	configMaps.objects[marker.Name] = marker

	return &predecessorRetirementFixture{
		rollout:           rollout,
		retirement:        NewPredecessorRetirement(rollout, policies, bindings, configMaps),
		policies:          policies,
		bindings:          bindings,
		configMaps:        configMaps,
		recorder:          recorder,
		pairNames:         pairNames,
		probeName:         probe.Name,
		currentMarkerName: marker.Name,
	}
}

func newSealedPredecessorRetirementFixture(t *testing.T) *predecessorRetirementFixture {
	t.Helper()
	fixture := newPredecessorRetirementFixture(t)
	if err := fixture.retirement.SealCurrent(context.Background()); err != nil {
		t.Fatalf("seal predecessor fixture: %v", err)
	}
	fixture.previousMarkerName = fixture.currentMarkerName
	fixture.recorder.events = nil
	fixture.advanceToNextRelease(t)
	return fixture
}

func (f *predecessorRetirementFixture) advanceToNextRelease(t *testing.T) {
	t.Helper()
	previous := f.rollout.ReleaseSequence
	f.rollout.ReleaseSequence++
	f.rollout.PreviousControllerReleaseSequence = previous
	f.rollout.ManagerImage = "registry.example/ptah@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	f.rollout.HookServiceAccountName = "ptah-crd-v2-" + hookIdentityDigest(
		f.rollout.ReleaseNamespace, f.rollout.ReleaseName, f.rollout.ReleaseSequence, f.rollout.ManagerImage,
	)[:12]
	f.retirement = NewPredecessorRetirement(f.rollout, f.policies, f.bindings, f.configMaps)
}

func setPredecessorRetirementIdentity(object metav1.Object, prefix string, index int) {
	object.SetUID(types.UID(fmt.Sprintf("%s-uid-%d", prefix, index)))
	object.SetResourceVersion(fmt.Sprintf("%d", index+1))
}

type predecessorRetirementRecorder struct {
	events []string
}

func (r *predecessorRetirementRecorder) record(kind, name string, options metav1.DeleteOptions, object metav1.Object) error {
	if object == nil {
		return apierrors.NewNotFound(predecessorRetirementGroupResource(kind), name)
	}
	if options.Preconditions == nil || options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil {
		return errors.New("delete preconditions are missing")
	}
	if *options.Preconditions.UID != object.GetUID() || *options.Preconditions.ResourceVersion != object.GetResourceVersion() {
		return apierrors.NewConflict(predecessorRetirementGroupResource(kind), name, errors.New("precondition mismatch"))
	}
	r.events = append(r.events, fmt.Sprintf("%s/%s uid=%s rv=%s", kind, name, *options.Preconditions.UID, *options.Preconditions.ResourceVersion))
	return nil
}

func (r *predecessorRetirementRecorder) kinds() []string {
	kinds := make([]string, 0, len(r.events))
	for _, event := range r.events {
		if event == "Barrier" {
			kinds = append(kinds, event)
			continue
		}
		kind, _, _ := strings.Cut(event, "/")
		kinds = append(kinds, kind)
	}
	return kinds
}

type predecessorRetirementPolicyClient struct {
	objects  map[string]*admissionregistrationv1.ValidatingAdmissionPolicy
	recorder *predecessorRetirementRecorder
}

func (c *predecessorRetirementPolicyClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*admissionregistrationv1.ValidatingAdmissionPolicy, error) {
	object := c.objects[name]
	if object == nil {
		return nil, apierrors.NewNotFound(predecessorRetirementGroupResource("ValidatingAdmissionPolicy"), name)
	}
	return object.DeepCopy(), nil
}

func (c *predecessorRetirementPolicyClient) Delete(_ context.Context, name string, options metav1.DeleteOptions) error {
	object := c.objects[name]
	if err := c.recorder.record("ValidatingAdmissionPolicy", name, options, object); err != nil {
		return err
	}
	delete(c.objects, name)
	return nil
}

type predecessorRetirementBindingClient struct {
	objects  map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding
	recorder *predecessorRetirementRecorder
}

func (c *predecessorRetirementBindingClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*admissionregistrationv1.ValidatingAdmissionPolicyBinding, error) {
	object := c.objects[name]
	if object == nil {
		return nil, apierrors.NewNotFound(predecessorRetirementGroupResource("ValidatingAdmissionPolicyBinding"), name)
	}
	return object.DeepCopy(), nil
}

func (c *predecessorRetirementBindingClient) Delete(_ context.Context, name string, options metav1.DeleteOptions) error {
	object := c.objects[name]
	if err := c.recorder.record("ValidatingAdmissionPolicyBinding", name, options, object); err != nil {
		return err
	}
	delete(c.objects, name)
	return nil
}

type predecessorRetirementConfigMapClient struct {
	objects  map[string]*corev1.ConfigMap
	recorder *predecessorRetirementRecorder
	updates  int
}

func (c *predecessorRetirementConfigMapClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*corev1.ConfigMap, error) {
	object := c.objects[name]
	if object == nil {
		return nil, apierrors.NewNotFound(predecessorRetirementGroupResource("ConfigMap"), name)
	}
	return object.DeepCopy(), nil
}

func (c *predecessorRetirementConfigMapClient) Update(_ context.Context, object *corev1.ConfigMap, _ metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	stored := c.objects[object.Name]
	if stored == nil {
		return nil, apierrors.NewNotFound(predecessorRetirementGroupResource("ConfigMap"), object.Name)
	}
	if object.UID != stored.UID || object.ResourceVersion != stored.ResourceVersion {
		return nil, apierrors.NewConflict(predecessorRetirementGroupResource("ConfigMap"), object.Name, errors.New("identity changed"))
	}
	copy := object.DeepCopy()
	copy.ResourceVersion = copy.ResourceVersion + "-sealed"
	c.objects[copy.Name] = copy
	c.updates++
	return copy.DeepCopy(), nil
}

func (c *predecessorRetirementConfigMapClient) Delete(_ context.Context, name string, options metav1.DeleteOptions) error {
	object := c.objects[name]
	if err := c.recorder.record("ConfigMap", name, options, object); err != nil {
		return err
	}
	delete(c.objects, name)
	return nil
}

func predecessorRetirementGroupResource(kind string) schema.GroupResource {
	switch kind {
	case "ValidatingAdmissionPolicy":
		return schema.GroupResource{Group: admissionregistrationv1.GroupName, Resource: "validatingadmissionpolicies"}
	case "ValidatingAdmissionPolicyBinding":
		return schema.GroupResource{Group: admissionregistrationv1.GroupName, Resource: "validatingadmissionpolicybindings"}
	default:
		return schema.GroupResource{Resource: "configmaps"}
	}
}
