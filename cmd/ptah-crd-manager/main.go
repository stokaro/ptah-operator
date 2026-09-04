package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/stokaro/ptah-operator/internal/controllerstate"
	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

const (
	defaultTimeout                   = 2 * time.Minute
	retiredCredentialRevocationDelay = 65 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "ptah-crd-manager: %v\n", err)
		os.Exit(1)
	}
}

func run(parent context.Context, args []string, output io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("mode is required: image-check, identity-probe, preflight, reconcile, teardown-quiesce, teardown, verify, or runtime-verify")
	}
	mode := args[0]
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
	webhookSecretName := flags.String("webhook-secret-name", "", "exact admission webhook TLS Secret name")
	webhookPort := flags.Int("webhook-port", 0, "exact admission webhook container port")
	certificateHealthPort := flags.Int("certificate-health-port", 0, "exact certificate rotator health port")
	hookServiceAccountName := flags.String("hook-service-account-name", "", "exact preflight hook ServiceAccount name")
	controllerServiceAccountName := flags.String("controller-service-account-name", "", "exact controller ServiceAccount name")
	controllerDeploymentName := flags.String("controller-deployment-name", "", "exact controller Deployment name")
	controllerReplicas := flags.Int64("controller-replicas", 0, "exact candidate controller replica count")
	certificateDeploymentName := flags.String("certificate-deployment-name", "", "exact certificate-rotator Deployment name")
	releaseSequence := flags.Int64("release-sequence", 0, "monotonic published operator release sequence")
	managerImage := flags.String("manager-image", "", "exact manager image accepted by the runtime guard")
	controllerRuntimeArgsB64 := flags.String("controller-runtime-args-b64", "", "base64-encoded exact controller argument array")
	certificateRuntimeArgsB64 := flags.String("certificate-runtime-args-b64", "", "base64-encoded exact certificate rotator argument array")
	runtimeDeploymentConfigExpressionsB64 := flags.String("runtime-deployment-config-expressions-b64", "", "base64-encoded exact runtime Deployment CEL expression array")
	runtimePodConfigExpressionsB64 := flags.String("runtime-pod-config-expressions-b64", "", "base64-encoded exact runtime Pod CEL expression array")
	runtimeAdmissionContractB64 := flags.String("runtime-admission-contract-b64", "", "base64-encoded runtime Pod admission preflight contract")
	verifyControllerState := flags.Bool("verify-controller-state", false, "reject controller downgrades incompatible with stored PtahSchema state")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if *timeout <= 0 {
		return fmt.Errorf("timeout must be positive")
	}
	if mode != "image-check" && mode != "identity-probe" && mode != "preflight" && mode != "reconcile" && mode != "teardown-quiesce" && mode != "teardown" && mode != "verify" && mode != "runtime-verify" {
		return fmt.Errorf("unsupported mode %q: use image-check, identity-probe, preflight, reconcile, teardown-quiesce, teardown, verify, or runtime-verify", mode)
	}
	if err := validateModeFlags(mode, flags); err != nil {
		return err
	}
	if mode != "runtime-verify" && *verifyControllerState {
		return fmt.Errorf("verify-controller-state is valid only in runtime-verify mode")
	}
	if mode != "verify" {
		if *releaseSequence != int64(crdupgrade.CurrentReleaseSequence) {
			return fmt.Errorf("release-sequence must equal the binary contract %d", crdupgrade.CurrentReleaseSequence)
		}
		if *managerImage == "" {
			return fmt.Errorf("manager-image is required")
		}
		if mode != "image-check" && (*controllerReplicas < 1 || *controllerReplicas > int64(^uint32(0)>>1)) {
			return fmt.Errorf("controller-replicas must be between 1 and %d", int64(^uint32(0)>>1))
		}
	}
	if mode == "image-check" {
		_, err := fmt.Fprintf(output, "candidate manager image contract v%d verified for %s\n", crdupgrade.CurrentReleaseSequence, *managerImage)
		return err
	}
	var controllerRuntimeArgs, certificateRuntimeArgs, runtimeDeploymentConfigExpressions, runtimePodConfigExpressions []string
	var runtimeAdmissionContract crdupgrade.RuntimeAdmissionContract
	var err error
	if mode != "verify" {
		controllerRuntimeArgs, err = decodeRuntimeArgs(*controllerRuntimeArgsB64, "controller")
		if err != nil {
			return err
		}
		certificateRuntimeArgs, err = decodeRuntimeArgs(*certificateRuntimeArgsB64, "certificate")
		if err != nil {
			return err
		}
		runtimeDeploymentConfigExpressions, err = decodeRuntimeArgs(*runtimeDeploymentConfigExpressionsB64, "runtime Deployment config expressions")
		if err != nil {
			return err
		}
		runtimePodConfigExpressions, err = decodeRuntimeArgs(*runtimePodConfigExpressionsB64, "runtime Pod config expressions")
		if err != nil {
			return err
		}
		runtimeAdmissionContract, err = decodeRuntimeAdmissionContract(*runtimeAdmissionContractB64)
		if err != nil {
			return err
		}
		if *webhookSecretName == "" {
			return fmt.Errorf("webhook-secret-name is required")
		}
		if *webhookPort < 1 || *webhookPort > 65535 || *certificateHealthPort < 1 || *certificateHealthPort > 65535 {
			return fmt.Errorf("runtime container ports must be between 1 and 65535")
		}
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("load in-cluster configuration: %w", err)
	}
	extensionsClient, err := apiextensionsclient.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create apiextensions client: %w", err)
	}
	manager := crdupgrade.New(extensionsClient.ApiextensionsV1().CustomResourceDefinitions())
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()

	switch mode {
	case "identity-probe":
		expected, expectedErr := runtimeInvariants(
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
			int32(*releaseSequence),
		)
		if expectedErr != nil {
			return expectedErr
		}
		clientset, clientErr := kubernetes.NewForConfig(config)
		if clientErr != nil {
			return fmt.Errorf("create Kubernetes client: %w", clientErr)
		}
		rollout := newRolloutGuard(clientset, expected, *managerImage, *webhookSecretName, int32(*webhookPort), int32(*certificateHealthPort), int32(*controllerReplicas), controllerRuntimeArgs, certificateRuntimeArgs, runtimeDeploymentConfigExpressions, runtimePodConfigExpressions, runtimeAdmissionContract, *runtimeAdmissionContractB64)
		origin := crdupgrade.NewServiceAccountOriginGuard(
			rollout,
			clientset.CoreV1().ServiceAccounts(expected.ReleaseNamespace),
		)
		inventory := newWorkloadInventory(clientset, rollout)
		if err = origin.Prepare(ctx); err != nil {
			err = fmt.Errorf("prepare service account origin guard: %w", err)
			break
		}
		if err = inventory.VerifyHookBootstrap(ctx); err != nil {
			err = fmt.Errorf("verify pre-staged hook workloads: %w", err)
			break
		}
		err = rollout.PrepareHookIdentity(ctx)
	case "preflight":
		expected, expectedErr := runtimeInvariants(
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
			int32(*releaseSequence),
		)
		if expectedErr != nil {
			return expectedErr
		}
		clientset, clientErr := kubernetes.NewForConfig(config)
		if clientErr != nil {
			return fmt.Errorf("create Kubernetes client: %w", clientErr)
		}
		adopter := &crdupgrade.AdmissionAdopter{
			Mutating:   clientset.AdmissionregistrationV1().MutatingWebhookConfigurations(),
			Validating: clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations(),
			Expected:   expected,
		}
		rollout := newRolloutGuard(clientset, expected, *managerImage, *webhookSecretName, int32(*webhookPort), int32(*certificateHealthPort), int32(*controllerReplicas), controllerRuntimeArgs, certificateRuntimeArgs, runtimeDeploymentConfigExpressions, runtimePodConfigExpressions, runtimeAdmissionContract, *runtimeAdmissionContractB64)
		inventory := newWorkloadInventory(clientset, rollout)
		admissionPreflight, preflightErr := newRuntimeAdmissionPreflight(clientset, expected, runtimeAdmissionContract)
		if preflightErr != nil {
			return preflightErr
		}
		quotaPreflight := newRuntimeResourceQuotaPreflight(clientset, expected, runtimeAdmissionContract, int32(*controllerReplicas))
		stateClients, stateClientErr := newStoredControllerStateClients(config)
		if stateClientErr != nil {
			return stateClientErr
		}
		if err = rollout.VerifyHookIdentity(ctx); err != nil {
			err = fmt.Errorf("verify privileged hook identity guard: %w", err)
			break
		}
		if err = inventory.VerifyRuntimeBeforeQuiesce(ctx); err != nil {
			err = fmt.Errorf("verify pre-staged runtime workloads: %w", err)
			break
		}
		if err = quotaPreflight.Check(ctx); err != nil {
			err = fmt.Errorf("preflight runtime ResourceQuota capacity: %w", err)
			break
		}
		if err = admissionPreflight.Check(ctx); err != nil {
			err = fmt.Errorf("preflight runtime Pod admission: %w", err)
			break
		}
		if err = manager.PreflightWithState(ctx, stateClients, int64(controllerstate.CurrentVersion)); err != nil {
			break
		}
		if err = adopter.Preflight(ctx); err != nil {
			err = fmt.Errorf("preflight admission singleton adoption: %w", err)
			break
		}
		err = rollout.PreflightQuiesce(ctx)
	case "reconcile":
		expected, expectedErr := runtimeInvariants(
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
			int32(*releaseSequence),
		)
		if expectedErr != nil {
			return expectedErr
		}
		clientset, clientErr := kubernetes.NewForConfig(config)
		if clientErr != nil {
			return fmt.Errorf("create Kubernetes client: %w", clientErr)
		}
		adopter := &crdupgrade.AdmissionAdopter{
			Mutating:   clientset.AdmissionregistrationV1().MutatingWebhookConfigurations(),
			Validating: clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations(),
			Expected:   expected,
		}
		rollout := newRolloutGuard(clientset, expected, *managerImage, *webhookSecretName, int32(*webhookPort), int32(*certificateHealthPort), int32(*controllerReplicas), controllerRuntimeArgs, certificateRuntimeArgs, runtimeDeploymentConfigExpressions, runtimePodConfigExpressions, runtimeAdmissionContract, *runtimeAdmissionContractB64)
		inventory := newWorkloadInventory(clientset, rollout)
		admissionPreflight, preflightErr := newRuntimeAdmissionPreflight(clientset, expected, runtimeAdmissionContract)
		if preflightErr != nil {
			return preflightErr
		}
		quotaPreflight := newRuntimeResourceQuotaPreflight(clientset, expected, runtimeAdmissionContract, int32(*controllerReplicas))
		stateClients, stateClientErr := newStoredControllerStateClients(config)
		if stateClientErr != nil {
			return stateClientErr
		}
		err = manager.ReconcileWithStatePreflightAndPrepare(
			ctx,
			stateClients,
			int64(controllerstate.CurrentVersion),
			func(prepareCtx context.Context) error {
				if inventoryErr := inventory.VerifyRuntimeBeforeQuiesce(prepareCtx); inventoryErr != nil {
					return fmt.Errorf("verify pre-staged runtime workloads: %w", inventoryErr)
				}
				if quotaErr := quotaPreflight.Check(prepareCtx); quotaErr != nil {
					return fmt.Errorf("preflight runtime ResourceQuota capacity: %w", quotaErr)
				}
				if admissionErr := admissionPreflight.Check(prepareCtx); admissionErr != nil {
					return fmt.Errorf("preflight runtime Pod admission: %w", admissionErr)
				}
				if prepareErr := rollout.Prepare(prepareCtx); prepareErr != nil {
					return prepareErr
				}
				if quiesceErr := rollout.Quiesce(prepareCtx); quiesceErr != nil {
					return quiesceErr
				}
				if inventoryErr := waitForNoProtectedRuntimePods(prepareCtx, inventory, rollout.PollEvery); inventoryErr != nil {
					return fmt.Errorf("wait for namespace-wide runtime Pod quiescence: %w", inventoryErr)
				}
				// Re-read quota and admission inputs after the old runtime is fully
				// stopped. This closes the race after the initial availability-
				// preserving checks and before activation of the candidate contract.
				if quotaErr := quotaPreflight.WaitForCapacityAfterQuiesce(prepareCtx, rollout.PollEvery); quotaErr != nil {
					return fmt.Errorf("recheck runtime ResourceQuota capacity before activation: %w", quotaErr)
				}
				if admissionErr := admissionPreflight.Check(prepareCtx); admissionErr != nil {
					return fmt.Errorf("recheck runtime Pod admission before activation: %w", admissionErr)
				}
				if activateErr := rollout.Activate(prepareCtx); activateErr != nil {
					return activateErr
				}
				if adoptErr := adopter.Adopt(prepareCtx); adoptErr != nil {
					return fmt.Errorf("adopt admission singleton: %w", adoptErr)
				}
				return nil
			},
		)
	case "teardown-quiesce":
		expected, expectedErr := runtimeInvariants(
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
			int32(*releaseSequence),
		)
		if expectedErr != nil {
			return expectedErr
		}
		clientset, clientErr := kubernetes.NewForConfig(config)
		if clientErr != nil {
			return fmt.Errorf("create Kubernetes client: %w", clientErr)
		}
		rollout := newRolloutGuard(clientset, expected, *managerImage, *webhookSecretName, int32(*webhookPort), int32(*certificateHealthPort), int32(*controllerReplicas), controllerRuntimeArgs, certificateRuntimeArgs, runtimeDeploymentConfigExpressions, runtimePodConfigExpressions, runtimeAdmissionContract, *runtimeAdmissionContractB64)
		inventory := newWorkloadInventory(clientset, rollout)
		releaseTeardown, privilegeTeardown, teardownErr := newTeardownPhases(clientset, rollout, runtimeAdmissionContract)
		if teardownErr != nil {
			return teardownErr
		}
		if err = rollout.VerifyHookIdentity(ctx); err != nil {
			err = fmt.Errorf("verify teardown hook identity guard: %w", err)
			break
		}
		if err = releaseTeardown.Preflight(ctx); err != nil {
			err = fmt.Errorf("preflight exact admission teardown inventory: %w", err)
			break
		}
		if err = privilegeTeardown.Preflight(ctx); err != nil {
			err = fmt.Errorf("preflight exact privilege teardown inventory: %w", err)
			break
		}
		if err = inventory.VerifyRuntimeBeforeQuiesce(ctx); err != nil {
			err = fmt.Errorf("verify pre-staged teardown workloads: %w", err)
			break
		}
		if err = rollout.Quiesce(ctx); err != nil {
			err = fmt.Errorf("quiesce release runtime: %w", err)
			break
		}
		if err = waitForNoProtectedRuntimePods(ctx, inventory, rollout.PollEvery); err != nil {
			err = fmt.Errorf("wait for namespace-wide runtime Pod quiescence: %w", err)
		}
	case "teardown":
		expected, expectedErr := runtimeInvariants(
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
			int32(*releaseSequence),
		)
		if expectedErr != nil {
			return expectedErr
		}
		clientset, clientErr := kubernetes.NewForConfig(config)
		if clientErr != nil {
			return fmt.Errorf("create Kubernetes client: %w", clientErr)
		}
		rollout := newRolloutGuard(clientset, expected, *managerImage, *webhookSecretName, int32(*webhookPort), int32(*certificateHealthPort), int32(*controllerReplicas), controllerRuntimeArgs, certificateRuntimeArgs, runtimeDeploymentConfigExpressions, runtimePodConfigExpressions, runtimeAdmissionContract, *runtimeAdmissionContractB64)
		inventory := newWorkloadInventory(clientset, rollout)
		releaseTeardown, privilegeTeardown, teardownErr := newTeardownPhases(clientset, rollout, runtimeAdmissionContract)
		if teardownErr != nil {
			return teardownErr
		}
		if err = releaseTeardown.Preflight(ctx); err != nil {
			err = fmt.Errorf("preflight exact admission teardown inventory: %w", err)
			break
		}
		if err = privilegeTeardown.Preflight(ctx); err != nil {
			err = fmt.Errorf("preflight exact privilege teardown inventory: %w", err)
			break
		}
		remaining, inventoryErr := inventory.ProtectedRuntimePodsRemain(ctx)
		if inventoryErr != nil {
			err = fmt.Errorf("verify runtime Pod quiescence: %w", inventoryErr)
			break
		}
		if remaining {
			err = fmt.Errorf("protected runtime Pods remain after teardown quiescence")
			break
		}
		convergence, convergenceErr := newTeardownRBACConvergenceBarrier(ctx, config, clientset, rollout, runtimeAdmissionContract)
		if convergenceErr != nil {
			err = fmt.Errorf("prepare API-server authorization convergence barrier: %w", convergenceErr)
			break
		}
		if err = convergence.Validate(); err != nil {
			err = fmt.Errorf("validate API-server authorization convergence barrier: %w", err)
			break
		}
		if err = privilegeTeardown.Teardown(ctx); err != nil {
			err = fmt.Errorf("remove release privilege: %w", err)
			break
		}
		if err = waitForRetiredBoundCredentialRevocation(ctx, retiredCredentialRevocationDelay); err != nil {
			err = fmt.Errorf("wait for retired Pod-bound credentials to become invalid: %w", err)
			break
		}
		if err = convergence.Wait(ctx); err != nil {
			err = fmt.Errorf("wait for API-server authorization revocation: %w", err)
			break
		}
		if err = privilegeTeardown.Preflight(ctx); err != nil {
			err = fmt.Errorf("recheck privilege inventory after authorization revocation: %w", err)
			break
		}
		if err = releaseTeardown.Teardown(ctx); err != nil {
			err = fmt.Errorf("remove exact admission teardown inventory: %w", err)
		}
	case "verify":
		err = manager.Verify(ctx)
	case "runtime-verify":
		expected, expectedErr := runtimeInvariants(
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
			int32(*releaseSequence),
		)
		if expectedErr != nil {
			return expectedErr
		}
		clientset, clientErr := kubernetes.NewForConfig(config)
		if clientErr != nil {
			return fmt.Errorf("create Kubernetes client: %w", clientErr)
		}
		rollout := newRolloutGuard(clientset, expected, *managerImage, *webhookSecretName, int32(*webhookPort), int32(*certificateHealthPort), int32(*controllerReplicas), controllerRuntimeArgs, certificateRuntimeArgs, runtimeDeploymentConfigExpressions, runtimePodConfigExpressions, runtimeAdmissionContract, *runtimeAdmissionContractB64)
		admissionPreflight, preflightErr := newRuntimeAdmissionPreflight(clientset, expected, runtimeAdmissionContract)
		if preflightErr != nil {
			return preflightErr
		}
		if preflightErr = admissionPreflight.Check(ctx); preflightErr != nil {
			return fmt.Errorf("preflight runtime Pod admission: %w", preflightErr)
		}
		if verifyErr := rollout.Verify(ctx); verifyErr != nil {
			return fmt.Errorf("verify persistent rollout guards: %w", verifyErr)
		}
		verifier := &crdupgrade.RuntimeVerifier{
			CRDs:       manager,
			Mutating:   clientset.AdmissionregistrationV1().MutatingWebhookConfigurations(),
			Validating: clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations(),
			Expected:   expected,
			PollEvery:  500 * time.Millisecond,
		}
		if *verifyControllerState {
			stateClients, stateClientErr := newStoredControllerStateClients(config)
			if stateClientErr != nil {
				return stateClientErr
			}
			verifier.StoredState = &stateClients
			verifier.SupportedControllerStateVersion = int64(controllerstate.CurrentVersion)
		}
		err = verifier.Verify(ctx)
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("%s did not complete before timeout: %w", mode, err)
		}
		return err
	}
	success := "verified"
	if mode == "reconcile" {
		success = "reconciled"
	}
	if mode == "preflight" {
		_, err = fmt.Fprintln(output, "candidate release preflight verified without persistent mutation")
		return err
	}
	if mode == "identity-probe" {
		_, err = fmt.Fprintln(output, "privileged hook identity policy is ready and enforcing")
		return err
	}
	if mode == "teardown-quiesce" {
		_, err = fmt.Fprintln(output, "release teardown inventory verified and runtime quiesced")
		return err
	}
	if mode == "teardown" {
		_, err = fmt.Fprintln(output, "release privilege revoked and exact admission inventory removed")
		return err
	}
	if mode == "runtime-verify" {
		if *verifyControllerState {
			_, err = fmt.Fprintln(output, "candidate rollout guards, CRDs, admission singleton, and stored controller state verified")
		} else {
			_, err = fmt.Fprintln(output, "candidate rollout guards, CRDs, and admission singleton verified")
		}
		return err
	}
	_, err = fmt.Fprintf(output, "candidate CRDs %s and established\n", success)
	return err
}

