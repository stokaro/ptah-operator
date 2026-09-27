package policyenv

import (
	"context"
	"errors"
	"fmt"
	"sync"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/envtest/internal/harness"
)

// WebhookDisabledLabel is the only label the installed webhook entries
// select. Nothing carries it, so the API server never calls them: the chart's
// webhooks point at a Service no envtest process serves, and a webhook that
// fails closed would otherwise refuse every request it matches before a
// policy is asked. The webhook suite serves them for real; here they exist
// because a completed install leaves them.
const WebhookDisabledLabel = "policyenv.envtest.operator.ptah.run/webhook-disabled"

// OrdinaryUser is a person with write access and no admission authority.
// Install grants it exactly the RBAC the rows need, so a request it makes that
// is refused is refused by admission, not by authorization.
const OrdinaryUser = "envtest-ordinary-user"

// Env is a release installed on a running control plane.
type Env struct {
	Chart  *Chart
	Plane  *harness.ControlPlane
	Scheme *runtime.Scheme
	// Admin is the envtest administrator, a member of system:masters. RBAC
	// does not constrain it and the policies still judge it.
	Admin client.Client

	mu      sync.Mutex
	clients map[string]client.Client
}

// Scheme knows the built-in kinds and the operator's own.
func Scheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return scheme, nil
}

// Install puts chart into the state a completed install leaves: every object
// of the release, and the policies bound. It installs the policies last, so
// nothing it creates is judged by them.
func Install(ctx context.Context, plane *harness.ControlPlane, chart *Chart) (*Env, error) {
	scheme, err := Scheme()
	if err != nil {
		return nil, err
	}
	admin, err := client.New(plane.Config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}
	env := &Env{Chart: chart, Plane: plane, Scheme: scheme, Admin: admin, clients: map[string]client.Client{}}

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: chart.Names.Namespace}}
	if err := admin.Create(ctx, namespace); err != nil {
		return nil, fmt.Errorf("create the release namespace: %w", err)
	}
	for _, object := range chart.Installed {
		object = object.DeepCopy()
		if err := chart.prepare(object); err != nil {
			return nil, err
		}
		if err := admin.Create(ctx, object); err != nil {
			return nil, fmt.Errorf("create %s %s/%s: %w", object.GetKind(), object.GetNamespace(), object.GetName(), err)
		}
	}
	for _, object := range fixtureRBAC() {
		if err := admin.Create(ctx, object); err != nil {
			return nil, fmt.Errorf("create %T %s: %w", object, object.GetName(), err)
		}
	}
	for _, policy := range chart.Policies {
		if err := admin.Create(ctx, cleanPolicy(policy)); err != nil {
			return nil, fmt.Errorf("create policy %s: %w", policy.Name, err)
		}
	}
	for _, binding := range chart.Bindings {
		if err := admin.Create(ctx, cleanBinding(binding)); err != nil {
			return nil, fmt.Errorf("create binding %s: %w", binding.Name, err)
		}
	}
	return env, nil
}

// prepare adjusts a rendered object to the installed state: the webhook
// entries select nothing.
func (chart *Chart) prepare(object *unstructured.Unstructured) error {
	switch {
	case object.GetKind() == "MutatingWebhookConfiguration" || object.GetKind() == "ValidatingWebhookConfiguration":
		webhooks, _, err := unstructured.NestedSlice(object.Object, "webhooks")
		if err != nil {
			return err
		}
		for index := range webhooks {
			webhook, ok := webhooks[index].(map[string]any)
			if !ok {
				return fmt.Errorf("%s %s webhook %d is not an object", object.GetKind(), object.GetName(), index)
			}
			webhook["objectSelector"] = map[string]any{
				"matchLabels": map[string]any{WebhookDisabledLabel: "true"},
			}
		}
		return unstructured.SetNestedSlice(object.Object, webhooks, "webhooks")
	}
	return nil
}

// fixtureRBAC is what the suite adds beside the chart's own RBAC: the
// ordinary user's grants.
func fixtureRBAC() []client.Object {
	ordinary := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: OrdinaryUser},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"namespaces"}, Verbs: []string{"get", "delete"}},
			{
				APIGroups: []string{""},
				Resources: []string{"configmaps", "secrets", "serviceaccounts", "pods"},
				Verbs:     []string{"get", "list", "create", "update", "patch", "delete"},
			},
			{
				APIGroups: []string{"apps"},
				Resources: []string{"deployments", "deployments/scale", "replicasets"},
				Verbs:     []string{"get", "create", "update", "patch"},
			},
			{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"get", "create", "update", "patch", "delete"}},
			{
				APIGroups: []string{"operator.ptah.run"},
				Resources: []string{"ptahschemas", "ptahmigrations", "ptahschemaplans", "ptahmigrationplans"},
				Verbs:     []string{"get", "create", "update", "patch"},
			},
			{
				APIGroups: []string{"admissionregistration.k8s.io"},
				Resources: []string{"mutatingwebhookconfigurations", "validatingwebhookconfigurations"},
				Verbs:     []string{"get", "update", "patch"},
			},
		},
	}
	ordinaryBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: OrdinaryUser},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: OrdinaryUser},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: OrdinaryUser}},
	}
	return []client.Object{ordinary, ordinaryBinding}
}

func cleanPolicy(policy *admissionregistrationv1.ValidatingAdmissionPolicy) *admissionregistrationv1.ValidatingAdmissionPolicy {
	return &admissionregistrationv1.ValidatingAdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: policy.Name, Labels: policy.Labels, Annotations: policy.Annotations},
		Spec:       *policy.Spec.DeepCopy(),
	}
}

func cleanBinding(binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	return &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Name: binding.Name, Labels: binding.Labels, Annotations: binding.Annotations},
		Spec:       *binding.Spec.DeepCopy(),
	}
}

// ErrNotInstalled reports a policy or binding the chart does not render.
var ErrNotInstalled = errors.New("not rendered by the chart")

// renderedPolicy returns the chart's own form of the named policy.
func (env *Env) renderedPolicy(name string) (*admissionregistrationv1.ValidatingAdmissionPolicy, error) {
	for _, policy := range env.Chart.Policies {
		if policy.Name == name {
			return policy, nil
		}
	}
	return nil, fmt.Errorf("policy %s: %w", name, ErrNotInstalled)
}
