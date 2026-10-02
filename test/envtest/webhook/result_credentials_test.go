package webhook_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	recordapi "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultcredentials"
	"github.com/stokaro/ptah-operator/internal/resultcredentials/binding"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultstore"
)

var resultCredentialIssuer *resultcredentials.Issuer
var resultSigningCA tls.Certificate
var resultClientTrust *x509.CertPool
var resultServerTrust []byte

// This test installation supplies delivery trust explicitly. The production
// manager still leaves issuance disabled until its trust lifecycle is wired.
func setupResultCredentialIssuer() error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(7 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		return err
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	resultSigningCA = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	resultClientTrust = x509.NewCertPool()
	resultClientTrust.AddCert(parsed)
	resultServerTrust = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	resultCredentialIssuer, err = resultcredentials.New(admin, admin, resultSigningCA, resultClientTrust, resultServerTrust)
	return err
}

func TestResultCredentialAdmission(t *testing.T) {
	plane.Require(t)
	fixture := newDispatchFixture(t, "credentials", "https://receiver.operator.svc:9444")
	ctx := t.Context()
	job := fixture.job.DeepCopy()
	if err := admin.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	pod := podFor(job)
	if err := clientAs(t, jobController).Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	fixture.schema.Status.ActiveOperation.JobUID = job.UID
	writeStatus(t, fixture.schema)
	grant(t, fixture.namespace, "manager-result-secrets", managerSubject(t), rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"create"}})
	grant(t, fixture.namespace, "manager-result-records", managerSubject(t),
		rbacv1.PolicyRule{APIGroups: []string{"operator.ptah.run"}, Resources: []string{"ptahresultrecords"}, Verbs: []string{"get", "create"}},
		rbacv1.PolicyRule{APIGroups: []string{"operator.ptah.run"}, Resources: []string{"ptahschemas", "ptahmigrations"}, Verbs: []string{"get"}},
		rbacv1.PolicyRule{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"get"}},
		rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}})
	managerAPI := clientAs(t, manager.username)
	issuer, err := resultcredentials.New(managerAPI, managerAPI, resultSigningCA, resultClientTrust, resultServerTrust)
	if err != nil {
		t.Fatal(err)
	}
	op := fixture.schema.Status.ActiveOperation
	identity := resultdelivery.Identity{Binding: resultstore.Binding{Namespace: fixture.namespace, Kind: "PtahSchema", Name: fixture.schema.Name, UID: fixture.schema.UID, Generation: fixture.schema.Generation, ExecutionBindingID: op.ExecutionBindingID, InputFingerprint: op.InputFingerprint, OperationID: op.ID, Operation: "resolve", JobName: job.Name, JobUID: job.UID, PodName: pod.Name, PodUID: pod.UID}}
	receipt, err := issuer.Ensure(ctx, identity)
	if err != nil {
		t.Fatalf("the actual issuer's Secret was not admitted: %v", err)
	}
	secret := &corev1.Secret{}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: fixture.namespace, Name: receipt.Name}, secret); err != nil {
		t.Fatal(err)
	}
	record := &recordapi.PtahResultRecord{}
	if err := managerAPI.Get(ctx, client.ObjectKeyFromObject(secret), record); err != nil {
		t.Fatal(err)
	}
	databaseSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: fixture.namespace, Name: "database-boundary"}, Data: map[string][]byte{"password": []byte("test-only")}}
	if err := admin.Create(ctx, databaseSecret); err != nil {
		t.Fatal(err)
	}
	if err := managerAPI.Get(ctx, client.ObjectKeyFromObject(databaseSecret), &corev1.Secret{}); !apierrors.IsForbidden(err) {
		t.Fatalf("manager can read database credentials: %v", err)
	}
	if record.UID != receipt.UID {
		t.Fatal("receipt does not name the canonical record")
	}
	if err := managerAPI.Get(ctx, client.ObjectKeyFromObject(secret), &corev1.Secret{}); !apierrors.IsForbidden(err) {
		t.Fatalf("issuer can read Secrets: %v", err)
	}
	if repeated, err := issuer.Ensure(ctx, identity); err != nil || repeated != receipt {
		t.Fatalf("retry without Secret read changed credential: %#v %v", repeated, err)
	}
	t.Run("record metadata cannot move the Pod pin", func(t *testing.T) {
		next := record.DeepCopy()
		next.Annotations[resultcredentials.AnnotationPodUID] = "replacement"
		requireDenied(t, admin.Update(ctx, next, client.DryRunAll), controllerWriteWebhook, "immutable operation binding")
	})
	t.Run("active record deletion is refused", func(t *testing.T) {
		requireDenied(t, admin.Delete(ctx, record, client.DryRunAll), controllerWriteWebhook, "immutable operation binding")
		requireDenied(t, admin.DeleteAllOf(ctx, &recordapi.PtahResultRecord{}, client.InNamespace(fixture.namespace), client.MatchingFields{"metadata.name": record.Name}, client.DryRunAll), controllerWriteWebhook, "immutable operation binding")
	})
	t.Run("unchanged Secret update is admitted", func(t *testing.T) {
		if err := admin.Update(ctx, secret.DeepCopy(), client.DryRunAll); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("metadata update cannot move the Pod pin", func(t *testing.T) {
		next := secret.DeepCopy()
		next.Annotations[resultcredentials.AnnotationPodUID] = "replacement"
		requireDenied(t, admin.Update(ctx, next, client.DryRunAll), controllerWriteWebhook, "immutable operation binding")
	})
	t.Run("original Pod update reads only credential metadata", func(t *testing.T) {
		// PartialObjectMetadata goes through the real API content negotiation.
		if err := admin.Update(ctx, pod.DeepCopy(), client.DryRunAll); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("replacement Pod cannot mount the first credential", func(t *testing.T) {
		requireDenied(t, clientAs(t, jobController).Create(ctx, podFor(job), client.DryRunAll), podIntentWebhook, "already bound to the original Pod")
	})
	t.Run("active deletion is refused even to an administrator", func(t *testing.T) {
		requireDenied(t, admin.Delete(ctx, secret, client.DryRunAll), controllerWriteWebhook, "immutable operation binding")
	})
	t.Run("collection deletion cannot erase an active pin", func(t *testing.T) {
		requireDenied(t, admin.DeleteAllOf(ctx, &corev1.Secret{}, client.InNamespace(fixture.namespace), client.MatchingFields{"metadata.name": secret.Name}, client.DryRunAll), controllerWriteWebhook, "immutable operation binding")
	})
	t.Run("ordinary Secrets remain outside the guard", func(t *testing.T) {
		if err := admin.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: fixture.namespace, Name: "ordinary"}}, client.DryRunAll); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("reserved name cannot be precreated by another writer", func(t *testing.T) {
		candidate := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: fixture.namespace, Name: jobconfig.SecretPrefix + "foreign"}}
		requireDenied(t, admin.Create(ctx, candidate, client.DryRunAll), controllerWriteWebhook, "only the configured operator manager")
	})
	t.Run("refusal depends on the installed chart entry", func(t *testing.T) {
		attempt := func() error { return admin.Delete(ctx, secret, client.DryRunAll) }
		removed, index := removeValidatingEntry(t, controllerWriteWebhook)
		restored := false
		restore := func() {
			if !restored {
				restored = true
				insertValidatingEntry(t, removed, index)
			}
		}
		t.Cleanup(restore)
		if err := eventually(10*time.Second, attempt); err != nil {
			t.Fatalf("removing the guard did not admit deletion: %v", err)
		}
		if err := admin.Delete(ctx, record, client.DryRunAll); err != nil {
			t.Fatalf("record deletion stayed refused without its guard: %v", err)
		}
		restore()
		if err := eventually(10*time.Second, func() error {
			if attempt() == nil {
				return fmt.Errorf("deletion still admitted")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		requireDenied(t, attempt(), controllerWriteWebhook, "immutable operation binding")
		requireDenied(t, admin.Delete(ctx, record, client.DryRunAll), controllerWriteWebhook, "immutable operation binding")
	})
	t.Run("retired operation permits credential cleanup", func(t *testing.T) {
		fixture.schema.Status.ActiveOperation = nil
		writeStatus(t, fixture.schema)
		if err := admin.DeleteAllOf(ctx, &corev1.Secret{}, client.InNamespace(fixture.namespace), client.MatchingFields{"metadata.name": secret.Name}); err != nil {
			t.Fatal(err)
		}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(secret), &corev1.Secret{}); !apierrors.IsNotFound(err) {
			t.Fatalf("retired pin survived collection cleanup: %v", err)
		}
		if err := admin.DeleteAllOf(ctx, &recordapi.PtahResultRecord{}, client.InNamespace(fixture.namespace), client.MatchingFields{"metadata.name": record.Name}); err != nil {
			t.Fatal(err)
		}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(record), &recordapi.PtahResultRecord{}); !apierrors.IsNotFound(err) {
			t.Fatalf("retired record survived cleanup: %v", err)
		}
	})
}

func credentialProbe(namespace string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, GenerateName: "unrelated-"}, Spec: corev1.PodSpec{AutomountServiceAccountToken: ptr.To(false), Containers: []corev1.Container{{Name: "probe", Image: "example.invalid/probe:1"}}}}
}

