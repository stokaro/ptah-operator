package main

import (
	"context"
	"encoding/json"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
)

// TestPublishLaysTheArtifactOutAsPtahDoes reads back what the fixture wrote. The
// layout is what `oci inspect` reported of an artifact the pinned image accepted
// up to its parse, so a change here is a row that stops somewhere else.
func TestPublishLaysTheArtifactOutAsPtahDoes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := memory.New()
	descriptor, err := publish(ctx, store, "stable")
	if err != nil {
		t.Fatalf("publish() error = %v", err)
	}
	tagged, err := store.Resolve(ctx, "stable")
	if err != nil {
		t.Fatalf("the tag does not resolve: %v", err)
	}
	if tagged.Digest != descriptor.Digest {
		t.Fatalf("the tag names %s, want %s", tagged.Digest, descriptor.Digest)
	}
	raw, err := content.FetchAll(ctx, store, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.ArtifactType != "application/vnd.stokaro.ptah.schema.v1" {
		t.Errorf("artifact type = %q", manifest.ArtifactType)
	}
	if manifest.Annotations["io.stokaro.ptah.schema-format"] != "hcl" {
		t.Errorf("annotations = %v", manifest.Annotations)
	}
	if len(manifest.Layers) != 1 {
		t.Fatalf("the manifest has %d layers, want one", len(manifest.Layers))
	}
	layer := manifest.Layers[0]
	if layer.MediaType != "application/vnd.stokaro.ptah.schema.hcl.v1" || layer.Annotations[ocispec.AnnotationTitle] != "schema.hcl" {
		t.Errorf("layer = %+v", layer)
	}
	body, err := content.FetchAll(ctx, store, layer)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != unparsableSchema {
		t.Errorf("layer body = %q", body)
	}
}

// TestPublishCanRunTwice is a rerun against a retained registry.
func TestPublishCanRunTwice(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := memory.New()
	for attempt := range 2 {
		if _, err := publish(ctx, store, "stable"); err != nil {
			t.Fatalf("attempt %d: publish() error = %v", attempt, err)
		}
	}
}
