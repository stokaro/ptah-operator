package e2e

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/stokaro/ptah-operator/test/e2e/phases"
)

// The fixture passwords are derived with POSIX cksum, so the Go phase has to
// print what cksum prints for every length the byte count loop can take.
func TestPosixCksumMatchesCksum(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]uint32{
		"":               4294967295,
		"a":              1220704766,
		"123456789":      930766865,
		"ptah-e2e-12345": 216964841,
	} {
		if got := posixCksum([]byte(input)); got != want {
			t.Errorf("posixCksum(%q) = %d, cksum prints %d", input, got, want)
		}
	}
}

func TestFixtureCredentialsDeriveFromTheNamespace(t *testing.T) {
	t.Parallel()
	credentials := deriveFixtureCredentials("ptah-e2e-12345")
	if credentials.pgPassword != "e2ePg216964841Q7" || credentials.mysqlPassword != "e2eMy216964841Q7" ||
		credentials.mysqlRootPassword != "e2eMyRoot216964841Q7" {
		t.Fatalf("passwords = %q, %q, %q", credentials.pgPassword, credentials.mysqlPassword, credentials.mysqlRootPassword)
	}
	for name, url := range map[string]string{
		"primary":      credentials.pgURL,
		"custom CA":    credentials.customCAPGURL,
		"four-eyes":    credentials.fourEyesPGURL,
		"Pod metadata": credentials.podMetadataPGURL,
	} {
		if !strings.HasPrefix(url, "postgres://ptah_e2e:e2ePg216964841Q7@e2e-postgresql.ptah-e2e-12345.svc.cluster.local:5432/ptah_e2e") ||
			!strings.HasSuffix(url, "?sslmode=disable") {
			t.Errorf("%s URL = %s", name, url)
		}
	}
	databases := []string{credentials.pgURL, credentials.customCAPGURL, credentials.fourEyesPGURL, credentials.podMetadataPGURL}
	if len(slices.Compact(slices.Sorted(slices.Values(databases)))) != 4 {
		t.Fatalf("two rows share a database: %v", databases)
	}
	if credentials.mysqlURL != "mysql://ptah_e2e:e2eMy216964841Q7@tcp(e2e-mysql.ptah-e2e-12345.svc.cluster.local:3306)/ptah_e2e" {
		t.Fatalf("MySQL URL = %s", credentials.mysqlURL)
	}
}

// The rows that plan a schema of their own against the lifecycle's server
// need databases the lifecycle's schema does not declare, or each plan drops
// e2e_widgets and blocks.
func TestIsolatedDatabasesAreNotTheLifecycles(t *testing.T) {
	t.Parallel()
	for _, database := range []string{customCAPGDatabase, fourEyesPGDatabase, podMetadataPGDatabase} {
		if database == pgDatabase || !strings.HasPrefix(database, "ptah_e2e_") {
			t.Errorf("database %s is the lifecycle's or outside the ptah_e2e_ prefix", database)
		}
	}
	for _, secret := range []string{customCAPGSecret, fourEyesPGSecret, podMetadataPGSecret} {
		if secret == pgSecret {
			t.Errorf("Secret %s is the lifecycle's", secret)
		}
	}
}

// Each proof that moves a schema between two intervals relies on the two
// differing, and the waits have to cover three blocked refresh intervals.
func TestDataPlaneIntervalsDiffer(t *testing.T) {
	t.Parallel()
	for _, pair := range [][2]string{
		{reconcileInterval, approvalInterval},
		{tagMoveInterval, reconcileInterval},
		{staleApprovalInterval, tagMoveInterval},
		{quiescentInterval, blockedRefreshInterval},
	} {
		if pair[0] == pair[1] {
			t.Errorf("intervals %s and %s are one interval", pair[0], pair[1])
		}
	}
	if blockedRefreshInterval != strconv.Itoa(blockedRefreshSeconds)+"s" {
		t.Errorf("blockedRefreshInterval %s is not %d seconds", blockedRefreshInterval, blockedRefreshSeconds)
	}
	if blockedRefreshSeconds < 10 {
		t.Error("the blocked refresh interval must be at least ten seconds")
	}
	if waitTimeout < time.Duration(blockedRefreshSeconds*3+120)*time.Second {
		t.Error("a wait must cover three blocked refresh intervals plus two minutes")
	}
}

