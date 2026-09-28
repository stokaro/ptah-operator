package e2e

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
)

const fixtureImage = "registry.example/ptah-operator@sha256:1111111111111111111111111111111111111111111111111111111111111111"

func TestControlPlaneInputs(t *testing.T) {
	t.Parallel()
	valid := func() (string, string, string, string, string) {
		return "0123456789abcdef", fixtureImage, "2", "ptah-e2e", "ptah-foreign"
	}
	if err := controlPlaneInputsOK(valid()); err != nil {
		t.Fatalf("valid inputs were refused: %v", err)
	}
	for _, test := range []struct {
		name   string
		edit   func(revision, image, state, test, foreign *string)
		refuse string
	}{
		{"overlong revision", func(r, _, _, _, _ *string) { *r = strings.Repeat("a", 129) }, "at most 128 bytes"},
		{"control character", func(r, _, _, _, _ *string) { *r = "abc\x01def" }, "control characters"},
		{"newline", func(r, _, _, _, _ *string) { *r = "abc\ndef" }, "control characters"},
		{"delete character", func(r, _, _, _, _ *string) { *r = "abc\x7f" }, "control characters"},
		{"leading space", func(r, _, _, _, _ *string) { *r = " abc" }, "edge whitespace"},
		{"trailing space", func(r, _, _, _, _ *string) { *r = "abc " }, "edge whitespace"},
		{"empty revision", func(r, _, _, _, _ *string) { *r = "" }, "edge whitespace"},
		{"image by tag", func(_, i, _, _, _ *string) { *i = "registry.example/ptah-operator:dev" }, "pinned"},
		{"uppercase digest", func(_, i, _, _, _ *string) { *i = strings.ToUpper(fixtureImage) }, "pinned"},
		{"image with a space", func(_, i, _, _, _ *string) { *i = "registry example" + fixtureImage[16:] }, "pinned"},
		{"zero state version", func(_, _, s, _, _ *string) { *s = "0" }, "positive integer"},
		{"leading zero", func(_, _, s, _, _ *string) { *s = "02" }, "positive integer"},
		{"one namespace twice", func(_, _, _, test, foreign *string) { *foreign = *test }, "must differ"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			revision, image, state, testNamespace, foreign := valid()
			test.edit(&revision, &image, &state, &testNamespace, &foreign)
			err := controlPlaneInputsOK(revision, image, state, testNamespace, foreign)
			if err == nil || !strings.Contains(err.Error(), test.refuse) {
				t.Fatalf("controlPlaneInputsOK() = %v, want a refusal naming %q", err, test.refuse)
			}
		})
	}
	// The revision limit counts bytes, as wc -c did.
	if err := controllerRevisionOK(strings.Repeat("é", 64)); err != nil {
		t.Fatalf("128 bytes of revision were refused: %v", err)
	}
	if err := controllerRevisionOK(strings.Repeat("é", 64) + "a"); err == nil {
		t.Fatal("129 bytes of revision were accepted")
	}
}

func TestEdgeRunnerProtocolVersion(t *testing.T) {
	t.Parallel()
	catalog, err := os.ReadFile(filepath.Join("..", "..", "support", "ptah.json"))
	if err != nil {
		t.Fatal(err)
	}
	if version, err := edgeRunnerProtocolVersion(catalog); err != nil || version < 1 {
		t.Fatalf("the committed catalog reads as %d, %v", version, err)
	}
	for name, document := range map[string]string{
		"one version twice": `{"releases":[{"operator":"edge","verified":[{"runnerProtocolVersion":7},{"runnerProtocolVersion":7}]},` +
			`{"operator":"v0.1.0","verified":[{"runnerProtocolVersion":6}]}]}`,
	} {
		if version, err := edgeRunnerProtocolVersion([]byte(document)); err != nil || version != 7 {
			t.Errorf("%s: = %d, %v", name, version, err)
		}
	}
	for name, document := range map[string]string{
		"two versions": `{"releases":[{"operator":"edge","verified":[{"runnerProtocolVersion":7},{"runnerProtocolVersion":6}]}]}`,
		"a string":     `{"releases":[{"operator":"edge","verified":[{"runnerProtocolVersion":"7"}]}]}`,
		"no edge":      `{"releases":[{"operator":"v0.1.0","verified":[{"runnerProtocolVersion":7}]}]}`,
		"no verified":  `{"releases":[{"operator":"edge","verified":[]}]}`,
		"a fraction":   `{"releases":[{"operator":"edge","verified":[{"runnerProtocolVersion":7.5}]}]}`,
		"not JSON":     `edge`,
	} {
		if version, err := edgeRunnerProtocolVersion([]byte(document)); err == nil {
			t.Errorf("%s: read as %d", name, version)
		}
	}
}

