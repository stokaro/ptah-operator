package certrotation

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// A CA transition normally spans two reconciliations, and the staging Secret
// records each step before the next one starts:
//
//  1. Expand: every managed webhook entry trusts the old and the new CA, and
//     the record notes when that became true.
//  2. Switch: no earlier than CASwitchDelay later, one atomic write gives the
//     generated Secret the new CA and a serving certificate issued by it.
//  3. Retire: once every webhook endpoint serves that certificate, every
//     managed entry trusts the new CA alone, and the record is cleared.
//
// The delay is what the removed admission canary used to prove directly: that
// every API server has picked up the expanded bundle before any webhook
// presents a certificate only the new CA verifies. Nothing proves it now; the
// delay makes it true with a wide margin, since an API server sees a webhook
// configuration change within seconds and the default delay is hours.

// runPendingCATransition takes one durable pending transition as far as the
// clock allows. It returns a positive RequeueAfter while the switch waits.
func (r *Rotator) runPendingCATransition(
	ctx context.Context,
	staging *corev1.Secret,
	primary *corev1.Secret,
	pending *pendingCandidate,
	relationship pendingRelationship,
) (Result, error) {
	switch relationship {
	case pendingAfterPrimaryWrite:
		// The generated Secret already holds the new material, so the switch
		// happened on an earlier pass whose later steps were interrupted.
		return Result{}, r.retireOldCA(ctx, staging, pending)
	case pendingBeforePrimaryWrite:
	default:
		return Result{}, errors.New("durable CA transition has no known relationship to the generated TLS Secret")
	}
	if pending.sourceState == stagingSourceMissing {
		// Prove the broad-CREATE guard on every pass, before trust changes, so
		// a guard that stopped holding blocks the transition where it stands.
		if err := r.ensureSecretCreateGuard(ctx, generatedSecret(r.config, pending.material)); err != nil {
			return Result{}, err
		}
	}

	current, err := r.currentServingTrust(ctx, primary)
	if err != nil {
		return Result{}, err
	}
	additions := [][]byte{pending.material.caPEM}
	if len(current.bundle) != 0 {
		additions = [][]byte{current.bundle, pending.material.caPEM}
	}
	candidateWasPublished, err := r.candidateTrustedEverywhere(ctx, pending.material.caPEM)
	if err != nil {
		return Result{}, err
	}
	r.logStep("publishing the old and the new CA in every managed webhook entry")
	if err := r.publishPerEntryTransition(ctx, additions...); err != nil {
		return Result{}, fmt.Errorf("publish CA trust expansion: %w", err)
	}

	now := r.now()
	expandedNow := expansionInstant(now)
	switch {
	case pending.phase == stagingPhasePrepared:
		staging, err = r.recordExpansion(ctx, staging, pending, expandedNow)
	case !candidateWasPublished:
		// An entry lost the new CA since the expansion was recorded, so some
		// API server may have seen it without; its dwell starts again.
		r.logStep("CA trust expansion was incomplete; restarting the switch delay")
		staging, err = r.recordExpansion(ctx, staging, pending, expandedNow)
	case pending.expandedAt.After(expandedNow):
		// The recorded instant lies ahead of this clock, which moved back or
		// belonged to another host. Restarting the dwell from now bounds the
		// wait to one delay; keeping the record could stretch it without limit.
		r.logStep("CA trust expansion is dated in the future; restarting the switch delay")
		staging, err = r.recordExpansion(ctx, staging, pending, expandedNow)
	}
	if err != nil {
		return Result{}, err
	}

	switchAt := pending.expandedAt.Add(r.config.CASwitchDelay)
	if current.serving && now.Before(switchAt) {
		r.logStep("CA transition waits for its switch", "switchAt", switchAt.Format(time.RFC3339))
		return Result{RequeueAfter: switchAt.Sub(now)}, nil
	}
	if !current.serving {
		// The current serving certificate has expired, no managed entry holds
		// a CA that issued it, or it is the install's bootstrap material, so
		// there is nothing the delay would protect.
		r.logStep("current serving certificate is expired, unverifiable, or bootstrap; switching without the delay")
	}
	if err := r.switchToNewCA(ctx, primary, pending); err != nil {
		return Result{}, err
	}
	return Result{}, r.retireOldCA(ctx, staging, pending)
}

// servingTrust is what the transition must keep trusted until the switch.
type servingTrust struct {
	// bundle holds the CA certificates that authenticate the current serving
	// certificate. It is empty when none is known.
	bundle []byte
	// serving reports whether an API server can still verify the current
	// serving certificate through a CA in bundle. The switch delay protects
	// only a certificate that it can.
	serving bool
}

