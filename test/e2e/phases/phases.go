// Package phases declares the acceptance phases the Go harness carries.
//
// hack/e2e-kind.sh stands the cluster up and runs every phase through
// run_recorded_phase. A phase ported to Go is one test function in the binary
// the driver builds from test/e2e, and this package is the contract between the
// two: the name the driver and support/e2e-suites.json know the phase by, the
// test that is the phase, the environment the driver hands it, the scenarios it
// records in the timing ledger, and the bound it runs under.
//
// Three programs read it. The harness loads a phase's inputs from nothing else.
// hack/verify-kubernetes-support.go holds the driver's call to exactly those
// inputs. hack/acceptancecoverage lists the scenarios. The inputs are one
// struct per phase, so a phase reads what it declared and the compiler refuses
// anything else; a shell phase needed a hand-written environment contract and
// a scan of its source for the same answer.
//
// The package imports nothing heavier than reflect, because the verifier and
// the coverage table import it too.
package phases

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Phase is one acceptance phase the Go harness carries.
type Phase struct {
	// Name is the phase as run_recorded_phase and the suite catalog spell it.
	Name string
	// Test is the test function in test/e2e that is the phase. The binary runs
	// that function and nothing else when it is asked for the phase.
	Test string
	// Timeout bounds the whole phase. It sits above the sum of the phase's own
	// waits, so it is a backstop for a call that hangs rather than a deadline
	// any wait relies on.
	Timeout time.Duration
	// Scenarios are the stages the phase records in the timing ledger, in the
	// order it runs them. The harness refuses one out of order and fails a
	// phase that ends without running all of them.
	Scenarios []string
	// IsolatesNode says the phase cuts the isolation worker off from the API
	// server, so only a suite that declares that worker may run it.
	IsolatesNode bool
	// Preparation is how many of the leading scenarios make up the phase's
	// preparation mode: what another suite runs it for, the objects it stands
	// up, and none of its own acceptance. Zero means the phase has no such
	// mode. A run that stopped at the boundary proved those scenarios alone.
	Preparation int

	inputs reflect.Type
}

// Of is a phase whose inputs are T. The harness takes one and returns a T, so
// the type of what a phase reads is decided where the phase is declared.
type Of[T any] struct {
	Phase
}

// ControlPlaneInputs is what the driver hands the control-plane contract
// phase, which the driver knows as assert.
type ControlPlaneInputs struct {
	// Kubeconfig names the cluster the driver stood up.
	Kubeconfig string `env:"E2E_KUBECONFIG"`
	// OperatorNamespace is the release namespace.
	OperatorNamespace string `env:"E2E_OPERATOR_NAMESPACE"`
	// TestNamespace is where the phase creates its schema, plan and approval
	// fixtures, which the certificates and data-plane phases go on to use.
	TestNamespace string `env:"E2E_TEST_NAMESPACE"`
	// ForeignNamespace holds the objects a reference from TestNamespace must
	// not reach.
	ForeignNamespace string `env:"E2E_FOREIGN_NAMESPACE"`
	// HelmRelease is the installed release.
	HelmRelease string `env:"E2E_HELM_RELEASE"`
	// ExecutorImage is the digest-pinned Ptah executor the release runs.
	ExecutorImage string `env:"E2E_EXECUTOR_IMAGE"`
	// RunnerImage is the runner image built beside the manager.
	RunnerImage string `env:"E2E_RUNNER_IMAGE"`
	// PtahVersion is the Ptah version bound beside the executor.
	PtahVersion string `env:"E2E_PTAH_VERSION"`
	// ControllerImage is the candidate manager image, pinned by digest.
	ControllerImage string `env:"E2E_CONTROLLER_IMAGE"`
	// ControllerRevision is the commit the candidate was built from.
	ControllerRevision string `env:"E2E_CONTROLLER_REVISION"`
	// ControllerStateVersion is the controller-state version the chart was
	// stamped with.
	ControllerStateVersion string `env:"E2E_CONTROLLER_STATE_VERSION"`
}

