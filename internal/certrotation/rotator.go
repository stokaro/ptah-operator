package certrotation

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const (
	CACertificateKey = "ca.crt"
	CAPrivateKeyKey  = "ca.key"

	defaultAcquireTimeout = 30 * time.Second
	minimumLeaseDuration  = 30 * time.Second
	maximumValidity       = 20 * 365 * 24 * time.Hour
	maximumOperationTime  = 24 * time.Hour
)

// Config names the exact objects in one chart-managed certificate lifecycle.
// It contains no credentials or certificate material.
type Config struct {
	Namespace                      string
	SecretName                     string
	LeaseName                      string
	MutatingWebhookConfiguration   string
	MutatingWebhookNames           []string
	ValidatingWebhookConfiguration string
	ValidatingWebhookNames         []string
	ServiceName                    string
	ServiceNamespace               string
	EndpointPortName               string
	HolderIdentity                 string

	RenewalThreshold           time.Duration
	ServingCertificateValidity time.Duration
	CACertificateValidity      time.Duration
	ProbeTimeout               time.Duration
	ProbeInterval              time.Duration
	LeaseDuration              time.Duration
	AcquireTimeout             time.Duration
}

// Rotator serializes and performs one fail-closed reconciliation of the
// chart-managed webhook certificate and trust bundles.
type Rotator struct {
	client kubernetes.Interface
	config Config
	now    func() time.Time
	random io.Reader
	probe  certificateProber
}

// New returns a Rotator after validating all names and lifecycle durations.
func New(client kubernetes.Interface, config Config) (*Rotator, error) {
	if client == nil {
		return nil, errors.New("Kubernetes client is required")
	}
	if config.AcquireTimeout == 0 {
		config.AcquireTimeout = defaultAcquireTimeout
	}
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	return &Rotator{
		client: client,
		config: config,
		now:    time.Now,
		random: rand.Reader,
		probe:  tlsCertificateProber{},
	}, nil
}

// Run acquires the task-scoped Lease and reconciles the Secret and both
// webhook configurations. The Lease is released on every return path.
func (r *Rotator) Run(ctx context.Context) (runErr error) {
	guard, err := acquireLease(ctx, r.client, leaseConfig{
		Namespace:      r.config.Namespace,
		Name:           r.config.LeaseName,
		HolderIdentity: r.config.HolderIdentity,
		Duration:       r.config.LeaseDuration,
		AcquireTimeout: r.config.AcquireTimeout,
		Now:            r.now,
	})
	if err != nil {
		return fmt.Errorf("acquire certificate rotation lease: %w", err)
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := guard.Close(releaseCtx); err != nil && runErr == nil {
			runErr = fmt.Errorf("release certificate rotation lease: %w", err)
		}
	}()

	if err := r.reconcile(guard.Context()); err != nil {
		if cause := context.Cause(guard.Context()); cause != nil && ctx.Err() == nil && !errors.Is(cause, context.Canceled) {
			return fmt.Errorf("certificate rotation lease lost: %w", cause)
		}
		return err
	}
	return nil
}

func (r *Rotator) reconcile(ctx context.Context) error {
	secret, err := r.client.CoreV1().Secrets(r.config.Namespace).Get(ctx, r.config.SecretName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get generated TLS Secret %q: %w", r.config.SecretName, err)
	}
	state, err := inspectSecret(secret, r.config, r.now())
	if err != nil {
		return fmt.Errorf("inspect generated TLS Secret %q: %w", r.config.SecretName, err)
	}

	mutatingBundles, err := r.readMutatingBundles(ctx)
	if err != nil {
		return err
	}
	validatingBundles, err := r.readValidatingBundles(ctx)
	if err != nil {
		return err
	}

	switch {
	case state.rotateCA:
		return r.rotateCA(ctx, secret, state)
	case state.rotateServing:
		return r.rotateServingCertificate(ctx, secret, state, mutatingBundles, validatingBundles)
	default:
		return r.repairTrustBundles(ctx, state.current, mutatingBundles, validatingBundles)
	}
}

