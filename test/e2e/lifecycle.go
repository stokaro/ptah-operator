package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The objects the upgrade phase creates in the proof namespace, whose
// preservation every CRD change in the lifecycle suite is held to.
const (
	lifecycleProofSchema   = "crd-upgrade-proof"
	lifecycleProofPlan     = "crd-upgrade-proof"
	lifecycleProofApproval = "crd-upgrade-proof"
)

// lifecycleManagedCRDs are the CRDs the lifecycle proofs annotate, drift and
// hold unchanged, in the order the scripts walked them.
var lifecycleManagedCRDs = []string{
	"ptahschemas.operator.ptah.run",
	"ptahschemaplans.operator.ptah.run",
	"ptahschemaapprovals.operator.ptah.run",
}

// lifecycleProofResources are the proof objects by the resource name the
// scripts used, with the kind each one is.
var lifecycleProofResources = []struct{ resource, kind string }{
	{"ptahschema", "PtahSchema"},
	{"ptahschemaplan", "PtahSchemaPlan"},
	{"ptahschemaapproval", "PtahSchemaApproval"},
}

var (
	lifecycleDNSLabel        = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	lifecycleExactVersion    = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	lifecycleImageRepository = regexp.MustCompile(`^[^[:space:]@]+$`)
	lifecycleImageDigest     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	lifecycleImageIdentity   = regexp.MustCompile(`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`)
	lifecyclePositiveDecimal = regexp.MustCompile(`^[1-9][0-9]*$`)
)

// lifecycleProofNamespaceOK is the check the script made on
// E2E_PROOF_NAMESPACE: a DNS-1123 label.
func lifecycleProofNamespaceOK(namespace string) error {
	if !lifecycleDNSLabel.MatchString(namespace) {
		return errors.New("E2E_PROOF_NAMESPACE must be a DNS-1123 label")
	}
	if len(namespace) > 63 {
		return errors.New("E2E_PROOF_NAMESPACE must not exceed 63 characters")
	}
	return nil
}

// lifecycleKubernetesMajorMinor reads an exact major.minor.patch version and
// returns its major.minor.
func lifecycleKubernetesMajorMinor(version string) (string, error) {
	if !lifecycleExactVersion.MatchString(version) {
		return "", errors.New("E2E_KUBERNETES_VERSION must be an exact major.minor.patch version")
	}
	return version[:strings.LastIndex(version, ".")], nil
}

// lifecycleServerVersionMatches is the case the script made on the server's
// gitVersion: the exact version, or the exact version with a suffix.
func lifecycleServerVersionMatches(serverVersion, expected string) bool {
	return serverVersion == "v"+expected || strings.HasPrefix(serverVersion, "v"+expected+"-")
}

// lifecycleCandidateCRDSchemaVersion reads the CRD schema version the
// generated CRD is stamped with: the first line whose first field is the
// annotation key, quotes removed, as the script's awk read it. The stamp is a
// positive exact decimal.
func lifecycleCandidateCRDSchemaVersion(crd []byte) (int64, error) {
	scanner := bufio.NewScanner(bytes.NewReader(crd))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || fields[0] != "operator.ptah.run/crd-schema-version:" {
			continue
		}
		value := ""
		if len(fields) > 1 {
			value = strings.ReplaceAll(fields[1], `"`, "")
		}
		if !lifecyclePositiveDecimal.MatchString(value) {
			return 0, errors.New("candidate CRD schema version is not a positive exact decimal")
		}
		return strconv.ParseInt(value, 10, 64)
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("candidate CRD schema version is not a positive exact decimal")
}

// lifecycleProductionControllerImage is production_controller_image_from_values:
// the release values carry exactly one production controller image, an object
// with a repository, a digest, and neither of the two test-only switches, and
// the identity is repository@digest.
func lifecycleProductionControllerImage(values []byte) (string, error) {
	refusal := errors.New("release values do not contain one exact production controller image identity")
	var document map[string]any
	if err := json.Unmarshal(values, &document); err != nil {
		return "", refusal
	}
	image, ok := document["image"].(map[string]any)
	if !ok {
		return "", refusal
	}
	repository, repositoryOK := image["repository"].(string)
	digest, digestOK := image["digest"].(string)
	if !repositoryOK || !lifecycleImageRepository.MatchString(repository) ||
		!digestOK || !lifecycleImageDigest.MatchString(digest) {
		return "", refusal
	}
	if _, mutable := image["allowMutableTag"]; mutable {
		return "", refusal
	}
	if _, test := image["testIdentityDigest"]; test {
		return "", refusal
	}
	identity := repository + "@" + digest
	if !lifecycleImageIdentity.MatchString(identity) {
		return "", errors.New("release values produced an invalid production controller image identity")
	}
	return identity, nil
}

// lifecycleImageIdentityOK is an exact repository-and-digest identity.
func lifecycleImageIdentityOK(image string) bool {
	return lifecycleImageIdentity.MatchString(image)
}

