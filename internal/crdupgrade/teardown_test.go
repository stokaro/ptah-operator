package crdupgrade

// These tests intentionally use the package internals: safe deletion depends
// on proving the same immutable contracts used by each private guard builder.

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

func TestReleaseTeardownDeletesExactInventoryInSafeOrder(t *testing.T) {
	t.Parallel()

	fixture := newReleaseTeardownFixture(t)
	wantOrder := expectedReleaseTeardownOrder(fixture.guard)
	if len(wantOrder) != 40 {
		t.Fatalf("known teardown inventory has %d objects, want 40", len(wantOrder))
	}
	if err := fixture.teardown.Preflight(context.Background()); err != nil {
		t.Fatalf("read-only preflight: %v", err)
	}
	if len(fixture.recorder.deletes) != 0 {
		t.Fatalf("read-only preflight issued deletes: %v", fixture.recorder.deletes)
	}

	if err := fixture.teardown.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fixture.recorder.deletes, wantOrder) {
		t.Fatalf("delete order:\n got: %v\nwant: %v", fixture.recorder.deletes, wantOrder)
	}
	for _, key := range wantOrder {
		options, found := fixture.recorder.options[key]
		if !found {
			t.Fatalf("%s delete options were not recorded", key)
		}
		if options.Preconditions == nil || options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil {
			t.Fatalf("%s delete lacks UID/resourceVersion preconditions: %#v", key, options)
		}
		wantIdentity := fixture.identities[key]
		if *options.Preconditions.UID != wantIdentity.uid || *options.Preconditions.ResourceVersion != wantIdentity.resourceVersion {
			t.Fatalf("%s delete preconditions = %s/%s, want %s/%s", key,
				*options.Preconditions.UID, *options.Preconditions.ResourceVersion,
				wantIdentity.uid, wantIdentity.resourceVersion,
			)
		}
	}
	tail := fixture.recorder.deletes[len(fixture.recorder.deletes)-4:]
	wantTail := []string{
		teardownKey("ConfigMap", HookIdentityProbeObjectName(fixture.guard.ReleaseNamespace, fixture.guard.ReleaseName, fixture.guard.ReleaseSequence, fixture.guard.ManagerImage)),
		teardownKey("ConfigMap", ReleaseActivationName),
		teardownKey("ValidatingAdmissionPolicyBinding", NamespaceDeletionGuardPolicyName(fixture.guard.ReleaseNamespace, fixture.guard.ReleaseName)),
		teardownKey("ValidatingAdmissionPolicy", NamespaceDeletionGuardPolicyName(fixture.guard.ReleaseNamespace, fixture.guard.ReleaseName)),
	}
	if !reflect.DeepEqual(tail, wantTail) {
		t.Fatalf("teardown tail = %v, want %v", tail, wantTail)
	}
}

func TestReleaseTeardownRejectsSequenceWithoutPredecessorIdentityInventory(t *testing.T) {
	t.Parallel()

	fixture := newReleaseTeardownFixture(t)
	fixture.guard.ReleaseSequence = 2
	base := strings.Split(fixture.guard.HookServiceAccountName, "-crd-v")[0]
	fixture.guard.HookServiceAccountName = fmt.Sprintf("%s-crd-v2-%s", base, hookIdentityDigest(fixture.guard.ReleaseNamespace, fixture.guard.ReleaseName, 2, fixture.guard.ManagerImage)[:12])
	err := fixture.teardown.Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no explicit predecessor identity inventory") {
		t.Fatalf("Preflight error = %v, want missing predecessor inventory refusal", err)
	}
	if len(fixture.recorder.deletes) != 0 {
		t.Fatalf("predecessor inventory refusal mutated resources: %v", fixture.recorder.deletes)
	}
}

