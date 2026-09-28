//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
	"github.com/stokaro/ptah-operator/test/e2e/phases"
)

// brokenCA is what the corrupt-CA row writes over ca.crt and over one entry's
// bundle: bytes that are not a certificate at all.
var brokenCA = []byte("not a certificate")

// TestCertRotation is the certificates suite. Against the release the driver
// installed, it proves that the rotator's identity cannot create a Secret
// outside its recovery contract, that Helm's live lookup keeps each webhook
// entry's trust apart across an upgrade, that a corrupt CA is recovered by
// publishing the new CA in every entry and switching no earlier than the
// configured delay after, and that a deleted Secret is recreated at once,
// exactly as the chart shapes it.
func TestCertRotation(t *testing.T) {
	run, inputs := harness.Begin(t, phases.CertRotation)
	for _, file := range []struct{ variable, path string }{
		{"E2E_KUBECONFIG", inputs.Kubeconfig},
		{"E2E_CHART_PACKAGE", inputs.ChartPackage},
	} {
		if info, err := os.Stat(file.path); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("e2e certificate rotation: %s does not name a file: %s", file.variable, file.path)
		}
	}
	cluster, err := harness.Connect(inputs.Kubeconfig)
	if err != nil {
		t.Fatalf("e2e certificate rotation: %v", err)
	}
	phase := &certificatePhase{ctx: run.Context(), cluster: cluster, in: inputs}
	phase.resolve(t)

	if !run.Scenario("recovery-guard", phase.recoveryGuard) ||
		!run.Scenario("live-helm-lookup", phase.liveHelmLookup) ||
		!run.Scenario("corrupt-ca-recovery", phase.corruptCARecovery) ||
		!run.Scenario("missing-secret-recreation", phase.missingSecretRecreation) {
		return
	}
	run.Logf("e2e certificate rotation: PASS live Helm lookup, corrupt-CA recovery, and exact guarded recreation")
}

// certificatePhase is what the scenarios share: the release's objects as the
// phase resolved them, and what each scenario leaves for the next.
type certificatePhase struct {
	ctx     context.Context
	cluster *harness.Cluster
	in      phases.CertRotationInputs

	requireDistinctApprover bool
	managerDeployment       string
	managerServiceAccount   string
	rotatorDeployment       *appsv1.Deployment
	rotatorIdentity         rest.ImpersonationConfig
	stagingSecret           string
	mutatingConfiguration   string
	validatingConfiguration string
	service                 string
	secretName              string

	// The generated Secret before the phase changed anything, and the root
	// that was serving then.
	originalCA   []byte
	originalCert []byte
	servingCA    *x509.Certificate
	// The generated Secret the corrupt-CA recovery left.
	recoveredCA   []byte
	recoveredCert []byte
}

func (p *certificatePhase) fatalf(t *testing.T, format string, arguments ...any) {
	t.Helper()
	t.Fatalf("e2e certificate rotation: "+format, arguments...)
}

func (p *certificatePhase) key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: p.in.OperatorNamespace, Name: name}
}

// resolve finds the release's objects by what the chart labels them with and
// by what the running workloads reference, rather than by names written here.
func (p *certificatePhase) resolve(t *testing.T) {
	t.Helper()
	// The spec-writer mutating entries exist only when the release has
	// approvals.requireDistinctApprover on. This phase never changes that
	// value, but it reads the live release rather than assuming its default,
	// so every place below that enumerates the mutating inventory follows what
	// is actually installed.
	values, err := p.cluster.Helm(p.ctx, "-n", p.in.OperatorNamespace, "get", "values", p.in.HelmRelease, "--all", "-o", "json")
	if err != nil {
		p.fatalf(t, "could not read approvals.requireDistinctApprover from the live release: %v", err)
	}
	if p.requireDistinctApprover, err = releaseRequiresDistinctApprover(values); err != nil {
		p.fatalf(t, "%v", err)
	}

	manager := p.onlyDeployment(t, "controller")
	p.managerDeployment = manager.Name
	p.rotatorDeployment = p.onlyDeployment(t, "certificate-rotation")
	var found bool
	if p.stagingSecret, found = containerArgument(p.rotatorDeployment, rotatorContainer, "staging-secret-name"); !found || p.stagingSecret == "" {
		p.fatalf(t, "could not resolve the exact certificate staging Secret")
	}
	rotatorServiceAccount := p.rotatorDeployment.Spec.Template.Spec.ServiceAccountName
	rotatorPod := p.livePod(t, "certificate-rotation")
	p.mutatingConfiguration = p.onlyWebhookConfiguration(t, "mutatingwebhookconfiguration",
		&admissionregistrationv1.MutatingWebhookConfigurationList{})
	p.validatingConfiguration = p.onlyWebhookConfiguration(t, "validatingwebhookconfiguration",
		&admissionregistrationv1.ValidatingWebhookConfigurationList{})
	mutating := p.mutating(t)
	if p.service, err = webhookService(mutating); err != nil {
		p.fatalf(t, "could not resolve the exact webhook Service: %v", err)
	}
	if p.secretName, err = webhookCertificateSecret(manager); err != nil {
		p.fatalf(t, "manager webhook Secret was not found: %v", err)
	}
	p.managerServiceAccount = manager.Spec.Template.Spec.ServiceAccountName
	if p.managerServiceAccount == "" {
		p.fatalf(t, "manager ServiceAccount was not found")
	}
	if rotatorServiceAccount == "" {
		p.fatalf(t, "certificate rotator ServiceAccount was not found")
	}
	serviceAccount := &corev1.ServiceAccount{}
	if err := p.cluster.Client.Get(p.ctx, p.key(rotatorServiceAccount), serviceAccount); err != nil {
		p.fatalf(t, "certificate rotator workload-bound identity was not found: %v", err)
	}
	if serviceAccount.UID == "" || rotatorPod.Name == "" || rotatorPod.UID == "" {
		p.fatalf(t, "certificate rotator workload-bound identity was not found")
	}
	// The identity the rotator actually has: its ServiceAccount, bound to its
	// running Pod.
	p.rotatorIdentity = rest.ImpersonationConfig{
		UserName: serviceAccountUser(p.in.OperatorNamespace, rotatorServiceAccount),
		UID:      string(serviceAccount.UID),
		Groups: []string{
			"system:serviceaccounts",
			"system:serviceaccounts:" + p.in.OperatorNamespace,
			"system:authenticated",
		},
		Extra: map[string][]string{
			"authentication.kubernetes.io/pod-name": {rotatorPod.Name},
			"authentication.kubernetes.io/pod-uid":  {string(rotatorPod.UID)},
		},
	}
	if !containerHasArgument(p.rotatorDeployment, rotatorContainer, "--recreate-missing-secret=true") {
		p.fatalf(t, "certificate rotation E2E requires explicit missing-Secret recreation opt-in")
	}
}

