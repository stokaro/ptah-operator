package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	recordapi "github.com/stokaro/ptah-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"github.com/stokaro/ptah-operator/internal/runner"
)

type deliveryAPI struct {
	client.Client
	serial atomic.Int64
}

func (c *deliveryAPI) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	obj.SetUID(types.UID(fmt.Sprintf("secret-%d", c.serial.Add(1))))
	return c.Client.Create(ctx, obj, opts...)
}

type deliveryFixture struct {
	identity          resultdelivery.Identity
	credentials       string
	serverCertificate tls.Certificate
	roots             *x509.CertPool
	store             resultstore.Store
}

func newDeliveryFixture(t *testing.T, configureCertificate func(*x509.Certificate)) *deliveryFixture {
	t.Helper()
	identity := resultdelivery.Identity{Binding: resultstore.Binding{Namespace: "tenant", Kind: "PtahMigration", Name: "migration", UID: "migration-uid", Generation: 1, ExecutionBindingID: "v1-" + strings.Repeat("a", 32), InputFingerprint: "sha256:" + strings.Repeat("b", 64), Operation: "migration-apply", OperationID: "sha256:" + strings.Repeat("c", 64), JobName: "apply-job", JobUID: "job-uid", PodName: "apply-job-abcde", PodUID: "pod-uid"}, Engine: "postgresql"}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "delivery test CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	uri, err := resultdelivery.CertificateURI(identity)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(serial int64, usage x509.ExtKeyUsage, uris []*url.URL) tls.Certificate {
		private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: root.NotBefore, NotAfter: root.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}, URIs: uris, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
		if usage == x509.ExtKeyUsageClientAuth && configureCertificate != nil {
			configureCertificate(leaf)
		}
		encoded, err := x509.CreateCertificate(rand.Reader, leaf, ca, &private.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{encoded}, PrivateKey: private}
	}
	certificate := issue(3, x509.ExtKeyUsageClientAuth, []*url.URL{uri})
	private, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	for name, content := range map[string][]byte{"ca.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), "tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}), "tls.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})} {
		if err := os.WriteFile(filepath.Join(directory, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	scheme := runtime.NewScheme()
	if err := recordapi.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	api := &deliveryAPI{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	return &deliveryFixture{identity: identity, credentials: directory, roots: roots, serverCertificate: issue(2, x509.ExtKeyUsageServerAuth, nil), store: resultstore.Store{Client: api, Reader: api}}
}

func (f *deliveryFixture) environment(t *testing.T) []string {
	return append(migrationApplyEnvironment(t, f.identity.Binding.OperationID, 30*time.Second), jobconfig.Generation+"=1", jobconfig.PodNamespace+"="+f.identity.Binding.Namespace, jobconfig.PodName+"="+f.identity.Binding.PodName, jobconfig.PodUID+"="+string(f.identity.Binding.PodUID))
}

func (f *deliveryFixture) server(t *testing.T, wrap func(http.Handler) http.Handler) *httptest.Server {
	t.Helper()
	receiver, err := resultdelivery.NewReceiver(resultdelivery.ReceiverConfig{Store: f.store, Authorize: func(_ context.Context, i resultdelivery.Identity) error {
		if i != f.identity {
			return resultdelivery.ErrAuthority
		}
		return nil
	}, MaxConcurrent: 1, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	server, err := receiver.Server(f.serverCertificate, f.roots)
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler
	if wrap != nil {
		handler = wrap(handler)
	}
	local := httptest.NewUnstartedServer(handler)
	local.TLS = server.TLSConfig
	local.StartTLS()
	t.Cleanup(local.Close)
	return local
}

func applyExecutable(t *testing.T) (string, string) {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "ptah")
	counter := filepath.Join(directory, "invocations")
	// A real child process with a counted invocation and a valid native Apply
	// report. This exercises command dispatch, not a database transaction.
	script := `#!/bin/sh
printf 'run\n' >> "$PTAH_TEST_INVOCATIONS"
printf '%s\n' '{"contract_version":1,"direction":"up","outcome":"applied","planned":[3,4],"applied":[3,4],"status":{"contract_version":1,"current_version":4,"total_migrations":4,"has_pending_changes":false}}'
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path, counter
}

// Preserve the real socket's deadlines while withholding only its response.
type withheldResponse struct {
	*httptest.ResponseRecorder
	underlying http.ResponseWriter
}

func (w withheldResponse) Unwrap() http.ResponseWriter { return w.underlying }

func TestRunnerRedeliversAfterLostAcknowledgmentWithoutReexecuting(t *testing.T) {
	f := newDeliveryFixture(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var attempts atomic.Int64
	var mu sync.Mutex
	var first []byte
	server := f.server(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				next.ServeHTTP(w, r)
				return
			}
			payload, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(payload))
			mu.Lock()
			if first == nil {
				first = bytes.Clone(payload)
			} else if !bytes.Equal(first, payload) {
				t.Error("retry changed the executed result")
			}
			mu.Unlock()
			recorded := httptest.NewRecorder()
			next.ServeHTTP(withheldResponse{recorded, w}, r)
			if recorded.Code != http.StatusOK {
				t.Errorf("receiver returned %d: %s", recorded.Code, recorded.Body.String())
			}
			if attempts.Add(1) == 1 {
				cancel()
				panic(http.ErrAbortHandler)
			}
			for key, values := range recorded.Header() {
				w.Header()[key] = values
			}
			w.WriteHeader(recorded.Code)
			_, _ = w.Write(recorded.Body.Bytes())
		})
	})
	executable, counter := applyExecutable(t)
	terminationLog := filepath.Join(t.TempDir(), "termination-log")
	if err := os.WriteFile(terminationLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"--ptah-binary", executable, "--operation", "migration-apply", "--result-endpoint", server.URL, "--result-credentials", f.credentials}, &stdout, &stderr, append(f.environment(t), "PTAH_TEST_INVOCATIONS="+counter), terminationLog)
	if code != 0 || stdout.Len() != 0 || attempts.Load() != 2 {
		t.Fatalf("exit=%d stdout=%q stderr=%q attempts=%d", code, stdout.String(), stderr.String(), attempts.Load())
	}
	invocations, err := os.ReadFile(counter)
	if err != nil || string(invocations) != "run\n" {
		t.Fatalf("Apply dispatches %q, %v", invocations, err)
	}
	payload, receipt, err := f.store.Load(t.Context(), f.identity.Binding)
	if err != nil {
		t.Fatal(err)
	}
	result, err := resultdelivery.Decode(f.identity, payload)
	if err != nil || !result.MutationStarted || result.Uncertain || result.Error != nil || result.MigrationRun == nil || len(result.MigrationRun.Applied) != 2 {
		t.Fatalf("persisted result does not prove successful Apply: %#v, %v", result, err)
	}
	summaryBytes, err := os.ReadFile(terminationLog)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := runner.ParseSummaryFor(string(summaryBytes), runner.OperationMigrationApply, f.identity.Binding.OperationID)
	if err != nil || summary.FrameDigest != receipt.Digest || !summary.MutationStarted {
		t.Fatalf("summary does not bind stored outcome: %#v, %v", summary, err)
	}
}

func TestRunnerDoesNotFallBackToLogsAfterDeliveryRefusal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		attempts int64
		reason   string
	}{
		{"authority refusal", http.StatusForbidden, 1, "result receiver returned HTTP 403"},
		{"busy receiver", http.StatusServiceUnavailable, 4, "result delivery attempts exhausted: result receiver returned HTTP 503"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeliveryFixture(t, nil)
			var attempts atomic.Int64
			server := f.server(t, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodHead {
						next.ServeHTTP(w, r)
						return
					}
					attempts.Add(1)
					http.Error(w, "private receiver response", tc.status)
				})
			})
			executable, counter := applyExecutable(t)
			terminationLog := filepath.Join(t.TempDir(), "termination-log")
			if err := os.WriteFile(terminationLog, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), []string{"--ptah-binary", executable, "--operation", "migration-apply", "--result-endpoint", server.URL, "--result-credentials", f.credentials}, &stdout, &stderr, append(f.environment(t), "PTAH_TEST_INVOCATIONS="+counter), terminationLog)
			if code != 2 || stdout.Len() != 0 || attempts.Load() != tc.attempts || stderr.String() != "ptah-runner: durable result delivery failed: "+tc.reason+"\n" {
				t.Fatalf("exit=%d stdout=%q stderr=%q attempts=%d", code, stdout.String(), stderr.String(), attempts.Load())
			}
			invocations, err := os.ReadFile(counter)
			if err != nil || string(invocations) != "run\n" {
				t.Fatalf("Apply dispatches %q, %v", invocations, err)
			}
			summaryBytes, err := os.ReadFile(terminationLog)
			if err != nil {
				t.Fatal(err)
			}
			summary, err := runner.ParseSummaryFor(string(summaryBytes), runner.OperationMigrationApply, f.identity.Binding.OperationID)
			if err != nil || !summary.MutationStarted {
				t.Fatalf("delivery refusal lost the execution summary: %#v, %v", summary, err)
			}
			if _, _, err := f.store.Load(t.Context(), f.identity.Binding); err == nil {
				t.Fatal("refused delivery unexpectedly persisted a result")
			}
		})
	}
}

func TestRunnerRefusesDurableProtocolMismatchBeforeDelivery(t *testing.T) {
	f := newDeliveryFixture(t, nil)
	var requests atomic.Int64
	server := f.server(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			next.ServeHTTP(w, r)
		})
	})
	executable, counter := applyExecutable(t)
	env := f.environment(t)
	for i, value := range env {
		if strings.HasPrefix(value, runner.EnvRunnerProtocolVersion+"=") {
			env[i] = fmt.Sprintf("%s=%d", runner.EnvRunnerProtocolVersion, runner.ProtocolVersion+1)
		}
	}
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"--ptah-binary", executable, "--operation", "migration-apply", "--result-endpoint", server.URL, "--result-credentials", f.credentials}, &stdout, &stderr, append(env, "PTAH_TEST_INVOCATIONS="+counter), "")
	want := fmt.Sprintf("ptah-runner: runner_protocol_mismatch: the Job expects runner protocol %d; this runner speaks protocol %d\n", runner.ProtocolVersion+1, runner.ProtocolVersion)
	if code != 2 || stdout.Len() != 0 || stderr.String() != want || requests.Load() != 0 {
		t.Errorf("exit=%d stdout=%q stderr=%q receiver requests=%d", code, stdout.String(), stderr.String(), requests.Load())
	}
	if _, err := os.Stat(counter); !os.IsNotExist(err) {
		t.Fatalf("executor started under a foreign protocol: %v", err)
	}
	if _, _, err := f.store.Load(t.Context(), f.identity.Binding); err == nil {
		t.Fatal("foreign protocol unexpectedly persisted a result")
	}
}

func TestRunnerRefusesDeliveryMisconfigurationBeforeDispatch(t *testing.T) {
	for _, name := range []string{"empty endpoint", "missing directory", "HTTP endpoint", "missing key", "key is a directory", "oversized CA", "Pod UID", "generation", "operation ID", "duplicate identity", "engine", "expired certificate", "server certificate"} {
		t.Run(name, func(t *testing.T) {
			f := newDeliveryFixture(t, func(cert *x509.Certificate) {
				if name == "expired certificate" {
					cert.NotBefore = time.Now().Add(-2 * time.Hour)
					cert.NotAfter = time.Now().Add(-time.Hour)
				}
				if name == "server certificate" {
					cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
				}
			})
			env := f.environment(t)
			endpoint := "https://receiver.invalid"
			directory := f.credentials
			switch name {
			case "empty endpoint":
				endpoint = ""
			case "missing directory":
				directory = ""
			case "HTTP endpoint":
				endpoint = "http://receiver.invalid"
			case "missing key":
				if err := os.Remove(filepath.Join(directory, "tls.key")); err != nil {
					t.Fatal(err)
				}
			case "key is a directory":
				if err := os.Remove(filepath.Join(directory, "tls.key")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(directory, "tls.key"), 0o700); err != nil {
					t.Fatal(err)
				}
			case "oversized CA":
				if err := os.WriteFile(filepath.Join(directory, "ca.crt"), bytes.Repeat([]byte("x"), 65537), 0o600); err != nil {
					t.Fatal(err)
				}
			case "Pod UID":
				for i, v := range env {
					if strings.HasPrefix(v, jobconfig.PodUID+"=") {
						env[i] = jobconfig.PodUID + "=other"
					}
				}
			case "generation":
				for i, v := range env {
					if strings.HasPrefix(v, jobconfig.Generation+"=") {
						env[i] = jobconfig.Generation + "=2"
					}
				}
			case "operation ID":
				for i, v := range env {
					if strings.HasPrefix(v, runner.EnvOperationID+"=") {
						env[i] = runner.EnvOperationID + "=other"
					}
				}
			case "duplicate identity":
				env = append(env, jobconfig.PodUID+"="+string(f.identity.Binding.PodUID))
			case "engine":
				for i, v := range env {
					if strings.HasPrefix(v, runner.EnvExpectedDatabaseEngine+"=") {
						env[i] = runner.EnvExpectedDatabaseEngine + "=MySQL"
					}
				}
			}
			executable, counter := applyExecutable(t)
			env = append(env, "PTAH_TEST_INVOCATIONS="+counter)
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), []string{"--ptah-binary", executable, "--operation", "migration-apply", "--result-endpoint", endpoint, "--result-credentials", directory}, &stdout, &stderr, env, "")
			if code != 2 || stdout.Len() != 0 || stderr.String() != "ptah-runner: invalid result delivery configuration\n" {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if _, err := os.Stat(counter); !os.IsNotExist(err) {
				t.Fatalf("executor started with invalid delivery configuration: %v", err)
			}
		})
	}
}

func TestRunnerRetriesPreflightBeforeExecutingApplyOnce(t *testing.T) {
	f := newDeliveryFixture(t, nil)
	executable, counter := applyExecutable(t)
	var checks atomic.Int32
	server := f.server(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				if _, err := os.Stat(counter); !os.IsNotExist(err) {
					t.Errorf("Apply started before successful authentication: %v", err)
				}
				if checks.Add(1) == 1 {
					http.Error(w, "busy", http.StatusServiceUnavailable)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"--ptah-binary", executable, "--operation", "migration-apply", "--result-endpoint", server.URL, "--result-credentials", f.credentials}, &stdout, &stderr, append(f.environment(t), "PTAH_TEST_INVOCATIONS="+counter), "")
	if code != 0 || stdout.Len() != 0 || checks.Load() != 2 {
		t.Fatalf("exit=%d stdout=%q stderr=%q preflights=%d", code, stdout.String(), stderr.String(), checks.Load())
	}
	invocations, err := os.ReadFile(counter)
	if err != nil || string(invocations) != "run\n" {
		t.Fatalf("Apply dispatches %q, %v", invocations, err)
	}
	payload, _, err := f.store.Load(t.Context(), f.identity.Binding)
	if err != nil {
		t.Fatal(err)
	}
	result, err := resultdelivery.Decode(f.identity, payload)
	if err != nil || !result.MutationStarted || result.Uncertain || result.Error != nil {
		t.Fatalf("the one Apply did not persist a successful result: %#v, %v", result, err)
	}
}

func TestRunnerAuthenticatesProjectionBeforeStartingSQL(t *testing.T) {
	for _, name := range []string{"foreign client key", "foreign server trust", "authority refused", "receiver unavailable"} {
		t.Run(name, func(t *testing.T) {
			f := newDeliveryFixture(t, nil)
			server := f.server(t, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if name == "authority refused" {
						http.Error(w, "private receiver refusal", http.StatusForbidden)
						return
					}
					if name == "receiver unavailable" {
						http.Error(w, "private receiver failure", http.StatusServiceUnavailable)
						return
					}
					next.ServeHTTP(w, r)
				})
			})
			if strings.HasPrefix(name, "foreign") {
				foreign := newDeliveryFixture(t, nil)
				files := []string{"tls.crt", "tls.key"}
				if name == "foreign server trust" {
					files = []string{"ca.crt"}
				}
				for _, file := range files {
					data, err := os.ReadFile(filepath.Join(foreign.credentials, file))
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(f.credentials, file), data, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			executable, counter := applyExecutable(t)
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), []string{"--ptah-binary", executable, "--operation", "migration-apply", "--result-endpoint", server.URL, "--result-credentials", f.credentials}, &stdout, &stderr, append(f.environment(t), "PTAH_TEST_INVOCATIONS="+counter), "")
			reason := "result receiver authentication failed"
			if name == "authority refused" {
				reason = "result receiver preflight returned HTTP 403"
			} else if name == "receiver unavailable" {
				reason = "result receiver preflight returned HTTP 503"
			}
			if code != 2 || stdout.Len() != 0 || stderr.String() != "ptah-runner: result receiver preflight failed: "+reason+"\n" {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if _, err := os.Stat(counter); !os.IsNotExist(err) {
				t.Fatalf("SQL started despite failed receiver authentication: %v", err)
			}
			if _, _, err := f.store.Load(t.Context(), f.identity.Binding); err == nil {
				t.Fatal("preflight published a result")
			}
		})
	}
}