func runtimeInvariants(
	releaseName, releaseNamespace, coordinationNamespace, leaderElection,
	leaderElectionID, webhookServiceName string, webhookTimeoutSeconds int,
	hookServiceAccountName, controllerServiceAccountName, controllerDeploymentName,
	certificateDeploymentName string, releaseSequence int32,
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
		ReleaseSequence:              releaseSequence,
	}, nil
}

func newRolloutGuard(
	clientset kubernetes.Interface,
	expected crdupgrade.RuntimeInvariants,
	managerImage, webhookSecretName string,
	webhookPort, certificateHealthPort, controllerReplicas int32,
	controllerArgs, certificateArgs, runtimeDeploymentConfigExpressions, runtimePodConfigExpressions []string,
	runtimeAdmissionContract crdupgrade.RuntimeAdmissionContract,
	runtimeAdmissionContractB64 string,
) *crdupgrade.RolloutGuard {
	return &crdupgrade.RolloutGuard{
		Policies:                           clientset.AdmissionregistrationV1().ValidatingAdmissionPolicies(),
		Bindings:                           clientset.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings(),
		Deployments:                        clientset.AppsV1().Deployments(expected.ReleaseNamespace),
		Pods:                               clientset.CoreV1().Pods(expected.ReleaseNamespace),
		ConfigMaps:                         clientset.CoreV1().ConfigMaps(expected.ReleaseNamespace),
		ReleaseName:                        expected.ReleaseName,
		ReleaseNamespace:                   expected.ReleaseNamespace,
		CoordinationNamespace:              expected.CoordinationNamespace,
		LeaderElection:                     expected.LeaderElection,
		LeaderElectionID:                   expected.LeaderElectionID,
		WebhookServiceName:                 expected.WebhookServiceName,
		WebhookTimeoutSeconds:              expected.WebhookTimeoutSeconds,
		WebhookSecretName:                  webhookSecretName,
		WebhookPort:                        webhookPort,
		CertificateHealthPort:              certificateHealthPort,
		HookServiceAccountName:             expected.HookServiceAccountName,
		ControllerServiceAccountName:       expected.ControllerServiceAccountName,
		ControllerDeploymentName:           expected.ControllerDeploymentName,
		ControllerReplicas:                 controllerReplicas,
		CertificateDeploymentName:          expected.CertificateDeploymentName,
		ControllerStateVersion:             expected.ControllerStateVersion,
		AdmissionContractVersion:           expected.AdmissionContractVersion,
		ReleaseSequence:                    expected.ReleaseSequence,
		ManagerImage:                       managerImage,
		ControllerArgs:                     append([]string(nil), controllerArgs...),
		CertificateArgs:                    append([]string(nil), certificateArgs...),
		RuntimeDeploymentConfigExpressions: append([]string(nil), runtimeDeploymentConfigExpressions...),
		RuntimePodConfigExpressions:        append([]string(nil), runtimePodConfigExpressions...),
		PriorityClassName:                  runtimeAdmissionContract.PriorityClassName,
		RuntimeAdmissionContractB64:        runtimeAdmissionContractB64,
		PollEvery:                          500 * time.Millisecond,
	}
}

