package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// overloadWorkload is the probe outside the admitted profile: twice the fleet
// at once, held, then returned to the admitted count. It establishes behavior
// beyond the limits, not a larger admitted capacity.
type overloadWorkload struct {
	// Admitted is how many resources of each family remain after the return.
	Admitted int `json:"admitted"`
	// Recovery bounds fresh convergence of the remaining fleet after the return.
	Recovery duration `json:"recovery"`
}

func (o *overloadWorkload) validate(w workload) error {
	var problems []error
	if o.Admitted < 1 || w.Schemas != 2*o.Admitted || w.Migrations != 2*o.Admitted {
		problems = append(problems, errors.New("overload runs twice the admitted count of each family"))
	}
	if o.Recovery.Duration <= 0 {
		problems = append(problems, errors.New("overload.recovery has to be positive"))
	}
	if w.Soak != nil || w.ApprovalBacklog || w.DatabaseDelay != nil || w.ChangeBatch != 0 || w.Outage.Duration != 0 {
		problems = append(problems, errors.New("overload is a separate probe with no other change or fault"))
	}
	return errors.Join(problems...)
}

// overloadProof is what the report keeps beside the measured windows.
type overloadProof struct {
	Resources       int               `json:"resources"`
	Admitted        int               `json:"admittedPerFamily"`
	PeakRunningPods int               `json:"peakRunningOperationPods"`
	PeakPendingPods int               `json:"peakPendingOperationPods"`
	ManagersBefore  []managerProcess  `json:"managersBefore"`
	ManagersAfter   []managerProcess  `json:"managersAfter"`
	UnresolvedSeen  []string          `json:"unresolvedSeen"`
	ApplyJobs       map[string]int    `json:"applyJobs"`
	Retired         []retiredResource `json:"retired"`
	ReturnedAt      time.Time         `json:"returnedAt"`
	Recovery        string            `json:"recovery"`
	RecoverySeconds float64           `json:"recoverySeconds"`
	RecoveryBound   string            `json:"recoveryBound"`
}

// managerProcess identifies one manager container. A different UID, container
// or restart count between two readings is a restart the overload caused.
type managerProcess struct {
	Pod          string `json:"pod"`
	PodUID       string `json:"podUID"`
	ContainerID  string `json:"containerID"`
	RestartCount int32  `json:"restartCount"`
	LastReason   string `json:"lastTerminationReason,omitempty"`
}

type retiredResource struct {
	Family       string    `json:"family"`
	Namespace    string    `json:"namespace"`
	Name         string    `json:"name"`
	UID          string    `json:"uid"`
	ExportPath   string    `json:"exportPath"`
	ExportSHA256 string    `json:"exportSHA256"`
	DeletedAt    time.Time `json:"deletedAt"`
}

func (s *scenarios) expectedResources() int {
	if s.admittedOnly && s.load.Overload != nil {
		return 2 * s.load.Overload.Admitted
	}
	return s.load.Schemas + s.load.Migrations
}

func (s *scenarios) managerProcesses(ctx context.Context) ([]managerProcess, error) {
	pods, err := s.clientset.CoreV1().Pods(s.in.operatorNamespace).List(ctx, metav1.ListOptions{LabelSelector: s.in.managerSelector})
	if err != nil {
		return nil, err
	}
	var out []managerProcess
	for _, pod := range pods.Items {
		if len(pod.Status.ContainerStatuses) == 0 {
			return nil, fmt.Errorf("manager %s reports no container", pod.Name)
		}
		status := pod.Status.ContainerStatuses[0]
		process := managerProcess{Pod: pod.Name, PodUID: string(pod.UID), ContainerID: status.ContainerID, RestartCount: status.RestartCount}
		if last := status.LastTerminationState.Terminated; last != nil {
			process.LastReason = last.Reason
		}
		out = append(out, process)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pod < out[j].Pod })
	return out, nil
}

// sameManagers holds the overload to its first rule: no manager restarts,
// whether from an OOM kill or otherwise.
func sameManagers(before, after []managerProcess) error {
	if len(before) == 0 || len(before) != len(after) {
		return fmt.Errorf("the overload changed the manager set from %d to %d processes", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			return fmt.Errorf("manager %s restarted during the overload (%+v -> %+v)", before[i].Pod, before[i], after[i])
		}
		if after[i].LastReason == "OOMKilled" {
			return fmt.Errorf("manager %s was OOM killed", after[i].Pod)
		}
	}
	return nil
}

