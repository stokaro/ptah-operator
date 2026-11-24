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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	fakeToken     = "anonymous-pull-token"
	ociIndex      = "application/vnd.oci.image.index.v1+json"
	ociManifest   = "application/vnd.oci.image.manifest.v1+json"
	newEnvoy      = "docker.io/envoyproxy/envoy:v1.37.0"
	newEnvoyTag   = "v1.37.0"
	pinnedEnvoy   = "docker.io/envoyproxy/envoy:v1.35.3"
	pinnedTag     = "v1.35.3"
	movedKind     = "v0.32.0"
	envoyRepo     = "envoyproxy/envoy"
	envoyScope    = "repository:envoyproxy/envoy:pull"
	dockerService = "registry.docker.io"
)

// fakeUpstream stands in for the kind repository's raw files, a registry that
// answers anonymous reads with a Bearer challenge, its token service, and the
// release metadata run reads. Each field shapes one answer.
type fakeUpstream struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []string
	// registries records each registry host the resolver asked an endpoint for.
	registries []string
	// authorizations records the Authorization header of each manifest read.
	authorizations []string
	// tokenQueries records the query of each token request.
	tokenQueries []string

	stable   string
	releases []githubRelease
	// sources maps a kind tag to the const.go served at it.
	sources map[string]string
	// challenge is the WWW-Authenticate value of an unauthenticated read.
	challenge   string
	tokenStatus int
	tokenBody   string
	// manifests maps a tag of envoyproxy/envoy to what the registry serves.
	manifests map[string]servedManifest
}

type servedManifest struct {
	status      int
	contentType string
	body        []byte
	// digest overrides Docker-Content-Digest; empty sends the digest of body.
	digest string
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	upstream := &fakeUpstream{
		sources: map[string]string{
			movedKind: kindSource(newEnvoy),
		},
		tokenStatus: http.StatusOK,
		tokenBody:   `{"token":"` + fakeToken + `"}`,
		manifests: map[string]servedManifest{
			newEnvoyTag: indexManifest("linux/amd64", "linux/arm64"),
			pinnedTag:   indexManifest("linux/amd64", "linux/arm64", "linux/arm/v7"),
		},
	}
	upstream.server = httptest.NewTLSServer(http.HandlerFunc(upstream.serve))
	upstream.challenge = `Bearer realm="` + upstream.server.URL + `/token",service="` + dockerService + `",scope="` + envoyScope + `"`
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (u *fakeUpstream) serve(writer http.ResponseWriter, request *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.requests = append(u.requests, request.URL.Path)
	path := request.URL.Path
	switch {
	case path == "/stable.txt":
		_, _ = writer.Write([]byte(u.stable))
	case path == "/releases":
		contents, _ := json.Marshal(u.releases)
		_, _ = writer.Write(contents)
	case strings.HasPrefix(path, "/kind/"):
		tag, rest, _ := strings.Cut(strings.TrimPrefix(path, "/kind/"), "/")
		source, ok := u.sources[tag]
		if !ok || rest != kindLoadBalancerSource {
			http.NotFound(writer, request)
			return
		}
		_, _ = writer.Write([]byte(source))
	case path == "/token":
		u.tokenQueries = append(u.tokenQueries, request.URL.RawQuery)
		writer.WriteHeader(u.tokenStatus)
		_, _ = writer.Write([]byte(u.tokenBody))
	case strings.HasPrefix(path, "/v2/"+envoyRepo+"/manifests/"):
		authorization := request.Header.Get("Authorization")
		u.authorizations = append(u.authorizations, authorization)
		if authorization != "Bearer "+fakeToken {
			writer.Header().Set("WWW-Authenticate", u.challenge)
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		served, ok := u.manifests[strings.TrimPrefix(path, "/v2/"+envoyRepo+"/manifests/")]
		if !ok {
			http.NotFound(writer, request)
			return
		}
		digest := served.digest
		if digest == "" {
			digest = digestOf(served.body)
		}
		writer.Header().Set("Content-Type", served.contentType)
		writer.Header().Set("Docker-Content-Digest", digest)
		status := served.status
		if status == 0 {
			status = http.StatusOK
		}
		writer.WriteHeader(status)
		_, _ = writer.Write(served.body)
	default:
		http.NotFound(writer, request)
	}
}

func (u *fakeUpstream) resolver() loadBalancerResolver {
	return loadBalancerResolver{
		client:    u.server.Client(),
		sourceURL: u.server.URL + "/kind",
		registryEndpoint: func(registry string) string {
			u.mu.Lock()
			defer u.mu.Unlock()
			u.registries = append(u.registries, registry)
			return u.server.URL
		},
	}
}

func (u *fakeUpstream) requestCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.requests)
}

