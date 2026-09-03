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

	celgo "github.com/google/cel-go/cel"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestControllerObjectGuardNamesAreStableDistinctAndVersioned(t *testing.T) {
	t.Parallel()

	names := map[string]string{
		ControllerJobWriteGuardPolicyName("ptah-system", "ptah"):   controllerJobWriteGuardNamePrefix,
		ControllerChunkWriteGuardPolicyName("ptah-system", "ptah"): controllerChunkWriteGuardPrefix,
		ControllerPlanWriteGuardPolicyName("ptah-system", "ptah"):  controllerPlanWriteGuardNamePrefix,
	}
	if len(names) != 3 {
		t.Fatal("typed controller object guards do not have distinct names")
	}
	for name, prefix := range names {
		if !strings.HasPrefix(name, prefix) || len(name) > 63 {
			t.Fatalf("controller object guard name %q is not bounded and versioned", name)
		}
	}
	if ControllerJobWriteGuardPolicyName("ptah-system", "ptah") !=
		ControllerJobWriteGuardPolicyName("ptah-system", "ptah") {
		t.Fatal("controller object guard name is not deterministic")
	}
	if ControllerJobWriteGuardPolicyName("ptah-system", "ptah") ==
		ControllerJobWriteGuardPolicyName("other", "ptah") ||
		ControllerJobWriteGuardPolicyName("ptah-system", "ptah") ==
			ControllerJobWriteGuardPolicyName("ptah-system", "other") {
		t.Fatal("controller object guard name does not bind both release identity fields")
	}

	rollout := runtimePodGuardFixture()
	other := *rollout
	other.ReleaseSequence++
	other.ManagerImage = "registry.example/ptah@sha256:" + strings.Repeat("b", 64)
	if ControllerJobWriteGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName) !=
		ControllerJobWriteGuardPolicyName(other.ReleaseNamespace, other.ReleaseName) {
		t.Fatal("stable controller object guard name changed with candidate release identity")
	}
}

func TestControllerObjectGuardsAreTypedExactAndFailClosed(t *testing.T) {
	t.Parallel()

	guard := testControllerObjectGuard()
	entries := guard.entries()
	if len(entries) != 3 {
		t.Fatalf("controller object guard entries = %d, want three typed policies", len(entries))
	}
	wantGVK := map[string]struct {
		apiGroup   string
		apiVersion string
	}{
		"jobs":            {apiGroup: "batch", apiVersion: "v1"},
		"configmaps":      {apiGroup: "", apiVersion: "v1"},
		"ptahschemaplans": {apiGroup: "operator.ptah.dev", apiVersion: "v1alpha1"},
	}
	seenResources := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		entry := entry
		t.Run(entry.resource, func(t *testing.T) {
			t.Parallel()
			want := wantGVK[entry.resource]
			if !reflect.DeepEqual(entry.apiGroups, []string{want.apiGroup}) ||
				!reflect.DeepEqual(entry.apiVersions, []string{want.apiVersion}) {
				t.Fatalf("%s guard matches %#v/%#v, want exact %q/%q", entry.resource, entry.apiGroups, entry.apiVersions, want.apiGroup, want.apiVersion)
			}
			policy := guard.policy(entry)
			binding := guard.binding(entry)
			if policy.Spec.ParamKind != nil || binding.Spec.ParamRef != nil {
				t.Fatal("controller object guard must not depend on admission parameters")
			}
			if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionregistrationv1.Fail {
				t.Fatal("controller object guard is not fail-closed")
			}
			assertExactControllerObjectMatch(t, policy.Spec.MatchConstraints, entry)
			assertExactControllerObjectMatch(t, binding.Spec.MatchResources, entry)
			wantUsername := `request.userInfo.username == "system:serviceaccount:ptah-system:ptah-controller"`
			if !reflect.DeepEqual(policy.Spec.MatchConditions, []admissionregistrationv1.MatchCondition{{
				Name: "exact-controller-service-account", Expression: wantUsername,
			}}) {
				t.Fatalf("controller caller match is not exact: %#v", policy.Spec.MatchConditions)
			}
			if binding.Spec.PolicyName != policy.Name ||
				!reflect.DeepEqual(binding.Spec.ValidationActions, []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}) {
				t.Fatalf("controller object binding is not exact deny-only enforcement: %#v", binding.Spec)
			}
		})
		if _, exists := seenResources[entry.resource]; exists {
			t.Fatalf("resource %q is mixed into more than one typed policy", entry.resource)
		}
		seenResources[entry.resource] = struct{}{}
	}
}

