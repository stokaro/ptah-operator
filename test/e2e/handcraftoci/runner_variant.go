package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-operator/internal/runner"
)

const (
	runnerVariantAnnotation = "operator.ptah.run/e2e-unsupported-runner"
	maxRunnerFixtureBytes   = 64 << 20
	runnerFixtureScope      = "unsupported runner refusal only; no future protocol compatibility"
	fixtureRunnerPath       = "/e2e-protocol-runner"
)

// Replace /ptah-runner in a private task-registry image with the copied-source
// fixture. The manager, executor and shipping protocol declaration stay fixed.
func runRunnerVariant(arguments []string) error {
	if len(arguments) != 1 {
		return errors.New("usage: e2e-handcraft-oci runner-protocol-variant <repository@sha256:digest>")
	}
	repository, source, ok := strings.Cut(arguments[0], "@")
	ref, err := parseReference("oci://" + repository + ":unsupported-runner-proof")
	credential := credentials{username: os.Getenv("PTAH_OCI_USERNAME"), password: os.Getenv("PTAH_OCI_PASSWORD")}
	if !ok || !imageDigestPattern.MatchString(source) || err != nil || ref.host != os.Getenv("PTAH_OCI_REGISTRY") ||
		credential.username == "" || credential.password == "" {
		return errors.New("runner variant needs its exact digest, registry and credentials")
	}
	binary, err := readBoundedFile(fixtureRunnerPath, maxRunnerFixtureBytes)
	if err != nil {
		return errors.New("read the bounded incompatible runner fixture")
	}
	record, err := readBoundedFile(fixtureRunnerPath+".json", maxResponseBytes)
	if err != nil || validateRunnerFixtureRecord(record) != nil {
		return errors.New("the incompatible runner has no exact source provenance")
	}
	probe := exec.CommandContext(context.Background(), fixtureRunnerPath, "--operation", "apply")
	probe.Env = []string{runner.EnvOperationID + "=fixture-protocol-selftest", runner.EnvRunnerProtocolVersion + "=" + strconv.Itoa(runner.ProtocolVersion)}
	logs, err := probe.Output()
	if err != nil {
		return errors.New("the incompatible runner did not complete its refusal transport")
	}
	_, err = runner.ParseResultFor(logs, runner.OperationApply, "fixture-protocol-selftest")
	var mismatch *runner.ProtocolMismatchError
	if !errors.As(err, &mismatch) || mismatch.RunnerVersion != runner.ProtocolVersion+1 ||
		mismatch.Message != fmt.Sprintf("the Job expects runner protocol %d; this runner speaks protocol %d", runner.ProtocolVersion, runner.ProtocolVersion+1) {
		return errors.New("the actual runner fixture did not return the pinned foreign refusal")
	}
	updated, err := publishRunnerVariant(context.Background(), newRegistryClient(nil), ref, credential, source, binary, record, runtime.GOARCH)
	if err != nil {
		return err
	}
	fmt.Printf("Runner: %s@%s\n", repository, updated)
	return nil
}

func validateRunnerFixtureRecord(raw []byte) error {
	var record struct {
		SupportedProtocol, RefusedProtocol int
		SourceSHA256, OverlaySHA256, Scope string
	}
	if json.Unmarshal(raw, &record) != nil || record.SupportedProtocol != runner.ProtocolVersion || record.RefusedProtocol != runner.ProtocolVersion+1 ||
		!imageDigestPattern.MatchString("sha256:"+record.SourceSHA256) || !imageDigestPattern.MatchString("sha256:"+record.OverlaySHA256) ||
		record.SourceSHA256 == record.OverlaySHA256 || record.Scope != runnerFixtureScope {
		return errors.New("runner fixture provenance does not name the exact supported and refused source contracts")
	}
	return nil
}