// recoveryGuard proves the manager cannot read the webhook Secret, and that
// the rotator's own identity cannot create a Secret outside its recovery
// contract: the namespace-wide create grant the recreation opt-in adds is
// narrowed by the exact-object policy, and that policy is what refuses it.
func (p *certificatePhase) recoveryGuard(t *testing.T) {
	manager, err := p.cluster.As(rest.ImpersonationConfig{
		UserName: serviceAccountUser(p.in.OperatorNamespace, p.managerServiceAccount),
	})
	if err != nil {
		p.fatalf(t, "impersonate the manager ServiceAccount: %v", err)
	}
	review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
		ResourceAttributes: &authorizationv1.ResourceAttributes{
			Namespace: p.in.OperatorNamespace, Verb: "get", Resource: "secrets", Name: p.secretName,
		},
	}}
	if err := manager.Create(p.ctx, review); err != nil {
		p.fatalf(t, "could not ask whether the manager ServiceAccount can read the webhook Secret: %v", err)
	}
	if review.Status.Allowed {
		p.fatalf(t, "manager ServiceAccount can read the webhook Secret")
	}

	rotator, err := p.cluster.As(p.rotatorIdentity)
	if err != nil {
		p.fatalf(t, "impersonate the certificate rotator: %v", err)
	}
	unrelated := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ptah-rotator-unauthorized", Namespace: p.in.OperatorNamespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"uncontrolled": []byte("value")},
	}
	err = rotator.Create(p.ctx, unrelated, client.DryRunAll)
	if err == nil {
		p.fatalf(t, "certificate rotator ServiceAccount created an unrelated Secret")
	}
	if !strings.Contains(err.Error(), recoveryGuardRefusal) {
		p.fatalf(t, "unrelated Secret CREATE was not rejected by the exact recovery guard: %v", err)
	}
}

