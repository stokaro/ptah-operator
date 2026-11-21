package blockedsince_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/blockedsince"
	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

// statusPhases reads the phases a CRD's status.phase admits, so the tables
// below are held to the API rather than to a list written beside them.
func statusPhases(t *testing.T, crdName string) []string {
	t.Helper()
	candidates, err := crdupgrade.Candidates()
	if err != nil {
		t.Fatal(err)
	}
	for _, crd := range candidates {
		if crd.Name != crdName {
			continue
		}
		var phases []string
		for _, value := range crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"].Properties["phase"].Enum {
			var phase string
			if err := json.Unmarshal(value.Raw, &phase); err != nil {
				t.Fatal(err)
			}
			phases = append(phases, phase)
		}
		if len(phases) == 0 {
			t.Fatalf("%s declares no status.phase enum", crdName)
		}
		return phases
	}
	t.Fatalf("no CRD named %s", crdName)
	return nil
}

// assertEveryPhaseListed refuses a table that misses a phase the API admits or
// names one it does not, so a new phase cannot reach the default unread.
func assertEveryPhaseListed[P ~string](t *testing.T, crdName string, table map[P]blockedsince.End) {
	t.Helper()
	phases := statusPhases(t, crdName)
	for _, phase := range phases {
		if _, listed := table[P(phase)]; !listed {
			t.Errorf("%s phase %s has no row", crdName, phase)
		}
	}
	for phase := range table {
		if !slices.Contains(phases, string(phase)) {
			t.Errorf("row for %s phase %s, which the API does not admit", crdName, phase)
		}
	}
}

func schemaWith(apply operatorv1alpha1.ApplyPolicy, status operatorv1alpha1.PtahSchemaStatus) *operatorv1alpha1.PtahSchema {
	return &operatorv1alpha1.PtahSchema{
		Spec:   operatorv1alpha1.PtahSchemaSpec{Policy: operatorv1alpha1.ReconciliationPolicy{Apply: apply}},
		Status: status,
	}
}

func migrationWith(status operatorv1alpha1.PtahMigrationStatus) *operatorv1alpha1.PtahMigration {
	return &operatorv1alpha1.PtahMigration{Status: status}
}

func ready(reason operatorv1alpha1.ConditionReason) []metav1.Condition {
	return []metav1.Condition{{Type: operatorv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: string(reason)}}
}

// What each phase says about the pass that wrote it, with no claim in status
// and no Ready reason that decides it.
func TestEachPhaseKeepsSetsOrClearsTheBlockedDate(t *testing.T) {
	t.Parallel()
	schemaPhases := map[operatorv1alpha1.ReconciliationPhase]blockedsince.End{
		operatorv1alpha1.PhasePending:              blockedsince.Open,
		operatorv1alpha1.PhaseResolving:            blockedsince.Open,
		operatorv1alpha1.PhaseVerifying:            blockedsince.Open,
		operatorv1alpha1.PhaseObserving:            blockedsince.Open,
		operatorv1alpha1.PhasePlanning:             blockedsince.Open,
		operatorv1alpha1.PhaseApplying:             blockedsince.Open,
		operatorv1alpha1.PhaseVerifyingConvergence: blockedsince.Open,
		operatorv1alpha1.PhaseBlocked:              blockedsince.Blocked,
		operatorv1alpha1.PhaseReadyToApply:         blockedsince.Settled,
		operatorv1alpha1.PhaseAwaitingApproval:     blockedsince.Settled,
		operatorv1alpha1.PhaseInSync:               blockedsince.Settled,
		operatorv1alpha1.PhaseSuspended:            blockedsince.Settled,
		operatorv1alpha1.PhaseFailed:               blockedsince.Settled,
	}
	assertEveryPhaseListed(t, "ptahschemas.operator.ptah.run", schemaPhases)
	for phase, want := range schemaPhases {
		if got := blockedsince.Schema(schemaWith(operatorv1alpha1.ApplyPolicyOnApproval, operatorv1alpha1.PtahSchemaStatus{Phase: phase})); got != want {
			t.Errorf("schema %s = %d, want %d", phase, got, want)
		}
	}
	migrationPhases := map[operatorv1alpha1.MigrationPhase]blockedsince.End{
		operatorv1alpha1.MigrationPhasePending:   blockedsince.Open,
		operatorv1alpha1.MigrationPhaseResolving: blockedsince.Open,
		operatorv1alpha1.MigrationPhaseVerifying: blockedsince.Open,
		operatorv1alpha1.MigrationPhaseReading:   blockedsince.Open,
		// Written with no claim only by the history read that found nothing
		// blocking.
		operatorv1alpha1.MigrationPhasePlanning:         blockedsince.Settled,
		operatorv1alpha1.MigrationPhaseApplying:         blockedsince.Open,
		operatorv1alpha1.MigrationPhaseVerifyingHistory: blockedsince.Open,
		operatorv1alpha1.MigrationPhaseBlocked:          blockedsince.Blocked,
		operatorv1alpha1.MigrationPhaseAwaitingApproval: blockedsince.Settled,
		operatorv1alpha1.MigrationPhaseInSync:           blockedsince.Settled,
		operatorv1alpha1.MigrationPhaseSuspended:        blockedsince.Settled,
		operatorv1alpha1.MigrationPhaseFailed:           blockedsince.Settled,
	}
	assertEveryPhaseListed(t, "ptahmigrations.operator.ptah.run", migrationPhases)
	for phase, want := range migrationPhases {
		if got := blockedsince.Migration(migrationWith(operatorv1alpha1.PtahMigrationStatus{Phase: phase})); got != want {
			t.Errorf("migration %s = %d, want %d", phase, got, want)
		}
	}
}

