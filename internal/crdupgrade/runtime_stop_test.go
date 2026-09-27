package crdupgrade_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

const (
	stopReleaseName   = "ptah"
	stopNamespace     = "ptah-system"
	stopController    = "ptah-ptah-operator"
	stopCertificate   = "ptah-ptah-operator-cert-rotator"
	stopReleaseImage  = "registry.example/ptah@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	stopPreviousImage = "registry.example/ptah@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// stopDeployments is the release's two Deployments as the API server holds
// them, and a record of every update the stop makes.
type stopDeployments struct {
	mu      sync.Mutex
	objects map[string]*appsv1.Deployment
	updates []string
}

func (d *stopDeployments) Get(_ context.Context, name string, _ metav1.GetOptions) (*appsv1.Deployment, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	object, found := d.objects[name]
	if !found {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "apps", Resource: "deployments"}, name)
	}
	return object.DeepCopy(), nil
}

func (d *stopDeployments) Update(_ context.Context, deployment *appsv1.Deployment, _ metav1.UpdateOptions) (*appsv1.Deployment, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.objects[deployment.Name] = deployment.DeepCopy()
	d.updates = append(d.updates, deployment.Name)
	return deployment.DeepCopy(), nil
}

func (d *stopDeployments) replicas(name string) int32 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return *d.objects[name].Spec.Replicas
}

// stopPods answers a list with the Pods a Deployment still runs: each
// Deployment's Pods stay for polls lists after it is scaled to zero, or for
// good when polls is negative.
type stopPods struct {
	mu          sync.Mutex
	deployments *stopDeployments
	polls       int
	seen        map[string]int
	selectors   []string
}

func (p *stopPods) List(_ context.Context, options metav1.ListOptions) (*corev1.PodList, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.selectors = append(p.selectors, options.LabelSelector)
	component := ""
	for _, part := range strings.Split(options.LabelSelector, ",") {
		if value, found := strings.CutPrefix(part, "app.kubernetes.io/component="); found {
			component = value
		}
	}
	name := stopController
	if component == "certificate-rotation" {
		name = stopCertificate
	}
	if p.deployments.replicas(name) != 0 {
		return &corev1.PodList{Items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: name + "-running"}}}}, nil
	}
	if p.seen == nil {
		p.seen = map[string]int{}
	}
	p.seen[name]++
	if p.polls < 0 || p.seen[name] <= p.polls {
		return &corev1.PodList{Items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: name + "-terminating"}}}}, nil
	}
	return &corev1.PodList{}, nil
}

func stopDeployment(name, component, image string) *appsv1.Deployment {
	replicas := int32(1)
	selector := map[string]string{
		"app.kubernetes.io/name":      "ptah-operator",
		"app.kubernetes.io/instance":  stopReleaseName,
		"app.kubernetes.io/component": component,
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: stopNamespace,
			Annotations: map[string]string{
				"meta.helm.sh/release-name":      stopReleaseName,
				"meta.helm.sh/release-namespace": stopNamespace,
			},
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "Helm",
				"app.kubernetes.io/instance":   stopReleaseName,
				"app.kubernetes.io/component":  component,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: selector},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "verify-candidate-runtime", Image: image}},
					Containers:     []corev1.Container{{Name: "manager", Image: image}},
				},
			},
		},
	}
}

func newRuntimeStop(deployments ...*appsv1.Deployment) (*crdupgrade.RuntimeStop, *stopDeployments, *stopPods) {
	store := &stopDeployments{objects: map[string]*appsv1.Deployment{}}
	for _, deployment := range deployments {
		store.objects[deployment.Name] = deployment
	}
	pods := &stopPods{deployments: store, polls: 2}
	return &crdupgrade.RuntimeStop{
		Deployments:               store,
		Pods:                      pods,
		ReleaseName:               stopReleaseName,
		ReleaseNamespace:          stopNamespace,
		ControllerDeploymentName:  stopController,
		CertificateDeploymentName: stopCertificate,
		ManagerImage:              stopReleaseImage,
		PollEvery:                 time.Millisecond,
	}, store, pods
}

func TestRuntimeStopLeavesTheReleaseImageRunning(t *testing.T) {
	t.Parallel()

	stop, store, pods := newRuntimeStop(
		stopDeployment(stopController, "controller", stopReleaseImage),
		stopDeployment(stopCertificate, "certificate-rotation", stopReleaseImage),
	)
	if err := stop.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want the runtime left running", err)
	}
	if len(store.updates) != 0 || len(pods.selectors) != 0 {
		t.Fatalf("Run() updated %v and listed Pods %v, want a runtime on the release image left alone", store.updates, pods.selectors)
	}
	if store.replicas(stopController) != 1 || store.replicas(stopCertificate) != 1 {
		t.Fatal("Run() changed the replica count of a runtime on the release image")
	}
}

