// Command unknownlayerfixture publishes a migration artifact carrying one layer
// whose media type the pinned executor does not accept.
//
// It exists because no product command can produce this artifact. `ptah
// migrations push` writes the layers it knows, which is the point of it, so an
// artifact built by a newer publisher than the executor installed here has to
// be assembled directly. What the fixture is imitating is not corruption: it is
// the next version of the format, carrying a layer this executor predates.
//
// The refusal that follows belongs to the executor and not to this command. A
// reader that meets a layer outside its accepted set refuses the whole artifact
// on the layer descriptor, before the bytes are fetched, which is how it fails
// closed rather than reading around what it cannot understand.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	// migrationArtifactType and migrationLayerMediaType are Ptah's, repeated
	// here rather than imported: ptah.run/internal is another module's internal
	// package, so a fixture that needs the exact strings carries them and says
	// where they come from.
	migrationArtifactType   = "application/vnd.stokaro.ptah.migrations.v1"
	migrationLayerMediaType = "application/vnd.stokaro.ptah.migration.file.v1"

	dirFormatAnnotation = "io.stokaro.ptah.migration-format"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "unknownlayerfixture:", err)
		os.Exit(1)
	}
}

func run() error {
	reference := flag.String("reference", "", "registry reference to publish, without a scheme")
	directory := flag.String("dir", "", "directory of migration files to publish")
	unknownMediaType := flag.String("unknown-media-type", "", "media type of the extra layer this executor cannot accept")
	unknownName := flag.String("unknown-name", "capabilities.json", "file name the extra layer carries")
	username := flag.String("username", "", "registry username")
	password := flag.String("password", "", "registry password")
	plainHTTP := flag.Bool("plain-http", false, "talk to an explicitly trusted local registry over HTTP")
	flag.Parse()

	switch {
	case strings.TrimSpace(*reference) == "":
		return errors.New("a reference is required")
	case strings.TrimSpace(*directory) == "":
		return errors.New("a migrations directory is required")
	case strings.TrimSpace(*unknownMediaType) == "":
		return errors.New("an unknown media type is required")
	case *unknownMediaType == migrationLayerMediaType:
		return fmt.Errorf("the extra layer must not be %q, which every executor accepts", migrationLayerMediaType)
	}

	repository, tag, err := splitReference(*reference)
	if err != nil {
		return err
	}
	target, err := remote.NewRepository(repository)
	if err != nil {
		return fmt.Errorf("address the repository: %w", err)
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

	ctx := context.Background()
	layers, err := pushMigrationFiles(ctx, target, *directory)
	if err != nil {
		return err
	}
	unknown, err := pushBlob(ctx, target, *unknownMediaType, *unknownName,
		[]byte(`{"requires":["a capability this executor predates"]}`))
	if err != nil {
		return fmt.Errorf("push the extra layer: %w", err)
	}
	layers = append(layers, unknown)

	manifest, err := oras.PackManifest(ctx, target, oras.PackManifestVersion1_1, migrationArtifactType,
		oras.PackManifestOptions{
			Layers: layers,
			ManifestAnnotations: map[string]string{
				dirFormatAnnotation: "ptah",
			},
		})
	if err != nil {
		return fmt.Errorf("pack the manifest: %w", err)
	}
	if err := target.Tag(ctx, manifest, tag); err != nil {
		return fmt.Errorf("tag the manifest: %w", err)
	}
	fmt.Printf("Digest: %s\n", manifest.Digest.String())
	return nil
}

// pushMigrationFiles publishes the directory the way a real artifact carries
// it: one layer per file, named by its title annotation, in a stable order.
func pushMigrationFiles(ctx context.Context, target oras.Target, directory string) ([]ocispec.Descriptor, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read the migrations directory: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		names = append(names, entry.Name())
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no migration files in %s", directory)
	}
	sort.Strings(names)
	descriptors := make([]ocispec.Descriptor, 0, len(names))
	for _, name := range names {
		contents, readErr := os.ReadFile(filepath.Join(directory, name))
		if readErr != nil {
			return nil, fmt.Errorf("read %s: %w", name, readErr)
		}
		descriptor, pushErr := pushBlob(ctx, target, migrationLayerMediaType, name, contents)
		if pushErr != nil {
			return nil, fmt.Errorf("push %s: %w", name, pushErr)
		}
		descriptors = append(descriptors, descriptor)
	}
	return descriptors, nil
}

func pushBlob(
	ctx context.Context,
	target oras.Target,
	mediaType string,
	name string,
	contents []byte,
) (ocispec.Descriptor, error) {
	descriptor := content.NewDescriptorFromBytes(mediaType, contents)
	descriptor.Annotations = map[string]string{ocispec.AnnotationTitle: name}
	// A rerun against a retained registry republishes the same bytes, and a
	// blob that is already there is not a failure to publish it.
	if err := target.Push(ctx, descriptor, bytes.NewReader(contents)); err != nil &&
		!errors.Is(err, errdef.ErrAlreadyExists) {
		return ocispec.Descriptor{}, err
	}
	return descriptor, nil
}

func splitReference(reference string) (repository string, tag string, err error) {
	index := strings.LastIndex(reference, ":")
	if index <= 0 || index == len(reference)-1 || strings.Contains(reference[index+1:], "/") {
		return "", "", fmt.Errorf("reference %q carries no tag", reference)
	}
	return reference[:index], reference[index+1:], nil
}
