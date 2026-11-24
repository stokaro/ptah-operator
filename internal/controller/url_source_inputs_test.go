package controller

import (
	"maps"
	"testing"

	corev1 "k8s.io/api/core/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/fingerprint"
	"github.com/stokaro/ptah-operator/internal/urlsource"
)

// TestURLSourceInputsKeepTheSecretEntries holds the fingerprint inputs of a
// URL source to the entries a release before SecretProviderClass sources
// wrote for a Secret key, so a resource that reads a Secret keeps its claims,
// and to two entries of its own for a class object, so a class never hashes
// like a Secret of the same names.
func TestURLSourceInputsKeepTheSecretEntries(t *testing.T) {
	t.Parallel()

	secret := urlsource.OfSecret(corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "ledger"}, Key: "url"})
	class := urlsource.Source{Class: &operatorv1alpha1.SecretProviderClassObjectSelector{Name: "ledger", ObjectName: "url"}}
	for _, test := range []struct {
		name   string
		source urlsource.Source
		want   map[string]string
	}{
		{name: "a Secret key", source: secret, want: map[string]string{"target_secret": "ledger", "target_key": "url"}},
		{name: "a class object", source: class, want: map[string]string{"target_secret_provider_class": "ledger", "target_object_name": "url"}},
	} {
		inputs := map[string]string{}
		addURLSourceInputs(inputs, "target", test.source)
		if !maps.Equal(inputs, test.want) {
			t.Errorf("%s: inputs = %v, want %v", test.name, inputs, test.want)
		}
	}
}

// TestADriftCheckOnAClassShadowIsAnotherPolicy holds the policy fingerprint
// of a Secret shadow to the digest of the map a release before class sources
// hashed, and a class shadow of the same names to another digest.
func TestADriftCheckOnAClassShadowIsAnotherPolicy(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Spec.Policy.DriftCheck = &operatorv1alpha1.MigrationDriftCheck{Shadow: operatorv1alpha1.DatabaseTargetRef{
		URLFrom: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "ledger-shadow"}, Key: "url"},
	}}
	bySecret, err := migrationPolicyFingerprint(migration)
	if err != nil {
		t.Fatal(err)
	}
	before, err := fingerprint.DigestCanonicalJSON(map[string]string{
		"apply": string(migration.Spec.Policy.Apply), "lock_timeout": migration.Spec.Policy.LockTimeout.Duration.String(),
		"transaction_mode":          migration.Spec.Policy.TransactionMode,
		"drift_check_shadow_secret": "ledger-shadow", "drift_check_shadow_key": "url",
	})
	if err != nil {
		t.Fatal(err)
	}
	if bySecret != before {
		t.Fatalf("a Secret shadow's policy fingerprint = %s, want the %s it had before class sources", bySecret, before)
	}

	migration.Spec.Policy.DriftCheck.Shadow = operatorv1alpha1.DatabaseTargetRef{
		URLFromSecretProviderClass: &operatorv1alpha1.SecretProviderClassObjectSelector{Name: "ledger-shadow", ObjectName: "url"},
	}
	byClass, err := migrationPolicyFingerprint(migration)
	if err != nil {
		t.Fatal(err)
	}
	if byClass == bySecret {
		t.Fatal("a class shadow hashes like the Secret shadow of the same names")
	}
}
