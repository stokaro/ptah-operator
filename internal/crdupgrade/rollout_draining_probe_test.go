package crdupgrade

// These white-box tests evaluate internal overlapping admission contracts.
// They do not claim API-server enforcement or runtime lifecycle coverage.

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func TestRolloutGuardDrainingEnforcementProbe(t *testing.T) {
	t.Parallel()
	objects := renderControllerRBACCutoverChart(t, "--set", "replicaCount=3")
	for _, test := range []struct {
		name              string
		state             int32
		certificate       bool
		bootstrap         bool
		controllerMissing bool
	}{
		{name: "unchanged state certificate", state: 1, certificate: true},
		{name: "changed state certificate", state: 2, certificate: true},
		{name: "missing certificate uses predecessor controller replicas", state: 1},
		{name: "bootstrap stopped controller", state: 1, bootstrap: true},
		{name: "bootstrap stopped certificate fallback", state: 1, certificate: true, bootstrap: true, controllerMissing: true},
		{name: "stopped certificate fallback", state: 1, certificate: true, controllerMissing: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newDrainingProbeFixture(t, objects, test.state, test.certificate, test.bootstrap)
			if test.controllerMissing {
				delete(fixture.client.objects, fixture.guard.ControllerDeploymentName)
			}
			for _, target := range []struct{ name, message string }{
				{RolloutGuardPolicyName(fixture.guard.ReleaseSequence), rolloutGuardProbeDenialMessage(fixture.guard.ReleaseSequence)},
				{RuntimeGuardPolicyName(fixture.guard.ReleaseSequence), runtimeGuardProbeDenialMessage(fixture.guard.ReleaseSequence)},
			} {
				t.Run(target.name, func(t *testing.T) {
					fixture.client.updates = nil
					originals := make(map[string]*appsv1.Deployment, len(fixture.client.objects))
					for name, deployment := range fixture.client.objects {
						originals[name] = deployment.DeepCopy()
					}
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					t.Cleanup(cancel)
					if err := fixture.guard.waitEnforced(ctx, target.name, target.message); err != nil {
						t.Fatal(err)
					}
					if len(fixture.client.updates) != 2 {
						t.Fatalf("dry-run requests = %d, want baseline then sentinel", len(fixture.client.updates))
					}
					baseline := fixture.client.updates[0]
					wantName := fixture.guard.ControllerDeploymentName
					wantReplicas := int32(3)
					if test.certificate {
						wantName = fixture.guard.CertificateDeploymentName
						wantReplicas = 1
					}
					if test.bootstrap {
						wantReplicas = 0
					}
					if baseline.Name != wantName || baseline.Spec.Replicas == nil || *baseline.Spec.Replicas != wantReplicas {
						t.Fatalf("baseline name/replicas = %s/%v, want %s/%d", baseline.Name, baseline.Spec.Replicas, wantName, wantReplicas)
					}
					original := fixture.client.objects[wantName]
					restored := baseline.DeepCopy()
					restored.Annotations[ControllerStateVersionAnnotation] = original.Annotations[ControllerStateVersionAnnotation]
					restored.Annotations[ReleaseSequenceAnnotation] = original.Annotations[ReleaseSequenceAnnotation]
					restored.Spec.Replicas = original.Spec.Replicas
					if !reflect.DeepEqual(restored, original) {
						t.Fatal("probe baseline changed something beyond its two top-level identity annotations and dry-run replicas")
					}
					probe := fixture.client.updates[1].DeepCopy()
					if probe.Annotations[guardEnforcementProbeAnnotation] != target.name {
						t.Fatal("sentinel request lacks the exact target policy token")
					}
					delete(probe.Annotations, guardEnforcementProbeAnnotation)
					if !reflect.DeepEqual(probe, baseline) {
						t.Fatal("sentinel request changed more than its reserved annotation")
					}
					if !reflect.DeepEqual(originals, fixture.client.objects) {
						t.Fatal("enforcement probing mutated a stored source Deployment")
					}
				})
			}
		})
	}
}

