package crdupgrade

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	admissionConvergenceMarkerPrefix      = "ptah-admission-convergence-v1-"
	admissionConvergenceComponent         = "admission-convergence"
	admissionConvergenceContractVersion   = "1"
	admissionConvergenceVersionAnnotation = "operator.ptah.run/admission-convergence-version"
	admissionConvergenceCleanupAnnotation = "operator.ptah.run/admission-convergence-cleanup-service-account"
	admissionConvergenceExpectedDataKey   = "expected-active-release-sequence"
	admissionConvergenceAttemptDataKey    = "release-attempt"
	admissionConvergenceMarkerHookWeight  = "-165"
)

var admissionConvergenceManagerImagePattern = regexp.MustCompile(`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`)

// AdmissionConvergenceMarkerName returns the release- and sequence-owned
// ConfigMap that records the admission inventory this release installed. The
// reconcile hook seals it, and the next release's hook reads it back to retire
// exactly that inventory.
func AdmissionConvergenceMarkerName(releaseNamespace, releaseName string, releaseSequence int32) string {
	return admissionConvergenceMarkerPrefix + strconv.FormatInt(int64(releaseSequence), 10) + "-" + admissionConvergenceReleaseDigest(releaseNamespace, releaseName)
}

func admissionConvergenceReleaseDigest(releaseNamespace, releaseName string) string {
	digest := sha256.Sum256([]byte(releaseNamespace + "\n" + releaseName))
	return fmt.Sprintf("%x", digest)[:12]
}

// AdmissionConvergenceGuard owns the exact contract of the sequence-keyed
// admission marker: the unsealed form Helm creates, and the immutable sealed
// form that carries the release's admission inventory for predecessor
// retirement.
type AdmissionConvergenceGuard struct {
	ReleaseName                          string
	ReleaseNamespace                     string
	HookServiceAccountName               string
	CleanupServiceAccountName            string
	ControllerServiceAccountName         string
	CertificateServiceAccountName        string
	PreviousControllerServiceAccountName string
	PreviousControllerReleaseSequence    int32
	ReleaseSequence                      int32
	ManagerImage                         string
}

// NewAdmissionConvergenceGuard derives the marker contract from a rollout
// guard. CertificateDeploymentName is also the certificate runtime's
// ServiceAccount name in the chart contract.
func NewAdmissionConvergenceGuard(rollout *RolloutGuard) *AdmissionConvergenceGuard {
	if rollout == nil {
		return nil
	}
	cleanupServiceAccountName, _ := TeardownServiceAccountName(rollout.HookServiceAccountName, rollout.ReleaseSequence)
	return &AdmissionConvergenceGuard{
		ReleaseName:                          rollout.ReleaseName,
		ReleaseNamespace:                     rollout.ReleaseNamespace,
		HookServiceAccountName:               rollout.HookServiceAccountName,
		CleanupServiceAccountName:            cleanupServiceAccountName,
		ControllerServiceAccountName:         rollout.ControllerServiceAccountName,
		CertificateServiceAccountName:        rollout.CertificateDeploymentName,
		PreviousControllerServiceAccountName: rollout.PreviousControllerServiceAccountName,
		PreviousControllerReleaseSequence:    rollout.PreviousControllerReleaseSequence,
		ReleaseSequence:                      rollout.ReleaseSequence,
		ManagerImage:                         rollout.ManagerImage,
	}
}

