package crdupgrade_test

import (
	"context"
	"errors"
	"fmt"
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

var stopComponents = map[string]string{
	stopController:  "controller",
	stopCertificate: "certificate-rotation",
}

// stopCluster is the release namespace as the stop sees it. It moves only when
// it is read, the way the Deployment controller and the kubelet move while the
// stop polls: a Deployment observes a scale-down some reads after it, its
// status drops its replicas some reads after that, and the Pods go some lists
// later. What the stop saw last is therefore what the cluster holds when it
// returns, and a test reads that to know what it waited for.
type stopCluster struct {
	mu          sync.Mutex
	deployments map[string]*appsv1.Deployment
	pods        map[string][]corev1.Pod
	updates     []string
	selectors   []string

	// observeAfter is how many reads a Deployment takes to observe an update.
	observeAfter int
	// replicasAfter is how many further reads its status keeps the old count.
	replicasAfter int
	// podsLinger is how many lists a stopped Deployment's Pods outlive its
	// status reporting zero; negative keeps them for good. The Pods of a
	// Deployment that does not exist are never removed.
	podsLinger int

	reads   map[string]int
	lingers map[string]int
}

func newStopCluster(deployments ...*appsv1.Deployment) *stopCluster {
	cluster := &stopCluster{
		deployments: map[string]*appsv1.Deployment{},
		pods:        map[string][]corev1.Pod{},
		reads:       map[string]int{},
		lingers:     map[string]int{},
		podsLinger:  2,
	}
	for _, deployment := range deployments {
		cluster.deployments[deployment.Name] = deployment
		component := stopComponents[deployment.Name]
		for index := int32(0); index < *deployment.Spec.Replicas; index++ {
			cluster.pods[component] = append(cluster.pods[component], stopPod(deployment.Name, index, deployment.Spec.Template.Spec))
		}
	}
	return cluster
}

func stopPod(owner string, index int32, spec corev1.PodSpec) corev1.Pod {
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", owner, index)}, Spec: *spec.DeepCopy()}
}

func (c *stopCluster) Get(_ context.Context, name string, _ metav1.GetOptions) (*appsv1.Deployment, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	object, found := c.deployments[name]
	if !found {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "apps", Resource: "deployments"}, name)
	}
	if object.Status.ObservedGeneration < object.Generation {
		c.reads[name]++
		if c.reads[name] > c.observeAfter {
			object.Status.ObservedGeneration = object.Generation
			c.reads[name] = 0
		}
	} else if object.Status.Replicas != *object.Spec.Replicas {
		c.reads[name]++
		if c.reads[name] > c.replicasAfter {
			object.Status.Replicas = *object.Spec.Replicas
		}
	}
	return object.DeepCopy(), nil
}

func (c *stopCluster) Update(_ context.Context, deployment *appsv1.Deployment, _ metav1.UpdateOptions) (*appsv1.Deployment, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	stored := c.deployments[deployment.Name]
	updated := deployment.DeepCopy()
	updated.Generation = stored.Generation + 1
	updated.Status = stored.Status
	c.deployments[deployment.Name] = updated
	c.reads[deployment.Name] = 0
	c.updates = append(c.updates, deployment.Name)
	return updated.DeepCopy(), nil
}

// List answers only the selector the chart's runtime labels make: the
// release's instance and one component.
func (c *stopCluster) List(_ context.Context, options metav1.ListOptions) (*corev1.PodList, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.selectors = append(c.selectors, options.LabelSelector)
	component := ""
	instance := false
	for _, part := range strings.Split(options.LabelSelector, ",") {
		if value, found := strings.CutPrefix(part, "app.kubernetes.io/component="); found {
			component = value
		}
		if part == "app.kubernetes.io/instance="+stopReleaseName {
			instance = true
		}
	}
	if !instance || component == "" {
		return nil, fmt.Errorf("selector %q does not name the release's runtime", options.LabelSelector)
	}
	name := stopController
	if component == "certificate-rotation" {
		name = stopCertificate
	}
	if deployment, found := c.deployments[name]; found && *deployment.Spec.Replicas == 0 &&
		deployment.Status.ObservedGeneration >= deployment.Generation && deployment.Status.Replicas == 0 {
		c.lingers[component]++
		if c.podsLinger >= 0 && c.lingers[component] > c.podsLinger {
			c.pods[component] = nil
		}
	}
	return &corev1.PodList{Items: append([]corev1.Pod(nil), c.pods[component]...)}, nil
}

