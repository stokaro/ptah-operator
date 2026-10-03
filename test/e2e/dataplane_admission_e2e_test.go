//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// The authenticated HTTPS custom-CA registry, the MySQL invalid-DSN refusal,
// the four-eyes approval and the declared Pod metadata.

// tlsProxyLabels select the one proxy Pod.
func (d *dataPlane) tlsProxyLabels() client.MatchingLabels {
	return client.MatchingLabels{
		"app.kubernetes.io/name": d.in.TLSProxyService, "app.kubernetes.io/component": "e2e-tls-registry-proxy",
	}
}

func (d *dataPlane) tlsProxyIdentityCaptured() proxyPodIdentity {
	identity := d.tlsProxyIdentity
	return proxyPodIdentity{name: identity.name, uid: identity.uid, podIP: identity.podIP, containerID: identity.containerID}
}

// tlsProxyRequestCount reads how many requests the proxy has served, from the
// credential-free counter on its admin port, through the API server's proxy
// to the exact Pod the row captured, and holds that Pod's identity on both
// sides of the read: a counter of another process counts nothing this row
// did.
func (d *dataPlane) tlsProxyRequestCount() int64 {
	d.t.Helper()
	d.assertTLSProxyIdentityStable()
	body, err := d.cluster.Raw(d.ctx,
		"/api/v1/namespaces/"+d.in.TestNamespace+"/pods/http:"+d.tlsProxyIdentity.name+":8081/proxy/")
	if err != nil {
		d.fatalf("could not read the credential-free TLS proxy request counter through the exact Pod API proxy: %v", err)
	}
	count, err := tlsProxyCounter(body)
	if err != nil {
		d.fatalf("%v", err)
	}
	d.assertTLSProxyIdentityStable()
	return count
}

// captureTLSProxyIdentity records the one ready proxy Pod and its container,
// and waits for the proxy's Service to route to that Pod alone.
func (d *dataPlane) captureTLSProxyIdentity() {
	d.t.Helper()
	pods := &corev1.PodList{}
	d.mustList(pods, d.tlsProxyLabels())
	identity, err := tlsProxyPodIdentity(pods.Items)
	if err != nil {
		d.fatalf("TLS registry proxy does not have one exact zero-restart ready Pod: %v", err)
	}
	if identity.name == "" || identity.uid == "" || identity.podIP == "" || identity.containerID == "" {
		d.fatalf("TLS registry proxy exact Pod identity is incomplete")
	}
	d.tlsProxyIdentity = tlsProxyPod{
		name: identity.name, uid: identity.uid, podIP: identity.podIP, containerID: identity.containerID,
	}
	d.waitForTLSProxyServiceEndpoints()
	d.assertTLSProxyIdentityStable()
}

// tlsProxyServiceEndpointsMatch reports whether the proxy's Service routes to
// the captured Pod and nowhere else. A read that failed is a Service that did
// not match.
func (d *dataPlane) tlsProxyServiceEndpointsMatch() bool {
	endpointSlices := &discoveryv1.EndpointSliceList{}
	if err := d.list(endpointSlices, client.MatchingLabels{"kubernetes.io/service-name": d.in.TLSProxyService}); err != nil {
		return false
	}
	identity := d.tlsProxyIdentity
	return tlsProxyServiceEndpoints(endpointSlices.Items, d.in.TestNamespace, identity.name, types.UID(identity.uid), identity.podIP)
}

// waitForTLSProxyServiceEndpoints gives the Service a bounded number of
// one-second readings to converge on the captured Pod.
func (d *dataPlane) waitForTLSProxyServiceEndpoints() {
	d.t.Helper()
	for attempt := range tlsProxyEndpointWaitAttempts {
		if d.tlsProxyServiceEndpointsMatch() {
			return
		}
		if attempt+1 < tlsProxyEndpointWaitAttempts {
			d.sleep(time.Second)
		}
	}
	d.fatalf("TLS proxy Service %s did not converge on the captured exact Pod", d.in.TLSProxyService)
}

func (d *dataPlane) assertTLSProxyServiceEndpoints() {
	d.t.Helper()
	if !d.tlsProxyServiceEndpointsMatch() {
		d.fatalf("TLS proxy Service %s can route outside the captured exact Pod", d.in.TLSProxyService)
	}
}

// assertTLSProxyIdentityStable holds the proxy to the Pod and container the
// row captured, and its Service to routing there alone.
func (d *dataPlane) assertTLSProxyIdentityStable() {
	d.t.Helper()
	identity := d.tlsProxyIdentityCaptured()
	if identity.name == "" || identity.uid == "" || identity.podIP == "" || identity.containerID == "" {
		d.fatalf("TLS registry proxy identity was not captured before the counter window")
	}
	pods := &corev1.PodList{}
	d.mustList(pods, d.tlsProxyLabels())
	if !tlsProxyIdentityStable(pods.Items, identity) {
		d.fatalf("TLS registry proxy Pod or container identity changed inside the counter window")
	}
	d.assertTLSProxyServiceEndpoints()
}

