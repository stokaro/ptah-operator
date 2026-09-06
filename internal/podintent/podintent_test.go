package podintent_test

import (
	"context"
	"fmt"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/podintent"
)

func TestResolvedBuiltInAdmissionMutationsAreAccepted(t *testing.T) {
	t.Parallel()

	template, snapshot := resolvedFixture(t)
	actual := admittedPodSpec(template, snapshot)
	if err := podintent.ValidatePodSpec(actual, podTemplate(template), snapshot); err != nil {
		t.Fatalf("ValidatePodSpec() error = %v", err)
	}
}

func TestAdmissionSnapshotRejectsResourceReinterpretation(t *testing.T) {
	t.Parallel()

	template, snapshot := resolvedFixture(t)
	tests := map[string]func(*corev1.PodSpec){
		"configured resource changed": func(spec *corev1.PodSpec) {
			spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("192Mi")
		},
		"default quantity changed": func(spec *corev1.PodSpec) {
			spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("999m")
		},
		"unsnapshotted resource added": func(spec *corev1.PodSpec) {
			spec.Containers[0].Resources.Requests[corev1.ResourceEphemeralStorage] = resource.MustParse("1Gi")
		},
		"default omitted": func(spec *corev1.PodSpec) {
			delete(spec.Containers[0].Resources.Limits, corev1.ResourceMemory)
		},
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			actual := admittedPodSpec(template, snapshot)
			mutate(actual)
			if err := podintent.ValidatePodSpec(actual, podTemplate(template), snapshot); err == nil {
				t.Fatal("ValidatePodSpec() accepted resource reinterpretation")
			}
		})
	}
}

func TestAdmissionSnapshotAcceptsOnlyExactLimitDerivedRequests(t *testing.T) {
	t.Parallel()

	template, snapshot := resolvedFixture(t)
	actual := admittedPodSpec(template, snapshot)
	if err := podintent.ValidatePodSpec(actual, podTemplate(template), snapshot); err != nil {
		t.Fatalf("ValidatePodSpec() rejected the API-defaulted init-container request: %v", err)
	}
	actual.InitContainers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("255Mi")
	if err := podintent.ValidatePodSpec(actual, podTemplate(template), snapshot); err == nil {
		t.Fatal("ValidatePodSpec() accepted a request different from its exact admitted limit")
	}
}

func TestAdmissionAcceptsServiceAccountAliasAcrossSupportedVersions(t *testing.T) {
	t.Parallel()

	template, snapshot := resolvedFixture(t)
	actual := admittedPodSpec(template, snapshot)
	actual.DeprecatedServiceAccount = "schema-jobs"
	if err := podintent.ValidatePodSpec(actual, podTemplate(template), snapshot); err != nil {
		t.Fatalf("ValidatePodSpec() rejected the 1.37 ServiceAccount alias: %v", err)
	}
	actual.DeprecatedServiceAccount = "different-account"
	if err := podintent.ValidatePodSpec(actual, podTemplate(template), snapshot); err == nil {
		t.Fatal("ValidatePodSpec() accepted a mismatched deprecated ServiceAccount alias")
	}
}

func TestTemplateDigestNormalizesSupportedServiceAccountAliases(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		legacy corev1.PodSpec
		newer  corev1.PodSpec
	}{
		{
			name:   "explicit",
			legacy: corev1.PodSpec{ServiceAccountName: "schema-jobs"},
			newer: corev1.PodSpec{
				ServiceAccountName: "schema-jobs", DeprecatedServiceAccount: "schema-jobs",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			legacy, err := podintent.DigestTemplate(podTemplate(&test.legacy))
			if err != nil {
				t.Fatal(err)
			}
			newer, err := podintent.DigestTemplate(podTemplate(&test.newer))
			if err != nil {
				t.Fatal(err)
			}
			if legacy != newer {
				t.Fatalf("supported ServiceAccount aliases have different template digests: %q != %q", legacy, newer)
			}
		})
	}
}

