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
	// IsolatesNode says the full phase faults the isolation worker's API
	// connection, so only a suite that declares that worker may run it.
	// These faults run after the Preparation boundary, when one exists.
	IsolatesNode bool
	// Preparation is how many of the leading scenarios make up the phase's
	// preparation mode: what another suite runs it for, the objects it stands
	// up, and none of its own acceptance. Zero means the phase has no such
	// mode. A run that stopped at the boundary proved those scenarios alone.
	Preparation int
	// RequiresFull names earlier phases whose completed acceptance leaves
	// fixtures this phase uses. These dependencies apply when this phase runs
	// in full; its preparation prefix must stand on its own. Running a required
	// phase only through its own preparation boundary is insufficient.
	RequiresFull []string

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
	Name:         "cert-rotation",
	Test:         "TestCertRotation",
	Timeout:      100 * time.Minute,
	RequiresFull: []string{"assert"},
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
	Name:         "dataplane",
	Test:         "TestDataPlane",
	Timeout:      150 * time.Minute,
	IsolatesNode: true,
	RequiresFull: []string{"assert"},
	Scenarios: []string{
		"databases-and-fixtures",
		"postgresql-lifecycle",
		"external-postgresql-lifecycle",
		"mysql-lifecycle",
		"mysql-dsn-refusal",
		"watches",
		"approval-resource-replacement",
		"approval-target-secret-change",
		"approval-destructive-policy-change",
		"approval-exclusion-policy-change",
		"approval-verification-policy-change",
		"mysql-drift-before-dispatch",
		"hung-schema-result-read",
		"job-deadline",
		"manager-restart",
		"runner-termination",
		"job-deletion",
		"closing-audits",
		"four-eyes-distinct-approver",
		"pod-metadata-admission",
		"approval-executor-image-change",
		"running-apply-executor-image-change",
	},
	Preparation: 1,
})

// MigrationsInputs is what the driver hands the migration phases. Each phase
// runs one engine, and the driver names it: a phase that fell back to one
// engine because nobody named one would be coverage nobody noticed was gone.
type MigrationsInputs struct {
	// Kubeconfig names the cluster the driver stood up.
	Kubeconfig string `env:"E2E_KUBECONFIG"`
	// TestNamespace is the namespace the data plane stood up in preparation:
	// its registry Service, its databases and its admission fixtures.
	TestNamespace string `env:"E2E_TEST_NAMESPACE"`
	// ExecutorImage publishes the migration artifacts, with the command a
	// person would use.
	ExecutorImage string `env:"E2E_EXECUTOR_IMAGE"`
	// RunnerImage is the runner image the migration Jobs carry.
	RunnerImage string `env:"E2E_RUNNER_IMAGE"`
	// ControllerImage is the candidate manager image, pinned by digest.
	ControllerImage string `env:"E2E_CONTROLLER_IMAGE"`
	// ControllerRevision is the commit the candidate was built from.
	ControllerRevision string `env:"E2E_CONTROLLER_REVISION"`
	// ControllerStateVersion is the controller-state version the chart was
	// stamped with.
	ControllerStateVersion string `env:"E2E_CONTROLLER_STATE_VERSION"`
	// RegistryService is the Service the cluster reaches the registry by.
	RegistryService string `env:"E2E_REGISTRY_SERVICE"`
	// RegistryHostAddress is the same registry as the host reaches it, for the
	// one artifact no product command can produce.
	RegistryHostAddress string `env:"E2E_REGISTRY_HOST_ADDRESS"`
	// RegistryCredentialsFile holds the registry's username and password.
	RegistryCredentialsFile string `env:"E2E_REGISTRY_CREDENTIALS_FILE"`
	// DockerContext and KindClusterName reach the isolation worker's node
	// container, which the phase cuts off from the API server.
	DockerContext   string `env:"E2E_DOCKER_CONTEXT"`
	KindClusterName string `env:"E2E_KIND_CLUSTER_NAME"`
	// Engine is postgresql or mysql, and has to be the phase's own.
	Engine string `env:"E2E_ENGINE"`
}