func (r *Rotator) rotateCA(ctx context.Context, secret *corev1.Secret, state secretState) error {
	next, err := generateMaterial(r.random, r.now(), r.config)
	if err != nil {
		return fmt.Errorf("generate replacement CA and serving certificate: %w", err)
	}
	transitionCAs := [][]byte{next.caPEM}
	if state.currentServingChainAuthentic {
		transitionCAs = append([][]byte{state.current.caPEM}, transitionCAs...)
	}
	overlap, err := combineCABundles(transitionCAs...)
	if err != nil {
		return fmt.Errorf("build CA trust transition: %w", err)
	}
	if err := r.setBothBundles(ctx, overlap); err != nil {
		return fmt.Errorf("publish overlapping CA trust: %w", err)
	}
	if err := r.updateSecret(ctx, secret, next); err != nil {
		return err
	}
	if err := r.probeCurrentCertificate(ctx, next); err != nil {
		return err
	}
	if err := r.setBothBundles(ctx, next.caPEM); err != nil {
		return fmt.Errorf("contract CA trust after serving-certificate proof: %w", err)
	}
	return nil
}

func (r *Rotator) rotateServingCertificate(
	ctx context.Context,
	secret *corev1.Secret,
	state secretState,
	mutatingBundles observedCABundles,
	validatingBundles observedCABundles,
) error {
	// Repair trust first. This also makes recovery safe when one configuration
	// was modified independently before this run.
	overlap, err := combineAnchoredCABundles(mutatingBundles, validatingBundles, state.current.caPEM)
	if err != nil {
		return fmt.Errorf("build serving-certificate trust precondition: %w", err)
	}
	if err := r.setBothBundles(ctx, overlap); err != nil {
		return fmt.Errorf("publish serving-certificate trust precondition: %w", err)
	}

	next, err := generateServingMaterial(r.random, r.now(), r.config, state.current)
	if err != nil {
		return fmt.Errorf("generate replacement serving certificate: %w", err)
	}
	if err := r.updateSecret(ctx, secret, next); err != nil {
		return err
	}
	if err := r.probeCurrentCertificate(ctx, next); err != nil {
		return err
	}
	if err := r.setBothBundles(ctx, next.caPEM); err != nil {
		return fmt.Errorf("contract repaired CA trust after serving-certificate proof: %w", err)
	}
	return nil
}

func (r *Rotator) repairTrustBundles(
	ctx context.Context,
	current certificateMaterial,
	mutatingBundles observedCABundles,
	validatingBundles observedCABundles,
) error {
	if mutatingBundles.allEqual(current.caPEM) && validatingBundles.allEqual(current.caPEM) {
		return nil
	}
	overlap, err := combineAnchoredCABundles(mutatingBundles, validatingBundles, current.caPEM)
	if err != nil {
		return fmt.Errorf("build interrupted-transition trust bundle: %w", err)
	}
	if err := r.setBothBundles(ctx, overlap); err != nil {
		return fmt.Errorf("repair interrupted CA trust transition: %w", err)
	}
	if err := r.probeCurrentCertificate(ctx, current); err != nil {
		return err
	}
	if err := r.setBothBundles(ctx, current.caPEM); err != nil {
		return fmt.Errorf("contract interrupted CA trust transition: %w", err)
	}
	return nil
}

func (r *Rotator) updateSecret(ctx context.Context, previous *corev1.Secret, next certificateMaterial) error {
	updated := previous.DeepCopy()
	updated.Type = corev1.SecretTypeTLS
	updated.Data = cloneBytesMap(previous.Data)
	updated.Data[corev1.TLSCertKey] = append([]byte(nil), next.certPEM...)
	updated.Data[corev1.TLSPrivateKeyKey] = append([]byte(nil), next.keyPEM...)
	updated.Data[CACertificateKey] = append([]byte(nil), next.caPEM...)
	updated.Data[CAPrivateKeyKey] = append([]byte(nil), next.caKeyPEM...)

	if _, err := r.client.CoreV1().Secrets(r.config.Namespace).Update(ctx, updated, metav1.UpdateOptions{}); err == nil {
		return nil
	} else {
		// An API timeout may hide a successful write. Read back the exact four
		// fields before classifying the transition as interrupted.
		observed, getErr := r.client.CoreV1().Secrets(r.config.Namespace).Get(ctx, r.config.SecretName, metav1.GetOptions{})
		if getErr == nil && secretContainsMaterial(observed, next) {
			return nil
		}
		if getErr != nil {
			return fmt.Errorf("atomically update generated TLS Secret: %w (read-back failed: %v)", err, getErr)
		}
		return fmt.Errorf("atomically update generated TLS Secret: %w", err)
	}
}

