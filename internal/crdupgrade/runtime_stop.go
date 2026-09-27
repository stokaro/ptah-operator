package crdupgrade

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
)

// DeploymentClient is the namespaced API surface RuntimeStop needs to read a
// runtime Deployment and scale it to zero.
type DeploymentClient interface {
	Get(context.Context, string, metav1.GetOptions) (*appsv1.Deployment, error)
	Update(context.Context, *appsv1.Deployment, metav1.UpdateOptions) (*appsv1.Deployment, error)
}

// PodLister lists the Pods a runtime Deployment selects.
type PodLister interface {
	List(context.Context, metav1.ListOptions) (*corev1.PodList, error)
}

// RuntimeStop stops the release's running manager and certificate rotator
// before the CRD manager changes a CRD they serve.
//
// The approval webhooks answer with a patch computed from the object the
// manager decoded, so a manager older than the schema drops every field it
// does not know from the objects it mutates. Stopping the runtime before the
// first CRD update keeps an old binary from serving a new schema. Helm scales
// the Deployments back up when it applies this release's manifests.
//
// A runtime that already runs the release's manager image is left running:
// its CRDs cannot change, because the CRDs are compiled into the image.
type RuntimeStop struct {
	Deployments               DeploymentClient
	Pods                      PodLister
	ReleaseName               string
	ReleaseNamespace          string
	ControllerDeploymentName  string
	CertificateDeploymentName string
	ManagerImage              string
	PollEvery                 time.Duration
}

type runtimeDeployment struct {
	name      string
	component string
}

func (s *RuntimeStop) deployments() []runtimeDeployment {
	return []runtimeDeployment{
		{name: s.CertificateDeploymentName, component: "certificate-rotation"},
		{name: s.ControllerDeploymentName, component: "controller"},
	}
}

// Run stops every runtime Deployment of the release when any of them, or any
// Pod carrying the release's runtime labels, runs an image other than
// ManagerImage, and returns once each Deployment has observed its scale-down
// and no such Pod is left. A Deployment that does not exist has nothing to
// scale, but its Pods are waited for all the same: a Deployment deleted with
// orphaned dependents leaves Pods that still serve. Running it again changes
// nothing: a stopped Deployment stays at zero, and the wait finds no Pod.
func (s *RuntimeStop) Run(ctx context.Context) error {
	if err := s.validate(); err != nil {
		return err
	}
	existing := make(map[string]*appsv1.Deployment, 2)
	stale := false
	for _, target := range s.deployments() {
		deployment, err := s.Deployments.Get(ctx, target.name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			return fmt.Errorf("get %s Deployment %s/%s: %w", target.component, s.ReleaseNamespace, target.name, err)
		default:
			if err := s.verifyOwnership(target, deployment); err != nil {
				return err
			}
			if !runsImage(deployment.Spec.Template.Spec, s.ManagerImage) {
				stale = true
			}
			existing[target.name] = deployment
		}
		pods, err := s.runtimePods(ctx, target)
		if err != nil {
			return err
		}
		for index := range pods {
			if !runsImage(pods[index].Spec, s.ManagerImage) {
				stale = true
			}
		}
	}
	if !stale {
		return nil
	}
	for _, target := range s.deployments() {
		if deployment, found := existing[target.name]; found {
			if err := s.scaleToZero(ctx, deployment); err != nil {
				return err
			}
		}
		if err := s.waitStopped(ctx, target); err != nil {
			return err
		}
	}
	return nil
}

func (s *RuntimeStop) scaleToZero(ctx context.Context, deployment *appsv1.Deployment) error {
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := s.Deployments.Get(ctx, deployment.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if current.Spec.Replicas != nil && *current.Spec.Replicas == 0 {
			return nil
		}
		stopped := current.DeepCopy()
		zero := int32(0)
		stopped.Spec.Replicas = &zero
		_, err = s.Deployments.Update(ctx, stopped, metav1.UpdateOptions{})
		return err
	}); err != nil {
		return fmt.Errorf("scale Deployment %s/%s to zero: %w", s.ReleaseNamespace, deployment.Name, err)
	}
	return nil
}