// The phase derives the realm and the plan fingerprint itself rather than
// calling the operator, so the webhook's refusal of a plan bound to anything
// else is compared with an independent answer. This holds the two derivations
// to the operator's own.
func TestDerivationsAreTheOperators(t *testing.T) {
	t.Parallel()
	for _, sample := range []struct{ engine, canonical, namespace, key string }{
		{"PostgreSQL", "postgresql", "ptah-test-a-1234", "e2e/admission/postgresql"},
		{"MySQL", "mysql", "team-b", "e2e/migrations/mysql"},
	} {
		want, err := fingerprint.DatabaseCoordinationDigest(sample.engine, sample.namespace, sample.key)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := coordinationDigest(sample.canonical, sample.namespace, sample.key); err != nil || got != want {
			t.Errorf("coordinationDigest(%s) = %s, %v; the operator derives %s", sample.canonical, got, err, want)
		}
	}
	binding := planBinding{
		ContractVersion: 3, SchemaUID: "5e6f0a1b-0000-4000-8000-000000000001", PlanContentDigest: contentDigestFixtureForTest,
		ArtifactDigest: "sha256:" + strings.Repeat("2", 64), CoordinationDigest: "sha256:" + strings.Repeat("3", 64),
		TargetIdentityDigest: "sha256:" + strings.Repeat("4", 64), ActualStateFingerprint: "sha256:" + strings.Repeat("5", 64),
		DesiredStateFingerprint: "sha256:" + strings.Repeat("6", 64), PolicyFingerprint: "sha256:" + strings.Repeat("7", 64),
		VerificationPolicyUID: "5e6f0a1b-0000-4000-8000-000000000002", VerificationPolicyDigest: "sha256:" + strings.Repeat("8", 64),
		ExecutionBindingID: "v1-" + strings.Repeat("a", 32), ControllerStateVersion: 2, PtahVersion: "v0.9.0-73-gf6e562c5b",
		ExecutorImage: "registry.example/ptah@sha256:" + strings.Repeat("9", 64), RunnerProtocolVersion: 7,
		StatementCount: 1,
	}
	got, err := binding.fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	want, err := fingerprint.PlanBinding{
		ContractVersion: 3, SchemaUID: binding.SchemaUID, PlanContentDigest: binding.PlanContentDigest,
		ArtifactDigest: binding.ArtifactDigest, CoordinationDigest: binding.CoordinationDigest,
		TargetIdentityDigest: binding.TargetIdentityDigest, ActualStateFingerprint: binding.ActualStateFingerprint,
		DesiredStateFingerprint: binding.DesiredStateFingerprint, PolicyFingerprint: binding.PolicyFingerprint,
		VerificationPolicyUID: binding.VerificationPolicyUID, VerificationPolicyDigest: binding.VerificationPolicyDigest,
		ExecutionBindingID: binding.ExecutionBindingID, ControllerStateVersion: 2, PtahVersion: binding.PtahVersion,
		ExecutorImage: binding.ExecutorImage, RunnerProtocolVersion: 7, StatementCount: 1,
	}.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("the plan fingerprint is %s; the operator derives %s", got, want)
	}
	binding.StatementCount = 2
	if changed, _ := binding.fingerprint(); changed == want {
		t.Fatal("a changed binding kept its fingerprint")
	}
}

func equalJSON(got, want map[string]any) bool {
	return reflect.DeepEqual(got, want)
}

func mustRegexp(pattern string) *regexp.Regexp {
	return regexp.MustCompile(pattern)
}

const contentDigestFixtureForTest = "sha256:2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"