func runnerFixtureLayer(binary []byte, architecture string) (compressed []byte, diffID string, err error) {
	if len(binary) == 0 || len(binary) > maxRunnerFixtureBytes {
		return nil, "", errors.New("the runner fixture is empty or over its byte bound")
	}
	parsed, err := elf.NewFile(bytes.NewReader(binary))
	if err != nil {
		return nil, "", errors.New("the runner fixture is not a Linux executable")
	}
	defer parsed.Close()
	wanted := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}
	machine, ok := wanted[architecture]
	if !ok || parsed.Class != elf.ELFCLASS64 || parsed.Machine != machine || (parsed.Type != elf.ET_EXEC && parsed.Type != elf.ET_DYN) {
		return nil, "", errors.New("the runner fixture does not match its image architecture")
	}
	var raw bytes.Buffer
	archive := tar.NewWriter(&raw)
	if err := archive.WriteHeader(&tar.Header{Name: "ptah-runner", Mode: 0o755, Uid: 65532, Gid: 65532, Size: int64(len(binary)), Typeflag: tar.TypeReg}); err != nil {
		return nil, "", err
	}
	if _, err := archive.Write(binary); err != nil {
		return nil, "", err
	}
	if err := archive.Close(); err != nil {
		return nil, "", err
	}
	var encoded bytes.Buffer
	compression := gzip.NewWriter(&encoded)
	if _, err := compression.Write(raw.Bytes()); err != nil {
		return nil, "", err
	}
	if err := compression.Close(); err != nil {
		return nil, "", err
	}
	return encoded.Bytes(), digest(raw.Bytes()), nil
}

type runnerVariantContents struct {
	manifest, config, layer []byte
	configDescriptor        descriptor
	layerDescriptor         descriptor
	media                   string
}

func runnerVariant(source, config, binary, record []byte, architecture string) (runnerVariantContents, error) {
	var failed runnerVariantContents
	if err := validateRunnerFixtureRecord(record); err != nil {
		return failed, err
	}
	var fields map[string]json.RawMessage
	var original manifest
	if json.Unmarshal(source, &fields) != nil || json.Unmarshal(source, &original) != nil || original.SchemaVersion != 2 || original.ArtifactType != "" ||
		(original.MediaType != manifestType && original.MediaType != "application/vnd.docker.distribution.manifest.v2+json") ||
		len(original.Layers) == 0 || original.Config.Size != len(config) || original.Config.Digest != digest(config) {
		return failed, errors.New("runner source must be one exact platform image with its verified configuration")
	}
	if _, exists := original.Annotations[runnerVariantAnnotation]; exists {
		return failed, errors.New("a runner variant cannot be used as the shipping source")
	}
	for _, layer := range original.Layers {
		if !imageDigestPattern.MatchString(layer.Digest) || layer.Size < 1 {
			return failed, errors.New("the runner source has an invalid retained layer")
		}
	}
	var configuration map[string]json.RawMessage
	var platform struct{ OS, Architecture string }
	var rootFS struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	}
	var rootFSFields map[string]json.RawMessage
	if json.Unmarshal(config, &configuration) != nil || json.Unmarshal(config, &platform) != nil || platform.OS != "linux" || platform.Architecture != architecture ||
		json.Unmarshal(configuration["rootfs"], &rootFS) != nil || rootFS.Type != "layers" || len(rootFS.DiffIDs) != len(original.Layers) {
		return failed, errors.New("the runner fixture must match the source image platform and complete filesystem")
	}
	if json.Unmarshal(configuration["rootfs"], &rootFSFields) != nil {
		return failed, errors.New("the source filesystem metadata is invalid")
	}
	for _, id := range rootFS.DiffIDs {
		if !imageDigestPattern.MatchString(id) {
			return failed, errors.New("the runner source has an invalid filesystem layer digest")
		}
	}
	layer, diffID, err := runnerFixtureLayer(binary, architecture)
	if err != nil {
		return failed, err
	}
	rootFS.DiffIDs = append(rootFS.DiffIDs, diffID)
	rootFSFields["diff_ids"], _ = json.Marshal(rootFS.DiffIDs)
	configuration["rootfs"], _ = json.Marshal(rootFSFields)
	updatedConfig, err := json.Marshal(configuration)
	if err != nil {
		return failed, errors.New("encode the runner variant's filesystem configuration")
	}
	configDescriptor := original.Config
	configDescriptor.Digest, configDescriptor.Size = digest(updatedConfig), len(updatedConfig)
	layerDescriptor := descriptor{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: digest(layer), Size: len(layer)}
	if original.MediaType == "application/vnd.docker.distribution.manifest.v2+json" {
		layerDescriptor.MediaType = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	}
	fields["config"], _ = json.Marshal(configDescriptor)
	fields["layers"], _ = json.Marshal(append(original.Layers, layerDescriptor))
	if original.Annotations == nil {
		original.Annotations = map[string]string{}
	}
	original.Annotations[runnerVariantAnnotation] = "protocol=" + strconv.Itoa(runner.ProtocolVersion+1) + ";unsupported-refusal-only"
	original.Annotations[runnerVariantAnnotation+"-provenance"] = digest(record)
	fields["annotations"], _ = json.Marshal(original.Annotations)
	updatedManifest, err := json.Marshal(fields)
	if err != nil {
		return failed, errors.New("encode the explicitly synthetic runner manifest")
	}
	return runnerVariantContents{manifest: updatedManifest, config: updatedConfig, layer: layer,
		configDescriptor: configDescriptor, layerDescriptor: layerDescriptor, media: original.MediaType}, nil
}

