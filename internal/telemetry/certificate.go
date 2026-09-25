package telemetry

import (
	"crypto/x509"
	"encoding/pem"
	"os"

	"github.com/prometheus/client_golang/prometheus"
)

// CertificateCollector publishes when the serving certificate this process
// answers admission with stops being valid.
//
// Every replica serves admission, so every replica publishes the certificate
// it would present, and an alert takes the earliest. The certificate rotator
// replaces the Secret long before this instant; a value that keeps falling
// towards now is a rotation that is not happening, which is what the alert on
// it is for. It is read from the file on every scrape rather than cached,
// because the file is what the webhook server reloads.
type CertificateCollector struct {
	path   string
	expiry *prometheus.Desc
	failed prometheus.Counter
}

// NewCertificateCollector registers the gauge for the PEM certificate at path.
func NewCertificateCollector(registerer prometheus.Registerer, path string) *CertificateCollector {
	collector := &CertificateCollector{
		path: path,
		expiry: prometheus.NewDesc(
			"ptah_operator_webhook_certificate_expiry_timestamp_seconds",
			"Unix time at which the admission serving certificate this process presents stops being valid.",
			nil, nil),
		failed: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "ptah_operator",
			Name:      "webhook_certificate_read_failures_total",
			Help:      "Scrapes that could not read the admission serving certificate.",
		}),
	}
	if registerer != nil {
		registerer.MustRegister(collector)
	}
	return collector
}

func (c *CertificateCollector) Describe(into chan<- *prometheus.Desc) {
	into <- c.expiry
	c.failed.Describe(into)
}

// Collect publishes the expiry, or nothing and a counted failure when the file
// cannot be read: an absent expiry is not a certificate that never expires.
//
// The failure counter is emitted from here, after the read, so the scrape
// that failed is the one that reports it. Registered on its own, the registry
// could gather it before this ran and publish the count from before.
func (c *CertificateCollector) Collect(into chan<- prometheus.Metric) {
	defer func() { into <- c.failed }()
	content, err := os.ReadFile(c.path)
	if err != nil {
		c.failed.Inc()
		return
	}
	block, _ := pem.Decode(content)
	if block == nil || block.Type != "CERTIFICATE" {
		c.failed.Inc()
		return
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		c.failed.Inc()
		return
	}
	into <- prometheus.MustNewConstMetric(c.expiry, prometheus.GaugeValue, float64(certificate.NotAfter.Unix()))
}