// The writes a phase alone does not decide: a claim, a retained claim, and the
// Ready reasons the apply policy writes.
func TestAClaimKeepsTheBlockedDateAndThePolicyAtWorkClearsIt(t *testing.T) {
	t.Parallel()
	claim := &operatorv1alpha1.ActiveOperationStatus{Type: operatorv1alpha1.OperationResolve}
	migrationClaim := &operatorv1alpha1.MigrationOperationStatus{Type: operatorv1alpha1.MigrationOperationResolve}
	blocked := func(reason operatorv1alpha1.ConditionReason) operatorv1alpha1.PtahSchemaStatus {
		return operatorv1alpha1.PtahSchemaStatus{Phase: operatorv1alpha1.PhaseBlocked, Conditions: ready(reason)}
	}
	for _, row := range []struct {
		name string
		got  blockedsince.End
		want blockedsince.End
	}{
		{"a schema claim", blockedsince.Schema(schemaWith(operatorv1alpha1.ApplyPolicyOnApproval, operatorv1alpha1.PtahSchemaStatus{
			Phase: operatorv1alpha1.PhaseResolving, ActiveOperation: claim})), blockedsince.Open},
		{"a schema read that failed and waits for its retry", blockedsince.Schema(schemaWith(operatorv1alpha1.ApplyPolicyOnApproval, operatorv1alpha1.PtahSchemaStatus{
			Phase: operatorv1alpha1.PhaseFailed, ActiveOperation: claim})), blockedsince.Open},
		{"a schema proof whose target moved keeps its claim and waits for a person", blockedsince.Schema(schemaWith(operatorv1alpha1.ApplyPolicyOnApproval, operatorv1alpha1.PtahSchemaStatus{
			Phase: operatorv1alpha1.PhaseBlocked, ActiveOperation: claim, PendingObservation: &operatorv1alpha1.PendingObservationStatus{}})), blockedsince.Blocked},
		{"a schema under apply: Never with a plan it could apply", blockedsince.Schema(schemaWith(operatorv1alpha1.ApplyPolicyNever,
			blocked(operatorv1alpha1.ReasonApplyDisabled))), blockedsince.Settled},
		{"a destructive plan under apply: Never, as the Plan publishes it", blockedsince.Schema(schemaWith(operatorv1alpha1.ApplyPolicyNever,
			blocked(operatorv1alpha1.ReasonPolicyBlocked))), blockedsince.Settled},
		{"a destructive plan under apply: Never, as the approval gate meets it", blockedsince.Schema(schemaWith(operatorv1alpha1.ApplyPolicyNever,
			blocked(operatorv1alpha1.ReasonDestructiveChangesDisabled))), blockedsince.Settled},
		{"a destructive plan under OnApproval", blockedsince.Schema(schemaWith(operatorv1alpha1.ApplyPolicyOnApproval,
			blocked(operatorv1alpha1.ReasonPolicyBlocked))), blockedsince.Blocked},
		{"a destructive plan under Always, at the approval gate", blockedsince.Schema(schemaWith(operatorv1alpha1.ApplyPolicyAlways,
			blocked(operatorv1alpha1.ReasonDestructiveChangesDisabled))), blockedsince.Blocked},
		{"a refused artifact under apply: Never", blockedsince.Schema(schemaWith(operatorv1alpha1.ApplyPolicyNever,
			blocked(operatorv1alpha1.ReasonPolicyRefused))), blockedsince.Blocked},
		{"an unreadable source under apply: Never", blockedsince.Schema(schemaWith(operatorv1alpha1.ApplyPolicyNever,
			blocked(operatorv1alpha1.ReasonSchemaSourceUnreadable))), blockedsince.Blocked},
		{"a migration claim", blockedsince.Migration(migrationWith(operatorv1alpha1.PtahMigrationStatus{
			Phase: operatorv1alpha1.MigrationPhaseReading, ActiveOperation: migrationClaim})), blockedsince.Open},
		{"a migration read retrying under its claim", blockedsince.Migration(migrationWith(operatorv1alpha1.PtahMigrationStatus{
			Phase: operatorv1alpha1.MigrationPhaseResolving, ActiveOperation: migrationClaim})), blockedsince.Open},
		{"a migration claiming its read again at Planning", blockedsince.Migration(migrationWith(operatorv1alpha1.PtahMigrationStatus{
			Phase: operatorv1alpha1.MigrationPhasePlanning, ActiveOperation: migrationClaim})), blockedsince.Open},
		{"a migration under apply: Never at rest", blockedsince.Migration(migrationWith(operatorv1alpha1.PtahMigrationStatus{
			Phase: operatorv1alpha1.MigrationPhasePlanning, Conditions: ready(operatorv1alpha1.ReasonApplyDisabled)})), blockedsince.Settled},
		{"a migration plan about to be published", blockedsince.Migration(migrationWith(operatorv1alpha1.PtahMigrationStatus{
			Phase: operatorv1alpha1.MigrationPhasePlanning, Conditions: ready(operatorv1alpha1.ReasonMigrationsPending)})), blockedsince.Settled},
		{"a migration under apply: Always handing its plan to the Apply", blockedsince.Migration(migrationWith(operatorv1alpha1.PtahMigrationStatus{
			Phase: operatorv1alpha1.MigrationPhasePlanning, Conditions: ready(operatorv1alpha1.ReasonApplyPending)})), blockedsince.Settled},
		{"a migration whose Apply outcome is unknown", blockedsince.Migration(migrationWith(operatorv1alpha1.PtahMigrationStatus{
			Phase: operatorv1alpha1.MigrationPhaseBlocked, Conditions: ready(operatorv1alpha1.ReasonApplyOutcomeUnknown)})), blockedsince.Blocked},
	} {
		if row.got != row.want {
			t.Errorf("%s = %d, want %d", row.name, row.got, row.want)
		}
	}
}