// unresolvedRecord names a resource carrying an Apply nobody could account for.
// Overload alone may not create one: excess concurrency is allowed to queue
// and refuse, not to lose track of SQL it started.
func unresolvedRecord(family string, item *unstructured.Unstructured) string {
	key := family + "/" + item.GetNamespace() + "/" + item.GetName()
	if family == "migration" {
		if value, exists, _ := unstructured.NestedFieldNoCopy(item.Object, "status", "unresolvedRun"); exists && value != nil {
			return key
		}
		if _, exists := item.GetAnnotations()[operatorv1alpha1.UnresolvedRunAnnotation]; exists {
			return key
		}
		return ""
	}
	outcome, _, _ := unstructured.NestedString(item.Object, "status", "pendingObservation", "outcome")
	if outcome == string(operatorv1alpha1.PendingObservationOutcomeUnknown) {
		return key
	}
	return ""
}

func (s *scenarios) unresolvedResources(ctx context.Context) ([]string, error) {
	var found []string
	selector := capacityLabel + "=" + s.load.Name
	for _, namespace := range workloadNamespaces(s.in.namespace, s.in.namespaces) {
		for _, family := range []struct {
			name     string
			resource schema.GroupVersionResource
		}{{"schema", schemaResource}, {"migration", migrationResource}} {
			list, err := s.dynamic.Resource(family.resource).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				return nil, err
			}
			for i := range list.Items {
				if key := unresolvedRecord(family.name, &list.Items[i]); key != "" {
					found = append(found, key)
				}
			}
		}
	}
	sort.Strings(found)
	return found, nil
}

// applyJobsPerResource counts the Apply Jobs each resource was given over the
// whole run. Neither family changes its input during the overload, so a second
// Apply for one resource is a replay. The Job's operation label is the claim's
// type, which is "apply" for both families.
func applyJobsPerResource(jobs []jobRecord) map[string]int {
	counts := map[string]int{}
	for _, job := range jobs {
		if job.Operation == "apply" {
			counts[job.Family+"/"+job.Namespace+"/"+job.Resource]++
		}
	}
	return counts
}

func replayedApplies(counts map[string]int) []string {
	var replayed []string
	for resource, count := range counts {
		if count > 1 {
			replayed = append(replayed, fmt.Sprintf("%s (%d)", resource, count))
		}
	}
	sort.Strings(replayed)
	return replayed
}

// overload holds the doubled fleet after its cold start, watching for a lost
// Apply and for a manager restart while the cluster carries twice the work.
func (s *scenarios) overload(ctx context.Context) error {
	proof := &overloadProof{Resources: s.load.Schemas + s.load.Migrations, Admitted: s.load.Overload.Admitted,
		RecoveryBound: s.load.Overload.Recovery.String(), ApplyJobs: map[string]int{}}
	s.overloadProof = proof
	before, err := s.managerProcesses(ctx)
	if err != nil {
		return err
	}
	proof.ManagersBefore = before
	start := time.Now().UTC()
	deadline := start.Add(s.load.SteadyState.Duration)
	seen := map[string]bool{}
	for {
		unresolved, err := s.unresolvedResources(ctx)
		if err != nil {
			return err
		}
		for _, key := range unresolved {
			if !seen[key] {
				seen[key] = true
				proof.UnresolvedSeen = append(proof.UnresolvedSeen, key)
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(pollEvery, time.Until(deadline))):
		}
	}
	after, err := s.managerProcesses(ctx)
	if err != nil {
		return err
	}
	proof.ManagersAfter = after
	for _, reading := range s.sampleSnapshot() {
		if reading.At.Before(start) {
			continue
		}
		proof.PeakRunningPods = max(proof.PeakRunningPods, reading.PodsRunning)
		proof.PeakPendingPods = max(proof.PeakPendingPods, reading.PodsPending)
	}
	s.mark("overload", start, map[string]string{"resources": fmt.Sprint(proof.Resources),
		"peakRunningPods": fmt.Sprint(proof.PeakRunningPods), "peakPendingPods": fmt.Sprint(proof.PeakPendingPods)})
	if err := sameManagers(before, after); err != nil {
		return err
	}
	if len(proof.UnresolvedSeen) > 0 {
		return fmt.Errorf("the overload left Apply outcomes unresolved: %v", proof.UnresolvedSeen)
	}
	return nil
}

