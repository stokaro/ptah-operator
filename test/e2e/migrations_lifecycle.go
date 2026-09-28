package e2e

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/migrationplan"
)

// The readings the main migration's lifecycle is held to: the approval gate
// before its first run, the plan a person approves, the approval admission
// stamped, the history the run left, and the reconciliation after it.

// awaitingApprovalGate is a migration that stopped at an approval gate over
// the three migrations of a database nothing has migrated: its artifact the
// published digest, an empty history of the current contract, a plan
// published, no run, and approval required without Ready.
func awaitingApprovalGate(migration *ptahv1alpha1.PtahMigration, digest string, stateVersion int32) error {
	status := migration.Status
	history := status.History
	switch {
	case status.Phase != ptahv1alpha1.MigrationPhaseAwaitingApproval:
		return fmt.Errorf("it is %s", status.Phase)
	case status.Artifact == nil || status.Artifact.Digest != digest:
		return errors.New("its artifact is not the published digest")
	case history == nil || history.ContractVersion != 1 || history.CurrentVersion != 0 || history.AppliedCount != 0 ||
		history.PendingCount != 3 || history.Dirty || len(history.ModifiedVersions) != 0:
		return fmt.Errorf("its history is not an empty contract-1 history with three pending: %+v", history)
	case !sha256Pattern.MatchString(history.Fingerprint) || !sha256Pattern.MatchString(history.TargetIdentityDigest):
		return errors.New("its history has no fingerprint or target identity")
	case status.ExecutionBinding == nil || status.ExecutionBinding.ControllerStateVersion != stateVersion:
		return errors.New("its execution binding is not this controller state")
	case status.Plan == nil || !strings.HasPrefix(status.Plan.Name, "ptah-mplan-"):
		return errors.New("it published no plan")
	case status.LastRun != nil:
		return errors.New("it records a run")
	case !conditionIs(status.Conditions, ptahv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionTrue, "AwaitingApproval"),
		!conditionStatus(status.Conditions, "ArtifactVerified", metav1.ConditionTrue),
		conditionStatus(status.Conditions, ptahv1alpha1.ConditionMigrationReady, metav1.ConditionTrue):
		return fmt.Errorf("its conditions are not an approval gate: %v", conditionSummary(status.Conditions))
	}
	return nil
}

// planSequence is the plan a person approves: the migration's, of the
// published digest and the realm's coordination digest, from version 0, the
// three migrations in order with checksums and no checkpoint, published by
// this manager.
func planSequence(plan *ptahv1alpha1.PtahMigrationPlan, migration, digest, coordinationDigest string,
	controller controllerIdentity, stateVersion int32,
) error {
	spec := plan.Spec
	var versions []int64
	for _, planned := range spec.Migrations {
		versions = append(versions, planned.Version)
		if planned.Checksum == "" || planned.Checkpoint {
			return fmt.Errorf("migration %d has no checksum or is a checkpoint", planned.Version)
		}
	}
	switch {
	case spec.ContractVersion != migrationplan.ContractVersion:
		return fmt.Errorf("it is contract version %d", spec.ContractVersion)
	case spec.MigrationRef.Name != migration:
		return fmt.Errorf("it belongs to %s", spec.MigrationRef.Name)
	case spec.ArtifactDigest != digest || spec.CoordinationDigest != coordinationDigest:
		return errors.New("it names another artifact or realm")
	case spec.CurrentVersion != 0:
		return fmt.Errorf("it starts at version %d", spec.CurrentVersion)
	case !sha256Pattern.MatchString(spec.Fingerprint) || !sha256Pattern.MatchString(spec.HistoryFingerprint):
		return errors.New("it has no fingerprint or history fingerprint")
	case !slices.Equal(versions, []int64{1, 2, 3}):
		return fmt.Errorf("it plans versions %v", versions)
	case spec.ControllerImage != controller.image || spec.ControllerStateVersion != stateVersion:
		return errors.New("it was not published by this manager")
	}
	return nil
}

