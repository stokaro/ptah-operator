package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPreparationRequiresCurrentMasterAndNoReleaseTag(t *testing.T) {
	t.Parallel()
	sha := strings.Repeat("a", 40)
	for _, test := range []struct {
		name, ref, action, master string
		tagStatus                 int
		wantError                 bool
	}{
		{"untagged master", preparationSourceRef, "prepare", sha, 404, false},
		{"tag appeared", preparationSourceRef, "prepare", sha, 200, true},
		{"tag query forbidden", preparationSourceRef, "prepare", sha, 403, true},
		{"tag query unavailable", preparationSourceRef, "prepare", sha, 503, true},
		{"master moved", preparationSourceRef, "prepare", strings.Repeat("b", 40), 404, true},
		{"branch publication", preparationSourceRef, "publish", sha, 404, true},
		{"untrusted branch", "refs/heads/feature", "prepare", sha, 404, true},
		{"PR ref", "refs/pull/1/merge", "prepare", sha, 404, true},
		{"tag publication", "refs/tags/v0.2.0", "publish", sha, 200, false},
		{"publication without tag", "refs/tags/v0.2.0", "publish", sha, 404, true},
		{"wrong tag", "refs/tags/v0.3.0", "publish", sha, 200, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer fixture" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				value := test.master
				switch r.URL.Path {
				case "/repos/stokaro/ptah-operator/git/ref/heads/master":
				case "/repos/stokaro/ptah-operator/git/ref/tags/v0.2.0":
					w.WriteHeader(test.tagStatus)
					value = sha
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(500)
				}
				fmt.Fprintf(w, `{"object":{"type":"commit","sha":%q}}`, value)
			}))
			defer server.Close()
			err := verifyReleaseSource(context.Background(), server.Client(), server.URL, repositoryName, "v0.2.0", test.ref, sha, test.action, "fixture")
			if (err != nil) != test.wantError {
				t.Fatalf("error=%v, want error=%t", err, test.wantError)
			}
		})
	}
}

func TestReleaseLookupIncludesUntaggedDraftsAndRefusesAmbiguity(t *testing.T) {
	t.Parallel()
	sha := strings.Repeat("a", 40)
	draft := map[string]any{"tag_name": "v0.2.0", "draft": true, "target_commitish": sha, "id": 12}
	for _, test := range []struct {
		name                string
		status              int
		values              []map[string]any
		body                string
		wantError, wantNull bool
	}{
		{name: "absent", status: 200, values: []map[string]any{}, wantNull: true},
		{name: "untagged draft", status: 200, values: []map[string]any{draft}},
		{name: "duplicate", status: 200, values: []map[string]any{draft, draft}, wantError: true},
		{name: "mutable branch target", status: 200, values: []map[string]any{{"tag_name": "v0.2.0", "draft": true, "target_commitish": "master"}}, wantError: true},
		{name: "wrong commit", status: 200, values: []map[string]any{{"tag_name": "v0.2.0", "draft": true, "target_commitish": strings.Repeat("b", 40)}}, wantError: true},
		{name: "unrelated", status: 200, values: []map[string]any{{"tag_name": "v0.1.0"}}, wantNull: true},
		{name: "published", status: 200, values: []map[string]any{{"tag_name": "v0.2.0", "draft": false, "target_commitish": "master"}}},
		{name: "denied", status: 403, wantError: true},
		{name: "not found is not a fresh transaction", status: 404, wantError: true},
		{name: "service error", status: 503, wantError: true},
		{name: "malformed", status: 200, body: "broken", wantError: true},
		{name: "null response", status: 200, body: "null", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/stokaro/ptah-operator/releases" || r.URL.Query().Get("page") != "1" || r.Header.Get("Authorization") != "Bearer fixture" {
					t.Errorf("unexpected request %s", r.URL)
				}
				w.WriteHeader(test.status)
				if test.body != "" {
					fmt.Fprint(w, test.body)
				} else {
					_ = json.NewEncoder(w).Encode(test.values)
				}
			}))
			defer server.Close()
			body, err := readGitHubRelease(context.Background(), server.Client(), server.URL, repositoryName, "v0.2.0", sha, "fixture")
			if (err != nil) != test.wantError {
				t.Fatalf("body=%s error=%v", body, err)
			}
			if err == nil && (string(body) == "null") != test.wantNull {
				t.Fatalf("unexpected lookup %s", body)
			}
		})
	}
}

func TestReleaseLookupChecksAllPagesBeforeChoosingADraft(t *testing.T) {
	t.Parallel()
	sha := strings.Repeat("a", 40)
	for _, duplicate := range []bool{false, true} {
		t.Run(fmt.Sprint(duplicate), func(t *testing.T) {
			pages := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				pages++
				releases := []map[string]any{{"tag_name": "v0.2.0", "draft": true, "target_commitish": sha}}
				if r.URL.Query().Get("page") == "1" {
					releases = make([]map[string]any, 100)
					for i := range releases {
						releases[i] = map[string]any{"tag_name": fmt.Sprintf("v0.1.%d", i)}
					}
					if duplicate {
						releases[0] = map[string]any{"tag_name": "v0.2.0", "draft": true, "target_commitish": sha}
					}
				}
				_ = json.NewEncoder(w).Encode(releases)
			}))
			defer server.Close()
			_, err := readGitHubRelease(context.Background(), server.Client(), server.URL, repositoryName, "v0.2.0", sha, "fixture")
			if (err != nil) != duplicate || pages != 2 {
				t.Fatalf("error=%v pages=%d", err, pages)
			}
		})
	}
}
