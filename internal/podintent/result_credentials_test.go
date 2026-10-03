package podintent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/internal/resultcredentials/binding"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
)

type credentialMetadataReader struct {
	client.Reader
	metadata metav1.PartialObjectMetadata
	failure  error
	reads    int
}

func (r *credentialMetadataReader) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	r.reads++
	metadata, ok := obj.(*metav1.PartialObjectMetadata)
	if !ok || metadata.Kind != "PtahResultRecord" || metadata.APIVersion != "operator.ptah.run/v1alpha1" {
		return errors.New("credential check requested payload instead of credential record metadata")
	}
	if key.Namespace != r.metadata.Namespace || key.Name != r.metadata.Name {
		return errors.New("credential metadata read used another identity")
	}
	if r.failure != nil {
		return r.failure
	}
	r.metadata.DeepCopyInto(metadata)
	return nil
}

func TestResultCredentialPodMetadataGuard(t *testing.T) {
	for _, row := range []struct {
		name      string
		operation admissionv1.Operation
		allow     bool
	}{
		{"initial Pod", admissionv1.Create, true},
		{"original update", admissionv1.Update, true},
		{"replacement create", admissionv1.Create, false},
		{"wrong Pod UID", admissionv1.Update, false},
		{"wrong Pod name", admissionv1.Update, false},
		{"wrong Job UID", admissionv1.Update, false},
		{"wrong operation", admissionv1.Update, false},
		{"deleting pin", admissionv1.Update, false},
		{"API unavailable", admissionv1.Create, false},
		{"alias reference", admissionv1.Create, false},
		{"foreign credential", admissionv1.Create, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "team", UID: "job-uid", OwnerReferences: []metav1.OwnerReference{{UID: "subject-uid"}}}, Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{AutomountServiceAccountToken: ptr.To(false), Containers: []corev1.Container{{Name: "ptah"}}}}}}
			if err := jobconfig.Attach(job, "subject-uid", 1, "operation", "https://receiver.test"); err != nil {
				t.Fatal(err)
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "team", UID: "pod-uid"}, Spec: *job.Spec.Template.Spec.DeepCopy()}
			reader := &credentialMetadataReader{metadata: metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Namespace: pod.Namespace, Name: jobconfig.CredentialName("subject-uid", "operation", job.Name), Annotations: map[string]string{binding.PodUID: string(pod.UID), binding.PodName: pod.Name, binding.JobUID: string(job.UID), binding.OperationID: "operation"}}}}
			switch row.name {
			case "initial Pod":
				reader.failure = apierrors.NewNotFound(schema.GroupResource{Group: "operator.ptah.run", Resource: "ptahresultrecords"}, reader.metadata.Name)
			case "wrong Pod UID":
				reader.metadata.Annotations[binding.PodUID] = "replacement"
			case "wrong Pod name":
				reader.metadata.Annotations[binding.PodName] = "replacement"
			case "wrong Job UID":
				reader.metadata.Annotations[binding.JobUID] = "replacement"
			case "wrong operation":
				reader.metadata.Annotations[binding.OperationID] = "replacement"
			case "deleting pin":
				reader.metadata.DeletionTimestamp = ptr.To(metav1.Now())
			case "API unavailable":
				reader.failure = errors.New("unavailable")
			case "alias reference":
				pod.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: reader.metadata.Name}}
			case "foreign credential":
				pod.Spec.Volumes[0].Secret.SecretName = jobconfig.SecretPrefix + "other"
			}
			response := (&ValidationHandler{Reader: reader}).validateResultCredential(t.Context(), row.operation, pod, job, "operation")
			if (response == nil) != row.allow {
				t.Fatalf("allow=%v, response=%#v", row.allow, response)
			}
			if row.allow && reader.reads != 1 {
				t.Fatalf("guard did not read metadata: reads=%d", reader.reads)
			}
			if row.name == "API unavailable" && (response == nil || response.Result.Code != 503) {
				t.Fatal("API failure did not fail closed with retryable status")
			}
		})
	}
}

func TestResultPodTokenStillGuardsTheOriginalPod(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, public, private)
	if err != nil {
		t.Fatal(err)
	}
	trust := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "team", UID: "job-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: "operator.ptah.run/v1alpha1", Kind: "PtahSchema", Name: "schema", UID: "subject-uid", Controller: ptr.To(true)}}, Annotations: map[string]string{"operator.ptah.run/operation-id": "operation", "operator.ptah.run/execution-binding-id": "v1-" + strings.Repeat("a", 32), "operator.ptah.run/input-fingerprint": "sha256:" + strings.Repeat("b", 64)}}, Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{AutomountServiceAccountToken: ptr.To(false), Containers: []corev1.Container{{Name: "ptah", Args: []string{"--operation", "resolve"}}}}}}}
	if err := jobconfig.AttachPodToken(job, "subject-uid", 1, "operation", "https://receiver.test", trust); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "team", UID: "pod-uid"}, Spec: *job.Spec.Template.Spec.DeepCopy()}
	for _, row := range []struct {
		name  string
		op    admissionv1.Operation
		allow bool
	}{{"first", admissionv1.Create, true}, {"original", admissionv1.Update, true}, {"replacement", admissionv1.Create, false}, {"changed UID", admissionv1.Update, false}, {"API failure", admissionv1.Create, false}} {
		t.Run(row.name, func(t *testing.T) {
			reader := &credentialMetadataReader{metadata: metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Namespace: pod.Namespace, Name: jobconfig.CredentialName("subject-uid", "operation", job.Name), Annotations: map[string]string{binding.PodUID: string(pod.UID), binding.PodName: pod.Name, binding.JobUID: string(job.UID), binding.OperationID: "operation"}}}}
			switch row.name {
			case "first":
				reader.failure = apierrors.NewNotFound(schema.GroupResource{Group: "operator.ptah.run", Resource: "ptahresultrecords"}, reader.metadata.Name)
			case "changed UID":
				reader.metadata.Annotations[binding.PodUID] = "replaced-pod"
			case "API failure":
				reader.failure = errors.New("unavailable")
			}
			response := (&ValidationHandler{Reader: reader}).validateResultCredential(t.Context(), row.op, pod, job, "operation")
			if (response == nil) != row.allow || reader.reads != 1 {
				t.Fatalf("allow=%v, response=%#v, reads=%d", row.allow, response, reader.reads)
			}
		})
	}
}
