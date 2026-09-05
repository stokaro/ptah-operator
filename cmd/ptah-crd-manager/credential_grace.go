package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

const protectedRuntimePodWatchDrainLimit = 256

type protectedRuntimePodSnapshotter interface {
	ProtectedRuntimePodSnapshot(context.Context) (crdupgrade.ProtectedRuntimePodSnapshot, error)
}

type protectedRuntimePodWatcher interface {
	Watch(context.Context, metav1.ListOptions) (watch.Interface, error)
}

// protectedRuntimePodStabilityObserver joins a complete Pod LIST to a watch at
// the LIST resourceVersion. Every protected-Pod event or watch restart changes
// the observer identity, forcing the enclosing convergence barrier to begin a
// fresh credential-lifetime window even when a Pod appears and disappears
// between polling sweeps.
type protectedRuntimePodStabilityObserver struct {
	inventory               protectedRuntimePodSnapshotter
	pods                    protectedRuntimePodWatcher
	releaseNamespace        string
	protectedServiceAccount map[string]struct{}

	watch      watch.Interface
	results    <-chan watch.Event
	generation uint64
	identity   string
	proven     bool
}

func newProtectedRuntimePodStabilityObserver(
	inventory protectedRuntimePodSnapshotter,
	pods protectedRuntimePodWatcher,
	rollout *crdupgrade.RolloutGuard,
) (*protectedRuntimePodStabilityObserver, error) {
	if inventory == nil || pods == nil || rollout == nil {
		return nil, errors.New("protected runtime Pod stability dependencies are required")
	}
	if rollout.ReleaseNamespace == "" || rollout.ReleaseNamespace != strings.TrimSpace(rollout.ReleaseNamespace) {
		return nil, errors.New("protected runtime Pod stability release namespace is empty or padded")
	}
	serviceAccounts := []string{
		rollout.ControllerServiceAccountName,
		rollout.CertificateDeploymentName,
	}
	if rollout.PreviousControllerServiceAccountName != "" {
		serviceAccounts = append(serviceAccounts, rollout.PreviousControllerServiceAccountName)
	}
	protected := make(map[string]struct{}, len(serviceAccounts))
	for _, serviceAccount := range serviceAccounts {
		if serviceAccount == "" || serviceAccount != strings.TrimSpace(serviceAccount) {
			return nil, errors.New("protected runtime Pod stability ServiceAccount identity is empty or padded")
		}
		if _, duplicate := protected[serviceAccount]; duplicate {
			return nil, fmt.Errorf("protected runtime Pod stability ServiceAccount %q is duplicated", serviceAccount)
		}
		protected[serviceAccount] = struct{}{}
	}
	return &protectedRuntimePodStabilityObserver{
		inventory:               inventory,
		pods:                    pods,
		releaseNamespace:        rollout.ReleaseNamespace,
		protectedServiceAccount: protected,
	}, nil
}

func (o *protectedRuntimePodStabilityObserver) Observe(ctx context.Context, _ string) (string, bool, error) {
	if o == nil || o.inventory == nil || o.pods == nil || len(o.protectedServiceAccount) == 0 {
		return "", false, errors.New("protected runtime Pod stability observer is incomplete")
	}
	if ctx == nil {
		return "", false, errors.New("protected runtime Pod stability context is nil")
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if o.watch == nil {
		if err := o.restart(ctx); err != nil {
			return "", false, err
		}
	}

	for range protectedRuntimePodWatchDrainLimit {
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		case event, open := <-o.results:
			if !open {
				if err := o.restart(ctx); err != nil {
					return "", false, err
				}
				return o.identity, o.proven, nil
			}
			if event.Type == watch.Bookmark {
				continue
			}
			if event.Type == watch.Error {
				if err := o.restart(ctx); err != nil {
					return "", false, fmt.Errorf("restart protected runtime Pod watch after error event: %w", err)
				}
				return o.identity, o.proven, nil
			}
			pod, ok := event.Object.(*corev1.Pod)
			if !ok || pod == nil {
				return "", false, fmt.Errorf("protected runtime Pod watch returned %T for %s event", event.Object, event.Type)
			}
			if pod.Namespace != o.releaseNamespace || pod.Name == "" || pod.UID == "" {
				return "", false, fmt.Errorf("protected runtime Pod watch returned malformed Pod %q/%q", pod.Namespace, pod.Name)
			}
			if _, protected := o.protectedServiceAccount[pod.Spec.ServiceAccountName]; !protected {
				continue
			}
			if err := o.restart(ctx); err != nil {
				return "", false, fmt.Errorf("restart protected runtime Pod watch after %s event for %s/%s: %w", event.Type, pod.Namespace, pod.Name, err)
			}
			return o.identity, o.proven, nil
		default:
			return o.identity, o.proven, nil
		}
	}
	// A bounded drain prevents an unrelated event flood from monopolizing one
	// convergence sweep. Restart from a fresh LIST instead of preserving the
	// previous proven identity: a protected event may be queued immediately
	// after the drained batch.
	if err := o.restart(ctx); err != nil {
		return "", false, fmt.Errorf("restart saturated protected runtime Pod watch: %w", err)
	}
	return o.identity, o.proven, nil
}