// The Pod intent entry's match condition, as the phase writes it from its
// parts, is the chart's with its whitespace collapsed.
func TestOperationPodConditionIsTheCharts(t *testing.T) {
	t.Parallel()
	template, err := os.ReadFile(filepath.Join("..", "..", "charts", "ptah-operator", "templates", "webhook.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(template)
	start := strings.Index(text, "- name: managed-or-operation-job-pod")
	if start < 0 {
		t.Fatal("the chart carries no managed-or-operation-job-pod condition")
	}
	block := text[start:]
	begin := strings.Index(block, "expression: >-")
	end := strings.Index(block, "clientConfig:")
	if begin < 0 || end < begin {
		t.Fatal("the condition's expression could not be found")
	}
	expression := strings.TrimSpace(collapseWhitespace(block[begin+len("expression: >-") : end]))
	if expression != operationPodCondition() {
		t.Fatalf("the chart's condition is\n%s\nthe phase expects\n%s", expression, operationPodCondition())
	}
	if collapseWhitespace("a \n\t  b") != "a b" {
		t.Fatal("collapseWhitespace does not collapse a run of whitespace")
	}
}

func mutatingFixture(requireDistinctApprover bool) *admissionregistrationv1.MutatingWebhookConfiguration {
	entry := func(name, path string, operations []admissionregistrationv1.OperationType, resource string) admissionregistrationv1.MutatingWebhook {
		return admissionregistrationv1.MutatingWebhook{
			Name:                    name,
			ClientConfig:            admissionregistrationv1.WebhookClientConfig{CABundle: []byte("ca"), Service: pointer(serviceReference("operator", "webhook", path))},
			Rules:                   []admissionregistrationv1.RuleWithOperations{namespacedRule("operator.ptah.run", "v1alpha1", operations, resource)},
			FailurePolicy:           pointer(admissionregistrationv1.Fail),
			MatchPolicy:             pointer(admissionregistrationv1.Equivalent),
			NamespaceSelector:       &metav1.LabelSelector{},
			ObjectSelector:          &metav1.LabelSelector{},
			SideEffects:             pointer(admissionregistrationv1.SideEffectClassNone),
			TimeoutSeconds:          pointer[int32](5),
			AdmissionReviewVersions: []string{"v1"},
			ReinvocationPolicy:      pointer(admissionregistrationv1.NeverReinvocationPolicy),
		}
	}
	configuration := &admissionregistrationv1.MutatingWebhookConfiguration{Webhooks: []admissionregistrationv1.MutatingWebhook{
		entry("mapproval.operator.ptah.run", "/mutate-operator-ptah-run-v1alpha1-ptahschemaapproval", createOnly, "ptahschemaapprovals"),
		entry("mmigrationapproval.operator.ptah.run", "/mutate-operator-ptah-run-v1alpha1-ptahmigrationapproval", createOnly, "ptahmigrationapprovals"),
		entry("mmigrationrunacknowledgment.operator.ptah.run", "/mutate-operator-ptah-run-v1alpha1-ptahmigrationrunacknowledgment",
			createOnly, "ptahmigrationrunacknowledgments"),
	}}
	if requireDistinctApprover {
		configuration.Webhooks = append(configuration.Webhooks,
			entry("mschemawriter.operator.ptah.run", "/mutate-operator-ptah-run-v1alpha1-ptahschema", createAndUpdate, "ptahschemas"),
			entry("mmigrationwriter.operator.ptah.run", "/mutate-operator-ptah-run-v1alpha1-ptahmigration", createAndUpdate, "ptahmigrations"))
	}
	return configuration
}

func TestMutatingAdmissionExact(t *testing.T) {
	t.Parallel()
	for _, flag := range []bool{false, true} {
		if err := mutatingAdmissionExact(mutatingFixture(flag), "operator", "webhook", flag); err != nil {
			t.Fatalf("the chart's shape with requireDistinctApprover=%v was refused: %v", flag, err)
		}
	}
	for _, test := range []struct {
		name   string
		flag   bool
		change func(*admissionregistrationv1.MutatingWebhookConfiguration)
	}{
		{"writer entries with the control off", false, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			c.Webhooks = mutatingFixture(true).Webhooks
		}},
		{"writer entries missing with the control on", true, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			c.Webhooks = c.Webhooks[:3]
		}},
		{"the acknowledgment entry missing", false, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			c.Webhooks = slices.Delete(c.Webhooks, 2, 3)
		}},
		{"the acknowledgment entry missing with the control on", true, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			c.Webhooks = slices.Delete(c.Webhooks, 2, 3)
		}},
		{"the acknowledgment entry fails open", false, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			c.Webhooks[2].FailurePolicy = pointer(admissionregistrationv1.Ignore)
		}},
		{"the acknowledgment entry reinvoked", false, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			c.Webhooks[2].ReinvocationPolicy = pointer(admissionregistrationv1.IfNeededReinvocationPolicy)
		}},
		{"the acknowledgment entry calls the validating path", false, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			c.Webhooks[2].ClientConfig.Service.Path = pointer("/validate-operator-ptah-run-v1alpha1-ptahmigrationrunacknowledgment")
		}},
		{"the acknowledgment entry stamps updates", false, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			c.Webhooks[2].Rules[0].Operations = createAndUpdate
		}},
		{"the acknowledgment entry on approvals", false, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			c.Webhooks[2].Rules[0].Resources = []string{"ptahmigrationapprovals"}
		}},
		{"an extra entry", false, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			extra := c.Webhooks[0]
			extra.Name = "extra.operator.ptah.run"
			c.Webhooks = append(c.Webhooks, extra)
		}},
		{"fail open", false, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			c.Webhooks[0].FailurePolicy = pointer(admissionregistrationv1.Ignore)
		}},
		{"side effects", false, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			c.Webhooks[0].SideEffects = pointer(admissionregistrationv1.SideEffectClassNoneOnDryRun)
		}},
		{"exact match policy", false, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			c.Webhooks[0].MatchPolicy = pointer(admissionregistrationv1.Exact)
		}},
		{"reinvoked", false, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			c.Webhooks[0].ReinvocationPolicy = pointer(admissionregistrationv1.IfNeededReinvocationPolicy)
		}},
		{"another timeout", false, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			c.Webhooks[0].TimeoutSeconds = pointer[int32](10)
		}},
		{"a namespace selector", false, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			c.Webhooks[0].NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}}
		}},
		{"an empty but present label map", false, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			c.Webhooks[0].ObjectSelector = &metav1.LabelSelector{MatchLabels: map[string]string{}}
		}},
		{"a match condition", false, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			c.Webhooks[0].MatchConditions = []admissionregistrationv1.MatchCondition{{Name: "x", Expression: "true"}}
		}},
		{"another review version", false, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			c.Webhooks[0].AdmissionReviewVersions = []string{"v1", "v1beta1"}
		}},
		{"no bundle", false, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			c.Webhooks[0].ClientConfig.CABundle = nil
		}},
		{"a URL", false, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			c.Webhooks[0].ClientConfig.URL = pointer("https://example.com")
		}},
		{"another path", false, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			c.Webhooks[0].ClientConfig.Service.Path = pointer("/other")
		}},
		{"another port", false, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			c.Webhooks[0].ClientConfig.Service.Port = pointer[int32](8443)
		}},
		{"another namespace", false, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			c.Webhooks[0].ClientConfig.Service.Namespace = "foreign"
		}},
		{"an update rule", false, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			c.Webhooks[0].Rules[0].Operations = createAndUpdate
		}},
		{"a writer entry fails open", true, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			c.Webhooks[3].FailurePolicy = pointer(admissionregistrationv1.Ignore)
		}},
		{"a writer entry misses updates", true, func(c *admissionregistrationv1.MutatingWebhookConfiguration) {
			c.Webhooks[4].Rules[0].Operations = createOnly
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			configuration := mutatingFixture(test.flag)
			test.change(configuration)
			if err := mutatingAdmissionExact(configuration, "operator", "webhook", test.flag); err == nil {
				t.Fatal("the shape was accepted")
			}
		})
	}
}

