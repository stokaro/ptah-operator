package certrotation

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// runPendingCATransition executes the complete durable transition from live
// evidence on every attempt. The staged phase is only a monotonic audit cursor:
// it never substitutes for exact API readback, endpoint proof, or a canary
// convergence barrier after a restart.
func (r *Rotator) runPendingCATransition(
	ctx context.Context,
	staging *corev1.Secret,
	pending *pendingCandidate,
	trustedOldCA []byte,
) error {
	if pending == nil {
		return errors.New("durable CA transition is required")
	}
	if err := r.candidateSink.StoreCandidateCertificate(pending.listenerCertPEM, pending.listenerKeyPEM); err != nil {
		r.candidateSink.ClearCandidateCertificate()
		return fmt.Errorf("serve durable expansion certificate: %w", err)
	}
	expansion, err := NewAdmissionCanaryExpansion(trustedOldCA, pending.material.caPEM)
	if err != nil {
		return fmt.Errorf("build durable CA trust expansion: %w", err)
	}
	if err := r.canary.PublishMutating(ctx, expansion); err != nil {
		return fmt.Errorf("publish mutating CA trust expansion: %w", err)
	}
	if err := r.recordPendingEvidence(ctx, &staging, pending, stagingPhaseExpansionMutatingStored); err != nil {
		return err
	}
	if err := r.canary.PublishValidating(ctx, expansion); err != nil {
		return fmt.Errorf("publish validating CA trust expansion: %w", err)
	}
	if err := r.recordPendingEvidence(ctx, &staging, pending, stagingPhaseExpansionBothStored); err != nil {
		return err
	}
	if err := r.canary.Wait(ctx, expansion); err != nil {
		return fmt.Errorf("prove CA trust expansion through every API server: %w", err)
	}
	if err := r.recordPendingEvidence(ctx, &staging, pending, stagingPhaseExpansionProven); err != nil {
		return err
	}

	primary, err := r.readPendingPrimary(ctx)
	if err != nil {
		return err
	}
	switch relationship := relatePendingCandidate(primary, pending, r.config); relationship {
	case pendingBeforePrimaryWrite:
		if pending.sourceState == stagingSourceMissing {
			desired := generatedSecret(r.config, pending.material)
			if err := r.ensureSecretCreateGuard(ctx, desired); err != nil {
				return err
			}
			if err := r.createSecret(ctx, desired, pending.material); err != nil {
				return err
			}
		} else if err := r.updateSecret(ctx, primary, pending.material); err != nil {
			return err
		}
	case pendingAfterPrimaryWrite:
		// An exact primary readback is authoritative evidence of an earlier
		// successful write whose response or phase update may have been lost.
	case pendingUnrelated:
		return errors.New("durable CA transition became unrelated to the generated TLS Secret")
	default:
		return errors.New("durable CA transition has an unknown primary relationship")
	}
	if err := r.recordPendingEvidence(ctx, &staging, pending, stagingPhasePrimaryWritten); err != nil {
		return err
	}
	if err := r.probeCurrentCertificate(ctx, pending.material); err != nil {
		return err
	}
	if err := r.recordPendingEvidence(ctx, &staging, pending, stagingPhasePrimaryServed); err != nil {
		return err
	}

	if err := r.candidateSink.StoreCandidateCertificate(
		pending.proofListenerCertPEM,
		pending.proofListenerKeyPEM,
	); err != nil {
		r.candidateSink.ClearCandidateCertificate()
		return fmt.Errorf("serve durable contraction-proof certificate: %w", err)
	}
	contraction, err := NewAdmissionCanaryContraction(pending.material.caPEM, pending.proofCACertPEM)
	if err != nil {
		return fmt.Errorf("build durable CA trust contraction: %w", err)
	}
	if err := r.canary.PublishMutating(ctx, contraction); err != nil {
		return fmt.Errorf("publish mutating CA trust contraction: %w", err)
	}
	if err := r.recordPendingEvidence(ctx, &staging, pending, stagingPhaseContractionMutatingStored); err != nil {
		return err
	}
	if err := r.canary.PublishValidating(ctx, contraction); err != nil {
		return fmt.Errorf("publish validating CA trust contraction: %w", err)
	}
	if err := r.recordPendingEvidence(ctx, &staging, pending, stagingPhaseContractionBothStored); err != nil {
		return err
	}
	if err := r.canary.Wait(ctx, contraction); err != nil {
		return fmt.Errorf("prove CA trust contraction through every API server: %w", err)
	}
	if err := r.recordPendingEvidence(ctx, &staging, pending, stagingPhaseContractionProven); err != nil {
		return err
	}

	// Switch back to the candidate CA before parking either canary. The canary
	// selectors are exact and dormant between our probes, so the sequential
	// singleton updates cannot affect unrelated API requests.
	if err := r.candidateSink.StoreCandidateCertificate(pending.listenerCertPEM, pending.listenerKeyPEM); err != nil {
		r.candidateSink.ClearCandidateCertificate()
		return fmt.Errorf("restore durable candidate certificate for canary parking: %w", err)
	}
	parked, err := NewAdmissionCanaryParked(pending.material.caPEM)
	if err != nil {
		return fmt.Errorf("build parked admission canary state: %w", err)
	}
	if err := r.canary.PublishMutating(ctx, parked); err != nil {
		return fmt.Errorf("park mutating admission canary: %w", err)
	}
	if err := r.recordPendingEvidence(ctx, &staging, pending, stagingPhaseMutatingParked); err != nil {
		return err
	}
	if err := r.canary.PublishValidating(ctx, parked); err != nil {
		return fmt.Errorf("park validating admission canary: %w", err)
	}
	if err := r.canary.Wait(ctx, parked); err != nil {
		return fmt.Errorf("prove parked admission canaries through every API server: %w", err)
	}
	if err := r.recordPendingEvidence(ctx, &staging, pending, stagingPhaseBothParked); err != nil {
		return err
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

func (r *Rotator) readPendingPrimary(ctx context.Context) (*corev1.Secret, error) {
	secret, err := r.client.CoreV1().Secrets(r.config.Namespace).Get(ctx, r.config.SecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("re-read generated TLS Secret before durable primary write: %w", err)
	}
	if err := validatePrimarySecretSource(secret, r.config); err != nil {
		return nil, fmt.Errorf("re-read generated TLS Secret source contract: %w", err)
	}
	return secret, nil
}

func (r *Rotator) recordPendingEvidence(
	ctx context.Context,
	staging **corev1.Secret,
	pending *pendingCandidate,
	target stagingPhase,
) error {
	currentRank, currentOK := stagingPhaseRank(pending.phase)
	targetRank, targetOK := stagingPhaseRank(target)
	if !currentOK || !targetOK || staging == nil || *staging == nil {
		return errors.New("record durable CA transition evidence: phase and staging Secret must be valid")
	}
	if currentRank >= targetRank {
		return nil
	}
	next, ok := nextStagingPhase(pending.phase)
	if !ok || next != target {
		return fmt.Errorf(
			"record durable CA transition evidence: phase %q cannot follow verified target %q",
			pending.phase,
			target,
		)
	}
	updated, err := r.advancePendingPhase(ctx, *staging, pending, target)
	if err != nil {
		return err
	}
	*staging = updated
	return nil
}

func stagingPhaseRank(phase stagingPhase) (int, bool) {
	for index, candidate := range []stagingPhase{
		stagingPhasePrepared,
		stagingPhaseExpansionMutatingStored,
		stagingPhaseExpansionBothStored,
		stagingPhaseExpansionProven,
		stagingPhasePrimaryWritten,
		stagingPhasePrimaryServed,
		stagingPhaseContractionMutatingStored,
		stagingPhaseContractionBothStored,
		stagingPhaseContractionProven,
		stagingPhaseMutatingParked,
		stagingPhaseBothParked,
	} {
		if phase == candidate {
			return index, true
		}
	}
	return 0, false
}
