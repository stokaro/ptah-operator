package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/planstore"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestChurnExportsTheExactCommittedBytesBeforeOwnerDeletion(t *testing.T) {
	for _, mode := range []string{"valid", "chunk replacement", "corrupt chunk", "corrupt projection", "missing pin"} {
		t.Run(mode, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/churn-plan-export.json")
			if err != nil {
				t.Fatal(err)
			}
			var fixture struct {
				Plan        operatorv1alpha1.PtahSchemaPlan        `json:"plan"`
				Chunks      []operatorv1alpha1.PtahSchemaPlanChunk `json:"chunks"`
				Projections []corev1.ConfigMap                     `json:"projections"`
			}
			if err := json.Unmarshal(raw, &fixture); err != nil {
				t.Fatal(err)
			}
			if len(fixture.Chunks) != 1 || len(fixture.Projections) != 1 || fixture.Plan.Spec.Size != 3582 {
				t.Fatal("native fixture changed")
			}
			switch mode {
			case "chunk replacement":
				fixture.Chunks[0].UID = "replacement"
			case "corrupt chunk":
				fixture.Chunks[0].Spec.Data[0] ^= 1
			case "corrupt projection":
				for key := range fixture.Projections[0].BinaryData {
					fixture.Projections[0].BinaryData[key] = []byte("wrong")
				}
			}
			objects := []runtime.Object{}
			for _, object := range []any{&fixture.Plan, &fixture.Chunks[0]} {
				value, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
				if err != nil {
					t.Fatal(err)
				}
				objects = append(objects, &unstructured.Unstructured{Object: value})
			}
			s := soakScenarios()
			s.evidenceDir = t.TempDir()
			s.dynamic = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{schemaPlanResource: "PtahSchemaPlanList"}, objects...)
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			s.inputReader = clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(&fixture.Projections[0]).Build()
			original := soakObject(t, s, "schema", 0)
			original.SetUID(fixture.Plan.Spec.SchemaRef.UID)
			original.SetNamespace(fixture.Plan.Namespace)
			original.SetName(fixture.Plan.Spec.SchemaRef.Name)
			pin := string(fixture.Plan.UID)
			if mode == "missing pin" {
				pin = "missing"
			}
			_ = unstructured.SetNestedMap(original.Object, map[string]any{"name": fixture.Plan.Name, "uid": pin}, "status", "plan")
			path, digest, children, err := s.exportChurn(t.Context(), original, "schema", 1, 0)
			if mode != "valid" {
				if err == nil {
					t.Fatalf("bad exported bytes passed: %s", mode)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(children) != 3 || !capacityHexDigest.MatchString(digest) {
				t.Fatal("export did not include the plan, chunk and projection", len(children), digest)
			}
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var export struct {
				Payloads map[string][]byte `json:"verifiedPayloads"`
				Objects  []retainedObject  `json:"objects"`
			}
			if err := json.Unmarshal(content, &export); err != nil {
				t.Fatal(err)
			}
			if len(export.Payloads[string(fixture.Plan.UID)]) != 3582 {
				t.Fatal("exported payload size differs")
			}
			snapshot := exportedChunks{}
			for _, entry := range export.Objects {
				if entry.Resource == planChunkResource {
					var chunk operatorv1alpha1.PtahSchemaPlanChunk
					if err := runtime.DefaultUnstructuredConverter.FromUnstructured(entry.Object.Object, &chunk); err != nil {
						t.Fatal(err)
					}
					snapshot[client.ObjectKeyFromObject(&chunk)] = &chunk
				}
			}
			loaded, err := (planstore.Store{Reader: snapshot}).Load(t.Context(), &fixture.Plan)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(loaded, export.Payloads[string(fixture.Plan.UID)]) {
				t.Fatal("export did not preserve the verified bytes")
			}
		})
	}
}
