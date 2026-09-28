package mutationlifecycle_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/mutationlifecycle"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// Each row is one type's answers, spelled out rather than derived, so a change
// to any of them is a change to this table that a reviewer reads.
func TestEachSchemaOperationTypeIsDescribedOnce(t *testing.T) {
	t.Parallel()

	type phase = operatorv1alpha1.ReconciliationPhase
	for _, row := range []struct {
		operation operatorv1alpha1.OperationType
		want      mutationlifecycle.Operation[phase]
	}{
		{operatorv1alpha1.OperationResolve, mutationlifecycle.Operation[phase]{
			Known: true, Phase: operatorv1alpha1.PhaseResolving, Runner: runner.OperationResolve,
		}},
		{operatorv1alpha1.OperationVerify, mutationlifecycle.Operation[phase]{
			Known: true, Phase: operatorv1alpha1.PhaseVerifying, Runner: runner.OperationVerify,
		}},
		{operatorv1alpha1.OperationObserve, mutationlifecycle.Operation[phase]{
			Known: true, Lock: mutationlifecycle.LockWhileProving, ServesProof: true,
			Phase: operatorv1alpha1.PhaseObserving, Runner: runner.OperationObserve,
		}},
		{operatorv1alpha1.OperationPlan, mutationlifecycle.Operation[phase]{
			Known: true, Lock: mutationlifecycle.LockAlways, ServesProof: true,
			Phase: operatorv1alpha1.PhasePlanning, Runner: runner.OperationPlan,
		}},
		{operatorv1alpha1.OperationApply, mutationlifecycle.Operation[phase]{
			Known: true, Mutating: true, Lock: mutationlifecycle.LockAlways,
			Phase: operatorv1alpha1.PhaseApplying, Runner: runner.OperationApply,
		}},
	} {
		t.Run(string(row.operation), func(t *testing.T) {
			t.Parallel()

			if got := mutationlifecycle.SchemaOperation(row.operation); got != row.want {
				t.Fatalf("SchemaOperation(%q) = %+v, want %+v", row.operation, got, row.want)
			}
		})
	}
}

func TestEachMigrationOperationTypeIsDescribedOnce(t *testing.T) {
	t.Parallel()

	type phase = operatorv1alpha1.MigrationPhase
	for _, row := range []struct {
		operation operatorv1alpha1.MigrationOperationType
		want      mutationlifecycle.Operation[phase]
	}{
		{operatorv1alpha1.MigrationOperationResolve, mutationlifecycle.Operation[phase]{
			Known: true, Phase: operatorv1alpha1.MigrationPhaseResolving, Runner: runner.OperationResolve,
		}},
		{operatorv1alpha1.MigrationOperationVerify, mutationlifecycle.Operation[phase]{
			Known: true, Phase: operatorv1alpha1.MigrationPhaseVerifying, Runner: runner.OperationVerify,
		}},
		{operatorv1alpha1.MigrationOperationHistory, mutationlifecycle.Operation[phase]{
			Known: true, Phase: operatorv1alpha1.MigrationPhaseReading, Runner: runner.OperationMigrationHistory,
		}},
		{operatorv1alpha1.MigrationOperationApply, mutationlifecycle.Operation[phase]{
			Known: true, Mutating: true, Lock: mutationlifecycle.LockAlways,
			Phase: operatorv1alpha1.MigrationPhaseApplying, Runner: runner.OperationMigrationApply,
		}},
	} {
		t.Run(string(row.operation), func(t *testing.T) {
			t.Parallel()

			if got := mutationlifecycle.MigrationOperation(row.operation); got != row.want {
				t.Fatalf("MigrationOperation(%q) = %+v, want %+v", row.operation, got, row.want)
			}
		})
	}
}

// A type a newer operator wrote is one this binary cannot reason about, and a
// negation must not turn that into permission. The unknown type answers no to
// every question, including whether it is read-only.
func TestAnUnknownTypeIsNeitherMutatingNorReadOnly(t *testing.T) {
	t.Parallel()

	schema := mutationlifecycle.SchemaOperation("Rollback")
	migration := mutationlifecycle.MigrationOperation("Rollback")
	for name, answers := range map[string][]bool{
		"schema": {
			schema.Known, schema.Mutating, schema.ReadOnly(), schema.ServesProof,
			schema.HoldsLock(false), schema.HoldsLock(true),
		},
		"migration": {
			migration.Known, migration.Mutating, migration.ReadOnly(), migration.ServesProof,
			migration.HoldsLock(false), migration.HoldsLock(true),
		},
	} {
		if slices.Contains(answers, true) {
			t.Fatalf("%s: an unknown type answered yes somewhere: %v", name, answers)
		}
	}
	if schema.Phase != "" || schema.Runner != "" || migration.Phase != "" || migration.Runner != "" {
		t.Fatalf("an unknown type names a phase or a runner operation: %+v %+v", schema, migration)
	}
	// The control: a known read-only type does answer yes, so the row above
	// is not passing on a ReadOnly that always says no.
	if !mutationlifecycle.SchemaOperation(operatorv1alpha1.OperationResolve).ReadOnly() {
		t.Fatal("a Resolve is not read-only")
	}
}