const controllerUserFixture = "system:serviceaccount:operator:ptah-controller"

func validatingFixture() *admissionregistrationv1.ValidatingWebhookConfiguration {
	entry := func(name, path string, rules ...admissionregistrationv1.RuleWithOperations) admissionregistrationv1.ValidatingWebhook {
		return admissionregistrationv1.ValidatingWebhook{
			Name:                    name,
			ClientConfig:            admissionregistrationv1.WebhookClientConfig{CABundle: []byte("ca"), Service: pointer(serviceReference("operator", "webhook", path))},
			Rules:                   rules,
			FailurePolicy:           pointer(admissionregistrationv1.Fail),
			MatchPolicy:             pointer(admissionregistrationv1.Equivalent),
			NamespaceSelector:       &metav1.LabelSelector{},
			ObjectSelector:          &metav1.LabelSelector{},
			SideEffects:             pointer(admissionregistrationv1.SideEffectClassNone),
			TimeoutSeconds:          pointer[int32](5),
			AdmissionReviewVersions: []string{"v1"},
		}
	}
	pod := entry("vpodintent.operator.ptah.run", "/validate-v1-pod-ptah-operation-intent",
		namespacedRule("", "v1", createAndUpdate, "pods", "pods/ephemeralcontainers", "pods/resize"))
	// Stored as the chart folds it: the same expression across lines.
	pod.MatchConditions = []admissionregistrationv1.MatchCondition{{
		Name:       "managed-or-operation-job-pod",
		Expression: strings.ReplaceAll(operationPodCondition(), " && ", " &&\n    "),
	}}
	write := entry("vcontrollerwrite.operator.ptah.run", "/validate-operator-controller-write",
		namespacedRule("batch", "v1", createAndUpdate, "jobs"),
		namespacedRule("", "v1", createOnly, "configmaps"),
		namespacedRule("operator.ptah.run", "v1alpha1", createOnly, "ptahschemaplans", "ptahschemaplanchunks", "ptahmigrationplans"))
	write.MatchPolicy = pointer(admissionregistrationv1.Exact)
	write.TimeoutSeconds = pointer[int32](30)
	write.MatchConditions = []admissionregistrationv1.MatchCondition{{
		Name: "controller-service-account", Expression: "request.userInfo.username == '" + controllerUserFixture + "'",
	}}
	return &admissionregistrationv1.ValidatingWebhookConfiguration{Webhooks: []admissionregistrationv1.ValidatingWebhook{
		entry("vapproval.operator.ptah.run", "/validate-operator-ptah-run-v1alpha1-ptahschemaapproval",
			namespacedRule("operator.ptah.run", "v1alpha1", createAndUpdate, "ptahschemaapprovals")),
		entry("vmigrationapproval.operator.ptah.run", "/validate-operator-ptah-run-v1alpha1-ptahmigrationapproval",
			namespacedRule("operator.ptah.run", "v1alpha1", createAndUpdate, "ptahmigrationapprovals")),
		pod,
		write,
		entry("vmigrationrunacknowledgment.operator.ptah.run", "/validate-operator-ptah-run-v1alpha1-ptahmigrationrunacknowledgment",
			namespacedRule("operator.ptah.run", "v1alpha1", createAndUpdate, "ptahmigrationrunacknowledgments")),
	}}
}

