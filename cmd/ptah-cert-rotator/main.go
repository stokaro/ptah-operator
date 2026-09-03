package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/stokaro/ptah-operator/internal/certrotation"
)

const healthServerShutdownTimeout = 5 * time.Second

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], logger); err != nil {
		logger.Error("certificate rotation failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, logger *slog.Logger) error {
	flags := flag.NewFlagSet("ptah-cert-rotator", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var config certrotation.Config
	var mutatingWebhookNames string
	var validatingWebhookNames string
	var runInterval time.Duration
	var operationTimeout time.Duration
	var retryInitial time.Duration
	var retryMax time.Duration
	var healthBindAddress string
	flags.StringVar(&config.Namespace, "namespace", "", "namespace containing the generated TLS Secret and Lease")
	flags.StringVar(&config.SecretName, "secret-name", "", "exact generated TLS Secret name")
	flags.BoolVar(&config.RecreateMissingSecret, "recreate-missing-secret", false, "allow guarded recreation of a deleted generated TLS Secret")
	flags.StringVar(&config.SecretCreatePolicyName, "secret-create-policy-name", "", "exact ValidatingAdmissionPolicy guarding generated TLS Secret recreation")
	flags.StringVar(&config.SecretCreatePolicyBindingName, "secret-create-policy-binding-name", "", "exact ValidatingAdmissionPolicyBinding guarding generated TLS Secret recreation")
	flags.StringVar(&config.SecretCreateServiceAccountName, "secret-create-service-account-name", "", "exact ServiceAccount subject guarded for generated TLS Secret recreation")
	flags.StringVar(&config.LeaseName, "lease-name", "", "exact certificate rotation Lease name")
	flags.StringVar(&config.MutatingWebhookConfiguration, "mutating-webhook-configuration", "", "exact MutatingWebhookConfiguration name")
	flags.StringVar(&mutatingWebhookNames, "mutating-webhook-names", "", "required comma-separated mutating webhook anchors for the exact Service")
	flags.StringVar(&config.ValidatingWebhookConfiguration, "validating-webhook-configuration", "", "exact ValidatingWebhookConfiguration name")
	flags.StringVar(&validatingWebhookNames, "validating-webhook-names", "", "required comma-separated validating webhook anchors for the exact Service")
	flags.StringVar(&config.ServiceName, "service-name", "", "webhook Service name")
	flags.StringVar(&config.ServiceNamespace, "service-namespace", "", "webhook Service namespace")
	flags.StringVar(&config.EndpointPortName, "endpoint-port-name", "https", "EndpointSlice port name used for direct Pod probes")
	flags.StringVar(&config.HolderIdentity, "holder-identity", "", "unique Pod identity used to hold the rotation Lease")
	flags.DurationVar(&runInterval, "run-interval", 6*time.Hour, "interval between successful certificate reconciliations")
	flags.DurationVar(&operationTimeout, "operation-timeout", 15*time.Minute, "maximum duration of one certificate reconciliation")
	flags.DurationVar(&retryInitial, "retry-initial", 5*time.Second, "initial retry delay after a failed reconciliation")
	flags.DurationVar(&retryMax, "retry-max", 5*time.Minute, "maximum retry delay after consecutive failed reconciliations")
	flags.StringVar(&healthBindAddress, "health-bind-address", ":8081", "address for healthz and readyz probes")
	flags.DurationVar(&config.RenewalThreshold, "renewal-threshold", 720*time.Hour, "rotate certificates with no more than this validity remaining")
	flags.DurationVar(&config.ServingCertificateValidity, "serving-certificate-validity", 2160*time.Hour, "validity of newly issued serving certificates")
	flags.DurationVar(&config.CACertificateValidity, "ca-certificate-validity", 26280*time.Hour, "validity of newly issued CA certificates")
	flags.DurationVar(&config.ProbeTimeout, "probe-timeout", 5*time.Minute, "maximum time to wait for every webhook endpoint to serve a replacement certificate")
	flags.DurationVar(&config.ProbeInterval, "probe-interval", 2*time.Second, "interval between serving-certificate probes")
	flags.DurationVar(&config.LeaseDuration, "lease-duration", 10*time.Minute, "certificate rotation Lease duration")
	flags.DurationVar(&config.AcquireTimeout, "lease-acquire-timeout", 30*time.Second, "maximum time to acquire the certificate rotation Lease")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	config.MutatingWebhookNames = splitNames(mutatingWebhookNames)
	config.ValidatingWebhookNames = splitNames(validatingWebhookNames)
	if logger == nil {
		return errors.New("logger is required")
	}
	supervisorConfig := supervisorConfig{
		RunInterval:      runInterval,
		OperationTimeout: operationTimeout,
		RetryInitial:     retryInitial,
		RetryMax:         retryMax,
	}
	if err := supervisorConfig.validate(); err != nil {
		return err
	}
	if err := validateRuntimeRelationships(supervisorConfig, config); err != nil {
		return err
	}
	if strings.TrimSpace(healthBindAddress) == "" {
		return errors.New("health bind address is required")
	}

	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("load in-cluster Kubernetes configuration: %w", err)
	}
	restConfig.UserAgent = "ptah-cert-rotator"
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}
	rotator, err := certrotation.New(client, config)
	if err != nil {
		return fmt.Errorf("validate certificate rotation configuration: %w", err)
	}
	probes := &probeState{}
	supervisor := newSupervisor(
		rotator,
		supervisorConfig,
		probes,
		logger.With("secret", config.SecretName, "namespace", config.Namespace),
	)
	return runService(ctx, healthBindAddress, probes.handler(), supervisor)
}

