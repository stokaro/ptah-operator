package e2e

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/lifecyclejournal"
)

func TestApprovalJournaledOnceRefusesEachClause(t *testing.T) {
	t.Parallel()
	accepted := func(name, uid, plan string) ptahv1alpha1.JournalEntry {
		return ptahv1alpha1.JournalEntry{Type: ptahv1alpha1.JournalApprovalAccepted, Approval: name, ApprovalUID: types.UID(uid), Plan: plan}
	}
	for _, test := range []struct {
		name    string
		journal *ptahv1alpha1.LifecycleJournal
		want    string
	}{
		{"the decision's entry", &ptahv1alpha1.LifecycleJournal{Entries: []ptahv1alpha1.JournalEntry{accepted("approve", "uid-a", "plan-a")}}, ""},
		{"an earlier decision under the same name beside it", &ptahv1alpha1.LifecycleJournal{Entries: []ptahv1alpha1.JournalEntry{
			accepted("approve", "uid-old", "plan-old"), accepted("approve", "uid-a", "plan-a"),
		}}, ""},
		{"no journal", nil, "keeps no journal"},
		{"no entry", &ptahv1alpha1.LifecycleJournal{}, "holds 0"},
		{"an entry that lost its UID", &ptahv1alpha1.LifecycleJournal{Entries: []ptahv1alpha1.JournalEntry{accepted("approve", "", "plan-a")}}, "holds 0"},
		{"another decision under the name", &ptahv1alpha1.LifecycleJournal{Entries: []ptahv1alpha1.JournalEntry{accepted("approve", "uid-old", "plan-a")}}, "holds 0"},
		{"the decision twice", &ptahv1alpha1.LifecycleJournal{Entries: []ptahv1alpha1.JournalEntry{
			accepted("approve", "uid-a", "plan-a"), accepted("approve", "uid-a", "plan-a"),
		}}, "holds 2"},
		{"another type carrying the UID", &ptahv1alpha1.LifecycleJournal{Entries: []ptahv1alpha1.JournalEntry{
			{Type: ptahv1alpha1.JournalRunFinished, Approval: "approve", ApprovalUID: "uid-a", Plan: "plan-a"},
		}}, "holds 0"},
		{"another name on the UID", &ptahv1alpha1.LifecycleJournal{Entries: []ptahv1alpha1.JournalEntry{accepted("other", "uid-a", "plan-a")}}, "names approval"},
		{"another plan on the UID", &ptahv1alpha1.LifecycleJournal{Entries: []ptahv1alpha1.JournalEntry{accepted("approve", "uid-a", "plan-b")}}, "names approval"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := approvalJournaledOnce(test.journal, "approve", "uid-a", "plan-a")
			switch {
			case test.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)):
				t.Fatalf("error = %v, want one naming %q", err, test.want)
			}
		})
	}
}

func TestEveryApprovalJournaledByUIDProvesItReadOne(t *testing.T) {
	t.Parallel()
	accepted := func(uid, plan string) ptahv1alpha1.JournalEntry {
		return ptahv1alpha1.JournalEntry{Type: ptahv1alpha1.JournalApprovalAccepted, Approval: "approve", ApprovalUID: types.UID(uid), Plan: plan}
	}
	for _, test := range []struct {
		name    string
		journal *ptahv1alpha1.LifecycleJournal
		want    string
	}{
		{"one with its UID", &ptahv1alpha1.LifecycleJournal{Entries: []ptahv1alpha1.JournalEntry{accepted("uid-a", "plan-a")}}, ""},
		{"another plan's entry with none", &ptahv1alpha1.LifecycleJournal{Entries: []ptahv1alpha1.JournalEntry{
			accepted("", "plan-old"), accepted("uid-a", "plan-a"),
		}}, ""},
		{"no journal", nil, "keeps no journal"},
		{"no approval of the plan", &ptahv1alpha1.LifecycleJournal{Entries: []ptahv1alpha1.JournalEntry{accepted("uid-a", "plan-old")}}, "records no approval"},
		{"one with no UID", &ptahv1alpha1.LifecycleJournal{Entries: []ptahv1alpha1.JournalEntry{accepted("uid-a", "plan-a"), accepted("", "plan-a")}}, "with no UID"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := everyApprovalJournaledByUID(test.journal, "plan-a")
			switch {
			case test.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)):
				t.Fatalf("error = %v, want one naming %q", err, test.want)
			}
		})
	}
}

