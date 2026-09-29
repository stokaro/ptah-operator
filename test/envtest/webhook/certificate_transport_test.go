package webhook_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The live certificate-expiry proof changes only CA PEM whitespace to make
// admission open a new TLS connection. An established connection keeps working
// after its serving certificate expires. Hold that assumption to a real API
// server: changing the served leaf alone leaves its cached connection working;
// changing the bundle makes expiry visible, and a valid leaf restores admission.
func TestCABundleRefreshesAdmissionTLS(t *testing.T) {
	plane.Require(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	ca, valid, expired := transportCertificates(t)
	var leaf atomic.Pointer[tls.Certificate]
	leaf.Store(valid)
	var calls atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var review admissionv1.AdmissionReview
		if err := json.NewDecoder(r.Body).Decode(&review); err != nil || review.Request == nil {
			http.Error(w, "invalid admission review", http.StatusBadRequest)
			return
		}
		calls.Add(1)
		review.Response = &admissionv1.AdmissionResponse{UID: review.Request.UID, Allowed: true}
		review.Request = nil
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(review); err != nil {
			t.Errorf("write admission response: %v", err)
		}
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{*leaf.Load()}}, nil
	}}
	server.StartTLS()
	t.Cleanup(server.Close)

	namespace := newNamespace(t, "certificate-transport")
	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "probe"}}
	if err := admin.Create(ctx, configMap); err != nil {
		t.Fatal(err)
	}
	const hookName = "certificate-transport.operator.ptah.run"
	configuration := &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "certificate-transport-"},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{{
			Name: hookName, AdmissionReviewVersions: []string{"v1"},
			ClientConfig:  admissionregistrationv1.WebhookClientConfig{URL: &server.URL, CABundle: ca},
			FailurePolicy: ptr.To(admissionregistrationv1.Fail), SideEffects: ptr.To(admissionregistrationv1.SideEffectClassNone),
			TimeoutSeconds:    ptr.To[int32](5),
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": namespace}},
			Rules: []admissionregistrationv1.RuleWithOperations{{
				Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
				Rule: admissionregistrationv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"},
					Resources: []string{"configmaps"}, Scope: ptr.To(admissionregistrationv1.NamespacedScope)},
			}},
		}},
	}
	if err := admin.Create(ctx, configuration); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := admin.Delete(cleanup, configuration, client.Preconditions{UID: &configuration.UID}); err != nil {
			t.Errorf("remove transport probe webhook: %v", err)
		}
	})
	probe := func() error {
		return admin.Patch(ctx, configMap, client.RawPatch(types.MergePatchType,
			[]byte(`{"metadata":{"annotations":{"probe":"true"}}}`)), client.DryRunAll)
	}
	await := func(what string, check func(error) bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		var err error
		for time.Now().Before(deadline) && ctx.Err() == nil {
			err = probe()
			if check(err) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%s: last admission result: %v", what, err)
	}
	await("the initial valid TLS connection", func(err error) bool { return err == nil && calls.Load() > 0 })
	before := calls.Load()
	leaf.Store(expired)
	if err := probe(); err != nil || calls.Load() <= before {
		t.Fatalf("the established connection did not remain usable after replacing its leaf: %v", err)
	}
	configuration.Webhooks[0].ClientConfig.CABundle = append(bytes.Clone(ca), '\n')
	if err := admin.Update(ctx, configuration); err != nil {
		t.Fatal(err)
	}
	await("the refreshed connection to refuse expiry", func(err error) bool {
		return err != nil && strings.Contains(err.Error(), hookName) &&
			strings.Contains(err.Error(), "x509: certificate has expired") && strings.Contains(err.Error(), " is after ")
	})
	before = calls.Load()
	leaf.Store(valid)
	configuration.Webhooks[0].ClientConfig.CABundle = append(bytes.Clone(ca), '\n', '\n')
	if err := admin.Update(ctx, configuration); err != nil {
		t.Fatal(err)
	}
	await("admission to recover on the valid certificate", func(err error) bool { return err == nil && calls.Load() > before })
	t.Log("cached admission remained usable; a CA bundle refresh exposed expiry and then restored admission")
}

func transportCertificates(t *testing.T) ([]byte, *tls.Certificate, *tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	servingKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true,
		NotBefore: now.Add(-2 * time.Hour), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(serial int64, until time.Time) *tls.Certificate {
		certificate := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: now.Add(-time.Hour), NotAfter: until,
			IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			KeyUsage: x509.KeyUsageDigitalSignature}
		leaf, err := x509.CreateCertificate(rand.Reader, certificate, ca, &servingKey.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		return &tls.Certificate{Certificate: [][]byte{leaf}, PrivateKey: servingKey}
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), issue(2, now.Add(time.Hour)), issue(3, now.Add(-time.Minute))
}