// liveHelmLookup exercises Helm's live lookup while the generated Secret
// exists. Every managed entry begins with the serving root plus a distinct
// valid local root; the upgrade must preserve that exact trust partition
// without cross-copying.
func (p *certificatePhase) liveHelmLookup(t *testing.T) {
	ns := p.in.OperatorNamespace
	if err := p.cluster.Client.Patch(p.ctx,
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: p.managerDeployment, Namespace: ns}},
		client.RawPatch(types.MergePatchType, []byte(`{"spec":{"replicas":2}}`)),
		client.FieldOwner(harness.FieldOwner)); err != nil {
		p.fatalf(t, "could not ask for two manager replicas: %v", err)
	}
	if err := p.cluster.WaitForRollout(p.ctx, ns, p.managerDeployment, 5*time.Minute); err != nil {
		p.fatalf(t, "manager Deployment did not roll out at two replicas: %v", err)
	}
	err := harness.Wait(p.ctx, "the webhook Service's two ready endpoint addresses", 120*time.Second, time.Second,
		func(ctx context.Context) (bool, string, error) {
			endpointSlices := &discoveryv1.EndpointSliceList{}
			if err := p.cluster.Client.List(ctx, endpointSlices, client.InNamespace(ns),
				client.MatchingLabels{discoveryv1.LabelServiceName: p.service}); err != nil {
				return false, fmt.Sprintf("list failed: %v", err), nil
			}
			ready := readyEndpointAddresses(endpointSlices.Items)
			return ready == 2, fmt.Sprintf("%d ready addresses in %d slices", ready, len(endpointSlices.Items)), nil
		})
	if err != nil {
		p.fatalf(t, "webhook Service did not converge to two ready endpoint addresses: %v", err)
	}

	secret := &corev1.Secret{}
	if err := p.cluster.Client.Get(p.ctx, p.key(p.secretName), secret); err != nil {
		p.fatalf(t, "could not capture the generated webhook Secret before the Helm lookup proof: %v", err)
	}
	if err := generatedSecretExact(secret, p.secretName, ns, p.in.HelmRelease); err != nil {
		p.fatalf(t, "generated webhook Secret lacks exact ownership, type, or required certificate material: %v", err)
	}
	p.originalCA, p.originalCert = secret.Data["ca.crt"], secret.Data["tls.crt"]
	if p.servingCA, err = selfSignedRoot(p.originalCA); err != nil {
		p.fatalf(t, "generated webhook Secret ca.crt is not a valid self-signed root: %v", err)
	}

	now := time.Now()
	fixtures := map[string]*x509.Certificate{}
	overlaps := map[string][]byte{}
	for _, name := range []string{"mutating", "approval-validating", "pod-validating", "controller-write-validating"} {
		encoded, certificate, err := fixtureAuthority(name, now)
		if err != nil {
			p.fatalf(t, "could not generate the %s Helm-upgrade CA fixture: %v", name, err)
		}
		fixtures[name] = certificate
		if overlaps[name], err = overlapBundle(p.originalCA, encoded); err != nil {
			p.fatalf(t, "the %s Helm-upgrade CA overlap is not exactly two valid certificates: %v", name, err)
		}
	}
	p.replaceMutatingBundles(t, map[string][]byte{"mapproval.operator.ptah.run": overlaps["mutating"]})
	p.replaceValidatingBundles(t, map[string][]byte{
		"vapproval.operator.ptah.run":        overlaps["approval-validating"],
		"vpodintent.operator.ptah.run":       overlaps["pod-validating"],
		"vcontrollerwrite.operator.ptah.run": overlaps["controller-write-validating"],
	})

	partition := func() {
		t.Helper()
		for _, entry := range []struct {
			mutating bool
			name     string
			own      string
		}{
			{true, "mapproval.operator.ptah.run", "mutating"},
			{false, "vapproval.operator.ptah.run", "approval-validating"},
			{false, "vpodintent.operator.ptah.run", "pod-validating"},
			{false, "vcontrollerwrite.operator.ptah.run", "controller-write-validating"},
		} {
			var foreign []*x509.Certificate
			for name, certificate := range fixtures {
				if name != entry.own {
					foreign = append(foreign, certificate)
				}
			}
			p.assertEntryPartition(t, entry.mutating, entry.name, fixtures[entry.own], foreign)
		}
	}
	partition()
	p.approvalAdmissionCallable(t, "before the Helm upgrade")

	if _, err := p.cluster.Helm(p.ctx, "upgrade", p.in.HelmRelease, p.in.ChartPackage,
		"--namespace", ns, "--reuse-values", "--wait", "--timeout", "5m"); err != nil {
		p.fatalf(t, "packaged-chart upgrade failed during live certificate lookup: %v", err)
	}

	partition()
	p.approvalAdmissionCallable(t, "after the Helm upgrade")
}

