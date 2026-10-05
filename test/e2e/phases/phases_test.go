package phases

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLoadReadsEveryDeclaredInput(t *testing.T) {
	t.Parallel()
	environment := map[string]string{
		"E2E_KUBECONFIG":         "/work/kubeconfig",
		"E2E_OPERATOR_NAMESPACE": "ptah-system",
		"E2E_TEST_NAMESPACE":     "ptah-e2e",
		"E2E_HELM_RELEASE":       "ptah",
		"E2E_CHART_PACKAGE":      "/work/chart.tgz",
		// Something the driver binds for another phase is not this phase's
		// business, and reading it would be a phase reading what it never
		// declared.
		"E2E_ENGINE": "postgresql",
	}
	inputs, err := Load(CertRotation, lookupIn(environment))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := CertRotationInputs{
		Kubeconfig:        "/work/kubeconfig",
		OperatorNamespace: "ptah-system",
		TestNamespace:     "ptah-e2e",
		HelmRelease:       "ptah",
		ChartPackage:      "/work/chart.tgz",
	}
	if inputs != want {
		t.Fatalf("Load() = %+v, want %+v", inputs, want)
	}
}

func TestLoadRefusesAMissingOrEmptyInput(t *testing.T) {
	t.Parallel()
	complete := map[string]string{
		"E2E_KUBECONFIG":         "/work/kubeconfig",
		"E2E_OPERATOR_NAMESPACE": "ptah-system",
		"E2E_TEST_NAMESPACE":     "ptah-e2e",
		"E2E_HELM_RELEASE":       "ptah",
		"E2E_CHART_PACKAGE":      "/work/chart.tgz",
	}
	for _, test := range []struct {
		name  string
		edit  func(map[string]string)
		names []string
	}{
		{name: "unset", edit: func(env map[string]string) { delete(env, "E2E_CHART_PACKAGE") }, names: []string{"E2E_CHART_PACKAGE"}},
		{name: "empty", edit: func(env map[string]string) { env["E2E_KUBECONFIG"] = "" }, names: []string{"E2E_KUBECONFIG"}},
		{name: "two missing", edit: func(env map[string]string) {
			delete(env, "E2E_TEST_NAMESPACE")
			env["E2E_HELM_RELEASE"] = ""
		}, names: []string{"E2E_TEST_NAMESPACE", "E2E_HELM_RELEASE"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			environment := map[string]string{}
			for name, value := range complete {
				environment[name] = value
			}
			test.edit(environment)
			_, err := Load(CertRotation, lookupIn(environment))
			if err == nil {
				t.Fatal("Load() accepted an environment with an input missing")
			}
			for _, name := range test.names {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("Load() error %q does not name %s", err, name)
				}
			}
			if !strings.Contains(err.Error(), "cert-rotation") {
				t.Errorf("Load() error %q does not name the phase", err)
			}
		})
	}
}

func TestInputsFollowTheDeclaration(t *testing.T) {
	t.Parallel()
	want := []string{
		"E2E_KUBECONFIG", "E2E_OPERATOR_NAMESPACE", "E2E_TEST_NAMESPACE", "E2E_HELM_RELEASE", "E2E_CHART_PACKAGE",
	}
	if got := CertRotation.Inputs(); !slices.Equal(got, want) {
		t.Fatalf("Inputs() = %v, want %v", got, want)
	}
}

func TestLookupFindsOnlyDeclaredPhases(t *testing.T) {
	t.Parallel()
	if phase, ok := Lookup("cert-rotation"); !ok || phase.Test != "TestCertRotation" {
		t.Fatalf("Lookup(cert-rotation) = %+v, %v", phase, ok)
	}
	for _, name := range []string{"", "Assert", "cert", "cert-rotation ", "TestCertRotation"} {
		if _, ok := Lookup(name); ok {
			t.Errorf("Lookup(%q) found a phase", name)
		}
	}
}

