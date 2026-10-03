package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/podintent"
	"github.com/stokaro/ptah-operator/internal/resultcredentials"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultstore"
)

// Issuance performs only bounded control-plane reads and a small credential
// write. It must not hold the family worker across a large result upload.
const resultIssuanceTimeout = 5 * time.Second

type ResultCredentialIssuer interface {
	Ensure(context.Context, resultdelivery.Identity) (resultcredentials.Credential, error)
}

func issueResultCredential(ctx context.Context, reader client.Reader, issuer ResultCredentialIssuer, subject metav1.Object, kind string, job *batchv1.Job, snapshot *api.PodAdmissionSnapshot, executionBinding, inputFingerprint, operation, operationID, engine string) (bool, error) {
	if issuer == nil {
		return false, errors.New("result credential issuer is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, resultIssuanceTimeout)
	defer cancel()
	config, err := jobconfig.Read(job, subject.GetUID(), operationID)
	if err != nil || config.Generation != subject.GetGeneration() || snapshot == nil {
		return false, errors.New("result credential operation binding is invalid")
	}
	pods := &corev1.PodList{}
	if err := reader.List(ctx, pods, client.InNamespace(subject.GetNamespace()), client.MatchingLabels{batchv1.ControllerUidLabel: string(job.UID)}, client.Limit(2)); err != nil {
		return false, err
	}
	if len(pods.Items) == 0 && pods.Continue == "" {
		return false, nil
	}
	if len(pods.Items) != 1 || pods.Continue != "" {
		return false, errors.New("result credential requires one exact Job Pod")
	}
	pod := &pods.Items[0]
	if pod.UID == "" || podintent.ValidateStoredPod(pod, job, snapshot) != nil {
		return false, errors.New("result credential Pod differs from admission intent")
	}
	identity := resultdelivery.Identity{Binding: resultstore.Binding{Namespace: subject.GetNamespace(), Kind: kind, Name: subject.GetName(), UID: subject.GetUID(), Generation: config.Generation, ExecutionBindingID: executionBinding, InputFingerprint: inputFingerprint, Operation: operation, OperationID: operationID, JobName: job.Name, JobUID: job.UID, PodName: pod.Name, PodUID: pod.UID}, Engine: strings.ToLower(engine)}
	credential, err := issuer.Ensure(ctx, identity)
	if err != nil {
		return false, err
	}
	if credential.Name != config.SecretName || credential.UID == "" {
		return false, fmt.Errorf("issuer returned a foreign credential")
	}
	return true, nil
}
