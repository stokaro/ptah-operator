package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/internal/runner"
	operationworkload "github.com/stokaro/ptah-operator/internal/workload"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
)

// databasePod is a terminated operation Pod whose Ptah container transported
// result through its summary, as the real runner writes it.
func databasePod(t *testing.T, operation runner.Operation, family string, created time.Time, run time.Duration, mutate func(*runner.Result)) corev1.Pod {
	t.Helper()
	result := runner.Result{Operation: operation, OperationID: "operation-id", ChildExitCode: 1,
		Error: &runner.ResultError{Code: "child_exit", Message: "Ptah exited with code 1"}}
	if mutate != nil {
		mutate(&result)
	}
	encoded, err := runner.EncodeResult(result)
	if err != nil || encoded.SummaryErr != nil {
		t.Fatalf("encode the result: %v / %v", err, encoded.SummaryErr)
	}
	started := created.Add(time.Second)
	// The label is the claim's type, as the operator writes it: a migration's
	// History and Apply claims run migration-history and migration-apply.
	label := strings.TrimPrefix(string(operation), "migration-")
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: string(operation), Namespace: "work", UID: "pod-uid",
			CreationTimestamp: metav1.NewTime(created),
			Labels: map[string]string{operationworkload.LabelOperation: label,
				"operator.ptah.run/" + family: "capacity-" + family + "-000"},
			Annotations: map[string]string{operationworkload.AnnotationOperationID: "operation-id"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "ptah",
			Args: []string{"--ptah-binary", "/ptah", "--operation", string(operation)}}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "ptah",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				StartedAt: metav1.NewTime(started), FinishedAt: metav1.NewTime(started.Add(run)),
				Message: string(encoded.Summary), ExitCode: 0,
			}}}}},
	}
}

func TestADelayedDatabaseOperationIsOneTheFaultReached(t *testing.T) {
	t.Parallel()
	injected := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	after := injected.Add(10 * time.Second)
	const delay = 30 * time.Second
	s := &scenarios{in: inputs{namespace: "work"}, load: workload{Schemas: 1, Migrations: 1}}
	succeeded := func(r *runner.Result) {
		*r = runner.Result{Operation: runner.OperationObserve, OperationID: r.OperationID, ChildExitCode: 0,
			CoordinationDigest:   "sha256:" + strings.Repeat("9", 64),
			TargetIdentityDigest: "sha256:" + strings.Repeat("8", 64),
			DriftReportDigest:    "sha256:" + strings.Repeat("7", 64), ObservedDialect: "postgres"}
	}

	for _, row := range []struct {
		name    string
		pod     corev1.Pod
		family  string
		outcome string
	}{
		{name: "an Observe that failed to reach the database", pod: databasePod(t, runner.OperationObserve, "schema", after, 10*time.Second, nil), family: "schema", outcome: "failed"},
		{name: "a History that failed to reach the database", pod: databasePod(t, runner.OperationMigrationHistory, "migration", after, 10*time.Second, nil), family: "migration", outcome: "failed"},
		{name: "an Observe that waited out the delay and succeeded", pod: databasePod(t, runner.OperationObserve, "schema", after, 31*time.Second, succeeded), family: "schema", outcome: "delayed"},
		{name: "a Resolve, which reads the registry", pod: databasePod(t, runner.OperationResolve, "schema", after, 10*time.Second, nil)},
		{name: "a Verify, which reads the registry", pod: databasePod(t, runner.OperationVerify, "migration", after, 40*time.Second, nil)},
		{name: "an operation created before the fault", pod: databasePod(t, runner.OperationObserve, "schema", injected.Add(-time.Second), 40*time.Second, nil)},
		{name: "a prompt success", pod: databasePod(t, runner.OperationObserve, "schema", after, 2*time.Second, succeeded)},
		{name: "a failure that may have mutated", pod: databasePod(t, runner.OperationApply, "schema", after, 10*time.Second, func(r *runner.Result) { r.MutationStarted = true })},
		{name: "a failure that left the outcome uncertain", pod: databasePod(t, runner.OperationMigrationApply, "migration", after, 10*time.Second, func(r *runner.Result) {
			r.MutationStarted, r.Uncertain = true, true
		})},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			family, record, ok := s.delayedDatabaseOperation(row.pod, injected, delay)
			if ok != (row.family != "") || family != row.family || record.Outcome != row.outcome {
				t.Fatalf("got %q %+v %v, want family %q outcome %q", family, record, ok, row.family, row.outcome)
			}
		})
	}

	relabeled := databasePod(t, runner.OperationResolve, "schema", after, 10*time.Second, nil)
	relabeled.Labels[operationworkload.LabelOperation] = "observe"
	if _, _, ok := s.delayedDatabaseOperation(relabeled, injected, delay); ok {
		t.Fatal("a registry operation labeled as a database one counted as evidence")
	}
	if got := databasePod(t, runner.OperationMigrationHistory, "migration", after, 10*time.Second, nil).Labels[operationworkload.LabelOperation]; got != "history" {
		t.Fatalf("the fixture labels a History claim %q, not as the operator does", got)
	}
	foreign := databasePod(t, runner.OperationObserve, "schema", after, 10*time.Second, nil)
	foreign.Labels["operator.ptah.run/schema"] = "someone-else"
	if _, _, ok := s.delayedDatabaseOperation(foreign, injected, delay); ok {
		t.Fatal("a resource outside the workload counted as evidence")
	}
	anonymous := databasePod(t, runner.OperationObserve, "schema", after, 10*time.Second, nil)
	delete(anonymous.Annotations, operationworkload.AnnotationOperationID)
	if _, _, ok := s.delayedDatabaseOperation(anonymous, injected, delay); ok {
		t.Fatal("a Pod without its operation ID counted as evidence")
	}
	mislabeled := databasePod(t, runner.OperationObserve, "schema", after, 10*time.Second, nil)
	mislabeled.Annotations[operationworkload.AnnotationOperationID] = "another-operation"
	if _, _, ok := s.delayedDatabaseOperation(mislabeled, injected, delay); ok {
		t.Fatal("a summary for another operation counted as evidence")
	}
}