// createCustomCASchemaResource creates a schema that reads its artifact from
// the HTTPS proxy with the custom CA and the registry credential given, into
// the custom-CA database, at an interval long enough that nothing refreshes
// while the row watches.
func (d *dataPlane) createCustomCASchemaResource(name, authSecret, coordinationKey string) {
	d.t.Helper()
	d.mustCreate(map[string]any{
		"apiVersion": ptahSchemaAPIVersion, "kind": "PtahSchema",
		"metadata": map[string]any{"namespace": d.in.TestNamespace, "name": name},
		"spec": map[string]any{
			"target": map[string]any{
				"engine": "PostgreSQL", "coordinationKey": coordinationKey,
				"urlFrom": map[string]any{"name": customCAPGSecret, "key": "url"},
			},
			"desired": map[string]any{
				"ociRef": d.tlsProxy.reference,
				"registryAuthFrom": map[string]any{
					"name": authSecret, "mode": "Environment",
					"usernameKey": "username", "passwordKey": "password", "tokenKey": "token",
				},
				"verificationPolicyFrom": map[string]any{"name": verificationPolicyName, "key": verificationPolicyKey},
				"transport":              map[string]any{"caFrom": map[string]any{"name": tlsProxyCAConfigMap, "key": "ca.pem"}},
			},
			"policy": map[string]any{
				"apply": "OnApproval", "allowDestructive": false, "driftSeverity": "all",
				"lockTimeout": "30s", "transactionMode": "file",
			},
			"interval": "30m",
			"execution": map[string]any{
				"activeDeadlineSeconds": int64(300), "failureRetryInterval": "30m", "connectTimeout": "30s",
				"runtimeClassName": admissionRuntimeClass,
			},
		},
	})
}

// assertCustomCACompletedPods holds the custom-CA schema's Jobs to credential
// isolation, and its completed Observe and Plan Pods to the guard, the CA
// snapshot, the resolved source and the fetch they were given.
func (d *dataPlane) assertCustomCACompletedPods(schema, resolved string) {
	d.t.Helper()
	jobs := &batchv1.JobList{}
	d.mustList(jobs, client.MatchingLabels{labelSchema: schema})
	if !controllerJobIsolation(jobs.Items, customCAPGSecret, tlsProxyGoodAuthSecret, false) {
		d.fatalf("%s Jobs lost custom-CA credential isolation", schema)
	}
	pods := &corev1.PodList{}
	d.mustList(pods, client.MatchingLabels{labelSchema: schema})
	if !customCAPodIsolation(pods.Items, customCAPodIsolationInputs{
		jobs:           jobs.Items,
		databaseSecret: customCAPGSecret, registrySecret: tlsProxyGoodAuthSecret,
		registryAuthority: d.tlsProxy.authority, caConfigMap: tlsProxyCAConfigMap, resolvedReference: resolved,
	}) {
		d.fatalf("%s completed Observe/Plan Pods lost guard, CA snapshot, source, or fetch isolation", schema)
	}
}

// assertCustomCAPreChildRefusal creates a schema whose registry credential
// grants the wrong CA or the wrong authority, and proves the runner refused it
// before it started the child that would have reached the registry: the
// schema fails for invalid_oci_access, one Resolve Job ran and nothing else,
// its result says the child never started, and the proxy served no request.
func (d *dataPlane) assertCustomCAPreChildRefusal(schema, authSecret, coordinationKey, description string) {
	d.t.Helper()
	proxyBefore := d.tlsProxyRequestCount()
	before := d.checkpointJobs(schema, "")
	d.createCustomCASchemaResource(schema, authSecret, coordinationKey)
	d.waitForSchema(schema, description+" to fail before the Resolve child", failedBeforeResolveChild)
	result := d.captureOneNewJobResult(schema, "resolve", before, nil)
	if err := preChildAccessRefusal(result); err != nil {
		d.fatalf("%s did not retain the typed pre-child invalid_oci_access result: %v", schema, err)
	}
	d.suspend(schema, true)
	d.waitForSchema(schema, description+" fixture to suspend before retry", func(s *ptahv1alpha1.PtahSchema) bool {
		return s.Status.Phase == ptahv1alpha1.PhaseSuspended && s.Status.ActiveOperation == nil
	})
	after := d.checkpointJobs(schema, "")
	d.assertOneJobBetween(schema, "resolve", before, after)
	for _, operation := range []string{"verify", "observe", "plan", "apply"} {
		d.assertNoJobBetween(schema, operation, before, after)
	}
	if count := d.countBetween(schema, "", before, after); count != 1 {
		d.fatalf("%s created %d total Jobs, expected only one Resolve", schema, count)
	}
	proxyAfter := d.tlsProxyRequestCount()
	d.assertTLSProxyIdentityStable()
	if proxyAfter != proxyBefore {
		d.fatalf("%s reached the TLS registry before %s was refused", schema, description)
	}
}