func TestAdmissionSnapshotPreservesExecutableAndSecurityFields(t *testing.T) {
	t.Parallel()

	template, snapshot := resolvedFixture(t)
	tests := map[string]func(*corev1.PodSpec){
		"command": func(spec *corev1.PodSpec) {
			spec.Containers[0].Command = []string{"/bin/sh"}
		},
		"image": func(spec *corev1.PodSpec) {
			spec.Containers[0].Image = "example.invalid/replaced@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		},
		"environment": func(spec *corev1.PodSpec) {
			spec.Containers[0].Env = append(spec.Containers[0].Env, corev1.EnvVar{Name: "UNSAFE", Value: "1"})
		},
		"volume": func(spec *corev1.PodSpec) {
			spec.Volumes = append(spec.Volumes, corev1.Volume{Name: "host", VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/"},
			}})
		},
		"container security": func(spec *corev1.PodSpec) {
			value := true
			spec.Containers[0].SecurityContext.Privileged = &value
		},
		"pod security": func(spec *corev1.PodSpec) {
			value := true
			spec.HostNetwork = value
		},
		"node selector": func(spec *corev1.PodSpec) {
			spec.NodeSelector["unsafe.example/node"] = "true"
		},
		"toleration": func(spec *corev1.PodSpec) {
			spec.Tolerations = append(spec.Tolerations, corev1.Toleration{
				Key: "unsafe.example/taint", Operator: corev1.TolerationOpExists,
			})
		},
		"default toleration seconds": func(spec *corev1.PodSpec) {
			seconds := int64(301)
			spec.Tolerations[0].TolerationSeconds = &seconds
		},
		"default toleration order": func(spec *corev1.PodSpec) {
			spec.Tolerations[0], spec.Tolerations[1] = spec.Tolerations[1], spec.Tolerations[0]
		},
		"default toleration duplicate": func(spec *corev1.PodSpec) {
			spec.Tolerations = append(spec.Tolerations, *spec.Tolerations[0].DeepCopy())
		},
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			actual := admittedPodSpec(template, snapshot)
			mutate(actual)
			if err := podintent.ValidatePodSpec(actual, podTemplate(template), snapshot); err == nil {
				t.Fatal("ValidatePodSpec() accepted a security-critical mutation")
			}
		})
	}
}

func TestAdmissionSnapshotDigestRejectsResourceVersionTampering(t *testing.T) {
	t.Parallel()

	_, snapshot := resolvedFixture(t)
	snapshot.RuntimeClass.Object.ResourceVersion = "changed"
	if err := podintent.ValidateSnapshot(snapshot); err == nil {
		t.Fatal("ValidateSnapshot() accepted mutated source identity")
	}
}

func TestAdmissionPluginSwitchesAreExact(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		options podintent.Options
		mutate  func(*corev1.PodSpec)
	}{
		{
			name:    "disabled DefaultTolerationSeconds rejects injected defaults",
			options: podintent.Options{},
			mutate: func(spec *corev1.PodSpec) {
				seconds := int64(300)
				spec.Tolerations = append(spec.Tolerations, corev1.Toleration{
					Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists,
					Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &seconds,
				})
			},
		},
		{
			name: "enabled DefaultTolerationSeconds rejects missing defaults",
			options: podintent.Options{
				DefaultTolerationsEnabled: true, DefaultNotReadyTolerationSeconds: 0,
				DefaultUnreachableTolerationSeconds: 0,
			},
			mutate: func(spec *corev1.PodSpec) { spec.Tolerations = spec.Tolerations[2:] },
		},
		{
			name:    "disabled ExtendedResourceToleration rejects derived toleration",
			options: podintent.Options{},
			mutate: func(spec *corev1.PodSpec) {
				spec.Tolerations = append(spec.Tolerations, corev1.Toleration{
					Key: "example.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
				})
			},
		},
		{
			name: "enabled ExtendedResourceToleration rejects missing derived toleration",
			options: podintent.Options{
				ExtendedResourceTolerationEnabled: true,
			},
			mutate: func(spec *corev1.PodSpec) {
				for i, toleration := range spec.Tolerations {
					if toleration.Key == "example.com/gpu" {
						spec.Tolerations = append(spec.Tolerations[:i], spec.Tolerations[i+1:]...)
						return
					}
				}
			},
		},
		{
			name:    "disabled AlwaysPullImages rejects pull-policy upgrade",
			options: podintent.Options{},
			mutate:  func(spec *corev1.PodSpec) { spec.Containers[0].ImagePullPolicy = corev1.PullAlways },
		},
		{
			name: "enabled AlwaysPullImages rejects original pull policy",
			options: podintent.Options{
				AlwaysPullImagesEnabled: true,
			},
			mutate: func(spec *corev1.PodSpec) { spec.Containers[0].ImagePullPolicy = corev1.PullIfNotPresent },
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			template, snapshot := resolvedFixtureWithOptions(t, test.options)
			actual := admittedPodSpec(template, snapshot)
			if err := podintent.ValidatePodSpec(actual, podTemplate(template), snapshot); err != nil {
				t.Fatalf("valid exact configuration rejected before mutation: %v", err)
			}
			test.mutate(actual)
			if err := podintent.ValidatePodSpec(actual, podTemplate(template), snapshot); err == nil {
				t.Fatal("ValidatePodSpec() accepted a mutation from a differently configured admission chain")
			}
		})
	}
}

