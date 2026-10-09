package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	ptahv1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/crdupgrade"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Progress belongs to one completed hook. A restart discards these receipts
// and proves them again; it cannot inherit an unverified recovery.
type recoveryVerifier struct {
	attempt string
	probes  map[string]bool
}

func verifyRecovery(ctx context.Context, c client.Client, s State) error {
	return new(recoveryVerifier).verify(ctx, c, s)
}

func (v *recoveryVerifier) verify(ctx context.Context, c client.Client, s State) error {
	if s.HistoryLost || len(s.Attempts) == 0 || s.Attempts[len(s.Attempts)-1].CompletedAt == nil {
		return errors.New("no completed hook with continuous history")
	}
	attempt := s.Attempts[len(s.Attempts)-1]
	if v.attempt != attempt.UID {
		v.attempt = attempt.UID
		v.probes = map[string]bool{}
	}
	after := *attempt.CompletedAt
	// Check runtimes again after admission and workload probes. A successful
	// hook alone cannot resolve an alert while the installation is stopped.
	for _, name := range []string{s.Intent.Manager, s.Intent.Rotator} {
		if err := verifyRuntime(ctx, c, s.Intent, name); err != nil {
			return err
		}
	}
	for name, want := range s.Intent.CRDDigests {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := c.Get(ctx, client.ObjectKey{Name: name}, crd); err != nil {
			return err
		}
		got, err := crdupgrade.ComputeSchemaDigest(crd)
		if err != nil {
			return err
		}
		established, names := false, false
		for _, condition := range crd.Status.Conditions {
			if condition.Type == apiextensionsv1.Established && condition.Status == apiextensionsv1.ConditionTrue {
				established = true
			}
			if condition.Type == apiextensionsv1.NamesAccepted && condition.Status == apiextensionsv1.ConditionTrue {
				names = true
			}
			if condition.Type == apiextensionsv1.Terminating && condition.Status == apiextensionsv1.ConditionTrue {
				return errors.New("candidate CRD is terminating")
			}
		}
		if crd.DeletionTimestamp != nil || !established || !names || got != want || crd.Annotations[crdupgrade.SchemaDigestAnnotation] != want {
			return errors.New("candidate CRD identity or availability differs")
		}
	}
	var pending error
	for _, probe := range s.Intent.Probes {
		var object client.Object
		switch probe.Kind {
		case "PtahSchema":
			object = &ptahv1.PtahSchema{}
		case "PtahMigration":
			object = &ptahv1.PtahMigration{}
		default:
			return errors.New("unknown recovery probe")
		}
		if err := c.Get(ctx, client.ObjectKey{Namespace: probe.Namespace, Name: probe.Name}, object); err != nil {
			return err
		}
		if err := verifyProbeState(object, probe, after, v.probes[probe.UID]); err != nil {
			delete(v.probes, probe.UID)
			pending = fmt.Errorf("probe %s: %w", probe.Name, err)
			continue
		}
		// Retain the post-hook progress before admission: the first dry run
		// can also conflict with the next ordinary read's status update.
		v.probes[probe.UID] = true
		before := object.DeepCopyObject().(client.Object)
		annotations := object.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations["qualification.ptah.run/upgrade-admission-probe"] = s.StartedAt.Format(time.RFC3339Nano)
		object.SetAnnotations(annotations)
		// An actual server dry run traverses admission. It changes no stored
		// spec, status, generation or authorization.
		if err := c.Patch(ctx, object, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}), client.DryRunAll); err != nil {
			// A status write can race this optimistic dry run without undoing
			// the post-hook progress already verified for this generation. Keep
			// that boundary on conflict, but require fresh state and successful
			// admission on the next attempt before reporting recovery.
			if !apierrors.IsConflict(err) {
				delete(v.probes, probe.UID)
			}
			pending = fmt.Errorf("candidate admission probe: %w", err)
			continue
		}
	}
	if pending != nil {
		return pending
	}
	for _, name := range []string{s.Intent.Manager, s.Intent.Rotator} {
		if err := verifyRuntime(ctx, c, s.Intent, name); err != nil {
			return err
		}
	}
	return nil
}

func verifyProbe(object client.Object, p Probe, after time.Time) error {
	return verifyProbeState(object, p, after, false)
}

