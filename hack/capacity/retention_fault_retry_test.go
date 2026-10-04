package main

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestRetentionFaultRetryRequiresTheOriginalResumedFleet(t *testing.T) {
	for _, mode := range []string{"valid", "missing baseline", "duplicate baseline", "replaced UID", "changed spec", "wrong artifact", "wrong label", "still suspended", "extra resource"} {
		t.Run(mode, func(t *testing.T) {
			s := soakScenarios()
			s.load.Soak.RetentionFault = true
			var inventory retentionInventory
			var live []runtime.Object
			for _, family := range []string{"schema", "migration"} {
				for i := 0; i < 10; i++ {
					o := soakObject(t, s, family, i)
					resource, field, reference := schemaResource, "desired", s.schemaReference(i, s.load.Soak.Rounds)
					if family == "migration" {
						resource, field, reference = migrationResource, "artifact", s.migrationReference(i, s.load.Soak.Rounds)
					}
					_ = unstructured.SetNestedField(o.Object, reference, "spec", field, "ociRef")
					_ = unstructured.SetNestedField(o.Object, false, "spec", "suspend")
					before := o.DeepCopy()
					_ = unstructured.SetNestedField(before.Object, true, "spec", "suspend")
					inventory.Objects = append(inventory.Objects, retainedObject{resource, before})
					live = append(live, o)
				}
			}
			o := live[0].(*unstructured.Unstructured)
			switch mode {
			case "missing baseline":
				inventory.Objects = inventory.Objects[1:]
			case "duplicate baseline":
				inventory.Objects = append(inventory.Objects, inventory.Objects[0])
			case "replaced UID":
				o.SetUID("replacement")
			case "changed spec":
				_ = unstructured.SetNestedField(o.Object, "other", "spec", "target", "urlFrom", "name")
			case "wrong artifact":
				_ = unstructured.SetNestedField(o.Object, "other", "spec", "desired", "ociRef")
			case "wrong label":
				o.SetLabels(map[string]string{capacityLabel: "other"})
			case "extra resource":
				extra := o.DeepCopy()
				extra.SetName("extra")
				extra.SetUID("extra")
				live = append(live, extra)
			case "still suspended":
				_ = unstructured.SetNestedField(o.Object, true, "spec", "suspend")
			}
			s.faultBaseline = &inventory
			s.dynamic = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{schemaResource: "PtahSchemaList", migrationResource: "PtahMigrationList"}, live...)
			if err := s.validateFaultBaseline(t.Context()); (err == nil) != (mode == "valid") {
				t.Fatal(err)
			}
		})
	}
}
