package crdupgrade

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// Run stops every runtime Deployment of the release when any of them runs an
// image other than ManagerImage, and returns once none of their Pods is left.
// A Deployment that does not exist has nothing to stop. Running it again
// changes nothing: a stopped Deployment stays at zero, and the wait finds no
// Pod.
func (s *RuntimeStop) Run(ctx context.Context) error {
	if err := s.validate(); err != nil {
		return err
	}
	existing := make([]*appsv1.Deployment, 0, 2)
	stale := false
	for _, target := range s.deployments() {
		deployment, err := s.Deployments.Get(ctx, target.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("get %s Deployment %s/%s: %w", target.component, s.ReleaseNamespace, target.name, err)
		}
		if err := s.verifyOwnership(target, deployment); err != nil {
			return err
		}
		if !runsImage(deployment, s.ManagerImage) {
			stale = true
		}
		existing = append(existing, deployment)
	}
	if !stale {
		return nil
	}
	for _, deployment := range existing {
		if err := s.stop(ctx, deployment); err != nil {
			return err
		}
	}
	return nil
}

func (s *RuntimeStop) stop(ctx context.Context, deployment *appsv1.Deployment) error {
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return fmt.Errorf("runtime Deployment %s/%s has an invalid selector: %w", s.ReleaseNamespace, deployment.Name, err)
	}
	if selector.Empty() {
		return fmt.Errorf("runtime Deployment %s/%s selects every Pod in the namespace", s.ReleaseNamespace, deployment.Name)
	}
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
	var remaining []string
	err = wait.PollUntilContextCancel(ctx, s.PollEvery, true, func(pollCtx context.Context) (bool, error) {
		pods, err := s.Pods.List(pollCtx, metav1.ListOptions{LabelSelector: selector.String()})
		if err != nil {
			return false, fmt.Errorf("list the Pods of Deployment %s/%s: %w", s.ReleaseNamespace, deployment.Name, err)
		}
		remaining = remaining[:0]
		for index := range pods.Items {
			remaining = append(remaining, pods.Items[index].Name)
		}
		return len(remaining) == 0, nil
	})
	if err != nil && len(remaining) != 0 {
		return fmt.Errorf("runtime Deployment %s/%s is scaled to zero and its Pods %v are still there: %w", s.ReleaseNamespace, deployment.Name, remaining, err)
	}
	return err
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

// runsImage reports whether every container of the Deployment's Pods runs
// image.
func runsImage(deployment *appsv1.Deployment, image string) bool {
	spec := deployment.Spec.Template.Spec
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
