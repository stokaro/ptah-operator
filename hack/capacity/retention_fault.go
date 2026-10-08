package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	jsonpatch "github.com/evanphx/json-patch/v5"
	ptah "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/jobclaim"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"
)

var schemaApprovalResource = schema.GroupVersionResource{Group: schemaResource.Group, Version: schemaResource.Version, Resource: "ptahschemaapprovals"}

type retentionFaultProof struct {
	SchemaApprovalAfterRecovery *unstructured.Unstructured                    `json:"schemaApprovalAfterRecovery,omitempty"`
	RecoverySchemaApproval      *unstructured.Unstructured                    `json:"recoverySchemaApproval,omitempty"`
	ObsoleteSchemaPlanUID       types.UID                                     `json:"obsoleteSchemaPlanUID"`
	StartedAt                   time.Time                                     `json:"startedAt"`
	FinishedAt                  time.Time                                     `json:"finishedAt"`
	SchemaApproval              *unstructured.Unstructured                    `json:"schemaApproval,omitempty"`
	MigrationApproval           *unstructured.Unstructured                    `json:"migrationApproval,omitempty"`
	DispatchedMigration         *ptah.PtahMigration                           `json:"dispatchedMigration,omitempty"`
	ApplyJob                    *batchv1.Job                                  `json:"applyJob,omitempty"`
	ApplyPod                    *corev1.Pod                                   `json:"applyPod,omitempty"`
	SuspendedMigration          *unstructured.Unstructured                    `json:"suspendedMigration,omitempty"`
	RecoveredMigration          *unstructured.Unstructured                    `json:"recoveredMigration,omitempty"`
	RecoveredSchema             *unstructured.Unstructured                    `json:"recoveredSchema,omitempty"`
	GatePolicy                  *admissionv1.ValidatingAdmissionPolicy        `json:"gatePolicy,omitempty"`
	GateBinding                 *admissionv1.ValidatingAdmissionPolicyBinding `json:"gateBinding,omitempty"`
	Maintenance                 retentionProof                                `json:"maintenance"`
	Archives                    []retentionArchive                            `json:"archives"`
	Error                       string                                        `json:"error,omitempty"`
}

func faultGate(original *unstructured.Unstructured) (*admissionv1.ValidatingAdmissionPolicy, *admissionv1.ValidatingAdmissionPolicyBinding) {
	name := "capacity-retention-gate-" + string(original.GetUID())
	failure := admissionv1.Fail
	policy := &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: admissionv1.ValidatingAdmissionPolicySpec{
		FailurePolicy: &failure, MatchConstraints: &admissionv1.MatchResources{ResourceRules: []admissionv1.NamedRuleWithOperations{{RuleWithOperations: admissionv1.RuleWithOperations{Operations: []admissionv1.OperationType{admissionv1.Update}, Rule: admissionv1.Rule{APIGroups: []string{schemaResource.Group}, APIVersions: []string{schemaResource.Version}, Resources: []string{"ptahschemas/status"}}}}}},
		Validations: []admissionv1.Validation{{Expression: fmt.Sprintf("object.metadata.uid != %q || !has(object.status) || !has(object.status.activeOperation) || object.status.activeOperation.type != 'Apply'", original.GetUID()), Message: name}},
	}}
	binding := &admissionv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: admissionv1.ValidatingAdmissionPolicyBindingSpec{PolicyName: name, ValidationActions: []admissionv1.ValidationAction{admissionv1.Deny}}}
	return policy, binding
}

func (s *scenarios) patchFaultSpec(ctx context.Context, resource schema.GroupVersionResource, original *unstructured.Unstructured, fields map[string]any) (*unstructured.Unstructured, error) {
	return patchCapacitySpec(ctx, s.workloadWriter(), resource, original, fields)
}

