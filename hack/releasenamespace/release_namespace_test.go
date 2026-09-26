// Package releasenamespace_test renders the chart against a fake API server to
// prove the release-namespace check: the refusal webhook.yaml runs before
// anything is installed, and the RoleBinding warning NOTES.txt prints after.
//
// Both read the cluster through Helm's lookup, which a plain helm template
// cannot populate. helm install --dry-run=server does populate it, from
// whatever answers at the kubeconfig's address, so each case stands up an
// httptest server holding exactly the objects the case is about. That is the
// same approach internal/crdupgrade/crd_bootstrap_render_test.go takes.
//
// The renders live in a package of their own so their time counts against a
// test binary of their own rather than the hack package's.
package releasenamespace_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const releaseName = "ptah-e2e"

// workloadKinds are the kinds the check lists, with where the API serves them.
var workloadKinds = []struct {
	kind, groupVersion, resource string
}{
	{"Pod", "v1", "pods"},
	{"ReplicationController", "v1", "replicationcontrollers"},
	{"Deployment", "apps/v1", "deployments"},
	{"StatefulSet", "apps/v1", "statefulsets"},
	{"DaemonSet", "apps/v1", "daemonsets"},
	{"ReplicaSet", "apps/v1", "replicasets"},
	{"Job", "batch/v1", "jobs"},
	{"CronJob", "batch/v1", "cronjobs"},
}

// An install into a namespace the operator would share is refused before
// anything is written, and one into a namespace of its own is not.
func TestTheChartRefusesASharedReleaseNamespace(t *testing.T) {
	t.Parallel()
	helm := helmOrSkip(t)

	type testCase struct {
		name      string
		namespace string
		objects   []object
		values    []string
		// refusal is every fragment the error must carry; empty means the
		// render must succeed.
		refusal []string
	}
	cases := []testCase{
		{name: "a dedicated empty namespace", namespace: "ptah-system"},
		{
			name:      "default",
			namespace: "default",
			refusal:   []string{"release namespace default is shared with the whole cluster", "releaseNamespace.allowSharedNamespace=true"},
		},
		{
			name:      "a kube- namespace",
			namespace: "kube-public",
			refusal:   []string{"release namespace kube-public is shared with the whole cluster"},
		},
		{
			// kube- is a prefix of the reserved names, not of every name that
			// starts with those letters.
			name:      "a namespace that merely starts with kube",
			namespace: "kubernetes-operators",
		},
		{
			name:      "another release's workload",
			namespace: "ptah-system",
			objects:   []object{workload("Deployment", "ptah-system", "other-release", "other")},
			refusal:   []string{"runs workloads without app.kubernetes.io/instance=" + releaseName + " (Deployment/other-release)"},
		},
		{
			name:      "this release's workloads of every kind",
			namespace: "ptah-system",
			objects:   everyKind("ptah-system", releaseName),
		},
		{
			name:      "the override over default with a foreign Pod",
			namespace: "default",
			objects:   []object{workload("Pod", "default", "application", "")},
			values:    []string{"releaseNamespace.allowSharedNamespace=true"},
		},
		{
			name:      "the override over a foreign workload of every kind",
			namespace: "ptah-system",
			objects:   everyKind("ptah-system", ""),
			values:    []string{"releaseNamespace.allowSharedNamespace=true"},
		},
		{
			// The message names five, sorted, and counts the rest.
			name:      "more foreign workloads than the message names",
			namespace: "ptah-system",
			objects:   everyKind("ptah-system", ""),
			refusal: []string{
				"(CronJob/foreign-cronjob, DaemonSet/foreign-daemonset, Deployment/foreign-deployment, Job/foreign-job, Pod/foreign-pod and 3 more)",
			},
		},
		{
			// A workload elsewhere in the cluster is not the release
			// namespace's business.
			name:      "a foreign workload in another namespace",
			namespace: "ptah-system",
			objects:   []object{workload("Pod", "applications", "application", "")},
		},
	}
	// Each kind on its own, so every lookup the check makes is shown to be
	// read: a kind whose lookup were dropped would pass this row.
	for _, kind := range workloadKinds {
		name := "foreign-" + strings.ToLower(kind.kind)
		cases = append(cases, testCase{
			name:      "a foreign " + kind.kind,
			namespace: "ptah-system",
			objects:   []object{workload(kind.kind, "ptah-system", name, "")},
			refusal: []string{
				"release namespace ptah-system runs workloads without app.kubernetes.io/instance=" + releaseName + " (" + kind.kind + "/" + name + ")",
			},
		})
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cluster := newFakeCluster(t, test.objects)
			output, err := installDryRun(t, helm, cluster.kubeconfig(t), test.namespace, test.values)
			if len(test.refusal) == 0 {
				if err != nil {
					t.Fatalf("the install was refused: %v\n%s", err, tail(output))
				}
			} else {
				if err == nil {
					t.Fatalf("the install was not refused; want an error carrying %q", test.refusal)
				}
				for _, fragment := range test.refusal {
					if !strings.Contains(string(output), fragment) {
						t.Errorf("the refusal does not carry %q:\n%s", fragment, tail(output))
					}
				}
			}
			// Every case reaches the Pod list, which is what gates the check:
			// a render that never asked would pass the accepting rows above
			// without having looked.
			podList := "/api/v1/namespaces/" + test.namespace + "/pods"
			if cluster.listed(podList) == 0 {
				t.Errorf("the render never listed %s, so the check did not run", podList)
			}
		})
	}
}