func TestOnlyASettledEndLeavesADateUncounted(t *testing.T) {
	t.Parallel()
	for end, want := range map[blockedsince.End]bool{
		blockedsince.Open: true, blockedsince.Blocked: true, blockedsince.Settled: false,
	} {
		if got := end.Counted(); got != want {
			t.Errorf("End(%d).Counted() = %t, want %t", end, got, want)
		}
	}
}

func TestTheBlockedDateIsSetOnceAndClearedByAnotherEnd(t *testing.T) {
	t.Parallel()
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	earlier := metav1.NewTime(now.Add(-time.Hour))
	for _, row := range []struct {
		name    string
		current *metav1.Time
		end     blockedsince.End
		want    *metav1.Time
	}{
		{"the first Blocked dates the block", nil, blockedsince.Blocked, &metav1.Time{Time: now}},
		{"another Blocked keeps the first date", &earlier, blockedsince.Blocked, &earlier},
		{"a write on the way keeps the date", &earlier, blockedsince.Open, &earlier},
		{"a write on the way dates nothing", nil, blockedsince.Open, nil},
		{"another end clears the date", &earlier, blockedsince.Settled, nil},
	} {
		reads := 0
		got := blockedsince.Next(row.current, row.end, func() time.Time { reads++; return now })
		if (got == nil) != (row.want == nil) || (got != nil && !got.Equal(row.want)) {
			t.Errorf("%s: blockedSince = %v, want %v", row.name, got, row.want)
		}
		// Only a new date reads the clock.
		if wantReads := map[bool]int{true: 1, false: 0}[row.current == nil && row.end == blockedsince.Blocked]; reads != wantReads {
			t.Errorf("%s: read the clock %d times, want %d", row.name, reads, wantReads)
		}
	}
}
