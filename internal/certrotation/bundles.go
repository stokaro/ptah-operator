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
// leave every managed webhook unusable. Malformed bytes are never copied
// verbatim; independently parseable certificate candidates are tracked for
// entry-local preservation or later authentication.
type observedCABundles struct {
	valid        [][]byte
	certificates []*x509.Certificate
	invalidCount int
	total        int
}

func observeCABundles(bundles ...[]byte) observedCABundles {
	observed := observedCABundles{total: len(bundles)}
	for _, bundle := range bundles {
		observed.certificates = append(observed.certificates, certificateCandidates(bundle)...)
		normalized, err := combineCABundles(bundle)
		if err != nil {
			observed.invalidCount++
			continue
		}
		observed.valid = append(observed.valid, normalized)
	}
	return observed
}

// authenticServingCABundle returns only certificates that directly
// authenticate the persisted serving leaf and all required Service DNS names.
// Candidate extraction is intentionally certificate-by-certificate: malformed
// neighbors are ignored, but no candidate enters trust merely because another
// certificate in the same bundle is authentic.
func authenticServingCABundle(
	mutating observedCABundles,
	validating observedCABundles,
	leaf *x509.Certificate,
	requiredNames []string,
) ([]byte, bool, error) {
	candidates := make([][]byte, 0, len(mutating.certificates)+len(validating.certificates))
	seen := make(map[[sha256.Size]byte]struct{})
	for _, observed := range []observedCABundles{mutating, validating} {
		for _, certificate := range observed.certificates {
			if !servingCertificateAuthentic(leaf, certificate, requiredNames) {
				continue
			}
			digest := sha256.Sum256(certificate.Raw)
			if _, exists := seen[digest]; exists {
				continue
			}
			seen[digest] = struct{}{}
			candidates = append(candidates, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}))
		}
	}
	if len(candidates) == 0 {
		return nil, false, nil
	}
	bundle, err := combineCABundles(candidates...)
	if err != nil {
		return nil, false, err
	}
	return bundle, true, nil
}

// certificateCandidates extracts every independently parseable CERTIFICATE
// PEM block while ignoring malformed and non-certificate neighbors. Callers
// decide whether a candidate is merely preserved in its original entry or can
// become authoritative after cryptographic and live-endpoint proof.
func certificateCandidates(bundle []byte) []*x509.Certificate {
	remaining := bundle
	var certificates []*x509.Certificate
	for len(remaining) != 0 {
		block, rest := pem.Decode(remaining)
		if block == nil {
			break
		}
		remaining = rest
		if block.Type != "CERTIFICATE" {
			continue
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err == nil {
			certificates = append(certificates, certificate)
		}
	}
	return certificates
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

func caBundleContainsCertificate(bundle, wanted []byte) bool {
	return caBundleContainsAllCertificates(bundle, wanted)
}

func caBundleContainsAllCertificates(bundle, wanted []byte) bool {
	certificates, err := parseCertificateBundle(bundle)
	if err != nil {
		return false
	}
	wantedCertificates, err := parseCertificateBundle(wanted)
	if err != nil || len(wantedCertificates) == 0 {
		return false
	}
	for _, wantedCertificate := range wantedCertificates {
		found := false
		for _, certificate := range certificates {
			if bytes.Equal(certificate.Raw, wantedCertificate.Raw) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
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

func encodeCertificateBundle(certificates []*x509.Certificate) ([]byte, error) {
	var encoded bytes.Buffer
	for _, certificate := range certificates {
		if err := pem.Encode(&encoded, &pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}); err != nil {
			return nil, fmt.Errorf("encode preserved CA certificate: %w", err)
		}
	}
	if encoded.Len() == 0 {
		return nil, errors.New("CA bundle contains no parseable certificates")
	}
	return encoded.Bytes(), nil
}

func caBundlesEqual(left, right []byte) bool {
	leftCombined, leftErr := combineCABundles(left)
	rightCombined, rightErr := combineCABundles(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftCombined, rightCombined)
}