func TestValidatingAdmissionExact(t *testing.T) {
	t.Parallel()
	if err := validatingAdmissionExact(validatingFixture(), "operator", "webhook", controllerUserFixture); err != nil {
		t.Fatalf("the chart's shape was refused: %v", err)
	}
	for _, test := range []struct {
		name   string
		change func(*admissionregistrationv1.ValidatingWebhookConfiguration)
	}{
		{"an approval entry twice", func(c *admissionregistrationv1.ValidatingWebhookConfiguration) {
			c.Webhooks = append(c.Webhooks, c.Webhooks[0])
		}},
		{"an entry missing", func(c *admissionregistrationv1.ValidatingWebhookConfiguration) { c.Webhooks = c.Webhooks[1:] }},
		{"approval fails open", func(c *admissionregistrationv1.ValidatingWebhookConfiguration) {
			c.Webhooks[0].FailurePolicy = pointer(admissionregistrationv1.Ignore)
		}},
		{"approval misses updates", func(c *admissionregistrationv1.ValidatingWebhookConfiguration) {
			c.Webhooks[0].Rules[0].Operations = createOnly
		}},
		{"Pod intent condition widened", func(c *admissionregistrationv1.ValidatingWebhookConfiguration) {
			widened := strings.Replace(c.Webhooks[2].MatchConditions[0].Expression, "ref.controller == true &&", "", 2)
			if widened == c.Webhooks[2].MatchConditions[0].Expression {
				t.Fatal("the mutation changed nothing")
			}
			c.Webhooks[2].MatchConditions[0].Expression = widened
		}},
		{"Pod intent condition renamed", func(c *admissionregistrationv1.ValidatingWebhookConfiguration) {
			c.Webhooks[2].MatchConditions[0].Name = "operation-pod"
		}},
		{"Pod intent condition doubled", func(c *admissionregistrationv1.ValidatingWebhookConfiguration) {
			c.Webhooks[2].MatchConditions = append(c.Webhooks[2].MatchConditions, c.Webhooks[2].MatchConditions[0])
		}},
		{"Pod intent misses resize", func(c *admissionregistrationv1.ValidatingWebhookConfiguration) {
			c.Webhooks[2].Rules[0].Resources = []string{"pods", "pods/ephemeralcontainers"}
		}},
		{"controller write matched equivalently", func(c *admissionregistrationv1.ValidatingWebhookConfiguration) {
			c.Webhooks[3].MatchPolicy = pointer(admissionregistrationv1.Equivalent)
		}},
		{"controller write on a short timeout", func(c *admissionregistrationv1.ValidatingWebhookConfiguration) {
			c.Webhooks[3].TimeoutSeconds = pointer[int32](5)
		}},
		{"controller write for another identity", func(c *admissionregistrationv1.ValidatingWebhookConfiguration) {
			c.Webhooks[3].MatchConditions[0].Expression = "request.userInfo.username == 'system:serviceaccount:operator:other'"
		}},
		{"controller write misses chunks", func(c *admissionregistrationv1.ValidatingWebhookConfiguration) {
			c.Webhooks[3].Rules[2].Resources = []string{"ptahschemaplans", "ptahmigrationplans"}
		}},
		{"controller write on another port", func(c *admissionregistrationv1.ValidatingWebhookConfiguration) {
			c.Webhooks[3].ClientConfig.Service.Port = pointer[int32](8443)
		}},
		{"the acknowledgment entry missing", func(c *admissionregistrationv1.ValidatingWebhookConfiguration) {
			c.Webhooks = c.Webhooks[:4]
		}},
		{"the acknowledgment entry fails open", func(c *admissionregistrationv1.ValidatingWebhookConfiguration) {
			c.Webhooks[4].FailurePolicy = pointer(admissionregistrationv1.Ignore)
		}},
		{"the acknowledgment entry misses updates", func(c *admissionregistrationv1.ValidatingWebhookConfiguration) {
			c.Webhooks[4].Rules[0].Operations = createOnly
		}},
		{"the acknowledgment entry calls the mutating path", func(c *admissionregistrationv1.ValidatingWebhookConfiguration) {
			c.Webhooks[4].ClientConfig.Service.Path = pointer("/mutate-operator-ptah-run-v1alpha1-ptahmigrationrunacknowledgment")
		}},
		{"the acknowledgment entry narrowed by a condition", func(c *admissionregistrationv1.ValidatingWebhookConfiguration) {
			c.Webhooks[4].MatchConditions = []admissionregistrationv1.MatchCondition{{Name: "x", Expression: "true"}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			configuration := validatingFixture()
			test.change(configuration)
			if err := validatingAdmissionExact(configuration, "operator", "webhook", controllerUserFixture); err == nil {
				t.Fatal("the shape was accepted")
			}
		})
	}
}

func crdFixture(name string, spec map[string]apiextensionsv1.JSONSchemaProps) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
			Name: "v1alpha1",
			Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
				Properties: map[string]apiextensionsv1.JSONSchemaProps{"spec": {Properties: spec}},
			}},
		}}},
	}
}

func reference(fields ...string) apiextensionsv1.JSONSchemaProps {
	properties := map[string]apiextensionsv1.JSONSchemaProps{}
	for _, field := range fields {
		properties[field] = apiextensionsv1.JSONSchemaProps{Type: "string"}
	}
	return apiextensionsv1.JSONSchemaProps{Type: "object", Properties: properties}
}

func TestReferencesStayInTheirNamespace(t *testing.T) {
	t.Parallel()
	schema := func(policyFrom apiextensionsv1.JSONSchemaProps) *apiextensionsv1.CustomResourceDefinition {
		return crdFixture("ptahschemas.operator.ptah.run", map[string]apiextensionsv1.JSONSchemaProps{
			"target": {Properties: map[string]apiextensionsv1.JSONSchemaProps{"urlFrom": reference("name", "key")}},
			"desired": {Properties: map[string]apiextensionsv1.JSONSchemaProps{
				"verificationPolicyFrom": policyFrom,
				"registryAuthFrom":       reference("name", "mode"),
			}},
		})
	}
	if err := schemaReferencesLocal(schema(reference("name", "key"))); err != nil {
		t.Fatalf("local references were refused: %v", err)
	}
	if err := schemaReferencesLocal(schema(reference("name", "key", "namespace"))); err == nil {
		t.Fatal("a verification policy in another namespace was accepted")
	}
	missing := schema(reference("name", "key"))
	delete(missing.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties, "target")
	if err := schemaReferencesLocal(missing); err == nil {
		t.Fatal("a schema whose reference is absent was read as local")
	}
	approval := func(planRef apiextensionsv1.JSONSchemaProps) *apiextensionsv1.CustomResourceDefinition {
		return crdFixture("ptahschemaapprovals.operator.ptah.run", map[string]apiextensionsv1.JSONSchemaProps{
			"schemaRef": reference("name", "uid"), "planRef": planRef,
		})
	}
	if err := approvalReferencesLocal(approval(reference("name", "uid"))); err != nil {
		t.Fatalf("local references were refused: %v", err)
	}
	if err := approvalReferencesLocal(approval(reference("name", "uid", "namespace"))); err == nil {
		t.Fatal("a plan in another namespace was accepted")
	}
	otherVersion := approval(reference("name", "uid"))
	otherVersion.Spec.Versions[0].Name = "v1beta1"
	if err := approvalReferencesLocal(otherVersion); err == nil {
		t.Fatal("a CRD with no v1alpha1 was read as local")
	}
}

