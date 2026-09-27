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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
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
	manager := renderIdentities(t, helm, chartPath(t), releaseName, namespace, nil).manager
	objects := []object{
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
			subject("ServiceAccount", namespace, manager), subject("User", "", "frank")),
		roleBinding(coordination, "leases-for-grace", "Role", "lease-writer", subject("User", "", "grace")),
		roleBinding(coordination, "edit-for-foreign-sa", "ClusterRole", "edit", subject("ServiceAccount", "applications", "worker")),

		// Not warned about: this release's own ServiceAccount, a read-only
		// role, a create the authorizer can never grant by name, a role
		// that grants nothing dangerous, and a namespace the operator does
		// not use.
		roleBinding(namespace, "own-controller", "Role", "pod-creator", subject("ServiceAccount", namespace, manager)),
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

// chartIdentities is the number of distinct ServiceAccounts a release runs
// its Pods as: the manager, the certificate rotator and the CRD manager hook,
// which the uninstall hook runs as too. A render that yields another number is
// a chart that grew or lost an identity, and the warning's list of its own
// names has to follow it.
const chartIdentities = 3

// The warning knows a release's own identities by the names the chart gives
// them, not by the objects: on a retried upgrade the stable coordination
// binding already names this sequence's manager before Helm has created that
// ServiceAccount. So no case here has a ServiceAccount object at all. The
// names come from the chart's own offline render, so this checks what the
// chart runs as rather than a second copy of its naming rules, including where
// a long name forces the helpers to truncate and at a later release sequence.
func TestTheNotesKnowEveryIdentityTheChartRunsAs(t *testing.T) {
	t.Parallel()
	helm := helmOrSkip(t)
	const (
		namespace    = "ptah-system"
		longRelease  = "ptah-runtime-generated-name-prefix-boundary-proof-rel"
		longFullname = "ptah-runtime-generated-name-prefix-boundary-proof-0123456789"
	)
	if len(longRelease) != 53 {
		t.Fatalf("the long release name is %d bytes; Helm's limit is 53 and the case needs all of them", len(longRelease))
	}
	cases := []struct {
		name     string
		release  string
		sequence int
		values   []string
		// fullname is what the chart would call the release if nothing were
		// truncated; truncated says the identities must be shorter.
		fullname  string
		truncated bool
	}{
		{name: "sequence 1", release: releaseName, sequence: 1, fullname: releaseName + "-ptah-operator"},
		{
			name: "a release name that truncates every identity", release: longRelease, sequence: 1,
			fullname: longRelease + "-ptah-operator", truncated: true,
		},
		{
			// The acceptance lifecycle's shape: a 60-byte fullname override,
			// upgraded to the synthetic next release.
			name: "sequence 2 under a truncated fullname override", release: releaseName, sequence: 2,
			values: []string{"fullnameOverride=" + longFullname}, fullname: longFullname, truncated: true,
		},
		{
			name: "external identities at sequence 2", release: releaseName, sequence: 2,
			values:   []string{"serviceAccount.create=false", "serviceAccount.name=external-controller"},
			fullname: releaseName + "-ptah-operator",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			chart := chartAtSequence(t, test.sequence)
			identities := renderIdentities(t, helm, chart, test.release, namespace, test.values)
			if len(identities.names) != chartIdentities {
				t.Fatalf("the chart runs as %d identities, want %d: %q", len(identities.names), chartIdentities, identities.names)
			}
			sequenceMarker := fmt.Sprintf("-v%d", test.sequence)
			if !strings.Contains(identities.manager, sequenceMarker) {
				t.Fatalf("the manager %s does not carry release sequence %d", identities.manager, test.sequence)
			}
			for _, name := range identities.names {
				if len(name) > 63 {
					t.Errorf("identity %s is longer than a ServiceAccount name may be", name)
				}
				if test.truncated && strings.HasPrefix(name, test.fullname) {
					t.Errorf("identity %s carries the whole name %s, so this case does not reach the truncation it is for", name, test.fullname)
				}
			}

			objects := []object{
				role(namespace, "pod-creator", rule([]string{""}, []string{"pods"}, []string{"create"}, nil)),
				role(namespace, "lease-writer", rule([]string{"coordination.k8s.io"}, []string{"leases"}, []string{"create"}, nil)),
				// The coordination binding as a retried upgrade finds it.
				roleBinding(namespace, "coordination", "Role", "lease-writer", subject("ServiceAccount", namespace, identities.manager)),
			}
			for index, name := range identities.names {
				objects = append(objects,
					roleBinding(namespace, fmt.Sprintf("own-%d", index), "Role", "pod-creator", subject("ServiceAccount", namespace, name)))
			}
			// Near misses: the manager at a release sequence this render does
			// not run as, the manager with one more character, and the manager
			// in another namespace.
			otherSequence := strings.Replace(identities.manager, sequenceMarker, fmt.Sprintf("-v%d", test.sequence+1), 1)
			objects = append(objects,
				roleBinding(namespace, "other-sequence", "Role", "pod-creator", subject("ServiceAccount", namespace, otherSequence)),
				roleBinding(namespace, "longer-name", "Role", "pod-creator", subject("ServiceAccount", namespace, identities.manager+"x")),
				roleBinding(namespace, "other-namespace", "Role", "pod-creator", subject("ServiceAccount", "applications", identities.manager)),
			)
			cluster := newFakeCluster(t, objects)
			output, err := installChartDryRun(t, helm, chart, test.release, cluster.kubeconfig(t), namespace, test.values)
			if err != nil {
				t.Fatalf("the install was refused: %v\n%s", err, tail(output))
			}
			assertSameLines(t, warnings(t, string(output)), []string{
				"RoleBinding ptah-system/other-sequence grants create on pods through Role pod-creator to ServiceAccount ptah-system/" + otherSequence,
				"RoleBinding ptah-system/longer-name grants create on pods through Role pod-creator to ServiceAccount ptah-system/" + identities.manager + "x",
				"RoleBinding ptah-system/other-namespace grants create on pods through Role pod-creator to ServiceAccount applications/" + identities.manager,
			})
		})
	}
}