// corruptCARecovery corrupts ca.crt while leaving the serving leaf and key
// intact, and one entry's bundle with it. One malformed webhook entry proves
// that recovery filters candidates independently and can authenticate the
// live leaf from the remaining exact entries.
//
// A CA transition publishes the old and the new CA in every managed entry,
// records when, and switches the generated Secret no earlier than the
// configured delay after that. This row proves that order from the objects'
// own timestamps.
func (p *certificatePhase) corruptCARecovery(t *testing.T) {
	ns := p.in.OperatorNamespace
	switchDelay, err := caSwitchDelay(p.rotatorDeployment)
	if err != nil {
		p.fatalf(t, "%v", err)
	}
	patch, err := json.Marshal([]map[string]string{{
		"op": "replace", "path": "/data/ca.crt", "value": base64.StdEncoding.EncodeToString(brokenCA),
	}})
	if err != nil {
		p.fatalf(t, "encode the corrupting patch: %v", err)
	}
	if err := p.cluster.Client.Patch(p.ctx,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: p.secretName, Namespace: ns}},
		client.RawPatch(types.JSONPatchType, patch), client.FieldOwner(harness.FieldOwner)); err != nil {
		p.fatalf(t, "could not corrupt the generated Secret's ca.crt: %v", err)
	}
	p.replaceValidatingBundles(t, map[string][]byte{"vapproval.operator.ptah.run": brokenCA})

	// Replacing the rotator's Pod is what this proof needs: a new process that
	// reads the corrupted CA state from nothing but the cluster.
	oldRotator := p.livePod(t, "certificate-rotation")
	p.deletePod(t, oldRotator.Name)
	p.waitForRotatorRollout(t, "certificate rotator Deployment could not restart with corrupted CA state")
	newRotator := p.livePod(t, "certificate-rotation")
	if newRotator.UID == oldRotator.UID {
		p.fatalf(t, "certificate rotator Pod was not replaced")
	}
	expandedAt, stagedCA, stagedCert := p.observeExpandedTrust(t, "corrupt-CA recovery", p.servingCA, brokenCA)
	p.approvalAdmissionCallable(t, "while the CA switch waits")

	var switchedCA []byte
	err = harness.Wait(p.ctx, "the switch to a new CA with the old one retired", 660*time.Second, 2*time.Second,
		func(ctx context.Context) (bool, string, error) {
			secret := &corev1.Secret{}
			if err := p.cluster.Client.Get(ctx, p.key(p.secretName), secret); err != nil {
				return false, "", fmt.Errorf("could not read the generated Secret: %w", err)
			}
			candidate := secret.Data["ca.crt"]
			switch {
			case len(candidate) == 0 || bytes.Equal(candidate, p.originalCA):
				return false, "ca.crt still holds the CA the phase started from", nil
			case len(secret.Data["ca.key"]) == 0:
				return false, "ca.key is empty", nil
			}
			complete, observed := p.rotationTransitionComplete(ctx, candidate)
			if complete {
				switchedCA = candidate
			}
			return complete, observed, nil
		})
	if err != nil {
		p.fatalf(t, "certificate rotation did not switch to the new CA and retire the old one: %v", err)
	}
	switched := &corev1.Secret{}
	if err := p.cluster.Client.Get(p.ctx, p.key(p.secretName), switched); err != nil {
		p.fatalf(t, "could not read the switched generated Secret: %v", err)
	}
	stagedAt := expandedAt.UTC().Format(time.RFC3339)
	if !bytes.Equal(switchedCA, stagedCA) {
		p.fatalf(t, "the switch installed a CA other than the one staged at %s", stagedAt)
	}
	// The serving certificate must still be the staged one, so the rotator's
	// last write of tls.crt is the switch itself and not a later renewal that
	// would hide an early switch.
	if !bytes.Equal(switched.Data["tls.crt"], stagedCert) {
		p.fatalf(t, "the generated Secret serves a certificate other than the one staged at %s", stagedAt)
	}
	switchedAt, err := rotatorCertificateWriteTime(switched)
	if err != nil {
		p.fatalf(t, "the generated Secret does not record exactly one rotator write of its serving certificate: %v", err)
	}
	if !switchedAfterDelay(switchedAt, expandedAt, switchDelay) {
		p.fatalf(t, "the rotator switched the generated Secret at %s, less than %s after the expansion it recorded at %s",
			switchedAt.UTC().Format(time.RFC3339), switchDelay, stagedAt)
	}
	p.approvalAdmissionCallable(t, "after the corrupt-CA recovery")
	p.rotatorLogHoldsNoKey(t, newRotator.Name,
		"could not inspect certificate rotation logs", "certificate rotation logs contain private key material", true)

	recovered := &corev1.Secret{}
	if err := p.cluster.Client.Get(p.ctx, p.key(p.secretName), recovered); err != nil {
		p.fatalf(t, "could not read the recovered generated Secret: %v", err)
	}
	if len(recovered.Data["ca.key"]) == 0 {
		p.fatalf(t, "certificate rotation did not retain a valid CA private key")
	}
	p.recoveredCA, p.recoveredCert = recovered.Data["ca.crt"], recovered.Data["tls.crt"]
	if bytes.Equal(p.recoveredCA, p.originalCA) {
		p.fatalf(t, "recovery rotation did not replace the CA")
	}
	if bytes.Equal(p.recoveredCert, p.originalCert) {
		p.fatalf(t, "recovery rotation did not replace the serving certificate")
	}
	mutating, validating := p.uniformBundles(t)
	if !bytes.Equal(mutating, p.recoveredCA) {
		p.fatalf(t, "mutating webhook trust did not contract to the replacement CA")
	}
	if !bytes.Equal(validating, p.recoveredCA) {
		p.fatalf(t, "validating webhook trust did not contract to the replacement CA")
	}
}