func validateModeFlags(mode string, flags *flag.FlagSet) error {
	allowed := map[string]struct{}{}
	switch mode {
	case "image-check":
		allowed["release-sequence"] = struct{}{}
		allowed["manager-image"] = struct{}{}
	case "verify":
		allowed["timeout"] = struct{}{}
	case "identity-probe", "preflight", "reconcile", "teardown-quiesce", "teardown", "runtime-verify":
		for _, name := range []string{
			"timeout",
			"release-name",
			"release-namespace",
			"coordination-namespace",
			"leader-election",
			"leader-election-id",
			"webhook-service-name",
			"webhook-timeout-seconds",
			"webhook-secret-name",
			"webhook-port",
			"certificate-health-port",
			"hook-service-account-name",
			"controller-service-account-name",
			"controller-deployment-name",
			"controller-replicas",
			"certificate-deployment-name",
			"release-sequence",
			"manager-image",
			"controller-runtime-args-b64",
			"certificate-runtime-args-b64",
			"runtime-deployment-config-expressions-b64",
			"runtime-pod-config-expressions-b64",
			"runtime-admission-contract-b64",
		} {
			allowed[name] = struct{}{}
		}
		if mode == "runtime-verify" {
			allowed["verify-controller-state"] = struct{}{}
		}
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

func newRuntimeResourceQuotaPreflight(
	clientset kubernetes.Interface,
	expected crdupgrade.RuntimeInvariants,
	contract crdupgrade.RuntimeAdmissionContract,
	controllerReplicas int32,
) *crdupgrade.RuntimeResourceQuotaPreflight {
	return crdupgrade.NewRuntimeResourceQuotaPreflight(
		contract,
		controllerReplicas,
		expected.ReleaseName,
		expected.ReleaseNamespace,
		expected.ControllerDeploymentName,
		expected.CertificateDeploymentName,
		clientset.CoreV1().ResourceQuotas(expected.ReleaseNamespace),
		clientset.CoreV1().Pods(expected.ReleaseNamespace),
	)
}

func newRuntimeAdmissionPreflight(
	clientset kubernetes.Interface,
	expected crdupgrade.RuntimeInvariants,
	contract crdupgrade.RuntimeAdmissionContract,
) (*crdupgrade.RuntimeAdmissionPreflight, error) {
	if contract.Namespace != expected.ReleaseNamespace {
		return nil, fmt.Errorf("runtime admission contract namespace %q differs from release namespace %q", contract.Namespace, expected.ReleaseNamespace)
	}
	if contract.ControllerServiceAccountName != expected.ControllerServiceAccountName {
		return nil, fmt.Errorf("runtime admission controller ServiceAccount %q differs from rollout identity %q", contract.ControllerServiceAccountName, expected.ControllerServiceAccountName)
	}
	if contract.CertificateServiceAccountName != expected.CertificateDeploymentName {
		return nil, fmt.Errorf("runtime admission certificate ServiceAccount %q differs from rollout identity %q", contract.CertificateServiceAccountName, expected.CertificateDeploymentName)
	}
	return crdupgrade.NewRuntimeAdmissionPreflight(
		contract,
		clientset.CoreV1().LimitRanges(expected.ReleaseNamespace),
		clientset.CoreV1().ServiceAccounts(expected.ReleaseNamespace),
		clientset.SchedulingV1().PriorityClasses(),
	), nil
}

func newWorkloadInventory(clientset kubernetes.Interface, rollout *crdupgrade.RolloutGuard) *crdupgrade.WorkloadInventory {
	namespace := rollout.ReleaseNamespace
	return crdupgrade.NewWorkloadInventory(
		rollout,
		clientset.CoreV1().Pods(namespace),
		clientset.BatchV1().Jobs(namespace),
		clientset.AppsV1().ReplicaSets(namespace),
		clientset.AppsV1().Deployments(namespace),
	)
}

func waitForNoProtectedRuntimePods(ctx context.Context, inventory *crdupgrade.WorkloadInventory, pollEvery time.Duration) error {
	return wait.PollUntilContextCancel(ctx, pollEvery, true, func(pollCtx context.Context) (bool, error) {
		remaining, err := inventory.ProtectedRuntimePodsRemain(pollCtx)
		return !remaining, err
	})
}

func waitForRetiredBoundCredentialRevocation(ctx context.Context, delay time.Duration) error {
	if ctx == nil {
		return errors.New("retired credential revocation context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay <= 0 {
		return errors.New("retired credential revocation delay must be positive")
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func decodeRuntimeArgs(encoded, component string) ([]string, error) {
	if encoded == "" {
		return nil, fmt.Errorf("%s-runtime-args-b64 is required", component)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode %s runtime arguments: %w", component, err)
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("parse %s runtime arguments: %w", component, err)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("%s runtime arguments must not be empty", component)
	}
	for index, value := range values {
		if value == "" {
			return nil, fmt.Errorf("%s runtime argument %d must not be empty", component, index)
		}
	}
	return values, nil
}

func decodeRuntimeAdmissionContract(encoded string) (crdupgrade.RuntimeAdmissionContract, error) {
	if encoded == "" {
		return crdupgrade.RuntimeAdmissionContract{}, fmt.Errorf("runtime-admission-contract-b64 is required")
	}
	if len(encoded) > 256*1024 {
		return crdupgrade.RuntimeAdmissionContract{}, fmt.Errorf("runtime admission contract is too large")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return crdupgrade.RuntimeAdmissionContract{}, fmt.Errorf("decode runtime admission contract: %w", err)
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return crdupgrade.RuntimeAdmissionContract{}, fmt.Errorf("parse runtime admission contract: %w", err)
	}
	type wireContract struct {
		Version                                         *int                           `json:"version"`
		Namespace                                       *string                        `json:"namespace"`
		CommonInitContainerResources                    *corev1.ResourceRequirements   `json:"commonInitContainerResources"`
		ControllerContainerResources                    *corev1.ResourceRequirements   `json:"controllerContainerResources"`
		CertificateContainerResources                   *corev1.ResourceRequirements   `json:"certificateContainerResources"`
		ImagePullSecrets                                *[]corev1.LocalObjectReference `json:"imagePullSecrets"`
		PriorityClassName                               *string                        `json:"priorityClassName"`
		PriorityClassValue                              *int32                         `json:"priorityClassValue"`
		PriorityClassPreemptionPolicy                   *string                        `json:"priorityClassPreemptionPolicy"`
		ControllerServiceAccountName                    *string                        `json:"controllerServiceAccountName"`
		CertificateServiceAccountName                   *string                        `json:"certificateServiceAccountName"`
		ControllerServiceAccountCreate                  *bool                          `json:"controllerServiceAccountCreate"`
		ControllerServiceAccountEnforceMountableSecrets *bool                          `json:"controllerServiceAccountEnforceMountableSecrets"`
		ControllerSecretNames                           *[]string                      `json:"controllerSecretNames"`
		CertificateSecretNames                          *[]string                      `json:"certificateSecretNames"`
		CertificateRuntimeEnabled                       *bool                          `json:"certificateRuntimeEnabled"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var wire wireContract
	if err := decoder.Decode(&wire); err != nil {
		return crdupgrade.RuntimeAdmissionContract{}, fmt.Errorf("parse runtime admission contract: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return crdupgrade.RuntimeAdmissionContract{}, fmt.Errorf("parse runtime admission contract: trailing JSON value")
		}
		return crdupgrade.RuntimeAdmissionContract{}, fmt.Errorf("parse runtime admission contract: %w", err)
	}
	required := []struct {
		name    string
		present bool
	}{
		{name: "version", present: wire.Version != nil},
		{name: "namespace", present: wire.Namespace != nil},
		{name: "commonInitContainerResources", present: wire.CommonInitContainerResources != nil},
		{name: "controllerContainerResources", present: wire.ControllerContainerResources != nil},
		{name: "certificateContainerResources", present: wire.CertificateContainerResources != nil},
		{name: "imagePullSecrets", present: wire.ImagePullSecrets != nil},
		{name: "priorityClassName", present: wire.PriorityClassName != nil},
		{name: "priorityClassValue", present: wire.PriorityClassValue != nil},
		{name: "priorityClassPreemptionPolicy", present: wire.PriorityClassPreemptionPolicy != nil},
		{name: "controllerServiceAccountName", present: wire.ControllerServiceAccountName != nil},
		{name: "certificateServiceAccountName", present: wire.CertificateServiceAccountName != nil},
		{name: "controllerServiceAccountCreate", present: wire.ControllerServiceAccountCreate != nil},
		{name: "controllerServiceAccountEnforceMountableSecrets", present: wire.ControllerServiceAccountEnforceMountableSecrets != nil},
		{name: "controllerSecretNames", present: wire.ControllerSecretNames != nil},
		{name: "certificateSecretNames", present: wire.CertificateSecretNames != nil},
		{name: "certificateRuntimeEnabled", present: wire.CertificateRuntimeEnabled != nil},
	}
	for _, field := range required {
		if !field.present {
			return crdupgrade.RuntimeAdmissionContract{}, fmt.Errorf("parse runtime admission contract: required field %q is missing", field.name)
		}
	}
	if *wire.Version != 1 {
		return crdupgrade.RuntimeAdmissionContract{}, fmt.Errorf("parse runtime admission contract: unsupported version %d", *wire.Version)
	}
	return crdupgrade.RuntimeAdmissionContract{
		Namespace:                                       *wire.Namespace,
		CommonInitContainerResources:                    *wire.CommonInitContainerResources,
		ControllerContainerResources:                    *wire.ControllerContainerResources,
		CertificateContainerResources:                   *wire.CertificateContainerResources,
		ImagePullSecrets:                                append([]corev1.LocalObjectReference(nil), (*wire.ImagePullSecrets)...),
		PriorityClassName:                               *wire.PriorityClassName,
		PriorityClassValue:                              *wire.PriorityClassValue,
		PriorityClassPreemptionPolicy:                   *wire.PriorityClassPreemptionPolicy,
		ControllerServiceAccountName:                    *wire.ControllerServiceAccountName,
		CertificateServiceAccountName:                   *wire.CertificateServiceAccountName,
		ControllerServiceAccountCreate:                  *wire.ControllerServiceAccountCreate,
		ControllerServiceAccountEnforceMountableSecrets: *wire.ControllerServiceAccountEnforceMountableSecrets,
		ControllerSecretNames:                           append([]string(nil), (*wire.ControllerSecretNames)...),
		CertificateSecretNames:                          append([]string(nil), (*wire.CertificateSecretNames)...),
		CertificateRuntimeEnabled:                       *wire.CertificateRuntimeEnabled,
	}, nil
}

func rejectDuplicateJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := scanJSONValue(decoder, "$"); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, path string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key at %s is not a string", path)
			}
			if _, found := seen[key]; found {
				return fmt.Errorf("duplicate JSON key %q at %s", key, path)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder, path+"."+key); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("object at %s has an invalid closing delimiter", path)
		}
		return nil
	case '[':
		index := 0
		for decoder.More() {
			if err := scanJSONValue(decoder, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
			index++
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("array at %s has an invalid closing delimiter", path)
		}
		return nil
	default:
		return fmt.Errorf("unexpected JSON delimiter %q at %s", delimiter, path)
	}
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
			Group: "operator.ptah.dev", Version: "v1alpha1", Resource: name,
		})
	}
	return crdupgrade.StoredControllerStateClients{
		Schemas:   resource("ptahschemas"),
		Plans:     resource("ptahschemaplans"),
		Approvals: resource("ptahschemaapprovals"),
	}
}