// A retried upgrade to sequence 2 reads the manager it succeeds from the
// service-account-origin guard the first attempt left, and while the bindings
// hand over, that predecessor still holds them. The warning counts it as the
// release's own exactly when the chart knows it as the predecessor: with the
// guard it is quiet, and without the guard the same binding is a stranger's.
func TestTheNotesKnowThePredecessorOnARetriedUpgrade(t *testing.T) {
	t.Parallel()
	helm := helmOrSkip(t)
	const namespace = "ptah-system"
	predecessor := renderIdentities(t, helm, chartAtSequence(t, 1), releaseName, namespace, nil)
	chart := chartAtSequence(t, 2)
	candidate := renderIdentities(t, helm, chart, releaseName, namespace, nil)
	if predecessor.manager == candidate.manager {
		t.Fatalf("sequences 1 and 2 render the same manager %s", candidate.manager)
	}
	guard := candidate.originGuard(t)
	guard.body["metadata"].(map[string]any)["uid"] = "retained-origin-guard"
	annotations := guard.body["metadata"].(map[string]any)["annotations"].(map[string]any)
	annotations["operator.ptah.run/previous-controller-service-account-name"] = predecessor.manager
	annotations["operator.ptah.run/previous-controller-service-account-uid"] = "predecessor-manager"
	annotations["operator.ptah.run/previous-controller-service-account-managed"] = "true"
	annotations["operator.ptah.run/previous-controller-release-sequence"] = "1"
	annotations["operator.ptah.run/previous-controller-manager-image"] = predecessor.managerImage

	bindings := []object{
		role(namespace, "lease-writer", rule([]string{"coordination.k8s.io"}, []string{"leases"}, []string{"create"}, nil)),
		roleBinding(namespace, "coordination", "Role", "lease-writer", subject("ServiceAccount", namespace, predecessor.manager)),
		roleBinding(namespace, "coordination-next", "Role", "lease-writer", subject("ServiceAccount", namespace, candidate.manager)),
	}
	for _, test := range []struct {
		name    string
		objects []object
		want    []string
	}{
		{name: "with the retained guard", objects: append([]object{guard}, bindings...)},
		{
			name:    "without it",
			objects: bindings,
			want: []string{
				"RoleBinding ptah-system/coordination grants create on leases.coordination.k8s.io through Role lease-writer to ServiceAccount ptah-system/" + predecessor.manager,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cluster := newFakeCluster(t, test.objects)
			output, err := installChartDryRun(t, helm, chart, releaseName, cluster.kubeconfig(t), namespace, nil)
			if err != nil {
				t.Fatalf("the install was refused: %v\n%s", err, tail(output))
			}
			assertSameLines(t, warnings(t, string(output)), test.want)
		})
	}
}

// identities is what an offline render of the chart says the release runs as.
type identities struct {
	// manager is the manager Deployment's ServiceAccount, and managerImage its
	// container image.
	manager, managerImage string
	// names are every ServiceAccount a Pod the chart renders runs as.
	names []string
	// policies are the ValidatingAdmissionPolicies the render carries. One
	// name can occur twice: uninstall replaces a guard with a policy of the
	// same name.
	policies []map[string]any
}

