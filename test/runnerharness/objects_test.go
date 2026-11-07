package runnerharness_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/coordination"
	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/migrationplan"
	"github.com/stokaro/ptah-operator/internal/planstore"
	"github.com/stokaro/ptah-operator/internal/runner"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
	"github.com/stokaro/ptah-operator/internal/workload"
	"github.com/stokaro/ptah-operator/test/runnerharness"
)

const (
	enginePostgres = "postgres"
	engineMySQL    = "mysql"

	harnessNamespace = "harness"
	bindingEpoch     = "v1-33333333333333333333333333333333"
	databaseSecret   = "harness-database"
	databaseKey      = "url"
	policyConfigMap  = "harness-policy"
	policyKey        = "policy.yaml"
)

func digestOf(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// operationID is a row's operation ID. The frame has to name it back.
func operationID(row string) string { return digestOf("operation", row) }

func engineKind(engine string) operatorv1alpha1.DatabaseEngine {
	if engine == engineMySQL {
		return operatorv1alpha1.DatabaseEngineMySQL
	}
	return operatorv1alpha1.DatabaseEnginePostgreSQL
}

func engineName(engine string) string { return string(engineKind(engine)) }

// builder is the manager's Job builder under this run's configuration.
func (l *lab) builder() workload.Builder {
	return workload.Builder{
		ExecutorImage:          l.images.executor,
		RunnerImage:            "harness.invalid/operator@" + digestOf("runner"),
		PtahVersion:            "harness",
		ControllerImage:        "harness.invalid/manager@" + digestOf("manager"),
		ControllerRevision:     "runner-harness",
		ControllerStateVersion: 1,
		PlanSealPublicKey:      l.seal.PublicKey(),
	}
}

func executionBinding(builder workload.Builder) *operatorv1alpha1.ExecutionBindingStatus {
	return &operatorv1alpha1.ExecutionBindingStatus{
		Epoch:                  bindingEpoch,
		ControllerStateVersion: builder.ControllerStateVersion,
		PtahVersion:            builder.PtahVersion,
		ExecutorImage:          builder.ExecutorImage,
		RunnerProtocolVersion:  int32(runner.ProtocolVersion),
	}
}

func databaseTarget(engine string) operatorv1alpha1.DatabaseTargetSpec {
	return operatorv1alpha1.DatabaseTargetSpec{
		Engine:          engineKind(engine),
		CoordinationKey: "harness/" + engine,
		URLFrom: corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: databaseSecret},
			Key:                  databaseKey,
		},
	}
}

func objectUID(kind, name string) types.UID { return types.UID(kind + "-" + name) }

// gitHTTPS and gitSSH are a Git source over each transport, reading the
// credential and the trust cluster provides.
func (l *lab) gitHTTPS(path string, ref operatorv1alpha1.GitRef) *operatorv1alpha1.GitSourceSpec {
	return &operatorv1alpha1.GitSourceSpec{
		URL: "https://" + gitHost + "/acme/app.git", Ref: ref, Path: path,
		AuthFrom: &operatorv1alpha1.GitAuthSource{Name: "harness-git-token"},
		Transport: operatorv1alpha1.GitTransportSpec{CAFrom: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "harness-git-ca"}, Key: "ca.pem",
		}},
	}
}

func (l *lab) gitSSH(path string, ref operatorv1alpha1.GitRef) *operatorv1alpha1.GitSourceSpec {
	return &operatorv1alpha1.GitSourceSpec{
		URL: "ssh://git@" + gitHost + "/acme/app.git", Ref: ref, Path: path,
		AuthFrom: &operatorv1alpha1.GitAuthSource{Name: "harness-git-deploy-key"},
		Transport: operatorv1alpha1.GitTransportSpec{KnownHostsFrom: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "harness-git-hosts"}, Key: "known_hosts",
		}},
	}
}

