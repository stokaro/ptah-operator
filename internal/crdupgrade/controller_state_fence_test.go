package crdupgrade

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Every location the scan covers has to refuse on its own. A fence that reads
// one path and reports on all of them is green over the ones it never looked at.
func TestTheStoredStateFenceRefusesAtEveryLocationItClaims(t *testing.T) {
	t.Parallel()
	for _, resourceKind := range storedControllerStateKinds {
		for _, location := range resourceKind.locations {
			t.Run(resourceKind.kind+"/"+location.name, func(t *testing.T) {
				t.Parallel()
				object := schemaWithControllerStateAt(
					"tenant-a", "resource-a", int64(newerStateVersion), location.path...)
				clients := emptyStoredStateClients()
				page := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{object}}
				switch resourceKind.kind {
				case "PtahSchema":
					clients.Schemas = &schemaListClient{pages: []*unstructured.UnstructuredList{page}}
				case "PtahSchemaPlan":
					clients.Plans = &schemaListClient{pages: []*unstructured.UnstructuredList{page}}
				case "PtahMigration":
					clients.Migrations = &schemaListClient{pages: []*unstructured.UnstructuredList{page}}
				case "PtahMigrationPlan":
					clients.MigrationPlans = &schemaListClient{pages: []*unstructured.UnstructuredList{page}}
				default:
					t.Fatalf("no client is wired for %s, so this proof would pass over nothing", resourceKind.kind)
				}
				err := VerifyStoredControllerState(
					context.Background(), clients, int64(ourStateVersion))
				if err == nil {
					t.Fatalf("%s %s admitted state a newer manager wrote", resourceKind.kind, location.name)
				}
				for _, want := range []string{
					"controller downgrade refused",
					resourceKind.kind,
					location.name + ".controllerStateVersion",
				} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("refusal = %v, want it to name %q", err, want)
					}
				}
			})
		}
	}
}
