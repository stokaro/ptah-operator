//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
	"github.com/stokaro/ptah-operator/test/e2e/phases"
)

// The names the phase creates. The certificates and data-plane phases reuse
// the approval, the plan and the Secrets, so they are the names those phases
// read.
const (
	verificationPolicyName     = "e2e-verification-policy"
	verificationPolicyKey      = "policy.yaml"
	suspendedSchemaName        = "e2e-suspended-schema"
	suspendedCoordinationKey   = "e2e/admission/postgresql"
	unsupportedEngineSchema    = "e2e-unsupported-engine"
	fixturePlanName            = "e2e-plan"
	fixturePlanChunkName       = "e2e-plan-chunk-0"
	fixtureApprovalName        = "e2e-approval"
	webhookUnrelatedJob        = "e2e-webhook-unrelated"
	webhookOutageJob           = "e2e-webhook-managed-outage"
	webhookSpoofJob            = "e2e-webhook-foreign-spoof"
	unusedDatabaseURL          = "postgres://e2e:unused@database.invalid/e2e"
	artifactDigestFixture      = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	contentDigestFixture       = "sha256:2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"
	targetDigestFixture        = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
	actualFingerprintFixture   = "sha256:5555555555555555555555555555555555555555555555555555555555555555"
	desiredFingerprintFixture  = "sha256:6666666666666666666666666666666666666666666666666666666666666666"
	policyFingerprintFixture   = "sha256:7777777777777777777777777777777777777777777777777777777777777777"
	staleFingerprintFixture    = "sha256:8888888888888888888888888888888888888888888888888888888888888888"
	driftReportDigestFixture   = "sha256:9999999999999999999999999999999999999999999999999999999999999999"
	foreignPlanName            = "foreign-plan"
	crossNamespaceApprovalName = "e2e-cross-namespace-approval"
)

// TestControlPlaneContract is the data-plane suite's first phase and the
// certificates suite's preparation. It holds the installed release to its
// readiness, discovery, authorization and admission shape, sends the API
// server the objects it must refuse, and binds an approval to a plan fixture
// that the later phases go on to use.
func TestControlPlaneContract(t *testing.T) {
	run, inputs := harness.Begin(t, phases.ControlPlane)
	if err := controlPlaneInputsOK(inputs.ControllerRevision, inputs.ControllerImage,
		inputs.ControllerStateVersion, inputs.TestNamespace, inputs.ForeignNamespace); err != nil {
		t.Fatalf("e2e assertions: %v", err)
	}
	if info, err := os.Stat(inputs.Kubeconfig); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("e2e assertions: E2E_KUBECONFIG does not name a file")
	}
	catalog, err := os.ReadFile(filepath.Join(repositoryRoot, "support", "ptah.json"))
	if err != nil {
		t.Fatalf("e2e assertions: %v", err)
	}
	runnerProtocol, err := edgeRunnerProtocolVersion(catalog)
	if err != nil {
		t.Fatalf("e2e assertions: %v", err)
	}
	stateVersion, err := strconv.ParseInt(inputs.ControllerStateVersion, 10, 64)
	if err != nil {
		t.Fatalf("e2e assertions: E2E_CONTROLLER_STATE_VERSION must be a positive integer")
	}
	cluster, err := harness.Connect(inputs.Kubeconfig)
	if err != nil {
		t.Fatalf("e2e assertions: %v", err)
	}
	phase := &controlPlanePhase{
		ctx: run.Context(), cluster: cluster, in: inputs,
		runnerProtocol: runnerProtocol, stateVersion: stateVersion,
	}
	phase.resolve(t)
	t.Cleanup(func() { phase.cleanup(t) })

	for _, scenario := range []struct {
		name string
		body func(*testing.T)
	}{
		{"manager-readiness", phase.managerReadiness},
		{"crd-discovery", phase.crdDiscovery},
		{"finalizer-authorization", phase.finalizerAuthorization},
		{"realm-authorization", phase.realmAuthorization},
		{"plan-chunk-authorization", phase.planChunkAuthorization},
		{"webhook-configuration", phase.webhookConfiguration},
		{"secret-isolation", phase.secretIsolation},
		{"namespace-local-references", phase.namespaceLocalReferences},
		{"verification-policy", phase.verificationPolicy},
		{"pod-webhook-outage-scope", phase.podWebhookOutageScope},
		{"duration-bounds", phase.durationBounds},
		{"reference-keys", phase.referenceKeys},
		{"managed-scope-selectors", phase.managedScopeSelectors},
		{"unsupported-engine", phase.unsupportedEngine},
		{"suspended-schema-fixture", phase.suspendedSchemaFixture},
		{"approval-binding", phase.approvalBinding},
		{"cross-namespace-approval", phase.crossNamespaceApproval},
	} {
		if !run.Scenario(scenario.name, scenario.body) {
			return
		}
	}
	run.Logf("e2e assertions: PASS control-plane contract")
}

// repositoryRoot is where the test binary finds the files the phase compares
// the cluster with: go test runs it in this package's directory.
const repositoryRoot = "../.."

// controlPlanePhase is what the scenarios share.
type controlPlanePhase struct {
	ctx            context.Context
	cluster        *harness.Cluster
	in             phases.ControlPlaneInputs
	runnerProtocol int64
	stateVersion   int64

	requireDistinctApprover bool
	controllerName          string
	webhookService          string
	webhookCertSecret       string
	serviceAccount          string

	// The verification policy the fixtures name.
	policyUID    types.UID
	policyDigest string

	// The webhook Deployment while the outage row holds it down: the
	// document to restore and the field manager to restore it as.
	deploymentStopped  bool
	deploymentRestore  *unstructured.Unstructured
	deploymentManager  string
	deploymentReplicas int64

	// The suspended schema and the fixtures bound to it.
	schemaFixture     map[string]any
	schemaUID         types.UID
	schemaGeneration  int64
	executionEpoch    string
	planFixture       map[string]any
	planUID           types.UID
	planFingerprint   string
	approvalFixture   map[string]any
	foreignPlanUID    types.UID
	unsupportedSchema bool
}

func (p *controlPlanePhase) fatalf(t *testing.T, format string, arguments ...any) {
	t.Helper()
	t.Fatalf("e2e assertions: "+format, arguments...)
}

// resolve reads the names the chart derived: the controller's fullname, the
// webhook Service the admission singleton calls, and the certificate Secret
// the rotator's own arguments name.
func (p *controlPlanePhase) resolve(t *testing.T) {
	t.Helper()
	values, err := p.cluster.Helm(p.ctx, "-n", p.in.OperatorNamespace, "get", "values", p.in.HelmRelease, "--all", "-o", "json")
	if err != nil {
		p.fatalf(t, "could not read approvals.requireDistinctApprover from the live release: %v", err)
	}
	if p.requireDistinctApprover, err = releaseRequiresDistinctApprover(values); err != nil {
		p.fatalf(t, "%v", err)
	}
	controllers := &appsv1.DeploymentList{}
	if err := p.cluster.Client.List(p.ctx, controllers, client.InNamespace(p.in.OperatorNamespace),
		client.MatchingLabels{"app.kubernetes.io/component": "controller"}); err != nil || len(controllers.Items) == 0 {
		p.fatalf(t, "installed controller Deployment is missing: %v", err)
	}
	p.controllerName = controllers.Items[0].Name
	validating := &admissionregistrationv1.ValidatingWebhookConfiguration{}
	if err := p.cluster.Client.Get(p.ctx, types.NamespacedName{Name: admissionConfiguration}, validating); err != nil ||
		len(validating.Webhooks) == 0 || validating.Webhooks[0].ClientConfig.Service == nil ||
		validating.Webhooks[0].ClientConfig.Service.Name == "" {
		p.fatalf(t, "admission singleton names no webhook Service: %v", err)
	}
	p.webhookService = validating.Webhooks[0].ClientConfig.Service.Name
	rotators := &appsv1.DeploymentList{}
	if err := p.cluster.Client.List(p.ctx, rotators, client.InNamespace(p.in.OperatorNamespace),
		client.MatchingLabels{"app.kubernetes.io/component": "certificate-rotation"}); err != nil || len(rotators.Items) == 0 {
		p.fatalf(t, "certificate rotator carries no --secret-name: %v", err)
	}
	if p.webhookCertSecret, _ = firstRotatorArgument(&rotators.Items[0], "secret-name"); p.webhookCertSecret == "" {
		p.fatalf(t, "certificate rotator carries no --secret-name")
	}
}

