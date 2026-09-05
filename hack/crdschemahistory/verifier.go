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

// Package crdschemahistory verifies that the generated CRD schema identity
// advances monotonically relative to an exact Git commit.
package crdschemahistory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

const (
	defaultCandidateDirectory = "config/crd/bases"
	schemaVersionAnnotation   = "operator.ptah.dev/crd-schema-version"
	schemaDigestAnnotation    = "operator.ptah.dev/crd-schema-digest"
)

var (
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	filePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*[.]yaml$`)
)

// Config selects the repository, generated CRD directory, and Git baseline.
// An empty BaselineRef enables the documented local-only automatic selection.
type Config struct {
	Root                    string
	CandidateDirectory      string
	BaselineRef             string
	RequireExplicitBaseline bool
}

// Result describes the exact history transition that was verified.
type Result struct {
	BaselineCommit   string
	BaselineSource   string
	BaselineVersion  uint64
	CandidateVersion uint64
	SchemaChanged    bool
	InitialAdoption  bool
}

type documentSet struct {
	byName map[string]document
}

type document struct {
	crd            *apiextensionsv1.CustomResourceDefinition
	normalizedSpec []byte
}

type identity struct {
	managed bool
	version uint64
}

// Verify loads the current generated CRDs from the working tree and their
// baseline counterparts from Git, then enforces the monotonic schema contract.
func Verify(ctx context.Context, config Config) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("verify CRD schema history: context is required")
	}
	root := config.Root
	if root == "" {
		root = "."
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return Result{}, fmt.Errorf("verify CRD schema history: resolve repository root: %w", err)
	}
	candidateDirectory, err := repositoryDirectory(config.CandidateDirectory)
	if err != nil {
		return Result{}, fmt.Errorf("verify CRD schema history: %w", err)
	}
	baselineCommit, baselineSource, err := selectBaseline(
		ctx,
		absoluteRoot,
		candidateDirectory,
		config.BaselineRef,
		config.RequireExplicitBaseline,
	)
	if err != nil {
		return Result{}, fmt.Errorf("verify CRD schema history: %w", err)
	}

	candidateFiles, err := readWorkingTreeFiles(absoluteRoot, candidateDirectory)
	if err != nil {
		return Result{}, fmt.Errorf("verify CRD schema history: load candidate CRDs: %w", err)
	}
	baselineFiles, err := readGitFiles(ctx, absoluteRoot, baselineCommit, candidateDirectory)
	if err != nil {
		return Result{}, fmt.Errorf("verify CRD schema history: load baseline CRDs: %w", err)
	}
	candidate, err := decodeSet("candidate", candidateFiles)
	if err != nil {
		return Result{}, fmt.Errorf("verify CRD schema history: %w", err)
	}
	baseline, err := decodeBaselineSet(baselineFiles)
	if err != nil {
		return Result{}, fmt.Errorf("verify CRD schema history: %w", err)
	}

	result, err := evaluateTransition(baseline, candidate)
	if err != nil {
		return Result{}, fmt.Errorf("verify CRD schema history against %s: %w", baselineCommit, err)
	}
	result.BaselineCommit = baselineCommit
	result.BaselineSource = baselineSource
	return result, nil
}

func repositoryDirectory(value string) (string, error) {
	if value == "" {
		value = defaultCandidateDirectory
	}
	if filepath.IsAbs(value) {
		return "", fmt.Errorf("candidate directory %q must be repository-relative", value)
	}
	cleaned := filepath.Clean(value)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("candidate directory %q escapes the repository", value)
	}
	return filepath.ToSlash(cleaned), nil
}

func selectBaseline(
	ctx context.Context,
	root, candidateDirectory, requested string,
	requireExplicit bool,
) (string, string, error) {
	requested = strings.TrimSpace(requested)
	if requested != "" {
		commit, err := resolveCommit(ctx, root, requested)
		if err != nil {
			return "", "", fmt.Errorf("resolve explicit baseline %q: %w", requested, err)
		}
		return commit, "explicit", nil
	}
	if requireExplicit {
		return "", "", errors.New("an explicit baseline is required; set CRD_SCHEMA_BASELINE_REF or pass -baseline-ref")
	}

	dirty, err := generatedDirectoryDirty(ctx, root, candidateDirectory)
	if err != nil {
		return "", "", err
	}
	if dirty {
		commit, err := resolveCommit(ctx, root, "HEAD")
		if err != nil {
			return "", "", fmt.Errorf("resolve HEAD for modified generated CRDs: %w", err)
		}
		return commit, "working-tree-head", nil
	}
	commit, err := resolveCommit(ctx, root, "HEAD^")
	if err != nil {
		return "", "", fmt.Errorf("resolve HEAD^ for clean generated CRDs: %w; pass -baseline-ref explicitly", err)
	}
	return commit, "clean-parent", nil
}

func resolveCommit(ctx context.Context, root, reference string) (string, error) {
	if reference == "" || strings.HasPrefix(reference, "-") || strings.ContainsAny(reference, "\x00\r\n") {
		return "", fmt.Errorf("Git reference %q is invalid", reference)
	}
	output, err := gitOutput(ctx, root, "rev-parse", "--verify", "--end-of-options", reference+"^{commit}")
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(output))
	if !commitPattern.MatchString(commit) {
		return "", fmt.Errorf("Git resolved %q to invalid commit %q", reference, commit)
	}
	return commit, nil
}

func generatedDirectoryDirty(ctx context.Context, root, candidateDirectory string) (bool, error) {
	output, err := gitOutput(
		ctx,
		root,
		"status",
		"--porcelain=v1",
		"--untracked-files=all",
		"-z",
		"--",
		candidateDirectory,
	)
	if err != nil {
		return false, fmt.Errorf("inspect generated CRD working tree: %w", err)
	}
	return len(output) != 0, nil
}

func readWorkingTreeFiles(root, candidateDirectory string) (map[string][]byte, error) {
	directory := filepath.Join(root, filepath.FromSlash(candidateDirectory))
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", candidateDirectory, err)
	}
	files := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		if !filePattern.MatchString(entry.Name()) {
			return nil, fmt.Errorf("%s contains unexpected entry %q", candidateDirectory, entry.Name())
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("inspect %s/%s: %w", candidateDirectory, entry.Name(), err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s/%s is not a regular file", candidateDirectory, entry.Name())
		}
		contents, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s/%s: %w", candidateDirectory, entry.Name(), err)
		}
		files[entry.Name()] = contents
	}
	return files, nil
}

func readGitFiles(ctx context.Context, root, commit, candidateDirectory string) (map[string][]byte, error) {
	output, err := gitOutput(
		ctx,
		root,
		"ls-tree",
		"-r",
		"--name-only",
		"-z",
		commit,
		"--",
		candidateDirectory,
	)
	if err != nil {
		return nil, fmt.Errorf("enumerate %s at %s: %w", candidateDirectory, commit, err)
	}
	paths := splitNUL(output)
	files := make(map[string][]byte, len(paths))
	prefix := candidateDirectory + "/"
	for _, path := range paths {
		if !strings.HasPrefix(path, prefix) {
			return nil, fmt.Errorf("Git returned path %q outside %s", path, candidateDirectory)
		}
		name := strings.TrimPrefix(path, prefix)
		if strings.Contains(name, "/") || !filePattern.MatchString(name) {
			return nil, fmt.Errorf("%s at %s contains unexpected entry %q", candidateDirectory, commit, name)
		}
		contents, err := gitOutput(ctx, root, "show", commit+":"+path)
		if err != nil {
			return nil, fmt.Errorf("read %s at %s: %w", path, commit, err)
		}
		files[name] = contents
	}
	return files, nil
}

func splitNUL(value []byte) []string {
	if len(value) == 0 {
		return nil
	}
	parts := bytes.Split(value, []byte{0})
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) != 0 {
			result = append(result, string(part))
		}
	}
	return result
}

func gitOutput(ctx context.Context, root string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Dir = root
	command.Env = append(os.Environ(), "LC_ALL=C")
	output, err := command.Output()
	if err == nil {
		return output, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		message := strings.TrimSpace(string(exitError.Stderr))
		if len(message) > 4096 {
			message = message[:4096] + "..."
		}
		if message != "" {
			return nil, fmt.Errorf("git %s: %s: %w", arguments[0], message, err)
		}
	}
	return nil, fmt.Errorf("git %s: %w", arguments[0], err)
}

func decodeSet(label string, files map[string][]byte) (documentSet, error) {
	set := documentSet{byName: make(map[string]document, len(files))}
	fileNames := make([]string, 0, len(files))
	for name := range files {
		fileNames = append(fileNames, name)
	}
	sort.Strings(fileNames)
	for _, fileName := range fileNames {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.UnmarshalStrict(files[fileName], crd); err != nil {
			return documentSet{}, fmt.Errorf("decode %s CRD %s: %w", label, fileName, err)
		}
		if crd.APIVersion != apiextensionsv1.SchemeGroupVersion.String() || crd.Kind != "CustomResourceDefinition" {
			return documentSet{}, fmt.Errorf(
				"%s file %s has unexpected type %q %q",
				label,
				fileName,
				crd.APIVersion,
				crd.Kind,
			)
		}
		if _, duplicate := set.byName[crd.Name]; duplicate {
			return documentSet{}, fmt.Errorf("%s CRD name %q is duplicated", label, crd.Name)
		}
		normalized, err := normalizeSpec(crd)
		if err != nil {
			return documentSet{}, fmt.Errorf("normalize %s CRD %s: %w", label, crd.Name, err)
		}
		set.byName[crd.Name] = document{crd: crd, normalizedSpec: normalized}
	}

	expectedNames := requiredCRDNames()
	sort.Strings(expectedNames)
	actualNames := make([]string, 0, len(set.byName))
	for name := range set.byName {
		actualNames = append(actualNames, name)
	}
	sort.Strings(actualNames)
	if !equalStrings(actualNames, expectedNames) {
		return documentSet{}, fmt.Errorf(
			"%s CRD set is %v, want the complete generated set %v; added or removed CRDs require a separately reviewed migration",
			label,
			actualNames,
			expectedNames,
		)
	}
	return set, nil
}

func decodeBaselineSet(files map[string][]byte) (documentSet, error) {
	if len(files) == 0 {
		return documentSet{byName: map[string]document{}}, nil
	}
	return decodeSet("baseline", files)
}

func requiredCRDNames() []string {
	return []string{
		"ptahschemaapprovals.operator.ptah.dev",
		"ptahschemaplans.operator.ptah.dev",
		"ptahschemas.operator.ptah.dev",
	}
}

func evaluateTransition(baseline, candidate documentSet) (Result, error) {
	candidateIdentity, err := validateIdentity("candidate", candidate, false)
	if err != nil {
		return Result{}, err
	}
	if len(baseline.byName) == 0 {
		if candidateIdentity.version != 1 {
			return Result{}, fmt.Errorf(
				"initial bootstrap from an empty CRD baseline requires candidate %s=1, got %d",
				schemaVersionAnnotation,
				candidateIdentity.version,
			)
		}
		return Result{
			CandidateVersion: candidateIdentity.version,
			SchemaChanged:    true,
			InitialAdoption:  true,
		}, nil
	}
	baselineIdentity, err := validateIdentity("baseline", baseline, true)
	if err != nil {
		return Result{}, err
	}

	changed, err := specsChanged(baseline, candidate)
	if err != nil {
		return Result{}, err
	}
	result := Result{
		BaselineVersion:  baselineIdentity.version,
		CandidateVersion: candidateIdentity.version,
		SchemaChanged:    changed,
		InitialAdoption:  !baselineIdentity.managed,
	}
	if !baselineIdentity.managed {
		if candidateIdentity.version != 1 {
			return Result{}, fmt.Errorf(
				"initial adoption from a fully unowned baseline requires candidate %s=1, got %d",
				schemaVersionAnnotation,
				candidateIdentity.version,
			)
		}
		return result, nil
	}
	if changed {
		if candidateIdentity.version <= baselineIdentity.version {
			return Result{}, fmt.Errorf(
				"normalized CRD specs changed, so candidate %s=%d must strictly increase baseline version %d",
				schemaVersionAnnotation,
				candidateIdentity.version,
				baselineIdentity.version,
			)
		}
		return result, nil
	}
	if candidateIdentity.version != baselineIdentity.version {
		return Result{}, fmt.Errorf(
			"normalized CRD specs are unchanged, so candidate %s=%d must equal baseline version %d",
			schemaVersionAnnotation,
			candidateIdentity.version,
			baselineIdentity.version,
		)
	}
	return result, nil
}

func validateIdentity(label string, set documentSet, allowUnmanaged bool) (identity, error) {
	names := make([]string, 0, len(set.byName))
	for name := range set.byName {
		names = append(names, name)
	}
	sort.Strings(names)

	managedCount := 0
	var sharedVersion uint64
	for _, name := range names {
		document := set.byName[name]
		annotations := document.crd.Annotations
		rawVersion, hasVersion := annotations[schemaVersionAnnotation]
		rawDigest, hasDigest := annotations[schemaDigestAnnotation]
		if hasVersion != hasDigest {
			return identity{}, fmt.Errorf(
				"%s CRD %s has an incomplete managed identity; %s and %s must both be present or both be absent",
				label,
				name,
				schemaVersionAnnotation,
				schemaDigestAnnotation,
			)
		}
		if !hasVersion {
			continue
		}
		version, err := parseVersion(rawVersion)
		if err != nil {
			return identity{}, fmt.Errorf("%s CRD %s: %w", label, name, err)
		}
		if err := validateDigest(rawDigest); err != nil {
			return identity{}, fmt.Errorf("%s CRD %s: %w", label, name, err)
		}
		computed := digestSpec(document.normalizedSpec)
		if rawDigest != computed {
			return identity{}, fmt.Errorf(
				"%s CRD %s annotation %s=%q does not match normalized spec digest %q",
				label,
				name,
				schemaDigestAnnotation,
				rawDigest,
				computed,
			)
		}
		if managedCount == 0 {
			sharedVersion = version
		} else if version != sharedVersion {
			return identity{}, fmt.Errorf(
				"%s CRDs do not share one %s: %s has %d, want %d",
				label,
				schemaVersionAnnotation,
				name,
				version,
				sharedVersion,
			)
		}
		managedCount++
	}

	if managedCount == 0 {
		if allowUnmanaged {
			return identity{}, nil
		}
		return identity{}, fmt.Errorf(
			"%s CRDs are missing required managed identity annotations %s and %s",
			label,
			schemaVersionAnnotation,
			schemaDigestAnnotation,
		)
	}
	if managedCount != len(set.byName) {
		return identity{}, fmt.Errorf(
			"%s CRD set has incomplete managed identity: %d of %d CRDs carry the required annotation pair",
			label,
			managedCount,
			len(set.byName),
		)
	}
	return identity{managed: true, version: sharedVersion}, nil
}

func parseVersion(raw string) (uint64, error) {
	if raw == "" || raw[0] < '1' || raw[0] > '9' {
		return 0, fmt.Errorf("annotation %s=%q is not a positive exact decimal version", schemaVersionAnnotation, raw)
	}
	for _, character := range raw[1:] {
		if character < '0' || character > '9' {
			return 0, fmt.Errorf("annotation %s=%q is not a positive exact decimal version", schemaVersionAnnotation, raw)
		}
	}
	version, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("annotation %s=%q is not a positive exact decimal version: %w", schemaVersionAnnotation, raw, err)
	}
	return version, nil
}

func validateDigest(raw string) error {
	if len(raw) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(raw, "sha256:") {
		return fmt.Errorf("annotation %s=%q is not a lowercase SHA-256 digest", schemaDigestAnnotation, raw)
	}
	for _, character := range raw[len("sha256:"):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return fmt.Errorf("annotation %s=%q is not a lowercase SHA-256 digest", schemaDigestAnnotation, raw)
		}
	}
	return nil
}

func normalizeSpec(crd *apiextensionsv1.CustomResourceDefinition) ([]byte, error) {
	if crd == nil {
		return nil, errors.New("CRD is required")
	}
	normalized := &apiextensionsv1.CustomResourceDefinition{Spec: *crd.Spec.DeepCopy()}
	apiextensionsv1.SetObjectDefaults_CustomResourceDefinition(normalized)
	encoded, err := json.Marshal(normalized.Spec)
	if err != nil {
		return nil, fmt.Errorf("encode normalized spec: %w", err)
	}
	return encoded, nil
}

func digestSpec(normalized []byte) string {
	sum := sha256.Sum256(normalized)
	return fmt.Sprintf("sha256:%x", sum)
}

func specsChanged(baseline, candidate documentSet) (bool, error) {
	if len(baseline.byName) != len(candidate.byName) {
		return false, errors.New("baseline and candidate CRD sets differ")
	}
	for name, baselineDocument := range baseline.byName {
		candidateDocument, found := candidate.byName[name]
		if !found {
			return false, fmt.Errorf("candidate CRD set is missing %s", name)
		}
		if !bytes.Equal(baselineDocument.normalizedSpec, candidateDocument.normalizedSpec) {
			return true, nil
		}
	}
	return false, nil
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
