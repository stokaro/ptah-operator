// Copyright 2026 The Ptah Operator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildUpdatePlanSelectsNewestCompleteStableKindRelease(t *testing.T) {
	t.Parallel()

	digest := func(character string) string { return strings.Repeat(character, 64) }
	releases := []githubRelease{
		{
			TagName: "v0.34.0",
			Body:    "kindest/node:v1.37.2@sha256:" + digest("a"),
		},
		{
			TagName:    "v0.33.0",
			Prerelease: true,
			Body: strings.Join([]string{
				"kindest/node:v1.35.6@sha256:" + digest("b"),
				"kindest/node:v1.36.2@sha256:" + digest("c"),
				"kindest/node:v1.37.1@sha256:" + digest("d"),
			}, "\n"),
		},
		{
			TagName: "v0.32.0",
			Body: strings.Join([]string{
				"default kindest/node:v1.37.0@sha256:" + digest("3"),
				"kindest/node:v1.35.5@sha256:" + digest("1"),
				"kindest/node:v1.36.1@sha256:" + digest("2"),
				"kindest/node:v1.37.0@sha256:" + digest("3"),
			}, "\n"),
		},
		{
			TagName: "v0.31.0",
			Body: strings.Join([]string{
				"kindest/node:v1.35.0@sha256:" + digest("4"),
				"kindest/node:v1.36.0@sha256:" + digest("5"),
				"kindest/node:v1.37.0@sha256:" + digest("6"),
			}, "\n"),
		},
	}
	kindContents, err := json.Marshal(releases)
	if err != nil {
		t.Fatalf("marshal kind releases: %v", err)
	}
	date, _ := time.Parse("2006-01-02", "2026-08-31")
	plan, err := buildUpdatePlan(baseManifest(), []byte("v1.37.4\n"), kindContents, date)
	if err != nil {
		t.Fatalf("buildUpdatePlan() error = %v", err)
	}
	if plan.manifest.KindVersion != "v0.32.0" {
		t.Fatalf("kindVersion = %q, want v0.32.0", plan.manifest.KindVersion)
	}
	if plan.oldest != "1.35" || plan.newest != "1.37" {
		t.Fatalf("window = %s-%s, want 1.35-1.37", plan.oldest, plan.newest)
	}
	if plan.manifest.LastVerified != "2026-08-31" {
		t.Fatalf("lastVerified = %q", plan.manifest.LastVerified)
	}
	if got := plan.manifest.Releases[1].NodeImage; got != "kindest/node:v1.36.1@sha256:"+digest("2") {
		t.Fatalf("middle node image = %q", got)
	}
}

func TestBuildUpdatePlanRequiresOneCompatibleKindRelease(t *testing.T) {
	t.Parallel()

	digest := strings.Repeat("1", 64)
	releases := []githubRelease{
		{TagName: "v0.32.0", Body: "kindest/node:v1.35.5@sha256:" + digest},
		{TagName: "v0.31.0", Body: "kindest/node:v1.36.1@sha256:" + digest + "\nkindest/node:v1.37.0@sha256:" + digest},
	}
	kindContents, _ := json.Marshal(releases)
	date, _ := time.Parse("2006-01-02", "2026-08-31")
	_, err := buildUpdatePlan(baseManifest(), []byte("v1.37.0"), kindContents, date)
	if err == nil || !strings.Contains(err.Error(), "complete 1.35, 1.36, 1.37 window") {
		t.Fatalf("buildUpdatePlan() error = %v, want complete-window error", err)
	}
}

func TestImagesFromReleaseRejectsAmbiguousDigest(t *testing.T) {
	t.Parallel()
	targets := map[string]struct{}{"1.37": {}}
	body := strings.Join([]string{
		"kindest/node:v1.37.0@sha256:" + strings.Repeat("1", 64),
		"kindest/node:v1.37.0@sha256:" + strings.Repeat("2", 64),
	}, "\n")
	_, err := imagesFromRelease(body, targets)
	if err == nil || !strings.Contains(err.Error(), "ambiguous digest") {
		t.Fatalf("imagesFromRelease() error = %v, want ambiguous digest error", err)
	}
}