// customCACatalogQuery describes every user object in a PostgreSQL database
// as one JSON document, ordered, so two readings of an unchanged database are
// the same bytes.
const customCACatalogQuery = `WITH user_namespaces AS (
SELECT oid, nspname FROM pg_namespace WHERE nspname !~ '^pg_' AND nspname <> 'information_schema'
), relations AS (
SELECT n.nspname, c.relname, c.relkind::text, c.relpersistence::text,
CASE WHEN c.relkind IN ('v','m') THEN pg_get_viewdef(c.oid, true) ELSE '' END AS definition
FROM pg_class c JOIN user_namespaces n ON n.oid = c.relnamespace
), columns AS (
SELECT n.nspname, c.relname, a.attnum, a.attname, format_type(a.atttypid, a.atttypmod) AS data_type,
a.attnotnull, a.attidentity::text, a.attgenerated::text, COALESCE(pg_get_expr(d.adbin, d.adrelid), '') AS default_expression
FROM pg_class c JOIN user_namespaces n ON n.oid = c.relnamespace
JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
LEFT JOIN pg_attrdef d ON d.adrelid = c.oid AND d.adnum = a.attnum
), constraints AS (
SELECT n.nspname, c.relname, x.conname, x.contype::text, pg_get_constraintdef(x.oid, true) AS definition
FROM pg_constraint x JOIN pg_class c ON c.oid = x.conrelid
JOIN user_namespaces n ON n.oid = c.relnamespace
), indexes AS (
SELECT n.nspname, c.relname, i.relname AS index_name, pg_get_indexdef(i.oid) AS definition
FROM pg_index x JOIN pg_class c ON c.oid = x.indrelid
JOIN pg_class i ON i.oid = x.indexrelid JOIN user_namespaces n ON n.oid = c.relnamespace
), types AS (
SELECT n.nspname, t.typname, t.typtype::text, t.typcategory::text,
COALESCE((SELECT json_agg(e.enumlabel ORDER BY e.enumsortorder) FROM pg_enum e WHERE e.enumtypid = t.oid), '[]'::json) AS enum_labels
FROM pg_type t JOIN user_namespaces n ON n.oid = t.typnamespace WHERE t.typtype <> 'b'
), routines AS (
SELECT n.nspname, p.proname, pg_get_function_identity_arguments(p.oid) AS arguments,
pg_get_function_result(p.oid) AS result, p.prokind::text, pg_get_functiondef(p.oid) AS definition
FROM pg_proc p JOIN user_namespaces n ON n.oid = p.pronamespace WHERE p.prokind IN ('f','p')
)
SELECT json_build_object(
'schemas', (SELECT COALESCE(json_agg(nspname ORDER BY nspname), '[]'::json) FROM user_namespaces),
'relations', (SELECT COALESCE(json_agg(relations ORDER BY nspname, relname), '[]'::json) FROM relations),
'columns', (SELECT COALESCE(json_agg(columns ORDER BY nspname, relname, attnum), '[]'::json) FROM columns),
'constraints', (SELECT COALESCE(json_agg(constraints ORDER BY nspname, relname, conname), '[]'::json) FROM constraints),
'indexes', (SELECT COALESCE(json_agg(indexes ORDER BY nspname, relname, index_name), '[]'::json) FROM indexes),
'types', (SELECT COALESCE(json_agg(types ORDER BY nspname, typname), '[]'::json) FROM types),
'routines', (SELECT COALESCE(json_agg(routines ORDER BY nspname, proname, arguments), '[]'::json) FROM routines)
)`

// customCADatabaseSchemaFingerprint is the digest of the custom-CA database's
// catalog: what an approval boundary must leave exactly as it found it.
func (d *dataPlane) customCADatabaseSchemaFingerprint() string {
	d.t.Helper()
	catalog, err := d.psql(customCAPGDatabase, customCACatalogQuery)
	d.check(err, "read the custom-CA PostgreSQL catalog")
	d.scan([]byte(catalog), "the custom-CA PostgreSQL schema fingerprint")
	return sha256Digest([]byte(catalog))
}