func patchCapacitySpec(ctx context.Context, writer dynamic.Interface, resource schema.GroupVersionResource, original *unstructured.Unstructured, fields map[string]any) (*unstructured.Unstructured, error) {
	raw, err := json.Marshal(map[string]any{"metadata": map[string]any{"uid": original.GetUID(), "resourceVersion": original.GetResourceVersion()}, "spec": fields})
	if err != nil {
		return nil, err
	}
	client := writer.Resource(resource).Namespace(original.GetNamespace())
	var result *unstructured.Unstructured
	refresh := false
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		patch := raw
		if refresh {
			current, err := client.Get(ctx, original.GetName(), metav1.GetOptions{})
			if err != nil {
				return err
			}
			// Status and finalizer writes can race with the author/installer
			// handoff. A retry may refresh the version, never the desired state
			// or the resource identity the original write was bound to.
			if current.GetUID() != original.GetUID() || current.GetName() != original.GetName() || current.GetNamespace() != original.GetNamespace() || current.GetGeneration() != original.GetGeneration() || current.GetDeletionTimestamp() != nil || !reflect.DeepEqual(current.Object["spec"], original.Object["spec"]) {
				return fmt.Errorf("spec patch conflict changed identity, generation or desired state")
			}
			patch, err = json.Marshal(map[string]any{"metadata": map[string]any{"uid": original.GetUID(), "resourceVersion": current.GetResourceVersion()}, "spec": fields})
			if err != nil {
				return err
			}
		}
		var err error
		result, err = client.Patch(ctx, original.GetName(), types.MergePatchType, patch, metav1.PatchOptions{})
		refresh = true
		return err
	})
	if err != nil {
		return nil, err
	}
	originalJSON, err := json.Marshal(original.Object)
	if err != nil {
		return nil, err
	}
	expectedJSON, err := jsonpatch.MergePatch(originalJSON, raw)
	if err != nil {
		return nil, err
	}
	var expected unstructured.Unstructured
	if err = expected.UnmarshalJSON(expectedJSON); err != nil {
		return nil, err
	}
	if result.GetUID() != original.GetUID() || result.GetName() != original.GetName() || result.GetNamespace() != original.GetNamespace() || result.GetGeneration() != original.GetGeneration()+1 || !reflect.DeepEqual(result.Object["spec"], expected.Object["spec"]) {
		return nil, fmt.Errorf("fault patch changed identity, generation or unrelated spec")
	}
	return result, nil
}