// waitStopped returns once the Deployment, if there is one, reports that it
// observed its scale-down and runs no replica, and no Pod carries its runtime
// labels. A Pod is counted until it is gone, terminating or not.
func (s *RuntimeStop) waitStopped(ctx context.Context, target runtimeDeployment) error {
	pending := ""
	err := wait.PollUntilContextCancel(ctx, s.PollEvery, true, func(pollCtx context.Context) (bool, error) {
		var err error
		pending, err = s.stopPending(pollCtx, target)
		return pending == "", err
	})
	if err != nil && pending != "" {
		return fmt.Errorf("runtime Deployment %s/%s did not stop: %s: %w", s.ReleaseNamespace, target.name, pending, err)
	}
	return err
}

// stopPending says what still keeps the target from being stopped, or nothing.
func (s *RuntimeStop) stopPending(ctx context.Context, target runtimeDeployment) (string, error) {
	deployment, err := s.Deployments.Get(ctx, target.name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return "", fmt.Errorf("get %s Deployment %s/%s: %w", target.component, s.ReleaseNamespace, target.name, err)
	case deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0:
		return "spec.replicas is no longer zero", nil
	case deployment.Status.ObservedGeneration < deployment.Generation:
		return fmt.Sprintf("its status has not observed generation %d", deployment.Generation), nil
	case deployment.Status.Replicas != 0:
		return fmt.Sprintf("its status reports %d replicas", deployment.Status.Replicas), nil
	}
	pods, err := s.runtimePods(ctx, target)
	if err != nil {
		return "", err
	}
	if len(pods) != 0 {
		names := make([]string, 0, len(pods))
		for index := range pods {
			names = append(names, pods[index].Name)
		}
		return fmt.Sprintf("its Pods %v are still there", names), nil
	}
	return "", nil
}

// runtimePods lists the Pods that carry the release's runtime labels for the
// target, whether or not its Deployment exists.
func (s *RuntimeStop) runtimePods(ctx context.Context, target runtimeDeployment) ([]corev1.Pod, error) {
	selector := labels.SelectorFromSet(labels.Set{
		instanceLabel:                 s.ReleaseName,
		"app.kubernetes.io/component": target.component,
	})
	pods, err := s.Pods.List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, fmt.Errorf("list the %s Pods of release %s/%s: %w", target.component, s.ReleaseNamespace, s.ReleaseName, err)
	}
	return pods.Items, nil
}

// verifyOwnership refuses to stop a Deployment this release did not install.
func (s *RuntimeStop) verifyOwnership(target runtimeDeployment, deployment *appsv1.Deployment) error {
	if deployment.Annotations[helmReleaseNameAnnotation] != s.ReleaseName ||
		deployment.Annotations[helmReleaseNamespaceAnnotation] != s.ReleaseNamespace ||
		deployment.Labels[managedByLabel] != "Helm" ||
		deployment.Labels[instanceLabel] != s.ReleaseName ||
		deployment.Labels["app.kubernetes.io/component"] != target.component {
		return fmt.Errorf("%s Deployment %s/%s is not owned by Helm release %s/%s", target.component, s.ReleaseNamespace, target.name, s.ReleaseNamespace, s.ReleaseName)
	}
	return nil
}

// runsImage reports whether every container and init container of the Pod
// spec runs image.
func runsImage(spec corev1.PodSpec, image string) bool {
	if len(spec.Containers) == 0 {
		return false
	}
	for _, containers := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
		for index := range containers {
			if containers[index].Image != image {
				return false
			}
		}
	}
	return true
}

func (s *RuntimeStop) validate() error {
	if s == nil || s.Deployments == nil || s.Pods == nil {
		return fmt.Errorf("runtime stop clients are required")
	}
	for name, value := range map[string]string{
		"release name":                s.ReleaseName,
		"release namespace":           s.ReleaseNamespace,
		"controller Deployment name":  s.ControllerDeploymentName,
		"certificate Deployment name": s.CertificateDeploymentName,
		"manager image":               s.ManagerImage,
	} {
		if value == "" {
			return fmt.Errorf("runtime stop %s is required", name)
		}
	}
	if s.ControllerDeploymentName == s.CertificateDeploymentName {
		return fmt.Errorf("controller and certificate Deployment names must differ")
	}
	if s.PollEvery <= 0 {
		return fmt.Errorf("runtime stop poll interval must be positive")
	}
	return nil
}