func TestReleaseTeardownPreflightsCompleteInventoryBeforeMutation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*releaseTeardownFixture)
		want   string
	}{
		{
			name: "foreign mutating webhook owner",
			mutate: func(f *releaseTeardownFixture) {
				f.mutating.object.Annotations[helmReleaseNameAnnotation] = "foreign"
			},
			want: "is not owned by Helm release",
		},
		{
			name: "parameterized binding drift",
			mutate: func(f *releaseTeardownFixture) {
				name := RuntimeGuardPolicyName(f.guard.ReleaseSequence)
				f.bindings.objects[name].Spec.ParamRef = nil
			},
			want: "binding spec differs",
		},
		{
			name: "remaining binding foreign owner",
			mutate: func(f *releaseTeardownFixture) {
				name := HookIdentityGuardPolicyName(f.guard.ReleaseNamespace, f.guard.ReleaseName, f.guard.ReleaseSequence, f.guard.ManagerImage)
				f.bindings.objects[name].Labels[instanceLabel] = "foreign"
			},
			want: "foreign or incomplete ownership",
		},
		{
			name: "policy contract drift",
			mutate: func(f *releaseTeardownFixture) {
				name := ParentReplicaSetGuardPolicyName(f.guard.ReleaseNamespace, f.guard.ReleaseName)
				f.policies.objects[name].Spec.Validations[0].Expression = "true"
			},
			want: "differs from the immutable contract",
		},
		{
			name: "namespace boundary drift",
			mutate: func(f *releaseTeardownFixture) {
				name := NamespaceDeletionGuardPolicyName(f.guard.ReleaseNamespace, f.guard.ReleaseName)
				f.bindings.objects[name].Spec.ValidationActions = nil
			},
			want: "namespace deletion guard binding spec differs",
		},
		{
			name: "activation owner drift",
			mutate: func(f *releaseTeardownFixture) {
				f.configMaps.objects[ReleaseActivationName].Labels[instanceLabel] = "foreign"
			},
			want: "foreign or incomplete ownership",
		},
		{
			name: "probe marker shape drift",
			mutate: func(f *releaseTeardownFixture) {
				name := HookIdentityProbeObjectName(f.guard.ReleaseNamespace, f.guard.ReleaseName, f.guard.ReleaseSequence, f.guard.ManagerImage)
				f.configMaps.objects[name].Data["probe"] = "changed"
			},
			want: "shape is not exact",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newReleaseTeardownFixture(t)
			test.mutate(fixture)
			err := fixture.teardown.Teardown(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Teardown error = %v, want %q", err, test.want)
			}
			if len(fixture.recorder.deletes) != 0 {
				t.Fatalf("preflight failure mutated resources: %v", fixture.recorder.deletes)
			}
		})
	}
}

func TestReleaseTeardownRejectsObjectsThatMayRemainAfterDelete(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*releaseTeardownFixture)
		want   string
	}{
		{
			name: "finalizer",
			mutate: func(f *releaseTeardownFixture) {
				f.mutating.object.Finalizers = []string{"operator.example/hold"}
			},
			want: "has finalizers",
		},
		{
			name: "deletion in progress",
			mutate: func(f *releaseTeardownFixture) {
				now := metav1.Now()
				f.policies.objects[RolloutGuardPolicyName(f.guard.ReleaseSequence)].DeletionTimestamp = &now
			},
			want: "deletion is already in progress",
		},
		{
			name: "nonzero deletion grace period",
			mutate: func(f *releaseTeardownFixture) {
				grace := int64(30)
				f.bindings.objects[RuntimeGuardPolicyName(f.guard.ReleaseSequence)].DeletionGracePeriodSeconds = &grace
			},
			want: "nonzero deletion grace period",
		},
		{
			name: "owner reference",
			mutate: func(f *releaseTeardownFixture) {
				f.validating.object.OwnerReferences = []metav1.OwnerReference{{
					APIVersion: "v1", Kind: "Namespace", Name: f.guard.ReleaseNamespace, UID: "owner-uid",
				}}
			},
			want: "unexpected owner references",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newReleaseTeardownFixture(t)
			test.mutate(fixture)

			err := fixture.teardown.Teardown(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Teardown error = %v, want %q", err, test.want)
			}
			if len(fixture.recorder.deletes) != 0 {
				t.Fatalf("unsafe object state caused deletes: %v", fixture.recorder.deletes)
			}
		})
	}
}

