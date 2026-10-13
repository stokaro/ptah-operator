package main

import (
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

func TestCapacityWritesReleaseConnectionsWithoutChangingRequests(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		name := "HTTP1"
		if http2 {
			name = "HTTP2"
		}
		t.Run(name, func(t *testing.T) {
			type received struct {
				address, method, path, query, authorization, body string
				protocol                                          int
			}
			requests := make(chan received, 16)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				requests <- received{r.RemoteAddr, r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), string(body), r.ProtoMajor}
				_, _ = io.WriteString(w, "ok")
			}))
			server.EnableHTTP2 = http2
			server.StartTLS()
			defer server.Close()
			config := &rest.Config{Host: server.URL, BearerToken: "fixture-actor", TLSClientConfig: rest.TLSClientConfig{
				CAData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}),
			}}
			distributeCapacityWrites(config)
			client, err := rest.HTTPClientFor(config)
			if err != nil {
				t.Fatal(err)
			}
			send := func(method, path string) received {
				t.Helper()
				request, err := http.NewRequestWithContext(t.Context(), method, server.URL+path+"?fieldManager=capacity", strings.NewReader("fixture-body"))
				if err != nil {
					t.Fatal(err)
				}
				response, err := client.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				_, readErr := io.Copy(io.Discard, response.Body)
				closeErr := response.Body.Close()
				if readErr != nil || closeErr != nil || response.StatusCode != http.StatusOK {
					t.Fatal("request did not complete", response.StatusCode, readErr, closeErr)
				}
				if request.Close {
					t.Fatal("transport changed the caller's request")
				}
				got := <-requests
				if got.method != method || got.path != path || got.query != "fieldManager=capacity" || got.authorization != "Bearer fixture-actor" || got.body != "fixture-body" {
					t.Fatal("transport changed request content or actor")
				}
				if (got.protocol == 2) != http2 {
					t.Fatalf("expected %s, negotiated HTTP/%d", name, got.protocol)
				}
				return got
			}
			path := "/apis/operator.ptah.run/v1alpha1/namespaces/work/ptahschemas/schema"
			read := send(http.MethodGet, path)
			if again := send(http.MethodGet, path); again.address != read.address {
				t.Fatal("read-only traffic lost connection reuse")
			}
			if other := send(http.MethodPost, "/api/v1/namespaces/work/pods/pod/portforward"); other.address != read.address {
				t.Fatal("non-Ptah requests lost connection reuse")
			}
			previous := ""
			for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
				got := send(method, path)
				if got.address == previous {
					t.Fatalf("%s reused the previous workload write's connection", method)
				}
				previous = got.address
			}
			if len(requests) != 0 {
				t.Fatal("transport added a request")
			}
		})
	}
}
