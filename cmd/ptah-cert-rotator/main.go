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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/stokaro/ptah-operator/internal/certrotation"
	"github.com/stokaro/ptah-operator/internal/kubeapi"
)

const (
	defaultCandidateStabilityDuration = 10 * time.Second
	defaultCandidatePollInterval      = time.Second
	defaultCandidateRequestTimeout    = 5 * time.Second
	admissionCanaryDirectRequestBurst = 3
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
	var candidateBindAddress string
	var candidateProbeConfigMapName string
	var candidateProbeUsername string
	var candidateMutatingFieldManager string
	var candidateValidatingFieldManager string
	var candidateStabilityDuration time.Duration
	var candidatePollInterval time.Duration
	var candidateRequestTimeout time.Duration
	flags.StringVar(&config.Namespace, "namespace", "", "namespace containing the generated TLS Secret and Lease")
	flags.StringVar(&config.ReleaseName, "release-name", "", "owning Helm release name used for exact Secret metadata")
	flags.StringVar(&config.SecretName, "secret-name", "", "exact generated TLS Secret name")
	flags.StringVar(&config.StagingSecretName, "staging-secret-name", "", "exact precreated Secret for durable pending certificate material")
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
	flags.StringVar(&config.CandidateServiceName, "candidate-service-name", "", "candidate certificate listener Service name")
	flags.StringVar(&config.EndpointPortName, "endpoint-port-name", "https", "EndpointSlice port name used for direct Pod probes")
	flags.StringVar(&config.HolderIdentity, "holder-identity", "", "unique Pod identity used to hold the rotation Lease")
	flags.DurationVar(&runInterval, "run-interval", 6*time.Hour, "interval between successful certificate reconciliations")
	flags.DurationVar(&operationTimeout, "operation-timeout", 15*time.Minute, "maximum duration of one certificate reconciliation")
	flags.DurationVar(&retryInitial, "retry-initial", 5*time.Second, "initial retry delay after a failed reconciliation")
	flags.DurationVar(&retryMax, "retry-max", 5*time.Minute, "maximum retry delay after consecutive failed reconciliations")
	flags.StringVar(&healthBindAddress, "health-bind-address", ":8081", "address for healthz and readyz probes")
	flags.StringVar(&candidateBindAddress, "candidate-bind-address", ":9444", "address for candidate admission TLS requests")
	flags.StringVar(&candidateProbeConfigMapName, "candidate-probe-config-map-name", "", "exact immutable ConfigMap used for candidate admission probes")
	flags.StringVar(&candidateProbeUsername, "candidate-probe-username", "", "exact certificate rotator ServiceAccount username used for candidate admission probes")
	flags.StringVar(&candidateMutatingFieldManager, "candidate-mutating-field-manager", "", "exact field manager accepted by the mutating candidate admission path")
	flags.StringVar(&candidateValidatingFieldManager, "candidate-validating-field-manager", "", "exact field manager accepted by the validating candidate admission path")
	flags.DurationVar(&candidateStabilityDuration, "candidate-stability-duration", defaultCandidateStabilityDuration, "continuous all-API-server stability window for candidate admission proof")
	flags.DurationVar(&candidatePollInterval, "candidate-poll-interval", defaultCandidatePollInterval, "poll interval for candidate admission convergence")
	flags.DurationVar(&candidateRequestTimeout, "candidate-request-timeout", defaultCandidateRequestTimeout, "timeout for one complete direct candidate admission API-server endpoint observation")
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
	if err := validateCandidateRuntimeRelationships(
		operationTimeout,
		config.AcquireTimeout,
		config.ProbeTimeout,
		candidateStabilityDuration,
		candidatePollInterval,
		candidateRequestTimeout,
	); err != nil {
		return err
	}
	candidateHandler, err := newCandidateAdmissionHandler(candidateAdmissionConfig{
		ReleaseName:            config.ReleaseName,
		Namespace:              config.Namespace,
		ConfigMapName:          candidateProbeConfigMapName,
		Username:               candidateProbeUsername,
		MutatingFieldManager:   candidateMutatingFieldManager,
		ValidatingFieldManager: candidateValidatingFieldManager,
	})
	if err != nil {
		return fmt.Errorf("validate candidate admission configuration: %w", err)
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
	apiServerProvider, err := kubeapi.NewDefaultServiceProvider(
		restConfig,
		client.DiscoveryV1().EndpointSlices(metav1.NamespaceDefault),
		admissionCanaryDirectRequestBurst,
	)
	if err != nil {
		return fmt.Errorf("create direct Kubernetes API-server provider: %w", err)
	}
	serviceAccountName := strings.TrimPrefix(
		candidateProbeUsername,
		"system:serviceaccount:"+config.Namespace+":",
	)
	canary, err := certrotation.NewAdmissionCanary(client, apiServerProvider, certrotation.AdmissionCanaryConfig{
		ReleaseName:                    config.ReleaseName,
		MarkerNamespace:                config.Namespace,
		MarkerName:                     candidateProbeConfigMapName,
		ServiceAccountName:             serviceAccountName,
		MutatingWebhookConfiguration:   config.MutatingWebhookConfiguration,
		MutatingWebhookNames:           config.MutatingWebhookNames,
		ValidatingWebhookConfiguration: config.ValidatingWebhookConfiguration,
		ValidatingWebhookNames:         config.ValidatingWebhookNames,
		PrimaryServiceName:             config.ServiceName,
		CandidateServiceName:           config.CandidateServiceName,
		ServiceNamespace:               config.ServiceNamespace,
		StabilityDuration:              candidateStabilityDuration,
		PollEvery:                      candidatePollInterval,
		RequestTimeout:                 candidateRequestTimeout,
	})
	if err != nil {
		return fmt.Errorf("validate candidate admission canary: %w", err)
	}
	candidateCertificates := &candidateCertificateStore{}
	rotator, err := certrotation.New(client, config, candidateCertificates, canary)
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
		HealthBindAddress:    healthBindAddress,
		HealthHandler:        probes.handler(),
		CandidateBindAddress: candidateBindAddress,
		CandidateHandler:     candidateHandler,
		CandidateTLSConfig:   candidateCertificates.tlsConfig(),
		Supervisor:           supervisor,
	})
}