func TestReleaseTeardownStopsAtFirstDeleteFailureAndResumes(t *testing.T) {
	t.Parallel()

	fixture := newReleaseTeardownFixture(t)
	wantOrder := expectedReleaseTeardownOrder(fixture.guard)
	failureIndex := 8
	failureKey := wantOrder[failureIndex]
	fixture.recorder.errors[failureKey] = errors.New("injected delete failure")

	err := fixture.teardown.Teardown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "injected delete failure") {
		t.Fatalf("Teardown error = %v, want injected failure", err)
	}
	if want := wantOrder[:failureIndex+1]; !reflect.DeepEqual(fixture.recorder.deletes, want) {
		t.Fatalf("delete calls before failure = %v, want %v", fixture.recorder.deletes, want)
	}

	delete(fixture.recorder.errors, failureKey)
	fixture.recorder.deletes = nil
	if err := fixture.teardown.Teardown(context.Background()); err != nil {
		t.Fatalf("resume teardown: %v", err)
	}
	if want := wantOrder[failureIndex:]; !reflect.DeepEqual(fixture.recorder.deletes, want) {
		t.Fatalf("resumed delete calls = %v, want %v", fixture.recorder.deletes, want)
	}
}

func TestReleaseTeardownAllowsOnlyContiguousDeletedPrefix(t *testing.T) {
	t.Parallel()

	t.Run("contiguous prefix and completed retry", func(t *testing.T) {
		t.Parallel()
		fixture := newReleaseTeardownFixture(t)
		order := expectedReleaseTeardownOrder(fixture.guard)
		const removed = 6
		for _, key := range order[:removed] {
			fixture.remove(key)
		}

		if err := fixture.teardown.Teardown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if want := order[removed:]; !reflect.DeepEqual(fixture.recorder.deletes, want) {
			t.Fatalf("delete calls = %v, want %v", fixture.recorder.deletes, want)
		}

		fixture.recorder.deletes = nil
		if err := fixture.teardown.Teardown(context.Background()); err != nil {
			t.Fatalf("completed teardown retry: %v", err)
		}
		if len(fixture.recorder.deletes) != 0 {
			t.Fatalf("completed retry issued deletes: %v", fixture.recorder.deletes)
		}
	})

	t.Run("noncontiguous hole", func(t *testing.T) {
		t.Parallel()
		fixture := newReleaseTeardownFixture(t)
		order := expectedReleaseTeardownOrder(fixture.guard)
		fixture.remove(order[5])

		err := fixture.teardown.Teardown(context.Background())
		if err == nil || !strings.Contains(err.Error(), "inventory is incomplete") {
			t.Fatalf("Teardown error = %v, want incomplete inventory", err)
		}
		if len(fixture.recorder.deletes) != 0 {
			t.Fatalf("incomplete inventory mutated resources: %v", fixture.recorder.deletes)
		}
	})

	t.Run("missing activation anchor", func(t *testing.T) {
		t.Parallel()
		fixture := newReleaseTeardownFixture(t)
		fixture.remove(teardownKey("ConfigMap", ReleaseActivationName))

		err := fixture.teardown.Teardown(context.Background())
		if err == nil || !strings.Contains(err.Error(), "inventory is incomplete") {
			t.Fatalf("Teardown error = %v, want missing activation anchor", err)
		}
		if len(fixture.recorder.deletes) != 0 {
			t.Fatalf("missing anchor mutated resources: %v", fixture.recorder.deletes)
		}
	})
}

