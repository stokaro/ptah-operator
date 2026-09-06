package controllerwrite

import (
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func TestDecodeObjectAcceptsKnownJobFields(t *testing.T) {
	t.Parallel()

	var job batchv1.Job
	err := decodeObject(
		[]byte(`{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"known"},"spec":{}}`),
		&job,
		jobKind,
	)
	if err != nil {
		t.Fatalf("decodeObject() rejected known Job fields: %v", err)
	}
}

func TestDecodeObjectRejectsUnknownFieldsForEveryControllerWriteKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		raw    string
		object any
		kind   metav1.GroupVersionKind
	}{
		{
			name:   "Job nested field from a newer Kubernetes API",
			raw:    `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"unknown"},"spec":{"scheduling":{}}}`,
			object: &batchv1.Job{},
			kind:   jobKind,
		},
		{
			name:   "PodSpec nested field from a newer Kubernetes API",
			raw:    `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"unknown"},"spec":{"template":{"spec":{"evictionResponders":[]}}}}`,
			object: &batchv1.Job{},
			kind:   jobKind,
		},
		{
			name:   "PodSpec field from an older supported Kubernetes API",
			raw:    `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"unknown"},"spec":{"template":{"spec":{"workloadRef":{"name":"foreign"}}}}}`,
			object: &batchv1.Job{},
			kind:   jobKind,
		},
		{
			name:   "volume source field from a newer Kubernetes API",
			raw:    `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"unknown"},"spec":{"template":{"spec":{"volumes":[{"name":"work","emptyDir":{"mode":448}}]}}}}`,
			object: &batchv1.Job{},
			kind:   jobKind,
		},
		{
			name:   "ConfigMap volume default user from a newer Kubernetes API",
			raw:    `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"unknown"},"spec":{"template":{"spec":{"volumes":[{"name":"policy","configMap":{"name":"policy","defaultUser":65532}}]}}}}`,
			object: &batchv1.Job{},
			kind:   jobKind,
		},
		{
			name:   "Secret volume default user from a newer Kubernetes API",
			raw:    `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"unknown"},"spec":{"template":{"spec":{"volumes":[{"name":"registry","secret":{"secretName":"registry","defaultUser":65532}}]}}}}`,
			object: &batchv1.Job{},
			kind:   jobKind,
		},
		{
			name:   "projected volume default user from a newer Kubernetes API",
			raw:    `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"unknown"},"spec":{"template":{"spec":{"volumes":[{"name":"plan","projected":{"defaultUser":65532,"sources":[]}}]}}}}`,
			object: &batchv1.Job{},
			kind:   jobKind,
		},
		{
			name:   "projected path user from a newer Kubernetes API",
			raw:    `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"unknown"},"spec":{"template":{"spec":{"volumes":[{"name":"plan","projected":{"sources":[{"configMap":{"name":"plan","items":[{"key":"chunk","path":"chunk","user":65532}]}}]}}]}}}}`,
			object: &batchv1.Job{},
			kind:   jobKind,
		},
		{
			name:   "service account token projection user from a newer Kubernetes API",
			raw:    `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"unknown"},"spec":{"template":{"spec":{"volumes":[{"name":"token","projected":{"sources":[{"serviceAccountToken":{"path":"token","user":65532}}]}}]}}}}`,
			object: &batchv1.Job{},
			kind:   jobKind,
		},
		{
			name:   "cluster trust bundle projection user from a newer Kubernetes API",
			raw:    `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"unknown"},"spec":{"template":{"spec":{"volumes":[{"name":"trust","projected":{"sources":[{"clusterTrustBundle":{"name":"trust","path":"ca.pem","user":65532}}]}}]}}}}`,
			object: &batchv1.Job{},
			kind:   jobKind,
		},
		{
			name:   "Pod certificate projection user from a newer Kubernetes API",
			raw:    `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"unknown"},"spec":{"template":{"spec":{"volumes":[{"name":"identity","projected":{"sources":[{"podCertificate":{"signerName":"example.test/signer","keyPath":"tls.key","certificateChainPath":"tls.crt","user":65532}}]}}]}}}}`,
			object: &batchv1.Job{},
			kind:   jobKind,
		},
		{
			name:   "downward API volume default user from a newer Kubernetes API",
			raw:    `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"unknown"},"spec":{"template":{"spec":{"volumes":[{"name":"metadata","downwardAPI":{"defaultUser":65532}}]}}}}`,
			object: &batchv1.Job{},
			kind:   jobKind,
		},
		{
			name:   "downward API volume file user from a newer Kubernetes API",
			raw:    `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"unknown"},"spec":{"template":{"spec":{"volumes":[{"name":"metadata","downwardAPI":{"items":[{"path":"labels","user":65532}]}}]}}}}`,
			object: &batchv1.Job{},
			kind:   jobKind,
		},
		{
			name:   "HTTP probe protocol from a newer Kubernetes API",
			raw:    `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"unknown"},"spec":{"template":{"spec":{"containers":[{"name":"ptah","livenessProbe":{"httpGet":{"port":8080,"protocol":"HTTP/1.1"}}}]}}}}`,
			object: &batchv1.Job{},
			kind:   jobKind,
		},
		{
			name:   "gRPC probe mode from a newer Kubernetes API",
			raw:    `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"unknown"},"spec":{"template":{"spec":{"containers":[{"name":"ptah","livenessProbe":{"grpc":{"port":8080,"mode":"Pod"}}}]}}}}`,
			object: &batchv1.Job{},
			kind:   jobKind,
		},
		{
			name:   "volume mount field from a newer Kubernetes API",
			raw:    `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"unknown"},"spec":{"template":{"spec":{"containers":[{"name":"ptah","volumeMounts":[{"name":"work","mountPath":"/work","bindMountOptions":["noexec"]}]}]}}}}`,
			object: &batchv1.Job{},
			kind:   jobKind,
		},
		{
			name:   "ConfigMap top-level field",
			raw:    `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"unknown"},"future":true}`,
			object: &corev1.ConfigMap{},
			kind:   configMapKind,
		},
		{
			name:   "plan spec field",
			raw:    `{"apiVersion":"operator.ptah.dev/v1alpha1","kind":"PtahSchemaPlan","metadata":{"name":"unknown"},"spec":{"future":true}}`,
			object: &operatorv1alpha1.PtahSchemaPlan{},
			kind:   planKind,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := decodeObject([]byte(test.raw), test.object, test.kind)
			if err == nil {
				t.Fatal("decodeObject() accepted an unknown field")
			}
			if !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("decodeObject() error = %v, want an unknown-field diagnostic", err)
			}
		})
	}
}