// assertAuthenticatedHTTPSCustomCA proves the operator reads an artifact over
// authenticated HTTPS with a custom CA only under the credential owner's
// grants: a credential that grants another CA, or another registry authority,
// is refused before the registry is reached; the one that grants both reaches
// it, plans against a database of its own, and stops for a person with the
// database untouched and every Pod isolated from the database credential.
func (d *dataPlane) assertAuthenticatedHTTPSCustomCA(digest string) {
	d.t.Helper()
	resolved := referenceAtDigest(d.tlsProxy.reference, digest)
	const badCASchema, badAuthoritySchema, goodSchema = "e2e-https-ca-bad-ca", "e2e-https-ca-bad-authority", "e2e-https-ca-good"
	d.logf("checking authenticated HTTPS custom-CA authority grants")
	d.captureTLSProxyIdentity()
	d.assertCustomCAPreChildRefusal(badCASchema, tlsProxyBadCAAuthSecret, customCACoordinationKey,
		"its mismatched CA digest grant")
	d.assertCustomCAPreChildRefusal(badAuthoritySchema, tlsProxyBadAuthoritySecret, customCACoordinationKey,
		"its mismatched registry authority grant")

	fingerprintBefore := d.customCADatabaseSchemaFingerprint()
	if !sha256Pattern.MatchString(fingerprintBefore) {
		d.fatalf("custom-CA PostgreSQL preflight schema fingerprint is invalid")
	}
	proxyBefore := d.tlsProxyRequestCount()
	before := d.checkpointJobs(goodSchema, "")
	d.createCustomCASchemaResource(goodSchema, tlsProxyGoodAuthSecret, customCACoordinationKey)
	d.assertPlan(goodSchema, d.tlsProxy.reference, digest, "postgres", false, before, before, false)
	d.waitForSchema(goodSchema, "the authenticated HTTPS custom-CA source to reach a nonmutating approval boundary",
		func(s *ptahv1alpha1.PtahSchema) bool { return customCAApprovalBoundary(s, time.Now()) })
	after := d.checkpointJobs(goodSchema, "")
	d.assertReadOnlyCycleBetween(goodSchema, before, after)
	d.assertCustomCACompletedPods(goodSchema, resolved)
	proxyAfter := d.tlsProxyRequestCount()
	d.assertTLSProxyIdentityStable()
	if proxyAfter <= proxyBefore {
		d.fatalf("%s did not make authenticated requests through the TLS registry proxy", goodSchema)
	}
	d.suspend(goodSchema, true)
	d.waitForSchema(goodSchema, "the authenticated HTTPS custom-CA fixture to suspend without Apply",
		func(s *ptahv1alpha1.PtahSchema) bool {
			return s.Status.Phase == ptahv1alpha1.PhaseSuspended && s.Status.ActiveOperation == nil
		})
	suspended := d.checkpointJobs(goodSchema, "")
	d.assertNoJobBetween(goodSchema, "apply", before, suspended)
	if d.customCADatabaseSchemaFingerprint() != fingerprintBefore {
		d.fatalf("%s changed the dedicated database before approval", goodSchema)
	}
	d.auditRuntimeCredentials()
	d.logf("PASS authenticated HTTPS custom-CA authority and isolation")
}

// mysqlDSNRefusal runs a completed MySQL Observe and Plan Job again against a
// URL whose query would run a second statement in the server session, and
// proves the runner refuses the target before it dispatches executor work:
// the result is invalid_target with nothing started, the transport does not
// repeat the payload, and the database keeps every column and index it had.
func (d *dataPlane) mysqlDSNRefusal() {
	d.t.Helper()
	const unsafeSecret, unsafeSchemaLabel = "e2e-mysql-unsafe-dsn", "e2e-mysql-dsn-negative"
	d.mustCreate(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": map[string]any{"namespace": d.in.TestNamespace, "name": unsafeSecret},
		"data":     secretData(map[string]string{"url": unsafeMySQLDSN(d.credentials.mysqlURL)}),
	})
	d.assertMySQLRefusalDatabaseUnchanged()
	for _, operation := range []string{"observe", "plan"} {
		name := "e2e-mysql-dsn-" + operation
		operationID := name + "-operation"
		sources := &unstructured.UnstructuredList{}
		sources.SetAPIVersion("batch/v1")
		sources.SetKind("JobList")
		d.mustList(sources, client.MatchingLabels{labelSchema: "e2e-mysql", labelOperation: operation})
		source, err := latestCompletedJob(sources.Items)
		if err != nil {
			d.fatalf("%s has %v", operation, err)
		}
		rewritten, err := rewriteMySQLRefusalJob(source.Object, d.in.TestNamespace, name, unsafeSchemaLabel,
			operation, operationID, unsafeSecret)
		if err != nil {
			d.fatalf("MySQL invalid-DSN Job rewrite rejected the %s source %s: %v", operation, source.GetName(), err)
		}
		d.mustCreate(rewritten)
		d.waitForJobComplete(name)
		job := d.job(name)
		pods := &corev1.PodList{}
		d.mustList(pods, client.MatchingLabels{"job-name": name})
		var owned []corev1.Pod
		for _, pod := range pods.Items {
			if podControlledByJobUID(pod.OwnerReferences, job.UID) {
				owned = append(owned, pod)
			}
		}
		if len(owned) != 1 {
			d.fatalf("invalid-DSN Job %s does not own one Pod", name)
		}
		transport, result := d.readResultTransport(job, &owned[0], operation, operationID)
		d.scan(transport, "the "+operation+" invalid-DSN runner transport")
		if disclosesSessionPayload(transport) {
			d.fatalf("%s invalid-DSN result disclosed the encoded server-session payload", operation)
		}
		if err := invalidTargetRefusal(result, d.runnerProtocol, operation, operationID); err != nil {
			d.fatalf("%s invalid-DSN Job dispatched executor work: %v", operation, err)
		}
	}
	d.assertMySQLRefusalDatabaseUnchanged()
	d.auditRuntimeCredentials()
}

// assertMySQLRefusalDatabaseUnchanged holds the MySQL database to the columns
// and indexes v3 applied, which the refusal must leave where they are.
func (d *dataPlane) assertMySQLRefusalDatabaseUnchanged() {
	d.t.Helper()
	d.assertDatabaseColumn("mysql", "note", 1)
	d.assertDatabaseColumn("mysql", "enabled", 1)
	d.assertMySQLUniqueIndex(1)
	d.assertMySQLPlainIndex(1)
}