func TestUnrelatedPodCredentialReferences(t *testing.T) {
	plane.Require(t)
	namespace := newNamespace(t, "credential-pods")
	if err := admin.Create(t.Context(), &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "default"}}); err != nil {
		t.Fatal(err)
	}
	secretName := jobconfig.SecretPrefix + "not-created-yet"
	env := corev1.EnvVar{Name: "KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secretName}, Key: "tls.key"}}}
	from := corev1.EnvFromSource{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: secretName}}}
	rows := map[string]func(*corev1.Pod){
		"Secret volume": func(p *corev1.Pod) {
			p.Spec.Volumes = []corev1.Volume{{Name: "key", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secretName}}}}
		},
		"projected volume": func(p *corev1.Pod) {
			p.Spec.Volumes = []corev1.Volume{{Name: "key", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: secretName}}}}}}}}
		},
		"CSI authentication": func(p *corev1.Pod) {
			p.Spec.Volumes = []corev1.Volume{{Name: "key", VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: "example.test", NodePublishSecretRef: &corev1.LocalObjectReference{Name: secretName}}}}}
		},
		"env":     func(p *corev1.Pod) { p.Spec.Containers[0].Env = []corev1.EnvVar{env} },
		"envFrom": func(p *corev1.Pod) { p.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{from} },
		"init env": func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "init", Image: "example.invalid/probe:1", Env: []corev1.EnvVar{env}}}
		},
		"init envFrom": func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "init", Image: "example.invalid/probe:1", EnvFrom: []corev1.EnvFromSource{from}}}
		},
		"image pull": func(p *corev1.Pod) { p.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: secretName}} },
	}

	for name, source := range map[string]corev1.VolumeSource{
		"Azure File": {AzureFile: &corev1.AzureFileVolumeSource{SecretName: secretName, ShareName: "probe"}},
		"CephFS":     {CephFS: &corev1.CephFSVolumeSource{Monitors: []string{"127.0.0.1"}, SecretRef: &corev1.LocalObjectReference{Name: secretName}}},
		"Cinder":     {Cinder: &corev1.CinderVolumeSource{VolumeID: "probe", SecretRef: &corev1.LocalObjectReference{Name: secretName}}},
		"FlexVolume": {FlexVolume: &corev1.FlexVolumeSource{Driver: "example.test/probe", SecretRef: &corev1.LocalObjectReference{Name: secretName}}},
		"iSCSI":      {ISCSI: &corev1.ISCSIVolumeSource{TargetPortal: "127.0.0.1:3260", IQN: "iqn.2026-10.example:probe", SecretRef: &corev1.LocalObjectReference{Name: secretName}}},
		"RBD":        {RBD: &corev1.RBDVolumeSource{CephMonitors: []string{"127.0.0.1"}, RBDImage: "probe", SecretRef: &corev1.LocalObjectReference{Name: secretName}}},
		"ScaleIO":    {ScaleIO: &corev1.ScaleIOVolumeSource{Gateway: "https://example.test", System: "probe", VolumeName: "probe", SecretRef: &corev1.LocalObjectReference{Name: secretName}}},
		"StorageOS":  {StorageOS: &corev1.StorageOSVolumeSource{VolumeName: "probe", SecretRef: &corev1.LocalObjectReference{Name: secretName}}},
	} {
		rows[name] = func(p *corev1.Pod) { p.Spec.Volumes = []corev1.Volume{{Name: "key", VolumeSource: source}} }
	}
	for name, mutate := range rows {
		t.Run(name, func(t *testing.T) {
			pod := credentialProbe(namespace)
			mutate(pod)
			if refs := binding.References(pod); len(refs) != 1 || refs[0] != secretName {
				t.Fatalf("credential scanner missed reference: %v", refs)
			}
			requireDenied(t, admin.Create(t.Context(), pod, client.DryRunAll), podIntentWebhook, "result credentials may only be projected")
		})
	}
	t.Run("ordinary Pod remains admitted", func(t *testing.T) {
		if err := admin.Create(t.Context(), credentialProbe(namespace), client.DryRunAll); err != nil {
			t.Fatal(err)
		}
	})
	for _, mode := range []string{"env", "envFrom"} {
		t.Run("ephemeral "+mode, func(t *testing.T) {
			pod := credentialProbe(namespace)
			if err := admin.Create(t.Context(), pod); err != nil {
				t.Fatal(err)
			}
			ephemeral := corev1.EphemeralContainer{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", Image: "example.invalid/probe:1"}}
			if mode == "env" {
				ephemeral.Env = []corev1.EnvVar{env}
			} else {
				ephemeral.EnvFrom = []corev1.EnvFromSource{from}
			}
			pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{ephemeral}
			requireDenied(t, admin.SubResource("ephemeralcontainers").Update(t.Context(), pod, client.DryRunAll), podIntentWebhook, "result credentials may only be projected")
		})
	}
	t.Run("refusal depends on the chart routing", func(t *testing.T) {
		attempt := func() error {
			pod := credentialProbe(namespace)
			rows["env"](pod)
			return admin.Create(t.Context(), pod, client.DryRunAll)
		}
		removed, index := removeValidatingEntry(t, podIntentWebhook)
		restored := false
		restore := func() {
			if !restored {
				restored = true
				insertValidatingEntry(t, removed, index)
			}
		}
		t.Cleanup(restore)
		if err := eventually(10*time.Second, attempt); err != nil {
			t.Fatalf("without the guard the Pod was never admitted: %v", err)
		}
		restore()
		if err := eventually(10*time.Second, func() error {
			if attempt() == nil {
				return fmt.Errorf("Pod still admitted")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		requireDenied(t, attempt(), podIntentWebhook, "result credentials may only be projected")
	})
}