// helm template, a client-side dry run and a GitOps render have no cluster to
// read. The check then has nothing to judge, and it lets the render through
// rather than refusing every offline render into default -- which is where
// helm lint renders.
func TestAnOfflineRenderSkipsTheCheck(t *testing.T) {
	t.Parallel()
	helm := helmOrSkip(t)
	for _, namespace := range []string{"default", "kube-system"} {
		t.Run(namespace, func(t *testing.T) {
			t.Parallel()
			arguments := append([]string{"template", releaseName, chartPath(t), "--namespace", namespace}, requiredValues()...)
			output, err := runHelm(t, helm, arguments...)
			if err != nil {
				t.Fatalf("an offline render into %s was refused: %v\n%s", namespace, err, tail(output))
			}
		})
	}
}

// The warning names every RoleBinding that makes a principal other than this
// release's ServiceAccounts a Ptah administrator, in the release namespace
// and in a separate coordination namespace, and nothing else.
func TestTheNotesWarnAboutAdministratorGrants(t *testing.T) {
	t.Parallel()
	helm := helmOrSkip(t)
	const (
		namespace    = "ptah-system"
		coordination = "ptah-coordination"
	)
	ownServiceAccount := serviceAccount(namespace, "ptah-e2e-ptah-operator-v1-000000000000", releaseName)
	objects := []object{
		ownServiceAccount,
		serviceAccount(namespace, "application", ""),
		role(namespace, "pod-creator", rule([]string{""}, []string{"pods"}, []string{"create"}, nil)),
		role(namespace, "reader", rule([]string{""}, []string{"pods", "pods/log"}, []string{"get", "list", "watch"}, nil)),
		role(namespace, "everything", rule([]string{"*"}, []string{"*"}, []string{"*"}, nil)),
		role(namespace, "exec-anywhere", rule([]string{""}, []string{"*/exec"}, []string{"create"}, nil)),
		role(namespace, "named-pods", rule([]string{""}, []string{"pods"}, []string{"create"}, []string{"one"})),
		role(namespace, "named-tokens", rule([]string{""}, []string{"serviceaccounts/token"}, []string{"create"}, []string{"application"})),
		role(coordination, "lease-writer", rule([]string{"coordination.k8s.io"}, []string{"leases"}, []string{"create", "update"}, nil)),
		// An aggregated ClusterRole carries the rules the aggregation
		// controller wrote into it; those are what grant.
		aggregatedClusterRole("aggregated-jobs", rule([]string{"batch"}, []string{"jobs", "cronjobs"}, []string{"create"}, nil)),
		clusterRole("viewer", rule([]string{"apps"}, []string{"deployments"}, []string{"get"}, nil)),

		// Warned about.
		roleBinding(namespace, "edit-for-developers", "ClusterRole", "edit", subject("Group", "", "developers")),
		roleBinding(namespace, "admin-for-alice", "ClusterRole", "admin", subject("User", "", "alice")),
		roleBinding(namespace, "pods-for-application", "Role", "pod-creator", subject("ServiceAccount", namespace, "application")),
		roleBinding(namespace, "everything-for-bob", "Role", "everything", subject("User", "", "bob")),
		roleBinding(namespace, "exec-for-carol", "Role", "exec-anywhere", subject("User", "", "carol")),
		roleBinding(namespace, "tokens-for-dave", "Role", "named-tokens", subject("User", "", "dave")),
		roleBinding(namespace, "aggregated-for-erin", "ClusterRole", "aggregated-jobs", subject("User", "", "erin")),
		roleBinding(namespace, "mixed-subjects", "Role", "pod-creator",
			subject("ServiceAccount", namespace, ownServiceAccount.name), subject("User", "", "frank")),
		roleBinding(coordination, "leases-for-grace", "Role", "lease-writer", subject("User", "", "grace")),
		roleBinding(coordination, "edit-for-foreign-sa", "ClusterRole", "edit", subject("ServiceAccount", "applications", "worker")),

		// Not warned about: this release's own ServiceAccount, a read-only
		// role, a create the authorizer can never grant by name, a role
		// that grants nothing dangerous, and a namespace the operator does
		// not use.
		roleBinding(namespace, "own-controller", "Role", "pod-creator", subject("ServiceAccount", namespace, ownServiceAccount.name)),
		roleBinding(namespace, "readers", "Role", "reader", subject("Group", "", "readers")),
		roleBinding(namespace, "named-pods-for-heidi", "Role", "named-pods", subject("User", "", "heidi")),
		roleBinding(namespace, "viewers", "ClusterRole", "viewer", subject("User", "", "ivan")),
		roleBinding(namespace, "missing-role", "Role", "does-not-exist", subject("User", "", "judy")),
		roleBinding("applications", "edit-elsewhere", "ClusterRole", "edit", subject("User", "", "mallory")),
	}
	cluster := newFakeCluster(t, objects)
	output, err := installDryRun(t, helm, cluster.kubeconfig(t), namespace,
		[]string{"coordination.namespace=" + coordination})
	if err != nil {
		t.Fatalf("the install was refused: %v\n%s", err, tail(output))
	}
	got := warnings(t, string(output))
	want := []string{
		"RoleBinding ptah-system/edit-for-developers binds ClusterRole edit to Group developers",
		"RoleBinding ptah-system/admin-for-alice binds ClusterRole admin to User alice",
		"RoleBinding ptah-system/pods-for-application grants create on pods through Role pod-creator to ServiceAccount ptah-system/application",
		"RoleBinding ptah-system/everything-for-bob grants create on pods, pods/exec, serviceaccounts/token, replicationcontrollers, " +
			"deployments.apps, statefulsets.apps, daemonsets.apps, replicasets.apps, jobs.batch, cronjobs.batch, leases.coordination.k8s.io " +
			"through Role everything to User bob",
		"RoleBinding ptah-system/exec-for-carol grants create on pods/exec through Role exec-anywhere to User carol",
		"RoleBinding ptah-system/tokens-for-dave grants create on serviceaccounts/token through Role named-tokens to User dave",
		"RoleBinding ptah-system/aggregated-for-erin grants create on jobs.batch, cronjobs.batch through ClusterRole aggregated-jobs to User erin",
		"RoleBinding ptah-system/mixed-subjects grants create on pods through Role pod-creator to User frank",
		"RoleBinding ptah-coordination/leases-for-grace grants create on leases.coordination.k8s.io through Role lease-writer to User grace",
		"RoleBinding ptah-coordination/edit-for-foreign-sa binds ClusterRole edit to ServiceAccount applications/worker",
	}
	assertSameLines(t, got, want)
}

