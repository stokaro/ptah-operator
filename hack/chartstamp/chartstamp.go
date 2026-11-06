// Package chartstamp derives the chart a release publishes to its OCI registry
// from the chart asset the acceptance matrix installed.
//
// The derivation is a function of the asset's bytes and of the identities the
// release manifest records, and it reads no clock, so anyone holding those can
// compute the published chart and its registry digest offline. The release
// records that digest in its manifest before the chart reaches a registry, and
// every later run computes it again.
//
// Only Chart.yaml changes. The stamp is three annotations the chart's templates
// read beside the values, and every other entry keeps its bytes and its header,
// so the templates that run are the ones the matrix ran.
package chartstamp

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gopkg.in/yaml.v3"
	"oras.land/oras-go/v2/content"
)

const (
	// Repository is where a release publishes the chart.
	Repository = "ghcr.io/stokaro/charts/ptah-operator"

	// The annotations the chart's templates read. A chart from the source tree
	// carries none of them, which is what makes it refuse to install without
	// explicit values.
	AnnotationImage       = "operator.ptah.run/stamped-image"
	AnnotationExecutor    = "operator.ptah.run/stamped-executor-image"
	AnnotationPtahVersion = "operator.ptah.run/stamped-ptah-version"
	// AnnotationPrefix is what no chart from the source tree may carry.
	AnnotationPrefix = "operator.ptah.run/stamped-"

	// The media types Helm requires of a chart in an OCI registry.
	configMediaType = "application/vnd.cncf.helm.config.v1+json"
	layerMediaType  = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	sourceURL = "https://github.com/stokaro/ptah-operator"
	// A bound on what the asset may inflate to. The chart is about a megabyte
	// unpacked; nothing near this is a chart this repository packaged.
	maximumChartBytes = 64 << 20
)