// cleanup puts back what the phase took away, pass or fail: the webhook
// scope Jobs go, the unsupported-engine schema goes, and a webhook
// Deployment the outage row removed comes back. It runs on a context of its
// own, because the phase's may be what ran out.
func (p *controlPlanePhase) cleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	_ = p.deleteWebhookScopeJobs(ctx)
	if p.unsupportedSchema {
		_ = p.deleteAndWait(ctx, &ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{
			Name: unsupportedEngineSchema, Namespace: p.in.TestNamespace,
		}}, time.Minute)
	}
	if err := p.restoreWebhookDeployment(ctx); err != nil {
		t.Errorf("e2e assertions: could not restore the webhook Deployment during cleanup: %v", err)
	}
}

func (p *controlPlanePhase) managerReadiness(t *testing.T) {
	ns := p.in.OperatorNamespace
	if _, err := p.cluster.Helm(p.ctx, "-n", ns, "status", p.in.HelmRelease); err != nil {
		p.fatalf(t, "the release is not readable: %v", err)
	}
	if err := p.cluster.WaitForRollout(p.ctx, ns, p.controllerName, 180*time.Second); err != nil {
		p.fatalf(t, "%v", err)
	}
	controller := &appsv1.Deployment{}
	if err := p.cluster.Client.Get(p.ctx, types.NamespacedName{Namespace: ns, Name: p.controllerName}, controller); err != nil {
		p.fatalf(t, "could not read the controller Deployment: %v", err)
	}
	mutating := &admissionregistrationv1.MutatingWebhookConfiguration{}
	if err := p.cluster.Client.Get(p.ctx, types.NamespacedName{Name: admissionConfiguration}, mutating); err != nil {
		p.fatalf(t, "could not read the mutating admission singleton: %v", err)
	}
	controllerAccount := controller.Spec.Template.Spec.ServiceAccountName
	admissionAccount := mutating.Annotations["operator.ptah.run/controller-service-account-name"]
	if controllerAccount == "" || admissionAccount == "" || controllerAccount != admissionAccount {
		p.fatalf(t, "controller Deployment and admission singleton disagree on the active ServiceAccount")
	}
	p.serviceAccount = serviceAccountUser(ns, controllerAccount)
	ready, err := p.webhookServiceReady(p.ctx)
	if err != nil || !ready {
		p.fatalf(t, "webhook Service has no ready endpoint: %v", err)
	}
}

func (p *controlPlanePhase) crdDiscovery(t *testing.T) {
	resources := []string{
		"ptahschemas.operator.ptah.run", "ptahschemaplans.operator.ptah.run",
		"ptahschemaplanchunks.operator.ptah.run", "ptahschemaapprovals.operator.ptah.run",
	}
	for _, name := range resources {
		err := harness.Wait(p.ctx, "CRD "+name+" Established", 60*time.Second, time.Second,
			func(ctx context.Context) (bool, string, error) {
				crd := &apiextensionsv1.CustomResourceDefinition{}
				if err := p.cluster.Client.Get(ctx, types.NamespacedName{Name: name}, crd); err != nil {
					return false, fmt.Sprintf("read failed: %v", err), nil
				}
				for _, condition := range crd.Status.Conditions {
					if condition.Type == apiextensionsv1.Established && condition.Status == apiextensionsv1.ConditionTrue {
						return true, "Established", nil
					}
				}
				return false, "not Established", nil
			})
		if err != nil {
			p.fatalf(t, "%v", err)
		}
	}
	served, err := p.discoveredResources("operator.ptah.run")
	if err != nil {
		p.fatalf(t, "API discovery of operator.ptah.run failed: %v", err)
	}
	for _, name := range resources {
		if !slices.Contains(served, name) {
			p.fatalf(t, "API discovery is missing %s", name)
		}
	}
}

// discoveredResources is what `kubectl api-resources --api-group=<group> -o
// name` prints: the group's preferred version's resources, subresources left
// out, each as resource.group.
func (p *controlPlanePhase) discoveredResources(group string) ([]string, error) {
	groups, err := p.cluster.Clientset.Discovery().ServerGroups()
	if err != nil {
		return nil, err
	}
	for _, candidate := range groups.Groups {
		if candidate.Name != group {
			continue
		}
		list, err := p.cluster.Clientset.Discovery().ServerResourcesForGroupVersion(candidate.PreferredVersion.GroupVersion)
		if err != nil {
			return nil, err
		}
		var names []string
		for _, resource := range list.APIResources {
			if !strings.Contains(resource.Name, "/") {
				names = append(names, resource.Name+"."+group)
			}
		}
		return names, nil
	}
	return nil, fmt.Errorf("the API server serves no group %s", group)
}

// canI asks as the manager's ServiceAccount, failing on an answer that
// could not be had.
func (p *controlPlanePhase) canI(t *testing.T, attributes authorizationv1.ResourceAttributes) bool {
	t.Helper()
	if attributes.Namespace == "" {
		attributes.Namespace = p.cluster.Namespace
	}
	allowed, err := p.cluster.CanI(p.ctx, p.serviceAccount, attributes)
	if err != nil {
		p.fatalf(t, "could not ask whether the controller service account can %s %s: %v", attributes.Verb, attributes.Resource, err)
	}
	return allowed
}

func (p *controlPlanePhase) finalizerAuthorization(t *testing.T) {
	for _, resource := range []string{"ptahschemas", "ptahschemaplans"} {
		if !p.canI(t, authorizationv1.ResourceAttributes{
			Verb: "update", Group: "operator.ptah.run", Resource: resource, Subresource: "finalizers",
		}) {
			p.fatalf(t, "controller service account cannot update the %s.operator.ptah.run finalizers subresource", resource)
		}
	}
}

// realmAuthorization holds the manager to reading realms and writing none. A
// realm is an administrator's grant, and an identity that could edit the
// grant it is judged by would be the author of its own authorization.
func (p *controlPlanePhase) realmAuthorization(t *testing.T) {
	realm := func(verb string) authorizationv1.ResourceAttributes {
		return authorizationv1.ResourceAttributes{Verb: verb, Group: "operator.ptah.run", Resource: "ptahrealms"}
	}
	for _, verb := range []string{"get", "list", "watch"} {
		if !p.canI(t, realm(verb)) {
			p.fatalf(t, "controller service account cannot %s ptahrealms, so the realm census cannot read a grant", verb)
		}
	}
	for _, verb := range []string{"create", "update", "patch", "delete", "deletecollection"} {
		if p.canI(t, realm(verb)) {
			p.fatalf(t, "controller service account can %s ptahrealms, which only an administrator may", verb)
		}
	}
}

// planChunkAuthorization holds the manager to what it does with a plan's
// bytes. They are PtahSchemaPlanChunk objects it writes when it publishes the
// plan and reads back by name, uncached; it never lists or watches them, and
// never changes one. The ConfigMaps an Apply mounts the plan through are the
// one ConfigMap write it keeps, and it creates them and changes none.
func (p *controlPlanePhase) planChunkAuthorization(t *testing.T) {
	chunk := func(verb string) authorizationv1.ResourceAttributes {
		return authorizationv1.ResourceAttributes{Verb: verb, Group: "operator.ptah.run", Resource: "ptahschemaplanchunks"}
	}
	for _, verb := range []string{"get", "create"} {
		if !p.canI(t, chunk(verb)) {
			p.fatalf(t, "controller service account cannot %s ptahschemaplanchunks, so it cannot publish or read a plan", verb)
		}
	}
	for _, verb := range []string{"list", "watch", "update", "patch", "delete", "deletecollection"} {
		if p.canI(t, chunk(verb)) {
			p.fatalf(t, "controller service account can %s ptahschemaplanchunks, which it never needs", verb)
		}
	}
	if !p.canI(t, authorizationv1.ResourceAttributes{Verb: "create", Resource: "configmaps"}) {
		p.fatalf(t, "controller service account cannot create ConfigMaps, so no Apply Pod can mount its plan")
	}
	for _, verb := range []string{"update", "patch", "delete", "deletecollection"} {
		if p.canI(t, authorizationv1.ResourceAttributes{Verb: verb, Resource: "configmaps"}) {
			p.fatalf(t, "controller service account can %s ConfigMaps, and it only ever creates them", verb)
		}
	}
}

