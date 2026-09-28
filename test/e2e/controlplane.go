package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The admission singleton's configurations are named for the chart, not for
// the release.
const admissionConfiguration = "ptah-operator-admission"

// controllerRevisionOK holds the revision the manager records to what a plan
// can carry: at most 128 bytes, no control character, and no whitespace at
// either edge.
func controllerRevisionOK(revision string) error {
	switch {
	case len(revision) > 128:
		return errors.New("E2E_CONTROLLER_REVISION must be at most 128 bytes")
	case strings.IndexFunc(revision, isControl) >= 0:
		return errors.New("E2E_CONTROLLER_REVISION must not contain control characters")
	case revision == "" || isSpace(rune(revision[0])) || isSpace(rune(revision[len(revision)-1])):
		return errors.New("E2E_CONTROLLER_REVISION must not be empty or have edge whitespace")
	}
	return nil
}

// isControl is the C locale's [:cntrl:], which tr -d removed.
func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f
}

// isSpace is the C locale's [:space:].
func isSpace(r rune) bool {
	return strings.ContainsRune(" \t\n\v\f\r", r)
}

var (
	digestPinnedImage = regexp.MustCompile(`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`)
	positiveInteger   = regexp.MustCompile(`^[1-9][0-9]*$`)
)

// controlPlaneInputsOK is the refusal the phase gives before it reads the
// cluster: a revision the plan cannot carry, an image not pinned by digest, a
// state version that is not a positive integer, or one namespace given twice.
func controlPlaneInputsOK(revision, image, stateVersion, testNamespace, foreignNamespace string) error {
	if err := controllerRevisionOK(revision); err != nil {
		return err
	}
	if !digestPinnedImage.MatchString(image) {
		return errors.New("E2E_CONTROLLER_IMAGE must be pinned by a lowercase SHA-256 digest")
	}
	if !positiveInteger.MatchString(stateVersion) {
		return errors.New("E2E_CONTROLLER_STATE_VERSION must be a positive integer")
	}
	if testNamespace == foreignNamespace {
		return errors.New("E2E_TEST_NAMESPACE and E2E_FOREIGN_NAMESPACE must differ")
	}
	return nil
}

// edgeRunnerProtocolVersion is the runner protocol support/ptah.json records
// for the release under test. hack/verifyptahsupport holds that record to
// runner.ProtocolVersion, so the phase reads it rather than writing the number
// down a second time.
func edgeRunnerProtocolVersion(catalog []byte) (int64, error) {
	var document struct {
		Releases []struct {
			Operator string `json:"operator"`
			Verified []struct {
				RunnerProtocolVersion json.RawMessage `json:"runnerProtocolVersion"`
			} `json:"verified"`
		} `json:"releases"`
	}
	if err := json.Unmarshal(catalog, &document); err != nil {
		return 0, fmt.Errorf("support/ptah.json does not parse: %w", err)
	}
	versions := map[string]bool{}
	for _, release := range document.Releases {
		if release.Operator != "edge" {
			continue
		}
		for _, verified := range release.Verified {
			versions[string(bytes.TrimSpace(verified.RunnerProtocolVersion))] = true
		}
	}
	refusal := errors.New("support/ptah.json must record exactly one runner protocol version for edge")
	if len(versions) != 1 {
		return 0, refusal
	}
	value := slices.Collect(maps.Keys(versions))[0]
	version, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, refusal
	}
	return version, nil
}

