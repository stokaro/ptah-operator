package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
)

// The refusals the guard proofs hold the API server to, each the text the
// admission step that refuses writes, so a refusal for any other reason
// cannot pass a row.
const (
	// lifecycleGuardWriteDenial is the webhook's refusal of a manager write to
	// a PtahSchema's desired state.
	lifecycleGuardWriteDenial = "Ptah controller write guard rejected a desired-state mutation"
	// lifecycleGuardJobVAPDenial is the controller-object policy's refusal of
	// a Job shape the manager may not create.
	lifecycleGuardJobVAPDenial = "Ptah controller Job write guard rejected an unsafe workload shape"
	// lifecycleGuardJobSemanticBoundary is the webhook's refusal of a Job that
	// passed the policy and matches no operation a schema is about to start.
	lifecycleGuardJobSemanticBoundary = "Job does not match a not-yet-created active operation"
	// lifecycleGuardDirectWriteRefusal is the uncached webhook's refusal of a
	// plan projection or chunk no persisted plan names.
	lifecycleGuardDirectWriteRefusal = "directly read plan manifest"
	// lifecycleGuardActiveOperationFinalizer is the one finalizer the manager
	// may add to a schema.
	lifecycleGuardActiveOperationFinalizer = "operator.ptah.run/active-operation"
	// lifecycleGuardOwnerConfigMap is CONTROLLER_GUARD_OWNER, the ConfigMap
	// whose owner reference the manager may not add.
	lifecycleGuardOwnerConfigMap = "controller-write-guard-owner"
	// lifecycleGuardAdmissionSingleton is the webhook configuration pair the
	// runtime refuses to serve unless it is whole and this release's.
	lifecycleGuardAdmissionSingleton = "ptah-operator-admission"
	// lifecycleGuardApprovalWebhook is the webhook the validating
	// configuration renders first.
	lifecycleGuardApprovalWebhook = "vapproval.operator.ptah.run"
	// lifecycleGuardPlanName is the plan the direct-write probes name, which
	// no persisted plan is.
	lifecycleGuardPlanName = "ptah-plan-111111111111111111111111"
)

// lifecycleGuardWebhookUnreachable is what the API server says when it could
// not reach the webhook at all, which the baseline probe waits out: any
// refusal that arrives is the answer under test.
var lifecycleGuardWebhookUnreachable = regexp.MustCompile(`failed calling webhook|no endpoints available|connection refused|service unavailable`)

// lifecycleGuardOtherReleaseImage is a manager image no release of this chart
// runs.
var lifecycleGuardOtherReleaseImage = "registry.invalid/ptah-operator@sha256:" + strings.Repeat("0", 64)

// lifecycleGuardFieldProbe is one version-specific field a Kubernetes minor
// adds to a Job or its Pod template that the manager must never set: the API
// server has to keep it (so the refusal is the policy's, not a dropped field),
// and the controller-object policy has to refuse it.
type lifecycleGuardFieldProbe struct {
	// field names the API field, as the proof reports it.
	field string
	// mutate sets the field on the baseline manifest.
	mutate func(manifest map[string]any) error
	// retained reads the API server's dry-run answer for the field as set.
	retained func(response map[string]any) bool
}