func kindSource(image string) string {
	return "/*\nCopyright 2019 The Kubernetes Authors.\n*/\n\npackage loadbalancer\n\n" +
		"// Image defines the loadbalancer image:tag\nconst Image = \"" + image + "\"\n"
}

func indexManifest(platforms ...string) servedManifest {
	type platform struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant,omitempty"`
	}
	type entry struct {
		MediaType string   `json:"mediaType"`
		Digest    string   `json:"digest"`
		Size      int      `json:"size"`
		Platform  platform `json:"platform"`
	}
	index := struct {
		SchemaVersion int     `json:"schemaVersion"`
		MediaType     string  `json:"mediaType"`
		Manifests     []entry `json:"manifests"`
	}{SchemaVersion: 2, MediaType: ociIndex}
	for number, name := range platforms {
		parts := strings.Split(name, "/")
		item := platform{OS: parts[0], Architecture: parts[1]}
		if len(parts) > 2 {
			item.Variant = parts[2]
		}
		index.Manifests = append(index.Manifests, entry{
			MediaType: ociManifest,
			Digest:    "sha256:" + strings.Repeat(string(rune('a'+number)), 64),
			Size:      1000 + number,
			Platform:  item,
		})
	}
	body, _ := json.Marshal(index)
	return servedManifest{contentType: ociIndex, body: body}
}

func digestOf(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// movedPlan is a plan whose kind moved from the one baseManifest's pin was
// read from.
func movedPlan() updatePlan {
	manifest := baseManifest()
	manifest.KindVersion = movedKind
	return updatePlan{manifest: manifest, oldest: "1.35", newest: "1.37"}
}

func TestRefreshKindLoadBalancerReadsNothingWhenKindDidNotMove(t *testing.T) {
	t.Parallel()
	upstream := newFakeUpstream(t)
	plan := updatePlan{manifest: baseManifest()}

	got, err := refreshKindLoadBalancer(context.Background(), plan, upstream.resolver())
	if err != nil {
		t.Fatalf("refreshKindLoadBalancer() error = %v", err)
	}
	if got.manifest.KindLoadBalancer != baseManifest().KindLoadBalancer {
		t.Fatalf("kindLoadBalancer = %+v, want the pin unchanged", got.manifest.KindLoadBalancer)
	}
	if count := upstream.requestCount(); count != 0 {
		t.Fatalf("refreshKindLoadBalancer() sent %d requests, want none when kind did not move", count)
	}
}

func TestRefreshKindLoadBalancerPinsTheImageTheNewKindNames(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name      string
		reference string
		tag       string
	}{
		{name: "moved reference", reference: newEnvoy, tag: newEnvoyTag},
		{name: "unchanged reference", reference: pinnedEnvoy, tag: pinnedTag},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			upstream := newFakeUpstream(t)
			upstream.sources[movedKind] = kindSource(row.reference)

			got, err := refreshKindLoadBalancer(context.Background(), movedPlan(), upstream.resolver())
			if err != nil {
				t.Fatalf("refreshKindLoadBalancer() error = %v", err)
			}
			want := kindLoadBalancerPin{
				KindVersion: movedKind,
				Image:       row.reference + "@" + digestOf(upstream.manifests[row.tag].body),
			}
			if got.manifest.KindLoadBalancer != want {
				t.Fatalf("kindLoadBalancer = %+v, want %+v", got.manifest.KindLoadBalancer, want)
			}
			if got.manifest.KindLoadBalancer.Image == baseManifest().KindLoadBalancer.Image {
				t.Fatal("kindLoadBalancer.image kept the previous digest; the row must serve another")
			}
			if strings.Join(upstream.registries, ",") != dockerHubRegistry {
				t.Fatalf("asked for the endpoints of %q, want docker.io once", upstream.registries)
			}
			// An anonymous read is challenged, the token is asked for the
			// challenge's service and scope, and only the retry carries it.
			if want := []string{"", "Bearer " + fakeToken}; strings.Join(upstream.authorizations, "|") != strings.Join(want, "|") {
				t.Fatalf("manifest reads carried Authorization %q, want %q", upstream.authorizations, want)
			}
			if len(upstream.tokenQueries) != 1 {
				t.Fatalf("token requests = %q, want one", upstream.tokenQueries)
			}
			query, err := url.ParseQuery(upstream.tokenQueries[0])
			if err != nil || query.Get("service") != dockerService || query.Get("scope") != envoyScope || len(query) != 2 {
				t.Fatalf("token request query = %q, want service %s and scope %s", upstream.tokenQueries[0], dockerService, envoyScope)
			}
		})
	}
}