// canonicalJSON is jq -c of a document whose keys are in the order the
// struct declares them: no HTML escaping, no trailing newline. The digests
// below hash these bytes, and the webhook hashes the same.
func canonicalJSON(document any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(document); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

func sha256Digest(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// coordinationDigest is the realm a coordination key names, derived as
// internal/fingerprint derives it: the canonical engine, the namespace the
// resource lives in and the key, in that field order. The approval webhook
// recomputes it from the schema's spec and refuses a plan bound to anything
// else, so a literal would go stale the day the derivation moves.
func coordinationDigest(engine, namespace, key string) (string, error) {
	canonical, err := canonicalJSON(struct {
		ContractVersion int    `json:"contract_version"`
		Engine          string `json:"engine"`
		Namespace       string `json:"namespace"`
		CoordinationKey string `json:"coordination_key"`
	}{1, engine, namespace, key})
	if err != nil {
		return "", err
	}
	return sha256Digest(canonical), nil
}

// planBinding is what a plan's fingerprint covers, in the order the
// fingerprint serializes it.
type planBinding struct {
	ContractVersion          int      `json:"contract_version"`
	SchemaUID                string   `json:"schema_uid"`
	PlanContentDigest        string   `json:"plan_content_digest"`
	ArtifactDigest           string   `json:"artifact_digest"`
	CoordinationDigest       string   `json:"coordination_digest"`
	TargetIdentityDigest     string   `json:"target_identity_digest"`
	ActualStateFingerprint   string   `json:"actual_state_fingerprint"`
	DesiredStateFingerprint  string   `json:"desired_state_fingerprint"`
	PolicyFingerprint        string   `json:"policy_fingerprint"`
	VerificationPolicyUID    string   `json:"verification_policy_uid"`
	VerificationPolicyDigest string   `json:"verification_policy_digest"`
	ExecutionBindingID       string   `json:"execution_binding_id"`
	ControllerStateVersion   int64    `json:"controller_state_version"`
	PtahVersion              string   `json:"ptah_version"`
	ExecutorImage            string   `json:"executor_image"`
	RunnerProtocolVersion    int64    `json:"runner_protocol_version"`
	Destructive              bool     `json:"destructive"`
	PrivilegeChanges         []string `json:"privilege_changes"`
	StatementCount           int      `json:"statement_count"`
}

func (binding planBinding) fingerprint() (string, error) {
	if binding.PrivilegeChanges == nil {
		binding.PrivilegeChanges = []string{}
	}
	canonical, err := canonicalJSON(binding)
	if err != nil {
		return "", err
	}
	return sha256Digest(canonical), nil
}

// emptySelector is what `(.selector // {}) == {}` accepts: no selector, or
// one that names nothing at all.
func emptySelector(selector *metav1.LabelSelector) bool {
	return selector == nil || (selector.MatchLabels == nil && selector.MatchExpressions == nil)
}

// admissionEntry is the part of a mutating or validating webhook the shape
// checks read, so one check serves both kinds.
type admissionEntry struct {
	name               string
	client             admissionregistrationv1.WebhookClientConfig
	rules              []admissionregistrationv1.RuleWithOperations
	failurePolicy      *admissionregistrationv1.FailurePolicyType
	matchPolicy        *admissionregistrationv1.MatchPolicyType
	sideEffects        *admissionregistrationv1.SideEffectClass
	timeoutSeconds     *int32
	namespaceSelector  *metav1.LabelSelector
	objectSelector     *metav1.LabelSelector
	matchConditions    []admissionregistrationv1.MatchCondition
	reviewVersions     []string
	reinvocationPolicy *admissionregistrationv1.ReinvocationPolicyType
}

func mutatingAdmissionEntries(configuration *admissionregistrationv1.MutatingWebhookConfiguration) []admissionEntry {
	entries := make([]admissionEntry, 0, len(configuration.Webhooks))
	for _, webhook := range configuration.Webhooks {
		entries = append(entries, admissionEntry{
			name: webhook.Name, client: webhook.ClientConfig, rules: webhook.Rules,
			failurePolicy: webhook.FailurePolicy, matchPolicy: webhook.MatchPolicy,
			sideEffects: webhook.SideEffects, timeoutSeconds: webhook.TimeoutSeconds,
			namespaceSelector: webhook.NamespaceSelector, objectSelector: webhook.ObjectSelector,
			matchConditions: webhook.MatchConditions, reviewVersions: webhook.AdmissionReviewVersions,
			reinvocationPolicy: webhook.ReinvocationPolicy,
		})
	}
	return entries
}

func validatingAdmissionEntries(configuration *admissionregistrationv1.ValidatingWebhookConfiguration) []admissionEntry {
	entries := make([]admissionEntry, 0, len(configuration.Webhooks))
	for _, webhook := range configuration.Webhooks {
		entries = append(entries, admissionEntry{
			name: webhook.Name, client: webhook.ClientConfig, rules: webhook.Rules,
			failurePolicy: webhook.FailurePolicy, matchPolicy: webhook.MatchPolicy,
			sideEffects: webhook.SideEffects, timeoutSeconds: webhook.TimeoutSeconds,
			namespaceSelector: webhook.NamespaceSelector, objectSelector: webhook.ObjectSelector,
			matchConditions: webhook.MatchConditions, reviewVersions: webhook.AdmissionReviewVersions,
		})
	}
	return entries
}

func pointer[T any](value T) *T {
	return &value
}

func equalPointer[T comparable](value *T, want T) bool {
	return value != nil && *value == want
}

// entryShape is one entry's expected shape.
type entryShape struct {
	matchPolicy     admissionregistrationv1.MatchPolicyType
	timeoutSeconds  int32
	reinvocation    *admissionregistrationv1.ReinvocationPolicyType
	matchConditions func([]admissionregistrationv1.MatchCondition) bool
	service         admissionregistrationv1.ServiceReference
	rules           []admissionregistrationv1.RuleWithOperations
}

// entryMismatch names the first way the entry differs from the shape, or
// returns "" when it has it exactly.
func entryMismatch(entry admissionEntry, shape entryShape) string {
	switch {
	case !equalPointer(entry.failurePolicy, admissionregistrationv1.Fail):
		return "failurePolicy is not Fail"
	case !equalPointer(entry.sideEffects, admissionregistrationv1.SideEffectClassNone):
		return "sideEffects is not None"
	case !equalPointer(entry.matchPolicy, shape.matchPolicy):
		return fmt.Sprintf("matchPolicy is not %s", shape.matchPolicy)
	case shape.reinvocation != nil && !equalPointer(entry.reinvocationPolicy, *shape.reinvocation):
		return fmt.Sprintf("reinvocationPolicy is not %s", *shape.reinvocation)
	case !equalPointer(entry.timeoutSeconds, shape.timeoutSeconds):
		return fmt.Sprintf("timeoutSeconds is not %d", shape.timeoutSeconds)
	case !emptySelector(entry.namespaceSelector):
		return "it has a namespaceSelector"
	case !emptySelector(entry.objectSelector):
		return "it has an objectSelector"
	case !shape.matchConditions(entry.matchConditions):
		return "its matchConditions are not the expected ones"
	case !slices.Equal(entry.reviewVersions, []string{"v1"}):
		return "admissionReviewVersions is not [v1]"
	case len(entry.client.CABundle) == 0:
		return "it carries no caBundle"
	case entry.client.URL != nil:
		return "it calls a URL"
	case entry.client.Service == nil || !reflect.DeepEqual(*entry.client.Service, shape.service):
		return "it does not call exactly the webhook Service path on port 443"
	case !reflect.DeepEqual(entry.rules, shape.rules):
		return "its rules are not exact"
	}
	return ""
}

func noMatchConditions(conditions []admissionregistrationv1.MatchCondition) bool {
	return len(conditions) == 0
}

func serviceReference(namespace, name, path string) admissionregistrationv1.ServiceReference {
	return admissionregistrationv1.ServiceReference{Namespace: namespace, Name: name, Path: &path, Port: pointer[int32](443)}
}

func namespacedRule(group, version string, operations []admissionregistrationv1.OperationType, resources ...string) admissionregistrationv1.RuleWithOperations {
	return admissionregistrationv1.RuleWithOperations{
		Operations: operations,
		Rule: admissionregistrationv1.Rule{
			APIGroups: []string{group}, APIVersions: []string{version}, Resources: resources,
			Scope: pointer(admissionregistrationv1.NamespacedScope),
		},
	}
}

var (
	createOnly      = []admissionregistrationv1.OperationType{admissionregistrationv1.Create}
	createAndUpdate = []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update}
)

// mutatingAdmissionExact holds the mutating singleton to its exact,
// fail-closed shape: exactly the approval entries, and the spec-writer
// entries when the release turned the four-eyes control on, each calling its
// own path with nothing that could narrow what it sees.
func mutatingAdmissionExact(configuration *admissionregistrationv1.MutatingWebhookConfiguration,
	namespace, service string, requireDistinctApprover bool,
) error {
	entries := mutatingAdmissionEntries(configuration)
	want := []string{"mapproval.operator.ptah.run", "mmigrationapproval.operator.ptah.run"}
	if requireDistinctApprover {
		want = append(want, "mmigrationwriter.operator.ptah.run", "mschemawriter.operator.ptah.run")
	}
	if err := entryNames(entries, want); err != nil {
		return err
	}
	never := admissionregistrationv1.NeverReinvocationPolicy
	shapes := map[string]entryShape{
		"mapproval.operator.ptah.run": {
			service: serviceReference(namespace, service, "/mutate-operator-ptah-run-v1alpha1-ptahschemaapproval"),
			rules:   []admissionregistrationv1.RuleWithOperations{namespacedRule("operator.ptah.run", "v1alpha1", createOnly, "ptahschemaapprovals")},
		},
	}
	if requireDistinctApprover {
		shapes["mschemawriter.operator.ptah.run"] = entryShape{
			service: serviceReference(namespace, service, "/mutate-operator-ptah-run-v1alpha1-ptahschema"),
			rules:   []admissionregistrationv1.RuleWithOperations{namespacedRule("operator.ptah.run", "v1alpha1", createAndUpdate, "ptahschemas")},
		}
		shapes["mmigrationwriter.operator.ptah.run"] = entryShape{
			service: serviceReference(namespace, service, "/mutate-operator-ptah-run-v1alpha1-ptahmigration"),
			rules:   []admissionregistrationv1.RuleWithOperations{namespacedRule("operator.ptah.run", "v1alpha1", createAndUpdate, "ptahmigrations")},
		}
	}
	for _, entry := range entries {
		shape, checked := shapes[entry.name]
		if !checked {
			continue
		}
		shape.matchPolicy, shape.timeoutSeconds, shape.reinvocation = admissionregistrationv1.Equivalent, 5, &never
		shape.matchConditions = noMatchConditions
		if mismatch := entryMismatch(entry, shape); mismatch != "" {
			return fmt.Errorf("%s: %s", entry.name, mismatch)
		}
	}
	return nil
}

// validatingAdmissionExact holds the validating singleton to its exact,
// fail-closed shape. The Pod intent entry is narrowed by one match condition,
// written here from its parts, and the controller-write entry by the
// manager's identity.
func validatingAdmissionExact(configuration *admissionregistrationv1.ValidatingWebhookConfiguration,
	namespace, service, controllerUser string,
) error {
	entries := validatingAdmissionEntries(configuration)
	if err := entryNames(entries, []string{
		"vapproval.operator.ptah.run", "vcontrollerwrite.operator.ptah.run",
		"vmigrationapproval.operator.ptah.run", "vpodintent.operator.ptah.run",
	}); err != nil {
		return err
	}
	shapes := map[string]entryShape{
		"vapproval.operator.ptah.run": {
			matchPolicy: admissionregistrationv1.Equivalent, timeoutSeconds: 5, matchConditions: noMatchConditions,
			service: serviceReference(namespace, service, "/validate-operator-ptah-run-v1alpha1-ptahschemaapproval"),
			rules:   []admissionregistrationv1.RuleWithOperations{namespacedRule("operator.ptah.run", "v1alpha1", createAndUpdate, "ptahschemaapprovals")},
		},
		"vpodintent.operator.ptah.run": {
			matchPolicy: admissionregistrationv1.Equivalent, timeoutSeconds: 5,
			matchConditions: func(conditions []admissionregistrationv1.MatchCondition) bool {
				return len(conditions) == 1 && conditions[0].Name == "managed-or-operation-job-pod" &&
					collapseWhitespace(conditions[0].Expression) == operationPodCondition()
			},
			service: serviceReference(namespace, service, "/validate-v1-pod-ptah-operation-intent"),
			rules: []admissionregistrationv1.RuleWithOperations{
				namespacedRule("", "v1", createAndUpdate, "pods", "pods/ephemeralcontainers", "pods/resize"),
			},
		},
		"vcontrollerwrite.operator.ptah.run": {
			matchPolicy: admissionregistrationv1.Exact, timeoutSeconds: 30,
			matchConditions: func(conditions []admissionregistrationv1.MatchCondition) bool {
				return reflect.DeepEqual(conditions, []admissionregistrationv1.MatchCondition{{
					Name:       "controller-service-account",
					Expression: "request.userInfo.username == '" + controllerUser + "'",
				}})
			},
			service: serviceReference(namespace, service, "/validate-operator-controller-write"),
			rules: []admissionregistrationv1.RuleWithOperations{
				namespacedRule("batch", "v1", createAndUpdate, "jobs"),
				namespacedRule("", "v1", createOnly, "configmaps"),
				namespacedRule("operator.ptah.run", "v1alpha1", createOnly,
					"ptahschemaplans", "ptahschemaplanchunks", "ptahmigrationplans"),
			},
		},
	}
	for _, name := range []string{"vapproval.operator.ptah.run", "vpodintent.operator.ptah.run"} {
		count := 0
		for _, entry := range entries {
			if entry.name == name {
				count++
			}
		}
		if count != 1 {
			return fmt.Errorf("%s appears %d times, want once", name, count)
		}
	}
	for _, entry := range entries {
		shape, checked := shapes[entry.name]
		if !checked {
			continue
		}
		if mismatch := entryMismatch(entry, shape); mismatch != "" {
			return fmt.Errorf("%s: %s", entry.name, mismatch)
		}
	}
	return nil
}

func entryNames(entries []admissionEntry, want []string) error {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.name)
	}
	slices.Sort(names)
	want = slices.Sorted(slices.Values(want))
	if !slices.Equal(names, want) {
		return fmt.Errorf("entries are %v, want exactly %v", names, want)
	}
	return nil
}