func (p *controlPlanePhase) webhookConfiguration(t *testing.T) {
	mutating := &admissionregistrationv1.MutatingWebhookConfiguration{}
	if err := p.cluster.Client.Get(p.ctx, types.NamespacedName{Name: admissionConfiguration}, mutating); err != nil {
		p.fatalf(t, "could not read the mutating admission singleton: %v", err)
	}
	if err := mutatingAdmissionExact(mutating, p.in.OperatorNamespace, p.webhookService, p.requireDistinctApprover); err != nil {
		p.fatalf(t, "approval mutating webhook is not exact and fail-closed: %v", err)
	}
	validating := &admissionregistrationv1.ValidatingWebhookConfiguration{}
	if err := p.cluster.Client.Get(p.ctx, types.NamespacedName{Name: admissionConfiguration}, validating); err != nil {
		p.fatalf(t, "could not read the validating admission singleton: %v", err)
	}
	if err := validatingAdmissionExact(validating, p.in.OperatorNamespace, p.webhookService, p.serviceAccount); err != nil {
		p.fatalf(t, "validating webhooks are not exact and fail-closed: %v", err)
	}
}

// secretIsolation holds the manager away from every Secret while it can
// still resolve the ServiceAccount and the LimitRanges an operation Pod is
// admitted against.
func (p *controlPlanePhase) secretIsolation(t *testing.T) {
	// The bootstrap creates these, because every suite needs them and
	// creating a namespace proves nothing. The phase still refuses to run
	// without them: a bootstrap that stopped creating them would otherwise be
	// found by whichever assertion needed one first.
	for _, namespace := range []string{p.in.TestNamespace, p.in.ForeignNamespace} {
		if err := p.cluster.Client.Get(p.ctx, types.NamespacedName{Name: namespace}, &corev1.Namespace{}); err != nil {
			p.fatalf(t, "namespace %s does not exist; the bootstrap creates it", namespace)
		}
	}
	for _, secret := range []struct{ namespace, name string }{
		{p.in.TestNamespace, "local-database"},
		{p.in.ForeignNamespace, "foreign-database"},
	} {
		if err := p.create(&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
			"metadata": map[string]any{"namespace": secret.namespace, "name": secret.name},
			"data":     map[string]any{"url": base64String(unusedDatabaseURL)},
		}}); err != nil {
			p.fatalf(t, "could not create Secret %s/%s: %v", secret.namespace, secret.name, err)
		}
	}
	for _, namespace := range []string{p.in.OperatorNamespace, p.in.TestNamespace, p.in.ForeignNamespace} {
		if !p.canI(t, authorizationv1.ResourceAttributes{Namespace: namespace, Verb: "get", Resource: "serviceaccounts", Name: "default"}) {
			p.fatalf(t, "controller service account cannot resolve a ServiceAccount in namespace %s", namespace)
		}
		if !p.canI(t, authorizationv1.ResourceAttributes{Namespace: namespace, Verb: "list", Resource: "limitranges"}) {
			p.fatalf(t, "controller service account cannot resolve LimitRanges in namespace %s", namespace)
		}
		for _, denied := range []struct{ verb, resource string }{
			{"list", "serviceaccounts"}, {"watch", "serviceaccounts"},
			{"get", "limitranges"}, {"watch", "limitranges"},
			{"create", "serviceaccounts"}, {"update", "serviceaccounts"},
			{"patch", "serviceaccounts"}, {"delete", "serviceaccounts"},
			{"create", "limitranges"}, {"update", "limitranges"},
			{"patch", "limitranges"}, {"delete", "limitranges"},
		} {
			if p.canI(t, authorizationv1.ResourceAttributes{Namespace: namespace, Verb: denied.verb, Resource: denied.resource}) {
				p.fatalf(t, "controller service account can %s %s in namespace %s", denied.verb, denied.resource, namespace)
			}
		}
	}
	for _, namespace := range []string{p.in.OperatorNamespace, p.in.TestNamespace, p.in.ForeignNamespace} {
		for _, verb := range []string{"get", "list", "watch"} {
			if p.canI(t, authorizationv1.ResourceAttributes{Namespace: namespace, Verb: verb, Resource: "secrets"}) {
				p.fatalf(t, "controller service account can %s Secrets in namespace %s", verb, namespace)
			}
		}
	}
	for _, secret := range []struct{ namespace, name string }{
		{p.in.OperatorNamespace, p.webhookCertSecret},
		{p.in.TestNamespace, "local-database"},
		{p.in.ForeignNamespace, "foreign-database"},
	} {
		if err := p.cluster.Client.Get(p.ctx, types.NamespacedName{Namespace: secret.namespace, Name: secret.name}, &corev1.Secret{}); err != nil {
			p.fatalf(t, "Secret %s/%s is not there to be refused: %v", secret.namespace, secret.name, err)
		}
		if p.canI(t, authorizationv1.ResourceAttributes{Namespace: secret.namespace, Verb: "get", Resource: "secrets", Name: secret.name}) {
			p.fatalf(t, "controller service account can read Secret %s/%s", secret.namespace, secret.name)
		}
	}
}

func (p *controlPlanePhase) namespaceLocalReferences(t *testing.T) {
	schemas := &apiextensionsv1.CustomResourceDefinition{}
	if err := p.cluster.Client.Get(p.ctx, types.NamespacedName{Name: "ptahschemas.operator.ptah.run"}, schemas); err != nil {
		p.fatalf(t, "could not read the PtahSchema CRD: %v", err)
	}
	if err := schemaReferencesLocal(schemas); err != nil {
		p.fatalf(t, "PtahSchema exposes a cross-namespace reference: %v", err)
	}
	approvals := &apiextensionsv1.CustomResourceDefinition{}
	if err := p.cluster.Client.Get(p.ctx, types.NamespacedName{Name: "ptahschemaapprovals.operator.ptah.run"}, approvals); err != nil {
		p.fatalf(t, "could not read the PtahSchemaApproval CRD: %v", err)
	}
	if err := approvalReferencesLocal(approvals); err != nil {
		p.fatalf(t, "PtahSchemaApproval exposes a cross-namespace reference: %v", err)
	}
}

// verificationPolicy holds the ConfigMap the bootstrap creates to the
// contract the fixtures rely on: it exists, it is immutable, and it carries
// the committed policy rather than something a run wrote.
func (p *controlPlanePhase) verificationPolicy(t *testing.T) {
	committed, err := os.ReadFile(filepath.Join(repositoryRoot, "testdata", "e2e", "verification-policy.yaml"))
	if err != nil {
		p.fatalf(t, "could not read the committed verification policy: %v", err)
	}
	policy := &corev1.ConfigMap{}
	if err := p.cluster.Client.Get(p.ctx, types.NamespacedName{Namespace: p.in.TestNamespace, Name: verificationPolicyName}, policy); err != nil {
		p.fatalf(t, "verification policy ConfigMap %s does not exist; the bootstrap creates it", verificationPolicyName)
	}
	if policy.Immutable == nil || !*policy.Immutable {
		p.fatalf(t, "verification policy ConfigMap is mutable")
	}
	stored, found := policy.Data[verificationPolicyKey]
	if !found || !bytes.Equal([]byte(stored), committed) {
		p.fatalf(t, "verification policy ConfigMap does not carry the committed policy")
	}
	p.policyUID = policy.UID
	p.policyDigest = sha256Digest(committed)
}