func TestWhenAClaimHoldsTheRealm(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		lock             mutationlifecycle.Lock
		proofOutstanding bool
		want             bool
	}{
		{mutationlifecycle.LockNever, false, false},
		{mutationlifecycle.LockNever, true, false},
		{mutationlifecycle.LockAlways, false, true},
		{mutationlifecycle.LockAlways, true, true},
		{mutationlifecycle.LockWhileProving, false, false},
		{mutationlifecycle.LockWhileProving, true, true},
	} {
		operation := mutationlifecycle.Operation[operatorv1alpha1.ReconciliationPhase]{Known: true, Lock: row.lock}
		if got := operation.HoldsLock(row.proofOutstanding); got != row.want {
			t.Errorf("lock rule %d with proof outstanding %t holds the realm = %t, want %t",
				row.lock, row.proofOutstanding, got, row.want)
		}
	}
}

// The runner refuses to be asked for a mutation it predates, and decides what
// a mutation is from its own operation names. The two lists have to agree: a
// claim the controller treats as read-only must not be one the runner opens
// the database to change.
func TestTheRunnerAgreesWhichOperationsMutate(t *testing.T) {
	t.Parallel()

	for _, operation := range schemaEnum(t) {
		described := mutationlifecycle.SchemaOperation(operatorv1alpha1.OperationType(operation))
		if described.Runner.Mutating() != described.Mutating {
			t.Errorf("schema %s: the descriptor says mutating=%t, the runner says %t for %q",
				operation, described.Mutating, described.Runner.Mutating(), described.Runner)
		}
		if !described.Runner.Valid() {
			t.Errorf("schema %s: runner operation %q is not one the runner performs", operation, described.Runner)
		}
	}
	for _, operation := range migrationEnum(t) {
		described := mutationlifecycle.MigrationOperation(operatorv1alpha1.MigrationOperationType(operation))
		if described.Runner.Mutating() != described.Mutating {
			t.Errorf("migration %s: the descriptor says mutating=%t, the runner says %t for %q",
				operation, described.Mutating, described.Runner.Mutating(), described.Runner)
		}
		if !described.Runner.Valid() {
			t.Errorf("migration %s: runner operation %q is not one the runner performs", operation, described.Runner)
		}
	}
}

// A type the API accepts and nothing here describes would read as unknown to
// every site that asks, which is the fail-closed answer and still the wrong
// one. The enumerations are read from the markers the CRD is generated from.
func TestEveryTypeTheAPIAcceptsIsDescribed(t *testing.T) {
	t.Parallel()

	for _, operation := range schemaEnum(t) {
		if !mutationlifecycle.SchemaOperation(operatorv1alpha1.OperationType(operation)).Known {
			t.Errorf("PtahSchema accepts operation type %q and nothing describes it", operation)
		}
	}
	for _, operation := range migrationEnum(t) {
		if !mutationlifecycle.MigrationOperation(operatorv1alpha1.MigrationOperationType(operation)).Known {
			t.Errorf("PtahMigration accepts operation type %q and nothing describes it", operation)
		}
	}
}

func schemaEnum(t *testing.T) []string {
	t.Helper()
	return enumOf(t, "../../api/v1alpha1/ptahschema_types.go", "OperationType", 5)
}

func migrationEnum(t *testing.T) []string {
	t.Helper()
	return enumOf(t, "../../api/v1alpha1/ptahmigration_types.go", "MigrationOperationType", 4)
}

// enumOf reads the enumeration marker above a type declaration. The count is
// the number of values the API declares today, so a marker this stops finding,
// or one it finds with a value fewer, fails here rather than passing on
// nothing; a new value fails here too, and whoever adds it describes it.
func enumOf(t *testing.T, path, typeName string, want int) []string {
	t.Helper()

	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	marker := regexp.MustCompile(`(?m)^// \+kubebuilder:validation:Enum=(\S+)\ntype ` + typeName + ` string$`)
	match := marker.FindSubmatch(source)
	if match == nil {
		t.Fatalf("%s: no enumeration marker above type %s", path, typeName)
	}
	values := strings.Split(string(match[1]), ";")
	if len(values) != want {
		t.Fatalf("%s: type %s enumerates %d values %v, want %d", path, typeName, len(values), values, want)
	}
	return values
}
