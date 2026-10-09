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
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry"
)

func push(t *testing.T, store *memory.Store, mediaType string, body []byte) ocispec.Descriptor {
	t.Helper()
	desc := content.NewDescriptorFromBytes(mediaType, body)
	if err := store.Push(context.Background(), desc, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	return desc
}

func pushJSON(t *testing.T, store *memory.Store, mediaType string, value any) ocispec.Descriptor {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return push(t, store, mediaType, raw)
}

func image(t *testing.T, store *memory.Store, os, arch string) ocispec.Descriptor {
	t.Helper()
	config := pushJSON(t, store, ocispec.MediaTypeImageConfig, ocispec.Image{Platform: ocispec.Platform{OS: os, Architecture: arch}})
	layer := push(t, store, ocispec.MediaTypeImageLayerGzip, []byte("layer for "+os+"/"+arch))
	manifest := pushJSON(t, store, ocispec.MediaTypeImageManifest, ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    config,
		Layers:    []ocispec.Descriptor{layer},
	})
	manifest.Platform = &ocispec.Platform{OS: os, Architecture: arch}
	return manifest
}

// releaseIndex is the shape the release pushes: two platform images and an
// attestation manifest that names no real platform.
func releaseIndex(t *testing.T, store *memory.Store, platforms ...[2]string) ocispec.Descriptor {
	t.Helper()
	var manifests []ocispec.Descriptor
	for _, platform := range platforms {
		manifests = append(manifests, image(t, store, platform[0], platform[1]))
	}
	attestation := image(t, store, "unknown", "unknown")
	manifests = append(manifests, attestation)
	root := pushJSON(t, store, ocispec.MediaTypeImageIndex, ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: manifests,
	})
	resolvable(t, store, root)
	return root
}

// resolvable tags a descriptor with its own digest: a registry resolves a
// digest reference, and the memory store resolves only tags.
func resolvable(t *testing.T, store *memory.Store, desc ocispec.Descriptor) {
	t.Helper()
	if err := store.Tag(context.Background(), desc, desc.Digest.String()); err != nil {
		t.Fatal(err)
	}
}