// migrationApprovalStamped is an approval as stored: the decision as written
// -- the migration, the plan by name and UID, and the plan's fingerprint --
// plus who made it and when, and no field copied from the plan.
func migrationApprovalStamped(stored *unstructured.Unstructured, migration string, plan *ptahv1alpha1.PtahMigrationPlan) error {
	spec, found, err := unstructured.NestedMap(stored.Object, "spec")
	if err != nil || !found {
		return errors.New("it has no spec")
	}
	if keys := slices.Sorted(maps.Keys(spec)); !slices.Equal(keys,
		[]string{"approvedAt", "approver", "migrationRef", "mutationRequestUID", "planFingerprint", "planRef"}) {
		return fmt.Errorf("its spec carries %v", keys)
	}
	name, _, _ := unstructured.NestedString(spec, "migrationRef", "name")
	uid, _, _ := unstructured.NestedString(spec, "migrationRef", "uid")
	username, _, _ := unstructured.NestedString(spec, "approver", "username")
	request, _, _ := unstructured.NestedString(spec, "mutationRequestUID")
	switch {
	case name != migration || uid != string(plan.Spec.MigrationRef.UID):
		return fmt.Errorf("it approves %s UID %s", name, uid)
	case !sameJSON(spec["planRef"], map[string]any{"name": plan.Name, "uid": string(plan.UID)}):
		return fmt.Errorf("its planRef is %v", spec["planRef"])
	case spec["planFingerprint"] != plan.Spec.Fingerprint:
		return errors.New("it names another plan fingerprint")
	case username == "":
		return errors.New("no approver was stamped")
	case spec["approvedAt"] == nil:
		return errors.New("no approval time was stamped")
	case request == "":
		return errors.New("no mutation request was stamped")
	}
	return nil
}

// settledInSync is a migration whose history matches the published artifact
// after one Applied run of the three migrations: no plan, no operation, Ready
// by HistoryMatched, and neither approval required nor blocked.
func settledInSync(migration *ptahv1alpha1.PtahMigration, digest string) error {
	status := migration.Status
	history, run := status.History, status.LastRun
	switch {
	case status.Phase != ptahv1alpha1.MigrationPhaseInSync:
		return fmt.Errorf("it is %s", status.Phase)
	case status.Artifact == nil || status.Artifact.Digest != digest:
		return errors.New("its artifact is not the published digest")
	case history == nil || history.CurrentVersion != 3 || history.AppliedCount != 3 || history.PendingCount != 0 ||
		history.Dirty || len(history.ModifiedVersions) != 0:
		return fmt.Errorf("its history is not the three applied migrations: %+v", history)
	case run == nil || run.Outcome != ptahv1alpha1.MigrationRunOutcomeApplied:
		return errors.New("its last run did not apply")
	case !slices.Equal(slices.Sorted(slices.Values(run.AppliedVersions)), []int64{1, 2, 3}):
		return fmt.Errorf("its last run applied %v", run.AppliedVersions)
	case run.JobName == "" || run.JobUID == "" || run.FinishedAt == nil:
		return errors.New("its last run names no Job or never finished")
	case status.Plan != nil || status.ActiveOperation != nil:
		return errors.New("it still carries a plan or an operation")
	case !conditionIs(status.Conditions, ptahv1alpha1.ConditionMigrationReady, metav1.ConditionTrue, "HistoryMatched"),
		conditionStatus(status.Conditions, ptahv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionTrue),
		conditionStatus(status.Conditions, ptahv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue):
		return fmt.Errorf("its conditions are not a settled history: %v", conditionSummary(status.Conditions))
	}
	return nil
}

// reconciledWithoutWork is the migration after it read its history again:
// still InSync on the three migrations, the same last run, and no plan.
func reconciledWithoutWork(migration *ptahv1alpha1.PtahMigration, runUID string) error {
	status := migration.Status
	switch {
	case status.Phase != ptahv1alpha1.MigrationPhaseInSync:
		return fmt.Errorf("it is %s", status.Phase)
	case status.History == nil || status.History.CurrentVersion != 3 || status.History.PendingCount != 0:
		return errors.New("its history moved")
	case status.LastRun == nil || string(status.LastRun.JobUID) != runUID:
		return errors.New("it recorded another run")
	case status.Plan != nil:
		return errors.New("it published a plan")
	}
	return nil
}

