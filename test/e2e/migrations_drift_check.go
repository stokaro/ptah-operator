package e2e

import (
	"encoding/csv"
	"errors"
	"fmt"
	"slices"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/kubeletenv"
)

// driftFoundMessage opens the message of a run a drift check refused because
// the target's schema is not the one its applied migrations produce. The other
// two refusals, a check that could not decide and a shadow it could not use,
// open with other words.
const driftFoundMessage = "The drift check found "

// driftAppliedNothing closes every drift refusal's first sentence.
const driftAppliedNothing = ", so the run applied nothing"

// shadowVolume is the volume an operation Job mounts the shadow database's
// URL from when the migration asks for a drift check.
const shadowVolume = "shadow-database-url"

// driftCheckRefusedRun holds a migration's last run to the approved run of the
// Job named, refused by a drift check that found the schema changed: recorded
// as Failed, with no version applied and no statement failed, and with the
// message only that refusal writes. A run whose first migration failed also
// reads as Failed with nothing applied; it names the failure instead.
func driftCheckRefusedRun(status ptahv1alpha1.PtahMigrationStatus, jobUID string) error {
	run := status.LastRun
	switch {
	case run == nil:
		return errors.New("the migration has recorded no run")
	case !presentIs(string(run.JobUID), jobUID):
		return fmt.Errorf("the last run is Job %q, want the approved run %q", run.JobUID, jobUID)
	case run.Outcome != ptahv1alpha1.MigrationRunOutcomeFailed:
		return fmt.Errorf("the last run reads %s, want Failed", run.Outcome)
	case len(run.AppliedVersions) != 0:
		return fmt.Errorf("the last run applied %v, and a refused run applies nothing", run.AppliedVersions)
	case run.Failure != nil:
		return fmt.Errorf("the last run names failed version %d, and a refused run ran no statement", run.Failure.Version)
	case !strings.HasPrefix(run.Message, driftFoundMessage) || !strings.Contains(run.Message, driftAppliedNothing):
		return fmt.Errorf("the last run says %q, want the drift check's finding", run.Message)
	}
	return nil
}

// decisionAfterRun is a migration asking for a decision again once the run of
// the Job named has been recorded, and the plan it asks about. The plan can be
// the one the run carried out: a refused run leaves the history as the plan
// read it, and the approval it spent covers no second run.
func decisionAfterRun(status ptahv1alpha1.PtahMigrationStatus, jobUID string) (string, bool) {
	if status.LastRun == nil || !presentIs(string(status.LastRun.JobUID), jobUID) ||
		status.Plan == nil || status.Plan.Name == "" ||
		!conditionIs(status.Conditions, ptahv1alpha1.ConditionMigrationApprovalRequired, metav1.ConditionTrue, "AwaitingApproval") {
		return "", false
	}
	return status.Plan.Name, true
}

// driftCheckedRunApplied is the migration settled on a run other than the
// refused one that applied exactly the version the refusal held back, for its
// current generation, with nothing in flight.
func driftCheckedRunApplied(resource *ptahv1alpha1.PtahMigration, refusedJobUID string, version int64) error {
	status := resource.Status
	run := status.LastRun
	switch {
	case refusedJobUID == "":
		return errors.New("no refused run to tell the applied one from")
	case status.Phase != ptahv1alpha1.MigrationPhaseInSync:
		return fmt.Errorf("the migration is %s, want InSync", status.Phase)
	case resource.Generation < 1 || status.ObservedGeneration != resource.Generation:
		return fmt.Errorf("the status was written for generation %d, and the migration is at %d", status.ObservedGeneration, resource.Generation)
	case status.ActiveOperation != nil || status.UnresolvedRun != nil:
		return errors.New("the migration still has an operation or an unresolved run")
	case run == nil || run.Outcome != ptahv1alpha1.MigrationRunOutcomeApplied:
		return fmt.Errorf("the last run is %+v, want an applied run", run)
	case run.JobUID == "" || string(run.JobUID) == refusedJobUID:
		return fmt.Errorf("the applied run is Job %q, want a run other than the refused %q", run.JobUID, refusedJobUID)
	case !slices.Equal(run.AppliedVersions, []int64{version}):
		return fmt.Errorf("the run applied %v, want [%d]", run.AppliedVersions, version)
	}
	return nil
}

// driftedColumnSentence is the start and the end of the line the runner writes
// to the Pod log for the row's refused run: Ptah's own sentence about the
// drift, naming the column the row added.
const (
	driftedSentenceStart = reportedErrorPrefix + "the database schema differs from the schema its 2 applied migrations produce"
	driftedSentenceEnd   = "first: extra column e2e_migration_widgets.nickname"
)

// driftSentenceLogged holds the executor's Pod log of the refused run to the
// one line the runner writes from Ptah's error, which names the first
// difference: the message points a person at it.
func driftSentenceLogged(log []byte) error {
	reported := reportedErrors(log)
	switch {
	case len(reported) != 1:
		return fmt.Errorf("the Pod log holds %d reported errors, want 1", len(reported))
	case !strings.HasPrefix(reported[0], driftedSentenceStart) || !strings.HasSuffix(reported[0], driftedSentenceEnd):
		return fmt.Errorf("the reported error is %q, want Ptah's drift sentence naming the added column", reported[0])
	}
	return nil
}

// driftExcludeVariable is the variable an Apply Job hands its runner the
// selectors a drift check leaves out in, as one CSV record.
const driftExcludeVariable = "PTAH_DRIFT_EXCLUDE"

// carriesDriftExclude says whether a Job hands exactly one container exactly
// the selectors given, in that order, which is how the runner is asked to pass
// them to the check. It reads the list the runner's process reads, which is
// the Job's value after the kubelet expanded it. A Secret's value is taken as
// empty, since the list must name no variable at all.
func carriesDriftExclude(job *batchv1.Job, selectors ...string) bool {
	var carried [][]string
	for _, container := range job.Spec.Template.Spec.Containers {
		environment, err := kubeletenv.Resolve(container.Env, func(corev1.EnvVar) (string, bool, error) { return "", true, nil })
		if err != nil {
			return false
		}
		for _, variable := range environment {
			if variable.Name != driftExcludeVariable {
				continue
			}
			record, err := csv.NewReader(strings.NewReader(variable.Value)).Read()
			if err != nil {
				return false
			}
			carried = append(carried, record)
		}
	}
	return len(carried) == 1 && slices.Equal(carried[0], selectors)
}

// mountsShadow says whether a Job mounts the shadow database's URL from the
// Secret named, which is how the runner is asked to check for drift.
func mountsShadow(job *batchv1.Job, secret string) bool {
	return slices.ContainsFunc(job.Spec.Template.Spec.Volumes, func(volume corev1.Volume) bool {
		return volume.Name == shadowVolume && volume.Secret != nil && volume.Secret.SecretName == secret
	})
}
