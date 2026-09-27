package admissionpolicy_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"regexp"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/test/envtest/internal/policyenv"
)

// certificateRows holds the policies that keep the identities an install
// depends on from being rewritten: the certificate rotator may change the
// trust a webhook entry carries and nothing else about it, only the rotator
// writes the staging Secret's key material, and only someone who administers
// admission may create or edit the ServiceAccounts the release runs as.
//
// The certificate guards match the webhook configurations only when the
// rotator writes them: an administrator reapplying the release rewrites the
// entries as Helm renders them, and the widened-match mutations hold the
// guards to leaving that alone.
func certificateRows(t *testing.T, c *catalog) {
	mutateGuard := policy(t, "ptah-operator-certificate-mutate-guard-")
	validateGuard := policy(t, "ptah-operator-certificate-validate-guard-")
	stageGuard := policy(t, "ptah-operator-cert-stage-guard-")
	accountGuard := policy(t, "ptah-operator-service-account-object-guard-")
	names := env.Chart.Names
	rotator := env.Certificate()
	user := policyenv.User()

	bundle, err := certificateAuthority()
	if err != nil {
		t.Fatal(err)
	}

	// Webhook configurations: the rotator swaps each entry's CA bundle, which is
	// its whole job, and may change nothing else about an entry.
	const (
		mutatingBundleRow   = "certificate rotator replaces the mutating webhooks' CA bundle"
		mutatingPolicyRow   = "certificate rotator makes a mutating webhook fail open"
		validatingBundleRow = "certificate rotator replaces the validating webhooks' CA bundle"
		validatingPolicyRow = "certificate rotator makes a validating webhook fail open"
		adminMutatingRow    = "administrator rewrites a mutating webhook entry's failure policy"
		adminValidatingRow  = "administrator rewrites a validating webhook entry's failure policy"
	)
	mutating := &admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: names.WebhookConfiguration}}
	validating := &admissionregistrationv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: names.WebhookConfiguration}}
	ignore := admissionregistrationv1.Ignore
	c.row(policyenv.Row{Name: mutatingBundleRow, Do: as(rotator, certificateUpdate(mutating,
		func(configuration *admissionregistrationv1.MutatingWebhookConfiguration) {
			for index := range configuration.Webhooks {
				configuration.Webhooks[index].ClientConfig.CABundle = bundle
			}
		}))})
	c.row(policyenv.Row{
		Name: mutatingPolicyRow, Deny: []string{mutateGuard}, Message: "certificate mutating write guard rejected an unsafe mutation",
		Do: as(rotator, certificateUpdate(mutating, func(configuration *admissionregistrationv1.MutatingWebhookConfiguration) {
			configuration.Webhooks[0].FailurePolicy = &ignore
		})),
	})
	c.row(policyenv.Row{Name: validatingBundleRow, Do: as(rotator, certificateUpdate(validating,
		func(configuration *admissionregistrationv1.ValidatingWebhookConfiguration) {
			for index := range configuration.Webhooks {
				configuration.Webhooks[index].ClientConfig.CABundle = bundle
			}
		}))})
	c.row(policyenv.Row{
		Name: validatingPolicyRow, Deny: []string{validateGuard}, Message: "certificate validating write guard rejected an unsafe mutation",
		Do: as(rotator, certificateUpdate(validating, func(configuration *admissionregistrationv1.ValidatingWebhookConfiguration) {
			configuration.Webhooks[0].FailurePolicy = &ignore
		})),
	})
	// Helm applies the webhook configurations as an administrator, whole, and
	// the certificate guards have no say in that.
	c.row(policyenv.Row{Name: adminMutatingRow, Do: func(ctx context.Context, env *policyenv.Env) error {
		return certificateUpdate(mutating, func(configuration *admissionregistrationv1.MutatingWebhookConfiguration) {
			configuration.Webhooks[0].FailurePolicy = &ignore
		})(ctx, env.Admin)
	}})
	c.row(policyenv.Row{Name: adminValidatingRow, Do: func(ctx context.Context, env *policyenv.Env) error {
		return certificateUpdate(validating, func(configuration *admissionregistrationv1.ValidatingWebhookConfiguration) {
			configuration.Webhooks[0].FailurePolicy = &ignore
		})(ctx, env.Admin)
	}})

	// The staging Secret holds the next serving key between the rotator's two
	// writes. Only the rotator fills it, and only the uninstall cleanup empties
	// or deletes it.
	const (
		rotatorStagesRow = "certificate rotator stages the next serving key"
		userStagesRow    = "ordinary user writes key material into the staging Secret"
		userDeletesRow   = "ordinary user deletes the staging Secret"
	)
	staging := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: names.Namespace, Name: certificateStagingSecret(t)}}
	stage := func(secret *corev1.Secret) {
		secret.Data = map[string][]byte{"tls.crt": bundle, "tls.key": []byte("staged key")}
	}
	c.row(policyenv.Row{Name: rotatorStagesRow, Do: as(rotator, certificateUpdate(staging, stage))})
	c.row(policyenv.Row{
		Name: userStagesRow, Deny: []string{stageGuard}, Message: "certificate staging Secret guard rejected an unsafe lifecycle request",
		Do: as(user, certificateUpdate(staging, stage)),
	})
	c.row(policyenv.Row{
		Name: userDeletesRow, Deny: []string{stageGuard}, Message: "certificate staging Secret guard rejected an unsafe lifecycle request",
		Do: as(user, func(ctx context.Context, api client.Client) error {
			return api.Delete(ctx, staging.DeepCopy(), client.DryRunAll)
		}),
	})

	// ServiceAccounts: the release's identities are created and edited by
	// whoever administers admission -- Helm, as an administrator -- and by no
	// one who merely may write ServiceAccounts.
	const (
		adminManagerRow   = "administrator reapplies the manager's ServiceAccount as Helm owns it"
		adminNextHookRow  = "administrator creates the next release's hook ServiceAccount"
		userManagerRow    = "ordinary user edits the manager's ServiceAccount"
		userNextHookRow   = "ordinary user creates a ServiceAccount under a release hook's name"
		userOwnAccountRow = "ordinary user creates a ServiceAccount of their own"
	)
	manager := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: names.Namespace, Name: names.Manager}}
	helmOwned := func(account *corev1.ServiceAccount) {
		// Helm records these on every release object it applies; the chart
		// does not render them, and the guard requires them.
		if account.Annotations == nil {
			account.Annotations = map[string]string{}
		}
		account.Annotations["meta.helm.sh/release-name"] = env.Chart.Release.Name
		account.Annotations["meta.helm.sh/release-namespace"] = env.Chart.Release.Namespace
	}
	nextHook := func() (client.Object, error) { return certificateNextHookAccount(names) }
	c.row(policyenv.Row{Name: adminManagerRow, Do: func(ctx context.Context, env *policyenv.Env) error {
		return certificateUpdate(manager, helmOwned)(ctx, env.Admin)
	}})
	c.row(policyenv.Row{Name: adminNextHookRow, Do: func(ctx context.Context, env *policyenv.Env) error {
		return dryRunCreate(nextHook)(ctx, env.Admin)
	}})
	c.row(policyenv.Row{
		Name: userManagerRow, Deny: []string{accountGuard}, Message: "service account object guard rejected an unsafe identity lifecycle request",
		Do: as(user, certificateUpdate(manager, func(account *corev1.ServiceAccount) {
			helmOwned(account)
			account.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "someone-elses-registry"}}
		})),
	})
	c.row(policyenv.Row{
		Name: userNextHookRow, Deny: []string{accountGuard}, Message: "service account object guard rejected an unsafe identity lifecycle request",
		Do: as(user, dryRunCreate(nextHook)),
	})
	c.row(policyenv.Row{Name: userOwnAccountRow, Do: as(user, dryRunCreate(func() (client.Object, error) {
		return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: tenantNamespace, Name: "reporting"}}, nil
	}))})

	c.mutation(policyenv.Mutation{
		Name: "certificate mutating guard binding dropped", Policies: []string{mutateGuard},
		Apply: policyenv.DropBinding(mutateGuard), Breaks: []string{mutatingPolicyRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "certificate mutating guard refuses what it matches", Policies: []string{mutateGuard},
		Apply: policyenv.RefuseEverything(mutateGuard), Breaks: []string{mutatingBundleRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "certificate mutating guard matches every identity", Policies: []string{mutateGuard},
		Apply: policyenv.WidenMatch(mutateGuard), Breaks: []string{adminMutatingRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "certificate validating guard binding dropped", Policies: []string{validateGuard},
		Apply: policyenv.DropBinding(validateGuard), Breaks: []string{validatingPolicyRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "certificate validating guard refuses what it matches", Policies: []string{validateGuard},
		Apply: policyenv.RefuseEverything(validateGuard), Breaks: []string{validatingBundleRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "certificate validating guard matches every identity", Policies: []string{validateGuard},
		Apply: policyenv.WidenMatch(validateGuard), Breaks: []string{adminValidatingRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "staging Secret guard binding dropped", Policies: []string{stageGuard},
		Apply:  policyenv.DropBinding(stageGuard),
		Breaks: []string{userStagesRow, userDeletesRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "staging Secret guard parameter reference fails open", Policies: []string{stageGuard},
		Apply: policyenv.RedirectParameters(stageGuard), Breaks: []string{userStagesRow, userDeletesRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "staging Secret guard refuses what it matches", Policies: []string{stageGuard},
		Apply: policyenv.RefuseEverything(stageGuard), Breaks: []string{rotatorStagesRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "ServiceAccount object guard binding dropped", Policies: []string{accountGuard},
		Apply:  policyenv.DropBinding(accountGuard),
		Breaks: []string{userManagerRow, userNextHookRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "ServiceAccount object guard refuses what it matches", Policies: []string{accountGuard},
		Apply: policyenv.RefuseEverything(accountGuard), Breaks: []string{adminManagerRow, adminNextHookRow},
	})
	c.mutation(policyenv.Mutation{
		Name: "ServiceAccount object guard matches every ServiceAccount", Policies: []string{accountGuard},
		Apply: policyenv.WidenMatch(accountGuard), Breaks: []string{userOwnAccountRow},
	})
}

// certificateUpdate updates the stored form of object as edit changes it, as a
// dry run. An update rather than a patch, because the rotator and Helm send the
// whole object.
func certificateUpdate[T client.Object](object T, edit func(T)) func(context.Context, client.Client) error {
	return func(ctx context.Context, api client.Client) error {
		current, err := stored(ctx, object)
		if err != nil {
			return err
		}
		edit(current)
		return api.Update(ctx, current, client.DryRunAll)
	}
}

// certificateAuthority is a CA certificate of the kind the rotator publishes,
// PEM-encoded, so nothing that parses a bundle sees bytes no rotator would
// write.
func certificateAuthority() ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "ptah-ptah-operator-ca"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// certificateStagingSecret is the Secret the chart renders for the rotator to
// stage into, found by the label the chart puts on it.
func certificateStagingSecret(t *testing.T) string {
	t.Helper()
	for _, object := range env.Chart.Installed {
		if object.GetKind() == "Secret" && object.GetLabels()["operator.ptah.run/certificate-rotation-staging"] == "true" {
			return object.GetName()
		}
	}
	t.Fatal("the chart renders no certificate staging Secret")
	return ""
}

var certificateHookSequence = regexp.MustCompile(`-crd-v[1-9][0-9]*-[0-9a-f]{12}$`)

// certificateNextHookAccount is the hook ServiceAccount the next release would
// create, as the chart renders the current one: the same labels and hook
// annotations, under the next sequence's name. It cannot exist yet, so a dry
// run can create it every time.
func certificateNextHookAccount(names policyenv.Names) (*corev1.ServiceAccount, error) {
	rendered, err := env.Chart.Object("ServiceAccount", names.Hook)
	if err != nil {
		return nil, err
	}
	account := &corev1.ServiceAccount{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(rendered.Object, account); err != nil {
		return nil, err
	}
	account.Name = certificateHookSequence.ReplaceAllString(names.Hook, "-crd-v2-0123456789ab")
	account.Namespace = names.Namespace
	return account, nil
}
