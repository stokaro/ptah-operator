// Package policyenv installs the chart's admission policies into an envtest
// API server in the state a completed install leaves them, and decides
// requests against them: which policy refused a request, and with what.
//
// Every name the suite relies on -- ServiceAccounts, Deployments and
// policies -- is read out of the rendered chart rather than written down here,
// so a renamed identity or a new digest moves the suite with the chart instead
// of leaving it asserting the old one.
package policyenv

import (
	"context"
	"fmt"
	"sort"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/stokaro/ptah-operator/test/envtest/internal/harness"
)

const (
	hookAnnotation       = "helm.sh/hook"
	webhookConfiguration = "ptah-operator-admission"
)

// Chart is one rendered release, sorted into what a completed install leaves
// in the cluster.
type Chart struct {
	Release harness.Release
	// Rendered is every document helm printed.
	Rendered []*unstructured.Unstructured
	// Installed are the objects applied before any policy: the identities and
	// their RBAC, the runtime Deployments and the webhook configurations.
	// Hook objects are not among them: Helm deletes each one once its hook
	// succeeds, so a completed install leaves none.
	Installed []*unstructured.Unstructured
	// Policies and Bindings are the admission policies an install leaves
	// bound. Each is an ordinary release object; the chart renders none as a
	// hook.
	Policies []*admissionregistrationv1.ValidatingAdmissionPolicy
	Bindings []*admissionregistrationv1.ValidatingAdmissionPolicyBinding
	Names    Names
}

// Names are the identities and objects the policies are written against.
type Names struct {
	Namespace string
	// Manager is the manager's ServiceAccount, in Namespace.
	Manager string
	// ManagerDeployment is the Deployment the manager runs in, in Namespace.
	ManagerDeployment string
	// WebhookConfiguration names both the mutating and the validating one.
	WebhookConfiguration string
}

// Render renders release and sorts it.
func Render(ctx context.Context, release harness.Release) (*Chart, error) {
	rendered, err := harness.RenderChart(ctx, release)
	if err != nil {
		return nil, err
	}
	chart := &Chart{Release: release, Rendered: rendered}
	if err := chart.sort(); err != nil {
		return nil, err
	}
	if err := chart.name(); err != nil {
		return nil, err
	}
	return chart, nil
}

// hook reports whether Helm runs object as a hook rather than keeping it as
// part of the release.
func hook(object *unstructured.Unstructured) bool {
	return object.GetAnnotations()[hookAnnotation] != ""
}

func (chart *Chart) sort() error {
	seen := map[string]bool{}
	for _, object := range chart.Rendered {
		key := object.GetKind() + "/" + object.GetNamespace() + "/" + object.GetName()
		switch object.GetKind() {
		case "ValidatingAdmissionPolicy":
			// A policy rendered as a hook is created and deleted around a Helm
			// operation, and a completed install leaves it unbound or absent.
			if hook(object) {
				return fmt.Errorf("the chart renders policy %s as a hook", object.GetName())
			}
			policy := &admissionregistrationv1.ValidatingAdmissionPolicy{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, policy); err != nil {
				return fmt.Errorf("decode policy %s: %w", object.GetName(), err)
			}
			if seen[key] {
				return fmt.Errorf("the chart renders policy %s twice", object.GetName())
			}
			seen[key] = true
			chart.Policies = append(chart.Policies, policy)
		case "ValidatingAdmissionPolicyBinding":
			if hook(object) {
				return fmt.Errorf("the chart renders binding %s as a hook", object.GetName())
			}
			binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, binding); err != nil {
				return fmt.Errorf("decode binding %s: %w", object.GetName(), err)
			}
			if seen[key] {
				return fmt.Errorf("the chart renders binding %s twice", object.GetName())
			}
			seen[key] = true
			chart.Bindings = append(chart.Bindings, binding)
		case "CustomResourceDefinition":
			// envtest installs config/crd/bases, which verify-source holds
			// byte for byte equal to what the chart ships.
		case "Job":
			// Hook Jobs run once and are gone; a row that needs one creates it.
		default:
			if hook(object) {
				continue
			}
			if seen[key] {
				return fmt.Errorf("the chart renders %s twice", key)
			}
			seen[key] = true
			chart.Installed = append(chart.Installed, object.DeepCopy())
		}
	}
	sort.Slice(chart.Policies, func(i, j int) bool { return chart.Policies[i].Name < chart.Policies[j].Name })
	sort.Slice(chart.Bindings, func(i, j int) bool { return chart.Bindings[i].Name < chart.Bindings[j].Name })
	if len(chart.Policies) == 0 || len(chart.Policies) != len(chart.Bindings) {
		return fmt.Errorf("the chart renders %d policies and %d bindings", len(chart.Policies), len(chart.Bindings))
	}
	return nil
}

// managerComponent labels the manager's Deployment apart from the certificate
// rotator's.
const managerComponent = "controller"

func (chart *Chart) name() error {
	names := Names{
		Namespace:            chart.Release.Namespace,
		WebhookConfiguration: webhookConfiguration,
	}
	for _, object := range chart.Rendered {
		if object.GetKind() != "Deployment" || object.GetLabels()["app.kubernetes.io/component"] != managerComponent {
			continue
		}
		deployment := &appsv1.Deployment{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, deployment); err != nil {
			return fmt.Errorf("decode Deployment %s: %w", object.GetName(), err)
		}
		if names.ManagerDeployment != "" {
			return fmt.Errorf("the chart renders two manager Deployments, %s and %s", names.ManagerDeployment, deployment.Name)
		}
		names.ManagerDeployment, names.Manager = deployment.Name, deployment.Spec.Template.Spec.ServiceAccountName
	}
	for field, value := range map[string]string{
		"manager ServiceAccount": names.Manager,
		"manager Deployment":     names.ManagerDeployment,
	} {
		if value == "" {
			return fmt.Errorf("the rendered chart names no %s", field)
		}
	}
	chart.Names = names
	return nil
}

// Policy returns the policy whose name starts with prefix. The prefix is the
// stable part of a name; the chart appends a release digest.
func (chart *Chart) Policy(prefix string) (*admissionregistrationv1.ValidatingAdmissionPolicy, error) {
	var found *admissionregistrationv1.ValidatingAdmissionPolicy
	for _, policy := range chart.Policies {
		if strings.HasPrefix(policy.Name, prefix) {
			if found != nil {
				return nil, fmt.Errorf("both %s and %s start with %q", found.Name, policy.Name, prefix)
			}
			found = policy
		}
	}
	if found == nil {
		return nil, fmt.Errorf("the chart renders no policy starting with %q", prefix)
	}
	return found, nil
}

// Binding returns the binding of the named policy. Each policy has one.
func (chart *Chart) Binding(policy string) (*admissionregistrationv1.ValidatingAdmissionPolicyBinding, error) {
	var found *admissionregistrationv1.ValidatingAdmissionPolicyBinding
	for _, binding := range chart.Bindings {
		if binding.Spec.PolicyName == policy {
			if found != nil {
				return nil, fmt.Errorf("policy %s has more than one binding", policy)
			}
			found = binding
		}
	}
	if found == nil {
		return nil, fmt.Errorf("policy %s has no binding", policy)
	}
	return found, nil
}

// Object returns the rendered object of kind named name.
func (chart *Chart) Object(kind, name string) (*unstructured.Unstructured, error) {
	for _, object := range chart.Rendered {
		if object.GetKind() == kind && object.GetName() == name {
			return object.DeepCopy(), nil
		}
	}
	return nil, fmt.Errorf("the chart renders no %s %s", kind, name)
}