func TestApprovedApplyJournaledReadsThePlansLifeInOrder(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 24, 7, 0, 0, 0, time.UTC)
	entry := func(kind ptahv1alpha1.JournalEntryType, plan string, minute int, outcome ptahv1alpha1.JournalOutcome) ptahv1alpha1.JournalEntry {
		return ptahv1alpha1.JournalEntry{Type: kind, Plan: plan, At: metav1.NewTime(start.Add(time.Duration(minute) * time.Minute)), Outcome: outcome}
	}
	apply := func(plan string, minute int, outcome ptahv1alpha1.JournalOutcome, started int) ptahv1alpha1.JournalEntry {
		finished := entry(ptahv1alpha1.JournalApplyFinished, plan, minute, outcome)
		began := metav1.NewTime(start.Add(time.Duration(started) * time.Minute))
		finished.StartedAt = &began
		return finished
	}
	life := []ptahv1alpha1.JournalEntry{
		entry(ptahv1alpha1.JournalPlanPublished, "plan-a", 1, ""),
		entry(ptahv1alpha1.JournalApprovalAccepted, "plan-a", 2, ""),
		apply("plan-a", 4, ptahv1alpha1.JournalSucceeded, 3),
		entry(ptahv1alpha1.JournalVerified, "plan-a", 5, ptahv1alpha1.JournalInSync),
	}
	with := func(entries ...ptahv1alpha1.JournalEntry) *ptahv1alpha1.LifecycleJournal {
		return &ptahv1alpha1.LifecycleJournal{Entries: entries}
	}
	if err := approvedApplyJournaled(with(life...), "plan-a"); err != nil {
		t.Fatalf("a plan's whole life was refused: %v", err)
	}
	earlier := append([]ptahv1alpha1.JournalEntry{entry(ptahv1alpha1.JournalPlanPublished, "plan-old", 0, "")}, life...)
	if err := approvedApplyJournaled(with(earlier...), "plan-a"); err != nil {
		t.Fatalf("another plan's entry before it was refused: %v", err)
	}
	approval := func(name string, minute int) ptahv1alpha1.JournalEntry {
		accepted := entry(ptahv1alpha1.JournalApprovalAccepted, "plan-a", minute, "")
		accepted.Approval = name
		return accepted
	}
	// The unsupported-runner row: the first approval is spent on an Apply the
	// runner refused, whose outcome nobody can prove, and a fresh approval of
	// the same plan applies it. The operator puts the plan back before the
	// proof settles, in this order, as the Kubernetes 1.35 schema-approvals
	// job 120972718286 of run 40123087447 printed it.
	respent := []ptahv1alpha1.JournalEntry{
		entry(ptahv1alpha1.JournalPlanPublished, "plan-a", 1, ""),
		approval("old", 2),
		apply("plan-a", 4, ptahv1alpha1.JournalUnknown, 3),
		entry(ptahv1alpha1.JournalPlanPublished, "plan-a", 5, ""),
		entry(ptahv1alpha1.JournalVerified, "plan-a", 6, ptahv1alpha1.JournalDriftRemains),
		approval("current", 7),
		apply("plan-a", 9, ptahv1alpha1.JournalSucceeded, 8),
		entry(ptahv1alpha1.JournalVerified, "plan-a", 10, ptahv1alpha1.JournalInSync),
	}
	if err := approvedApplyJournaled(with(respent...), "plan-a"); err != nil {
		t.Fatalf("a plan applied under a fresh approval after an unproven Apply was refused: %v", err)
	}
	// The source moved to another plan and back before anyone approved.
	returned := append([]ptahv1alpha1.JournalEntry{
		entry(ptahv1alpha1.JournalPlanPublished, "plan-a", 0, ""),
		entry(ptahv1alpha1.JournalPlanPublished, "plan-b", 0, ""),
	}, life...)
	if err := approvedApplyJournaled(with(returned...), "plan-a"); err != nil {
		t.Fatalf("a plan published again after another replaced it was refused: %v", err)
	}
	for _, test := range []struct {
		name    string
		journal *ptahv1alpha1.LifecycleJournal
		want    string
	}{
		{"no journal", nil, "keeps no journal"},
		{"no proof", with(life[:3]...), "stops before Verified"},
		{"another plan's proof", with(append(life[:3:3], entry(ptahv1alpha1.JournalVerified, "plan-b", 5, ptahv1alpha1.JournalInSync))...), "stops before Verified"},
		{"out of order", with(life[0], life[2], life[1], life[3]), "stops before"},
		{"an Apply of unknown outcome", with(life[0], life[1], apply("plan-a", 4, ptahv1alpha1.JournalUnknown, 3), life[3]), `as "Unknown"`},
		{"a proof that found drift", with(life[0], life[1], life[2], entry(ptahv1alpha1.JournalVerified, "plan-a", 5, ptahv1alpha1.JournalDriftRemains)), `as "DriftRemains"`},
		{"an Apply that ended before it began", with(life[0], life[1], apply("plan-a", 4, ptahv1alpha1.JournalSucceeded, 6), life[3]), "did not start before"},
		{"an unproven Apply under the last approval, retried without one", with(respent[0], respent[1], respent[2], respent[6], respent[7]), `as "Unknown"`},
		{"a plan published again after an unproven Apply, applied without a fresh approval", with(respent[0], respent[1], respent[2], respent[3], respent[4], respent[6], respent[7]), `as "Unknown"`},
		{"an approval of a plan never published", with(life[1], life[2], life[3]), "never published"},
		{"no approval", with(life[0], life[2], life[3]), "stops before ApprovalAccepted"},
		{"a fresh approval nothing applied", with(append(respent[:6:6], respent[7])...), "stops before ApplyFinished"},
		{"a plan published again after its Apply succeeded", with(life[0], life[1], life[2], life[0], life[3]), "nothing carried out or replaced since"},
		{"a plan published again before its approval", with(life[0], life[0], life[1], life[2], life[3]), "nothing carried out or replaced since"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := approvedApplyJournaled(test.journal, "plan-a")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("approvedApplyJournaled() = %v, want an error naming %q", err, test.want)
			}
		})
	}
}

