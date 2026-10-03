package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type backlogMember struct {
	ApplyJob         *batchv1.Job               `json:"applyJob,omitempty"`
	Family           string                     `json:"family"`
	Expected         *unstructured.Unstructured `json:"expected"`
	Gate             *unstructured.Unstructured `json:"gate,omitempty"`
	Approval         *unstructured.Unstructured `json:"approval,omitempty"`
	ConsumedApproval *unstructured.Unstructured `json:"consumedApproval,omitempty"`
}

type approvalBacklogProof struct {
	StartedAt       time.Time                    `json:"startedAt"`
	WaitingAt       time.Time                    `json:"waitingAt"`
	FirstApprovalAt time.Time                    `json:"firstApprovalAt"`
	FinishedAt      time.Time                    `json:"finishedAt"`
	Before          []*unstructured.Unstructured `json:"before"`
	After           []*unstructured.Unstructured `json:"after"`
	Members         []backlogMember              `json:"members"`
	Archives        []retentionArchive           `json:"archives"`
	Error           string                       `json:"error,omitempty"`
}

func backlogResource(family string) schema.GroupVersionResource {
	if family == "schema" {
		return schemaResource
	}
	return migrationResource
}

func backlogApprovalResource(family string) schema.GroupVersionResource {
	if family == "schema" {
		return schemaApprovalResource
	}
	return approvalGVR
}

func backlogGate(family string) func(*unstructured.Unstructured) bool {
	if family == "schema" {
		return schemaGateReady
	}
	return approvalGateReady
}

// Read both namespaces and both families. A point lookup alone would miss an
// accidentally added twenty-first resource and understate the workload cost.
func (s *scenarios) backlogFleet(ctx context.Context) ([]*unstructured.Unstructured, error) {
	var fleet []*unstructured.Unstructured
	for _, ns := range workloadNamespaces(s.in.namespace, s.in.namespaces) {
		for _, resource := range []schema.GroupVersionResource{schemaResource, migrationResource} {
			list, err := s.dynamic.Resource(resource).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: capacityLabel + "=" + s.load.Name})
			if err != nil {
				return nil, err
			}
			for i := range list.Items {
				fleet = append(fleet, list.Items[i].DeepCopy())
			}
		}
	}
	if len(fleet) != 20 {
		return nil, fmt.Errorf("approval backlog has %d resources, need twenty", len(fleet))
	}
	seen := map[string]bool{}
	for _, family := range []string{"schema", "migration"} {
		for index := range 10 {
			name, kind := s.schemaName(index), "PtahSchema"
			if family == "migration" {
				name, kind = s.migrationName(index), "PtahMigration"
			}
			matches := 0
			for _, o := range fleet {
				if o.GetKind() != kind || o.GetName() != name || o.GetNamespace() != s.in.namespaceFor(index) {
					continue
				}
				uid := string(o.GetUID())
				suspended, _, err := unstructured.NestedBool(o.Object, "spec", "suspend")
				if err != nil || suspended || o.GetDeletionTimestamp() != nil || uid == "" || seen[uid] || o.GetResourceVersion() == "" || o.GetGeneration() < 1 {
					return nil, fmt.Errorf("approval backlog has an invalid or repeated resource identity")
				}
				seen[uid] = true
				matches++
			}
			if matches != 1 {
				return nil, fmt.Errorf("approval backlog lacks exactly one %s/%s", family, name)
			}
		}
	}
	return fleet, nil
}