var whitespaceRun = regexp.MustCompile(`\s+`)

// collapseWhitespace is jq's gsub("\\s+"; " ").
func collapseWhitespace(expression string) string {
	return whitespaceRun.ReplaceAllString(expression, " ")
}

// operationPodCondition is the Pod intent entry's match condition, built from
// its parts: a Pod the operator's labels name, or one a Job of an operation
// controls, and on an update the same of the Pod it replaces.
func operationPodCondition() string {
	labels := func(object string) string {
		return "(has(" + object + ".metadata.labels) && " +
			"'app.kubernetes.io/managed-by' in " + object + ".metadata.labels && " +
			object + ".metadata.labels['app.kubernetes.io/managed-by'] == 'ptah-operator' && " +
			"'app.kubernetes.io/component' in " + object + ".metadata.labels && " +
			object + ".metadata.labels['app.kubernetes.io/component'] in ['schema-operation', 'migration-operation'])"
	}
	owner := func(object string) string {
		return "(has(" + object + ".metadata.ownerReferences) && " +
			object + ".metadata.ownerReferences.exists(ref, ref.apiVersion == 'batch/v1'" +
			" && ref.kind == 'Job' && ref.controller == true && ref.name.matches(" +
			"'^ptah-(m-)?(resolve|verify|observe|plan|history|apply)-')))"
	}
	return labels("object") + " || " + owner("object") +
		" || (request.operation == 'UPDATE' && oldObject != null && ( " +
		labels("oldObject") + " || " + owner("oldObject") + "))"
}

