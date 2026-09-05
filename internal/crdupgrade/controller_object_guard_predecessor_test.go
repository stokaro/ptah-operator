package crdupgrade

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	celgo "github.com/google/cel-go/cel"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
)

func TestControllerJobGuardAcceptsExactPredecessorCreateDuringBootstrap(t *testing.T) {
	t.Parallel()

	job := predecessorControllerJobFixture(t)
	object := predecessorControllerJobProbe(t, job)
	activation := map[string]any{
		"activeRelease":               int64(0),
		"candidateRelease":            int64(1),
		"previousRelease":             int64(0),
		"activeControllerStateString": "1",
		"activeControllerState":       int64(1),
		"activeControllerImage":       predecessorControllerImage,
	}
	validations, evaluate := controllerJobValidationEvaluator(t, activation)
	for index, validation := range validations {
		if !evaluate(index, object) {
			t.Errorf("Job validation %d rejected the exact predecessor CREATE contract: %s", index, validation.Expression)
		}
	}
	status, ok := object["status"].(map[string]any)
	if !ok || len(status) != 0 {
		t.Fatalf("typed Job admission status = %#v, want a present empty object", object["status"])
	}
	statusValidation := controllerObjectValidationIndex(t, validations, `dyn(object.status).size() == 0`)
	withoutStatus := controllerJobCELClone(t, object)
	delete(withoutStatus, "status")
	if !evaluate(statusValidation, withoutStatus) {
		t.Fatal("CREATE status validation rejected an absent status")
	}
	withClientStatus := controllerJobCELClone(t, object)
	withClientStatus["status"] = map[string]any{"active": int64(1)}
	if evaluate(statusValidation, withClientStatus) {
		t.Fatal("CREATE status validation accepted a nonempty status")
	}

	containerSecurityValidation := controllerObjectValidationIndex(t, validations, `securityContext.privileged`)
	explicitFalse := controllerJobCELClone(t, object)
	setControllerJobContainerPrivilege(t, explicitFalse, false)
	if !evaluate(containerSecurityValidation, explicitFalse) {
		t.Fatal("container security validation rejected explicit privileged=false")
	}
	explicitTrue := controllerJobCELClone(t, object)
	setControllerJobContainerPrivilege(t, explicitTrue, false)
	containers := controllerJobCELPodSpec(explicitTrue)["containers"].([]any)
	containers[0].(map[string]any)["securityContext"].(map[string]any)["privileged"] = true
	if evaluate(containerSecurityValidation, explicitTrue) {
		t.Fatal("container security validation accepted privileged=true")
	}
}

func TestControllerJobGuardAcceptsCurrentCreateAfterActivation(t *testing.T) {
	t.Parallel()

	object := controllerJobCreateProbe(t, predecessorControllerJobFixture(t), false)
	activation := map[string]any{
		"activeRelease":               int64(1),
		"candidateRelease":            int64(1),
		"previousRelease":             int64(0),
		"activeControllerStateString": "1",
		"activeControllerState":       int64(1),
		"activeControllerImage":       predecessorControllerImage,
	}
	validations, evaluate := controllerJobValidationEvaluator(t, activation)
	for index, validation := range validations {
		if !evaluate(index, object) {
			t.Errorf("Job validation %d rejected the generated current CREATE contract: %s", index, validation.Expression)
		}
	}
}

