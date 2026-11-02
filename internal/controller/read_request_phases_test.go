package controller

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/operate"
)

// phaseReading is what one phase means to a read request: what this
// controller does with a request pending in it, once its retry delay has
// passed and before (nextRead true and false), and whether kubectl ptah's
// wait counts the phase as the end of a read (operate.ReadEndsIn).
type phaseReading struct {
	due, early readOnRequest
	readEnds   bool
}

var (
	// answers is a phase a read cycle ends in: the request is answered and
	// the next read brought forward.
	answers = phaseReading{due: readOnRequest{answer: true, schedule: true}, early: readOnRequest{answer: true, schedule: true}, readEnds: true}
	// underway is a read part way through, which does not answer a request.
	underway = phaseReading{}
	// startsFresh is a pass that starts a new Resolve, which answers the
	// request without bringing anything forward.
	startsFresh = phaseReading{due: readOnRequest{answer: true}, early: readOnRequest{answer: true}}
	// failed answers once the retry delay has passed.
	failed = phaseReading{due: readOnRequest{answer: true}, readEnds: true}
)

// TestEveryPhaseMeansTheSameToTheControllerAndTheWait holds the controller's
// answer to a read request and the wait's end of a read to one table, for
// every phase constant the API package declares. A phase added to either
// enum, or a phase one side reclassifies and the other does not, fails here.
//
// Two rules hold across the table: a phase in which the controller leaves a
// request for a read still running is never the end of a read, and a phase
// in which it answers and brings the next read forward always is. Suspended
// starts a fresh Resolve once the spec is resumed and is a resting state
// until then; the controller never reaches the request while it is
// suspended.
func TestEveryPhaseMeansTheSameToTheControllerAndTheWait(t *testing.T) {
	t.Parallel()

	schemaPhases := map[operatorv1alpha1.ReconciliationPhase]phaseReading{
		operatorv1alpha1.PhasePending:              startsFresh,
		operatorv1alpha1.PhaseResolving:            startsFresh,
		operatorv1alpha1.PhaseVerifying:            underway,
		operatorv1alpha1.PhaseObserving:            underway,
		operatorv1alpha1.PhasePlanning:             underway,
		operatorv1alpha1.PhaseReadyToApply:         answers,
		operatorv1alpha1.PhaseAwaitingApproval:     answers,
		operatorv1alpha1.PhaseBlocked:              answers,
		operatorv1alpha1.PhaseApplying:             startsFresh,
		operatorv1alpha1.PhaseVerifyingConvergence: underway,
		operatorv1alpha1.PhaseInSync:               answers,
		operatorv1alpha1.PhaseSuspended:            {due: startsFresh.due, early: startsFresh.early, readEnds: true},
		operatorv1alpha1.PhaseFailed:               failed,
	}
	migrationPhases := map[operatorv1alpha1.MigrationPhase]phaseReading{
		operatorv1alpha1.MigrationPhasePending:          startsFresh,
		operatorv1alpha1.MigrationPhaseResolving:        startsFresh,
		operatorv1alpha1.MigrationPhaseVerifying:        underway,
		operatorv1alpha1.MigrationPhaseReading:          underway,
		operatorv1alpha1.MigrationPhasePlanning:         answers,
		operatorv1alpha1.MigrationPhaseAwaitingApproval: answers,
		operatorv1alpha1.MigrationPhaseBlocked:          answers,
		operatorv1alpha1.MigrationPhaseApplying:         startsFresh,
		operatorv1alpha1.MigrationPhaseVerifyingHistory: underway,
		operatorv1alpha1.MigrationPhaseInSync:           answers,
		operatorv1alpha1.MigrationPhaseSuspended:        {due: startsFresh.due, early: startsFresh.early, readEnds: true},
		operatorv1alpha1.MigrationPhaseFailed:           failed,
	}

	declared := declaredPhases(t)
	if got, want := sortedKeys(schemaPhases), declared["ReconciliationPhase"]; !slices.Equal(got, want) {
		t.Fatalf("the table classifies schema phases %v; the API declares %v", got, want)
	}
	if got, want := sortedKeys(migrationPhases), declared["MigrationPhase"]; !slices.Equal(got, want) {
		t.Fatalf("the table classifies migration phases %v; the API declares %v", got, want)
	}

	for phase, want := range schemaPhases {
		got := phaseReading{
			due: schemaReadOnRequest(phase, true), early: schemaReadOnRequest(phase, false),
			readEnds: operate.ReadEndsIn(operate.FamilySchema, string(phase)),
		}
		checkPhaseReading(t, "schema", string(phase), got, want)
	}
	for phase, want := range migrationPhases {
		got := phaseReading{
			due: migrationReadOnRequest(phase, true), early: migrationReadOnRequest(phase, false),
			readEnds: operate.ReadEndsIn(operate.FamilyMigration, string(phase)),
		}
		checkPhaseReading(t, "migration", string(phase), got, want)
	}
}

func checkPhaseReading(t *testing.T, family, phase string, got, want phaseReading) {
	t.Helper()
	if got != want {
		t.Errorf("%s phase %s: controller and wait read it as %+v, the table says %+v", family, phase, got, want)
	}
	if !got.due.answer && got.readEnds {
		t.Errorf("%s phase %s: the controller leaves a request for a read still running, and the wait ends a read there", family, phase)
	}
	if got.due.answer && got.due.schedule && !got.readEnds {
		t.Errorf("%s phase %s: the controller ends a read there, and the wait does not", family, phase)
	}
}

// declaredPhases reads the phase constants out of the API package's source,
// by type name, so a phase nobody added to the table is found here rather
// than by a wait that never ends.
func declaredPhases(t *testing.T) map[string][]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "api", "v1alpha1", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string][]string{}
	fileSet := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			spec, ok := node.(*ast.ValueSpec)
			if !ok {
				return true
			}
			typeName, ok := spec.Type.(*ast.Ident)
			if !ok || (typeName.Name != "ReconciliationPhase" && typeName.Name != "MigrationPhase") {
				return true
			}
			for _, value := range spec.Values {
				literal, ok := value.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					t.Fatalf("%s declares a %s that is not a string literal", file, typeName.Name)
				}
				phase, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatal(err)
				}
				declared[typeName.Name] = append(declared[typeName.Name], phase)
			}
			return true
		})
	}
	if len(declared["ReconciliationPhase"]) == 0 || len(declared["MigrationPhase"]) == 0 {
		t.Fatalf("found phases %v; the parse is reading the wrong files", declared)
	}
	for _, phases := range declared {
		slices.Sort(phases)
	}
	return declared
}

func sortedKeys[K ~string, V any](table map[K]V) []string {
	keys := make([]string, 0, len(table))
	for key := range table {
		keys = append(keys, string(key))
	}
	slices.Sort(keys)
	return keys
}
