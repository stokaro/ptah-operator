package crdupgrade

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func TestRuntimeAdmissionPreflightRejectsCorePodLimitToRequestDefault(t *testing.T) {
	preflight, _, _, _ := runtimeAdmissionPreflightFixture()
	delete(preflight.Contract.CommonInitContainerResources.Requests, corev1.ResourceMemory)

	err := preflight.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "common init container resources.requests[memory] is omitted") ||
		!strings.Contains(err.Error(), "core Pod defaulting would copy the limit") {
		t.Fatalf("Check() error = %v, want core limit-to-request default rejection", err)
	}
}

func TestRuntimeAdmissionPreflightRejectsResourceClaims(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*RuntimeAdmissionContract)
		want   string
	}{
		{
			name: "common init container",
			mutate: func(contract *RuntimeAdmissionContract) {
				contract.CommonInitContainerResources.Claims = []corev1.ResourceClaim{{Name: "runtime-device"}}
			},
			want: "common init container resources.claims must be empty",
		},
		{
			name: "controller container",
			mutate: func(contract *RuntimeAdmissionContract) {
				contract.ControllerContainerResources.Claims = []corev1.ResourceClaim{{Name: "runtime-device"}}
			},
			want: "controller container resources.claims must be empty",
		},
		{
			name: "certificate container",
			mutate: func(contract *RuntimeAdmissionContract) {
				contract.CertificateContainerResources.Claims = []corev1.ResourceClaim{{Name: "runtime-device"}}
			},
			want: "certificate container resources.claims must be empty",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			preflight, _, _, _ := runtimeAdmissionPreflightFixture()
			test.mutate(&preflight.Contract)

			err := preflight.Check(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "outside the immutable runtime contract") {
				t.Fatalf("Check() error = %v, want resource claim rejection", err)
			}
		})
	}
}

func TestRuntimeAdmissionPreflightRejectsUnsupportedResourceKeys(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*RuntimeAdmissionContract)
		want   string
	}{
		{
			name: "init request",
			mutate: func(contract *RuntimeAdmissionContract) {
				contract.CommonInitContainerResources.Requests[corev1.ResourceName("example.com/device")] = resource.MustParse("1")
			},
			want: "common init container resources.requests[example.com/device] is unsupported",
		},
		{
			name: "controller limit",
			mutate: func(contract *RuntimeAdmissionContract) {
				contract.ControllerContainerResources.Limits[corev1.ResourceName("hugepages-2Mi")] = resource.MustParse("2Mi")
			},
			want: "controller container resources.limits[hugepages-2Mi] is unsupported",
		},
		{
			name: "certificate request",
			mutate: func(contract *RuntimeAdmissionContract) {
				contract.CertificateContainerResources.Requests[corev1.ResourceStorage] = resource.MustParse("1Mi")
			},
			want: "certificate container resources.requests[storage] is unsupported",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			preflight, _, _, _ := runtimeAdmissionPreflightFixture()
			test.mutate(&preflight.Contract)

			err := preflight.Check(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) ||
				!strings.Contains(err.Error(), "only cpu, memory, and ephemeral-storage are allowed") {
				t.Fatalf("Check() error = %v, want unsupported resource rejection", err)
			}
		})
	}
}

func TestRuntimeAdmissionPreflightRejectsSubMilliQuantities(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*RuntimeAdmissionContract)
		want        string
		wantRounded string
	}{
		{
			name: "init request decimal suffix",
			mutate: func(contract *RuntimeAdmissionContract) {
				contract.CommonInitContainerResources.Requests[corev1.ResourceCPU] = resource.MustParse("0.1m")
			},
			want:        "common init container resources.requests[cpu]=100u",
			wantRounded: "would round up to 1m",
		},
		{
			name: "controller limit decimal exponent",
			mutate: func(contract *RuntimeAdmissionContract) {
				contract.ControllerContainerResources.Limits[corev1.ResourceCPU] = resource.MustParse("1e-4")
			},
			want:        "controller container resources.limits[cpu]=100e-6",
			wantRounded: "would round up to 1e-3",
		},
		{
			name: "certificate request decimal",
			mutate: func(contract *RuntimeAdmissionContract) {
				contract.CertificateContainerResources.Requests[corev1.ResourceEphemeralStorage] = resource.MustParse("0.0001")
			},
			want:        "certificate container resources.requests[ephemeral-storage]=100u",
			wantRounded: "would round up to 1m",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			preflight, _, _, _ := runtimeAdmissionPreflightFixture()
			test.mutate(&preflight.Contract)

			err := preflight.Check(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) ||
				!strings.Contains(err.Error(), test.wantRounded) ||
				!strings.Contains(err.Error(), "configure the rounded value explicitly") {
				t.Fatalf("Check() error = %v, want Kubernetes quantity rounding rejection", err)
			}
		})
	}
}

func TestRuntimeAdmissionPreflightAcceptsSupportedCanonicalResources(t *testing.T) {
	preflight, _, _, _ := runtimeAdmissionPreflightFixture()
	for _, resources := range []*corev1.ResourceRequirements{
		&preflight.Contract.CommonInitContainerResources,
		&preflight.Contract.ControllerContainerResources,
		&preflight.Contract.CertificateContainerResources,
	} {
		resources.Requests[corev1.ResourceEphemeralStorage] = resource.MustParse("1Mi")
		resources.Limits[corev1.ResourceEphemeralStorage] = resource.MustParse("2Mi")
	}

	if err := preflight.Check(context.Background()); err != nil {
		t.Fatalf("Check() rejected supported canonical resource requirements: %v", err)
	}
}

