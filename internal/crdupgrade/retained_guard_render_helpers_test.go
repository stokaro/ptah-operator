package crdupgrade

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
)

// persistedPolicy is a compiled policy as an API server returns it: with the
// UID and resourceVersion storage assigns.
func persistedPolicy(policy *admissionregistrationv1.ValidatingAdmissionPolicy) *admissionregistrationv1.ValidatingAdmissionPolicy {
	result := policy.DeepCopy()
	result.UID = "retained-policy-uid"
	result.ResourceVersion = "42"
	return result
}

// persistedBinding is a compiled binding as an API server returns it.
func persistedBinding(binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	result := binding.DeepCopy()
	result.UID = "retained-binding-uid"
	result.ResourceVersion = "42"
	return result
}

// retainedGuardAPIServer answers the discovery a server-side Helm render makes
// and serves one retained policy and binding, recording that the render read
// the policy. Everything else is NotFound.
func retainedGuardAPIServer(
	t *testing.T,
	policy *admissionregistrationv1.ValidatingAdmissionPolicy,
	binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding,
	policyRead *atomic.Bool,
) http.Handler {
	t.Helper()
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	bindingJSON, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	policyPath := "/apis/admissionregistration.k8s.io/v1/validatingadmissionpolicies/" + policy.Name
	bindingPath := "/apis/admissionregistration.k8s.io/v1/validatingadmissionpolicybindings/" + binding.Name

	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/version":
			_, _ = response.Write([]byte(`{"major":"1","minor":"35","gitVersion":"v1.35.0","gitCommit":"test","gitTreeState":"clean","buildDate":"2026-01-01T00:00:00Z","goVersion":"go1.26.0","compiler":"gc","platform":"darwin/arm64"}`))
		case "/api":
			_, _ = response.Write([]byte(`{"kind":"APIVersions","apiVersion":"v1","versions":["v1"],"serverAddressByClientCIDRs":[]}`))
		case "/apis":
			_, _ = response.Write([]byte(retainedGuardAPIGroupList))
		case "/api/v1":
			_, _ = response.Write([]byte(retainedGuardCoreResources))
		case "/apis/admissionregistration.k8s.io/v1":
			_, _ = response.Write([]byte(retainedGuardAdmissionResources))
		case "/apis/apps/v1":
			_, _ = response.Write([]byte(retainedGuardAppsResources))
		case "/apis/rbac.authorization.k8s.io/v1":
			_, _ = response.Write([]byte(retainedGuardRBACResources))
		case "/apis/batch/v1":
			_, _ = response.Write([]byte(retainedGuardBatchResources))
		case policyPath:
			policyRead.Store(true)
			_, _ = response.Write(policyJSON)
		case bindingPath:
			_, _ = response.Write(bindingJSON)
		default:
			response.WriteHeader(http.StatusNotFound)
			_, _ = response.Write([]byte(`{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"not found","reason":"NotFound","code":404}`))
		}
	})
}

const retainedGuardAPIGroupList = `{
  "kind":"APIGroupList","apiVersion":"v1","groups":[
    {"name":"admissionregistration.k8s.io","versions":[{"groupVersion":"admissionregistration.k8s.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"admissionregistration.k8s.io/v1","version":"v1"}},
    {"name":"apps","versions":[{"groupVersion":"apps/v1","version":"v1"}],"preferredVersion":{"groupVersion":"apps/v1","version":"v1"}},
    {"name":"batch","versions":[{"groupVersion":"batch/v1","version":"v1"}],"preferredVersion":{"groupVersion":"batch/v1","version":"v1"}},
    {"name":"rbac.authorization.k8s.io","versions":[{"groupVersion":"rbac.authorization.k8s.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"rbac.authorization.k8s.io/v1","version":"v1"}}
  ]
}`

const retainedGuardCoreResources = `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"v1","resources":[{"name":"serviceaccounts","singularName":"serviceaccount","namespaced":true,"kind":"ServiceAccount","verbs":["get"]},{"name":"configmaps","singularName":"configmap","namespaced":true,"kind":"ConfigMap","verbs":["get"]}]}`
const retainedGuardAdmissionResources = `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"admissionregistration.k8s.io/v1","resources":[{"name":"validatingadmissionpolicies","singularName":"validatingadmissionpolicy","namespaced":false,"kind":"ValidatingAdmissionPolicy","verbs":["get"]},{"name":"validatingadmissionpolicybindings","singularName":"validatingadmissionpolicybinding","namespaced":false,"kind":"ValidatingAdmissionPolicyBinding","verbs":["get"]}]}`
const retainedGuardAppsResources = `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"apps/v1","resources":[{"name":"deployments","singularName":"deployment","namespaced":true,"kind":"Deployment","verbs":["get"]}]}`
const retainedGuardRBACResources = `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"rbac.authorization.k8s.io/v1","resources":[{"name":"clusterrolebindings","singularName":"clusterrolebinding","namespaced":false,"kind":"ClusterRoleBinding","verbs":["get"]},{"name":"rolebindings","singularName":"rolebinding","namespaced":true,"kind":"RoleBinding","verbs":["get"]}]}`
const retainedGuardBatchResources = `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"batch/v1","resources":[{"name":"jobs","singularName":"job","namespaced":true,"kind":"Job","verbs":["get"]}]}`
