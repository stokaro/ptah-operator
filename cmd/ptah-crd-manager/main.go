package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/stokaro/ptah-operator/internal/controllerstate"
	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

const (
	defaultTimeout = 2 * time.Minute
	supportedModes = "reconcile, verify, or runtime-verify"
	pollInterval   = 500 * time.Millisecond
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "ptah-crd-manager: %v\n", err)
		reportTerminationMessage(err)
		os.Exit(1)
	}
}

// terminationMessageFile is the file Kubernetes reads a failed container's
// message from. The hook container declares it and the policy that reads it.
var terminationMessageFile = "/dev/termination-log"

// terminationMessageLimit is what Kubernetes keeps of that file.
const terminationMessageLimit = 4096

// reportTerminationMessage puts the refusal where the person who ran Helm can
// read it. A hook that refuses prints its reason to stderr, which stays inside
// the Pod: Helm reports only that the Job failed, so an operator is told that
// an upgrade was refused and never why. Writing the same reason to the
// termination message carries it into the Job's status and out through Helm.
// It is best effort: a manager that cannot write the file has already said
// what happened on stderr, and failing here would replace a precise refusal
// with a write error.
func reportTerminationMessage(err error) {
	message := fmt.Sprintf("ptah-crd-manager: %v\n", err)
	if len(message) > terminationMessageLimit {
		message = message[:terminationMessageLimit]
	}
	file, openErr := os.OpenFile(terminationMessageFile, os.O_WRONLY|os.O_TRUNC, 0o600)
	if openErr != nil {
		return
	}
	defer func() { _ = file.Close() }()
	_, _ = file.WriteString(message)
}

// modeFlags are the flags each mode reads, and the only ones it accepts.
var modeFlags = map[string][]string{
	"reconcile": {
		"timeout",
		"release-name",
		"release-namespace",
		"controller-deployment-name",
		"certificate-deployment-name",
		"manager-image",
		"controller-state-version",
	},
	"verify": {"timeout"},
	"runtime-verify": {
		"timeout",
		"release-name",
		"release-namespace",
		"coordination-namespace",
		"leader-election",
		"leader-election-id",
		"webhook-service-name",
		"webhook-timeout-seconds",
		"hook-service-account-name",
		"controller-service-account-name",
		"controller-deployment-name",
		"certificate-deployment-name",
		"verify-controller-state",
		"require-distinct-approver",
	},
}

