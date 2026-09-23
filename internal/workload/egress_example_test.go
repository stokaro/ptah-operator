package workload

import (
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// The security model sends an installation to examples/networkpolicy-egress.yaml
// to restrict what an operation Pod can reach. A Pod no policy selects is not
// isolated at all, so an example that stops selecting one leaves that Pod
// unrestricted while the page says otherwise -- which is what happened to the
// migration family, whose Pods carry a component label none of the policies
// named.
//
// So the example is measured against Pods the real builders produce, both
// ways: every operation Pod is covered by a default-deny policy, and a policy
// grants an operation the registry or the database only where that operation's
// own containers use it. Over-permission fails as loudly as a gap, because a
// least-privilege example that quietly widens is the thing a reader trusts.

const egressExample = "../../examples/networkpolicy-egress.yaml"

// operationPod is one built Pod and what its containers actually reach.
type operationPod struct {
	name      string
	labels    map[string]string
	database  bool
	registry  bool
	component string
}

func builtOperationPods(t *testing.T) []operationPod {
	t.Helper()
	builder := builderFixture()
	schema := schemaFixture()
	plan := planFixture(schema, builder)

	var pods []operationPod
	for _, operation := range []operatorv1alpha1.OperationType{
		operatorv1alpha1.OperationResolve, operatorv1alpha1.OperationVerify,
		operatorv1alpha1.OperationObserve, operatorv1alpha1.OperationPlan,
		operatorv1alpha1.OperationApply,
	} {
		var operationPlan *operatorv1alpha1.PtahSchemaPlan
		if operation == operatorv1alpha1.OperationApply {
			operationPlan = plan.DeepCopy()
		}
		job, err := builder.Build(schema.DeepCopy(), operationFixture(operation), operationPlan)
		if err != nil {
			t.Fatalf("build the schema %s Job: %v", operation, err)
		}
		pods = append(pods, describePod(t, "schema "+string(operation), job.Spec.Template))
	}

	migration := migrationFixture()
	for _, operation := range []operatorv1alpha1.MigrationOperationType{
		operatorv1alpha1.MigrationOperationResolve, operatorv1alpha1.MigrationOperationVerify,
		operatorv1alpha1.MigrationOperationHistory, operatorv1alpha1.MigrationOperationApply,
	} {
		job, err := builder.BuildMigration(migration, migrationOperationFixture(operation), migrationPlanFixture())
		if err != nil {
			t.Fatalf("build the migration %s Job: %v", operation, err)
		}
		pods = append(pods, describePod(t, "migration "+string(operation), job.Spec.Template))
	}
	if len(pods) != 9 {
		t.Fatalf("built %d operation Pods; both families together have nine operations", len(pods))
	}
	return pods
}

// describePod reads what a built Pod reaches from the Pod itself rather than
// from a table beside it.
func describePod(t *testing.T, name string, template corev1.PodTemplateSpec) operationPod {
	t.Helper()
	pod := operationPod{name: name, labels: template.Labels, component: template.Labels[LabelComponent]}
	for _, container := range append(append([]corev1.Container{}, template.Spec.InitContainers...), template.Spec.Containers...) {
		for _, env := range container.Env {
			if env.Name == runner.EnvDatabaseURL {
				pod.database = true
			}
		}
	}
	for _, container := range template.Spec.InitContainers {
		if container.Name == fetchContainerName || container.Name == migrationFetchContainerName {
			pod.registry = true
		}
	}
	for _, container := range template.Spec.Containers {
		for _, env := range container.Env {
			// The operations that talk to the registry from the main process
			// are the ones handed the reference to resolve or read.
			if env.Name == runner.EnvResolvedReference || env.Name == runner.EnvRequestedReference {
				pod.registry = true
			}
		}
	}
	if pod.component == "" {
		t.Fatalf("%s carries no component label, so no policy can select it", name)
	}
	return pod
}

func readEgressPolicies(t *testing.T) []networkingv1.NetworkPolicy {
	t.Helper()
	body, err := os.ReadFile(egressExample) //nolint:gosec // A fixed path in this repository.
	if err != nil {
		t.Fatalf("read %s: %v", egressExample, err)
	}
	var policies []networkingv1.NetworkPolicy
	for _, document := range splitYAML(string(body)) {
		var policy networkingv1.NetworkPolicy
		if err := yaml.Unmarshal([]byte(document), &policy); err != nil {
			t.Fatalf("parse a policy in %s: %v", egressExample, err)
		}
		if policy.Kind != "NetworkPolicy" {
			continue
		}
		policies = append(policies, policy)
	}
	if len(policies) < 6 {
		t.Fatalf("%s carries %d policies; the example was written with more", egressExample, len(policies))
	}
	return policies
}

func splitYAML(body string) []string {
	var documents []string
	for _, document := range strings.Split(body, "\n---\n") {
		if strings.TrimSpace(document) != "" {
			documents = append(documents, document)
		}
	}
	return documents
}

// isRegistryPolicy and isDatabasePolicy read the policy's own name. The
// example names them, and a policy renamed out of these two shapes stops being
// measured -- which the count check above is there to notice.
func isRegistryPolicy(policy networkingv1.NetworkPolicy) bool {
	return strings.HasSuffix(policy.Name, "-registry")
}

func isDatabasePolicy(policy networkingv1.NetworkPolicy) bool {
	return strings.HasSuffix(policy.Name, "-database")
}

func selects(t *testing.T, policy networkingv1.NetworkPolicy, pod operationPod) bool {
	t.Helper()
	selector, err := metav1.LabelSelectorAsSelector(&policy.Spec.PodSelector)
	if err != nil {
		t.Fatalf("policy %s has an unusable selector: %v", policy.Name, err)
	}
	return selector.Matches(labels.Set(pod.labels))
}

// Every operation Pod both families produce is selected by a default-deny
// policy. A Pod none selects is not isolated, which is the failure the
// migration family shipped with.
func TestTheEgressExampleSelectsEveryOperationPod(t *testing.T) {
	t.Parallel()
	policies := readEgressPolicies(t)
	for _, pod := range builtOperationPods(t) {
		denied := false
		for _, policy := range policies {
			if len(policy.Spec.Egress) == 0 && selects(t, policy, pod) {
				denied = true
			}
		}
		if !denied {
			t.Errorf("%s is selected by no default-deny policy, so following this example leaves it unrestricted", pod.name)
		}
	}
}

// A policy opens the registry or the database for an operation only where that
// operation's own containers use it.
func TestTheEgressExampleGrantsOnlyWhatAnOperationUses(t *testing.T) {
	t.Parallel()
	policies := readEgressPolicies(t)
	for _, pod := range builtOperationPods(t) {
		// Policies union, so the question is whether any policy opens the
		// destination, not whether each one does.
		registry, database := false, false
		for _, policy := range policies {
			if len(policy.Spec.Egress) == 0 || !selects(t, policy, pod) {
				continue
			}
			switch {
			case isRegistryPolicy(policy):
				registry = true
			case isDatabasePolicy(policy):
				database = true
			}
		}
		if registry != pod.registry {
			t.Errorf("%s: the example opens the registry=%t, the Pod's containers fetch=%t",
				pod.name, registry, pod.registry)
		}
		if database != pod.database {
			t.Errorf("%s: the example opens the database=%t, the Pod's containers carry a credential=%t",
				pod.name, database, pod.database)
		}
	}
}

// A workload that is not an operation Pod is selected by nothing here. A
// selector wide enough to catch the manager would hand this example authority
// over traffic it was never meant to describe.
func TestTheEgressExampleSelectsNothingElse(t *testing.T) {
	t.Parallel()
	policies := readEgressPolicies(t)
	for _, foreign := range []operationPod{
		{name: "the manager", labels: map[string]string{
			LabelManagedBy: "ptah-operator", LabelComponent: "manager"}},
		{name: "another operator's Pod", labels: map[string]string{
			LabelManagedBy: "other-operator", LabelComponent: "schema-operation"}},
		{name: "an unlabelled Pod", labels: map[string]string{}},
	} {
		for _, policy := range policies {
			if selects(t, policy, foreign) {
				t.Errorf("policy %s selects %s", policy.Name, foreign.name)
			}
		}
	}
}