func TestAnyReadyEndpoint(t *testing.T) {
	t.Parallel()
	ready, notReady := true, false
	slice := func(states ...*bool) discoveryv1.EndpointSlice {
		endpoints := make([]discoveryv1.Endpoint, 0, len(states))
		for _, state := range states {
			endpoints = append(endpoints, discoveryv1.Endpoint{Conditions: discoveryv1.EndpointConditions{Ready: state}})
		}
		return discoveryv1.EndpointSlice{Endpoints: endpoints}
	}
	if !anyReadyEndpoint([]discoveryv1.EndpointSlice{slice(&notReady), slice(nil, &ready)}) {
		t.Fatal("a ready endpoint in the second slice was missed")
	}
	if anyReadyEndpoint([]discoveryv1.EndpointSlice{slice(&notReady, nil), {}}) || anyReadyEndpoint(nil) {
		t.Fatal("no ready endpoint was read as one")
	}
}

func TestSingleApplyManager(t *testing.T) {
	t.Parallel()
	entry := func(manager string, operation metav1.ManagedFieldsOperationType) metav1.ManagedFieldsEntry {
		return metav1.ManagedFieldsEntry{Manager: manager, Operation: operation}
	}
	if manager, err := singleApplyManager([]metav1.ManagedFieldsEntry{
		entry("helm", metav1.ManagedFieldsOperationApply),
		entry("kube-controller-manager", metav1.ManagedFieldsOperationUpdate),
		entry("helm", metav1.ManagedFieldsOperationApply),
	}); err != nil || manager != "helm" {
		t.Fatalf("singleApplyManager() = %q, %v", manager, err)
	}
	for name, entries := range map[string][]metav1.ManagedFieldsEntry{
		"none":    {entry("kubectl-patch", metav1.ManagedFieldsOperationUpdate)},
		"two":     {entry("helm", metav1.ManagedFieldsOperationApply), entry("kubectl", metav1.ManagedFieldsOperationApply)},
		"nothing": nil,
	} {
		if manager, err := singleApplyManager(entries); err == nil {
			t.Errorf("%s: restored as %q", name, manager)
		}
	}
}

func TestRestorableDeploymentDropsWhatTheServerOwns(t *testing.T) {
	t.Parallel()
	live := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{
			"name": "manager", "namespace": "operator", "uid": "u", "resourceVersion": "1", "generation": int64(3),
			"creationTimestamp": "2026-09-28T10:00:00Z", "managedFields": []any{map[string]any{"manager": "helm"}},
			"labels": map[string]any{"app": "manager"},
			"annotations": map[string]any{
				"deployment.kubernetes.io/revision": "4", "meta.helm.sh/release-name": "ptah",
			},
		},
		"spec":   map[string]any{"replicas": int64(2)},
		"status": map[string]any{"replicas": int64(2)},
	}}
	restored := restorableDeployment(live)
	want := map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{
			"name": "manager", "namespace": "operator",
			"labels":      map[string]any{"app": "manager"},
			"annotations": map[string]any{"meta.helm.sh/release-name": "ptah"},
		},
		"spec": map[string]any{"replicas": int64(2)},
	}
	if !equalJSON(restored.Object, want) {
		t.Fatalf("restorableDeployment() = %v, want %v", restored.Object, want)
	}
	if _, found := live.Object["status"]; !found {
		t.Fatal("the live document was changed")
	}
}

func TestJobPodsAndRefusals(t *testing.T) {
	t.Parallel()
	controller := true
	uid := types.UID("job-uid")
	pod := func(name string, references ...metav1.OwnerReference) corev1.Pod {
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, OwnerReferences: references}}
	}
	owner := metav1.OwnerReference{APIVersion: "batch/v1", Kind: "Job", UID: uid, Controller: &controller}
	notController := owner
	notController.Controller = nil
	otherKind := owner
	otherKind.Kind = "CronJob"
	pods := []corev1.Pod{pod("owned", owner), pod("unowned"), pod("referenced", notController), pod("cron", otherKind)}
	if owned := ownedPods(pods, uid); len(owned) != 1 || owned[0].Name != "owned" {
		t.Fatalf("ownedPods() = %v", owned)
	}
	if !referencesJob([]corev1.Pod{pod("referenced", notController)}, uid) {
		t.Fatal("a Pod naming the Job without controlling it was not seen")
	}
	if referencesJob([]corev1.Pod{pod("unowned")}, uid) {
		t.Fatal("a Pod naming no Job was read as the Job's")
	}

	reason := mustRegexp(`(?i)failed calling webhook|no endpoints available`)
	event := func(involved types.UID, kind, message string) corev1.Event {
		return corev1.Event{InvolvedObject: corev1.ObjectReference{UID: involved}, Reason: kind, Message: message}
	}
	refused := `Error creating: Internal error occurred: failed calling webhook "vpodintent.operator.ptah.run": no endpoints available`
	if !podCreationRefused([]corev1.Event{event(uid, "FailedCreate", refused)}, uid, reason) {
		t.Fatal("the refusal was not read")
	}
	for name, events := range map[string][]corev1.Event{
		"another Job":     {event("other", "FailedCreate", refused)},
		"another reason":  {event(uid, "SuccessfulCreate", refused)},
		"another webhook": {event(uid, "FailedCreate", strings.ReplaceAll(refused, "vpodintent", "vapproval"))},
		"another cause":   {event(uid, "FailedCreate", `admission webhook "vpodintent.operator.ptah.run" denied the request`)},
		"no Event":        nil,
	} {
		if podCreationRefused(events, uid, reason) {
			t.Errorf("%s: read as the refusal", name)
		}
	}
}