func (g *AdmissionConvergenceGuard) validate() error {
	if g == nil {
		return errors.New("admission convergence marker identity is required")
	}
	for description, value := range map[string]string{
		"release name":                    g.ReleaseName,
		"release namespace":               g.ReleaseNamespace,
		"hook ServiceAccount name":        g.HookServiceAccountName,
		"cleanup ServiceAccount name":     g.CleanupServiceAccountName,
		"controller ServiceAccount name":  g.ControllerServiceAccountName,
		"certificate ServiceAccount name": g.CertificateServiceAccountName,
		"manager image":                   g.ManagerImage,
	} {
		if value == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("admission convergence %s is empty or padded", description)
		}
	}
	if g.ReleaseSequence < 1 {
		return errors.New("admission convergence release sequence must be positive")
	}
	if err := validatePredecessorRelease(g.PreviousControllerServiceAccountName, g.PreviousControllerReleaseSequence, g.ReleaseSequence); err != nil {
		return fmt.Errorf("admission convergence: %w", err)
	}
	if g.PreviousControllerServiceAccountName != strings.TrimSpace(g.PreviousControllerServiceAccountName) {
		return errors.New("admission convergence predecessor ServiceAccount name is padded")
	}
	wantCleanup, err := TeardownServiceAccountName(g.HookServiceAccountName, g.ReleaseSequence)
	if err != nil {
		return fmt.Errorf("derive admission convergence cleanup ServiceAccount: %w", err)
	}
	if g.CleanupServiceAccountName != wantCleanup {
		return errors.New("admission convergence cleanup ServiceAccount does not match the candidate release identity")
	}
	activation := &ReleaseActivationGuard{
		ReleaseName: g.ReleaseName, ReleaseNamespace: g.ReleaseNamespace,
		HookServiceAccountName: g.HookServiceAccountName, ReleaseSequence: g.ReleaseSequence,
		ManagerImage: g.ManagerImage,
	}
	if _, err := activation.hookUsernamePattern(); err != nil {
		return fmt.Errorf("validate admission convergence hook identity: %w", err)
	}
	serviceAccounts := []string{
		g.HookServiceAccountName,
		g.CleanupServiceAccountName,
		g.ControllerServiceAccountName,
		g.CertificateServiceAccountName,
	}
	if g.PreviousControllerServiceAccountName != "" {
		serviceAccounts = append(serviceAccounts, g.PreviousControllerServiceAccountName)
	}
	slices.Sort(serviceAccounts)
	if len(slices.Compact(serviceAccounts)) != len(serviceAccounts) {
		return errors.New("admission convergence ServiceAccount identities must be distinct")
	}
	return nil
}

func (g *AdmissionConvergenceGuard) markerMetadata(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		Namespace: g.ReleaseNamespace,
		Annotations: map[string]string{
			"helm.sh/hook":                        "pre-install,pre-upgrade",
			"helm.sh/hook-weight":                 admissionConvergenceMarkerHookWeight,
			"helm.sh/resource-policy":             "keep",
			admissionConvergenceVersionAnnotation: admissionConvergenceContractVersion,
			ReleaseNameAnnotation:                 g.ReleaseName,
			ReleaseNamespaceAnnotation:            g.ReleaseNamespace,
			ReleaseSequenceAnnotation:             strconv.FormatInt(int64(g.ReleaseSequence), 10),
			ManagerImageAnnotation:                g.ManagerImage,
			admissionConvergenceCleanupAnnotation: g.CleanupServiceAccountName,
		},
		Labels: map[string]string{
			managedByLabel:                rolloutGuardManagedBy,
			instanceLabel:                 g.ReleaseName,
			"app.kubernetes.io/component": admissionConvergenceComponent,
		},
	}
}

// verifyMarker checks a stored marker, sealed or not, against the exact contract
// of this release attempt.
func (g *AdmissionConvergenceGuard) verifyMarker(marker *corev1.ConfigMap) error {
	if err := g.validate(); err != nil {
		return err
	}
	if marker != nil && marker.Immutable != nil && *marker.Immutable {
		_, err := g.verifySealedMarker(marker)
		return err
	}
	return g.verifyUnsealedMarker(marker)
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func admissionConvergenceMarkerImmutableExpression(object string, sealed bool) string {
	if sealed {
		return fmt.Sprintf(`has(dyn(%s).immutable) && dyn(%s).immutable == true`, object, object)
	}
	return fmt.Sprintf(`(!has(dyn(%s).immutable) || dyn(%s).immutable == false)`, object, object)
}

func admissionConvergenceMarkerInventoryExpression(object string, sealed bool) string {
	if !sealed {
		return ""
	}
	return fmt.Sprintf(` && %q in dyn(%s).data && dyn(%s).data[%q].matches(%q)`, PredecessorRetirementInventoryDataKey, object, object, PredecessorRetirementInventoryDataKey, `^\{"version":"1","entries":\[.+\]\}$`)
}