func TestControllerPlanGuardAcceptsExactContractV2PredecessorCreateDuringBootstrap(t *testing.T) {
	t.Parallel()

	fixtureObject := predecessorControllerPlanProbe(t)
	metadata := fixtureObject["metadata"].(map[string]any)
	spec := fixtureObject["spec"].(map[string]any)
	const probeName = "ptah-plan-eeeeeeeeeeeeeeeeeeeeeeee"
	if metadata["name"] != probeName {
		t.Fatalf("predecessor probe name = %v, want %s", metadata["name"], probeName)
	}
	fingerprint := "sha256:" + strings.Repeat("1", 64)
	if spec["fingerprint"] != fingerprint {
		t.Fatalf("predecessor probe fingerprint = %v, want unchanged %s", spec["fingerprint"], fingerprint)
	}
	if metadata["name"] == "ptah-plan-"+strings.TrimPrefix(fingerprint, "sha256:")[:24] {
		t.Fatal("predecessor probe name still matches its fingerprint and cannot exercise the semantic boundary")
	}
	chunks := spec["chunks"].([]any)
	if got := chunks[0].(map[string]any)["name"]; got != probeName+"-000" {
		t.Fatalf("predecessor probe chunk name = %v, want %s-000", got, probeName)
	}
	if spec["contractVersion"] != int64(2) {
		t.Fatalf("predecessor probe contractVersion = %v, want 2", spec["contractVersion"])
	}
	for _, field := range []string{"controllerImage", "controllerRevision", "controllerStateVersion"} {
		if _, present := spec[field]; present {
			t.Fatalf("contract-v2 predecessor probe unexpectedly contains %s", field)
		}
	}
	status, ok := fixtureObject["status"].(map[string]any)
	if !ok || len(status) != 0 {
		t.Fatalf("typed PtahSchemaPlan fixture status = %#v, want a present empty object", fixtureObject["status"])
	}
	object := controllerPlanCELClone(t, fixtureObject)
	// The custom-resource create strategy removes status before validating
	// admission, including a zero value emitted by the typed fixture producer.
	delete(object, "status")
	if _, present := object["status"]; present {
		t.Fatal("PtahSchemaPlan status survived the modeled create-strategy reset")
	}

	activation := map[string]any{
		"activeRelease":               int64(0),
		"candidateRelease":            int64(1),
		"previousRelease":             int64(0),
		"activeControllerStateString": "1",
		"activeControllerState":       int64(1),
		"activeControllerImage":       predecessorControllerImage,
	}
	validations, evaluate := controllerPlanValidationEvaluator(t, activation)
	for index, validation := range validations {
		if !evaluate(index, object) {
			t.Errorf("PtahSchemaPlan validation %d rejected the exact contract-v2 predecessor CREATE: %s", index, validation.Expression)
		}
	}

	statusValidation := controllerObjectValidationIndex(t, validations, `!has(object.status)`)
	if evaluate(statusValidation, fixtureObject) {
		t.Fatal("PtahSchemaPlan status validation accepted the pre-reset fixture object")
	}
}

func controllerObjectValidationIndex(t *testing.T, validations []admissionregistrationv1.Validation, fragment string) int {
	t.Helper()
	index := -1
	for candidate, validation := range validations {
		if !strings.Contains(validation.Expression, fragment) {
			continue
		}
		if index >= 0 {
			t.Fatalf("multiple controller-object validations contain %q", fragment)
		}
		index = candidate
	}
	if index < 0 {
		t.Fatalf("no controller-object validation contains %q", fragment)
	}
	return index
}

func controllerPlanValidationEvaluator(t *testing.T, activation map[string]any) ([]admissionregistrationv1.Validation, func(int, map[string]any) bool) {
	t.Helper()
	environment, err := celgo.NewEnv(
		celgo.Variable("object", celgo.DynType),
		celgo.Variable("oldObject", celgo.DynType),
		celgo.Variable("request", celgo.DynType),
		celgo.Variable("variables", celgo.DynType),
	)
	if err != nil {
		t.Fatal(err)
	}
	validations := controllerPlanWriteValidations("test")
	evaluate := func(index int, candidate map[string]any) bool {
		t.Helper()
		validation := validations[index]
		ast, issues := environment.Compile(validation.Expression)
		if issues != nil && issues.Err() != nil {
			t.Fatalf("compile PtahSchemaPlan validation %d: %v", index, issues.Err())
		}
		program, programErr := environment.Program(ast)
		if programErr != nil {
			t.Fatalf("build PtahSchemaPlan validation %d: %v", index, programErr)
		}
		result, _, evaluationErr := program.Eval(map[string]any{
			"object":    candidate,
			"oldObject": nil,
			"request":   map[string]any{"operation": "CREATE"},
			"variables": activation,
		})
		if evaluationErr != nil {
			t.Fatalf("evaluate PtahSchemaPlan validation %d: %v", index, evaluationErr)
		}
		allowed, ok := result.Value().(bool)
		if !ok {
			t.Fatalf("PtahSchemaPlan validation %d result = %T(%v), want bool", index, result.Value(), result.Value())
		}
		return allowed
	}
	return validations, evaluate
}