func TestRuntimeAdmissionPreflightRejectsContainerLimitRangeDefaults(t *testing.T) {
	tests := []struct {
		name       string
		item       corev1.LimitRangeItem
		wantFields []string
	}{
		{
			name: "explicit default",
			item: corev1.LimitRangeItem{
				Type:    corev1.LimitTypeContainer,
				Default: runtimeAdmissionResourceList(corev1.ResourceCPU, "250m"),
			},
			wantFields: []string{"resources.limits[cpu]", "250m"},
		},
		{
			name: "max-derived default limit",
			item: corev1.LimitRangeItem{
				Type: corev1.LimitTypeContainer,
				Max:  runtimeAdmissionResourceList(corev1.ResourceEphemeralStorage, "1Gi"),
			},
			wantFields: []string{"resources.limits[ephemeral-storage]", "1Gi"},
		},
		{
			name: "min-derived default request",
			item: corev1.LimitRangeItem{
				Type: corev1.LimitTypeContainer,
				Min:  runtimeAdmissionResourceList(corev1.ResourceEphemeralStorage, "8Mi"),
			},
			wantFields: []string{"resources.requests[ephemeral-storage]", "8Mi"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			preflight, limitRanges, _, _ := runtimeAdmissionPreflightFixture()
			limitRanges.list.Items = []corev1.LimitRange{{
				ObjectMeta: metav1.ObjectMeta{Name: "runtime-defaults", Namespace: "operators"},
				Spec:       corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{test.item}},
			}}

			err := preflight.Check(context.Background())
			if err == nil {
				t.Fatal("Check() accepted a runtime resource mutation")
			}
			for _, field := range test.wantFields {
				if !strings.Contains(err.Error(), field) {
					t.Fatalf("Check() error = %v, want %q", err, field)
				}
			}
		})
	}
}

func TestNormalizedRuntimeContainerDefaultsMatchesAPIDefaultOrder(t *testing.T) {
	item := corev1.LimitRangeItem{
		Type: corev1.LimitTypeContainer,
		DefaultRequest: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("25m"),
		},
		Default: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
		Max: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("1"),
			corev1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
		},
		Min: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("10m"),
			corev1.ResourceMemory:           resource.MustParse("32Mi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("64Mi"),
		},
	}

	requests, limits := normalizedRuntimeContainerDefaults(item)
	runtimeAdmissionRequireQuantity(t, limits, corev1.ResourceCPU, "1")
	runtimeAdmissionRequireQuantity(t, limits, corev1.ResourceMemory, "128Mi")
	runtimeAdmissionRequireQuantity(t, limits, corev1.ResourceEphemeralStorage, "2Gi")
	runtimeAdmissionRequireQuantity(t, requests, corev1.ResourceCPU, "25m")
	runtimeAdmissionRequireQuantity(t, requests, corev1.ResourceMemory, "128Mi")
	runtimeAdmissionRequireQuantity(t, requests, corev1.ResourceEphemeralStorage, "2Gi")
}

func TestRuntimeAdmissionPreflightValidatesLimitRangeConstraints(t *testing.T) {
	tests := []struct {
		name string
		item corev1.LimitRangeItem
		want string
	}{
		{
			name: "container maximum",
			item: corev1.LimitRangeItem{
				Type: corev1.LimitTypeContainer,
				Max:  runtimeAdmissionResourceList(corev1.ResourceMemory, "128Mi"),
			},
			want: "controller container limits it to 256Mi",
		},
		{
			name: "container ratio",
			item: corev1.LimitRangeItem{
				Type:                 corev1.LimitTypeContainer,
				MaxLimitRequestRatio: runtimeAdmissionResourceList(corev1.ResourceMemory, "2"),
			},
			want: "controller container has limit 256Mi and request 96Mi",
		},
		{
			name: "aggregate Pod maximum",
			item: corev1.LimitRangeItem{
				Type: corev1.LimitTypePod,
				Max:  runtimeAdmissionResourceList(corev1.ResourceMemory, "200Mi"),
			},
			want: "controller Pod limits it to 256Mi",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			preflight, limitRanges, _, _ := runtimeAdmissionPreflightFixture()
			limitRanges.list.Items = []corev1.LimitRange{{
				ObjectMeta: metav1.ObjectMeta{Name: "constraints", Namespace: "operators"},
				Spec:       corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{test.item}},
			}}

			err := preflight.Check(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Check() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRuntimeAdmissionPreflightAcceptsSatisfiedLimitRangeConstraints(t *testing.T) {
	preflight, limitRanges, _, _ := runtimeAdmissionPreflightFixture()
	limitRanges.list.Items = []corev1.LimitRange{{
		ObjectMeta: metav1.ObjectMeta{Name: "constraints", Namespace: "operators"},
		Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{
			{
				Type: corev1.LimitTypeContainer,
				Min:  runtimeAdmissionResourceList(corev1.ResourceCPU, "1m"),
				Max:  runtimeAdmissionResourceList(corev1.ResourceMemory, "512Mi"),
				MaxLimitRequestRatio: runtimeAdmissionResourceList(
					corev1.ResourceMemory, "8",
				),
			},
			{
				Type: corev1.LimitTypePod,
				Min:  runtimeAdmissionResourceList(corev1.ResourceCPU, "1m"),
				Max:  runtimeAdmissionResourceList(corev1.ResourceMemory, "512Mi"),
			},
		}},
	}}

	if err := preflight.Check(context.Background()); err != nil {
		t.Fatalf("Check() rejected satisfied constraints: %v", err)
	}
}

func TestRuntimeAdmissionPreflightRejectsServiceAccountImagePullSecretInjection(t *testing.T) {
	preflight, _, serviceAccounts, _ := runtimeAdmissionPreflightFixture()
	serviceAccounts.accounts[preflight.Contract.ControllerServiceAccountName].ImagePullSecrets = []corev1.LocalObjectReference{{Name: "private-registry"}}

	err := preflight.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ServiceAccount operators/ptah-controller has imagePullSecrets") ||
		!strings.Contains(err.Error(), "would inject them") {
		t.Fatalf("Check() error = %v, want ServiceAccount imagePullSecrets rejection", err)
	}
}

func TestRuntimeAdmissionPreflightExplicitPodSecretsSuppressServiceAccountInjection(t *testing.T) {
	preflight, _, serviceAccounts, _ := runtimeAdmissionPreflightFixture()
	preflight.Contract.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "chart-registry"}}
	serviceAccounts.accounts[preflight.Contract.ControllerServiceAccountName].ImagePullSecrets = []corev1.LocalObjectReference{{Name: "account-registry"}}
	serviceAccounts.accounts[preflight.Contract.CertificateServiceAccountName].ImagePullSecrets = []corev1.LocalObjectReference{{Name: "account-registry"}}

	if err := preflight.Check(context.Background()); err != nil {
		t.Fatalf("Check() rejected explicit Pod imagePullSecrets: %v", err)
	}
	if len(serviceAccounts.gets) != 2 {
		t.Fatalf("ServiceAccount Get calls = %v, want both runtime identities checked for existence", serviceAccounts.gets)
	}
}

