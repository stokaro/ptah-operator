// Command unparsableschemafixture publishes a schema artifact the executor cannot
// parse.
//
// It exists because no product command can produce this artifact: `ptah schema
// push` parses a file before it publishes it. What the fixture imitates is a
// schema published by a newer Ptah than the executor, in a syntax this one does
// not read. The artifact passes `oci resolve` and `oci inspect`, which read the
// manifest, and `schema pull --skip-parse`, which checks the layer's type, size,
// digest and name and writes it unparsed. That pull runs in the fetch step of an
// Observe or a Plan, so the run goes on to the operation container, and its
// first read of the file is where the run ends.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

const (
	// These are Ptah's, repeated here rather than imported: ptah.run/internal is
	// another module's internal package, so a fixture that needs the exact
	// strings carries them and says where they come from.
	schemaArtifactType     = "application/vnd.stokaro.ptah.schema.v1"
	schemaLayerMediaType   = "application/vnd.stokaro.ptah.schema.hcl.v1"
	schemaFormatAnnotation = "io.stokaro.ptah.schema-format"
	schemaLayerName        = "schema.hcl"

	// unparsableSchema has two values where the block allows one.
	unparsableSchema = `table "t" {
  schema = schema.public
  column "id" { type = bigint "a second value" }
}
`
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "unparsableschemafixture:", err)
		os.Exit(1)
	}
}

func run() error {
	reference := flag.String("reference", "", "registry reference to publish, without a scheme")
	username := flag.String("username", "", "registry username")
	password := flag.String("password", "", "registry password")
	plainHTTP := flag.Bool("plain-http", false, "talk to an explicitly trusted local registry over HTTP")
	flag.Parse()

	if strings.TrimSpace(*reference) == "" {
		return errors.New("a reference is required")
	}
	target, err := remote.NewRepository(*reference)
	if err != nil {
		return fmt.Errorf("address the repository: %w", err)
	}
	tag := target.Reference.Reference
	if tag == "" {
		return fmt.Errorf("the reference %q names no tag", *reference)
	}
	target.PlainHTTP = *plainHTTP
	if *username != "" || *password != "" {
		target.Client = &auth.Client{
			Client: retry.DefaultClient,
			Cache:  auth.NewCache(),
			Credential: auth.StaticCredential(target.Reference.Registry, auth.Credential{
				Username: *username,
				Password: *password,
			}),
		}
	}
	manifest, err := publish(context.Background(), target, tag)
	if err != nil {
		return err
	}
	fmt.Printf("Digest: %s\n", manifest.Digest.String())
	return nil
}

// publish writes the artifact the way a real one is laid out: one layer named by
// its title annotation, and the manifest naming the schema's format.
func publish(ctx context.Context, target oras.Target, tag string) (ocispec.Descriptor, error) {
	contents := []byte(unparsableSchema)
	layer := content.NewDescriptorFromBytes(schemaLayerMediaType, contents)
	layer.Annotations = map[string]string{ocispec.AnnotationTitle: schemaLayerName}
	// A rerun against a retained registry republishes the same bytes, and a blob
	// that is already there is not a failure to publish it.
	if err := target.Push(ctx, layer, bytes.NewReader(contents)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return ocispec.Descriptor{}, fmt.Errorf("push the layer: %w", err)
	}
	manifest, err := oras.PackManifest(ctx, target, oras.PackManifestVersion1_1, schemaArtifactType, oras.PackManifestOptions{
		Layers:              []ocispec.Descriptor{layer},
		ManifestAnnotations: map[string]string{schemaFormatAnnotation: "hcl"},
	})
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("pack the manifest: %w", err)
	}
	if err := target.Tag(ctx, manifest, tag); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("tag the manifest: %w", err)
	}
	return manifest, nil
}