// lifecycleGuardFieldProbeTable is every supported Kubernetes minor with the
// guarded fields that minor adds, in the order the proof sends them. A minor
// with no entry is one nobody has decided about yet, and the proof refuses to
// run on it rather than skip its probes; an empty entry is the decision that
// the minor adds nothing to probe. TestLifecycleGuardProbeTableCoversTheSupportWindow
// holds the keys to support/kubernetes.json.
var lifecycleGuardFieldProbeTable = map[string][]lifecycleGuardFieldProbe{
	"1.35": {
		{
			field: "PodSpec.workloadRef",
			mutate: func(manifest map[string]any) error {
				return lifecycleGuardSet(manifest, map[string]any{"name": "probe", "podGroup": "probe"},
					"spec", "template", "spec", "workloadRef")
			},
			retained: func(response map[string]any) bool {
				return lifecycleGuardJSONEqual(lifecycleGuardPath(response, "spec", "template", "spec", "workloadRef"),
					`{"name":"probe","podGroup":"probe"}`)
			},
		},
	},
	"1.36": {},
	"1.37": {
		{
			field: "JobSpec.scheduling",
			mutate: func(manifest map[string]any) error {
				return lifecycleGuardSet(manifest, map[string]any{"schedulingPolicy": map[string]any{"basic": map[string]any{}}},
					"spec", "scheduling")
			},
			retained: func(response map[string]any) bool {
				return lifecycleGuardJSONEqual(lifecycleGuardPath(response, "spec", "scheduling", "schedulingPolicy", "basic"), `{}`)
			},
		},
		{
			field: "PodSpec.evictionResponders",
			mutate: func(manifest map[string]any) error {
				return lifecycleGuardSet(manifest, []any{map[string]any{"name": "example.com/probe", "priority": 1000}},
					"spec", "template", "spec", "evictionResponders")
			},
			retained: func(response map[string]any) bool {
				return lifecycleGuardJSONEqual(lifecycleGuardPath(response, "spec", "template", "spec", "evictionResponders"),
					`[{"name":"example.com/probe","priority":1000}]`)
			},
		},
		{
			field: "EmptyDirVolumeSource.mode",
			mutate: func(manifest map[string]any) error {
				return lifecycleGuardEachNamed(manifest, "work", func(volume map[string]any) error {
					emptyDir, _ := volume["emptyDir"].(map[string]any)
					if emptyDir == nil {
						if volume["emptyDir"] != nil {
							return errors.New("the work volume's emptyDir is not an object")
						}
						emptyDir = map[string]any{}
						volume["emptyDir"] = emptyDir
					}
					emptyDir["mode"] = 448
					return nil
				}, "spec", "template", "spec", "volumes")
			},
			retained: func(response map[string]any) bool {
				return lifecycleGuardAnyNamed(response, "work", func(volume map[string]any) bool {
					return lifecycleGuardJSONEqual(lifecycleGuardPath(volume, "emptyDir", "mode"), `448`)
				}, "spec", "template", "spec", "volumes")
			},
		},
		{
			field: "VolumeMount.bindMountOptions",
			mutate: func(manifest map[string]any) error {
				container, err := lifecycleGuardFirstContainer(manifest)
				if err != nil {
					return err
				}
				return lifecycleGuardEachNamed(container, "work", func(mount map[string]any) error {
					mount["bindMountOptions"] = []any{"noexec"}
					return nil
				}, "volumeMounts")
			},
			retained: func(response map[string]any) bool {
				container, err := lifecycleGuardFirstContainer(response)
				if err != nil {
					return false
				}
				return lifecycleGuardAnyNamed(container, "work", func(mount map[string]any) bool {
					return lifecycleGuardJSONEqual(mount["bindMountOptions"], `["noexec"]`)
				}, "volumeMounts")
			},
		},
	},
}

// lifecycleGuardFieldProbes is the probe set the proof sends on a minor, and
// false for a minor the table does not name.
func lifecycleGuardFieldProbes(minor string) ([]lifecycleGuardFieldProbe, bool) {
	probes, ok := lifecycleGuardFieldProbeTable[minor]
	return probes, ok
}

// lifecycleGuardBaseManifest is the controller-object baseline: the Job the
// current release's manager dispatched for a read-only schema, cleared of
// what the API server assigned, renamed after its own operation as the Job
// write guard requires, and stamped with this release's controller identity
// on the Job and its Pod template alike.
func lifecycleGuardBaseManifest(job []byte, controllerImage, controllerStateVersion string) (map[string]any, error) {
	var manifest map[string]any
	if err := json.Unmarshal(job, &manifest); err != nil {
		return nil, fmt.Errorf("the read-only Job evidence is not a JSON document: %w", err)
	}
	metadata, err := lifecycleGuardObject(manifest, "metadata")
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"creationTimestamp", "generation", "managedFields", "resourceVersion", "uid"} {
		delete(metadata, key)
	}
	if spec, ok := manifest["spec"].(map[string]any); ok {
		delete(spec, "selector")
		delete(spec, "ttlSecondsAfterFinished")
	}
	delete(manifest, "status")
	template, _ := lifecycleGuardPath(manifest, "spec", "template", "metadata").(map[string]any)
	for _, key := range []string{"creationTimestamp", "generation", "managedFields", "resourceVersion", "uid"} {
		delete(template, key)
	}
	// The Job write guard requires the name to start with the operation its
	// own label names, so the probe is named after the operation the
	// captured Job carries rather than after a fixed one.
	operation, ok := lifecycleGuardPath(metadata, "labels", "operator.ptah.run/operation").(string)
	if !ok {
		return nil, errors.New("the read-only Job carries no operator.ptah.run/operation label to name the probe after")
	}
	metadata["name"] = "ptah-" + operation + "-vap-probe-0123456789abcdef"
	annotations, _ := metadata["annotations"].(map[string]any)
	if annotations == nil {
		annotations = map[string]any{}
		metadata["annotations"] = annotations
	}
	annotations["operator.ptah.run/controller-image"] = controllerImage
	annotations["operator.ptah.run/controller-revision"] = "e2e-controller-object-guard"
	annotations["operator.ptah.run/controller-state-version"] = controllerStateVersion
	if labels, ok := lifecycleGuardPath(manifest, "spec", "template", "metadata", "labels").(map[string]any); ok {
		for _, key := range []string{"batch.kubernetes.io/controller-uid", "batch.kubernetes.io/job-name", "controller-uid", "job-name"} {
			delete(labels, key)
		}
	}
	if err := lifecycleGuardSet(manifest, lifecycleGuardCopy(annotations), "spec", "template", "metadata", "annotations"); err != nil {
		return nil, err
	}
	return manifest, nil
}

