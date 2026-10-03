package main

import (
	"context"
	"fmt"
	"reflect"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// A fresh read can retire an already reserved approval. Retention preserves
// evidence; it does not grant permission to reuse an authorization the
// controller has retired. Renew it through the normal admission path.
func (s *scenarios) recoverFaultApproval(ctx context.Context, proof *retentionFaultProof, expected *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	original := proof.SchemaApproval
	for {
		approval, err := s.dynamic.Resource(schemaApprovalResource).Namespace(original.GetNamespace()).Get(ctx, original.GetName(), metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if approval.GetUID() != original.GetUID() || !reflect.DeepEqual(approval.Object["spec"], original.Object["spec"]) {
			return nil, fmt.Errorf("fault recovery lost the original approval")
		}
		name, _, _ := unstructured.NestedString(original.Object, "spec", "schemaRef", "name")
		current, err := s.dynamic.Resource(schemaResource).Namespace(original.GetNamespace()).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		action, err := faultApprovalRecoveryAction(approval, current, expected)
		if err != nil {
			return nil, err
		}
		switch action {
		case "consumed":
			return approval, nil
		case "renew":
			proof.RecoverySchemaApproval, err = s.createFaultApproval(ctx, "schema", current)
			return proof.RecoverySchemaApproval, err
		}
		if err := waitCapacityPoll(ctx); err != nil {
			return nil, fmt.Errorf("schema approval recovery did not settle: %w", err)
		}
	}
}

func faultApprovalRecoveryAction(approval, current, expected *unstructured.Unstructured) (string, error) {
	uid, _, _ := unstructured.NestedString(approval.Object, "spec", "schemaRef", "uid")
	name, _, _ := unstructured.NestedString(approval.Object, "spec", "schemaRef", "name")
	if expected == nil || current.GetGeneration() != expected.GetGeneration() || !reflect.DeepEqual(current.Object["spec"], expected.Object["spec"]) || uid == "" || current.GetUID() == "" || string(current.GetUID()) != uid || current.GetName() != name || current.GetNamespace() != approval.GetNamespace() || current.GetDeletionTimestamp() != nil {
		return "", fmt.Errorf("fault recovery changed schema identity")
	}
	conditions, _, _ := unstructured.NestedSlice(approval.Object, "status", "conditions")
	stale := false
	for _, raw := range conditions {
		c, _ := raw.(map[string]any)
		if c["status"] != "True" {
			continue
		}
		if c["type"] == "Consumed" {
			return "consumed", nil
		}
		if c["type"] == "Stale" {
			if c["reason"] != "PlanNoLongerCurrent" {
				return "", fmt.Errorf("fault approval retired for an unexpected reason")
			}
			stale = true
		}
	}
	if stale && schemaGateReady(current) {
		return "renew", nil
	}
	return "wait", nil
}