// originGuard is the release's service-account-origin guard as the install
// hook creates it, told apart from the uninstall policy of the same name by
// its component.
func (i identities) originGuard(t *testing.T) object {
	t.Helper()
	var found []map[string]any
	for _, policy := range i.policies {
		metadata, _ := policy["metadata"].(map[string]any)
		labels, _ := metadata["labels"].(map[string]any)
		if labels["app.kubernetes.io/component"] == "service-account-origin-guard" {
			found = append(found, policy)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the render carries %d service-account-origin guards, want 1", len(found))
	}
	name, _ := found[0]["metadata"].(map[string]any)["name"].(string)
	return object{
		groupVersion: "admissionregistration.k8s.io/v1", resource: "validatingadmissionpolicies", name: name,
		body: found[0],
	}
}

// renderIdentities renders the chart offline and reads which ServiceAccounts
// its Pods run as. Offline, the names are the ones the helpers derive from the
// release and the values alone, which is also what they are when the render
// can read the cluster.
func renderIdentities(t *testing.T, helm, chart, release, namespace string, values []string) identities {
	t.Helper()
	arguments := append([]string{"template", release, chart, "--namespace", namespace}, requiredValues()...)
	for _, value := range values {
		arguments = append(arguments, "--set", value)
	}
	output, err := runHelm(t, helm, arguments...)
	if err != nil {
		t.Fatalf("render the chart offline: %v\n%s", err, tail(output))
	}
	var result identities
	names := map[string]bool{}
	for _, document := range strings.Split(string(output), "\n---\n") {
		var parsed map[string]any
		if err := yaml.Unmarshal([]byte(document), &parsed); err != nil {
			t.Fatalf("parse a rendered document: %v\n%s", err, document)
		}
		if parsed == nil {
			continue
		}
		metadata, _ := parsed["metadata"].(map[string]any)
		switch parsed["kind"] {
		case "ValidatingAdmissionPolicy":
			result.policies = append(result.policies, parsed)
		case "Deployment":
			labels, _ := metadata["labels"].(map[string]any)
			if labels["app.kubernetes.io/component"] != "controller" {
				break
			}
			spec, _ := parsed["spec"].(map[string]any)
			template, _ := spec["template"].(map[string]any)
			podSpec, _ := template["spec"].(map[string]any)
			result.manager, _ = podSpec["serviceAccountName"].(string)
			containers, _ := podSpec["containers"].([]any)
			for _, container := range containers {
				if entry, ok := container.(map[string]any); ok && entry["name"] == "manager" {
					result.managerImage, _ = entry["image"].(string)
				}
			}
		}
		// Every Pod template names the identity it runs as.
		collectServiceAccountNames(parsed, names)
	}
	if result.manager == "" || result.managerImage == "" {
		t.Fatalf("the render has no manager Deployment with a ServiceAccount and an image")
	}
	for name := range names {
		result.names = append(result.names, name)
	}
	sort.Strings(result.names)
	return result
}

// collectServiceAccountNames adds every serviceAccountName a document's Pod
// templates carry, however deep the template sits.
func collectServiceAccountNames(value any, names map[string]bool) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if name, ok := child.(string); ok && key == "serviceAccountName" && name != "" {
				names[name] = true
				continue
			}
			collectServiceAccountNames(child, names)
		}
	case []any:
		for _, child := range typed {
			collectServiceAccountNames(child, names)
		}
	}
}

// chartAtSequence copies the chart and compiles it at another release
// sequence, the way the acceptance harness builds its synthetic next release.
func chartAtSequence(t *testing.T, sequence int) string {
	t.Helper()
	source := chartPath(t)
	if sequence == 1 {
		return source
	}
	const compiled = `{{- define "ptah-operator.releaseSequence" -}}1{{- end -}}`
	destination := filepath.Join(t.TempDir(), "ptah-operator")
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		content, err := os.ReadFile(path) //nolint:gosec // A path this test walked under the repository.
		if err != nil {
			return err
		}
		if relative == filepath.Join("templates", "_helpers.tpl") {
			if strings.Count(string(content), compiled) != 1 {
				return fmt.Errorf("the chart no longer compiles its release sequence as %s", compiled)
			}
			content = []byte(strings.Replace(string(content), compiled,
				fmt.Sprintf(`{{- define "ptah-operator.releaseSequence" -}}%d{{- end -}}`, sequence), 1))
		}
		return os.WriteFile(target, content, 0o600)
	})
	if err != nil {
		t.Fatalf("compile the chart at release sequence %d: %v", sequence, err)
	}
	return destination
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
// Roles, ClusterRoles and ValidatingAdmissionPolicies by name. Everything else is 404, which Helm's lookup
// reads as absent -- the same answer the chart's other lookups get from
// internal/crdupgrade's fake.
type fakeCluster struct {
	server  *httptest.Server
	objects []object

	mu    sync.Mutex
	lists map[string]int
}

// listable are the resources served as lists: the workloads the check
// lists, and the RoleBindings the warning lists.
var listable = func() map[string]bool {
	result := map[string]bool{"rbac.authorization.k8s.io/v1/rolebindings": true}
	for _, kind := range workloadKinds {
		result[kind.groupVersion+"/"+kind.resource] = true
	}
	return result
}()

// gettable are the resources served by name: the roles a binding names, and
// the retained policy a retried upgrade reads its predecessor from.
var gettable = map[string]bool{
	"rbac.authorization.k8s.io/v1/roles":                          true,
	"rbac.authorization.k8s.io/v1/clusterroles":                   true,
	"admissionregistration.k8s.io/v1/validatingadmissionpolicies": true,
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
	return installChartDryRun(t, helm, chartPath(t), releaseName, kubeconfig, namespace, values)
}

func installChartDryRun(t *testing.T, helm, chart, release, kubeconfig, namespace string, values []string) ([]byte, error) {
	t.Helper()
	arguments := []string{
		"install", release, chart,
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