// A release's own grants, including the per-sequence ServiceAccounts a
// deployment supplies itself with serviceAccount.create=false, produce no
// warning at all.
func TestTheNotesAreQuietAboutTheReleasesOwnGrants(t *testing.T) {
	t.Parallel()
	helm := helmOrSkip(t)
	const namespace = "ptah-system"
	objects := []object{
		role(namespace, "pod-creator", rule([]string{""}, []string{"pods"}, []string{"create"}, nil)),
		roleBinding(namespace, "external-controller", "Role", "pod-creator", subject("ServiceAccount", namespace, "external-controller-v1")),
		roleBinding(namespace, "external-controller-next", "Role", "pod-creator", subject("ServiceAccount", namespace, "external-controller-v2")),
		roleBinding(namespace, "readers", "ClusterRole", "view", subject("Group", "", "readers")),
	}
	cluster := newFakeCluster(t, objects)
	output, err := installDryRun(t, helm, cluster.kubeconfig(t), namespace,
		[]string{"serviceAccount.create=false", "serviceAccount.name=external-controller"})
	if err != nil {
		t.Fatalf("the install was refused: %v\n%s", err, tail(output))
	}
	if got := warnings(t, string(output)); len(got) != 0 {
		t.Fatalf("the notes warn about the release's own grants: %q", got)
	}
	// The same fixture with a ServiceAccount outside the base name is
	// warned about, so the quiet above is the filter at work.
	objects = append(objects,
		roleBinding(namespace, "impostor", "Role", "pod-creator", subject("ServiceAccount", namespace, "external-controller-v1-impostor")))
	cluster = newFakeCluster(t, objects)
	output, err = installDryRun(t, helm, cluster.kubeconfig(t), namespace,
		[]string{"serviceAccount.create=false", "serviceAccount.name=external-controller"})
	if err != nil {
		t.Fatalf("the install was refused: %v\n%s", err, tail(output))
	}
	assertSameLines(t, warnings(t, string(output)), []string{
		"RoleBinding ptah-system/impostor grants create on pods through Role pod-creator to ServiceAccount ptah-system/external-controller-v1-impostor",
	})
}

