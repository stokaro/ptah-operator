package main

import (
	"context"
	"fmt"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type approvalActors struct {
	WorkloadWriter string `json:"workloadWriter"`
	Approver       string `json:"approver"`
}

// Measurements and maintenance keep the original client. Only approval CREATE
// uses the separate credentials, so qualification can enforce distinct writers
// without granting the approver the harness's installation privileges.
func capacityApprover(ctx context.Context, path string, workload *rest.Config, fallback dynamic.Interface) (dynamic.Interface, *approvalActors, error) {
	if path == "" {
		return fallback, nil, nil
	}
	config, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, nil, fmt.Errorf("read approver kubeconfig: %w", err)
	}
	if config.Host != workload.Host {
		return nil, nil, fmt.Errorf("approver kubeconfig must name the workload API endpoint")
	}
	config.QPS, config.Burst = workload.QPS, workload.Burst
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	author, err := capacityUsername(ctx, workload)
	if err != nil {
		return nil, nil, fmt.Errorf("identify workload writer: %w", err)
	}
	approver, err := capacityUsername(ctx, config)
	if err != nil {
		return nil, nil, fmt.Errorf("identify approver: %w", err)
	}
	if author == approver {
		return nil, nil, fmt.Errorf("approver must differ from workload writer %q", author)
	}
	writer, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, nil, err
	}
	return writer, &approvalActors{WorkloadWriter: author, Approver: approver}, nil
}

func capacityUsername(ctx context.Context, config *rest.Config) (string, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return "", err
	}
	review, err := clientset.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	if review.Status.UserInfo.Username == "" {
		return "", fmt.Errorf("SelfSubjectReview returned no username")
	}
	return review.Status.UserInfo.Username, nil
}

func (s *scenarios) approvalWriter() dynamic.Interface {
	if s.approver != nil {
		return s.approver
	}
	return s.dynamic
}
