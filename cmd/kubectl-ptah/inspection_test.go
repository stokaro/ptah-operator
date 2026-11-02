package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// A reader granted get on the resource and nothing else is the reader the
// plugin was written for. The views list approvals and acknowledgments on top
// of that, and a list the API server refuses has to leave the command printing
// the resource and exiting 0.
//
// The API server here is a fake that answers the requests the plugin's real
// client makes -- discovery, the get, the lists -- and refuses the lists it is
// told to with the 403 Status a kube-apiserver sends. Whether a real
// authorizer refuses them for a given Role is RBAC's business, not this
// test's.
func TestReadingSucceedsWhenTheDecisionListsAreForbidden(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name      string
		arguments []string
		forbidden []string
		says      []string
		unsaid    []string
	}{
		{
			name:      "schema, approvals forbidden",
			arguments: []string{"schema", "storefront"},
			forbidden: []string{"ptahschemaapprovals"},
			says: []string{
				"Schema:               application/storefront\n",
				"\nApprovals: not shown; listing ptahschemaapprovals is forbidden to you (RBAC): " +
					`ptahschemaapprovals.operator.ptah.run is forbidden: User "plan-reader" cannot list resource "ptahschemaapprovals"`,
			},
			unsaid: []string{"Approvals: none"},
		},
		{
			// The same server allowing the list: the line above is the
			// refusal's, not something the view prints regardless.
			name:      "schema, approvals allowed",
			arguments: []string{"schema", "storefront"},
			says:      []string{"\nApprovals: none\n"},
			unsaid:    []string{"forbidden"},
		},
		{
			name:      "schema as JSON, approvals forbidden",
			arguments: []string{"schema", "storefront", "-o", "json"},
			forbidden: []string{"ptahschemaapprovals"},
			says:      []string{`"approvalsUnreadable": {` + "\n" + `    "forbidden": true,`},
			unsaid:    []string{`"approvals": {`},
		},
		{
			name:      "migration, both lists forbidden",
			arguments: []string{"migration", "orders"},
			forbidden: []string{"ptahmigrationapprovals", "ptahmigrationrunacknowledgments"},
			says: []string{
				"Migration:            application/orders\n",
				"\nApprovals: not shown; listing ptahmigrationapprovals is forbidden to you (RBAC): ",
				"\nRun acknowledgments: not shown; listing ptahmigrationrunacknowledgments is forbidden to you (RBAC): ",
			},
		},
		{
			name:      "migration as JSON, acknowledgments forbidden",
			arguments: []string{"migration", "orders", "-o", "json"},
			forbidden: []string{"ptahmigrationrunacknowledgments"},
			says: []string{
				`"approvals": {` + "\n" + `    "total": 0,`,
				`"acknowledgmentsUnreadable": {` + "\n" + `    "forbidden": true,`,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := newFakeAPIServer(t, test.forbidden...)
			kubeconfig := writeKubeconfig(t, server.URL)
			var stdout, stderr bytes.Buffer

			arguments := append(append([]string(nil), test.arguments...), "-n", "application", "--kubeconfig", kubeconfig)
			status := run(context.Background(), arguments, &stdout, &stderr)

			if status != exitOK {
				t.Fatalf("run(%v) = %d, want %d\nstdout:\n%s\nstderr:\n%s", arguments, status, exitOK, stdout.String(), stderr.String())
			}
			for _, want := range test.says {
				if !strings.Contains(stdout.String(), want) {
					t.Fatalf("stdout is missing %q:\n%s", want, stdout.String())
				}
			}
			for _, unwanted := range test.unsaid {
				if strings.Contains(stdout.String(), unwanted) {
					t.Fatalf("stdout carries %q:\n%s", unwanted, stdout.String())
				}
			}
			if stderr.Len() != 0 {
				t.Fatalf("a refused list wrote to stderr:\n%s", stderr.String())
			}
			if refused := server.refused(); len(refused) != len(test.forbidden) {
				t.Fatalf("the server refused %v, want one refusal each of %v", refused, test.forbidden)
			}
		})
	}
}

type fakeAPIServer struct {
	*httptest.Server

	mu         sync.Mutex
	refusals   []string
	forbidden  map[string]bool
	namespaced map[string]any
}

func (s *fakeAPIServer) refused() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.refusals...)
}