// missingSecretRecreation deletes the generated Secret entirely. This release
// opts in to the namespace-wide RBAC create verb, whose use by the rotator the
// exact-object policy recoveryGuard measured constrains.
//
// A missing Secret is already broken: a manager Pod that restarts cannot mount
// its certificate. The rotator publishes the new CA in every managed entry and
// recreates the Secret in the same pass, without waiting out the switch delay.
func (p *certificatePhase) missingSecretRecreation(t *testing.T) {
	ns := p.in.OperatorNamespace
	switchDelay, err := caSwitchDelay(p.rotatorDeployment)
	if err != nil {
		p.fatalf(t, "%v", err)
	}
	recovered := &corev1.Secret{}
	if err := p.cluster.Client.Get(p.ctx, p.key(p.secretName), recovered); err != nil {
		p.fatalf(t, "could not read the generated Secret before deleting it: %v", err)
	}
	recoveredUID := recovered.UID
	if err := p.cluster.Client.Delete(p.ctx, recovered); err != nil {
		p.fatalf(t, "could not delete the generated Secret: %v", err)
	}
	// Gone means the object this phase deleted is gone: kubectl's --wait
	// counts a Secret of the same name with another UID as its deletion done.
	err = harness.Wait(p.ctx, "the deletion of the generated Secret", 60*time.Second, time.Second,
		func(ctx context.Context) (bool, string, error) {
			current := &corev1.Secret{}
			err := p.cluster.Client.Get(ctx, p.key(p.secretName), current)
			switch {
			case apierrors.IsNotFound(err):
				return true, "absent", nil
			case err != nil:
				return false, fmt.Sprintf("read failed: %v", err), nil
			case current.UID != recoveredUID:
				return true, "replaced by another object", nil
			}
			return false, "still present", nil
		})
	if err != nil {
		p.fatalf(t, "%v", err)
	}
	rotatorBefore := p.livePod(t, "certificate-rotation")
	p.deletePod(t, rotatorBefore.Name)

	// Watch for the Secret from the moment the rotator is replaced, so the
	// entries are read as close to its return as this harness can.
	var firstSeen *corev1.Secret
	err = harness.Wait(p.ctx, "the recreated generated Secret", 660*time.Second, time.Second,
		func(ctx context.Context) (bool, string, error) {
			reading := &corev1.Secret{}
			if err := p.cluster.Client.Get(ctx, p.key(p.secretName), reading); err != nil {
				return false, fmt.Sprintf("read failed: %v", err), nil
			}
			firstSeen = reading
			return true, "present", nil
		})
	if err != nil {
		p.fatalf(t, "certificate rotator did not recreate the deleted Secret within 660 seconds: %v", err)
	}
	firstSeenCA := firstSeen.Data["ca.crt"]
	firstSeenRoot, err := selfSignedRoot(firstSeenCA)
	if err != nil {
		p.fatalf(t, "the recreated Secret's CA is not a valid self-signed root: %v", err)
	}
	recreatedAt := firstSeen.CreationTimestamp.Time
	for _, entry := range p.managedEntries() {
		p.assertEntryTrusts(t, entry.mutating, entry.name, firstSeenRoot)
	}

	p.waitForRotatorRollout(t, "certificate rotator Deployment could not restart with a missing TLS Secret")
	rotatorAfter := p.livePod(t, "certificate-rotation")
	replacement := &corev1.Pod{}
	if err := p.cluster.Client.Get(p.ctx, p.key(rotatorAfter.Name), replacement); err != nil {
		p.fatalf(t, "could not read the replacement certificate rotator Pod: %v", err)
	}
	if replacement.UID == rotatorBefore.UID {
		p.fatalf(t, "certificate rotator Pod was not replaced for missing-Secret recovery")
	}
	startedAt, err := rotatorContainerStartedAt(replacement)
	if err != nil {
		p.fatalf(t, "the replacement certificate rotator Pod does not record when its rotator container started: %v", err)
	}
	// Both instants come from the objects: the rotator container's start and
	// the Secret's creation. A recreation that waited out the delay lands
	// after both.
	if switchedAfterDelay(recreatedAt, startedAt, switchDelay) {
		p.fatalf(t, "the rotator recreated the generated Secret at %s, %s or more after its container started at %s; "+
			"a missing Secret must not wait out the switch delay",
			recreatedAt.UTC().Format(time.RFC3339), switchDelay, startedAt.UTC().Format(time.RFC3339))
	}

	var recreatedCA []byte
	err = harness.Wait(p.ctx, "the recreated Secret under the exact recovery contract", 660*time.Second, 2*time.Second,
		func(ctx context.Context) (bool, string, error) {
			secret := &corev1.Secret{}
			if err := p.cluster.Client.Get(ctx, p.key(p.secretName), secret); err != nil {
				return false, fmt.Sprintf("read failed: %v", err), nil
			}
			candidateCA, candidateCert := secret.Data["ca.crt"], secret.Data["tls.crt"]
			switch {
			case secret.UID == "" || secret.UID == recoveredUID:
				return false, "the Secret is the one the phase deleted", nil
			case len(candidateCA) == 0 || bytes.Equal(candidateCA, p.recoveredCA):
				return false, "ca.crt is empty or the CA before the deletion", nil
			case len(candidateCert) == 0 || bytes.Equal(candidateCert, p.recoveredCert):
				return false, "tls.crt is empty or the certificate before the deletion", nil
			}
			if err := recreatedSecretExact(secret, ns, p.in.HelmRelease); err != nil {
				return false, err.Error(), nil
			}
			complete, observed := p.rotationTransitionComplete(ctx, candidateCA)
			if complete {
				recreatedCA = candidateCA
			}
			return complete, observed, nil
		})
	if err != nil {
		p.fatalf(t, "certificate rotator did not recreate the deleted Secret with the exact recovery contract: %v", err)
	}
	if !bytes.Equal(recreatedCA, firstSeenCA) {
		p.fatalf(t, "the recreated Secret's CA changed after it was first seen")
	}

	if err := p.cluster.WaitForRollout(p.ctx, ns, p.managerDeployment, 5*time.Minute); err != nil {
		p.fatalf(t, "manager Deployment did not remain ready after Secret recreation: %v", err)
	}
	mutating, validating := p.uniformBundles(t)
	if !bytes.Equal(mutating, recreatedCA) {
		p.fatalf(t, "mutating webhook trust did not contract after Secret recreation")
	}
	if !bytes.Equal(validating, recreatedCA) {
		p.fatalf(t, "validating webhook trust did not contract after Secret recreation")
	}
	p.approvalAdmissionCallable(t, "after the Secret recreation")
	p.rotatorLogHoldsNoKey(t, rotatorAfter.Name,
		"could not inspect missing-Secret recovery logs", "missing-Secret recovery logs contain private key material", false)
}

