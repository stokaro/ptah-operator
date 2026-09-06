package crdupgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestRenderedReleaseActivationGuardMatchesCompiledContract(t *testing.T) {
	path := os.Getenv("PTAH_ROLLOUT_GUARD_RENDER")
	if path == "" {
		t.Skip("PTAH_ROLLOUT_GUARD_RENDER is set by the chart contract gate")
	}
	rendered, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var policy *admissionregistrationv1.ValidatingAdmissionPolicy
	var binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding
	var activation *corev1.ConfigMap
	guard := testReleaseActivationGuard()
	guard.ReleaseName = "ptah-e2e"
	guard.ReleaseNamespace = "ptah-e2e"
	guard.ManagerImage = "ghcr.io/stokaro/ptah-operator@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	guard.HookServiceAccountName = "ptah-e2e-ptah-operator-crd-v1-" + hookIdentityDigest(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)[:12]
	name := ReleaseActivationGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	decoder := utilyaml.NewYAMLToJSONDecoder(bytes.NewReader(rendered))
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var typeMeta metav1.TypeMeta
		if err := json.Unmarshal(raw, &typeMeta); err != nil {
			t.Fatal(err)
		}
		switch typeMeta.Kind {
		case "ValidatingAdmissionPolicy":
			var object admissionregistrationv1.ValidatingAdmissionPolicy
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			if object.Name == name {
				policy = &object
			}
		case "ValidatingAdmissionPolicyBinding":
			var object admissionregistrationv1.ValidatingAdmissionPolicyBinding
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			if object.Name == name {
				binding = &object
			}
		case "ConfigMap":
			var object corev1.ConfigMap
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			if object.Name == ReleaseActivationName {
				activation = &object
			}
		}
	}
	if err := guard.verifyPolicy(policy); err != nil {
		t.Fatalf("rendered release activation policy: %v", err)
	}
	if err := guard.verifyBinding(binding); err != nil {
		t.Fatalf("rendered release activation binding: %v", err)
	}
	activation.UID = "rendered-activation"
	activation.ResourceVersion = "1"
	if _, err := guard.verifyActivationObject(activation); err != nil {
		t.Fatalf("rendered release activation parameter: %v", err)
	}
	if activation.Annotations["helm.sh/hook-weight"] != releaseActivationHookWeight || policy.Annotations["helm.sh/hook-weight"] != releaseActivationPolicyWeight || binding.Annotations["helm.sh/hook-weight"] != releaseActivationBindingWeight {
		t.Fatal("release activation parameter must exist before any fail-closed parameter binding")
	}
}

func TestReleaseActivationGuardPolicyIsStableAndExact(t *testing.T) {
	guard := testReleaseActivationGuard()
	policy := guard.policy()
	binding := guard.binding()
	if policy.Spec.ParamKind == nil || policy.Spec.ParamKind.APIVersion != "v1" || policy.Spec.ParamKind.Kind != "ConfigMap" {
		t.Fatal("release activation policy must use the retained ConfigMap as its parameter")
	}
	operations := policy.Spec.MatchConstraints.ResourceRules[0].Operations
	if !reflect.DeepEqual(operations, []admissionregistrationv1.OperationType{admissionregistrationv1.Update, admissionregistrationv1.Delete}) {
		t.Fatalf("operations = %v, want UPDATE and DELETE", operations)
	}
	if binding.Spec.MatchResources == nil || binding.Spec.MatchResources.NamespaceSelector == nil ||
		binding.Spec.MatchResources.NamespaceSelector.MatchLabels[corev1.LabelMetadataName] != guard.ReleaseNamespace {
		t.Fatalf("release activation binding is not isolated to namespace %q: %#v", guard.ReleaseNamespace, binding.Spec.MatchResources)
	}
	if len(binding.Spec.MatchResources.ResourceRules) != 1 ||
		len(binding.Spec.MatchResources.ResourceRules[0].ResourceNames) != 0 {
		t.Fatalf("release activation binding must leave name filtering to the DELETE-safe policy condition: %#v", binding.Spec.MatchResources.ResourceRules)
	}
	if len(policy.Spec.MatchConditions) != 1 || !strings.Contains(policy.Spec.MatchConditions[0].Expression, "oldObject.metadata.name") {
		t.Fatalf("release activation policy does not derive the DELETE target from oldObject: %#v", policy.Spec.MatchConditions)
	}
	serialized, err := json.Marshal(policy.Spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`[0-9a-f]{12}$`,
		`[0-9a-f]{64}$`,
		`substring(0, 12)`,
		`system:kube-controller-manager`,
		`request.userInfo.groups.size() == 3`,
		`variables.paramsActive == variables.oldActive`,
		`object.metadata.annotations == oldObject.metadata.annotations`,
	} {
		if !strings.Contains(string(serialized), required) {
			t.Fatalf("release activation policy does not contain %q", required)
		}
	}

	other := *guard
	other.ReleaseSequence = 2
	other.ManagerImage = "registry.example/ptah@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	other.HookServiceAccountName = "ptah-crd-v2-" + hookIdentityDigest(other.ReleaseNamespace, other.ReleaseName, other.ReleaseSequence, other.ManagerImage)[:12]
	if policy.Name != other.policy().Name || !reflect.DeepEqual(policy.Spec, other.policy().Spec) ||
		binding.Name != other.binding().Name || !reflect.DeepEqual(binding.Spec, other.binding().Spec) {
		t.Fatal("retained release activation policy changed across release sequences")
	}
}