func (s *scenarios) waitFaultResource(ctx context.Context, resource schema.GroupVersionResource, original *unstructured.Unstructured, ready func(*unstructured.Unstructured) bool) (*unstructured.Unstructured, error) {
	for {
		current, err := s.dynamic.Resource(resource).Namespace(original.GetNamespace()).Get(ctx, original.GetName(), metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if current.GetUID() != original.GetUID() || current.GetGeneration() != original.GetGeneration() || !reflect.DeepEqual(current.Object["spec"], original.Object["spec"]) || current.GetDeletionTimestamp() != nil {
			return nil, fmt.Errorf("fault resource identity or spec changed")
		}
		if ready(current) {
			return current, nil
		}
		if err := waitCapacityPoll(ctx); err != nil {
			return nil, fmt.Errorf("fault resource %s did not reach its required condition: %w", original.GetName(), err)
		}
	}
}

func schemaGateReady(o *unstructured.Unstructured) bool {
	var v ptah.PtahSchema
	if runtime.DefaultUnstructuredConverter.FromUnstructured(o.Object, &v) != nil || v.UID == "" || v.Spec.Policy.Apply != ptah.ApplyPolicyOnApproval || v.Status.ObservedGeneration != v.Generation || v.Status.Plan == nil || v.Status.Plan.UID == "" || v.Status.Plan.Approval != nil || v.Status.ActiveOperation != nil {
		return false
	}
	for _, c := range v.Status.Conditions {
		if c.Type == "ApprovalRequired" && c.Status == metav1.ConditionTrue && c.ObservedGeneration == v.Generation {
			return v.Status.Phase == ptah.PhaseAwaitingApproval
		}
	}
	return false
}

func (s *scenarios) approvalForResource(ctx context.Context, family string, original *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if family != "schema" && family != "migration" {
		return nil, fmt.Errorf("unknown approval family %q", family)
	}
	var approval *unstructured.Unstructured
	var err error
	if family == "migration" {
		approval, err = s.approvalFor(ctx, original)
	} else {
		if !schemaGateReady(original) {
			return nil, fmt.Errorf("schema has no current approval gate")
		}
		name, _, _ := unstructured.NestedString(original.Object, "status", "plan", "name")
		uid, _, _ := unstructured.NestedString(original.Object, "status", "plan", "uid")
		plan, e := s.dynamic.Resource(schemaPlanResource).Namespace(original.GetNamespace()).Get(ctx, name, metav1.GetOptions{})
		if e != nil {
			return nil, e
		}
		ownerName, _, _ := unstructured.NestedString(plan.Object, "spec", "schemaRef", "name")
		ownerUID, _, _ := unstructured.NestedString(plan.Object, "spec", "schemaRef", "uid")
		fingerprint, _, _ := unstructured.NestedString(plan.Object, "spec", "fingerprint")
		if string(plan.GetUID()) != uid || ownerName != original.GetName() || ownerUID != string(original.GetUID()) || fingerprint == "" {
			return nil, fmt.Errorf("fault approval lost the exact schema plan")
		}
		approval = &unstructured.Unstructured{Object: map[string]any{"apiVersion": "operator.ptah.run/v1alpha1", "kind": "PtahSchemaApproval", "metadata": map[string]any{"namespace": original.GetNamespace()}, "spec": map[string]any{"schemaRef": map[string]any{"name": original.GetName(), "uid": string(original.GetUID())}, "planRef": map[string]any{"name": name, "uid": uid}, "planFingerprint": fingerprint}}}
	}
	if err != nil {
		return nil, err
	}
	return approval, nil
}

func (s *scenarios) createFaultApproval(ctx context.Context, family string, original *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	approval, err := s.approvalForResource(ctx, family, original)
	if err != nil {
		return nil, err
	}
	resource := approvalGVR
	if family == "schema" {
		resource = schemaApprovalResource
	}
	approval.SetName(fmt.Sprintf("capacity-retention-%s-g%d", family, original.GetGeneration()))
	created, err := s.approvalWriter().Resource(resource).Namespace(original.GetNamespace()).Create(ctx, approval, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	if created.GetUID() == "" || created.GetCreationTimestamp().Time.IsZero() {
		return nil, fmt.Errorf("fault approval has no admitted identity")
	}
	return created, nil
}

func (s *scenarios) waitFaultGateRefusal(ctx context.Context, policy string, since time.Time) (retentionArchive, error) {
	for {
		pods, err := s.clientset.CoreV1().Pods(s.in.operatorNamespace).List(ctx, metav1.ListOptions{LabelSelector: s.in.managerSelector})
		if err != nil {
			return retentionArchive{}, err
		}
		for _, pod := range pods.Items {
			body, err := s.clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: "manager", SinceTime: &metav1.Time{Time: since}, Timestamps: true}).DoRaw(ctx)
			if err != nil {
				return retentionArchive{}, err
			}
			if strings.Contains(string(body), policy) && strings.Contains(string(body), "denied") {
				return writeRetentionEvidence(s.evidenceDir, "retention-fault-gate-refusal.json", map[string]any{"podUID": pod.UID, "policy": policy, "since": since, "log": string(body)})
			}
		}
		if err := waitCapacityPoll(ctx); err != nil {
			return retentionArchive{}, fmt.Errorf("no actual controller claim refusal from the fault gate: %w", err)
		}
	}
}

func faultUnresolved(original *ptah.PtahMigration, current *unstructured.Unstructured) bool {
	var value ptah.PtahMigration
	if runtime.DefaultUnstructuredConverter.FromUnstructured(current.Object, &value) != nil || original == nil || original.Status.ActiveOperation == nil {
		return false
	}
	op := original.Status.ActiveOperation
	if op.Type != ptah.MigrationOperationApply || op.JobUID == "" || op.ID == "" || op.StartedAt.IsZero() || original.Status.History == nil || original.Status.History.TargetIdentityDigest == "" {
		return false
	}
	unknown := value.Status.UnresolvedRun
	if unknown == nil || value.UID != original.UID || value.Status.ActiveOperation != nil || value.Status.PendingLockRelease != nil || value.Status.ObservedGeneration != value.Generation || !value.Spec.Suspend || value.Status.LastRun == nil {
		return false
	}
	if unknown.Outcome != ptah.MigrationRunOutcomeUnknown || unknown.OperationID != op.ID || unknown.JobUID != op.JobUID || unknown.JobName != op.JobName || op.PlanRef == nil || unknown.PlanRef != *op.PlanRef || !unknown.RecordedAt.After(op.StartedAt.Time) || unknown.TargetIdentityDigest != original.Status.History.TargetIdentityDigest || value.Status.LastRun.JobUID != op.JobUID || value.Status.LastRun.JobName != op.JobName || value.Status.LastRun.Outcome != ptah.MigrationRunOutcomeUnknown || !value.Status.LastRun.StartedAt.Equal(&op.StartedAt) {
		return false
	}
	var copied ptah.UnresolvedMigrationRunStatus
	return json.Unmarshal([]byte(value.Annotations[ptah.UnresolvedRunAnnotation]), &copied) == nil && reflect.DeepEqual(&copied, unknown)
}

