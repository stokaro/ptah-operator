package controllerwrite_test

import (
	"bytes"
	"context"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func TestValidationHandlerRejectsUnknownFieldInOldJob(t *testing.T) {
	t.Parallel()

	schema := schemaFixture(operatorv1alpha1.OperationResolve)
	expected := expectedJob(schema, schema.Status.ActiveOperation)
	oldJob := withGeneratedJobIdentity(expected)
	oldJob.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
	}}
	schema.Status.ActiveOperation.JobUID = oldJob.UID
	job := oldJob.DeepCopy()
	ttl := int32(300)
	job.Spec.TTLSecondsAfterFinished = &ttl
	handler := handlerFixture(t, staticJobBuilder{job: expected}, schema)

	request := requestFor(t, admissionv1.Update, job)
	request.OldObject = rawObject(t, oldJob)
	request.OldObject.Raw = bytes.Replace(
		request.OldObject.Raw,
		[]byte(`"spec":{`),
		[]byte(`"spec":{"scheduling":{},`),
		1,
	)
	response := handler.Handle(context.Background(), request)
	if response.Allowed {
		t.Fatal("Handle() accepted an unknown field in oldObject")
	}
	if response.Result == nil || response.Result.Code != 400 {
		t.Fatalf("Handle() status = %#v, want 400", response.Result)
	}
}