func TestReleaseActivationGuardMatchesCollectionDeleteByOldObjectName(t *testing.T) {
	guard := testReleaseActivationGuard()
	expression := guard.policy().Spec.MatchConditions[0].Expression
	tests := []struct {
		name        string
		operation   string
		requestName string
		oldName     string
		want        bool
	}{
		{name: "named update", operation: "UPDATE", requestName: ReleaseActivationName, want: true},
		{name: "collection delete", operation: "DELETE", oldName: ReleaseActivationName, want: true},
		{name: "foreign collection member", operation: "DELETE", oldName: "foreign"},
		{name: "delete without old object", operation: "DELETE"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var oldObject any
			if test.oldName != "" {
				oldObject = map[string]any{"metadata": map[string]any{"name": test.oldName}}
			}
			got := evaluateRolloutCEL(t, expression, map[string]any{
				"request": map[string]any{
					"operation": test.operation,
					"namespace": guard.ReleaseNamespace,
					"name":      test.requestName,
				},
				"oldObject": oldObject,
			}, nil)
			if got != test.want {
				t.Fatalf("activation match = %v, want %t", got, test.want)
			}
		})
	}
}

func TestReleaseActivationDeleteControllerPrincipalsRequireExactGroups(t *testing.T) {
	tests := []struct {
		name     string
		username string
		groups   []any
		want     bool
	}{
		{name: "legacy controller", username: "system:kube-controller-manager", groups: []any{"system:authenticated"}, want: true},
		{name: "service account controller", username: "system:serviceaccount:kube-system:namespace-controller", groups: []any{"system:serviceaccounts", "system:serviceaccounts:kube-system", "system:authenticated"}, want: true},
		{name: "legacy injected group", username: "system:kube-controller-manager", groups: []any{"system:authenticated", "system:masters"}},
		{name: "service account missing namespace group", username: "system:serviceaccount:kube-system:namespace-controller", groups: []any{"system:serviceaccounts", "system:authenticated"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := evaluateRolloutCEL(t, activationNamespaceControllerExpression(), map[string]any{
				"request": map[string]any{"userInfo": map[string]any{"username": test.username, "groups": test.groups}},
			}, nil)
			if got != test.want {
				t.Fatalf("namespace-controller principal = %v, want %t", got, test.want)
			}
		})
	}
}