func TestRolloutGuardDrainingProbeRejectsUnprovenIdentity(t *testing.T) {
	t.Parallel()
	objects := renderControllerRBACCutoverChart(t)
	for _, test := range []struct {
		name   string
		mutate func(*RolloutGuard, *appsv1.Deployment, *corev1.ConfigMap)
	}{
		{name: "wrong attempt", mutate: func(_ *RolloutGuard, _ *appsv1.Deployment, p *corev1.ConfigMap) {
			p.Data[controllerCredentialsAttemptDataKey] = strings.Repeat("0", 64)
		}},
		{name: "wrong target", mutate: func(_ *RolloutGuard, _ *appsv1.Deployment, p *corev1.ConfigMap) {
			p.Data[controllerCredentialsTargetDataKey] = "3"
		}},
		{name: "missing drain", mutate: func(_ *RolloutGuard, _ *appsv1.Deployment, p *corev1.ConfigMap) {
			p.Data[controllerCredentialsDataKey] = string(ControllerCredentialsActive)
			delete(p.Data, controllerCredentialsTargetDataKey)
			delete(p.Data, controllerCredentialsAttemptDataKey)
		}},
		{name: "foreign parameter", mutate: func(_ *RolloutGuard, _ *appsv1.Deployment, p *corev1.ConfigMap) { p.Labels[instanceLabel] = "foreign" }},
		{name: "missing UID", mutate: func(_ *RolloutGuard, d *appsv1.Deployment, _ *corev1.ConfigMap) { d.UID = "" }},
		{name: "missing RV", mutate: func(_ *RolloutGuard, d *appsv1.Deployment, _ *corev1.ConfigMap) { d.ResourceVersion = "" }},
		{name: "foreign ownership", mutate: func(_ *RolloutGuard, d *appsv1.Deployment, _ *corev1.ConfigMap) {
			d.Annotations[helmReleaseNameAnnotation] = "foreign"
		}},
		{name: "foreign service account", mutate: func(_ *RolloutGuard, d *appsv1.Deployment, _ *corev1.ConfigMap) {
			d.Spec.Template.Spec.ServiceAccountName = "foreign"
		}},
		{name: "not fully stopped", mutate: func(_ *RolloutGuard, d *appsv1.Deployment, _ *corev1.ConfigMap) { d.Status.Replicas = 1 }},
		{name: "deleting", mutate: func(_ *RolloutGuard, d *appsv1.Deployment, _ *corev1.ConfigMap) {
			now := metav1.Now()
			d.DeletionTimestamp = &now
		}},
		{name: "reserved annotation present", mutate: func(_ *RolloutGuard, d *appsv1.Deployment, _ *corev1.ConfigMap) {
			d.Annotations[guardEnforcementProbeAnnotation] = "foreign"
		}},
		{name: "wrong template state", mutate: func(_ *RolloutGuard, d *appsv1.Deployment, _ *corev1.ConfigMap) {
			d.Spec.Template.Annotations[ControllerStateVersionAnnotation] = "2"
		}},
		{name: "wrong template release", mutate: func(_ *RolloutGuard, d *appsv1.Deployment, _ *corev1.ConfigMap) {
			d.Spec.Template.Annotations[ReleaseSequenceAnnotation] = "2"
		}},
		{name: "wrong image", mutate: func(_ *RolloutGuard, d *appsv1.Deployment, _ *corev1.ConfigMap) {
			d.Spec.Template.Spec.Containers[0].Image = "foreign"
		}},
		{name: "wrong verifier image", mutate: func(_ *RolloutGuard, d *appsv1.Deployment, _ *corev1.ConfigMap) {
			d.Spec.Template.Spec.InitContainers[0].Image = "foreign"
		}},
		{name: "wrong verifier name", mutate: func(_ *RolloutGuard, d *appsv1.Deployment, _ *corev1.ConfigMap) {
			d.Spec.Template.Spec.InitContainers[0].Name = "foreign"
		}},
		{name: "wrong verifier command", mutate: func(_ *RolloutGuard, d *appsv1.Deployment, _ *corev1.ConfigMap) {
			d.Spec.Template.Spec.InitContainers[0].Command = []string{"/bin/sh"}
		}},
		{name: "missing verifier args", mutate: func(_ *RolloutGuard, d *appsv1.Deployment, _ *corev1.ConfigMap) {
			d.Spec.Template.Spec.InitContainers[0].Args = nil
		}},
		{name: "wrong verifier mode", mutate: func(_ *RolloutGuard, d *appsv1.Deployment, _ *corev1.ConfigMap) {
			d.Spec.Template.Spec.InitContainers[0].Args[0] = "reconcile"
		}},
		{name: "extra verifier", mutate: func(_ *RolloutGuard, d *appsv1.Deployment, _ *corev1.ConfigMap) {
			d.Spec.Template.Spec.InitContainers = append(d.Spec.Template.Spec.InitContainers, corev1.Container{Name: "extra"})
		}},
		{name: "missing replicas flag", mutate: func(_ *RolloutGuard, d *appsv1.Deployment, _ *corev1.ConfigMap) {
			d.Spec.Template.Spec.InitContainers[0].Args = slices.DeleteFunc(d.Spec.Template.Spec.InitContainers[0].Args, func(arg string) bool { return strings.HasPrefix(arg, "--controller-replicas=") })
		}},
		{name: "duplicate replicas flag", mutate: func(_ *RolloutGuard, d *appsv1.Deployment, _ *corev1.ConfigMap) {
			d.Spec.Template.Spec.InitContainers[0].Args = append(d.Spec.Template.Spec.InitContainers[0].Args, "--controller-replicas=2")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newDrainingProbeFixture(t, objects, 1, false, false)
			deployment := fixture.client.objects[fixture.guard.ControllerDeploymentName]
			parameter := fixture.guard.ConfigMaps.(*rolloutConfigMapClient).objects[ReleaseActivationName]
			test.mutate(fixture.guard, deployment, parameter)
			before := deployment.DeepCopy()
			if _, _, err := fixture.guard.enforcementProbeDeployment(context.Background()); err == nil {
				t.Fatal("unproven stopped baseline was accepted")
			}
			if !reflect.DeepEqual(before, deployment) || len(fixture.client.updates) != 0 {
				t.Fatal("refused probe changed or submitted the live Deployment")
			}
		})
	}
}