func (r *Rotator) probeCurrentCertificate(ctx context.Context, material certificateMaterial) error {
	probeCtx, cancel := context.WithTimeout(ctx, r.config.ProbeTimeout)
	defer cancel()
	ticker := time.NewTicker(r.config.ProbeInterval)
	defer ticker.Stop()
	var lastErr error
	for {
		before, err := r.endpointSnapshot(probeCtx)
		if err == nil {
			for _, endpoint := range before {
				err = r.probe.Probe(probeCtx, probeRequest{
					Address:          net.JoinHostPort(endpoint.address, fmt.Sprintf("%d", endpoint.port)),
					ServerName:       fmt.Sprintf("%s.%s.svc", r.config.ServiceName, r.config.ServiceNamespace),
					CACertificatePEM: material.caPEM,
					LeafCertificate:  material.leaf,
				})
				if err != nil {
					break
				}
			}
		}
		if err == nil {
			after, snapshotErr := r.endpointSnapshot(probeCtx)
			if snapshotErr != nil {
				err = snapshotErr
			} else if !slices.Equal(before, after) {
				err = errors.New("webhook endpoint set changed while certificates were probed")
			} else {
				return nil
			}
		}
		lastErr = err
		select {
		case <-probeCtx.Done():
			return fmt.Errorf("prove every stable webhook endpoint serves the replacement certificate: %w (last attempt: %v)", probeCtx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

func (r *Rotator) setBothBundles(ctx context.Context, bundle []byte) error {
	if err := r.setMutatingBundle(ctx, bundle); err != nil {
		return err
	}
	if err := r.setValidatingBundle(ctx, bundle); err != nil {
		return err
	}
	mutating, err := r.readMutatingBundles(ctx)
	if err != nil {
		return fmt.Errorf("verify managed mutating CA bundles: %w", err)
	}
	validating, err := r.readValidatingBundles(ctx)
	if err != nil {
		return fmt.Errorf("verify managed validating CA bundles: %w", err)
	}
	if !mutating.allEqual(bundle) || !validating.allEqual(bundle) {
		return errors.New("not every explicitly managed webhook contains the published CA bundle")
	}
	return nil
}

func (r *Rotator) readMutatingBundles(ctx context.Context) (observedCABundles, error) {
	configuration, err := r.client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(
		ctx, r.config.MutatingWebhookConfiguration, metav1.GetOptions{},
	)
	if err != nil {
		return observedCABundles{}, fmt.Errorf("get MutatingWebhookConfiguration %q: %w", r.config.MutatingWebhookConfiguration, err)
	}
	webhooks, err := managedMutatingWebhooks(configuration.Webhooks, r.config)
	if err != nil {
		return observedCABundles{}, fmt.Errorf("inspect MutatingWebhookConfiguration %q: %w", r.config.MutatingWebhookConfiguration, err)
	}
	bundles := make([][]byte, 0, len(webhooks))
	for _, webhook := range webhooks {
		bundles = append(bundles, webhook.ClientConfig.CABundle)
	}
	return observeCABundles(bundles...), nil
}

func (r *Rotator) readValidatingBundles(ctx context.Context) (observedCABundles, error) {
	configuration, err := r.client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(
		ctx, r.config.ValidatingWebhookConfiguration, metav1.GetOptions{},
	)
	if err != nil {
		return observedCABundles{}, fmt.Errorf("get ValidatingWebhookConfiguration %q: %w", r.config.ValidatingWebhookConfiguration, err)
	}
	webhooks, err := managedValidatingWebhooks(configuration.Webhooks, r.config)
	if err != nil {
		return observedCABundles{}, fmt.Errorf("inspect ValidatingWebhookConfiguration %q: %w", r.config.ValidatingWebhookConfiguration, err)
	}
	bundles := make([][]byte, 0, len(webhooks))
	for _, webhook := range webhooks {
		bundles = append(bundles, webhook.ClientConfig.CABundle)
	}
	return observeCABundles(bundles...), nil
}

func (r *Rotator) setMutatingBundle(ctx context.Context, bundle []byte) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		client := r.client.AdmissionregistrationV1().MutatingWebhookConfigurations()
		configuration, err := client.Get(ctx, r.config.MutatingWebhookConfiguration, metav1.GetOptions{})
		if err != nil {
			return err
		}
		webhooks, err := managedMutatingWebhooks(configuration.Webhooks, r.config)
		if err != nil {
			return err
		}
		changed := false
		for _, webhook := range webhooks {
			if bytes.Equal(webhook.ClientConfig.CABundle, bundle) {
				continue
			}
			webhook.ClientConfig.CABundle = append([]byte(nil), bundle...)
			changed = true
		}
		if !changed {
			return nil
		}
		_, err = client.Update(ctx, configuration, metav1.UpdateOptions{})
		return err
	})
}

func (r *Rotator) setValidatingBundle(ctx context.Context, bundle []byte) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		client := r.client.AdmissionregistrationV1().ValidatingWebhookConfigurations()
		configuration, err := client.Get(ctx, r.config.ValidatingWebhookConfiguration, metav1.GetOptions{})
		if err != nil {
			return err
		}
		webhooks, err := managedValidatingWebhooks(configuration.Webhooks, r.config)
		if err != nil {
			return err
		}
		changed := false
		for _, webhook := range webhooks {
			if bytes.Equal(webhook.ClientConfig.CABundle, bundle) {
				continue
			}
			webhook.ClientConfig.CABundle = append([]byte(nil), bundle...)
			changed = true
		}
		if !changed {
			return nil
		}
		_, err = client.Update(ctx, configuration, metav1.UpdateOptions{})
		return err
	})
}