// The journal a 0.2.7 development build kept for a schema it applied twice,
// read from a cluster: every Apply was followed by the plan's publication
// again, dated when the plan was created. The filter refuses that reading,
// and admits it once the repeated publications are left out.
func TestApprovedApplyJournaledRefusesThePlanItsApplyPublishedAgain(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile("../../testdata/e2e/readings/journal-plan-published-again.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema ptahv1alpha1.PtahSchema
	if err := json.Unmarshal(body, &schema); err != nil {
		t.Fatal(err)
	}
	plan := schema.Status.Applied.PlanRef.Name
	journal := schema.Status.Journal
	if err := approvedApplyJournaled(journal, plan); err == nil || !strings.Contains(err.Error(), "nothing carried out or replaced since") {
		t.Fatalf("approvedApplyJournaled(reading) = %v, want the repeated publication refused", err)
	}
	once := journal.DeepCopy()
	once.Entries = nil
	seen := map[string]bool{}
	repeated := 0
	for _, entry := range journal.Entries {
		if entry.Type == ptahv1alpha1.JournalPlanPublished && seen[entry.Plan] {
			repeated++
			continue
		}
		seen[entry.Plan] = entry.Type == ptahv1alpha1.JournalPlanPublished || seen[entry.Plan]
		once.Entries = append(once.Entries, entry)
	}
	// Each of the three applied plans was published a second time.
	if repeated != 3 {
		t.Fatalf("the reading repeats %d publications, want 3", repeated)
	}
	if err := approvedApplyJournaled(once, plan); err != nil {
		t.Fatalf("approvedApplyJournaled(reading without the repeats) = %v", err)
	}
}

// appliedObjects are the plan, the approval and the Job of one approved Apply, as
// a row reads them from the cluster.
func appliedObjects() (*ptahv1alpha1.PtahSchemaPlan, *ptahv1alpha1.PtahSchemaApproval, *batchv1.Job) {
	plan := &ptahv1alpha1.PtahSchemaPlan{
		ObjectMeta: metav1.ObjectMeta{Name: "ptah-plan-index", UID: "plan-uid"},
		Spec:       ptahv1alpha1.PtahSchemaPlanSpec{Fingerprint: "sha256:" + strings.Repeat("f", 64)},
	}
	approval := &ptahv1alpha1.PtahSchemaApproval{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-configmap-refused-v2", UID: "approval-uid"},
		Spec:       ptahv1alpha1.PtahSchemaApprovalSpec{Approver: ptahv1alpha1.ApprovalIdentity{Username: "kubernetes-admin"}},
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "ptah-apply-e2e-configmap-refused-schema-1", UID: "job-uid"}}
	return plan, approval, job
}