func verifyProbeState(object client.Object, p Probe, after time.Time, previouslyVerified bool) error {
	if string(object.GetUID()) != p.UID || object.GetGeneration() != p.Generation || object.GetDeletionTimestamp() != nil {
		return errors.New("workload probe changed identity or generation")
	}
	var conditions []metav1.Condition
	var observed int64
	var fresh, ordinaryRead bool
	switch v := object.(type) {
	case *ptahv1.PtahSchema:
		if v.Spec.Suspend || v.Status.PendingObservation != nil || v.Status.PendingLockRelease != nil || v.Status.PendingBindingRetirement != nil {
			return errors.New("schema probe is suspended or has unsettled work")
		}
		if op := v.Status.ActiveOperation; op != nil {
			switch op.Type {
			case ptahv1.OperationResolve, ptahv1.OperationVerify, ptahv1.OperationObserve, ptahv1.OperationPlan:
				ordinaryRead = op.Attempt == 1
			}
			if !previouslyVerified || !ordinaryRead {
				return errors.New("schema probe has unverified or mutating work")
			}
		}
		observed, conditions = v.Status.ObservedGeneration, v.Status.Conditions
		fresh = v.Status.Target.LastObservedAt != nil && v.Status.Target.LastObservedAt.After(after) && !v.Status.Target.LastObservedAt.After(time.Now())
	case *ptahv1.PtahMigration:
		if v.Spec.Suspend || v.Status.UnresolvedRun != nil || v.Status.PendingLockRelease != nil {
			return errors.New("migration probe is suspended or has unsettled work")
		}
		if op := v.Status.ActiveOperation; op != nil {
			switch op.Type {
			case ptahv1.MigrationOperationResolve, ptahv1.MigrationOperationVerify, ptahv1.MigrationOperationHistory:
				ordinaryRead = op.Attempt == 1 && op.RetryNotBefore == nil
			}
			if !previouslyVerified || !ordinaryRead {
				return errors.New("migration probe has unverified or mutating work")
			}
		}
		observed, conditions = v.Status.ObservedGeneration, v.Status.Conditions
		fresh = v.Status.History != nil && v.Status.History.ObservedAt.After(after) && !v.Status.History.ObservedAt.After(time.Now())
	default:
		return errors.New("unknown recovery probe type")
	}
	if observed != p.Generation || !fresh {
		return errors.New("workload has no current post-upgrade progress")
	}
	failed := meta.FindStatusCondition(conditions, "ReconciliationFailed")
	if failed != nil && failed.Status == metav1.ConditionTrue {
		return errors.New("workload probe reports a failure")
	}
	// A new ordinary read does not undo the post-hook healthy boundary this
	// exact generation already reached. Identity, fresh progress, admission and
	// all mutation/unknown-outcome exclusions still hold on every observation.
	if previouslyVerified && ordinaryRead {
		return nil
	}
	ready := meta.FindStatusCondition(conditions, ptahv1.ConditionReady)
	approval := meta.FindStatusCondition(conditions, ptahv1.ConditionApprovalRequired)
	healthy := ready != nil && ready.ObservedGeneration == p.Generation && (ready.Status == metav1.ConditionTrue || ready.Status == metav1.ConditionFalse && ready.Reason == "ApplyDisabled")
	healthy = healthy || approval != nil && approval.Status == metav1.ConditionTrue && approval.ObservedGeneration == p.Generation
	if !healthy {
		return errors.New("workload probe has not reached a healthy policy boundary")
	}
	return nil
}

func verifyRuntime(ctx context.Context, c client.Client, i Intent, name string) error {
	d := &appsv1.Deployment{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: i.Namespace, Name: name}, d); err != nil {
		return err
	}
	if d.UID == "" || d.Labels["app.kubernetes.io/instance"] != i.Release || d.DeletionTimestamp != nil || d.Spec.Replicas == nil || *d.Spec.Replicas < 1 || d.Status.ObservedGeneration != d.Generation || d.Status.UpdatedReplicas != *d.Spec.Replicas || d.Status.ReadyReplicas != *d.Spec.Replicas || d.Status.AvailableReplicas != *d.Spec.Replicas || d.Status.Replicas != *d.Spec.Replicas {
		return errors.New("candidate runtime is not fully available")
	}
	if len(d.Spec.Template.Spec.Containers) != 1 || d.Spec.Template.Spec.Containers[0].Image != i.Image {
		return errors.New("runtime uses another candidate image")
	}
	if d.Spec.Selector == nil {
		return errors.New("candidate runtime has no selector")
	}
	selector, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil || selector.Empty() {
		return errors.New("candidate runtime selector is empty or invalid")
	}
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(i.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return err
	}
	if len(pods.Items) != int(*d.Spec.Replicas) {
		return errors.New("candidate runtime has an incomplete Pod inventory")
	}
	for _, pod := range pods.Items {
		owner := metav1.GetControllerOf(&pod)
		if owner == nil || owner.Kind != "ReplicaSet" || owner.APIVersion != appsv1.SchemeGroupVersion.String() {
			return errors.New("candidate Pod has no ReplicaSet owner")
		}
		rs := &appsv1.ReplicaSet{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: i.Namespace, Name: owner.Name}, rs); err != nil {
			return err
		}
		parent := metav1.GetControllerOf(rs)
		if rs.UID != owner.UID || parent == nil || parent.Kind != "Deployment" || parent.APIVersion != appsv1.SchemeGroupVersion.String() || parent.Name != d.Name || parent.UID != d.UID {
			return errors.New("candidate Pod belongs to another runtime")
		}

		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning || len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Image != i.Image || len(pod.Status.ContainerStatuses) != 1 || !pod.Status.ContainerStatuses[0].Ready || pod.Status.ContainerStatuses[0].Name != pod.Spec.Containers[0].Name || pod.Status.ContainerStatuses[0].State.Running == nil || pod.Status.ContainerStatuses[0].ImageID == "" {
			return errors.New("candidate Pod is not ready on its exact image")
		}
		ready := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if !ready {
			return errors.New("candidate Pod has no Ready condition")
		}
	}
	return nil
}