// warnings reads the list NOTES.txt prints under its WARNING paragraph. A
// NOTES section that is missing entirely is a failure: a render that printed
// no notes cannot say it found nothing.
func warnings(t *testing.T, output string) []string {
	t.Helper()
	_, notes, found := strings.Cut(output, "NOTES:\n")
	if !found {
		t.Fatalf("the dry run printed no NOTES:\n%s", tail([]byte(output)))
	}
	if !strings.Contains(notes, "exact-plan approvals") {
		t.Fatalf("the NOTES are not the chart's:\n%s", notes)
	}
	_, warning, found := strings.Cut(notes, "WARNING:")
	if !found {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(warning, "\n") {
		if entry, ok := strings.CutPrefix(line, "  - "); ok {
			lines = append(lines, entry)
		}
	}
	if len(lines) == 0 {
		t.Fatalf("the NOTES carry the warning paragraph and no binding:\n%s", notes)
	}
	return lines
}

func assertSameLines(t *testing.T, got, want []string) {
	t.Helper()
	remaining := map[string]int{}
	for _, line := range want {
		remaining[line]++
	}
	for _, line := range got {
		if remaining[line] == 0 {
			t.Errorf("unexpected warning: %s", line)
			continue
		}
		remaining[line]--
	}
	for line, count := range remaining {
		if count > 0 {
			t.Errorf("missing warning: %s", line)
		}
	}
}

// object is one API object the fake cluster serves, keyed the way the
// dynamic client addresses it.
type object struct {
	groupVersion, resource, namespace, name string
	body                                    map[string]any
}

func workload(kind, namespace, name, instance string) object {
	for _, candidate := range workloadKinds {
		if candidate.kind != kind {
			continue
		}
		metadata := map[string]any{"name": name, "namespace": namespace}
		if instance != "" {
			metadata["labels"] = map[string]any{"app.kubernetes.io/instance": instance}
		}
		return object{
			groupVersion: candidate.groupVersion, resource: candidate.resource, namespace: namespace, name: name,
			body: map[string]any{"apiVersion": candidate.groupVersion, "kind": kind, "metadata": metadata},
		}
	}
	panic("unknown workload kind " + kind)
}

func everyKind(namespace, instance string) []object {
	objects := make([]object, 0, len(workloadKinds))
	for _, kind := range workloadKinds {
		objects = append(objects, workload(kind.kind, namespace, "foreign-"+strings.ToLower(kind.kind), instance))
	}
	return objects
}

func serviceAccount(namespace, name, instance string) object {
	metadata := map[string]any{"name": name, "namespace": namespace}
	if instance != "" {
		metadata["labels"] = map[string]any{"app.kubernetes.io/instance": instance}
	}
	return object{
		groupVersion: "v1", resource: "serviceaccounts", namespace: namespace, name: name,
		body: map[string]any{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": metadata},
	}
}

func rule(groups, resources, verbs, names []string) map[string]any {
	result := map[string]any{"apiGroups": groups, "resources": resources, "verbs": verbs}
	if names != nil {
		result["resourceNames"] = names
	}
	return result
}

func role(namespace, name string, rules ...map[string]any) object {
	return object{
		groupVersion: "rbac.authorization.k8s.io/v1", resource: "roles", namespace: namespace, name: name,
		body: map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
			"metadata": map[string]any{"name": name, "namespace": namespace},
			"rules":    rules,
		},
	}
}

func clusterRole(name string, rules ...map[string]any) object {
	return object{
		groupVersion: "rbac.authorization.k8s.io/v1", resource: "clusterroles", name: name,
		body: map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
			"metadata": map[string]any{"name": name},
			"rules":    rules,
		},
	}
}