func predecessorControllerPlanProbe(t *testing.T) map[string]any {
	t.Helper()
	digest := func(character string) string { return "sha256:" + strings.Repeat(character, 64) }
	const (
		schemaName = "predecessor-apply"
		schemaUID  = types.UID("schema-uid")
		probeName  = "ptah-plan-eeeeeeeeeeeeeeeeeeeeeeee"
	)
	controller := true
	blockOwnerDeletion := true
	plan := operatorv1alpha1.PtahSchemaPlan{
		TypeMeta: metav1.TypeMeta{
			APIVersion: operatorv1alpha1.GroupVersion.String(),
			Kind:       "PtahSchemaPlan",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a",
			Name:      "ptah-plan-111111111111111111111111",
			Labels:    map[string]string{"operator.ptah.dev/schema": schemaName},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         operatorv1alpha1.GroupVersion.String(),
				Kind:               "PtahSchema",
				Name:               schemaName,
				UID:                schemaUID,
				Controller:         &controller,
				BlockOwnerDeletion: &blockOwnerDeletion,
			}},
		},
		Spec: operatorv1alpha1.PtahSchemaPlanSpec{
			ContractVersion:          2,
			SchemaRef:                operatorv1alpha1.ImmutableObjectReference{Name: schemaName, UID: schemaUID},
			Fingerprint:              digest("1"),
			ContentDigest:            digest("2"),
			Size:                     128,
			ArtifactDigest:           digest("3"),
			CoordinationDigest:       digest("4"),
			TargetIdentityDigest:     digest("5"),
			ActualStateFingerprint:   digest("6"),
			DesiredStateFingerprint:  digest("7"),
			PolicyFingerprint:        digest("8"),
			VerificationPolicyUID:    types.UID("policy-uid"),
			VerificationPolicyDigest: digest("9"),
			ExecutionBindingID:       "v1-" + strings.Repeat("a", 32),
			PtahVersion:              "v0.3.0",
			ExecutorImage:            "registry.example/ptah@" + digest("b"),
			RunnerImage:              "registry.example/operator@" + digest("c"),
			RunnerProtocolVersion:    1,
			Dialect:                  "postgresql",
			Destructive:              false,
			StatementCount:           1,
			Chunks: []operatorv1alpha1.PlanChunkReference{{
				Name:   "ptah-plan-111111111111111111111111-000",
				Key:    "chunk",
				Index:  0,
				Digest: digest("2"),
				Size:   128,
			}},
		},
	}

	// Match the upgrade harness transformation of the predecessor fixture before
	// normalizing the JSON through the operator type and Kubernetes unstructured
	// conversion. This exposes the concrete type's zero-valued status shape.
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]any
	if err := json.Unmarshal(payload, &probe); err != nil {
		t.Fatal(err)
	}
	probe["metadata"].(map[string]any)["name"] = probeName
	chunks := probe["spec"].(map[string]any)["chunks"].([]any)
	chunks[0].(map[string]any)["name"] = probeName + "-000"

	requestJSON, err := json.Marshal(probe)
	if err != nil {
		t.Fatal(err)
	}
	decoded := &operatorv1alpha1.PtahSchemaPlan{}
	if err := json.Unmarshal(requestJSON, decoded); err != nil {
		t.Fatal(err)
	}
	admissionObject, err := runtime.DefaultUnstructuredConverter.ToUnstructured(decoded)
	if err != nil {
		t.Fatal(err)
	}
	return admissionObject
}