// lifecycleGuardStampOtherReleaseImage stamps the manifest, and its Pod
// template, with a manager image no release of this chart runs.
func lifecycleGuardStampOtherReleaseImage(manifest map[string]any) error {
	annotations, err := lifecycleGuardObject(manifest, "metadata", "annotations")
	if err != nil {
		return err
	}
	annotations["operator.ptah.run/controller-image"] = lifecycleGuardOtherReleaseImage
	return lifecycleGuardSet(manifest, lifecycleGuardCopy(annotations), "spec", "template", "metadata", "annotations")
}

// lifecycleGuardWriteEvidence is controller_write_evidence: everything about
// a schema the manager could have changed, with the empty values jq's `//`
// gave an absent field.
func lifecycleGuardWriteEvidence(schema map[string]any) ([]byte, error) {
	metadata, _ := schema["metadata"].(map[string]any)
	evidence := map[string]any{"uid": metadataValue(metadata, "uid"), "spec": schema["spec"]}
	for key, empty := range map[string]any{
		"labels": map[string]any{}, "annotations": map[string]any{}, "ownerReferences": []any{}, "finalizers": []any{},
	} {
		value := metadataValue(metadata, key)
		if value == nil {
			value = empty
		}
		evidence[key] = value
	}
	evidence["status"] = lifecycleGuardStatusOrEmpty(schema)
	return json.Marshal(evidence)
}

// lifecycleGuardStatusOrEmpty is `.status // {}`.
func lifecycleGuardStatusOrEmpty(object map[string]any) any {
	if status := object["status"]; status != nil && status != false {
		return status
	}
	return map[string]any{}
}

// lifecycleGuardStatusEvidence is `jq -S '.status // {}'` as bytes, and
// lifecycleGuardStatusExactly `jq -S '.status'`: null when there is none.
func lifecycleGuardStatusEvidence(object map[string]any) ([]byte, error) {
	return json.Marshal(lifecycleGuardStatusOrEmpty(object))
}

func lifecycleGuardStatusExactly(object map[string]any) ([]byte, error) {
	return json.Marshal(object["status"])
}

// lifecycleGuardSuspendPatch flips a schema's suspension: `.spec.suspend //
// false`, which has to be a boolean, negated.
func lifecycleGuardSuspendPatch(schema map[string]any) ([]byte, error) {
	current := lifecycleGuardPath(schema, "spec", "suspend")
	if current == nil {
		current = false
	}
	suspended, ok := current.(bool)
	if !ok {
		return nil, errors.New("schema spec.suspend must be a boolean")
	}
	return json.Marshal(map[string]any{"spec": map[string]any{"suspend": !suspended}})
}

// lifecycleGuardOwnerPatch gives a schema an owner reference to a ConfigMap.
func lifecycleGuardOwnerPatch(name, uid string) []byte {
	return lifecycleGuardJSON(map[string]any{"metadata": map[string]any{"ownerReferences": []any{
		map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "name": name, "uid": uid},
	}}})
}