func faultEvidencePreserved(before, after retentionInventory, approval, migration *unstructured.Unstructured) error {
	find := func(inventory retentionInventory, uid types.UID) *unstructured.Unstructured {
		for _, e := range inventory.Objects {
			if e.Object.GetUID() == uid {
				return e.Object
			}
		}
		return nil
	}
	for _, original := range []*unstructured.Unstructured{approval, migration} {
		first, last := find(before, original.GetUID()), find(after, original.GetUID())
		if first == nil || last == nil || original.GetUID() == "" || !reflect.DeepEqual(original.Object["spec"], first.Object["spec"]) || !reflect.DeepEqual(first.Object["spec"], last.Object["spec"]) {
			return fmt.Errorf("retention lost the original fault subject")
		}
		for _, o := range []*unstructured.Unstructured{first, last} {
			if o.GetName() != original.GetName() || o.GetNamespace() != original.GetNamespace() || o.GetKind() != original.GetKind() || o.GetDeletionTimestamp() != nil {
				return fmt.Errorf("retention changed a fault subject identity")
			}
		}
		if original.GetKind() == "PtahMigration" {
			for _, o := range []*unstructured.Unstructured{first, last} {
				want, _, _ := unstructured.NestedMap(original.Object, "status", "unresolvedRun")
				got, _, _ := unstructured.NestedMap(o.Object, "status", "unresolvedRun")
				if len(want) == 0 || !reflect.DeepEqual(want, got) || o.GetAnnotations()[ptah.UnresolvedRunAnnotation] != original.GetAnnotations()[ptah.UnresolvedRunAnnotation] {
					return fmt.Errorf("retention changed unresolved Apply evidence")
				}
			}
		} else {
			if !reflect.DeepEqual(original.Object["spec"], last.Object["spec"]) {
				return fmt.Errorf("retention changed the pending approval")
			}
			for _, o := range []*unstructured.Unstructured{first, last} {
				conditions, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
				for _, raw := range conditions {
					c, _ := raw.(map[string]any)
					if (c["type"] == "Consumed" || c["type"] == "Stale") && c["status"] == "True" {
						return fmt.Errorf("pending approval was consumed or invalidated during pruning")
					}
				}
			}
		}
	}
	return nil
}

func readFaultInventory(path string) (retentionInventory, error) {
	var inventory retentionInventory
	raw, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(raw, &inventory)
	}
	return inventory, err
}

