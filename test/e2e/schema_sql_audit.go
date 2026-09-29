package e2e

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

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
	if err := decoder.Decode(&contract); err != nil || contract.SchemaVersion != 1 || len(contract.PtahCommit) != 40 || len(contract.Statements) != 69 {
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
			if (operation != "observe" && operation != "plan") || policy.allowed[key] {
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