// crdSpecProperties is the v1alpha1 spec's properties of a CRD.
func crdSpecProperties(crd *apiextensionsv1.CustomResourceDefinition) (map[string]apiextensionsv1.JSONSchemaProps, error) {
	for _, version := range crd.Spec.Versions {
		if version.Name != "v1alpha1" || version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
			continue
		}
		spec, found := version.Schema.OpenAPIV3Schema.Properties["spec"]
		if !found {
			return nil, fmt.Errorf("%s v1alpha1 declares no spec", crd.Name)
		}
		return spec.Properties, nil
	}
	return nil, fmt.Errorf("%s serves no v1alpha1 schema", crd.Name)
}

// referenceHasNoNamespace fails unless the property path leads to an object
// whose properties do not include namespace.
func referenceHasNoNamespace(properties map[string]apiextensionsv1.JSONSchemaProps, path ...string) error {
	current := properties
	for _, name := range path {
		property, found := current[name]
		if !found {
			return fmt.Errorf("spec.%s is not in the schema", strings.Join(path, "."))
		}
		current = property.Properties
	}
	if _, has := current["namespace"]; has {
		return fmt.Errorf("spec.%s can name another namespace", strings.Join(path, "."))
	}
	return nil
}

// schemaReferencesLocal fails when a PtahSchema can name a Secret or a
// ConfigMap in another namespace.
func schemaReferencesLocal(crd *apiextensionsv1.CustomResourceDefinition) error {
	properties, err := crdSpecProperties(crd)
	if err != nil {
		return err
	}
	for _, path := range [][]string{
		{"target", "urlFrom"},
		{"desired", "verificationPolicyFrom"},
		{"desired", "registryAuthFrom"},
	} {
		if err := referenceHasNoNamespace(properties, path...); err != nil {
			return err
		}
	}
	return nil
}

