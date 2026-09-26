package telemetry_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/telemetry"
)

// stateView is a view whose resources a test controls.
type stateView struct {
	synced     bool
	schemas    []operatorv1alpha1.PtahSchema
	migrations []operatorv1alpha1.PtahMigration
	err        error
}

func (v stateView) Synced() bool { return v.synced }

func (v stateView) ListSchemas(context.Context) ([]operatorv1alpha1.PtahSchema, error) {
	return v.schemas, v.err
}

func (v stateView) ListMigrations(context.Context) ([]operatorv1alpha1.PtahMigration, error) {
	return v.migrations, v.err
}

func gatherRegistry(t *testing.T, register func(*prometheus.Registry)) string {
	t.Helper()
	registry := prometheus.NewRegistry()
	register(registry)
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var out strings.Builder
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			out.WriteString(family.GetName())
			for _, label := range metric.GetLabel() {
				out.WriteString("{" + label.GetName() + "=" + label.GetValue() + "}")
			}
			switch {
			case metric.GetGauge() != nil:
				out.WriteString(" " + formatValue(metric.GetGauge().GetValue()))
			case metric.GetCounter() != nil:
				out.WriteString(" " + formatValue(metric.GetCounter().GetValue()))
			}
			out.WriteString("\n")
		}
	}
	return out.String()
}

func gatherState(t *testing.T, view telemetry.StateView) string {
	t.Helper()
	return gatherRegistry(t, func(registry *prometheus.Registry) {
		telemetry.NewStateCollector(registry, view, func() time.Time { return collectorNow })
	})
}

// minutesBefore dates a fixture distinctly from the collector's clock, so a
// comparison between the two instants measures something.
func minutesBefore(minutes int) *metav1.Time {
	at := metav1.NewTime(collectorNow.Add(-time.Duration(minutes) * time.Minute))
	return &at
}

// A view that has not caught up, or one whose read failed, publishes no state
// at all: an empty list would read as a fleet with nothing overdue.
func TestTheStateGaugesAreAbsentUntilTheViewCanBeRead(t *testing.T) {
	t.Parallel()
	for name, view := range map[string]stateView{
		"unsynchronized": {synced: false, schemas: []operatorv1alpha1.PtahSchema{{}}},
		"read failed":    {synced: true, err: errors.New("boom")},
	} {
		if out := gatherState(t, view); out != "" {
			t.Errorf("%s: the scrape published state:\n%s", name, out)
		}
	}
}

func TestTheStateGaugesReportTheFleetAsItIs(t *testing.T) {
	t.Parallel()
	schemas := []operatorv1alpha1.PtahSchema{
		// In sync and due in the future: counted, not overdue.
		{Status: operatorv1alpha1.PtahSchemaStatus{Phase: "InSync", NextReconciliationTime: minutesBefore(-5)}},
		// Seven minutes past its own next reconciliation, with an Observe in
		// flight for twelve.
		{Status: operatorv1alpha1.PtahSchemaStatus{
			Phase: "Observing", NextReconciliationTime: minutesBefore(7),
			ActiveOperation: &operatorv1alpha1.ActiveOperationStatus{Type: "Observe", StartedAt: *minutesBefore(12)},
		}},
		// Suspended: past its time, and not overdue, because nothing is due.
		{
			Spec:   operatorv1alpha1.PtahSchemaSpec{Suspend: true},
			Status: operatorv1alpha1.PtahSchemaStatus{Phase: "Suspended", NextReconciliationTime: minutesBefore(60)},
		},
		// Never reconciled: no phase yet.
		{},
		// A Resolve that failed an hour ago and waits for its retry. The claim
		// stays in status and is not an operation in flight.
		{Status: operatorv1alpha1.PtahSchemaStatus{
			Phase:           "Failed",
			ActiveOperation: &operatorv1alpha1.ActiveOperationStatus{Type: "Resolve", StartedAt: *minutesBefore(60)},
		}},
	}
	migrations := []operatorv1alpha1.PtahMigration{
		{Status: operatorv1alpha1.PtahMigrationStatus{
			Phase:              "Applying",
			ActiveOperation:    &operatorv1alpha1.MigrationOperationStatus{Type: "Apply", StartedAt: *minutesBefore(3)},
			PendingLockRelease: &operatorv1alpha1.TargetLockReleaseStatus{OperationID: "released-later"},
		}},
		{Status: operatorv1alpha1.PtahMigrationStatus{
			Phase:           "Applying",
			ActiveOperation: &operatorv1alpha1.MigrationOperationStatus{Type: "Apply", StartedAt: *minutesBefore(9)},
		}},
	}
	out := gatherState(t, stateView{synced: true, schemas: schemas, migrations: migrations})
	for _, want := range []string{
		"ptah_operator_resources{family=schema}{phase=InSync} 1",
		"ptah_operator_resources{family=schema}{phase=Observing} 1",
		"ptah_operator_resources{family=schema}{phase=Suspended} 1",
		"ptah_operator_resources{family=schema}{phase=Unset} 1",
		"ptah_operator_resources{family=schema}{phase=Failed} 1",
		"ptah_operator_resources{family=migration}{phase=Applying} 2",
		"ptah_operator_overdue_resources{family=schema} 1",
		"ptah_operator_overdue_seconds{family=schema} 420",
		"ptah_operator_overdue_resources{family=migration} 0",
		"ptah_operator_active_operations{family=schema}{operation=Observe} 1",
		"ptah_operator_active_operation_seconds{family=schema}{operation=Observe} 720",
		"ptah_operator_active_operations{family=migration}{operation=Apply} 2",
		"ptah_operator_active_operation_seconds{family=migration}{operation=Apply} 540",
		"ptah_operator_pending_lock_releases{family=migration} 1",
		"ptah_operator_pending_lock_releases{family=schema} 0",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("the scrape does not carry %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "{operation=Resolve}") {
		t.Errorf("a failed attempt waiting for its retry was published as in flight:\n%s", out)
	}
	// No overdue migration, so no age for one: zero would read as "one is due
	// this instant".
	if strings.Contains(out, "ptah_operator_overdue_seconds{family=migration}") {
		t.Errorf("an age was published for a family with nothing overdue:\n%s", out)
	}
}

func TestTheCertificateExpiryIsTheCertificatesOwn(t *testing.T) {
	t.Parallel()
	notAfter := time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "tls.crt")
	writeCertificate(t, path, notAfter)
	out := gatherRegistry(t, func(registry *prometheus.Registry) {
		telemetry.NewCertificateCollector(registry, path)
	})
	if want := "ptah_operator_webhook_certificate_expiry_timestamp_seconds " + formatValue(float64(notAfter.Unix())); !strings.Contains(out, want) {
		t.Fatalf("the scrape does not carry %q:\n%s", want, out)
	}

	missing := gatherRegistry(t, func(registry *prometheus.Registry) {
		telemetry.NewCertificateCollector(registry, filepath.Join(t.TempDir(), "absent.crt"))
	})
	if strings.Contains(missing, "expiry_timestamp_seconds") ||
		!strings.Contains(missing, "ptah_operator_webhook_certificate_read_failures_total 1") {
		t.Fatalf("an unreadable certificate was not reported as a failure:\n%s", missing)
	}
}

func writeCertificate(t *testing.T, path string, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "webhook"},
		NotBefore:    notAfter.Add(-24 * time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}
