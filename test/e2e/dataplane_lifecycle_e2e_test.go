//go:build e2e

package e2e

import (
	"regexp"
	"strings"
	"time"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func (d *dataPlane) postgresqlLifecycle() {
	d.runEngineLifecycle("postgresql", "PostgreSQL", "postgres", pgSecret)
}

func (d *dataPlane) mysqlLifecycle() {
	d.runEngineLifecycle("mysql", "MySQL", "mysql", mysqlSecret)
}

var dropIndexStatement = regexp.MustCompile(`(?i)\bDROP[[:space:]]+INDEX\b`)

// runEngineLifecycle takes one engine's schema from its first plan to a
// destructive plan the policy refuses: an exact approval applies v1 and the
// schema converges; a changed interval and the persisted timer each start one
// read-only cycle; a moved tag is found by the timer alone and planned; an
// approval of a plan the next tag move made obsolete goes stale instead of
// applying; the approved v3 applies; and v4, destructive, is blocked for as
// long as the row watches. On PostgreSQL the row also proves the custom-CA
// registry, the digest-pin refusal and a registry outage in between.
func (d *dataPlane) runEngineLifecycle(slug, engine, dialect, secret string) {
	d.t.Helper()
	schema := "e2e-" + slug
	key := "e2e/" + slug + "/app"
	coordinationEngine := map[string]string{"PostgreSQL": "postgresql", "MySQL": "mysql"}[engine]
	if coordinationEngine == "" {
		d.fatalf("unsupported coordination engine %s", engine)
	}
	realm, err := coordinationDigest(coordinationEngine, d.in.TestNamespace, key)
	d.check(err, "derive the %s realm", schema)
	reference := d.registryReference(slug)

	d.logf("starting %s lifecycle", engine)
	v1Apply := d.checkpointJobs(schema, "apply")
	v1Observe := d.checkpointJobs(schema, "observe")
	v1Plan := d.checkpointJobs(schema, "plan")
	digestV1 := d.publishSchema(slug, "v1", dialect, reference, "")
	if slug == "postgresql" {
		if customCACoordinationKey == key {
			d.fatalf("custom-CA acceptance cannot share the primary lifecycle coordination key")
		}
		d.assertAuthenticatedHTTPSCustomCA(digestV1)
		d.assertRequestedDigestPinRefusal(reference, digestV1, engine, secret)
	}
	leases := d.checkpointCoordinationLeases()
	resource := schemaResource{name: schema, engine: engine, secret: secret, reference: reference, coordinationKey: key}
	if slug == "postgresql" {
		resource.failureRetry, resource.interval = "45s", quiescentInterval
	}
	database := pgDatabase
	if slug == "mysql" {
		database = mysqlDatabase
	}
	sqlWindow := d.startSchemaRefusalWindow(schema, slug, database, secret)
	d.createSchemaResource(resource)
	d.assertPlan(schema, reference, digestV1, dialect, false, v1Observe, v1Plan, false)
	initial := d.schema(schema)
	controls := []operationSQLClient{
		sqlWindow.resultControl(initial, "observe", v1Observe),
		sqlWindow.resultControl(initial, "plan", v1Plan),
	}
	d.assertCoordinationBoundary(schema, key, realm)
	controls = append(controls, d.changeApprovedSchemaInputs(schema, key, realm, sqlWindow)...)
	sqlWindow.assert(d.schema(schema), controls...)
	d.assertDatabaseColumn(slug, "name", 0)
	planV1 := d.plan
	assertAppliedSQLReadAuthorization := d.pendingSQLReadAuthorization(schema)
	d.assertJobIsolation(schema, secret, false, nil)
	d.assertNoNewJobs(schema, "apply", v1Apply)
	v1PostObserve := d.checkpointJobs(schema, "observe")
	v1PostPlan := d.checkpointJobs(schema, "plan")
	assertV1SQL := d.approvedSchemaSQLControl(schema, slug, planV1, v1Apply)
	d.createExactApproval(schema, planV1.name, schema+"-v1", key, realm)
	d.waitForOneNewJob(schema, "apply", v1Apply)
	d.waitForInSync(schema, digestV1, v1PostObserve, v1PostPlan)
	d.assertApprovalConsumed(schema+"-v1", planV1.uid)
	assertV1SQL()
	assertAppliedSQLReadAuthorization()
	d.assertOneNewJob(schema, "apply", v1Apply)
	d.assertCoordinationLeaseBoundary(key, leases)
	d.assertJobIsolation(schema, secret, true, nil)
	d.assertDatabaseColumn(slug, "name", 1)
	d.assertDatabaseColumn(slug, "note", 0)
	if slug == "mysql" {
		d.assertMySQLDeclaredElementOrder()
	}
	d.setReconcileIntervalAndAssertNoop(schema, digestV1, reconcileInterval, false)
	d.assertPeriodicNoop(schema, d.periodicNoop)
	if slug == "postgresql" {
		d.assertRegistryOutageAndRecovery(schema, digestV1, planV1)
	}
	d.setReconcileIntervalAndAssertNoop(schema, digestV1, tagMoveInterval, true)

	// Only the controller's status writes are held while the tag moves. The
	// schema's generation stays where it is, so the persisted timer is the
	// only trigger that can find the moved tag once the writes come back.
	if !d.rbac.paused {
		d.fatalf("scheduled tag proof lacks a status-write barrier")
	}
	sqlWindow.reopen()
	generation := d.schema(schema).Generation
	digestV2 := d.publishSchema(slug, "v2", dialect, reference, "")
	if digestV2 == digestV1 {
		d.fatalf("%s v2 did not move the mutable tag", schema)
	}
	d.waitForSchema(schema, "the persisted mutable-tag polling deadline to elapse without a spec change",
		func(s *ptahv1alpha1.PtahSchema) bool {
			return s.Generation == generation && s.Status.ObservedGeneration == generation &&
				s.Status.Phase == ptahv1alpha1.PhaseInSync && s.Status.Source.Digest == digestV1 &&
				s.Status.ActiveOperation == nil && due(s.Status.NextReconciliationTime, time.Now())
		})
	v2 := d.checkpointJobs(schema, "")
	d.mustResumeStatusWrites("could not restore controller status-write RBAC")
	v2After := d.assertPlan(schema, reference, digestV2, dialect, false, v2, v2, true)
	second := d.schema(schema)
	secondObserve := sqlWindow.resultControl(second, "observe", v2)
	secondPlan := sqlWindow.resultControl(second, "plan", v2)
	d.assertReadOnlyCycleBetween(schema, v2, *v2After)
	if moved := d.schema(schema); moved.Generation != generation || moved.Status.ObservedGeneration != generation {
		d.fatalf("%s scheduled tag refresh depended on a spec generation change", schema)
	}
	planV2 := d.plan
	d.assertDistinctPlan(schema, "v1", planV1, planV2)

	// Admission and reconciliation normally race after an approval is
	// created. With only the controller's status verb held back and the
	// webhook's reads still available, the tag moves again, and giving the
	// verbs back makes the controller observe v3 before it can consume the
	// decision about v2.
	obsolete := schema + "-obsolete"
	stale := *v2After
	d.createExactApproval(schema, planV2.name, obsolete, key, realm)
	digestV3 := d.publishSchema(slug, "v3", dialect, reference, "")
	if digestV3 == digestV2 {
		d.fatalf("%s v3 did not move the mutable tag", schema)
	}
	d.patchSchema(schema, map[string]any{"spec": map[string]any{"interval": staleApprovalInterval}})
	d.mustResumeStatusWrites("could not restore controller status-write RBAC")
	v3After := d.assertPlan(schema, reference, digestV3, dialect, false, stale, stale, true)
	third := d.schema(schema)
	thirdObserve := sqlWindow.resultControl(third, "observe", stale)
	thirdPlan := sqlWindow.resultControl(third, "plan", stale)
	planV3 := d.plan
	d.assertDistinctPlan(schema, "v2", planV2, planV3)
	d.assertNoJobBetween(schema, "apply", stale, *v3After)
	d.mustResumeStatusWrites("could not restore controller status-write RBAC")
	d.waitForApproval(obsolete, "the old exact approval to become stale", func(approval *ptahv1alpha1.PtahSchemaApproval) bool {
		return conditionIs(approval.Status.Conditions, "Stale", "True", "PlanNoLongerCurrent")
	})
	d.assertNoNewJobs(schema, "apply", stale)
	sqlWindow.assert(third, secondObserve, secondPlan, thirdObserve, thirdPlan)
	v3Apply := d.checkpointJobs(schema, "")
	assertV3SQL := d.approvedSchemaSQLControl(schema, slug, planV3, v3Apply)
	d.createExactApproval(schema, planV3.name, schema+"-v3", key, realm)
	d.waitForOneNewJob(schema, "apply", v3Apply)
	d.waitForInSync(schema, digestV3, v3Apply, v3Apply)
	d.assertApprovalConsumed(schema+"-v3", planV3.uid)
	assertV3SQL()
	d.assertOneNewJob(schema, "apply", v3Apply)
	d.assertCoordinationLeaseBoundary(key, leases)
	d.assertDatabaseColumn(slug, "note", 1)
	d.assertDatabaseColumn(slug, "enabled", 1)
	if slug == "mysql" {
		d.assertMySQLUniqueIndex(1)
		d.assertMySQLPlainIndex(1)
	}

	d.suspendSchemaForTagMove(schema, approvalInterval)
	digestV4 := d.publishSchema(slug, "v4", dialect, reference, "")
	if digestV4 == digestV3 {
		d.fatalf("%s v4 did not move the mutable tag", schema)
	}
	destructiveApply := d.checkpointJobs(schema, "")
	d.resumeSchemaAfterTagMove(schema)
	v4After := d.assertPlan(schema, reference, digestV4, dialect, true, destructiveApply, destructiveApply, true)
	if slug == "mysql" {
		d.assertUnderclassifiedDropIndex()
	}
	d.waitForSchema(schema, "the destructive policy gate", func(s *ptahv1alpha1.PtahSchema) bool {
		return s.Status.Phase == ptahv1alpha1.PhaseBlocked &&
			conditionIs(s.Status.Conditions, "ApprovalRequired", "False", "DestructiveChangesDisabled") &&
			conditionStatus(s.Status.Conditions, "Ready", "False")
	})
	d.assertNoJobBetween(schema, "apply", destructiveApply, *v4After)
	d.prepareBlockedRefreshCadence(schema)
	blocked := d.plan
	d.assertDestructiveGate(schema, destructiveApply, blocked, digestV4, d.blockedGate)
	d.restoreBlockedRefreshCadence(schema, blocked, digestV4)
	d.assertDatabaseColumn(slug, "note", 1)
	d.assertDatabaseColumn(slug, "enabled", 1)
	if slug == "mysql" {
		d.assertMySQLUniqueIndex(1)
		d.assertMySQLPlainIndex(1)
		d.mysqlDestructive = mysqlDestructiveEvidence{
			schema: schema, plan: blocked.name, planUID: blocked.uid, digest: digestV4,
			applyCheckpoint: destructiveApply, retained: true,
		}
	}
	d.assertCoordinationBoundary(schema, key, realm)
	d.assertCoordinationLeaseBoundary(key, leases)
	d.auditRuntimeCredentials()
	d.logf("PASS %s lifecycle", engine)
}

// assertDistinctPlan holds a plan published after a tag move to being a new
// plan: another name, another UID and another fingerprint.
func (d *dataPlane) assertDistinctPlan(schema, previousRevision string, previous, next currentPlan) {
	d.t.Helper()
	switch {
	case next.name == previous.name:
		d.fatalf("%s reused the %s plan name after a tag move", schema, previousRevision)
	case next.uid == previous.uid:
		d.fatalf("%s reused the %s plan UID after a tag move", schema, previousRevision)
	case next.fingerprint == previous.fingerprint:
		d.fatalf("%s reused the %s fingerprint after a tag move", schema, previousRevision)
	}
}

// assertUnderclassifiedDropIndex holds the MySQL v4 plan to what the fixture is
// for: Ptah rates the one DROP INDEX statement as not destructive, and the
// operator elevates the plan to destructive anyway.
func (d *dataPlane) assertUnderclassifiedDropIndex() {
	d.t.Helper()
	document, err := parsePlanDocument(d.planDocument)
	if err != nil || !isFalse(document.Destructive) || len(document.Statements) != 1 ||
		!dropIndexStatement.MatchString(document.Statements[0].SQL) ||
		!strings.Contains(document.Statements[0].SQL, "e2e_widgets_name_idx") ||
		strings.ToLower(document.Statements[0].Severity) == "destructive" {
		d.fatalf("MySQL fixture did not exercise an executor-underclassified DROP INDEX")
	}
	if !d.schemaPlan(d.plan.name).Spec.Destructive {
		d.fatalf("MySQL DROP INDEX was not conservatively elevated to destructive")
	}
}

// mysqlDSNRefusalScenario runs the invalid-DSN refusal and closes the engine
// rows: by now some operation Pod was live long enough to be sent the
// admission refusals only a live Pod can be sent, and nothing the rows ran
// left a credential behind.
func (d *dataPlane) mysqlDSNRefusalScenario() {
	d.t.Helper()
	d.mysqlDSNRefusal()
	if !d.ephemeralTested {
		d.fatalf("no UID-bound active operation Pod was available for the admission subresource and managed-identity tests")
	}
	d.auditRuntimeCredentials()
}

// closingAudits holds what the lifecycles left to still hold after the fault
// injection: the MySQL destructive plan still refused, the external database
// still exactly v1, nothing credential-bearing anywhere, and every Job the
// phase saw audited.
func (d *dataPlane) closingAudits() {
	d.t.Helper()
	d.assertMySQLDestructiveRefusalDurable()
	d.assertExternalPostgresqlCatalog()
	d.auditRuntimeCredentials()
	d.assertObservedJobsAudited()
}