// podWebhookOutageScope removes the webhook Deployment and proves the Pod
// intent webhook's outage scope: an unrelated Job's Pod is still admitted, a
// managed-operation Pod is refused closed, and once the webhook is back a
// Job spoofing the managed labels is refused for its identity.
func (p *controlPlanePhase) podWebhookOutageScope(t *testing.T) {
	ns := p.in.OperatorNamespace
	controller := &appsv1.Deployment{}
	if err := p.cluster.Client.Get(p.ctx, types.NamespacedName{Namespace: ns, Name: p.controllerName}, controller); err != nil {
		p.fatalf(t, "could not read the webhook Deployment: %v", err)
	}
	p.deploymentReplicas = 1
	if controller.Spec.Replicas != nil {
		p.deploymentReplicas = int64(*controller.Spec.Replicas)
	}
	if p.deploymentReplicas < 1 {
		p.fatalf(t, "webhook Deployment does not have a positive replica count")
	}
	// The snapshot records the field manager that owns the Deployment as well
	// as the object. Helm 4 applies server-side, so restoring the snapshot as
	// another manager would leave every field to that manager, and the next
	// helm upgrade that changes one of them -- the data-plane suite's
	// four-eyes row changes the manager's arguments -- would fail with a
	// field-manager conflict instead of upgrading.
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("Deployment"))
	if err := p.cluster.Client.Get(p.ctx, types.NamespacedName{Namespace: ns, Name: p.controllerName}, live); err != nil {
		p.fatalf(t, "could not snapshot the webhook Deployment: %v", err)
	}
	manager, err := singleApplyManager(live.GetManagedFields())
	if err != nil {
		p.fatalf(t, "webhook Deployment %s has no single server-side apply field manager to restore: %v", p.controllerName, err)
	}
	p.deploymentManager, p.deploymentRestore = manager, restorableDeployment(live)
	p.deploymentStopped = true
	if err := p.cluster.Client.Delete(p.ctx, controller, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil {
		p.fatalf(t, "could not delete the webhook Deployment: %v", err)
	}
	if err := p.waitGone(p.ctx, &appsv1.Deployment{}, types.NamespacedName{Namespace: ns, Name: p.controllerName}, controller.UID, 5*time.Minute); err != nil {
		p.fatalf(t, "%v", err)
	}
	_ = harness.Wait(p.ctx, "the webhook Deployment and its ready endpoints to go", 180*time.Second, time.Second,
		func(ctx context.Context) (bool, string, error) {
			err := p.cluster.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: p.controllerName}, &appsv1.Deployment{})
			ready, readErr := p.webhookServiceReady(ctx)
			return apierrors.IsNotFound(err) && readErr == nil && !ready,
				fmt.Sprintf("Deployment read: %v; endpoint ready: %v (%v)", err, ready, readErr), nil
		})
	if ready, err := p.webhookServiceReady(p.ctx); err != nil || ready {
		p.fatalf(t, "webhook Service retained a ready endpoint after the Deployment was removed (%v)", err)
	}

	unrelatedUID := p.createWebhookScopeJob(t, webhookUnrelatedJob, false)
	err = harness.Wait(p.ctx, "the unrelated Job's Pod", 60*time.Second, time.Second,
		func(ctx context.Context) (bool, string, error) {
			pods, err := p.jobPods(ctx, webhookUnrelatedJob)
			if err != nil {
				return false, fmt.Sprintf("list failed: %v", err), nil
			}
			owned := ownedPods(pods, unrelatedUID)
			return len(owned) == 1, fmt.Sprintf("%d owned Pods", len(owned)), nil
		})
	if err != nil {
		p.fatalf(t, "webhook outage blocked an unrelated non-operation Job Pod: %v", err)
	}

	outageUID := p.createWebhookScopeJob(t, webhookOutageJob, true)
	if err := p.waitForFailedCreate(t, webhookOutageJob, outageUID,
		`(?i)failed calling webhook|no endpoints available|connection refused|service unavailable`); err != nil {
		p.fatalf(t, "webhook outage did not fail closed for a managed-operation Pod: %v", err)
	}

	if err := p.deleteWebhookScopeJobs(p.ctx); err != nil {
		p.fatalf(t, "could not delete the webhook scope Jobs: %v", err)
	}
	if err := p.restoreWebhookDeployment(p.ctx); err != nil {
		p.fatalf(t, "could not restore the webhook Deployment after the outage proof: %v", err)
	}

	spoofUID := p.createWebhookScopeJob(t, webhookSpoofJob, true)
	if err := p.waitForFailedCreate(t, webhookSpoofJob, spoofUID,
		`(?i)managed Pod Job has no exact operator controller identity`); err != nil {
		p.fatalf(t, "a foreign Job spoofing managed-operation labels was not denied by the Pod intent webhook: %v", err)
	}
	if err := p.deleteWebhookScopeJobs(p.ctx); err != nil {
		p.fatalf(t, "could not delete the webhook scope Jobs: %v", err)
	}
}

// webhookScopeJob is a Job whose Pod asks the Pod intent webhook one
// question: with the managed labels it is an operation Pod the webhook must
// judge, and without them one the webhook must not see.
func (p *controlPlanePhase) webhookScopeJob(name string, managed bool) map[string]any {
	labels := map[string]any{"operator.ptah.run/e2e-webhook-scope": "unrelated"}
	env := []any{}
	if managed {
		labels = map[string]any{
			"app.kubernetes.io/managed-by": "ptah-operator",
			"app.kubernetes.io/component":  "schema-operation",
		}
		env = []any{map[string]any{
			"name":      "PTAH_DB_URL",
			"valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "local-database", "key": "url"}},
		}}
	}
	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata":   map[string]any{"namespace": p.in.TestNamespace, "name": name, "labels": labels},
		"spec": map[string]any{
			"backoffLimit": int64(0),
			"template": map[string]any{
				"metadata": map[string]any{"labels": deepCopyMap(labels)},
				"spec": map[string]any{
					"automountServiceAccountToken": false,
					"restartPolicy":                "Never",
					"containers": []any{map[string]any{
						"name":            "probe",
						"image":           p.in.RunnerImage,
						"imagePullPolicy": "IfNotPresent",
						"command":         []any{"/ptah-runner"},
						"args":            []any{"--version"},
						"env":             env,
						"securityContext": map[string]any{
							"allowPrivilegeEscalation": false,
							"capabilities":             map[string]any{"drop": []any{"ALL"}},
						},
					}},
				},
			},
		},
	}
}

func (p *controlPlanePhase) createWebhookScopeJob(t *testing.T, name string, managed bool) types.UID {
	t.Helper()
	job := &unstructured.Unstructured{Object: p.webhookScopeJob(name, managed)}
	if err := p.create(job); err != nil {
		p.fatalf(t, "could not create Job %s: %v", name, err)
	}
	created := &batchv1.Job{}
	if err := p.cluster.Client.Get(p.ctx, types.NamespacedName{Namespace: p.in.TestNamespace, Name: name}, created); err != nil {
		p.fatalf(t, "could not read Job %s: %v", name, err)
	}
	return created.UID
}

func (p *controlPlanePhase) jobPods(ctx context.Context, job string) ([]corev1.Pod, error) {
	pods := &corev1.PodList{}
	err := p.cluster.Client.List(ctx, pods, client.InNamespace(p.in.TestNamespace), client.MatchingLabels{"job-name": job})
	return pods.Items, err
}

// waitForFailedCreate waits for the Job's FailedCreate Event naming the Pod
// intent webhook and the reason, with no Pod the Job owns. Every reading of
// the Events is first scanned for the database credential the refused Pod
// referenced: a refusal that repeats it is a leak whatever else it says.
func (p *controlPlanePhase) waitForFailedCreate(t *testing.T, job string, uid types.UID, reason string) error {
	t.Helper()
	pattern := regexp.MustCompile(reason)
	return harness.Wait(p.ctx, "a FailedCreate Event for Job "+job, 90*time.Second, time.Second,
		func(ctx context.Context) (bool, string, error) {
			events := &corev1.EventList{}
			if err := p.cluster.Client.List(ctx, events, client.InNamespace(p.in.TestNamespace),
				client.MatchingFields{"involvedObject.kind": "Job", "involvedObject.name": job}); err != nil {
				return false, fmt.Sprintf("Event list failed: %v", err), nil
			}
			document, err := json.Marshal(events)
			if err != nil {
				return false, "", err
			}
			if bytes.Contains(document, []byte(unusedDatabaseURL)) {
				p.fatalf(t, "Pod admission failure Event exposed the referenced database credential")
			}
			if !podCreationRefused(events.Items, uid, pattern) {
				return false, fmt.Sprintf("%d Events, none the refusal", len(events.Items)), nil
			}
			pods, err := p.jobPods(ctx, job)
			if err != nil {
				return false, fmt.Sprintf("Pod list failed: %v", err), nil
			}
			if referencesJob(pods, uid) {
				return false, "a Pod of the Job exists", nil
			}
			return true, "refused, with no Pod", nil
		})
}