// cluster is what a Pod reads from the API server in a real run: the database
// Secret of this row, the verification policy, the Git credentials and trust,
// and the plan's chunks.
func (l *lab) cluster(url string, policy []byte, chunks map[string]map[string][]byte) runnerharness.Cluster {
	cluster := runnerharness.Cluster{
		Secrets: map[string]map[string][]byte{
			databaseSecret: {databaseKey: []byte(url)},
			"harness-git-token": {
				operatorv1alpha1.GitHostSecretKey:     []byte(gitHost),
				operatorv1alpha1.GitPasswordSecretKey: []byte(l.keys.Token),
				operatorv1alpha1.GitCASHA256SecretKey: []byte(digestOfBytes(l.keys.CAPEM)),
			},
			"harness-git-deploy-key": {
				operatorv1alpha1.GitHostSecretKey:             []byte(gitHost),
				operatorv1alpha1.GitSSHPrivateKeySecretKey:    l.keys.ClientKeyPEM,
				operatorv1alpha1.GitKnownHostsSHA256SecretKey: []byte(digestOfBytes(l.keys.KnownHosts)),
			},
		},
		ConfigMaps: map[string]map[string][]byte{
			policyConfigMap:     {policyKey: policy},
			"harness-git-ca":    {"ca.pem": l.keys.CAPEM},
			"harness-git-hosts": {"known_hosts": l.keys.KnownHosts},
		},
	}
	for name, data := range chunks {
		cluster.ConfigMaps[name] = data
	}
	return cluster
}

func digestOfBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// schemaRun is one PtahSchema across its operations: the object the controller
// would hold, kept current the way the controller keeps it, and the Job its
// builder makes for each row.
type schemaRun struct {
	l        *lab
	t        *testing.T
	family   string
	engine   string
	url      string
	policy   []byte
	schema   *operatorv1alpha1.PtahSchema
	sequence int
	// plan and chunks are the current plan, for an Apply.
	current *operatorv1alpha1.PtahSchemaPlan
	chunks  map[string]map[string][]byte
}

func (l *lab) newSchema(t *testing.T, family, engine, url string, desired operatorv1alpha1.SourceSpec) *schemaRun {
	t.Helper()
	schema := &operatorv1alpha1.PtahSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: harnessNamespace, Name: "harness", UID: objectUID("schema", family), Generation: 1},
		Spec: operatorv1alpha1.PtahSchemaSpec{
			Target:  databaseTarget(engine),
			Desired: desired,
			Policy: operatorv1alpha1.ReconciliationPolicy{
				Apply: operatorv1alpha1.ApplyPolicyOnApproval, AllowDestructive: true,
			},
			Execution: operatorv1alpha1.ExecutionSpec{ActiveDeadlineSeconds: 300},
		},
		Status: operatorv1alpha1.PtahSchemaStatus{ExecutionBinding: executionBinding(l.builder())},
	}
	coordinationDigest, err := coordination.Digest(schema.Namespace, schema.Spec.Target)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := runner.TargetIdentityDigest(url)
	if err != nil {
		t.Fatal(err)
	}
	schema.Status.Target.CoordinationDigest = coordinationDigest
	schema.Status.Target.IdentityDigest = identity
	return &schemaRun{l: l, t: t, family: family, engine: engine, url: url, schema: schema}
}

func (s *schemaRun) operation(row string, kind operatorv1alpha1.OperationType) operatorv1alpha1.ActiveOperationStatus {
	return operatorv1alpha1.ActiveOperationStatus{
		Type: kind, ID: operationID(row), InputFingerprint: digestOf("input", row),
		ExecutionBindingID: bindingEpoch, StartedAt: metav1.Now(), Attempt: 1,
	}
}

// access is what an operation reads the desired schema through.
func (s *schemaRun) access() operatorv1alpha1.SourceAccessBinding {
	if git := s.schema.Spec.Desired.Git; git != nil {
		return operatorv1alpha1.SourceAccessBinding{Git: sourcebinding.GitAccess(*git, *s.schema.Status.Source.Git)}
	}
	return operatorv1alpha1.SourceAccessBinding{
		ResolvedReference: s.schema.Status.Source.ResolvedReference,
		Digest:            s.schema.Status.Source.Digest,
		Transport:         s.schema.Spec.Desired.Transport,
	}
}

func (s *schemaRun) source() runnerharness.Source {
	if s.schema.Spec.Desired.Git != nil {
		return runnerharness.SourceGit
	}
	return runnerharness.SourceOCI
}

func (s *schemaRun) row(name string, operation runner.Operation, source runnerharness.Source, job *batchv1.Job) rowSpec {
	return rowSpec{
		name: s.family + "/" + name, operation: operation, source: source, engine: engineName(s.engine),
		job: job, cluster: s.l.cluster(s.url, s.policy, s.chunks),
	}
}

func (s *schemaRun) build(operation operatorv1alpha1.ActiveOperationStatus, plan *operatorv1alpha1.PtahSchemaPlan) *batchv1.Job {
	s.t.Helper()
	job, err := s.l.builder().Build(s.schema, operation, plan)
	if err != nil {
		s.t.Fatalf("build the %s Job: %v", operation.Type, err)
	}
	return job
}