func (c *stopCluster) replicas(name string) int32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return *c.deployments[name].Spec.Replicas
}

// requireStopped holds the cluster, as the stop last read it, to a stopped
// runtime: every Deployment observed its scale-down and reports no replica,
// and no runtime Pod is left.
func (c *stopCluster) requireStopped(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for name, deployment := range c.deployments {
		if *deployment.Spec.Replicas != 0 || deployment.Status.ObservedGeneration < deployment.Generation ||
			deployment.Status.Replicas != 0 {
			t.Fatalf("Run() returned while Deployment %s had replicas %d, generation %d observed %d, status replicas %d",
				name, *deployment.Spec.Replicas, deployment.Generation, deployment.Status.ObservedGeneration, deployment.Status.Replicas)
		}
	}
	for component, pods := range c.pods {
		if len(pods) != 0 {
			t.Fatalf("Run() returned while %s Pods %s were still there", component, pods[0].Name)
		}
	}
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
			Name:       name,
			Namespace:  stopNamespace,
			Generation: 1,
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
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: replicas},
	}
}

func newRuntimeStop(cluster *stopCluster) *crdupgrade.RuntimeStop {
	return &crdupgrade.RuntimeStop{
		Deployments:               cluster,
		Pods:                      cluster,
		ReleaseName:               stopReleaseName,
		ReleaseNamespace:          stopNamespace,
		ControllerDeploymentName:  stopController,
		CertificateDeploymentName: stopCertificate,
		ManagerImage:              stopReleaseImage,
		PollEvery:                 time.Millisecond,
	}
}

func TestRuntimeStopLeavesTheReleaseImageRunning(t *testing.T) {
	t.Parallel()

	cluster := newStopCluster(
		stopDeployment(stopController, "controller", stopReleaseImage),
		stopDeployment(stopCertificate, "certificate-rotation", stopReleaseImage),
	)
	if err := newRuntimeStop(cluster).Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want the runtime left running", err)
	}
	if len(cluster.updates) != 0 {
		t.Fatalf("Run() updated %v, want a runtime on the release image left alone", cluster.updates)
	}
	if cluster.replicas(stopController) != 1 || cluster.replicas(stopCertificate) != 1 {
		t.Fatal("Run() changed the replica count of a runtime on the release image")
	}
	if len(cluster.pods["controller"]) != 1 || len(cluster.pods["certificate-rotation"]) != 1 {
		t.Fatal("Run() removed a runtime Pod on the release image")
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
			cluster := newStopCluster(controller, stopDeployment(stopCertificate, "certificate-rotation", test.certificate))
			cluster.observeAfter, cluster.replicasAfter = 2, 2
			if err := newRuntimeStop(cluster).Run(context.Background()); err != nil {
				t.Fatalf("Run() = %v, want the runtime stopped", err)
			}
			if got, want := strings.Join(cluster.updates, ","), stopCertificate+","+stopController; got != want {
				t.Fatalf("Run() updated %q, want the rotator and then the manager: %q", got, want)
			}
			cluster.requireStopped(t)
		})
	}
}

// The status a stop reads can lag its own write, and neither half of it proves
// the stop alone: a Deployment whose Pods are already gone still has to report
// that it observed the scale-down, and one that observed it still has to stop
// counting a replica the Pod list no longer shows.
func TestRuntimeStopWaitsForTheDeploymentToReportTheStop(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name           string
		statusReplicas int32
		observeAfter   int
		replicasAfter  int
	}{
		// The status already counts no replica, so only the observed
		// generation can hold the stop.
		{name: "the scale-down not yet observed", statusReplicas: 0, observeAfter: 5, replicasAfter: 1000},
		{name: "a replica still counted after it was observed", statusReplicas: 1, replicasAfter: 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			controller := stopDeployment(stopController, "controller", stopPreviousImage)
			certificate := stopDeployment(stopCertificate, "certificate-rotation", stopReleaseImage)
			controller.Status.Replicas, certificate.Status.Replicas = test.statusReplicas, test.statusReplicas
			cluster := newStopCluster(controller, certificate)
			// No Pod is left to wait for, so only the Deployment's own report can
			// hold the stop.
			cluster.pods = map[string][]corev1.Pod{}
			cluster.observeAfter, cluster.replicasAfter = test.observeAfter, test.replicasAfter
			if err := newRuntimeStop(cluster).Run(context.Background()); err != nil {
				t.Fatalf("Run() = %v, want the runtime stopped", err)
			}
			cluster.requireStopped(t)
		})
	}
}