// approvalReferencesLocal fails when a PtahSchemaApproval can name a schema or
// a plan in another namespace.
func approvalReferencesLocal(crd *apiextensionsv1.CustomResourceDefinition) error {
	properties, err := crdSpecProperties(crd)
	if err != nil {
		return err
	}
	for _, path := range [][]string{{"schemaRef"}, {"planRef"}} {
		if err := referenceHasNoNamespace(properties, path...); err != nil {
			return err
		}
	}
	return nil
}

// anyReadyEndpoint reports whether any endpoint of the slices is ready.
func anyReadyEndpoint(endpointSlices []discoveryv1.EndpointSlice) bool {
	for _, slice := range endpointSlices {
		for _, endpoint := range slice.Endpoints {
			if endpoint.Conditions.Ready != nil && *endpoint.Conditions.Ready {
				return true
			}
		}
	}
	return false
}

// singleApplyManager is the one field manager that applied the Deployment
// server-side. Helm 4 applies server-side, and restoring the Deployment as
// any other manager would leave its fields to that manager and conflict with
// the next upgrade, so none or several is a refusal rather than a guess.
func singleApplyManager(entries []metav1.ManagedFieldsEntry) (string, error) {
	managers := map[string]bool{}
	for _, entry := range entries {
		if entry.Operation == metav1.ManagedFieldsOperationApply {
			managers[entry.Manager] = true
		}
	}
	if len(managers) != 1 {
		return "", fmt.Errorf("it has %d server-side apply field managers, want exactly one: %v",
			len(managers), slices.Sorted(maps.Keys(managers)))
	}
	return slices.Collect(maps.Keys(managers))[0], nil
}