func aggregatedClusterRole(name string, rules ...map[string]any) object {
	role := clusterRole(name, rules...)
	role.body["aggregationRule"] = map[string]any{
		"clusterRoleSelectors": []any{map[string]any{"matchLabels": map[string]any{"example.invalid/aggregate-to-" + name: "true"}}},
	}
	return role
}

func subject(kind, namespace, name string) map[string]any {
	result := map[string]any{"kind": kind, "name": name}
	if kind == "ServiceAccount" {
		result["namespace"] = namespace
	} else {
		result["apiGroup"] = "rbac.authorization.k8s.io"
	}
	return result
}

func roleBinding(namespace, name, roleKind, roleName string, subjects ...map[string]any) object {
	return object{
		groupVersion: "rbac.authorization.k8s.io/v1", resource: "rolebindings", namespace: namespace, name: name,
		body: map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
			"metadata": map[string]any{"name": name, "namespace": namespace},
			"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": roleKind, "name": roleName},
			"subjects": subjects,
		},
	}
}

// fakeCluster answers discovery for every kind the chart renders or looks up,
// lists the kinds the release-namespace check and warning read, and gets
// Roles and ClusterRoles by name. Everything else is 404, which Helm's lookup
// reads as absent -- the same answer the chart's other lookups get from
// internal/crdupgrade's fake.
type fakeCluster struct {
	server  *httptest.Server
	objects []object

	mu    sync.Mutex
	lists map[string]int
}

// listable are the resources served as lists. Workloads, ServiceAccounts
// and RoleBindings are what the check and the warning list.
var listable = func() map[string]bool {
	result := map[string]bool{"v1/serviceaccounts": true, "rbac.authorization.k8s.io/v1/rolebindings": true}
	for _, kind := range workloadKinds {
		result[kind.groupVersion+"/"+kind.resource] = true
	}
	return result
}()

// gettable are the resources served by name.
var gettable = map[string]bool{
	"rbac.authorization.k8s.io/v1/roles":        true,
	"rbac.authorization.k8s.io/v1/clusterroles": true,
}

func newFakeCluster(t *testing.T, objects []object) *fakeCluster {
	t.Helper()
	cluster := &fakeCluster{objects: objects, lists: map[string]int{}}
	cluster.server = httptest.NewServer(http.HandlerFunc(cluster.serve))
	t.Cleanup(cluster.server.Close)
	return cluster
}

func (c *fakeCluster) listed(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lists[path]
}

