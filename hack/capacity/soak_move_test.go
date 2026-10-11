package main

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func roundSchema(reference string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahSchema",
		"metadata": map[string]any{"name": "capacity-schema-000", "namespace": "work-0", "uid": "uid-0", "resourceVersion": "7", "generation": int64(3)},
		"spec":     map[string]any{"desired": map[string]any{"ociRef": reference}},
	}}
}

// The 0.2.1 amd64 soak cell stopped at round 2 on a conflict that the
// operator's own status write caused; the move reads again and retries.
func TestRoundInputSurvivesAStatusWriteBetweenReadAndPatch(t *testing.T) {
	t.Parallel()
	fake := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), roundSchema("oci://r/s:1"))
	conflicts := 0
	fake.PrependReactor("patch", "ptahschemas", func(clienttesting.Action) (bool, runtime.Object, error) {
		if conflicts == 0 {
			conflicts++
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "operator.ptah.run", Resource: "ptahschemas"}, "capacity-schema-000", nil)
		}
		return false, nil, nil
	})
	var start time.Time
	_, object, err := moveRoundInput(context.Background(), fake.Resource(schemaResource).Namespace("work-0"), "capacity-schema-000", "desired", "oci://r/s:1", "oci://r/s:2", &start)
	if err != nil {
		t.Fatalf("a conflict from a status write failed the round: %v", err)
	}
	if got, _, _ := unstructured.NestedString(object.Object, "spec", "desired", "ociRef"); got != "oci://r/s:2" || conflicts != 1 || start.IsZero() {
		t.Fatalf("moved to %q after %d conflicts, start %v", got, conflicts, start)
	}
}

// A retry never overwrites a spec edit that arrived between the attempts.
func TestRoundInputRefusesASpecThatMovedBetweenAttempts(t *testing.T) {
	t.Parallel()
	fake := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), roundSchema("oci://r/s:1"))
	patches := 0
	fake.PrependReactor("patch", "ptahschemas", func(clienttesting.Action) (bool, runtime.Object, error) {
		patches++
		if patches == 1 {
			if err := fake.Tracker().Update(schemaResource, roundSchema("oci://r/s:other"), "work-0"); err != nil {
				t.Fatal(err)
			}
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "operator.ptah.run", Resource: "ptahschemas"}, "capacity-schema-000", nil)
		}
		return false, nil, nil
	})
	var start time.Time
	_, _, err := moveRoundInput(context.Background(), fake.Resource(schemaResource).Namespace("work-0"), "capacity-schema-000", "desired", "oci://r/s:1", "oci://r/s:2", &start)
	if err == nil || !strings.Contains(err.Error(), "does not hold the previous round's original input") || patches != 1 {
		t.Fatalf("a moved spec was not refused: err=%v patches=%d", err, patches)
	}
}