// restorableDeployment is the Deployment as the API server returned it,
// without what the server owns: the identity, the generation, the field
// management, the revision annotation the Deployment controller writes, and
// the status. Applying it recreates the Deployment the phase removed.
func restorableDeployment(live *unstructured.Unstructured) *unstructured.Unstructured {
	restored := live.DeepCopy()
	for _, field := range []string{"creationTimestamp", "generation", "managedFields", "resourceVersion", "uid"} {
		unstructured.RemoveNestedField(restored.Object, "metadata", field)
	}
	unstructured.RemoveNestedField(restored.Object, "metadata", "annotations", "deployment.kubernetes.io/revision")
	unstructured.RemoveNestedField(restored.Object, "status")
	return restored
}

// ownedPods is the Pods a Job with the UID controls.
func ownedPods(pods []corev1.Pod, jobUID types.UID) []corev1.Pod {
	var owned []corev1.Pod
	for _, pod := range pods {
		if controlledByJob(pod.OwnerReferences, jobUID) {
			owned = append(owned, pod)
		}
	}
	return owned
}

func controlledByJob(references []metav1.OwnerReference, jobUID types.UID) bool {
	for _, reference := range references {
		if reference.APIVersion == "batch/v1" && reference.Kind == "Job" && reference.UID == jobUID &&
			reference.Controller != nil && *reference.Controller {
			return true
		}
	}
	return false
}

// referencesJob reports whether any owner reference names the Job's UID, as a
// controller or not.
func referencesJob(pods []corev1.Pod, jobUID types.UID) bool {
	for _, pod := range pods {
		for _, reference := range pod.OwnerReferences {
			if reference.UID == jobUID {
				return true
			}
		}
	}
	return false
}