func TestPredecessorProbeControllerReplicas(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		args []string
		want int32
	}{
		{name: "one", args: []string{"--unrelated=true", "--controller-replicas=1"}, want: 1},
		{name: "maximum", args: []string{"--controller-replicas=2147483647"}, want: 2147483647},
		{name: "missing"},
		{name: "zero", args: []string{"--controller-replicas=0"}},
		{name: "negative", args: []string{"--controller-replicas=-1"}},
		{name: "leading zero", args: []string{"--controller-replicas=01"}},
		{name: "positive sign", args: []string{"--controller-replicas=+1"}},
		{name: "whitespace", args: []string{"--controller-replicas=1 "}},
		{name: "empty", args: []string{"--controller-replicas="}},
		{name: "split", args: []string{"--controller-replicas", "1"}},
		{name: "prefix decoy", args: []string{"--controller-replicas-extra=1"}},
		{name: "overflow", args: []string{"--controller-replicas=2147483648"}},
		{name: "huge", args: []string{"--controller-replicas=" + strings.Repeat("9", 40)}},
		{name: "duplicate", args: []string{"--controller-replicas=1", "--controller-replicas=1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := predecessorProbeControllerReplicas(test.args)
			if got != test.want || (err == nil) != (test.want > 0) {
				t.Fatalf("replicas = %d, error = %v, want %d", got, err, test.want)
			}
		})
	}
}