func TestRuntimeAdmissionPreflightEnforcesMountableSecretAllowlist(t *testing.T) {
	t.Run("controller Secret volume must be allowed", func(t *testing.T) {
		preflight, _, serviceAccounts, _ := runtimeAdmissionPreflightFixture()
		serviceAccounts.accounts[preflight.Contract.ControllerServiceAccountName].Annotations = map[string]string{
			runtimeAdmissionEnforceMountableSecretsAnnotation: "true",
		}

		err := preflight.Check(context.Background())
		if err == nil || !strings.Contains(err.Error(), "enforces mountable secrets but does not list Pod Secret ptah-webhook-cert in secrets") {
			t.Fatalf("Check() error = %v, want missing controller Secret allowlist rejection", err)
		}
	})

	t.Run("explicit image pull Secret must be allowed", func(t *testing.T) {
		preflight, _, serviceAccounts, _ := runtimeAdmissionPreflightFixture()
		preflight.Contract.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "chart-registry"}}
		account := serviceAccounts.accounts[preflight.Contract.ControllerServiceAccountName]
		account.Annotations = map[string]string{runtimeAdmissionEnforceMountableSecretsAnnotation: "TRUE"}
		account.Secrets = []corev1.ObjectReference{{Name: "ptah-webhook-cert"}}

		err := preflight.Check(context.Background())
		if err == nil || !strings.Contains(err.Error(), "does not list Pod image pull Secret chart-registry in imagePullSecrets") {
			t.Fatalf("Check() error = %v, want missing image pull Secret allowlist rejection", err)
		}
	})

	t.Run("complete allowlists pass", func(t *testing.T) {
		preflight, _, serviceAccounts, _ := runtimeAdmissionPreflightFixture()
		preflight.Contract.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "chart-registry"}}
		account := serviceAccounts.accounts[preflight.Contract.ControllerServiceAccountName]
		account.Annotations = map[string]string{runtimeAdmissionEnforceMountableSecretsAnnotation: "1"}
		account.Secrets = []corev1.ObjectReference{{Name: "ptah-webhook-cert"}, {Name: "unrelated"}}
		account.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "chart-registry"}, {Name: "unrelated"}}

		if err := preflight.Check(context.Background()); err != nil {
			t.Fatalf("Check() rejected complete mountable Secret allowlists: %v", err)
		}
	})

	t.Run("certificate Secret volume must be allowed", func(t *testing.T) {
		preflight, _, serviceAccounts, _ := runtimeAdmissionPreflightFixture()
		preflight.Contract.CertificateSecretNames = []string{"certificate-input"}
		account := serviceAccounts.accounts[preflight.Contract.CertificateServiceAccountName]
		account.Annotations = map[string]string{runtimeAdmissionEnforceMountableSecretsAnnotation: "True"}

		err := preflight.Check(context.Background())
		if err == nil || !strings.Contains(err.Error(), "ServiceAccount operators/ptah-cert-rotator enforces mountable secrets") ||
			!strings.Contains(err.Error(), "Pod Secret certificate-input") {
			t.Fatalf("Check() error = %v, want missing certificate Secret allowlist rejection", err)
		}
	})
}