func TestReleaseTeardownAcceptsDeleteNotFoundOnlyAfterVerification(t *testing.T) {
	t.Parallel()

	fixture := newReleaseTeardownFixture(t)
	order := expectedReleaseTeardownOrder(fixture.guard)
	notFoundKey := order[4]
	fixture.recorder.notFound[notFoundKey] = true

	if err := fixture.teardown.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fixture.recorder.deletes, order) {
		t.Fatalf("delete calls = %v, want %v", fixture.recorder.deletes, order)
	}
}

func TestReleaseTeardownDeletionPreconditionsRejectConcurrentReplacement(t *testing.T) {
	t.Parallel()

	fixture := newReleaseTeardownFixture(t)
	order := expectedReleaseTeardownOrder(fixture.guard)
	target := teardownKey("ValidatingAdmissionPolicyBinding", RuntimeGuardPolicyName(fixture.guard.ReleaseSequence))
	targetIndex := slicesIndex(order, target)
	fixture.recorder.beforeDelete[target] = func() {
		name := RuntimeGuardPolicyName(fixture.guard.ReleaseSequence)
		fixture.bindings.objects[name].UID = "replacement-uid"
		fixture.bindings.objects[name].ResourceVersion = "replacement-version"
	}

	err := fixture.teardown.Teardown(context.Background())
	if err == nil || !apierrors.IsConflict(err) {
		t.Fatalf("Teardown error = %v, want precondition conflict", err)
	}
	if want := order[:targetIndex+1]; !reflect.DeepEqual(fixture.recorder.deletes, want) {
		t.Fatalf("delete calls = %v, want %v", fixture.recorder.deletes, want)
	}
	if fixture.bindings.objects[RuntimeGuardPolicyName(fixture.guard.ReleaseSequence)] == nil {
		t.Fatal("concurrently replaced object was deleted")
	}
}

func TestReleaseTeardownValidatesDependencies(t *testing.T) {
	t.Parallel()

	fixture := newReleaseTeardownFixture(t)
	tests := []struct {
		name     string
		teardown *ReleaseTeardown
	}{
		{name: "nil receiver"},
		{name: "nil rollout", teardown: NewReleaseTeardown(nil, fixture.mutating, fixture.validating, fixture.policies, fixture.bindings, fixture.configMaps)},
		{name: "nil mutating", teardown: NewReleaseTeardown(fixture.guard, nil, fixture.validating, fixture.policies, fixture.bindings, fixture.configMaps)},
		{name: "nil validating", teardown: NewReleaseTeardown(fixture.guard, fixture.mutating, nil, fixture.policies, fixture.bindings, fixture.configMaps)},
		{name: "nil policies", teardown: NewReleaseTeardown(fixture.guard, fixture.mutating, fixture.validating, nil, fixture.bindings, fixture.configMaps)},
		{name: "nil bindings", teardown: NewReleaseTeardown(fixture.guard, fixture.mutating, fixture.validating, fixture.policies, nil, fixture.configMaps)},
		{name: "nil ConfigMaps", teardown: NewReleaseTeardown(fixture.guard, fixture.mutating, fixture.validating, fixture.policies, fixture.bindings, nil)},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.teardown.Teardown(context.Background())
			if err == nil || !strings.Contains(err.Error(), "clients and rollout identity are required") {
				t.Fatalf("Teardown error = %v, want dependency validation", err)
			}
		})
	}
}

type releaseTeardownFixture struct {
	guard      *RolloutGuard
	teardown   *ReleaseTeardown
	recorder   *teardownRecorder
	mutating   *teardownMutatingClient
	validating *teardownValidatingClient
	policies   *teardownPolicyClient
	bindings   *teardownBindingClient
	configMaps *teardownConfigMapClient
	identities map[string]teardownIdentity
}