func TestControllerImageArgument(t *testing.T) {
	t.Parallel()
	deployment := func(containers ...corev1.Container) *appsv1.Deployment {
		result := &appsv1.Deployment{}
		result.Spec.Template.Spec.Containers = containers
		return result
	}
	manager := corev1.Container{Name: "manager", Args: []string{"--leader-elect", "--controller-image=" + fixtureImage}}
	sidecar := corev1.Container{Name: "sidecar", Args: []string{"--controller-image=other"}}
	if image, err := controllerImageArgument(deployment(sidecar, manager)); err != nil || image != fixtureImage {
		t.Fatalf("controllerImageArgument() = %q, %v", image, err)
	}
	twice := manager
	twice.Args = append(append([]string(nil), manager.Args...), "--controller-image=again")
	for name, candidate := range map[string]*appsv1.Deployment{
		"none":    deployment(corev1.Container{Name: "manager"}),
		"twice":   deployment(twice),
		"sidecar": deployment(sidecar),
	} {
		if image, err := controllerImageArgument(candidate); err == nil {
			t.Errorf("%s: read %q", name, image)
		}
	}
	rotator := deployment(corev1.Container{Name: "certificate-rotator", Args: []string{"--secret-name=", "--secret-name=second"}})
	if value, found := firstRotatorArgument(rotator, "secret-name"); !found || value != "" {
		t.Fatalf("the first --secret-name is empty, read %q, %v", value, found)
	}
	if _, found := firstRotatorArgument(deployment(), "secret-name"); found {
		t.Fatal("a Deployment with no container carried the argument")
	}
}

func TestUnsupportedEngineSettled(t *testing.T) {
	t.Parallel()
	settled := func() *ptahv1alpha1.PtahSchema {
		schema := &ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Generation: 2}}
		schema.Status.ObservedGeneration = 2
		schema.Status.Phase = "Blocked"
		schema.Status.Conditions = []metav1.Condition{
			{Type: "EngineSupported", Status: metav1.ConditionFalse, Reason: "UnsupportedEngine", ObservedGeneration: 2},
			{Type: "Ready", Status: metav1.ConditionFalse, Reason: "UnsupportedEngine", ObservedGeneration: 2},
		}
		return schema
	}
	if !unsupportedEngineSettled(settled()) {
		t.Fatal("the settled status was refused")
	}
	for name, change := range map[string]func(*ptahv1alpha1.PtahSchema){
		"an older generation": func(s *ptahv1alpha1.PtahSchema) { s.Status.ObservedGeneration = 1 },
		"another phase":       func(s *ptahv1alpha1.PtahSchema) { s.Status.Phase = "Resolving" },
		"a plan":              func(s *ptahv1alpha1.PtahSchema) { s.Status.Plan = &ptahv1alpha1.CurrentPlanStatus{} },
		"an operation": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.ActiveOperation = &ptahv1alpha1.ActiveOperationStatus{}
		},
		"an observation": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.PendingObservation = &ptahv1alpha1.PendingObservationStatus{}
		},
		"a stale condition": func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions[0].ObservedGeneration = 1 },
		"another reason":    func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions[1].Reason = "NotReady" },
		"supported":         func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions[0].Status = metav1.ConditionTrue },
		"no conditions":     func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions = nil },
	} {
		schema := settled()
		change(schema)
		if unsupportedEngineSettled(schema) {
			t.Errorf("%s: read as settled", name)
		}
	}
}

func storedSchema() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"uid": "schema-uid", "generation": int64(1)},
		"spec": map[string]any{
			"interval": "10m",
			"policy": map[string]any{
				"apply": "OnApproval", "allowDestructive": false, "driftSeverity": "all",
				"lockTimeout": "30s", "transactionMode": "file",
			},
			"execution": map[string]any{
				"activeDeadlineSeconds": int64(900), "failureRetryInterval": "30s", "connectTimeout": "10s",
			},
		},
		"status": map[string]any{"executionBinding": map[string]any{
			"epoch": "v1-" + strings.Repeat("0a", 16), "controllerStateVersion": int64(2), "ptahVersion": "v0.9.0",
			"executorImage": "registry.example/ptah@sha256:" + strings.Repeat("9", 64), "runnerProtocolVersion": int64(7),
		}},
	}}
}