func TestReleaseActivationParameterRequiresExactShape(t *testing.T) {
	guard := testReleaseActivationGuard()
	valid := activationObject(guard, 1)
	if _, err := guard.verifyActivationObject(valid); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*corev1.ConfigMap)
	}{
		{name: "extra annotation", mutate: func(object *corev1.ConfigMap) { object.Annotations["foreign"] = "value" }},
		{name: "generate name", mutate: func(object *corev1.ConfigMap) { object.GenerateName = "foreign-" }},
		{name: "extra label", mutate: func(object *corev1.ConfigMap) { object.Labels["foreign"] = "value" }},
		{name: "extra data", mutate: func(object *corev1.ConfigMap) { object.Data["foreign"] = "value" }},
		{name: "binary data", mutate: func(object *corev1.ConfigMap) { object.BinaryData = map[string][]byte{"foreign": []byte("value")} }},
		{name: "immutable", mutate: func(object *corev1.ConfigMap) { object.Immutable = activationBoolPtr(true) }},
		{name: "owner", mutate: func(object *corev1.ConfigMap) { object.OwnerReferences = []metav1.OwnerReference{{Name: "foreign"}} }},
		{name: "mismatched release", mutate: func(object *corev1.ConfigMap) { object.Annotations[ReleaseSequenceAnnotation] = "2" }},
		{name: "whitespace image", mutate: func(object *corev1.ConfigMap) { object.Annotations[ManagerImageAnnotation] = "bad image" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid.DeepCopy()
			test.mutate(candidate)
			if _, err := guard.verifyActivationObject(candidate); err == nil {
				t.Fatal("malformed activation parameter was accepted")
			}
		})
	}
}

func TestReleaseActivationRetriesAdmissionCacheBeforeAndAfterPersistence(t *testing.T) {
	guard := testReleaseActivationGuard()
	client := &activationConfigMapClient{
		object:            activationObject(guard, 0),
		preUpdateDenials:  1,
		postUpdateDenials: 2,
		denialMessage:     releaseActivationGuardDenialMessage(),
	}
	guard.ConfigMaps = client
	if err := guard.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.realUpdates != 1 {
		t.Fatalf("persistent updates = %d, want 1", client.realUpdates)
	}
	if client.dryUpdates != 5 {
		t.Fatalf("dry-run updates = %d, want 5 (two pre-persist and three post-persist)", client.dryUpdates)
	}
	if got := client.object.Data[activeReleaseDataKey]; got != "1" {
		t.Fatalf("active release = %q, want 1", got)
	}
}

func TestReleaseActivationRejectsSameSequenceRuntimeRebind(t *testing.T) {
	guard := testReleaseActivationGuard()
	object := activationObject(guard, 1)
	object.Annotations[ManagerImageAnnotation] = "registry.example/ptah@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	guard.ConfigMaps = &activationConfigMapClient{object: object}
	err := guard.Activate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "different runtime identity") {
		t.Fatalf("Activate error = %v, want same-sequence rebind refusal", err)
	}
}

func TestReleaseActivationPrepareRejectsRollbackAndSequenceGapBeforeMutation(t *testing.T) {
	tests := []struct {
		name      string
		candidate int32
		active    int
		want      string
	}{
		{name: "rollback", candidate: 1, active: 2, want: "rollback refused"},
		{name: "gap", candidate: 3, active: 1, want: "sequence gap refused"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			guard := testReleaseActivationGuard()
			guard.ReleaseSequence = test.candidate
			guard.ManagerImage = "registry.example/ptah@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
			guard.HookServiceAccountName = "ptah-crd-v" + activationDecimal(test.candidate) + "-" + hookIdentityDigest(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)[:12]
			policies := &rolloutPolicyClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{}}
			bindings := &rolloutBindingClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding{}}
			name := ReleaseActivationGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
			policies.objects[name] = readyPolicy(guard.policy())
			bindings.objects[name] = guard.binding()
			object := activationObject(guard, test.active)
			object.Annotations[ReleaseSequenceAnnotation] = activationDecimal(int32(test.active))
			guard.Policies = policies
			guard.Bindings = bindings
			client := &activationConfigMapClient{object: object, denyMalformed: true, denialMessage: releaseActivationGuardDenialMessage()}
			guard.ConfigMaps = client
			err := guard.Prepare(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Prepare error = %v, want %q", err, test.want)
			}
			if client.dryUpdates != 0 || client.realUpdates != 0 {
				t.Fatal("incompatible candidate reached the API mutation boundary")
			}
		})
	}
}