// deleteWebhookScopeJobs deletes the three Jobs in the foreground and waits
// until they and their Pods are gone.
func (p *controlPlanePhase) deleteWebhookScopeJobs(ctx context.Context) error {
	for _, name := range []string{webhookUnrelatedJob, webhookOutageJob, webhookSpoofJob} {
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: p.in.TestNamespace}}
		if err := p.deleteAndWait(ctx, job, 3*time.Minute, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil {
			return err
		}
	}
	return nil
}

// deleteAndWait deletes the object, a missing one included, and waits until
// the object it deleted is gone: kubectl's --wait counts an object of the
// same name with another UID as its deletion done.
func (p *controlPlanePhase) deleteAndWait(ctx context.Context, object client.Object, timeout time.Duration, options ...client.DeleteOption) error {
	key := client.ObjectKeyFromObject(object)
	current, ok := object.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("%T is not an object", object)
	}
	if err := p.cluster.Client.Get(ctx, key, current); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	if err := p.cluster.Client.Delete(ctx, current, options...); client.IgnoreNotFound(err) != nil {
		return err
	}
	fresh, _ := object.DeepCopyObject().(client.Object)
	return p.waitGone(ctx, fresh, key, current.GetUID(), timeout)
}

func (p *controlPlanePhase) waitGone(ctx context.Context, object client.Object, key types.NamespacedName, uid types.UID, timeout time.Duration) error {
	return harness.Wait(ctx, fmt.Sprintf("the deletion of %T %s", object, key), timeout, time.Second,
		func(ctx context.Context) (bool, string, error) {
			err := p.cluster.Client.Get(ctx, key, object)
			switch {
			case apierrors.IsNotFound(err):
				return true, "gone", nil
			case err != nil:
				return false, fmt.Sprintf("read failed: %v", err), nil
			case object.GetUID() != uid:
				return true, "replaced by another object", nil
			}
			return false, "still present", nil
		})
}

func (p *controlPlanePhase) webhookServiceReady(ctx context.Context) (bool, error) {
	endpointSlices := &discoveryv1.EndpointSliceList{}
	if err := p.cluster.Client.List(ctx, endpointSlices, client.InNamespace(p.in.OperatorNamespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: p.webhookService}); err != nil {
		return false, err
	}
	return anyReadyEndpoint(endpointSlices.Items), nil
}

// restoreWebhookDeployment brings back a Deployment the outage row removed,
// applied as the field manager that owned it, and waits for its rollout and
// a ready webhook endpoint. It does nothing when the Deployment was never
// stopped or is back already.
func (p *controlPlanePhase) restoreWebhookDeployment(ctx context.Context) error {
	if !p.deploymentStopped {
		return nil
	}
	if p.deploymentRestore == nil || p.deploymentManager == "" {
		return fmt.Errorf("the webhook Deployment was stopped with no snapshot to restore")
	}
	key := types.NamespacedName{Namespace: p.in.OperatorNamespace, Name: p.controllerName}
	err := p.cluster.Client.Get(ctx, key, &appsv1.Deployment{})
	switch {
	case apierrors.IsNotFound(err):
		apply := client.ApplyConfigurationFromUnstructured(p.deploymentRestore.DeepCopy())
		if err := p.cluster.Client.Apply(ctx, apply, client.FieldOwner(p.deploymentManager)); err != nil {
			return fmt.Errorf("apply the snapshot as %s: %w", p.deploymentManager, err)
		}
	case err != nil:
		return err
	}
	if err := p.cluster.WaitForRollout(ctx, p.in.OperatorNamespace, p.controllerName, 180*time.Second); err != nil {
		return err
	}
	err = harness.Wait(ctx, "a ready webhook endpoint after the restore", 60*time.Second, time.Second,
		func(ctx context.Context) (bool, string, error) {
			ready, err := p.webhookServiceReady(ctx)
			return err == nil && ready, fmt.Sprintf("ready %v (%v)", ready, err), nil
		})
	if err != nil {
		return err
	}
	p.deploymentStopped = false
	return nil
}

// schemaWith is the suspended schema fixture with one change, for a request
// the API server must refuse.
func (p *controlPlanePhase) schemaWith(change func(map[string]any)) *unstructured.Unstructured {
	document := deepCopyMap(p.suspendedSchema())
	change(document)
	return &unstructured.Unstructured{Object: document}
}

// suspendedSchema is the schema fixture every refusal starts from: suspended,
// so nothing it names is ever reached.
func (p *controlPlanePhase) suspendedSchema() map[string]any {
	return map[string]any{
		"apiVersion": "operator.ptah.run/v1alpha1",
		"kind":       "PtahSchema",
		"metadata":   map[string]any{"namespace": p.in.TestNamespace, "name": suspendedSchemaName},
		"spec": map[string]any{
			"target": map[string]any{
				"engine":          "PostgreSQL",
				"coordinationKey": suspendedCoordinationKey,
				"urlFrom":         map[string]any{"name": "database-url", "key": "url"},
			},
			"desired": map[string]any{
				"ociRef":                 "oci://registry.invalid/e2e/schema:unused",
				"verificationPolicyFrom": map[string]any{"name": verificationPolicyName, "key": verificationPolicyKey},
			},
			"suspend": true,
		},
	}
}

// expectDenied creates the object and requires the API server to refuse it
// for the reason the pattern names, as kubectl create's strict validation
// asks.
func (p *controlPlanePhase) expectDenied(t *testing.T, description, reason string, object *unstructured.Unstructured) {
	t.Helper()
	err := p.create(object)
	if err == nil {
		p.fatalf(t, "%s unexpectedly succeeded", description)
	}
	if !regexp.MustCompile("(?i)" + reason).MatchString(err.Error()) {
		p.fatalf(t, "%s did not fail for the expected reason: %v", description, err)
	}
}

func (p *controlPlanePhase) create(object *unstructured.Unstructured) error {
	return p.cluster.Client.Create(p.ctx, object, client.FieldValidation("Strict"), client.FieldOwner(harness.FieldOwner))
}

func (p *controlPlanePhase) durationBounds(t *testing.T) {
	for _, row := range []struct {
		description, reason string
		change              func(map[string]any)
	}{
		{"nanosecond interval", "interval must be between 10s and 24h",
			func(s map[string]any) { set(s, "1ns", "spec", "interval") }},
		{"negative failure retry interval", "failureRetryInterval must be between 5s and 1h",
			func(s map[string]any) { set(s, "-1s", "spec", "execution", "failureRetryInterval") }},
		{"nanosecond connect timeout", "connectTimeout must be between 1s and 10m",
			func(s map[string]any) { set(s, "1ns", "spec", "execution", "connectTimeout") }},
		{"negative lock timeout", "lockTimeout must be between 1s and 10m",
			func(s map[string]any) { set(s, "-1s", "spec", "policy", "lockTimeout") }},
		{"connect timeout over active deadline", "connectTimeout must not exceed execution.activeDeadlineSeconds",
			func(s map[string]any) {
				set(s, map[string]any{"activeDeadlineSeconds": int64(30), "connectTimeout": "31s"}, "spec", "execution")
			}},
		{"lock timeout over active deadline", "lockTimeout must not exceed execution.activeDeadlineSeconds",
			func(s map[string]any) {
				set(s, int64(30), "spec", "execution", "activeDeadlineSeconds")
				set(s, "31s", "spec", "policy", "lockTimeout")
			}},
	} {
		p.expectDenied(t, row.description, row.reason, p.schemaWith(row.change))
	}
}

// operationJobUIDs is the UIDs of the Jobs in the test namespace, sorted, so
// two readings compare whole.
func (p *controlPlanePhase) operationJobUIDs(t *testing.T) []types.UID {
	t.Helper()
	jobs := &batchv1.JobList{}
	if err := p.cluster.Client.List(p.ctx, jobs, client.InNamespace(p.in.TestNamespace)); err != nil {
		p.fatalf(t, "could not list Jobs: %v", err)
	}
	uids := make([]types.UID, 0, len(jobs.Items))
	for _, job := range jobs.Items {
		uids = append(uids, job.UID)
	}
	slices.Sort(uids)
	return uids
}

