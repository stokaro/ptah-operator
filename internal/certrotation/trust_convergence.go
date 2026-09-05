package certrotation

import (
	"context"
	"fmt"
)

type canaryServingMaterial struct {
	certificatePEM []byte
	privateKeyPEM  []byte
}

// prepareCurrentTrust makes the Secret-authoritative CA additive everywhere
// and proves the combined mutating and validating cache state through every
// API server before a caller changes primary serving material.
func (r *Rotator) prepareCurrentTrust(
	ctx context.Context,
	current certificateMaterial,
) (canaryServingMaterial, error) {
	listener, err := generateServingMaterialForService(
		r.random,
		r.now(),
		r.config.ServingCertificateValidity,
		r.config.CandidateServiceName,
		r.config.Namespace,
		current,
	)
	if err != nil {
		return canaryServingMaterial{}, fmt.Errorf("generate current-CA admission canary certificate: %w", err)
	}
	served := canaryServingMaterial{
		certificatePEM: listener.certPEM,
		privateKeyPEM:  listener.keyPEM,
	}
	if err := r.candidateSink.StoreCandidateCertificate(served.certificatePEM, served.privateKeyPEM); err != nil {
		r.candidateSink.ClearCandidateCertificate()
		return canaryServingMaterial{}, fmt.Errorf("serve current-CA admission canary certificate: %w", err)
	}
	desired, err := NewAdmissionCanaryExpansion(nil, current.caPEM)
	if err != nil {
		return canaryServingMaterial{}, fmt.Errorf("build current CA trust expansion: %w", err)
	}
	if err := r.convergeAdmissionCanary(ctx, desired); err != nil {
		return canaryServingMaterial{}, fmt.Errorf("prove current CA trust expansion through every API server: %w", err)
	}
	return served, nil
}

// contractAndParkCurrentTrust removes every non-authoritative production root
// only while an independent listener CA proves the new cached configuration.
// It then switches back to the authoritative CA and proves both canaries are
// parked before making the candidate listener unavailable.
func (r *Rotator) contractAndParkCurrentTrust(
	ctx context.Context,
	current certificateMaterial,
	parkedListener canaryServingMaterial,
) error {
	proofConfig := r.config
	proofConfig.ServiceName = r.config.CandidateServiceName
	proofConfig.ServiceNamespace = r.config.Namespace
	proof, err := generateMaterial(r.random, r.now(), proofConfig)
	if err != nil {
		return fmt.Errorf("generate independent CA contraction proof: %w", err)
	}
	if err := r.candidateSink.StoreCandidateCertificate(proof.certPEM, proof.keyPEM); err != nil {
		r.candidateSink.ClearCandidateCertificate()
		return fmt.Errorf("serve independent CA contraction proof: %w", err)
	}
	contraction, err := NewAdmissionCanaryContraction(current.caPEM, proof.caPEM)
	if err != nil {
		return fmt.Errorf("build current CA trust contraction: %w", err)
	}
	if err := r.convergeAdmissionCanary(ctx, contraction); err != nil {
		return fmt.Errorf("prove current CA trust contraction through every API server: %w", err)
	}

	if err := r.candidateSink.StoreCandidateCertificate(
		parkedListener.certificatePEM,
		parkedListener.privateKeyPEM,
	); err != nil {
		r.candidateSink.ClearCandidateCertificate()
		return fmt.Errorf("restore current-CA certificate for admission canary parking: %w", err)
	}
	parked, err := NewAdmissionCanaryParked(current.caPEM)
	if err != nil {
		return fmt.Errorf("build parked current-CA admission canary state: %w", err)
	}
	if err := r.convergeAdmissionCanary(ctx, parked); err != nil {
		return fmt.Errorf("prove parked admission canaries through every API server: %w", err)
	}
	r.candidateSink.ClearCandidateCertificate()
	return nil
}

func (r *Rotator) convergeAdmissionCanary(ctx context.Context, desired AdmissionCanaryDesiredState) error {
	if err := r.canary.PublishMutating(ctx, desired); err != nil {
		return err
	}
	if err := r.canary.PublishValidating(ctx, desired); err != nil {
		return err
	}
	return r.canary.Wait(ctx, desired)
}