// podCreationRefused reports whether an Event says the Pod intent webhook
// refused a Pod of the Job for the reason the pattern names.
func podCreationRefused(events []corev1.Event, jobUID types.UID, reason *regexp.Regexp) bool {
	for _, event := range events {
		if event.InvolvedObject.UID == jobUID && event.Reason == "FailedCreate" &&
			strings.Contains(event.Message, "vpodintent.operator.ptah.run") && reason.MatchString(event.Message) {
			return true
		}
	}
	return false
}

// controllerImageArgument is the manager's one --controller-image argument.
func controllerImageArgument(deployment *appsv1.Deployment) (string, error) {
	var images []string
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name != "manager" {
			continue
		}
		for _, argument := range container.Args {
			if image, found := strings.CutPrefix(argument, "--controller-image="); found {
				images = append(images, image)
			}
		}
	}
	if len(images) != 1 {
		return "", fmt.Errorf("manager must have exactly one --controller-image argument, has %d", len(images))
	}
	return images[0], nil
}

// firstRotatorArgument is the value of the first --name= among the first
// container's arguments, which may be empty; false when there is none.
func firstRotatorArgument(deployment *appsv1.Deployment, name string) (string, bool) {
	if len(deployment.Spec.Template.Spec.Containers) == 0 {
		return "", false
	}
	for _, argument := range deployment.Spec.Template.Spec.Containers[0].Args {
		if value, found := strings.CutPrefix(argument, "--"+name+"="); found {
			return value, true
		}
	}
	return "", false
}

// unsupportedEngineSettled is the explicit status an engine the operator does
// not support reaches for its current generation: Blocked, with nothing
// planned, claimed or observed, and both conditions naming the engine.
func unsupportedEngineSettled(schema *ptahv1alpha1.PtahSchema) bool {
	generation := schema.Generation
	status := schema.Status
	return status.ObservedGeneration == generation &&
		status.Phase == "Blocked" &&
		status.Plan == nil && status.ActiveOperation == nil && status.PendingObservation == nil &&
		hasCondition(status.Conditions, "EngineSupported", metav1.ConditionFalse, "UnsupportedEngine", generation) &&
		hasCondition(status.Conditions, "Ready", metav1.ConditionFalse, "UnsupportedEngine", generation)
}

func hasCondition(conditions []metav1.Condition, kind string, status metav1.ConditionStatus, reason string, generation int64) bool {
	for _, condition := range conditions {
		if condition.Type == kind && condition.Status == status && condition.Reason == reason &&
			condition.ObservedGeneration == generation {
			return true
		}
	}
	return false
}

// schemaDefaultsPersisted holds the stored spec of a schema that omitted its
// policy and execution to the defaults the API server wrote, as written.
func schemaDefaultsPersisted(schema *unstructured.Unstructured) error {
	for _, want := range []struct {
		path  []string
		value any
	}{
		{[]string{"spec", "interval"}, "10m"},
		{[]string{"spec", "policy", "apply"}, "OnApproval"},
		{[]string{"spec", "policy", "allowDestructive"}, false},
		{[]string{"spec", "policy", "driftSeverity"}, "all"},
		{[]string{"spec", "policy", "lockTimeout"}, "30s"},
		{[]string{"spec", "policy", "transactionMode"}, "file"},
		{[]string{"spec", "execution", "activeDeadlineSeconds"}, int64(900)},
		{[]string{"spec", "execution", "failureRetryInterval"}, "30s"},
		{[]string{"spec", "execution", "connectTimeout"}, "10s"},
	} {
		value, found, err := unstructured.NestedFieldNoCopy(schema.Object, want.path...)
		if err != nil || !found || !reflect.DeepEqual(value, want.value) {
			return fmt.Errorf("%s is %v, want %v", strings.Join(want.path, "."), value, want.value)
		}
	}
	return nil
}

var executionEpoch = regexp.MustCompile(`^v1-[0-9a-f]{32}$`)