func TestCredentialScannerFindsEveryPattern(t *testing.T) {
	t.Parallel()
	scanner, err := newCredentialScanner("s3cret", "postgres://user:s3cret@host/db")
	if err != nil {
		t.Fatal(err)
	}
	if scanner.leaks([]byte(`{"message":"connected"}`)) {
		t.Error("clean content read as a leak")
	}
	for _, content := range []string{"password s3cret here", "dial postgres://user:s3cret@host/db failed"} {
		if !scanner.leaks([]byte(content)) {
			t.Errorf("%q did not read as a leak", content)
		}
	}
	for _, patterns := range [][]string{nil, {""}, {"ok", ""}, {"two\nlines"}} {
		if _, err := newCredentialScanner(patterns...); err == nil {
			t.Errorf("a scanner built from %q was accepted", patterns)
		}
	}
	if (credentialScanner{}).ready() {
		t.Error("a scanner never built reads as ready")
	}
}

func validDataPlaneInputs() phases.DataPlaneInputs {
	digest := "@sha256:" + strings.Repeat("a", 64)
	return phases.DataPlaneInputs{
		Kubeconfig: "/work/kubeconfig", OperatorNamespace: "ptah-system", TestNamespace: "ptah-e2e",
		HelmRelease: "ptah", ChartPackage: "/work/chart.tgz", PtahVersion: "v0.9.0",
		ExecutorImage: "registry/executor" + digest, RunnerImage: "registry/runner" + digest,
		FixtureImage: "registry/fixture" + digest, ControllerImage: "registry/manager" + digest,
		ControllerRevision: "abc123", ControllerStateVersion: "3",
		PostgresImage: "registry/postgres" + digest, MySQLImage: "registry/mysql" + digest,
		RegistryIP: "172.18.0.5", RegistryService: "registry", RegistryPort: "5001",
		RegistryCredentialsFile: "/work/registry.json", DockerContext: "remote",
		RegistryContainerID:         strings.Repeat("b", 64),
		ExternalPostgresContainerID: strings.Repeat("c", 64), ExternalPostgresIP: "172.18.0.6",
		ExternalPostgresService: "external-pg", ExternalPostgresImage: "registry/postgres" + digest,
		ExternalPostgresOwner: "ptah-e2e-1.35", ExternalPostgresCredentialsFile: "/work/external.json",
		TLSProxyService: "tls-proxy", TLSProxyCAFile: "/work/ca.pem", TLSProxyCertFile: "/work/tls.crt",
		TLSProxyKeyFile: "/work/tls.key", Mode: "full",
	}
}

func TestDataPlaneInputsRefuseWhatThePhaseCannotUse(t *testing.T) {
	t.Parallel()
	if err := dataPlaneInputsOK(validDataPlaneInputs()); err != nil {
		t.Fatalf("valid inputs refused: %v", err)
	}
	prepare := validDataPlaneInputs()
	prepare.Mode = "prepare"
	if err := dataPlaneInputsOK(prepare); err != nil {
		t.Fatalf("prepare mode refused: %v", err)
	}
	for _, test := range []struct {
		name   string
		edit   func(*phases.DataPlaneInputs)
		refuse string
	}{
		{"unknown mode", func(in *phases.DataPlaneInputs) { in.Mode = "partial" }, "full or prepare"},
		{"tagged controller image", func(in *phases.DataPlaneInputs) { in.ControllerImage = "registry/manager:latest" }, "E2E_CONTROLLER_IMAGE"},
		{"revision with edge space", func(in *phases.DataPlaneInputs) { in.ControllerRevision = " abc" }, "edge whitespace"},
		{"state version zero", func(in *phases.DataPlaneInputs) { in.ControllerStateVersion = "0" }, "positive integer"},
		{"default Docker context", func(in *phases.DataPlaneInputs) { in.DockerContext = "default" }, "nonlocal"},
		{"OrbStack Docker context", func(in *phases.DataPlaneInputs) { in.DockerContext = "orbstack" }, "nonlocal"},
		{"long Ptah version", func(in *phases.DataPlaneInputs) { in.PtahVersion = strings.Repeat("v", 129) }, "between 1 and 128"},
		{"Ptah version with a control character", func(in *phases.DataPlaneInputs) { in.PtahVersion = "v1\x01" }, "control"},
		{"short container ID", func(in *phases.DataPlaneInputs) { in.RegistryContainerID = "abc" }, "64-character"},
		{"uppercase container ID", func(in *phases.DataPlaneInputs) {
			in.ExternalPostgresContainerID = strings.Repeat("C", 64)
		}, "64-character"},
		{"external address not IPv4", func(in *phases.DataPlaneInputs) { in.ExternalPostgresIP = "fd00::1" }, "IPv4"},
		{"external Service not a label", func(in *phases.DataPlaneInputs) { in.ExternalPostgresService = "External" }, "DNS label"},
		{"owner with a slash", func(in *phases.DataPlaneInputs) { in.ExternalPostgresOwner = "a/b" }, "unsupported characters"},
		{"external image tagged", func(in *phases.DataPlaneInputs) { in.ExternalPostgresImage = "postgres:17" }, "digest-pinned"},
		{"proxy Service not a label", func(in *phases.DataPlaneInputs) { in.TLSProxyService = "-proxy" }, "DNS label"},
		{"CA and key one file", func(in *phases.DataPlaneInputs) { in.TLSProxyKeyFile = in.TLSProxyCAFile }, "separate files"},
		{"fixture image tagged", func(in *phases.DataPlaneInputs) { in.FixtureImage = "fixture:dev" }, "data-plane images"},
		{"registry address not IPv4", func(in *phases.DataPlaneInputs) { in.RegistryIP = "registry" }, "IPv4"},
		{"registry port not numeric", func(in *phases.DataPlaneInputs) { in.RegistryPort = "5k" }, "numeric"},
		{"privileged registry port", func(in *phases.DataPlaneInputs) { in.RegistryPort = "443" }, "between 1024 and 65535"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			in := validDataPlaneInputs()
			test.edit(&in)
			if err := dataPlaneInputsOK(in); err == nil || !strings.Contains(err.Error(), test.refuse) {
				t.Fatalf("dataPlaneInputsOK() = %v, want a refusal naming %q", err, test.refuse)
			}
		})
	}
}

