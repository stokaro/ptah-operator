package e2e

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/big"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
)

func alCertificateFixture(t *testing.T, now time.Time) map[string][]byte {
	t.Helper()
	ca := sameSubjectCA(t, 1)
	caTemplate := *ca.certificate
	caTemplate.NotBefore, caTemplate.NotAfter = now.Add(-time.Minute), now.Add(72*time.Hour)
	caDER, err := x509.CreateCertificate(rand.Reader, &caTemplate, &caTemplate, ca.key.Public(), ca.key)
	if err != nil {
		t.Fatal(err)
	}
	ca.encoded = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	ca.certificate, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "webhook.operator.svc"},
		DNSNames:  []string{"webhook.operator.svc", "webhook.operator.svc.cluster.local"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(48 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, ca.certificate, key.Public(), ca.key)
	if err != nil {
		t.Fatal(err)
	}
	encodeKey := func(key *ecdsa.PrivateKey) []byte {
		encoded, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})
	}
	return map[string][]byte{
		"ca.crt": ca.encoded, "ca.key": encodeKey(ca.key),
		"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), "tls.key": encodeKey(key),
	}
}

func TestAlShortCertificateKeepsServingIdentity(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	data := alCertificateFixture(t, now)
	original, err := firstCertificate(data["tls.crt"])
	if err != nil {
		t.Fatal(err)
	}
	short, expiry, err := alServingCertificate(data, now, alCertificateLifetime)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(short, data["tls.key"])
	if err != nil {
		t.Fatal("the reissued certificate changed the serving key")
	}
	leaf := pair.Leaf
	if !bytes.Equal(leaf.RawSubjectPublicKeyInfo, original.RawSubjectPublicKeyInfo) ||
		!reflect.DeepEqual(leaf.DNSNames, original.DNSNames) || leaf.SerialNumber.Cmp(original.SerialNumber) == 0 ||
		!leaf.NotAfter.Equal(expiry) || !expiry.Equal(now.Add(alCertificateLifetime)) ||
		!leaf.NotBefore.Equal(now) || !original.NotAfter.After(expiry) {
		t.Fatal("the fault did not preserve serving identity while shortening validity")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data["ca.crt"]) {
		t.Fatal("the original CA is unreadable")
	}
	options := x509.VerifyOptions{Roots: roots, DNSName: original.DNSNames[0], CurrentTime: now.Add(time.Minute)}
	if _, err := leaf.Verify(options); err != nil {
		t.Fatalf("the short-lived leaf was refused before expiry: %v", err)
	}
	options.CurrentTime = expiry.Add(time.Second)
	_, err = leaf.Verify(options)
	var invalid x509.CertificateInvalidError
	if !errors.As(err, &invalid) || invalid.Reason != x509.Expired {
		t.Fatalf("the expired leaf was not refused specifically for expiry: %v", err)
	}
	for generation := range 5 {
		bundle := alFreshCertificateBundle(data["ca.crt"], generation)
		certs, err := certificates(bundle)
		if err != nil || len(certs) != 1 || !bytes.Equal(certs[0].Raw, rootsCertificate(t, data["ca.crt"]).Raw) {
			t.Fatalf("fresh TLS generation %d changed the trusted authority", generation)
		}
		if generation > 0 && bytes.Equal(bundle, alFreshCertificateBundle(data["ca.crt"], generation-1)) {
			t.Fatal("the bundle did not change the webhook transport's cache key")
		}
	}
}