func TestSchemaDefaultsPersisted(t *testing.T) {
	t.Parallel()
	if err := schemaDefaultsPersisted(storedSchema()); err != nil {
		t.Fatalf("the defaults were refused: %v", err)
	}
	for name, change := range map[string]func(map[string]any){
		"the interval in seconds": func(s map[string]any) { set(s, "600s", "spec", "interval") },
		"automatic apply":         func(s map[string]any) { set(s, "Always", "spec", "policy", "apply") },
		"destructive allowed":     func(s map[string]any) { set(s, true, "spec", "policy", "allowDestructive") },
		"no execution":            func(s map[string]any) { unstructured.RemoveNestedField(s, "spec", "execution") },
		"a deadline as a string":  func(s map[string]any) { set(s, "900", "spec", "execution", "activeDeadlineSeconds") },
	} {
		schema := storedSchema()
		change(schema.Object)
		if err := schemaDefaultsPersisted(schema); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestExecutionBindingExact(t *testing.T) {
	t.Parallel()
	image := "registry.example/ptah@sha256:" + strings.Repeat("9", 64)
	epoch, err := executionBindingExact(storedSchema(), 7, 2, "v0.9.0", image)
	if err != nil || epoch != "v1-"+strings.Repeat("0a", 16) {
		t.Fatalf("executionBindingExact() = %q, %v", epoch, err)
	}
	for name, change := range map[string]func(map[string]any){
		"a manager identity in it": func(s map[string]any) {
			set(s, "registry.example/manager@sha256:"+strings.Repeat("1", 64), "status", "executionBinding", "controllerImage")
		},
		"an epoch of another form": func(s map[string]any) { set(s, "v2-"+strings.Repeat("0", 32), "status", "executionBinding", "epoch") },
		"an uppercase epoch":       func(s map[string]any) { set(s, "v1-"+strings.Repeat("A", 32), "status", "executionBinding", "epoch") },
		"another state version":    func(s map[string]any) { set(s, int64(1), "status", "executionBinding", "controllerStateVersion") },
		"another Ptah":             func(s map[string]any) { set(s, "v0.8.0", "status", "executionBinding", "ptahVersion") },
		"another executor":         func(s map[string]any) { set(s, image+"x", "status", "executionBinding", "executorImage") },
		"a protocol as a string":   func(s map[string]any) { set(s, "7", "status", "executionBinding", "runnerProtocolVersion") },
		"no binding":               func(s map[string]any) { unstructured.RemoveNestedField(s, "status", "executionBinding") },
	} {
		schema := storedSchema()
		change(schema.Object)
		if _, err := executionBindingExact(schema, 7, 2, "v0.9.0", image); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestApprovalStampedExact(t *testing.T) {
	t.Parallel()
	schema := ptahv1alpha1.ImmutableObjectReference{Name: "schema", UID: "schema-uid"}
	plan := ptahv1alpha1.ImmutableObjectReference{Name: "plan", UID: "plan-uid"}
	stamped := func() *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
			"schemaRef":          map[string]any{"name": "schema", "uid": "schema-uid"},
			"planRef":            map[string]any{"name": "plan", "uid": "plan-uid"},
			"planFingerprint":    "sha256:f",
			"approver":           map[string]any{"username": "kubernetes-admin", "groups": []any{"kubeadm:cluster-admins"}},
			"approvedAt":         "2026-09-28T10:00:00Z",
			"mutationRequestUID": "request-uid",
		}}}
	}
	if err := approvalStampedExact(stamped(), schema, plan, "sha256:f"); err != nil {
		t.Fatalf("the stamped approval was refused: %v", err)
	}
	for name, change := range map[string]func(map[string]any){
		"a plan binding copied": func(a map[string]any) { set(a, "image", "spec", "executorImage") },
		"no approver":           func(a map[string]any) { set(a, "", "spec", "approver", "username") },
		"no time":               func(a map[string]any) { set(a, nil, "spec", "approvedAt") },
		"no request":            func(a map[string]any) { set(a, "", "spec", "mutationRequestUID") },
		"another schema":        func(a map[string]any) { set(a, "other", "spec", "schemaRef", "uid") },
		"another plan":          func(a map[string]any) { set(a, "other", "spec", "planRef", "name") },
		"a plan reference with more in it": func(a map[string]any) {
			set(a, "default", "spec", "planRef", "namespace")
		},
		"another fingerprint": func(a map[string]any) { set(a, "sha256:e", "spec", "planFingerprint") },
		"a missing key":       func(a map[string]any) { unstructured.RemoveNestedField(a, "spec", "mutationRequestUID") },
	} {
		approval := stamped()
		change(approval.Object)
		if err := approvalStampedExact(approval, schema, plan, "sha256:f"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestForeignPlanStable(t *testing.T) {
	t.Parallel()
	plan := func() *ptahv1alpha1.PtahSchemaPlan {
		return &ptahv1alpha1.PtahSchemaPlan{ObjectMeta: metav1.ObjectMeta{UID: "plan-uid"}}
	}
	if !foreignPlanStable(plan(), "plan-uid") {
		t.Fatal("the created plan was read as changed")
	}
	for name, change := range map[string]func(*ptahv1alpha1.PtahSchemaPlan){
		"replaced":  func(p *ptahv1alpha1.PtahSchemaPlan) { p.UID = "other" },
		"deleting":  func(p *ptahv1alpha1.PtahSchemaPlan) { p.DeletionTimestamp = &metav1.Time{Time: time.Now()} },
		"adopted":   func(p *ptahv1alpha1.PtahSchemaPlan) { p.OwnerReferences = []metav1.OwnerReference{{Name: "schema"}} },
		"finalized": func(p *ptahv1alpha1.PtahSchemaPlan) { p.Finalizers = []string{"example.com/hold"} },
	} {
		candidate := plan()
		change(candidate)
		if foreignPlanStable(candidate, "plan-uid") {
			t.Errorf("%s: read as stable", name)
		}
	}
}