// observeExpandedTrust waits for the rotator to record its expansion, then
// proves that every managed entry trusted both the CA behind the certificate
// being served and the staged CA while the generated Secret still held its
// pre-switch state. Reading the Secret before and after the entries brackets
// that read: the old CA leaves an entry only after the Secret switches.
func (p *certificatePhase) observeExpandedTrust(
	t *testing.T, stage string, servingCA *x509.Certificate, preSwitchCA []byte,
) (expandedAt time.Time, stagedCA, stagedCert []byte) {
	t.Helper()
	// Each reading decodes into an object of its own: decoding into the last
	// one would keep a key the newer document no longer has.
	var staging *corev1.Secret
	err := harness.Wait(p.ctx, stage+": an expanded CA transition in the staging Secret", 120*time.Second, time.Second,
		func(ctx context.Context) (bool, string, error) {
			reading := &corev1.Secret{}
			if err := p.cluster.Client.Get(ctx, p.key(p.stagingSecret), reading); err != nil {
				return false, "", fmt.Errorf("could not read the staging Secret: %w", err)
			}
			instant, expanded := expandedTransitionTime(reading, p.stagingSecret, p.in.OperatorNamespace)
			if expanded {
				staging, expandedAt = reading, instant
			}
			return expanded, fmt.Sprintf("format %q, phase %q", reading.Data["format"], reading.Data["phase"]), nil
		})
	if err != nil {
		p.fatalf(t, "%v", err)
	}
	stagedCA, stagedCert = staging.Data["candidate.ca.crt"], staging.Data["candidate.tls.crt"]
	stagedRoot, err := selfSignedRoot(stagedCA)
	if err != nil {
		p.fatalf(t, "%s: the staged CA is not a valid self-signed root: %v", stage, err)
	}
	if err := issuedBy(stagedCert, stagedCA); err != nil {
		p.fatalf(t, "%s: the staged serving certificate is not issued by the staged CA: %v", stage, err)
	}

	before := p.primarySecretState(t)
	if before.absent || before.caCertificate != string(preSwitchCA) {
		p.fatalf(t, "%s: the generated Secret switched before its expansion could be read", stage)
	}
	for _, entry := range p.managedEntries() {
		p.assertEntryTrusts(t, entry.mutating, entry.name, servingCA, stagedRoot)
	}
	if after := p.primarySecretState(t); after != before {
		p.fatalf(t, "%s: the generated Secret switched while the expanded trust was read, so the read proves nothing", stage)
	}
	return expandedAt, stagedCA, stagedCert
}

// rotationTransitionComplete reports whether both configurations carry the
// expected CA, uniformly, in exactly the managed entries, and the staging
// Secret holds no transition any more.
func (p *certificatePhase) rotationTransitionComplete(ctx context.Context, expectedCA []byte) (bool, string) {
	mutating := &admissionregistrationv1.MutatingWebhookConfiguration{}
	if err := p.cluster.Client.Get(ctx, types.NamespacedName{Name: p.mutatingConfiguration}, mutating); err != nil {
		return false, fmt.Sprintf("mutating configuration read failed: %v", err)
	}
	if bundle, uniform := uniformServiceBundle(mutatingEntries(mutating),
		managedMutatingWebhooks(p.requireDistinctApprover), p.service, p.in.OperatorNamespace); !uniform || !bytes.Equal(bundle, expectedCA) {
		return false, "the mutating entries do not carry exactly the new CA"
	}
	validating := &admissionregistrationv1.ValidatingWebhookConfiguration{}
	if err := p.cluster.Client.Get(ctx, types.NamespacedName{Name: p.validatingConfiguration}, validating); err != nil {
		return false, fmt.Sprintf("validating configuration read failed: %v", err)
	}
	if bundle, uniform := uniformServiceBundle(validatingEntries(validating),
		managedValidatingWebhooks(), p.service, p.in.OperatorNamespace); !uniform || !bytes.Equal(bundle, expectedCA) {
		return false, "the validating entries do not carry exactly the new CA"
	}
	staging := &corev1.Secret{}
	if err := p.cluster.Client.Get(ctx, p.key(p.stagingSecret), staging); err != nil {
		return false, fmt.Sprintf("staging Secret read failed: %v", err)
	}
	if !stagingSecretRetired(staging, p.stagingSecret, p.in.OperatorNamespace, p.in.HelmRelease) {
		return false, "the staging Secret still holds a transition"
	}
	return true, "the transition is complete"
}

// uniformBundles reads the one bundle each configuration's managed entries
// carry, or nothing where they are not uniform.
func (p *certificatePhase) uniformBundles(t *testing.T) (mutating, validating []byte) {
	t.Helper()
	mutating, _ = uniformServiceBundle(mutatingEntries(p.mutating(t)),
		managedMutatingWebhooks(p.requireDistinctApprover), p.service, p.in.OperatorNamespace)
	validating, _ = uniformServiceBundle(validatingEntries(p.validating(t)),
		managedValidatingWebhooks(), p.service, p.in.OperatorNamespace)
	return mutating, validating
}

// managedEntry names one managed webhook and which configuration holds it.
type managedEntry struct {
	mutating bool
	name     string
}