func TestControllerObjectGuardCELContracts(t *testing.T) {
	t.Parallel()

	entries := testControllerObjectGuard().entries()
	for _, entry := range entries {
		for index, validation := range entry.validations {
			if validation.Message != entry.denialMessage {
				t.Fatalf("%s validation %d lacks its typed denial message", entry.resource, index)
			}
			assertCELExpressionHeadroom(t, entry.resource, validation.Expression)
		}
	}

	job := strings.Join(validationExpressions(entries[0].validations), "\n")
	for _, marker := range []string{
		`object.metadata.labels.size() == 5`,
		`["resolve", "verify", "observe", "plan", "apply"]`,
		`object.metadata.ownerReferences.size() == 1`,
		`object.spec.template.spec.automountServiceAccountToken`,
		`container.securityContext.allowPrivilegeEscalation`,
		`container.image.matches`,
		`object.spec.template.spec.volumes.size() <= 7`,
		`volume.name == "registry-docker-config" && has(volume.secret)`,
		`container.securityContext.capabilities.add.size() == 0`,
		`["install-runner", "validate-source-authority", "fetch-schema"]`,
		`request.operation == "UPDATE"`,
		`object.metadata.annotations.size() == 5`,
		`["resolve", "verify", "observe", "plan"]`,
		`object.metadata.labels["operator.ptah.dev/operation"] == "apply"`,
		`object.metadata.annotations.size() == 7`,
		`"operator.ptah.dev/plan-content-digest"`,
		`oldObject.status.conditions.exists`,
		`object.spec.ttlSecondsAfterFinished == 300`,
		`object.spec.template == oldObject.spec.template`,
		`has(object.spec.managedBy) == has(oldObject.spec.managedBy)`,
	} {
		if !strings.Contains(job, marker) {
			t.Fatalf("Job structural contract lacks %q", marker)
		}
	}
	jobExpressions := make(map[string]struct{}, len(entries[0].validations))
	for _, expression := range validationExpressions(entries[0].validations) {
		jobExpressions[expression] = struct{}{}
	}
	for _, expression := range []string{
		controllerJobSupportedWindowTopLevelExpression("object"),
		controllerJobSupportedWindowVolumeExpression("object"),
		controllerJobSupportedWindowProjectionExpression("object"),
		controllerJobSupportedWindowMountExpression("object"),
		controllerJobSupportedWindowContainerExpression("object"),
		controllerJobPreviousObjectExpression(controllerJobSupportedWindowTopLevelExpression("oldObject")),
		controllerJobPreviousObjectExpression(controllerJobSupportedWindowVolumeExpression("oldObject")),
		controllerJobPreviousObjectExpression(controllerJobSupportedWindowProjectionExpression("oldObject")),
		controllerJobPreviousObjectExpression(controllerJobSupportedWindowMountExpression("oldObject")),
		controllerJobPreviousObjectExpression(controllerJobSupportedWindowContainerExpression("oldObject")),
	} {
		if _, exists := jobExpressions[expression]; !exists {
			t.Fatalf("Job structural contract lacks supported-window expression %q", expression)
		}
	}
	for _, marker := range []string{
		`dyn(object.spec).scheduling`,
		`dyn(object.spec.template.spec).evictionResponders`,
		`dyn(object.spec.template.spec).workloadRef`,
		`dyn(oldObject.spec).scheduling`,
		`dyn(oldObject.spec.template.spec).evictionResponders`,
		`dyn(oldObject.spec.template.spec).workloadRef`,
		`dyn(volume.emptyDir).mode`,
		`dyn(volume.configMap).defaultUser`,
		`dyn(volume.secret).defaultUser`,
		`dyn(volume.projected).defaultUser`,
		`dyn(volume.downwardAPI).defaultUser`,
		`dyn(item).user`,
		`dyn(source.serviceAccountToken).user`,
		`dyn(source.clusterTrustBundle).user`,
		`dyn(source.podCertificate).user`,
		`dyn(mount).bindMountOptions`,
		`ephemeralContainers`,
		`!has(container.lifecycle)`,
		`!has(container.livenessProbe)`,
		`!has(container.readinessProbe)`,
		`!has(container.startupProbe)`,
	} {
		if !strings.Contains(job, marker) {
			t.Fatalf("Job supported-window contract lacks %q", marker)
		}
	}
	legacyEnvelope := entries[0].validations[1].Expression
	if !strings.HasPrefix(legacyEnvelope, `(request.operation == "UPDATE"`) {
		t.Fatalf("predecessor annotation envelope is not restricted to Job updates: %s", legacyEnvelope)
	}
	applyBranch := `) || (request.operation == "UPDATE" && object.metadata.labels["operator.ptah.dev/operation"] == "apply"`
	applyIndex := strings.Index(legacyEnvelope, applyBranch)
	currentIndex := strings.Index(legacyEnvelope, `) || (has(object.metadata.annotations)`)
	if applyIndex < 0 || currentIndex < 0 || applyIndex >= currentIndex {
		t.Fatalf("predecessor Apply cleanup branch is not separated from current envelopes: %s", legacyEnvelope)
	}
	if strings.Contains(legacyEnvelope[:applyIndex], `"apply"`) {
		t.Fatal("predecessor read-only cleanup annotation envelope permits Apply Jobs")
	}
	applyEnvelope := legacyEnvelope[applyIndex:currentIndex]
	for _, forbidden := range []string{
		`operator.ptah.dev/controller-image`,
		`operator.ptah.dev/controller-revision`,
		`operator.ptah.dev/controller-state-version`,
	} {
		if strings.Contains(applyEnvelope, forbidden) {
			t.Fatalf("predecessor Apply cleanup envelope permits %s", forbidden)
		}
	}
	chunk := strings.Join(validationExpressions(entries[1].validations), "\n")
	for _, marker := range []string{
		`object.metadata.labels.size() == 2`,
		`object.metadata.ownerReferences[0].kind == "PtahSchemaPlan"`,
		`object.immutable`,
		`object.binaryData.size() == 1`,
		`object.binaryData["chunk"].size() <= 524288`,
	} {
		if !strings.Contains(chunk, marker) {
			t.Fatalf("chunk structural contract lacks %q", marker)
		}
	}
	plan := strings.Join(validationExpressions(entries[2].validations), "\n")
	for _, marker := range []string{
		`object.metadata.labels.size() == 1`,
		`object.spec.schemaRef.uid == object.metadata.ownerReferences[0].uid`,
		`object.spec.contractVersion == 3`,
		`object.spec.executionBindingID.matches`,
		`object.spec.statementCount >= 1`,
		`object.spec.chunks.size() <= 16`,
		`chunk.key == "chunk"`,
		`!has(object.status)`,
	} {
		if !strings.Contains(plan, marker) {
			t.Fatalf("plan structural contract lacks %q", marker)
		}
	}
}

