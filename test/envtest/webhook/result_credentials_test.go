package webhook_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
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
	"github.com/stokaro/ptah-operator/internal/resultretention"
	"github.com/stokaro/ptah-operator/internal/resultstore"
)

var resultCredentialIssuer *resultcredentials.Issuer
var resultSigningCA tls.Certificate
var resultClientTrust *x509.CertPool
var resultServerTrust []byte

const resultEnrollmentNamespace = "result-enrollment-system"
const resultEnrollmentName = "current"

// This test installation supplies delivery trust explicitly. The production
// manager requires this public policy when durable delivery is enabled.
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
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: resultEnrollmentNamespace}}); err != nil {
		return err
	}
	if err := admin.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: resultEnrollmentNamespace, Name: resultEnrollmentName}, Data: resultcredentials.EnrollmentData(der, resultServerTrust)}); err != nil {
		return err
	}
	policy, err := resultcredentials.NewEnrollmentPolicy(admin, resultEnrollmentNamespace, resultEnrollmentName)
	if err != nil {
		return err
	}
	resultCredentialIssuer = resultCredentialIssuer.WithEnrollmentPolicy(policy)
	return nil
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
	retirement, err := resultretention.Record(identity.Binding, resultretention.Source{Name: record.Name, UID: record.UID, Type: "credential"}, resultretention.MinimumWindow)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("active attempt cannot be marked retired", func(t *testing.T) {
		requireDenied(t, managerAPI.Create(ctx, retirement.DeepCopy(), client.DryRunAll), controllerWriteWebhook, "immutable operation binding")
	})
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
	t.Run("manager cannot create ordinary Secrets with its CREATE grant", func(t *testing.T) {
		requireDenied(t, managerAPI.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: fixture.namespace, Name: "ordinary-manager"}}, client.DryRunAll), controllerWriteWebhook, "outside the result credential namespace")
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
	t.Run("retired record is collected before its Secret", func(t *testing.T) {
		fixture.schema.Status.ActiveOperation = nil
		writeStatus(t, fixture.schema)
		wrong, err := resultretention.Record(identity.Binding, resultretention.Source{Name: record.Name, UID: "replaced", Type: "credential"}, resultretention.MinimumWindow)
		if err != nil {
			t.Fatal(err)
		}
		requireDenied(t, managerAPI.Create(ctx, wrong, client.DryRunAll), controllerWriteWebhook, "immutable operation binding")
		requireDenied(t, admin.Create(ctx, retirement.DeepCopy(), client.DryRunAll), controllerWriteWebhook, "only the configured operator manager")
		before := time.Now().Add(-time.Second)
		// A caller-supplied date must not backdate the retention clock.
		retirement.CreationTimestamp = metav1.NewTime(before.Add(-24 * time.Hour))
		if err := managerAPI.Create(ctx, retirement); err != nil {
			t.Fatal(err)
		}
		if err := managerAPI.Get(ctx, client.ObjectKeyFromObject(retirement), retirement); err != nil {
			t.Fatal(err)
		}
		if retirement.UID == "" || retirement.CreationTimestamp.Before(&metav1.Time{Time: before}) {
			t.Fatal("retirement clock was not assigned by the API")
		}
		if err := resultretention.Eligible(ctx, admin, retirement, time.Now(), resultretention.MinimumWindow); !errors.Is(err, resultretention.ErrWindow) {
			t.Fatalf("new retirement marker bypasses its retention window: %v", err)
		}
		next := retirement.DeepCopy()
		next.Annotations = map[string]string{"override": "cleanup"}
		requireDenied(t, admin.Update(ctx, next, client.DryRunAll), controllerWriteWebhook, "immutable operation binding")
		// Restore the old claim while its original Job and Pod still exist.
		// Its persisted retirement must prevent issuing delivery authority again.
		fixture.schema.Status.ActiveOperation = op
		writeStatus(t, fixture.schema)
		if _, err := issuer.Ensure(ctx, identity); !errors.Is(err, resultdelivery.ErrAuthority) {
			t.Fatalf("retirement did not fence the restored claim: %v", err)
		}
		fixture.schema.Status.ActiveOperation = nil
		writeStatus(t, fixture.schema)
		if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].Kind != "PtahResultRecord" ||
			secret.OwnerReferences[0].Name != record.Name || secret.OwnerReferences[0].UID != record.UID ||
			secret.OwnerReferences[0].BlockOwnerDeletion == nil || *secret.OwnerReferences[0].BlockOwnerDeletion {
			t.Fatal("Secret does not belong to its exact canonical record")
		}
		requireDenied(t, admin.Delete(ctx, secret, client.DryRunAll), controllerWriteWebhook, "immutable operation binding")
		requireDenied(t, admin.Delete(ctx, record, client.PropagationPolicy(metav1.DeletePropagationOrphan), client.DryRunAll), controllerWriteWebhook, "require cascading deletion")
		requireDenied(t, admin.Delete(ctx, record, client.DryRunAll), controllerWriteWebhook, "immutable operation binding")
		cleanupClockOffset.Store(int64(2 * time.Hour))
		defer cleanupClockOffset.Store(0)
		requireDenied(t, admin.Delete(ctx, record, client.DryRunAll), controllerWriteWebhook, "immutable operation binding")
		if err := admin.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
			t.Fatal(err)
		}
		if err := admin.Delete(ctx, record, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &record.UID, ResourceVersion: &record.ResourceVersion}, PropagationPolicy: ptr.To(metav1.DeletePropagationForeground)}); err != nil {
			t.Fatal(err)
		}
		// Envtest has no garbage collector. Exercise its finalizer update and
		// dependent DELETE explicitly; this is admission, not collection proof.
		if err := admin.Get(ctx, client.ObjectKeyFromObject(record), record); err != nil {
			t.Fatal(err)
		}
		if record.DeletionTimestamp.IsZero() || len(record.Finalizers) != 1 || record.Finalizers[0] != metav1.FinalizerDeleteDependents {
			t.Fatal("API did not stage foreground deletion")
		}
		requireDenied(t, admin.Delete(ctx, secret, client.DryRunAll), controllerWriteWebhook, "immutable operation binding")
		record.Finalizers = nil
		if err := admin.Update(ctx, record); err != nil {
			t.Fatalf("garbage collector cannot finalize the record: %v", err)
		}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(record), &recordapi.PtahResultRecord{}); !apierrors.IsNotFound(err) {
			t.Fatalf("retired record survived cleanup: %v", err)
		}
		requireDenied(t, admin.Delete(ctx, secret, client.PropagationPolicy(metav1.DeletePropagationOrphan), client.DryRunAll), controllerWriteWebhook, "require cascading deletion")
		if err := admin.DeleteAllOf(ctx, &corev1.Secret{}, client.InNamespace(fixture.namespace), client.MatchingFields{"metadata.name": secret.Name}, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil {
			t.Fatal(err)
		}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil {
			t.Fatal(err)
		}
		if secret.DeletionTimestamp.IsZero() || len(secret.Finalizers) != 1 || secret.Finalizers[0] != metav1.FinalizerDeleteDependents {
			t.Fatal("API did not stage foreground Secret deletion")
		}
		secret.Finalizers = nil
		if err := admin.Update(ctx, secret); err != nil {
			t.Fatal(err)
		}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(secret), &corev1.Secret{}); !apierrors.IsNotFound(err) {
			t.Fatalf("orphaned projection survived cleanup: %v", err)
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

// Neither issuer nor webhook reloads its local CA during this test. Only the
// policy changes in the API, so refusal proves the stale-replica fence.
func TestResultEnrollmentPolicyFencesStaleReplicas(t *testing.T) {
	f, identity, store := publicationFixture(t, false)
	_, issuedIdentity, issuedStore := publicationFixture(t, true)
	apiClient := clientAs(t, manager.username)
	grant(t, resultEnrollmentNamespace, "enrollment-read", managerSubject(t), rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{resultEnrollmentName}, Verbs: []string{"get"}})
	policyRef, err := resultcredentials.NewEnrollmentPolicy(apiClient, resultEnrollmentNamespace, resultEnrollmentName)
	if err != nil {
		t.Fatal(err)
	}
	oldIssuer, err := resultcredentials.New(apiClient, apiClient, resultSigningCA, resultClientTrust, resultServerTrust)
	if err != nil {
		t.Fatal(err)
	}
	oldIssuer = oldIssuer.WithEnrollmentPolicy(policyRef)
	initialReceipt, err := oldIssuer.Ensure(t.Context(), issuedIdentity)
	if err != nil {
		t.Fatal(err)
	}

	var candidate *recordapi.PtahResultRecord
	interrupted := errors.New("stop before credential persistence")
	capture := publicationWriter{Client: apiClient, before: func(record *recordapi.PtahResultRecord) error { candidate = record.DeepCopy(); return interrupted }}
	capturingIssuer, err := resultcredentials.New(capture, apiClient, resultSigningCA, resultClientTrust, resultServerTrust)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capturingIssuer.Ensure(t.Context(), identity); !errors.Is(err, interrupted) || candidate == nil {
		t.Fatalf("failed to prepare legitimate credential: %v", err)
	}
	attempt := func() error { return apiClient.Create(t.Context(), candidate.DeepCopy(), client.DryRunAll) }
	if err := attempt(); err != nil {
		t.Fatalf("current policy did not admit the candidate: %v", err)
	}

	key := client.ObjectKey{Namespace: resultEnrollmentNamespace, Name: resultEnrollmentName}
	policy := &corev1.ConfigMap{}
	if err := apiClient.Get(t.Context(), key, policy); err != nil {
		t.Fatal(err)
	}
	if err := apiClient.Update(t.Context(), policy.DeepCopy(), client.DryRunAll); !apierrors.IsForbidden(err) {
		t.Fatalf("manager can rewrite enrollment authority: %v", err)
	}
	originalData := policy.DeepCopy().Data
	restore := func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		current := &corev1.ConfigMap{}
		if err := admin.Get(ctx, key, current); err != nil {
			t.Fatal(err)
		}
		current.Data = originalData
		if err := admin.Update(ctx, current); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(restore)
	renewed, err := x509.ParseCertificate(resultSigningCA.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	renewed.SerialNumber = big.NewInt(2)
	renewed.NotAfter = renewed.NotAfter.Add(time.Hour)
	nextDER, err := x509.CreateCertificate(rand.Reader, renewed, renewed, renewed.PublicKey, resultSigningCA.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	policy.Data = resultcredentials.EnrollmentData(nextDER, resultServerTrust)
	if err := admin.Update(t.Context(), policy); err != nil {
		t.Fatal(err)
	}

	if _, err := oldIssuer.Ensure(t.Context(), identity); !errors.Is(err, resultcredentials.ErrCredential) {
		t.Fatalf("stale issuer enrolled after policy advanced: %v", err)
	}
	requireDenied(t, attempt(), controllerWriteWebhook, "immutable operation binding")
	if err := admin.Get(t.Context(), client.ObjectKey{Namespace: f.namespace, Name: candidate.Name}, &recordapi.PtahResultRecord{}); !apierrors.IsNotFound(err) {
		t.Fatalf("refused enrollment left a canonical record: %v", err)
	}
	if got, err := oldIssuer.Ensure(t.Context(), issuedIdentity); err != nil || got != initialReceipt {
		t.Fatalf("policy advancement changed already-issued authority: %v", err)
	}
	payload := publicationPayload(t, issuedIdentity)
	if _, err := issuedStore.Publish(t.Context(), issuedIdentity.Binding, payload, publicationDigest(payload)); err != nil {
		t.Fatalf("policy advancement blocked delivery by an already-issued credential: %v", err)
	}

	restore()
	if err := attempt(); err != nil {
		t.Fatalf("restoring the policy did not recover the same admission: %v", err)
	}
	if _, err := oldIssuer.Ensure(t.Context(), identity); err != nil {
		t.Fatalf("restored policy did not recover issuance: %v", err)
	}
	payload = publicationPayload(t, identity)
	if _, err := store.Publish(t.Context(), identity.Binding, payload, publicationDigest(payload)); err != nil {
		t.Fatalf("recovered credential cannot publish: %v", err)
	}
}