// MigrationsPostgreSQL proves the versioned-migration path against
// PostgreSQL: the approval gate, the applied sequence, the history it leaves,
// and every refusal and fault the path owes a person.
var MigrationsPostgreSQL = define[MigrationsInputs](Phase{
	Name:    "migrations-postgresql",
	Test:    "TestMigrationsPostgreSQL",
	Timeout: 150 * time.Minute,
	Scenarios: []string{
		"migration-policy",
		"postgresql-migrations",
		"approval-resource-replacement",
		"approval-policy-change",
		"approval-transaction-mode-change",
		"approval-artifact-change",
		"approval-verification-policy-uid-change",
		"approval-verification-policy-content-change",
		"approval-executor-image-change",
		"running-apply-executor-image-change",
	},
	// The isolated-node row cuts the isolation worker off from the API
	// server, so only a suite that declares the worker may run the phase.
	IsolatesNode: true,
})

// MigrationsMySQL proves the same path against MySQL, after the row that
// says which transaction mode a MySQL sequence may name.
var MigrationsMySQL = define[MigrationsInputs](Phase{
	Name:    "migrations-mysql",
	Test:    "TestMigrationsMySQL",
	Timeout: 150 * time.Minute,
	Scenarios: []string{
		"migration-policy",
		"mysql-transaction-mode",
		"mysql-migrations",
		"approval-resource-replacement",
		"approval-policy-change",
		"approval-transaction-mode-change",
		"approval-artifact-change",
		"approval-verification-policy-uid-change",
		"approval-verification-policy-content-change",
		"approval-executor-image-change",
		"running-apply-executor-image-change",
	},
	// The isolated-node row cuts the isolation worker off from the API
	// server, so only a suite that declares the worker may run the phase.
	IsolatesNode: true,
})

// ReferenceDataInputs is what the driver hands the reference-data phases.
// They run in the namespace the data plane stood up, on its database servers,
// each on a database of its own that no other phase touched.
type ReferenceDataInputs struct {
	// Kubeconfig names the cluster the driver stood up.
	Kubeconfig string `env:"E2E_KUBECONFIG"`
	// TestNamespace is the namespace the data plane stood up in preparation:
	// its registry Service, its databases and its registry credentials.
	TestNamespace string `env:"E2E_TEST_NAMESPACE"`
	// OperatorNamespace is the release namespace, whose controller log the
	// phase scans for declared row values.
	OperatorNamespace string `env:"E2E_OPERATOR_NAMESPACE"`
	// ExecutorImage publishes the reference-data artifacts, with the command a
	// person would use.
	ExecutorImage string `env:"E2E_EXECUTOR_IMAGE"`
	// RunnerImage is the runner image the operation Jobs carry.
	RunnerImage string `env:"E2E_RUNNER_IMAGE"`
	// RegistryService is the Service the cluster reaches the registry by.
	RegistryService string `env:"E2E_REGISTRY_SERVICE"`
	// Engine is postgresql or mysql, and has to be the phase's own.
	Engine string `env:"E2E_ENGINE"`
}

// ReferenceDataPostgreSQL proves declared reference data on PostgreSQL, from
// a database with no tables: the first rows, a data-only change, a stale
// approval after an external edit, a withdrawn and an emptied declaration, a
// protected table, and no row value anywhere but the database.
var ReferenceDataPostgreSQL = define[ReferenceDataInputs](Phase{
	Name:    "reference-data-postgresql",
	Test:    "TestReferenceDataPostgreSQL",
	Timeout: 60 * time.Minute,
	Scenarios: []string{
		"declared-row-values",
		"postgresql-reference-data",
	},
})

// ReferenceDataMySQL proves the same on MySQL.
var ReferenceDataMySQL = define[ReferenceDataInputs](Phase{
	Name:    "reference-data-mysql",
	Test:    "TestReferenceDataMySQL",
	Timeout: 60 * time.Minute,
	Scenarios: []string{
		"declared-row-values",
		"mysql-reference-data",
	},
})

// AlertingInputs is what the driver hands the alerting phase, which runs last
// in the PostgreSQL migrations suite, on the cluster that suite leaves.
type AlertingInputs struct {
	// Kubeconfig names the cluster the driver stood up.
	Kubeconfig string `env:"E2E_KUBECONFIG"`
	// OperatorNamespace is the release namespace.
	OperatorNamespace string `env:"E2E_OPERATOR_NAMESPACE"`
	// TestNamespace holds the existing approval used for a dry-run admission probe.
	TestNamespace string `env:"E2E_TEST_NAMESPACE"`
	// HelmRelease is the installed release, whose values the rules are
	// rendered with.
	HelmRelease string `env:"E2E_HELM_RELEASE"`
	// ChartPackage is the chart the release was installed from.
	ChartPackage string `env:"E2E_CHART_PACKAGE"`
	// FixtureImage carries the alert receiver.
	FixtureImage string `env:"E2E_FIXTURE_IMAGE"`
	// PrometheusImage and AlertmanagerImage are the monitoring path, mirrored
	// into the registry and pinned by digest.
	PrometheusImage   string `env:"E2E_PROMETHEUS_IMAGE"`
	AlertmanagerImage string `env:"E2E_ALERTMANAGER_IMAGE"`
	// RegistryCredentialsFile holds the registry's username and password,
	// which the monitoring Pods pull their images with.
	RegistryCredentialsFile string `env:"E2E_REGISTRY_CREDENTIALS_FILE"`
}