// lifecycleRenderedHookJobNames is rendered_hook_job_name: the names of the
// Jobs in a rendered manifest whose component label and hook weight are the
// ones given. It reads the render line by line as the script's awk did, so a
// document it cannot parse as YAML still yields what the chart wrote.
func lifecycleRenderedHookJobNames(render []byte, component, weight string) []string {
	var names []string
	isJob, name, gotComponent, gotWeight := false, "", "", ""
	emit := func() {
		if isJob && name != "" && gotComponent == component && gotWeight == weight {
			names = append(names, name)
		}
	}
	unquote := func(value string) string {
		return strings.TrimSuffix(strings.TrimPrefix(value, `"`), `"`)
	}
	secondField := func(line string) string {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return ""
		}
		return fields[1]
	}
	for line := range strings.SplitSeq(string(render), "\n") {
		switch {
		case line == "---":
			emit()
			isJob, name, gotComponent, gotWeight = false, "", "", ""
		case line == "kind: Job":
			isJob = true
		case isJob && strings.HasPrefix(line, "  name: ") && name == "":
			name = unquote(secondField(line))
		case isJob && strings.HasPrefix(line, "    helm.sh/hook-weight: "):
			gotWeight = unquote(secondField(line))
		case isJob && strings.HasPrefix(line, "    app.kubernetes.io/component: "):
			gotComponent = unquote(secondField(line))
		}
	}
	emit()
	return names
}

// lifecycleReconcileHookName is prepare_expected_hook_names' check: exactly one
// weight-0 crd-manager hook Job, whose name is a DNS-1123 label of at most 63
// characters.
func lifecycleReconcileHookName(render []byte) (string, error) {
	matches := lifecycleRenderedHookJobNames(render, "crd-manager", "0")
	if len(matches) != 1 {
		return "", errors.New("candidate render does not contain exactly one weight-0 reconcile hook Job")
	}
	name := matches[0]
	if !lifecycleDNSLabel.MatchString(name) {
		return "", errors.New("candidate render contains an invalid reconcile hook Job name")
	}
	if len(name) > 63 {
		return "", errors.New("candidate render contains an overlong reconcile hook Job name")
	}
	return name, nil
}

// lifecycleObjectEvidence is object_evidence: an object's UID, spec and
// status, the status {} when it has none. encoding/json writes map keys in
// order, which is what jq -S did, so two readings of an unchanged object are
// the same bytes.
func lifecycleObjectEvidence(object map[string]any) ([]byte, error) {
	metadata, _ := object["metadata"].(map[string]any)
	status := object["status"]
	if status == nil {
		status = map[string]any{}
	}
	return json.Marshal(map[string]any{
		"uid": metadataValue(metadata, "uid"), "spec": object["spec"], "status": status,
	})
}

// lifecycleCRDEvidence is crd_evidence: a CRD's UID, resourceVersion,
// annotations ({} when it has none) and spec.
func lifecycleCRDEvidence(crd map[string]any) ([]byte, error) {
	metadata, _ := crd["metadata"].(map[string]any)
	annotations := metadataValue(metadata, "annotations")
	if annotations == nil {
		annotations = map[string]any{}
	}
	return json.Marshal(map[string]any{
		"uid": metadataValue(metadata, "uid"), "resourceVersion": metadataValue(metadata, "resourceVersion"),
		"annotations": annotations, "spec": crd["spec"],
	})
}

// lifecycleDeploymentEvidence is deployment_evidence: every Deployment's name,
// UID, generation, labels, annotations, owner references and spec, by name.
func lifecycleDeploymentEvidence(deployments []map[string]any) ([]byte, error) {
	entries := make([]map[string]any, 0, len(deployments))
	for _, deployment := range deployments {
		metadata, _ := deployment["metadata"].(map[string]any)
		entry := map[string]any{
			"name": metadataValue(metadata, "name"), "uid": metadataValue(metadata, "uid"),
			"generation": metadataValue(metadata, "generation"), "spec": deployment["spec"],
		}
		for key, empty := range map[string]any{
			"labels": map[string]any{}, "annotations": map[string]any{}, "ownerReferences": []any{},
		} {
			value := metadataValue(metadata, key)
			if value == nil {
				value = empty
			}
			entry[key] = value
		}
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return fmt.Sprint(entries[i]["name"]) < fmt.Sprint(entries[j]["name"])
	})
	return json.Marshal(entries)
}

// unstructuredInt64 reads an integer field of a stored document the way jq
// read a number: an int64, or a float64 with no fraction. A field that is
// absent reports not found; one of another type is an error.
func unstructuredInt64(object map[string]any, fields ...string) (int64, bool, error) {
	var value any = object
	for _, field := range fields {
		next, ok := value.(map[string]any)
		if !ok {
			return 0, false, nil
		}
		if value, ok = next[field]; !ok {
			return 0, false, nil
		}
	}
	switch number := value.(type) {
	case int64:
		return number, true, nil
	case float64:
		if number == float64(int64(number)) {
			return int64(number), true, nil
		}
	}
	return 0, true, fmt.Errorf("%s is %T %v, not an integer", strings.Join(fields, "."), value, value)
}

func metadataValue(metadata map[string]any, key string) any {
	if metadata == nil {
		return nil
	}
	return metadata[key]
}