func TestPersistedSnapshotDoesNotReinterpretChangedAdmissionResources(t *testing.T) {
	t.Parallel()

	template, snapshot := resolvedFixture(t)
	actual := admittedPodSpec(template, snapshot)
	actual.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("250m")
	actual.NodeSelector["kubernetes.io/os"] = "windows"
	if err := podintent.ValidatePodSpec(actual, podTemplate(template), snapshot); err == nil {
		t.Fatal("ValidatePodSpec() reinterpreted changed LimitRange and RuntimeClass values")
	}
	if err := podintent.ValidatePodSpec(admittedPodSpec(template, snapshot), podTemplate(template), snapshot); err != nil {
		t.Fatalf("ValidatePodSpec() no longer accepts the values bound before the resource change: %v", err)
	}
}

func TestPersistedSnapshotRejectsReplacementJobTemplate(t *testing.T) {
	t.Parallel()

	template, snapshot := resolvedFixture(t)
	replacement := podTemplate(template)
	replacement.Spec.Containers[0].Command = []string{"/bin/sh"}
	actual := admittedPodSpec(&replacement.Spec, snapshot)
	if err := podintent.ValidatePodSpec(actual, replacement, snapshot); err == nil {
		t.Fatal("ValidatePodSpec() accepted a replacement Job template with a new command")
	}
}

func TestPersistedSnapshotNormalizesOnlyGeneratedJobIdentity(t *testing.T) {
	t.Parallel()

	template, snapshot := resolvedFixture(t)
	stored := podTemplate(template)
	stored.Annotations = map[string]string{"operator.ptah.dev/admission-snapshot-digest": snapshot.Digest}
	stored.Labels = map[string]string{
		batchv1.ControllerUidLabel: "job-uid",
		batchv1.JobNameLabel:       "operation-job",
		"controller-uid":           "job-uid",
		"job-name":                 "operation-job",
	}
	if err := podintent.ValidatePodSpec(admittedPodSpec(template, snapshot), stored, snapshot); err != nil {
		t.Fatalf("ValidatePodSpec() rejected API-generated Job template identity: %v", err)
	}
	stored.Labels["unmodeled.example/label"] = "changed"
	if err := podintent.ValidatePodSpec(admittedPodSpec(template, snapshot), stored, snapshot); err == nil {
		t.Fatal("ValidatePodSpec() normalized an unmodeled Job template label")
	}
}