// waitForJobComplete waits for a Job the phase created to complete, as
// kubectl wait --for=condition=Complete did: a Job that failed is still
// waited on until the bound, and the bound is the phase's.
func (d *dataPlane) waitForJobComplete(name string) {
	d.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		job := &batchv1.Job{}
		if err := d.get(name, job); err == nil && conditionTrue(job.Status.Conditions, batchv1.JobComplete) {
			return
		}
		d.sleep(2 * time.Second)
	}
	d.fatalf("timed out waiting for Job %s to complete", name)
}

// liveContext is the phase's context while it lasts, and a bounded one of its
// own once the phase's has ended: the cleanup undoes cluster-wide changes
// after a timeout too.
func (d *dataPlane) liveContext() (context.Context, context.CancelFunc) {
	if d.ctx.Err() == nil {
		return context.WithCancel(d.ctx)
	}
	return context.WithTimeout(context.Background(), 10*time.Minute)
}

// setRequireDistinctApprover upgrades the release with
// approvals.requireDistinctApprover set, and waits for it to settle. When it
// does not, what Helm cannot say is why a Pod did not become ready: the
// runtime-verify init container refuses a webhook configuration whose shape
// disagrees with the flag, and Helm applies the configuration after the
// Deployment, so the init container's own report is the evidence. Container
// states and the init containers' last lines only; the manager's log is
// audited credential-free elsewhere, and nothing here reads the test
// namespace.
func (d *dataPlane) setRequireDistinctApprover(on bool) error {
	ctx, cancel := d.liveContext()
	defer cancel()
	value := strconv.FormatBool(on)
	_, err := d.cluster.Helm(ctx, "-n", d.in.OperatorNamespace, "upgrade", d.in.HelmRelease, d.in.ChartPackage,
		"--reuse-values", "--set", "approvals.requireDistinctApprover="+value, "--wait", "--timeout", "5m")
	if err == nil {
		return nil
	}
	_, _ = fmt.Fprintf(os.Stderr, "e2e data plane: release %s did not settle after approvals.requireDistinctApprover=%s\n",
		d.in.HelmRelease, value)
	stdout, stderr, _ := d.cluster.Kubectl(ctx, "-n", d.in.OperatorNamespace, "get", "pods", "-o", "wide")
	_, _ = os.Stderr.Write(stdout)
	_, _ = os.Stderr.Write(stderr)
	pods := &corev1.PodList{}
	if d.cluster.Client.List(ctx, pods, client.InNamespace(d.in.OperatorNamespace)) == nil {
		for _, line := range unsettledContainerLines(pods.Items) {
			_, _ = fmt.Fprintln(os.Stderr, line)
		}
	}
	controllers := &corev1.PodList{}
	if d.cluster.Client.List(ctx, controllers, client.InNamespace(d.in.OperatorNamespace),
		client.MatchingLabels{"app.kubernetes.io/component": "controller"}) == nil {
		for _, pod := range controllers.Items {
			logs, logErrors, _ := d.cluster.Kubectl(ctx, "-n", d.in.OperatorNamespace, "logs", "pod/"+pod.Name,
				"-c", "verify-candidate-runtime", "--tail=20")
			_, _ = os.Stderr.Write(cutLines(append(logs, logErrors...), 400))
		}
	}
	return fmt.Errorf("release %s did not settle after approvals.requireDistinctApprover=%s: %w", d.in.HelmRelease, value, err)
}

var distinctApproverRefusal = regexp.MustCompile(`(?i)requires a distinct approver`)

