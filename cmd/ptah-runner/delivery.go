package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// The workload projects these from the actual Pod, never from a claim label.
const (
	resultPodNamespace = "PTAH_RESULT_POD_NAMESPACE"
	resultPodName      = "PTAH_RESULT_POD_NAME"
	resultPodUID       = "PTAH_RESULT_POD_UID"
)

var deliveryRetry = resultdelivery.RetryPolicy{Attempts: 4, Interval: time.Second, AttemptTimeout: 30 * time.Second, TotalTimeout: 2 * time.Minute}

type runnerDelivery struct {
	sender   *resultdelivery.Sender
	identity resultdelivery.Identity
}

// Prepare the complete transport before any child starts. Credentials are read
// once from bounded regular files (Kubernetes projections may use symlinks).
// Failures contain neither paths nor certificate material.
func prepareDelivery(endpoint, directory string, operation runner.Operation, environment []string) (*runnerDelivery, error) {
	invalid := errors.New("invalid result delivery configuration")
	if endpoint == "" || directory == "" || !filepath.IsAbs(directory) {
		return nil, invalid
	}
	read := func(name string) ([]byte, error) {
		path := filepath.Join(directory, name)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
			return nil, invalid
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, invalid
		}
		defer f.Close()
		stat, err := f.Stat()
		if err != nil || !stat.Mode().IsRegular() || stat.Size() > 64<<10 {
			return nil, invalid
		}
		b, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
		if err != nil || len(b) == 0 || len(b) > 64<<10 {
			return nil, invalid
		}
		return b, nil
	}
	certPEM, err := read("tls.crt")
	if err != nil {
		return nil, invalid
	}
	keyPEM, err := read("tls.key")
	if err != nil {
		return nil, invalid
	}
	caPEM, err := read("ca.crt")
	if err != nil {
		return nil, invalid
	}
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, invalid
	}
	identity, err := resultdelivery.ClientIdentity(certificate)
	if err != nil {
		return nil, invalid
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, invalid
	}
	// Duplicate authority variables are ambiguous and must not choose a binding
	// differently from the runner or child. Non-authority environment is untouched.
	wanted := map[string]string{runner.EnvOperationID: identity.Binding.OperationID, resultPodNamespace: identity.Binding.Namespace, resultPodName: identity.Binding.PodName, resultPodUID: string(identity.Binding.PodUID)}
	if identity.Engine != "" {
		wanted[runner.EnvExpectedDatabaseEngine] = identity.Engine
	}
	seen := map[string]bool{}
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		expected, relevant := wanted[key]
		if !ok || !relevant {
			continue
		}
		if key == runner.EnvExpectedDatabaseEngine {
			value = strings.ToLower(value)
		}
		if seen[key] || value != expected {
			return nil, invalid
		}
		seen[key] = true
	}
	if len(seen) != len(wanted) || identity.Binding.Operation != string(operation) {
		return nil, invalid
	}
	sender, err := resultdelivery.NewSender(endpoint, identity, &tls.Config{Certificates: []tls.Certificate{certificate}, RootCAs: roots}, deliveryRetry)
	if err != nil {
		return nil, invalid
	}
	return &runnerDelivery{sender: sender, identity: identity}, nil
}

// deliver receives only an already completed result. It cannot execute a child,
// and neither success nor failure writes a correctness payload to stdout.
func (d *runnerDelivery) deliver(ctx context.Context, result runner.Result, stderr io.Writer, terminationLog string) int {
	payload, err := resultdelivery.Encode(d.identity, result)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "ptah-runner: could not encode the durable result")
		return 2
	}
	summary, err := runner.EncodeSummary(result)
	writeSummary(summary, err, terminationLog, stderr)
	// An execution deadline or SIGTERM can stop SQL before we report that
	// outcome. Sender supplies a separate bounded delivery deadline. Kubelet's
	// termination grace may still kill the process; that remains outcome-unknown.
	if _, err := d.sender.Send(context.WithoutCancel(ctx), payload); err != nil {
		_, _ = fmt.Fprintln(stderr, "ptah-runner: durable result delivery failed")
		return 2
	}
	return 0
}

func writeSummary(summary []byte, err error, path string, stderr io.Writer) {
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "ptah-runner: the result has no termination summary")
		return
	}
	if path != "" {
		if err := runner.WriteTerminationSummary(path, summary); err != nil {
			_, _ = fmt.Fprintln(stderr, "ptah-runner: could not write the termination summary")
		}
	}
}