func TestCopyPinnedPreservesTheIndexAndEveryChild(t *testing.T) {
	ctx := context.Background()
	src, dst := memory.New(), memory.New()
	root := releaseIndex(t, src, [2]string{"linux", "arm64"}, [2]string{"linux", "amd64"})

	result, err := copyPinned(ctx, src, root.Digest.String(), dst, "candidate", []string{"linux/amd64", "linux/arm64"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Digest != root.Digest.String() || result.MediaType != ocispec.MediaTypeImageIndex {
		t.Fatalf("result = %+v, want the source index %s", result, root.Digest)
	}
	if want := []string{"linux/amd64", "linux/arm64"}; !reflect.DeepEqual(result.Platforms, want) {
		t.Fatalf("platforms = %v, want %v", result.Platforms, want)
	}
	stored, err := dst.Resolve(ctx, "candidate")
	if err != nil || stored.Digest != root.Digest {
		t.Fatalf("destination tag resolves to %v (%v), want %s", stored.Digest, err, root.Digest)
	}
	raw, err := content.FetchAll(ctx, src, root)
	if err != nil {
		t.Fatal(err)
	}
	var index ocispec.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatal(err)
	}
	for _, child := range index.Manifests {
		exists, err := dst.Exists(ctx, child)
		if err != nil || !exists {
			t.Fatalf("child %s was not copied (%v)", child.Digest, err)
		}
	}
}

func TestCopyPinnedRefusesAMissingPlatformBeforeWriting(t *testing.T) {
	ctx := context.Background()
	src, dst := memory.New(), memory.New()
	root := releaseIndex(t, src, [2]string{"linux", "amd64"})

	_, err := copyPinned(ctx, src, root.Digest.String(), dst, "candidate", []string{"linux/amd64", "linux/arm64"})
	if err == nil || !strings.Contains(err.Error(), "not linux/arm64") {
		t.Fatalf("err = %v, want a refusal naming linux/arm64", err)
	}
	if exists, _ := dst.Exists(ctx, root); exists {
		t.Fatal("a refused copy wrote the index")
	}
	if _, err := dst.Resolve(ctx, "candidate"); err == nil {
		t.Fatal("a refused copy tagged the destination")
	}
}

func TestCopyPinnedDoesNotCountTheAttestationAsAPlatform(t *testing.T) {
	ctx := context.Background()
	src := memory.New()
	root := releaseIndex(t, src, [2]string{"linux", "amd64"})

	_, err := copyPinned(ctx, src, root.Digest.String(), memory.New(), "candidate", []string{"unknown/unknown"})
	if err == nil {
		t.Fatal("the attestation manifest satisfied a platform requirement")
	}
}

// substitutingTarget stores what it is given but resolves every tag to another
// object, the way a registry that rewrote the manifest would.
type substitutingTarget struct {
	*memory.Store
	other ocispec.Descriptor
}

func (s substitutingTarget) Resolve(ctx context.Context, reference string) (ocispec.Descriptor, error) {
	if _, err := s.Store.Resolve(ctx, reference); err != nil {
		return ocispec.Descriptor{}, err
	}
	return s.other, nil
}

var _ oras.Target = substitutingTarget{}

func TestCopyPinnedRefusesADestinationThatResolvesToOtherBytes(t *testing.T) {
	ctx := context.Background()
	src := memory.New()
	root := releaseIndex(t, src, [2]string{"linux", "amd64"})
	other := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageIndex,
		Digest:    content.NewDescriptorFromBytes(ocispec.MediaTypeImageIndex, []byte("rewritten")).Digest,
		Size:      root.Size,
	}

	_, err := copyPinned(ctx, src, root.Digest.String(), substitutingTarget{Store: memory.New(), other: other}, "candidate", nil)
	if err == nil || !strings.Contains(err.Error(), "resolves to "+other.Digest.String()) {
		t.Fatalf("err = %v, want a refusal naming the substituted digest", err)
	}
}

func TestCopyPinnedReadsASingleManifestPlatformFromItsConfig(t *testing.T) {
	ctx := context.Background()
	src := memory.New()
	manifest := image(t, src, "linux", "arm64")
	manifest.Platform = nil
	resolvable(t, src, manifest)

	result, err := copyPinned(ctx, src, manifest.Digest.String(), memory.New(), "candidate", []string{"linux/arm64"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Platforms, []string{"linux/arm64"}) {
		t.Fatalf("platforms = %v", result.Platforms)
	}
	if _, err := copyPinned(ctx, src, manifest.Digest.String(), memory.New(), "candidate", []string{"linux/amd64"}); err == nil {
		t.Fatal("a single arm64 manifest satisfied linux/amd64")
	}
}

func TestRunRefusesUnpinnedSourcesAndUntaggedDestinations(t *testing.T) {
	pinned := "registry.example/team/operator@sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, from, to, want string
	}{
		{"tag source", "registry.example/team/operator:v0.2.0", "127.0.0.1:5000/ptah-operator:candidate", "pinned by a sha256 digest"},
		{"digest destination", pinned, "127.0.0.1:5000/ptah-operator@sha256:" + strings.Repeat("b", 64), "must name a tag"},
		{"no destination reference", pinned, "127.0.0.1:5000/ptah-operator", "must name a tag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := run(context.Background(), options{from: tc.from, to: tc.to})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func mustReference(t *testing.T, value string) registry.Reference {
	t.Helper()
	ref, err := registry.ParseReference(value)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestRepositoryRefusesAHalfCredential(t *testing.T) {
	ref := mustReference(t, "127.0.0.1:5000/ptah-operator:candidate")
	if _, err := repository(ref, true, "user", ""); err == nil {
		t.Fatal("a user without a password file was accepted")
	}
	if _, err := repository(ref, true, "", "/nonexistent"); err == nil {
		t.Fatal("a password file without a user was accepted")
	}
}