func TestSecureRedirectPolicy(t *testing.T) {
	t.Parallel()
	httpsURL, _ := url.Parse("https://cdn.example.test/stable.txt")
	httpURL, _ := url.Parse("http://cdn.example.test/stable.txt")
	if err := secureRedirectPolicy(&http.Request{URL: httpsURL}, []*http.Request{{URL: httpsURL}}); err != nil {
		t.Fatalf("secureRedirectPolicy(HTTPS) error = %v", err)
	}
	if err := secureRedirectPolicy(&http.Request{URL: httpURL}, []*http.Request{{URL: httpsURL}}); err == nil || !strings.Contains(err.Error(), "non-HTTPS") {
		t.Fatalf("secureRedirectPolicy(HTTP) error = %v, want downgrade rejection", err)
	}
	via := make([]*http.Request, 10)
	for index := range via {
		via[index] = &http.Request{URL: httpsURL}
	}
	if err := secureRedirectPolicy(&http.Request{URL: httpsURL}, via); err == nil || !strings.Contains(err.Error(), "10 redirects") {
		t.Fatalf("secureRedirectPolicy(redirect loop) error = %v, want redirect limit", err)
	}
}

func TestApplyUpdateChangesOnlyDerivedSupportFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, directory := range []string{"support", "charts/ptah-operator", "docs"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatalf("create %s: %v", directory, err)
		}
	}
	manifestContents, _ := json.MarshalIndent(baseManifest(), "", "  ")
	mustWrite(t, filepath.Join(root, manifestRelativePath), append(manifestContents, '\n'))
	mustWrite(t, filepath.Join(root, chartRelativePath), []byte("apiVersion: v2\nkubeVersion: \">=1.34.0-0 <1.37.0-0\"\n"))
	mustWrite(t, filepath.Join(root, docsRelativePath), []byte("before\n"+docsBeginMarker+"\nold\n"+docsEndMarker+"\nafter\n"))

	plan := updatePlan{
		manifest: supportManifest{
			SchemaVersion: 1,
			Policy:        "upstream-active-minors",
			WindowSize:    windowSize,
			LastVerified:  "2026-08-31",
			KindVersion:   "v0.32.0",
			Releases: []release{
				{Minor: "1.35", NodeImage: "kindest/node:v1.35.5@sha256:" + strings.Repeat("1", 64)},
				{Minor: "1.36", NodeImage: "kindest/node:v1.36.1@sha256:" + strings.Repeat("2", 64)},
				{Minor: "1.37", NodeImage: "kindest/node:v1.37.0@sha256:" + strings.Repeat("3", 64)},
			},
		},
		oldest: "1.35",
		newest: "1.37",
	}
	changed, err := applyUpdate(root, plan)
	if err != nil {
		t.Fatalf("applyUpdate() error = %v", err)
	}
	if !changed {
		t.Fatal("applyUpdate() changed = false, want true")
	}
	chart, _ := os.ReadFile(filepath.Join(root, chartRelativePath))
	if !strings.Contains(string(chart), `kubeVersion: ">=1.35.0-0 <1.38.0-0"`) {
		t.Fatalf("updated chart = %q", chart)
	}
	docs, _ := os.ReadFile(filepath.Join(root, docsRelativePath))
	if !strings.Contains(string(docs), "| 1.37 | `kindest/node:v1.37.0@sha256:") || !strings.HasPrefix(string(docs), "before\n") || !strings.HasSuffix(string(docs), "\nafter\n") {
		t.Fatalf("updated docs = %q", docs)
	}

	changed, err = applyUpdate(root, plan)
	if err != nil {
		t.Fatalf("second applyUpdate() error = %v", err)
	}
	if changed {
		t.Fatal("second applyUpdate() changed = true, want idempotent false")
	}
}

func baseManifest() supportManifest {
	return supportManifest{
		SchemaVersion: 1,
		Policy:        "upstream-active-minors",
		WindowSize:    windowSize,
		LastVerified:  "2026-07-01",
		KindVersion:   "v0.31.0",
		Releases: []release{
			{Minor: "1.34", NodeImage: "kindest/node:v1.34.0@sha256:" + strings.Repeat("4", 64)},
			{Minor: "1.35", NodeImage: "kindest/node:v1.35.0@sha256:" + strings.Repeat("5", 64)},
			{Minor: "1.36", NodeImage: "kindest/node:v1.36.0@sha256:" + strings.Repeat("6", 64)},
		},
	}
}

func mustWrite(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