func (p *controlPlanePhase) referenceKeys(t *testing.T) {
	before := p.operationJobUIDs(t)
	for _, row := range []struct {
		description, reason string
		change              func(map[string]any)
	}{
		{"empty target Secret key", "target.urlFrom must name a required Secret key",
			func(s map[string]any) { set(s, "", "spec", "target", "urlFrom", "key") }},
		{"empty development target Secret key", "dev.urlFrom must name a required Secret key",
			func(s map[string]any) {
				set(s, map[string]any{"urlFrom": map[string]any{"name": "development-url", "key": ""}}, "spec", "dev")
			}},
		{"empty verification policy ConfigMap key", "desired.verificationPolicyFrom must name a required ConfigMap key",
			func(s map[string]any) { set(s, "", "spec", "desired", "verificationPolicyFrom", "key") }},
		{"empty OCI CA ConfigMap key", "desired.transport.caFrom must name a required ConfigMap key",
			func(s map[string]any) {
				set(s, map[string]any{"name": "registry-ca", "key": ""}, "spec", "desired", "transport", "caFrom")
			}},
	} {
		p.expectDenied(t, row.description, row.reason, p.schemaWith(row.change))
	}
	if after := p.operationJobUIDs(t); !slices.Equal(after, before) {
		p.fatalf(t, "a rejected empty reference key created an operation Job")
	}
}

func (p *controlPlanePhase) managedScopeSelectors(t *testing.T) {
	before := p.operationJobUIDs(t)
	for _, row := range []struct {
		description string
		exclude     []any
	}{
		{"whitespace-only managed-scope selector", []any{"   "}},
		{"leading whitespace in managed-scope selector", []any{" public.*"}},
		{"trailing whitespace in managed-scope selector", []any{"public.* "}},
		{"control character in managed-scope selector", []any{"public.\nusers"}},
		{"overlong managed-scope selector", []any{strings.Repeat("x", 257)}},
		{"duplicate managed-scope selector", []any{"public.*", "public.*"}},
	} {
		p.expectDenied(t, row.description, "spec.policy.exclude|policy.exclude",
			p.schemaWith(func(s map[string]any) { set(s, row.exclude, "spec", "policy", "exclude") }))
	}
	if after := p.operationJobUIDs(t); !slices.Equal(after, before) {
		p.fatalf(t, "a rejected managed-scope selector created an operation Job")
	}
}

// unsupportedEngine proves an engine the operator does not support reaches
// an explicit status for its current generation and creates nothing: no Job,
// no plan, no approval.
func (p *controlPlanePhase) unsupportedEngine(t *testing.T) {
	ns := p.in.TestNamespace
	document := p.schemaWith(func(s map[string]any) {
		set(s, unsupportedEngineSchema, "metadata", "name")
		set(s, "SQLite", "spec", "target", "engine")
		set(s, "e2e/admission/unsupported", "spec", "target", "coordinationKey")
		set(s, false, "spec", "suspend")
	})
	if err := p.create(document); err != nil {
		p.fatalf(t, "could not create the unsupported-engine schema: %v", err)
	}
	p.unsupportedSchema = true
	// The reading that matched is the one asserted on, so there is no window
	// between the wait and the assertion.
	var matched *ptahv1alpha1.PtahSchema
	err := harness.Wait(p.ctx, "the unsupported engine's explicit current-generation status", 90*time.Second, time.Second,
		func(ctx context.Context) (bool, string, error) {
			reading := &ptahv1alpha1.PtahSchema{}
			if err := p.cluster.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: unsupportedEngineSchema}, reading); err != nil {
				return false, "", fmt.Errorf("could not read the unsupported-engine schema: %w", err)
			}
			matched = reading
			return unsupportedEngineSettled(reading), fmt.Sprintf("phase %q, observed generation %d of %d",
				reading.Status.Phase, reading.Status.ObservedGeneration, reading.Generation), nil
		})
	if matched == nil {
		p.fatalf(t, "unsupported engine did not produce a readable PtahSchema")
	}
	if err != nil || !unsupportedEngineSettled(matched) {
		p.fatalf(t, "unsupported engine did not reach its explicit current-generation status: %v", err)
	}
	jobs := &batchv1.JobList{}
	if err := p.cluster.Client.List(p.ctx, jobs, client.InNamespace(ns)); err != nil {
		p.fatalf(t, "could not list Jobs: %v", err)
	}
	for _, job := range jobs.Items {
		for _, reference := range job.OwnerReferences {
			if reference.UID == matched.UID {
				p.fatalf(t, "unsupported engine created an owned operation Job")
			}
		}
	}
	plans := &ptahv1alpha1.PtahSchemaPlanList{}
	if err := p.cluster.Client.List(p.ctx, plans, client.InNamespace(ns)); err != nil {
		p.fatalf(t, "could not list plans: %v", err)
	}
	for _, plan := range plans.Items {
		if plan.Spec.SchemaRef.UID == matched.UID {
			p.fatalf(t, "unsupported engine created a plan")
		}
	}
	approvals := &ptahv1alpha1.PtahSchemaApprovalList{}
	if err := p.cluster.Client.List(p.ctx, approvals, client.InNamespace(ns)); err != nil {
		p.fatalf(t, "could not list approvals: %v", err)
	}
	for _, approval := range approvals.Items {
		if approval.Spec.SchemaRef.UID == matched.UID {
			p.fatalf(t, "unsupported engine created an approval")
		}
	}
	if err := p.deleteAndWait(p.ctx, matched.DeepCopy(), 3*time.Minute); err != nil {
		p.fatalf(t, "could not delete the unsupported-engine schema: %v", err)
	}
	p.unsupportedSchema = false
}

