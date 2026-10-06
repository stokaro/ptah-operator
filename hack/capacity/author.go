package main

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func capacityAuthor(ctx context.Context, path string, installer *rest.Config) (dynamic.Interface, *rest.Config, string, error) {
	if path == "" {
		return nil, installer, "", nil
	}
	config, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, nil, "", fmt.Errorf("read author kubeconfig: %w", err)
	}
	if config.Host != installer.Host {
		return nil, nil, "", fmt.Errorf("author kubeconfig must name the workload API endpoint")
	}
	config.QPS, config.Burst = installer.QPS, installer.Burst
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	installationUser, err := capacityUsername(ctx, installer)
	if err != nil {
		return nil, nil, "", fmt.Errorf("identify installer: %w", err)
	}
	author, err := capacityUsername(ctx, config)
	if err != nil {
		return nil, nil, "", fmt.Errorf("identify author: %w", err)
	}
	if author == installationUser {
		return nil, nil, "", fmt.Errorf("author must differ from installer %q", author)
	}
	writer, err := dynamic.NewForConfig(config)
	return writer, config, installationUser, err
}

func (s *scenarios) workloadWriter() dynamic.Interface {
	if s.author != nil {
		return s.author
	}
	return s.dynamic
}

// The author owns desired state. Only the installer can select Always. Keep
// the resource suspended throughout that handoff so preparation cannot execute
// an intermediate input or create an unwanted approval gate.
func (s *scenarios) createWorkload(ctx context.Context, resource schema.GroupVersionResource, desired *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	writer := s.workloadWriter().Resource(resource).Namespace(desired.GetNamespace())
	apply, _, err := unstructured.NestedString(desired.Object, "spec", "policy", "apply")
	if err != nil {
		return nil, err
	}
	if s.author == nil || apply != "Always" {
		return writer.Create(ctx, desired, metav1.CreateOptions{})
	}
	paused := desired.DeepCopy()
	if err := unstructured.SetNestedField(paused.Object, "OnApproval", "spec", "policy", "apply"); err != nil {
		return nil, err
	}
	if err := unstructured.SetNestedField(paused.Object, true, "spec", "suspend"); err != nil {
		return nil, err
	}
	created, err := writer.Create(ctx, paused, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	granted, err := patchCapacitySpec(ctx, s.dynamic, resource, created, map[string]any{"policy": map[string]any{"apply": "Always"}})
	if err != nil {
		return nil, fmt.Errorf("installer selects Always: %w", err)
	}
	suspended, _, err := unstructured.NestedBool(desired.Object, "spec", "suspend")
	if err != nil || suspended {
		return granted, err
	}
	return patchCapacitySpec(ctx, s.workloadWriter(), resource, granted, map[string]any{"suspend": false})
}
