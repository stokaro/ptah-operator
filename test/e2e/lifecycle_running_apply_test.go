package e2e

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/controller"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/workload"
)

// The fixture tests were hack/predecessorapplyfixture's, which computed the
// bundle in a separate program; the phase computes it now, so they hold the
// phase's own builder.

const predecessorApplyTestURL = "postgres://user:pass@db.example.svc.cluster.local:5432/appdb?sslmode=disable"

func predecessorApplyTestDigest(fill byte) string {
	return "sha256:" + strings.Repeat(string(fill), 64)
}

func predecessorApplyTestSchema() *ptahv1alpha1.PtahSchema {
	return &ptahv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "upgrade-proof", Name: "predecessor-running-apply", UID: types.UID("schema-uid"), Generation: 2,
		},
		Spec: ptahv1alpha1.PtahSchemaSpec{
			Target: ptahv1alpha1.DatabaseTargetSpec{
				Engine: ptahv1alpha1.DatabaseEnginePostgreSQL, CoordinationKey: "upgrade-proof",
			},
			Desired: ptahv1alpha1.OCIArtifactSourceSpec{
				OCIRef: "oci://example.invalid/schema@" + predecessorApplyTestDigest('e'),
			},
			Policy: ptahv1alpha1.ReconciliationPolicy{
				Apply: ptahv1alpha1.ApplyPolicyAlways, DriftSeverity: "all",
				LockTimeout: metav1.Duration{Duration: 30 * time.Second}, TransactionMode: "file",
			},
			Execution: ptahv1alpha1.ExecutionSpec{ConnectTimeout: metav1.Duration{Duration: 10 * time.Second}},
		},
		Status: ptahv1alpha1.PtahSchemaStatus{ExecutionBinding: &ptahv1alpha1.ExecutionBindingStatus{
			Epoch: "v1-11111111111111111111111111111111", PtahVersion: "v1.2.3", ControllerStateVersion: 1,
			ExecutorImage: "registry.invalid/ptah@" + predecessorApplyTestDigest('a'), RunnerProtocolVersion: 4,
		}},
	}
}

func predecessorApplyTestManager() predecessorApplyManager {
	return predecessorApplyManager{
		controllerImage:    "registry.invalid/controller@" + predecessorApplyTestDigest('f'),
		controllerRevision: "0123456789abcdef0123456789abcdef01234567",
		runnerImage:        "registry.invalid/runner@" + predecessorApplyTestDigest('b'),
	}
}

func predecessorApplyTestPlan() []byte {
	return []byte(`{"format_version":1,"name":"upgrade-proof","dialect":"postgres","from_fingerprint":"` +
		predecessorApplyTestDigest('c') + `","to_fingerprint":"` + predecessorApplyTestDigest('d') +
		`","destructive":false,"statements":[{"sql":"SELECT pg_advisory_lock(742019370001)","severity":"safe","reason":"upgrade quiescence proof"}]}` + "\n")
}

// The fixture binds what the manager that dispatches this Apply re-derives:
// the plan fingerprint, the manager identity the contract carries, and the
// target identity the runner recomputes from the database URL. Each of these
// was wrong once, and each time the Apply exited before opening a connection.
func TestPredecessorApplyFixtureBindsTheCurrentPlanContract(t *testing.T) {
	t.Parallel()
	schema := predecessorApplyTestSchema()
	planData := predecessorApplyTestPlan()
	manager := predecessorApplyTestManager()
	bundle, err := predecessorApplyFixture(schema, manager, planData, "policy-uid", []byte("version: 1\n"),
		predecessorApplyTestURL, time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	wantIdentity, err := runner.TargetIdentityDigest(predecessorApplyTestURL)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.SchemaStatus.Target.IdentityDigest != wantIdentity || bundle.Plan.Spec.TargetIdentityDigest != wantIdentity {
		t.Fatalf("target identity digests = %q, %q; want the runner's %q",
			bundle.SchemaStatus.Target.IdentityDigest, bundle.Plan.Spec.TargetIdentityDigest, wantIdentity)
	}
	binding := schema.Status.ExecutionBinding
	if bundle.Plan.Spec.ContractVersion != fingerprint.CurrentPlanContractVersion ||
		bundle.Plan.Spec.ControllerImage != manager.controllerImage ||
		bundle.Plan.Spec.ControllerRevision != manager.controllerRevision ||
		bundle.Plan.Spec.RunnerImage != manager.runnerImage ||
		bundle.Plan.Spec.ControllerStateVersion != binding.ControllerStateVersion {
		t.Fatalf("plan does not carry the current manager contract: %#v", bundle.Plan.Spec)
	}
	if bundle.SchemaStatus.Plan == nil || bundle.SchemaStatus.Plan.Name != bundle.Plan.Name ||
		bundle.SchemaStatus.Plan.UID != "" || bundle.SchemaStatus.Phase != ptahv1alpha1.PhaseReadyToApply {
		t.Fatalf("schema status is not ready for an API-assigned plan UID: %#v", bundle.SchemaStatus)
	}
	if len(bundle.Plan.Spec.Chunks) != 1 || bundle.Plan.Spec.Chunks[0].Digest != fingerprint.DigestBytes(planData) ||
		bundle.Plan.Spec.Chunks[0].Size != int32(len(planData)) {
		t.Fatalf("chunk binding = %#v", bundle.Plan.Spec.Chunks)
	}
	wantPolicy, err := controller.PolicyFingerprint(schema)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Plan.Spec.PolicyFingerprint != wantPolicy {
		t.Fatalf("policy fingerprint = %q, want the controller's %q", bundle.Plan.Spec.PolicyFingerprint, wantPolicy)
	}
	// The webhook recomputes the fingerprint from the published spec through
	// planstore.Binding; the two must agree or the plan is refused.
	wantFingerprint, err := planstore.Binding(schema.UID, bundle.Plan.Spec).Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Plan.Spec.Fingerprint != wantFingerprint {
		t.Fatalf("fingerprint = %q, want %q", bundle.Plan.Spec.Fingerprint, wantFingerprint)
	}
	if !strings.HasPrefix(bundle.Plan.Name, "ptah-plan-") || bundle.Plan.Spec.Chunks[0].Name != bundle.Plan.Name+"-000" {
		t.Fatalf("plan and chunk names = %q, %q", bundle.Plan.Name, bundle.Plan.Spec.Chunks[0].Name)
	}
	if !predecessorApplyPlanCarriesContract(&bundle.Plan, fingerprint.CurrentPlanContractVersion) {
		t.Fatal("the phase's contract check refused the plan the builder made")
	}
}

// A plan whose manager fields are empty is refused by the admission guard
// over plan writes, so the fixture refuses to write one at all.
func TestPredecessorApplyFixtureRefusesAPlanWithoutItsPublisher(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*predecessorApplyManager){
		"controller image":    func(manager *predecessorApplyManager) { manager.controllerImage = "" },
		"controller revision": func(manager *predecessorApplyManager) { manager.controllerRevision = "" },
		"runner image":        func(manager *predecessorApplyManager) { manager.runnerImage = "" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			manager := predecessorApplyTestManager()
			mutate(&manager)
			_, err := predecessorApplyFixture(predecessorApplyTestSchema(), manager, predecessorApplyTestPlan(), "policy",
				[]byte("policy"), predecessorApplyTestURL, time.Now())
			if err == nil || !strings.Contains(err.Error(), "identity is incomplete") {
				t.Fatalf("predecessorApplyFixture() error = %v", err)
			}
		})
	}
}

