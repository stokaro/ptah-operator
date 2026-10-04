package main

import (
	"context"
	"fmt"
	"reflect"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// A local retry reuses expensive fixture publication, but may not adopt a
// replacement resource, changed spec or another workload. Its report declares
// only the fault; it does not claim to have repeated the measured soak.
func (s *scenarios) validateFaultBaseline(ctx context.Context) error {
	if s.faultBaseline == nil || s.load.Soak == nil || !s.load.Soak.RetentionFault || s.load.Schemas != 10 || s.load.Migrations != 10 {
		return fmt.Errorf("fault retry requires the twenty-resource soak baseline")
	}
	ctx, cancel := context.WithTimeout(ctx, s.load.Settle.Duration)
	defer cancel()
	expected := map[string]*unstructured.Unstructured{}
	for _, entry := range s.faultBaseline.Objects {
		if entry.Resource != schemaResource && entry.Resource != migrationResource {
			continue
		}
		o := entry.Object
		if o == nil || o.GetUID() == "" || o.GetLabels()[capacityLabel] != s.load.Name {
			return fmt.Errorf("fault baseline lacks a workload identity")
		}
		key := entry.Resource.Resource + "/" + o.GetNamespace() + "/" + o.GetName()
		if expected[key] != nil {
			return fmt.Errorf("fault baseline repeats a resource")
		}
		expected[key] = o
	}
	if len(expected) != 20 {
		return fmt.Errorf("fault baseline does not contain exactly twenty workload resources")
	}
	seen := 0
	for _, ns := range workloadNamespaces(s.in.namespace, s.in.namespaces) {
		for _, resource := range []schema.GroupVersionResource{schemaResource, migrationResource} {
			objects, err := s.dynamic.Resource(resource).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: capacityLabel + "=" + s.load.Name})
			if err != nil {
				return err
			}
			for _, o := range objects.Items {
				previous := expected[resource.Resource+"/"+ns+"/"+o.GetName()]
				if previous == nil || previous.GetUID() != o.GetUID() {
					return fmt.Errorf("fault retry contains an undeclared workload resource")
				}
				seen++
			}
		}
	}
	if seen != 20 {
		return fmt.Errorf("fault retry does not contain all twenty original workload resources")
	}
	for _, family := range []string{"schema", "migration"} {
		for index := 0; index < 10; index++ {
			resource, name, field, reference := schemaResource, s.schemaName(index), "desired", s.schemaReference(index, s.load.Soak.Rounds)
			if family == "migration" {
				resource, name, field, reference = migrationResource, s.migrationName(index), "artifact", s.migrationReference(index, s.load.Soak.Rounds)
			}
			ns := s.in.namespaceFor(index)
			original := expected[resource.Resource+"/"+ns+"/"+name]
			if original == nil {
				return fmt.Errorf("fault baseline omitted %s/%s", ns, name)
			}
			current, err := s.dynamic.Resource(resource).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			spec, found, err := unstructured.NestedMap(original.Object, "spec")
			if err != nil || !found {
				return fmt.Errorf("fault baseline lacks spec")
			}
			spec["suspend"] = false
			actual, _, _ := unstructured.NestedString(current.Object, "spec", field, "ociRef")
			if current.GetUID() != original.GetUID() || current.GetLabels()[capacityLabel] != s.load.Name || current.GetDeletionTimestamp() != nil || actual != reference || !reflect.DeepEqual(current.Object["spec"], spec) {
				return fmt.Errorf("fault retry refuses a replacement, changed spec, input or suspended resource: %s/%s", ns, name)
			}
			if _, err := s.waitFaultResource(ctx, resource, current, inactiveForMaintenance); err != nil {
				return err
			}
		}
	}
	return nil
}