func TestRolloutGuardDrainingProbeFullCELControls(t *testing.T) {
	t.Parallel()
	objects := renderControllerRBACCutoverChart(t)
	for _, test := range []struct {
		name   string
		mutate func(*appsv1.Deployment)
	}{
		{name: "extra template label", mutate: func(d *appsv1.Deployment) { d.Spec.Template.Labels["extra"] = "not-in-retained-contract" }},
		{name: "extra template argument", mutate: func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.Containers[0].Args = append(d.Spec.Template.Spec.Containers[0].Args, "--extra=true")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newDrainingProbeFixture(t, objects, 1, false, false)
			test.mutate(fixture.client.objects[fixture.guard.ControllerDeploymentName])
			err := fixture.guard.waitEnforced(context.Background(), RolloutGuardPolicyName(2), rolloutGuardProbeDenialMessage(2))
			if err == nil || !strings.Contains(err.Error(), "prove baseline Deployment is accepted") {
				t.Fatalf("changed predecessor contract was not refused at the full baseline: %v", err)
			}
			if len(fixture.client.updates) != 1 || fixture.client.updates[0].Annotations[guardEnforcementProbeAnnotation] != "" {
				t.Fatal("sentinel was submitted after a rejected baseline")
			}
		})
	}

	fixture := newDrainingProbeFixture(t, objects, 1, false, false)
	baseline, _, err := fixture.guard.enforcementProbeDeployment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	baseline.Annotations[guardEnforcementProbeAnnotation] = RolloutGuardPolicyName(2)
	object, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(baseline)
	if err != nil {
		t.Fatal(err)
	}
	oldObject, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(fixture.client.objects[baseline.Name])
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		actor  string
		dryRun bool
	}{
		{name: "persistent sentinel", actor: rolloutHookUsername(fixture.guard)},
		{name: "wrong actor cannot produce target proof", actor: "helm", dryRun: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := rolloutRequestCELObject(fixture.guard, "UPDATE", "", test.actor)
			request["dryRun"] = test.dryRun
			ordinary, sentinel := 0, 0
			for _, policy := range fixture.client.policies {
				for index, allowed := range evaluatePolicyValidations(t, policy, object, oldObject, request, fixture.client.params) {
					if !allowed {
						if strings.Contains(policy.Spec.Validations[index].Message, "exact enforcement probe") {
							sentinel++
						} else {
							ordinary++
						}
					}
				}
			}
			if sentinel != 0 || (!test.dryRun && ordinary == 0) {
				t.Fatalf("invalid proof produced ordinary/exact denials %d/%d", ordinary, sentinel)
			}
		})
	}

	// The active-identity dry-run branch intentionally allows unrelated
	// top-level metadata. The production stop-transition branch remains exact:
	// extra metadata or template edits must never become a permitted stop.
	for _, field := range []string{"annotation", "label", "template"} {
		t.Run("stop transition rejects extra "+field, func(t *testing.T) {
			changed := rolloutCELClone(t, oldObject).(map[string]any)
			switch field {
			case "annotation":
				changed["metadata"].(map[string]any)["annotations"].(map[string]any)["extra"] = "value"
			case "label":
				changed["metadata"].(map[string]any)["labels"].(map[string]any)["extra"] = "value"
			case "template":
				changed["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["labels"].(map[string]any)["extra"] = "value"
			}
			request := rolloutRequestCELObject(fixture.guard, "UPDATE", "", rolloutHookUsername(fixture.guard))
			request["dryRun"] = true
			for _, policy := range fixture.client.policies {
				if results := evaluatePolicyValidations(t, policy, changed, oldObject, request, fixture.client.params); !slices.Contains(results, false) {
					t.Fatalf("%s admitted an altered stopped Deployment", policy.Name)
				}
			}
		})
	}
}

func TestRolloutGuardDrainingProbePreservesOtherStates(t *testing.T) {
	t.Parallel()
	objects := renderControllerRBACCutoverChart(t)
	for _, variant := range []string{"running", "unstamped", "different state", "active candidate", "draining active candidate"} {
		t.Run(variant, func(t *testing.T) {
			fixture := newDrainingProbeFixture(t, objects, 1, false, false)
			deployment := fixture.client.objects[fixture.guard.ControllerDeploymentName]
			switch variant {
			case "running":
				deployment.Spec.Replicas = int32Ptr(1)
			case "unstamped":
				delete(deployment.Annotations, ReleaseSequenceAnnotation)
			case "different state":
				deployment.Annotations[ControllerStateVersionAnnotation] = "2"
			default:
				parameter := fixture.guard.ConfigMaps.(*rolloutConfigMapClient).objects[ReleaseActivationName]
				parameter.Data[activeReleaseDataKey] = "2"
				parameter.Annotations[ReleaseSequenceAnnotation] = "2"
				parameter.Annotations[ManagerImageAnnotation] = fixture.guard.ManagerImage
				if variant == "active candidate" {
					parameter.Data[controllerCredentialsDataKey] = string(ControllerCredentialsActive)
					delete(parameter.Data, controllerCredentialsTargetDataKey)
					delete(parameter.Data, controllerCredentialsAttemptDataKey)
				}
			}
			before := deployment.DeepCopy()
			baseline, create, err := fixture.guard.enforcementProbeDeployment(context.Background())
			if err != nil || create || !reflect.DeepEqual(baseline, before) || !reflect.DeepEqual(deployment, before) {
				t.Fatalf("unrelated state was normalized: create=%t error=%v", create, err)
			}
		})
	}
	for _, variant := range []string{"unstopped", "wrong stamp", "foreign owner", "template identity", "live replicas"} {
		t.Run("refuse certificate "+variant, func(t *testing.T) {
			fixture := newDrainingProbeFixture(t, objects, 1, true, false)
			certificate := fixture.client.objects[fixture.guard.CertificateDeploymentName]
			switch variant {
			case "unstopped":
				certificate.Spec.Replicas = int32Ptr(1)
			case "wrong stamp":
				certificate.Annotations[ReleaseSequenceAnnotation] = "1"
			case "foreign owner":
				certificate.Annotations[helmReleaseNameAnnotation] = "foreign"
			case "template identity":
				certificate.Spec.Template.Spec.Containers[0].Image = "foreign"
			case "live replicas":
				certificate.Status.ReadyReplicas = 1
			}
			if _, _, err := fixture.guard.enforcementProbeDeployment(context.Background()); err == nil {
				t.Fatal("invalid certificate baseline was accepted or silently bypassed")
			}
		})
	}
	for _, annotation := range []string{ControllerStateVersionAnnotation, ReleaseSequenceAnnotation} {
		t.Run("bootstrap refuses template "+annotation, func(t *testing.T) {
			fixture := newDrainingProbeFixture(t, objects, 1, false, true)
			fixture.client.objects[fixture.guard.ControllerDeploymentName].Spec.Template.Annotations[annotation] = "1"
			if _, _, err := fixture.guard.enforcementProbeDeployment(context.Background()); err == nil {
				t.Fatal("bootstrap normalization accepted an already versioned template")
			}
		})
	}
}

