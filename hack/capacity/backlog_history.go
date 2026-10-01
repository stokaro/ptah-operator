package main

import (
	"fmt"
	"reflect"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func backlogAdmissionMatches(request, admitted *unstructured.Unstructured, family string) bool {
	if request.GetNamespace() != admitted.GetNamespace() || request.GetName() != admitted.GetName() {
		return false
	}
	ref := "schemaRef"
	if family == "migration" {
		ref = "migrationRef"
	}
	for _, field := range []string{ref, "planRef", "planFingerprint"} {
		before, found, err := unstructured.NestedFieldNoCopy(request.Object, "spec", field)
		if err != nil || !found {
			return false
		}
		after, found, err := unstructured.NestedFieldNoCopy(admitted.Object, "spec", field)
		if err != nil || !found || !reflect.DeepEqual(before, after) {
			return false
		}
	}
	for _, path := range [][]string{{"approver", "username"}, {"approvedAt"}, {"mutationRequestUID"}} {
		value, found, err := unstructured.NestedString(admitted.Object, append([]string{"spec"}, path...)...)
		if err != nil || !found || value == "" {
			return false
		}
	}
	return true
}

// Return false only while the uninterrupted watch is still catching up to
// retained API versions. Wrong bindings and early/repeated Apply claims fail.
func validateBacklogHistory(proof approvalBacklogProof, histories []cycleHistory) (bool, error) {
	if proof.StartedAt.IsZero() || proof.FirstApprovalAt.IsZero() || len(proof.Members) != 10 || len(proof.After) != 20 {
		return false, fmt.Errorf("backlog history has an incomplete declared population")
	}
	seen := map[string]bool{}
	counts := map[string]int{}
	ready := true
	for _, m := range proof.Members {
		if m.Expected == nil || m.Approval == nil || m.Approval.GetUID() == "" || m.Approval.GetCreationTimestamp().Time.IsZero() {
			return false, fmt.Errorf("backlog member lacks its admitted identity")
		}
		uid := string(m.Expected.GetUID())
		if uid == "" || seen[uid] || (m.Family != "schema" && m.Family != "migration") {
			return false, fmt.Errorf("backlog repeats or misclassifies a member")
		}
		seen[uid] = true
		counts[m.Family]++
		var history *cycleHistory
		for i := range histories {
			h := &histories[i]
			if h.Family == m.Family && h.Namespace == m.Expected.GetNamespace() {
				if history != nil {
					return false, fmt.Errorf("backlog has duplicate watch histories")
				}
				history = h
			}
		}
		if history == nil || history.Error != "" || history.StartedAt.IsZero() || history.StartedAt.After(proof.StartedAt) || history.Cursor == "" {
			return false, fmt.Errorf("backlog lacks uninterrupted pre-update history")
		}
		var final *unstructured.Unstructured
		for _, o := range proof.After {
			if string(o.GetUID()) == uid {
				final = o
			}
		}
		if final == nil || final.GetResourceVersion() == "" || final.GetGeneration() != m.Expected.GetGeneration() {
			return false, fmt.Errorf("backlog lost its final resource reading")
		}
		patchSeen, finalSeen, dispatched := false, false, false
		claimID, jobUID := "", ""
		planName, _, _ := unstructured.NestedString(m.Approval.Object, "spec", "planRef", "name")
		planUID, _, _ := unstructured.NestedString(m.Approval.Object, "spec", "planRef", "uid")
		for _, r := range history.Readings {
			if r.UID != uid {
				continue
			}
			if r.Namespace != m.Expected.GetNamespace() || r.Name != m.Expected.GetName() {
				return false, fmt.Errorf("backlog watch identity disagrees with the fleet")
			}
			patchSeen = patchSeen || r.ResourceVersion == m.Expected.GetResourceVersion()
			finalSeen = finalSeen || r.ResourceVersion == final.GetResourceVersion()
			if r.Generation < m.Expected.GetGeneration() {
				continue
			}
			if r.Generation != m.Expected.GetGeneration() {
				return false, fmt.Errorf("backlog generation changed during approval")
			}
			op := r.Operation
			if op == nil || op.Type != "Apply" {
				continue
			}
			approvalRef, planRef := op.ApprovalRef, op.PlanRef
			if m.Family == "schema" {
				if r.SchemaPlan == nil {
					return false, fmt.Errorf("schema Apply has no same-version plan binding")
				}
				approvalRef, planRef = r.SchemaPlan.Approval, &r.SchemaPlan.ImmutableObjectReference
			}
			if op.ID == "" || op.JobName == "" || op.StartedAt.IsZero() || op.StartedAt.Before(m.Approval.GetCreationTimestamp().Time) || op.StartedAt.After(r.ReceivedAt) || approvalRef == nil || approvalRef.UID != m.Approval.GetUID() || approvalRef.Name != m.Approval.GetName() || planRef == nil || planUID == "" || planName == "" || string(planRef.UID) != planUID || planRef.Name != planName {
				return false, fmt.Errorf("backlog Apply does not bind the admitted approval and plan")
			}
			if claimID != "" && claimID != op.ID {
				return false, fmt.Errorf("backlog dispatched more than one Apply claim")
			}
			claimID = op.ID
			if op.JobUID != "" {
				if jobUID != "" && jobUID != op.JobUID {
					return false, fmt.Errorf("backlog claim changed its dispatched Job UID")
				}
				jobUID = op.JobUID
			}
			dispatched = dispatched || op.JobUID != ""
		}
		ready = ready && patchSeen && finalSeen && dispatched
	}
	if counts["schema"] != 5 || counts["migration"] != 5 {
		return false, fmt.Errorf("backlog did not retain five members of each family")
	}
	return ready, nil
}

func validateBacklogApplyJob(m backlogMember, op cycleOperation, job *batchv1.Job) error {
	if job == nil || m.Approval == nil || op.JobUID == "" || string(job.UID) != op.JobUID || job.Name != op.JobName || job.Namespace != m.Expected.GetNamespace() || job.CreationTimestamp.IsZero() || job.CreationTimestamp.Before(&metav1.Time{Time: op.StartedAt}) || job.Status.CompletionTime == nil || job.Status.CompletionTime.Before(&job.CreationTimestamp) || job.Status.Succeeded != 1 || job.Status.Active != 0 {
		return fmt.Errorf("backlog Apply Job lacks bound successful execution")
	}
	owned, complete := false, false
	kind := "PtahSchema"
	if m.Family == "migration" {
		kind = "PtahMigration"
	}
	for _, owner := range job.OwnerReferences {
		owned = owned || owner.UID == m.Expected.GetUID() && owner.Name == m.Expected.GetName() && owner.Kind == kind && owner.APIVersion == "operator.ptah.run/v1alpha1" && owner.Controller != nil && *owner.Controller
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == "True" {
			return fmt.Errorf("backlog Apply Job failed")
		}
		complete = complete || c.Type == batchv1.JobComplete && c.Status == "True"
	}
	if !owned || !complete {
		return fmt.Errorf("backlog Apply Job lacks its owner or completion")
	}
	return nil
}

// Admission stamps the decision before storage persists the object. Use the
// earlier server timestamp, preserving their second-level precision, so
// request/response latency cannot move the execution deadline forward.
func backlogAdmissionTime(admitted *unstructured.Unstructured, submitted, received time.Time) (time.Time, error) {
	raw, found, err := unstructured.NestedString(admitted.Object, "spec", "approvedAt")
	if err != nil || !found {
		return time.Time{}, fmt.Errorf("backlog approval has no admission timestamp")
	}
	approved, err := time.Parse(time.RFC3339Nano, raw)
	created := admitted.GetCreationTimestamp().Time
	if err != nil || approved.IsZero() || created.IsZero() || approved.Before(submitted.Truncate(time.Second)) || created.Before(submitted.Truncate(time.Second)) || approved.After(received) || created.After(received) {
		return time.Time{}, fmt.Errorf("backlog approval has an invalid server timestamp")
	}
	if created.Before(approved) {
		return created, nil
	}
	return approved, nil
}