func TestPredecessorApplyFixtureRefusesWhatItCannotBind(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		mutate func(*ptahv1alpha1.PtahSchema)
		url    string
		want   string
	}{
		"no execution binding": {func(s *ptahv1alpha1.PtahSchema) { s.Status.ExecutionBinding = nil }, predecessorApplyTestURL, "execution binding"},
		"incomplete binding":   {func(s *ptahv1alpha1.PtahSchema) { s.Status.ExecutionBinding.PtahVersion = "" }, predecessorApplyTestURL, "incomplete"},
		"tag reference": {func(s *ptahv1alpha1.PtahSchema) {
			s.Spec.Desired.OCIRef = "oci://example.invalid/schema:v1"
		}, predecessorApplyTestURL, "digest-pinned"},
		"no UID":          {func(s *ptahv1alpha1.PtahSchema) { s.UID = "" }, predecessorApplyTestURL, "UID"},
		"no database URL": {func(*ptahv1alpha1.PtahSchema) {}, "", "database URL"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			schema := predecessorApplyTestSchema()
			test.mutate(schema)
			_, err := predecessorApplyFixture(schema, predecessorApplyTestManager(), predecessorApplyTestPlan(), "policy",
				[]byte("policy"), test.url, time.Now())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("predecessorApplyFixture() error = %v, want one naming %q", err, test.want)
			}
		})
	}
}

// The identity is read from the Pod template, which the admission snapshot
// pins; a Job that records none of it is refused.
func TestPredecessorApplyManagerOfReadsTheDispatchingManager(t *testing.T) {
	t.Parallel()
	job := &batchv1.Job{}
	job.Spec.Template.Annotations = map[string]string{
		workload.AnnotationControllerImage:    "registry.invalid/controller@" + predecessorApplyTestDigest('f'),
		workload.AnnotationControllerRevision: "release-1",
	}
	job.Spec.Template.Spec.InitContainers = []corev1.Container{{
		Name: "install-runner", Image: "registry.invalid/runner@" + predecessorApplyTestDigest('b'),
	}}
	manager, err := predecessorApplyManagerOf(job)
	if err != nil {
		t.Fatal(err)
	}
	if manager.controllerImage != "registry.invalid/controller@"+predecessorApplyTestDigest('f') ||
		manager.controllerRevision != "release-1" || manager.runnerImage != "registry.invalid/runner@"+predecessorApplyTestDigest('b') {
		t.Fatalf("predecessorApplyManagerOf() = %#v", manager)
	}
	job.Spec.Template.Spec.InitContainers = nil
	if _, err := predecessorApplyManagerOf(job); err == nil {
		t.Fatal("predecessorApplyManagerOf() accepted a Job that records no runner image")
	}
}

// The Apply under test is held open by a database barrier, not by anything
// it changes, and ReadyToApply under Always is published only for a plan that
// changes no privilege.
func TestPredecessorApplyFixtureRefusesADestructiveOrPrivilegedPlan(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct{ plan, want string }{
		"destructive": {`"destructive":true,"statements":[{"sql":"DROP TABLE widgets","severity":"destructive","reason":"proof"}]`, "non-destructive"},
		"privileged":  {`"destructive":false,"statements":[{"sql":"GRANT SELECT ON widgets TO PUBLIC","severity":"safe","reason":"proof"}]`, "must change no privilege"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			plan := []byte(`{"format_version":1,"name":"upgrade-proof","dialect":"postgres","from_fingerprint":"` +
				predecessorApplyTestDigest('c') + `","to_fingerprint":"` + predecessorApplyTestDigest('d') + `",` + test.plan + "}\n")
			_, err := predecessorApplyFixture(predecessorApplyTestSchema(), predecessorApplyTestManager(), plan, "policy-uid",
				[]byte("version: 1\n"), predecessorApplyTestURL, time.Now())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("predecessorApplyFixture() error = %v", err)
			}
		})
	}
}

