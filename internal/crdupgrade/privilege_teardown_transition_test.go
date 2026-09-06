package crdupgrade

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// These tests intentionally share the white-box privilege teardown fixture:
// the security property is the unexported relationship between optional
// binding deletion and the controller cutover prefix, not a public API shape.
func TestPrivilegeTeardownAcceptsEveryLegacyCutoverStateAndPartialDeletion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		cursor           int
		deleteCluster    bool
		deleteCoord      bool
		deleteRuntime    bool
		deletePrevious   bool
		externalMetadata bool
		legacyRoleRules  bool
	}{
		{name: "all old", cursor: 0, deleteRuntime: true, legacyRoleRules: true},
		{name: "first core binding moved", cursor: 1, deleteRuntime: true, legacyRoleRules: true},
		{name: "all core bindings moved before ordinary apply", cursor: 2, deleteRuntime: true},
		{name: "all new after ordinary apply", cursor: 2},
		{name: "old state with cluster binding already deleted", cursor: 0, deleteCluster: true, deleteRuntime: true, legacyRoleRules: true},
		{name: "prefix with cluster binding already deleted", cursor: 1, deleteCluster: true, deleteRuntime: true, legacyRoleRules: true},
		{name: "prefix with coordination binding already deleted", cursor: 1, deleteCoord: true, deleteRuntime: true, legacyRoleRules: true},
		{name: "new state with cluster binding already deleted", cursor: 2, deleteCluster: true},
		{name: "new state with coordination binding already deleted", cursor: 2, deleteCoord: true},
		{name: "new state with runtime binding already deleted", cursor: 2, deleteRuntime: true},
		{name: "all controller bindings already deleted", cursor: 2, deleteCluster: true, deleteCoord: true, deleteRuntime: true},
		{name: "predecessor ServiceAccount already deleted", cursor: 2, deletePrevious: true},
		{name: "external predecessor metadata retained", cursor: 2, externalMetadata: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := privilegeTeardownLegacyTransitionFixture(t, test.cursor)
			if test.deleteCluster {
				delete(fixture.clusterBindings.objects, fixture.guard.ControllerDeploymentName)
			}
			if test.deleteCoord {
				delete(fixture.roleBindings.objects, privilegeBindingKey(
					fixture.guard.CoordinationNamespace,
					fixture.guard.ControllerDeploymentName,
				))
			}
			if test.deleteRuntime {
				delete(fixture.roleBindings.objects, privilegeBindingKey(
					fixture.guard.ReleaseNamespace,
					fixture.guard.ControllerDeploymentName+"-runtime-admission",
				))
			}
			if test.deletePrevious {
				delete(fixture.serviceAccounts.objects, fixture.guard.PreviousControllerServiceAccountName)
			}
			if test.externalMetadata {
				account := fixture.serviceAccounts.objects[fixture.guard.PreviousControllerServiceAccountName]
				account.Finalizers = []string{"platform.example.test/retain"}
				account.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "platform-owner", UID: "platform-owner-uid"}}
				account.Labels = map[string]string{"app.kubernetes.io/managed-by": "platform-team"}
			}
			if test.legacyRoleRules {
				fixture.clusterRoles.objects[fixture.guard.ControllerDeploymentName].Rules = legacyControllerClusterRoleRules()
			}

			if err := fixture.teardown.Teardown(context.Background()); err != nil {
				t.Fatalf("Teardown() error = %v", err)
			}
			if !test.deletePrevious && fixture.serviceAccounts.objects[fixture.guard.PreviousControllerServiceAccountName] == nil {
				t.Fatal("predecessor ServiceAccount was not retained")
			}
			for _, event := range fixture.events {
				if event == "ServiceAccount/"+fixture.guard.PreviousControllerServiceAccountName {
					t.Fatalf("predecessor ServiceAccount deletion was attempted: %v", fixture.events)
				}
			}
		})
	}
}