// returnToProfile retires the upper half of each family, exporting their plans
// first, and measures fresh convergence of the admitted fleet that remains.
func (s *scenarios) returnToProfile(ctx context.Context) error {
	proof := s.overloadProof
	if proof == nil {
		return errors.New("the return to the profile has no overload to end")
	}
	admitted := s.load.Overload.Admitted
	for _, family := range []string{"schema", "migration"} {
		for index := admitted; index < 2*admitted; index++ {
			retired, err := s.retire(ctx, family, index)
			if err != nil {
				return err
			}
			proof.Retired = append(proof.Retired, retired)
		}
	}
	s.admittedOnly = true
	returned := time.Now().UTC()
	proof.ReturnedAt = returned
	converged, err := s.waitConverged(ctx, returned, nil)
	proof.Recovery = converged
	if err == nil {
		proof.RecoverySeconds = time.Since(returned).Seconds()
		if proof.RecoverySeconds > s.load.Overload.Recovery.Seconds() {
			err = fmt.Errorf("the admitted fleet recovered in %.3fs, beyond the %s bound", proof.RecoverySeconds, s.load.Overload.Recovery)
		}
	}
	proof.ApplyJobs = applyJobsPerResource(s.restartJobs())
	if replayed := replayedApplies(proof.ApplyJobs); len(replayed) > 0 {
		err = errors.Join(err, fmt.Errorf("resources were applied more than once: %v", replayed))
	}
	unresolved, readErr := s.unresolvedResources(ctx)
	err = errors.Join(err, readErr)
	if len(unresolved) > 0 {
		err = errors.Join(err, fmt.Errorf("the return left Apply outcomes unresolved: %v", unresolved))
	}
	s.mark("return to profile", returned, map[string]string{"converged": converged, "retired": fmt.Sprint(len(proof.Retired))})
	return err
}

// retire deletes one idle, converged resource of the doubled fleet. Its plans
// are exported before the owner goes, and the delete carries the UID and
// resourceVersion the export read.
func (s *scenarios) retire(ctx context.Context, family string, index int) (retiredResource, error) {
	resource, name := schemaResource, s.schemaName(index)
	if family == "migration" {
		resource, name = migrationResource, s.migrationName(index)
	}
	namespace := s.in.namespaceFor(index)
	objects := s.workloadWriter().Resource(resource).Namespace(namespace)
	for attempt := 0; ; attempt++ {
		original, err := objects.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return retiredResource{}, err
		}
		if _, err := churnReplacement(original, family, s.load.Name); err != nil {
			if waitErr := waitCapacityPoll(ctx); waitErr != nil {
				return retiredResource{}, fmt.Errorf("retire %s remains unsafe: %w (wait: %v)", name, err, waitErr)
			}
			continue
		}
		path, digest, _, err := s.exportOwnedPlans(ctx, original, family, filepath.Join(s.evidenceDir, "overload"), fmt.Sprintf("%s-%s-attempt-%d.json", family, name, attempt))
		if err != nil {
			return retiredResource{}, err
		}
		relative, err := filepath.Rel(s.evidenceDir, path)
		if err != nil || !filepath.IsLocal(relative) {
			return retiredResource{}, errors.New("overload export escaped its evidence directory")
		}
		uid, rv := original.GetUID(), original.GetResourceVersion()
		err = objects.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}})
		if apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return retiredResource{}, err
		}
		retired := retiredResource{Family: family, Namespace: namespace, Name: name, UID: string(uid), ExportPath: relative, ExportSHA256: digest, DeletedAt: time.Now().UTC()}
		for {
			current, err := objects.Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return retired, nil
			}
			if err != nil {
				return retired, err
			}
			if current.GetUID() != uid {
				return retired, fmt.Errorf("retire found a foreign replacement of %s", name)
			}
			if err := waitCapacityPoll(ctx); err != nil {
				return retired, err
			}
		}
	}
}