func TestRegistryCredentialsAreUsernameAndPasswordAlone(t *testing.T) {
	t.Parallel()
	credentials, err := parseRegistryCredentials([]byte(`{"username":"ptah","password":"e2eRegistry1Q7"}`))
	if err != nil || credentials.Username != "ptah" || credentials.Password != "e2eRegistry1Q7" {
		t.Fatalf("parseRegistryCredentials() = %+v, %v", credentials, err)
	}
	for _, document := range []string{
		`{"username":"ptah"}`,
		`{"username":"ptah","password":"x","registry":"r"}`,
		`{"username":"pt ah","password":"x"}`,
		`{"username":"ptah","password":""}`,
		`{"username":"ptah","password":"line\nbreak"}`,
		`{"username":"ptah","password":7}`,
		`["ptah","x"]`,
	} {
		if _, err := parseRegistryCredentials([]byte(document)); err == nil {
			t.Errorf("%s was accepted", document)
		}
	}
}

func TestExternalPostgresCredentialsAreBoundToTheirService(t *testing.T) {
	t.Parallel()
	valid := `{"database":"ext","password":"pw","url":"postgres://login:pw@external-pg.ptah-e2e.svc.cluster.local:5432/ext?sslmode=disable","username":"login"}`
	credentials, err := parseExternalPostgresCredentials([]byte(valid), "external-pg", "ptah-e2e")
	if err != nil || credentials.Username != "login" || credentials.Database != "ext" {
		t.Fatalf("parseExternalPostgresCredentials() = %+v, %v", credentials, err)
	}
	for name, document := range map[string]string{
		"another Service":  strings.Replace(valid, "external-pg.", "other.", 1),
		"another database": strings.Replace(valid, "/ext?", "/other?", 1),
		"TLS required":     strings.Replace(valid, "sslmode=disable", "sslmode=require", 1),
		"extra key":        strings.Replace(valid, `"username"`, `"host":"x","username"`, 1),
		"password with @":  strings.ReplaceAll(valid, "pw", "p@w"),
	} {
		if _, err := parseExternalPostgresCredentials([]byte(document), "external-pg", "ptah-e2e"); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func ledgerJob(uid, name, schema, operation string, created time.Time) batchv1.Job {
	return batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		UID: types.UID(uid), Name: name, CreationTimestamp: metav1.NewTime(created),
		Labels: map[string]string{labelSchema: schema, labelOperation: operation},
	}}
}