func runService(ctx context.Context, healthBindAddress string, handler http.Handler, supervisor *rotationSupervisor) error {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", healthBindAddress)
	if err != nil {
		return fmt.Errorf("listen for health probes: %w", err)
	}

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	serviceCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	serverResult := make(chan error, 1)
	go func() {
		serverResult <- server.Serve(listener)
	}()
	supervisorResult := make(chan error, 1)
	go func() {
		supervisorResult <- supervisor.Run(serviceCtx)
	}()

	select {
	case supervisorErr := <-supervisorResult:
		cancel()
		serverErr, shutdownErr := stopHealthServer(server, serverResult, healthServerShutdownTimeout)
		if supervisorErr != nil {
			if shutdownErr != nil {
				return errors.Join(supervisorErr, fmt.Errorf("shut down health server: %w", shutdownErr))
			}
			return supervisorErr
		}
		if shutdownErr != nil {
			return fmt.Errorf("shut down health server: %w", shutdownErr)
		}
		if serverErr != nil && !errors.Is(serverErr, http.ErrServerClosed) {
			return fmt.Errorf("serve health probes: %w", serverErr)
		}
		return nil
	case serverErr := <-serverResult:
		cancel()
		supervisorErr := <-supervisorResult
		if supervisorErr != nil {
			return supervisorErr
		}
		if serverErr != nil && !errors.Is(serverErr, http.ErrServerClosed) {
			return fmt.Errorf("serve health probes: %w", serverErr)
		}
		if ctx.Err() != nil {
			return nil
		}
		return errors.New("health server stopped unexpectedly")
	}
}

type healthServerShutdown interface {
	Shutdown(context.Context) error
	Close() error
}

func stopHealthServer(
	server healthServerShutdown,
	serverResult <-chan error,
	timeout time.Duration,
) (serverErr, shutdownErr error) {
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), timeout)
	shutdownErr = server.Shutdown(shutdownCtx)
	shutdownCancel()
	closed := false
	if shutdownErr != nil {
		shutdownErr = errors.Join(shutdownErr, server.Close())
		closed = true
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case serverErr = <-serverResult:
		return serverErr, shutdownErr
	case <-timer.C:
		if !closed {
			shutdownErr = errors.Join(shutdownErr, server.Close())
		}
		shutdownErr = errors.Join(shutdownErr, errors.New("timed out waiting for health server to stop"))
		return nil, shutdownErr
	}
}

func splitNames(value string) []string {
	if value == "" {
		return nil
	}
	names := strings.Split(value, ",")
	for i := range names {
		names[i] = strings.TrimSpace(names[i])
	}
	return names
}