// Alerting proves the path from a manager's metrics to a person: an Apply
// nobody accounted for, an operation that stops moving, a failed leader
// scrape, certificate expiry and admission failure, and every manager gone each reach a receiver.
// Recoverable faults clear.
var Alerting = define[AlertingInputs](Phase{
	Name:         "alerting",
	Test:         "TestAlerting",
	Timeout:      80 * time.Minute,
	RequiresFull: []string{"assert"},
	Scenarios: []string{
		"monitoring-path",
		"unresolved-apply",
		"stalled-operation",
		"lost-scrape-target",
		"certificate-expiry",
		"lost-view",
	},
})

// UpgradeInputs is what the driver hands the upgrade phase, which proves the
// CRD upgrade path of the release the driver installed.
type UpgradeInputs struct {
	// Kubeconfig names the cluster the driver stood up.
	Kubeconfig string `env:"E2E_KUBECONFIG"`
	// DebugLogs is 1 to print the stderr of a refused Helm operation, and 0
	// otherwise. The driver refuses 1 on CI, where the run log is public.
	DebugLogs string `env:"E2E_DEBUG_LOGS"`
	// OperatorNamespace is the release namespace.
	OperatorNamespace string `env:"E2E_OPERATOR_NAMESPACE"`
	// ProofNamespace holds the schema, plan and approval whose preservation
	// the phase proves across every CRD change.
	ProofNamespace string `env:"E2E_PROOF_NAMESPACE"`
	// HelmRelease is the installed release.
	HelmRelease string `env:"E2E_HELM_RELEASE"`
	// ChartPackage is the chart the release was installed from.
	ChartPackage string `env:"E2E_CHART_PACKAGE"`
	// CandidateValuesFile is the values file the release was installed with.
	CandidateValuesFile string `env:"E2E_CANDIDATE_VALUES_FILE"`
	// ControllerImage is the candidate manager image, pinned by digest.
	ControllerImage string `env:"E2E_CONTROLLER_IMAGE"`
	// KubernetesVersion is the exact version the cluster has to report.
	KubernetesVersion string `env:"E2E_KUBERNETES_VERSION"`
}

// Upgrade proves the CRD upgrade path: a CRD preflight that refuses a missing
// CRD, a newer schema or state and an incomplete or colliding identity without
// changing anything, drifted CRDs converged before the manager rolls out with
// every live object preserved, the manager's write guards, and one release
// per namespace.
var Upgrade = define[UpgradeInputs](Phase{
	Name:    "upgrade",
	Test:    "TestUpgrade",
	Timeout: 120 * time.Minute,
	Scenarios: []string{
		"read-only-job-cleanup",
		"crd-preflight-refusals",
		"drifted-crd-upgrade",
		"controller-write-guard",
		"runtime-recovery",
		"singleton-coordination",
	},
})

// HAInputs is what the driver hands the high-availability phase.
type HAInputs struct {
	// Kubeconfig names the cluster the driver stood up.
	Kubeconfig string `env:"E2E_KUBECONFIG"`
	// OperatorNamespace is the release namespace.
	OperatorNamespace string `env:"E2E_OPERATOR_NAMESPACE"`
	// HATestNamespace is the namespace the phase creates its operation in,
	// and removes.
	HATestNamespace string `env:"E2E_HA_TEST_NAMESPACE"`
	// ForeignNamespace and ProofNamespace are namespaces the manager's Lease
	// grant must not reach.
	ForeignNamespace string `env:"E2E_FOREIGN_NAMESPACE"`
	ProofNamespace   string `env:"E2E_PROOF_NAMESPACE"`
	// HelmRelease is the installed release.
	HelmRelease string `env:"E2E_HELM_RELEASE"`
	// RegistryCredentialsFile holds the registry's username and password,
	// which the operation Pod pulls its images with.
	RegistryCredentialsFile string `env:"E2E_REGISTRY_CREDENTIALS_FILE"`
}