func newReleaseTeardownFixture(t *testing.T) *releaseTeardownFixture {
	t.Helper()
	guard, policySource, bindingSource, _ := readyRolloutGuard()
	rolloutName := RolloutGuardPolicyName(guard.ReleaseSequence)
	runtimeName := RuntimeGuardPolicyName(guard.ReleaseSequence)
	policySource.objects[rolloutName] = readyPolicy(guard.policy(guard.ControllerStateVersion, guard.AdmissionContractVersion))
	policySource.objects[runtimeName] = readyPolicy(guard.runtimePolicy(guard.ControllerStateVersion, guard.ReleaseSequence, guard.ManagerImage))
	bindingSource.objects[rolloutName] = guard.binding(rolloutName)
	bindingSource.objects[runtimeName] = guard.binding(runtimeName)
	namespace := NewNamespaceDeletionGuard(guard)
	namespaceName := NamespaceDeletionGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	policySource.objects[namespaceName] = readyPolicy(namespace.policy())
	bindingSource.objects[namespaceName] = namespace.binding()

	recorder := &teardownRecorder{
		options:      map[string]metav1.DeleteOptions{},
		errors:       map[string]error{},
		notFound:     map[string]bool{},
		beforeDelete: map[string]func(){},
	}
	policies := &teardownPolicyClient{objects: policySource.objects, recorder: recorder}
	bindings := &teardownBindingClient{objects: bindingSource.objects, recorder: recorder}
	for name, object := range policies.objects {
		setTeardownObjectIdentity(object, "policy-"+name)
	}
	for name, object := range bindings.objects {
		setTeardownObjectIdentity(object, "binding-"+name)
	}

	expected := teardownRuntimeInvariants(guard)
	webhookAnnotations := copyStrings(expected.annotations())
	webhookAnnotations[helmReleaseNameAnnotation] = guard.ReleaseName
	webhookAnnotations[helmReleaseNamespaceAnnotation] = guard.ReleaseNamespace
	webhookLabels := map[string]string{
		managedByLabel: "Helm",
		instanceLabel:  guard.ReleaseName,
	}
	mutatingObject := &admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: AdmissionConfigurationName, Annotations: copyStrings(webhookAnnotations),
			Labels: copyStrings(webhookLabels),
		},
		Webhooks: []admissionregistrationv1.MutatingWebhook{readyMutatingApprovalWebhook(expected)},
	}
	setTeardownObjectIdentity(mutatingObject, "mutating-webhook")
	validatingObject := &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: AdmissionConfigurationName, Annotations: copyStrings(webhookAnnotations),
			Labels: copyStrings(webhookLabels),
		},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{
			readyValidatingApprovalWebhook(expected),
			readyPodIntentWebhook(expected),
			readyControllerWriteWebhook(expected),
		},
	}
	setTeardownObjectIdentity(validatingObject, "validating-webhook")
	mutating := &teardownMutatingClient{object: mutatingObject, recorder: recorder}
	validating := &teardownValidatingClient{object: validatingObject, recorder: recorder}

	probeName := HookIdentityProbeObjectName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	probePolicyName := HookIdentityProbeGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	probe := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: probeName, Namespace: guard.ReleaseNamespace,
			Annotations: map[string]string{
				"helm.sh/hook":                           "pre-install,pre-upgrade",
				"helm.sh/hook-weight":                    hookIdentityProbeMarkerWeight,
				"helm.sh/resource-policy":                "keep",
				"operator.ptah.dev/hook-identity-policy": probePolicyName,
			},
			Labels: map[string]string{
				managedByLabel:                rolloutGuardManagedBy,
				instanceLabel:                 guard.ReleaseName,
				"app.kubernetes.io/component": "hook-identity-probe",
			},
		},
		Data: map[string]string{"probe": "ready-for-denial-proof"},
	}
	setTeardownObjectIdentity(probe, "probe-marker")
	activation := guard.ConfigMaps.(*rolloutConfigMapClient).objects[ReleaseActivationName].DeepCopy()
	setTeardownObjectIdentity(activation, "activation")
	configMaps := &teardownConfigMapClient{objects: map[string]*corev1.ConfigMap{
		probeName:             probe,
		ReleaseActivationName: activation,
	}, recorder: recorder}
	guard.Policies = policies
	guard.Bindings = bindings

	fixture := &releaseTeardownFixture{
		guard: guard, recorder: recorder,
		mutating: mutating, validating: validating,
		policies: policies, bindings: bindings, configMaps: configMaps,
		identities: map[string]teardownIdentity{},
	}
	fixture.teardown = NewReleaseTeardown(guard, mutating, validating, policies, bindings, configMaps)
	for _, key := range expectedReleaseTeardownOrder(guard) {
		fixture.identities[key] = fixture.identity(key)
	}
	return fixture
}

