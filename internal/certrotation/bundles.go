package certrotation

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
)

// observedCABundles separates well-formed certificate bundles from damaged
// entries. A damaged managed caBundle is repairable state, not a reason to
// leave every managed webhook unusable. Its bytes are never copied into a
// replacement trust bundle.
type observedCABundles struct {
	valid        [][]byte
	invalidCount int
	total        int
}

func observeCABundles(bundles ...[]byte) observedCABundles {
	observed := observedCABundles{total: len(bundles)}
	for _, bundle := range bundles {
		normalized, err := combineCABundles(bundle)
		if err != nil {
			observed.invalidCount++
			continue
		}
		observed.valid = append(observed.valid, normalized)
	}
	return observed
}

func (observed observedCABundles) allEqual(expected []byte) bool {
	if observed.total == 0 || observed.invalidCount != 0 || len(observed.valid) != observed.total {
		return false
	}
	for _, bundle := range observed.valid {
		if !caBundlesEqual(bundle, expected) {
			return false
		}
	}
	return true
}

// combineAnchoredCABundles preserves only well-formed transition bundles that
// contain the authoritative CA from the current Secret. This retains the old
// CA during a restart of the two-phase transition without carrying forward an
// unrelated or malformed trust root. The authoritative CA is always present
// in the returned bundle, including when every observed entry is damaged.
func combineAnchoredCABundles(
	mutating observedCABundles,
	validating observedCABundles,
	authoritativeCA []byte,
) ([]byte, error) {
	candidates := make([][]byte, 0, len(mutating.valid)+len(validating.valid)+1)
	for _, observed := range []observedCABundles{mutating, validating} {
		for _, bundle := range observed.valid {
			if caBundleContainsCertificate(bundle, authoritativeCA) {
				candidates = append(candidates, bundle)
			}
		}
	}
	candidates = append(candidates, authoritativeCA)
	return combineCABundles(candidates...)
}

func caBundleContainsCertificate(bundle, wanted []byte) bool {
	certificates, err := parseCertificateBundle(bundle)
	if err != nil {
		return false
	}
	wantedCertificates, err := parseCertificateBundle(wanted)
	if err != nil || len(wantedCertificates) != 1 {
		return false
	}
	for _, certificate := range certificates {
		if bytes.Equal(certificate.Raw, wantedCertificates[0].Raw) {
			return true
		}
	}
	return false
}

func combineCABundles(bundles ...[]byte) ([]byte, error) {
	seen := make(map[[sha256.Size]byte]struct{})
	var combined bytes.Buffer
	for _, bundle := range bundles {
		certificates, err := parseCertificateBundle(bundle)
		if err != nil {
			return nil, err
		}
		for _, certificate := range certificates {
			digest := sha256.Sum256(certificate.Raw)
			if _, exists := seen[digest]; exists {
				continue
			}
			seen[digest] = struct{}{}
			if err := pem.Encode(&combined, &pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}); err != nil {
				return nil, fmt.Errorf("encode CA certificate: %w", err)
			}
		}
	}
	if combined.Len() == 0 {
		return nil, errors.New("CA bundle contains no certificates")
	}
	return combined.Bytes(), nil
}

func parseCertificateBundle(bundle []byte) ([]*x509.Certificate, error) {
	remaining := bytes.TrimSpace(bundle)
	if len(remaining) == 0 {
		return nil, errors.New("CA bundle is empty")
	}
	var certificates []*x509.Certificate
	for len(remaining) != 0 {
		block, rest := pem.Decode(remaining)
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, errors.New("CA bundle contains non-certificate PEM data")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse CA bundle certificate: %w", err)
		}
		certificates = append(certificates, certificate)
		remaining = bytes.TrimSpace(rest)
	}
	return certificates, nil
}

func caBundlesEqual(left, right []byte) bool {
	leftCombined, leftErr := combineCABundles(left)
	rightCombined, rightErr := combineCABundles(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftCombined, rightCombined)
}