func TestEveryDeclaredPhaseIsValid(t *testing.T) {
	t.Parallel()
	if len(All()) == 0 {
		t.Fatal("the harness declares no phase")
	}
	for _, phase := range All() {
		if err := phase.validate(); err != nil {
			t.Errorf("%v", err)
		}
	}
	if err := distinct(All()); err != nil {
		t.Fatal(err)
	}
}

type wellFormed struct {
	Kubeconfig string `env:"E2E_KUBECONFIG"`
}

type unexportedInput struct {
	kubeconfig string `env:"E2E_KUBECONFIG"`
}

type numericInput struct {
	Replicas int `env:"E2E_REPLICAS"`
}

type untaggedInput struct {
	Kubeconfig string
}

type foreignInput struct {
	Kubeconfig string `env:"KUBECONFIG"`
}

type doubleInput struct {
	Kubeconfig string `env:"E2E_KUBECONFIG"`
	Again      string `env:"E2E_KUBECONFIG"`
}

type emptyInput struct{}

// Every refusal the declaration check can give, each shown to refuse the one
// mistake it names while the well-formed declaration beside it passes.
func TestValidateRefusesMalformedDeclarations(t *testing.T) {
	t.Parallel()
	base := Phase{Name: "example", Test: "TestExample", Timeout: time.Minute, Scenarios: []string{"one", "two"}}
	withInputs := func(phase Phase, inputs reflect.Type) Phase {
		phase.inputs = inputs
		return phase
	}
	if err := withInputs(base, reflect.TypeFor[wellFormed]()).validate(); err != nil {
		t.Fatalf("a well-formed declaration was refused: %v", err)
	}
	for _, test := range []struct {
		name   string
		phase  Phase
		refuse string
	}{
		{name: "phase name", phase: func() Phase { p := base; p.Name = "Example"; return withInputs(p, reflect.TypeFor[wellFormed]()) }(), refuse: "phase name"},
		{name: "test name", phase: func() Phase { p := base; p.Test = "Example"; return withInputs(p, reflect.TypeFor[wellFormed]()) }(), refuse: "test function"},
		{name: "no bound", phase: func() Phase { p := base; p.Timeout = 0; return withInputs(p, reflect.TypeFor[wellFormed]()) }(), refuse: "no bound"},
		{name: "no scenario", phase: func() Phase { p := base; p.Scenarios = nil; return withInputs(p, reflect.TypeFor[wellFormed]()) }(), refuse: "no scenario"},
		{name: "scenario name", phase: func() Phase {
			p := base
			p.Scenarios = []string{"One"}
			return withInputs(p, reflect.TypeFor[wellFormed]())
		}(), refuse: "scenario name"},
		{name: "scenario twice", phase: func() Phase {
			p := base
			p.Scenarios = []string{"one", "one"}
			return withInputs(p, reflect.TypeFor[wellFormed]())
		}(), refuse: "declared twice"},
		{name: "no inputs", phase: withInputs(base, reflect.TypeFor[emptyInput]()), refuse: "struct of environment"},
		{name: "unexported input", phase: withInputs(base, reflect.TypeFor[unexportedInput]()), refuse: "unexported"},
		{name: "numeric input", phase: withInputs(base, reflect.TypeFor[numericInput]()), refuse: "not a string"},
		{name: "untagged input", phase: withInputs(base, reflect.TypeFor[untaggedInput]()), refuse: "not an E2E_ variable"},
		{name: "foreign variable", phase: withInputs(base, reflect.TypeFor[foreignInput]()), refuse: "not an E2E_ variable"},
		{name: "one variable twice", phase: withInputs(base, reflect.TypeFor[doubleInput]()), refuse: "two inputs"},
		{name: "negative preparation", phase: func() Phase {
			p := base
			p.Preparation = -1
			return withInputs(p, reflect.TypeFor[wellFormed]())
		}(), refuse: "leaves none of its own acceptance"},
		{name: "preparation is the whole phase", phase: func() Phase {
			p := base
			p.Preparation = len(p.Scenarios)
			return withInputs(p, reflect.TypeFor[wellFormed]())
		}(), refuse: "leaves none of its own acceptance"},
		{name: "prerequisite name", phase: func() Phase {
			p := base
			p.RequiresFull = []string{"Assert"}
			return withInputs(p, reflect.TypeFor[wellFormed]())
		}(), refuse: "invalid or repeated prerequisite"},
		{name: "self prerequisite", phase: func() Phase {
			p := base
			p.RequiresFull = []string{p.Name}
			return withInputs(p, reflect.TypeFor[wellFormed]())
		}(), refuse: "invalid or repeated prerequisite"},
		{name: "prerequisite twice", phase: func() Phase {
			p := base
			p.RequiresFull = []string{"assert", "assert"}
			return withInputs(p, reflect.TypeFor[wellFormed]())
		}(), refuse: "invalid or repeated prerequisite"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.phase.validate()
			if err == nil || !strings.Contains(err.Error(), test.refuse) {
				t.Fatalf("validate() error = %v, want one naming %q", err, test.refuse)
			}
		})
	}
}