// newFakeAPIServer serves one schema, one migration and its plan in the
// namespace "application", answers discovery for the operator's group, lists
// every decision kind as empty, and refuses with 403 the lists named.
func newFakeAPIServer(t *testing.T, forbidden ...string) *fakeAPIServer {
	t.Helper()
	server := &fakeAPIServer{forbidden: map[string]bool{}, namespaced: map[string]any{}}
	for _, resource := range forbidden {
		server.forbidden[resource] = true
	}
	plan := &operatorv1alpha1.PtahMigrationPlan{
		TypeMeta:   metav1.TypeMeta{APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "PtahMigrationPlan"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "application", Name: "ptah-mplan-orders", UID: "plan-uid"},
		Spec: operatorv1alpha1.PtahMigrationPlanSpec{
			MigrationRef: operatorv1alpha1.ImmutableObjectReference{Name: "orders", UID: "migration-uid"},
			Fingerprint:  "sha256:" + strings.Repeat("a", 64),
			Migrations:   []operatorv1alpha1.PlannedMigration{{Version: 3, Description: "add the orders index", Checksum: "c"}},
		},
	}
	server.namespaced["ptahschemas/storefront"] = &operatorv1alpha1.PtahSchema{
		TypeMeta:   metav1.TypeMeta{APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "PtahSchema"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "application", Name: "storefront", UID: "schema-uid"},
		Status:     operatorv1alpha1.PtahSchemaStatus{Phase: operatorv1alpha1.PhaseInSync},
	}
	server.namespaced["ptahmigrations/orders"] = &operatorv1alpha1.PtahMigration{
		TypeMeta:   metav1.TypeMeta{APIVersion: operatorv1alpha1.GroupVersion.String(), Kind: "PtahMigration"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "application", Name: "orders", UID: "migration-uid"},
		Status: operatorv1alpha1.PtahMigrationStatus{
			Phase: operatorv1alpha1.MigrationPhaseAwaitingApproval,
			Plan:  &operatorv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID},
		},
	}
	server.namespaced["ptahmigrationplans/"+plan.Name] = plan
	server.Server = httptest.NewServer(http.HandlerFunc(server.serve))
	t.Cleanup(server.Close)
	return server
}

var operatorResources = []struct{ plural, singular, kind string }{
	{"ptahschemas", "ptahschema", "PtahSchema"},
	{"ptahschemaapprovals", "ptahschemaapproval", "PtahSchemaApproval"},
	{"ptahmigrations", "ptahmigration", "PtahMigration"},
	{"ptahmigrationplans", "ptahmigrationplan", "PtahMigrationPlan"},
	{"ptahmigrationapprovals", "ptahmigrationapproval", "PtahMigrationApproval"},
	{"ptahmigrationrunacknowledgments", "ptahmigrationrunacknowledgment", "PtahMigrationRunAcknowledgment"},
}

func (s *fakeAPIServer) serve(w http.ResponseWriter, r *http.Request) {
	group := operatorv1alpha1.GroupVersion
	prefix := "/apis/" + group.String() + "/namespaces/application/"
	switch {
	case r.URL.Path == "/api":
		reply(w, http.StatusOK, metav1.APIVersions{TypeMeta: metav1.TypeMeta{Kind: "APIVersions"}, Versions: []string{"v1"}})
	case r.URL.Path == "/apis":
		version := metav1.GroupVersionForDiscovery{GroupVersion: group.String(), Version: group.Version}
		reply(w, http.StatusOK, metav1.APIGroupList{
			TypeMeta: metav1.TypeMeta{Kind: "APIGroupList", APIVersion: "v1"},
			Groups:   []metav1.APIGroup{{Name: group.Group, Versions: []metav1.GroupVersionForDiscovery{version}, PreferredVersion: version}},
		})
	case r.URL.Path == "/apis/"+group.String():
		resources := metav1.APIResourceList{TypeMeta: metav1.TypeMeta{Kind: "APIResourceList", APIVersion: "v1"}, GroupVersion: group.String()}
		for _, resource := range operatorResources {
			resources.APIResources = append(resources.APIResources, metav1.APIResource{
				Name: resource.plural, SingularName: resource.singular, Namespaced: true, Kind: resource.kind,
				Verbs: metav1.Verbs{"get", "list"},
			})
		}
		reply(w, http.StatusOK, resources)
	case strings.HasPrefix(r.URL.Path, prefix) && r.Method == http.MethodGet:
		path := strings.TrimPrefix(r.URL.Path, prefix)
		if object, ok := s.namespaced[path]; ok {
			reply(w, http.StatusOK, object)
			return
		}
		if strings.Contains(path, "/") {
			replyStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, path+" not found")
			return
		}
		if s.forbidden[path] {
			s.mu.Lock()
			s.refusals = append(s.refusals, path)
			s.mu.Unlock()
			replyStatus(w, http.StatusForbidden, metav1.StatusReasonForbidden, fmt.Sprintf(
				`%s.%s is forbidden: User "plan-reader" cannot list resource %q in API group %q in the namespace "application"`,
				path, group.Group, path, group.Group))
			return
		}
		for _, resource := range operatorResources {
			if resource.plural == path {
				reply(w, http.StatusOK, map[string]any{
					"apiVersion": group.String(), "kind": resource.kind + "List",
					"metadata": map[string]any{}, "items": []any{},
				})
				return
			}
		}
		replyStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, path+" not found")
	default:
		replyStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, r.URL.Path+" not found")
	}
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func replyStatus(w http.ResponseWriter, code int, reason metav1.StatusReason, message string) {
	reply(w, code, metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   metav1.StatusFailure, Message: message, Reason: reason, Code: int32(code),
	})
}

func writeKubeconfig(t *testing.T, server string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	config := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: fake
  cluster:
    server: %s
users:
- name: plan-reader
  user:
    token: not-a-secret
contexts:
- name: fake
  context:
    cluster: fake
    user: plan-reader
current-context: fake
`, server)
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
