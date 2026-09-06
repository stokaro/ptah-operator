package main

import (
	"strings"
	"testing"
)

// A candidate render comes from a client-side helm template, which has no
// lookup of the previous controller principal: the chart leaves the name, uid
// and release sequence empty and says managed=false. The live Job carries what
// the cluster established. The render fixes those arguments' positions and
// shapes, never their values.
func TestValidateManagerArgumentsAcceptsAnAbsentPreviousController(t *testing.T) {
	t.Parallel()
	config := captureConfig{namespace: testNamespace, jobName: testJobNameForMode(hookModePreflight), hookMode: hookModePreflight}
	job := validRenderedJobForMode(hookModePreflight)
	arguments := append([]string(nil), job.Spec.Template.Spec.Containers[0].Args...)
	arguments[15] = "--previous-controller-service-account-name="
	arguments[16] = "--previous-controller-service-account-uid="
	arguments[17] = "--previous-controller-service-account-managed=false"
	arguments[18] = "--previous-controller-release-sequence="
	if err := validateManagerArguments(arguments, config, testImage); err != nil {
		t.Fatalf("an absent previous controller is how every candidate render reads: %v", err)
	}
	malformed := map[int]string{
		15: "--previous-controller-service-account-name=Not A Name",
		16: "--previous-controller-service-account-uid=has space",
		17: "--previous-controller-service-account-managed=maybe",
		18: "--previous-controller-release-sequence=x",
		12: "--hook-service-account-name=",
	}
	for index, value := range malformed {
		broken := append([]string(nil), arguments...)
		broken[index] = value
		if err := validateManagerArguments(broken, config, testImage); err == nil || !strings.Contains(err.Error(), "argument") {
			t.Errorf("argument %d %q was accepted: %v", index, value, err)
		}
	}
}

func TestValidateJobAgainstRenderFixesPreviousControllerShapeNotValue(t *testing.T) {
	t.Parallel()
	expected := validRenderedJobForMode(hookModePreflight)
	rendered := expected.Spec.Template.Spec.Containers[0].Args
	rendered[15] = "--previous-controller-service-account-name="
	rendered[16] = "--previous-controller-service-account-uid="
	rendered[17] = "--previous-controller-service-account-managed=false"
	rendered[18] = "--previous-controller-release-sequence="
	observed := validJobForMode(hookModePreflight)
	if err := validateJobAgainstRender(observed, expected); err != nil {
		t.Fatalf("a live previous controller must match a render that could not know it: %v", err)
	}
	drifted := validJobForMode(hookModePreflight)
	drifted.Spec.Template.Spec.Containers[0].Args[20] = "--controller-replicas=3"
	if err := validateJobAgainstRender(drifted, expected); err == nil {
		t.Fatal("a drifted non-lookup argument was accepted")
	}
}
