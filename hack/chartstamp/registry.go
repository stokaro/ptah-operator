package chartstamp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/errdef"
)

// Stage writes the artifact by digest, under no tag, and refuses to report success
// unless the registry resolves that digest. It can sign and attest there and be
// read by digest, and no name a reader installs by reaches it. Running it again
// changes nothing, and the bytes can always be computed again, so a resumed
// release transaction needs no retention tag to find them.
func Stage(ctx context.Context, target oras.Target, artifact Artifact) error {
	store := memory.New()
	root := content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, artifact.Manifest)
	for _, part := range []struct {
		descriptor ocispec.Descriptor
		body       []byte
	}{
		{content.NewDescriptorFromBytes(configMediaType, artifact.Config), artifact.Config},
		{content.NewDescriptorFromBytes(layerMediaType, artifact.Chart), artifact.Chart},
		{root, artifact.Manifest},
	} {
		if err := store.Push(ctx, part.descriptor, bytes.NewReader(part.body)); err != nil {
			return fmt.Errorf("hold the chart artifact: %w", err)
		}
	}
	if err := oras.CopyGraph(ctx, store, target, root, oras.DefaultCopyGraphOptions); err != nil {
		return fmt.Errorf("push the chart artifact: %w", err)
	}
	stored, err := target.Resolve(ctx, artifact.Digest)
	if err != nil {
		return fmt.Errorf("resolve the pushed chart %s: %w", artifact.Digest, err)
	}
	if stored.Digest != root.Digest {
		return fmt.Errorf("the registry resolves %s to %s", artifact.Digest, stored.Digest)
	}
	return nil
}

// Tag gives the chart at digest its version tag, the name `helm --version` reads.
// A version names one chart for good: a tag that already holds that digest is the
// done state, and a tag that holds another is refused rather than moved, since a
// reader who verified it would otherwise install something else.
func Tag(ctx context.Context, target oras.Target, digest, version string) error {
	existing, err := target.Resolve(ctx, version)
	switch {
	case err == nil && existing.Digest.String() == digest:
		return nil
	case err == nil:
		return fmt.Errorf("version tag %s already holds %s, not %s; refusing to move it", version, existing.Digest, digest)
	case !errors.Is(err, errdef.ErrNotFound):
		return fmt.Errorf("read version tag %s: %w", version, err)
	}
	if _, err := oras.Tag(ctx, target, digest, version); err != nil {
		return fmt.Errorf("tag %s as %s: %w", digest, version, err)
	}
	return nil
}

// ReadBack reads a chart through reference the way Helm does, and refuses anything
// but the digest it was told to expect. Fetching checks each digest it reads.
func ReadBack(ctx context.Context, target oras.ReadOnlyTarget, reference, digest string) error {
	root, err := target.Resolve(ctx, reference)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", reference, err)
	}
	if root.Digest.String() != digest {
		return fmt.Errorf("%s resolves to %s, not %s", reference, root.Digest, digest)
	}
	raw, err := content.FetchAll(ctx, target, root)
	if err != nil {
		return fmt.Errorf("fetch the chart manifest: %w", err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return fmt.Errorf("decode the chart manifest: %w", err)
	}
	if manifest.Config.MediaType != configMediaType || len(manifest.Layers) != 1 ||
		manifest.Layers[0].MediaType != layerMediaType || manifest.Layers[0].Size > maximumChartBytes {
		return fmt.Errorf("%s is not one chart as Helm reads it", reference)
	}
	if _, err := content.FetchAll(ctx, target, manifest.Layers[0]); err != nil {
		return fmt.Errorf("fetch the chart: %w", err)
	}
	return nil
}