func readRunnerBlob(ctx context.Context, registry *http.Client, ref registryReference, credential credentials, expected descriptor) ([]byte, error) {
	if !imageDigestPattern.MatchString(expected.Digest) || expected.Size < 1 || expected.Size > maxRunnerFixtureBytes {
		return nil, errors.New("the runner blob has no bounded exact identity")
	}
	target := fmt.Sprintf("http://%s/v2/%s/blobs/%s", ref.host, ref.repository, expected.Digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, errors.New("create runner blob request")
	}
	req.SetBasicAuth(credential.username, credential.password)
	response, err := registry.Do(req)
	if err != nil {
		return nil, errors.New("read runner image blob")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, int64(expected.Size)+1))
	if err != nil || response.StatusCode != http.StatusOK || len(raw) != expected.Size || digest(raw) != expected.Digest {
		return nil, errors.New("runner blob read-back changed its size or digest")
	}
	return raw, nil
}

func publishRunnerVariant(ctx context.Context, registry *http.Client, ref registryReference, credential credentials, source string,
	binary, record []byte, architecture string,
) (string, error) {
	raw, err := readExecutorManifest(ctx, registry, ref, credential, source)
	if err != nil {
		return "", err
	}
	var original manifest
	if json.Unmarshal(raw, &original) != nil || original.Config.Size > maxSchemaBytes {
		return "", errors.New("the runner source configuration is absent or over its bound")
	}
	config, err := readRunnerBlob(ctx, registry, ref, credential, original.Config)
	if err != nil {
		return "", err
	}
	variant, err := runnerVariant(raw, config, binary, record, architecture)
	if err != nil {
		return "", err
	}
	for _, blob := range []struct {
		descriptor descriptor
		contents   []byte
	}{{variant.configDescriptor, variant.config}, {variant.layerDescriptor, variant.layer}} {
		if err := uploadBlob(ctx, registry, ref, credential, blob.descriptor, blob.contents); err != nil {
			return "", err
		}
		readBack, err := readRunnerBlob(ctx, registry, ref, credential, blob.descriptor)
		if err != nil || !bytes.Equal(readBack, blob.contents) {
			return "", errors.New("runner variant blob read-back did not match its exact bytes")
		}
	}
	result := digest(variant.manifest)
	target := fmt.Sprintf("http://%s/v2/%s/manifests/%s", ref.host, ref.repository, ref.tag)
	status, _, err := request(ctx, registry, http.MethodPut, target, variant.media, credential, variant.manifest)
	if err != nil || status != http.StatusCreated {
		return "", errors.New("store the unsupported runner manifest")
	}
	readBack, err := readExecutorManifest(ctx, registry, ref, credential, result)
	if err != nil || !bytes.Equal(readBack, variant.manifest) {
		return "", errors.New("unsupported runner manifest read-back did not match")
	}
	return result, nil
}
