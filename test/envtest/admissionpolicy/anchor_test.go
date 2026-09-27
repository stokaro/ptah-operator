package admissionpolicy_test

import (
	"context"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/stokaro/ptah-operator/test/envtest/internal/policyenv"
)

// TestParameterInformerAnchor measures what the parameter informer anchor is
// for. kube-apiserver reads a policy's ConfigMap parameter through a shared
// informer that it stops once no bound policy names a ConfigMap parameter, and
// never starts again (kubernetes/kubernetes#133827): a policy bound after that
// reads its parameter as it was when the informer stopped. An upgrade replaces
// every hook policy at once, so without a policy that stays bound throughout,
// the release's guards would come back reading a frozen activation state.
//
// Each half runs on an API server of its own, because the half without the
// anchor leaves that server's informer stopped for good. In both, a probe
// policy echoes the parameter it reads; it is removed, the parameter changes,
// and it is bound again. With the chart's anchor installed the probe reads the
// change. Without it the probe reads the parameter from before -- which is the
// row failing under the dropped binding, as every other policy's mutation row
// does. If an API server ever reads the change without the anchor, the
// upstream defect is fixed and this test says so.
func TestParameterInformerAnchor(t *testing.T) {
	plane.Require(t)
	anchor, err := env.Chart.Policy("ptah-operator-parameter-informer-anchor")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := env.Chart.Binding(anchor.Name)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name   string
		anchor bool
		want   string
	}{
		{name: "with the anchor bound, a returning policy reads the current parameter", anchor: true, want: "after"},
		{name: "without it, a returning policy reads the parameter the informer froze", anchor: false, want: "before"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := plane.Start(t, &envtest.Environment{})
			scheme, err := policyenv.Scheme()
			if err != nil {
				t.Fatal(err)
			}
			api, err := client.New(config, client.Options{Scheme: scheme})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if test.anchor {
				for _, object := range []client.Object{
					&admissionregistrationv1.ValidatingAdmissionPolicy{
						ObjectMeta: metav1.ObjectMeta{Name: anchor.Name}, Spec: *anchor.Spec.DeepCopy(),
					},
					&admissionregistrationv1.ValidatingAdmissionPolicyBinding{
						ObjectMeta: metav1.ObjectMeta{Name: binding.Name}, Spec: *binding.Spec.DeepCopy(),
					},
				} {
					if err := api.Create(ctx, object); err != nil {
						t.Fatalf("install the chart's anchor: %v", err)
					}
				}
			}
			if got := frozenParameterProbe(ctx, t, api); got != test.want {
				t.Fatalf("the returning policy read the parameter %q, want %q", got, test.want)
			}
		})
	}
}

// frozenParameterProbe binds a policy that reads a ConfigMap parameter,
// removes it, changes the parameter, binds it again, and returns the value
// the returning policy reads.
func frozenParameterProbe(ctx context.Context, t *testing.T, api client.Client) string {
	t.Helper()
	const (
		parameter = "anchor-probe-parameter"
		target    = "anchor-probe-target"
		probe     = "anchor-probe"
	)
	for _, object := range []client.Object{
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: parameter}, Data: map[string]string{"value": "before"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: target}},
	} {
		if err := api.Create(ctx, object); err != nil {
			t.Fatalf("create %s: %v", object.GetName(), err)
		}
	}
	deny := admissionregistrationv1.DenyAction
	policy := func() *admissionregistrationv1.ValidatingAdmissionPolicy {
		return &admissionregistrationv1.ValidatingAdmissionPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: probe},
			Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
				ParamKind: &admissionregistrationv1.ParamKind{APIVersion: "v1", Kind: "ConfigMap"},
				MatchConstraints: &admissionregistrationv1.MatchResources{
					ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
						ResourceNames: []string{target},
						RuleWithOperations: admissionregistrationv1.RuleWithOperations{
							Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
							Rule: admissionregistrationv1.Rule{
								APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"configmaps"},
							},
						},
					}},
				},
				Validations: []admissionregistrationv1.Validation{{
					Expression:        "false",
					MessageExpression: `"parameter=" + params.data["value"]`,
				}},
			},
		}
	}
	binding := func() *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
		return &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
			ObjectMeta: metav1.ObjectMeta{Name: probe},
			Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
				PolicyName: probe,
				ParamRef: &admissionregistrationv1.ParamRef{
					Name: parameter, Namespace: "default", ParameterNotFoundAction: &deny,
				},
				ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
			},
		}
	}
	// read updates the target as a dry run and returns what the probe said,
	// or "" while the probe does not answer.
	read := func() string {
		object := &corev1.ConfigMap{}
		if err := api.Get(ctx, types.NamespacedName{Namespace: "default", Name: target}, object); err != nil {
			t.Fatalf("read %s: %v", target, err)
		}
		err := api.Update(ctx, object, client.DryRunAll)
		verdict := policyenv.Decide(err)
		if verdict.Policy != probe {
			return ""
		}
		value, found := strings.CutPrefix(verdict.Message, "parameter=")
		if !found {
			return verdict.Message
		}
		return value
	}
	await := func(what string, done func(string) bool) string {
		deadline := time.Now().Add(20 * time.Second)
		for {
			value := read()
			if done(value) {
				return value
			}
			if time.Now().After(deadline) {
				t.Fatalf("waited 20s for %s; the probe last said %q", what, value)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	create := func() {
		for _, object := range []client.Object{policy(), binding()} {
			if err := api.Create(ctx, object); err != nil {
				t.Fatalf("bind the probe: %v", err)
			}
		}
	}
	create()
	await("the probe to read its parameter", func(value string) bool { return value == "before" })

	for _, object := range []client.Object{binding(), policy()} {
		if err := api.Delete(ctx, object); err != nil {
			t.Fatalf("remove the probe: %v", err)
		}
	}
	// The API server drops a policy and stops parameter informers nothing
	// names any more in the same pass, so once the probe stops answering, the
	// informer has stopped too -- when nothing else keeps it alive.
	await("the probe's removal to reach admission", func(value string) bool { return value == "" })

	parameterObject := &corev1.ConfigMap{}
	if err := api.Get(ctx, types.NamespacedName{Namespace: "default", Name: parameter}, parameterObject); err != nil {
		t.Fatalf("read %s: %v", parameter, err)
	}
	parameterObject.Data["value"] = "after"
	if err := api.Update(ctx, parameterObject); err != nil {
		t.Fatalf("change %s: %v", parameter, err)
	}

	create()
	answer := await("the returning probe to answer", func(value string) bool { return value != "" })
	if answer == "before" {
		// A live informer delivers the change within milliseconds; give it a
		// generous bound before calling the parameter frozen.
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && answer == "before" {
			time.Sleep(100 * time.Millisecond)
			answer = read()
		}
	}
	return answer
}
