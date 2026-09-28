package crdupgrade

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	goruntime "runtime"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// chartPath is the chart this package's render tests render.
func chartPath(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("resolve the chart path")
	}
	return filepath.Join(filepath.Dir(filename), "..", "..", "charts", "ptah-operator")
}

func renderedDeploymentServiceAccount(t *testing.T, rendered []byte, deploymentName string) string {
	t.Helper()
	decoder := utilyaml.NewYAMLToJSONDecoder(bytes.NewReader(rendered))
	serviceAccountName := ""
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
		if typeMeta.Kind != "Deployment" {
			continue
		}
		var deployment appsv1.Deployment
		if err := json.Unmarshal(raw, &deployment); err != nil {
			t.Fatal(err)
		}
		if deployment.Name != deploymentName {
			continue
		}
		if serviceAccountName != "" {
			t.Fatalf("rendered Deployment/%s is duplicated", deploymentName)
		}
		serviceAccountName = deployment.Spec.Template.Spec.ServiceAccountName
	}
	if serviceAccountName == "" {
		t.Fatalf("rendered Deployment/%s has no ServiceAccount", deploymentName)
	}
	return serviceAccountName
}