// The migration suites run the data plane for the namespace it stands up. The
// boundary has to fall after the fixtures and before everything the phase
// accepts on its own, or preparation would run the engine lifecycles in a
// suite that does not claim them. Faults belong to their own phase.
func TestDataPlanePreparesBeforeItsOwnAcceptance(t *testing.T) {
	t.Parallel()
	prepared := DataPlane.Scenarios[:DataPlane.Preparation]
	if !slices.Equal(prepared, []string{"databases-and-fixtures"}) {
		t.Fatalf("the data plane prepares with %v, want the databases and fixtures alone", prepared)
	}
	for _, acceptance := range []string{
		"postgresql-lifecycle", "native-plan-size-boundary",
	} {
		if !slices.Contains(DataPlane.Scenarios, acceptance) {
			t.Errorf("the data plane no longer runs %s", acceptance)
		}
		if slices.Contains(prepared, acceptance) {
			t.Errorf("%s runs before the preparation boundary, so preparation would execute it", acceptance)
		}
	}
	for _, fault := range SchemaFaults.Scenarios {
		if slices.Contains(prepared, fault) {
			t.Errorf("%s runs during preparation instead of its fault phase", fault)
		}
		if fault != "closing-audits" && slices.Contains(DataPlane.Scenarios, fault) {
			t.Errorf("%s is duplicated across lifecycle and fault acceptance", fault)
		}
	}
}

func TestDistinctRefusesACollision(t *testing.T) {
	t.Parallel()
	one := Phase{Name: "one", Test: "TestOne"}
	if err := distinct([]Phase{one, {Name: "two", Test: "TestTwo"}}); err != nil {
		t.Fatalf("distinct phases were refused: %v", err)
	}
	if err := distinct([]Phase{one, {Name: "one", Test: "TestTwo"}}); err == nil {
		t.Error("two phases with one name were accepted")
	}
	if err := distinct([]Phase{one, {Name: "two", Test: "TestOne"}}); err == nil {
		t.Error("one test for two phases was accepted")
	}
}

func lookupIn(environment map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := environment[name]
		return value, ok
	}
}

// Partitioning must preserve every incident exactly once; only the native
// producer and monitoring prerequisites run on both independent clusters.
func TestAlertPartitionsRetainEveryIncident(t *testing.T) {
	want := []string{
		"certificate-expiry", "lock-release-owed", "lost-scrape-target", "lost-view",
		"operations-failing", "ordinary-policy-waits", "plan-store-large",
		"resource-overdue", "stalled-operation", "unresolved-apply",
		"unresolved-view-read-failures", "upgrade-alerts",
	}
	var got []string
	for _, phase := range []Phase{Alerting.Phase, AlertingOperations.Phase} {
		if len(phase.Scenarios) < 3 || !slices.Equal(phase.Scenarios[:2], []string{"native-producers", "monitoring-path"}) {
			t.Fatalf("%s omits native prerequisites or all incidents", phase.Name)
		}
		got = append(got, phase.Scenarios[2:]...)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("alert partitions changed incident coverage: got %v, want %v", got, want)
	}
}