// kubectlPtahMigrationView holds the plugin's view of a migration: its phase
// and name, no SQL, and the plan a reader approves in order while one waits,
// or the applied versions once the run consumed it.
func kubectlPtahMigrationView(view []byte, namespace, migration string, phase ptahv1alpha1.MigrationPhase, plan string) error {
	text := string(view)
	lines := strings.Split(text, "\n")
	switch {
	case !slices.Contains(lines, "Phase:          "+string(phase)):
		return fmt.Errorf("it does not report phase %s", phase)
	case !slices.ContainsFunc(lines, func(line string) bool {
		return strings.Contains(line, "Migration:      "+namespace+"/"+migration)
	}):
		return errors.New("it does not name the resource it read")
	// grep read the view a line at a time, so a statement is one that
	// starts and ends on the same line.
	case slices.ContainsFunc(lines, kubectlPtahSQL.MatchString):
		return errors.New("it printed SQL")
	}
	if phase == ptahv1alpha1.MigrationPhaseAwaitingApproval {
		var order []string
		for _, line := range lines {
			if match := viewOrderLine.FindStringSubmatch(line); match != nil {
				order = append(order, match[1])
			}
		}
		switch {
		case !strings.Contains(text, "Plan "+plan+", 3 migrations from version 0:"):
			return errors.New("it does not publish the plan a reader has to approve")
		case !slices.Equal(order, []string{"1", "2", "3"}):
			return fmt.Errorf("it printed the order as %v, not the planned sequence", order)
		case !slices.Contains(lines, "Pending:        3"):
			return errors.New("it does not report the pending count")
		}
		return nil
	}
	if !slices.Contains(lines, "No plan is published.") {
		return errors.New("it still shows a plan after the run that consumed it")
	}
	// The shell split every "Run applied:" line at its commas, removed the
	// spaces, sorted the fields as numbers and compared the result with
	// "1 2 3", so each field has to be spelled exactly as a version, and a
	// field that is no number at all makes the list another list.
	var applied []string
	for _, line := range lines {
		if match := viewAppliedLine.FindStringSubmatch(line); match != nil {
			applied = append(applied, strings.Split(strings.ReplaceAll(match[1], " ", ""), ",")...)
		}
	}
	for _, field := range applied {
		if _, err := strconv.Atoi(field); err != nil {
			return fmt.Errorf("it reports %q applied, which is not a version", field)
		}
	}
	slices.SortStableFunc(applied, func(a, b string) int {
		first, _ := strconv.Atoi(a)
		second, _ := strconv.Atoi(b)
		return cmp.Compare(first, second)
	})
	if !slices.Equal(applied, []string{"1", "2", "3"}) {
		return fmt.Errorf("it reports %v applied, not the three versions the run recorded", applied)
	}
	return nil
}

var (
	kubectlPtahSQL  = regexp.MustCompile(`(?i)create[[:space:]]+table|alter[[:space:]]+table|insert[[:space:]]+into|update[[:space:]]+e2e`)
	viewOrderLine   = regexp.MustCompile(`^  ([0-9]+) `)
	viewAppliedLine = regexp.MustCompile(`^Run applied: *(.*)$`)
)

// jobOperations is the operations the archived Jobs cover, sorted, joined.
func jobOperations(jobs []batchv1.Job) string {
	var operations []string
	for _, job := range jobs {
		operations = append(operations, job.Labels[labelOperation])
	}
	slices.Sort(operations)
	return strings.Join(slices.Compact(operations), ",")
}

// consumedPlanRefusal is the admission refusal of an approval that names a
// consumed plan. Which refusal fires depends on where the resource is in its
// cycle -- the plan is no longer current, the migration is not awaiting
// approval, or an operation is in flight -- and each names what it read.
var consumedPlanRefusal = regexp.MustCompile(`(?i)plan|approval|migration`)