func expectedReleaseTeardownOrder(guard *RolloutGuard) []string {
	activationName := ReleaseActivationGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	rolloutName := RolloutGuardPolicyName(guard.ReleaseSequence)
	runtimeName := RuntimeGuardPolicyName(guard.ReleaseSequence)
	runtimePodName := RuntimePodGuardPolicyName(guard.ReleaseSequence)
	hookName := HookIdentityGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	hookProbeName := HookIdentityProbeGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	parentReplicaSetName := ParentReplicaSetGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	parentHookOriginName := ParentHookJobOriginGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	parentHookPodOriginName := ParentHookPodOriginGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	parentHookContractName := ParentHookJobContractPolicyName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	serviceAccountName := ServiceAccountOriginGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	controllerWriteName := ControllerWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	controllerJobWriteName := ControllerJobWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	controllerChunkWriteName := ControllerChunkWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	controllerPlanWriteName := ControllerPlanWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	certificateMutatingWriteName := CertificateMutatingWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	certificateValidatingWriteName := CertificateValidatingWriteGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	namespaceName := NamespaceDeletionGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)

	parameterized := []string{activationName, rolloutName, runtimeName, runtimePodName}
	remaining := []string{
		hookName, hookProbeName,
		parentReplicaSetName, parentHookOriginName, parentHookPodOriginName, parentHookContractName,
		serviceAccountName, controllerWriteName,
		controllerJobWriteName, controllerChunkWriteName, controllerPlanWriteName,
		certificateMutatingWriteName, certificateValidatingWriteName,
	}
	policies := append(append([]string(nil), parameterized...), remaining...)

	order := []string{
		teardownKey("MutatingWebhookConfiguration", AdmissionConfigurationName),
		teardownKey("ValidatingWebhookConfiguration", AdmissionConfigurationName),
	}
	for _, name := range parameterized {
		order = append(order, teardownKey("ValidatingAdmissionPolicyBinding", name))
	}
	for _, name := range remaining {
		order = append(order, teardownKey("ValidatingAdmissionPolicyBinding", name))
	}
	for _, name := range policies {
		order = append(order, teardownKey("ValidatingAdmissionPolicy", name))
	}
	order = append(order,
		teardownKey("ConfigMap", HookIdentityProbeObjectName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)),
		teardownKey("ConfigMap", ReleaseActivationName),
		teardownKey("ValidatingAdmissionPolicyBinding", namespaceName),
		teardownKey("ValidatingAdmissionPolicy", namespaceName),
	)
	return order
}

func (f *releaseTeardownFixture) identity(key string) teardownIdentity {
	kind, name := splitTeardownKey(key)
	switch kind {
	case "MutatingWebhookConfiguration":
		return objectTeardownIdentity(f.mutating.object)
	case "ValidatingWebhookConfiguration":
		return objectTeardownIdentity(f.validating.object)
	case "ValidatingAdmissionPolicyBinding":
		return objectTeardownIdentity(f.bindings.objects[name])
	case "ValidatingAdmissionPolicy":
		return objectTeardownIdentity(f.policies.objects[name])
	case "ConfigMap":
		return objectTeardownIdentity(f.configMaps.objects[name])
	default:
		panic("unknown teardown kind " + kind)
	}
}