func rootsCertificate(t *testing.T, data []byte) *x509.Certificate {
	t.Helper()
	certificate, err := firstCertificate(data)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

func TestAlShortCertificateRefusesInvalidIdentity(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	valid, other := alCertificateFixture(t, now), alCertificateFixture(t, now)
	for name, change := range map[string]func(map[string][]byte){
		"wrong CA key":                func(data map[string][]byte) { data["ca.key"] = other["ca.key"] },
		"wrong serving key":           func(data map[string][]byte) { data["tls.key"] = other["tls.key"] },
		"unrelated CA":                func(data map[string][]byte) { data["ca.crt"], data["ca.key"] = other["ca.crt"], other["ca.key"] },
		"leaf used as CA":             func(data map[string][]byte) { data["ca.crt"], data["ca.key"] = data["tls.crt"], data["tls.key"] },
		"secret bytes as certificate": func(data map[string][]byte) { data["tls.crt"] = []byte("sensitive-fixture-material") },
		"missing CA":                  func(data map[string][]byte) { delete(data, "ca.crt") },
	} {
		t.Run(name, func(t *testing.T) {
			data := maps.Clone(valid)
			change(data)
			encoded, _, err := alServingCertificate(data, now, alCertificateLifetime)
			if err == nil || len(encoded) != 0 {
				t.Fatal("issued a serving certificate under an invalid identity")
			}
			if strings.Contains(err.Error(), "sensitive-fixture-material") || strings.Contains(err.Error(), "-----BEGIN") {
				t.Fatal("the error exposed input material")
			}
		})
	}
	for _, date := range []time.Time{now.Add(-time.Hour), now.Add(72 * time.Hour), now.Add(48 * time.Hour)} {
		if _, _, err := alServingCertificate(valid, date, alCertificateLifetime); err == nil {
			t.Fatal("issued a certificate outside the installed authority or leaf's validity")
		}
	}
}

func TestAlCertificateWarningUsesTheFrozenDay(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	data := alCertificateFixture(t, now)
	encoded, expiry, err := alServingCertificate(data, now, alCertificateWarning+alCertificateLifetime)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := firstCertificate(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !expiry.Equal(now.Add(24*time.Hour + alCertificateLifetime)) {
		t.Fatal("the warning fixture shortened the frozen 24-hour threshold")
	}
	threshold := leaf.NotAfter.Add(-24 * time.Hour)
	if threshold.Sub(now) <= alCertificateProjection || threshold.Sub(now) != alCertificateLifetime {
		t.Fatal("the signed warning threshold leaves no time to observe every manager before it crosses")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data["ca.crt"]) {
		t.Fatal("the fixture authority is unreadable")
	}
	for _, at := range []time.Time{threshold.Add(-time.Second), threshold.Add(time.Second)} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: leaf.DNSNames[0], CurrentTime: at}); err != nil {
			t.Fatal("the warning threshold introduced an admission certificate fault")
		}
	}
	for _, lifetime := range []time.Duration{0, -time.Second, 49 * time.Hour, 73 * time.Hour} {
		if value, _, err := alServingCertificate(data, now, lifetime); err == nil || len(value) != 0 {
			t.Fatal("issued an invalid or unbounded serving certificate")
		}
	}
}