func TestLimitRangeDerivedDefaultsAreExactForRegularAndInitContainers(t *testing.T) {
	t.Parallel()

	limitRange := &corev1.LimitRange{
		ObjectMeta: objectMeta("team-a", "derived", "derived-uid", "2"),
		Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
			Type: corev1.LimitTypeContainer,
			Default: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
			Max: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("500m"),
			},
			Min: corev1.ResourceList{
				corev1.ResourceEphemeralStorage: resource.MustParse("64Mi"),
			},
		}}},
	}
	template, snapshot := minimalResolvedFixture(t, podintent.Options{}, limitRange)
	actual := template.DeepCopy()
	defaults := snapshot.LimitRanges[0]
	derivedRequests := corev1.ResourceList{
		corev1.ResourceMemory:           resource.MustParse("256Mi"),
		corev1.ResourceCPU:              resource.MustParse("500m"),
		corev1.ResourceEphemeralStorage: resource.MustParse("64Mi"),
	}
	for name, want := range derivedRequests {
		if got, ok := defaults.DefaultRequests[name]; !ok || got.Cmp(want) != 0 {
			t.Fatalf("derived default request %s = %v, want %s", name, got.String(), want.String())
		}
	}
	for _, name := range []corev1.ResourceName{corev1.ResourceMemory, corev1.ResourceCPU} {
		want := defaults.DefaultLimits[name]
		for i := range actual.Containers {
			actual.Containers[i].Resources.Limits[name] = want.DeepCopy()
			actual.Containers[i].Resources.Requests[name] = want.DeepCopy()
		}
		for i := range actual.InitContainers {
			actual.InitContainers[i].Resources.Limits[name] = want.DeepCopy()
			actual.InitContainers[i].Resources.Requests[name] = want.DeepCopy()
		}
	}
	for _, containers := range [][]corev1.Container{actual.Containers, actual.InitContainers} {
		for i := range containers {
			containers[i].Resources.Requests[corev1.ResourceEphemeralStorage] = resource.MustParse("64Mi")
		}
	}
	actual.ServiceAccountName = "default"
	setDefaultPriority(actual)
	if err := podintent.ValidatePodSpec(actual, podTemplate(template), snapshot); err != nil {
		t.Fatalf("ValidatePodSpec() rejected exact LimitRange-derived requests: %v", err)
	}
	actual.InitContainers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse("255Mi")
	if err := podintent.ValidatePodSpec(actual, podTemplate(template), snapshot); err == nil {
		t.Fatal("ValidatePodSpec() accepted a derived request different from its exact admitted limit")
	}
}

func TestResolveRejectsConflictingLimitRangeDefaultsForUnsetField(t *testing.T) {
	t.Parallel()

	one := &corev1.LimitRange{ObjectMeta: objectMeta("team-a", "one", "one-uid", "2"), Spec: corev1.LimitRangeSpec{
		Limits: []corev1.LimitRangeItem{{Type: corev1.LimitTypeContainer, DefaultRequest: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("100m"),
		}}},
	}}
	two := &corev1.LimitRange{ObjectMeta: objectMeta("team-a", "two", "two-uid", "3"), Spec: corev1.LimitRangeSpec{
		Limits: []corev1.LimitRangeItem{{Type: corev1.LimitTypeContainer, DefaultRequest: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("200m"),
		}}},
	}}
	if _, _, err := resolveMinimalFixture(t, podintent.Options{}, one, two); err == nil {
		t.Fatal("Resolve() accepted ambiguous first-wins LimitRange defaults")
	}
}

func TestAdmissionSnapshotBoundsAggregateLimitRangeDefaults(t *testing.T) {
	t.Parallel()

	requests := corev1.ResourceList{}
	limits := corev1.ResourceList{}
	for i := 0; i < 33; i++ {
		requests[corev1.ResourceName(fmt.Sprintf("requests.example/r%d", i))] = resource.MustParse("1")
		limits[corev1.ResourceName(fmt.Sprintf("limits.example/r%d", i))] = resource.MustParse("1")
	}
	limitRange := &corev1.LimitRange{
		ObjectMeta: objectMeta("team-a", "oversized", "oversized-uid", "2"),
		Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
			Type: corev1.LimitTypeContainer, DefaultRequest: requests, Default: limits,
		}}},
	}
	if _, _, err := resolveMinimalFixture(t, podintent.Options{}, limitRange); err == nil {
		t.Fatal("Resolve() accepted more than 64 aggregate LimitRange default entries")
	}

	_, snapshot := minimalResolvedFixture(t, podintent.Options{})
	snapshot.LimitRanges = []operatorv1alpha1.LimitRangeAdmissionSnapshot{{
		Object:          operatorv1alpha1.AdmissionObjectBinding{Name: "oversized", UID: "oversized-uid", ResourceVersion: "2"},
		DefaultRequests: requests,
		DefaultLimits:   limits,
	}}
	snapshot.Digest = ""
	digest, err := fingerprint.DigestCanonicalJSON(*snapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Digest = digest
	if err := podintent.ValidateSnapshot(snapshot); err == nil {
		t.Fatal("ValidateSnapshot() accepted more than 64 aggregate LimitRange default entries")
	}
}