func run(parent context.Context, args []string, output io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("mode is required: %s", supportedModes)
	}
	mode := args[0]
	if _, known := modeFlags[mode]; !known {
		return fmt.Errorf("unsupported mode %q: use %s", mode, supportedModes)
	}
	flags := flag.NewFlagSet("ptah-crd-manager "+mode, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	timeout := flags.Duration("timeout", defaultTimeout, "maximum API reconciliation time")
	releaseName := flags.String("release-name", "", "owning Helm release name")
	releaseNamespace := flags.String("release-namespace", "", "owning Helm release namespace")
	coordinationNamespace := flags.String("coordination-namespace", "", "effective coordination namespace")
	leaderElection := flags.String("leader-election", "", "exact leader-election mode")
	leaderElectionID := flags.String("leader-election-id", "", "fixed leader-election ID")
	webhookServiceName := flags.String("webhook-service-name", "", "exact admission webhook Service name")
	webhookTimeoutSeconds := flags.Int("webhook-timeout-seconds", 0, "exact admission webhook timeout in seconds")
	hookServiceAccountName := flags.String("hook-service-account-name", "", "exact CRD hook ServiceAccount name")
	controllerServiceAccountName := flags.String("controller-service-account-name", "", "exact controller ServiceAccount name")
	controllerDeploymentName := flags.String("controller-deployment-name", "", "exact controller Deployment name")
	certificateDeploymentName := flags.String("certificate-deployment-name", "", "exact certificate-rotator Deployment name")
	managerImage := flags.String("manager-image", "", "exact manager image this release runs")
	controllerStateVersion := flags.Int64("controller-state-version", 0, "controller-state version the chart was published with")
	verifyControllerState := flags.Bool("verify-controller-state", false, "reject controller downgrades incompatible with stored PtahSchema state")
	requireDistinctApprover := flags.Bool("require-distinct-approver", false, "whether the fixed admission singleton is expected to carry the spec-writer webhook entries")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if *timeout <= 0 {
		return fmt.Errorf("timeout must be positive")
	}
	if err := validateModeFlags(mode, flags); err != nil {
		return err
	}
	if mode == "reconcile" {
		if err := verifyChartPairing(*controllerStateVersion); err != nil {
			return err
		}
	}
	var expected crdupgrade.RuntimeInvariants
	if mode == "runtime-verify" {
		var err error
		expected, err = runtimeInvariants(
			*releaseName,
			*releaseNamespace,
			*coordinationNamespace,
			*leaderElection,
			*leaderElectionID,
			*webhookServiceName,
			*webhookTimeoutSeconds,
			*hookServiceAccountName,
			*controllerServiceAccountName,
			*controllerDeploymentName,
			*certificateDeploymentName,
			*requireDistinctApprover,
		)
		if err != nil {
			return err
		}
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("load in-cluster configuration: %w", err)
	}
	config = boundedSweepRESTConfig(config)
	extensionsClient, err := apiextensionsclient.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create apiextensions client: %w", err)
	}
	manager := crdupgrade.New(extensionsClient.ApiextensionsV1().CustomResourceDefinitions())
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()

	var success string
	switch mode {
	case "reconcile":
		clientset, clientErr := kubernetes.NewForConfig(config)
		if clientErr != nil {
			return fmt.Errorf("create Kubernetes client: %w", clientErr)
		}
		stateClients, stateClientErr := newStoredControllerStateClients(config)
		if stateClientErr != nil {
			return stateClientErr
		}
		stop := &crdupgrade.RuntimeStop{
			Deployments:               clientset.AppsV1().Deployments(*releaseNamespace),
			Pods:                      clientset.CoreV1().Pods(*releaseNamespace),
			ReleaseName:               *releaseName,
			ReleaseNamespace:          *releaseNamespace,
			ControllerDeploymentName:  *controllerDeploymentName,
			CertificateDeploymentName: *certificateDeploymentName,
			ManagerImage:              *managerImage,
			PollEvery:                 pollInterval,
		}
		err = manager.ReconcileWithStatePreflightAndPrepare(
			ctx,
			stateClients,
			*controllerStateVersion,
			func(prepareCtx context.Context) error {
				if stopErr := stop.Run(prepareCtx); stopErr != nil {
					return fmt.Errorf("stop the running release: %w", stopErr)
				}
				return nil
			},
		)
		success = "candidate CRDs reconciled and established"
	case "verify":
		err = manager.Verify(ctx)
		success = "candidate CRDs verified and established"
	case "runtime-verify":
		clientset, clientErr := kubernetes.NewForConfig(config)
		if clientErr != nil {
			return fmt.Errorf("create Kubernetes client: %w", clientErr)
		}
		verifier := &crdupgrade.RuntimeVerifier{
			CRDs:       manager,
			Mutating:   clientset.AdmissionregistrationV1().MutatingWebhookConfigurations(),
			Validating: clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations(),
			Expected:   expected,
			PollEvery:  pollInterval,
		}
		success = "candidate CRDs and admission singleton verified"
		if *verifyControllerState {
			stateClients, stateClientErr := newStoredControllerStateClients(config)
			if stateClientErr != nil {
				return stateClientErr
			}
			verifier.StoredState = &stateClients
			verifier.SupportedControllerStateVersion = int64(controllerstate.CurrentVersion)
			success = "candidate CRDs, admission singleton, and stored controller state verified"
		}
		err = verifier.Verify(ctx)
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("%s did not complete before timeout: %w", mode, err)
		}
		return err
	}
	_, err = fmt.Fprintln(output, success)
	return err
}

