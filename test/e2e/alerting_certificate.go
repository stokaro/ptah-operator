package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
)

// The leaf expires only after every manager has had time to project it. The
// receiver must warn within the usual detection target, measured from the
// signed expiry minus the warning threshold, not from a poll that noticed it.
const (
	alCertificateWarning    = 24 * time.Hour
	alCertificateLifetime   = 5 * time.Minute
	alCertificateProjection = 3 * time.Minute
	alCertificateAlert      = "PtahOperatorWebhookCertificateExpiring"
	alCertificateMetric     = "ptah_operator_webhook_certificate_expiry_timestamp_seconds"
	alApprovalWebhook       = "vapproval.operator.ptah.run"
)

func alFreshCertificateBundle(bundle []byte, generation int) []byte {
	return append(bytes.Clone(bundle), bytes.Repeat([]byte{'\n'}, generation)...)
}

// The probe PATCH is an UPDATE. The approval mutator handles only CREATE,
// so changing its transport would never force a new connection for this probe.
func alApprovalProbeWebhook(hook admissionregistrationv1.ValidatingWebhook) bool {
	if hook.Name != alApprovalWebhook || hook.FailurePolicy == nil || *hook.FailurePolicy != admissionregistrationv1.Fail ||
		hook.SideEffects == nil || *hook.SideEffects != admissionregistrationv1.SideEffectClassNone ||
		!emptySelector(hook.NamespaceSelector) || !emptySelector(hook.ObjectSelector) || len(hook.MatchConditions) != 0 {
		return false
	}
	for _, rule := range hook.Rules {
		if slices.Contains(rule.Operations, admissionregistrationv1.Update) && slices.Contains(rule.APIGroups, "operator.ptah.run") &&
			slices.Contains(rule.APIVersions, "v1alpha1") && slices.Contains(rule.Resources, "ptahschemaapprovals") &&
			(rule.Scope == nil || *rule.Scope == admissionregistrationv1.NamespacedScope || *rule.Scope == admissionregistrationv1.AllScopes) {
			return true
		}
	}
	return false
}

// Reissue the installed leaf under its own CA and key. Only its validity and
// serial change; the fault must not introduce a different trust or DNS error.
// Private keys stay in memory, and errors never include their input bytes.
func alServingCertificate(data map[string][]byte, now time.Time, lifetime time.Duration) ([]byte, time.Time, error) {
	if lifetime <= 0 {
		return nil, time.Time{}, errors.New("the serving certificate lifetime must be positive")
	}
	caPair, err := tls.X509KeyPair(data["ca.crt"], data["ca.key"])
	if err != nil {
		return nil, time.Time{}, errors.New("the installed certificate authority and key do not form a pair")
	}
	leafPair, err := tls.X509KeyPair(data["tls.crt"], data["tls.key"])
	if err != nil {
		return nil, time.Time{}, errors.New("the installed serving certificate and key do not form a pair")
	}
	ca, leaf := caPair.Leaf, leafPair.Leaf
	expires := now.Add(lifetime).UTC().Truncate(time.Second)
	if ca == nil || leaf == nil || leaf.CheckSignatureFrom(ca) != nil || leaf.IsCA ||
		now.Before(ca.NotBefore) || now.Before(leaf.NotBefore) || !ca.NotAfter.After(expires) || !leaf.NotAfter.After(expires) {
		return nil, time.Time{}, errors.New("the installed authority cannot issue the short-lived serving certificate")
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, time.Time{}, errors.New("could not generate the serving certificate serial")
	}
	template := *leaf
	template.SerialNumber = serial.Add(serial, big.NewInt(1))
	template.NotBefore, template.NotAfter = now.UTC().Truncate(time.Second), expires
	der, err := x509.CreateCertificate(rand.Reader, &template, ca, leaf.PublicKey, caPair.PrivateKey)
	if err != nil {
		return nil, time.Time{}, errors.New("could not sign the short-lived serving certificate")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), expires, nil
}

// Require one series for each actual manager Pod. Empty, duplicate, stale
// replica names or an absent metric cannot prove certificate projection.
func alCertificateExpiries(body []byte, pods []string, expires time.Time) bool {
	wanted := make(map[string]bool, len(pods))
	for _, pod := range pods {
		if pod == "" || wanted[pod] {
			return false
		}
		wanted[pod] = true
	}
	var response struct {
		Status string
		Data   struct {
			ResultType string
			Result     []struct {
				Metric map[string]string
				Value  []json.RawMessage
			}
		}
	}
	if len(wanted) == 0 || expires.IsZero() || json.Unmarshal(body, &response) != nil ||
		response.Status != "success" || response.Data.ResultType != "vector" || len(response.Data.Result) != len(wanted) {
		return false
	}
	seen := map[string]bool{}
	for _, sample := range response.Data.Result {
		pod := sample.Metric["pod"]
		if !wanted[pod] || seen[pod] || sample.Metric["job"] != alScrapeJob ||
			sample.Metric["__name__"] != alCertificateMetric || len(sample.Value) != 2 {
			return false
		}
		var raw string
		var sampledAt float64
		if json.Unmarshal(sample.Value[0], &sampledAt) != nil || sampledAt <= 0 || json.Unmarshal(sample.Value[1], &raw) != nil {
			return false
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || value != float64(expires.Unix()) {
			return false
		}
		seen[pod] = true
	}
	return true
}

func alExpiredApprovalError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, `failed calling webhook "`+alApprovalWebhook+`"`) &&
		strings.Contains(message, "x509: certificate has expired or is not yet valid") &&
		strings.Contains(message, " is after ")
}