// suspendedSchemaFixture creates the suspended schema, holds it to the
// defaults and the execution binding it must carry, and publishes a plan
// fixture bound to it, with its chunk, and the status the approval webhook
// reads.
func (p *controlPlanePhase) suspendedSchemaFixture(t *testing.T) {
	ns := p.in.TestNamespace
	p.schemaFixture = p.suspendedSchema()
	if err := p.create(&unstructured.Unstructured{Object: deepCopyMap(p.schemaFixture)}); err != nil {
		p.fatalf(t, "could not create the suspended schema: %v", err)
	}
	err := harness.Wait(p.ctx, "the suspended schema's phase Suspended", 90*time.Second, time.Second,
		func(ctx context.Context) (bool, string, error) {
			reading := &ptahv1alpha1.PtahSchema{}
			if err := p.cluster.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: suspendedSchemaName}, reading); err != nil {
				return false, fmt.Sprintf("read failed: %v", err), nil
			}
			return reading.Status.Phase == "Suspended", fmt.Sprintf("phase %q", reading.Status.Phase), nil
		})
	if err != nil {
		p.fatalf(t, "%v", err)
	}
	stored := &unstructured.Unstructured{}
	stored.SetGroupVersionKind(ptahv1alpha1.GroupVersion.WithKind("PtahSchema"))
	if err := p.cluster.Client.Get(p.ctx, types.NamespacedName{Namespace: ns, Name: suspendedSchemaName}, stored); err != nil {
		p.fatalf(t, "could not read the suspended schema: %v", err)
	}
	if err := schemaDefaultsPersisted(stored); err != nil {
		p.fatalf(t, "PtahSchema API defaults were not persisted for omitted safe policy and execution fields: %v", err)
	}
	p.schemaUID, p.schemaGeneration = stored.GetUID(), stored.GetGeneration()
	if p.executionEpoch, err = executionBindingExact(stored, p.runnerProtocol, p.stateVersion,
		p.in.PtahVersion, p.in.ExecutorImage); err != nil {
		p.fatalf(t, "suspended schema lacks the exact four-component execution binding: %v", err)
	}
	controller := &appsv1.Deployment{}
	if err := p.cluster.Client.Get(p.ctx, types.NamespacedName{Namespace: p.in.OperatorNamespace, Name: p.controllerName}, controller); err != nil {
		p.fatalf(t, "could not read the controller Deployment: %v", err)
	}
	deployedImage, err := controllerImageArgument(controller)
	if err != nil {
		p.fatalf(t, "%v", err)
	}
	if deployedImage != p.in.ControllerImage {
		p.fatalf(t, "manager controller image argument does not match the externally expected image identity")
	}

	// The suspended schema's own realm. It is the one value in this binding
	// the webhook derives rather than copies, so it is derived here too.
	coordination, err := coordinationDigest("postgresql", ns, suspendedCoordinationKey)
	if err != nil {
		p.fatalf(t, "%v", err)
	}
	createdAt := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	p.planFingerprint, err = planBinding{
		ContractVersion: int(fingerprint.CurrentPlanContractVersion), SchemaUID: string(p.schemaUID), PlanContentDigest: contentDigestFixture,
		ArtifactDigest: artifactDigestFixture, CoordinationDigest: coordination, TargetIdentityDigest: targetDigestFixture,
		ActualStateFingerprint: actualFingerprintFixture, DesiredStateFingerprint: desiredFingerprintFixture,
		PolicyFingerprint: policyFingerprintFixture, VerificationPolicyUID: string(p.policyUID),
		VerificationPolicyDigest: p.policyDigest, ExecutionBindingID: p.executionEpoch,
		ControllerStateVersion: p.stateVersion, PtahVersion: p.in.PtahVersion, ExecutorImage: p.in.ExecutorImage,
		RunnerProtocolVersion: p.runnerProtocol, Destructive: false, StatementCount: 1,
	}.fingerprint()
	if err != nil {
		p.fatalf(t, "%v", err)
	}
	// The plan records the manager that publishes it, as the manager itself
	// does: its image argument, its revision and its runner image.
	p.planFixture = map[string]any{
		"apiVersion": "operator.ptah.run/v1alpha1",
		"kind":       "PtahSchemaPlan",
		"metadata": map[string]any{
			"namespace": ns,
			"name":      fixturePlanName,
			"labels":    map[string]any{"operator.ptah.run/schema": suspendedSchemaName},
			"ownerReferences": []any{map[string]any{
				"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahSchema",
				"name": suspendedSchemaName, "uid": string(p.schemaUID),
				"controller": true, "blockOwnerDeletion": true,
			}},
		},
		"spec": map[string]any{
			"contractVersion":          int64(fingerprint.CurrentPlanContractVersion),
			"schemaRef":                map[string]any{"name": suspendedSchemaName, "uid": string(p.schemaUID)},
			"fingerprint":              p.planFingerprint,
			"contentDigest":            contentDigestFixture,
			"size":                     int64(1),
			"artifactDigest":           artifactDigestFixture,
			"coordinationDigest":       coordination,
			"targetIdentityDigest":     targetDigestFixture,
			"actualStateFingerprint":   actualFingerprintFixture,
			"desiredStateFingerprint":  desiredFingerprintFixture,
			"policyFingerprint":        policyFingerprintFixture,
			"verificationPolicyUID":    string(p.policyUID),
			"verificationPolicyDigest": p.policyDigest,
			"executionBindingID":       p.executionEpoch,
			"controllerImage":          deployedImage,
			"controllerRevision":       p.in.ControllerRevision,
			"controllerStateVersion":   p.stateVersion,
			"ptahVersion":              p.in.PtahVersion,
			"executorImage":            p.in.ExecutorImage,
			"runnerImage":              p.in.RunnerImage,
			"runnerProtocolVersion":    p.runnerProtocol,
			"dialect":                  "postgres",
			"destructive":              false,
			"statementCount":           int64(1),
			"chunks": []any{map[string]any{
				"name": fixturePlanChunkName, "index": int64(0),
				"digest": contentDigestFixture, "size": int64(1),
			}},
		},
	}
	plan := &unstructured.Unstructured{Object: deepCopyMap(p.planFixture)}
	if err := p.create(plan); err != nil {
		p.fatalf(t, "could not create the plan fixture: %v", err)
	}
	p.planUID = plan.GetUID()
	planGeneration := plan.GetGeneration()

	chunk := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "operator.ptah.run/v1alpha1",
		"kind":       "PtahSchemaPlanChunk",
		"metadata": map[string]any{
			"namespace": ns,
			"name":      fixturePlanChunkName,
			"labels": map[string]any{
				"operator.ptah.run/plan":   fixturePlanName,
				"operator.ptah.run/schema": suspendedSchemaName,
			},
			"ownerReferences": []any{map[string]any{
				"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahSchemaPlan",
				"name": fixturePlanName, "uid": string(p.planUID),
				"controller": true, "blockOwnerDeletion": true,
			}},
		},
		"spec": map[string]any{"data": "eA=="},
	}}
	if err := p.create(chunk); err != nil {
		p.fatalf(t, "could not create the plan chunk: %v", err)
	}
	p.patchStatus(t, &ptahv1alpha1.PtahSchemaPlan{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: fixturePlanName}},
		map[string]any{"status": map[string]any{
			"observedGeneration": planGeneration,
			"publishedChunks":    []any{map[string]any{"name": fixturePlanChunkName, "uid": string(chunk.GetUID()), "index": int64(0)}},
			"conditions": []any{map[string]any{
				"type": "Ready", "status": "True", "reason": "E2EFixture",
				"message":            "Plan fixture is committed for admission testing",
				"lastTransitionTime": createdAt,
			}},
		}})
	p.patchStatus(t, &ptahv1alpha1.PtahSchema{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: suspendedSchemaName}},
		map[string]any{"status": map[string]any{
			"observedGeneration": p.schemaGeneration,
			"phase":              "AwaitingApproval",
			"conditions": []any{
				map[string]any{
					"type": "Ready", "status": "False", "reason": "ApprovalRequired",
					"message": "Plan fixture is awaiting approval", "lastTransitionTime": createdAt,
				},
				map[string]any{
					"type": "ApprovalRequired", "status": "True", "reason": "Policy",
					"message": "Plan fixture requires explicit approval", "lastTransitionTime": createdAt,
				},
			},
			"source": map[string]any{
				"digest":                   artifactDigestFixture,
				"verificationPolicyUID":    string(p.policyUID),
				"verificationPolicyDigest": p.policyDigest,
			},
			"target": map[string]any{
				"engine":             "PostgreSQL",
				"coordinationDigest": coordination,
				"identityDigest":     targetDigestFixture,
				"driftReportDigest":  driftReportDigestFixture,
			},
			"plan": map[string]any{
				"name":                     fixturePlanName,
				"uid":                      string(p.planUID),
				"fingerprint":              p.planFingerprint,
				"contentDigest":            contentDigestFixture,
				"artifactDigest":           artifactDigestFixture,
				"coordinationDigest":       coordination,
				"targetIdentityDigest":     targetDigestFixture,
				"actualStateFingerprint":   actualFingerprintFixture,
				"desiredStateFingerprint":  desiredFingerprintFixture,
				"policyFingerprint":        policyFingerprintFixture,
				"verificationPolicyUID":    string(p.policyUID),
				"verificationPolicyDigest": p.policyDigest,
				"executionBindingID":       p.executionEpoch,
				"controllerImage":          deployedImage,
				"controllerRevision":       p.in.ControllerRevision,
				"controllerStateVersion":   p.stateVersion,
				"ptahVersion":              p.in.PtahVersion,
				"executorImage":            p.in.ExecutorImage,
				"runnerImage":              p.in.RunnerImage,
				"runnerProtocolVersion":    p.runnerProtocol,
				"destructive":              false,
				"statementCount":           int64(1),
				"createdAt":                createdAt,
			},
		}})
}

// patchStatus writes a fixture's status as the manager's ServiceAccount.
// Status is the manager's alone: the chart's status guard refuses every other
// writer, this harness included, and the fixture stands in for what the
// manager would have written.
func (p *controlPlanePhase) patchStatus(t *testing.T, object client.Object, status map[string]any) {
	t.Helper()
	if p.serviceAccount == "" {
		p.fatalf(t, "cannot write the status of %s before the manager's ServiceAccount is known", object.GetName())
	}
	patch, err := json.Marshal(status)
	if err != nil {
		p.fatalf(t, "encode the status of %s: %v", object.GetName(), err)
	}
	manager, err := p.cluster.As(rest.ImpersonationConfig{UserName: p.serviceAccount})
	if err != nil {
		p.fatalf(t, "could not build a client that acts as %s: %v", p.serviceAccount, err)
	}
	if err := manager.Status().Patch(p.ctx, object, client.RawPatch(types.MergePatchType, patch),
		client.FieldOwner(harness.FieldOwner)); err != nil {
		p.fatalf(t, "could not write the status of %s as %s: %v", object.GetName(), p.serviceAccount, err)
	}
}