func TestEveryFamilyAndNamespaceNeedsItsOwnDelayedOperation(t *testing.T) {
	t.Parallel()
	s := &scenarios{in: inputs{namespaces: []string{"a", "b"}, namespace: "a"}, load: workload{Schemas: 2, Migrations: 2}}
	proof := &databaseDelayProof{DelayedSessions: 4, Operations: map[string]delayedOperation{
		"a/schema": {}, "b/schema": {}, "a/migration": {},
	}}
	if err := s.requireDelayedFamilies(proof); err == nil || !strings.Contains(err.Error(), "migration operation in b") {
		t.Fatalf("a namespace without a delayed migration passed: %v", err)
	}
	proof.Operations["b/migration"] = delayedOperation{}
	if err := s.requireDelayedFamilies(proof); err != nil {
		t.Fatal(err)
	}
	proof.DelayedSessions = 0
	if err := s.requireDelayedFamilies(proof); err == nil {
		t.Fatal("operations failing on their own counted while the proxy delayed nothing")
	}
}

func TestTheProxyLogMustShowEverySessionHeldForTheDelay(t *testing.T) {
	t.Parallel()
	log := `{"event":"accepted","connection":1,"at":"2026-10-09T12:00:00Z"}
{"event":"connected","connection":1,"at":"2026-10-09T12:00:30Z","delayedSeconds":30.002}
{"event":"accepted","connection":2,"at":"2026-10-09T12:00:01Z"}
{"event":"connected","connection":2,"at":"2026-10-09T12:00:31Z","delayedSeconds":30.5}
{"event":"closed","connection":1,"at":"2026-10-09T12:00:40Z"}
`
	sessions, shortest, err := countDelayedSessions([]byte(log), 30*time.Second)
	if err != nil || sessions != 2 || shortest != 30.002 {
		t.Fatalf("counted %d sessions, shortest %v, %v", sessions, shortest, err)
	}
	early := log + `{"event":"connected","connection":3,"at":"2026-10-09T12:00:32Z","delayedSeconds":12}` + "\n"
	if _, _, err := countDelayedSessions([]byte(early), 30*time.Second); err == nil {
		t.Fatal("a session connected before the delay passed")
	}
	if _, _, err := countDelayedSessions([]byte("not json\n"), 30*time.Second); err == nil {
		t.Fatal("an unreadable proxy log passed")
	}
	if sessions, _, err := countDelayedSessions(nil, 30*time.Second); err != nil || sessions != 0 {
		t.Fatalf("an empty log counted %d sessions, %v", sessions, err)
	}
}