func TestPredecessorApplyPlanCarriesContractRefusesEachClause(t *testing.T) {
	t.Parallel()
	bundle, err := predecessorApplyFixture(predecessorApplyTestSchema(), predecessorApplyTestManager(), predecessorApplyTestPlan(),
		"policy-uid", []byte("version: 1\n"), predecessorApplyTestURL, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahSchemaPlan){
		"another contract": func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ContractVersion++ },
		"a tagged manager": func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ControllerImage = "registry.invalid/controller:v1" },
		"no revision":      func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ControllerRevision = "" },
		"no state version": func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.ControllerStateVersion = 0 },
		"two chunks":       func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.Chunks = append(p.Spec.Chunks, p.Spec.Chunks[0]) },
		"no chunk":         func(p *ptahv1alpha1.PtahSchemaPlan) { p.Spec.Chunks = nil },
		"a spaced repository": func(p *ptahv1alpha1.PtahSchemaPlan) {
			p.Spec.ControllerImage = "registry invalid/controller@" + predecessorApplyTestDigest('f')
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			plan := bundle.Plan.DeepCopy()
			mutate(plan)
			if predecessorApplyPlanCarriesContract(plan, fingerprint.CurrentPlanContractVersion) {
				t.Fatal("the contract check accepted the mistake")
			}
		})
	}
}

func TestPredecessorApplyBarrierDatabase(t *testing.T) {
	t.Parallel()
	if database, err := predecessorApplyBarrierDatabase([]byte(`{"database":"ptah_e2e","username":"u"}`)); err != nil || database != "ptah_e2e" {
		t.Fatalf("predecessorApplyBarrierDatabase() = %q, %v", database, err)
	}
	for _, content := range []string{`not json`, `{}`, `{"database":null}`, `{"database":false}`} {
		if _, err := predecessorApplyBarrierDatabase([]byte(content)); err == nil || !strings.Contains(err.Error(), "no database") {
			t.Errorf("predecessorApplyBarrierDatabase(%s) error = %v, want no database", content, err)
		}
	}
	for _, content := range []string{`{"database":""}`, `{"database":"1st"}`, `{"database":"a-b"}`, `{"database":"a b"}`, `{"database":7}`} {
		if _, err := predecessorApplyBarrierDatabase([]byte(content)); err == nil || !strings.Contains(err.Error(), "unusable") {
			t.Errorf("predecessorApplyBarrierDatabase(%s) error = %v, want unusable", content, err)
		}
	}
}

