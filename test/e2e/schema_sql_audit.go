package e2e

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

//go:embed schema-sql-contract.json
var schemaSQLContract []byte

type schemaSQLKey struct {
	operation, command, statement, parameters string
}

type schemaSQLPolicy struct {
	engine, database string
	allowed          map[schemaSQLKey]bool
}

// Statements and parameters are matched byte for byte. Only the declaration
// expands its MySQL database placeholder; incoming SQL is never normalized.
// The fixed queries and their source review are recorded beside the readings.
func newSchemaSQLPolicy(engine, database string) (*schemaSQLPolicy, error) {
	if (engine != "postgresql" && engine != "mysql") || !mysqlAuditIdentifier.MatchString(database) || len(database) > 63 {
		return nil, errors.New("schema SQL audit needs a declared engine and isolated database")
	}
	var contract struct {
		SchemaVersion int    `json:"schemaVersion"`
		PtahCommit    string `json:"ptahCommit"`
		Scope         string `json:"scope"`
		Statements    []struct {
			Engine     string   `json:"engine"`
			Operations []string `json:"operations"`
			Command    string   `json:"command"`
			Statement  string   `json:"statement"`
			Parameters string   `json:"parameters"`
		} `json:"statements"`
	}
	decoder := json.NewDecoder(bytes.NewReader(schemaSQLContract))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&contract); err != nil || contract.SchemaVersion != 1 || len(contract.PtahCommit) != 40 || len(contract.Statements) != 78 {
		return nil, errors.New("schema SQL audit contract is incomplete")
	}
	policy := &schemaSQLPolicy{engine: engine, database: database, allowed: map[schemaSQLKey]bool{}}
	for _, row := range contract.Statements {
		if row.Engine != engine {
			continue
		}
		if row.Statement == "" || len(row.Operations) == 0 ||
			(engine == "postgresql" && row.Command != "query") ||
			(engine == "mysql" && (row.Parameters != "" || (row.Command != "Query" && row.Command != "Prepare" && row.Command != "Execute"))) {
			return nil, errors.New("schema SQL audit has an invalid statement declaration")
		}
		statement := row.Statement
		if engine == "mysql" {
			statement = strings.ReplaceAll(statement, "'{{database}}'", "'"+database+"'")
		}
		for _, operation := range row.Operations {
			key := schemaSQLKey{operation, row.Command, statement, row.Parameters}
			if (operation != "observe" && operation != "plan" && operation != "stale-apply") || policy.allowed[key] {
				return nil, errors.New("schema SQL audit has an invalid or duplicate operation declaration")
			}
			policy.allowed[key] = true
		}
	}
	if len(policy.allowed) == 0 {
		return nil, errors.New("schema SQL audit has no permitted diagnostics")
	}
	return policy, nil
}

func schemaDiagnosticActor(actor operationSQLClient) bool {
	return actor.resourceUID != "" && actor.jobUID != "" && actor.podUID != "" && (actor.operation == "observe" || actor.operation == "plan")
}

func (p *schemaSQLPolicy) postgres(actor operationSQLClient, statement, parameters string) bool {
	return p.engine == "postgresql" && schemaDiagnosticActor(actor) && p.allowed[schemaSQLKey{actor.operation, "query", statement, parameters}]
}

func (p *schemaSQLPolicy) mysql(actor operationSQLClient, command, statement, database string) bool {
	return p.engine == "mysql" && database == p.database && schemaDiagnosticActor(actor) && p.allowed[schemaSQLKey{actor.operation, command, statement, ""}]
}

func schemaSQLClients(schema *ptahv1alpha1.PtahSchema, jobs []batchv1.Job, pods []corev1.Pod, predecessors ...*ptahv1alpha1.PtahSchema) (map[string]operationSQLClient, error) {
	if schema == nil || schema.UID == "" || schema.Name == "" || schema.Namespace == "" {
		return nil, errors.New("SQL audit has no schema identity")
	}
	identities := map[string]bool{string(schema.UID): true}
	for _, old := range predecessors {
		if old == nil || old.UID == "" || old.Name != schema.Name || old.Namespace != schema.Namespace || identities[string(old.UID)] {
			return nil, errors.New("SQL audit needs distinct same-name schema identities")
		}
		identities[string(old.UID)] = true
	}
	return operationSQLClientsForIdentities(schema.Namespace, schema.Name, "PtahSchema", labelSchema, identities, jobs, pods)
}

func schemaReplacementSQLControls(clients map[string]operationSQLClient, counts map[string]int, oldUID, currentUID string) error {
	if oldUID == "" || currentUID == "" || oldUID == currentUID {
		return errors.New("schema SQL controls need both resource identities")
	}
	seen := map[[2]string]bool{}
	for host, actor := range clients {
		if actor.resourceUID != oldUID && actor.resourceUID != currentUID {
			return errors.New("schema SQL control belongs to an undeclared resource")
		}
		if counts[host] > 0 && schemaDiagnosticActor(actor) {
			seen[[2]string{actor.resourceUID, actor.operation}] = true
		}
	}
	for _, uid := range []string{oldUID, currentUID} {
		for _, operation := range []string{"observe", "plan"} {
			if !seen[[2]string{uid, operation}] {
				return errors.New("schema SQL audit did not observe both diagnostics from both resource identities")
			}
		}
	}
	return nil
}