func TestRuntimeAdmissionPreflightMatchesMountableSecretAnnotationBooleanParsing(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		enforce bool
	}{
		{name: "one", value: "1", enforce: true},
		{name: "lowercase t", value: "t", enforce: true},
		{name: "uppercase T", value: "T", enforce: true},
		{name: "uppercase true", value: "TRUE", enforce: true},
		{name: "lowercase true", value: "true", enforce: true},
		{name: "titlecase true", value: "True", enforce: true},
		{name: "zero", value: "0"},
		{name: "lowercase f", value: "f"},
		{name: "uppercase F", value: "F"},
		{name: "uppercase false", value: "FALSE"},
		{name: "lowercase false", value: "false"},
		{name: "titlecase false", value: "False"},
		{name: "invalid text", value: "not-a-boolean"},
		{name: "invalid mixed case", value: "tRuE"},
		{name: "invalid whitespace", value: " true "},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				runtimeAdmissionEnforceMountableSecretsAnnotation: test.value,
			}}}
			if got := serviceAccountEnforcesMountableSecrets(account); got != test.enforce {
				t.Fatalf("serviceAccountEnforcesMountableSecrets() = %t for %q, want %t", got, test.value, test.enforce)
			}

			preflight, _, serviceAccounts, _ := runtimeAdmissionPreflightFixture()
			controllerAccount := serviceAccounts.accounts[preflight.Contract.ControllerServiceAccountName]
			controllerAccount.Annotations = account.Annotations
			if test.enforce {
				controllerAccount.Secrets = []corev1.ObjectReference{{Name: "ptah-webhook-cert"}}
			}

			if err := preflight.Check(context.Background()); err != nil {
				t.Fatalf("Check() rejected annotation value %q with a complete contract: %v", test.value, err)
			}
		})
	}
}

func TestRuntimeAdmissionPreflightBoundsServiceAccountSecretAllowlists(t *testing.T) {
	t.Run("mountable Secrets", func(t *testing.T) {
		preflight, _, serviceAccounts, _ := runtimeAdmissionPreflightFixture()
		account := serviceAccounts.accounts[preflight.Contract.ControllerServiceAccountName]
		account.Annotations = map[string]string{runtimeAdmissionEnforceMountableSecretsAnnotation: "true"}
		account.Secrets = make([]corev1.ObjectReference, runtimeAdmissionMaxSecretReferences+1)

		err := preflight.Check(context.Background())
		if err == nil || !strings.Contains(err.Error(), "has more than 64 mountable Secret references") {
			t.Fatalf("Check() error = %v, want bounded mountable Secret rejection", err)
		}
	})

	t.Run("image pull Secrets", func(t *testing.T) {
		preflight, _, serviceAccounts, _ := runtimeAdmissionPreflightFixture()
		preflight.Contract.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "chart-registry"}}
		account := serviceAccounts.accounts[preflight.Contract.ControllerServiceAccountName]
		account.ImagePullSecrets = make([]corev1.LocalObjectReference, runtimeAdmissionMaxImagePullSecrets+1)

		err := preflight.Check(context.Background())
		if err == nil || !strings.Contains(err.Error(), "has more than 64 image pull secrets") {
			t.Fatalf("Check() error = %v, want bounded image pull Secret rejection", err)
		}
	})
}

func TestRuntimeAdmissionPreflightValidatesSecretReferenceContract(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*RuntimeAdmissionContract)
		want   string
	}{
		{
			name: "missing controller Secret",
			mutate: func(contract *RuntimeAdmissionContract) {
				contract.ControllerSecretNames = nil
			},
			want: "controller Secret names are required",
		},
		{
			name: "duplicate controller Secret",
			mutate: func(contract *RuntimeAdmissionContract) {
				contract.ControllerSecretNames = []string{"ptah-webhook-cert", "ptah-webhook-cert"}
			},
			want: "Secret reference ptah-webhook-cert is duplicated",
		},
		{
			name: "whitespace certificate Secret",
			mutate: func(contract *RuntimeAdmissionContract) {
				contract.CertificateSecretNames = []string{" certificate-input"}
			},
			want: "certificate runtime Pod Secret reference at index 0 has an empty or whitespace-padded name",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			preflight, _, _, _ := runtimeAdmissionPreflightFixture()
			test.mutate(&preflight.Contract)
			if err := preflight.Check(context.Background()); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Check() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRuntimeAdmissionPreflightHandlesServiceAccountCreationContract(t *testing.T) {
	t.Run("chart-created accounts may not exist before main resources", func(t *testing.T) {
		preflight, _, serviceAccounts, _ := runtimeAdmissionPreflightFixture()
		preflight.Contract.ControllerServiceAccountEnforceMountableSecrets = true
		serviceAccounts.accounts = map[string]*corev1.ServiceAccount{}

		if err := preflight.Check(context.Background()); err != nil {
			t.Fatalf("Check() rejected ServiceAccounts that the chart will create: %v", err)
		}
	})

	t.Run("external controller account must already exist", func(t *testing.T) {
		preflight, _, serviceAccounts, _ := runtimeAdmissionPreflightFixture()
		preflight.Contract.ControllerServiceAccountCreate = false
		delete(serviceAccounts.accounts, preflight.Contract.ControllerServiceAccountName)

		err := preflight.Check(context.Background())
		if err == nil || !strings.Contains(err.Error(), "external runtime ServiceAccount operators/ptah-controller does not exist") {
			t.Fatalf("Check() error = %v, want missing external ServiceAccount rejection", err)
		}
	})

	t.Run("disabled certificate runtime is not required", func(t *testing.T) {
		preflight, _, serviceAccounts, _ := runtimeAdmissionPreflightFixture()
		preflight.Contract.CertificateRuntimeEnabled = false
		delete(serviceAccounts.accounts, preflight.Contract.CertificateServiceAccountName)

		if err := preflight.Check(context.Background()); err != nil {
			t.Fatalf("Check() rejected a disabled certificate runtime: %v", err)
		}
		if len(serviceAccounts.gets) != 1 || serviceAccounts.gets[0] != preflight.Contract.ControllerServiceAccountName {
			t.Fatalf("ServiceAccount Get calls = %v, want controller only", serviceAccounts.gets)
		}
	})
}

func TestRuntimeAdmissionPreflightRejectsGlobalDefaultPriorityClass(t *testing.T) {
	preflight, _, _, priorityClasses := runtimeAdmissionPreflightFixture()
	priorityClasses.list.Items = []schedulingv1.PriorityClass{{
		ObjectMeta:    metav1.ObjectMeta{Name: "default-workloads"},
		GlobalDefault: true,
	}}

	err := preflight.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "global default PriorityClass default-workloads would mutate runtime Pods") {
		t.Fatalf("Check() error = %v, want global default PriorityClass rejection", err)
	}
}

func TestRuntimeAdmissionPreflightRejectsAmbiguousGlobalPriorityClasses(t *testing.T) {
	preflight, _, _, priorityClasses := runtimeAdmissionPreflightFixture()
	priorityClasses.list.Items = []schedulingv1.PriorityClass{
		{ObjectMeta: metav1.ObjectMeta{Name: "second"}, GlobalDefault: true},
		{ObjectMeta: metav1.ObjectMeta{Name: "first"}, GlobalDefault: true},
	}

	err := preflight.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "multiple global default PriorityClasses") ||
		!strings.Contains(err.Error(), "first, second") {
		t.Fatalf("Check() error = %v, want deterministic ambiguous defaults rejection", err)
	}
}

