package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/schemaview"
)

// rdRoot is the repository root from this package's directory, which is
// where go test runs a test.
const rdRoot = "../.."

// rdRenderedLine renders a view and returns its first line that starts with
// prefix.
func rdRenderedLine(t *testing.T, view schemaview.View, prefix string) string {
	t.Helper()
	var rendered bytes.Buffer
	if err := schemaview.Render(&rendered, view, schemaview.Text); err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(rendered.String(), "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("the view wrote no %q line:\n%s", prefix, rendered.String())
	return ""
}

// The reference-data phase compares whole lines of `kubectl ptah schema`
// output against literals, and a cluster run is the worst place to find they
// disagree with the renderer: ninety minutes into a lifecycle, about a space.
// So the renderer is asked here, offline.
func TestReferenceDataPhasePinsTheLineTheViewWrites(t *testing.T) {
	t.Parallel()
	// The same drift the phase makes by hand: one managed row edited outside
	// the operator, nothing added and nothing removed.
	view := schemaview.View{
		Namespace: "namespace",
		Schema:    "schema",
		Observation: &schemaview.ObservationView{
			Drift:           true,
			HighestSeverity: "destructive",
			FindingCount:    1,
			Findings: []schemaview.FindingView{
				{Category: "data_rows_updated", Count: 1, Severity: "destructive"},
			},
			ReferenceData: &schemaview.ReferenceDataView{Inserts: 0, Updates: 1, Deletes: 0},
		},
	}
	if written := rdRenderedLine(t, view, "Reference data:"); written != referenceExternalEditLine {
		t.Fatalf("the reference-data phase does not pin the line the view writes.\nview writes: %q\nphase wants: %q",
			written, referenceExternalEditLine)
	}
	if written, want := rdRenderedLine(t, view, "Schema:"), referenceSchemaViewLine("namespace", "schema"); written != want {
		t.Fatalf("the reference-data phase does not pin the schema line the view writes.\nview writes: %q\nphase wants: %q",
			written, want)
	}
}

func TestReferenceViewHasLine(t *testing.T) {
	t.Parallel()
	view := []byte("Schema:           ns/s\nReference data:   0 to insert, 1 to update, 0 to delete\n  data_rows_updated 1  destructive\n")
	if !referenceViewHasLine(view, referenceExternalEditLine) {
		t.Fatal("the view's own line was not found")
	}
	if !referenceViewHasLine(view, referenceSchemaViewLine("ns", "s")) {
		t.Fatal("the schema line was not found")
	}
	// grep -Fx: a line that only contains the wanted one, or is padded
	// differently, is not it.
	for _, refused := range []string{
		"Reference data:  0 to insert, 1 to update, 0 to delete",
		"Reference data:   0 to insert, 1 to update",
		"0 to insert, 1 to update, 0 to delete",
		referenceSchemaViewLine("ns", "s-other"),
		referenceSchemaViewLine("n", "s"),
		"",
	} {
		if referenceViewHasLine(view, refused) {
			t.Errorf("the view was read as printing %q", refused)
		}
	}
	if referenceViewHasLine([]byte(referenceExternalEditLine+" and more\n"), referenceExternalEditLine) {
		t.Error("a longer line was read as the wanted one")
	}
}

func TestReferenceNamesFor(t *testing.T) {
	t.Parallel()
	names := referenceNamesFor("postgresql", "e2e-registry.ns.svc.cluster.local:5000", "schemas-r2")
	want := referenceNames{
		secret: "e2e-postgresql-reference-db", schema: "e2e-reference-postgresql",
		approval: "e2e-reference-postgresql-approval", coordinationKey: "e2e/reference/postgresql",
		artifact:        "oci://e2e-registry.ns.svc.cluster.local:5000/schemas-r2/reference-postgresql:stable",
		configMapPrefix: "e2e-reference-postgresql-", jobPrefix: "e2e-push-reference-postgresql-",
	}
	if names != want {
		t.Fatalf("referenceNamesFor = %+v, want %+v", names, want)
	}
}

func TestReferenceLeftover(t *testing.T) {
	t.Parallel()
	names := referenceNamesFor("postgresql", "registry:5000", "schemas")
	for _, taken := range []struct{ kind, name string }{
		{"PtahSchemaApproval", "e2e-reference-postgresql-approval"},
		{"PtahSchemaApproval", "e2e-reference-postgresql-approval-v2"},
		{"ConfigMap", "e2e-reference-postgresql-v1"},
		{"Job", "e2e-push-reference-postgresql-v5"},
	} {
		if !referenceLeftover(names, taken.kind, taken.name) {
			t.Errorf("%s %s was not taken as this engine's leftover", taken.kind, taken.name)
		}
	}
	// The other engine's objects, the data plane's beside them, and the
	// verification policy, which stays.
	for _, kept := range []struct{ kind, name string }{
		{"PtahSchemaApproval", "e2e-reference-mysql-approval"},
		{"PtahSchemaApproval", "e2e-approval"},
		{"ConfigMap", "e2e-reference-mysql-v1"},
		{"ConfigMap", referencePolicy},
		{"ConfigMap", "e2e-postgresql-v1"},
		{"Job", "e2e-push-reference-mysql-v1"},
		{"Job", "e2e-push-postgresql-v1"},
		{"PtahSchema", "e2e-reference-postgresql"},
		{"Secret", "e2e-reference-postgresql-v1"},
	} {
		if referenceLeftover(names, kept.kind, kept.name) {
			t.Errorf("%s %s was taken as this engine's leftover", kept.kind, kept.name)
		}
	}
}

func TestReferenceRepository(t *testing.T) {
	t.Parallel()
	for rerun, want := range map[string]string{"": "schemas", "r2": "schemas-r2", "r10": "schemas-r10"} {
		if got, err := referenceRepository(rerun); err != nil || got != want {
			t.Errorf("referenceRepository(%q) = %q, %v; want %q", rerun, got, err, want)
		}
	}
	for _, rerun := range []string{"r", "2", "R2", "r2a", "r-2", " r2"} {
		if _, err := referenceRepository(rerun); err == nil || !strings.Contains(err.Error(), "E2E_PHASE_RERUN must be r followed by digits") {
			t.Errorf("referenceRepository(%q) = %v, want the rerun refusal", rerun, err)
		}
	}
}

func TestReferenceInputsOK(t *testing.T) {
	t.Parallel()
	pinned := "registry/ptah@sha256:" + strings.Repeat("a", 64)
	if err := referenceInputsOK("mysql", "mysql", pinned, pinned); err != nil {
		t.Fatalf("pinned images and the phase's engine were refused: %v", err)
	}
	for name, test := range map[string]struct {
		engine, phase, image, refusal string
	}{
		"tag":             {"mysql", "mysql", "registry/ptah:v1", "must be pinned by a lowercase SHA-256 digest"},
		"uppercase":       {"mysql", "mysql", "registry/ptah@sha256:" + strings.Repeat("A", 64), "must be pinned"},
		"short digest":    {"mysql", "mysql", "registry/ptah@sha256:abc", "must be pinned"},
		"no engine":       {"", "mysql", pinned, "E2E_ENGINE must name postgresql or mysql"},
		"unknown engine":  {"sqlite", "mysql", pinned, "E2E_ENGINE must name postgresql or mysql"},
		"the other phase": {"postgresql", "mysql", pinned, "this phase runs mysql"},
	} {
		if err := referenceInputsOK(test.engine, test.phase, pinned, test.image); err == nil || !strings.Contains(err.Error(), test.refusal) {
			t.Errorf("%s: referenceInputsOK = %v, want %q", name, err, test.refusal)
		}
	}
}

// The proof needs a database no other phase touched, so the tables it
// declares really do not exist when it starts.
func TestReferenceDatabaseIsItsOwn(t *testing.T) {
	t.Parallel()
	for _, other := range []string{pgDatabase, mysqlDatabase, customCAPGDatabase, fourEyesPGDatabase, podMetadataPGDatabase, "ptah_e2e_migrations"} {
		if referenceDatabase == other {
			t.Fatalf("the reference-data proof shares database %s with another phase", other)
		}
	}
}

func TestReferenceDeclaredRowValues(t *testing.T) {
	t.Parallel()
	values, err := referenceDeclaredRowValues(rdRoot)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Americas", "Canada", "Czech Republic", "Czechia", "Europe, Middle East and Africa", "United States"}
	if !slices.Equal(values, want) {
		t.Fatalf("declared row values = %q, want %q", values, want)
	}
	for _, value := range values {
		if strings.TrimSpace(value) != value || value == "" {
			t.Errorf("value %q carries whitespace or is empty", value)
		}
	}
}

func TestReferenceDeclaredRowValuesReadsWhatSedRead(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(path, content string) {
		t.Helper()
		full := filepath.Join(root, "testdata", "e2e", "reference", path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("v1/rows.yaml", "- code: a\n  name: Alpha   \n- code: b\n\tname:\tBeta\n  region_name: Refused\n- name: Dashed\n  name:\n")
	write("v2/more.yaml", "- code: c\n  name: Gamma\n  name: Alpha\n")
	// Not a lower-case YAML file, so not a row fixture.
	write("v2/Upper.yaml", "  name: Upper\n")
	write("v2/rows.yml", "  name: Yml\n")
	write("v2/entities.go", "  name: Go\n")
	write("v2/nested/deep.yaml", "  name: Nested\n")
	if _, err := referenceDeclaredRowValues(root); err == nil || !strings.Contains(err.Error(), "too few values") {
		t.Fatalf("three values passed as enough for the row scanner: %v", err)
	}
	write("v3/rows.yaml", "  name: Delta\n  name: Epsilon\n")
	values, err := referenceDeclaredRowValues(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Alpha", "Beta", "Delta", "Epsilon", "Gamma"}; !slices.Equal(values, want) {
		t.Fatalf("declared row values = %q, want %q", values, want)
	}
}

func TestReferenceRowScanner(t *testing.T) {
	t.Parallel()
	if _, err := newReferenceRowScanner(); err == nil || !strings.Contains(err.Error(), "no declared values") {
		t.Fatalf("a scanner with nothing to look for was built: %v", err)
	}
	if _, err := newReferenceRowScanner("Canada", ""); err == nil || !strings.Contains(err.Error(), "empty declared value") {
		t.Fatalf("a scanner with an empty value was built: %v", err)
	}
	if (referenceRowScanner{}).ready() {
		t.Fatal("a scanner never built reads as ready")
	}
	scanner, err := newReferenceRowScanner("Canada", "Czech Republic")
	if err != nil || !scanner.ready() {
		t.Fatalf("newReferenceRowScanner = %v, ready %v", err, scanner.ready())
	}
	content := []byte("phase: InSync\nmessage: row Canada differs\nname: Czech\nRepublic\n  also Czech Republic here\n")
	if got, want := scanner.matches(content), []string{"message: row Canada differs", "  also Czech Republic here"}; !slices.Equal(got, want) {
		t.Fatalf("matches = %q, want %q", got, want)
	}
	// A value split across lines is not on any one line, as grep read it.
	if got := scanner.matches([]byte("Czech\nRepublic\nCanad a\n")); len(got) != 0 {
		t.Fatalf("matches found %q in content without a declared value", got)
	}
	if got := scanner.matches(nil); len(got) != 0 {
		t.Fatalf("matches found %q in nothing", got)
	}
}

func rdCondition(kind string, status metav1.ConditionStatus, reason string) metav1.Condition {
	return metav1.Condition{Type: kind, Status: status, Reason: reason}
}

func rdRefused() *ptahv1alpha1.PtahSchema {
	schema := &ptahv1alpha1.PtahSchema{}
	schema.Status.Phase = ptahv1alpha1.PhaseBlocked
	schema.Status.Conditions = []metav1.Condition{
		rdCondition("Ready", metav1.ConditionFalse, "ProtectedTable"),
		rdCondition("Supported", metav1.ConditionTrue, "Supported"),
		rdCondition("PlanReady", metav1.ConditionFalse, "ProtectedTable"),
		rdCondition("InSync", metav1.ConditionFalse, "ProtectedTable"),
	}
	return schema
}

func TestReferenceRefusalComplete(t *testing.T) {
	t.Parallel()
	if !referenceRefusalComplete(rdRefused()) {
		t.Fatal("the whole refusal was not recognized")
	}
	for name, mutate := range map[string]func(*ptahv1alpha1.PtahSchema){
		"a plan published": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Plan = &ptahv1alpha1.CurrentPlanStatus{}
		},
		"not Blocked": func(s *ptahv1alpha1.PtahSchema) { s.Status.Phase = ptahv1alpha1.PhaseObserving },
		"no phase":    func(s *ptahv1alpha1.PtahSchema) { s.Status.Phase = "" },
		// The reading measured in run 35299958793: PlanReady still named the
		// fence and Ready already said an Observe was in progress.
		"Ready from the pass after": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Conditions[0] = rdCondition("Ready", metav1.ConditionFalse, "Observing")
		},
		"PlanReady true": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Conditions[2] = rdCondition("PlanReady", metav1.ConditionTrue, "ProtectedTable")
		},
		"InSync missing": func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions = s.Status.Conditions[:3] },
		"a fourth refusal": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Conditions = append(s.Status.Conditions, rdCondition("Stalled", metav1.ConditionFalse, "ProtectedTable"))
		},
		"Ready twice": func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Conditions = append(s.Status.Conditions, rdCondition("Ready", metav1.ConditionFalse, "ProtectedTable"))
		},
		"no conditions": func(s *ptahv1alpha1.PtahSchema) { s.Status.Conditions = nil },
	} {
		schema := rdRefused()
		mutate(schema)
		if referenceRefusalComplete(schema) {
			t.Errorf("%s: the reading passed as a complete refusal", name)
		}
	}
}