func TestResolveRejectsPodLevelResourcesBeforeDispatch(t *testing.T) {
	t.Parallel()

	template, _, err := resolveMinimalFixture(t, podintent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	template.Resources = &corev1.ResourceRequirements{}
	if _, err := resolveMinimalTemplate(t, template, podintent.Options{}); err == nil {
		t.Fatal("Resolve() accepted unsupported Pod-level resources")
	}
}

func TestAdmissionAcceptsSupportedServiceAccountDefaultAliases(t *testing.T) {
	t.Parallel()

	for _, alias := range []string{"", "default"} {
		alias := alias
		t.Run("alias="+alias, func(t *testing.T) {
			t.Parallel()
			template, snapshot := minimalResolvedFixture(t, podintent.Options{})
			template.ServiceAccountName = "default"
			template.DeprecatedServiceAccount = alias
			setDefaultPriority(template)
			if err := podintent.ValidatePodSpec(template, podTemplate(&corev1.PodSpec{
				RestartPolicy:  corev1.RestartPolicyNever,
				Containers:     minimalContainers(),
				InitContainers: minimalInitContainers(),
			}), snapshot); err != nil {
				t.Fatalf("ValidatePodSpec() rejected supported default ServiceAccount alias %q: %v", alias, err)
			}
		})
	}
}

func TestAdmissionPreservesNilTolerationsWhenPluginsAreDisabled(t *testing.T) {
	t.Parallel()

	template, snapshot := minimalResolvedFixture(t, podintent.Options{})
	template.ServiceAccountName = "default"
	setDefaultPriority(template)
	if template.Tolerations != nil {
		t.Fatal("minimal fixture unexpectedly has tolerations")
	}
	if err := podintent.ValidatePodSpec(template, podTemplate(&corev1.PodSpec{
		RestartPolicy:  corev1.RestartPolicyNever,
		Containers:     minimalContainers(),
		InitContainers: minimalInitContainers(),
	}), snapshot); err != nil {
		t.Fatalf("ValidatePodSpec() rejected nil tolerations: %v", err)
	}
}

func TestAdmissionOptionsAllowLongKubeAPIServerTolerations(t *testing.T) {
	t.Parallel()

	options := podintent.Options{
		DefaultTolerationsEnabled:           true,
		DefaultNotReadyTolerationSeconds:    604800,
		DefaultUnreachableTolerationSeconds: 604800,
	}
	if err := options.Validate(); err != nil {
		t.Fatalf("Validate() rejected a valid seven-day kube-apiserver configuration: %v", err)
	}
	options.DefaultNotReadyTolerationSeconds = -1
	if err := options.Validate(); err == nil {
		t.Fatal("Validate() accepted a negative toleration duration")
	}
}

func minimalResolvedFixture(
	t *testing.T,
	options podintent.Options,
	objects ...runtime.Object,
) (*corev1.PodSpec, *operatorv1alpha1.PodAdmissionSnapshot) {
	t.Helper()
	template, snapshot, err := resolveMinimalFixture(t, options, objects...)
	if err != nil {
		t.Fatal(err)
	}
	return template, snapshot
}

func resolveMinimalFixture(
	t *testing.T,
	options podintent.Options,
	objects ...runtime.Object,
) (*corev1.PodSpec, *operatorv1alpha1.PodAdmissionSnapshot, error) {
	t.Helper()
	template := &corev1.PodSpec{
		RestartPolicy:  corev1.RestartPolicyNever,
		Containers:     minimalContainers(),
		InitContainers: minimalInitContainers(),
	}
	snapshot, err := resolveMinimalTemplateWithObjects(t, template, options, objects...)
	return template, snapshot, err
}

func resolveMinimalTemplate(
	t *testing.T,
	template *corev1.PodSpec,
	options podintent.Options,
) (*operatorv1alpha1.PodAdmissionSnapshot, error) {
	t.Helper()
	return resolveMinimalTemplateWithObjects(t, template, options)
}

func resolveMinimalTemplateWithObjects(
	t *testing.T,
	template *corev1.PodSpec,
	options podintent.Options,
	objects ...runtime.Object,
) (*operatorv1alpha1.PodAdmissionSnapshot, error) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, nodev1.AddToScheme, schedulingv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	allObjects := []runtime.Object{&corev1.ServiceAccount{ObjectMeta: objectMeta("team-a", "default", "default-sa-uid", "1")}}
	allObjects = append(allObjects, objects...)
	reader := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(allObjects...).Build()
	return podintent.Resolve(context.Background(), reader, "team-a", podTemplate(template), options)
}