// ControlPlane proves the control-plane contract: the release's readiness,
// discovery, authorization and admission shape, the API server's refusals,
// and the approval binding, against fixtures the later phases reuse.
var ControlPlane = define[ControlPlaneInputs](Phase{
	Name:    "assert",
	Test:    "TestControlPlaneContract",
	Timeout: 60 * time.Minute,
	Scenarios: []string{
		"manager-readiness",
		"crd-discovery",
		"finalizer-authorization",
		"realm-authorization",
		"plan-chunk-authorization",
		"webhook-configuration",
		"secret-isolation",
		"namespace-local-references",
		"verification-policy",
		"pod-webhook-outage-scope",
		"duration-bounds",
		"reference-keys",
		"managed-scope-selectors",
		"unsupported-engine",
		"suspended-schema-fixture",
		"approval-binding",
		"cross-namespace-approval",
	},
})

// CertRotationInputs is what the driver hands the certificate rotation phase.
type CertRotationInputs struct {
	// Kubeconfig names the cluster the driver stood up.
	Kubeconfig string `env:"E2E_KUBECONFIG"`
	// OperatorNamespace is the release namespace, where the manager, the
	// rotator, their Secrets and the webhook Service live.
	OperatorNamespace string `env:"E2E_OPERATOR_NAMESPACE"`
	// TestNamespace holds the approval the control-plane contract leaves
	// behind. The phase reaches approval admission through it.
	TestNamespace string `env:"E2E_TEST_NAMESPACE"`
	// HelmRelease is the installed release.
	HelmRelease string `env:"E2E_HELM_RELEASE"`
	// ChartPackage is the packaged chart the release was installed from. The
	// phase upgrades to it so that the chart's live lookup of the certificate
	// runs against a cluster that has one.
	ChartPackage string `env:"E2E_CHART_PACKAGE"`
}

// CertRotation proves certificate rotation: the exact recovery guard, Helm's
// live lookup, recovery from a corrupt CA and recreation of a deleted Secret.
var CertRotation = define[CertRotationInputs](Phase{
	Name:    "cert-rotation",
	Test:    "TestCertRotation",
	Timeout: 100 * time.Minute,
	Scenarios: []string{
		"recovery-guard",
		"live-helm-lookup",
		"corrupt-ca-recovery",
		"missing-secret-recreation",
	},
})