func TestRuntimeAdmissionPreflightRequiresConfiguredPriorityClass(t *testing.T) {
	preflight, _, _, priorityClasses := runtimeAdmissionPreflightFixture()
	preflight.Contract.PriorityClassName = "runtime-critical"
	preflight.Contract.PriorityClassValue = 1000
	priorityClasses.getErrors["runtime-critical"] = apierrors.NewNotFound(
		schema.GroupResource{Group: schedulingv1.GroupName, Resource: "priorityclasses"},
		"runtime-critical",
	)

	err := preflight.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "configured PriorityClass runtime-critical does not exist") {
		t.Fatalf("Check() error = %v, want missing configured PriorityClass rejection", err)
	}

	priorityClasses.getErrors = map[string]error{}
	priorityClasses.classes["runtime-critical"] = &schedulingv1.PriorityClass{
		ObjectMeta: metav1.ObjectMeta{Name: "runtime-critical"},
		Value:      1000,
	}
	if err := preflight.Check(context.Background()); err != nil {
		t.Fatalf("Check() rejected an existing configured PriorityClass: %v", err)
	}
	if priorityClasses.listCalls != 0 {
		t.Fatalf("PriorityClass List calls = %d, want none for a configured class", priorityClasses.listCalls)
	}
}

func TestRuntimeAdmissionPreflightMatchesConfiguredPriorityClassState(t *testing.T) {
	t.Run("value mismatch", func(t *testing.T) {
		preflight, _, _, priorityClasses := runtimeAdmissionPreflightFixture()
		preflight.Contract.PriorityClassName = "runtime-critical"
		preflight.Contract.PriorityClassValue = 1000
		priorityClasses.classes["runtime-critical"] = &schedulingv1.PriorityClass{
			ObjectMeta: metav1.ObjectMeta{Name: "runtime-critical"},
			Value:      999,
		}

		err := preflight.Check(context.Background())
		if err == nil || !strings.Contains(err.Error(), "PriorityClass runtime-critical value is 999") ||
			!strings.Contains(err.Error(), "runtime Pod contract requires 1000") {
			t.Fatalf("Check() error = %v, want PriorityClass value mismatch", err)
		}
	})

	t.Run("effective default preemption mismatch", func(t *testing.T) {
		preflight, _, _, priorityClasses := runtimeAdmissionPreflightFixture()
		preflight.Contract.PriorityClassName = "runtime-critical"
		preflight.Contract.PriorityClassValue = 1000
		preflight.Contract.PriorityClassPreemptionPolicy = string(corev1.PreemptNever)
		priorityClasses.classes["runtime-critical"] = &schedulingv1.PriorityClass{
			ObjectMeta: metav1.ObjectMeta{Name: "runtime-critical"},
			Value:      1000,
		}

		err := preflight.Check(context.Background())
		if err == nil || !strings.Contains(err.Error(), "effective preemptionPolicy is PreemptLowerPriority") ||
			!strings.Contains(err.Error(), "runtime Pod contract requires Never") {
			t.Fatalf("Check() error = %v, want PriorityClass preemption policy mismatch", err)
		}
	})

	t.Run("explicit Never preemption matches", func(t *testing.T) {
		preflight, _, _, priorityClasses := runtimeAdmissionPreflightFixture()
		preflight.Contract.PriorityClassName = "batch-runtime"
		preflight.Contract.PriorityClassValue = -10
		preflight.Contract.PriorityClassPreemptionPolicy = string(corev1.PreemptNever)
		preemptionPolicy := corev1.PreemptNever
		priorityClasses.classes["batch-runtime"] = &schedulingv1.PriorityClass{
			ObjectMeta:       metav1.ObjectMeta{Name: "batch-runtime"},
			Value:            -10,
			PreemptionPolicy: &preemptionPolicy,
		}

		if err := preflight.Check(context.Background()); err != nil {
			t.Fatalf("Check() rejected matching PriorityClass state: %v", err)
		}
	})
}