func unchangedBacklogFleet(expected, current []*unstructured.Unstructured) error {
	if len(expected) != 20 || len(current) != 20 {
		return fmt.Errorf("approval backlog lost its twenty-resource fleet")
	}
	seen := map[string]bool{}
	for _, before := range expected {
		matched := false
		for _, after := range current {
			if before.GetKind() != after.GetKind() || before.GetNamespace() != after.GetNamespace() || before.GetName() != after.GetName() {
				continue
			}
			uid := string(after.GetUID())
			if uid == "" || seen[uid] || before.GetUID() != after.GetUID() || before.GetGeneration() != after.GetGeneration() || after.GetDeletionTimestamp() != nil || !reflect.DeepEqual(before.Object["spec"], after.Object["spec"]) {
				return fmt.Errorf("approval backlog changed a fleet member's identity or specification")
			}
			seen[uid], matched = true, true
		}
		if !matched {
			return fmt.Errorf("approval backlog omitted a fleet member")
		}
	}
	return nil
}

func (s *scenarios) archiveBacklog(ctx context.Context, proof *approvalBacklogProof, name string) (retentionInventory, error) {
	inventory, err := s.retentionInventory(ctx)
	if err != nil {
		return inventory, err
	}
	if err = verifyRetentionPayloads(ctx, inventory); err != nil {
		return inventory, err
	}
	archive, err := writeRetentionEvidence(s.evidenceDir, "approval-backlog/"+name+".json", inventory)
	if err == nil {
		proof.Archives = append(proof.Archives, archive)
	}
	return inventory, err
}