// DataPlaneInputs is what the driver hands the data-plane phase.
type DataPlaneInputs struct {
	// Kubeconfig names the cluster the driver stood up.
	Kubeconfig string `env:"E2E_KUBECONFIG"`
	// OperatorNamespace is the release namespace.
	OperatorNamespace string `env:"E2E_OPERATOR_NAMESPACE"`
	// TestNamespace is where the phase stands up its registry endpoint, its
	// databases and its fixtures, and where the migration suites find them.
	TestNamespace string `env:"E2E_TEST_NAMESPACE"`
	// HelmRelease is the installed release.
	HelmRelease string `env:"E2E_HELM_RELEASE"`
	// ChartPackage is the packaged chart the release was installed from. The
	// four-eyes row upgrades the release to it with one value changed.
	ChartPackage string `env:"E2E_CHART_PACKAGE"`
	// PtahVersion is the Ptah version bound beside the executor.
	PtahVersion string `env:"E2E_PTAH_VERSION"`
	// ExecutorImage is the digest-pinned Ptah executor the release runs.
	ExecutorImage string `env:"E2E_EXECUTOR_IMAGE"`
	// RunnerImage is the runner image built beside the manager.
	RunnerImage string `env:"E2E_RUNNER_IMAGE"`
	// FixtureImage carries the TLS registry proxy and the fault fixtures.
	FixtureImage string `env:"E2E_FIXTURE_IMAGE"`
	// ControllerImage is the candidate manager image, pinned by digest.
	ControllerImage string `env:"E2E_CONTROLLER_IMAGE"`
	// ControllerRevision is the commit the candidate was built from.
	ControllerRevision string `env:"E2E_CONTROLLER_REVISION"`
	// ControllerStateVersion is the controller-state version the chart was
	// stamped with.
	ControllerStateVersion string `env:"E2E_CONTROLLER_STATE_VERSION"`
	// PostgresImage and MySQLImage are the database servers the phase runs in
	// the test namespace.
	PostgresImage string `env:"E2E_POSTGRES_IMAGE"`
	MySQLImage    string `env:"E2E_MYSQL_IMAGE"`
	// RegistryIP is the registry container's address on the kind network,
	// RegistryService the Service that routes to it, and RegistryPort the
	// host port the driver published it on.
	RegistryIP      string `env:"E2E_REGISTRY_IP"`
	RegistryService string `env:"E2E_REGISTRY_SERVICE"`
	RegistryPort    string `env:"E2E_REGISTRY_PORT"`
	// RegistryCredentialsFile holds the registry's username and password.
	RegistryCredentialsFile string `env:"E2E_REGISTRY_CREDENTIALS_FILE"`
	// DockerContext is the remote Docker daemon the registry and the external
	// PostgreSQL run on, and RegistryContainerID the registry container the
	// outage row stops.
	DockerContext       string `env:"E2E_DOCKER_CONTEXT"`
	RegistryContainerID string `env:"E2E_REGISTRY_CONTAINER_ID"`
	// The external PostgreSQL: a container outside Kubernetes that a
	// selectorless Service routes to.
	ExternalPostgresContainerID     string `env:"E2E_EXTERNAL_POSTGRES_CONTAINER_ID"`
	ExternalPostgresIP              string `env:"E2E_EXTERNAL_POSTGRES_IP"`
	ExternalPostgresService         string `env:"E2E_EXTERNAL_POSTGRES_SERVICE"`
	ExternalPostgresImage           string `env:"E2E_EXTERNAL_POSTGRES_IMAGE"`
	ExternalPostgresOwner           string `env:"E2E_EXTERNAL_POSTGRES_OWNER"`
	ExternalPostgresCredentialsFile string `env:"E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE"`
	// The authenticated HTTPS registry proxy: its Service and the CA,
	// certificate and key the driver generated for it.
	TLSProxyService  string `env:"E2E_TLS_PROXY_SERVICE"`
	TLSProxyCAFile   string `env:"E2E_TLS_PROXY_CA_FILE"`
	TLSProxyCertFile string `env:"E2E_TLS_PROXY_CERT_FILE"`
	TLSProxyKeyFile  string `env:"E2E_TLS_PROXY_KEY_FILE"`
	// Mode is full or prepare. The migration suites run the phase in prepare,
	// for the namespace it stands up and none of its own acceptance.
	Mode string `env:"E2E_DATAPLANE_MODE"`
}

// DataPlane proves both engines end to end: the registry, the databases and
// the admission fixtures it stands up, the PostgreSQL, external PostgreSQL and
// MySQL lifecycles, the refusals, the restart and fault injection, and the
// four-eyes and Pod-metadata rows.
var DataPlane = define[DataPlaneInputs](Phase{
	Name:    "dataplane",
	Test:    "TestDataPlane",
	Timeout: 150 * time.Minute,
	Scenarios: []string{
		"databases-and-fixtures",
		"postgresql-lifecycle",
		"external-postgresql-lifecycle",
		"mysql-lifecycle",
		"mysql-dsn-refusal",
		"watches",
		"job-deadline",
		"manager-restart",
		"runner-termination",
		"job-deletion",
		"closing-audits",
		"four-eyes-distinct-approver",
		"pod-metadata-admission",
	},
	Preparation: 1,
})

// all is every phase the harness carries, in the order the driver runs them.
var all = []Phase{
	ControlPlane.Phase,
	CertRotation.Phase,
	DataPlane.Phase,
}

func init() {
	if err := distinct(all); err != nil {
		panic(fmt.Sprintf("phases: %v", err))
	}
}

// distinct refuses two phases with one name or one test: the driver selects a
// phase by name and the binary runs it by test, so either collision runs the
// wrong code under the right name.
func distinct(phases []Phase) error {
	names, tests := map[string]bool{}, map[string]bool{}
	for _, phase := range phases {
		if names[phase.Name] {
			return fmt.Errorf("phase %s is declared twice", phase.Name)
		}
		if tests[phase.Test] {
			return fmt.Errorf("test %s is two phases", phase.Test)
		}
		names[phase.Name], tests[phase.Test] = true, true
	}
	return nil
}

// All returns every phase the harness carries.
func All() []Phase {
	return slices.Clone(all)
}

// Lookup returns the phase the driver knows by name.
func Lookup(name string) (Phase, bool) {
	for _, phase := range all {
		if phase.Name == name {
			return phase, true
		}
	}
	return Phase{}, false
}

