package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
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
			"usernameKey": "username", "passwordKey": "password", "registryKey": "registry",
		},
		"verificationPolicyFrom": map[string]any{"name": policy, "key": "policy.yaml"},
		"transport":              map[string]any{"plainHTTP": true},
	}
}

func (s *scenarios) target(name string, database int) map[string]any {
	return map[string]any{
		"engine":          "PostgreSQL",
		"coordinationKey": "capacity/" + name,
		"urlFrom":         map[string]any{"name": s.secretFor(database), "key": "url"},
	}
}

func (s *scenarios) schemaObject(index int) *unstructured.Unstructured {
	name := s.schemaName(index)
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahSchema",
		"metadata": map[string]any{"name": name, "namespace": s.in.namespace, "labels": map[string]any{capacityLabel: s.load.Name}},
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
		if _, err := s.dynamic.Resource(schemaResource).Namespace(s.in.namespace).Create(ctx, s.schemaObject(index), metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create %s: %w", s.schemaName(index), err)
		}
	}
	for index := range s.load.Migrations {
		object := s.migrationObject(s.migrationName(index), s.load.Schemas+index, "Always", true)
		if _, err := s.dynamic.Resource(migrationResource).Namespace(s.in.namespace).Create(ctx, object, metav1.CreateOptions{}); err != nil {
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
	for _, family := range []struct {
		resource schema.GroupVersionResource
		observed []string
	}{
		{schemaResource, []string{"status", "target", "lastObservedAt"}},
		{migrationResource, []string{"status", "history", "observedAt"}},
	} {
		list, err := s.dynamic.Resource(family.resource).Namespace(s.in.namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return false, err
		}
		for _, item := range list.Items {
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
	if _, err := s.dynamic.Resource(migrationResource).Namespace(s.in.namespace).Create(ctx, object, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create %s: %w", approvalResource, err)
	}
	deadline := time.Now().Add(s.load.Settle.Duration)
	for time.Now().Before(deadline) {
		item, err := s.dynamic.Resource(migrationResource).Namespace(s.in.namespace).Get(ctx, approvalResource, metav1.GetOptions{})
		if err == nil {
			if plan, _, _ := unstructured.NestedString(item.Object, "status", "plan", "name"); plan != "" {
				return nil
			}
		}
		time.Sleep(pollEvery)
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
		if err := s.clientset.CoreV1().Pods(s.in.operatorNamespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete manager Pod %s: %w", pod.Name, err)
		}
	}
	admitted, dispatched := s.approveAndWait(ctx, start)
	converged, err := s.waitConverged(ctx, start, nil)
	s.mark("restart burst", start, map[string]string{
		"converged":          converged,
		"approvalAdmitted":   admitted,
		"approvalDispatched": dispatched,
		"managerPods":        fmt.Sprint(len(pods.Items)),
	})
	return err
}

// approveAndWait creates the approval as soon as admission takes it -- the
// managers serve admission, so it is refused until one is back -- and reports
// how long after the restart it was admitted and how long until its Apply Job
// existed.
func (s *scenarios) approveAndWait(ctx context.Context, start time.Time) (string, string) {
	approval, err := s.approvalObject(ctx)
	if err != nil {
		slog.Warn("build the approval", "error", err)
		return "not built", "not built"
	}
	admitted := "not within " + restartReadyBudget.String()
	deadline := start.Add(restartReadyBudget)
	for time.Now().Before(deadline) {
		_, err := s.dynamic.Resource(approvalGVR).Namespace(s.in.namespace).Create(ctx, approval, metav1.CreateOptions{})
		if err == nil || apierrors.IsAlreadyExists(err) {
			admitted = time.Since(start).Round(time.Second).String()
			break
		}
		time.Sleep(pollEvery)
	}
	dispatched := "not within " + s.load.Settle.String()
	deadline = time.Now().Add(s.load.Settle.Duration)
	for time.Now().Before(deadline) {
		jobs, err := s.clientset.BatchV1().Jobs(s.in.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "operator.ptah.run/migration=" + approvalResource + ",operator.ptah.run/operation=apply",
		})
		if err == nil && len(jobs.Items) > 0 {
			dispatched = jobs.Items[0].CreationTimestamp.Sub(start).Round(time.Second).String()
			break
		}
		time.Sleep(pollEvery)
	}
	return admitted, dispatched
}

func (s *scenarios) approvalObject(ctx context.Context) (*unstructured.Unstructured, error) {
	migration, err := s.dynamic.Resource(migrationResource).Namespace(s.in.namespace).Get(ctx, approvalResource, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	planName, _, _ := unstructured.NestedString(migration.Object, "status", "plan", "name")
	plan, err := s.dynamic.Resource(migrationPlanResource).Namespace(s.in.namespace).Get(ctx, planName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	fingerprint, _, _ := unstructured.NestedString(plan.Object, "spec", "fingerprint")
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahMigrationApproval",
		"metadata": map[string]any{"name": approvalResource, "namespace": s.in.namespace},
		"spec": map[string]any{
			"migrationRef":    map[string]any{"name": approvalResource, "uid": string(migration.GetUID())},
			"planRef":         map[string]any{"name": planName, "uid": string(plan.GetUID())},
			"planFingerprint": fingerprint,
		},
	}}, nil
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
	patch := fmt.Sprintf(`{"spec":{%q:{"ociRef":%q}}}`, field, reference)
	_, err := s.dynamic.Resource(resource).Namespace(s.in.namespace).Patch(ctx, name, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("move %s to %s: %w", name, reference, err)
	}
	return nil
}

func digestOf(reference string) string {
	_, digest, _ := strings.Cut(reference, "@")
	return digest
}

// outage cuts every operation Pod off from the registry for a while, then
// restores it and times the recovery. A NetworkPolicy does the cutting, so the
// registry itself keeps running for everything else in the lab.
func (s *scenarios) outage(ctx context.Context) error {
	if s.load.Outage.Duration == 0 {
		return nil
	}
	if s.in.registryIP == "" {
		return errors.New("an outage needs the registry address")
	}
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: outagePolicyName, Namespace: s.in.namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/managed-by": "ptah-operator"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{
					CIDR: "0.0.0.0/0", Except: []string{s.in.registryIP + "/32"},
				}}},
			}},
		},
	}
	start := time.Now().UTC()
	if _, err := s.clientset.NetworkingV1().NetworkPolicies(s.in.namespace).Create(ctx, policy, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("cut the registry off: %w", err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.load.Outage.Duration):
	}
	s.mark("registry outage", start, nil)
	restored := time.Now().UTC()
	if err := s.clientset.NetworkingV1().NetworkPolicies(s.in.namespace).Delete(ctx, outagePolicyName, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("restore the registry: %w", err)
	}
	converged, err := s.waitConverged(ctx, restored, nil)
	s.mark("recovery", restored, map[string]string{"converged": converged})
	return err
}