func (f *releaseTeardownFixture) remove(key string) {
	kind, name := splitTeardownKey(key)
	switch kind {
	case "MutatingWebhookConfiguration":
		f.mutating.object = nil
	case "ValidatingWebhookConfiguration":
		f.validating.object = nil
	case "ValidatingAdmissionPolicyBinding":
		delete(f.bindings.objects, name)
	case "ValidatingAdmissionPolicy":
		delete(f.policies.objects, name)
	case "ConfigMap":
		delete(f.configMaps.objects, name)
	default:
		panic("unknown teardown kind " + kind)
	}
}

func setTeardownObjectIdentity(object metav1.Object, value string) {
	object.SetUID(types.UID("uid-" + value))
	object.SetResourceVersion("rv-" + value)
}

func objectTeardownIdentity(object metav1.Object) teardownIdentity {
	return teardownIdentity{uid: object.GetUID(), resourceVersion: object.GetResourceVersion()}
}

func teardownKey(kind, name string) string {
	return kind + "/" + name
}

func splitTeardownKey(key string) (string, string) {
	kind, name, found := strings.Cut(key, "/")
	if !found {
		panic("invalid teardown key " + key)
	}
	return kind, name
}

func slicesIndex(values []string, target string) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	panic("missing teardown key " + target)
}

type teardownRecorder struct {
	deletes      []string
	options      map[string]metav1.DeleteOptions
	errors       map[string]error
	notFound     map[string]bool
	beforeDelete map[string]func()
}

func (r *teardownRecorder) record(kind, name string, options metav1.DeleteOptions, object metav1.Object) error {
	key := teardownKey(kind, name)
	r.deletes = append(r.deletes, key)
	r.options[key] = options
	if before := r.beforeDelete[key]; before != nil {
		before()
	}
	if err := r.errors[key]; err != nil {
		return err
	}
	if r.notFound[key] {
		return apierrors.NewNotFound(teardownGroupResource(kind), name)
	}
	if options.Preconditions == nil || options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil ||
		object == nil || *options.Preconditions.UID != object.GetUID() || *options.Preconditions.ResourceVersion != object.GetResourceVersion() {
		return apierrors.NewConflict(teardownGroupResource(kind), name, errors.New("deletion precondition failed"))
	}
	return nil
}

func teardownGroupResource(kind string) schema.GroupResource {
	switch kind {
	case "MutatingWebhookConfiguration":
		return schema.GroupResource{Group: admissionregistrationv1.GroupName, Resource: "mutatingwebhookconfigurations"}
	case "ValidatingWebhookConfiguration":
		return schema.GroupResource{Group: admissionregistrationv1.GroupName, Resource: "validatingwebhookconfigurations"}
	case "ValidatingAdmissionPolicyBinding":
		return schema.GroupResource{Group: admissionregistrationv1.GroupName, Resource: "validatingadmissionpolicybindings"}
	case "ValidatingAdmissionPolicy":
		return schema.GroupResource{Group: admissionregistrationv1.GroupName, Resource: "validatingadmissionpolicies"}
	case "ConfigMap":
		return schema.GroupResource{Resource: "configmaps"}
	default:
		panic(fmt.Sprintf("unknown teardown kind %q", kind))
	}
}

type teardownMutatingClient struct {
	object   *admissionregistrationv1.MutatingWebhookConfiguration
	recorder *teardownRecorder
}

func (c *teardownMutatingClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*admissionregistrationv1.MutatingWebhookConfiguration, error) {
	if c.object == nil {
		return nil, apierrors.NewNotFound(teardownGroupResource("MutatingWebhookConfiguration"), name)
	}
	return c.object.DeepCopy(), nil
}

func (c *teardownMutatingClient) Delete(_ context.Context, name string, options metav1.DeleteOptions) error {
	if err := c.recorder.record("MutatingWebhookConfiguration", name, options, c.object); err != nil {
		if apierrors.IsNotFound(err) {
			c.object = nil
		}
		return err
	}
	c.object = nil
	return nil
}

