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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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
	supportedModes                   = "image-check, identity-probe, preflight, reconcile, teardown-retirement-probe-a, teardown-retirement-gate, teardown-quiesce, teardown, teardown-retirement-final, verify, or runtime-verify"
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
		return fmt.Errorf("mode is required: %s", supportedModes)
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
	controllerServiceAccountManagedFlag := flags.String("controller-service-account-managed", "", "whether Helm creates the candidate controller ServiceAccount, exactly true or false")
	previousControllerServiceAccountName := flags.String("previous-controller-service-account-name", "", "controller ServiceAccount active before candidate cutover")
	previousControllerServiceAccountUID := flags.String("previous-controller-service-account-uid", "", "immutable UID of the controller ServiceAccount active before candidate cutover")
	previousControllerServiceAccountManagedFlag := flags.String("previous-controller-service-account-managed", "", "whether Helm safely owns the previous controller ServiceAccount, exactly true or false")
	previousControllerReleaseSequence := flags.Int64("previous-controller-release-sequence", 0, "release sequence active before candidate cutover")
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
	verifyCertificateRecovery := flags.Bool("verify-certificate-recovery", false, "directly verify optional certificate Secret recovery admission on every API server")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if *timeout <= 0 {
		return fmt.Errorf("timeout must be positive")
	}
	if mode != "image-check" && mode != "identity-probe" && mode != "preflight" && mode != "reconcile" && mode != "teardown-retirement-probe-a" && mode != "teardown-retirement-gate" && mode != "teardown-quiesce" && mode != "teardown" && mode != "teardown-retirement-final" && mode != "verify" && mode != "runtime-verify" {
		return fmt.Errorf("unsupported mode %q: use %s", mode, supportedModes)
	}
	if err := validateModeFlags(mode, flags); err != nil {
		return err
	}
	if mode != "runtime-verify" && *verifyControllerState {
		return fmt.Errorf("verify-controller-state is valid only in runtime-verify mode")
	}
	if mode != "runtime-verify" && *verifyCertificateRecovery {
		return fmt.Errorf("verify-certificate-recovery is valid only in runtime-verify mode")
	}
	if *verifyControllerState && *verifyCertificateRecovery {
		return fmt.Errorf("controller-state and certificate-recovery runtime verification are mutually exclusive")
	}
	var err error
	controllerServiceAccountManaged := false
	previousControllerServiceAccountManaged := false
	if mode != "image-check" && mode != "verify" {
		controllerServiceAccountManaged, err = parseExactBooleanFlag(*controllerServiceAccountManagedFlag, "controller-service-account-managed")
		if err != nil {
			return err
		}
		previousControllerServiceAccountManaged, err = parseExactBooleanFlag(*previousControllerServiceAccountManagedFlag, "previous-controller-service-account-managed")
		if err != nil {
			return err
		}
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
			controllerServiceAccountManaged,
			*previousControllerServiceAccountName,
			types.UID(*previousControllerServiceAccountUID),
			previousControllerServiceAccountManaged,
			*controllerDeploymentName,
			*certificateDeploymentName,
			int32(*releaseSequence),
			int32(*previousControllerReleaseSequence),
		)
		if expectedErr != nil {
			return expectedErr
		}
		clientset, clientErr := kubernetes.NewForConfig(config)
		if clientErr != nil {
			return fmt.Errorf("create Kubernetes client: %w", clientErr)
		}
		rollout := newRolloutGuard(clientset, expected, *managerImage, *webhookSecretName, int32(*webhookPort), int32(*certificateHealthPort), int32(*controllerReplicas), controllerRuntimeArgs, certificateRuntimeArgs, runtimeDeploymentConfigExpressions, runtimePodConfigExpressions, runtimeAdmissionContract, *runtimeAdmissionContractB64)
		serviceAccountObjectGuard := crdupgrade.NewServiceAccountObjectGuard(rollout)
		origin := crdupgrade.NewServiceAccountOriginGuard(rollout)
		inventory := newWorkloadInventory(clientset, rollout)
		if err = serviceAccountObjectGuard.WaitReady(ctx); err != nil {
			err = fmt.Errorf("wait for stable ServiceAccount object guard: %w", err)
			break
		}
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
			controllerServiceAccountManaged,
			*previousControllerServiceAccountName,
			types.UID(*previousControllerServiceAccountUID),
			previousControllerServiceAccountManaged,
			*controllerDeploymentName,
			*certificateDeploymentName,
			int32(*releaseSequence),
			int32(*previousControllerReleaseSequence),
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
		serviceAccountObjectGuard := crdupgrade.NewServiceAccountObjectGuard(rollout)
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
		if err = serviceAccountObjectGuard.WaitReady(ctx); err != nil {
			err = fmt.Errorf("wait for stable ServiceAccount object guard: %w", err)
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
			controllerServiceAccountManaged,
			*previousControllerServiceAccountName,
			types.UID(*previousControllerServiceAccountUID),
			previousControllerServiceAccountManaged,
			*controllerDeploymentName,
			*certificateDeploymentName,
			int32(*releaseSequence),
			int32(*previousControllerReleaseSequence),
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
		serviceAccountObjectGuard := crdupgrade.NewServiceAccountObjectGuard(rollout)
		predecessorRetirement := crdupgrade.NewPredecessorRetirement(
			rollout,
			clientset.AdmissionregistrationV1().ValidatingAdmissionPolicies(),
			clientset.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings(),
			clientset.CoreV1().ConfigMaps(expected.ReleaseNamespace),
		)
		inventory := newWorkloadInventory(clientset, rollout)
		admissionPreflight, preflightErr := newRuntimeAdmissionPreflight(clientset, expected, runtimeAdmissionContract)
		if preflightErr != nil {
			return preflightErr
		}
		quotaPreflight := newRuntimeResourceQuotaPreflight(clientset, expected, runtimeAdmissionContract, int32(*controllerReplicas))
		controllerRBACTransition, transitionErr := crdupgrade.NewControllerRBACTransition(
			rollout,
			runtimeAdmissionContract,
			newControllerRBACClient(clientset),
		)
		if transitionErr != nil {
			return fmt.Errorf("configure controller RBAC transition: %w", transitionErr)
		}
		stateClients, stateClientErr := newStoredControllerStateClients(config)
		if stateClientErr != nil {
			return stateClientErr
		}
		err = manager.ReconcileWithStatePreflightAndPrepare(
			ctx,
			stateClients,
			int64(controllerstate.CurrentVersion),
			func(prepareCtx context.Context) error {
				if readyErr := serviceAccountObjectGuard.WaitReady(prepareCtx); readyErr != nil {
					return fmt.Errorf("wait for stable ServiceAccount object guard: %w", readyErr)
				}
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
				if verifyErr := rollout.Verify(prepareCtx); verifyErr != nil {
					return fmt.Errorf("verify retained rollout guards before admission convergence: %w", verifyErr)
				}
				admissionGuard := crdupgrade.NewAdmissionConvergenceGuard(rollout)
				activationState, verifyErr := admissionGuard.VerifyPreCutover(prepareCtx)
				if verifyErr != nil {
					return fmt.Errorf("verify pre-cutover admission convergence sentinel: %w", verifyErr)
				}
				if preflightErr := controllerRBACTransition.Preflight(prepareCtx); preflightErr != nil {
					return fmt.Errorf("preflight exact controller RBAC transition: %w", preflightErr)
				}
				protectedPodsRemain, podInventoryErr := inventory.ProtectedRuntimePodsRemain(prepareCtx)
				if podInventoryErr != nil {
					return fmt.Errorf("inventory protected runtime Pods before credential decision: %w", podInventoryErr)
				}
				requiresCredentialGrace, graceErr := controllerRBACTransition.RequiresCredentialGrace(activationState, protectedPodsRemain)
				if graceErr != nil {
					return fmt.Errorf("decide controller credential grace from durable preflight state: %w", graceErr)
				}
				if requiresCredentialGrace {
					activationState, graceErr = rollout.BeginControllerCredentialDrain(prepareCtx)
					if graceErr != nil {
						return fmt.Errorf("begin controller credential drain: %w", graceErr)
					}
				}

				// The final sentinel is ordered after every retained release guard.
				// Its tuple-specific denial proves its own controller credential
				// fence on every directly addressed API server before quiescence or
				// any RBAC grant moves.
				admissionBarrier, barrierErr := newExpectedAdmissionConvergenceBarrier(
					prepareCtx,
					config,
					clientset.DiscoveryV1().EndpointSlices(metav1.NamespaceDefault),
					admissionGuard,
					serviceAccountObjectGuard,
					activationState,
				)
				if barrierErr != nil {
					return barrierErr
				}
				if barrierErr = admissionBarrier.Wait(prepareCtx); barrierErr != nil {
					return fmt.Errorf("wait for admission convergence on every API server: %w", barrierErr)
				}
				if verifyErr := rollout.Verify(prepareCtx); verifyErr != nil {
					return fmt.Errorf("re-verify retained rollout guards after admission convergence: %w", verifyErr)
				}
				if verifyErr := serviceAccountObjectGuard.Verify(prepareCtx); verifyErr != nil {
					return fmt.Errorf("re-verify stable ServiceAccount object guard after admission convergence: %w", verifyErr)
				}
				if verifyErr := admissionGuard.VerifyState(prepareCtx, activationState); verifyErr != nil {
					return fmt.Errorf("re-verify admission convergence sentinel: %w", verifyErr)
				}
				if sealErr := predecessorRetirement.SealCurrent(prepareCtx); sealErr != nil {
					return fmt.Errorf("seal current admission inventory: %w", sealErr)
				}
				if verifyErr := predecessorRetirement.VerifyCurrentSealed(prepareCtx); verifyErr != nil {
					return fmt.Errorf("verify sealed current admission inventory: %w", verifyErr)
				}
				sealedAdmissionBarrier, barrierErr := newSealedAdmissionConvergenceBarrier(
					prepareCtx,
					config,
					clientset.DiscoveryV1().EndpointSlices(metav1.NamespaceDefault),
					admissionGuard,
					serviceAccountObjectGuard,
					activationState,
				)
				if barrierErr != nil {
					return barrierErr
				}
				if barrierErr = sealedAdmissionBarrier.Wait(prepareCtx); barrierErr != nil {
					return fmt.Errorf("wait for sealed admission convergence on every API server: %w", barrierErr)
				}
				if verifyErr := rollout.Verify(prepareCtx); verifyErr != nil {
					return fmt.Errorf("re-verify retained rollout guards after sealed admission convergence: %w", verifyErr)
				}
				if verifyErr := serviceAccountObjectGuard.Verify(prepareCtx); verifyErr != nil {
					return fmt.Errorf("re-verify stable ServiceAccount object guard after sealed admission convergence: %w", verifyErr)
				}
				if verifyErr := predecessorRetirement.VerifyCurrentSealed(prepareCtx); verifyErr != nil {
					return fmt.Errorf("re-verify sealed current admission inventory after endpoint convergence: %w", verifyErr)
				}
				if verifyErr := admissionGuard.VerifySealedState(prepareCtx, activationState); verifyErr != nil {
					return fmt.Errorf("re-verify sealed admission convergence sentinel: %w", verifyErr)
				}
				if preflightErr := predecessorRetirement.Preflight(prepareCtx); preflightErr != nil {
					return fmt.Errorf("preflight predecessor admission retirement: %w", preflightErr)
				}
				if quiesceErr := rollout.Quiesce(prepareCtx); quiesceErr != nil {
					return quiesceErr
				}
				if cutoverErr := completeControllerRBACCutover(
					prepareCtx,
					func(cutoverCtx context.Context) error {
						return waitForNoProtectedRuntimePods(cutoverCtx, inventory, rollout.PollEvery)
					},
					controllerRBACTransition,
					requiresCredentialGrace,
					func(cutoverCtx context.Context) error {
						observer, observeErr := newProtectedRuntimePodStabilityObserver(
							inventory,
							clientset.CoreV1().Pods(rollout.ReleaseNamespace),
							rollout,
						)
						if observeErr != nil {
							return observeErr
						}
						return sealedAdmissionBarrier.WaitWithStabilityObserver(
							cutoverCtx,
							retiredCredentialRevocationDelay,
							observer,
						)
					},
					func(cutoverCtx context.Context) (authorizationConvergenceWaiter, error) {
						return newControllerRBACConvergenceBarrier(cutoverCtx, config, clientset, controllerRBACTransition)
					},
				); cutoverErr != nil {
					return cutoverErr
				}
				// Re-read quota and admission inputs after the old runtime is fully
				// stopped and predecessor authorization is denied on every advertised
				// API server. Activation remains strictly after the RBAC identity
				// snapshot has been reverified.
				if quotaErr := quotaPreflight.WaitForCapacityAfterQuiesce(prepareCtx, rollout.PollEvery); quotaErr != nil {
					return fmt.Errorf("recheck runtime ResourceQuota capacity before activation: %w", quotaErr)
				}
				if admissionErr := admissionPreflight.Check(prepareCtx); admissionErr != nil {
					return fmt.Errorf("recheck runtime Pod admission before activation: %w", admissionErr)
				}
				if verifyErr := serviceAccountObjectGuard.Verify(prepareCtx); verifyErr != nil {
					return fmt.Errorf("re-verify stable ServiceAccount object guard before activation: %w", verifyErr)
				}
				if verifyErr := predecessorRetirement.VerifyCurrentSealed(prepareCtx); verifyErr != nil {
					return fmt.Errorf("re-verify sealed current admission inventory before activation: %w", verifyErr)
				}
				if verifyErr := admissionGuard.VerifySealedState(prepareCtx, activationState); verifyErr != nil {
					return fmt.Errorf("re-verify sealed admission convergence state before activation: %w", verifyErr)
				}
				if activateErr := rollout.Activate(prepareCtx); activateErr != nil {
					return activateErr
				}
				if retireErr := predecessorRetirement.Retire(
					prepareCtx,
					newPredecessorRetirementAdmissionBarrier(
						config,
						clientset.DiscoveryV1().EndpointSlices(metav1.NamespaceDefault),
						admissionGuard,
						predecessorRetirement,
					),
				); retireErr != nil {
					return fmt.Errorf("retire predecessor admission inventory: %w", retireErr)
				}
				if adoptErr := adopter.Adopt(prepareCtx); adoptErr != nil {
					return fmt.Errorf("adopt admission singleton: %w", adoptErr)
				}
				return nil
			},
		)
	case "teardown-retirement-probe-a", "teardown-retirement-gate", "teardown-quiesce", "teardown", "teardown-retirement-final":
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
			controllerServiceAccountManaged,
			*previousControllerServiceAccountName,
			types.UID(*previousControllerServiceAccountUID),
			previousControllerServiceAccountManaged,
			*controllerDeploymentName,
			*certificateDeploymentName,
			int32(*releaseSequence),
			int32(*previousControllerReleaseSequence),
		)
		if expectedErr != nil {
			return expectedErr
		}
		clientset, clientErr := kubernetes.NewForConfig(config)
		if clientErr != nil {
			return fmt.Errorf("create Kubernetes client: %w", clientErr)
		}
		rollout := newRolloutGuard(clientset, expected, *managerImage, *webhookSecretName, int32(*webhookPort), int32(*certificateHealthPort), int32(*controllerReplicas), controllerRuntimeArgs, certificateRuntimeArgs, runtimeDeploymentConfigExpressions, runtimePodConfigExpressions, runtimeAdmissionContract, *runtimeAdmissionContractB64)
		err = runTeardownMode(ctx, mode, config, clientset, rollout, runtimeAdmissionContract)
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
			controllerServiceAccountManaged,
			*previousControllerServiceAccountName,
			types.UID(*previousControllerServiceAccountUID),
			previousControllerServiceAccountManaged,
			*controllerDeploymentName,
			*certificateDeploymentName,
			int32(*releaseSequence),
			int32(*previousControllerReleaseSequence),
		)
		if expectedErr != nil {
			return expectedErr
		}
		clientset, clientErr := kubernetes.NewForConfig(config)
		if clientErr != nil {
			return fmt.Errorf("create Kubernetes client: %w", clientErr)
		}
		rollout := newRolloutGuard(clientset, expected, *managerImage, *webhookSecretName, int32(*webhookPort), int32(*certificateHealthPort), int32(*controllerReplicas), controllerRuntimeArgs, certificateRuntimeArgs, runtimeDeploymentConfigExpressions, runtimePodConfigExpressions, runtimeAdmissionContract, *runtimeAdmissionContractB64)
		if verifyErr := rollout.Verify(ctx); verifyErr != nil {
			return fmt.Errorf("verify persistent rollout guards: %w", verifyErr)
		}
		serviceAccountObjectGuard := crdupgrade.NewServiceAccountObjectGuard(rollout)
		if verifyErr := serviceAccountObjectGuard.WaitReady(ctx); verifyErr != nil {
			return fmt.Errorf("wait for stable ServiceAccount object guard: %w", verifyErr)
		}
		admissionGuard := crdupgrade.NewAdmissionConvergenceGuard(rollout)
		admissionBarrier, barrierErr := newRuntimeAdmissionConvergenceBarrier(
			ctx,
			config,
			clientset.DiscoveryV1().EndpointSlices(metav1.NamespaceDefault),
			admissionGuard,
			serviceAccountObjectGuard,
		)
		if barrierErr != nil {
			return barrierErr
		}
		if barrierErr = admissionBarrier.Wait(ctx); barrierErr != nil {
			return fmt.Errorf("wait for runtime admission convergence on every API server: %w", barrierErr)
		}
		if verifyErr := rollout.Verify(ctx); verifyErr != nil {
			return fmt.Errorf("re-verify persistent rollout guards after admission convergence: %w", verifyErr)
		}
		if verifyErr := admissionGuard.VerifyRuntime(ctx); verifyErr != nil {
			return fmt.Errorf("re-verify runtime admission convergence sentinel: %w", verifyErr)
		}
		if *verifyCertificateRecovery {
			recoveryBarrier, recoveryErr := newCertificateRecoveryConvergenceBarrier(
				ctx,
				config,
				clientset.DiscoveryV1().EndpointSlices(metav1.NamespaceDefault),
				expected.ReleaseNamespace,
				expected.CertificateDeploymentName,
				expected.CertificateDeploymentName,
				*webhookSecretName,
			)
			if recoveryErr != nil {
				return fmt.Errorf("configure optional certificate recovery convergence: %w", recoveryErr)
			}
			if recoveryErr = recoveryBarrier.Wait(ctx); recoveryErr != nil {
				return fmt.Errorf("wait for optional certificate recovery admission on every API server: %w", recoveryErr)
			}
		}
		admissionPreflight, preflightErr := newRuntimeAdmissionPreflight(clientset, expected, runtimeAdmissionContract)
		if preflightErr != nil {
			return preflightErr
		}
		if preflightErr = admissionPreflight.Check(ctx); preflightErr != nil {
			return fmt.Errorf("preflight runtime Pod admission: %w", preflightErr)
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
	if mode == "teardown-retirement-probe-a" {
		_, err = fmt.Fprintln(output, "first teardown retirement fence is ready and enforcing")
		return err
	}
	if mode == "teardown-retirement-gate" {
		_, err = fmt.Fprintln(output, "teardown retirement fences and target inventory are ready")
		return err
	}
	if mode == "teardown-quiesce" {
		_, err = fmt.Fprintln(output, "release teardown inventory verified and runtime quiesced")
		return err
	}
	if mode == "teardown" {
		_, err = fmt.Fprintln(output, "release privilege revoked and admission retirement authorized")
		return err
	}
	if mode == "teardown-retirement-final" {
		_, err = fmt.Fprintln(output, "release admission retirement finalized")
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
	hookServiceAccountName, controllerServiceAccountName string, controllerServiceAccountManaged bool,
	previousControllerServiceAccountName string, previousControllerServiceAccountUID types.UID,
	previousControllerServiceAccountManaged bool, controllerDeploymentName,
	certificateDeploymentName string, releaseSequence, previousControllerReleaseSequence int32,
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
	if previousControllerServiceAccountName != "" && previousControllerServiceAccountName == controllerServiceAccountName {
		return crdupgrade.RuntimeInvariants{}, fmt.Errorf("candidate and previous controller ServiceAccount names must differ")
	}
	if previousControllerServiceAccountName == "" {
		if previousControllerServiceAccountUID != "" || previousControllerServiceAccountManaged {
			return crdupgrade.RuntimeInvariants{}, fmt.Errorf("previous controller ServiceAccount UID and ownership require a previous name")
		}
	} else if previousControllerServiceAccountUID == "" {
		return crdupgrade.RuntimeInvariants{}, fmt.Errorf("previous controller ServiceAccount UID is required")
	}
	return crdupgrade.RuntimeInvariants{
		ReleaseName:                             releaseName,
		ReleaseNamespace:                        releaseNamespace,
		CoordinationNamespace:                   coordinationNamespace,
		LeaderElection:                          leaderElectionEnabled,
		LeaderElectionID:                        leaderElectionID,
		WebhookServiceName:                      webhookServiceName,
		WebhookTimeoutSeconds:                   int32(webhookTimeoutSeconds),
		HookServiceAccountName:                  hookServiceAccountName,
		ControllerServiceAccountName:            controllerServiceAccountName,
		ControllerServiceAccountManaged:         controllerServiceAccountManaged,
		PreviousControllerServiceAccountName:    previousControllerServiceAccountName,
		PreviousControllerServiceAccountUID:     previousControllerServiceAccountUID,
		PreviousControllerServiceAccountManaged: previousControllerServiceAccountManaged,
		PreviousControllerReleaseSequence:       previousControllerReleaseSequence,
		ControllerDeploymentName:                controllerDeploymentName,
		CertificateDeploymentName:               certificateDeploymentName,
		ControllerStateVersion:                  controllerstate.CurrentVersion,
		AdmissionContractVersion:                crdupgrade.CurrentAdmissionContractVersion,
		ReleaseSequence:                         releaseSequence,
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
		Policies:                                clientset.AdmissionregistrationV1().ValidatingAdmissionPolicies(),
		Bindings:                                clientset.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings(),
		Deployments:                             clientset.AppsV1().Deployments(expected.ReleaseNamespace),
		Pods:                                    clientset.CoreV1().Pods(expected.ReleaseNamespace),
		ConfigMaps:                              clientset.CoreV1().ConfigMaps(expected.ReleaseNamespace),
		ConfigMapDeleter:                        clientset.CoreV1().ConfigMaps(expected.ReleaseNamespace),
		ReleaseName:                             expected.ReleaseName,
		ReleaseNamespace:                        expected.ReleaseNamespace,
		CoordinationNamespace:                   expected.CoordinationNamespace,
		LeaderElection:                          expected.LeaderElection,
		LeaderElectionID:                        expected.LeaderElectionID,
		WebhookServiceName:                      expected.WebhookServiceName,
		WebhookTimeoutSeconds:                   expected.WebhookTimeoutSeconds,
		WebhookSecretName:                       webhookSecretName,
		WebhookPort:                             webhookPort,
		CertificateHealthPort:                   certificateHealthPort,
		HookServiceAccountName:                  expected.HookServiceAccountName,
		ControllerServiceAccountName:            expected.ControllerServiceAccountName,
		ControllerServiceAccountManaged:         expected.ControllerServiceAccountManaged,
		PreviousControllerServiceAccountName:    expected.PreviousControllerServiceAccountName,
		PreviousControllerServiceAccountUID:     expected.PreviousControllerServiceAccountUID,
		PreviousControllerServiceAccountManaged: expected.PreviousControllerServiceAccountManaged,
		PreviousControllerReleaseSequence:       expected.PreviousControllerReleaseSequence,
		ControllerDeploymentName:                expected.ControllerDeploymentName,
		ControllerReplicas:                      controllerReplicas,
		CertificateDeploymentName:               expected.CertificateDeploymentName,
		ControllerStateVersion:                  expected.ControllerStateVersion,
		AdmissionContractVersion:                expected.AdmissionContractVersion,
		ReleaseSequence:                         expected.ReleaseSequence,
		ManagerImage:                            managerImage,
		ControllerArgs:                          append([]string(nil), controllerArgs...),
		CertificateArgs:                         append([]string(nil), certificateArgs...),
		RuntimeDeploymentConfigExpressions:      append([]string(nil), runtimeDeploymentConfigExpressions...),
		RuntimePodConfigExpressions:             append([]string(nil), runtimePodConfigExpressions...),
		PriorityClassName:                       runtimeAdmissionContract.PriorityClassName,
		RuntimeAdmissionContractB64:             runtimeAdmissionContractB64,
		PollEvery:                               500 * time.Millisecond,
	}
}

func runTeardownMode(
	ctx context.Context,
	mode string,
	config *rest.Config,
	clientset kubernetes.Interface,
	rollout *crdupgrade.RolloutGuard,
	contract crdupgrade.RuntimeAdmissionContract,
) error {
	if ctx == nil || config == nil || clientset == nil || rollout == nil {
		return errors.New("teardown mode dependencies are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	guard, err := newConfiguredTeardownRetirementGuard(rollout, contract)
	if err != nil {
		return fmt.Errorf("configure teardown retirement: %w", err)
	}
	admission := clientset.AdmissionregistrationV1()
	policies := admission.ValidatingAdmissionPolicies()
	bindings := admission.ValidatingAdmissionPolicyBindings()
	configMaps := clientset.CoreV1().ConfigMaps(rollout.ReleaseNamespace)
	phase, err := guard.Phase(ctx, configMaps)
	if err != nil {
		return fmt.Errorf("derive teardown retirement phase: %w", err)
	}

	switch mode {
	case "teardown-retirement-probe-a":
		if err := guard.VerifyOriginalFences(ctx, policies, bindings, crdupgrade.TeardownFenceA); err != nil {
			return fmt.Errorf("verify first teardown retirement fence: %w", err)
		}
		barrier, err := newTeardownRetirementFenceBarrier(
			ctx,
			config,
			clientset.DiscoveryV1().EndpointSlices(metav1.NamespaceDefault),
			guard,
			policies,
			bindings,
			crdupgrade.TeardownFenceA,
		)
		if err != nil {
			return fmt.Errorf("prepare first teardown retirement fence barrier: %w", err)
		}
		if err := bindTeardownRetirementPhase(barrier, guard, configMaps, phase, nil); err != nil {
			return err
		}
		if err := barrier.Wait(ctx); err != nil {
			return fmt.Errorf("wait for first teardown retirement fence on every API server: %w", err)
		}
		return nil

	case "teardown-retirement-gate":
		if err := verifyTeardownRetirementTransitionState(ctx, guard, configMaps, policies, bindings, phase, false); err != nil {
			return fmt.Errorf("preflight teardown retirement gate: %w", err)
		}
		barrier, err := newTeardownRetirementFenceBarrier(
			ctx,
			config,
			clientset.DiscoveryV1().EndpointSlices(metav1.NamespaceDefault),
			guard,
			policies,
			bindings,
			crdupgrade.TeardownFenceA,
			crdupgrade.TeardownFenceB,
		)
		if err != nil {
			return fmt.Errorf("prepare teardown retirement gate barrier: %w", err)
		}
		if err := bindTeardownRetirementPhase(barrier, guard, configMaps, phase, func(verifyCtx context.Context) error {
			_, verifyErr := guard.PreflightPairsForPhase(verifyCtx, policies, bindings, phase)
			return verifyErr
		}); err != nil {
			return err
		}
		if err := barrier.Wait(ctx); err != nil {
			return fmt.Errorf("wait for teardown retirement gate on every API server: %w", err)
		}
		return nil

	case "teardown-quiesce":
		if err := verifyTeardownRetirementTransitionState(ctx, guard, configMaps, policies, bindings, phase, false); err != nil {
			return fmt.Errorf("preflight teardown retirement state before quiescence: %w", err)
		}
		_, privilegeTeardown, err := newTeardownPhases(clientset, rollout, contract)
		if err != nil {
			return err
		}
		inventory := newWorkloadInventory(clientset, rollout)
		if err := privilegeTeardown.Preflight(ctx); err != nil {
			return fmt.Errorf("preflight exact privilege teardown inventory: %w", err)
		}
		if err := inventory.VerifyRuntimeBeforeQuiesce(ctx); err != nil {
			return fmt.Errorf("verify pre-staged teardown workloads: %w", err)
		}
		if phase == crdupgrade.TeardownRetirementActive {
			if _, err := rollout.BeginControllerCredentialDrain(ctx); err != nil {
				return fmt.Errorf("begin teardown controller credential drain: %w", err)
			}
		}
		barrier, err := newTeardownRetirementFenceBarrier(
			ctx,
			config,
			clientset.DiscoveryV1().EndpointSlices(metav1.NamespaceDefault),
			guard,
			policies,
			bindings,
			crdupgrade.TeardownFenceA,
			crdupgrade.TeardownFenceB,
		)
		if err != nil {
			return fmt.Errorf("prepare teardown credential admission fence: %w", err)
		}
		if err := bindTeardownRetirementPhase(barrier, guard, configMaps, phase, func(verifyCtx context.Context) error {
			return verifyTeardownRetirementTransitionInventory(verifyCtx, guard, configMaps, policies, bindings, phase, true)
		}); err != nil {
			return err
		}
		if err := barrier.Wait(ctx); err != nil {
			return fmt.Errorf("wait for teardown credential admission fence on every API server: %w", err)
		}
		if err := rollout.Quiesce(ctx); err != nil {
			return fmt.Errorf("quiesce release runtime: %w", err)
		}
		if err := waitForNoProtectedRuntimePods(ctx, inventory, rollout.PollEvery); err != nil {
			return fmt.Errorf("wait for namespace-wide runtime Pod quiescence: %w", err)
		}
		return nil

	case "teardown":
		if err := verifyTeardownRetirementTransitionState(ctx, guard, configMaps, policies, bindings, phase, true); err != nil {
			return fmt.Errorf("preflight teardown retirement state before privilege removal: %w", err)
		}
		_, privilegeTeardown, err := newTeardownPhases(clientset, rollout, contract)
		if err != nil {
			return err
		}
		if err := privilegeTeardown.Preflight(ctx); err != nil {
			return fmt.Errorf("preflight exact privilege teardown inventory: %w", err)
		}
		barrier, err := newTeardownRetirementFenceBarrier(
			ctx,
			config,
			clientset.DiscoveryV1().EndpointSlices(metav1.NamespaceDefault),
			guard,
			policies,
			bindings,
			crdupgrade.TeardownFenceA,
			crdupgrade.TeardownFenceB,
		)
		if err != nil {
			return fmt.Errorf("prepare teardown retirement admission barrier: %w", err)
		}
		if err := bindTeardownRetirementPhase(barrier, guard, configMaps, phase, func(verifyCtx context.Context) error {
			return verifyTeardownRetirementTransitionInventory(verifyCtx, guard, configMaps, policies, bindings, phase, true)
		}); err != nil {
			return err
		}
		convergence, err := newTeardownRBACConvergenceBarrier(ctx, config, clientset, rollout, contract)
		if err != nil {
			return fmt.Errorf("prepare API-server authorization convergence barrier: %w", err)
		}
		if err := convergence.Validate(); err != nil {
			return fmt.Errorf("validate API-server authorization convergence barrier: %w", err)
		}
		if err := verifyTeardownRetirementTransitionState(ctx, guard, configMaps, policies, bindings, phase, true); err != nil {
			return fmt.Errorf("recheck teardown retirement state before privilege removal: %w", err)
		}
		if err := privilegeTeardown.Teardown(ctx); err != nil {
			return fmt.Errorf("remove release privilege: %w", err)
		}
		inventory := newWorkloadInventory(clientset, rollout)
		podObserver, err := newProtectedRuntimePodStabilityObserver(
			inventory,
			clientset.CoreV1().Pods(rollout.ReleaseNamespace),
			rollout,
		)
		if err != nil {
			return fmt.Errorf("prepare protected runtime Pod stability observer: %w", err)
		}
		credentialObserver, err := newCredentialRevocationStabilityObserver(
			convergence,
			podObserver,
			privilegeTeardown.Preflight,
		)
		if err != nil {
			return fmt.Errorf("prepare joint teardown credential revocation observer: %w", err)
		}
		if err := barrier.WaitWithStabilityObserver(ctx, retiredCredentialRevocationDelay, credentialObserver); err != nil {
			return fmt.Errorf("wait for uninterrupted teardown admission, authorization, stored-state, and protected-Pod fence: %w", err)
		}
		return nil

	case "teardown-retirement-final":
		if err := verifyTeardownRetirementFinalState(ctx, guard, configMaps, policies, bindings, phase); err != nil {
			return fmt.Errorf("preflight final teardown retirement state: %w", err)
		}
		_, privilegeTeardown, err := newTeardownPhases(clientset, rollout, contract)
		if err != nil {
			return err
		}
		if err := privilegeTeardown.Preflight(ctx); err != nil {
			return fmt.Errorf("preflight exact privilege teardown inventory: %w", err)
		}
		barrier, err := newTeardownRetirementFinalBarrier(
			ctx,
			config,
			clientset.DiscoveryV1().EndpointSlices(metav1.NamespaceDefault),
			guard,
			policies,
			bindings,
		)
		if err != nil {
			return fmt.Errorf("prepare final teardown retirement admission barrier: %w", err)
		}
		if err := bindTeardownRetirementPhase(barrier, guard, configMaps, phase, func(verifyCtx context.Context) error {
			return verifyTeardownRetirementDrainAuthorization(verifyCtx, guard, configMaps, phase)
		}); err != nil {
			return err
		}
		convergence, err := newTeardownRBACConvergenceBarrier(ctx, config, clientset, rollout, contract)
		if err != nil {
			return fmt.Errorf("prepare final API-server authorization convergence barrier: %w", err)
		}
		if err := convergence.Validate(); err != nil {
			return fmt.Errorf("validate final API-server authorization convergence barrier: %w", err)
		}
		inventory := newWorkloadInventory(clientset, rollout)
		podObserver, err := newProtectedRuntimePodStabilityObserver(
			inventory,
			clientset.CoreV1().Pods(rollout.ReleaseNamespace),
			rollout,
		)
		if err != nil {
			return fmt.Errorf("prepare final protected runtime Pod stability observer: %w", err)
		}
		credentialObserver, err := newCredentialRevocationStabilityObserver(
			convergence,
			podObserver,
			privilegeTeardown.Preflight,
		)
		if err != nil {
			return fmt.Errorf("prepare final teardown credential revocation observer: %w", err)
		}
		if err := barrier.WaitWithStabilityObserver(ctx, retiredCredentialRevocationDelay, credentialObserver); err != nil {
			return fmt.Errorf("wait for uninterrupted final teardown admission, authorization, stored-state, and protected-Pod fence: %w", err)
		}
		if err := verifyTeardownRetirementFinalState(ctx, guard, configMaps, policies, bindings, phase); err != nil {
			return fmt.Errorf("recheck final teardown retirement state: %w", err)
		}
		if err := privilegeTeardown.Preflight(ctx); err != nil {
			return fmt.Errorf("recheck exact privilege teardown inventory: %w", err)
		}
		retirementObserver, err := newTeardownRetirementCredentialObserver(
			ctx,
			config,
			clientset.DiscoveryV1().EndpointSlices(kubernetesServiceNamespace),
			guard,
		)
		if err != nil {
			return fmt.Errorf("freeze API endpoints for cleanup credential retirement: %w", err)
		}
		defer retirementObserver.Close()
		if err := retirementObserver.verifyTopology(); err != nil {
			return fmt.Errorf("verify frozen API endpoint topology before finalization: %w", err)
		}
		convergenceMarker, err := crdupgrade.NewAdmissionConvergenceGuard(rollout).MarkerTarget()
		if err != nil {
			return fmt.Errorf("derive admission convergence retirement marker: %w", err)
		}
		readinessMarker, err := crdupgrade.NewParentWorkloadGuard(rollout).ReadinessMarkerTarget()
		if err != nil {
			return fmt.Errorf("derive parent origin readiness retirement marker: %w", err)
		}
		finalizer, err := newTeardownRetirementFinalizer(configMaps, guard, convergenceMarker, readinessMarker)
		if err != nil {
			return fmt.Errorf("configure teardown retirement finalizer: %w", err)
		}
		if err := finalizer.Finalize(ctx); err != nil {
			return fmt.Errorf("finalize teardown retirement: %w", err)
		}
		if err := retirementObserver.verifyTopology(); err != nil {
			return fmt.Errorf("verify frozen API endpoint topology before cleanup credential retirement: %w", err)
		}
		if err := privilegeTeardown.RetireCleanupServiceAccount(ctx); err != nil {
			return fmt.Errorf("retire cleanup ServiceAccount: %w", err)
		}
		if err := retirementObserver.Wait(ctx); err != nil {
			return fmt.Errorf("wait for cleanup credential retirement on every frozen API endpoint: %w", err)
		}
		return nil

	default:
		return fmt.Errorf("unsupported teardown mode %q", mode)
	}
}

func bindTeardownRetirementPhase(
	barrier *admissionConvergenceBarrier,
	guard *crdupgrade.TeardownRetirementGuard,
	activation crdupgrade.TeardownRetirementActivationReader,
	phase crdupgrade.TeardownRetirementPhase,
	verifyAdditional func(context.Context) error,
) error {
	if barrier == nil || guard == nil || activation == nil || barrier.verifyStored == nil {
		return errors.New("teardown retirement phase barrier dependencies are required")
	}
	if phase != crdupgrade.TeardownRetirementActive && phase != crdupgrade.TeardownRetirementTerminal {
		return fmt.Errorf("unknown teardown retirement phase %q", phase)
	}
	verifyStored := barrier.verifyStored
	barrier.verifyStored = func(ctx context.Context) error {
		if err := verifyTeardownRetirementPhase(ctx, guard, activation, phase); err != nil {
			return err
		}
		if err := verifyStored(ctx); err != nil {
			return err
		}
		if verifyAdditional != nil {
			if err := verifyAdditional(ctx); err != nil {
				return err
			}
		}
		return verifyTeardownRetirementPhase(ctx, guard, activation, phase)
	}
	return nil
}

func verifyTeardownRetirementPhase(
	ctx context.Context,
	guard *crdupgrade.TeardownRetirementGuard,
	activation crdupgrade.TeardownRetirementActivationReader,
	want crdupgrade.TeardownRetirementPhase,
) error {
	phase, err := guard.Phase(ctx, activation)
	if err != nil {
		return err
	}
	if phase != want {
		return fmt.Errorf("teardown retirement phase changed from %q to %q", want, phase)
	}
	return nil
}

func verifyTeardownRetirementTransitionState(
	ctx context.Context,
	guard *crdupgrade.TeardownRetirementGuard,
	activation crdupgrade.TeardownRetirementActivationReader,
	policies crdupgrade.ValidatingAdmissionPolicyReader,
	bindings crdupgrade.ValidatingAdmissionPolicyBindingReader,
	phase crdupgrade.TeardownRetirementPhase,
	requireDrain bool,
) error {
	if err := verifyTeardownRetirementPhase(ctx, guard, activation, phase); err != nil {
		return err
	}
	if err := guard.VerifyOriginalFences(
		ctx,
		policies,
		bindings,
		crdupgrade.TeardownFenceA,
		crdupgrade.TeardownFenceB,
	); err != nil {
		return err
	}
	if err := verifyTeardownRetirementTransitionInventory(ctx, guard, activation, policies, bindings, phase, requireDrain); err != nil {
		return err
	}
	return verifyTeardownRetirementPhase(ctx, guard, activation, phase)
}

func verifyTeardownRetirementTransitionInventory(
	ctx context.Context,
	guard *crdupgrade.TeardownRetirementGuard,
	activation crdupgrade.TeardownRetirementActivationReader,
	policies crdupgrade.ValidatingAdmissionPolicyReader,
	bindings crdupgrade.ValidatingAdmissionPolicyBindingReader,
	phase crdupgrade.TeardownRetirementPhase,
	requireDrain bool,
) error {
	if requireDrain {
		if err := verifyTeardownRetirementDrainAuthorization(ctx, guard, activation, phase); err != nil {
			return err
		}
	}
	_, err := guard.PreflightPairsForPhase(ctx, policies, bindings, phase)
	return err
}

func verifyTeardownRetirementDrainAuthorization(
	ctx context.Context,
	guard *crdupgrade.TeardownRetirementGuard,
	activation crdupgrade.TeardownRetirementActivationReader,
	phase crdupgrade.TeardownRetirementPhase,
) error {
	switch phase {
	case crdupgrade.TeardownRetirementTerminal:
		return nil
	case crdupgrade.TeardownRetirementActive:
	default:
		return fmt.Errorf("unknown teardown retirement phase %q", phase)
	}
	object, err := activation.Get(ctx, crdupgrade.ReleaseActivationName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get teardown retirement drain authorization: %w", err)
	}
	return guard.VerifyFinalActivation(object)
}

func verifyTeardownRetirementFinalState(
	ctx context.Context,
	guard *crdupgrade.TeardownRetirementGuard,
	activation crdupgrade.TeardownRetirementActivationReader,
	policies crdupgrade.ValidatingAdmissionPolicyReader,
	bindings crdupgrade.ValidatingAdmissionPolicyBindingReader,
	phase crdupgrade.TeardownRetirementPhase,
) error {
	if err := verifyTeardownRetirementPhase(ctx, guard, activation, phase); err != nil {
		return err
	}
	if err := verifyTeardownRetirementDrainAuthorization(ctx, guard, activation, phase); err != nil {
		return err
	}
	if err := guard.VerifyOriginalFences(
		ctx,
		policies,
		bindings,
		crdupgrade.TeardownFenceA,
		crdupgrade.TeardownFenceB,
	); err != nil {
		return err
	}
	if err := guard.VerifyRetiredPairs(ctx, policies, bindings); err != nil {
		return err
	}
	if err := verifyTeardownRetirementDrainAuthorization(ctx, guard, activation, phase); err != nil {
		return err
	}
	return verifyTeardownRetirementPhase(ctx, guard, activation, phase)
}

func validateModeFlags(mode string, flags *flag.FlagSet) error {
	allowed := map[string]struct{}{}
	switch mode {
	case "image-check":
		allowed["release-sequence"] = struct{}{}
		allowed["manager-image"] = struct{}{}
	case "verify":
		allowed["timeout"] = struct{}{}
	case "identity-probe", "preflight", "reconcile", "teardown-retirement-probe-a", "teardown-retirement-gate", "teardown-quiesce", "teardown", "teardown-retirement-final", "runtime-verify":
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
			"controller-service-account-managed",
			"previous-controller-service-account-name",
			"previous-controller-service-account-uid",
			"previous-controller-service-account-managed",
			"previous-controller-release-sequence",
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
			allowed["verify-certificate-recovery"] = struct{}{}
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

func waitForRetiredBoundCredentialRevocationSince(
	ctx context.Context,
	convergedAt time.Time,
	delay time.Duration,
	now func() time.Time,
) error {
	if ctx == nil {
		return errors.New("retired credential revocation context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if convergedAt.IsZero() {
		return errors.New("controller credential drain convergence time is required")
	}
	if delay <= 0 {
		return errors.New("retired credential revocation delay must be positive")
	}
	if now == nil {
		return errors.New("controller credential grace clock is required")
	}
	remaining := convergedAt.Add(delay).Sub(now())
	if remaining <= 0 {
		return nil
	}
	return waitForRetiredBoundCredentialRevocation(ctx, remaining)
}

func parseExactBooleanFlag(value, name string) (bool, error) {
	switch value {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be exactly true or false", name)
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
