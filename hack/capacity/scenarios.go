package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const (
	capacityLabel      = "operator.ptah.run/capacity"
	approvalResource   = "capacity-approval"
	outagePolicyName   = "capacity-registry-outage"
	pollEvery          = 2 * time.Second
	restartReadyBudget = 5 * time.Minute
)

// inputs are what the harness prepared on the lab before the tool runs.
type inputs struct {
	namespace         string
	namespaces        []string
	operatorNamespace string
	managerSelector   string
	registrySecret    string
	schemaPolicy      string
	migrationPolicy   string
	databaseSecret    string // a fmt pattern with one %d, one database per resource
	schemaRefs        [2]string
	migrationRefs     [2]string
	registryIP        string
}

type scenarios struct {
	in        inputs
	load      workload
	clientset kubernetes.Interface
	dynamic   dynamic.Interface
	windows   []window
	recorders []*cycleRecorder
}

func (s *scenarios) mark(name string, start time.Time, outcome map[string]string) {
	s.windows = append(s.windows, window{Name: name, Start: start, End: time.Now().UTC(), Outcome: outcome})
}

func (s *scenarios) schemaName(index int) string { return fmt.Sprintf("capacity-schema-%03d", index) }
func (s *scenarios) migrationName(index int) string {
	return fmt.Sprintf("capacity-migration-%03d", index)
}

// secretFor is the database a resource runs against. Schemas take the first
// databases, migrations the next ones and the approval resource the last, so no
// two resources share a database or a realm.
func (s *scenarios) secretFor(index int) string { return fmt.Sprintf(s.in.databaseSecret, index) }

func (s *scenarios) execution() map[string]any {
	return map[string]any{"activeDeadlineSeconds": int64(300), "failureRetryInterval": "10s", "connectTimeout": "30s"}
}

func (s *scenarios) artifactSource(reference, policy string) map[string]any {
	return map[string]any{
		"ociRef": reference,
		"registryAuthFrom": map[string]any{
			"name": s.in.registrySecret, "mode": "Environment",
			"usernameKey": "username", "passwordKey": "password",
		},
		"verificationPolicyFrom": map[string]any{"name": policy, "key": "policy.yaml"},
		"transport":              map[string]any{"plainHTTP": true},
	}
}

func (s *scenarios) target(name string, database int) map[string]any {
	return map[string]any{
		"engine":          s.load.engine(),
		"coordinationKey": "capacity/" + name,
		"urlFrom":         map[string]any{"name": s.secretFor(database), "key": "url"},
	}
}

func (s *scenarios) schemaObject(index int) *unstructured.Unstructured {
	name := s.schemaName(index)
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahSchema",
		"metadata": map[string]any{"name": name, "namespace": s.in.namespaceFor(index), "labels": map[string]any{capacityLabel: s.load.Name}},
		"spec": map[string]any{
			"target":    s.target(name, index),
			"desired":   s.artifactSource(s.in.schemaRefs[0], s.in.schemaPolicy),
			"policy":    map[string]any{"apply": "Always", "allowDestructive": false, "driftSeverity": "all"},
			"interval":  s.load.Interval.String(),
			"execution": s.execution(),
		},
	}}
}

func (s *scenarios) migrationObject(name string, database int, apply string, labelled bool) *unstructured.Unstructured {
	metadata := map[string]any{"name": name, "namespace": s.in.namespace}
	if labelled {
		metadata["namespace"] = s.in.namespaceFor(database - s.load.Schemas)
		metadata["labels"] = map[string]any{capacityLabel: s.load.Name}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahMigration",
		"metadata": metadata,
		"spec": map[string]any{
			"target":    s.target(name, database),
			"artifact":  s.artifactSource(s.in.migrationRefs[0], s.in.migrationPolicy),
			"policy":    map[string]any{"apply": apply, "lockTimeout": "30s"},
			"interval":  s.load.Interval.String(),
			"execution": s.execution(),
		},
	}}
}

// create puts the workload in place and waits for it to converge for the
// first time, which is the cold start every installation goes through once.
func (s *scenarios) create(ctx context.Context) error {
	start := time.Now().UTC()
	for index := range s.load.Schemas {
		if _, err := s.dynamic.Resource(schemaResource).Namespace(s.in.namespaceFor(index)).Create(ctx, s.schemaObject(index), metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create %s: %w", s.schemaName(index), err)
		}
	}
	for index := range s.load.Migrations {
		object := s.migrationObject(s.migrationName(index), s.load.Schemas+index, "Always", true)
		if _, err := s.dynamic.Resource(migrationResource).Namespace(object.GetNamespace()).Create(ctx, object, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create %s: %w", s.migrationName(index), err)
		}
	}
	converged, err := s.waitConverged(ctx, start, nil)
	s.mark("cold start", start, map[string]string{"converged": converged})
	return err
}

// waitConverged waits until every workload resource is InSync, read after
// `after` when that is set, and satisfies `extra` for the ones it names. It
// returns how long that took, or the budget if it never did.
func (s *scenarios) waitConverged(ctx context.Context, after time.Time, extra func(unstructured.Unstructured) bool) (string, error) {
	start := time.Now()
	deadline := start.Add(s.load.Settle.Duration)
	for time.Now().Before(deadline) {
		done, err := s.allConverged(ctx, after, extra)
		if err != nil {
			slog.Warn("read the workload", "error", err)
		} else if done {
			return time.Since(start).Round(time.Second).String(), nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pollEvery):
		}
	}
	return "not within " + s.load.Settle.String(), fmt.Errorf("the workload did not converge within %s", s.load.Settle)
}