func TestAlCertificateMetricsRequireEveryManager(t *testing.T) {
	t.Parallel()
	expiry := time.Unix(1790700000, 0)
	pods := []string{"leader", "follower"}
	sample := func(pod string) map[string]any {
		return map[string]any{"metric": map[string]string{"__name__": alCertificateMetric, "job": alScrapeJob, "pod": pod},
			"value": []any{1790699800, strconv.FormatInt(expiry.Unix(), 10)}}
	}
	answer := func() map[string]any {
		return map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": []any{sample("leader"), sample("follower")}}}
	}
	encode := func(value any) []byte {
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	if !alCertificateExpiries(encode(answer()), pods, expiry) {
		t.Fatal("refused both managers' matching expiry metrics")
	}
	for name, edit := range map[string]func(map[string]any){
		"failed query":    func(a map[string]any) { a["status"] = "error" },
		"non-vector":      func(a map[string]any) { a["data"].(map[string]any)["resultType"] = "scalar" },
		"missing manager": func(a map[string]any) { a["data"].(map[string]any)["result"] = []any{sample("leader")} },
		"empty":           func(a map[string]any) { a["data"].(map[string]any)["result"] = []any{} },
		"duplicate leader": func(a map[string]any) {
			a["data"].(map[string]any)["result"] = []any{sample("leader"), sample("leader")}
		},
		"old Pod": func(a map[string]any) {
			a["data"].(map[string]any)["result"] = []any{sample("leader"), sample("old-follower")}
		},
		"wrong expiry": func(a map[string]any) {
			a["data"].(map[string]any)["result"].([]any)[0].(map[string]any)["value"] = []any{1790699800, "1790699999"}
		},
		"wrong job": func(a map[string]any) {
			a["data"].(map[string]any)["result"].([]any)[0].(map[string]any)["metric"].(map[string]string)["job"] = "other"
		},
		"wrong metric": func(a map[string]any) {
			a["data"].(map[string]any)["result"].([]any)[0].(map[string]any)["metric"].(map[string]string)["__name__"] = "other"
		},
		"incomplete sample": func(a map[string]any) {
			a["data"].(map[string]any)["result"].([]any)[0].(map[string]any)["value"] = []any{}
		},
	} {
		t.Run(name, func(t *testing.T) {
			reading := answer()
			edit(reading)
			if alCertificateExpiries(encode(reading), pods, expiry) {
				t.Fatal("accepted incomplete or mismatched certificate evidence")
			}
		})
	}
	for _, names := range [][]string{nil, {"leader", "leader"}, {"leader", ""}, slices.Concat(pods, []string{"extra"})} {
		if alCertificateExpiries(encode(answer()), names, expiry) {
			t.Fatal("accepted invalid expected manager identities")
		}
	}
	if alCertificateExpiries([]byte("not JSON"), pods, expiry) || alCertificateExpiries(encode(answer()), pods, time.Time{}) {
		t.Fatal("accepted missing expiry evidence")
	}
}

func TestAlCertificateAdmissionNeedsAnExpiryRefusal(t *testing.T) {
	t.Parallel()
	message := fmt.Sprintf(`Internal error occurred: failed calling webhook %q: failed to verify certificate: x509: certificate has expired or is not yet valid: current time 2026-09-29T10:05:01Z is after 2026-09-29T10:05:00Z`, alApprovalWebhook)
	if !alExpiredApprovalError(errors.New(message)) {
		t.Fatal("refused the admission expiry error")
	}
	for _, err := range []error{nil, errors.New("connection refused"),
		errors.New(strings.Replace(message, " is after ", " is before ", 1)),
		errors.New(strings.Replace(message, alApprovalWebhook, "other.webhook", 1)),
		errors.New(strings.Replace(message, "x509: certificate has expired or is not yet valid", "x509: certificate signed by unknown authority", 1)),
	} {
		if alExpiredApprovalError(err) {
			t.Fatal("accepted an unrelated admission error as expiry")
		}
	}
}

func TestAlCertificateProbeUsesTheChartUpdateWebhook(t *testing.T) {
	t.Parallel()
	rendered := alHelm(t, "template", "ptah-operator", alChart, "--namespace", "ptah-system",
		"--set-string", "image.digest=sha256:"+strings.Repeat("a", 64),
		"--set-string", "execution.runnerImage=ghcr.io/stokaro/ptah-operator@sha256:"+strings.Repeat("a", 64),
		"--set-string", "execution.executorImage=ghcr.io/stokaro/ptah@sha256:"+strings.Repeat("b", 64),
		"--set-string", "execution.ptahVersion=v0.7.0",
		"--show-only", "templates/webhook.yaml")
	decoder := yamlutil.NewYAMLOrJSONDecoder(strings.NewReader(rendered), 4096)
	var selected *admissionregistrationv1.ValidatingWebhook
	for {
		var configuration admissionregistrationv1.ValidatingWebhookConfiguration
		err := decoder.Decode(&configuration)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if configuration.Kind != "ValidatingWebhookConfiguration" {
			continue
		}
		for _, hook := range configuration.Webhooks {
			if hook.Name != alApprovalWebhook {
				continue
			}
			if selected != nil || !alApprovalProbeWebhook(hook) {
				t.Fatal("the chart does not have one fail-closed approval UPDATE webhook for the probe")
			}
			selected = hook.DeepCopy()
		}
	}
	if selected == nil {
		t.Fatal("the chart does not install the webhook the expiry probe requires")
	}
	for name, mutate := range map[string]func(*admissionregistrationv1.ValidatingWebhook){
		"CREATE only": func(h *admissionregistrationv1.ValidatingWebhook) {
			h.Rules[0].Operations = []admissionregistrationv1.OperationType{admissionregistrationv1.Create}
		},
		"wrong resource": func(h *admissionregistrationv1.ValidatingWebhook) { h.Rules[0].Resources = []string{"ptahschemas"} },
		"wrong group":    func(h *admissionregistrationv1.ValidatingWebhook) { h.Rules[0].APIGroups = []string{"another.group"} },
		"wrong version":  func(h *admissionregistrationv1.ValidatingWebhook) { h.Rules[0].APIVersions = []string{"v1beta1"} },
		"wrong webhook":  func(h *admissionregistrationv1.ValidatingWebhook) { h.Name = "mapproval.operator.ptah.run" },
		"cluster scope": func(h *admissionregistrationv1.ValidatingWebhook) {
			scope := admissionregistrationv1.ClusterScope
			h.Rules[0].Scope = &scope
		},
		"fails open": func(h *admissionregistrationv1.ValidatingWebhook) { *h.FailurePolicy = admissionregistrationv1.Ignore },
		"no rules":   func(h *admissionregistrationv1.ValidatingWebhook) { h.Rules = nil },
		"namespace narrowed": func(h *admissionregistrationv1.ValidatingWebhook) {
			h.NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"outside": "probe"}}
		},
		"object narrowed": func(h *admissionregistrationv1.ValidatingWebhook) {
			h.ObjectSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"outside": "probe"}}
		},
		"conditional": func(h *admissionregistrationv1.ValidatingWebhook) {
			h.MatchConditions = []admissionregistrationv1.MatchCondition{{Name: "never", Expression: "false"}}
		},
		"dry run unsupported": func(h *admissionregistrationv1.ValidatingWebhook) {
			*h.SideEffects = admissionregistrationv1.SideEffectClassSome
		},
	} {
		t.Run(name, func(t *testing.T) {
			hook := selected.DeepCopy()
			mutate(hook)
			if alApprovalProbeWebhook(*hook) {
				t.Fatal("accepted a webhook that cannot prove the dry-run UPDATE refusal")
			}
		})
	}
}