// fourEyesDistinctApprover checks the installer-owned four-eyes control end to
// end. approvals.requireDistinctApprover is global for the whole installation,
// and every approval the lifecycles made was made by the identity that wrote
// each schema's spec, so the control is on for this row alone: an upgrade
// turns it on immediately before, a second turns it off immediately after,
// and the cleanup turns it off on any exit in between. The schema's author is
// refused its own approval for who they are rather than for lack of RBAC, a
// distinct identity approves the same plan, and the plan applies.
func (d *dataPlane) fourEyesDistinctApprover() {
	d.t.Helper()
	d.logf("checking the installer-owned four-eyes control end to end")
	const (
		schemaName  = "e2e-four-eyes-postgresql"
		key         = "e2e/admission/four-eyes-postgresql"
		approver    = "e2e-four-eyes-approver"
		table       = "e2e_four_eyes_widgets"
		refused     = "e2e-four-eyes-approve-by-author"
		admittedBy  = "e2e-four-eyes-approve-by-distinct-approver"
		writerLabel = "operator.ptah.run/last-spec-writer-username"
	)
	reference := d.registryReference("four-eyes-postgresql")
	digest := d.publishSchema("four-eyes-postgresql", "v1", "postgres", reference, "")
	if err := d.setRequireDistinctApprover(true); err != nil {
		d.fatalf("could not turn approvals.requireDistinctApprover on for the four-eyes row: %v", err)
	}
	d.fourEyesSwitchOn = true

	// A real schema, through both webhooks, planned by the controller against
	// the lifecycle's server in a database of this row's own. The database is
	// empty, so the plan is the one table the schema declares: something to
	// approve and nothing destructive.
	d.createIsolatedPostgreSQLDatabase(fourEyesPGDatabase, fourEyesPGSecret, d.credentials.fourEyesPGURL)
	d.createSchemaResource(schemaResource{
		name: schemaName, engine: "PostgreSQL", secret: fourEyesPGSecret, reference: reference, coordinationKey: key,
		failureRetry: "5s", interval: approvalInterval,
	})
	matched := d.waitForSchema(schemaName, "the four-eyes schema to publish a plan awaiting approval",
		func(s *ptahv1alpha1.PtahSchema) bool { return awaitingApprovalOf(s, digest) })
	// The schema UID, the plan binding and the recorded writer come from the
	// document that satisfied the wait, not from a later read.
	plan := matched.Status.Plan
	if matched.UID == "" || plan.UID == "" || plan.Fingerprint == "" {
		d.fatalf("%s published a plan with no UID or fingerprint to approve", schemaName)
	}
	author := matched.Annotations[writerLabel]
	if author == "" {
		d.fatalf("%s was not stamped with a last spec writer on create", schemaName)
	}
	if author == approver {
		d.fatalf("the four-eyes fixture's distinct-approver name collides with its own author %s", author)
	}
	approval := func(name string) map[string]any {
		return approvalDocument(d.in.TestNamespace, name, schemaName, string(matched.UID), plan.Name, string(plan.UID),
			plan.Fingerprint)
	}

	// The author holds exactly the rights every earlier approval in this phase
	// used; the refusal is admission reading who they are.
	err := d.create(approval(refused))
	if err == nil {
		d.fatalf("the author's self-approval of %s unexpectedly succeeded", schemaName)
	}
	if !distinctApproverRefusal.MatchString(err.Error()) {
		d.fatalf("the self-approval of %s did not fail for the four-eyes reason: %v", schemaName, err)
	}
	if d.get(refused, &ptahv1alpha1.PtahSchemaApproval{}) == nil {
		d.fatalf("the refused self-approval was created anyway")
	}

	d.mustApply(
		map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
			"metadata": map[string]any{"namespace": d.in.TestNamespace, "name": approver},
			"rules": []any{map[string]any{
				"apiGroups": []any{"operator.ptah.run"}, "resources": []any{"ptahschemaapprovals"},
				"verbs": []any{"create", "get"},
			}},
		},
		map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
			"metadata": map[string]any{"namespace": d.in.TestNamespace, "name": approver},
			"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": approver},
			"subjects": []any{map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "User", "name": approver}},
		},
	)
	distinct, err := d.cluster.As(rest.ImpersonationConfig{UserName: approver, Groups: []string{"system:authenticated"}})
	d.check(err, "act as %s", approver)
	if err := distinct.Create(d.ctx, &unstructured.Unstructured{Object: approval(admittedBy)},
		client.FieldOwner(harness.FieldOwner), client.FieldValidation("Strict")); err != nil {
		d.fatalf("a distinct approver's approval of %s was refused: %v", schemaName, err)
	}
	admitted := &ptahv1alpha1.PtahSchemaApproval{}
	d.mustGet(admittedBy, admitted)
	if admitted.Spec.Approver.Username != approver {
		d.fatalf("the admitted four-eyes approval does not name the distinct approver who made it")
	}

	d.waitForSchema(schemaName, "the distinct approver's approval to apply and reach the converged state",
		func(s *ptahv1alpha1.PtahSchema) bool { return convergedOn(s, digest) })
	if err := d.setRequireDistinctApprover(false); err != nil {
		d.fatalf("could not turn approvals.requireDistinctApprover back off after the four-eyes row: %v", err)
	}
	d.fourEyesSwitchOn = false
	if count := d.isolatedPostgreSQLTableCount(fourEyesPGDatabase, table); count != "1" {
		d.fatalf("%s is not present in %s after the four-eyes-approved plan applied", table, fourEyesPGDatabase)
	}
	d.logf("PASS the installer-owned four-eyes control refuses a self-approval, admits a distinct one, and the approved plan converges")
}

