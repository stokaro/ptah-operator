package certrotation

import (
	"bytes"
	"context"
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"
)

type resultLeaf struct {
	Certificate []byte `json:"certificate"`
	Key         []byte `json:"key"`
}

type resultLeafRepair struct {
	Current *resultLeaf `json:"current,omitempty"`
	Next    *resultLeaf `json:"next,omitempty"`
}

func (st resultJournal) beforeLeafRepair() resultJournal {
	if st.PreviousLeaves == nil {
		return st
	}
	if leaf := st.PreviousLeaves.Current; leaf != nil {
		st.Current.ServerCertificate, st.Current.ServerCertificateKey = leaf.Certificate, leaf.Key
	}
	if leaf := st.PreviousLeaves.Next; leaf != nil && st.Next != nil {
		next := *st.Next
		next.ServerCertificate, next.ServerCertificateKey = leaf.Certificate, leaf.Key
		st.Next = &next
	}
	st.PreviousLeaves = nil
	return st
}

func (st resultJournal) acceptsProjection(data map[string][]byte) bool {
	desired, prior := st.projections()
	if equalBytes(data, desired) || equalBytes(data, prior) {
		return true
	}
	if st.PreviousLeaves != nil {
		oldDesired, oldPrior := st.beforeLeafRepair().projections()
		if equalBytes(data, oldDesired) || equalBytes(data, oldPrior) {
			return true
		}
	}
	return st.DiscardExpiredCandidate && equalBytes(data, st.Current.projection())
}

func (r *ResultRotator) validateRecovery(st resultJournal) error {
	invalid := errors.New("invalid result rotation recovery")
	if st.Phase == "stable" && (st.PreviousLeaves != nil || st.DiscardExpiredCandidate) {
		return invalid
	}
	if st.PreviousLeaves == nil {
		return nil
	}
	if st.PreviousLeaves.Current == nil && st.PreviousLeaves.Next == nil || st.PreviousLeaves.Next != nil && st.Next == nil {
		return invalid
	}
	old := st.beforeLeafRepair()
	if _, _, err := r.inspectKeys(old.Current); err != nil {
		return invalid
	}
	if old.Next != nil {
		if _, _, err := r.inspectKeys(*old.Next); err != nil {
			return invalid
		}
	}
	return nil
}

// Renew only leaves under their original authorities. Trust bundles, the
// enrollment policy, candidate authorities, and persisted fences do not change.
// Journal both versions before any projection write so uncertain writes resume.
func (r *ResultRotator) repairPendingLeaves(st *resultJournal) (bool, error) {
	repair := &resultLeafRepair{}
	var servingCurrent *resultKeys
	if st.Phase == "bootstrap" || st.Phase == "prepare" {
		servingCurrent = &st.Current
	}
	for _, item := range []struct {
		keys     *resultKeys
		previous **resultLeaf
	}{{servingCurrent, &repair.Current}, {st.Next, &repair.Next}} {
		if item.keys == nil {
			continue
		}
		server, _, err := r.inspectKeys(*item.keys)
		if err != nil {
			return false, err
		}
		if server.ca.NotAfter.Sub(r.now()) <= time.Minute || certificateCurrentlyValid(server.leaf, r.now()) && server.leaf.NotAfter.After(r.now().Add(r.config.ProbeTimeout+time.Minute)) {
			continue
		}
		validity := min(r.config.ServingCertificateValidity, server.ca.NotAfter.Sub(r.now())-time.Second)
		material, err := generateServingMaterialForService(r.random, r.now(), validity, r.config.ServiceName, r.config.Namespace, server)
		if err != nil {
			return false, err
		}
		*item.previous = &resultLeaf{Certificate: bytes.Clone(item.keys.ServerCertificate), Key: bytes.Clone(item.keys.ServerCertificateKey)}
		item.keys.ServerCertificate, item.keys.ServerCertificateKey = material.certPEM, material.keyPEM
	}
	if repair.Current == nil && repair.Next == nil {
		return false, nil
	}
	st.PreviousLeaves = repair
	return true, nil
}

func (r *ResultRotator) candidateExpired(st resultJournal) bool {
	keys := st.Next
	if keys == nil {
		if st.Phase != "bootstrap" {
			return false
		}
		keys = &st.Current
	}
	server, client, err := r.inspectKeys(*keys)
	return err == nil && !r.now().Before(server.ca.NotAfter.Add(maximumCertificatePolicyClockSkew)) && !r.now().Before(client.ca.NotAfter.Add(maximumCertificatePolicyClockSkew))
}

func (r *ResultRotator) discardExpiredCandidate(ctx context.Context, journal, projection *corev1.Secret, policy *corev1.ConfigMap, st resultJournal) (Result, error) {
	if !r.candidateExpired(st) {
		return Result{}, errors.New("result candidate authorities have not both expired")
	}
	// Every credential issued under Current still trusts its server CA. Every
	// credential issued under Next is unusable now. Keep Current, even if a
	// configuration change gave it a later expiration than Next, then let the
	// stable path renew it normally. This checkpoint is pending, not readiness:
	// Current may itself have expired while the rotator was down.
	data := st.Current.projection()
	if err := r.writePolicy(ctx, policy, enrollment(data)); err != nil {
		return Result{}, err
	}
	if err := r.writeProjection(ctx, projection, data); err != nil {
		return Result{}, err
	}
	st.Phase, st.Next, st.FencedAt, st.PreviousLeaves, st.DiscardExpiredCandidate = "stable", nil, nil, nil, false
	return Result{Pending: true, RequeueAfter: time.Second}, r.saveJournal(ctx, journal, st)
}
