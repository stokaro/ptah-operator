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

func TestControllerObjectGuardNamesAreReleaseDistinctAndVersioned(t *testing.T) {
	t.Parallel()

	names := map[string]string{
		ControllerJobWriteGuardPolicyName("ptah-system", "ptah", 1, "manager:v1"):   controllerJobWriteGuardNamePrefix,
		ControllerChunkWriteGuardPolicyName("ptah-system", "ptah", 1, "manager:v1"): controllerChunkWriteGuardPrefix,
		ControllerPlanWriteGuardPolicyName("ptah-system", "ptah", 1, "manager:v1"):  controllerPlanWriteGuardNamePrefix,
	}
	if len(names) != 3 {
		t.Fatal("typed controller object guards do not have distinct names")
	}
	for name, prefix := range names {
		if !strings.HasPrefix(name, prefix) || len(name) > 63 {
			t.Fatalf("controller object guard name %q is not bounded and versioned", name)
		}
	}
	if ControllerJobWriteGuardPolicyName("ptah-system", "ptah", 1, "manager:v1") !=
		ControllerJobWriteGuardPolicyName("ptah-system", "ptah", 1, "manager:v1") {
		t.Fatal("controller object guard name is not deterministic")
	}
	if ControllerJobWriteGuardPolicyName("ptah-system", "ptah", 1, "manager:v1") ==
		ControllerJobWriteGuardPolicyName("other", "ptah", 1, "manager:v1") ||
		ControllerJobWriteGuardPolicyName("ptah-system", "ptah", 1, "manager:v1") ==
			ControllerJobWriteGuardPolicyName("ptah-system", "other", 1, "manager:v1") {
		t.Fatal("controller object guard name does not bind both release identity fields")
	}

	rollout := runtimePodGuardFixture()
	other := *rollout
	other.ReleaseSequence++
	other.ManagerImage = "registry.example/ptah@sha256:" + strings.Repeat("b", 64)
	if ControllerJobWriteGuardPolicyName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage) ==
		ControllerJobWriteGuardPolicyName(other.ReleaseNamespace, other.ReleaseName, other.ReleaseSequence, other.ManagerImage) {
		t.Fatal("controller object guard name did not change with candidate release identity")
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
			native := stripAdmissionConvergenceDependencyProbe(t, policy)
			binding := guard.binding(entry)
			if policy.Spec.ParamKind == nil || policy.Spec.ParamKind.APIVersion != "v1" || policy.Spec.ParamKind.Kind != "ConfigMap" {
				t.Fatalf("controller object guard does not use the release activation ConfigMap: %#v", policy.Spec.ParamKind)
			}
			if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionregistrationv1.Fail {
				t.Fatal("controller object guard is not fail-closed")
			}
			assertExactControllerObjectMatch(t, native.Spec.MatchConstraints, entry)
			assertControllerObjectMatchWithConvergenceProbe(t, binding.Spec.MatchResources, entry)
			wantUsername := `request.userInfo.username in ["system:serviceaccount:ptah-system:ptah-controller"]`
			if !reflect.DeepEqual(native.Spec.MatchConditions, []admissionregistrationv1.MatchCondition{{
				Name: "candidate-or-predecessor-controller-service-account", Expression: wantUsername,
			}}) {
				t.Fatalf("controller caller match is not exact: %#v", native.Spec.MatchConditions)
			}
			if binding.Spec.PolicyName != policy.Name ||
				!reflect.DeepEqual(binding.Spec.ValidationActions, []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}) {
				t.Fatalf("controller object binding is not exact deny-only enforcement: %#v", binding.Spec)
			}
			if binding.Spec.ParamRef == nil || binding.Spec.ParamRef.Name != ReleaseActivationName ||
				binding.Spec.ParamRef.Namespace != guard.ReleaseNamespace ||
				binding.Spec.ParamRef.ParameterNotFoundAction == nil ||
				*binding.Spec.ParamRef.ParameterNotFoundAction != admissionregistrationv1.DenyAction {
				t.Fatalf("controller object binding is not fail-closed on the exact activation parameter: %#v", binding.Spec.ParamRef)
			}
			if !reflect.DeepEqual(native.Spec.Variables, controllerObjectActivationVariables(guard.ReleaseSequence, guard.PreviousControllerReleaseSequence)) {
				t.Fatalf("controller object activation variables differ from the exact contract: %#v", native.Spec.Variables)
			}
			variableNames := make([]string, len(native.Spec.Variables))
			for index, variable := range native.Spec.Variables {
				variableNames[index] = variable.Name
			}
			wantVariableNames := []string{
				"activeRelease",
				"activeControllerStateString",
				"activeControllerState",
				"activeControllerImage",
				"candidateRelease",
				"previousRelease",
			}
			if !reflect.DeepEqual(variableNames, wantVariableNames) {
				t.Fatalf("controller object activation variable order = %v, want %v", variableNames, wantVariableNames)
			}
			if len(native.Spec.Validations) != len(entry.validations)+2 ||
				native.Spec.Validations[0].Expression != guard.activationParameterExpression() {
				t.Fatalf("controller object guard does not validate its activation parameter first: %#v", native.Spec.Validations)
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
	bootstrapMarker := `(request.operation == "CREATE" && variables.activeRelease == variables.previousRelease)`
	currentMarker := `)) || ((has(object.metadata.annotations)`
	bootstrapIndex := strings.Index(legacyEnvelope, bootstrapMarker)
	currentIndex := strings.Index(legacyEnvelope, currentMarker)
	if bootstrapIndex < 0 || currentIndex < 0 || bootstrapIndex >= currentIndex {
		t.Fatalf("legacy Job CREATE is not isolated to bootstrap before the active contract: %s", legacyEnvelope)
	}
	legacyPart := legacyEnvelope[:currentIndex]
	currentPart := legacyEnvelope[currentIndex:]
	if !strings.Contains(legacyPart, `request.operation == "UPDATE"`) ||
		!strings.Contains(legacyPart, `object.metadata.labels["operator.ptah.dev/operation"] == "apply"`) {
		t.Fatalf("legacy terminal Job update envelopes are incomplete: %s", legacyPart)
	}
	for _, forbidden := range []string{
		`operator.ptah.dev/controller-image`,
		`operator.ptah.dev/controller-revision`,
		`operator.ptah.dev/controller-state-version`,
	} {
		if strings.Contains(legacyPart, forbidden) {
			t.Fatalf("legacy Job envelope permits %s", forbidden)
		}
	}
	for _, required := range []string{
		`request.operation == "UPDATE" || (request.operation == "CREATE" && (`,
		`object.metadata.annotations["operator.ptah.dev/controller-image"] == variables.activeControllerImage`,
		`object.metadata.annotations["operator.ptah.dev/controller-state-version"] == variables.activeControllerStateString`,
	} {
		if !strings.Contains(currentPart, required) {
			t.Fatalf("current Job envelope is not bound to active controller identity: missing %q", required)
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
		`variables.activeRelease == variables.previousRelease && object.spec.contractVersion == 2`,
		`object.spec.executionBindingID.matches`,
		`has(dyn(object.spec).controllerImage)`,
		`dyn(object.spec).controllerImage.matches`,
		`has(dyn(object.spec).controllerRevision)`,
		`dyn(object.spec).controllerRevision != ""`,
		`has(dyn(object.spec).controllerStateVersion)`,
		`dyn(object.spec).controllerStateVersion >= 1`,
		`dyn(object.spec).controllerImage == variables.activeControllerImage`,
		`dyn(object.spec).controllerStateVersion == variables.activeControllerState`,
		`object.spec.statementCount >= 1`,
		`object.spec.chunks.size() <= 16`,
		`chunk.key == "chunk"`,
		`!has(object.status)`,
	} {
		if !strings.Contains(plan, marker) {
			t.Fatalf("plan structural contract lacks %q", marker)
		}
	}
	for _, staticReference := range []string{
		`object.spec.controllerImage`,
		`object.spec.controllerRevision`,
		`object.spec.controllerStateVersion`,
	} {
		if strings.Contains(plan, staticReference) {
			t.Fatalf("plan structural contract statically references candidate-only field %q and cannot type-check against the predecessor CRD", staticReference)
		}
	}
}

// This white-box test evaluates the unexported CEL fragments directly because
// their bootstrap-to-active transition is not observable through a public Go
// API until the policy is installed in an API server.
func TestControllerObjectActivationContractsEvaluate(t *testing.T) {
	t.Parallel()

	environment, err := celgo.NewEnv(
		celgo.Variable("object", celgo.DynType),
		celgo.Variable("request", celgo.DynType),
		celgo.Variable("variables", celgo.DynType),
	)
	if err != nil {
		t.Fatal(err)
	}
	evaluate := func(expression string, object map[string]any, operation string, variables map[string]any) bool {
		t.Helper()
		ast, issues := environment.Compile(expression)
		if issues != nil && issues.Err() != nil {
			t.Fatalf("compile activation contract: %v", issues.Err())
		}
		program, programErr := environment.Program(ast)
		if programErr != nil {
			t.Fatalf("build activation contract: %v", programErr)
		}
		result, _, evaluationErr := program.Eval(map[string]any{
			"object":    object,
			"request":   map[string]any{"operation": operation},
			"variables": variables,
		})
		if evaluationErr != nil {
			t.Fatalf("evaluate activation contract: %v", evaluationErr)
		}
		allowed, ok := result.Value().(bool)
		if !ok {
			t.Fatalf("activation contract result = %T(%v), want bool", result.Value(), result.Value())
		}
		return allowed
	}

	const activeImage = "registry.example/ptah@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	bootstrap := map[string]any{
		"activeRelease":               int64(0),
		"candidateRelease":            int64(1),
		"previousRelease":             int64(0),
		"activeControllerStateString": "1",
		"activeControllerState":       int64(1),
		"activeControllerImage":       activeImage,
	}
	active := map[string]any{
		"activeRelease":               int64(1),
		"candidateRelease":            int64(1),
		"previousRelease":             int64(0),
		"activeControllerStateString": "1",
		"activeControllerState":       int64(1),
		"activeControllerImage":       activeImage,
	}
	const nextActiveImage = "registry.example/ptah@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	nextActive := map[string]any{
		"activeRelease":               int64(2),
		"candidateRelease":            int64(1),
		"previousRelease":             int64(0),
		"activeControllerStateString": "2",
		"activeControllerState":       int64(2),
		"activeControllerImage":       nextActiveImage,
	}

	legacyJob := controllerObjectLegacyJobCELObject(false)
	legacyApplyJob := controllerObjectLegacyJobCELObject(true)
	currentJob := controllerObjectCurrentJobCELObject(activeImage, int64(1))
	jobExpression := controllerJobAnnotationContractExpression()
	for _, test := range []struct {
		name      string
		object    map[string]any
		operation string
		variables map[string]any
		want      bool
	}{
		{name: "legacy create during bootstrap", object: legacyJob, operation: "CREATE", variables: bootstrap, want: true},
		{name: "legacy create after activation", object: legacyJob, operation: "CREATE", variables: active, want: false},
		{name: "legacy terminal update after activation", object: legacyJob, operation: "UPDATE", variables: active, want: true},
		{name: "legacy apply create during bootstrap", object: legacyApplyJob, operation: "CREATE", variables: bootstrap, want: true},
		{name: "legacy apply create after activation", object: legacyApplyJob, operation: "CREATE", variables: active, want: false},
		{name: "legacy apply terminal update after activation", object: legacyApplyJob, operation: "UPDATE", variables: active, want: true},
		{name: "current create after activation", object: currentJob, operation: "CREATE", variables: active, want: true},
		{name: "active predecessor identity create before cutover", object: currentJob, operation: "CREATE", variables: bootstrap, want: true},
		{name: "active predecessor identity update before cutover", object: currentJob, operation: "UPDATE", variables: bootstrap, want: true},
		{name: "previous current update after newer activation", object: currentJob, operation: "UPDATE", variables: nextActive, want: true},
		{name: "previous current create after newer activation", object: currentJob, operation: "CREATE", variables: nextActive, want: false},
		{name: "current create with foreign image", object: controllerObjectCurrentJobCELObject(nextActiveImage, int64(1)), operation: "CREATE", variables: active, want: false},
		{name: "current create with foreign state", object: controllerObjectCurrentJobCELObject(activeImage, int64(2)), operation: "CREATE", variables: active, want: false},
	} {
		t.Run("Job/"+test.name, func(t *testing.T) {
			if got := evaluate(jobExpression, test.object, test.operation, test.variables); got != test.want {
				t.Fatalf("Job activation contract = %t, want %t", got, test.want)
			}
		})
	}

	legacyPlan := controllerObjectPlanCELObject(2, "", 0)
	currentPlan := controllerObjectPlanCELObject(3, activeImage, 1)
	planExpression := controllerPlanContractExpression()
	for _, test := range []struct {
		name      string
		object    map[string]any
		variables map[string]any
		want      bool
	}{
		{name: "legacy v2 during bootstrap", object: legacyPlan, variables: bootstrap, want: true},
		{name: "legacy v2 after activation", object: legacyPlan, variables: active, want: false},
		{name: "current v3 after activation", object: currentPlan, variables: active, want: true},
		{name: "active predecessor v3 before cutover", object: currentPlan, variables: bootstrap, want: true},
		{name: "current v3 with foreign image", object: controllerObjectPlanCELObject(3, "registry.example/ptah@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", 1), variables: active, want: false},
		{name: "current v3 with foreign state", object: controllerObjectPlanCELObject(3, activeImage, 2), variables: active, want: false},
	} {
		t.Run("Plan/"+test.name, func(t *testing.T) {
			if got := evaluate(planExpression, test.object, "CREATE", test.variables); got != test.want {
				t.Fatalf("Plan activation contract = %t, want %t", got, test.want)
			}
		})
	}
}