// podMetadataAdmission meets a service mesh, a policy engine or a managed
// platform where it decides what a Pod carries: spec.execution.podMetadata
// (#447). A ValidatingAdmissionPolicy, which every supported minor serves,
// stands in for all of them and refuses an operation Pod of this row's
// schemas unless it carries the mesh opt-out annotation. A schema that
// declares nothing is refused at Pod creation and reports why; a schema that
// declares the annotation and a label runs every operation under the policy
// and converges. The policy is cluster-scoped, and the cleanup removes it.
func (d *dataPlane) podMetadataAdmission() {
	d.t.Helper()
	d.logf("checking declared Pod metadata against a namespace admission policy")
	const (
		schemaName     = "e2e-pod-metadata-postgresql"
		refusedSchema  = "e2e-pod-metadata-refused-postgresql"
		key            = "e2e/admission/pod-metadata-postgresql"
		refusedKey     = "e2e/admission/pod-metadata-refused-postgresql"
		table          = "e2e_pod_metadata_widgets"
		deleteDeadline = 180 * time.Second
	)
	reference := d.registryReference("pod-metadata-postgresql")
	digest := d.publishSchema("pod-metadata-postgresql", "v1", "postgres", reference, "")

	d.mustApply(podMetadataPolicyDocuments(podMetadataPolicyName, d.in.TestNamespace, schemaName, refusedSchema,
		podMetadataAnnotation)...)
	d.podMetadataPolicyCreated = true

	// A policy is enforced about a second after it is written. A server dry
	// run of a Pod the policy must refuse says when it is, and the refusal has
	// to name the policy: a Pod refused for another reason proves nothing
	// about this one.
	deadline := time.Now().Add(waitTimeout)
	for {
		probe := &unstructured.Unstructured{Object: podMetadataProbePod(d.in.TestNamespace, refusedSchema, d.in.ExecutorImage)}
		err := d.cluster.Client.Create(d.ctx, probe, client.DryRunAll, client.FieldValidation("Strict"))
		if err != nil && strings.Contains(err.Error(), podMetadataPolicyName) {
			break
		}
		if !time.Now().Before(deadline) {
			refusal := ""
			if err != nil {
				refusal = err.Error()
			}
			d.fatalf("the %s policy did not start refusing a Pod without the declared annotation: %s",
				podMetadataPolicyName, refusal)
		}
		d.sleep(2 * time.Second)
	}

	// Both schemas name a database of the row's own: the second converges,
	// and against the lifecycle's database its plan would also drop
	// e2e_widgets and be refused as destructive. The first never reaches the
	// database and names the same one, so nothing in this row touches another
	// row's.
	//
	// A schema that declares nothing: its Resolve Job stands, its Pod is
	// refused at creation, and the resource reports the refusal the API server
	// gave.
	d.createIsolatedPostgreSQLDatabase(podMetadataPGDatabase, podMetadataPGSecret, d.credentials.podMetadataPGURL)
	d.createSchemaResource(schemaResource{
		name: refusedSchema, engine: "PostgreSQL", secret: podMetadataPGSecret, reference: reference,
		coordinationKey: refusedKey, failureRetry: "5s", interval: approvalInterval, apply: "Always",
	})
	matched := d.waitForSchema(refusedSchema,
		"the schema declaring no Pod metadata to report the policy refusal as PodAdmissionRefused",
		podAdmissionRefusalReported)
	// The claim it reports on, and the Job the message names, come from the
	// document that matched rather than from a later read.
	refusedJob := matched.Status.ActiveOperation.JobName
	if refusedJob == "" {
		d.fatalf("%s holds an operation that names no Job", refusedSchema)
	}
	if !readyConditionNamesJob(matched, refusedJob) {
		d.fatalf("%s does not name the Job whose Pod was refused in its Ready condition", refusedSchema)
	}
	// Nothing ran beside the credential: the Job has no Pod, active or done,
	// and the namespace holds no Pod of this schema.
	refusedWorkload := d.job(refusedJob)
	if !refusedJobIdle(refusedWorkload) {
		d.fatalf("the refused Job %s has a Pod or a verdict, so the refusal proved nothing", refusedJob)
	}
	pods := &corev1.PodList{}
	d.mustList(pods, client.MatchingLabels{labelSchema: refusedSchema})
	if len(pods.Items) != 0 {
		d.fatalf("an operation Pod of %s exists under a policy that refuses it", refusedSchema)
	}
	// The record the condition was read from, and the Event the resource
	// carries.
	failures := &corev1.EventList{}
	d.mustList(failures, client.MatchingFields{"involvedObject.name": refusedJob, "reason": "FailedCreate"})
	if !failedCreateNamesPolicy(failures.Items, refusedJob, podMetadataPolicyName) {
		d.fatalf("the Job controller recorded no FailedCreate naming %s against %s", podMetadataPolicyName, refusedJob)
	}
	refusals := &corev1.EventList{}
	d.mustList(refusals, client.MatchingFields{"involvedObject.name": refusedSchema, "reason": "PodAdmissionRefused"})
	if len(refusals.Items) == 0 {
		d.fatalf("%s carries no PodAdmissionRefused Event", refusedSchema)
	}
	// This Job never becomes terminal because admission created no Pod. Audit
	// its exact identity and refusal while the policy still prevents execution.
	d.mustList(pods)
	d.check(podAdmissionRefusalAudit(matched, refusedWorkload, pods.Items, failures.Items, podMetadataPolicyName),
		"audit the exact admission-refused Resolve before its schema is deleted")
	if !executionIdentityOnJob(refusedWorkload, d.controller) {
		d.fatalf("the admission-refused Job lacks its exact controller execution identity")
	}
	d.scan(append(append(d.jsonBytes(matched), d.jsonBytes(refusedWorkload)...),
		append(d.jsonBytes(failures), d.jsonBytes(refusals)...)...), "the exact no-Pod admission-refusal evidence")
	d.scan(d.jsonBytes(pods), "the complete Pod inventory before admission-refusal cleanup")
	d.deleteAndWait(refusedSchema, deleteDeadline)
	deadline = time.Now().Add(deleteDeadline)
	for {
		remaining := &batchv1.Job{}
		err := d.get(refusedJob, remaining)
		if apierrors.IsNotFound(err) {
			break
		}
		if err != nil || remaining.UID != refusedWorkload.UID || remaining.Status.Active != 0 || remaining.Status.Succeeded != 0 || remaining.Status.Failed != 0 {
			d.fatalf("the exact admission-refused Job changed before no-Pod cleanup completed")
		}
		if !time.Now().Before(deadline) {
			d.fatalf("the exact admission-refused Job survived its deleted schema")
		}
		d.sleep(time.Second)
	}
	d.mustList(pods)
	for _, pod := range pods.Items {
		if podMatchesAdmissionRefusal(pod, matched.Namespace, refusedSchema, refusedWorkload.UID) {
			d.fatalf("an admission-refused workload acquired a Pod before cleanup finished")
		}
	}
	d.scan(d.jsonBytes(pods), "the complete Pod inventory after admission-refusal cleanup")
	d.audited.add(string(refusedWorkload.UID))
	d.fullyAudited.add(string(refusedWorkload.UID))
	d.logf("PASS exact admission-refused Job UID %s was audited and removed without creating a Pod", refusedWorkload.UID)

	// A schema that declares the opt-out and a label of its own runs every
	// operation under the same policy: each Pod is admitted because it carries
	// what was declared.
	declaration := map[string]any{
		"labels":      map[string]any{podMetadataLabel: "platform"},
		"annotations": map[string]any{podMetadataAnnotation: "false"},
	}
	d.createSchemaResource(schemaResource{
		name: schemaName, engine: "PostgreSQL", secret: podMetadataPGSecret, reference: reference,
		coordinationKey: key, failureRetry: "5s", interval: approvalInterval, apply: "Always", podMetadata: declaration,
	})
	stored, _, _ := unstructured.NestedFieldNoCopy(d.unstructuredSchema(schemaName).Object, "spec", "execution", "podMetadata")
	if !reflect.DeepEqual(stored, declaration) {
		d.fatalf("%s did not persist the declared Pod metadata", schemaName)
	}
	d.waitForSchema(schemaName, "the schema declaring its Pod metadata to apply under the policy and converge",
		func(s *ptahv1alpha1.PtahSchema) bool { return convergedOn(s, digest) })
	if count := d.isolatedPostgreSQLTableCount(podMetadataPGDatabase, table); count != "1" {
		d.fatalf("%s is not present in %s after the declared-metadata schema applied", table, podMetadataPGDatabase)
	}
	// Every Job of the schema carries the declaration on itself and on its
	// template, under the operator's five labels, and the Apply is among them.
	jobs := &batchv1.JobList{}
	d.mustList(jobs, client.MatchingLabels{labelSchema: schemaName})
	if len(jobs.Items) < 1 {
		d.fatalf("no operation Job of %s is left to read the declared metadata from", schemaName)
	}
	if err := declaredMetadataOnJobs(jobs.Items, podMetadataLabel, podMetadataAnnotation); err != nil {
		d.fatalf("an operation Job of %s does not carry the declared metadata beside the operator's own: %v", schemaName, err)
	}
	// And the Pods the policy admitted carry it, which is what the policy read.
	admitted := &corev1.PodList{}
	d.mustList(admitted, client.MatchingLabels{labelSchema: schemaName, podMetadataLabel: "platform"})
	if len(admitted.Items) < 1 {
		d.fatalf("no operation Pod of %s carrying the declared label is left to read", schemaName)
	}
	if !declaredAnnotationOnPods(admitted.Items, podMetadataAnnotation) {
		d.fatalf("an operation Pod of %s was admitted without the declared annotation", schemaName)
	}
	if err := d.removePodMetadataPolicy(); err != nil {
		d.fatalf("could not remove the %s policy after the Pod-metadata row: %v", podMetadataPolicyName, err)
	}
	d.podMetadataPolicyCreated = false
	d.logf("PASS declared Pod metadata reaches every operation Pod under a namespace admission policy, and a Pod the policy refuses is reported as PodAdmissionRefused")
}