// operatorJournalOfAnApply is the journal the operator's own code keeps across
// the two status writes of an approved Apply: the claim that ends into the proof
// it owes, and the proof that settles in sync. The statuses are what the
// controller writes; the entries are what lifecyclejournal derives from them.
func operatorJournalOfAnApply(
	plan *ptahv1alpha1.PtahSchemaPlan, approval *ptahv1alpha1.PtahSchemaApproval, job *batchv1.Job,
	outcome ptahv1alpha1.PendingObservationOutcome, stopped *ptahv1alpha1.SchemaApplyFailureStatus,
) *ptahv1alpha1.LifecycleJournal {
	start := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	claim := "sha256:" + strings.Repeat("d", 64)
	applying := &ptahv1alpha1.PtahSchemaStatus{ActiveOperation: &ptahv1alpha1.ActiveOperationStatus{
		Type: ptahv1alpha1.OperationApply, ID: claim, JobName: job.Name, JobUID: job.UID, StartedAt: metav1.NewTime(start),
	}}
	owed := &ptahv1alpha1.PtahSchemaStatus{PendingObservation: &ptahv1alpha1.PendingObservationStatus{
		Outcome: outcome, ApplyOperationID: claim, ApplyJobName: job.Name, ApplyJobUID: job.UID, ApplyFailure: stopped.DeepCopy(),
		Plan: ptahv1alpha1.CurrentPlanStatus{
			Name: plan.Name, UID: plan.UID, Fingerprint: plan.Spec.Fingerprint,
			Approval: &ptahv1alpha1.ConsumedApprovalStatus{Name: approval.Name, UID: approval.UID, Approver: approval.Spec.Approver},
		},
	}}
	settled := &ptahv1alpha1.PtahSchemaStatus{Phase: ptahv1alpha1.PhaseInSync}
	ended := start.Add(time.Minute)
	journal := lifecyclejournal.Record(nil, lifecyclejournal.SchemaEntries(applying, owed, nil, ended), ended)
	proved := ended.Add(time.Minute)
	return lifecyclejournal.Record(journal, lifecyclejournal.SchemaEntries(owed, settled, journal, proved), proved)
}