// A Job is recorded once, by UID, in the order the phase first saw it, and a
// checkpoint taken after names it.
func TestJobLedgerRecordsEachJobOnce(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	ledger := newJobLedger()
	records, err := observedJobRecords([]batchv1.Job{
		ledgerJob("uid-1", "resolve-1", "schema", "resolve", created),
		ledgerJob("uid-2", "verify-1", "schema", "verify", created.Add(time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		ledger.add(records)
	}
	if len(ledger.records) != 2 || ledger.records[0].UID != "uid-1" || ledger.records[1].UID != "uid-2" {
		t.Fatalf("ledger holds %v", ledger.records)
	}
	if want := (checkpoint{"uid-1", "uid-2"}); !slices.Equal(ledger.checkpoint("schema", ""), want) {
		t.Fatalf("checkpoint = %v, want %v", ledger.checkpoint("schema", ""), want)
	}
	if got := ledger.checkpoint("schema", "verify"); !slices.Equal(got, checkpoint{"uid-2"}) {
		t.Fatalf("verify checkpoint = %v", got)
	}
}

func TestObservedJobRecordsRefuseAJobWithoutIdentity(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for name, candidate := range map[string]batchv1.Job{
		"no UID":      ledgerJob("", "name", "schema", "plan", created),
		"no name":     ledgerJob("uid", "", "schema", "plan", created),
		"no creation": ledgerJob("uid", "name", "schema", "plan", time.Time{}),
	} {
		if _, err := observedJobRecords([]batchv1.Job{candidate}); err == nil {
			t.Errorf("a Job with %s was recorded", name)
		}
	}
	// A Job of no schema is still recorded: the ledger counts Jobs, and the
	// selection by schema is the counter's.
	if records, err := observedJobRecords([]batchv1.Job{ledgerJob("uid", "name", "", "", created)}); err != nil || len(records) != 1 {
		t.Fatalf("an unlabeled Job = %v, %v", records, err)
	}
}

func TestCheckpointsCountWhatAppearedBetweenThem(t *testing.T) {
	t.Parallel()
	ledger := &jobLedger{seen: map[string]bool{}}
	add := func(uid, schema, operation string) {
		ledger.records = append(ledger.records, observedJob{UID: uid, Name: uid, Schema: schema, Operation: operation})
		ledger.seen[uid] = true
	}
	add("a", "one", "resolve")
	add("b", "two", "resolve")
	before := ledger.checkpoint("one", "")
	add("c", "one", "resolve")
	add("d", "one", "plan")
	after := ledger.checkpoint("one", "")
	add("e", "one", "resolve")

	if got := ledger.between("one", "resolve", before, after); len(got) != 1 || got[0].UID != "c" {
		t.Fatalf("between = %v, want c alone", got)
	}
	if got := ledger.between("one", "", before, after); len(got) != 2 {
		t.Fatalf("between every operation = %v, want c and d", got)
	}
	if got := ledger.since("one", "resolve", before); len(got) != 2 {
		t.Fatalf("since = %v, want c and e", got)
	}
	if got := ledger.since("two", "resolve", checkpoint{}); len(got) != 1 {
		t.Fatalf("another schema's Jobs = %v", got)
	}
	// An empty checkpoint taken after nothing ran admits nothing between it
	// and itself.
	empty := ledger.checkpoint("three", "")
	if got := ledger.between("one", "", empty, empty); len(got) != 0 {
		t.Fatalf("between two empty checkpoints = %v", got)
	}
	if !slices.IsSorted(after) || !after.holds("d") || after.holds("e") {
		t.Fatalf("checkpoint = %v", after)
	}
}

// The automatic lifecycle holds a suspended schema to the seven Jobs it read
// the results of. A Job that appears after the capture, or a capture of the
// wrong size, is work the proof did not account for.
func TestJobBoundaryUnchangedRefusesALaterJob(t *testing.T) {
	t.Parallel()
	records := []observedJob{{UID: "b"}, {UID: "a"}, {UID: "c"}}
	actual, err := jobBoundaryUnchanged(records, []string{"a", "b", "c"}, 3)
	if err != nil || !slices.Equal(actual, []string{"a", "b", "c"}) {
		t.Fatalf("an unchanged boundary = %v, %v", actual, err)
	}
	for name, test := range map[string]struct {
		records  []observedJob
		expected []string
		count    int
	}{
		"a Job after the capture":   {append(slices.Clone(records), observedJob{UID: "d"}), []string{"a", "b", "c"}, 3},
		"a captured Job missing":    {records[:2], []string{"a", "b", "c"}, 3},
		"another Job in its place":  {[]observedJob{{UID: "a"}, {UID: "b"}, {UID: "x"}}, []string{"a", "b", "c"}, 3},
		"a capture of another size": {records, []string{"a", "b", "c"}, 7},
	} {
		if _, err := jobBoundaryUnchanged(test.records, test.expected, test.count); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestUIDLedgerAppendsOnce(t *testing.T) {
	t.Parallel()
	ledger := newUIDLedger()
	for _, uid := range []string{"a", "b", "a", ""} {
		ledger.add(uid)
	}
	if !slices.Equal(ledger.uids, []string{"a", "b"}) || !ledger.holds("a") || ledger.holds("") || ledger.holds("c") {
		t.Fatalf("ledger = %v", ledger.uids)
	}
}