// managedEntries is every entry the rotator manages on this release, mutating
// first.
func (p *certificatePhase) managedEntries() []managedEntry {
	var entries []managedEntry
	for _, webhook := range managedMutatingWebhooks(p.requireDistinctApprover) {
		entries = append(entries, managedEntry{mutating: true, name: webhook.name})
	}
	for _, webhook := range managedValidatingWebhooks() {
		entries = append(entries, managedEntry{name: webhook.name})
	}
	return entries
}

func (p *certificatePhase) entryBundle(t *testing.T, mutating bool, name string) []byte {
	t.Helper()
	var entries []webhookEntry
	if mutating {
		entries = mutatingEntries(p.mutating(t))
	} else {
		entries = validatingEntries(p.validating(t))
	}
	bundle, found := entryBundle(entries, name)
	if !found {
		p.fatalf(t, "could not read caBundle for %s", name)
	}
	return bundle
}

// assertEntryTrusts fails unless the entry's caBundle holds every given
// certificate.
func (p *certificatePhase) assertEntryTrusts(t *testing.T, mutating bool, name string, trusted ...*x509.Certificate) {
	t.Helper()
	bundle := p.entryBundle(t, mutating, name)
	for _, certificate := range trusted {
		if !bundleContains(bundle, certificate) {
			p.fatalf(t, "caBundle for %s does not trust %s (serial %s)", name, certificate.Subject.CommonName, certificate.SerialNumber)
		}
	}
}

// assertEntryPartition fails unless the entry's bundle is exactly two valid
// certificates that trust the serving root and the entry's own root, and no
// other entry's.
func (p *certificatePhase) assertEntryPartition(
	t *testing.T, mutating bool, name string, own *x509.Certificate, foreign []*x509.Certificate,
) {
	t.Helper()
	bundle := p.entryBundle(t, mutating, name)
	if err := exactlyTwoCertificates(bundle); err != nil {
		p.fatalf(t, "caBundle for %s is not exactly two valid certificates: %v", name, err)
	}
	if !trusts(bundle, p.servingCA) {
		p.fatalf(t, "caBundle for %s dropped the current serving root", name)
	}
	if !trusts(bundle, own) {
		p.fatalf(t, "caBundle for %s dropped its entry-local root", name)
	}
	for _, certificate := range foreign {
		if trusts(bundle, certificate) {
			p.fatalf(t, "caBundle for %s gained another entry's root", name)
		}
	}
}

// approvalAdmissionCallable sends approval admission a server-side dry run,
// which reaches the approval webhooks through the bundles under test.
func (p *certificatePhase) approvalAdmissionCallable(t *testing.T, stage string) {
	t.Helper()
	approval := &ptahv1alpha1.PtahSchemaApproval{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-approval", Namespace: p.in.TestNamespace},
	}
	patch := client.RawPatch(types.MergePatchType,
		[]byte(`{"metadata":{"annotations":{"operator.ptah.run/certificate-upgrade-probe":"true"}}}`))
	if err := p.cluster.Client.Patch(p.ctx, approval, patch, client.DryRunAll, client.FieldOwner(harness.FieldOwner)); err != nil {
		p.fatalf(t, "approval admission was not callable %s: %v", stage, err)
	}
}

// primarySecretState reads the generated Secret's ca.crt and resourceVersion,
// or its absence.
func (p *certificatePhase) primarySecretState(t *testing.T) secretState {
	t.Helper()
	secret := &corev1.Secret{}
	err := p.cluster.Client.Get(p.ctx, p.key(p.secretName), secret)
	switch {
	case apierrors.IsNotFound(err):
		return secretState{absent: true}
	case err != nil:
		p.fatalf(t, "could not read the generated Secret: %v", err)
	}
	return presentSecretState(secret)
}

func (p *certificatePhase) replaceMutatingBundles(t *testing.T, bundles map[string][]byte) {
	t.Helper()
	configuration := p.mutating(t)
	for index := range configuration.Webhooks {
		if bundle, replace := bundles[configuration.Webhooks[index].Name]; replace {
			configuration.Webhooks[index].ClientConfig.CABundle = bundle
		}
	}
	if err := p.cluster.Client.Update(p.ctx, configuration, client.FieldOwner(harness.FieldOwner)); err != nil {
		p.fatalf(t, "could not replace the mutating webhook bundles: %v", err)
	}
}

func (p *certificatePhase) replaceValidatingBundles(t *testing.T, bundles map[string][]byte) {
	t.Helper()
	configuration := p.validating(t)
	for index := range configuration.Webhooks {
		if bundle, replace := bundles[configuration.Webhooks[index].Name]; replace {
			configuration.Webhooks[index].ClientConfig.CABundle = bundle
		}
	}
	if err := p.cluster.Client.Update(p.ctx, configuration, client.FieldOwner(harness.FieldOwner)); err != nil {
		p.fatalf(t, "could not replace the validating webhook bundles: %v", err)
	}
}

func (p *certificatePhase) mutating(t *testing.T) *admissionregistrationv1.MutatingWebhookConfiguration {
	t.Helper()
	configuration := &admissionregistrationv1.MutatingWebhookConfiguration{}
	if err := p.cluster.Client.Get(p.ctx, types.NamespacedName{Name: p.mutatingConfiguration}, configuration); err != nil {
		p.fatalf(t, "could not read the mutating webhook configuration %s: %v", p.mutatingConfiguration, err)
	}
	return configuration
}