// This runs after the measured soak, on the same twenty resource UIDs and
// databases. Deliberately blocked resources are not counted as eligible cycles.
func (s *scenarios) retentionFault(ctx context.Context) (err error) {
	if s.load.Soak == nil || !s.load.Soak.RetentionFault {
		return nil
	}
	if s.faultProbe == nil {
		return fmt.Errorf("retention fault has no native database probe")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	proof := &retentionFaultProof{StartedAt: time.Now().UTC(), Maintenance: retentionProof{Round: s.load.Soak.Rounds, Name: "fault", StartedAt: time.Now().UTC()}}
	s.retentionFaultProof = proof
	var paused []pausedResource
	cleanupGate := func(c context.Context) error {
		var problems []error
		if b := proof.GateBinding; b != nil {
			u := b.UID
			e := s.clientset.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Delete(c, b.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &u}})
			if !apierrors.IsNotFound(e) {
				problems = append(problems, e)
			}
		}
		if p := proof.GatePolicy; p != nil {
			u := p.UID
			e := s.clientset.AdmissionregistrationV1().ValidatingAdmissionPolicies().Delete(c, p.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &u}})
			if !apierrors.IsNotFound(e) {
				problems = append(problems, e)
			}
		}
		return errors.Join(problems...)
	}
	defer func() {
		recovery, stop := context.WithTimeout(context.WithoutCancel(ctx), s.load.Settle.Duration)
		defer stop()
		err = errors.Join(err, cleanupGate(recovery))
		if !proof.Maintenance.Resumed && len(paused) > 0 {
			_, e := s.resumeMaintenance(recovery, paused)
			err = errors.Join(err, e)
		}
		proof.FinishedAt = time.Now().UTC()
		if err != nil {
			proof.Error = err.Error()
		}
		s.mark("retention fault", proof.StartedAt, map[string]string{"error": proof.Error, "deletedPlans": fmt.Sprint(len(proof.Maintenance.Deleted))})
	}()
	round := s.load.Soak.Rounds
	if err := s.checkpointDatabase(ctx, round, "retention-fault-before"); err != nil {
		return err
	}
	originals := map[string]*unstructured.Unstructured{}
	identities := map[string]any{}
	for _, family := range []string{"schema", "migration"} {
		resource, name := schemaResource, s.schemaName(0)
		if family == "migration" {
			resource, name = migrationResource, s.migrationName(0)
		}
		object, e := s.dynamic.Resource(resource).Namespace(s.in.namespaceFor(0)).Get(ctx, name, metav1.GetOptions{})
		if e != nil {
			return e
		}
		object, e = s.waitFaultResource(ctx, resource, object, inactiveForMaintenance)
		if e != nil {
			return e
		}
		object, e = s.patchFaultSpec(ctx, resource, object, map[string]any{"policy": map[string]any{"apply": "OnApproval"}})
		if e != nil {
			return e
		}
		object, e = s.waitFaultResource(ctx, resource, object, inactiveForMaintenance)
		if e != nil {
			return e
		}
		originals[family] = object
		identities[family] = map[string]any{"name": object.GetName(), "namespace": object.GetNamespace(), "uid": object.GetUID(), "generation": object.GetGeneration()}
	}
	identityArchive, e := writeRetentionEvidence(s.evidenceDir, "retention-fault-identities.json", identities)
	if e != nil {
		return e
	}
	proof.Archives = append(proof.Archives, identityArchive)
	identityPath := filepath.Join(s.evidenceDir, identityArchive.Path)
	if err := s.faultProbe(ctx, "prepare", round, identityPath); err != nil {
		return err
	}
	schemaObject, e := s.waitFaultResource(ctx, schemaResource, originals["schema"], schemaGateReady)
	if e != nil {
		return e
	}
	// The measured maintenance already removed obsolete plans. Publish a
	// real plan under Never with a different lock timeout, then restore the
	// original policy before admitting the
	// pending approval. The Never plan is the unpinned deletion control.
	pendingPlan, _, _ := unstructured.NestedString(schemaObject.Object, "status", "plan", "uid")
	lockTimeout, _, _ := unstructured.NestedString(schemaObject.Object, "spec", "policy", "lockTimeout")
	duration, parseErr := time.ParseDuration(lockTimeout)
	if parseErr != nil {
		return fmt.Errorf("fault schema has no valid lock timeout: %w", parseErr)
	}
	controlTimeout := "31s"
	if duration == 31*time.Second {
		controlTimeout = "32s"
	}
	disabled, e := s.patchFaultSpec(ctx, schemaResource, schemaObject, map[string]any{"policy": map[string]any{"apply": "Never", "lockTimeout": controlTimeout}})
	if e != nil {
		return e
	}
	disabled, e = s.waitFaultResource(ctx, schemaResource, disabled, func(o *unstructured.Unstructured) bool { return faultDisabledPlan(o, pendingPlan) })
	if e != nil {
		return e
	}
	obsoleteUID, _, _ := unstructured.NestedString(disabled.Object, "status", "plan", "uid")
	proof.ObsoleteSchemaPlanUID = types.UID(obsoleteUID)
	schemaObject, e = s.patchFaultSpec(ctx, schemaResource, disabled, map[string]any{"policy": map[string]any{"apply": "OnApproval", "lockTimeout": lockTimeout}})
	if e != nil {
		return e
	}
	schemaObject, e = s.waitFaultResource(ctx, schemaResource, schemaObject, schemaGateReady)
	if e != nil {
		return e
	}
	currentPlan, _, _ := unstructured.NestedString(schemaObject.Object, "status", "plan", "uid")
	if currentPlan != pendingPlan {
		return fmt.Errorf("restoring approval policy did not recover its original plan")
	}
	policy, binding := faultGate(schemaObject)
	proof.GatePolicy, e = s.clientset.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, policy, metav1.CreateOptions{})
	if e != nil {
		return e
	}
	proof.GateBinding, e = s.clientset.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(ctx, binding, metav1.CreateOptions{})
	if e != nil {
		return e
	}
	for {
		p, e := s.clientset.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, policy.Name, metav1.GetOptions{})
		if e != nil {
			return e
		}
		if p.UID != proof.GatePolicy.UID {
			return fmt.Errorf("fault policy replaced")
		}
		if p.Status.ObservedGeneration == p.Generation {
			if p.Status.TypeChecking != nil && len(p.Status.TypeChecking.ExpressionWarnings) > 0 {
				return fmt.Errorf("fault policy failed CEL type checking")
			}
			break
		}
		if err := waitCapacityPoll(ctx); err != nil {
			return err
		}
	}
	if err := waitCapacityPoll(ctx); err != nil {
		return err
	}
	proof.SchemaApproval, e = s.createFaultApproval(ctx, "schema", schemaObject)
	if e != nil {
		return e
	}
	refusal, e := s.waitFaultGateRefusal(ctx, policy.Name, proof.SchemaApproval.GetCreationTimestamp().Time)
	if e != nil {
		return e
	}
	proof.Archives = append(proof.Archives, refusal)
	// No Apply claim could be persisted behind the demonstrated gate. Suspend
	// before removing it, preserving the admitted pending approval as a live pin.
	p, a, e := s.exportAndSuspendTo(ctx, "schema", 0, round, filepath.Join(s.evidenceDir, "retention", "fault"))
	if p.target.uid != "" {
		paused = append(paused, p)
	}
	if a.Path != "" {
		proof.Maintenance.Archives = append(proof.Maintenance.Archives, a)
	}
	if e != nil {
		return e
	}
	migration, e := s.waitFaultResource(ctx, migrationResource, originals["migration"], approvalGateReady)
	if e != nil {
		return e
	}
	lockCtx, stopLock := context.WithTimeout(ctx, 90*time.Second)
	lockDone := make(chan error, 1)
	lockWaited := false
	defer func() {
		stopLock()
		if !lockWaited {
			select {
			case <-lockDone:
			case <-time.After(25 * time.Second):
				err = errors.Join(err, fmt.Errorf("database lock helper did not stop"))
			}
		}
	}()
	go func() { lockDone <- s.faultProbe(lockCtx, "lock", round, identityPath) }()
	lockPath := filepath.Join(s.evidenceDir, "retention-fault", "lock-ready.json")
	for {
		if _, e := os.Stat(lockPath); e == nil {
			break
		} else if !os.IsNotExist(e) {
			return e
		}
		select {
		case e := <-lockDone:
			lockWaited = true
			if e == nil {
				e = fmt.Errorf("lock finished before it was used")
			}
			return e
		default:
		}
		if e := waitCapacityPoll(ctx); e != nil {
			return e
		}
	}
	proof.MigrationApproval, e = s.createFaultApproval(ctx, "migration", migration)
	if e != nil {
		return e
	}
	for {
		select {
		case lockErr := <-lockDone:
			lockWaited = true
			return fmt.Errorf("database lock finished before a waiting Apply was observed: %v", lockErr)
		default:
		}
		current, e := s.dynamic.Resource(migrationResource).Namespace(migration.GetNamespace()).Get(lockCtx, migration.GetName(), metav1.GetOptions{})
		if e != nil {
			return e
		}
		claimed, e := approvedClaim(current, migration, proof.MigrationApproval)
		if e != nil {
			return e
		}
		if claimed != nil {
			op := claimed.Status.ActiveOperation
			job, e := s.clientset.BatchV1().Jobs(migration.GetNamespace()).Get(ctx, op.JobName, metav1.GetOptions{})
			if e != nil {
				return e
			}
			if e := jobclaim.Match(job, jobclaim.MigrationOperation(claimed, op)); e != nil {
				return e
			}
			waitingErr := s.faultProbe(lockCtx, "waiting", round, identityPath)
			if waitingErr == nil {
				proof.DispatchedMigration = claimed
				proof.ApplyJob = job
				break
			}
			var exit *exec.ExitError
			if !errors.As(waitingErr, &exit) || exit.ExitCode() != 3 {
				return waitingErr
			}
		}
		if e := waitCapacityPoll(ctx); e != nil {
			return e
		}
	}
	current, e := s.dynamic.Resource(migrationResource).Namespace(migration.GetNamespace()).Get(ctx, migration.GetName(), metav1.GetOptions{})
	if e != nil {
		return e
	}
	op := proof.DispatchedMigration.Status.ActiveOperation
	jobUID, _, _ := unstructured.NestedString(current.Object, "status", "activeOperation", "jobUID")
	if jobUID != string(op.JobUID) {
		return fmt.Errorf("Apply stopped before suspension")
	}
	// Retain the original claim and plan before changing the generation.
	a, e = writeRetentionEvidence(s.evidenceDir, "retention-fault-dispatched.json", map[string]any{"migration": current, "job": proof.ApplyJob})
	if e != nil {
		return e
	}
	proof.Archives = append(proof.Archives, a)
	spec, _, _ := unstructured.NestedMap(current.Object, "spec")
	spec["suspend"] = true
	p = pausedResource{target: batchTarget{family: "migration", resource: migrationResource, namespace: current.GetNamespace(), name: current.GetName(), uid: current.GetUID(), generation: current.GetGeneration() + 1, reference: s.migrationReference(0, round)}, spec: spec}
	paused = append(paused, p)
	current, e = s.patchFaultSpec(ctx, migrationResource, current, map[string]any{"suspend": true})
	if e != nil {
		return e
	}

	proof.SuspendedMigration, e = s.waitFaultResource(ctx, migrationResource, current, func(o *unstructured.Unstructured) bool {
		return faultUnresolved(proof.DispatchedMigration, o) && maintenanceSuspended(o, p)
	})
	if e != nil {
		return e
	}
	e = <-lockDone
	lockWaited = true
	if e != nil {
		return e
	}
	if e := s.recordFaultApplyResult(ctx, proof); e != nil {
		return e
	}
	path, digest, _, e := s.exportOwnedPlans(ctx, proof.SuspendedMigration, "migration", filepath.Join(s.evidenceDir, "retention", "fault"), "migration-original.json")
	if e != nil {
		return e
	}
	relative, _ := filepath.Rel(s.evidenceDir, path)
	proof.Maintenance.Archives = append(proof.Maintenance.Archives, retentionArchive{relative, digest})
	for _, family := range []string{"schema", "migration"} {
		for index := 1; index < 10; index++ {
			p, a, e := s.exportAndSuspendTo(ctx, family, index, round, filepath.Join(s.evidenceDir, "retention", "fault"))
			if p.target.uid != "" {
				paused = append(paused, p)
			}
			if a.Path != "" {
				proof.Maintenance.Archives = append(proof.Maintenance.Archives, a)
			}
			if e != nil {
				return e
			}
		}
	}
	if e := s.pruneMaintenance(ctx, paused, &proof.Maintenance); e != nil {
		return e
	}
	deletedControl := false
	for _, deletion := range proof.Maintenance.Deleted {
		deletedControl = deletedControl || deletion.Plan.UID == string(proof.ObsoleteSchemaPlanUID) && deletion.GarbageCollected
	}
	if !deletedControl {
		return fmt.Errorf("retention fault did not garbage-collect its unpinned control plan")
	}
	before, e := readFaultInventory(filepath.Join(s.evidenceDir, "retention/fault/before-prune.json"))
	if e != nil {
		return e
	}
	after, e := readFaultInventory(filepath.Join(s.evidenceDir, "retention/fault/after-prune.json"))
	if e != nil {
		return e
	}
	if e := faultEvidencePreserved(before, after, proof.SchemaApproval, proof.SuspendedMigration); e != nil {
		return e
	}
	if e := cleanupGate(ctx); e != nil {
		return e
	}
	recoveryStart := time.Now().UTC()
	targets, e := s.resumeMaintenance(ctx, paused)
	if e != nil {
		return e
	}
	recoveryCtx, stopRecovery := context.WithDeadline(ctx, recoveryStart.Add(s.load.Settle.Duration))
	defer stopRecovery()
	expectedSchema := schemaObject.DeepCopy()
	for _, target := range targets {
		if target.uid == schemaObject.GetUID() {
			expectedSchema.SetGeneration(target.generation)
		}
	}
	approved, e := s.recoverFaultApproval(recoveryCtx, proof, expectedSchema)
	if e != nil {
		return e
	}
	if e := s.waitBatch(recoveryCtx, targets, recoveryStart); e != nil {
		return e
	}
	proof.Maintenance.Resumed = true
	proof.Maintenance.FinishedAt = time.Now().UTC()
	proof.RecoveredMigration, e = s.dynamic.Resource(migrationResource).Namespace(migration.GetNamespace()).Get(ctx, migration.GetName(), metav1.GetOptions{})
	if e != nil {
		return e
	}
	unknown, found, _ := unstructured.NestedMap(proof.RecoveredMigration.Object, "status", "unresolvedRun")
	if found && len(unknown) > 0 {
		return fmt.Errorf("resumed migration remains unresolved")
	}
	if _, found := proof.RecoveredMigration.GetAnnotations()[ptah.UnresolvedRunAnnotation]; found {
		return fmt.Errorf("resolved migration retained unresolved metadata")
	}
	proof.RecoveredSchema, e = s.dynamic.Resource(schemaResource).Namespace(schemaObject.GetNamespace()).Get(ctx, schemaObject.GetName(), metav1.GetOptions{})
	if e != nil {
		return e
	}
	consumed, e := s.dynamic.Resource(schemaApprovalResource).Namespace(schemaObject.GetNamespace()).Get(ctx, approved.GetName(), metav1.GetOptions{})
	if e != nil {
		return e
	}
	conditions, _, _ := unstructured.NestedSlice(consumed.Object, "status", "conditions")
	accepted := false
	for _, raw := range conditions {
		c, _ := raw.(map[string]any)
		accepted = accepted || (c["type"] == "Consumed" && c["status"] == "True")
	}
	if consumed.GetUID() != approved.GetUID() || !accepted {
		return fmt.Errorf("resumed schema did not consume its exact recovery approval")
	}
	if proof.RecoverySchemaApproval != nil {
		proof.RecoverySchemaApproval = consumed
	}
	proof.SchemaApprovalAfterRecovery, e = s.dynamic.Resource(schemaApprovalResource).Namespace(schemaObject.GetNamespace()).Get(ctx, proof.SchemaApproval.GetName(), metav1.GetOptions{})
	if e != nil {
		return e
	}
	if proof.SchemaApprovalAfterRecovery.GetUID() != proof.SchemaApproval.GetUID() || !reflect.DeepEqual(proof.SchemaApprovalAfterRecovery.Object["spec"], proof.SchemaApproval.Object["spec"]) {
		return fmt.Errorf("recovery lost the retained approval identity or spec")
	}
	jobs, e := s.clientset.BatchV1().Jobs(migration.GetNamespace()).List(ctx, metav1.ListOptions{LabelSelector: "operator.ptah.run/migration=" + migration.GetName() + ",operator.ptah.run/operation=apply"})
	if e != nil {
		return e
	}
	if s.restartJobs == nil {
		return fmt.Errorf("retention fault has no continuous Job history")
	}
	records := s.restartJobs()
	for _, job := range jobs.Items {
		records = append(records, jobRecord{Namespace: job.Namespace, Name: job.Name, UID: string(job.UID), Created: job.CreationTimestamp.Time, Family: "migration", Resource: migration.GetName(), Operation: "apply"})
	}
	if e := faultNoReplay(records, proof.DispatchedMigration, proof.StartedAt); e != nil {
		return e
	}
	return s.checkpointDatabase(ctx, round, "retention-fault-after")
}