// resolve runs a Resolve and records its result in the source status.
func (s *schemaRun) resolve(name string, adjust ...func(*rowSpec)) rowOutcome {
	s.t.Helper()
	rowName := s.family + "/" + name
	job := s.build(s.operation(rowName, operatorv1alpha1.OperationResolve), nil)
	row := s.row(name, runner.OperationResolve, s.source(), job)
	for _, change := range adjust {
		change(&row)
	}
	outcome := s.l.runRow(s.t, row)
	if outcome.result.Error != nil {
		return outcome
	}
	if git := s.schema.Spec.Desired.Git; git != nil {
		s.schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{Git: &operatorv1alpha1.GitSourceRevision{
			URL: git.URL, Ref: git.Ref, Path: git.Path,
			Commit: outcome.result.ResolvedCommit, ContentDigest: outcome.result.ResolvedContentDigest,
		}}
		return outcome
	}
	s.schema.Status.Source = operatorv1alpha1.SchemaSourceStatus{
		RequestedReference: s.schema.Spec.Desired.OCIRef,
		ResolvedReference:  outcome.result.ResolvedReference,
		Digest:             outcome.result.ResolvedDigest,
	}
	return outcome
}

// verify runs a Verify and records its result in the source status.
func (s *schemaRun) verify(name string, policy []byte, adjust ...func(*rowSpec)) rowOutcome {
	s.t.Helper()
	rowName := s.family + "/" + name
	s.policy = policy
	operation := s.operation(rowName, operatorv1alpha1.OperationVerify)
	operation.VerificationPolicyUID = "harness-policy"
	operation.VerificationPolicyDigest = digestOfBytes(policy)
	job := s.build(operation, nil)
	row := s.row(name, runner.OperationVerify, runnerharness.SourceOCI, job)
	for _, change := range adjust {
		change(&row)
	}
	outcome := s.l.runRow(s.t, row)
	if outcome.result.Error != nil {
		return outcome
	}
	s.schema.Status.Source.Verified = true
	s.schema.Status.Source.ArtifactType = dataplane.SchemaArtifactType
	s.schema.Status.Source.VerificationPolicyUID = "harness-policy"
	s.schema.Status.Source.VerificationPolicyDigest = digestOfBytes(policy)
	return outcome
}

func (s *schemaRun) reads(row string, kind operatorv1alpha1.OperationType) operatorv1alpha1.ActiveOperationStatus {
	operation := s.operation(row, kind)
	access := s.access()
	operation.Source = &access
	operation.Target = &operatorv1alpha1.DatabaseTargetBinding{Engine: s.schema.Spec.Target.Engine, URLFrom: s.schema.Spec.Target.URLFrom}
	operation.CoordinationDigest = s.schema.Status.Target.CoordinationDigest
	operation.TargetIdentityDigest = s.schema.Status.Target.IdentityDigest
	operation.ObservationSeverity = "all"
	operation.ObservationConnectTimeout = metav1.Duration{Duration: 10 * time.Second}
	operation.ObservationLockTimeout = metav1.Duration{Duration: 30 * time.Second}
	return operation
}

func (s *schemaRun) observe(name string, adjust ...func(*rowSpec)) rowOutcome {
	s.t.Helper()
	job := s.build(s.reads(s.family+"/"+name, operatorv1alpha1.OperationObserve), nil)
	row := s.row(name, runner.OperationObserve, s.source(), job)
	for _, change := range adjust {
		change(&row)
	}
	return s.l.runRow(s.t, row)
}

// plan runs a Plan. fence names tables the plan may not change.
func (s *schemaRun) plan(name string, fence []string, adjust ...func(*rowSpec)) rowOutcome {
	s.t.Helper()
	rowName := s.family + "/" + name
	operation := s.reads(rowName, operatorv1alpha1.OperationPlan)
	operation.LeaseDurationSeconds = 660
	operation.ObservationProtectedTables = fence
	jobName, err := workload.NameFor(s.schema, operation)
	if err != nil {
		s.t.Fatal(err)
	}
	operation.JobName = jobName
	job := s.build(operation, nil)
	row := s.row(name, runner.OperationPlan, s.source(), job)
	for _, change := range adjust {
		change(&row)
	}
	return s.l.runRow(s.t, row)
}