func controllerObjectLegacyJobCELObject(apply bool) map[string]any {
	operation := "plan"
	annotations := map[string]any{
		"operator.ptah.dev/operation-id":              "operation-id",
		"operator.ptah.dev/input-fingerprint":         "sha256:" + strings.Repeat("1", 64),
		"operator.ptah.dev/ptah-version":              "v1",
		"operator.ptah.dev/execution-binding-id":      "v1-" + strings.Repeat("2", 32),
		"operator.ptah.dev/admission-snapshot-digest": "sha256:" + strings.Repeat("3", 64),
	}
	if apply {
		operation = "apply"
		annotations["operator.ptah.dev/plan-fingerprint"] = "sha256:" + strings.Repeat("4", 64)
		annotations["operator.ptah.dev/plan-content-digest"] = "sha256:" + strings.Repeat("5", 64)
	}
	return map[string]any{"metadata": map[string]any{
		"labels":      map[string]any{"operator.ptah.dev/operation": operation},
		"annotations": annotations,
	}}
}

func controllerObjectCurrentJobCELObject(image string, state int64) map[string]any {
	object := controllerObjectLegacyJobCELObject(false)
	annotations := object["metadata"].(map[string]any)["annotations"].(map[string]any)
	annotations["operator.ptah.dev/controller-image"] = image
	annotations["operator.ptah.dev/controller-revision"] = "revision"
	annotations["operator.ptah.dev/controller-state-version"] = strconv.FormatInt(state, 10)
	return object
}