func TestControllerJobSupportedWindowExpressionsEvaluate(t *testing.T) {
	t.Parallel()

	environment, err := celgo.NewEnv(
		celgo.Variable("object", celgo.DynType),
		celgo.Variable("oldObject", celgo.DynType),
		celgo.Variable("request", celgo.DynType),
	)
	if err != nil {
		t.Fatal(err)
	}
	expressions := []string{
		controllerJobSupportedWindowTopLevelExpression("object"),
		controllerJobSupportedWindowVolumeExpression("object"),
		controllerJobSupportedWindowProjectionExpression("object"),
		controllerJobSupportedWindowMountExpression("object"),
		controllerJobSupportedWindowContainerExpression("object"),
		controllerJobPreviousObjectExpression(controllerJobSupportedWindowTopLevelExpression("oldObject")),
		controllerJobPreviousObjectExpression(controllerJobSupportedWindowVolumeExpression("oldObject")),
		controllerJobPreviousObjectExpression(controllerJobSupportedWindowProjectionExpression("oldObject")),
		controllerJobPreviousObjectExpression(controllerJobSupportedWindowMountExpression("oldObject")),
		controllerJobPreviousObjectExpression(controllerJobSupportedWindowContainerExpression("oldObject")),
	}
	programs := make([]celgo.Program, 0, len(expressions))
	for index, expression := range expressions {
		ast, issues := environment.Compile(expression)
		if issues != nil && issues.Err() != nil {
			t.Fatalf("compile supported-window expression %d: %v", index, issues.Err())
		}
		program, programErr := environment.Program(ast)
		if programErr != nil {
			t.Fatalf("build supported-window expression %d: %v", index, programErr)
		}
		programs = append(programs, program)
	}
	evaluate := func(object, oldObject map[string]any, operation string) bool {
		t.Helper()
		for index, program := range programs {
			result, _, evaluationErr := program.Eval(map[string]any{
				"object":    object,
				"oldObject": oldObject,
				"request":   map[string]any{"operation": operation},
			})
			if evaluationErr != nil {
				t.Fatalf("evaluate supported-window expression %d: %v", index, evaluationErr)
			}
			allowed, ok := result.Value().(bool)
			if !ok {
				t.Fatalf("supported-window expression %d result = %T(%v), want bool", index, result.Value(), result.Value())
			}
			if !allowed {
				return false
			}
		}
		return true
	}

	base := controllerJobCELObject()
	if !evaluate(base, controllerJobCELClone(t, base), "UPDATE") {
		t.Fatal("supported-window expressions rejected the generated Job baseline")
	}

	mutations := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "Job scheduling", mutate: func(object map[string]any) {
			controllerJobCELSpec(object)["scheduling"] = map[string]any{}
		}},
		{name: "Pod eviction responders", mutate: func(object map[string]any) {
			controllerJobCELPodSpec(object)["evictionResponders"] = []any{}
		}},
		{name: "Pod workload reference", mutate: func(object map[string]any) {
			controllerJobCELPodSpec(object)["workloadRef"] = map[string]any{"name": "foreign"}
		}},
		{name: "empty directory mode", mutate: controllerJobCELVolumeMutation(map[string]any{"emptyDir": map[string]any{"mode": int64(0o700)}})},
		{name: "ConfigMap default user", mutate: controllerJobCELVolumeMutation(map[string]any{"configMap": map[string]any{"defaultUser": int64(65532)}})},
		{name: "ConfigMap item user", mutate: controllerJobCELVolumeMutation(map[string]any{"configMap": map[string]any{"items": []any{map[string]any{"user": int64(65532)}}}})},
		{name: "Secret default user", mutate: controllerJobCELVolumeMutation(map[string]any{"secret": map[string]any{"defaultUser": int64(65532)}})},
		{name: "Secret item user", mutate: controllerJobCELVolumeMutation(map[string]any{"secret": map[string]any{"items": []any{map[string]any{"user": int64(65532)}}}})},
		{name: "projected default user", mutate: controllerJobCELVolumeMutation(map[string]any{"projected": map[string]any{"defaultUser": int64(65532), "sources": []any{}}})},
		{name: "projected ConfigMap item user", mutate: controllerJobCELProjectionMutation(map[string]any{"configMap": map[string]any{"items": []any{map[string]any{"user": int64(65532)}}}})},
		{name: "projected Secret item user", mutate: controllerJobCELProjectionMutation(map[string]any{"secret": map[string]any{"items": []any{map[string]any{"user": int64(65532)}}}})},
		{name: "service account token user", mutate: controllerJobCELProjectionMutation(map[string]any{"serviceAccountToken": map[string]any{"user": int64(65532)}})},
		{name: "cluster trust bundle user", mutate: controllerJobCELProjectionMutation(map[string]any{"clusterTrustBundle": map[string]any{"user": int64(65532)}})},
		{name: "Pod certificate user", mutate: controllerJobCELProjectionMutation(map[string]any{"podCertificate": map[string]any{"user": int64(65532)}})},
		{name: "downward API default user", mutate: controllerJobCELVolumeMutation(map[string]any{"downwardAPI": map[string]any{"defaultUser": int64(65532)}})},
		{name: "downward API item user", mutate: controllerJobCELVolumeMutation(map[string]any{"downwardAPI": map[string]any{"items": []any{map[string]any{"user": int64(65532)}}}})},
		{name: "projected downward API item user", mutate: controllerJobCELProjectionMutation(map[string]any{"downwardAPI": map[string]any{"items": []any{map[string]any{"user": int64(65532)}}}})},
		{name: "bind mount options", mutate: controllerJobCELContainerMutation("volumeMounts", []any{map[string]any{"bindMountOptions": []any{"noexec"}}})},
		{name: "ephemeral container", mutate: func(object map[string]any) {
			controllerJobCELPodSpec(object)["ephemeralContainers"] = []any{map[string]any{"name": "foreign"}}
		}},
		{name: "container lifecycle", mutate: controllerJobCELContainerMutation("lifecycle", map[string]any{})},
		{name: "liveness probe", mutate: controllerJobCELContainerMutation("livenessProbe", map[string]any{"httpGet": map[string]any{"protocol": "HTTP/1.1"}})},
		{name: "readiness probe", mutate: controllerJobCELContainerMutation("readinessProbe", map[string]any{"grpc": map[string]any{"mode": "Pod"}})},
		{name: "startup probe", mutate: controllerJobCELContainerMutation("startupProbe", map[string]any{})},
	}

	for _, test := range mutations {
		test := test
		t.Run(test.name, func(t *testing.T) {
			current := controllerJobCELClone(t, base)
			test.mutate(current)
			if evaluate(current, controllerJobCELClone(t, base), "UPDATE") {
				t.Fatal("supported-window expressions accepted the field on object")
			}

			previous := controllerJobCELClone(t, base)
			test.mutate(previous)
			if evaluate(controllerJobCELClone(t, base), previous, "UPDATE") {
				t.Fatal("supported-window expressions accepted the field on oldObject")
			}
			if !evaluate(controllerJobCELClone(t, base), previous, "CREATE") {
				t.Fatal("supported-window expressions evaluated oldObject during CREATE")
			}
		})
	}
}