func TestRuntimeStopStopsTheRuntimeWhenTheImageChanges(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name        string
		controller  string
		certificate string
		stale       func(*appsv1.Deployment)
	}{
		{name: "both Deployments on the previous image", controller: stopPreviousImage, certificate: stopPreviousImage},
		{name: "only the rotator on the previous image", controller: stopReleaseImage, certificate: stopPreviousImage},
		{name: "only the manager on the previous image", controller: stopPreviousImage, certificate: stopReleaseImage},
		{
			name: "the verifier init container on the previous image", controller: stopReleaseImage, certificate: stopReleaseImage,
			stale: func(deployment *appsv1.Deployment) {
				deployment.Spec.Template.Spec.InitContainers[0].Image = stopPreviousImage
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			controller := stopDeployment(stopController, "controller", test.controller)
			if test.stale != nil {
				test.stale(controller)
			}
			stop, store, pods := newRuntimeStop(
				controller,
				stopDeployment(stopCertificate, "certificate-rotation", test.certificate),
			)
			if err := stop.Run(context.Background()); err != nil {
				t.Fatalf("Run() = %v, want the runtime stopped", err)
			}
			if got, want := strings.Join(store.updates, ","), stopCertificate+","+stopController; got != want {
				t.Fatalf("Run() updated %q, want the rotator and then the manager: %q", got, want)
			}
			if store.replicas(stopController) != 0 || store.replicas(stopCertificate) != 0 {
				t.Fatal("Run() left a runtime Deployment scaled up")
			}
			// Each Deployment's Pods stay for two lists after the scale-down, so
			// a stop that returned without waiting for them would list fewer.
			if pods.seen[stopController] != 3 || pods.seen[stopCertificate] != 3 {
				t.Fatalf("Run() listed the stopped Pods %v times, want it to wait until none was left", pods.seen)
			}
		})
	}
}

func TestRuntimeStopIsRepeatable(t *testing.T) {
	t.Parallel()

	controller := stopDeployment(stopController, "controller", stopPreviousImage)
	certificate := stopDeployment(stopCertificate, "certificate-rotation", stopPreviousImage)
	zero := int32(0)
	controller.Spec.Replicas = &zero
	certificate.Spec.Replicas = &zero
	stop, store, pods := newRuntimeStop(controller, certificate)
	pods.polls = 0
	if err := stop.Run(context.Background()); err != nil {
		t.Fatalf("Run() over an already stopped runtime = %v", err)
	}
	if len(store.updates) != 0 {
		t.Fatalf("Run() updated %v, want an already stopped runtime left as it is", store.updates)
	}
}

func TestRuntimeStopHasNothingToStopOnAFreshInstall(t *testing.T) {
	t.Parallel()

	stop, store, pods := newRuntimeStop()
	if err := stop.Run(context.Background()); err != nil {
		t.Fatalf("Run() without Deployments = %v", err)
	}
	if len(store.updates) != 0 || len(pods.selectors) != 0 {
		t.Fatalf("Run() without Deployments updated %v and listed %v", store.updates, pods.selectors)
	}
}

func TestRuntimeStopRefusesADeploymentAnotherReleaseOwns(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		mutate func(*appsv1.Deployment)
	}{
		{name: "another release name", mutate: func(deployment *appsv1.Deployment) {
			deployment.Annotations["meta.helm.sh/release-name"] = "other"
		}},
		{name: "another instance", mutate: func(deployment *appsv1.Deployment) {
			deployment.Labels["app.kubernetes.io/instance"] = "other"
		}},
		{name: "not managed by Helm", mutate: func(deployment *appsv1.Deployment) {
			delete(deployment.Labels, "app.kubernetes.io/managed-by")
		}},
		{name: "another component", mutate: func(deployment *appsv1.Deployment) {
			deployment.Labels["app.kubernetes.io/component"] = "certificate-rotation"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			controller := stopDeployment(stopController, "controller", stopPreviousImage)
			test.mutate(controller)
			stop, store, _ := newRuntimeStop(
				controller,
				stopDeployment(stopCertificate, "certificate-rotation", stopPreviousImage),
			)
			err := stop.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "is not owned by Helm release ptah-system/ptah") {
				t.Fatalf("Run() = %v, want the ownership refusal", err)
			}
			if len(store.updates) != 0 {
				t.Fatalf("Run() updated %v before refusing", store.updates)
			}
		})
	}
}

func TestRuntimeStopNamesThePodsThatOutliveTheDeadline(t *testing.T) {
	t.Parallel()

	stop, _, pods := newRuntimeStop(
		stopDeployment(stopController, "controller", stopPreviousImage),
		stopDeployment(stopCertificate, "certificate-rotation", stopPreviousImage),
	)
	pods.polls = -1
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := stop.Run(ctx)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) ||
		!strings.Contains(err.Error(), stopCertificate+"-terminating") {
		t.Fatalf("Run() = %v, want the deadline naming the Pod still there", err)
	}
}