func controllerObjectPlanCELObject(contractVersion int64, image string, state int64) map[string]any {
	spec := map[string]any{
		"contractVersion":          contractVersion,
		"fingerprint":              "sha256:" + strings.Repeat("1", 64),
		"contentDigest":            "sha256:" + strings.Repeat("2", 64),
		"artifactDigest":           "sha256:" + strings.Repeat("3", 64),
		"coordinationDigest":       "sha256:" + strings.Repeat("4", 64),
		"targetIdentityDigest":     "sha256:" + strings.Repeat("5", 64),
		"actualStateFingerprint":   "sha256:" + strings.Repeat("6", 64),
		"desiredStateFingerprint":  "sha256:" + strings.Repeat("7", 64),
		"policyFingerprint":        "sha256:" + strings.Repeat("8", 64),
		"verificationPolicyUID":    "uid",
		"verificationPolicyDigest": "sha256:" + strings.Repeat("9", 64),
		"executionBindingID":       "v1-" + strings.Repeat("a", 32),
		"ptahVersion":              "v1",
		"executorImage":            "registry.example/executor@sha256:" + strings.Repeat("b", 64),
		"runnerImage":              "registry.example/runner@sha256:" + strings.Repeat("c", 64),
		"runnerProtocolVersion":    int64(1),
		"dialect":                  "postgresql",
		"statementCount":           int64(1),
		"size":                     int64(1),
	}
	if contractVersion == 3 {
		spec["controllerImage"] = image
		spec["controllerRevision"] = "revision"
		spec["controllerStateVersion"] = state
	}
	return map[string]any{"spec": spec}
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
		releaseActivationHookWeight,
		certificateValidatingWriteBindingWeight,
		controllerObjectPolicyWeight,
		"-149",
		"-148",
		controllerObjectBindingWeight,
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
			t.Fatalf("controller object policy, activation parameter/guard, and binding order is not strictly increasing: %v", weights)
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
	guard.ManagerImage = renderedGuardManagerImage
	guard.ControllerServiceAccountName = renderedDeploymentServiceAccount(t, rendered, "ptah-e2e-ptah-operator")
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
	if match.NamespaceSelector == nil || len(match.NamespaceSelector.MatchLabels) != 0 ||
		len(match.NamespaceSelector.MatchExpressions) != 0 || match.ObjectSelector == nil ||
		len(match.ObjectSelector.MatchLabels) != 0 || len(match.ObjectSelector.MatchExpressions) != 0 ||
		len(match.ExcludeResourceRules) != 0 {
		t.Fatalf("controller object guard must declare exact match-all selectors without exclusions: %#v", match)
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

func assertControllerObjectMatchWithConvergenceProbe(
	t *testing.T,
	match *admissionregistrationv1.MatchResources,
	entry controllerObjectGuardEntry,
) {
	t.Helper()
	if match == nil || len(match.ResourceRules) != 2 {
		t.Fatalf("controller object binding rules = %#v, want native rule plus convergence marker rule", match)
	}
	if !reflect.DeepEqual(match.ResourceRules[1], admissionConvergenceProbeResourceRule()) {
		t.Fatalf("controller object binding convergence rule = %#v, want %#v", match.ResourceRules[1], admissionConvergenceProbeResourceRule())
	}
	native := match.DeepCopy()
	native.ResourceRules = native.ResourceRules[:1]
	assertExactControllerObjectMatch(t, native, entry)
}

func testControllerObjectGuard() *ControllerObjectGuard {
	return &ControllerObjectGuard{
		Policies:                     &rolloutPolicyClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{}},
		Bindings:                     &rolloutBindingClient{objects: map[string]*admissionregistrationv1.ValidatingAdmissionPolicyBinding{}},
		ReleaseName:                  "ptah",
		ReleaseNamespace:             "ptah-system",
		ControllerServiceAccountName: "ptah-controller",
		ReleaseSequence:              1,
		ManagerImage:                 "registry.example/ptah@sha256:" + strings.Repeat("a", 64),
		PollEvery:                    time.Millisecond,
	}
}