func TestReleaseActivationPrepareProvesLiveDenial(t *testing.T) {
	guard := testReleaseActivationGuard()
	policies := &rolloutPolicyClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{}}
	bindings := &rolloutBindingClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding{}}
	name := ReleaseActivationGuardPolicyName(guard.ReleaseNamespace, guard.ReleaseName)
	policies.objects[name] = readyPolicy(guard.policy())
	bindings.objects[name] = guard.binding()
	client := &activationConfigMapClient{object: activationObject(guard, 0), denyMalformed: true, denialMessage: releaseActivationGuardDenialMessage()}
	guard.Policies = policies
	guard.Bindings = bindings
	guard.ConfigMaps = client
	if err := guard.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.dryUpdates != 1 || client.realUpdates != 0 {
		t.Fatalf("prepare updates dry/real = %d/%d, want 1/0", client.dryUpdates, client.realUpdates)
	}
}

func TestReleaseActivationBeginDrainingPersistsFullAttemptAndResumesLostResponse(t *testing.T) {
	t.Parallel()
	guard := testReleaseActivationGuard()
	client := &activationConfigMapClient{
		object:             activationObject(guard, 0),
		persistError:       errors.New("response stream reset"),
		persistBeforeError: true,
	}
	guard.ConfigMaps = client
	state, err := guard.BeginDraining(context.Background())
	if err != nil {
		t.Fatalf("BeginDraining() after lost response error = %v", err)
	}
	wantAttempt := hookIdentityDigest(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	if state != (ReleaseActivationState{
		ActiveReleaseSequence: 0, ControllerCredentialPhase: ControllerCredentialsDraining,
		DrainTargetReleaseSequence: guard.ReleaseSequence,
		DrainAttempt:               wantAttempt,
	}) {
		t.Fatalf("BeginDraining() state = %#v", state)
	}
	if len(wantAttempt) != 64 || client.object.Data[controllerCredentialsAttemptDataKey] != wantAttempt {
		t.Fatalf("persisted attempt = %q, want full digest %q", client.object.Data[controllerCredentialsAttemptDataKey], wantAttempt)
	}
	client.persistError = nil
	if _, err := guard.BeginDraining(context.Background()); err != nil {
		t.Fatalf("same-tuple BeginDraining() retry error = %v", err)
	}
	if client.realUpdates != 1 {
		t.Fatalf("persistent drain updates = %d, want one", client.realUpdates)
	}
}

func TestReleaseActivationRejectsSamePrefixDifferentFullDrainAttempt(t *testing.T) {
	t.Parallel()
	guard := testReleaseActivationGuard()
	object := activationObject(guard, 0)
	attempt := guard.candidateAttempt()
	object.Data = map[string]string{
		activeReleaseDataKey:                "0",
		controllerCredentialsDataKey:        string(ControllerCredentialsDraining),
		controllerCredentialsTargetDataKey:  "1",
		controllerCredentialsAttemptDataKey: attempt[:len(attempt)-1] + differentHexDigit(attempt[len(attempt)-1]),
	}
	guard.ConfigMaps = &activationConfigMapClient{object: object}
	_, err := guard.CurrentState(context.Background())
	if err == nil || !strings.Contains(err.Error(), "want candidate") {
		t.Fatalf("CurrentState() error = %v, want full-attempt collision refusal", err)
	}
}

func TestReleaseActivationRejectsFailedBootstrapSequenceGap(t *testing.T) {
	t.Parallel()
	guard := testReleaseActivationGuard()
	guard.ReleaseSequence = 2
	guard.ManagerImage = "registry.example/ptah@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	guard.HookServiceAccountName = "ptah-crd-v2-" + hookIdentityDigest(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)[:12]
	residue := activationObject(guard, 0)
	residue.Annotations[ReleaseSequenceAnnotation] = "1"
	residue.Annotations[ManagerImageAnnotation] = "registry.example/ptah@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	guard.ConfigMaps = &activationConfigMapClient{object: residue}
	_, err := guard.CurrentState(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bootstrap attempt 1 cannot be superseded by candidate 2") {
		t.Fatalf("CurrentState() error = %v, want failed-bootstrap gap refusal", err)
	}
}

func differentHexDigit(value byte) string {
	if value == '0' {
		return "1"
	}
	return "0"
}

func testReleaseActivationGuard() *ReleaseActivationGuard {
	managerImage := "registry.example/ptah@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hook := "ptah-crd-v1-" + hookIdentityDigest("ptah-system", "ptah", 1, managerImage)[:12]
	return &ReleaseActivationGuard{
		Policies:                 &rolloutPolicyClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{}},
		Bindings:                 &rolloutBindingClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding{}},
		ConfigMaps:               &activationConfigMapClient{},
		ReleaseName:              "ptah",
		ReleaseNamespace:         "ptah-system",
		HookServiceAccountName:   hook,
		ControllerStateVersion:   1,
		AdmissionContractVersion: 1,
		ReleaseSequence:          1,
		ManagerImage:             managerImage,
		PollEvery:                time.Microsecond,
	}
}