func validateConfig(config Config) error {
	for label, value := range map[string]string{
		"Secret name":                         config.SecretName,
		"Lease name":                          config.LeaseName,
		"MutatingWebhookConfiguration name":   config.MutatingWebhookConfiguration,
		"ValidatingWebhookConfiguration name": config.ValidatingWebhookConfiguration,
	} {
		if problems := validation.IsDNS1123Subdomain(value); len(problems) != 0 {
			return fmt.Errorf("%s is invalid: %s", label, problems[0])
		}
	}
	for label, value := range map[string]string{
		"namespace":         config.Namespace,
		"service name":      config.ServiceName,
		"service namespace": config.ServiceNamespace,
	} {
		if problems := validation.IsDNS1123Label(value); len(problems) != 0 {
			return fmt.Errorf("%s is invalid: %s", label, problems[0])
		}
	}
	if err := validateWebhookNames("mutating", config.MutatingWebhookNames); err != nil {
		return err
	}
	if err := validateWebhookNames("validating", config.ValidatingWebhookNames); err != nil {
		return err
	}
	if problems := validation.IsValidPortName(config.EndpointPortName); len(problems) != 0 {
		return fmt.Errorf("EndpointSlice port name is invalid: %s", problems[0])
	}
	if config.HolderIdentity == "" || len(config.HolderIdentity) > 128 {
		return errors.New("holder identity must contain 1 to 128 characters")
	}
	if config.RenewalThreshold <= 0 {
		return errors.New("renewal threshold must be positive")
	}
	if config.ServingCertificateValidity <= config.RenewalThreshold || config.ServingCertificateValidity > maximumValidity {
		return errors.New("serving certificate validity must exceed the renewal threshold and be at most 20 years")
	}
	if config.CACertificateValidity <= config.ServingCertificateValidity || config.CACertificateValidity > maximumValidity {
		return errors.New("CA certificate validity must exceed serving certificate validity and be at most 20 years")
	}
	if config.ProbeInterval <= 0 || config.ProbeTimeout <= config.ProbeInterval || config.ProbeTimeout > maximumOperationTime {
		return errors.New("probe timeout must exceed the positive probe interval and be at most 24 hours")
	}
	if config.LeaseDuration < minimumLeaseDuration || config.LeaseDuration > maximumOperationTime {
		return errors.New("Lease duration must be between 30 seconds and 24 hours")
	}
	if config.AcquireTimeout <= 0 || config.AcquireTimeout > maximumOperationTime {
		return errors.New("Lease acquire timeout must be positive and at most 24 hours")
	}
	return nil
}

