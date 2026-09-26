package crdupgrade

import (
	"context"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
)

// A release that succeeded another moves its controller bindings from the
// predecessor's ServiceAccount to its own in a fixed order, and an uninstall
// can meet that move anywhere along the way. Every prefix of it is a state
// teardown accepts, including one where some bindings are already gone.
//
// These tests share the white-box privilege teardown fixture: the property is
// the unexported relationship between binding deletion and the cutover order,
// not a public API shape.
func TestPrivilegeTeardownAcceptsEveryCutoverPrefixAndPartialDeletion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		cursor        int
		deleteCluster bool
		deleteCoord   bool
		deleteRuntime bool
	}{
		{name: "every binding still names the predecessor", cursor: 0},
		{name: "cluster binding moved", cursor: 1},
		{name: "runtime-admission binding moved", cursor: 2},
		{name: "coordination binding moved", cursor: 3},
		{name: "every binding moved", cursor: 4},
		{name: "predecessor state with cluster binding already deleted", cursor: 0, deleteCluster: true},
		{name: "prefix with coordination binding already deleted", cursor: 1, deleteCoord: true},
		{name: "prefix with runtime-admission binding already deleted", cursor: 1, deleteRuntime: true},
		{name: "every core binding already deleted", cursor: 4, deleteCluster: true, deleteCoord: true, deleteRuntime: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := privilegeTeardownCutoverFixture(t, test.cursor)
			controller := fixture.guard.ControllerDeploymentName
			if test.deleteCluster {
				delete(fixture.clusterBindings.objects, controller)
			}
			if test.deleteCoord {
				delete(fixture.roleBindings.objects, privilegeBindingKey(fixture.guard.CoordinationNamespace, controller))
			}
			if test.deleteRuntime {
				delete(fixture.roleBindings.objects, privilegeBindingKey(fixture.guard.ReleaseNamespace, controller+"-runtime-admission"))
			}

			if err := fixture.teardown.Teardown(context.Background()); err != nil {
				t.Fatalf("Teardown() error = %v", err)
			}
			if fixture.clusterBindings.objects[controller] != nil {
				t.Fatal("teardown left the controller ClusterRoleBinding behind")
			}
			for _, key := range []string{
				privilegeBindingKey(fixture.guard.CoordinationNamespace, controller),
				privilegeBindingKey(fixture.guard.ReleaseNamespace, controller+"-runtime-admission"),
				privilegeBindingKey(corev1.NamespaceDefault, ControllerDiscoveryBindingName(controller)),
			} {
				if fixture.roleBindings.objects[key] != nil {
					t.Fatalf("teardown left the controller RoleBinding %s behind", key)
				}
			}
		})
	}
}

func TestPrivilegeTeardownRejectsInvalidCutoverStatesBeforeMutation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		cursor int
		mutate func(*privilegeTeardownFixture)
		want   string
	}{
		{
			name: "coordination binding moved before the cluster binding",
			mutate: func(f *privilegeTeardownFixture) {
				f.setControllerBindingSubject(false, f.guard.CoordinationNamespace, f.guard.ControllerDeploymentName, true)
			},
			want: "valid candidate prefix",
		},
		{
			name:   "discovery binding moved before the coordination binding",
			cursor: 2,
			mutate: func(f *privilegeTeardownFixture) {
				f.setControllerBindingSubject(false, corev1.NamespaceDefault, ControllerDiscoveryBindingName(f.guard.ControllerDeploymentName), true)
			},
			want: "valid candidate prefix",
		},
		{
			name:   "runtime-admission binding names neither controller",
			cursor: 1,
			mutate: func(f *privilegeTeardownFixture) {
				binding := f.roleBindings.objects[privilegeBindingKey(
					f.guard.ReleaseNamespace,
					f.guard.ControllerDeploymentName+"-runtime-admission",
				)]
				binding.Subjects = []rbacv1.Subject{privilegeServiceAccountSubject(f.guard.ReleaseNamespace, "someone-else")}
			},
			want: "exact candidate or predecessor controller subject contract",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := privilegeTeardownCutoverFixture(t, test.cursor)
			test.mutate(fixture)
			err := fixture.teardown.Teardown(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Teardown() error = %v, want containing %q", err, test.want)
			}
			if len(fixture.events) != 0 {
				t.Fatalf("invalid cutover state caused mutations: %v", fixture.events)
			}
		})
	}
}

// privilegeTeardownCutoverFixture is a release at sequence 2 that succeeded
// sequence 1, with its first cursor controller bindings, in cutover order,
// already moved to the candidate. The predecessor's ServiceAccount is gone, as
// every completed cutover leaves it.
func privilegeTeardownCutoverFixture(t *testing.T, cursor int) *privilegeTeardownFixture {
	t.Helper()
	fixture := newPrivilegeTeardownFixtureAt(t, true, true, "ptah-coordination", 2, true)
	delete(fixture.serviceAccounts.objects, fixture.guard.PreviousControllerServiceAccountName)
	var controllerBindings []privilegeBindingContract
	for _, contract := range fixture.teardown.bindingContracts() {
		if contract.controllerBinding {
			controllerBindings = append(controllerBindings, contract)
		}
	}
	if len(controllerBindings) != 4 {
		t.Fatalf("controller bindings = %d, want the four a cutover moves", len(controllerBindings))
	}
	sort.Slice(controllerBindings, func(i, j int) bool {
		return controllerBindings[i].controllerOrder < controllerBindings[j].controllerOrder
	})
	for index, contract := range controllerBindings {
		fixture.setControllerBindingSubject(contract.cluster, contract.namespace, contract.name, index < cursor)
	}
	return fixture
}

func (f *privilegeTeardownFixture) setControllerBindingSubject(
	cluster bool,
	namespace, name string,
	candidate bool,
) {
	for _, contract := range f.teardown.bindingContracts() {
		if contract.cluster != cluster || contract.namespace != namespace || contract.name != name {
			continue
		}
		subject := contract.subject
		if !candidate {
			subject = *contract.predecessorSubject
		}
		subjects := append([]rbacv1.Subject{subject}, contract.fixedSubjects...)
		if cluster {
			f.clusterBindings.objects[name].Subjects = subjects
			return
		}
		f.roleBindings.objects[privilegeBindingKey(namespace, name)].Subjects = subjects
		return
	}
	panic("no controller binding contract named " + namespace + "/" + name)
}