func controllerPlanCELClone(t *testing.T, source map[string]any) map[string]any {
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

func controllerJobValidationEvaluator(t *testing.T, activation map[string]any) ([]admissionregistrationv1.Validation, func(int, map[string]any) bool) {
	t.Helper()
	environment, err := celgo.NewEnv(
		celgo.Variable("object", celgo.DynType),
		celgo.Variable("oldObject", celgo.DynType),
		celgo.Variable("request", celgo.DynType),
		celgo.Variable("variables", celgo.DynType),
	)
	if err != nil {
		t.Fatal(err)
	}
	validations := controllerJobWriteValidations("test")
	evaluate := func(index int, candidate map[string]any) bool {
		t.Helper()
		validation := validations[index]
		ast, issues := environment.Compile(validation.Expression)
		if issues != nil && issues.Err() != nil {
			t.Fatalf("compile Job validation %d: %v", index, issues.Err())
		}
		program, programErr := environment.Program(ast)
		if programErr != nil {
			t.Fatalf("build Job validation %d: %v", index, programErr)
		}
		result, _, evaluationErr := program.Eval(map[string]any{
			"object":    candidate,
			"oldObject": nil,
			"request":   map[string]any{"operation": "CREATE"},
			"variables": activation,
		})
		if evaluationErr != nil {
			t.Fatalf("evaluate Job validation %d: %v", index, evaluationErr)
		}
		allowed, ok := result.Value().(bool)
		if !ok {
			t.Fatalf("Job validation %d result = %T(%v), want bool", index, result.Value(), result.Value())
		}
		return allowed
	}
	return validations, evaluate
}

func setControllerJobContainerPrivilege(t *testing.T, object map[string]any, value bool) {
	t.Helper()
	podSpec := controllerJobCELPodSpec(object)
	for _, field := range []string{"containers", "initContainers"} {
		containers, ok := podSpec[field].([]any)
		if !ok {
			t.Fatalf("Job %s = %T, want list", field, podSpec[field])
		}
		for _, item := range containers {
			container, ok := item.(map[string]any)
			if !ok {
				t.Fatalf("Job %s item = %T, want object", field, item)
			}
			securityContext, ok := container["securityContext"].(map[string]any)
			if !ok {
				t.Fatalf("Job %s securityContext = %T, want object", field, container["securityContext"])
			}
			securityContext["privileged"] = value
		}
	}
}

const predecessorControllerImage = "registry.example/manager@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"

func predecessorControllerJobFixture(t *testing.T) *batchv1.Job {
	t.Helper()
	digest := func(character string) string { return "sha256:" + strings.Repeat(character, 64) }
	schema := &operatorv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a",
			Name:      "predecessor-read-only-job",
			UID:       types.UID("schema-uid"),
		},
		Spec: operatorv1alpha1.PtahSchemaSpec{
			Target: operatorv1alpha1.DatabaseTargetSpec{
				Engine:          operatorv1alpha1.DatabaseEnginePostgreSQL,
				CoordinationKey: "prod/team-a/orders-primary",
				URLFrom: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "database"},
					Key:                  "url",
				},
			},
			Desired: operatorv1alpha1.OCIArtifactSourceSpec{
				OCIRef: "oci://registry.example/acme/orders:stable",
			},
			Execution: operatorv1alpha1.ExecutionSpec{
				ActiveDeadlineSeconds: 600,
				ServiceAccountName:    "schema-jobs",
			},
		},
		Status: operatorv1alpha1.PtahSchemaStatus{
			ExecutionBinding: &operatorv1alpha1.ExecutionBindingStatus{
				Epoch:                  "v1-33333333333333333333333333333333",
				ControllerImage:        predecessorControllerImage,
				ControllerRevision:     "controller-test-revision",
				ControllerStateVersion: 1,
				PtahVersion:            "v0.3.0",
				ExecutorImage:          "registry.example/ptah@" + digest("d"),
				RunnerImage:            "registry.example/operator@" + digest("e"),
				RunnerProtocolVersion:  int32(runner.ProtocolVersion),
			},
		},
	}
	operation := operatorv1alpha1.ActiveOperationStatus{
		Type:               operatorv1alpha1.OperationResolve,
		ID:                 "operation-01",
		InputFingerprint:   digest("a"),
		ExecutionBindingID: schema.Status.ExecutionBinding.Epoch,
		StartedAt:          metav1.NewTime(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)),
		Attempt:            1,
		AdmissionSnapshot: &operatorv1alpha1.PodAdmissionSnapshot{
			Digest:         digest("b"),
			TemplateDigest: digest("c"),
		},
	}
	builder := workload.Builder{
		ExecutorImage:          schema.Status.ExecutionBinding.ExecutorImage,
		RunnerImage:            schema.Status.ExecutionBinding.RunnerImage,
		PtahVersion:            schema.Status.ExecutionBinding.PtahVersion,
		ControllerImage:        schema.Status.ExecutionBinding.ControllerImage,
		ControllerRevision:     schema.Status.ExecutionBinding.ControllerRevision,
		ControllerStateVersion: schema.Status.ExecutionBinding.ControllerStateVersion,
	}
	job, err := builder.Build(schema, operation, nil)
	if err != nil {
		t.Fatal(err)
	}
	clientgoscheme.Scheme.Default(job)
	return job
}