func TestRuntimeAdmissionPreflightValidatesPriorityClassContract(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*RuntimeAdmissionContract)
		want   string
	}{
		{
			name: "empty name with nonzero value",
			mutate: func(contract *RuntimeAdmissionContract) {
				contract.PriorityClassValue = 10
			},
			want: "priorityClassName is empty, so priorityClassValue must be 0, got 10",
		},
		{
			name: "empty name with nondefault preemption",
			mutate: func(contract *RuntimeAdmissionContract) {
				contract.PriorityClassPreemptionPolicy = string(corev1.PreemptNever)
			},
			want: "priorityClassName is empty, so priorityClassPreemptionPolicy must be PreemptLowerPriority",
		},
		{
			name: "named class with invalid preemption",
			mutate: func(contract *RuntimeAdmissionContract) {
				contract.PriorityClassName = "runtime-critical"
				contract.PriorityClassPreemptionPolicy = "Sometimes"
			},
			want: "priorityClassPreemptionPolicy for PriorityClass runtime-critical must be PreemptLowerPriority or Never",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			preflight, _, _, _ := runtimeAdmissionPreflightFixture()
			test.mutate(&preflight.Contract)
			if err := preflight.Check(context.Background()); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Check() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRuntimeAdmissionPreflightExhaustsPaginatedEnvironmentLists(t *testing.T) {
	preflight, limitRanges, _, priorityClasses := runtimeAdmissionPreflightFixture()

	limitRangeItems := make([]corev1.LimitRange, 40)
	for index := range limitRangeItems {
		limitRangeItems[index] = corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("limits-%02d", index),
			Namespace: preflight.Contract.Namespace,
			UID:       types.UID(fmt.Sprintf("limits-%02d", index)),
		}}
	}
	limitRanges.pages = map[string]*corev1.LimitRangeList{
		"": {
			ListMeta: runtimeAdmissionListMeta("limits-rv", "limits-next", 20),
			Items:    limitRangeItems[:20],
		},
		"limits-next": {
			ListMeta: runtimeAdmissionListMeta("limits-rv", "", 0),
			Items:    limitRangeItems[20:],
		},
	}

	priorityClassItems := make([]schedulingv1.PriorityClass, 70)
	for index := range priorityClassItems {
		priorityClassItems[index] = schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("priority-%02d", index),
			UID:  types.UID(fmt.Sprintf("priority-%02d", index)),
		}}
	}
	priorityClasses.pages = map[string]*schedulingv1.PriorityClassList{
		"": {
			ListMeta: runtimeAdmissionListMeta("priority-rv", "priority-next", 35),
			Items:    priorityClassItems[:35],
		},
		"priority-next": {
			ListMeta: runtimeAdmissionListMeta("priority-rv", "", 0),
			Items:    priorityClassItems[35:],
		},
	}

	if err := preflight.Check(context.Background()); err != nil {
		t.Fatalf("Check() rejected complete paginated environment inventories: %v", err)
	}
	assertRuntimeAdmissionListOptions(t, "LimitRange", limitRanges.calls, []string{"", "limits-next"})
	assertRuntimeAdmissionListOptions(t, "PriorityClass", priorityClasses.calls, []string{"", "priority-next"})
}

func TestRuntimeAdmissionPreflightEvaluatesEveryPaginatedObject(t *testing.T) {
	t.Run("LimitRange on final page", func(t *testing.T) {
		preflight, limitRanges, _, _ := runtimeAdmissionPreflightFixture()
		limitRanges.pages = map[string]*corev1.LimitRangeList{
			"": {
				ListMeta: runtimeAdmissionListMeta("1", "next", 1),
				Items: []corev1.LimitRange{{ObjectMeta: metav1.ObjectMeta{
					Name: "harmless", Namespace: preflight.Contract.Namespace,
				}}},
			},
			"next": {
				ListMeta: runtimeAdmissionListMeta("1", "", 0),
				Items: []corev1.LimitRange{{
					ObjectMeta: metav1.ObjectMeta{Name: "mutating", Namespace: preflight.Contract.Namespace},
					Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
						Type:    corev1.LimitTypeContainer,
						Default: runtimeAdmissionResourceList(corev1.ResourceCPU, "250m"),
					}}},
				}},
			},
		}

		err := preflight.Check(context.Background())
		if err == nil || !strings.Contains(err.Error(), "mutating") || !strings.Contains(err.Error(), "250m") {
			t.Fatalf("Check() error = %v, want final-page LimitRange rejection", err)
		}
	})

	t.Run("global PriorityClass on final page", func(t *testing.T) {
		preflight, _, _, priorityClasses := runtimeAdmissionPreflightFixture()
		priorityClasses.pages = map[string]*schedulingv1.PriorityClassList{
			"": {
				ListMeta: runtimeAdmissionListMeta("1", "next", 1),
				Items:    []schedulingv1.PriorityClass{{ObjectMeta: metav1.ObjectMeta{Name: "ordinary"}}},
			},
			"next": {
				ListMeta: runtimeAdmissionListMeta("1", "", 0),
				Items: []schedulingv1.PriorityClass{{
					ObjectMeta:    metav1.ObjectMeta{Name: "default-workloads"},
					GlobalDefault: true,
				}},
			},
		}

		err := preflight.Check(context.Background())
		if err == nil || !strings.Contains(err.Error(), "global default PriorityClass default-workloads") {
			t.Fatalf("Check() error = %v, want final-page PriorityClass rejection", err)
		}
	})
}

