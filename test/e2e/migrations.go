package e2e

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// The migration phase's fixtures and bounds.
const (
	// migrationDatabaseUser is the unprivileged user the migration Jobs
	// connect as: the data plane's application user, which owns only the
	// databases a row grants it.
	migrationDatabaseUser = "ptah_e2e"
	// migrationPolicy is the verification policy the phase applies. It names
	// the migration artifact type, where the schema path's names the schema
	// type, so a schema artifact cannot stand in for a migration directory.
	migrationPolicy       = "e2e-migrations-verification-policy"
	migrationPolicyKey    = "policy.yaml"
	migrationArtifactType = "application/vnd.stokaro.ptah.migrations.v1"
	// migrationInterval is the reconcile interval of the main resource.
	migrationInterval = "5m"
	// migrationPoll is how often a wait reads a migration.
	migrationPoll = 5 * time.Second
	// applyGateLabel is the node label a proof gates an Apply Pod on: the
	// Pods of a resource whose nodeSelector names it schedule only while the
	// label is on the nodes.
	applyGateLabel = "operator.ptah.run/e2e-apply-gate"
	// isolationNodeKey is the label and taint key of the node the isolated
	// node proof cuts off from the API server, and isolationRuleComment the
	// comment its rules carry, so removing them finds exactly these.
	isolationNodeKey     = "operator.ptah.run/e2e-isolation"
	isolationRuleComment = "ptah-e2e-isolated-node"
	// migrationOperationComponent is the component label every migration
	// operation Job carries.
	migrationOperationComponent = "migration-operation"
	labelMigration              = "operator.ptah.run/migration"
	migrationFinalizer          = "operator.ptah.run/migration-operation"
)

var (
	// rerunMarker is the suffix hack/e2e-rerun-phase.sh gives a rerun.
	rerunMarker = regexp.MustCompile(`^r[0-9]+$`)
	kindCluster = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	// migrationSQLText is SQL a status, a plan or a view must never carry.
	migrationSQLText = regexp.MustCompile(`(?i)(create[[:space:]]+table|alter[[:space:]]+table|insert[[:space:]]+into)`)
)

// migrationEngine is what one engine's lifecycle needs from the data plane.
type migrationEngine struct {
	// name is postgresql or mysql; kind is the engine as the API spells it.
	name, kind string
	// service is the database Deployment and Service, and sourceSecret the
	// data plane's Secret the phase reads the application password from.
	service, sourceSecret string
	// port is the database's port on the Service.
	port string
}

// migrationEngineFor names the engine a phase runs.
func migrationEngineFor(engine string) (migrationEngine, error) {
	switch engine {
	case "postgresql":
		return migrationEngine{name: engine, kind: "PostgreSQL", service: pgService, sourceSecret: pgSecret, port: "5432"}, nil
	case "mysql":
		return migrationEngine{name: engine, kind: "MySQL", service: mysqlService, sourceSecret: mysqlSecret, port: "3306"}, nil
	}
	return migrationEngine{}, fmt.Errorf("E2E_ENGINE must name postgresql or mysql, and names %q", engine)
}

// migrationRepository is where the phase publishes: its own repository on a
// rerun, because Ptah refuses to move a version tag that already names
// another digest, and the fixtures do not publish to the same bytes twice.
func migrationRepository(rerun string) (string, error) {
	if rerun == "" {
		return "migrations", nil
	}
	if !rerunMarker.MatchString(rerun) {
		return "", fmt.Errorf("E2E_PHASE_RERUN must be r followed by digits, not %s", rerun)
	}
	return "migrations-" + rerun, nil
}

// databaseURL is the URL an operation Pod reaches a database of the engine
// by, as a user.
func (e migrationEngine) databaseURL(namespace, user, password, database string) string {
	authority := e.service + "." + namespace + ".svc.cluster.local"
	if e.name == "mysql" {
		return "mysql://" + user + ":" + password + "@tcp(" + authority + ":3306)/" + database
	}
	return "postgres://" + user + ":" + password + "@" + authority + ":5432/" + database + "?sslmode=disable"
}

// currentSchemaFilter is how information_schema names the current database's
// tables on the engine. MySQL's catalog spans the whole server, so an
// unfiltered count there would answer for another phase's database.
func (e migrationEngine) currentSchemaFilter() string {
	if e.name == "mysql" {
		return "table_schema=DATABASE()"
	}
	return "table_schema='public'"
}