func controllerJobCELObject() map[string]any {
	return map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"volumes":        []any{},
					"containers":     []any{map[string]any{}},
					"initContainers": []any{map[string]any{}},
				},
			},
		},
	}
}

func controllerJobCELClone(t *testing.T, source map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var clone map[string]any
	if err := json.Unmarshal(encoded, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func controllerJobCELSpec(object map[string]any) map[string]any {
	return object["spec"].(map[string]any)
}

func controllerJobCELPodSpec(object map[string]any) map[string]any {
	template := controllerJobCELSpec(object)["template"].(map[string]any)
	return template["spec"].(map[string]any)
}

func controllerJobCELVolumeMutation(volume map[string]any) func(map[string]any) {
	return func(object map[string]any) {
		controllerJobCELPodSpec(object)["volumes"] = []any{volume}
	}
}

func controllerJobCELProjectionMutation(source map[string]any) func(map[string]any) {
	return controllerJobCELVolumeMutation(map[string]any{
		"projected": map[string]any{"sources": []any{source}},
	})
}

func controllerJobCELContainerMutation(field string, value any) func(map[string]any) {
	return func(object map[string]any) {
		controllerJobCELPodSpec(object)["containers"] = []any{map[string]any{field: value}}
	}
}

func TestControllerObjectGuardsPrecedeControllerPrivileges(t *testing.T) {
	t.Parallel()

	weights := []string{
		certificateValidatingWriteBindingWeight,
		controllerObjectPolicyWeight,
		controllerObjectBindingWeight,
		releaseActivationHookWeight,
	}
	previous, err := strconv.Atoi(weights[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, weight := range weights[1:] {
		current, err := strconv.Atoi(weight)
		if err != nil {
			t.Fatal(err)
		}
		if current <= previous {
			t.Fatalf("controller object guard hook order is not strictly increasing: %v", weights)
		}
		previous = current
	}
}

func TestControllerObjectGuardVerifyRejectsContractTampering(t *testing.T) {
	t.Parallel()

	guard := testControllerObjectGuard()
	policies := make(map[string]*admissionregistrationv1.ValidatingAdmissionPolicy)
	bindings := make(map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding)
	for _, entry := range guard.entries() {
		policies[entry.name] = guard.policy(entry)
		bindings[entry.name] = guard.binding(entry)
	}
	guard.Policies = &rolloutPolicyClient{objects: policies}
	guard.Bindings = &rolloutBindingClient{objects: bindings}
	if err := guard.Verify(context.Background()); err != nil {
		t.Fatalf("verify exact controller object guards: %v", err)
	}

	job := guard.entries()[0]
	policies[job.name].Spec.Validations[0].Expression = "true"
	if err := guard.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), "immutable contract") {
		t.Fatalf("tampered controller object policy error = %v", err)
	}
	policies[job.name] = guard.policy(job)

	plan := guard.entries()[2]
	bindings[plan.name].Spec.MatchResources.ResourceRules[0].RuleWithOperations.Operations =
		[]admissionregistrationv1.OperationType{admissionregistrationv1.Update}
	if err := guard.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), "immutable contract") {
		t.Fatalf("tampered controller object binding error = %v", err)
	}
}

