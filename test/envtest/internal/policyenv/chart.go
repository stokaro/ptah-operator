// Package policyenv installs the chart's admission policies into an envtest
// API server in the state a completed install leaves them, and decides
// requests against them: which policy refused a request, and with what.
//
// Every name the suite relies on -- ServiceAccounts, Deployments, parameters
// and policies -- is read out of the rendered chart rather than written down
// here, so a renamed identity or a new digest moves the suite with the chart
// instead of leaving it asserting the old one.
package policyenv

import (
	"context"
	"fmt"
	"regexp"
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
	activationName       = "ptah-operator-release-activation"
	activeSequenceKey    = "active-release-sequence"
	releaseSequenceKey   = "operator.ptah.run/release-sequence"
	webhookConfiguration = "ptah-operator-admission"
)

// Chart is one rendered release, sorted into what a completed install leaves
// in the cluster.
type Chart struct {
	Release harness.Release
	// Rendered is every document helm printed.
	Rendered []*unstructured.Unstructured
	// Installed are the objects applied before any policy: the identities and
	// their RBAC, the parameters and markers the policies read, the runtime
	// Deployments and the webhook configurations.
	Installed []*unstructured.Unstructured
	// Policies and Bindings are the admission policies an install leaves
	// bound: the pre-install and pre-upgrade hook forms Helm keeps, and the
	// ordinary release objects. The pre-delete forms replace them only during
	// an uninstall and are not installed.
	Policies []*admissionregistrationv1.ValidatingAdmissionPolicy
	Bindings []*admissionregistrationv1.ValidatingAdmissionPolicyBinding
	Names    Names
}

// Names are the identities and objects the policies are written against.
type Names struct {
	Namespace string
	// ServiceAccount names, all in Namespace.
	Manager     string
	Certificate string
	Hook        string
	// Deployments, in Namespace.
	ManagerDeployment     string
	CertificateDeployment string
	// Activation is the ConfigMap every activation-gated guard reads as its
	// parameter, in Namespace.
	Activation string
	// WebhookConfiguration names both the mutating and the validating one.
	WebhookConfiguration string
	// ReleaseSequence is the sequence the chart renders, which an activated
	// install records as active.
	ReleaseSequence string
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

func installTime(object *unstructured.Unstructured) bool {
	hook := object.GetAnnotations()[hookAnnotation]
	return hook == "" || strings.Contains(hook, "pre-install")
}

func (chart *Chart) sort() error {
	seen := map[string]bool{}
	for _, object := range chart.Rendered {
		key := object.GetKind() + "/" + object.GetNamespace() + "/" + object.GetName()
		switch object.GetKind() {
		case "ValidatingAdmissionPolicy":
			if !installTime(object) {
				continue
			}
			policy := &admissionregistrationv1.ValidatingAdmissionPolicy{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, policy); err != nil {
				return fmt.Errorf("decode policy %s: %w", object.GetName(), err)
			}
			if seen[key] {
				return fmt.Errorf("the chart renders install-time policy %s twice", object.GetName())
			}
			seen[key] = true
			chart.Policies = append(chart.Policies, policy)
		case "ValidatingAdmissionPolicyBinding":
			if !installTime(object) {
				continue
			}
			binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, binding); err != nil {
				return fmt.Errorf("decode binding %s: %w", object.GetName(), err)
			}
			if seen[key] {
				return fmt.Errorf("the chart renders install-time binding %s twice", object.GetName())
			}
			seen[key] = true
			chart.Bindings = append(chart.Bindings, binding)
		case "CustomResourceDefinition":
			// envtest installs config/crd/bases, which verify-source holds
			// byte for byte equal to what the chart ships.
		case "Job":
			// Hook Jobs run once and are gone; a row that needs one creates it.
		default:
			if !installTime(object) {
				continue
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			chart.Installed = append(chart.Installed, object.DeepCopy())
		}
	}
	sort.Slice(chart.Policies, func(i, j int) bool { return chart.Policies[i].Name < chart.Policies[j].Name })
	sort.Slice(chart.Bindings, func(i, j int) bool { return chart.Bindings[i].Name < chart.Bindings[j].Name })
	if len(chart.Policies) == 0 || len(chart.Policies) != len(chart.Bindings) {
		return fmt.Errorf("the chart renders %d install-time policies and %d bindings", len(chart.Policies), len(chart.Bindings))
	}
	return nil
}

var hookServiceAccount = regexp.MustCompile(`-crd-v[1-9][0-9]*-[0-9a-f]{12}$`)

func (chart *Chart) name() error {
	names := Names{
		Namespace:            chart.Release.Namespace,
		Activation:           activationName,
		WebhookConfiguration: webhookConfiguration,
	}
	for _, object := range chart.Rendered {
		switch object.GetKind() {
		case "Deployment":
			deployment := &appsv1.Deployment{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, deployment); err != nil {
				return fmt.Errorf("decode Deployment %s: %w", object.GetName(), err)
			}
			serviceAccount := deployment.Spec.Template.Spec.ServiceAccountName
			if strings.HasSuffix(deployment.Name, "-cert-rotator") {
				names.CertificateDeployment, names.Certificate = deployment.Name, serviceAccount
			} else {
				names.ManagerDeployment, names.Manager = deployment.Name, serviceAccount
			}
		case "ServiceAccount":
			if hookServiceAccount.MatchString(object.GetName()) {
				names.Hook = object.GetName()
			}
		case "ConfigMap":
			if object.GetName() == activationName {
				names.ReleaseSequence = object.GetAnnotations()[releaseSequenceKey]
			}
		}
	}
	for field, value := range map[string]string{
		"manager ServiceAccount":     names.Manager,
		"certificate ServiceAccount": names.Certificate,
		"hook ServiceAccount":        names.Hook,
		"manager Deployment":         names.ManagerDeployment,
		"certificate Deployment":     names.CertificateDeployment,
		"release sequence":           names.ReleaseSequence,
	} {
		if value == "" {
			return fmt.Errorf("the rendered chart names no %s", field)
		}
	}
	chart.Names = names
	return nil
}

// Policy returns the install-time policy whose name starts with prefix. The
// prefix is the stable part of a name; the chart appends a release digest.
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
		return nil, fmt.Errorf("the chart renders no install-time policy starting with %q", prefix)
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
		if object.GetKind() == kind && object.GetName() == name && installTime(object) {
			return object.DeepCopy(), nil
		}
	}
	for _, object := range chart.Rendered {
		if object.GetKind() == kind && object.GetName() == name {
			return object.DeepCopy(), nil
		}
	}
	return nil, fmt.Errorf("the chart renders no %s %s", kind, name)
}