// Inputs names the environment variables the phase reads, in the order its
// input struct declares them.
func (p Phase) Inputs() []string {
	names := make([]string, 0, p.inputs.NumField())
	for index := range p.inputs.NumField() {
		names = append(names, p.inputs.Field(index).Tag.Get(environmentTag))
	}
	return names
}

// Load reads the phase's inputs through lookup, which is os.LookupEnv outside
// a test. Every input is required and has to be non-empty: the driver binds
// each one, and a phase started without one would measure some other cluster
// or none. The error names every input that is missing, not only the first.
func Load[T any](p Of[T], lookup func(string) (string, bool)) (T, error) {
	var inputs T
	value := reflect.ValueOf(&inputs).Elem()
	var missing []string
	for index := range value.NumField() {
		name := value.Type().Field(index).Tag.Get(environmentTag)
		found, ok := lookup(name)
		if !ok || found == "" {
			missing = append(missing, name)
			continue
		}
		value.Field(index).SetString(found)
	}
	if len(missing) > 0 {
		return inputs, fmt.Errorf("the %s phase reads %s, and the driver bound nothing to %s",
			p.Name, strings.Join(missing, ", "), pronoun(len(missing)))
	}
	return inputs, nil
}

const environmentTag = "env"

var (
	labelPattern       = regexp.MustCompile(`^[a-z][a-z0-9-]*[a-z0-9]$`)
	testPattern        = regexp.MustCompile(`^Test[A-Z][A-Za-z0-9]*$`)
	environmentPattern = regexp.MustCompile(`^E2E_[A-Z0-9_]+$`)
)

// define checks a declaration once, when the package loads, so a malformed
// phase fails every program that reads this package rather than the run that
// would have reached it.
func define[T any](p Phase) Of[T] {
	p.inputs = reflect.TypeFor[T]()
	if err := p.validate(); err != nil {
		panic(fmt.Sprintf("phases: %v", err))
	}
	return Of[T]{Phase: p}
}

func (p Phase) validate() error {
	if !labelPattern.MatchString(p.Name) {
		return fmt.Errorf("%q is not a usable phase name", p.Name)
	}
	if !testPattern.MatchString(p.Test) {
		return fmt.Errorf("phase %s: %q is not a test function name", p.Name, p.Test)
	}
	if p.Timeout <= 0 {
		return fmt.Errorf("phase %s: runs with no bound", p.Name)
	}
	if len(p.Scenarios) == 0 {
		return fmt.Errorf("phase %s: records no scenario, so the ledger would name it and nothing inside it", p.Name)
	}
	seen := map[string]bool{}
	for _, scenario := range p.Scenarios {
		if !labelPattern.MatchString(scenario) {
			return fmt.Errorf("phase %s: %q is not a usable scenario name", p.Name, scenario)
		}
		if seen[scenario] {
			return fmt.Errorf("phase %s: scenario %s is declared twice", p.Name, scenario)
		}
		seen[scenario] = true
	}
	if p.Preparation < 0 || p.Preparation >= len(p.Scenarios) {
		return fmt.Errorf("phase %s: a preparation of %d scenarios leaves none of its own acceptance out of %d",
			p.Name, p.Preparation, len(p.Scenarios))
	}
	if p.inputs.Kind() != reflect.Struct || p.inputs.NumField() == 0 {
		return fmt.Errorf("phase %s: its inputs must be a struct of environment variables", p.Name)
	}
	names := map[string]bool{}
	for index := range p.inputs.NumField() {
		field := p.inputs.Field(index)
		name := field.Tag.Get(environmentTag)
		switch {
		case !field.IsExported():
			return fmt.Errorf("phase %s: input %s is unexported, so it cannot be loaded", p.Name, field.Name)
		case field.Type.Kind() != reflect.String:
			return fmt.Errorf("phase %s: input %s is not a string", p.Name, field.Name)
		case !environmentPattern.MatchString(name):
			return fmt.Errorf("phase %s: input %s names %q, which is not an E2E_ variable", p.Name, field.Name, name)
		case names[name]:
			return fmt.Errorf("phase %s: %s is read into two inputs", p.Name, name)
		}
		names[name] = true
	}
	return nil
}

func pronoun(count int) string {
	if count == 1 {
		return "it"
	}
	return "them"
}