func minimalContainers() []corev1.Container {
	return []corev1.Container{{
		Name: "ptah", Image: "example.invalid/ptah@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{}},
	}}
}

func minimalInitContainers() []corev1.Container {
	return []corev1.Container{{
		Name: "install", Image: "example.invalid/runner@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{}},
	}}
}

func setDefaultPriority(spec *corev1.PodSpec) {
	priority := int32(0)
	policy := corev1.PreemptLowerPriority
	spec.Priority = &priority
	spec.PreemptionPolicy = &policy
}

func resolvedFixture(t *testing.T) (*corev1.PodSpec, *operatorv1alpha1.PodAdmissionSnapshot) {
	t.Helper()
	options := podintent.DefaultOptions()
	options.ExtendedResourceTolerationEnabled = true
	options.AlwaysPullImagesEnabled = true
	return resolvedFixtureWithOptions(t, options)
}

func resolvedFixtureWithOptions(
	t *testing.T,
	options podintent.Options,
) (*corev1.PodSpec, *operatorv1alpha1.PodAdmissionSnapshot) {
	t.Helper()
	return resolvedFixtureWithOptionsAndMetadata(t, options, metav1.ObjectMeta{})
}

func resolvedFixtureWithOptionsAndMetadata(
	t *testing.T,
	options podintent.Options,
	metadata metav1.ObjectMeta,
) (*corev1.PodSpec, *operatorv1alpha1.PodAdmissionSnapshot) {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		nodev1.AddToScheme,
		schedulingv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	objects := admissionFixtureObjects()
	reader := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build()
	runtimeClassName := "sandbox"
	falseValue := false
	template := &corev1.PodSpec{
		ServiceAccountName:           "schema-jobs",
		AutomountServiceAccountToken: &falseValue,
		RuntimeClassName:             &runtimeClassName,
		PriorityClassName:            "database",
		NodeSelector:                 map[string]string{"workload": "database"},
		RestartPolicy:                corev1.RestartPolicyNever,
		Containers: []corev1.Container{{
			Name: "ptah", Image: "example.invalid/ptah@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         []string{"/runner/ptah-runner"},
			Env:             []corev1.EnvVar{{Name: "OPERATION", Value: "plan"}},
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			}},
			SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &falseValue},
		}},
		InitContainers: []corev1.Container{{
			Name: "install-runner", Image: "example.invalid/runner@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			ImagePullPolicy: corev1.PullIfNotPresent,
			Resources:       corev1.ResourceRequirements{},
		}},
		Volumes: []corev1.Volume{{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
	}
	templateObject := podTemplate(template)
	templateObject.ObjectMeta = *metadata.DeepCopy()
	snapshot, err := podintent.Resolve(context.Background(), reader, "team-a", templateObject, options)
	if err != nil {
		t.Fatal(err)
	}
	return template, snapshot
}