// executionBindingExact holds status.executionBinding to exactly the five
// components that decide what a plan means when it runs, each the value this
// release installed, and returns its epoch. Nothing that names the manager's
// own release is in it: a manager that changes only its image, its revision
// or its runner keeps the epoch.
func executionBindingExact(schema *unstructured.Unstructured, runnerProtocolVersion, controllerStateVersion int64,
	ptahVersion, executorImage string,
) (string, error) {
	binding, found, err := unstructured.NestedMap(schema.Object, "status", "executionBinding")
	if err != nil || !found {
		return "", errors.New("status.executionBinding is absent")
	}
	if keys := slices.Sorted(maps.Keys(binding)); !slices.Equal(keys,
		[]string{"controllerStateVersion", "epoch", "executorImage", "ptahVersion", "runnerProtocolVersion"}) {
		return "", fmt.Errorf("status.executionBinding carries %v", keys)
	}
	epoch, _ := binding["epoch"].(string)
	switch {
	case !executionEpoch.MatchString(epoch):
		return "", fmt.Errorf("epoch %q is not v1- and 32 hex digits", epoch)
	case !reflect.DeepEqual(binding["controllerStateVersion"], controllerStateVersion):
		return "", fmt.Errorf("controllerStateVersion is %v, want %d", binding["controllerStateVersion"], controllerStateVersion)
	case binding["ptahVersion"] != ptahVersion:
		return "", fmt.Errorf("ptahVersion is %v, want %s", binding["ptahVersion"], ptahVersion)
	case binding["executorImage"] != executorImage:
		return "", fmt.Errorf("executorImage is %v, want %s", binding["executorImage"], executorImage)
	case !reflect.DeepEqual(binding["runnerProtocolVersion"], runnerProtocolVersion):
		return "", fmt.Errorf("runnerProtocolVersion is %v, want %d", binding["runnerProtocolVersion"], runnerProtocolVersion)
	}
	return epoch, nil
}

// approvalStampedExact holds a stored approval to the decision as written --
// schema, plan and the plan's fingerprint -- plus who made it and when, and
// nothing else: the plan's own bindings stay on the plan.
func approvalStampedExact(approval *unstructured.Unstructured, schema, plan ptahv1alpha1.ImmutableObjectReference,
	fingerprint string,
) error {
	spec, found, err := unstructured.NestedMap(approval.Object, "spec")
	if err != nil || !found {
		return errors.New("it has no spec")
	}
	if keys := slices.Sorted(maps.Keys(spec)); !slices.Equal(keys,
		[]string{"approvedAt", "approver", "mutationRequestUID", "planFingerprint", "planRef", "schemaRef"}) {
		return fmt.Errorf("its spec carries %v", keys)
	}
	username, _, _ := unstructured.NestedString(spec, "approver", "username")
	requestUID, _, _ := unstructured.NestedString(spec, "mutationRequestUID")
	switch {
	case username == "":
		return errors.New("no approver was stamped")
	case spec["approvedAt"] == nil:
		return errors.New("no approval time was stamped")
	case requestUID == "":
		return errors.New("no mutation request was stamped")
	case !reflect.DeepEqual(spec["schemaRef"], map[string]any{"name": schema.Name, "uid": string(schema.UID)}):
		return fmt.Errorf("schemaRef is %v", spec["schemaRef"])
	case !reflect.DeepEqual(spec["planRef"], map[string]any{"name": plan.Name, "uid": string(plan.UID)}):
		return fmt.Errorf("planRef is %v", spec["planRef"])
	case spec["planFingerprint"] != fingerprint:
		return fmt.Errorf("planFingerprint is %v", spec["planFingerprint"])
	}
	return nil
}

// set writes value at path in a JSON document, making the objects on the way,
// as jq's assignment does.
func set(document map[string]any, value any, path ...string) {
	if err := unstructured.SetNestedField(document, runtime.DeepCopyJSONValue(value), path...); err != nil {
		panic(err)
	}
}

func deepCopyMap(document map[string]any) map[string]any {
	return runtime.DeepCopyJSON(document)
}

func base64String(value string) string {
	return base64.StdEncoding.EncodeToString([]byte(value))
}

// foreignPlanStable holds the plan in the other namespace to the object the
// phase created: the same UID, not being deleted, and nothing that ties it to
// a schema.
func foreignPlanStable(plan *ptahv1alpha1.PtahSchemaPlan, uid types.UID) bool {
	return plan.UID == uid && plan.DeletionTimestamp == nil &&
		len(plan.OwnerReferences) == 0 && len(plan.Finalizers) == 0
}