// publishPlan stores plan bytes the way the plan store does, and makes them the
// schema's current plan, approved.
func (s *schemaRun) publishPlan(name string, content []byte) {
	s.t.Helper()
	decoded, err := dataplane.DecodePlan(content, engineName(s.engine))
	if err != nil {
		s.t.Fatalf("the plan does not decode: %v", err)
	}
	builder := s.l.builder()
	artifact, source := sourcebinding.SchemaPlanSource(s.schema.Status.Source)
	contract := int32(fingerprint.CurrentPlanContractVersion)
	if source != nil {
		contract = fingerprint.SourcePlanContractVersion
	}
	spec := operatorv1alpha1.PtahSchemaPlanSpec{
		ContractVersion:          contract,
		SchemaRef:                operatorv1alpha1.ImmutableObjectReference{Name: s.schema.Name, UID: s.schema.UID},
		Fingerprint:              digestOf("plan", s.family, name),
		ArtifactDigest:           artifact,
		Source:                   source,
		CoordinationDigest:       s.schema.Status.Target.CoordinationDigest,
		TargetIdentityDigest:     s.schema.Status.Target.IdentityDigest,
		ActualStateFingerprint:   digestOf("actual"),
		DesiredStateFingerprint:  digestOf("desired"),
		PolicyFingerprint:        digestOf("policy"),
		VerificationPolicyUID:    s.schema.Status.Source.VerificationPolicyUID,
		VerificationPolicyDigest: s.schema.Status.Source.VerificationPolicyDigest,
		ExecutionBindingID:       bindingEpoch,
		ControllerImage:          builder.ControllerImage,
		ControllerRevision:       builder.ControllerRevision,
		ControllerStateVersion:   builder.ControllerStateVersion,
		PtahVersion:              builder.PtahVersion,
		ExecutorImage:            builder.ExecutorImage,
		RunnerImage:              builder.RunnerImage,
		RunnerProtocolVersion:    int32(runner.ProtocolVersion),
		Dialect:                  engineName(s.engine),
		StatementCount:           int32(len(decoded.Statements)),
		Destructive:              decoded.Destructive,
	}
	plan, chunks, err := planstore.Prepare(s.schema, spec, content)
	if err != nil {
		s.t.Fatalf("store the plan: %v", err)
	}
	plan.UID = objectUID("plan", name)
	plan.Generation = 1
	plan.Status.ObservedGeneration = 1
	plan.Status.Conditions = []metav1.Condition{{
		Type: operatorv1alpha1.ConditionPlanStorageReady, Status: metav1.ConditionTrue, ObservedGeneration: 1, Reason: "Published",
	}}
	s.chunks = map[string]map[string][]byte{}
	for index, reference := range plan.Spec.Chunks {
		plan.Status.PublishedChunks = append(plan.Status.PublishedChunks, operatorv1alpha1.PublishedPlanChunkStatus{
			Name: reference.Name, UID: types.UID("chunk-" + strconv.Itoa(index)), Index: reference.Index,
		})
		projection := planstore.DesiredProjection(plan, reference, chunks[index])
		s.chunks[reference.Name] = projection.BinaryData
	}
	s.current = plan
	s.schema.Status.Plan = &operatorv1alpha1.CurrentPlanStatus{
		Name: plan.Name, UID: plan.UID,
		Fingerprint: plan.Spec.Fingerprint, ContentDigest: plan.Spec.ContentDigest,
		ArtifactDigest: plan.Spec.ArtifactDigest, Source: plan.Spec.Source,
		CoordinationDigest: plan.Spec.CoordinationDigest, TargetIdentityDigest: plan.Spec.TargetIdentityDigest,
		ActualStateFingerprint: plan.Spec.ActualStateFingerprint, DesiredStateFingerprint: plan.Spec.DesiredStateFingerprint,
		PolicyFingerprint:     plan.Spec.PolicyFingerprint,
		VerificationPolicyUID: plan.Spec.VerificationPolicyUID, VerificationPolicyDigest: plan.Spec.VerificationPolicyDigest,
		ExecutionBindingID: plan.Spec.ExecutionBindingID,
		ControllerImage:    plan.Spec.ControllerImage, ControllerRevision: plan.Spec.ControllerRevision,
		ControllerStateVersion: plan.Spec.ControllerStateVersion,
		PtahVersion:            plan.Spec.PtahVersion, ExecutorImage: plan.Spec.ExecutorImage,
		RunnerImage: plan.Spec.RunnerImage, RunnerProtocolVersion: plan.Spec.RunnerProtocolVersion,
		Destructive: plan.Spec.Destructive, StatementCount: plan.Spec.StatementCount,
		Approval: &operatorv1alpha1.ConsumedApprovalStatus{Name: "harness-approval", UID: "harness-approval"},
	}
}