func TestControllerObjectGuardWaitReady(t *testing.T) {
	t.Parallel()

	guard := testControllerObjectGuard()
	policies := make(map[string]*admissionregistrationv1.ValidatingAdmissionPolicy)
	bindings := make(map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding)
	for _, entry := range guard.entries() {
		policies[entry.name] = readyPolicy(guard.policy(entry))
		bindings[entry.name] = guard.binding(entry)
	}
	guard.Policies = &rolloutPolicyClient{objects: policies}
	guard.Bindings = &rolloutBindingClient{objects: bindings}
	if err := guard.WaitReady(context.Background()); err != nil {
		t.Fatalf("wait for ready controller object guards: %v", err)
	}
}

func TestControllerObjectGuardWaitReadyRejectsTypeWarnings(t *testing.T) {
	t.Parallel()

	guard := testControllerObjectGuard()
	policies := make(map[string]*admissionregistrationv1.ValidatingAdmissionPolicy)
	bindings := make(map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding)
	entries := guard.entries()
	for _, entry := range entries {
		policies[entry.name] = readyPolicy(guard.policy(entry))
		bindings[entry.name] = guard.binding(entry)
	}
	policies[entries[1].name].Status.TypeChecking.ExpressionWarnings = []admissionregistrationv1.ExpressionWarning{{
		FieldRef: "spec.validations[0].expression",
		Warning:  "unexpected object type",
	}}
	guard.Policies = &rolloutPolicyClient{objects: policies}
	guard.Bindings = &rolloutBindingClient{objects: bindings}
	err := guard.WaitReady(context.Background())
	if err == nil || !strings.Contains(err.Error(), "CEL type-check warnings: unexpected object type") {
		t.Fatalf("type-check warning error = %v", err)
	}
}