// verifyChartPairing refuses a chart and a manager image from different
// releases before the hook reads or changes anything. The chart names the image
// in image.digest, and a --reuse-values upgrade keeps the old one: the new
// chart's hook would run the old binary, which stops nothing and updates no
// CRD, and Helm would then replace the running Pods with ones whose verifier
// refuses to start. The other pairing is worse, an old chart with a new image,
// whose hook would stop the runtime and update the CRDs first. The chart says
// which controller-state contract it was published with, and the binary
// compiles the one it can serve.
func verifyChartPairing(controllerStateVersion int64) error {
	if controllerStateVersion != int64(controllerstate.CurrentVersion) {
		return fmt.Errorf(
			"the chart carries controller-state version %d and this manager image compiles %d: install the chart published with the image, or the image published with the chart",
			controllerStateVersion, controllerstate.CurrentVersion)
	}
	return nil
}

func runtimeInvariants(
	releaseName, releaseNamespace, coordinationNamespace, leaderElection,
	leaderElectionID, webhookServiceName string, webhookTimeoutSeconds int,
	hookServiceAccountName, controllerServiceAccountName, controllerDeploymentName,
	certificateDeploymentName string, requireDistinctApprover bool,
) (crdupgrade.RuntimeInvariants, error) {
	if leaderElection != "true" && leaderElection != "false" {
		return crdupgrade.RuntimeInvariants{}, fmt.Errorf("leader-election must be exactly true or false")
	}
	if webhookTimeoutSeconds < 1 || webhookTimeoutSeconds > 30 {
		return crdupgrade.RuntimeInvariants{}, fmt.Errorf("webhook-timeout-seconds must be between 1 and 30")
	}
	leaderElectionEnabled, err := strconv.ParseBool(leaderElection)
	if err != nil {
		return crdupgrade.RuntimeInvariants{}, fmt.Errorf("parse leader-election: %w", err)
	}
	return crdupgrade.RuntimeInvariants{
		ReleaseName:                  releaseName,
		ReleaseNamespace:             releaseNamespace,
		CoordinationNamespace:        coordinationNamespace,
		LeaderElection:               leaderElectionEnabled,
		LeaderElectionID:             leaderElectionID,
		WebhookServiceName:           webhookServiceName,
		WebhookTimeoutSeconds:        int32(webhookTimeoutSeconds),
		HookServiceAccountName:       hookServiceAccountName,
		ControllerServiceAccountName: controllerServiceAccountName,
		ControllerDeploymentName:     controllerDeploymentName,
		CertificateDeploymentName:    certificateDeploymentName,
		ControllerStateVersion:       controllerstate.CurrentVersion,
		AdmissionContractVersion:     crdupgrade.CurrentAdmissionContractVersion,
		RequireDistinctApprover:      requireDistinctApprover,
	}, nil
}

// validateModeFlags refuses a flag the mode does not read, so an argument the
// chart passes and the binary ignores cannot go unnoticed.
func validateModeFlags(mode string, flags *flag.FlagSet) error {
	allowed := make(map[string]struct{}, len(modeFlags[mode]))
	for _, name := range modeFlags[mode] {
		allowed[name] = struct{}{}
	}
	var unexpected string
	flags.Visit(func(current *flag.Flag) {
		if _, found := allowed[current.Name]; !found && unexpected == "" {
			unexpected = current.Name
		}
	})
	if unexpected != "" {
		return fmt.Errorf("%s is not valid in %s mode", unexpected, mode)
	}
	return nil
}

func newStoredControllerStateClients(config *rest.Config) (crdupgrade.StoredControllerStateClients, error) {
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return crdupgrade.StoredControllerStateClients{}, fmt.Errorf("create dynamic Kubernetes client: %w", err)
	}
	return storedControllerStateClients(dynamicClient), nil
}

func storedControllerStateClients(dynamicClient dynamic.Interface) crdupgrade.StoredControllerStateClients {
	resource := func(name string) crdupgrade.ControllerStateListClient {
		return dynamicClient.Resource(schema.GroupVersionResource{
			Group: "operator.ptah.run", Version: "v1alpha1", Resource: name,
		})
	}
	return crdupgrade.StoredControllerStateClients{
		Schemas:        resource("ptahschemas"),
		Plans:          resource("ptahschemaplans"),
		Migrations:     resource("ptahmigrations"),
		MigrationPlans: resource("ptahmigrationplans"),
	}
}