// apply runs an Apply of the current plan.
func (s *schemaRun) apply(name string, adjust ...func(*rowSpec)) rowOutcome {
	s.t.Helper()
	rowName := s.family + "/" + name
	operation := s.operation(rowName, operatorv1alpha1.OperationApply)
	operation.CoordinationDigest = s.schema.Status.Target.CoordinationDigest
	operation.TargetIdentityDigest = s.schema.Status.Target.IdentityDigest
	operation.Target = &operatorv1alpha1.DatabaseTargetBinding{Engine: s.schema.Spec.Target.Engine, URLFrom: s.schema.Spec.Target.URLFrom}
	operation.LeaseDurationSeconds = 660
	dispatchNotAfter := metav1.NewTime(operation.StartedAt.Add(300 * time.Second))
	operation.DispatchNotAfter = &dispatchNotAfter
	operation.ExecutionNotAfter = dispatchNotAfter.DeepCopy()
	operation.TerminationGracePeriodSeconds = 30
	job := s.build(operation, s.current)
	row := s.row(name, runner.OperationApply, runnerharness.SourceNone, job)
	for _, change := range adjust {
		change(&row)
	}
	return s.l.runRow(s.t, row)
}

// migrationRun is one PtahMigration across its operations.
type migrationRun struct {
	l         *lab
	t         *testing.T
	family    string
	engine    string
	url       string
	policy    []byte
	migration *operatorv1alpha1.PtahMigration
	targetID  string
	realm     string
}

func (l *lab) newMigration(t *testing.T, family, engine, url string, artifact operatorv1alpha1.SourceSpec) *migrationRun {
	t.Helper()
	migration := &operatorv1alpha1.PtahMigration{
		ObjectMeta: metav1.ObjectMeta{Namespace: harnessNamespace, Name: "harness", UID: objectUID("migration", family), Generation: 1},
		Spec: operatorv1alpha1.PtahMigrationSpec{
			Target:    databaseTarget(engine),
			Artifact:  artifact,
			Policy:    operatorv1alpha1.MigrationPolicy{Apply: operatorv1alpha1.ApplyPolicyOnApproval, LockTimeout: metav1.Duration{Duration: 30 * time.Second}},
			Execution: operatorv1alpha1.ExecutionSpec{ActiveDeadlineSeconds: 300, ConnectTimeout: metav1.Duration{Duration: 10 * time.Second}},
		},
		Status: operatorv1alpha1.PtahMigrationStatus{ExecutionBinding: executionBinding(l.builder())},
	}
	realm, err := coordination.Digest(migration.Namespace, migration.Spec.Target)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := runner.TargetIdentityDigest(url)
	if err != nil {
		t.Fatal(err)
	}
	return &migrationRun{l: l, t: t, family: family, engine: engine, url: url, migration: migration, targetID: identity, realm: realm}
}

func (m *migrationRun) operation(row string, kind operatorv1alpha1.MigrationOperationType) operatorv1alpha1.MigrationOperationStatus {
	return operatorv1alpha1.MigrationOperationStatus{
		Type: kind, ID: operationID(row), InputFingerprint: digestOf("input", row),
		ExecutionBindingID: bindingEpoch, StartedAt: metav1.Now(), Attempt: 1,
	}
}

func (m *migrationRun) source() runnerharness.Source {
	if m.migration.Spec.Artifact.Git != nil {
		return runnerharness.SourceGit
	}
	return runnerharness.SourceOCI
}

func (m *migrationRun) row(name string, operation runner.Operation, job *batchv1.Job) rowSpec {
	return rowSpec{
		name: m.family + "/" + name, operation: operation, source: m.source(), engine: engineName(m.engine),
		job: job, cluster: m.l.cluster(m.url, m.policy, nil),
	}
}

func (m *migrationRun) build(operation operatorv1alpha1.MigrationOperationStatus, plan *operatorv1alpha1.PtahMigrationPlan) *batchv1.Job {
	m.t.Helper()
	job, err := m.l.builder().BuildMigration(m.migration, operation, plan)
	if err != nil {
		m.t.Fatalf("build the %s Job: %v", operation.Type, err)
	}
	return job
}