func validateCandidateRuntimeRelationships(
	operationTimeout time.Duration,
	acquireTimeout time.Duration,
	probeTimeout time.Duration,
	stabilityDuration time.Duration,
	pollInterval time.Duration,
	requestTimeout time.Duration,
) error {
	if operationTimeout <= 0 || acquireTimeout <= 0 || probeTimeout <= 0 ||
		stabilityDuration <= 0 || pollInterval <= 0 || requestTimeout <= 0 {
		return errors.New("candidate admission convergence timing values must be positive")
	}
	if requestTimeout >= operationTimeout {
		return errors.New("candidate admission request timeout must be shorter than the operation timeout")
	}
	barrierFloor, err := candidateAdmissionBarrierFloor(stabilityDuration, pollInterval)
	if err != nil {
		return err
	}
	minimum, err := sumCandidateOperationBudget(
		acquireTimeout,
		probeTimeout,
		probeTimeout,
		barrierFloor,
		barrierFloor,
		barrierFloor,
	)
	if err != nil {
		return err
	}
	if operationTimeout <= minimum {
		return fmt.Errorf(
			"operation timeout must exceed Lease acquisition, two endpoint probe windows, and three candidate admission stability barriers (%s)",
			minimum,
		)
	}
	return nil
}

func candidateAdmissionBarrierFloor(stabilityDuration, pollInterval time.Duration) (time.Duration, error) {
	polls := stabilityDuration / pollInterval
	if stabilityDuration%pollInterval != 0 {
		polls++
	}
	const maximumDuration = time.Duration(1<<63 - 1)
	if polls > maximumDuration/pollInterval {
		return 0, errors.New("candidate admission stability barrier exceeds the supported duration")
	}
	return polls * pollInterval, nil
}

func sumCandidateOperationBudget(parts ...time.Duration) (time.Duration, error) {
	const maximumDuration = time.Duration(1<<63 - 1)
	var total time.Duration
	for _, part := range parts {
		if part <= 0 || total > maximumDuration-part {
			return 0, errors.New("candidate admission operation budget exceeds the supported duration")
		}
		total += part
	}
	return total, nil
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