type teardownValidatingClient struct {
	object   *admissionregistrationv1.ValidatingWebhookConfiguration
	recorder *teardownRecorder
}

func (c *teardownValidatingClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*admissionregistrationv1.ValidatingWebhookConfiguration, error) {
	if c.object == nil {
		return nil, apierrors.NewNotFound(teardownGroupResource("ValidatingWebhookConfiguration"), name)
	}
	return c.object.DeepCopy(), nil
}

func (c *teardownValidatingClient) Delete(_ context.Context, name string, options metav1.DeleteOptions) error {
	if err := c.recorder.record("ValidatingWebhookConfiguration", name, options, c.object); err != nil {
		if apierrors.IsNotFound(err) {
			c.object = nil
		}
		return err
	}
	c.object = nil
	return nil
}

type teardownPolicyClient struct {
	objects  map[string]*admissionregistrationv1.ValidatingAdmissionPolicy
	recorder *teardownRecorder
}

func (c *teardownPolicyClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*admissionregistrationv1.ValidatingAdmissionPolicy, error) {
	object := c.objects[name]
	if object == nil {
		return nil, apierrors.NewNotFound(teardownGroupResource("ValidatingAdmissionPolicy"), name)
	}
	return object.DeepCopy(), nil
}

func (c *teardownPolicyClient) Delete(_ context.Context, name string, options metav1.DeleteOptions) error {
	object := c.objects[name]
	if err := c.recorder.record("ValidatingAdmissionPolicy", name, options, object); err != nil {
		if apierrors.IsNotFound(err) {
			delete(c.objects, name)
		}
		return err
	}
	delete(c.objects, name)
	return nil
}

type teardownBindingClient struct {
	objects  map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding
	recorder *teardownRecorder
}

func (c *teardownBindingClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*admissionregistrationv1.ValidatingAdmissionPolicyBinding, error) {
	object := c.objects[name]
	if object == nil {
		return nil, apierrors.NewNotFound(teardownGroupResource("ValidatingAdmissionPolicyBinding"), name)
	}
	return object.DeepCopy(), nil
}

func (c *teardownBindingClient) Delete(_ context.Context, name string, options metav1.DeleteOptions) error {
	object := c.objects[name]
	if err := c.recorder.record("ValidatingAdmissionPolicyBinding", name, options, object); err != nil {
		if apierrors.IsNotFound(err) {
			delete(c.objects, name)
		}
		return err
	}
	delete(c.objects, name)
	return nil
}

type teardownConfigMapClient struct {
	objects  map[string]*corev1.ConfigMap
	recorder *teardownRecorder
}

func (c *teardownConfigMapClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*corev1.ConfigMap, error) {
	object := c.objects[name]
	if object == nil {
		return nil, apierrors.NewNotFound(teardownGroupResource("ConfigMap"), name)
	}
	return object.DeepCopy(), nil
}

func (c *teardownConfigMapClient) Delete(_ context.Context, name string, options metav1.DeleteOptions) error {
	object := c.objects[name]
	if err := c.recorder.record("ConfigMap", name, options, object); err != nil {
		if apierrors.IsNotFound(err) {
			delete(c.objects, name)
		}
		return err
	}
	delete(c.objects, name)
	return nil
}

var (
	_ MutatingWebhookTeardownClient                  = (*teardownMutatingClient)(nil)
	_ ValidatingWebhookTeardownClient                = (*teardownValidatingClient)(nil)
	_ ValidatingAdmissionPolicyTeardownClient        = (*teardownPolicyClient)(nil)
	_ ValidatingAdmissionPolicyBindingTeardownClient = (*teardownBindingClient)(nil)
	_ ConfigMapTeardownClient                        = (*teardownConfigMapClient)(nil)
)