func (s *scenarios) allConverged(ctx context.Context, after time.Time, extra func(unstructured.Unstructured) bool) (bool, error) {
	selector := capacityLabel + "=" + s.load.Name
	total := 0
	for _, namespace := range workloadNamespaces(s.in.namespace, s.in.namespaces) {
		for _, family := range []struct {
			resource schema.GroupVersionResource
			observed []string
		}{
			{schemaResource, []string{"status", "target", "lastObservedAt"}},
			{migrationResource, []string{"status", "history", "observedAt"}},
		} {
			list, err := s.dynamic.Resource(family.resource).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				return false, err
			}
			for _, item := range list.Items {
				wantNamespace, err := s.resourceNamespace(family.resource, item.GetName())
				if err != nil || wantNamespace != namespace {
					return false, fmt.Errorf("unexpected workload resource %s/%s", namespace, item.GetName())
				}
				total++
				if phase, _, _ := unstructured.NestedString(item.Object, "status", "phase"); phase != "InSync" {
					return false, nil
				}
				if !after.IsZero() {
					observed, ok := timestampAt(item.Object, family.observed...)
					if !ok || observed.Before(after.Truncate(time.Second)) {
						return false, nil
					}
				}
				if extra != nil && !extra(item) {
					return false, nil
				}
			}
		}
	}
	return total == s.load.Schemas+s.load.Migrations, nil
}

// steady watches the converged workload with nothing changing.
func (s *scenarios) steady(ctx context.Context) error {
	start := time.Now().UTC()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.load.SteadyState.Duration):
	}
	s.mark("steady state", start, nil)
	return nil
}

// prepareApproval puts one resource at the approval gate before the burst, so
// the burst can be measured against the one piece of work a person asked for.
func (s *scenarios) prepareApproval(ctx context.Context) error {
	object := s.migrationObject(approvalResource, s.load.Schemas+s.load.Migrations, "OnApproval", false)
	if _, err := s.dynamic.Resource(migrationResource).Namespace(object.GetNamespace()).Create(ctx, object, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create %s: %w", approvalResource, err)
	}
	deadline := time.Now().Add(s.load.Settle.Duration)
	for time.Now().Before(deadline) {
		item, err := s.dynamic.Resource(migrationResource).Namespace(s.in.namespace).Get(ctx, approvalResource, metav1.GetOptions{})
		if err == nil && approvalGateReady(item) {
			return nil
		}
		if err := waitCapacityPoll(ctx); err != nil {
			return err
		}
	}
	return fmt.Errorf("%s published no plan within %s", approvalResource, s.load.Settle)
}

// restart deletes every manager Pod at once, which is what a rollout does to
// the whole workload's cadence, and at the same instant asks for the one
// Apply a person approved.
func (s *scenarios) restart(ctx context.Context) error {
	pods, err := s.clientset.CoreV1().Pods(s.in.operatorNamespace).List(ctx, metav1.ListOptions{LabelSelector: s.in.managerSelector})
	if err != nil {
		return err
	}
	if len(pods.Items) == 0 {
		return fmt.Errorf("no manager Pod matches %q in %s", s.in.managerSelector, s.in.operatorNamespace)
	}
	start := time.Now().UTC()
	for _, pod := range pods.Items {
		if err := s.clientset.CoreV1().Pods(s.in.operatorNamespace).Delete(ctx, pod.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &pod.UID}}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete manager Pod %s: %w", pod.Name, err)
		}
	}
	outcome, err := s.approveAndWait(ctx, start)
	outcome["managerPods"] = fmt.Sprint(len(pods.Items))
	if err == nil {
		outcome["converged"], err = s.waitRestartConverged(ctx, start)
	}
	if err != nil {
		outcome["error"] = err.Error()
	}
	s.mark("restart burst", start, outcome)
	return err
}

// change moves a batch of each family to its second artifact at once.
func (s *scenarios) change(ctx context.Context) error {
	if s.load.ChangeBatch == 0 {
		return nil
	}
	start := time.Now().UTC()
	moved := map[string]string{}
	for index := range min(s.load.ChangeBatch, s.load.Schemas) {
		name := s.schemaName(index)
		if err := s.patchReference(ctx, schemaResource, name, "desired", s.in.schemaRefs[1]); err != nil {
			return err
		}
		moved["PtahSchema/"+name] = digestOf(s.in.schemaRefs[1])
	}
	for index := range min(s.load.ChangeBatch, s.load.Migrations) {
		name := s.migrationName(index)
		if err := s.patchReference(ctx, migrationResource, name, "artifact", s.in.migrationRefs[1]); err != nil {
			return err
		}
		moved["PtahMigration/"+name] = digestOf(s.in.migrationRefs[1])
	}
	converged, err := s.waitConverged(ctx, start, func(item unstructured.Unstructured) bool {
		want, ok := moved[item.GetKind()+"/"+item.GetName()]
		if !ok {
			return true
		}
		var got string
		if item.GetKind() == "PtahSchema" {
			got, _, _ = unstructured.NestedString(item.Object, "status", "applied", "artifactDigest")
		} else {
			got, _, _ = unstructured.NestedString(item.Object, "status", "artifact", "digest")
		}
		return got == want
	})
	s.mark("change batch", start, map[string]string{"converged": converged, "moved": fmt.Sprint(len(moved))})
	return err
}

func (s *scenarios) patchReference(ctx context.Context, resource schema.GroupVersionResource, name, field, reference string) error {
	namespace, err := s.resourceNamespace(resource, name)
	if err != nil {
		return err
	}
	patch := fmt.Sprintf(`{"spec":{%q:{"ociRef":%q}}}`, field, reference)
	_, err = s.dynamic.Resource(resource).Namespace(namespace).Patch(ctx, name, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("move %s to %s: %w", name, reference, err)
	}
	return nil
}

func digestOf(reference string) string {
	_, digest, _ := strings.Cut(reference, "@")
	return digest
}