func managedMutatingWebhooks(webhooks []admissionregistrationv1.MutatingWebhook, config Config) ([]*admissionregistrationv1.MutatingWebhook, error) {
	expected := make(map[string]struct{}, len(config.MutatingWebhookNames))
	for _, name := range config.MutatingWebhookNames {
		expected[name] = struct{}{}
	}
	found := make(map[string]struct{}, len(expected))
	managed := make([]*admissionregistrationv1.MutatingWebhook, 0, len(expected))
	for i := range webhooks {
		webhook := &webhooks[i]
		if _, wanted := expected[webhook.Name]; !wanted {
			continue
		}
		if _, duplicate := found[webhook.Name]; duplicate {
			return nil, fmt.Errorf("webhook %q appears more than once", webhook.Name)
		}
		if !webhookTargetsService(webhook.ClientConfig, config) {
			return nil, fmt.Errorf("webhook %q does not target the configured Service", webhook.Name)
		}
		managed = append(managed, webhook)
		found[webhook.Name] = struct{}{}
	}
	if missing := missingMapKeys(expected, found); len(missing) != 0 {
		return nil, fmt.Errorf("required mutating webhooks were not found: %v", missing)
	}
	return managed, nil
}

func managedValidatingWebhooks(webhooks []admissionregistrationv1.ValidatingWebhook, config Config) ([]*admissionregistrationv1.ValidatingWebhook, error) {
	expected := make(map[string]struct{}, len(config.ValidatingWebhookNames))
	for _, name := range config.ValidatingWebhookNames {
		expected[name] = struct{}{}
	}
	found := make(map[string]struct{}, len(expected))
	managed := make([]*admissionregistrationv1.ValidatingWebhook, 0, len(expected))
	for i := range webhooks {
		webhook := &webhooks[i]
		if _, wanted := expected[webhook.Name]; !wanted {
			continue
		}
		if _, duplicate := found[webhook.Name]; duplicate {
			return nil, fmt.Errorf("webhook %q appears more than once", webhook.Name)
		}
		if !webhookTargetsService(webhook.ClientConfig, config) {
			return nil, fmt.Errorf("webhook %q does not target the configured Service", webhook.Name)
		}
		managed = append(managed, webhook)
		found[webhook.Name] = struct{}{}
	}
	if missing := missingMapKeys(expected, found); len(missing) != 0 {
		return nil, fmt.Errorf("required validating webhooks were not found: %v", missing)
	}
	return managed, nil
}

func webhookTargetsService(clientConfig admissionregistrationv1.WebhookClientConfig, config Config) bool {
	return clientConfig.Service != nil &&
		clientConfig.Service.Name == config.ServiceName &&
		clientConfig.Service.Namespace == config.ServiceNamespace
}

func validateWebhookNames(kind string, names []string) error {
	if len(names) == 0 {
		return fmt.Errorf("at least one %s webhook name is required", kind)
	}
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if problems := validation.IsDNS1123Subdomain(name); len(problems) != 0 {
			return fmt.Errorf("%s webhook name %q is invalid: %s", kind, name, problems[0])
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("%s webhook name %q appears more than once", kind, name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func missingMapKeys(expected, found map[string]struct{}) []string {
	keys := make([]string, 0, len(expected)-len(found))
	for key := range expected {
		if _, ok := found[key]; !ok {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

func cloneBytesMap(source map[string][]byte) map[string][]byte {
	cloned := make(map[string][]byte, len(source)+4)
	for key, value := range source {
		cloned[key] = append([]byte(nil), value...)
	}
	return cloned
}

func secretContainsMaterial(secret *corev1.Secret, material certificateMaterial) bool {
	return bytes.Equal(secret.Data[corev1.TLSCertKey], material.certPEM) &&
		bytes.Equal(secret.Data[corev1.TLSPrivateKeyKey], material.keyPEM) &&
		bytes.Equal(secret.Data[CACertificateKey], material.caPEM) &&
		bytes.Equal(secret.Data[CAPrivateKeyKey], material.caKeyPEM)
}

func certificateRawEqual(left, right *x509.Certificate) bool {
	return left != nil && right != nil && bytes.Equal(left.Raw, right.Raw)
}