func TestRefreshKindLoadBalancerRefusesWhatItCannotPin(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name   string
		change func(*fakeUpstream)
		want   string
	}{
		{
			name:   "kind source missing at the tag",
			change: func(u *fakeUpstream) { delete(u.sources, movedKind) },
			want:   "read " + kindLoadBalancerSource + " at " + movedKind + ": GET",
		},
		{
			name: "another package served",
			change: func(u *fakeUpstream) {
				u.sources[movedKind] = "package cluster\n\nconst Image = \"" + newEnvoy + "\"\n"
			},
			want: "declares package cluster, not loadbalancer",
		},
		{
			name: "no Image constant",
			change: func(u *fakeUpstream) {
				u.sources[movedKind] = "package loadbalancer\n\nconst Name = \"" + newEnvoy + "\"\n"
			},
			want: "declares 0 constants named Image, want 1",
		},
		{
			name: "two Image constants",
			change: func(u *fakeUpstream) {
				u.sources[movedKind] = "package loadbalancer\n\nconst Image = \"" + newEnvoy + "\"\n\nconst (\n\tImage = \"" + pinnedEnvoy + "\"\n)\n"
			},
			want: "declares 2 constants named Image, want 1",
		},
		{
			name: "Image built from an expression",
			change: func(u *fakeUpstream) {
				u.sources[movedKind] = "package loadbalancer\n\nconst repository = \"docker.io/envoyproxy/envoy\"\n\nconst Image = repository + \":v1.37.0\"\n"
			},
			want: "constant Image is not a string literal",
		},
		{
			name:   "Image without a registry",
			change: func(u *fakeUpstream) { u.sources[movedKind] = kindSource("envoyproxy/envoy:v1.37.0") },
			want:   `constant Image is "envoyproxy/envoy:v1.37.0", not the registry, repository and tag of one image`,
		},
		{
			name: "Image already pinned",
			change: func(u *fakeUpstream) {
				u.sources[movedKind] = kindSource(newEnvoy + "@sha256:" + strings.Repeat("1", 64))
			},
			want: "not the registry, repository and tag of one image",
		},
		{
			name:   "registry challenge that is not Bearer",
			change: func(u *fakeUpstream) { u.challenge = `Basic realm="registry"` },
			want:   "not a Bearer token challenge",
		},
		{
			name:   "Bearer challenge without a realm",
			change: func(u *fakeUpstream) { u.challenge = `Bearer service="` + dockerService + `"` },
			want:   "names no realm",
		},
		{
			name:   "token service fails",
			change: func(u *fakeUpstream) { u.tokenStatus = http.StatusInternalServerError },
			want:   "request an anonymous pull token: GET",
		},
		{
			name:   "token service answers something other than JSON",
			change: func(u *fakeUpstream) { u.tokenBody = `not json` },
			want:   "decode the anonymous pull token",
		},
		{
			name:   "token service grants nothing",
			change: func(u *fakeUpstream) { u.tokenBody = `{}` },
			want:   "the token service granted no anonymous pull token",
		},
		{
			name:   "tag unknown to the registry",
			change: func(u *fakeUpstream) { delete(u.manifests, newEnvoyTag) },
			want:   "unexpected HTTP status 404",
		},
		{
			name: "tag names a single-platform manifest",
			change: func(u *fakeUpstream) {
				u.manifests[newEnvoyTag] = servedManifest{contentType: ociManifest, body: []byte(`{"schemaVersion":2,"mediaType":"` + ociManifest + `"}`)}
			},
			want: "the tag names a " + ociManifest + ", not a multi-platform index",
		},
		{
			name: "manifest without a Content-Type",
			change: func(u *fakeUpstream) {
				served := u.manifests[newEnvoyTag]
				served.contentType = ""
				u.manifests[newEnvoyTag] = served
			},
			want: "unreadable Content-Type",
		},
		{
			name: "index that is not JSON",
			change: func(u *fakeUpstream) {
				u.manifests[newEnvoyTag] = servedManifest{contentType: ociIndex, body: []byte("<html>")}
			},
			want: "decode the index",
		},
		{
			name: "index that says it is another media type",
			change: func(u *fakeUpstream) {
				served := u.manifests[newEnvoyTag]
				served.contentType = "application/vnd.docker.distribution.manifest.list.v2+json"
				u.manifests[newEnvoyTag] = served
			},
			want: "the index says it is a " + ociIndex + ", served as a application/vnd.docker.distribution.manifest.list.v2+json",
		},
		{
			name:   "index without an arm64 image",
			change: func(u *fakeUpstream) { u.manifests[newEnvoyTag] = indexManifest("linux/amd64", "linux/arm/v7") },
			want:   "the index lists no linux/arm64 image",
		},
		{
			name: "registry digest that is not the bytes served",
			change: func(u *fakeUpstream) {
				served := u.manifests[newEnvoyTag]
				served.digest = "sha256:" + strings.Repeat("0", 64)
				u.manifests[newEnvoyTag] = served
			},
			want: "the registry says the index is sha256:" + strings.Repeat("0", 64),
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			upstream := newFakeUpstream(t)
			row.change(upstream)

			got, err := refreshKindLoadBalancer(context.Background(), movedPlan(), upstream.resolver())
			if err == nil {
				t.Fatalf("refreshKindLoadBalancer() pinned %+v, want a refusal", got.manifest.KindLoadBalancer)
			}
			for _, fragment := range []string{
				manifestRelativePath + " kindLoadBalancer: cannot pin the load balancer image of kind " + movedKind,
				row.want,
			} {
				if !strings.Contains(err.Error(), fragment) {
					t.Fatalf("refreshKindLoadBalancer() error = %v, want it to contain %q", err, fragment)
				}
			}
		})
	}
}