// applyJournaledAs reads the journal the operator keeps for an Apply, and refuses
// each field that does not name the object it should: the mistakes an entry that
// lost a field on its way through the API server, or one an older release wrote,
// would make.
func TestApplyJournaledAsRefusesEachField(t *testing.T) {
	t.Parallel()
	plan, approval, job := appliedObjects()
	stopped := &ptahv1alpha1.SchemaApplyFailureStatus{Statement: 1, Statements: 1, SQLState: "23505"}
	succeeded := journaledApply{plan: plan, approval: approval, job: job, outcome: ptahv1alpha1.JournalSucceeded}
	refused := journaledApply{plan: plan, approval: approval, job: job, outcome: ptahv1alpha1.JournalUnknown, stopped: stopped}
	ofSuccess := func() *ptahv1alpha1.LifecycleJournal {
		return operatorJournalOfAnApply(plan, approval, job, ptahv1alpha1.PendingObservationApplySucceeded, nil)
	}
	ofRefusal := func() *ptahv1alpha1.LifecycleJournal {
		return operatorJournalOfAnApply(plan, approval, job, ptahv1alpha1.PendingObservationOutcomeUnknown, stopped)
	}
	for name, test := range map[string]struct {
		journal *ptahv1alpha1.LifecycleJournal
		want    journaledApply
	}{
		"an Apply that succeeded":           {ofSuccess(), succeeded},
		"an Apply refused at its statement": {ofRefusal(), refused},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if count := len(journaled(test.journal, ptahv1alpha1.JournalApplyFinished)); count != 1 {
				t.Fatalf("the operator's journal holds %d ApplyFinished entries, want the one this reads", count)
			}
			entry, err := applyJournaledAs(test.journal, test.want)
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if err := proofJournaledFor(test.journal, entry, ptahv1alpha1.JournalInSync); err != nil {
				t.Fatalf("the proof was refused: %v", err)
			}
		})
	}

	edited := func(base func() *ptahv1alpha1.LifecycleJournal, edit func(*ptahv1alpha1.JournalEntry)) *ptahv1alpha1.LifecycleJournal {
		journal := base()
		for index := range journal.Entries {
			if journal.Entries[index].Type == ptahv1alpha1.JournalApplyFinished {
				edit(&journal.Entries[index])
			}
		}
		return journal
	}
	other := journaledApply{plan: plan.DeepCopy(), approval: approval, job: job, outcome: ptahv1alpha1.JournalSucceeded}
	other.plan.Name = "ptah-plan-other"
	for _, test := range []struct {
		name    string
		journal *ptahv1alpha1.LifecycleJournal
		want    journaledApply
		refusal string
	}{
		{"no journal", nil, succeeded, "keeps no journal"},
		{"another plan's Apply", ofSuccess(), other, "holds 0"},
		{"the plan applied twice", func() *ptahv1alpha1.LifecycleJournal {
			journal := ofSuccess()
			journal.Entries = append(journal.Entries, journaled(journal, ptahv1alpha1.JournalApplyFinished)...)
			return journal
		}(), succeeded, "holds 2"},
		{"an entry an older release wrote", edited(ofSuccess, func(entry *ptahv1alpha1.JournalEntry) {
			*entry = ptahv1alpha1.JournalEntry{Type: entry.Type, At: entry.At, StartedAt: entry.StartedAt, Plan: entry.Plan, Outcome: entry.Outcome}
		}), succeeded, "not an Apply claim's ID"},
		{"an operation that is not a claim's", edited(ofSuccess, func(entry *ptahv1alpha1.JournalEntry) { entry.OperationID = "apply-1" }),
			succeeded, "not an Apply claim's ID"},
		{"another outcome", ofRefusal(), journaledApply{plan: plan, approval: approval, job: job, outcome: ptahv1alpha1.JournalSucceeded, stopped: stopped},
			"outcome"},
		{"no plan UID", edited(ofSuccess, func(entry *ptahv1alpha1.JournalEntry) { entry.PlanUID = "" }), succeeded, "planUID"},
		{"another plan UID", edited(ofSuccess, func(entry *ptahv1alpha1.JournalEntry) { entry.PlanUID = "replaced-plan-uid" }), succeeded, "planUID"},
		{"no fingerprint", edited(ofSuccess, func(entry *ptahv1alpha1.JournalEntry) { entry.PlanFingerprint = "" }), succeeded, "planFingerprint"},
		{"no approval", edited(ofSuccess, func(entry *ptahv1alpha1.JournalEntry) { entry.Approval = "" }), succeeded, `approval ""`},
		{"another approval under the name", edited(ofSuccess, func(entry *ptahv1alpha1.JournalEntry) { entry.ApprovalUID = "earlier-approval-uid" }),
			succeeded, "approvalUID"},
		{"no approval UID", edited(ofSuccess, func(entry *ptahv1alpha1.JournalEntry) { entry.ApprovalUID = "" }), succeeded, "approvalUID"},
		{"another approver", edited(ofSuccess, func(entry *ptahv1alpha1.JournalEntry) { entry.Approver = "e2e-approver" }), succeeded, "approver"},
		{"no Job", edited(ofSuccess, func(entry *ptahv1alpha1.JournalEntry) { entry.JobName, entry.JobUID = "", "" }), succeeded, "jobName"},
		{"a later Job under the name", edited(ofSuccess, func(entry *ptahv1alpha1.JournalEntry) { entry.JobUID = "later-job-uid" }), succeeded, "jobUID"},
		{"a refusal that names no statement", edited(ofRefusal, func(entry *ptahv1alpha1.JournalEntry) { entry.ApplyFailure = nil }),
			refused, "applyFailure none"},
		{"another SQLSTATE", edited(ofRefusal, func(entry *ptahv1alpha1.JournalEntry) { entry.ApplyFailure.SQLState = "42P01" }),
			refused, "applyFailure"},
		{"another statement", edited(ofRefusal, func(entry *ptahv1alpha1.JournalEntry) { entry.ApplyFailure.Statements = 2 }),
			refused, "applyFailure"},
		{"a success that names a statement", edited(ofSuccess, func(entry *ptahv1alpha1.JournalEntry) {
			entry.ApplyFailure = stopped.DeepCopy()
		}), succeeded, "applyFailure"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := applyJournaledAs(test.journal, test.want)
			if err == nil || !strings.Contains(err.Error(), test.refusal) {
				t.Fatalf("applyJournaledAs() = %v, want an error naming %q", err, test.refusal)
			}
		})
	}
}