func TestRuntimeAdmissionPreflightRejectsNilContinuedPages(t *testing.T) {
	t.Run("LimitRanges", func(t *testing.T) {
		preflight, limitRanges, _, _ := runtimeAdmissionPreflightFixture()
		limitRanges.pages = map[string]*corev1.LimitRangeList{
			"": {
				ListMeta: metav1.ListMeta{ResourceVersion: "1", Continue: "next"},
				Items: []corev1.LimitRange{{ObjectMeta: metav1.ObjectMeta{
					Name: "first", Namespace: preflight.Contract.Namespace,
				}}},
			},
			"next": nil,
		}
		err := preflight.Check(context.Background())
		if err == nil || !strings.Contains(err.Error(), "LimitRanges returned a nil page") {
			t.Fatalf("Check() error = %v", err)
		}
	})

	t.Run("PriorityClasses", func(t *testing.T) {
		preflight, _, _, priorityClasses := runtimeAdmissionPreflightFixture()
		priorityClasses.pages = map[string]*schedulingv1.PriorityClassList{
			"": {
				ListMeta: metav1.ListMeta{ResourceVersion: "1", Continue: "next"},
				Items: []schedulingv1.PriorityClass{{ObjectMeta: metav1.ObjectMeta{
					Name: "first",
				}}},
			},
			"next": nil,
		}
		err := preflight.Check(context.Background())
		if err == nil || !strings.Contains(err.Error(), "PriorityClasses returned a nil page") {
			t.Fatalf("Check() error = %v", err)
		}
	})
}

func TestRuntimeAdmissionPreflightCleanContract(t *testing.T) {
	preflight, _, _, _ := runtimeAdmissionPreflightFixture()
	if err := preflight.Check(context.Background()); err != nil {
		t.Fatalf("Check() rejected a clean runtime admission contract: %v", err)
	}
}

func runtimeAdmissionPreflightFixture() (
	*RuntimeAdmissionPreflight,
	*runtimeAdmissionFakeLimitRanges,
	*runtimeAdmissionFakeServiceAccounts,
	*runtimeAdmissionFakePriorityClasses,
) {
	contract := RuntimeAdmissionContract{
		Namespace: "operators",
		CommonInitContainerResources: runtimeAdmissionRequirements(
			map[corev1.ResourceName]string{corev1.ResourceCPU: "5m", corev1.ResourceMemory: "16Mi"},
			map[corev1.ResourceName]string{corev1.ResourceMemory: "32Mi"},
		),
		ControllerContainerResources: runtimeAdmissionRequirements(
			map[corev1.ResourceName]string{corev1.ResourceCPU: "50m", corev1.ResourceMemory: "96Mi"},
			map[corev1.ResourceName]string{corev1.ResourceMemory: "256Mi"},
		),
		CertificateContainerResources: runtimeAdmissionRequirements(
			map[corev1.ResourceName]string{corev1.ResourceCPU: "10m", corev1.ResourceMemory: "32Mi"},
			map[corev1.ResourceName]string{corev1.ResourceMemory: "64Mi"},
		),
		ControllerSecretNames:          []string{"ptah-webhook-cert"},
		PriorityClassPreemptionPolicy:  string(corev1.PreemptLowerPriority),
		ControllerServiceAccountName:   "ptah-controller",
		CertificateServiceAccountName:  "ptah-cert-rotator",
		ControllerServiceAccountCreate: true,
		CertificateRuntimeEnabled:      true,
	}
	limitRanges := &runtimeAdmissionFakeLimitRanges{list: &corev1.LimitRangeList{}}
	serviceAccounts := &runtimeAdmissionFakeServiceAccounts{accounts: map[string]*corev1.ServiceAccount{
		"ptah-controller": {
			ObjectMeta: metav1.ObjectMeta{Name: "ptah-controller", Namespace: "operators"},
		},
		"ptah-cert-rotator": {
			ObjectMeta: metav1.ObjectMeta{Name: "ptah-cert-rotator", Namespace: "operators"},
		},
	}}
	priorityClasses := &runtimeAdmissionFakePriorityClasses{
		list:      &schedulingv1.PriorityClassList{},
		classes:   map[string]*schedulingv1.PriorityClass{},
		getErrors: map[string]error{},
	}
	return NewRuntimeAdmissionPreflight(contract, limitRanges, serviceAccounts, priorityClasses), limitRanges, serviceAccounts, priorityClasses
}

func runtimeAdmissionRequirements(
	requests map[corev1.ResourceName]string,
	limits map[corev1.ResourceName]string,
) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: runtimeAdmissionQuantities(requests),
		Limits:   runtimeAdmissionQuantities(limits),
	}
}

func runtimeAdmissionQuantities(values map[corev1.ResourceName]string) corev1.ResourceList {
	result := make(corev1.ResourceList, len(values))
	for name, value := range values {
		result[name] = resource.MustParse(value)
	}
	return result
}