func TestPredecessorApplyDatabaseCredentials(t *testing.T) {
	t.Parallel()
	valid := `{"username":"ptah_e2e","password":"s3cret.pw","database":"ptah_e2e_external","url":"postgres://x"}`
	credentials, err := predecessorApplyDatabaseCredentials([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if got := predecessorApplyDatabaseURL(credentials, predecessorApplyDatabase+":5432"); got !=
		"postgres://ptah_e2e:s3cret.pw@running-apply-database:5432/ptah_e2e_external?sslmode=disable" {
		t.Fatalf("predecessorApplyDatabaseURL() = %q", got)
	}
	for name, content := range map[string]string{
		"an unknown field":     `{"username":"u","password":"p","database":"d","url":"x","extra":1}`,
		"no username":          `{"password":"p","database":"d","url":"x"}`,
		"a password with an @": `{"username":"u","password":"p@ss","database":"d","url":"x"}`,
		"an empty password":    `{"username":"u","password":"","database":"d","url":"x"}`,
		"a dashed database":    `{"username":"u","password":"p","database":"d-b","url":"x"}`,
		"not a document":       `[]`,
	} {
		if _, err := predecessorApplyDatabaseCredentials([]byte(content)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestPredecessorApplyBarrierQueriesSelectTheBarrier(t *testing.T) {
	t.Parallel()
	for name, query := range map[string]string{
		"held": predecessorApplyHeldQuery(), "contention": predecessorApplyContentionQuery(), "release": predecessorApplyReleaseQuery(),
	} {
		if !strings.Contains(query, "application_name = '"+predecessorApplyBarrierApplication+"'") {
			t.Errorf("the %s query does not select the barrier's session: %s", name, query)
		}
	}
	if !strings.Contains(predecessorApplyContentionQuery(), "NOT waiting.granted AND waiting.pid <> held.pid") {
		t.Error("the contention query does not count a distinct waiter")
	}
	if !strings.Contains(predecessorApplyReleaseQuery(), "pid <> pg_backend_pid()") {
		t.Error("the release query would terminate its own session")
	}
	// What psql printed, read as command substitution read it.
	for output, want := range map[string]string{"1\n": "1", "t\n": "t", "t\nt\n": "t\nt", "0": "0", "": ""} {
		if got := predecessorApplyValue(output); got != want {
			t.Errorf("predecessorApplyValue(%q) = %q, want %q", output, got, want)
		}
	}
}

func TestPredecessorApplyDockerTarget(t *testing.T) {
	t.Parallel()
	container := strings.Repeat("0123456789abcdef", 4)
	if err := predecessorApplyDockerTarget("remote-dev-container", container); err != nil {
		t.Fatal(err)
	}
	for _, dockerContext := range []string{"", "default", "orbstack"} {
		if err := predecessorApplyDockerTarget(dockerContext, container); err == nil || !strings.Contains(err.Error(), "E2E_DOCKER_CONTEXT") {
			t.Errorf("context %q error = %v", dockerContext, err)
		}
	}
	for _, id := range []string{"", container[:12], strings.ToUpper(container), container + "0"} {
		if err := predecessorApplyDockerTarget("remote", id); err == nil || !strings.Contains(err.Error(), "CONTAINER_ID") {
			t.Errorf("container %q error = %v", id, err)
		}
	}
}

func TestPredecessorApplyExecutorImage(t *testing.T) {
	t.Parallel()
	image := "registry.invalid:5000/ptah-executor@" + predecessorApplyTestDigest('a')
	deployment := func(containers ...corev1.Container) *appsv1.Deployment {
		d := &appsv1.Deployment{}
		d.Spec.Template.Spec.Containers = containers
		return d
	}
	got, err := predecessorApplyExecutorImage(deployment(corev1.Container{Name: "manager", Args: []string{"--leader-elect", "--executor-image=" + image}}))
	if err != nil || got != image {
		t.Fatalf("predecessorApplyExecutorImage() = %q, %v", got, err)
	}
	if registry := predecessorApplyRegistry(got); registry != "registry.invalid:5000" {
		t.Fatalf("predecessorApplyRegistry() = %q", registry)
	}
	for name, d := range map[string]*appsv1.Deployment{
		"no argument":  deployment(corev1.Container{Name: "manager", Args: []string{"--leader-elect"}}),
		"two":          deployment(corev1.Container{Name: "manager", Args: []string{"--executor-image=" + image, "--executor-image=" + image}}),
		"empty":        deployment(corev1.Container{Name: "manager", Args: []string{"--executor-image="}}),
		"on a sidecar": deployment(corev1.Container{Name: "sidecar", Args: []string{"--executor-image=" + image}}),
	} {
		if _, err := predecessorApplyExecutorImage(d); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestPredecessorApplyPlanRewritesTheNativePlan(t *testing.T) {
	t.Parallel()
	native := []byte(`{"format_version":1,"name":"native","dialect":"postgres","from_fingerprint":"` +
		predecessorApplyTestDigest('c') + `","to_fingerprint":"` + predecessorApplyTestDigest('d') +
		`","destructive":true,"statements":[{"sql":"CREATE TABLE t (id int)","severity":"safe","reason":"x"}]}` + "\n")
	plan, err := predecessorApplyPlan(native, predecessorApplySchema)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(plan, []byte("}\n")) || bytes.Count(plan, []byte("\n")) != 1 {
		t.Fatalf("the plan is not one compact line: %q", plan)
	}
	decoded, err := dataplane.DecodePlan(plan, string(ptahv1alpha1.DatabaseEnginePostgreSQL))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Destructive || len(decoded.Statements) != 1 ||
		decoded.Statements[0].SQL != "SELECT pg_advisory_lock(742019370001)" ||
		decoded.FromFingerprint != predecessorApplyTestDigest('c') || decoded.ToFingerprint != predecessorApplyTestDigest('d') {
		t.Fatalf("decoded plan = %#v", decoded)
	}
	if !bytes.Contains(plan, []byte(`"name":"`+predecessorApplySchema+`"`)) {
		t.Fatalf("the plan does not carry its name: %s", plan)
	}
	// A field the rewrite does not touch passes through, a number exactly.
	extended := bytes.Replace(native, []byte(`"dialect"`), []byte(`"large":12345678901234567890,"dialect"`), 1)
	kept, err := predecessorApplyPlan(extended, predecessorApplySchema)
	if err != nil || !bytes.Contains(kept, []byte(`"large":12345678901234567890`)) {
		t.Fatalf("the rewrite lost a field it does not own: %s, %v", kept, err)
	}
	for name, content := range map[string]string{
		"another format":   `{"format_version":2,"from_fingerprint":"` + predecessorApplyTestDigest('c') + `","to_fingerprint":"` + predecessorApplyTestDigest('d') + `"}`,
		"a string format":  `{"format_version":"1","from_fingerprint":"` + predecessorApplyTestDigest('c') + `","to_fingerprint":"` + predecessorApplyTestDigest('d') + `"}`,
		"no from":          `{"format_version":1,"to_fingerprint":"` + predecessorApplyTestDigest('d') + `"}`,
		"a short to":       `{"format_version":1,"from_fingerprint":"` + predecessorApplyTestDigest('c') + `","to_fingerprint":"sha256:abc"}`,
		"not a plan":       `the planner failed`,
		"an absent format": `{"from_fingerprint":"` + predecessorApplyTestDigest('c') + `","to_fingerprint":"` + predecessorApplyTestDigest('d') + `"}`,
	} {
		if _, err := predecessorApplyPlan([]byte(content), "x"); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestPredecessorApplyReadyStatusBindsTheGenerationAndPlan(t *testing.T) {
	t.Parallel()
	bundle, err := predecessorApplyFixture(predecessorApplyTestSchema(), predecessorApplyTestManager(), predecessorApplyTestPlan(),
		"policy-uid", []byte("version: 1\n"), predecessorApplyTestURL, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ready := predecessorApplyReadyStatus(bundle.SchemaStatus, 7, "plan-uid")
	if ready.ObservedGeneration != 7 || ready.Plan.UID != "plan-uid" || len(ready.Conditions) != 3 {
		t.Fatalf("ready status = %#v", ready)
	}
	for _, condition := range ready.Conditions {
		if condition.ObservedGeneration != 7 {
			t.Fatalf("condition %s observes generation %d", condition.Type, condition.ObservedGeneration)
		}
	}
	if bundle.SchemaStatus.ObservedGeneration == 7 || bundle.SchemaStatus.Plan.UID != "" ||
		bundle.SchemaStatus.Conditions[0].ObservedGeneration == 7 {
		t.Fatal("the ready status rewrote the bundle it was made from")
	}
	if planless := predecessorApplyReadyStatus(ptahv1alpha1.PtahSchemaStatus{}, 1, "u"); planless.Plan == nil || planless.Plan.UID != "u" {
		t.Fatalf("a status with no plan did not gain one with the UID: %#v", planless.Plan)
	}
}

func predecessorApplyTestDocument(t *testing.T, document string) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(document), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestPredecessorApplyTerminalFailure(t *testing.T) {
	t.Parallel()
	for document, want := range map[string]bool{
		`{"status":{"pendingObservation":{"outcome":"OutcomeUnknown"}}}`:               true,
		`{"status":{"conditions":[{"type":"ReconciliationFailed","status":"True"}]}}`:  true,
		`{"status":{"pendingObservation":{"outcome":"ApplySucceeded"}}}`:               false,
		`{"status":{"conditions":[{"type":"ReconciliationFailed","status":"False"}]}}`: false,
		`{"status":{"conditions":[{"type":"Ready","status":"True"}]}}`:                 false,
		`{"status":{}}`: false,
		`{}`:            false,
	} {
		if got := predecessorApplyTerminalFailure(predecessorApplyTestDocument(t, document)); got != want {
			t.Errorf("predecessorApplyTerminalFailure(%s) = %v, want %v", document, got, want)
		}
	}
}

func TestPredecessorApplyDispatchedJob(t *testing.T) {
	t.Parallel()
	for document, want := range map[string][2]string{
		`{"status":{"activeOperation":{"type":"Apply","dispatchStarted":true,"jobName":"j","jobUID":"u"}}}`:   {"j", "u"},
		`{"status":{"activeOperation":{"type":"Apply","dispatchStarted":true,"jobName":"j"}}}`:                {"j", ""},
		`{"status":{"activeOperation":{"type":"Apply","dispatchStarted":false,"jobName":"j","jobUID":"u"}}}`:  {"", "u"},
		`{"status":{"activeOperation":{"type":"Resolve","dispatchStarted":true,"jobName":"j","jobUID":"u"}}}`: {"", "u"},
		`{"status":{"activeOperation":{"type":"Apply","jobName":"j"}}}`:                                       {"", ""},
		`{"status":{}}`: {"", ""},
	} {
		name, uid := predecessorApplyDispatchedJob(predecessorApplyTestDocument(t, document))
		if name != want[0] || uid != want[1] {
			t.Errorf("predecessorApplyDispatchedJob(%s) = %q, %q; want %q, %q", document, name, uid, want[0], want[1])
		}
	}
}

func TestPredecessorApplyRunningPod(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	pod := func(name string, phase corev1.PodPhase, owner metav1.OwnerReference) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid"), OwnerReferences: []metav1.OwnerReference{owner}},
			Status:     corev1.PodStatus{Phase: phase},
		}
	}
	owner := metav1.OwnerReference{APIVersion: "batch/v1", Kind: "Job", UID: "job-uid", Controller: &yes}
	found, ok := predecessorApplyRunningPod([]corev1.Pod{
		pod("pending", corev1.PodPending, owner), pod("running", corev1.PodRunning, owner),
	}, "job-uid")
	if !ok || found.Name != "running" {
		t.Fatalf("predecessorApplyRunningPod() = %q, %v", found.Name, ok)
	}
	for name, pods := range map[string][]corev1.Pod{
		"none running": {pod("a", corev1.PodSucceeded, owner)},
		"two running":  {pod("a", corev1.PodRunning, owner), pod("b", corev1.PodRunning, owner)},
		"another Job": {pod("a", corev1.PodRunning, metav1.OwnerReference{
			APIVersion: "batch/v1", Kind: "Job", UID: "other", Controller: &yes})},
		"not the controller": {pod("a", corev1.PodRunning, metav1.OwnerReference{
			APIVersion: "batch/v1", Kind: "Job", UID: "job-uid", Controller: &no})},
		"no controller field": {pod("a", corev1.PodRunning, metav1.OwnerReference{
			APIVersion: "batch/v1", Kind: "Job", UID: "job-uid"})},
		"another kind": {pod("a", corev1.PodRunning, metav1.OwnerReference{
			APIVersion: "batch/v1", Kind: "CronJob", UID: "job-uid", Controller: &yes})},
		"another version": {pod("a", corev1.PodRunning, metav1.OwnerReference{
			APIVersion: "batch/v2", Kind: "Job", UID: "job-uid", Controller: &yes})},
		"nothing": nil,
	} {
		if _, ok := predecessorApplyRunningPod(pods, "job-uid"); ok {
			t.Errorf("%s was accepted", name)
		}
	}
}

const predecessorApplyTestJob = `{
  "metadata": {"name": "apply-job", "namespace": "proof", "uid": "job-uid",
    "labels": {"operator.ptah.run/schema": "running-apply-across-upgrade", "operator.ptah.run/operation": "apply"},
    "annotations": {"operator.ptah.run/plan-fingerprint": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
      "operator.ptah.run/admission-snapshot-digest": "sha256:2222222222222222222222222222222222222222222222222222222222222222"},
    "ownerReferences": [{"kind": "PtahSchema", "uid": "schema-uid"}]},
  "spec": {"backoffLimit": 0, "template": {"metadata": {"annotations": {
    "operator.ptah.run/plan-fingerprint": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
    "operator.ptah.run/admission-snapshot-digest": "sha256:2222222222222222222222222222222222222222222222222222222222222222",
    "operator.ptah.run/controller-image": "registry.invalid/controller@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}}}},
  "status": {"conditions": []}
}`

func TestPredecessorApplyJobCarriesIdentity(t *testing.T) {
	t.Parallel()
	job := predecessorApplyTestDocument(t, predecessorApplyTestJob)
	// The template's annotations have to equal the Job's, so the fixture's
	// template carries the dispatcher on the Job too.
	set(job, "registry.invalid/controller@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		"metadata", "annotations", "operator.ptah.run/controller-image")
	if !predecessorApplyJobCarriesIdentity(job, predecessorApplySchema) {
		t.Fatal("the dispatched Apply Job was refused")
	}
	for name, mutate := range map[string]func(map[string]any){
		"another schema":    func(j map[string]any) { set(j, "other", "metadata", "labels", "operator.ptah.run/schema") },
		"another operation": func(j map[string]any) { set(j, "plan", "metadata", "labels", "operator.ptah.run/operation") },
		"no plan fingerprint": func(j map[string]any) {
			delete(j["metadata"].(map[string]any)["annotations"].(map[string]any), "operator.ptah.run/plan-fingerprint")
		},
		"a short snapshot digest": func(j map[string]any) {
			set(j, "sha256:22", "metadata", "annotations", "operator.ptah.run/admission-snapshot-digest")
		},
		"template annotations differ": func(j map[string]any) {
			set(j, "x", "spec", "template", "metadata", "annotations", "extra")
		},
		"a finished-Job lifetime": func(j map[string]any) { set(j, int64(300), "spec", "ttlSecondsAfterFinished") },
		"no spec":                 func(j map[string]any) { delete(j, "spec") },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mutated := deepCopyMap(job)
			mutate(mutated)
			if predecessorApplyJobCarriesIdentity(mutated, predecessorApplySchema) {
				t.Fatal("the mistake passed as the dispatched identity")
			}
		})
	}
}

func TestPredecessorApplyJobRunningAndTerminal(t *testing.T) {
	t.Parallel()
	running := predecessorApplyTestDocument(t, predecessorApplyTestJob)
	if !predecessorApplyJobRunning(running, "job-uid") || predecessorApplyJobTerminal(running, "job-uid") {
		t.Fatal("a running Job was not read as running")
	}
	if predecessorApplyJobRunning(running, "other") {
		t.Fatal("another UID was read as the running Job")
	}
	withTTL := deepCopyMap(running)
	set(withTTL, int64(300), "spec", "ttlSecondsAfterFinished")
	if predecessorApplyJobRunning(withTTL, "job-uid") {
		t.Fatal("a Job with a finished-Job lifetime was read as running")
	}
	for _, conditionType := range []string{"Complete", "Failed"} {
		finished := deepCopyMap(running)
		set(finished, []any{map[string]any{"type": conditionType, "status": "True"}}, "status", "conditions")
		if predecessorApplyJobRunning(finished, "job-uid") || !predecessorApplyJobTerminal(finished, "job-uid") {
			t.Errorf("a %s Job was not read as finished", conditionType)
		}
		if predecessorApplyJobTerminal(finished, "other") {
			t.Errorf("a %s Job of another UID was read as this one", conditionType)
		}
		unfinished := deepCopyMap(running)
		set(unfinished, []any{map[string]any{"type": conditionType, "status": "False"}}, "status", "conditions")
		if predecessorApplyJobTerminal(unfinished, "job-uid") {
			t.Errorf("a %s=False Job was read as finished", conditionType)
		}
	}
	if predecessorApplyTTL(running) != 0 || predecessorApplyTTL(withTTL) != 300 {
		t.Fatalf("ttl readings = %d, %d", predecessorApplyTTL(running), predecessorApplyTTL(withTTL))
	}
	if got := predecessorApplyDispatcher(running); got != "registry.invalid/controller@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" {
		t.Fatalf("predecessorApplyDispatcher() = %q", got)
	}
	if got := predecessorApplyDispatcher(map[string]any{}); got != "" {
		t.Fatalf("a Job with no template recorded dispatcher %q", got)
	}
}

func TestPredecessorApplyPodIn(t *testing.T) {
	t.Parallel()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "pod-uid"}, Status: corev1.PodStatus{Phase: corev1.PodFailed}}
	if !predecessorApplyPodIn(pod, "pod-uid", corev1.PodSucceeded, corev1.PodFailed) {
		t.Fatal("a failed Pod was not terminal")
	}
	if predecessorApplyPodIn(pod, "pod-uid", corev1.PodRunning) || predecessorApplyPodIn(pod, "other", corev1.PodFailed) {
		t.Fatal("a Pod of another phase or UID was accepted")
	}
}

// The successor may set the finished-Job lifetime and nothing else.
func TestPredecessorApplyJobEvidenceIgnoresOnlyTheLifetime(t *testing.T) {
	t.Parallel()
	before := predecessorApplyTestDocument(t, predecessorApplyTestJob)
	after := deepCopyMap(before)
	set(after, int64(300), "spec", "ttlSecondsAfterFinished")
	set(after, []any{map[string]any{"type": "Complete", "status": "True"}}, "status", "conditions")
	set(after, "12345", "metadata", "resourceVersion")
	set(after, []any{}, "metadata", "finalizers")
	beforeEvidence, err := predecessorApplyJobEvidence(before)
	if err != nil {
		t.Fatal(err)
	}
	afterEvidence, err := predecessorApplyJobEvidence(after)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeEvidence, afterEvidence) {
		t.Fatalf("the lifetime, status, resourceVersion or an empty finalizer list changed the evidence:\n%s\n%s", beforeEvidence, afterEvidence)
	}
	for name, mutate := range map[string]func(map[string]any){
		"a label":           func(j map[string]any) { set(j, "x", "metadata", "labels", "extra") },
		"an annotation":     func(j map[string]any) { set(j, "x", "metadata", "annotations", "extra") },
		"the owner":         func(j map[string]any) { set(j, []any{}, "metadata", "ownerReferences") },
		"a finalizer":       func(j map[string]any) { set(j, []any{"x"}, "metadata", "finalizers") },
		"the spec":          func(j map[string]any) { set(j, int64(1), "spec", "backoffLimit") },
		"the UID":           func(j map[string]any) { set(j, "other", "metadata", "uid") },
		"the template":      func(j map[string]any) { set(j, "x", "spec", "template", "metadata", "annotations", "extra") },
		"the lifetime only": nil,
	} {
		if mutate == nil {
			continue
		}
		mutated := deepCopyMap(before)
		mutate(mutated)
		evidence, err := predecessorApplyJobEvidence(mutated)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(evidence, beforeEvidence) {
			t.Errorf("a change to %s left the evidence unchanged", name)
		}
	}
}

const predecessorApplyTestStaged = `{"status": {
  "executionBinding": {"epoch": "v1-aaaa"},
  "activeOperation": {"id": "op-1", "type": "Apply", "dispatchStarted": true, "jobName": "apply-job",
    "executionBindingID": "v1-aaaa"},
  "conditions": [{"type": "Ready", "status": "False", "reason": "Applying"}]
}}`

func TestPredecessorApplyStagedGapChecks(t *testing.T) {
	t.Parallel()
	staged := predecessorApplyTestDocument(t, predecessorApplyTestStaged)
	for _, check := range predecessorApplyStagedGapChecks {
		if !check.holds(staged, "apply-job") {
			t.Fatalf("the staged gap failed %q", check.reason)
		}
	}
	for reason, mutate := range map[string]func(map[string]any){
		"the claim is no longer an Apply":                           func(s map[string]any) { set(s, "Resolve", "status", "activeOperation", "type") },
		"the claim no longer records a started dispatch":            func(s map[string]any) { set(s, false, "status", "activeOperation", "dispatchStarted") },
		"the claim names another Job":                               func(s map[string]any) { set(s, "other", "status", "activeOperation", "jobName") },
		"the manager recorded the Job UID again before the upgrade": func(s map[string]any) { set(s, "job-uid", "status", "activeOperation", "jobUID") },
		"the Apply was already retired into a pending observation":  func(s map[string]any) { set(s, map[string]any{}, "status", "pendingObservation") },
	} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			mutated := deepCopyMap(staged)
			mutate(mutated)
			for _, check := range predecessorApplyStagedGapChecks {
				if holds := check.holds(mutated, "apply-job"); holds == (check.reason == reason) {
					t.Fatalf("check %q holds = %v for the mistake %q", check.reason, holds, reason)
				}
			}
		})
	}
	// has() on something that is not an object is an error, which -e reads
	// as false: a claim that is gone does not pass as one lacking a UID.
	gone := deepCopyMap(staged)
	delete(gone["status"].(map[string]any), "activeOperation")
	for _, check := range predecessorApplyStagedGapChecks {
		if check.reason == "the manager recorded the Job UID again before the upgrade" && check.holds(gone, "apply-job") {
			t.Fatal("a missing claim passed as one lacking a Job UID")
		}
	}
	if predecessorApplyLacks(map[string]any{}, "pendingObservation", "status") {
		t.Fatal("a document with no status passed as one lacking a pending observation")
	}
}

func predecessorApplyTestAdopted(t *testing.T) map[string]any {
	t.Helper()
	adopted := predecessorApplyTestDocument(t, predecessorApplyTestStaged)
	set(adopted, "job-uid", "status", "activeOperation", "jobUID")
	return adopted
}

func TestPredecessorApplyExclusive(t *testing.T) {
	t.Parallel()
	staged := predecessorApplyTestDocument(t, predecessorApplyTestStaged)
	if !predecessorApplyExclusive(predecessorApplyTestAdopted(t), staged, "apply-job", "job-uid") {
		t.Fatal("the adopted Apply was refused")
	}
	if predecessorApplyExclusive(staged, staged, "apply-job", "job-uid") {
		t.Fatal("the staged reading, with no UID recorded again, passed as adopted")
	}
	for name, mutate := range map[string]func(map[string]any){
		"another epoch":            func(s map[string]any) { set(s, "v1-bbbb", "status", "executionBinding", "epoch") },
		"another claim":            func(s map[string]any) { set(s, "op-2", "status", "activeOperation", "id") },
		"another operation":        func(s map[string]any) { set(s, "Observe", "status", "activeOperation", "type") },
		"another Job":              func(s map[string]any) { set(s, "other", "status", "activeOperation", "jobName") },
		"another UID":              func(s map[string]any) { set(s, "other", "status", "activeOperation", "jobUID") },
		"dispatch not started":     func(s map[string]any) { set(s, false, "status", "activeOperation", "dispatchStarted") },
		"a claim of another epoch": func(s map[string]any) { set(s, "v1-cccc", "status", "activeOperation", "executionBindingID") },
		"a pending observation": func(s map[string]any) {
			set(s, map[string]any{"outcome": "ApplySucceeded"}, "status", "pendingObservation")
		},
		"a binding change reported": func(s map[string]any) {
			set(s, []any{map[string]any{"reason": "ExecutionBindingChanged"}}, "status", "conditions")
		},
		"no claim": func(s map[string]any) { delete(s["status"].(map[string]any), "activeOperation") },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			adopted := predecessorApplyTestAdopted(t)
			mutate(adopted)
			if predecessorApplyExclusive(adopted, staged, "apply-job", "job-uid") {
				t.Fatal("the mistake passed as the adopted Apply")
			}
		})
	}
}

