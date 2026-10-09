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

// imagecopy copies a digest-pinned image, index and all, into another
// registry without changing a byte of it.
//
// The acceptance harness installs from a registry of its own. A docker
// tag-and-push re-encodes what it pushes, so the digest a Pod runs is not the
// digest that was released; this copies the manifest graph as stored, and
// refuses to report success unless the destination resolves to the source
// digest.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

const (
	mediaTypeDockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	maximumIndexBytes           = 4 << 20
)

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Result is what the harness records: where the bytes came from, where they
// went, and the platforms the copied index serves.
type Result struct {
	Source      string   `json:"source"`
	Destination string   `json:"destination"`
	Digest      string   `json:"digest"`
	MediaType   string   `json:"mediaType"`
	Platforms   []string `json:"platforms"`
}

func main() {
	from := flag.String("from", "", "source image as registry/repository@sha256:<digest>")
	to := flag.String("to", "", "destination as registry/repository:tag")
	toPlainHTTP := flag.Bool("to-plain-http", false, "reach the destination registry over plain HTTP")
	toUsername := flag.String("to-username", "", "destination registry user")
	toPasswordFile := flag.String("to-password-file", "", "file holding the destination registry password")
	fromUsername := flag.String("from-username", "", "source registry user; anonymous when empty")
	fromPasswordFile := flag.String("from-password-file", "", "file holding the source registry password")
	platforms := flag.String("require-platforms", "", "comma-separated os/arch the source index must serve")
	timeout := flag.Duration("timeout", 10*time.Minute, "bound on the whole copy")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	result, err := run(ctx, options{
		from:             *from,
		to:               *to,
		toPlainHTTP:      *toPlainHTTP,
		toUsername:       *toUsername,
		toPasswordFile:   *toPasswordFile,
		fromUsername:     *fromUsername,
		fromPasswordFile: *fromPasswordFile,
		platforms:        splitPlatforms(*platforms),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "imagecopy:", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, "imagecopy:", err)
		os.Exit(1)
	}
}

type options struct {
	from, to                       string
	toPlainHTTP                    bool
	toUsername, toPasswordFile     string
	fromUsername, fromPasswordFile string
	platforms                      []string
}

func splitPlatforms(value string) []string {
	var platforms []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			platforms = append(platforms, item)
		}
	}
	return platforms
}

func run(ctx context.Context, opts options) (Result, error) {
	source, err := registry.ParseReference(opts.from)
	if err != nil {
		return Result{}, fmt.Errorf("source %q: %w", opts.from, err)
	}
	if !digestPattern.MatchString(source.Reference) {
		return Result{}, fmt.Errorf("source %q must be pinned by a sha256 digest", opts.from)
	}
	destination, err := registry.ParseReference(opts.to)
	if err != nil {
		return Result{}, fmt.Errorf("destination %q: %w", opts.to, err)
	}
	if destination.Reference == "" || strings.HasPrefix(destination.Reference, "sha256:") {
		return Result{}, fmt.Errorf("destination %q must name a tag", opts.to)
	}

	src, err := repository(source, false, opts.fromUsername, opts.fromPasswordFile)
	if err != nil {
		return Result{}, err
	}
	dst, err := repository(destination, opts.toPlainHTTP, opts.toUsername, opts.toPasswordFile)
	if err != nil {
		return Result{}, err
	}
	result, err := copyPinned(ctx, src, source.Reference, dst, destination.Reference, opts.platforms)
	if err != nil {
		return Result{}, err
	}
	result.Source = opts.from
	result.Destination = destination.Registry + "/" + destination.Repository + "@" + result.Digest
	return result, nil
}

func repository(ref registry.Reference, plainHTTP bool, username, passwordFile string) (*remote.Repository, error) {
	repo, err := remote.NewRepository(ref.Registry + "/" + ref.Repository)
	if err != nil {
		return nil, err
	}
	repo.PlainHTTP = plainHTTP
	client := &auth.Client{Client: retry.DefaultClient, Cache: auth.NewCache()}
	if username != "" || passwordFile != "" {
		if username == "" || passwordFile == "" {
			return nil, fmt.Errorf("%s: a registry credential needs both a user and a password file", ref.Registry)
		}
		password, err := os.ReadFile(passwordFile)
		if err != nil {
			return nil, fmt.Errorf("%s: read password file: %w", ref.Registry, err)
		}
		client.Credential = auth.StaticCredential(ref.Registry, auth.Credential{
			Username: username,
			Password: strings.TrimRight(string(password), "\r\n"),
		})
	}
	repo.Client = client
	return repo, nil
}

