package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestPostgresRefusalReadsNativeReusedAddressSessions(t *testing.T) {
	t.Parallel()
	read := func(name string) []byte {
		t.Helper()
		data, err := os.ReadFile("../../testdata/e2e/readings/postgresql-reused-address." + name)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	var metadata struct {
		Address, PreviousID, CurrentID, Database string
		Created, Started, Finished               time.Time
	}
	if err := json.Unmarshal(read("json"), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.PreviousID == metadata.CurrentID || metadata.PreviousID == "" || metadata.CurrentID == "" || metadata.Created.IsZero() ||
		metadata.Started.Before(metadata.Created) || metadata.Finished.Before(metadata.Started) {
		t.Fatal("the native reading lost its distinct clients or actual container lifetime")
	}
	clients := map[string]operationSQLClient{metadata.Address: {resourceUID: "native-probe", jobUID: "native-probe", podUID: metadata.CurrentID, operation: "observe"}}
	spans := map[string]operationSQLLifetime{metadata.Address: {
		created: metadata.Created.Truncate(time.Second), endedBefore: metadata.Finished.Truncate(time.Second).Add(time.Second),
	}}
	check := func(raw []byte, lifetimes ...map[string]operationSQLLifetime) (map[string]int, error) {
		return postgresStatementRefusalSQL(raw, metadata.Database, clients, schemaDiagnosticActor,
			func(actor operationSQLClient, statement, parameters string) bool {
				return statement == "SELECT 24242" && parameters == ""
			}, nil, lifetimes...)
	}
	raw := read("jsonl")
	var positive []byte
	rows, refused := 0, 0
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
		var row struct {
			Message    string `json:"message"`
			RemoteHost string `json:"remote_host"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		if row.RemoteHost != metadata.Address {
			t.Fatal("the native server did not observe the reused address")
		}
		rows++
		if row.Message == "statement: SELECT 24243" {
			refused++
			continue
		}
		positive = append(positive, line...)
		positive = append(positive, '\n')
	}
	if rows != 3 || refused != 1 {
		t.Fatal("the native reading must retain earlier, target and current cross-database SQL")
	}
	if _, err := check(positive); err == nil || !strings.Contains(err.Error(), "undeclared database") {
		t.Fatal("the native receipt no longer reproduces the original attribution failure", err)
	}
	counts, err := check(positive, spans)
	if err != nil || len(counts) != 1 || counts[metadata.Address] != 1 {
		t.Fatal("the earlier IP owner hid or supplied the new native control", counts, err)
	}
	if _, err := check(raw, spans); err == nil || !strings.Contains(err.Error(), "undeclared database") {
		t.Fatal("the actual current client's cross-database SQL was admitted", err)
	}
}

func TestSQLClientLifetimeRequiresTheActualTerminalPod(t *testing.T) {
	t.Parallel()
	_, refused, _, _, _, _ := runnerApplyFixture("PtahMigration")
	refused.CreationTimestamp = metav1.NewTime(time.Unix(100, 0))
	succeeded := refused.DeepCopy()
	succeeded.Status.Phase = corev1.PodSucceeded
	succeeded.Status.InitContainerStatuses[1].State.Terminated.ExitCode = 0
	succeeded.Status.InitContainerStatuses = append(succeeded.Status.InitContainerStatuses, corev1.ContainerStatus{
		Name: "fetch-migrations", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			StartedAt: metav1.NewTime(time.Unix(105, 0)), FinishedAt: metav1.NewTime(time.Unix(106, 0)),
		}},
	})
	succeeded.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "ptah", State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{StartedAt: metav1.NewTime(time.Unix(107, 0)), FinishedAt: metav1.NewTime(time.Unix(109, 0))},
	}}}
	clients := map[string]operationSQLClient{refused.Status.PodIP: {podUID: string(refused.UID), jobUID: "exact-job", resourceUID: "exact-resource", operation: "apply"}}
	for _, pod := range []*corev1.Pod{refused, succeeded} {
		lifetimes, err := operationSQLLifetimes(clients, map[types.UID]corev1.Pod{pod.UID: *pod})
		end := time.Unix(105, 0)
		if pod.Status.Phase == corev1.PodSucceeded {
			end = time.Unix(110, 0)
		}
		if err != nil || len(lifetimes) != 1 || !lifetimes[pod.Status.PodIP].created.Equal(pod.CreationTimestamp.Time) || !lifetimes[pod.Status.PodIP].endedBefore.Equal(end) {
			t.Fatal("the actual terminal container boundary was lost", err)
		}
	}
	for name, mutate := range map[string]func(*corev1.Pod){
		"replacement UID": func(p *corev1.Pod) { p.UID = "replacement" },
		"other address":   func(p *corev1.Pod) { p.Status.PodIP = "10.0.0.3" },
		"no creation":     func(p *corev1.Pod) { p.CreationTimestamp = metav1.Time{} },
		"no finish":       func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.FinishedAt = metav1.Time{} },
		"reversed dates": func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Terminated.FinishedAt = metav1.NewTime(time.Unix(106, 0))
		},
		"started before creation": func(p *corev1.Pod) {
			p.Status.InitContainerStatuses[0].State.Terminated.StartedAt = metav1.NewTime(time.Unix(99, 0))
		},
		"restarted":      func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 1 },
		"still running":  func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning },
		"missing status": func(p *corev1.Pod) { p.Status.ContainerStatuses = nil },
	} {
		t.Run(name, func(t *testing.T) {
			pod := succeeded.DeepCopy()
			mutate(pod)
			if _, err := operationSQLLifetimes(clients, map[types.UID]corev1.Pod{refused.UID: *pod}); err == nil {
				t.Fatal("an incomplete or replaced workload supplied a lifetime")
			}
		})
	}
	if _, err := operationSQLLifetimes(clients, nil); err == nil {
		t.Fatal("a missing Pod supplied the exact client's lifetime")
	}
	if _, err := operationSQLLifetimes(nil, map[types.UID]corev1.Pod{succeeded.UID: *succeeded}); err == nil {
		t.Fatal("an empty client inventory passed")
	}
}

func TestPostgresRefusalSeparatesAddressReuseFromCrossDatabaseSQL(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../testdata/e2e/readings/postgresql-sql-audit.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var native map[string]any
	if err := json.Unmarshal(raw, &native); err != nil {
		t.Fatal(err)
	}
	instant, err := time.Parse("2006-01-02 15:04:05.999999999 UTC", native["timestamp"].(string))
	if err != nil {
		t.Fatal(err)
	}
	host, database := native["remote_host"].(string), native["dbname"].(string)
	clients := map[string]operationSQLClient{host: {resourceUID: "exact-resource", jobUID: "exact-job", podUID: "exact-pod", operation: "observe"}}
	life := operationSQLLifetime{created: instant.Add(-time.Second), endedBefore: instant.Add(time.Second)}
	spans := map[string]operationSQLLifetime{host: life}
	accept := func(actor operationSQLClient, statement, parameters string) bool {
		return schemaDiagnosticActor(actor) && statement == "SELECT 24242" && parameters == ""
	}
	check := func(journal []byte, scope ...map[string]operationSQLLifetime) (map[string]int, error) {
		return postgresStatementRefusalSQL(journal, database, clients, schemaDiagnosticActor, accept, nil, scope...)
	}
	row := func(db string, at time.Time, change func(map[string]any)) []byte {
		r := make(map[string]any, len(native))
		for key, value := range native {
			r[key] = value
		}
		r["dbname"], r["timestamp"] = db, at.Format("2006-01-02 15:04:05.999999999 UTC")
		r["session_start"] = at.Format("2006-01-02 15:04:05 UTC")
		if change != nil {
			change(r)
		}
		encoded, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		return append(encoded, '\n')
	}
	journal := append(row("earlier_database", life.created.Add(-time.Second), nil), raw...)
	journal = append(journal, row("later_database", life.endedBefore.Add(time.Second), nil)...)
	if _, err := check(journal); err == nil {
		t.Fatal("the original matcher no longer reproduces the address-reuse refusal")
	}
	counts, err := check(journal, spans)
	if err != nil || len(counts) != 1 || counts[host] != 1 {
		t.Fatal("another use of the IP supplied SQL or hid the exact native control", counts, err)
	}
	for name, bad := range map[string][]byte{
		"other database during lifetime": row("unauthorized_database", instant, nil),
		"other database at creation":     row("unauthorized_database", life.created, nil),
		"target before creation":         row(database, life.created.Add(-time.Nanosecond), nil),
		"target after termination":       row(database, life.endedBefore, nil),
		"late error on the same connection": row("unauthorized_database", life.endedBefore.Add(time.Hour), func(r map[string]any) {
			r["session_start"] = instant.Format("2006-01-02 15:04:05 UTC")
			r["message"], r["statement"] = "canceling statement due to user request", "DELETE FROM widgets"
		}),
		"ambiguous session at termination": row("unauthorized_database", life.endedBefore, nil),
		"missing session start":            row("other_database", life.created.Add(-time.Second), func(r map[string]any) { delete(r, "session_start") }),
		"session starts after statement": row("other_database", life.endedBefore.Add(time.Second), func(r map[string]any) {
			r["session_start"] = life.endedBefore.Add(time.Hour).Format("2006-01-02 15:04:05 UTC")
		}),
		"missing timestamp": row(database, instant, func(r map[string]any) { delete(r, "timestamp") }),
		"invalid timestamp": row(database, instant, func(r map[string]any) { r["timestamp"] = "unknown" }),
		"unknown client":    row(database, instant, func(r map[string]any) { r["remote_host"] = "172.17.0.99" }),
		"unauthorized SQL":  row(database, instant, func(r map[string]any) { r["message"] = "statement: DELETE FROM widgets" }),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := check(append(append([]byte{}, raw...), bad...), spans); err == nil {
				t.Fatal("an unauthorized or unattributed statement entered the diagnostic window")
			}
		})
	}
	if _, err := check(journal, nil); err == nil {
		t.Fatal("the optional inventory silently dropped its required client")
	}
	if _, err := check(journal, map[string]operationSQLLifetime{host: {created: life.endedBefore, endedBefore: life.created}}); err == nil {
		t.Fatal("a reversed lifetime passed")
	}
	if _, err := check(journal, spans, spans); err == nil {
		t.Fatal("ambiguous lifetime inventories passed")
	}
	if _, err := check(row("earlier_database", life.created.Add(-time.Second), nil), spans); err == nil || !strings.Contains(err.Error(), "did not observe") {
		t.Fatal("other-database traffic supplied the nonempty diagnostic control", err)
	}
}