// proofJournaledFor joins an Apply to its proof by the operation ID, and refuses
// a proof journaled under another claim or none, one of another plan, one from
// before the Apply, and a verdict the row did not expect.
func TestProofJournaledForJoinsByTheOperation(t *testing.T) {
	t.Parallel()
	plan, approval, job := appliedObjects()
	base := operatorJournalOfAnApply(plan, approval, job, ptahv1alpha1.PendingObservationApplySucceeded, nil)
	applied := journaled(base, ptahv1alpha1.JournalApplyFinished)
	proofs := journaled(base, ptahv1alpha1.JournalVerified)
	if len(applied) != 1 || len(proofs) != 1 {
		t.Fatalf("the operator's journal holds %d ApplyFinished and %d Verified entries, want one of each", len(applied), len(proofs))
	}
	apply, proof := applied[0], proofs[0]
	with := func(entries ...ptahv1alpha1.JournalEntry) *ptahv1alpha1.LifecycleJournal {
		return &ptahv1alpha1.LifecycleJournal{Entries: entries}
	}
	proofOf := func(edit func(*ptahv1alpha1.JournalEntry)) ptahv1alpha1.JournalEntry {
		edited := *proof.DeepCopy()
		edit(&edited)
		return edited
	}
	if err := proofJournaledFor(with(apply, proof), apply, ""); err != nil {
		t.Fatalf("a proof the row claims no verdict for was refused: %v", err)
	}
	for _, test := range []struct {
		name    string
		journal *ptahv1alpha1.LifecycleJournal
		outcome ptahv1alpha1.JournalOutcome
		want    string
	}{
		{"no journal", nil, "", "keeps no journal"},
		{"no proof yet", with(apply), "", "stops before the Verified"},
		{"a proof from before the Apply", with(proof, apply), "", "stops before the Verified"},
		{"another plan's proof", with(apply, proofOf(func(entry *ptahv1alpha1.JournalEntry) { entry.Plan = "ptah-plan-other" })), "", "stops before the Verified"},
		{"a proof under another claim", with(apply, proofOf(func(entry *ptahv1alpha1.JournalEntry) {
			entry.OperationID = "sha256:" + strings.Repeat("e", 64)
		})), "", "names operation"},
		{"a proof an older release wrote", with(apply, proofOf(func(entry *ptahv1alpha1.JournalEntry) { entry.OperationID = "" })), "", `names operation ""`},
		{"another verdict", with(apply, proof), ptahv1alpha1.JournalDriftRemains, `as "InSync", not "DriftRemains"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := proofJournaledFor(test.journal, apply, test.outcome)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("proofJournaledFor() = %v, want an error naming %q", err, test.want)
			}
		})
	}
}

// journaled is every entry of entryType the journal holds.
func journaled(journal *ptahv1alpha1.LifecycleJournal, entryType ptahv1alpha1.JournalEntryType) []ptahv1alpha1.JournalEntry {
	if journal == nil {
		return nil
	}
	var found []ptahv1alpha1.JournalEntry
	for _, entry := range journal.Entries {
		if entry.Type == entryType {
			found = append(found, entry)
		}
	}
	return found
}
