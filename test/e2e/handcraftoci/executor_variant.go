package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
)

const executorVariantAnnotation = "operator.ptah.run/e2e-executor-variant"

var imageDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// The variant changes the immutable image identity without changing the Ptah
// executable or its supported version. This isolates executor-image binding
// from a protocol or version upgrade; it does not qualify either upgrade.
func runExecutorVariant(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: e2e-handcraft-oci executor-variant <repository@sha256:digest>")
	}
	repository, source, ok := strings.Cut(args[0], "@")
	if !ok || !imageDigestPattern.MatchString(source) {
		return errors.New("executor variant requires a digest-pinned image")
	}
	ref, err := parseReference("oci://" + repository + ":execution-binding-proof")
	credential := credentials{username: os.Getenv("PTAH_OCI_USERNAME"), password: os.Getenv("PTAH_OCI_PASSWORD")}
	if err != nil || ref.host != os.Getenv("PTAH_OCI_REGISTRY") || credential.username == "" || credential.password == "" {
		return errors.New("executor variant needs its exact registry and credentials")
	}
	updated, err := publishExecutorVariant(context.Background(), newRegistryClient(nil), ref, credential, source)
	if err != nil {
		return err
	}
	fmt.Printf("Executor: %s@%s\n", repository, updated)
	return nil
}

func executorVariant(raw []byte) ([]byte, string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, "", errors.New("invalid executor manifest")
	}
	var version int
	var media string
	if json.Unmarshal(fields["schemaVersion"], &version) != nil || version != 2 || json.Unmarshal(fields["mediaType"], &media) != nil || len(fields["artifactType"]) != 0 {
		return nil, "", errors.New("executor manifest is not an image")
	}
	switch media {
	case "application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json":
		var manifests []json.RawMessage
		if json.Unmarshal(fields["manifests"], &manifests) != nil || len(manifests) == 0 {
			return nil, "", errors.New("executor index has no platforms")
		}
	case manifestType, "application/vnd.docker.distribution.manifest.v2+json":
		var layers []json.RawMessage
		if len(fields["config"]) == 0 || json.Unmarshal(fields["layers"], &layers) != nil || len(layers) == 0 {
			return nil, "", errors.New("executor image has no configuration or layers")
		}
	default:
		return nil, "", errors.New("unsupported executor image media type")
	}
	annotations := map[string]string{}
	if value, found := fields["annotations"]; found {
		if json.Unmarshal(value, &annotations) != nil || annotations == nil {
			return nil, "", errors.New("invalid executor annotations")
		}
	}
	if _, exists := annotations[executorVariantAnnotation]; exists {
		return nil, "", errors.New("executor variant was already annotated")
	}
	annotations[executorVariantAnnotation] = "execution-binding-proof"
	fields["annotations"], _ = json.Marshal(annotations)
	updated, err := json.Marshal(fields)
	return updated, media, err
}

func readExecutorManifest(ctx context.Context, client *http.Client, ref registryReference, credential credentials, expected string) ([]byte, error) {
	target := fmt.Sprintf("http://%s/v2/%s/manifests/%s", ref.host, ref.repository, expected)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, errors.New("create executor manifest request")
	}
	req.SetBasicAuth(credential.username, credential.password)
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, "+manifestType+", application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json")
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("read executor manifest")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxSchemaBytes+1))
	if err != nil || response.StatusCode != http.StatusOK || len(raw) > maxSchemaBytes || digest(raw) != expected || response.Header.Get("Docker-Content-Digest") != expected {
		return nil, errors.New("executor manifest read-back changed its digest or exceeded its bound")
	}
	return raw, nil
}

func publishExecutorVariant(ctx context.Context, client *http.Client, ref registryReference, credential credentials, source string) (string, error) {
	if !imageDigestPattern.MatchString(source) {
		return "", errors.New("executor source has no digest")
	}
	raw, err := readExecutorManifest(ctx, client, ref, credential, source)
	if err != nil {
		return "", err
	}
	updated, media, err := executorVariant(raw)
	if err != nil {
		return "", err
	}
	result := digest(updated)
	if result == source {
		return "", errors.New("executor variant did not change the image identity")
	}
	target := fmt.Sprintf("http://%s/v2/%s/manifests/%s", ref.host, ref.repository, ref.tag)
	status, _, err := request(ctx, client, http.MethodPut, target, media, credential, updated)
	if err != nil || status != http.StatusCreated {
		return "", errors.New("store executor variant")
	}
	readBack, err := readExecutorManifest(ctx, client, ref, credential, result)
	if err != nil || !bytes.Equal(readBack, updated) {
		return "", errors.New("executor variant read-back did not match")
	}
	return result, nil
}