var discovery = map[string][]string{
	"v1": {
		"configmaps/ConfigMap/true", "secrets/Secret/true", "services/Service/true",
		"serviceaccounts/ServiceAccount/true", "pods/Pod/true", "namespaces/Namespace/false",
		"replicationcontrollers/ReplicationController/true",
	},
	"admissionregistration.k8s.io/v1": {
		"validatingadmissionpolicies/ValidatingAdmissionPolicy/false",
		"validatingadmissionpolicybindings/ValidatingAdmissionPolicyBinding/false",
		"mutatingwebhookconfigurations/MutatingWebhookConfiguration/false",
		"validatingwebhookconfigurations/ValidatingWebhookConfiguration/false",
	},
	"apiextensions.k8s.io/v1": {"customresourcedefinitions/CustomResourceDefinition/false"},
	"apps/v1": {
		"deployments/Deployment/true", "statefulsets/StatefulSet/true",
		"daemonsets/DaemonSet/true", "replicasets/ReplicaSet/true",
	},
	"batch/v1":               {"jobs/Job/true", "cronjobs/CronJob/true"},
	"coordination.k8s.io/v1": {"leases/Lease/true"},
	"policy/v1":              {"poddisruptionbudgets/PodDisruptionBudget/true"},
	"rbac.authorization.k8s.io/v1": {
		"clusterroles/ClusterRole/false", "clusterrolebindings/ClusterRoleBinding/false",
		"roles/Role/true", "rolebindings/RoleBinding/true",
	},
}

func (c *fakeCluster) serve(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "application/json")
	path := request.URL.Path
	switch path {
	case "/version":
		writeJSON(response, map[string]any{
			"major": "1", "minor": "35", "gitVersion": "v1.35.0", "gitCommit": "test", "gitTreeState": "clean",
			"buildDate": "2026-01-01T00:00:00Z", "goVersion": "go1.26.0", "compiler": "gc", "platform": "linux/amd64",
		})
		return
	case "/api":
		writeJSON(response, map[string]any{"kind": "APIVersions", "apiVersion": "v1", "versions": []string{"v1"}, "serverAddressByClientCIDRs": []any{}})
		return
	case "/apis":
		var groups []any
		for groupVersion := range discovery {
			group, version, found := strings.Cut(groupVersion, "/")
			if !found {
				continue
			}
			entry := map[string]any{"groupVersion": groupVersion, "version": version}
			groups = append(groups, map[string]any{"name": group, "versions": []any{entry}, "preferredVersion": entry})
		}
		writeJSON(response, map[string]any{"kind": "APIGroupList", "apiVersion": "v1", "groups": groups})
		return
	}
	groupVersion, rest, ok := splitAPIPath(path)
	if !ok {
		notFound(response)
		return
	}
	if rest == "" {
		resources, served := discovery[groupVersion]
		if !served {
			notFound(response)
			return
		}
		var entries []any
		for _, resource := range resources {
			parts := strings.Split(resource, "/")
			entries = append(entries, map[string]any{
				"name": parts[0], "singularName": strings.TrimSuffix(parts[0], "s"),
				"namespaced": parts[2] == "true", "kind": parts[1], "verbs": []string{"get", "list"},
			})
		}
		writeJSON(response, map[string]any{"kind": "APIResourceList", "apiVersion": "v1", "groupVersion": groupVersion, "resources": entries})
		return
	}
	namespace := ""
	if after, found := strings.CutPrefix(rest, "namespaces/"); found {
		var remainder string
		namespace, remainder, found = strings.Cut(after, "/")
		if !found {
			// A Namespace object itself; none exists here.
			notFound(response)
			return
		}
		rest = remainder
	}
	resource, name, _ := strings.Cut(rest, "/")
	key := groupVersion + "/" + resource
	switch {
	case name == "" && listable[key]:
		c.mu.Lock()
		c.lists[path]++
		c.mu.Unlock()
		items := []any{}
		for _, candidate := range c.objects {
			if candidate.groupVersion == groupVersion && candidate.resource == resource && candidate.namespace == namespace {
				items = append(items, candidate.body)
			}
		}
		writeJSON(response, map[string]any{
			"apiVersion": groupVersion, "kind": itemKind(groupVersion, resource) + "List",
			"metadata": map[string]any{"resourceVersion": "1"}, "items": items,
		})
	case name != "" && gettable[key]:
		for _, candidate := range c.objects {
			if candidate.groupVersion == groupVersion && candidate.resource == resource &&
				candidate.namespace == namespace && candidate.name == name {
				writeJSON(response, candidate.body)
				return
			}
		}
		notFound(response)
	default:
		notFound(response)
	}
}