func TestPredecessorApplyAccounted(t *testing.T) {
	t.Parallel()
	staged := predecessorApplyTestDocument(t, predecessorApplyTestStaged)
	const dispatcher = "registry.invalid/controller@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	pending := func(outcome string) map[string]any {
		document := predecessorApplyTestDocument(t, `{"status":{"executionBinding":{"epoch":"v1-aaaa"},"conditions":[]}}`)
		set(document, map[string]any{
			"applyJobName": "apply-job", "applyJobUID": "job-uid", "outcome": outcome,
			"plan":         map[string]any{"executionBindingID": "v1-aaaa"},
			"dispatchedBy": map[string]any{"controllerImage": dispatcher},
		}, "status", "pendingObservation")
		return document
	}
	applied := func() map[string]any {
		document := predecessorApplyTestDocument(t, `{"status":{"executionBinding":{"epoch":"v1-aaaa"}}}`)
		set(document, map[string]any{
			"planRef": map[string]any{"uid": "plan-uid"}, "executionBindingID": "v1-aaaa",
			"dispatchedBy": map[string]any{"controllerImage": dispatcher},
		}, "status", "applied")
		return document
	}
	for name, document := range map[string]map[string]any{
		"applied and observed": pending("ApplySucceeded"), "unknown and observed": pending("OutcomeUnknown"), "recorded": applied(),
	} {
		if !predecessorApplyAccounted(document, staged, "apply-job", "job-uid", "plan-uid", dispatcher) {
			t.Errorf("%s was refused", name)
		}
	}
	for name, test := range map[string]struct {
		document map[string]any
		mutate   func(map[string]any)
	}{
		"a failed outcome":         {pending("ApplyFailed"), func(map[string]any) {}},
		"another Job observed":     {pending("ApplySucceeded"), func(s map[string]any) { set(s, "other", "status", "pendingObservation", "applyJobName") }},
		"another UID observed":     {pending("ApplySucceeded"), func(s map[string]any) { set(s, "other", "status", "pendingObservation", "applyJobUID") }},
		"observed in another plan": {pending("ApplySucceeded"), func(s map[string]any) { set(s, "v1-x", "status", "pendingObservation", "plan", "executionBindingID") }},
		"observed by the successor": {pending("ApplySucceeded"), func(s map[string]any) {
			set(s, "successor", "status", "pendingObservation", "dispatchedBy", "controllerImage")
		}},
		"another plan recorded": {applied(), func(s map[string]any) { set(s, "other", "status", "applied", "planRef", "uid") }},
		"recorded in another epoch": {applied(), func(s map[string]any) {
			set(s, "v1-x", "status", "applied", "executionBindingID")
		}},
		"recorded by the successor": {applied(), func(s map[string]any) {
			set(s, "successor", "status", "applied", "dispatchedBy", "controllerImage")
		}},
		"the epoch moved": {applied(), func(s map[string]any) {
			set(s, "v1-bbbb", "status", "executionBinding", "epoch")
			set(s, "v1-bbbb", "status", "applied", "executionBindingID")
		}},
		"a binding change reported": {pending("ApplySucceeded"), func(s map[string]any) {
			set(s, []any{map[string]any{"reason": "ExecutionBindingChanged"}}, "status", "conditions")
		}},
		"nothing accounted": {predecessorApplyTestDocument(t, `{"status":{"executionBinding":{"epoch":"v1-aaaa"}}}`), func(map[string]any) {}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			test.mutate(test.document)
			if predecessorApplyAccounted(test.document, staged, "apply-job", "job-uid", "plan-uid", dispatcher) {
				t.Fatal("the mistake passed as the Apply accounted for")
			}
		})
	}
}