// copyPinned copies the graph rooted at srcDigest and tags it dstTag. It reads
// the platforms before copying, so an index that lacks one is refused without
// writing anything, and it resolves the tag afterwards, so a registry that
// stored something else is a failure rather than a reported success.
func copyPinned(ctx context.Context, src oras.ReadOnlyTarget, srcDigest string, dst oras.Target, dstTag string, required []string) (Result, error) {
	root, err := src.Resolve(ctx, srcDigest)
	if err != nil {
		return Result{}, fmt.Errorf("resolve %s: %w", srcDigest, err)
	}
	if root.Digest.String() != srcDigest {
		return Result{}, fmt.Errorf("source resolved %s to %s", srcDigest, root.Digest)
	}
	platforms, err := servedPlatforms(ctx, src, root)
	if err != nil {
		return Result{}, err
	}
	for _, want := range required {
		if !contains(platforms, want) {
			return Result{}, fmt.Errorf("%s serves %s, not %s", srcDigest, strings.Join(platforms, ","), want)
		}
	}
	copied, err := oras.Copy(ctx, src, srcDigest, dst, dstTag, oras.DefaultCopyOptions)
	if err != nil {
		return Result{}, fmt.Errorf("copy %s: %w", srcDigest, err)
	}
	if copied.Digest != root.Digest {
		return Result{}, fmt.Errorf("copy produced %s from %s", copied.Digest, root.Digest)
	}
	stored, err := dst.Resolve(ctx, dstTag)
	if err != nil {
		return Result{}, fmt.Errorf("resolve copied tag %s: %w", dstTag, err)
	}
	if stored.Digest != root.Digest || stored.MediaType != root.MediaType || stored.Size != root.Size {
		return Result{}, fmt.Errorf("destination tag %s resolves to %s (%s, %d bytes), not %s (%s, %d bytes)",
			dstTag, stored.Digest, stored.MediaType, stored.Size, root.Digest, root.MediaType, root.Size)
	}
	return Result{Digest: root.Digest.String(), MediaType: root.MediaType, Platforms: platforms}, nil
}

// servedPlatforms lists the os/arch of every image an index names, leaving out
// the attestation manifests buildx records as unknown/unknown. A single image
// manifest serves the platform its config declares.
func servedPlatforms(ctx context.Context, src content.ReadOnlyStorage, root ocispec.Descriptor) ([]string, error) {
	switch root.MediaType {
	case ocispec.MediaTypeImageIndex, mediaTypeDockerManifestList:
	default:
		return singlePlatform(ctx, src, root)
	}
	if root.Size > maximumIndexBytes {
		return nil, fmt.Errorf("index %s is %d bytes", root.Digest, root.Size)
	}
	raw, err := content.FetchAll(ctx, src, root)
	if err != nil {
		return nil, fmt.Errorf("fetch index %s: %w", root.Digest, err)
	}
	var index ocispec.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		return nil, fmt.Errorf("decode index %s: %w", root.Digest, err)
	}
	var platforms []string
	for _, manifest := range index.Manifests {
		if manifest.Platform == nil || manifest.Platform.OS == "unknown" {
			continue
		}
		platform := manifest.Platform.OS + "/" + manifest.Platform.Architecture
		if manifest.Platform.Variant != "" {
			platform += "/" + manifest.Platform.Variant
		}
		platforms = append(platforms, platform)
	}
	if len(platforms) == 0 {
		return nil, fmt.Errorf("index %s names no platform image", root.Digest)
	}
	sort.Strings(platforms)
	return platforms, nil
}

func singlePlatform(ctx context.Context, src content.ReadOnlyStorage, root ocispec.Descriptor) ([]string, error) {
	raw, err := content.FetchAll(ctx, src, root)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest %s: %w", root.Digest, err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("decode manifest %s: %w", root.Digest, err)
	}
	config, err := content.FetchAll(ctx, src, manifest.Config)
	if err != nil {
		return nil, fmt.Errorf("fetch config %s: %w", manifest.Config.Digest, err)
	}
	var platform ocispec.Platform
	if err := json.Unmarshal(config, &platform); err != nil || platform.OS == "" || platform.Architecture == "" {
		return nil, errors.New("image config names no platform")
	}
	return []string{platform.OS + "/" + platform.Architecture}, nil
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