func (s *scenarios) approvalBacklog(ctx context.Context) (err error) {
	if !s.load.ApprovalBacklog || s.load.Soak != nil || s.in.catalog == nil || s.checkpoint == nil || len(s.recorders) == 0 {
		return fmt.Errorf("approval backlog requires its separate populated workload, database probe and continuous watches")
	}
	ctx, cancelRun := context.WithTimeout(ctx, 4*s.load.Settle.Duration+s.load.Interval.Duration)
	defer cancelRun()
	proof := &approvalBacklogProof{StartedAt: time.Now().UTC()}
	s.backlogProof = proof
	defer func() {
		proof.FinishedAt = time.Now().UTC()
		if err != nil {
			proof.Error = err.Error()
		}
		archive, e := writeRetentionEvidence(s.evidenceDir, "approval-backlog/outcome.json", proof)
		if e == nil {
			proof.Archives = append(proof.Archives, archive)
		}
		err = errors.Join(err, e)
		if err != nil {
			proof.Error = err.Error()
		}
		outcome := map[string]string{}
		if err != nil {
			outcome["error"] = err.Error()
		}
		preparationEnd := proof.WaitingAt
		if preparationEnd.IsZero() {
			preparationEnd = proof.FinishedAt
		}
		s.windows = append(s.windows, window{Name: "approval backlog preparation", Start: proof.StartedAt, End: preparationEnd, Outcome: outcome})
		if !proof.WaitingAt.IsZero() {
			waitingEnd := proof.FirstApprovalAt
			if waitingEnd.IsZero() {
				waitingEnd = proof.FinishedAt
			}
			s.windows = append(s.windows, window{Name: "approval backlog waiting", Start: proof.WaitingAt, End: waitingEnd, Outcome: outcome})
		}
		if !proof.FirstApprovalAt.IsZero() {
			outcome = map[string]string{}
			if err != nil {
				outcome["error"] = err.Error()
			}
			if err == nil {
				outcome["databaseVerifiedConvergence"] = proof.FinishedAt.Sub(proof.FirstApprovalAt).String()
			} else {
				outcome["elapsed"] = proof.FinishedAt.Sub(proof.FirstApprovalAt).String()
			}
			s.mark("approval backlog execution", proof.FirstApprovalAt, outcome)
		}
	}()
	proof.Before, err = s.backlogFleet(ctx)
	if err != nil {
		return err
	}
	if err = s.checkpointDatabase(ctx, 0, "backlog-before-update"); err != nil {
		return err
	}
	expected := append([]*unstructured.Unstructured(nil), proof.Before...)
	preparation, cancel := context.WithTimeout(ctx, s.load.Settle.Duration)
	defer cancel()
	for _, family := range []string{"schema", "migration"} {
		for index := range 5 {
			name, field, reference := s.schemaName(index), "desired", s.schemaReference(index, 1)
			if family == "migration" {
				name, field, reference = s.migrationName(index), "artifact", s.migrationReference(index, 1)
			}
			resource := backlogResource(family)
			for i, original := range expected {
				if original.GetName() != name || original.GetNamespace() != s.in.namespaceFor(index) {
					continue
				}
				current, e := s.dynamic.Resource(resource).Namespace(original.GetNamespace()).Get(preparation, name, metav1.GetOptions{})
				if e != nil {
					return e
				}
				if current.GetUID() != original.GetUID() || current.GetGeneration() != original.GetGeneration() || !reflect.DeepEqual(current.Object["spec"], original.Object["spec"]) {
					return fmt.Errorf("backlog input changed before update")
				}
				updated, e := s.patchFaultSpec(preparation, resource, current, map[string]any{"policy": map[string]any{"apply": "OnApproval"}, field: map[string]any{"ociRef": reference}})
				if e != nil {
					return e
				}
				expected[i] = updated
				proof.Members = append(proof.Members, backlogMember{Family: family, Expected: updated})
			}
		}
	}
	if len(proof.Members) != 10 {
		return fmt.Errorf("approval backlog did not update five resources of each family")
	}
	for i := range proof.Members {
		m := &proof.Members[i]
		m.Gate, err = s.waitFaultResource(preparation, backlogResource(m.Family), m.Expected, backlogGate(m.Family))
		if err != nil {
			return err
		}
	}
	proof.WaitingAt = time.Now().UTC()
	// Human waiting is a separate window. Leave all resources refreshing for
	// a complete configured interval, retaining their plans and observations.
	if _, err = s.archiveBacklog(ctx, proof, "waiting"); err != nil {
		return err
	}
	if err = waitUntil(ctx, proof.WaitingAt.Add(s.load.Interval.Duration)); err != nil {
		return err
	}
	if err = s.checkpointDatabase(ctx, 0, "backlog-still-unapproved"); err != nil {
		return err
	}
	current, err := s.backlogFleet(ctx)
	if err != nil {
		return err
	}
	if err = unchangedBacklogFleet(expected, current); err != nil {
		return err
	}
	if _, err = s.archiveBacklog(ctx, proof, "before-approval"); err != nil {
		return err
	}

	// Fresh gate reads bind approvals to the plan current at admission. Each
	// approval is retained; an ambiguous or rejected creation fails the run.
	var execution context.Context
	for i := range proof.Members {
		m := &proof.Members[i]
		admitCtx := execution
		if admitCtx == nil {
			var stop context.CancelFunc
			admitCtx, stop = context.WithTimeout(ctx, s.load.Settle.Duration)
			defer stop()
		}
		m.Gate, err = s.waitFaultResource(admitCtx, backlogResource(m.Family), m.Expected, backlogGate(m.Family))
		if err != nil {
			return err
		}
		approval, e := s.approvalForResource(admitCtx, m.Family, m.Gate)
		if e != nil {
			return e
		}
		approval.SetName("capacity-backlog-" + string(m.Expected.GetUID()))
		submitted := time.Now().UTC()
		m.Approval, err = s.dynamic.Resource(backlogApprovalResource(m.Family)).Namespace(m.Expected.GetNamespace()).Create(admitCtx, approval, metav1.CreateOptions{})
		if err != nil {
			return err
		}
		at, admissionErr := backlogAdmissionTime(m.Approval, submitted, time.Now().UTC())
		if admissionErr != nil {
			return admissionErr
		}
		if m.Approval.GetUID() == "" || !backlogAdmissionMatches(approval, m.Approval, m.Family) {
			return fmt.Errorf("backlog approval lacks an unambiguous admission identity")
		}
		if proof.FirstApprovalAt.IsZero() {
			proof.FirstApprovalAt = at
			executionContext, cancelExecution := context.WithDeadline(ctx, at.Add(s.load.Settle.Duration))
			defer cancelExecution()
			execution = executionContext
		}
	}
	var targets []batchTarget
	for i, m := range proof.Members {
		ref := s.schemaReference(i%5, 1)
		if m.Family == "migration" {
			ref = s.migrationReference(i%5, 1)
		}
		targets = append(targets, batchTarget{family: m.Family, resource: backlogResource(m.Family), namespace: m.Expected.GetNamespace(), name: m.Expected.GetName(), uid: m.Expected.GetUID(), generation: m.Expected.GetGeneration(), reference: ref, applied: m.Family == "schema"})
	}
	if err = s.waitBatch(execution, targets, proof.FirstApprovalAt); err != nil {
		return err
	}
	if err = s.checkpointDatabase(execution, 1, "backlog-after-approval"); err != nil {
		return err
	}
	if err = s.verifyInputPlans(execution, 1); err != nil {
		return err
	}
	proof.After, err = s.backlogFleet(execution)
	if err != nil {
		return err
	}
	if err = unchangedBacklogFleet(expected, proof.After); err != nil {
		return err
	}
	for i := range proof.Members {
		m := &proof.Members[i]
		m.ConsumedApproval, err = s.dynamic.Resource(backlogApprovalResource(m.Family)).Namespace(m.Approval.GetNamespace()).Get(execution, m.Approval.GetName(), metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err = consumedBacklogApproval(*m); err != nil {
			return err
		}
	}
	if _, err = s.archiveBacklog(execution, proof, "completed"); err != nil {
		return err
	}
	if err = s.waitBacklogHistory(execution, proof); err != nil {
		return err
	}
	return execution.Err()
}

func consumedBacklogApproval(m backlogMember) error {
	a, current := m.Approval, m.ConsumedApproval
	if a == nil || current == nil || a.GetUID() == "" || a.GetUID() != current.GetUID() || a.GetName() != current.GetName() || a.GetNamespace() != current.GetNamespace() || current.GetDeletionTimestamp() != nil || !reflect.DeepEqual(a.Object["spec"], current.Object["spec"]) {
		return fmt.Errorf("backlog lost the admitted approval")
	}
	conditions, _, err := unstructured.NestedSlice(current.Object, "status", "conditions")
	if err != nil {
		return err
	}
	for _, raw := range conditions {
		condition, _ := raw.(map[string]any)
		if condition["type"] == "Consumed" && condition["status"] == "True" {
			return nil
		}
	}
	return fmt.Errorf("backlog approval was not consumed")
}

// Wait until each final API reading has reached its continuous watch. This
// barrier prevents an Apply hidden in a delayed event from escaping the audit.
func (s *scenarios) waitBacklogHistory(ctx context.Context, proof *approvalBacklogProof) error {
	for {
		var histories []cycleHistory
		for _, r := range s.recorders {
			histories = append(histories, r.snapshot())
		}
		ready, err := validateBacklogHistory(*proof, histories)
		if err != nil {
			return err
		}
		if ready {
			for i := range proof.Members {
				m := &proof.Members[i]
				var op *cycleOperation
				for _, h := range histories {
					for _, r := range h.Readings {
						if r.UID == string(m.Expected.GetUID()) && r.Generation == m.Expected.GetGeneration() && r.Operation != nil && r.Operation.Type == "Apply" && r.Operation.JobUID != "" {
							op = r.Operation
						}
					}
				}
				if op == nil {
					return fmt.Errorf("backlog lost its dispatched claim")
				}
				job, err := s.clientset.BatchV1().Jobs(m.Expected.GetNamespace()).Get(ctx, op.JobName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				if err = validateBacklogApplyJob(*m, *op, job); err != nil {
					return err
				}
				m.ApplyJob = job
			}
			return nil
		}
		if err := waitCapacityPoll(ctx); err != nil {
			return fmt.Errorf("backlog watch did not reach its final readings: %w", err)
		}
	}
}