// deleteAndWait deletes a schema and waits, as kubectl delete --wait did, for
// the object it deleted to be gone: absent, or replaced by another under the
// same name.
func (d *dataPlane) deleteAndWait(name string, bound time.Duration) {
	d.t.Helper()
	schema := d.schema(name)
	if err := d.cluster.Client.Delete(d.ctx, schema); err != nil {
		d.fatalf("could not delete %s with its refused Job standing: %v", name, err)
	}
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		current := &ptahv1alpha1.PtahSchema{}
		err := d.get(name, current)
		if apierrors.IsNotFound(err) || (err == nil && current.UID != schema.UID) {
			return
		}
		d.sleep(time.Second)
	}
	d.fatalf("could not delete %s with its refused Job standing: it was still there after %s", name, bound)
}

// removePodMetadataPolicy removes the cluster-scoped policy and its binding,
// binding first. The namespace goes with the cluster, a policy does not.
func (d *dataPlane) removePodMetadataPolicy() error {
	ctx, cancel := d.liveContext()
	defer cancel()
	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	binding.Name = podMetadataPolicyName
	if err := d.cluster.Client.Delete(ctx, binding); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	policy := &admissionregistrationv1.ValidatingAdmissionPolicy{}
	policy.Name = podMetadataPolicyName
	if err := d.cluster.Client.Delete(ctx, policy); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