func (p *certificatePhase) validating(t *testing.T) *admissionregistrationv1.ValidatingWebhookConfiguration {
	t.Helper()
	configuration := &admissionregistrationv1.ValidatingWebhookConfiguration{}
	if err := p.cluster.Client.Get(p.ctx, types.NamespacedName{Name: p.validatingConfiguration}, configuration); err != nil {
		p.fatalf(t, "could not read the validating webhook configuration %s: %v", p.validatingConfiguration, err)
	}
	return configuration
}

// releaseSelector is the release's own label, with the component when one is
// named.
func (p *certificatePhase) releaseSelector(component string) client.MatchingLabels {
	selector := client.MatchingLabels{"app.kubernetes.io/instance": p.in.HelmRelease}
	if component != "" {
		selector["app.kubernetes.io/component"] = component
	}
	return selector
}

func (p *certificatePhase) onlyDeployment(t *testing.T, component string) *appsv1.Deployment {
	t.Helper()
	deployments := &appsv1.DeploymentList{}
	if err := p.cluster.Client.List(p.ctx, deployments, client.InNamespace(p.in.OperatorNamespace),
		p.releaseSelector(component)); err != nil {
		p.fatalf(t, "could not list the release's Deployments: %v", err)
	}
	if len(deployments.Items) != 1 {
		p.fatalf(t, "expected exactly one deployment for component %s, found %d", component, len(deployments.Items))
	}
	return &deployments.Items[0]
}

// onlyWebhookConfiguration returns the name of the one configuration of the
// list's kind that carries the release's label.
func (p *certificatePhase) onlyWebhookConfiguration(t *testing.T, kind string, list client.ObjectList) string {
	t.Helper()
	if err := p.cluster.Client.List(p.ctx, list, p.releaseSelector("")); err != nil {
		p.fatalf(t, "could not list the release's %s objects: %v", kind, err)
	}
	var names []string
	switch configurations := list.(type) {
	case *admissionregistrationv1.MutatingWebhookConfigurationList:
		for _, item := range configurations.Items {
			names = append(names, item.Name)
		}
	case *admissionregistrationv1.ValidatingWebhookConfigurationList:
		for _, item := range configurations.Items {
			names = append(names, item.Name)
		}
	}
	if len(names) != 1 {
		p.fatalf(t, "expected exactly one %s for the release, found %d", kind, len(names))
	}
	return names[0]
}

// livePod waits for exactly one live, ready Pod of the release's component.
func (p *certificatePhase) livePod(t *testing.T, component string) corev1.Pod {
	t.Helper()
	var live corev1.Pod
	err := harness.Wait(p.ctx, "exactly one live ready pod for component "+component, 180*time.Second, 2*time.Second,
		func(ctx context.Context) (bool, string, error) {
			pods := &corev1.PodList{}
			if err := p.cluster.Client.List(ctx, pods, client.InNamespace(p.in.OperatorNamespace),
				p.releaseSelector(component)); err != nil {
				return false, fmt.Sprintf("list failed: %v", err), nil
			}
			var found bool
			live, found = livePod(pods.Items)
			return found, fmt.Sprintf("%d Pods carry the component", len(pods.Items)), nil
		})
	if err != nil {
		p.fatalf(t, "%v", err)
	}
	return live
}

func (p *certificatePhase) deletePod(t *testing.T, name string) {
	t.Helper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: p.in.OperatorNamespace}}
	if err := p.cluster.Client.Delete(p.ctx, pod); err != nil {
		p.fatalf(t, "could not delete Pod %s: %v", name, err)
	}
}

// waitForRotatorRollout waits for the rotator Deployment, and on failure
// describes it ahead of the reason.
func (p *certificatePhase) waitForRotatorRollout(t *testing.T, failure string) {
	t.Helper()
	name := p.rotatorDeployment.Name
	if err := p.cluster.WaitForRollout(p.ctx, p.in.OperatorNamespace, name, 5*time.Minute); err != nil {
		p.describeRotator()
		p.fatalf(t, "%s: %v", failure, err)
	}
}

// rotatorLogHoldsNoKey reads the rotator container's log and fails if any of
// it reads as private key material.
func (p *certificatePhase) rotatorLogHoldsNoKey(t *testing.T, pod, unreadable, leaked string, describe bool) {
	t.Helper()
	log, err := p.cluster.ContainerLog(p.ctx, p.in.OperatorNamespace, pod, rotatorContainer)
	if err != nil {
		if describe {
			p.describeRotator()
		}
		p.fatalf(t, "%s: %v", unreadable, err)
	}
	if mentionsPrivateKey(log) {
		p.fatalf(t, "%s", leaked)
	}
}

// describeRotator writes `kubectl describe` of the rotator Deployment. It runs
// on a context of its own, because the phase's may be what ran out.
func (p *certificatePhase) describeRotator() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p.cluster.Describe(ctx, os.Stderr, p.in.OperatorNamespace, "deployment", p.rotatorDeployment.Name)
}

func serviceAccountUser(namespace, name string) string {
	return "system:serviceaccount:" + namespace + ":" + name
}