func TestTheDatabaseServiceSettlesOnExactlyTheProxyOrTheDatabase(t *testing.T) {
	t.Parallel()
	ready, notReady := true, false
	slice := func(entries ...discoveryv1.Endpoint) []discoveryv1.EndpointSlice {
		return []discoveryv1.EndpointSlice{{Endpoints: entries}}
	}
	endpoint := func(address string, isReady *bool) discoveryv1.Endpoint {
		return discoveryv1.Endpoint{Addresses: []string{address}, Conditions: discoveryv1.EndpointConditions{Ready: isReady}}
	}
	for _, row := range []struct {
		name    string
		slices  []discoveryv1.EndpointSlice
		proxy   string
		settled bool
	}{
		{"only the proxy", slice(endpoint("10.0.0.9", &ready)), "10.0.0.9", true},
		{"the proxy beside the database", slice(endpoint("10.0.0.9", &ready), endpoint("10.0.0.5", &ready)), "10.0.0.9", false},
		{"the database alone while the proxy is wanted", slice(endpoint("10.0.0.5", &ready)), "10.0.0.9", false},
		{"a draining database beside the proxy", slice(endpoint("10.0.0.9", &ready), endpoint("10.0.0.5", &notReady)), "10.0.0.9", true},
		{"the database after the restore", slice(endpoint("10.0.0.5", &ready)), "", true},
		{"no ready endpoint after the restore", slice(endpoint("10.0.0.5", &notReady)), "", false},
	} {
		if got := delayEndpointsSettled(row.slices, row.proxy); got != row.settled {
			t.Fatalf("%s: settled = %v, want %v", row.name, got, row.settled)
		}
	}
}