func TestReferenceExactlyOneCondition(t *testing.T) {
	t.Parallel()
	conditions := []metav1.Condition{
		rdCondition("PlanReady", metav1.ConditionTrue, "Published"),
		rdCondition("Ready", metav1.ConditionFalse, "AwaitingApproval"),
	}
	if !referenceExactlyOneCondition(conditions, "PlanReady", metav1.ConditionTrue, "Published") {
		t.Fatal("the one condition was not found")
	}
	for name, test := range map[string]struct {
		conditions []metav1.Condition
		kind       string
		status     metav1.ConditionStatus
		reason     string
	}{
		"another status": {conditions, "PlanReady", metav1.ConditionFalse, "Published"},
		"another reason": {conditions, "PlanReady", metav1.ConditionTrue, "ProtectedTable"},
		"another type":   {conditions, "InSync", metav1.ConditionTrue, "Published"},
		"none at all":    {nil, "PlanReady", metav1.ConditionTrue, "Published"},
		"two of them": {append(slices.Clone(conditions), rdCondition("PlanReady", metav1.ConditionTrue, "Published")),
			"PlanReady", metav1.ConditionTrue, "Published"},
	} {
		if referenceExactlyOneCondition(test.conditions, test.kind, test.status, test.reason) {
			t.Errorf("%s: exactly one matching condition was reported", name)
		}
	}
}

