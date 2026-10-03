package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/runner"
)

func (f *deliveryFixture) tokenEnvironment(t *testing.T) []string {
	t.Helper()
	identity := f.identity
	identity.Binding.JobUID, identity.Binding.PodName, identity.Binding.PodUID = "", "", ""
	data, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	trust, err := os.ReadFile(filepath.Join(f.credentials, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	return append(f.environment(t), jobconfig.JobUID+"="+string(f.identity.Binding.JobUID), jobconfig.ServerTrust+"="+string(trust), jobconfig.IdentityTemplate+"="+string(data))
}

func TestRunnerPodTokenRotationRedeliversWithoutReexecuting(t *testing.T) {
	f := newDeliveryFixture(t, nil)
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("first.bound.token"), 0600); err != nil {
		t.Fatal(err)
	}
	var reviews, attempts atomic.Int64
	receiver, err := resultdelivery.NewReceiver(resultdelivery.ReceiverConfig{
		Store: f.store, MaxConcurrent: 1, Timeout: 10 * time.Second,
		AuthenticateToken: func(_ context.Context, token string, identity resultdelivery.Identity) error {
			reviews.Add(1)
			want := "first.bound.token"
			if attempts.Load() > 0 {
				want = "rotated.bound.token"
			}
			if token != want || identity != f.identity {
				return resultdelivery.ErrAuthority
			}
			return nil
		},
		Authorize: func(_ context.Context, identity resultdelivery.Identity) error {
			if identity != f.identity {
				return resultdelivery.ErrAuthority
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := receiver.Server(f.serverCertificate, nil)
	if err != nil {
		t.Fatal(err)
	}
	var first []byte
	local := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			server.Handler.ServeHTTP(w, r)
			return
		}
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if first == nil {
			first = bytes.Clone(payload)
		} else if !bytes.Equal(first, payload) {
			t.Error("retry changed the completed result")
		}
		r.Body = io.NopCloser(bytes.NewReader(payload))
		recorded := httptest.NewRecorder()
		server.Handler.ServeHTTP(withheldResponse{recorded, w}, r)
		if recorded.Code != http.StatusOK {
			t.Errorf("receiver returned %d", recorded.Code)
		}
		if attempts.Add(1) == 1 {
			// Atomic replacement follows the kubelet projection's file lifecycle.
			if err := os.WriteFile(path+".next", []byte("rotated.bound.token"), 0600); err != nil {
				t.Error(err)
			}
			if err := os.Rename(path+".next", path); err != nil {
				t.Error(err)
			}
			panic(http.ErrAbortHandler)
		}
		for key, values := range recorded.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(recorded.Code)
		_, _ = w.Write(recorded.Body.Bytes())
	}))
	local.TLS = server.TLSConfig
	local.StartTLS()
	defer local.Close()
	executable, counter := applyExecutable(t)
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"--ptah-binary", executable, "--operation", "migration-apply", "--result-endpoint", local.URL, "--result-token", path}, &stdout, &stderr, append(f.tokenEnvironment(t), "PTAH_TEST_INVOCATIONS="+counter), "")
	if code != 0 || stdout.Len() != 0 || attempts.Load() != 2 || reviews.Load() < 5 {
		t.Fatalf("exit=%d stdout=%q stderr=%q attempts=%d reviews=%d", code, stdout.String(), stderr.String(), attempts.Load(), reviews.Load())
	}
	invocations, err := os.ReadFile(counter)
	if err != nil || string(invocations) != "run\n" {
		t.Fatalf("Apply dispatches %q: %v", invocations, err)
	}
	payload, _, err := f.store.Load(t.Context(), f.identity.Binding)
	if err != nil {
		t.Fatal(err)
	}
	result, err := resultdelivery.Decode(f.identity, payload)
	if err != nil || result.MigrationRun == nil || len(result.MigrationRun.Applied) != 2 || result.Uncertain {
		t.Fatalf("wrong durable result: %v", err)
	}
}

func TestRunnerPodTokenRefusesInvalidIdentityBeforeDispatch(t *testing.T) {
	f := newDeliveryFixture(t, nil)
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("bound.token.value"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, extra := range []string{jobconfig.PodUID + "=other-pod", jobconfig.JobUID + "=other-job", jobconfig.IdentityTemplate + "={}", jobconfig.ServerTrust + "=invalid"} {
		if delivery, err := prepareTokenDelivery("https://127.0.0.1", path, runner.OperationMigrationApply, append(f.tokenEnvironment(t), extra)); err == nil {
			delivery.sender.Close()
			t.Fatal("ambiguous authority variables were accepted")
		}
	}
}