func activationObject(guard *ReleaseActivationGuard, active int) *corev1.ConfigMap {
	release := guard.ReleaseSequence
	if active > 0 {
		release = int32(active)
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            ReleaseActivationName,
			Namespace:       guard.ReleaseNamespace,
			UID:             "activation-uid",
			ResourceVersion: "1",
			Annotations: map[string]string{
				"helm.sh/hook":                     "pre-install,pre-upgrade",
				"helm.sh/hook-weight":              releaseActivationHookWeight,
				"helm.sh/resource-policy":          "keep",
				rolloutGuardVersionAnnotation:      rolloutGuardVersion,
				ReleaseNameAnnotation:              guard.ReleaseName,
				ReleaseNamespaceAnnotation:         guard.ReleaseNamespace,
				ControllerStateVersionAnnotation:   "1",
				AdmissionContractVersionAnnotation: "1",
				ReleaseSequenceAnnotation:          activationDecimal(release),
				ManagerImageAnnotation:             guard.ManagerImage,
			},
			Labels: map[string]string{
				managedByLabel:                rolloutGuardManagedBy,
				instanceLabel:                 guard.ReleaseName,
				"app.kubernetes.io/component": rolloutGuardComponent,
			},
		},
		Data: map[string]string{
			activeReleaseDataKey:         activationDecimal(int32(active)),
			controllerCredentialsDataKey: string(ControllerCredentialsActive),
		},
	}
}

func activationDecimal(value int32) string {
	return strconv.FormatInt(int64(value), 10)
}

func activationBoolPtr(value bool) *bool {
	return &value
}

type activationConfigMapClient struct {
	object             *corev1.ConfigMap
	preUpdateDenials   int
	postUpdateDenials  int
	denyMalformed      bool
	denialMessage      string
	dryUpdates         int
	realUpdates        int
	persistError       error
	persistBeforeError bool
}

func (c *activationConfigMapClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*corev1.ConfigMap, error) {
	if c.object == nil || c.object.Name != name {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
	}
	return c.object.DeepCopy(), nil
}

func (c *activationConfigMapClient) Create(_ context.Context, _ *corev1.ConfigMap, _ metav1.CreateOptions) (*corev1.ConfigMap, error) {
	return nil, errors.New("unexpected activation ConfigMap create")
}

func (c *activationConfigMapClient) Update(_ context.Context, object *corev1.ConfigMap, options metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	if len(options.DryRun) != 0 {
		c.dryUpdates++
		if _, malformed := object.Data["unexpected"]; c.denyMalformed && malformed {
			return nil, errors.New(c.denialMessage)
		}
		candidate := object.Data[activeReleaseDataKey]
		stored := ""
		if c.object != nil {
			stored = c.object.Data[activeReleaseDataKey]
		}
		if candidate != stored && c.preUpdateDenials > 0 {
			c.preUpdateDenials--
			return nil, errors.New(c.denialMessage)
		}
		if candidate == stored && c.postUpdateDenials > 0 {
			c.postUpdateDenials--
			return nil, errors.New(c.denialMessage)
		}
		return object.DeepCopy(), nil
	}
	c.realUpdates++
	if c.persistError != nil && !c.persistBeforeError {
		return nil, c.persistError
	}
	c.object = object.DeepCopy()
	if c.persistError != nil {
		return nil, c.persistError
	}
	return c.object.DeepCopy(), nil
}