func TestRegistryEndpointNamesDockerHubsAPIHost(t *testing.T) {
	t.Parallel()
	for registry, want := range map[string]string{
		"docker.io":       "https://registry-1.docker.io",
		"ghcr.io":         "https://ghcr.io",
		"registry.k8s.io": "https://registry.k8s.io",
	} {
		if got := registryEndpoint(registry); got != want {
			t.Errorf("registryEndpoint(%q) = %q, want %q", registry, got, want)
		}
	}
}

// run reads the stable release, the kind releases and the load balancer before
// it writes, so a proposal either carries the new kind's image or is not made.
func TestRunWritesTheNewPinOrNothing(t *testing.T) {
	t.Parallel()
	digest := func(character string) string { return strings.Repeat(character, 64) }
	releases := []githubRelease{{
		TagName: movedKind,
		Body: strings.Join([]string{
			"kindest/node:v1.35.5@sha256:" + digest("1"),
			"kindest/node:v1.36.1@sha256:" + digest("2"),
			"kindest/node:v1.37.0@sha256:" + digest("3"),
		}, "\n"),
	}}
	date, _ := time.Parse("2006-01-02", "2026-08-31")

	for _, row := range []struct {
		name    string
		change  func(*fakeUpstream)
		refused bool
	}{
		{name: "pinned", change: func(*fakeUpstream) {}},
		{name: "refused", change: func(u *fakeUpstream) { delete(u.sources, movedKind) }, refused: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			upstream := newFakeUpstream(t)
			upstream.stable = "v1.37.4\n"
			upstream.releases = releases
			row.change(upstream)
			root := writeRepository(t)
			before := readRepository(t, root)

			message, err := run(context.Background(), options{
				repositoryRoot:  root,
				stableURL:       upstream.server.URL + "/stable.txt",
				kindReleasesURL: upstream.server.URL + "/releases",
				date:            date,
				client:          upstream.server.Client(),
				loadBalancer:    upstream.resolver(),
			})
			after := readRepository(t, root)

			if row.refused {
				if err == nil || !strings.Contains(err.Error(), manifestRelativePath+" kindLoadBalancer") {
					t.Fatalf("run() = %q, %v; want a refusal naming %s kindLoadBalancer", message, err, manifestRelativePath)
				}
				for path, contents := range before {
					if after[path] != contents {
						t.Fatalf("run() refused and still rewrote %s:\n%s", path, after[path])
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("run() error = %v", err)
			}
			if !strings.HasPrefix(message, "Updated Kubernetes support window to 1.35-1.37 with kind "+movedKind) {
				t.Fatalf("run() message = %q", message)
			}
			manifest, err := readManifest(filepath.Join(root, manifestRelativePath))
			if err != nil {
				t.Fatalf("read the written manifest: %v", err)
			}
			want := kindLoadBalancerPin{KindVersion: movedKind, Image: newEnvoy + "@" + digestOf(upstream.manifests[newEnvoyTag].body)}
			if manifest.KindVersion != movedKind || manifest.KindLoadBalancer != want {
				t.Fatalf("written kindVersion %s, kindLoadBalancer %+v; want %s, %+v", manifest.KindVersion, manifest.KindLoadBalancer, movedKind, want)
			}
		})
	}
}

func writeRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, directory := range []string{"support", "charts/ptah-operator", filepath.Dir(docsRelativePath)} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatalf("create %s: %v", directory, err)
		}
	}
	manifestContents, _ := json.MarshalIndent(baseManifest(), "", "  ")
	mustWrite(t, filepath.Join(root, manifestRelativePath), append(manifestContents, '\n'))
	mustWrite(t, filepath.Join(root, chartRelativePath), []byte("apiVersion: v2\nkubeVersion: \">=1.34.0-0 <1.37.0-0\"\n"))
	mustWrite(t, filepath.Join(root, docsRelativePath), []byte("before\n"+docsBeginMarker+"\nold\n"+docsEndMarker+"\nafter\n"))
	return root
}

func readRepository(t *testing.T, root string) map[string]string {
	t.Helper()
	contents := map[string]string{}
	for _, path := range []string{manifestRelativePath, chartRelativePath, docsRelativePath} {
		read, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		contents[path] = string(read)
	}
	return contents
}