func TestTheBootstrapStateNamesTheDatabaseServer(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	db, err := readCapacityDatabase(write("pg.json", `{"engine":"PostgreSQL","fixtureNamespace":"fixtures"}`))
	if err != nil || db != (capacityDatabase{namespace: "fixtures", name: "capacity-postgres", portName: "postgresql", port: 5432}) {
		t.Fatalf("PostgreSQL state read as %+v, %v", db, err)
	}
	db, err = readCapacityDatabase(write("mysql.json", `{"engine":"MySQL","fixtureNamespace":"fixtures"}`))
	if err != nil || db.name != "capacity-mysql" || db.port != 3306 || db.portName != "mysql" {
		t.Fatalf("MySQL state read as %+v, %v", db, err)
	}
	for name, content := range map[string]string{
		"no-fixture.json": `{"engine":"PostgreSQL"}`,
		"engine.json":     `{"engine":"Oracle","fixtureNamespace":"fixtures"}`,
		"broken.json":     `{`,
	} {
		if _, err := readCapacityDatabase(write(name, content)); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func TestTheDatabaseDelayWorkloadSpansTwoIntervals(t *testing.T) {
	t.Parallel()
	base := workload{Name: "soak", Schemas: 10, Migrations: 10, Interval: duration{2 * time.Minute}, Settle: duration{5 * time.Minute},
		SteadyState: duration{time.Minute}, SampleEvery: duration{5 * time.Second}}
	valid := base
	valid.DatabaseDelay = &databaseDelayWorkload{Delay: duration{30 * time.Second}, Hold: duration{5 * time.Minute}}
	if err := valid.validate(); err != nil {
		t.Fatal(err)
	}
	for name, delay := range map[string]*databaseDelayWorkload{
		"a delay under a second":         {Delay: duration{time.Millisecond}, Hold: duration{5 * time.Minute}},
		"a hold shorter than two cycles": {Delay: duration{30 * time.Second}, Hold: duration{3 * time.Minute}},
	} {
		w := base
		w.DatabaseDelay = delay
		if err := w.validate(); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	backlog := valid
	backlog.ApprovalBacklog, backlog.ChangeBatch = true, 5
	if err := backlog.validate(); err == nil || !strings.Contains(err.Error(), "runs no database delay") {
		t.Fatalf("the approval backlog accepted a database delay: %v", err)
	}
}

func databaseService(selector map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "capacity-postgres", Namespace: "fixtures", UID: "service-uid"},
		Spec: corev1.ServiceSpec{Selector: selector, Ports: []corev1.ServicePort{{
			Name: "postgresql", Port: 5432, TargetPort: intstr.FromString("postgresql"),
		}}},
	}
}

func TestADatabaseServiceTheBootstrapDidNotCreateIsRefusedBeforeAnyChange(t *testing.T) {
	t.Parallel()
	db := capacityDatabase{namespace: "fixtures", name: "capacity-postgres", portName: "postgresql", port: 5432}
	clientset := fake.NewClientset(databaseService(map[string]string{"app": "something-else"}))
	s := &scenarios{clientset: clientset, in: inputs{namespace: "work", fixtureImage: "registry/fixture@sha256:" + strings.Repeat("a", 64), database: db},
		load: workload{Schemas: 1, Migrations: 1, DatabaseDelay: &databaseDelayWorkload{Delay: duration{30 * time.Second}, Hold: duration{5 * time.Minute}}}}
	if err := s.databaseDelay(context.Background()); err == nil || !strings.Contains(err.Error(), "not the one the bootstrap created") {
		t.Fatalf("a foreign database Service was accepted: %v", err)
	}
	for _, action := range clientset.Actions() {
		if action.GetVerb() != "get" {
			t.Fatalf("a refused delay changed the cluster: %s %s", action.GetVerb(), action.GetResource().Resource)
		}
	}
}

func TestTheDatabaseSelectorMovesOnlyFromWhatItExpects(t *testing.T) {
	t.Parallel()
	db := capacityDatabase{namespace: "fixtures", name: "capacity-postgres", portName: "postgresql", port: 5432}
	ctx := context.Background()

	clientset := fake.NewClientset(databaseService(db.labels()))
	s := &scenarios{clientset: clientset}
	if err := s.setDatabaseSelector(ctx, db, "service-uid", db.labels(), db.delayedLabels()); err != nil {
		t.Fatal(err)
	}
	moved, _ := clientset.CoreV1().Services("fixtures").Get(ctx, db.name, metav1.GetOptions{})
	if !reflect.DeepEqual(moved.Spec.Selector, db.delayedLabels()) {
		t.Fatalf("the Service selects %v", moved.Spec.Selector)
	}
	if err := s.setDatabaseSelector(ctx, db, "service-uid", db.labels(), db.delayedLabels()); err != nil {
		t.Fatalf("a repeated move to the same selector failed: %v", err)
	}
	if err := s.setDatabaseSelector(ctx, db, "another-uid", db.delayedLabels(), db.labels()); err == nil {
		t.Fatal("a replaced Service was moved")
	}

	foreign := fake.NewClientset(databaseService(map[string]string{"app.kubernetes.io/name": "capacity-postgres", "tier": "edited"}))
	s = &scenarios{clientset: foreign}
	if err := s.setDatabaseSelector(ctx, db, "service-uid", db.labels(), db.delayedLabels()); err == nil {
		t.Fatal("a selector someone else edited was overwritten")
	}
}

// Every qualification workload must load, and each engine's twin must run the
// same dimensions: a MySQL soak with a shorter hold or a smaller fleet would
// qualify a different profile under the same name.
func TestEveryWorkloadLoadsAndEachEngineRunsTheSameDimensions(t *testing.T) {
	t.Parallel()
	paths, err := filepath.Glob("../../support/capacity/*.json")
	if err != nil {
		t.Fatal(err)
	}
	loaded := map[string]workload{}
	for _, path := range paths {
		name := filepath.Base(path)
		if name == "input-calibration.json" {
			continue
		}
		w, err := loadWorkload(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		loaded[strings.TrimSuffix(name, ".json")] = w
	}
	for _, pair := range [][2]string{{"soak", "soak-mysql"}, {"backlog", "backlog-mysql"}, {"overload", "overload-mysql"}, {"workload", "workload-mysql"}} {
		postgres, ok := loaded[pair[0]]
		mysql, twin := loaded[pair[1]]
		if !ok || !twin {
			t.Fatalf("%s needs both engines", pair[0])
		}
		if postgres.Engine != "PostgreSQL" || mysql.Engine != "MySQL" {
			t.Fatalf("%s engines are %s and %s", pair[0], postgres.Engine, mysql.Engine)
		}
		if pair[0] == "workload" {
			continue
		}
		postgres.Name, postgres.Engine, postgres.Description = "", "", ""
		mysql.Name, mysql.Engine, mysql.Description = "", "", ""
		if !reflect.DeepEqual(postgres, mysql) {
			t.Fatalf("%s and %s run different dimensions", pair[0], pair[1])
		}
	}
	soak := loaded["soak"]
	if soak.DatabaseDelay == nil || soak.DatabaseDelay.Delay.Duration != 30*time.Second || soak.Outage.Duration != 3*time.Minute {
		t.Fatal("the soak lost the frozen slow-dependency probes: a 3-minute registry outage and a 30-second database delay")
	}
	overload := loaded["overload"]
	if overload.Overload == nil || overload.Schemas+overload.Migrations != 40 || overload.Overload.Recovery.Duration != 5*time.Minute {
		t.Fatal("the overload probe lost the frozen 40-resource fleet or its 5-minute recovery")
	}
}