func TestReferenceRepeatAllowed(t *testing.T) {
	t.Parallel()
	for _, phase := range []ptahv1alpha1.ReconciliationPhase{
		ptahv1alpha1.PhaseInSync, ptahv1alpha1.PhaseObserving, ptahv1alpha1.PhasePlanning,
		ptahv1alpha1.PhaseVerifyingConvergence, ptahv1alpha1.PhaseResolving, ptahv1alpha1.PhaseVerifying,
		ptahv1alpha1.PhasePending,
	} {
		if !referenceRepeatAllowed(phase) {
			t.Errorf("%s was refused in the converged cycle", phase)
		}
	}
	// The phases that say the operator decided there was work, and the ones
	// that say something else went wrong.
	for _, phase := range []ptahv1alpha1.ReconciliationPhase{
		ptahv1alpha1.PhaseReadyToApply, ptahv1alpha1.PhaseAwaitingApproval, ptahv1alpha1.PhaseApplying,
		ptahv1alpha1.PhaseBlocked, ptahv1alpha1.PhaseFailed, ptahv1alpha1.PhaseSuspended, "",
	} {
		if referenceRepeatAllowed(phase) {
			t.Errorf("%s was allowed in the converged cycle", phase)
		}
	}
}

func TestReferenceRemovalSettled(t *testing.T) {
	t.Parallel()
	for _, phase := range []ptahv1alpha1.ReconciliationPhase{ptahv1alpha1.PhaseInSync, ptahv1alpha1.PhaseAwaitingApproval} {
		if !referenceRemovalSettled(phase) {
			t.Errorf("%s was not a settled phase", phase)
		}
	}
	for _, phase := range []ptahv1alpha1.ReconciliationPhase{ptahv1alpha1.PhaseObserving, ptahv1alpha1.PhaseBlocked, ptahv1alpha1.PhaseFailed, ""} {
		if referenceRemovalSettled(phase) {
			t.Errorf("%s was a settled phase", phase)
		}
	}
}