// lifecycleGuardFinalizers is `.metadata.finalizers // []`.
func lifecycleGuardFinalizers(object map[string]any) ([]any, error) {
	value := lifecycleGuardPath(object, "metadata", "finalizers")
	if value == nil {
		return []any{}, nil
	}
	finalizers, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("metadata.finalizers is %T, not a list", value)
	}
	return finalizers, nil
}

// lifecycleGuardHasActiveOperation is `index("operator.ptah.run/active-operation") != null`.
func lifecycleGuardHasActiveOperation(finalizers []any) bool {
	for _, finalizer := range finalizers {
		if finalizer == lifecycleGuardActiveOperationFinalizer {
			return true
		}
	}
	return false
}

// lifecycleGuardFinalizersPatch sets a schema's finalizers to the list given,
// with the manager's own appended when add is true.
func lifecycleGuardFinalizersPatch(finalizers []any, add bool) []byte {
	list := append([]any{}, finalizers...)
	if add {
		list = append(list, lifecycleGuardActiveOperationFinalizer)
	}
	return lifecycleGuardJSON(map[string]any{"metadata": map[string]any{"finalizers": list}})
}

// lifecycleGuardAddedExactly is the check that the manager added exactly its
// active-operation finalizer: the list is the one before with that one
// appended, in order.
func lifecycleGuardAddedExactly(object map[string]any, before []any) bool {
	after, err := lifecycleGuardFinalizers(object)
	if err != nil {
		return false
	}
	return reflect.DeepEqual(lifecycleGuardNormalize(after), lifecycleGuardNormalize(append(append([]any{}, before...), lifecycleGuardActiveOperationFinalizer)))
}

// lifecycleGuardSaid is grep -F over what a command wrote on either stream.
func lifecycleGuardSaid(text string, streams ...[]byte) bool {
	for _, stream := range streams {
		if bytes.Contains(stream, []byte(text)) {
			return true
		}
	}
	return false
}

// lifecycleGuardHead is `head -c 600 | tr '\n' ' '`: the start of a refusal
// on one line.
func lifecycleGuardHead(stream []byte) string {
	if len(stream) > 600 {
		stream = stream[:600]
	}
	return strings.ReplaceAll(string(stream), "\n", " ")
}

// lifecycleGuardDirectWriteProbes are the two objects a manager creates from
// a plan and reads back uncached: the ConfigMap an Apply mounts the plan
// through and the chunk the plan's bytes are stored in. Each names a plan no
// persisted object is, so the webhook has to refuse each on its own.
func lifecycleGuardDirectWriteProbes(namespace, schema string) []struct {
	kind     string
	manifest map[string]any
} {
	metadata := func() map[string]any {
		return map[string]any{
			"name": lifecycleGuardPlanName + "-000", "namespace": namespace,
			"labels": map[string]any{"operator.ptah.run/plan": lifecycleGuardPlanName, "operator.ptah.run/schema": schema},
			"ownerReferences": []any{map[string]any{
				"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahSchemaPlan", "name": lifecycleGuardPlanName,
				"uid": "11111111-1111-1111-1111-111111111111", "controller": true, "blockOwnerDeletion": true,
			}},
		}
	}
	return []struct {
		kind     string
		manifest map[string]any
	}{
		{"plan projection ConfigMap", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap", "metadata": metadata(),
			"immutable": true, "binaryData": map[string]any{"chunk": "cHJvYmU="},
		}},
		{"PtahSchemaPlanChunk", map[string]any{
			"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahSchemaPlanChunk", "metadata": metadata(),
			"spec": map[string]any{"data": "cHJvYmU="},
		}},
	}
}

// lifecycleGuardAdmissionSnapshot is the validating configuration as kubectl
// create can send it back: the fields the API server assigned removed.
func lifecycleGuardAdmissionSnapshot(configuration map[string]any) ([]byte, error) {
	snapshot := lifecycleGuardCopy(configuration).(map[string]any)
	if metadata, ok := snapshot["metadata"].(map[string]any); ok {
		for _, key := range []string{"creationTimestamp", "generation", "managedFields", "resourceVersion", "uid"} {
			delete(metadata, key)
		}
	}
	return json.Marshal(snapshot)
}