func admissionFixtureObjects() []runtime.Object {
	preemptNever := corev1.PreemptNever
	return []runtime.Object{
		&corev1.ServiceAccount{ObjectMeta: objectMeta("team-a", "schema-jobs", "sa-uid", "11"), ImagePullSecrets: []corev1.LocalObjectReference{{Name: "registry"}}},
		&corev1.LimitRange{ObjectMeta: objectMeta("team-a", "defaults", "limits-uid", "12"), Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
			Type: corev1.LimitTypeContainer,
			DefaultRequest: corev1.ResourceList{
				corev1.ResourceCPU:                     resource.MustParse("100m"),
				corev1.ResourceName("example.com/gpu"): resource.MustParse("1"),
			},
			Default: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		}}}},
		&nodev1.RuntimeClass{ObjectMeta: clusterObjectMeta("sandbox", "runtime-uid", "13"), Handler: "runc",
			Overhead: &nodev1.Overhead{PodFixed: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Mi")}},
			Scheduling: &nodev1.Scheduling{
				NodeSelector: map[string]string{"kubernetes.io/os": "linux"},
				Tolerations:  []corev1.Toleration{{Key: "sandbox", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}},
			}},
		&schedulingv1.PriorityClass{ObjectMeta: clusterObjectMeta("database", "priority-uid", "14"), Value: 1000, PreemptionPolicy: &preemptNever},
	}
}

func podTemplate(spec *corev1.PodSpec) *corev1.PodTemplateSpec {
	return &corev1.PodTemplateSpec{Spec: *spec.DeepCopy()}
}

func admittedPodSpec(template *corev1.PodSpec, snapshot *operatorv1alpha1.PodAdmissionSnapshot) *corev1.PodSpec {
	actual := template.DeepCopy()
	actual.ServiceAccountName = "schema-jobs"
	actual.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "registry"}}
	actual.NodeSelector["kubernetes.io/os"] = "linux"
	actual.Overhead = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Mi")}
	priority := int32(1000)
	policy := corev1.PreemptNever
	actual.Priority = &priority
	actual.PreemptionPolicy = &policy
	actual.NodeName = "worker-1"
	if snapshot.DefaultTolerationsEnabled {
		notReady := snapshot.DefaultNotReadyTolerationSeconds
		unreachable := snapshot.DefaultUnreachableTolerationSeconds
		actual.Tolerations = append(actual.Tolerations,
			corev1.Toleration{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &notReady},
			corev1.Toleration{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &unreachable},
		)
	}
	if snapshot.ExtendedResourceTolerationEnabled {
		actual.Tolerations = append(actual.Tolerations, corev1.Toleration{
			Key: "example.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
		})
	}
	actual.Tolerations = append(actual.Tolerations, corev1.Toleration{
		Key: "sandbox", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
	})
	for i := range actual.Containers {
		actual.Containers[i].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("100m")
		actual.Containers[i].Resources.Requests[corev1.ResourceName("example.com/gpu")] = resource.MustParse("1")
		actual.Containers[i].Resources.Limits = corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi"),
		}
		if snapshot.AlwaysPullImagesEnabled {
			actual.Containers[i].ImagePullPolicy = corev1.PullAlways
		}
	}
	for i := range actual.InitContainers {
		actual.InitContainers[i].Resources.Requests = corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi"),
			corev1.ResourceName("example.com/gpu"): resource.MustParse("1"),
		}
		actual.InitContainers[i].Resources.Limits = corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi"),
		}
		if snapshot.AlwaysPullImagesEnabled {
			actual.InitContainers[i].ImagePullPolicy = corev1.PullAlways
		}
	}
	return actual
}

func objectMeta(namespace, name string, uid types.UID, resourceVersion string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: namespace, Name: name, UID: uid, ResourceVersion: resourceVersion}
}

func clusterObjectMeta(name string, uid types.UID, resourceVersion string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, UID: uid, ResourceVersion: resourceVersion}
}
