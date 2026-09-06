package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// PodLogReader isolates the pod/log subresource from the reconciliation logic.
type PodLogReader interface {
	Read(ctx context.Context, namespace, podName, containerName string) ([]byte, error)
}

// ClientsetPodLogs reads exact container logs through the Kubernetes API.
type ClientsetPodLogs struct {
	Client kubernetes.Interface
}

func (r ClientsetPodLogs) Read(ctx context.Context, namespace, podName, containerName string) ([]byte, error) {
	if r.Client == nil {
		return nil, fmt.Errorf("Kubernetes clientset is required")
	}
	logs, err := r.Client.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{Container: containerName}).DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("read executor logs: %w", err)
	}
	return logs, nil
}