// approval is an approval of the fixture plan by the given name and
// fingerprint, as a person writes it: the decision, and nothing the webhook
// stamps.
func (p *controlPlanePhase) approval(name, fingerprint string) map[string]any {
	return map[string]any{
		"apiVersion": "operator.ptah.run/v1alpha1",
		"kind":       "PtahSchemaApproval",
		"metadata":   map[string]any{"namespace": p.in.TestNamespace, "name": name},
		"spec": map[string]any{
			"schemaRef":       map[string]any{"name": suspendedSchemaName, "uid": string(p.schemaUID)},
			"planRef":         map[string]any{"name": fixturePlanName, "uid": string(p.planUID)},
			"planFingerprint": fingerprint,
		},
	}
}

func (p *controlPlanePhase) approvalWith(name string, change func(map[string]any)) *unstructured.Unstructured {
	document := deepCopyMap(p.approvalFixture)
	set(document, name, "metadata", "name")
	change(document)
	return &unstructured.Unstructured{Object: document}
}

func (p *controlPlanePhase) approvalBinding(t *testing.T) {
	ns := p.in.TestNamespace
	p.approvalFixture = p.approval(fixtureApprovalName, p.planFingerprint)
	if err := p.create(&unstructured.Unstructured{Object: deepCopyMap(p.approvalFixture)}); err != nil {
		p.fatalf(t, "could not create the approval: %v", err)
	}
	// The stored approval is the decision as written -- schema, plan and the
	// plan's fingerprint -- plus who made it and when. The plan's own
	// bindings stay on the plan: the fingerprint names every one of them, so
	// the spec carries exactly these six keys and nothing copied from it.
	stored := &unstructured.Unstructured{}
	stored.SetGroupVersionKind(ptahv1alpha1.GroupVersion.WithKind("PtahSchemaApproval"))
	if err := p.cluster.Client.Get(p.ctx, types.NamespacedName{Namespace: ns, Name: fixtureApprovalName}, stored); err != nil {
		p.fatalf(t, "could not read the approval: %v", err)
	}
	if err := approvalStampedExact(stored,
		ptahv1alpha1.ImmutableObjectReference{Name: suspendedSchemaName, UID: p.schemaUID},
		ptahv1alpha1.ImmutableObjectReference{Name: fixturePlanName, UID: p.planUID},
		p.planFingerprint); err != nil {
		p.fatalf(t, "mutating webhook did not stamp identity onto the exact decision: %v", err)
	}

	for _, missing := range []struct {
		binding, reason string
		path            []string
	}{
		{"schema-name", "explicitly identify the schema name and UID", []string{"spec", "schemaRef", "name"}},
		{"schema-uid", "explicitly identify the schema name and UID", []string{"spec", "schemaRef", "uid"}},
		{"plan-name", "explicitly identify the plan name and UID", []string{"spec", "planRef", "name"}},
		{"plan-uid", "explicitly identify the plan name and UID", []string{"spec", "planRef", "uid"}},
		{"plan-fingerprint", "explicitly identify the plan fingerprint", []string{"spec", "planFingerprint"}},
	} {
		p.expectDenied(t, "approval without explicit "+missing.binding, missing.reason,
			p.approvalWith("e2e-missing-"+missing.binding, func(a map[string]any) {
				unstructured.RemoveNestedField(a, missing.path...)
			}))
	}
	// A binding of the plan written onto the approval is neither corrected
	// nor compared: the API has no such field, so the server refuses the
	// document before any webhook reads it.
	p.expectDenied(t, "approval carrying a plan binding the API dropped", `unknown field "spec\.executorImage"`,
		p.approvalWith("e2e-copied-executor-image", func(a map[string]any) {
			set(a, "e2e.invalid/ptah@sha256:"+strings.Repeat("a", 64), "spec", "executorImage")
		}))
	// The plan named exists and carries the fingerprint the approval names;
	// the schema the approval names is not the one the plan was published
	// for.
	p.expectDenied(t, "approval naming a plan of another schema", "schema reference does not match the plan",
		p.approvalWith("e2e-another-schema", func(a map[string]any) {
			set(a, "00000000-0000-4000-8000-000000000000", "spec", "schemaRef", "uid")
		}))
	p.expectDenied(t, "approval naming a replaced plan", "plan UID does not match; the plan was replaced",
		p.approvalWith("e2e-replaced-plan", func(a map[string]any) {
			set(a, "00000000-0000-4000-8000-000000000000", "spec", "planRef", "uid")
		}))
	p.expectDenied(t, "approval with a stale plan binding", "plan fingerprint does not match the immutable plan",
		&unstructured.Unstructured{Object: p.approval("e2e-invalid-approval", staleFingerprintFixture)})

	err := p.cluster.Client.Patch(p.ctx,
		&ptahv1alpha1.PtahSchemaApproval{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: fixtureApprovalName}},
		client.RawPatch(types.MergePatchType, []byte(`{"spec":{"planFingerprint":"`+staleFingerprintFixture+`"}}`)),
		client.FieldOwner(harness.FieldOwner))
	if err == nil {
		p.fatalf(t, "immutable approval spec update unexpectedly succeeded")
	}
	if !regexp.MustCompile(`(?i)immutable`).MatchString(err.Error()) {
		p.fatalf(t, "approval update was refused for an unexpected reason: %v", err)
	}
}

// crossNamespaceApproval proves an approval cannot reach a plan in another
// namespace, and that trying leaves that plan exactly as it was.
func (p *controlPlanePhase) crossNamespaceApproval(t *testing.T) {
	foreign := deepCopyMap(p.planFixture)
	set(foreign, p.in.ForeignNamespace, "metadata", "namespace")
	set(foreign, foreignPlanName, "metadata", "name")
	unstructured.RemoveNestedField(foreign, "metadata", "ownerReferences")
	unstructured.RemoveNestedField(foreign, "metadata", "finalizers")
	plan := &unstructured.Unstructured{Object: foreign}
	if err := p.create(plan); err != nil {
		p.fatalf(t, "could not create the foreign plan: %v", err)
	}
	if p.foreignPlanUID = plan.GetUID(); p.foreignPlanUID == "" {
		p.fatalf(t, "foreign plan was created without a UID")
	}
	p.assertForeignPlanStable(t)
	err := p.cluster.Client.Get(p.ctx, types.NamespacedName{Namespace: p.in.TestNamespace, Name: foreignPlanName}, &ptahv1alpha1.PtahSchemaPlan{})
	if !apierrors.IsNotFound(err) {
		p.fatalf(t, "cross-namespace approval fixture unexpectedly has a local plan (%v)", err)
	}
	p.expectDenied(t, "cross-namespace approval", "foreign-plan.*not found|not found.*foreign-plan",
		p.approvalWith(crossNamespaceApprovalName, func(a map[string]any) {
			set(a, foreignPlanName, "spec", "planRef", "name")
			set(a, string(p.foreignPlanUID), "spec", "planRef", "uid")
		}))
	p.assertForeignPlanStable(t)
	err = p.cluster.Client.Get(p.ctx, types.NamespacedName{Namespace: p.in.TestNamespace, Name: crossNamespaceApprovalName}, &ptahv1alpha1.PtahSchemaApproval{})
	if !apierrors.IsNotFound(err) {
		p.fatalf(t, "cross-namespace approval refusal created an approval (%v)", err)
	}
	// The four-eyes control is approvals.requireDistinctApprover, a chart
	// value the installer sets, because a field on the resource's own spec
	// could not bind that resource's author. Its proof -- a self-approval
	// refused and a distinct approver admitted and applied -- needs a live
	// database and lives in the data-plane phase, which this phase runs
	// before any database exists.
}

func (p *controlPlanePhase) assertForeignPlanStable(t *testing.T) {
	t.Helper()
	plan := &ptahv1alpha1.PtahSchemaPlan{}
	if err := p.cluster.Client.Get(p.ctx, types.NamespacedName{Namespace: p.in.ForeignNamespace, Name: foreignPlanName}, plan); err != nil ||
		!foreignPlanStable(plan, p.foreignPlanUID) {
		p.fatalf(t, "foreign plan disappeared, changed, or entered deletion (%v)", err)
	}
}