func TestRenderedControllerObjectGuardsMatchCompiledContracts(t *testing.T) {
	path := os.Getenv("PTAH_ROLLOUT_GUARD_RENDER")
	if path == "" {
		t.Skip("PTAH_ROLLOUT_GUARD_RENDER is set by the chart contract gate")
	}
	rendered, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	guard := testControllerObjectGuard()
	guard.ReleaseName = "ptah-e2e"
	guard.ReleaseNamespace = "ptah-e2e"
	guard.ControllerServiceAccountName = "ptah-e2e-ptah-operator"
	policies := make(map[string]*admissionregistrationv1.ValidatingAdmissionPolicy)
	bindings := make(map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding)
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
			policies[object.Name] = &object
		case "ValidatingAdmissionPolicyBinding":
			var object admissionregistrationv1.ValidatingAdmissionPolicyBinding
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			bindings[object.Name] = &object
		}
	}
	for _, entry := range guard.entries() {
		policy := policies[entry.name]
		binding := bindings[entry.name]
		if err := guard.verifyPolicy(entry, policy); err != nil {
			expected := guard.policy(entry)
			if policy != nil && len(policy.Spec.Validations) == len(expected.Spec.Validations) {
				for index := range expected.Spec.Validations {
					if policy.Spec.Validations[index].Expression != expected.Spec.Validations[index].Expression {
						t.Fatalf(
							"rendered %s policy validation %d differs\nactual:   %s\nexpected: %s",
							entry.component,
							index,
							policy.Spec.Validations[index].Expression,
							expected.Spec.Validations[index].Expression,
						)
					}
				}
			}
			t.Fatalf("rendered %s policy: %v", entry.component, err)
		}
		if err := guard.verifyBinding(entry, binding); err != nil {
			t.Fatalf("rendered %s binding: %v", entry.component, err)
		}
		if policy.Annotations["helm.sh/hook-weight"] != controllerObjectPolicyWeight ||
			binding.Annotations["helm.sh/hook-weight"] != controllerObjectBindingWeight {
			t.Fatalf("%s is not installed in its exact early hook order", entry.component)
		}
	}
}