func (m *migrationRun) resolve(name string) rowOutcome {
	m.t.Helper()
	job := m.build(m.operation(m.family+"/"+name, operatorv1alpha1.MigrationOperationResolve), nil)
	outcome := m.l.runRow(m.t, m.row(name, runner.OperationResolve, job))
	if git := m.migration.Spec.Artifact.Git; git != nil {
		m.migration.Status.Artifact = &operatorv1alpha1.SourceAccessBinding{Git: sourcebinding.GitAccess(*git, operatorv1alpha1.GitSourceRevision{
			URL: git.URL, Ref: git.Ref, Path: git.Path,
			Commit: outcome.result.ResolvedCommit, ContentDigest: outcome.result.ResolvedContentDigest,
		})}
		return outcome
	}
	m.migration.Status.Artifact = &operatorv1alpha1.SourceAccessBinding{
		ResolvedReference: outcome.result.ResolvedReference,
		Digest:            outcome.result.ResolvedDigest,
		Transport:         m.migration.Spec.Artifact.Transport,
	}
	return outcome
}

func (m *migrationRun) verify(name string, policy []byte, adjust ...func(*rowSpec)) rowOutcome {
	m.t.Helper()
	m.policy = policy
	operation := m.operation(m.family+"/"+name, operatorv1alpha1.MigrationOperationVerify)
	operation.Source = m.migration.Status.Artifact.DeepCopy()
	job := m.build(operation, nil)
	row := m.row(name, runner.OperationVerify, job)
	for _, change := range adjust {
		change(&row)
	}
	return m.l.runRow(m.t, row)
}

func (m *migrationRun) reads(row string, kind operatorv1alpha1.MigrationOperationType) operatorv1alpha1.MigrationOperationStatus {
	operation := m.operation(row, kind)
	operation.Source = m.migration.Status.Artifact.DeepCopy()
	operation.Target = &operatorv1alpha1.DatabaseTargetBinding{Engine: m.migration.Spec.Target.Engine, URLFrom: m.migration.Spec.Target.URLFrom}
	operation.CoordinationDigest = m.realm
	return operation
}

func (m *migrationRun) history(name string, adjust ...func(*rowSpec)) rowOutcome {
	m.t.Helper()
	job := m.build(m.reads(m.family+"/"+name, operatorv1alpha1.MigrationOperationHistory), nil)
	row := m.row(name, runner.OperationMigrationHistory, job)
	for _, change := range adjust {
		change(&row)
	}
	return m.l.runRow(m.t, row)
}

// applyPlanned runs the migrations a history reading selected.
func (m *migrationRun) applyPlanned(name string, planned []operatorv1alpha1.PlannedMigration, adjust ...func(*rowSpec)) rowOutcome {
	m.t.Helper()
	rowName := m.family + "/" + name
	operation := m.reads(rowName, operatorv1alpha1.MigrationOperationApply)
	plan := &operatorv1alpha1.PtahMigrationPlan{
		ObjectMeta: metav1.ObjectMeta{Namespace: harnessNamespace, Name: "harness-plan", UID: objectUID("migration-plan", rowName)},
		Spec: operatorv1alpha1.PtahMigrationPlanSpec{
			ContractVersion:      1,
			MigrationRef:         operatorv1alpha1.ImmutableObjectReference{Name: m.migration.Name, UID: m.migration.UID},
			Fingerprint:          digestOf("plan", rowName),
			HistoryFingerprint:   digestOf("history", rowName),
			CoordinationDigest:   m.realm,
			TargetIdentityDigest: m.targetID,
			Migrations:           planned,
		},
	}
	operation.PlanRef = &operatorv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID}
	dispatchNotAfter := metav1.NewTime(operation.StartedAt.Add(300 * time.Second))
	operation.DispatchNotAfter = &dispatchNotAfter
	operation.ExecutionNotAfter = dispatchNotAfter.DeepCopy()
	job := m.build(operation, plan)
	row := m.row(name, runner.OperationMigrationApply, job)
	for _, change := range adjust {
		change(&row)
	}
	return m.l.runRow(m.t, row)
}

// pending is what a plan would select from a history reading.
func pending(t *testing.T, row string, outcome rowOutcome) []operatorv1alpha1.PlannedMigration {
	t.Helper()
	if outcome.result.MigrationHistory == nil {
		t.Fatalf("%s: the frame carries no history", row)
	}
	planned, err := migrationplan.Sequence(*outcome.result.MigrationHistory)
	if err != nil {
		t.Fatalf("%s: %v", row, err)
	}
	return planned
}
