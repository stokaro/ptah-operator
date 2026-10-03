//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

const narrowedPolicyKey = "narrowed.yaml"

type verificationPolicyFixture struct {
	t       *testing.T
	ctx     context.Context
	cluster *harness.Cluster
	object  *corev1.ConfigMap
}

func newVerificationPolicyFixture(t *testing.T, ctx context.Context, cluster *harness.Cluster, namespace, name, artifactType string) *verificationPolicyFixture {
	t.Helper()
	if artifactType != schemaArtifactType && artifactType != migrationArtifactType {
		t.Fatal("verification-policy fixture needs a supported artifact type")
	}
	f := &verificationPolicyFixture{t: t, ctx: ctx, cluster: cluster, object: &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}, Immutable: ptr.To(true),
		Data: map[string]string{
			verificationPolicyKey: "version: 1\nartifact_types:\n  - " + schemaArtifactType + "\n  - " + migrationArtifactType + "\n",
			narrowedPolicyKey:     "version: 1\nartifact_types:\n  - " + artifactType + "\n",
		},
	}}
	if err := cluster.Client.Create(ctx, f.object); err != nil {
		t.Fatalf("create isolated verification policy: %v", err)
	}
	return f
}

func (f *verificationPolicyFixture) identity(key string) verificationPolicyIdentity {
	f.t.Helper()
	current := &corev1.ConfigMap{}
	if err := f.cluster.Client.Get(f.ctx, client.ObjectKeyFromObject(f.object), current); err != nil {
		f.t.Fatalf("read isolated verification policy: %v", err)
	}
	if current.UID == "" || current.UID != f.object.UID || current.Immutable == nil || !*current.Immutable ||
		current.DeletionTimestamp != nil || current.Data[key] == "" || current.Data[key] != f.object.Data[key] {
		f.t.Fatal("isolated verification policy lost its exact immutable contents or UID")
	}
	return verificationPolicyIdentity{uid: current.UID, digest: sha256Digest([]byte(current.Data[key]))}
}

func (f *verificationPolicyFixture) replaceIdentity() {
	f.t.Helper()
	old := f.object.DeepCopy()
	if err := f.cluster.Client.Delete(f.ctx, old, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &old.UID}}); err != nil {
		f.t.Fatalf("delete the exact verification policy: %v", err)
	}
	err := wait.PollUntilContextTimeout(f.ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		err := f.cluster.Client.Get(ctx, client.ObjectKeyFromObject(old), &corev1.ConfigMap{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
	if err != nil {
		f.t.Fatalf("wait for the deleted verification policy: %v", err)
	}
	f.object = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: old.Namespace, Name: old.Name},
		Immutable:  ptr.To(true), Data: old.Data,
	}
	if err := f.cluster.Client.Create(f.ctx, f.object); err != nil {
		f.t.Fatalf("recreate the verification policy under the same name: %v", err)
	}
	if f.object.UID == "" || f.object.UID == old.UID {
		f.t.Fatal("recreated verification policy reused its old UID")
	}
}