// Include the sampler's retained history: TTL deletion cannot hide a replay.
func faultNoReplay(jobs []jobRecord, migration *ptah.PtahMigration, start time.Time) error {
	if migration == nil || migration.Status.ActiveOperation == nil || start.IsZero() {
		return fmt.Errorf("retention fault has no original Apply claim")
	}
	op := migration.Status.ActiveOperation
	found := false
	for _, job := range jobs {
		if job.Namespace != migration.Namespace || job.Family != "migration" || job.Resource != migration.Name || job.Operation != "apply" {
			continue
		}
		if job.Created.Before(start) {
			continue
		}
		if job.UID == "" || job.UID != string(op.JobUID) || job.Name != op.JobName {
			return fmt.Errorf("migration Apply was replayed during retention recovery")
		}
		found = true
	}
	if !found {
		return fmt.Errorf("retention fault Job history omitted the original Apply")
	}
	return nil
}

func faultDisabledPlan(o *unstructured.Unstructured, previousUID string) bool {
	if !inactiveForMaintenance(o) || previousUID == "" {
		return false
	}
	policy, _, _ := unstructured.NestedString(o.Object, "spec", "policy", "apply")
	uid, _, _ := unstructured.NestedString(o.Object, "status", "plan", "uid")
	if policy != "Never" || uid == "" || uid == previousUID {
		return false
	}
	conditions, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	for _, raw := range conditions {
		c, _ := raw.(map[string]any)
		if c["type"] == "Ready" && c["status"] == "False" && c["reason"] == "ApplyDisabled" && c["observedGeneration"] == o.GetGeneration() {
			return true
		}
	}
	return false
}