func (o *protectedRuntimePodStabilityObserver) Close() {
	if o == nil || o.watch == nil {
		return
	}
	o.watch.Stop()
	o.watch = nil
	o.results = nil
}

func (o *protectedRuntimePodStabilityObserver) restart(ctx context.Context) error {
	o.Close()
	snapshot, err := o.inventory.ProtectedRuntimePodSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("list protected runtime Pods for stability watch: %w", err)
	}
	if snapshot.ResourceVersion == "" || snapshot.ResourceVersion != strings.TrimSpace(snapshot.ResourceVersion) {
		return errors.New("protected runtime Pod LIST returned an empty or padded resourceVersion")
	}
	podWatch, err := o.pods.Watch(ctx, metav1.ListOptions{
		ResourceVersion:     snapshot.ResourceVersion,
		AllowWatchBookmarks: true,
	})
	if err != nil {
		return fmt.Errorf("watch protected runtime Pods from resourceVersion %s: %w", snapshot.ResourceVersion, err)
	}
	if podWatch == nil {
		return errors.New("protected runtime Pod watch returned nil")
	}
	results := podWatch.ResultChan()
	if results == nil {
		podWatch.Stop()
		return errors.New("protected runtime Pod watch returned a nil result channel")
	}
	o.watch = podWatch
	o.results = results
	o.generation++
	o.identity = "protected-pods:" + strconv.FormatUint(o.generation, 10) + ":" + snapshot.ResourceVersion
	o.proven = !snapshot.PodsRemain
	return nil
}

var _ admissionConvergenceStabilityObserver = (*protectedRuntimePodStabilityObserver)(nil)

type credentialRevocationStabilityObserver struct {
	authorization *crdupgrade.RBACConvergenceBarrier
	pods          *protectedRuntimePodStabilityObserver
	verifyStored  func(context.Context) error
}

func newCredentialRevocationStabilityObserver(
	authorization *crdupgrade.RBACConvergenceBarrier,
	pods *protectedRuntimePodStabilityObserver,
	verifyStored func(context.Context) error,
) (*credentialRevocationStabilityObserver, error) {
	if authorization == nil || pods == nil || verifyStored == nil {
		return nil, errors.New("credential revocation stability dependencies are required")
	}
	if err := authorization.Validate(); err != nil {
		return nil, fmt.Errorf("validate credential revocation authorization barrier: %w", err)
	}
	return &credentialRevocationStabilityObserver{
		authorization: authorization,
		pods:          pods,
		verifyStored:  verifyStored,
	}, nil
}

func (o *credentialRevocationStabilityObserver) Observe(
	ctx context.Context,
	admissionTopologyIdentity string,
) (string, bool, error) {
	if o == nil || o.authorization == nil || o.pods == nil || o.verifyStored == nil {
		return "", false, errors.New("credential revocation stability observer is incomplete")
	}
	authorizationIdentity, authorizationDenied, err := o.authorization.Observe(ctx)
	if err != nil {
		return "", false, fmt.Errorf("observe API-server authorization revocation: %w", err)
	}
	podIdentity, podsAbsent, err := o.pods.Observe(ctx, admissionTopologyIdentity)
	if err != nil {
		return "", false, err
	}
	if err := o.verifyStored(ctx); err != nil {
		return "", false, fmt.Errorf("verify durable credential revocation state: %w", err)
	}
	identity := admissionTopologyIdentity + "\x00" + authorizationIdentity + "\x00" + podIdentity
	return identity, authorizationDenied && podsAbsent && authorizationIdentity == admissionTopologyIdentity, nil
}

func (o *credentialRevocationStabilityObserver) Close() {
	if o == nil || o.pods == nil {
		return
	}
	o.pods.Close()
}

var _ admissionConvergenceStabilityObserver = (*credentialRevocationStabilityObserver)(nil)