func TestPrivilegeTeardownRejectsInvalidLegacyCutoverStatesBeforeMutation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		cursor int
		mutate func(*privilegeTeardownFixture)
		want   string
	}{
		{
			name: "non-prefix core bindings",
			mutate: func(f *privilegeTeardownFixture) {
				f.setControllerBindingSubject(false, f.guard.CoordinationNamespace, f.guard.ControllerDeploymentName, true)
			},
			want: "valid candidate prefix",
		},
		{
			name:   "candidate-only runtime binding before core completion",
			cursor: 1,
			mutate: func(*privilegeTeardownFixture) {},
			want:   "valid candidate prefix",
		},
		{
			name:   "runtime binding has predecessor subject",
			cursor: 2,
			mutate: func(f *privilegeTeardownFixture) {
				binding := f.roleBindings.objects[privilegeBindingKey(
					f.guard.ReleaseNamespace,
					f.guard.ControllerDeploymentName+"-runtime-admission",
				)]
				binding.Subjects = []rbacv1.Subject{privilegeServiceAccountSubject(
					f.guard.ReleaseNamespace,
					f.guard.PreviousControllerServiceAccountName,
				)}
			},
			want: "exact candidate or predecessor controller subject contract",
		},
		{
			name:   "predecessor ServiceAccount UID reused",
			cursor: 2,
			mutate: func(f *privilegeTeardownFixture) {
				f.serviceAccounts.objects[f.guard.PreviousControllerServiceAccountName].UID = "reused-uid"
			},
			want: "UID changed",
		},
		{
			name:   "predecessor ServiceAccount is deleting",
			cursor: 2,
			mutate: func(f *privilegeTeardownFixture) {
				now := metav1.Now()
				f.serviceAccounts.objects[f.guard.PreviousControllerServiceAccountName].DeletionTimestamp = &now
			},
			want: "deletion is already in progress",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := privilegeTeardownLegacyTransitionFixture(t, test.cursor)
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

func privilegeTeardownLegacyTransitionFixture(t *testing.T, cursor int) *privilegeTeardownFixture {
	t.Helper()
	fixture := newPrivilegeTeardownFixture(t, true, true)
	fixture.guard.PreviousControllerServiceAccountName = "previous-controller"
	fixture.guard.PreviousControllerServiceAccountUID = "previous-controller-uid"
	fixture.guard.PreviousControllerServiceAccountManaged = false
	fixture.guard.PreviousControllerReleaseSequence = 0
	fixture.serviceAccounts.objects[fixture.guard.PreviousControllerServiceAccountName] = &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:            fixture.guard.PreviousControllerServiceAccountName,
			Namespace:       fixture.guard.ReleaseNamespace,
			UID:             fixture.guard.PreviousControllerServiceAccountUID,
			ResourceVersion: "previous-rv",
		},
	}
	for _, contract := range fixture.teardown.authorizationContracts() {
		if contract.cluster {
			if role := fixture.clusterRoles.objects[contract.name]; role != nil {
				role.Rules = append([]rbacv1.PolicyRule(nil), contract.rules...)
			}
			continue
		}
		if role := fixture.roles.objects[privilegeBindingKey(contract.namespace, contract.name)]; role != nil {
			role.Rules = append([]rbacv1.PolicyRule(nil), contract.rules...)
		}
	}
	fixture.setControllerBindingSubject(true, "", fixture.guard.ControllerDeploymentName, cursor >= 1)
	fixture.setControllerBindingSubject(false, fixture.guard.CoordinationNamespace, fixture.guard.ControllerDeploymentName, cursor >= 2)
	return fixture
}

func (f *privilegeTeardownFixture) setControllerBindingSubject(
	cluster bool,
	namespace, name string,
	candidate bool,
) {
	serviceAccount := f.guard.PreviousControllerServiceAccountName
	if candidate {
		serviceAccount = f.guard.ControllerServiceAccountName
	}
	subjects := []rbacv1.Subject{privilegeServiceAccountSubject(f.guard.ReleaseNamespace, serviceAccount)}
	if cluster {
		f.clusterBindings.objects[name].Subjects = subjects
		return
	}
	f.roleBindings.objects[privilegeBindingKey(namespace, name)].Subjects = subjects
}