func TestPredecessorApplyDiagnosticsCarryWhatWasDecided(t *testing.T) {
	t.Parallel()
	schema := predecessorApplyTestDocument(t, `{"status":{"phase":"Applying","activeOperation":{"type":"Apply"},
		"conditions":[{"type":"Ready","status":"False","reason":"Applying","message":"long message"}]}}`)
	line, err := predecessorApplySchemaDiagnostic(schema)
	if err != nil {
		t.Fatal(err)
	}
	if string(line) != `{"activeOperation":{"type":"Apply"},"conditions":[{"reason":"Applying","status":"False","type":"Ready"}],"pendingObservation":null,"phase":"Applying"}` {
		t.Fatalf("schema diagnostic = %s", line)
	}
	jobs, err := predecessorApplyJobsDiagnostic([]map[string]any{predecessorApplyTestDocument(t,
		`{"metadata":{"name":"j","uid":"u"},"status":{"conditions":[{"type":"Complete","status":"True","reason":"x"}]}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if string(jobs) != `[{"conditions":[{"status":"True","type":"Complete"}],"name":"j","uid":"u"}]` {
		t.Fatalf("jobs diagnostic = %s", jobs)
	}
	if empty, err := predecessorApplyJobsDiagnostic(nil); err != nil || string(empty) != `[]` {
		t.Fatalf("no Jobs = %s, %v", empty, err)
	}
}