func (r *Rotator) currentServingTrust(ctx context.Context, primary *corev1.Secret) (servingTrust, error) {
	if primary == nil {
		// The Secret is gone, but running manager Pods keep the certificate
		// they last loaded, and the managed entries keep the CA that issued it.
		// Every parseable certificate in an entry is preserved by the expansion,
		// so treat that certificate as serving and let the delay protect it.
		return servingTrust{serving: true}, nil
	}
	now := r.now()
	state, err := inspectSecret(primary, r.config, now)
	if err != nil {
		return servingTrust{}, fmt.Errorf("inspect source TLS Secret for durable CA transition: %w", err)
	}
	trust := servingTrust{}
	switch {
	case state.currentServingChainAuthentic:
		trust.bundle = state.current.caPEM
	case state.current.leaf != nil:
		mutating, err := r.readMutatingBundles(ctx)
		if err != nil {
			return servingTrust{}, err
		}
		validating, err := r.readValidatingBundles(ctx)
		if err != nil {
			return servingTrust{}, err
		}
		recovered, found, err := authenticServingCABundle(
			mutating,
			validating,
			state.current.leaf,
			requiredDNSNames(r.config),
		)
		if err != nil {
			return servingTrust{}, fmt.Errorf("filter authentic serving-certificate CAs: %w", err)
		}
		if found {
			if err := r.probeCertificateIdentity(ctx, recovered, state.current.leaf); err != nil {
				return servingTrust{}, fmt.Errorf("prove recovered CA signs the live serving certificate: %w", err)
			}
			trust.bundle = recovered
		}
	}
	serving, err := stillServing(state.current.leaf, trust.bundle, now)
	if err != nil {
		return servingTrust{}, err
	}
	trust.serving = serving && !issuedInsideRenewalThreshold(trust.bundle, r.config.RenewalThreshold)
	return trust, nil
}

// issuedInsideRenewalThreshold reports whether every CA in the bundle was
// already due for renewal when it was issued. That is the chart's bootstrap
// CA: it lives two days, it exists only until the rotator's first pass
// replaces it, and the rotator reports ready only after that pass. Waiting
// out the switch delay there would let `helm install --wait` return with the
// transition still open, and the rotator's final bundle write would then land
// inside a later `helm upgrade`, between its render and its server-side apply,
// which fails the upgrade with a field-manager conflict on caBundle.
func issuedInsideRenewalThreshold(bundle []byte, threshold time.Duration) bool {
	certificates, err := parseCertificateBundle(bundle)
	if err != nil || len(certificates) == 0 {
		return false
	}
	for _, certificate := range certificates {
		if certificate.NotAfter.Sub(certificate.NotBefore) > threshold {
			return false
		}
	}
	return true
}

// stillServing reports whether an API server could still verify the leaf
// through a CA in the bundle, both already proved to belong together. Only
// expiry counts against it: a certificate that looks not yet valid means this
// clock runs behind the one that issued it, and switching early on the word
// of a slow clock is exactly what the delay is there to prevent.
func stillServing(leaf *x509.Certificate, bundle []byte, now time.Time) (bool, error) {
	if leaf == nil || len(bundle) == 0 || !now.Before(leaf.NotAfter) {
		return false, nil
	}
	certificates, err := parseCertificateBundle(bundle)
	if err != nil {
		return false, fmt.Errorf("parse current serving trust: %w", err)
	}
	for _, certificate := range certificates {
		if now.Before(certificate.NotAfter) {
			return true, nil
		}
	}
	return false, nil
}

// candidateTrustedEverywhere reports whether every managed entry already
// holds the candidate CA in a well-formed bundle.
func (r *Rotator) candidateTrustedEverywhere(ctx context.Context, candidateCA []byte) (bool, error) {
	mutating, err := r.readMutatingBundles(ctx)
	if err != nil {
		return false, err
	}
	validating, err := r.readValidatingBundles(ctx)
	if err != nil {
		return false, err
	}
	for _, observed := range []observedCABundles{mutating, validating} {
		if observed.invalidCount != 0 || len(observed.valid) != observed.total {
			return false, nil
		}
		for _, bundle := range observed.valid {
			if !caBundleContainsCertificate(bundle, candidateCA) {
				return false, nil
			}
		}
	}
	return true, nil
}

func (r *Rotator) switchToNewCA(ctx context.Context, primary *corev1.Secret, pending *pendingCandidate) error {
	r.logStep("switching the generated TLS Secret to the new CA")
	if pending.sourceState == stagingSourceMissing {
		return r.createSecret(ctx, generatedSecret(r.config, pending.material), pending.material)
	}
	return r.updateSecret(ctx, primary, pending.material)
}

// retireOldCA finishes a transition whose switch is done: it proves every
// endpoint serves the new certificate, drops every other root from the
// managed entries, and clears the durable record.
func (r *Rotator) retireOldCA(ctx context.Context, staging *corev1.Secret, pending *pendingCandidate) error {
	if err := r.probeCurrentCertificate(ctx, pending.material); err != nil {
		return err
	}
	r.logStep("retiring the old CA from every managed webhook entry")
	if err := r.setBothBundles(ctx, pending.material.caPEM); err != nil {
		return fmt.Errorf("retire the old CA: %w", err)
	}
	if err := r.clearPendingCandidate(ctx, staging); err != nil {
		return fmt.Errorf("retire completed pending CA transition: %w", err)
	}
	needsRenewal, err := pendingMaterialNeedsCurrentPolicyRenewal(pending.material, r.config, r.now())
	if err != nil {
		return fmt.Errorf("reevaluate completed pending CA transition: %w", err)
	}
	if needsRenewal {
		// Durable material is decoded against absolute safety limits so a policy
		// change cannot strand a transition after the primary write. Complete
		// that transition first, then request immediate current-policy renewal.
		return errors.New("completed pending CA transition requires immediate renewal under the current certificate policy")
	}
	return nil
}
