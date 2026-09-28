package workload

import (
	"fmt"
	"sort"
	"strings"

	k8svalidation "k8s.io/apimachinery/pkg/util/validation"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// ReservedPodMetadataKey reports whether key is one spec.execution.podMetadata
// may not declare. It is the CRD's rule written once more in Go, for the
// readers that judge a Job after the API server admitted the resource: the
// builder, and the controller-write validator's cleanup path, which judges a
// terminal Job without rebuilding it.
//
// The reserved namespaces are the operator's own (ptah.run and every
// subdomain of it), and the ones Kubernetes keeps for its components
// (kubernetes.io and k8s.io, with their subdomains). The first holds the
// labels and annotations the builder writes; the second holds the Job
// controller's tracking labels, app.kubernetes.io, which every object the
// chart owns selects on, the LimitRanger annotation the Pod-intent webhook
// treats specially, and the annotations the API server still translates into
// a Pod's security context. controller-uid and job-name are the Job
// controller's legacy tracking labels, which carry no prefix.
func ReservedPodMetadataKey(key string) bool {
	if key == batchv1LegacyControllerUIDLabel || key == batchv1LegacyJobNameLabel {
		return true
	}
	for _, reserved := range []string{"ptah.run/", "kubernetes.io/", "k8s.io/"} {
		if strings.HasPrefix(key, reserved) || strings.Contains(key, "."+reserved) {
			return true
		}
	}
	return false
}

const (
	batchv1LegacyControllerUIDLabel = "controller-uid"
	batchv1LegacyJobNameLabel       = "job-name"
)

// declaredPodMetadata returns the labels and annotations
// spec.execution.podMetadata declares, as plain maps the builder writes under
// its own. The CRD refuses what is refused here before the resource is
// stored; this is the builder refusing to write a Job the API server or the
// Job write guard would refuse, with a reason that names the key.
func declaredPodMetadata(metadata *operatorv1alpha1.PodMetadataSpec) (labels, annotations map[string]string, err error) {
	if metadata == nil {
		return nil, nil, nil
	}
	if len(metadata.Labels) > operatorv1alpha1.MaxPodMetadataEntries ||
		len(metadata.Annotations) > operatorv1alpha1.MaxPodMetadataEntries {
		return nil, nil, fmt.Errorf("podMetadata declares more than %d labels or annotations", operatorv1alpha1.MaxPodMetadataEntries)
	}
	labels = make(map[string]string, len(metadata.Labels))
	for _, key := range sortedKeys(metadata.Labels) {
		value := string(metadata.Labels[key])
		if err := validateDeclaredKey("label", key); err != nil {
			return nil, nil, err
		}
		if problems := k8svalidation.IsValidLabelValue(value); len(problems) != 0 {
			return nil, nil, fmt.Errorf("podMetadata label %s has an invalid value: %s", key, problems[0])
		}
		labels[key] = value
	}
	annotations = make(map[string]string, len(metadata.Annotations))
	for _, key := range sortedKeys(metadata.Annotations) {
		value := string(metadata.Annotations[key])
		if err := validateDeclaredKey("annotation", key); err != nil {
			return nil, nil, err
		}
		if len(value) > operatorv1alpha1.MaxPodAnnotationValueLength {
			return nil, nil, fmt.Errorf("podMetadata annotation %s is longer than %d bytes", key, operatorv1alpha1.MaxPodAnnotationValueLength)
		}
		annotations[key] = value
	}
	return labels, annotations, nil
}

func validateDeclaredKey(kind, key string) error {
	if problems := k8svalidation.IsQualifiedName(key); len(problems) != 0 {
		return fmt.Errorf("podMetadata %s key %q is invalid: %s", kind, key, problems[0])
	}
	if ReservedPodMetadataKey(key) {
		return fmt.Errorf("podMetadata %s key %q is reserved", kind, key)
	}
	return nil
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