func TestRolloutGuardDrainingProbeRereadsAfterConflict(t *testing.T) {
	t.Parallel()
	fixture := newDrainingProbeFixture(t, renderControllerRBACCutoverChart(t), 1, true, false)
	fixture.client.beforeUpdate = func(call int) {
		if call == 2 {
			fixture.client.objects[fixture.guard.CertificateDeploymentName].ResourceVersion = "102"
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	if err := fixture.guard.waitEnforced(ctx, RolloutGuardPolicyName(2), rolloutGuardProbeDenialMessage(2)); err != nil {
		t.Fatal(err)
	}
	if len(fixture.client.updates) != 4 {
		t.Fatalf("requests = %d, want fresh baseline and sentinel after conflict", len(fixture.client.updates))
	}
	for index, deployment := range fixture.client.updates {
		want := "101"
		if index >= 2 {
			want = "102"
		}
		if deployment.ResourceVersion != want {
			t.Fatalf("request %d RV = %q, want %q", index, deployment.ResourceVersion, want)
		}
	}
}

type drainingProbeFixture struct {
	guard  *RolloutGuard
	client *drainingProbeDeploymentClient
}

func newDrainingProbeFixture(t *testing.T, objects []*unstructured.Unstructured, state int32, certificate, bootstrap bool) drainingProbeFixture {
	t.Helper()
	job := findControllerRBACCutoverJob(t, objects)
	containers, _, _ := unstructured.NestedSlice(job.Object, "spec", "template", "spec", "containers")
	predecessor := renderedRolloutGuard(t, transitionRenderStringSlice(containers[0].(map[string]any)["args"]), "")
	value := *predecessor
	guard := &value
	guard.ControllerStateVersion = state
	guard.ControllerReplicas = 5 // Never substitute these candidate values for predecessor replicas.
	guard.PollEvery = time.Millisecond
	active := int64(1)
	if !bootstrap {
		guard.ReleaseSequence = 2
		guard.ManagerImage = "registry.example/ptah@sha256:" + strings.Repeat("b", 64)
	} else {
		active = 0
	}
	prefix, _, found := strings.Cut(predecessor.HookServiceAccountName, "-crd-v1-")
	if !found {
		t.Fatal("rendered predecessor hook lacks its sequence-qualified suffix")
	}
	guard.HookServiceAccountName = fmt.Sprintf("%s-crd-v%d-%s", prefix, guard.ReleaseSequence, hookIdentityDigest(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)[:12])
	params := rolloutActivationCELObject(predecessor, active, int64(predecessor.ControllerStateVersion), int64(predecessor.AdmissionContractVersion), 1, predecessor.ManagerImage)
	data := params["data"].(map[string]any)
	data[controllerCredentialsDataKey] = string(ControllerCredentialsDraining)
	data[controllerCredentialsTargetDataKey] = strconv.Itoa(int(guard.ReleaseSequence))
	data[controllerCredentialsAttemptDataKey] = guard.releaseActivationGuard().candidateAttempt()
	var parameter corev1.ConfigMap
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(params, &parameter); err != nil {
		t.Fatal(err)
	}
	guard.ConfigMaps = &rolloutConfigMapClient{objects: map[string]*corev1.ConfigMap{ReleaseActivationName: &parameter}}
	client := &drainingProbeDeploymentClient{
		rolloutDeploymentClient: &rolloutDeploymentClient{objects: map[string]*appsv1.Deployment{}},
		t:                       t, guard: guard, params: params,
		policies: []*admissionregistrationv1.ValidatingAdmissionPolicy{
			guard.policy(guard.ControllerStateVersion, guard.AdmissionContractVersion),
			guard.runtimePolicy(guard.ControllerStateVersion, guard.ReleaseSequence, guard.ManagerImage),
		},
	}
	if !bootstrap {
		for _, name := range []string{RolloutGuardPolicyName(1), RuntimeGuardPolicyName(1)} {
			var policy admissionregistrationv1.ValidatingAdmissionPolicy
			if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(findTransitionRenderObject(t, objects, "ValidatingAdmissionPolicy", name).Object, &policy); err != nil {
				t.Fatal(err)
			}
			client.policies = append(client.policies, &policy)
		}
	}
	for _, name := range []string{guard.ControllerDeploymentName, guard.CertificateDeploymentName} {
		if !certificate && name == guard.CertificateDeploymentName {
			continue
		}
		live := findTransitionRenderObject(t, objects, "Deployment", name).DeepCopy()
		defaultDrainingProbeDeployment(live.Object)
		live.SetUID(types.UID("live-" + name))
		live.SetResourceVersion("101")
		annotations := live.GetAnnotations()
		annotations[ControllerStateVersionAnnotation] = strconv.Itoa(int(state))
		annotations[ReleaseSequenceAnnotation] = strconv.Itoa(int(guard.ReleaseSequence))
		annotations[helmReleaseNameAnnotation] = guard.ReleaseName
		annotations[helmReleaseNamespaceAnnotation] = guard.ReleaseNamespace
		live.SetAnnotations(annotations)
		if err := unstructured.SetNestedField(live.Object, int64(0), "spec", "replicas"); err != nil {
			t.Fatal(err)
		}
		var deployment appsv1.Deployment
		if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(live.Object, &deployment); err != nil {
			t.Fatal(err)
		}
		if bootstrap {
			delete(deployment.Spec.Template.Annotations, ControllerStateVersionAnnotation)
			delete(deployment.Spec.Template.Annotations, ReleaseSequenceAnnotation)
		}
		client.objects[name] = &deployment
	}
	guard.Deployments = client
	return drainingProbeFixture{guard: guard, client: client}
}

// This client evaluates every policy validation against the unchanged stored
// object. It is a native-CEL unit test, not an API-server simulator.
type drainingProbeDeploymentClient struct {
	*rolloutDeploymentClient
	t            *testing.T
	guard        *RolloutGuard
	params       map[string]any
	policies     []*admissionregistrationv1.ValidatingAdmissionPolicy
	updates      []*appsv1.Deployment
	beforeUpdate func(int)
}

func (c *drainingProbeDeploymentClient) Update(_ context.Context, deployment *appsv1.Deployment, options metav1.UpdateOptions) (*appsv1.Deployment, error) {
	if !reflect.DeepEqual(options.DryRun, []string{metav1.DryRunAll}) {
		c.t.Fatal("enforcement probe attempted a persistent Deployment update")
	}
	c.updates = append(c.updates, deployment.DeepCopy())
	if c.beforeUpdate != nil {
		c.beforeUpdate(len(c.updates))
	}
	if deployment.ResourceVersion != c.objects[deployment.Name].ResourceVersion {
		return nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, deployment.Name, fmt.Errorf("resource version changed"))
	}
	object, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(deployment)
	if err != nil {
		c.t.Fatal(err)
	}
	oldObject, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(c.objects[deployment.Name])
	if err != nil {
		c.t.Fatal(err)
	}
	request := rolloutRequestCELObject(c.guard, "UPDATE", "", rolloutHookUsername(c.guard))
	request["name"] = deployment.Name
	request["dryRun"] = true
	var denials []error
	for _, policy := range c.policies {
		for index, allowed := range evaluatePolicyValidations(c.t, policy, object, oldObject, request, c.params) {
			if !allowed {
				denials = append(denials, exactPolicyDenialError(policy.Name, policy.Name, policy.Spec.Validations[index].Message))
			}
		}
	}
	if len(denials) == 1 {
		return nil, denials[0]
	}
	if len(denials) > 1 {
		return nil, fmt.Errorf("multiple admission denials (%d): %v", len(denials), denials)
	}
	return deployment.DeepCopy(), nil
}

func TestRolloutDrainingProbeMetadataNormalizationHitsRetainedReplicaContract(t *testing.T) {
	t.Parallel()
	objects := renderControllerRBACCutoverChart(t)
	job := findControllerRBACCutoverJob(t, objects)
	containers, found, err := unstructured.NestedSlice(job.Object, "spec", "template", "spec", "containers")
	if err != nil || !found || len(containers) != 1 {
		t.Fatal("rendered reconcile Job must have one container")
	}
	predecessor := renderedRolloutGuard(t, transitionRenderStringSlice(containers[0].(map[string]any)["args"]), "")
	currentValue := *predecessor
	current := &currentValue
	current.ReleaseSequence++
	current.ControllerStateVersion++
	current.ManagerImage = "registry.example/ptah@sha256:" + strings.Repeat("b", 64)
	hookPrefix, _, found := strings.Cut(predecessor.HookServiceAccountName, "-crd-v1-")
	if !found {
		t.Fatal("rendered predecessor hook lacks its sequence-qualified suffix")
	}
	current.HookServiceAccountName = hookPrefix + "-crd-v2-" + hookIdentityDigest(current.ReleaseNamespace, current.ReleaseName, current.ReleaseSequence, current.ManagerImage)[:12]
	params := rolloutActivationCELObject(predecessor, 1, int64(predecessor.ControllerStateVersion), int64(predecessor.AdmissionContractVersion), 1, predecessor.ManagerImage)
	data := params["data"].(map[string]any)
	data[controllerCredentialsDataKey] = string(ControllerCredentialsDraining)
	data[controllerCredentialsTargetDataKey] = "2"
	data[controllerCredentialsAttemptDataKey] = current.releaseActivationGuard().candidateAttempt()

	var retainedRuntime admissionregistrationv1.ValidatingAdmissionPolicy
	rendered := findTransitionRenderObject(t, objects, "ValidatingAdmissionPolicy", RuntimeGuardPolicyName(predecessor.ReleaseSequence))
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(rendered.Object, &retainedRuntime); err != nil {
		t.Fatal(err)
	}
	// Isolate this necessary retained validation, rather than silently using
	// the old truth table's first-three-validation prefix as the full policy.
	var replicaValidation *admissionregistrationv1.Validation
	for _, validation := range retainedRuntime.Spec.Validations {
		if strings.Contains(validation.Expression, "dyn(object).spec.replicas == (") {
			if replicaValidation != nil {
				t.Fatal("rendered runtime policy has multiple replica validations")
			}
			copy := validation
			replicaValidation = &copy
		}
	}
	if replicaValidation == nil {
		t.Fatal("rendered runtime policy lacks the exact replica validation")
	}
	replicaPolicy := retainedRuntime.DeepCopy()
	replicaPolicy.Spec.Validations = []admissionregistrationv1.Validation{*replicaValidation}

	for _, name := range []string{current.ControllerDeploymentName, current.CertificateDeploymentName} {
		t.Run(name, func(t *testing.T) {
			live := findTransitionRenderObject(t, objects, "Deployment", name).DeepCopy()
			defaultDrainingProbeDeployment(live.Object)
			live.SetUID("live-deployment-uid")
			live.SetResourceVersion("101")
			annotations := live.GetAnnotations()
			annotations[ControllerStateVersionAnnotation] = strconv.Itoa(int(current.ControllerStateVersion))
			annotations[ReleaseSequenceAnnotation] = "2"
			live.SetAnnotations(annotations)
			if err := unstructured.SetNestedField(live.Object, int64(0), "spec", "replicas"); err != nil {
				t.Fatal(err)
			}
			request := rolloutRequestCELObject(current, "UPDATE", "", rolloutHookUsername(current))
			request["name"] = name
			request["dryRun"] = true
			candidateRollout := current.policy(current.ControllerStateVersion, current.AdmissionContractVersion)
			policies := []*admissionregistrationv1.ValidatingAdmissionPolicy{
				candidateRollout,
				current.runtimePolicy(current.ControllerStateVersion, current.ReleaseSequence, current.ManagerImage),
				predecessor.policy(predecessor.ControllerStateVersion, predecessor.AdmissionContractVersion),
				&retainedRuntime,
			}
			for _, policy := range policies {
				if results := evaluatePolicyValidations(t, policy, live.Object, live.Object, request, params); slices.Contains(results, false) {
					t.Fatalf("unchanged stopped baseline was rejected by %s: %v", policy.Name, results)
				}
			}

			probe := live.DeepCopy()
			annotations = probe.GetAnnotations()
			annotations[guardEnforcementProbeAnnotation] = candidateRollout.Name
			probe.SetAnnotations(annotations)
			results := evaluatePolicyValidations(t, candidateRollout, probe.Object, live.Object, request, params)
			ordinary, sentinel := 0, 0
			for index, allowed := range results {
				if allowed {
					continue
				}
				switch candidateRollout.Spec.Validations[index].Message {
				case rolloutGuardDenialMessage(current.ReleaseSequence):
					ordinary++
				case rolloutGuardProbeDenialMessage(current.ReleaseSequence):
					sentinel++
				}
			}
			if ordinary != 1 || sentinel != 1 {
				t.Fatalf("stopped sentinel causes ordinary/exact denials = %d/%d, want 1/1", ordinary, sentinel)
			}

			normalized := live.DeepCopy()
			annotations = normalized.GetAnnotations()
			annotations[ControllerStateVersionAnnotation] = strconv.Itoa(int(predecessor.ControllerStateVersion))
			annotations[ReleaseSequenceAnnotation] = "1"
			normalized.SetAnnotations(annotations)
			if results := evaluatePolicyValidations(t, replicaPolicy, normalized.Object, live.Object, request, params); len(results) != 1 || results[0] {
				t.Fatalf("metadata-only normalization bypassed retained replica contract: %v", results)
			}
			wantReplicas := int64(1)
			if name == predecessor.ControllerDeploymentName {
				wantReplicas = int64(predecessor.ControllerReplicas)
			}
			if err := unstructured.SetNestedField(normalized.Object, wantReplicas, "spec", "replicas"); err != nil {
				t.Fatal(err)
			}
			if results := evaluatePolicyValidations(t, replicaPolicy, normalized.Object, live.Object, request, params); len(results) != 1 || !results[0] {
				t.Fatalf("original desired replicas did not satisfy the isolated retained replica contract: %v", results)
			}
			for _, policy := range policies {
				if results := evaluatePolicyValidations(t, policy, normalized.Object, live.Object, request, params); slices.Contains(results, false) {
					t.Fatalf("active-identity dry-run baseline was rejected by full %s: %v", policy.Name, results)
				}
			}
		})
	}
}

// Helm omits these API defaults. Apply them before taking the simulated live
// snapshot, never as a mutation made by the enforcement probe itself.
func defaultDrainingProbeDeployment(object map[string]any) {
	spec := object["spec"].(map[string]any)
	setDefault := func(object map[string]any, key string, value any) {
		if _, found := object[key]; !found {
			object[key] = value
		}
	}
	setDefault(spec, "revisionHistoryLimit", int64(10))
	setDefault(spec, "progressDeadlineSeconds", int64(600))
	pod := spec["template"].(map[string]any)["spec"].(map[string]any)
	setDefault(pod, "restartPolicy", "Always")
	setDefault(pod, "dnsPolicy", "ClusterFirst")
	setDefault(pod, "schedulerName", "default-scheduler")
	setDefault(pod, "terminationGracePeriodSeconds", int64(30))
	for _, field := range []string{"containers", "initContainers"} {
		for _, value := range pod[field].([]any) {
			container := value.(map[string]any)
			setDefault(container, "terminationMessagePath", "/dev/termination-log")
			setDefault(container, "terminationMessagePolicy", "File")
			if ports, ok := container["ports"].([]any); ok {
				for _, port := range ports {
					setDefault(port.(map[string]any), "protocol", "TCP")
				}
			}
		}
	}
}