func itemKind(groupVersion, resource string) string {
	for _, entry := range discovery[groupVersion] {
		parts := strings.Split(entry, "/")
		if parts[0] == resource {
			return parts[1]
		}
	}
	return ""
}

// splitAPIPath splits /api/v1/... and /apis/<group>/<version>/... into the
// group version and what follows it.
func splitAPIPath(path string) (groupVersion, rest string, ok bool) {
	if after, found := strings.CutPrefix(path, "/api/v1"); found {
		return "v1", strings.TrimPrefix(after, "/"), true
	}
	after, found := strings.CutPrefix(path, "/apis/")
	if !found {
		return "", "", false
	}
	parts := strings.SplitN(after, "/", 3)
	if len(parts) < 2 {
		return "", "", false
	}
	groupVersion = parts[0] + "/" + parts[1]
	if len(parts) == 3 {
		rest = parts[2]
	}
	return groupVersion, rest, true
}

func writeJSON(response http.ResponseWriter, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		response.WriteHeader(http.StatusInternalServerError)
		return
	}
	_, _ = response.Write(encoded)
}

func notFound(response http.ResponseWriter) {
	response.WriteHeader(http.StatusNotFound)
	_, _ = response.Write([]byte(`{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"not found","reason":"NotFound","code":404}`))
}

func (c *fakeCluster) kubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	config := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: %s
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user:
    token: test
`, c.server.URL)
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// installDryRun is helm install --dry-run=server: it renders with lookup
// answered by the fake cluster, and it prints NOTES, which helm template does
// not.
func installDryRun(t *testing.T, helm, kubeconfig, namespace string, values []string) ([]byte, error) {
	t.Helper()
	arguments := []string{
		"install", releaseName, chartPath(t),
		"--namespace", namespace,
		"--dry-run=server",
		"--disable-openapi-validation",
		"--kubeconfig", kubeconfig,
	}
	arguments = append(arguments, requiredValues()...)
	// --set rather than --set-string: the override is a boolean, and the
	// values schema refuses the string "true".
	for _, value := range values {
		arguments = append(arguments, "--set", value)
	}
	return runHelm(t, helm, arguments...)
}

func runHelm(t *testing.T, helm string, arguments ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, helm, arguments...) //nolint:gosec // Arguments are this test's own.
	home := t.TempDir()
	command.Env = append(os.Environ(),
		"HELM_CACHE_HOME="+filepath.Join(home, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(home, "config"),
		"HELM_DATA_HOME="+filepath.Join(home, "data"),
	)
	return command.CombinedOutput()
}

// requiredValues are the four values the chart refuses to render without.
func requiredValues() []string {
	return []string{
		"--set-string", "image.digest=sha256:" + strings.Repeat("2", 64),
		"--set-string", "execution.executorImage=e2e.invalid/executor@sha256:" + strings.Repeat("0", 64),
		"--set-string", "execution.runnerImage=e2e.invalid/runner@sha256:" + strings.Repeat("1", 64),
		"--set-string", "execution.ptahVersion=e2e-explicit-version",
	}
}

var (
	chartInputs    sync.Once
	chartInputsErr error
)

// chartPath returns the chart directory, and reads every file in it once.
// Helm reads the chart in a child process, which go test's cache cannot see,
// so without that read a change to the chart alone would be answered with a
// cached pass. Files the test process opens itself are cache inputs.
func chartPath(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve the test's path")
	}
	root := filepath.Join(filepath.Dir(filename), "..", "..", "charts", "ptah-operator")
	chartInputs.Do(func() {
		chartInputsErr = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			_, err = os.ReadFile(path) //nolint:gosec // A path this test walked under the repository.
			return err
		})
	})
	if chartInputsErr != nil {
		t.Fatalf("read the chart: %v", chartInputsErr)
	}
	return root
}

func helmOrSkip(t *testing.T) string {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required to render the chart against a fake cluster")
	}
	return helm
}

// tail keeps a failure message readable: a successful dry run prints the
// whole 4 MB manifest.
func tail(output []byte) string {
	const limit = 4000
	if len(output) <= limit {
		return string(output)
	}
	return "...\n" + string(output[len(output)-limit:])
}