func TestReferenceRemovalReconciled(t *testing.T) {
	t.Parallel()
	const published = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	settled := func() *ptahv1alpha1.PtahSchema {
		schema := &ptahv1alpha1.PtahSchema{}
		schema.Status.Phase = ptahv1alpha1.PhaseInSync
		schema.Status.Source.Digest = published
		return schema
	}
	if !referenceRemovalReconciled(settled(), published) {
		t.Fatal("a schema converged on the published digest was not recognized")
	}
	awaiting := settled()
	awaiting.Status.Phase = ptahv1alpha1.PhaseAwaitingApproval
	if !referenceRemovalReconciled(awaiting, published) {
		t.Fatal("a schema holding a plan for the published digest was not recognized")
	}
	for _, test := range []struct {
		name   string
		mutate func(*ptahv1alpha1.PtahSchema)
		digest string
	}{
		// The reading that made the script's row vacuous: still InSync on the
		// revision before the publish.
		{"converged on the revision before", func(s *ptahv1alpha1.PtahSchema) {
			s.Status.Source.Digest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
		}, published},
		{"not resolved at all", func(s *ptahv1alpha1.PtahSchema) { s.Status.Source.Digest = "" }, published},
		{"resolved but still working", func(s *ptahv1alpha1.PtahSchema) { s.Status.Phase = ptahv1alpha1.PhaseObserving }, published},
		{"no published digest to wait for", func(s *ptahv1alpha1.PtahSchema) { s.Status.Source.Digest = "" }, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			schema := settled()
			test.mutate(schema)
			if referenceRemovalReconciled(schema, test.digest) {
				t.Fatal("the reading passed as the withdrawn declaration settled")
			}
		})
	}
}