func validationExpressions(validations []admissionregistrationv1.Validation) []string {
	expressions := make([]string, len(validations))
	for index, validation := range validations {
		expressions[index] = validation.Expression
	}
	return expressions
}

func assertExactControllerObjectMatch(
	t *testing.T,
	match *admissionregistrationv1.MatchResources,
	entry controllerObjectGuardEntry,
) {
	t.Helper()
	if match == nil || match.MatchPolicy == nil || *match.MatchPolicy != admissionregistrationv1.Exact {
		t.Fatal("controller object guard matching is not Exact")
	}
	if match.NamespaceSelector != nil || match.ObjectSelector != nil || len(match.ExcludeResourceRules) != 0 {
		t.Fatalf("controller object guard must not rely on selectors or exclusions: %#v", match)
	}
	if len(match.ResourceRules) != 1 {
		t.Fatalf("controller object guard rules = %d, want one", len(match.ResourceRules))
	}
	rule := match.ResourceRules[0]
	if !reflect.DeepEqual(rule.Operations, entry.operations) ||
		!reflect.DeepEqual(rule.APIGroups, entry.apiGroups) ||
		!reflect.DeepEqual(rule.APIVersions, entry.apiVersions) ||
		!reflect.DeepEqual(rule.Resources, []string{entry.resource}) ||
		len(rule.ResourceNames) != 0 || rule.Scope == nil || *rule.Scope != admissionregistrationv1.NamespacedScope {
		t.Fatalf("controller object rule is not exact: %#v", rule)
	}
}

func testControllerObjectGuard() *ControllerObjectGuard {
	return &ControllerObjectGuard{
		Policies:                     &rolloutPolicyClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{}},
		Bindings:                     &rolloutBindingClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding{}},
		ReleaseName:                  "ptah",
		ReleaseNamespace:             "ptah-system",
		ControllerServiceAccountName: "ptah-controller",
		PollEvery:                    time.Millisecond,
	}
}
