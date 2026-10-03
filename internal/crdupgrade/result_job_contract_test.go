package crdupgrade

import (
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/workload"
)

func TestControllerJobWriteGuardAdmitsDurableResult(t *testing.T) {
	controllerImage := "example.test/controller@sha256:" + strings.Repeat("1", 64)
	builder := workload.Builder{
		ExecutorImage:          "example.test/executor@sha256:" + strings.Repeat("2", 64),
		RunnerImage:            "example.test/runner@sha256:" + strings.Repeat("3", 64),
		PtahVersion:            "v0.3.0",
		ControllerImage:        controllerImage,
		ControllerRevision:     "test-revision",
		ControllerStateVersion: ourStateVersion,
	}

	builder.ResultEndpoint = "https://receiver.operator.svc"
	schema, operation := contractSchemaFixture(t, builder)
	schema.Generation = 1
	job, err := builder.Build(schema, operation, nil)
	if err != nil {
		t.Fatal(err)
	}
	object := celObject(t, job)
	for index, validation := range controllerJobWriteValidations("refused") {
		if !evaluateJobContract(t, validation.Expression, object, controllerImage) {
			t.Fatalf("validation %d refused the durable Job", index)
		}
	}
}