// A Deployment deleted with orphaned dependents leaves its Pods serving. They
// carry the release's runtime labels, so the stop finds them without the
// Deployment, and it cannot scale what has no Deployment: it refuses rather
// than update a CRD under them.
func TestRuntimeStopRefusesOrphanedPodsOfTheReleaseBefore(t *testing.T) {
	t.Parallel()

	orphan := stopDeployment(stopController, "controller", stopPreviousImage)
	cluster := newStopCluster(orphan, stopDeployment(stopCertificate, "certificate-rotation", stopReleaseImage))
	delete(cluster.deployments, stopController)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := newRuntimeStop(cluster).Run(ctx)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), stopController+"-0") {
		t.Fatalf("Run() = %v, want the deadline naming the orphaned Pod", err)
	}
	if got := strings.Join(cluster.updates, ","); got != stopCertificate {
		t.Fatalf("Run() updated %q, want only the rotator's Deployment scaled", got)
	}
}

func TestRuntimeStopLeavesOrphanedPodsOnTheReleaseImage(t *testing.T) {
	t.Parallel()

	cluster := newStopCluster(
		stopDeployment(stopController, "controller", stopReleaseImage),
		stopDeployment(stopCertificate, "certificate-rotation", stopReleaseImage),
	)
	delete(cluster.deployments, stopController)
	if err := newRuntimeStop(cluster).Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want Pods on the release image left alone", err)
	}
	if len(cluster.updates) != 0 {
		t.Fatalf("Run() updated %v, want nothing stopped", cluster.updates)
	}
}

func TestRuntimeStopIsRepeatable(t *testing.T) {
	t.Parallel()

	controller := stopDeployment(stopController, "controller", stopPreviousImage)
	certificate := stopDeployment(stopCertificate, "certificate-rotation", stopPreviousImage)
	zero := int32(0)
	for _, deployment := range []*appsv1.Deployment{controller, certificate} {
		deployment.Spec.Replicas = &zero
		deployment.Status.Replicas = 0
	}
	cluster := newStopCluster(controller, certificate)
	if err := newRuntimeStop(cluster).Run(context.Background()); err != nil {
		t.Fatalf("Run() over an already stopped runtime = %v", err)
	}
	if len(cluster.updates) != 0 {
		t.Fatalf("Run() updated %v, want an already stopped runtime left as it is", cluster.updates)
	}
	cluster.requireStopped(t)
}

func TestRuntimeStopHasNothingToStopOnAFreshInstall(t *testing.T) {
	t.Parallel()

	cluster := newStopCluster()
	if err := newRuntimeStop(cluster).Run(context.Background()); err != nil {
		t.Fatalf("Run() without Deployments = %v", err)
	}
	if len(cluster.updates) != 0 {
		t.Fatalf("Run() without Deployments updated %v", cluster.updates)
	}
	// The Pods are looked for all the same, by the release's runtime labels.
	want := "app.kubernetes.io/component=certificate-rotation,app.kubernetes.io/instance=ptah," +
		"app.kubernetes.io/component=controller,app.kubernetes.io/instance=ptah"
	if got := strings.Join(cluster.selectors, ","); got != want {
		t.Fatalf("Run() listed Pods by %q, want %q", got, want)
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
			cluster := newStopCluster(controller, stopDeployment(stopCertificate, "certificate-rotation", stopPreviousImage))
			err := newRuntimeStop(cluster).Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "is not owned by Helm release ptah-system/ptah") {
				t.Fatalf("Run() = %v, want the ownership refusal", err)
			}
			if len(cluster.updates) != 0 {
				t.Fatalf("Run() updated %v before refusing", cluster.updates)
			}
		})
	}
}

func TestRuntimeStopNamesThePodsThatOutliveTheDeadline(t *testing.T) {
	t.Parallel()

	cluster := newStopCluster(
		stopDeployment(stopController, "controller", stopPreviousImage),
		stopDeployment(stopCertificate, "certificate-rotation", stopPreviousImage),
	)
	cluster.podsLinger = -1
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := newRuntimeStop(cluster).Run(ctx)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) ||
		!strings.Contains(err.Error(), stopCertificate+"-0") {
		t.Fatalf("Run() = %v, want the deadline naming the Pod still there", err)
	}
}
