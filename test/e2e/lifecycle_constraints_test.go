package e2e

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestLifecycleConstraintRefusals(t *testing.T) {
	t.Parallel()
	const pod, job, minor = "e2e-install-admission", "e2e-install-quota-control", "1.37"
	psa := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, pod,
		errors.New(`violates PodSecurity "restricted:v1.37": host namespaces (hostPID=true)`))
	quota := apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"}, job,
		errors.New("exceeded quota: e2e-install-quota, requested: count/jobs.batch=1, used: count/jobs.batch=0, limited: count/jobs.batch=0"))
	for _, proof := range []struct {
		name  string
		valid *apierrors.StatusError
		check func(error) bool
	}{
		{"Pod Security", psa, func(err error) bool { return lifecyclePodSecurityRefusal(err, pod, minor) }},
		{"quota", quota, func(err error) bool { return lifecycleQuotaRefusal(err, job) }},
	} {
		t.Run(proof.name, func(t *testing.T) {
			if !proof.check(proof.valid) || !proof.check(fmt.Errorf("create control: %w", proof.valid)) {
				t.Fatal("the exact typed admission refusal was not accepted")
			}
			for _, err := range []error{nil, errors.New(proof.valid.Error()), apierrors.NewTimeoutError("admission timeout", 1)} {
				if proof.check(err) {
					t.Fatalf("accepted something other than the admission refusal: %v", err)
				}
			}
			for _, mutation := range []struct {
				name string
				edit func(*metav1.Status)
			}{
				{"different reason", func(s *metav1.Status) { s.Reason = metav1.StatusReasonInvalid; s.Code = 422 }},
				{"no object identity", func(s *metav1.Status) { s.Details = nil }},
				{"different object", func(s *metav1.Status) { s.Details.Name = "another-object" }},
				{"different resource", func(s *metav1.Status) { s.Details.Kind = "deployments" }},
				{"different group", func(s *metav1.Status) { s.Details.Group = "apps" }},
				{"RBAC instead", func(s *metav1.Status) { s.Message = "the identity cannot create this resource" }},
			} {
				t.Run(mutation.name, func(t *testing.T) {
					status := proof.valid.ErrStatus.DeepCopy()
					mutation.edit(status)
					if proof.check(&apierrors.StatusError{ErrStatus: *status}) {
						t.Fatal("the wrong guard or object counted as the intended refusal")
					}
				})
			}
		})
	}
	for _, replacement := range []struct{ from, to string }{
		{"restricted:v1.37", "baseline:v1.37"},
		{"restricted:v1.37", "restricted:v1.36"},
		{"host namespaces (hostPID=true)", "allowPrivilegeEscalation != false"},
	} {
		status := psa.ErrStatus.DeepCopy()
		status.Message = strings.ReplaceAll(status.Message, replacement.from, replacement.to)
		if lifecyclePodSecurityRefusal(&apierrors.StatusError{ErrStatus: *status}, pod, minor) {
			t.Errorf("accepted a different Pod Security rule: %s", replacement.to)
		}
	}
	for _, replacement := range []struct{ from, to string }{
		{"exceeded quota: e2e-install-quota,", "exceeded quota: another-quota,"},
		{"limited: count/jobs.batch=0", "limited: count/jobs.batch=512"},
		{"count/jobs.batch", "pods"},
	} {
		status := quota.ErrStatus.DeepCopy()
		status.Message = strings.ReplaceAll(status.Message, replacement.from, replacement.to)
		if lifecycleQuotaRefusal(&apierrors.StatusError{ErrStatus: *status}, job) {
			t.Errorf("accepted a different quota refusal: %s", replacement.to)
		}
	}
}

func TestLifecycleQuotaInstallRefused(t *testing.T) {
	t.Parallel()
	const release, namespace, hook = "ptah", "ptah-system", "ptah-crd-reconcile"
	const valid = `{
		"name": "ptah", "namespace": "ptah-system", "version": 1,
		"info": {"status": "failed", "description": "failed pre-install: jobs.batch \"ptah-crd-reconcile\" is forbidden: exceeded quota: e2e-install-quota, requested: count/jobs.batch=1, used: count/jobs.batch=0, limited: count/jobs.batch=0"}
	}`
	if err := lifecycleQuotaInstallRefused([]byte(valid), release, namespace, hook, 1); err != nil {
		t.Fatalf("the intended failed hook CREATE was refused: %v", err)
	}
	for _, test := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"wrong release", func(s map[string]any) { s["name"] = "another-release" }},
		{"wrong namespace", func(s map[string]any) { s["namespace"] = "another-namespace" }},
		{"wrong revision", func(s map[string]any) { s["version"] = 2 }},
		{"not failed", func(s map[string]any) { s["info"].(map[string]any)["status"] = "pending-install" }},
		{"missing error", func(s map[string]any) { delete(s["info"].(map[string]any), "description") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			status := lifecycleFailureFixture(t, valid)
			test.edit(status)
			if err := lifecycleQuotaInstallRefused(lifecycleFailureEncode(t, status), release, namespace, hook, 1); err == nil {
				t.Fatal("accepted an unrelated failed installation")
			}
		})
	}
	for _, replacement := range []struct{ from, to string }{
		{"ptah-crd-reconcile", "another-hook"},
		{"jobs.batch", "pods"},
		{"is forbidden", "timed out"},
		{"exceeded quota: e2e-install-quota,", "exceeded quota: another-quota,"},
		{"limited: count/jobs.batch=0", "limited: count/jobs.batch=512"},
	} {
		status := strings.ReplaceAll(valid, replacement.from, replacement.to)
		if err := lifecycleQuotaInstallRefused([]byte(status), release, namespace, hook, 1); err == nil {
			t.Errorf("accepted the wrong failure: %s", replacement.to)
		}
	}
	for _, raw := range []string{"", "null", "{}", "[]", `{"version":"1"}`} {
		if err := lifecycleQuotaInstallRefused([]byte(raw), release, namespace, hook, 1); err == nil {
			t.Errorf("accepted incomplete status: %s", raw)
		}
	}
	if err := lifecycleQuotaInstallRefused([]byte(`{"info":{"description":"sensitive-value"}`), release, namespace, hook, 1); err == nil || strings.Contains(err.Error(), "sensitive-value") {
		t.Fatal("malformed status did not fail without exposing its contents")
	}
}