func runtimeAdmissionResourceList(name corev1.ResourceName, value string) corev1.ResourceList {
	return corev1.ResourceList{name: resource.MustParse(value)}
}

func runtimeAdmissionRequireQuantity(t *testing.T, values corev1.ResourceList, name corev1.ResourceName, want string) {
	t.Helper()
	quantity, found := values[name]
	if !found || quantity.Cmp(resource.MustParse(want)) != 0 {
		t.Fatalf("resource %s = %s (found %t), want %s", name, quantity.String(), found, want)
	}
}

func runtimeAdmissionListMeta(resourceVersion, continueToken string, remaining int64) metav1.ListMeta {
	return metav1.ListMeta{
		ResourceVersion:    resourceVersion,
		Continue:           continueToken,
		RemainingItemCount: &remaining,
	}
}

func assertRuntimeAdmissionListOptions(t *testing.T, kind string, options []metav1.ListOptions, wantTokens []string) {
	t.Helper()
	if len(options) != len(wantTokens) {
		t.Fatalf("%s List calls = %d, want %d: %#v", kind, len(options), len(wantTokens), options)
	}
	for index := range options {
		if options[index].Limit != runtimeAdmissionPageSize || options[index].Continue != wantTokens[index] || options[index].LabelSelector != "" || options[index].FieldSelector != "" {
			t.Fatalf("%s List options[%d] = %#v, want Limit=%d Continue=%q without selectors", kind, index, options[index], runtimeAdmissionPageSize, wantTokens[index])
		}
	}
}

type runtimeAdmissionFakeLimitRanges struct {
	list             *corev1.LimitRangeList
	pages            map[string]*corev1.LimitRangeList
	err              error
	errorsByContinue map[string]error
	lastOptions      metav1.ListOptions
	calls            []metav1.ListOptions
}

func (f *runtimeAdmissionFakeLimitRanges) List(_ context.Context, options metav1.ListOptions) (*corev1.LimitRangeList, error) {
	f.lastOptions = options
	f.calls = append(f.calls, options)
	if f.err != nil {
		return nil, f.err
	}
	if err := f.errorsByContinue[options.Continue]; err != nil {
		return nil, err
	}
	if f.pages != nil {
		page, found := f.pages[options.Continue]
		if !found {
			return nil, errors.New("unexpected LimitRange continue token " + options.Continue)
		}
		if page == nil {
			return nil, nil
		}
		return page.DeepCopy(), nil
	}
	if f.list == nil {
		return nil, nil
	}
	return f.list.DeepCopy(), nil
}

type runtimeAdmissionFakeServiceAccounts struct {
	accounts map[string]*corev1.ServiceAccount
	errors   map[string]error
	gets     []string
}

func (f *runtimeAdmissionFakeServiceAccounts) Get(_ context.Context, name string, _ metav1.GetOptions) (*corev1.ServiceAccount, error) {
	f.gets = append(f.gets, name)
	if err := f.errors[name]; err != nil {
		return nil, err
	}
	account := f.accounts[name]
	if account == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "serviceaccounts"}, name)
	}
	return account.DeepCopy(), nil
}

type runtimeAdmissionFakePriorityClasses struct {
	list             *schedulingv1.PriorityClassList
	pages            map[string]*schedulingv1.PriorityClassList
	classes          map[string]*schedulingv1.PriorityClass
	getErrors        map[string]error
	listError        error
	errorsByContinue map[string]error
	listCalls        int
	lastOptions      metav1.ListOptions
	calls            []metav1.ListOptions
}

func (f *runtimeAdmissionFakePriorityClasses) Get(_ context.Context, name string, _ metav1.GetOptions) (*schedulingv1.PriorityClass, error) {
	if err := f.getErrors[name]; err != nil {
		return nil, err
	}
	priorityClass := f.classes[name]
	if priorityClass == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: schedulingv1.GroupName, Resource: "priorityclasses"}, name)
	}
	return priorityClass.DeepCopy(), nil
}

func (f *runtimeAdmissionFakePriorityClasses) List(_ context.Context, options metav1.ListOptions) (*schedulingv1.PriorityClassList, error) {
	f.listCalls++
	f.lastOptions = options
	f.calls = append(f.calls, options)
	if f.listError != nil {
		return nil, f.listError
	}
	if err := f.errorsByContinue[options.Continue]; err != nil {
		return nil, err
	}
	if f.pages != nil {
		page, found := f.pages[options.Continue]
		if !found {
			return nil, errors.New("unexpected PriorityClass continue token " + options.Continue)
		}
		if page == nil {
			return nil, nil
		}
		return page.DeepCopy(), nil
	}
	if f.list == nil {
		return nil, nil
	}
	return f.list.DeepCopy(), nil
}

func TestRuntimeAdmissionPreflightPropagatesReaderErrors(t *testing.T) {
	t.Run("LimitRange", func(t *testing.T) {
		preflight, limitRanges, _, _ := runtimeAdmissionPreflightFixture()
		limitRanges.err = errors.New("read failed")
		if err := preflight.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "list LimitRanges in namespace operators: read failed") {
			t.Fatalf("Check() error = %v", err)
		}
	})

	t.Run("PriorityClass", func(t *testing.T) {
		preflight, _, _, priorityClasses := runtimeAdmissionPreflightFixture()
		priorityClasses.listError = errors.New("read failed")
		if err := preflight.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "list global default PriorityClasses: read failed") {
			t.Fatalf("Check() error = %v", err)
		}
	})
}