func TestReferencePlanPredicates(t *testing.T) {
	t.Parallel()
	absent := &ptahv1alpha1.PtahSchema{}
	if referencePlanName(absent) != "" || referencePlanHasStatements(absent) || referencePlanDestructive(absent) {
		t.Fatal("a schema with no plan was read as having one")
	}
	planned := &ptahv1alpha1.PtahSchema{}
	planned.Status.Plan = &ptahv1alpha1.CurrentPlanStatus{Name: "plan-a"}
	if referencePlanName(planned) != "plan-a" {
		t.Fatalf("referencePlanName = %q", referencePlanName(planned))
	}
	if referencePlanHasStatements(planned) {
		t.Fatal("a plan with no statements was read as having some")
	}
	if referencePlanDestructive(planned) {
		t.Fatal("a plan not marked destructive was read as destructive")
	}
	planned.Status.Plan.StatementCount = 1
	planned.Status.Plan.Destructive = true
	if !referencePlanHasStatements(planned) || !referencePlanDestructive(planned) {
		t.Fatal("a destructive plan with a statement was not recognized")
	}
}

func TestReferenceRefusalNamesWhatItRefused(t *testing.T) {
	t.Parallel()
	for _, message := range []string{
		`admission webhook "approval.ptah.run" denied the request: spec.planRef names a plan that is no longer current`,
		"the SCHEMA is not awaiting approval",
		"Approval refused: an operation is in flight",
	} {
		if !referenceRefusalNamesWhatItRefused(message) {
			t.Errorf("%q was read as saying nothing about what it refused", message)
		}
	}
	for _, message := range []string{"connection refused", "context deadline exceeded", ""} {
		if referenceRefusalNamesWhatItRefused(message) {
			t.Errorf("%q was read as naming what it refused", message)
		}
	}
}