var (
	pinnedImage  = regexp.MustCompile(`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`)
	sha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// The version a release's manifest may carry, as releaseverify holds it.
	ptahVersion = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,127}$`)
	revision    = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Release is what a release manifest says about the chart's defaults.
type Release struct {
	// Image is the manager and runner image, pinned by digest.
	Image string
	// Executor is the Ptah image, pinned by digest.
	Executor string
	// PtahVersion is the version verified for Executor.
	PtahVersion string
	// Revision is the commit the chart asset was packaged from.
	Revision string
}

func (r Release) validate() error {
	if !pinnedImage.MatchString(r.Image) {
		return fmt.Errorf("image %q is not pinned with @sha256:<64 lowercase hex>", r.Image)
	}
	if !pinnedImage.MatchString(r.Executor) {
		return fmt.Errorf("executor %q is not pinned with @sha256:<64 lowercase hex>", r.Executor)
	}
	if !ptahVersion.MatchString(r.PtahVersion) {
		return fmt.Errorf("ptah version %q is not a version a release manifest may carry", r.PtahVersion)
	}
	if !revision.MatchString(r.Revision) {
		return fmt.Errorf("revision %q is not a full commit", r.Revision)
	}
	return nil
}

// Artifact is the chart as an OCI registry holds it.
type Artifact struct {
	// Chart is the stamped package, which is the artifact's only layer.
	Chart []byte
	// Config is the chart's metadata as Helm stores it.
	Config []byte
	// Manifest is the bytes the registry serves under Digest.
	Manifest []byte
	// Digest is what the release manifest records and what is signed and attested.
	Digest string
	// Name and Version are the chart's own.
	Name, Version string
}

// Reference is where a published release serves the artifact.
func (a Artifact) Reference() string {
	return Repository + "@" + a.Digest
}

// Build derives the published artifact from a packaged chart.
func Build(asset []byte, release Release) (Artifact, error) {
	if err := release.validate(); err != nil {
		return Artifact{}, err
	}
	stamped, metadata, modified, err := stamp(asset, release)
	if err != nil {
		return Artifact{}, err
	}
	config, err := json.Marshal(metadata)
	if err != nil {
		return Artifact{}, fmt.Errorf("encode the chart config: %w", err)
	}
	annotations := map[string]string{
		ocispec.AnnotationTitle:       metadata.Name,
		ocispec.AnnotationVersion:     metadata.Version,
		ocispec.AnnotationDescription: metadata.Description,
		ocispec.AnnotationCreated:     modified.UTC().Format(time.RFC3339),
		ocispec.AnnotationSource:      sourceURL,
		ocispec.AnnotationRevision:    release.Revision,
	}
	for key, value := range metadata.Annotations {
		annotations[key] = value
	}
	manifest, err := json.Marshal(ocispec.Manifest{
		Versioned:   specs.Versioned{SchemaVersion: 2},
		MediaType:   ocispec.MediaTypeImageManifest,
		Config:      content.NewDescriptorFromBytes(configMediaType, config),
		Layers:      []ocispec.Descriptor{content.NewDescriptorFromBytes(layerMediaType, stamped)},
		Annotations: annotations,
	})
	if err != nil {
		return Artifact{}, fmt.Errorf("encode the chart manifest: %w", err)
	}
	return Artifact{
		Chart:    stamped,
		Config:   config,
		Manifest: manifest,
		Digest:   content.NewDescriptorFromBytes(ocispec.MediaTypeImageManifest, manifest).Digest.String(),
		Name:     metadata.Name,
		Version:  metadata.Version,
	}, nil
}

// chartMetadata is Chart.yaml as Helm stores it in an artifact's config.
type chartMetadata struct {
	Name        string            `yaml:"name" json:"name,omitempty"`
	Version     string            `yaml:"version" json:"version,omitempty"`
	Description string            `yaml:"description" json:"description,omitempty"`
	APIVersion  string            `yaml:"apiVersion" json:"apiVersion,omitempty"`
	AppVersion  string            `yaml:"appVersion" json:"appVersion,omitempty"`
	Annotations map[string]string `yaml:"annotations" json:"annotations,omitempty"`
	KubeVersion string            `yaml:"kubeVersion" json:"kubeVersion,omitempty"`
	Type        string            `yaml:"type" json:"type,omitempty"`
}

type archiveEntry struct {
	header  tar.Header
	content []byte
}

// stamp rewrites the package with its Chart.yaml annotated. It keeps the gzip
// header and every tar header it read, so the bytes that differ are the ones
// that follow from Chart.yaml.
func stamp(asset []byte, release Release) ([]byte, chartMetadata, time.Time, error) {
	compressed, err := gzip.NewReader(bytes.NewReader(asset))
	if err != nil {
		return nil, chartMetadata{}, time.Time{}, fmt.Errorf("read the chart package: %w", err)
	}
	gzipHeader := compressed.Header
	entries, err := readEntries(compressed)
	if err != nil {
		return nil, chartMetadata{}, time.Time{}, err
	}
	if len(entries) == 0 {
		return nil, chartMetadata{}, time.Time{}, errors.New("the chart package holds nothing")
	}
	root, _, _ := strings.Cut(entries[0].header.Name, "/")
	index := -1
	for position, entry := range entries {
		if entry.header.Name == root+"/Chart.yaml" {
			index = position
		}
	}
	if index < 0 {
		return nil, chartMetadata{}, time.Time{}, errors.New("the chart package holds no Chart.yaml")
	}
	annotated, metadata, err := annotate(entries[index].content, release)
	if err != nil {
		return nil, chartMetadata{}, time.Time{}, err
	}
	if metadata.Name != root {
		return nil, chartMetadata{}, time.Time{}, fmt.Errorf("Chart.yaml names %q inside a package rooted at %q", metadata.Name, root)
	}
	entries[index].content = annotated
	entries[index].header.Size = int64(len(annotated))

	var output bytes.Buffer
	writer, err := gzip.NewWriterLevel(&output, gzip.BestCompression)
	if err != nil {
		return nil, chartMetadata{}, time.Time{}, fmt.Errorf("create the gzip writer: %w", err)
	}
	writer.Header = gzip.Header{ModTime: gzipHeader.ModTime, OS: gzipHeader.OS}
	archive := tar.NewWriter(writer)
	for _, entry := range entries {
		header := entry.header
		if err := archive.WriteHeader(&header); err != nil {
			return nil, chartMetadata{}, time.Time{}, fmt.Errorf("write %s: %w", entry.header.Name, err)
		}
		if _, err := archive.Write(entry.content); err != nil {
			return nil, chartMetadata{}, time.Time{}, fmt.Errorf("write %s: %w", entry.header.Name, err)
		}
	}
	if err := archive.Close(); err != nil {
		return nil, chartMetadata{}, time.Time{}, fmt.Errorf("finish the chart package: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, chartMetadata{}, time.Time{}, fmt.Errorf("finish the chart package: %w", err)
	}
	return output.Bytes(), metadata, gzipHeader.ModTime, nil
}

func readEntries(compressed io.Reader) ([]archiveEntry, error) {
	bounded := &io.LimitedReader{R: compressed, N: maximumChartBytes + 1}
	archive := tar.NewReader(bounded)
	var entries []archiveEntry
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return entries, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read the chart package: %w", err)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeDir {
			return nil, fmt.Errorf("the chart package holds %s, which is neither a file nor a directory", header.Name)
		}
		body, err := io.ReadAll(archive)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", header.Name, err)
		}
		if bounded.N <= 0 {
			return nil, fmt.Errorf("the chart package inflates past %d bytes", maximumChartBytes)
		}
		entries = append(entries, archiveEntry{header: *header, content: body})
	}
}

// annotate adds the stamp under Chart.yaml's annotations key. It edits the text
// rather than marshaling the parsed document, so nothing else in the file moves,
// and it parses the result to prove the edit landed where it was meant to.
func annotate(document []byte, release Release) ([]byte, chartMetadata, error) {
	var before chartMetadata
	if err := yaml.Unmarshal(document, &before); err != nil {
		return nil, chartMetadata{}, fmt.Errorf("parse Chart.yaml: %w", err)
	}
	for key := range before.Annotations {
		if strings.HasPrefix(key, AnnotationPrefix) {
			return nil, chartMetadata{}, fmt.Errorf("Chart.yaml already carries %s, so this chart was stamped before", key)
		}
	}
	const annotationsKey = "\nannotations:\n"
	if count := bytes.Count(document, []byte(annotationsKey)); count != 1 {
		return nil, chartMetadata{}, fmt.Errorf("Chart.yaml has %d top-level annotations keys in block form, want 1", count)
	}
	stampLines := fmt.Sprintf("  %s: %q\n  %s: %q\n  %s: %q\n",
		AnnotationImage, release.Image,
		AnnotationExecutor, release.Executor,
		AnnotationPtahVersion, release.PtahVersion)
	annotated := bytes.Replace(document, []byte(annotationsKey), []byte(annotationsKey+stampLines), 1)

	var after chartMetadata
	if err := yaml.Unmarshal(annotated, &after); err != nil {
		return nil, chartMetadata{}, fmt.Errorf("parse the stamped Chart.yaml: %w", err)
	}
	if after.Annotations[AnnotationImage] != release.Image || after.Annotations[AnnotationExecutor] != release.Executor ||
		after.Annotations[AnnotationPtahVersion] != release.PtahVersion || len(after.Annotations) != len(before.Annotations)+3 {
		return nil, chartMetadata{}, fmt.Errorf("the stamped Chart.yaml reads its annotations as %v", after.Annotations)
	}
	if after.Name == "" || after.Version == "" {
		return nil, chartMetadata{}, errors.New("Chart.yaml names no chart or version")
	}
	return annotated, after, nil
}