// HA proves leader election across two manager replicas: one namespaced
// Lease with exact RBAC, a failover that keeps the Lease and moves its holder,
// and an operation the new leader admits and reports in its metrics.
var HA = define[HAInputs](Phase{
	Name:    "ha",
	Test:    "TestHA",
	Timeout: 45 * time.Minute,
	Scenarios: []string{
		"lease-authorization",
		"leader-failover",
		"operation-after-failover",
	},
})

// UninstallInputs is what the driver hands the uninstall phase, which runs
// last on the cluster the upgrade phase leaves.
type UninstallInputs struct {
	// Kubeconfig names the cluster the driver stood up.
	Kubeconfig string `env:"E2E_KUBECONFIG"`
	// DebugLogs is 1 to print the stderr of a refused Helm operation, and 0
	// otherwise.
	DebugLogs string `env:"E2E_DEBUG_LOGS"`
	// OperatorNamespace is the release namespace.
	OperatorNamespace string `env:"E2E_OPERATOR_NAMESPACE"`
	// ProofNamespace holds the objects the upgrade phase created, which every
	// step here has to leave exactly as they were.
	ProofNamespace string `env:"E2E_PROOF_NAMESPACE"`
	// HelmRelease is the installed release.
	HelmRelease string `env:"E2E_HELM_RELEASE"`
	// ChartPackage and CandidateValuesFile are the current release, which the
	// phase rolls back to and installs again from its exact bytes.
	ChartPackage        string `env:"E2E_CHART_PACKAGE"`
	CandidateValuesFile string `env:"E2E_CANDIDATE_VALUES_FILE"`
	// ControllerImage is the candidate manager image, pinned by digest.
	ControllerImage string `env:"E2E_CONTROLLER_IMAGE"`
	// NextChartPackage, NextValuesFile and NextControllerImage are the
	// synthetic next release the phase upgrades to.
	NextChartPackage    string `env:"E2E_NEXT_CHART_PACKAGE"`
	NextValuesFile      string `env:"E2E_NEXT_VALUES_FILE"`
	NextControllerImage string `env:"E2E_NEXT_CONTROLLER_IMAGE"`
	// KubernetesVersion is the exact version the cluster has to report.
	KubernetesVersion string `env:"E2E_KUBERNETES_VERSION"`
	// RegistryCredentialsFile holds the registry's username and password,
	// which the running Apply's Pod pulls its images with.
	RegistryCredentialsFile string `env:"E2E_REGISTRY_CREDENTIALS_FILE"`
	// DockerContext and ExternalPostgresContainerID reach the external
	// PostgreSQL, where the running Apply's barrier holds its lock.
	DockerContext               string `env:"E2E_DOCKER_CONTEXT"`
	ExternalPostgresContainerID string `env:"E2E_EXTERNAL_POSTGRES_CONTAINER_ID"`
	// ExternalPostgresIP and ExternalPostgresCredentialsFile are the address
	// and credentials the running Apply connects with.
	ExternalPostgresIP              string `env:"E2E_EXTERNAL_POSTGRES_IP"`
	ExternalPostgresCredentialsFile string `env:"E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE"`
}

// Uninstall proves the release transitions after the upgrade: a synthetic
// next release that recovers from a late failure without interrupting a
// running Apply, a rollback refused over future state and then allowed, and
// an uninstall that removes the runtime and keeps the CRDs and live objects,
// followed by a reinstall over drifted CRDs and a fresh install of the exact
// exported chart bytes.
var Uninstall = define[UninstallInputs](Phase{
	Name:    "uninstall",
	Test:    "TestUninstall",
	Timeout: 150 * time.Minute,
	Scenarios: []string{
		"next-release-upgrade",
		"rollback",
		"uninstall",
		"reinstall-over-retained-crds",
		"exported-chart-install",
	},
})

// all is every phase the harness carries, in the order the driver runs them.
var all = []Phase{
	Upgrade.Phase,
	HA.Phase,
	ControlPlane.Phase,
	CertRotation.Phase,
	DataPlane.Phase,
	MigrationsPostgreSQL.Phase,
	MigrationsMySQL.Phase,
	ReferenceDataPostgreSQL.Phase,
	ReferenceDataMySQL.Phase,
	Alerting.Phase,
	Uninstall.Phase,
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
	required := map[string]bool{}
	for _, phase := range p.RequiresFull {
		if !labelPattern.MatchString(phase) || phase == p.Name || required[phase] {
			return fmt.Errorf("phase %s: invalid or repeated prerequisite %q", p.Name, phase)
		}
		required[phase] = true
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
