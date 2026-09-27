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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/stokaro/ptah-operator/internal/controllerstate"
	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

const (
	defaultTimeout = 2 * time.Minute
	supportedModes = "image-check, identity-probe, preflight, reconcile, teardown-quiesce, verify, or runtime-verify"
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
// message from. Every hook container declares it and the policy that reads it.
var terminationMessageFile = "/dev/termination-log"

// terminationMessageLimit is what Kubernetes keeps of that file.
const terminationMessageLimit = 4096

// reportTerminationMessage puts the refusal where the person who ran Helm can
// read it. A hook that refuses prints its reason to stderr, which stays inside
// the Pod: Helm reports only that the Job failed, so an operator is told that
// an uninstall was refused and never why. Writing the same reason to the
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
	previousControllerManagerImage := flags.String("previous-controller-manager-image", "", "manager image of the release sequence active before candidate cutover")
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
	if mode != "image-check" && mode != "identity-probe" && mode != "preflight" && mode != "reconcile" && mode != "teardown-quiesce" && mode != "verify" && mode != "runtime-verify" {
		return fmt.Errorf("unsupported mode %q: use %s", mode, supportedModes)
	}
	if err := validateModeFlags(mode, flags); err != nil {
		return err
	}
	if mode != "runtime-verify" && *verifyControllerState {
		return fmt.Errorf("verify-controller-state is valid only in runtime-verify mode")
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
	config = boundedSweepRESTConfig(config)
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
			*previousControllerManagerImage,
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
			*previousControllerManagerImage,
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
		converged, convergedErr := rollout.CandidateRuntimeConverged(ctx)
		if convergedErr != nil {
			err = fmt.Errorf("inspect candidate runtime convergence: %w", convergedErr)
			break
		}
		if converged {
			// The candidate is already the active release and its runtime is
			// up, so there is no stop transition to dry-run: the retained
			// runtime guard admits a stop only toward a newer release.
			_, err = fmt.Fprintf(output, "candidate release %d is already active with a converged runtime; no stop transition to preflight\n", expected.ReleaseSequence)
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
			*previousControllerManagerImage,
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
				activationState, stateErr := rollout.ReleaseActivationState(prepareCtx)
				if stateErr != nil {
					return fmt.Errorf("read release activation state before cutover: %w", stateErr)
				}
				if preflightErr := controllerRBACTransition.Preflight(prepareCtx); preflightErr != nil {
					return fmt.Errorf("preflight exact controller RBAC transition: %w", preflightErr)
				}
				// A repeated upgrade with the same chart finds the candidate
				// already active with its runtime up. Nothing below applies to
				// it: the credential drain, the stop, the cutover and the
				// activation all move an older release toward this one, and
				// the retained runtime guard refuses to stop the active
				// release. Leave the runtime running and let Helm apply the
				// unchanged manifests.
				converged, convergedErr := rollout.CandidateRuntimeConverged(prepareCtx)
				if convergedErr != nil {
					return fmt.Errorf("inspect candidate runtime convergence: %w", convergedErr)
				}
				if converged {
					_, printErr := fmt.Fprintf(output, "candidate release %d is already active with a converged runtime; leaving it running\n", expected.ReleaseSequence)
					return printErr
				}
				protectedPodsRemain, podInventoryErr := inventory.ProtectedRuntimePodsRemain(prepareCtx)
				if podInventoryErr != nil {
					return fmt.Errorf("inventory protected runtime Pods before credential decision: %w", podInventoryErr)
				}
				requiresCredentialDrain, drainErr := controllerRBACTransition.RequiresCredentialDrain(activationState, protectedPodsRemain)
				if drainErr != nil {
					return fmt.Errorf("decide controller credential drain from durable preflight state: %w", drainErr)
				}
				if requiresCredentialDrain {
					if _, drainErr = rollout.BeginControllerCredentialDrain(prepareCtx); drainErr != nil {
						return fmt.Errorf("begin controller credential drain: %w", drainErr)
					}
				}
				if sealErr := predecessorRetirement.SealCurrent(prepareCtx); sealErr != nil {
					return fmt.Errorf("seal current admission inventory: %w", sealErr)
				}
				if verifyErr := predecessorRetirement.VerifyCurrentSealed(prepareCtx); verifyErr != nil {
					return fmt.Errorf("verify sealed current admission inventory: %w", verifyErr)
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
				); cutoverErr != nil {
					return cutoverErr
				}
				// Re-read quota and admission inputs after the old runtime is fully
				// stopped. Activation remains strictly after the RBAC identity
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
				if activateErr := rollout.Activate(prepareCtx); activateErr != nil {
					return activateErr
				}
				if retireErr := predecessorRetirement.Retire(prepareCtx); retireErr != nil {
					return fmt.Errorf("retire predecessor admission inventory: %w", retireErr)
				}
				return nil
			},
		)
	case "teardown-quiesce":
		// The retained guards pin the uninstall hook's arguments, mode name
		// included, so the one uninstall hook keeps the name of the step it
		// began as: it stops the runtime and then deletes the retained
		// inventory.
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
			*previousControllerManagerImage,
		)
		if expectedErr != nil {
			return expectedErr
		}
		clientset, clientErr := kubernetes.NewForConfig(config)
		if clientErr != nil {
			return fmt.Errorf("create Kubernetes client: %w", clientErr)
		}
		rollout := newRolloutGuard(clientset, expected, *managerImage, *webhookSecretName, int32(*webhookPort), int32(*certificateHealthPort), int32(*controllerReplicas), controllerRuntimeArgs, certificateRuntimeArgs, runtimeDeploymentConfigExpressions, runtimePodConfigExpressions, runtimeAdmissionContract, *runtimeAdmissionContractB64)
		err = runTeardownMode(ctx, clientset, rollout)
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
			*previousControllerManagerImage,
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
	if mode == "teardown-quiesce" {
		_, err = fmt.Fprintln(output, "release runtime stopped and retained inventory deleted")
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
	previousControllerManagerImage string,
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
	if previousControllerReleaseSequence == 0 {
		if previousControllerManagerImage != "" {
			return crdupgrade.RuntimeInvariants{}, fmt.Errorf("previous controller manager image requires a previous release sequence")
		}
		if previousControllerServiceAccountName != "" {
			return crdupgrade.RuntimeInvariants{}, fmt.Errorf("previous controller ServiceAccount requires a previous release sequence")
		}
	} else {
		if previousControllerManagerImage == "" {
			return crdupgrade.RuntimeInvariants{}, fmt.Errorf("previous controller manager image is required with a previous release sequence")
		}
		if previousControllerServiceAccountName == "" {
			return crdupgrade.RuntimeInvariants{}, fmt.Errorf("previous controller ServiceAccount is required with a previous release sequence")
		}
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
		PreviousControllerManagerImage:          previousControllerManagerImage,
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
		CertificateRuntimeEnabled:               runtimeAdmissionContract.CertificateRuntimeEnabled,
		HookServiceAccountName:                  expected.HookServiceAccountName,
		ControllerServiceAccountName:            expected.ControllerServiceAccountName,
		ControllerServiceAccountManaged:         expected.ControllerServiceAccountManaged,
		PreviousControllerServiceAccountName:    expected.PreviousControllerServiceAccountName,
		PreviousControllerServiceAccountUID:     expected.PreviousControllerServiceAccountUID,
		PreviousControllerServiceAccountManaged: expected.PreviousControllerServiceAccountManaged,
		PreviousControllerReleaseSequence:       expected.PreviousControllerReleaseSequence,
		PreviousControllerManagerImage:          expected.PreviousControllerManagerImage,
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

func validateModeFlags(mode string, flags *flag.FlagSet) error {
	allowed := map[string]struct{}{}
	switch mode {
	case "image-check":
		allowed["release-sequence"] = struct{}{}
		allowed["manager-image"] = struct{}{}
	case "verify":
		allowed["timeout"] = struct{}{}
	case "identity-probe", "preflight", "reconcile", "teardown-quiesce", "runtime-verify":
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
			"previous-controller-manager-image",
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
	preflight := crdupgrade.NewRuntimeResourceQuotaPreflight(
		contract,
		controllerReplicas,
		expected.ReleaseName,
		expected.ReleaseNamespace,
		expected.ControllerDeploymentName,
		expected.CertificateDeploymentName,
		clientset.CoreV1().ResourceQuotas(expected.ReleaseNamespace),
		clientset.CoreV1().Pods(expected.ReleaseNamespace),
	)
	preflight.PreviousControllerServiceAccountName = expected.PreviousControllerServiceAccountName
	return preflight
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
			Group: "operator.ptah.run", Version: "v1alpha1", Resource: name,
		})
	}
	return crdupgrade.StoredControllerStateClients{
		Schemas:            resource("ptahschemas"),
		Plans:              resource("ptahschemaplans"),
		Approvals:          resource("ptahschemaapprovals"),
		Migrations:         resource("ptahmigrations"),
		MigrationPlans:     resource("ptahmigrationplans"),
		MigrationApprovals: resource("ptahmigrationapprovals"),
	}
}