func TestDecodeObjectRejectsDuplicateFields(t *testing.T) {
	t.Parallel()

	var job batchv1.Job
	err := decodeObject(
		[]byte(`{"apiVersion":"batch/v1","kind":"Job","kind":"Job","metadata":{"name":"duplicate"},"spec":{}}`),
		&job,
		jobKind,
	)
	if err == nil {
		t.Fatal("decodeObject() accepted a duplicate field")
	}
	if !strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("decodeObject() error = %v, want a duplicate-field diagnostic", err)
	}
}

func TestDecodeObjectBoundsStrictDiagnostics(t *testing.T) {
	t.Parallel()

	var job batchv1.Job
	err := decodeObject(
		[]byte(`{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"bounded"},"spec":{"future1":true,"future2":true,"future3":true,"future4":true,"future5":true,"future6":true}}`),
		&job,
		jobKind,
	)
	if err == nil {
		t.Fatal("decodeObject() accepted unknown fields")
	}
	if !strings.Contains(err.Error(), "(and 2 more strict decoding errors)") {
		t.Fatalf("decodeObject() error = %v, want bounded strict-error count", err)
	}
	if strings.Contains(err.Error(), "future5") || strings.Contains(err.Error(), "future6") {
		t.Fatalf("decodeObject() error = %v, want at most %d detailed strict errors", err, maxStrictDecodeErrors)
	}
}