func predecessorControllerJobProbe(t *testing.T, job *batchv1.Job) map[string]any {
	t.Helper()
	return controllerJobCreateProbe(t, job, true)
}

func controllerJobCreateProbe(t *testing.T, job *batchv1.Job, stripControllerIdentity bool) map[string]any {
	t.Helper()
	payload, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(payload, &object); err != nil {
		t.Fatal(err)
	}
	metadata := object["metadata"].(map[string]any)
	metadata["name"] = "ptah-resolve-vap-probe-0123456789abcdef"
	for _, field := range []string{
		"creationTimestamp", "deletionGracePeriodSeconds", "deletionTimestamp", "generateName",
		"generation", "managedFields", "resourceVersion", "selfLink", "uid",
	} {
		delete(metadata, field)
	}
	delete(object, "status")
	spec := object["spec"].(map[string]any)
	delete(spec, "selector")
	delete(spec, "ttlSecondsAfterFinished")
	template := spec["template"].(map[string]any)
	templateMetadata := template["metadata"].(map[string]any)
	for _, field := range []string{
		"creationTimestamp", "deletionGracePeriodSeconds", "deletionTimestamp", "generateName",
		"generation", "managedFields", "namespace", "resourceVersion", "selfLink", "uid",
	} {
		delete(templateMetadata, field)
	}
	templateLabels := templateMetadata["labels"].(map[string]any)
	for _, label := range []string{
		"batch.kubernetes.io/controller-uid", "batch.kubernetes.io/job-name", "controller-uid", "job-name",
	} {
		delete(templateLabels, label)
	}
	if stripControllerIdentity {
		for _, annotation := range []string{
			workload.AnnotationControllerImage,
			workload.AnnotationControllerRevision,
			workload.AnnotationControllerStateVersion,
		} {
			delete(metadata["annotations"].(map[string]any), annotation)
			delete(templateMetadata["annotations"].(map[string]any), annotation)
		}
		if got := len(metadata["annotations"].(map[string]any)); got != 5 {
			t.Fatalf("predecessor Job annotation count = %d, want 5", got)
		}
		if got := len(templateMetadata["annotations"].(map[string]any)); got != 5 {
			t.Fatalf("predecessor Pod template annotation count = %d, want 5", got)
		}
	}
	if spec["manualSelector"] != false || spec["completionMode"] != "NonIndexed" || spec["podReplacementPolicy"] != "Failed" {
		t.Fatalf("predecessor Job defaults are not explicit: %#v", spec)
	}
	if got := len(templateLabels); got != 5 {
		t.Fatalf("predecessor Pod template label count = %d, want 5", got)
	}

	// Admission CEL receives the built-in object after typed decoding. The
	// built-in Job status is a value struct, so conversion back to unstructured
	// data exposes the API-produced empty status even when the request omitted it.
	sanitized, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	decoded := &batchv1.Job{}
	if err := json.Unmarshal(sanitized, decoded); err != nil {
		t.Fatal(err)
	}
	admissionObject, err := runtime.DefaultUnstructuredConverter.ToUnstructured(decoded)
	if err != nil {
		t.Fatal(err)
	}
	return admissionObject
}
