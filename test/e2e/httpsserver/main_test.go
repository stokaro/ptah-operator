package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T) (*server, *httptest.Server) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "schema.hcl"), []byte("table {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &server{root: root, user: "deploy", password: "secret-password", token: "secret-token", otherOrigin: "https://other.example"}
	httpServer := httptest.NewServer(s)
	t.Cleanup(httpServer.Close)
	return s, httpServer
}

func TestTheServerAnswersEachAccessByItsRule(t *testing.T) {
	t.Parallel()
	_, httpServer := fixture(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for name, row := range map[string]struct {
		path, user, password, token string
		status                      int
		location                    string
	}{
		"a public file":                  {path: "/public/schema.hcl", status: http.StatusOK},
		"Basic with the password":        {path: "/basic/schema.hcl", user: "deploy", password: "secret-password", status: http.StatusOK},
		"Basic with another password":    {path: "/basic/schema.hcl", user: "deploy", password: "other", status: http.StatusUnauthorized},
		"Basic with no credential":       {path: "/basic/schema.hcl", status: http.StatusUnauthorized},
		"Bearer with the token":          {path: "/bearer/schema.hcl", token: "secret-token", status: http.StatusOK},
		"Bearer with another token":      {path: "/bearer/schema.hcl", token: "other", status: http.StatusUnauthorized},
		"a file that is not there":       {path: "/public/other.hcl", status: http.StatusNotFound},
		"a nested name":                  {path: "/public/a/schema.hcl", status: http.StatusNotFound},
		"an unknown access":              {path: "/private/schema.hcl", status: http.StatusNotFound},
		"a redirect on the same origin":  {path: "/same/basic/schema.hcl", status: http.StatusFound, location: "/basic/schema.hcl"},
		"a redirect to the other origin": {path: "/away/basic/schema.hcl", status: http.StatusFound, location: "https://other.example/basic/schema.hcl"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, httpServer.URL+row.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if row.user != "" {
				request.SetBasicAuth(row.user, row.password)
			}
			if row.token != "" {
				request.Header.Set("Authorization", "Bearer "+row.token)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if response.StatusCode != row.status || response.Header.Get("Location") != row.location {
				t.Fatalf("status %d, Location %q; want %d, %q", response.StatusCode, response.Header.Get("Location"), row.status, row.location)
			}
			if row.status == http.StatusOK && string(body) != "table {}\n" {
				t.Fatalf("body %q, want the file", body)
			}
		})
	}
}

// The log names the host each request asked for, so a row can show that the
// other origin received nothing; it does not log its own reads.
func TestTheRequestLogNamesTheHostAndTheCredential(t *testing.T) {
	t.Parallel()
	_, httpServer := fixture(t)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, httpServer.URL+"/bearer/schema.hcl", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "artifacts.example"
	request.Header.Set("Authorization", "Bearer secret-token")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	for range 2 {
		response, err = http.Get(httpServer.URL + "/requests") // #nosec G107 -- the test's own server
		if err != nil {
			t.Fatal(err)
		}
		var requests []Request
		err = json.NewDecoder(response.Body).Decode(&requests)
		_ = response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		want := Request{Host: "artifacts.example", Path: "/bearer/schema.hcl", Credential: "Bearer", Status: http.StatusOK}
		if len(requests) != 1 || requests[0] != want {
			t.Fatalf("requests = %+v, want only %+v", requests, want)
		}
	}
}

func TestServedNameRefusesAnythingButOneElement(t *testing.T) {
	t.Parallel()
	for rest, want := range map[string]bool{
		"schema.hcl": true, "orders-1.4.0.tar.gz": true,
		"": false, ".": false, "..": false, "../schema.hcl": false, "a/b": false, `a\b`: false, "..x": false,
	} {
		if _, ok := servedName(rest); ok != want {
			t.Errorf("servedName(%q) = %v, want %v", rest, ok, want)
		}
	}
}