// Each required control names a completed result already bound to the schema's
// observation or published plan. Other diagnostic SQL cannot substitute for it.
func schemaRequiredSQLControls(clients map[string]operationSQLClient, counts map[string]int, required []operationSQLClient) error {
	if len(required) == 0 {
		return errors.New("schema SQL audit has no required result controls")
	}
	jobs, pods := map[string]bool{}, map[string]bool{}
	for _, control := range required {
		if !schemaDiagnosticActor(control) || jobs[control.jobUID] || pods[control.podUID] {
			return errors.New("schema SQL audit has an incomplete or duplicate result control")
		}
		jobs[control.jobUID], pods[control.podUID] = true, true
		matches := 0
		for host, actor := range clients {
			if actor == control && counts[host] > 0 {
				matches++
			}
		}
		if matches != 1 {
			return errors.New("schema SQL audit did not observe the exact required diagnostic Job and Pod")
		}
	}
	return nil
}

type schemaSQLInventory struct {
	jobs         map[types.UID]batchv1.Job
	pods         map[types.UID]corev1.Pod
	excludedJobs checkpoint
}

// A read-only diagnostic may retry after a terminal failure. Select its sole
// successful result without removing failed attempts from the SQL inventory.
// Apply callers keep the exact-one-Job boundary instead.
func (inventory *schemaSQLInventory) diagnosticResult(schema *ptahv1alpha1.PtahSchema, operation string, records []observedJob) (observedJob, error) {
	var selected observedJob
	if schema == nil || schema.UID == "" || (operation != "observe" && operation != "plan") {
		return selected, errors.New("result selection requires an exact schema and a read-only diagnostic")
	}
	fingerprint := ""
	for _, record := range records {
		job, found := inventory.jobs[types.UID(record.UID)]
		if !found || job.UID == "" || string(job.UID) != record.UID || job.Name != record.Name || job.Namespace != schema.Namespace ||
			record.Schema != schema.Name || record.Operation != operation || job.Labels[labelSchema] != schema.Name || job.Labels[labelOperation] != operation ||
			!ownedExactlyOnce(job.OwnerReferences, ptahSchemaAPIVersion, "PtahSchema", schema.Name, schema.UID) {
			return observedJob{}, fmt.Errorf("diagnostic Job %s lost its exact schema identity", record.Name)
		}
		input := job.Annotations["operator.ptah.run/input-fingerprint"]
		if !sha256Pattern.MatchString(input) || (fingerprint != "" && input != fingerprint) {
			return observedJob{}, fmt.Errorf("diagnostic Job %s has different or missing inputs", job.Name)
		}
		fingerprint = input
		complete, failed := conditionTrue(job.Status.Conditions, batchv1.JobComplete), conditionTrue(job.Status.Conditions, batchv1.JobFailed)
		if complete == failed {
			return observedJob{}, fmt.Errorf("diagnostic Job %s has no unambiguous terminal outcome", job.Name)
		}
		if complete {
			if selected.UID != "" {
				return observedJob{}, errors.New("diagnostic has more than one successful Job")
			}
			selected = record
		}
	}
	if selected.UID == "" {
		return selected, errors.New("diagnostic has no successful Job")
	}
	return selected, nil
}

func (inventory *schemaSQLInventory) record(jobs []batchv1.Job, pods []corev1.Pod) error {
	for _, job := range jobs {
		if inventory.excludedJobs.holds(string(job.UID)) {
			continue
		}
		if job.UID == "" {
			return errors.New("schema SQL audit Job has no UID")
		}
		inventory.jobs[job.UID] = job
	}
	for _, pod := range pods {
		if owner := metav1.GetControllerOf(&pod); owner != nil && owner.Kind == "Job" && owner.APIVersion == "batch/v1" && inventory.excludedJobs.holds(string(owner.UID)) {
			continue
		}
		if pod.UID == "" {
			return errors.New("schema SQL audit Pod has no UID")
		}
		inventory.pods[pod.UID] = pod
	}
	return nil
}

func (inventory *schemaSQLInventory) clients(schema *ptahv1alpha1.PtahSchema, predecessors ...*ptahv1alpha1.PtahSchema) (map[string]operationSQLClient, error) {
	jobs, pods := make([]batchv1.Job, 0, len(inventory.jobs)), make([]corev1.Pod, 0, len(inventory.pods))
	for _, job := range inventory.jobs {
		jobs = append(jobs, job)
	}
	for _, pod := range inventory.pods {
		pods = append(pods, pod)
	}
	return schemaSQLClients(schema, jobs, pods, predecessors...)
}
