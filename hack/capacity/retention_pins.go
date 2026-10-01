package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type retainedPlanID struct {
	Family    string `json:"family"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

type retentionPin struct {
	SourceKind string         `json:"sourceKind"`
	SourceName string         `json:"sourceName"`
	SourceUID  string         `json:"sourceUID"`
	Field      string         `json:"field"`
	Plan       retainedPlanID `json:"plan"`
}

type pinField struct {
	kind, family, path string
	requiredParent     bool
}

// Keep the nine fields in the operations runbook explicit. The metadata copy
// of an unresolved migration is an additional conservative pin, even if status
// says it was settled: pruning is not authorized to discard conflicting evidence.
var retentionPinFields = []pinField{
	{"PtahSchema", "schema", "status.plan", false},
	{"PtahSchema", "schema", "status.pendingObservation.plan", true},
	{"PtahSchema", "schema", "status.applied.planRef", true},
	{"PtahSchema", "schema", "status.pendingBindingRetirement.plan", false},
	{"PtahSchemaApproval", "schema", "spec.planRef", true},
	{"PtahMigrationApproval", "migration", "spec.planRef", true},
	{"PtahMigration", "migration", "status.plan", false},
	{"PtahMigration", "migration", "status.activeOperation.planRef", false},
	{"PtahMigration", "migration", "status.unresolvedRun.planRef", true},
}

func planID(object *unstructured.Unstructured, family string) retainedPlanID {
	return retainedPlanID{family, object.GetNamespace(), object.GetName(), string(object.GetUID())}
}

func planResource(family string) schema.GroupVersionResource {
	if family == "schema" {
		return schemaPlanResource
	}
	return migrationPlanResource
}

func readRetentionPins(object *unstructured.Unstructured) ([]retentionPin, error) {
	if object.GetUID() == "" || object.GetName() == "" || object.GetNamespace() == "" {
		return nil, fmt.Errorf("pin source lacks identity")
	}
	var pins []retentionPin
	appendPin := func(field, family string, raw any) error {
		value, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("%s/%s has malformed pin %s", object.GetKind(), object.GetName(), field)
		}
		name, _ := value["name"].(string)
		uid, _ := value["uid"].(string)
		if strings.TrimSpace(name) == "" || strings.TrimSpace(uid) == "" {
			return fmt.Errorf("pin %s lacks name or UID", field)
		}
		pins = append(pins, retentionPin{object.GetKind(), object.GetName(), string(object.GetUID()), field, retainedPlanID{family, object.GetNamespace(), name, uid}})
		return nil
	}
	recognized := false
	for _, definition := range retentionPinFields {
		if definition.kind != object.GetKind() {
			continue
		}
		recognized = true
		path := strings.Split(definition.path, ".")
		value, exists, err := unstructured.NestedFieldNoCopy(object.Object, path...)
		if err != nil {
			return nil, err
		}
		if !exists || value == nil {
			parent, present, err := unstructured.NestedFieldNoCopy(object.Object, path[:len(path)-1]...)
			if err != nil {
				return nil, err
			}
			if definition.requiredParent && (strings.HasSuffix(definition.kind, "Approval") || present && parent != nil) {
				return nil, fmt.Errorf("missing required pin %s", definition.path)
			}
			continue
		}
		if err := appendPin(definition.path, definition.family, value); err != nil {
			return nil, err
		}
	}
	if !recognized {
		return nil, fmt.Errorf("unknown pin source kind %s", object.GetKind())
	}
	if object.GetKind() == "PtahMigration" {
		if raw, exists := object.GetAnnotations()[operatorv1alpha1.UnresolvedRunAnnotation]; exists {
			var record map[string]any
			if err := json.Unmarshal([]byte(raw), &record); err != nil {
				return nil, fmt.Errorf("malformed unresolved-run copy: %w", err)
			}
			if err := appendPin("metadata.annotations."+operatorv1alpha1.UnresolvedRunAnnotation, "migration", record["planRef"]); err != nil {
				return nil, err
			}
		}
	}
	return pins, nil
}

type retentionInventory struct {
	Objects []retainedObject `json:"objects"`
	Pins    []retentionPin   `json:"pins"`
}

func (s *scenarios) retentionInventory(ctx context.Context) (retentionInventory, error) {
	result := retentionInventory{}
	resources := []schema.GroupVersionResource{schemaResource, migrationResource,
		{Group: schemaResource.Group, Version: schemaResource.Version, Resource: "ptahschemaapprovals"}, approvalGVR,
		schemaPlanResource, migrationPlanResource, planChunkResource, configMapResource}
	for _, ns := range workloadNamespaces(s.in.namespace, s.in.namespaces) {
		for _, resource := range resources {
			list, err := s.dynamic.Resource(resource).Namespace(ns).List(ctx, metav1.ListOptions{})
			if err != nil {
				return result, err
			}
			for _, item := range list.Items {
				if item.GetNamespace() != ns || item.GetUID() == "" || item.GetResourceVersion() == "" {
					return result, fmt.Errorf("retention inventory lacks object identity")
				}
				result.Objects = append(result.Objects, retainedObject{resource, item.DeepCopy()})
				if resource == schemaPlanResource || resource == migrationPlanResource || resource == planChunkResource || resource == configMapResource {
					continue
				}
				pins, err := readRetentionPins(&item)
				if err != nil {
					return result, err
				}
				result.Pins = append(result.Pins, pins...)
			}
		}
	}
	return result, result.validatePins()
}

func (i retentionInventory) plans() map[retainedPlanID]*unstructured.Unstructured {
	plans := map[retainedPlanID]*unstructured.Unstructured{}
	for _, entry := range i.Objects {
		family := "schema"
		if entry.Resource == migrationPlanResource {
			family = "migration"
		} else if entry.Resource != schemaPlanResource {
			continue
		}
		plans[planID(entry.Object, family)] = entry.Object
	}
	return plans
}

func (i retentionInventory) validatePins() error {
	plans := i.plans()
	for _, pin := range i.Pins {
		plan := plans[pin.Plan]
		if plan == nil || plan.GetDeletionTimestamp() != nil {
			return fmt.Errorf("%s/%s %s names a missing, replaced or deleting plan %s/%s", pin.SourceKind, pin.SourceName, pin.Field, pin.Plan.Namespace, pin.Plan.Name)
		}
	}
	return nil
}

func (i retentionInventory) pinned(id retainedPlanID) bool {
	for _, pin := range i.Pins {
		if pin.Plan == id {
			return true
		}
	}
	return false
}

func (s *scenarios) validateMaintenanceInventory(inventory retentionInventory, paused []pausedResource) error {
	seen := map[string]bool{}
	for _, entry := range inventory.Objects {
		if entry.Resource != schemaResource && entry.Resource != migrationResource {
			continue
		}
		object := entry.Object
		matched := false
		for _, p := range paused {
			if entry.Resource == p.target.resource && object.GetNamespace() == p.target.namespace && object.GetName() == p.target.name {
				if !maintenanceSuspended(object, p) {
					return fmt.Errorf("inventory contains a changed or active maintenance resource")
				}
				key := string(object.GetUID())
				if seen[key] {
					return fmt.Errorf("inventory repeats a maintenance resource")
				}
				seen[key] = true
				matched = true
				break
			}
		}
		if !matched && object.GetLabels()[capacityLabel] == s.load.Name {
			return fmt.Errorf("inventory contains undeclared workload resources")
		}
	}
	if len(seen) != len(paused) || len(seen) == 0 {
		return fmt.Errorf("inventory omitted a maintenance resource")
	}
	for id, plan := range inventory.plans() {
		associated := false
		for _, field := range []string{"schemaRef", "migrationRef"} {
			uid, _, _ := unstructured.NestedString(plan.Object, "spec", field, "uid")
			for _, p := range paused {
				associated = associated || uid == string(p.target.uid)
			}
		}
		for _, owner := range plan.GetOwnerReferences() {
			for _, p := range paused {
				associated = associated || owner.UID == p.target.uid
			}
		}
		if associated && !ownedPlan(plan, id.Family, paused) {
			return fmt.Errorf("workload plan %s/%s has inconsistent ownership", id.Namespace, id.Name)
		}
	}
	return nil
}