func TestReferencePlanInTheClear(t *testing.T) {
	t.Parallel()
	if !referencePlanInTheClear([]byte(`{"frame":{"plan":{"format_version":2,"statements":[]}}}`)) {
		t.Fatal("a plaintext plan was not recognized")
	}
	for _, log := range []string{`{"sealed":"bG9vayBubyBrZXkgaGVyZQ=="}`, "format_version", ""} {
		if referencePlanInTheClear([]byte(log)) {
			t.Errorf("%q was read as a plan in the clear", log)
		}
	}
}

func TestReferenceRowsMismatch(t *testing.T) {
	t.Parallel()
	// The query folds away the space, and the caller writes the name it
	// declared.
	if err := referenceRowsMismatch("2", "3", "CzechRepublic", "2", "3", "Czech Republic"); err != nil {
		t.Fatalf("the declared rows were refused: %v", err)
	}
	if err := referenceRowsMismatch("2", "0", "", "2", "0", ""); err != nil {
		t.Fatalf("an undeclared child table was refused: %v", err)
	}
	for name, test := range map[string]struct {
		regions, countries, czechia, refusal string
	}{
		"regions":   {"1", "3", "CzechRepublic", "regions holds 1 rows, want 2"},
		"countries": {"2", "", "CzechRepublic", "countries holds  rows, want 3"},
		"czechia":   {"2", "3", "Czechia", "countries.CZ is Czechia, want CzechRepublic"},
		"no czech":  {"2", "3", "", "countries.CZ is , want CzechRepublic"},
	} {
		err := referenceRowsMismatch(test.regions, test.countries, test.czechia, "2", "3", "Czech Republic")
		if err == nil || err.Error() != test.refusal {
			t.Errorf("%s: referenceRowsMismatch = %v, want %q", name, err, test.refusal)
		}
	}
}

func TestReferencePublisherState(t *testing.T) {
	t.Parallel()
	job := func(conditions ...batchv1.JobCondition) *batchv1.Job {
		return &batchv1.Job{Status: batchv1.JobStatus{Conditions: conditions}}
	}
	condition := func(kind batchv1.JobConditionType, status corev1.ConditionStatus) batchv1.JobCondition {
		return batchv1.JobCondition{Type: kind, Status: status}
	}
	for name, test := range map[string]struct {
		job  *batchv1.Job
		want string
	}{
		"complete":            {job(condition(batchv1.JobComplete, corev1.ConditionTrue)), "complete"},
		"failed":              {job(condition(batchv1.JobFailed, corev1.ConditionTrue)), "failed"},
		"no condition":        {job(), "running"},
		"complete not true":   {job(condition(batchv1.JobComplete, corev1.ConditionFalse)), "running"},
		"failed unknown":      {job(condition(batchv1.JobFailed, corev1.ConditionUnknown)), "running"},
		"only a finish in":    {job(condition(batchv1.JobSuccessCriteriaMet, corev1.ConditionTrue)), "running"},
		"complete read first": {job(condition(batchv1.JobFailed, corev1.ConditionTrue), condition(batchv1.JobComplete, corev1.ConditionTrue)), "complete"},
	} {
		if got := referencePublisherState(test.job); got != test.want {
			t.Errorf("%s: referencePublisherState = %q, want %q", name, got, test.want)
		}
	}
}

func TestReferenceLatestPod(t *testing.T) {
	t.Parallel()
	if _, found := referenceLatestPod(nil); found {
		t.Fatal("a Pod was found among none")
	}
	at := func(name string, minute int) corev1.Pod {
		pod := corev1.Pod{}
		pod.Name = name
		pod.CreationTimestamp = metav1.NewTime(time.Date(2026, 9, 28, 12, minute, 0, 0, time.UTC))
		return pod
	}
	pods := []corev1.Pod{at("b", 5), at("c", 9), at("a", 1)}
	if pod, found := referenceLatestPod(pods); !found || pod.Name != "c" {
		t.Fatalf("referenceLatestPod = %q, %v; want c", pod.Name, found)
	}
	if pods[0].Name != "b" {
		t.Fatal("referenceLatestPod reordered the caller's Pods")
	}
	// Created in the same second, the later one in the list is the last one
	// a stable sort leaves, as kubectl's sort left it.
	if pod, _ := referenceLatestPod([]corev1.Pod{at("first", 3), at("second", 3)}); pod.Name != "second" {
		t.Fatalf("a tie was broken toward %q", pod.Name)
	}
}

