package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/stokaro/ptah-operator/internal/certrotation"
)

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
	if logger == nil {
		return errors.New("logger is required")
	}
	config, supervisorConfig, healthBindAddress, err := parseFlags(args)
	if err != nil {
		return err
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
	config.Logger = logger
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
	return runService(ctx, serviceRuntimeConfig{
		HealthBindAddress: healthBindAddress,
		HealthHandler:     probes.handler(),
		Supervisor:        supervisor,
	})
}

func parseFlags(args []string) (certrotation.Config, supervisorConfig, string, error) {
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
	flags.StringVar(&config.ReleaseName, "release-name", "", "owning Helm release name used for exact Secret metadata")
	flags.StringVar(&config.SecretName, "secret-name", "", "exact generated TLS Secret name")
	flags.StringVar(&config.StagingSecretName, "staging-secret-name", "", "exact precreated Secret for durable pending certificate material")
	flags.BoolVar(&config.RecreateMissingSecret, "recreate-missing-secret", false, "allow guarded recreation of a deleted generated TLS Secret")
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
	flags.DurationVar(&config.CASwitchDelay, "ca-switch-delay", 0, "how long every webhook entry trusts both CAs before the serving certificate moves to the new one; zero means the run interval")
	flags.DurationVar(&config.ProbeTimeout, "probe-timeout", 5*time.Minute, "maximum time to wait for every webhook endpoint to serve a replacement certificate")
	flags.DurationVar(&config.ProbeInterval, "probe-interval", 2*time.Second, "interval between serving-certificate probes")
	flags.DurationVar(&config.LeaseDuration, "lease-duration", 10*time.Minute, "certificate rotation Lease duration")
	flags.DurationVar(&config.AcquireTimeout, "lease-acquire-timeout", 30*time.Second, "maximum time to acquire the certificate rotation Lease")
	if err := flags.Parse(args); err != nil {
		return certrotation.Config{}, supervisorConfig{}, "", err
	}
	if flags.NArg() != 0 {
		return certrotation.Config{}, supervisorConfig{}, "", fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	config.MutatingWebhookNames = splitNames(mutatingWebhookNames)
	config.ValidatingWebhookNames = splitNames(validatingWebhookNames)
	if config.CASwitchDelay == 0 {
		config.CASwitchDelay = runInterval
	}
	supervisor := supervisorConfig{
		RunInterval:      runInterval,
		OperationTimeout: operationTimeout,
		RetryInitial:     retryInitial,
		RetryMax:         retryMax,
	}
	if err := supervisor.validate(); err != nil {
		return certrotation.Config{}, supervisorConfig{}, "", err
	}
	if err := validateRuntimeRelationships(supervisor, config); err != nil {
		return certrotation.Config{}, supervisorConfig{}, "", err
	}
	return config, supervisor, healthBindAddress, nil
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
