package crdupgrade

import (
	"fmt"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func verifyHelmOwnership(kind string, metadata metav1.ObjectMeta, expected RuntimeInvariants) error {
	if metadata.Name != AdmissionConfigurationName ||
		metadata.Annotations[helmReleaseNameAnnotation] != expected.ReleaseName ||
		metadata.Annotations[helmReleaseNamespaceAnnotation] != expected.ReleaseNamespace ||
		metadata.Labels[managedByLabel] != "Helm" ||
		metadata.Labels[instanceLabel] != expected.ReleaseName {
		return fmt.Errorf("fixed admission singleton %s/%s is not owned by Helm release %s/%s", kind, metadata.Name, expected.ReleaseNamespace, expected.ReleaseName)
	}
	return nil
}

func positiveDecimalValue(raw string) (uint64, error) {
	if raw == "" || raw[0] < '1' || raw[0] > '9' {
		return 0, fmt.Errorf("%q is not a positive exact decimal version", raw)
	}
	for _, character := range raw[1:] {
		if character < '0' || character > '9' {
			return 0, fmt.Errorf("%q is not a positive exact decimal version", raw)
		}
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a positive exact decimal version: %w", raw, err)
	}
	return value, nil
}

const (
	helmReleaseNameAnnotation      = "meta.helm.sh/release-name"
	helmReleaseNamespaceAnnotation = "meta.helm.sh/release-namespace"
	managedByLabel                 = "app.kubernetes.io/managed-by"
	instanceLabel                  = "app.kubernetes.io/instance"
)