// lifecycleGuardFirstWebhook reads the first webhook's name and Service, as
// the script's jsonpath did: "" where either is absent.
func lifecycleGuardFirstWebhook(configuration map[string]any) (name, service string) {
	webhooks, _ := configuration["webhooks"].([]any)
	if len(webhooks) == 0 {
		return "", ""
	}
	first, _ := webhooks[0].(map[string]any)
	name, _ = first["name"].(string)
	service, _ = lifecycleGuardPath(first, "clientConfig", "service", "name").(string)
	return name, service
}

// lifecycleGuardPath is jq's .a.b.c: null wherever the path leaves an object.
func lifecycleGuardPath(object any, fields ...string) any {
	value := object
	for _, field := range fields {
		next, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = next[field]
	}
	return value
}

// lifecycleGuardObject is the object at a path, which has to be one.
func lifecycleGuardObject(object map[string]any, fields ...string) (map[string]any, error) {
	value, ok := lifecycleGuardPath(object, fields...).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s is not an object", strings.Join(fields, "."))
	}
	return value, nil
}

// lifecycleGuardSet is jq's .a.b.c = value: the objects along the path are created
// where they are null, and anything else in the way is an error.
func lifecycleGuardSet(object map[string]any, value any, fields ...string) error {
	current := object
	for index, field := range fields[:len(fields)-1] {
		next, ok := current[field].(map[string]any)
		if !ok {
			if current[field] != nil {
				return fmt.Errorf("%s is not an object", strings.Join(fields[:index+1], "."))
			}
			next = map[string]any{}
			current[field] = next
		}
		current = next
	}
	current[fields[len(fields)-1]] = value
	return nil
}

// lifecycleGuardEachNamed is jq's (.path[] | select(.name == name)) |= update: it walks a
// list, which has to be one, and updates the objects with the name.
func lifecycleGuardEachNamed(object map[string]any, name string, update func(map[string]any) error, fields ...string) error {
	list, ok := lifecycleGuardPath(object, fields...).([]any)
	if !ok {
		return fmt.Errorf("%s is not a list", strings.Join(fields, "."))
	}
	for _, entry := range list {
		item, ok := entry.(map[string]any)
		if !ok || item["name"] != name {
			continue
		}
		if err := update(item); err != nil {
			return err
		}
	}
	return nil
}

// lifecycleGuardAnyNamed is jq's any(.path[]; .name == name and match): false over an
// absent or empty list.
func lifecycleGuardAnyNamed(object map[string]any, name string, match func(map[string]any) bool, fields ...string) bool {
	list, _ := lifecycleGuardPath(object, fields...).([]any)
	for _, entry := range list {
		if item, ok := entry.(map[string]any); ok && item["name"] == name && match(item) {
			return true
		}
	}
	return false
}

// lifecycleGuardFirstContainer is .spec.template.spec.containers[0].
func lifecycleGuardFirstContainer(manifest map[string]any) (map[string]any, error) {
	containers, _ := lifecycleGuardPath(manifest, "spec", "template", "spec", "containers").([]any)
	if len(containers) == 0 {
		return nil, errors.New("the Job's Pod template has no container")
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		return nil, errors.New("the Job's first container is not an object")
	}
	return container, nil
}

// lifecycleGuardJSONEqual is jq's == between a decoded value and a JSON literal: numbers by
// value, objects by keys, lists in order.
func lifecycleGuardJSONEqual(value any, literal string) bool {
	var want any
	if err := json.Unmarshal([]byte(literal), &want); err != nil {
		return false
	}
	return reflect.DeepEqual(lifecycleGuardNormalize(value), want)
}

// lifecycleGuardNormalize sends a value through encoding/json, so numbers of any Go
// type compare as the float64 a decoded document holds.
func lifecycleGuardNormalize(value any) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return value
	}
	return decoded
}

// lifecycleGuardJSON encodes a document built from maps, lists, strings,
// numbers and booleans, which cannot fail.
func lifecycleGuardJSON(document any) []byte {
	encoded, err := json.Marshal(document)
	if err != nil {
		panic(err)
	}
	return encoded
}

// lifecycleGuardCopy copies a decoded JSON value.
func lifecycleGuardCopy(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		copied := make(map[string]any, len(typed))
		for key, entry := range typed {
			copied[key] = lifecycleGuardCopy(entry)
		}
		return copied
	case []any:
		copied := make([]any, len(typed))
		for index, entry := range typed {
			copied[index] = lifecycleGuardCopy(entry)
		}
		return copied
	default:
		return typed
	}
}