// realmDigest is the coordination digest of a resource that names a
// PtahRealm: the canonical engine and the realm's name, and no namespace,
// because every namespace the realm admits has to meet in one census and one
// Lease. The field order is internal/fingerprint's; a unit test holds the two
// to one value.
func realmDigest(engine, realm string) (string, error) {
	canonical, err := canonicalJSON(struct {
		ContractVersion int    `json:"contract_version"`
		Engine          string `json:"engine"`
		Realm           string `json:"realm"`
	}{1, engine, realm})
	if err != nil {
		return "", err
	}
	return sha256Digest(canonical), nil
}

// carriesMigrationSQL reports whether any string in a document reads as a
// migration statement.
func carriesMigrationSQL(document any) bool {
	return holdsMatch(document, migrationSQLText)
}

// holdsMatch walks a decoded JSON document and reports whether any string in
// it matches, as jq's [.. | strings | select(test(...))] did.
func holdsMatch(document any, pattern *regexp.Regexp) bool {
	switch value := document.(type) {
	case string:
		return pattern.MatchString(value)
	case []any:
		for _, item := range value {
			if holdsMatch(item, pattern) {
				return true
			}
		}
	case map[string]any:
		for _, item := range value {
			if holdsMatch(item, pattern) {
				return true
			}
		}
	}
	return false
}

// validKindCluster holds the kind cluster name to a DNS label, which is what
// the isolation worker's node container is named from.
func validKindCluster(name string) error {
	if !kindCluster.MatchString(name) {
		return fmt.Errorf("E2E_KIND_CLUSTER_NAME must be a DNS label, and names %q", name)
	}
	return nil
}

// migrationInputsOK is the refusal the migration phase gives before it reads
// the cluster.
func migrationInputsOK(engine, phaseEngine string, images ...string) error {
	if engine != phaseEngine {
		return fmt.Errorf("E2E_ENGINE names %q, and this phase runs %s", engine, phaseEngine)
	}
	for _, image := range images {
		if !digestPinnedImage.MatchString(image) {
			return fmt.Errorf("migration phase images must be pinned by a lowercase SHA-256 digest: %s", image)
		}
	}
	return nil
}

// errNoPassword is a data plane Secret without the application password.
var errNoPassword = errors.New("the data plane Secret carries no password")

// trimmedSQL is sql_value: the output with every whitespace character
// removed, since the two clients pad and terminate a value differently.
func trimmedSQL(output string) string {
	return strings.Join(strings.Fields(output), "")
}

// blockedRefusalHeld is the refusal
// a stopped migration holds, as distinct from the phase it passes through
// while holding it. A resource that has stopped still resolves, verifies and
// reads its history at its interval, so it is legitimately out of Blocked for
// part of every cycle. What may never lapse is the refusal itself: the
// condition, the absence of a plan, and the absence of a Ready that would
// invite work. jq cannot iterate absent conditions, so a status without them
// holds nothing.
func blockedRefusalHeld(status ptahv1alpha1.PtahMigrationStatus) bool {
	if status.Conditions == nil {
		return false
	}
	return conditionStatus(status.Conditions, ptahv1alpha1.ConditionMigrationBlocked, metav1.ConditionTrue) &&
		status.Plan == nil &&
		!conditionStatus(status.Conditions, ptahv1alpha1.ConditionMigrationReady, metav1.ConditionTrue)
}

// gatedApplyPods is the Apply Pods the
// scheduling gate is holding off every node. The claim is that the gate, and
// nothing else, keeps the run from starting, so an empty list does not satisfy
// it: a Job whose Pod does not exist yet cannot be told from one whose Pod
// cannot be placed. Pending is not enough either, since a Pod bound to a node
// and pulling its image is Pending too; no Pod may have a node. And the
// selector has to be on the Pod, or a builder that stopped propagating it
// would pass in the moment before the scheduler placed its Pod.
func gatedApplyPods(pods []corev1.Pod) bool {
	if len(pods) == 0 {
		return false
	}
	for index := range pods {
		pod := &pods[index]
		if pod.Status.Phase != corev1.PodPending || pod.Spec.NodeName != "" || pod.Spec.NodeSelector[applyGateLabel] != "open" {
			return false
		}
	}
	return true
}

// conditionSummary is each condition's type, status and reason, without the
// message a diagnostic has no need for.
func conditionSummary(conditions []metav1.Condition) []map[string]string {
	summary := make([]map[string]string, 0, len(conditions))
	for _, condition := range conditions {
		summary = append(summary, map[string]string{
			"type": condition.Type, "status": string(condition.Status), "reason": condition.Reason,
		})
	}
	return summary
}

// sqlLines splits a multi-row query answer into its rows.
func sqlLines(output string) []string {
	var lines []string
	for line := range strings.SplitSeq(strings.TrimRight(output, "\n"), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