func TestReferenceFixtureFiles(t *testing.T) {
	t.Parallel()
	for _, revision := range []string{"v1", "v2", "v3", "v4", "v5"} {
		data, mounts, err := referenceFixtureFiles(filepath.Join(rdRoot, "testdata", "e2e", "reference", revision))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(mounts, "entities.go") || !slices.Contains(mounts, "regions.yaml") || !slices.IsSorted(mounts) {
			t.Errorf("%s: mounts = %q", revision, mounts)
		}
		if len(data) != len(mounts) {
			t.Errorf("%s: %d files stored and %d mounted", revision, len(data), len(mounts))
		}
	}
	directory := t.TempDir()
	for name, content := range map[string]string{"b.yaml": "b", "a.go": "a", "notes.txt": "n", "C.yaml": "C"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(directory, "nested.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	data, mounts, err := referenceFixtureFiles(directory)
	if err != nil {
		t.Fatal(err)
	}
	// Bytewise, as LC_ALL=C sort ordered them. The names differ in more than
	// case, which a case-insensitive filesystem would fold into one file.
	if want := []string{"C.yaml", "a.go", "b.yaml"}; !slices.Equal(mounts, want) {
		t.Fatalf("mounts = %q, want %q", mounts, want)
	}
	if len(data) != 4 || data["notes.txt"] != "n" {
		t.Fatalf("the ConfigMap data is %v, want every regular file", data)
	}
	if _, _, err := referenceFixtureFiles(filepath.Join(directory, "missing")); err == nil {
		t.Fatal("a missing directory was read")
	}
}

// rdDecodeStrict decodes a document into a typed object and refuses a field
// the type does not have, which is what the API server's strict field
// validation would refuse.
func rdDecodeStrict(t *testing.T, document map[string]any, into any) {
	t.Helper()
	content, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		t.Fatalf("the document does not decode strictly: %v", err)
	}
}

func TestReferencePublisherJob(t *testing.T) {
	t.Parallel()
	job := &batchv1.Job{}
	rdDecodeStrict(t, referencePublisherJob("ns", "e2e-push-reference-mysql-v2", "executor@sha256:1", "e2e-reference-mysql-v2",
		"oci://registry/schemas/reference-mysql:stable", "v2", []string{"countries.yaml", "entities.go"}), job)
	if job.Namespace != "ns" || job.Name != "e2e-push-reference-mysql-v2" || job.Labels[labelComponent] != referencePublisherComponent {
		t.Fatalf("metadata = %+v", job.ObjectMeta)
	}
	spec := job.Spec
	if *spec.BackoffLimit != 0 || *spec.ActiveDeadlineSeconds != 300 || *spec.TTLSecondsAfterFinished != 600 {
		t.Fatalf("bounds = %d %d %d", *spec.BackoffLimit, *spec.ActiveDeadlineSeconds, *spec.TTLSecondsAfterFinished)
	}
	pod := spec.Template.Spec
	if pod.RestartPolicy != corev1.RestartPolicyNever || *pod.AutomountServiceAccountToken ||
		len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != registryPullSecret ||
		!*pod.SecurityContext.RunAsNonRoot || *pod.SecurityContext.RunAsUser != 65532 {
		t.Fatalf("pod spec = %+v", pod)
	}
	container := pod.Containers[0]
	wantArgs := []string{"schema", "push", "oci://registry/schemas/reference-mysql:stable", "--root-dir", "/schema", "--version", "v2", "--plain-http"}
	if container.Image != "executor@sha256:1" || !slices.Equal(container.Command, []string{"/usr/local/bin/ptah"}) ||
		!slices.Equal(container.Args, wantArgs) {
		t.Fatalf("container = %+v", container)
	}
	var mounts []string
	for _, mount := range container.VolumeMounts {
		mounts = append(mounts, mount.Name+":"+mount.MountPath+":"+mount.SubPath)
	}
	if want := []string{"work:/work:", "schema:/schema/countries.yaml:countries.yaml", "schema:/schema/entities.go:entities.go"}; !slices.Equal(mounts, want) {
		t.Fatalf("mounts = %q, want %q", mounts, want)
	}
	// The publisher reaches the registry and holds no database credential.
	var secrets []string
	for _, env := range container.Env {
		if env.ValueFrom != nil {
			secrets = append(secrets, env.Name+"="+env.ValueFrom.SecretKeyRef.Name+"/"+env.ValueFrom.SecretKeyRef.Key)
		}
	}
	want := []string{
		"PTAH_OCI_USERNAME=" + registryAuthSecret + "/username",
		"PTAH_OCI_PASSWORD=" + registryAuthSecret + "/password",
		"PTAH_OCI_REGISTRY=" + registryAuthSecret + "/registry",
	}
	if !slices.Equal(secrets, want) {
		t.Fatalf("secret env = %q, want %q", secrets, want)
	}
	if pod.Volumes[0].ConfigMap == nil || pod.Volumes[0].ConfigMap.Name != "e2e-reference-mysql-v2" {
		t.Fatalf("volumes = %+v", pod.Volumes)
	}
}

func TestReferenceSchemaDocument(t *testing.T) {
	t.Parallel()
	names := referenceNamesFor("mysql", "registry:5000", "schemas")
	schema := &ptahv1alpha1.PtahSchema{}
	rdDecodeStrict(t, referenceSchemaDocument("ns", "MySQL", names), schema)
	spec := schema.Spec
	if schema.Namespace != "ns" || schema.Name != names.schema || schema.Kind != "PtahSchema" {
		t.Fatalf("object = %s/%s %s", schema.Namespace, schema.Name, schema.Kind)
	}
	if spec.Target.Engine != "MySQL" || spec.Target.CoordinationKey != names.coordinationKey ||
		spec.Target.URLFrom.Name != names.secret || spec.Target.URLFrom.Key != "url" {
		t.Fatalf("target = %+v", spec.Target)
	}
	desired := spec.Desired
	if desired.OCIRef != names.artifact || desired.RegistryAuthFrom == nil || desired.RegistryAuthFrom.Name != registryAuthSecret ||
		desired.RegistryAuthFrom.Mode != "Environment" || desired.RegistryAuthFrom.UsernameKey != "username" ||
		desired.RegistryAuthFrom.PasswordKey != "password" ||
		desired.VerificationPolicyFrom.Name != referencePolicy || desired.VerificationPolicyFrom.Key != referencePolicyKey ||
		!desired.Transport.PlainHTTP {
		t.Fatalf("desired = %+v", desired)
	}
	if spec.Interval.Duration != 45*time.Second || spec.Policy.Apply != ptahv1alpha1.ApplyPolicyOnApproval ||
		!spec.Policy.AllowDestructive || spec.Execution.ActiveDeadlineSeconds != 600 || len(spec.Policy.ProtectedTables) != 0 {
		t.Fatalf("spec = interval %s, policy %+v, execution %+v", spec.Interval.Duration, spec.Policy, spec.Execution)
	}
}

func TestReferenceApprovalDocument(t *testing.T) {
	t.Parallel()
	plan := &ptahv1alpha1.PtahSchemaPlan{}
	plan.Name, plan.UID = "plan-a", "11111111-1111-1111-1111-111111111111"
	plan.Spec.SchemaRef = ptahv1alpha1.ImmutableObjectReference{Name: "e2e-reference-mysql", UID: "22222222-2222-2222-2222-222222222222"}
	plan.Spec.Fingerprint = "sha256:" + strings.Repeat("f", 64)
	approval := &ptahv1alpha1.PtahSchemaApproval{}
	rdDecodeStrict(t, referenceApprovalDocument("ns", "e2e-reference-mysql-approval-v2", "e2e-reference-mysql", plan), approval)
	spec := approval.Spec
	if approval.Namespace != "ns" || approval.Name != "e2e-reference-mysql-approval-v2" ||
		spec.SchemaRef.Name != "e2e-reference-mysql" || spec.SchemaRef.UID != plan.Spec.SchemaRef.UID ||
		spec.PlanRef.Name != "plan-a" || spec.PlanRef.UID != plan.UID || spec.PlanFingerprint != plan.Spec.Fingerprint {
		t.Fatalf("approval = %+v", approval)
	}
}
